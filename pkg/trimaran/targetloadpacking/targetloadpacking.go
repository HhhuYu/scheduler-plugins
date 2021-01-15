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
	"sync"
	"time"

	"github.com/francoispqt/gojay"
	"github.com/paypal/load-watcher/pkg/watcher"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"

	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config/v1beta1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"
)

const (
	// Time interval in seconds for each metrics agent ingestion.
	metricsAgentReportingIntervalSeconds = 60
	httpClientTimeoutSeconds             = 55 * time.Second
	metricsUpdateIntervalSeconds         = 30
	Name                                 = "TargetLoadPacking"
)

var (
	reqiestsMilliMemory          = v1beta1.DefaultRequestsMilliMemory
	requestsMilliCores           = v1beta1.DefaultRequestsMilliCores
	scoringWeight                = v1beta1.DefaultNodeResourcesScoringWeightMap
	hostTargetUtilizationPercent = v1beta1.DefaultTargetUtilizationPercent
	watcherAddress               = "http://127.0.0.1:2020"
	requestsMultiplier           float64
	// WatcherBaseUrl Exported for testing
	WatcherBaseUrl = "/watcher"
)

// TargetLoadPacking plugin struct
type TargetLoadPacking struct {
	handle       framework.FrameworkHandle
	client       http.Client
	metrics      watcher.WatcherMetrics
	eventHandler *trimaran.PodAssignEventHandler
	// For safe access to metrics
	mu sync.RWMutex
}

// New TargetLoadPacking plugin new
func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	hostTargetUtilizationPercent = args.TargetUtilization
	// requestsMilliCores = 1000
	watcherAddress = args.WatcherAddress
	scoringWeight = args.WeightMap
	requestsMultiplier, _ = strconv.ParseFloat("1.0", 64)

	podAssignEventHandler := trimaran.New()
	pl := &TargetLoadPacking{
		handle: handle,
		client: http.Client{
			Timeout: httpClientTimeoutSeconds,
		},
		eventHandler: podAssignEventHandler,
	}

	pl.handle.SharedInformerFactory().Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					return isAssigned(t)
				case cache.DeletedFinalStateUnknown:
					if pod, ok := t.Obj.(*v1.Pod); ok {
						return isAssigned(pod)
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, pl))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", pl, obj))
					return false
				}
			},
			Handler: podAssignEventHandler,
		},
	)

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

	//TODO(aqadeer): Add a couple of retries for transient errors
	resp, err := pl.client.Do(req)
	if err != nil {
		klog.Errorf("request to watcher failed: %v", err)
		// Reset the metrics to avoid stale metrics. Probably use a timestamp for better control
		pl.mu.Lock()
		pl.metrics = watcher.WatcherMetrics{}
		pl.mu.Unlock()
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
		pl.mu.Lock()
		pl.metrics = metrics
		pl.mu.Unlock()
	} else {
		klog.Errorf("received status code %v from watcher", resp.StatusCode)
	}
	return nil
}

// Name return TargetLoadPacking name
func (pl *TargetLoadPacking) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (*pluginConfig.TargetLoadPackingArgs, error) {
	targetLoadPackingArgs, ok := obj.(*pluginConfig.TargetLoadPackingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type TargetLoadPackingArgs, got %T", obj)
	}
	if targetLoadPackingArgs.WatcherAddress == "" {
		return nil, errors.New("no watcher address configured")
	}
	_, err := strconv.ParseFloat("1.8", 64)
	if err != nil {
		return nil, errors.New("unable to parse DefaultRequestsMultiplier: " + err.Error())
	}
	return targetLoadPackingArgs, nil
}

