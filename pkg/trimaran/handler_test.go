package trimaran

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestHandlerCacheCleanup(t *testing.T) {
	testNode := "node-1"
	pod1 := st.MakePod().Name("Pod-1").Obj()
	pod2 := st.MakePod().Name("Pod-2").Obj()
	pod3 := st.MakePod().Name("Pod-3").Obj()
	p := New()
	// Test OnUpdate doesn't add unassigned pods
	p.ScheduledPodsCache[testNode] = append(p.ScheduledPodsCache[testNode], podInfo{Pod: pod1}, podInfo{Pod: pod2},
		podInfo{Pod: pod3})
	pod4 := st.MakePod().Name("Pod-4").Obj()
	pod4.Spec.NodeName = testNode
	pod4old := st.MakePod().Name("Pod-4").Obj()
	p.OnUpdate(pod4old, pod4)
	p.cleanupCache()
	assert.NotNil(t, p.ScheduledPodsCache[testNode])
	assert.Equal(t, 1, len(p.ScheduledPodsCache[testNode]))
	assert.Equal(t, pod4, p.ScheduledPodsCache[testNode][0].Pod)
	// Test cleanupCache doesn't delete newly added pods
	p.ScheduledPodsCache[testNode] = nil
	p.ScheduledPodsCache[testNode] = append(p.ScheduledPodsCache[testNode], podInfo{Pod: pod1}, podInfo{Pod: pod2},
		podInfo{Pod: pod3}, podInfo{Timestamp: time.Now().Unix(), Pod: pod4})
	pod5 := st.MakePod().Name("Pod-5").Obj()
	pod5.Spec.NodeName = testNode
	pod5old := st.MakePod().Name("Pod-5").Obj()
	p.OnUpdate(pod5old, pod5)
	p.cleanupCache()
	assert.NotNil(t, p.ScheduledPodsCache[testNode])
	assert.Equal(t, 2, len(p.ScheduledPodsCache[testNode]))
	assert.Equal(t, pod4, p.ScheduledPodsCache[testNode][0].Pod)
	assert.Equal(t, pod5, p.ScheduledPodsCache[testNode][1].Pod)
	// Test cleanupCache deletes old pods
	p.ScheduledPodsCache[testNode] = append(p.ScheduledPodsCache[testNode], podInfo{Timestamp: time.Now().Unix() - 10000, Pod: pod5})
	p.cleanupCache()
	assert.Nil(t, p.ScheduledPodsCache[testNode])
}
