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
Package Trimaran provides common code for plugins developed for real load aware scheduling like TargetLoadPacking etc.
*/
package trimaran

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	cacheCleanupIntervalMinutes          = 5  // This is the maximum staleness of metrics possible by load watcher
	metricsAgentReportingIntervalSeconds = 60 // Time interval in seconds for each metrics agent ingestion.
)

var _ clientcache.ResourceEventHandler = &PodAssignEventHandler{}

// This event handler listens to a pod's spec.NodeName assign events after successful binding
type PodAssignEventHandler struct {
	ScheduledPodsCache map[string][]podInfo // Maintains the node-name to podInfo mapping for pods successfully bound to nodes
	sync.RWMutex
}

// Stores Timestamp and Pod spec info object
type podInfo struct {
	Timestamp int64 // This timestamp is initialised when adding it to ScheduledPodsCache after successful binding
	Pod       *v1.Pod
}

// Returns a new instance of PodAssignEventHandler, after starting a background go routine for cache cleanup
func New() *PodAssignEventHandler {
	p := PodAssignEventHandler{ScheduledPodsCache: make(map[string][]podInfo)}
	go func() {
		cacheCleanerTicker := time.NewTicker(time.Minute * cacheCleanupIntervalMinutes)
		for range cacheCleanerTicker.C {
			p.cleanupCache()
		}
	}()
	return &p
}

func (p *PodAssignEventHandler) OnAdd(obj interface{}) {
	// Do nothing as newly added pods aren't assigned Spec.NodeName right away
}

func (p *PodAssignEventHandler) OnUpdate(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("dropping OnUpdate event: can't decode old object %#v", oldObj))
		return
	}
	newPod, ok := newObj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("dropping OnUpdate event: can't decode new object %#v", newObj))
		return
	}
	if oldPod.Spec.NodeName != newPod.Spec.NodeName && newPod.Spec.NodeName != "" {
		p.updateCache(newPod, newPod.Spec.NodeName)
	}
}

func (p *PodAssignEventHandler) OnDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("dropping OnDelete event: can't decode old object %#v", obj))
		return
	}
	nodeName := pod.Spec.NodeName
	if nodeName != "" {
		p.Lock()
		defer p.Unlock()
		if _, ok := p.ScheduledPodsCache[nodeName]; ok {
			for i, v := range p.ScheduledPodsCache[nodeName] {
				n := len(p.ScheduledPodsCache[nodeName])
				if pod.ObjectMeta.UID == v.Pod.ObjectMeta.UID {
					klog.V(10).Infof("deleting pod %#v", v.Pod)
					copy(p.ScheduledPodsCache[nodeName][i:], p.ScheduledPodsCache[nodeName][i+1:])
					p.ScheduledPodsCache[nodeName][n-1] = podInfo{}
					p.ScheduledPodsCache[nodeName] = p.ScheduledPodsCache[nodeName][:n-1]
					break
				}
			}
		}
	}
}

func (p *PodAssignEventHandler) updateCache(pod *v1.Pod, nodeName string) {
	if nodeName == "" {
		return
	}
	p.Lock()
	defer p.Unlock()
	p.ScheduledPodsCache[nodeName] = append(p.ScheduledPodsCache[nodeName],
		podInfo{Timestamp: time.Now().Unix(), Pod: pod})
}

func (p *PodAssignEventHandler) cleanupCache() {
	p.Lock()
	defer p.Unlock()
	for nodeName := range p.ScheduledPodsCache {
		cache := p.ScheduledPodsCache[nodeName]
		curTime := time.Now().Unix()
		for i := len(cache) - 1; i >= 0; i-- {
			if curTime-cache[i].Timestamp > metricsAgentReportingIntervalSeconds {
				n := copy(cache, cache[i+1:])
				for j := n; j < len(cache); j++ {
					cache[j] = podInfo{}
				}
				cache = cache[:n]
				break
			}
		}
		if len(cache) == 0 {
			delete(p.ScheduledPodsCache, nodeName)
		} else {
			p.ScheduledPodsCache[nodeName] = cache
		}
	}
}
