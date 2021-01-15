package loadvariationriskbalancing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/francoispqt/gojay"
	"github.com/paypal/load-watcher/pkg/watcher"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/config/v1beta1"
	"sigs.k8s.io/scheduler-plugins/pkg/trimaran"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	pluginConfig "sigs.k8s.io/scheduler-plugins/pkg/apis/config"
)

const (
	// Time interval in seconds for each metrics agent ingestion.
	metricsAgentReportingIntervalSeconds = 60
	httpClientTimeoutSeconds             = 55 * time.Second
	metricsUpdateIntervalSeconds         = 30
	Name                                 = "Loadvariationriskbalancing"
)

var (
	safeVarianceMargin = v1beta1.DefaultSafeVarianceMargin
	watcherAddress     = "http://127.0.0.1:2020"
	// WatcherBaseUrl Exported for testing
	WatcherBaseUrl = "/variation"
)

// Loadvariationriskbalancing plugin struct
type Loadvariationriskbalancing struct {
	handle       framework.FrameworkHandle
	client       http.Client
	metrics      watcher.WatcherMetrics
	eventHandler *trimaran.PodAssignEventHandler
	// For safe access to metrics
	mu sync.RWMutex
}

// New the Loadvariationriskbalancing constructor
func New(obj runtime.Object, handle framework.FrameworkHandle) (framework.Plugin, error) {
	args, err := getArgs(obj)
	if err != nil {
		return nil, err
	}
	safeVarianceMargin = args.SafeVarianceMargin
	watcherAddress = args.WatcherAddress

	podAssignEventHandler := trimaran.New()
	pl := &Loadvariationriskbalancing{
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

func (pl *Loadvariationriskbalancing) updateMetrics() error {
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

func (pl *Loadvariationriskbalancing) Score(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
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
		curPodCPUUsage += container.Resources.Limits.Cpu().MilliValue()
		curPodMemoryUsage += container.Resources.Limits.Memory().MilliValue()
	}
	klog.V(6).Infof("cpu predicted utilisation for pod %v: %v", pod.Name, curPodCPUUsage)
	klog.V(6).Infof("memory predicted utilisation for pod %v: %v", pod.Name, curPodMemoryUsage)
	if pod.Spec.Overhead != nil {
		curPodCPUUsage += pod.Spec.Overhead.Cpu().MilliValue()
		curPodMemoryUsage += pod.Spec.Overhead.Memory().MilliValue()
	}

	nodeCPUCapMillis := float64(nodeInfo.Node().Status.Capacity.Cpu().MilliValue())
	nodeCPUUtilPercent := float64(curPodCPUUsage) / float64(nodeCPUCapMillis)

	nodeMemoryCapMillis := float64(nodeInfo.Node().Status.Capacity.Memory().MilliValue())
	nodeMemoryUtilPercent := float64(curPodMemoryUsage) / float64(nodeMemoryCapMillis)

	klog.V(6).Infof("curPod %v CPU Utilisation: %v, Capacity: %v", nodeName, nodeCPUUtilPercent, nodeCPUCapMillis)
	klog.V(6).Infof("curPod %v Memory Utilisation: %v, Capacity: %v", nodeName, nodeMemoryUtilPercent, nodeMemoryCapMillis)

	type statistics struct {
		AVG float64
		STD float64
	}

	var rs = make(map[v1.ResourceName]statistics)
	for _, metric := range metrics.Data.NodeMetricsMap[nodeName].Metrics {
		if metric.Type == watcher.CPU {
			if metric.Rollup == "AVG" {
				rs[v1.ResourceCPU] = statistics{
					AVG: metric.Value,
					STD: rs[v1.ResourceCPU].STD,
				}
			}
			if metric.Rollup == "STD" {
				rs[v1.ResourceCPU] = statistics{
					AVG: rs[v1.ResourceCPU].AVG,
					STD: metric.Value,
				}
			}
		}
		if metric.Type == watcher.Memory {
			if metric.Rollup == "AVG" {
				rs[v1.ResourceMemory] = statistics{
					AVG: metric.Value,
					STD: rs[v1.ResourceMemory].STD,
				}
			}
			if metric.Rollup == "STD" {
				rs[v1.ResourceMemory] = statistics{
					AVG: rs[v1.ResourceMemory].AVG,
					STD: metric.Value,
				}
			}
		}

	}

	if rs[v1.ResourceCPU].AVG+rs[v1.ResourceCPU].STD*float64(safeVarianceMargin) > 1.0 || rs[v1.ResourceMemory].AVG+rs[v1.ResourceMemory].STD*float64(safeVarianceMargin) > 1.0 {
		klog.V(6).Infof("node has full with memory or cpu")
		return framework.MinNodeScore, nil
	}

	score := make([]float64, 2)
	score[0] = math.Min(1.0, nodeCPUUtilPercent+rs[v1.ResourceCPU].AVG+rs[v1.ResourceCPU].STD)
	score[1] = math.Min(1.0, nodeMemoryUtilPercent+rs[v1.ResourceMemory].AVG+rs[v1.ResourceMemory].STD)

	klog.V(6).Infof("cur node: %v, score list: %v", nodeName, score)
	finalScore := math.Min(1-score[0], 1-score[1]) * float64(framework.MaxNodeScore)

	klog.V(6).Infof("final score %v", finalScore)
	return int64(finalScore), framework.NewStatus(framework.Success, "")
}

// Name return Loadvariationriskbalancing name
func (pl *Loadvariationriskbalancing) Name() string {
	return Name
}

func getArgs(obj runtime.Object) (*pluginConfig.LoadVariationRiskBalancingArgs, error) {
	loadVariationRiskBalancingArgs, ok := obj.(*pluginConfig.LoadVariationRiskBalancingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type TargetLoadPackingArgs, got %T", obj)
	}
	if loadVariationRiskBalancingArgs.WatcherAddress == "" {
		return nil, errors.New("no watcher address configured")
	}

	return loadVariationRiskBalancingArgs, nil
}

func (pl *Loadvariationriskbalancing) ScoreExtensions() framework.ScoreExtensions {
	return pl
}

func (pl *Loadvariationriskbalancing) NormalizeScore(context.Context, *framework.CycleState, *v1.Pod, framework.NodeScoreList) *framework.Status {
	return nil
}

// Checks and returns true if the pod is assigned to a node
func isAssigned(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}
