/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
targetloadpacking package provides K8s scheduler plugin for best-fit variant of bin packing based on CPU utilisation around a target load
It contains plugin for Score extension point.
*/

package targetloadpacking

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/francoispqt/gojay"
	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	metricsAgentReportingIntervalSeconds       = 60  // Time interval in seconds for each metrics agent ingestion.
	requestMultiplier                          = 1.5 // CPU usage is predicted as 1.5*requests for containers without limits i.e. Burstable QOS.
	httpClientTimeoutSeconds                   = 55 * time.Second
	metricsUpdateIntervalSeconds               = 30
	DefaultRequestsMilliCores            int64 = 1000 // Default 1 core CPU usage for containers without requests/limits i.e. Best Effort QOS.
	DefaultTargetUtilizationPercent      int64 = 40   // Default CPU Util Threshold. Recommended to keep -10 than desired limit.
	Name                                       = "TargetLoadPacking"
)

var (
	requestsMilliCores           = DefaultRequestsMilliCores       // Default 1 core CPU usage for containers without requests/limits i.e. Best Effort QOS.
	hostTargetUtilizationPercent = DefaultTargetUtilizationPercent // Upper limit of CPU percent for bin packing. Recommended to keep -10 than desired limit.
	watcherAddress               = "http://127.0.0.1:2020"
	// Exported for testing
	WatcherBaseUrl = "/watcher"
)

type TargetLoadPacking struct {
	handle       framework.FrameworkHandle
	client       http.Client
	metrics      watcher.WatcherMetrics
	eventHandler *trimaran.PodAssignEventHandler
}

func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	hostTargetUtilizationPercent = args.TargetUtilization
	requestsMilliCores = args.DefaultRequests.Cpu().MilliValue()
	watcherAddress = args.WatcherAddress

	podAssignEventHandler := trimaran.New()
	pl := &TargetLoadPacking{
		handle: handle,
		client: http.Client{
			Timeout: httpClientTimeoutSeconds,
		},
		eventHandler: podAssignEventHandler,
	}

	pl.handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(podAssignEventHandler)

	// populate metrics before returning
	err = pl.updateMetrics()
	if err != nil {
		klog.Warningf("unable to populate metrics initially: %v", err)
	}
	go func() {
		metricsUpdaterTicker := time.NewTicker(time.Second * metricsUpdateIntervalSeconds)
		for range metricsUpdaterTicker.C {
			err = pl.updateMetrics()
			if err != nil {
				klog.Warningf("unable to update metrics: %v", err)
			}
		}
	}()

	return pl, nil
}