func (pl *TargetLoadPacking) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return framework.MinNodeScore, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	// copy reference lest updateMetrics() updates the value and to avoid locking for rest of the function
	pl.mu.RLock()
	metrics := pl.metrics
	pl.mu.RUnlock()
	// This means the node is new (no metrics yet) or metrics are unavailable due to 404 or 500
	if _, ok := metrics.Data.NodeMetricsMap[nodeName]; !ok {
		klog.V(6).Infof("unable to find metrics for node %v", nodeName)
		// Avoid the node by scoring minimum
		return framework.MinNodeScore, nil
		//TODO(aqadeer): If this happens for a long time, fall back to allocation based packing. This could mean maintaining failure state across cycles if scheduler doesn't provide this state
	}

	var curPodCPUUsage, curPodMemoryUsage int64
	for _, container := range pod.Spec.Containers {
		curPodCPUUsage += cpuPredictUtilisation(&container)
		curPodMemoryUsage += memoryPredictUtilisation(&container)
	}
	klog.V(6).Infof("cpu predicted utilisation for pod %v: %v", pod.Name, curPodCPUUsage)
	klog.V(6).Infof("memory predicted utilisation for pod %v: %v", pod.Name, curPodMemoryUsage)
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
		curPodMemoryUsage += pod.Spec.Overhead.Memory().MilliValue()
	}

	var nodeCPUUtilPercent, nodeMemoryUtilPercent float64
	var cpuMetricFound, memoryMetricFound bool
	for _, metric := range metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			nodeCPUUtilPercent = metric.Value
			cpuMetricFound = true
		}
		if metric.Type == watcher.Memory {
			nodeMemoryUtilPercent = metric.Value
			memoryMetricFound = true
		}
	}

	if !cpuMetricFound {
		klog.Errorf("cpu metric not found for node %v in node metrics %v", nodeName, metrics.Data.NodeMetricsMap[nodeName].Metrics)
		return framework.MinNodeScore, nil
	}
	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilMillis := (nodeCPUUtilPercent / 100) * nodeCPUCapMillis

	if !memoryMetricFound {
		klog.Errorf("memory metric not found for node %v in node metrics %v", nodeName, metrics.Data.NodeMetricsMap[nodeName].Metrics)
		return framework.MinNodeScore, nil
	}
	nodeMemoryCapMillis := float64(nodeInfo.Node().Status.Capacity.Memory().MilliValue())
	nodeMemoryUtilMillis := (nodeMemoryUtilPercent / 100) * nodeMemoryCapMillis

	klog.V(6).Infof("node %v CPU Utilisation (millicores): %v, Capacity: %v", nodeName, nodeCPUUtilMillis, nodeCPUCapMillis)
	klog.V(6).Infof("node %v Memory Utilisation (millicores): %v, Capacity: %v", nodeName, nodeMemoryUtilMillis, nodeMemoryCapMillis)

	var missingCPUUtilMillis, missingMemoryUtilMillis int64
	pl.eventHandler.RLock()
	for _, info := range pl.eventHandler.ScheduledPodsCache[nodeName] {
		// If the time stamp of the scheduled pod is outside fetched metrics window, or it is within metrics reporting interval seconds, we predict util.
		// Note that the second condition doesn't guarantee metrics for that pod are not reported yet as the 0 <= t <= 2*metricsAgentReportingIntervalSeconds
		// t = metricsAgentReportingIntervalSeconds is taken as average case and it doesn't hurt us much if we are
		// counting metrics twice in case actual t is less than metricsAgentReportingIntervalSeconds
		if info.Timestamp.Unix() > metrics.Window.End || info.Timestamp.Unix() <= metrics.Window.End &&
			(metrics.Window.End-info.Timestamp.Unix()) < metricsAgentReportingIntervalSeconds {
			for _, container := range info.Pod.Spec.Containers {
				missingCPUUtilMillis += cpuPredictUtilisation(&container)
				missingMemoryUtilMillis += memoryPredictUtilisation(&container)
			}
			missingCPUUtilMillis += info.Pod.Spec.Overhead.Cpu().MilliValue()
			missingMemoryUtilMillis += info.Pod.Spec.Overhead.Memory().MilliValue()
			klog.V(6).Infof("cpu missing utilisation for pod %v : %v", info.Pod.Name, missingCPUUtilMillis)
			klog.V(6).Infof("memory missing utilisation for pod %v : %v", info.Pod.Name, missingMemoryUtilMillis)
		}
	}
	pl.eventHandler.RUnlock()
	klog.V(6).Infof("cpu missing utilisation for node %v : %v", nodeName, missingCPUUtilMillis)
	klog.V(6).Infof("memory missing utilisation for node %v : %v", nodeName, missingMemoryUtilMillis)
	var predictedCPUUsage, predictedMemoryUsage float64
	if nodeCPUCapMillis != 0 {
		predictedCPUUsage = 100 * (nodeCPUUtilMillis + float64(curPodCPUUsage) + float64(missingCPUUtilMillis)) / nodeCPUCapMillis
	}
	if nodeMemoryCapMillis != 0 {
		predictedMemoryUsage = 100 * (nodeMemoryUtilMillis + float64(curPodMemoryUsage) + float64(missingMemoryUtilMillis)) / nodeMemoryCapMillis
	}
	if predictedCPUUsage > float64(hostTargetUtilizationPercent[v1.ResourceCPU]) {
		if predictedCPUUsage > 100 {
			return framework.MinNodeScore, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := int64(math.Round(50 * (100 - predictedCPUUsage) / (100 - float64(hostTargetUtilizationPercent[v1.ResourceCPU]))))
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	if predictedMemoryUsage > float64(hostTargetUtilizationPercent[v1.ResourceMemory]) {
		if predictedMemoryUsage > 100 {
			return framework.MinNodeScore, framework.NewStatus(framework.Success, "")
		}
		penalisedScore := int64(math.Round(50 * (100 - predictedMemoryUsage) / (100 - float64(hostTargetUtilizationPercent[v1.ResourceMemory]))))
		klog.V(6).Infof("penalised score for host %v: %v", nodeName, penalisedScore)
		return penalisedScore, framework.NewStatus(framework.Success, "")
	}

	cpuScore := int64(math.Round((100-float64(hostTargetUtilizationPercent[v1.ResourceCPU]))*
		predictedCPUUsage/float64(hostTargetUtilizationPercent[v1.ResourceCPU]) + float64(hostTargetUtilizationPercent[v1.ResourceCPU])))
	klog.V(6).Infof("cpu score for host %v: %v", nodeName, cpuScore)

	memoryScore := int64(math.Round((100-float64(hostTargetUtilizationPercent[v1.ResourceMemory]))*
		predictedMemoryUsage/float64(hostTargetUtilizationPercent[v1.ResourceMemory]) + float64(hostTargetUtilizationPercent[v1.ResourceMemory])))
	klog.V(6).Infof("memory score for host %v: %v", nodeName, memoryScore)

	score := int64(float64(scoringWeight[v1.ResourceCPU])*float64(cpuScore)/100 + float64(scoringWeight[v1.ResourceMemory])*float64(memoryScore)/100)
	klog.V(6).Infof("score for host %v: %v", nodeName, score)
	return score, framework.NewStatus(framework.Success, "")
}

func (pl *TargetLoadPacking) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *TargetLoadPacking) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// Checks and returns true if the pod is assigned to a node
func isAssigned(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}

// Predict utilisation for a container based on its requests/limits
func cpuPredictUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
		return container.Resources.Limits.Cpu().MilliValue()
	} else if _, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Cpu().MilliValue()) * requestsMultiplier))
	} else {
		return requestsMilliCores
	}
}

func memoryPredictUtilisation(container *v1.Container) int64 {
	if _, ok := container.Resources.Limits[v1.ResourceMemory]; ok {
		return container.Resources.Limits.Memory().MilliValue()
	} else if _, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
		return int64(math.Round(float64(container.Resources.Requests.Memory().MilliValue()) * requestsMultiplier))
	} else {
		return reqiestsMilliMemory
	}
}