func (pl *TargetLoadPacking) updateMetrics() error {
	req, err := http.NewRequest(http.MethodGet, watcherAddress+WatcherBaseUrl, nil)
	if err != nil {
		klog.Errorf("new watcher request failed: %v", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := pl.client.Do(req) //TODO(aqadeer): Add a couple of retries for transient errors
	if err != nil {
		klog.Errorf("request to watcher failed: %v", err)
		pl.metrics = watcher.WatcherMetrics{} // reset the metrics to avoid stale metrics. Probably use a timestamp for better control
		return err
	}
	defer resp.Body.Close()
	klog.V(6).Infof("received status code %v from watcher", resp.StatusCode)
	if resp.StatusCode == http.StatusOK {
		data := watcher.Data{NodeMetricsMap: make(map[string]watcher.NodeMetrics)}
		var metrics = watcher.WatcherMetrics{Data: data}
		dec := gojay.BorrowDecoder(resp.Body)
		defer dec.Release()
		err = dec.Decode(&metrics)
		if err != nil {
			klog.Errorf("unable to decode watcher metrics: %v", err)
		}
		pl.metrics = metrics
	} else {
		klog.Errorf("received status code %v from watcher", resp.StatusCode)
	}
	return nil
}

func (pl *TargetLoadPacking) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (*pluginConfig.TargetLoadPackingArgs, error) {
	if obj == nil {
		targetUtil := DefaultTargetUtilizationPercent
		return &pluginConfig.TargetLoadPackingArgs{
			TargetUtilization: int64(targetUtil),
			DefaultRequests:   v1.ResourceList{v1.ResourceCPU: resource.MustParse(strconv.FormatInt(DefaultRequestsMilliCores, 10) + "m")},
		}, nil
	}
	if targetLoadPackingArgs, ok := obj.(*pluginConfig.TargetLoadPackingArgs); ok {
		if targetLoadPackingArgs.WatcherAddress == "" {
			return nil, errors.New("no watcher address configured")
		}
		return targetLoadPackingArgs, nil
	}
	return nil, fmt.Errorf("want args to be of type TargetLoadPackingArgs, got %T", obj)
}

func (pl *TargetLoadPacking) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return framework.MinNodeScore, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	metrics := pl.metrics                                    // copy to maintain snapshot lest updateMetrics() updates the value
	if _, ok := metrics.Data.NodeMetricsMap[nodeName]; !ok { // This means the node is new (no metrics yet) or metrics are unavailable due to 404 or 500
		klog.V(6).Infof("unable to find metrics for node %v", nodeName)
		return framework.MinNodeScore, nil // Avoid the node by scoring minimum
		//TODO(aqadeer): If this happens for a long time, fall back to allocation based packing. This could mean maintaining failure state across cycles if scheduler doesn't provide this state
	}

	var curPodCPUUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += PredictUtilisation(&container)
	}
	klog.V(6).Infof("predicted utilisation for pod %v: %v", pod.Name, curPodCPUUsage)
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
	}

	var nodeCPUUtilPercent float64
	var cpuMetricFound bool
	for _, metric := range metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			nodeCPUUtilPercent = metric.Value
			cpuMetricFound = true
		}
	}

	if !cpuMetricFound {
		klog.Errorf("cpu metric not found for node %v in node metrics %v", nodeName, metrics.Data.NodeMetricsMap[nodeName].Metrics)
		return framework.MinNodeScore, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	klog.V(6).Infof("node %v CPU Utilisation (millicores): %v, Capacity: %v", nodeName, nodeCPUUtilMillis, nodeCPUCapMillis)

	var missingCPUUtilMillis int64 = 0
	pl.eventHandler.RLock()
	defer pl.eventHandler.RUnlock()
	if _, ok := pl.eventHandler.ScheduledPodsCache[nodeName]; ok {
		for _, info := range pl.eventHandler.ScheduledPodsCache[nodeName] {
			// If the time stamp of the scheduled pod is outside fetched metrics window, or it is within metrics reporting interval seconds, we predict util.
			// Note that the second condition doesn't guarantee metrics for that pod are not reported yet as the 0 <= t <= 2*metricsAgentReportingIntervalSeconds
			// t = metricsAgentReportingIntervalSeconds is taken as average case and it doesn't hurt us much if we are
			// counting metrics twice in case actual t is less than metricsAgentReportingIntervalSeconds
			if info.Timestamp.Unix() > metrics.Window.End || info.Timestamp.Unix() <= metrics.Window.End &&
				(metrics.Window.End-info.Timestamp.Unix()) < metricsAgentReportingIntervalSeconds {
				for _, container := range info.Pod.Spec.Containers {
					missingCPUUtilMillis += PredictUtilisation(&container)
				}
				missingCPUUtilMillis += info.Pod.Spec.Overhead.Cpu().MilliValue()
				klog.V(6).Infof("missing utilisation for pod %v : %v", info.Pod.Name, missingCPUUtilMillis)
			}
		}
		klog.V(6).Infof("missing utilisation for node %v : %v", nodeName, missingCPUUtilMillis)
	}

	var predictedCPUUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage) + float64(missingCPUUtilMillis)) / nodeCPUCapMillis
	}
	if predictedCPUUsage > float64(hostTargetUtilizationPercent) {
		if predictedCPUUsage > 100 {
			return framework.MinNodeScore, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := int64(math.Round(50 * (100 - predictedCPUUsage) / (100 - float64(hostTargetUtilizationPercent))))
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	score := int64(math.Round((100-float64(hostTargetUtilizationPercent))*
		predictedCPUUsage/float64(hostTargetUtilizationPercent) + float64(hostTargetUtilizationPercent)))
	klog.V(6).Infof("score for host %v: %v", nodeName, score)
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *TargetLoadPacking) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *TargetLoadPacking) NormalizeScore(ctx context.Context, state *framework.CycleState, pod *v1.Pod, scores framework.NodeScoreList) *framework.Status {
	return nil
}

// Predict utilisation for a container based on its requests/limits
func PredictUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
		return container.Resources.Limits.Cpu().MilliValue()
	} else if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Cpu().MilliValue()) * requestMultiplier))
	} else {
		return requestsMilliCores
	}
}
