package engine

import (
	"sync"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func cacheTestMapping(ns, name string, gen int64, labels map[string]string, asgID string) *v1alpha1.PodASGMapping {
	return &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  ns,
			Name:       name,
			Generation: gen,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: labels},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgID},
					},
				},
			},
		},
	}
}

func cacheTestPod(ns, name string, labels map[string]string, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    labels,
		},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

// ---------------------------------------------------------------------------
// TestCache_IncrementalAdd_MultiRuleMultiTarget_OnlyMatchedTargetsUpdate
// Multi-rule mapping with different selectors pointing to different ASGs.
// OnPodAdd must only add the pod's IP to targets whose rules match the pod.
// This fails with the naive "apply to all targets" implementation.
// ---------------------------------------------------------------------------
func TestCache_IncrementalAdd_MultiRuleMultiTarget_OnlyMatchedTargetsUpdate(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID1 := makeASGResourceID("sub1", "rg1", "asg-frontend")
	asgID2 := makeASGResourceID("sub1", "rg1", "asg-backend")

	// Mapping with 2 rules, each selecting different labels → different ASG targets
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "multi-rule-mapping",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"role": "frontend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID1}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"role": "backend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID2}},
				},
			},
		},
	}

	targetFrontend := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-frontend",
		FullResourceID: asgID1,
		PrefixSetName:  "test-cluster-default-multi-rule-mapping",
	}
	targetBackend := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-backend",
		FullResourceID: asgID2,
		PrefixSetName:  "test-cluster-default-multi-rule-mapping",
	}

	// Seed cache with one frontend pod already in place
	desired := map[ASGTarget]DesiredPrefixSet{
		targetFrontend: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		targetBackend:  {IPs: map[string]struct{}{"10.0.1.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "fe-pod-1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "be-pod-1"}: {PodIP: "10.0.1.1"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1, 1}, false)

	// Add a new BACKEND pod — should only appear in asg-backend target
	newBackendPod := cacheTestPod("default", "be-pod-2", map[string]string{"role": "backend"}, "10.0.1.2")
	cache.OnPodAdd(mapping, newBackendPod)

	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodAdd")
	}

	// Backend target should have the new IP
	backendDPS, exists := got.Desired[targetBackend]
	if !exists {
		t.Fatal("expected backend target in desired state")
	}
	if _, hasNewIP := backendDPS.IPs["10.0.1.2/32"]; !hasNewIP {
		t.Errorf("OnPodAdd should add backend pod IP to backend target; got IPs: %v", backendDPS.IPs)
	}

	// Frontend target should NOT have the backend pod's IP (selector-scoped)
	frontendDPS, exists := got.Desired[targetFrontend]
	if !exists {
		t.Fatal("expected frontend target in desired state")
	}
	if _, hasBackendIP := frontendDPS.IPs["10.0.1.2/32"]; hasBackendIP {
		t.Errorf("OnPodAdd must NOT add pod IP to non-matching target (selector-correct); frontend IPs: %v", frontendDPS.IPs)
	}
	// Frontend should still have its original IP
	if _, hasOrigIP := frontendDPS.IPs["10.0.0.1/32"]; !hasOrigIP {
		t.Errorf("OnPodAdd should preserve existing IPs in non-matching target; frontend IPs: %v", frontendDPS.IPs)
	}
}

// ---------------------------------------------------------------------------
// TestCache_SharedIPContributor_NoPrematureDelete
// Two pods contribute the same IP to a target. Deleting one pod must NOT
// remove the IP since the other pod still contributes it.
// ---------------------------------------------------------------------------
func TestCache_SharedIPContributor_NoPrematureDelete(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Two pods with the SAME IP (e.g., host-network pods on the same node)
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-a"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "pod-b"}: {PodIP: "10.0.0.1"},
	}}
	pods := []corev1.Pod{
		*cacheTestPod("default", "pod-a", map[string]string{"app": "web"}, "10.0.0.1"),
		*cacheTestPod("default", "pod-b", map[string]string{"app": "web"}, "10.0.0.1"),
	}
	cache.SetFromRecompute(mapping, pods, desired, snapshot, []int{2}, false)

	// Delete pod-a — pod-b still contributes 10.0.0.1
	deletedPod := cacheTestPod("default", "pod-a", map[string]string{"app": "web"}, "10.0.0.1")
	cache.OnPodDelete(mapping, deletedPod)

	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodDelete")
	}

	dps := got.Desired[target]
	if _, hasIP := dps.IPs["10.0.0.1/32"]; !hasIP {
		t.Errorf("OnPodDelete must NOT remove shared IP when another contributor exists; got IPs: %v", dps.IPs)
	}
}

// ---------------------------------------------------------------------------
// TestCache_PodAdd_UpdatesSnapshotAndPendingStatus
// OnPodAdd must update the Snapshot to include the new pod and recalculate
// pending-IP status for reconcile parity.
// ---------------------------------------------------------------------------
func TestCache_PodAdd_UpdatesSnapshotAndPendingStatus(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed with one pod
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1}, false)

	// Add a pod with IP
	newPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
	cache.OnPodAdd(mapping, newPod)

	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodAdd")
	}

	// Snapshot should include the new pod
	newPodID := PodIdentity{Namespace: "default", Name: "pod-2"}
	if _, inSnapshot := got.Snapshot.Pods[newPodID]; !inSnapshot {
		t.Errorf("OnPodAdd should add new pod to Snapshot; got Pods: %v", got.Snapshot.Pods)
	}

	// MatchedPodsByIndex should be updated (incremented for matching rule)
	if len(got.MatchedPodsByIndex) == 0 || got.MatchedPodsByIndex[0] < 2 {
		t.Errorf("OnPodAdd should update MatchedPodsByIndex; got: %v, want at least [2]", got.MatchedPodsByIndex)
	}
}

// ---------------------------------------------------------------------------
// TestCache_PodDelete_UpdatesSnapshotAndMatchedCounts
// OnPodDelete must update the Snapshot (remove pod) and adjust matched counts.
// ---------------------------------------------------------------------------
func TestCache_PodDelete_UpdatesSnapshotAndMatchedCounts(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed with two pods
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{
			"10.0.0.1/32": {},
			"10.0.0.2/32": {},
		}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "pod-2"}: {PodIP: "10.0.0.2"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{2}, false)

	// Delete pod-2
	deletedPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
	cache.OnPodDelete(mapping, deletedPod)

	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodDelete")
	}

	// Snapshot should no longer include the deleted pod
	deletedPodID := PodIdentity{Namespace: "default", Name: "pod-2"}
	if _, inSnapshot := got.Snapshot.Pods[deletedPodID]; inSnapshot {
		t.Errorf("OnPodDelete should remove pod from Snapshot; got Pods: %v", got.Snapshot.Pods)
	}

	// Remaining pod should still be in snapshot
	remainingPodID := PodIdentity{Namespace: "default", Name: "pod-1"}
	if _, inSnapshot := got.Snapshot.Pods[remainingPodID]; !inSnapshot {
		t.Errorf("OnPodDelete should preserve other pods in Snapshot; got Pods: %v", got.Snapshot.Pods)
	}

	// MatchedPodsByIndex should be decremented
	if len(got.MatchedPodsByIndex) == 0 || got.MatchedPodsByIndex[0] != 1 {
		t.Errorf("OnPodDelete should decrement MatchedPodsByIndex; got: %v, want [1]", got.MatchedPodsByIndex)
	}
}

// ---------------------------------------------------------------------------
// TestCache_IncrementalAdd
// After SetFromRecompute, OnPodAdd should update the cached desired state to
// include the new pod's IP for the matched mapping targets.
// ---------------------------------------------------------------------------
func TestCache_IncrementalAdd(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache with one pod
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1}, false)

	// Add a new pod
	newPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
	cache.OnPodAdd(mapping, newPod)

	// Verify the new pod's IP is in the cached desired state
	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodAdd")
	}

	dps, exists := got.Desired[target]
	if !exists {
		t.Fatal("expected target in cached desired state")
	}

	if _, hasNewIP := dps.IPs["10.0.0.2/32"]; !hasNewIP {
		t.Errorf("OnPodAdd should add new pod IP to desired state; got IPs: %v", dps.IPs)
	}
	if _, hasOldIP := dps.IPs["10.0.0.1/32"]; !hasOldIP {
		t.Errorf("OnPodAdd should preserve existing IPs; got IPs: %v", dps.IPs)
	}
}

// ---------------------------------------------------------------------------
// TestCache_PodDelete
// OnPodDelete should remove the pod's IP from the cached desired state.
// ---------------------------------------------------------------------------
func TestCache_PodDelete(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache with two pods
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{
			"10.0.0.1/32": {},
			"10.0.0.2/32": {},
		}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "pod-2"}: {PodIP: "10.0.0.2"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{2}, false)

	// Delete pod-2
	deletedPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
	cache.OnPodDelete(mapping, deletedPod)

	// Verify pod-2's IP is removed
	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit after OnPodDelete")
	}

	dps, exists := got.Desired[target]
	if !exists {
		t.Fatal("expected target in cached desired state")
	}

	if _, hasDeletedIP := dps.IPs["10.0.0.2/32"]; hasDeletedIP {
		t.Errorf("OnPodDelete should remove deleted pod's IP; got IPs: %v", dps.IPs)
	}
	if _, hasRemainingIP := dps.IPs["10.0.0.1/32"]; !hasRemainingIP {
		t.Errorf("OnPodDelete should preserve other pods' IPs; got IPs: %v", dps.IPs)
	}
}

// ---------------------------------------------------------------------------
// TestCache_PodIPChange
// OnPodUpdate should atomically replace the old IP with the new IP.
// ---------------------------------------------------------------------------
func TestCache_PodIPChange(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache with pod at old IP
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1}, false)

	// Pod IP changes
	oldPod := cacheTestPod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	newPod := cacheTestPod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.99")

	updated := cache.OnPodUpdate(mapping, oldPod, newPod)

	// Stub returns false → caller must invalidate (proving incremental not yet implemented)
	// When implemented, updated should be true and IPs should change atomically.
	if updated {
		// Once implemented: verify old IP removed, new IP added
		got, ok := cache.Get(mapping)
		if !ok {
			t.Fatal("expected cache hit after successful OnPodUpdate")
		}
		dps := got.Desired[target]
		if _, hasOld := dps.IPs["10.0.0.1/32"]; hasOld {
			t.Errorf("OnPodUpdate should remove old IP; got IPs: %v", dps.IPs)
		}
		if _, hasNew := dps.IPs["10.0.0.99/32"]; !hasNew {
			t.Errorf("OnPodUpdate should add new IP; got IPs: %v", dps.IPs)
		}
	} else {
		// Stub behavior: OnPodUpdate returns false when mutation cannot be applied safely.
		// The design requires the caller to invalidate. Verify the cache entry is still
		// present (not auto-invalidated - caller is responsible).
		t.Errorf("OnPodUpdate returned false (stub behavior), expected true for safe IP change mutation")
	}
}

// ---------------------------------------------------------------------------
// TestCache_Invalidate
// Invalidate removes the cached entry for a specific mapping key.
// ---------------------------------------------------------------------------
func TestCache_Invalidate(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1", PrefixSetName: "test-cluster-default-web-mapping"}: {
			IPs: map[string]struct{}{"10.0.0.1/32": {}},
		},
	}
	cache.SetFromRecompute(mapping, nil, desired, PodSnapshot{}, []int{1}, false)

	// Verify entry exists
	if _, ok := cache.Get(mapping); !ok {
		t.Fatal("expected cache entry before invalidation")
	}

	// Invalidate
	cache.Invalidate(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	// Verify entry removed
	if _, ok := cache.Get(mapping); ok {
		t.Error("expected cache miss after Invalidate")
	}
}

// ---------------------------------------------------------------------------
// TestCache_ConcurrentAccess
// Verify cache is safe under concurrent read/write/invalidation.
// ---------------------------------------------------------------------------
func TestCache_ConcurrentAccess(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1", PrefixSetName: "test-cluster-default-web-mapping"}: {
			IPs: map[string]struct{}{"10.0.0.1/32": {}},
		},
	}

	var wg sync.WaitGroup
	iterations := 100

	// Concurrent writers
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cache.SetFromRecompute(mapping, nil, desired, PodSnapshot{}, []int{1}, false)
		}
	}()

	// Concurrent readers
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cache.Get(mapping)
		}
	}()

	// Concurrent invalidators
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cache.Invalidate(types.NamespacedName{Namespace: "default", Name: "web-mapping"})
		}
	}()

	// Concurrent namespace invalidators
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cache.InvalidateNamespace("default")
		}
	}()

	// Concurrent pod operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		pod := cacheTestPod("default", "pod-x", map[string]string{"app": "web"}, "10.0.0.99")
		for i := 0; i < iterations; i++ {
			cache.OnPodAdd(mapping, pod)
			cache.OnPodDelete(mapping, pod)
		}
	}()

	wg.Wait()
	// No panic or race detected = pass (run with -race flag)
}

// ---------------------------------------------------------------------------
// TestCache_OutputParity_MatchedPodsPendingSnapshot
// Cached output (from SetFromRecompute) must include correct MatchedPodsByIndex,
// HasPendingIPPods, and Snapshot data.
// ---------------------------------------------------------------------------
func TestCache_OutputParity_MatchedPodsPendingSnapshot(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "pod-2"}: {PodIP: ""},
	}}
	matchedPods := []int{3}

	cache.SetFromRecompute(mapping, nil, desired, snapshot, matchedPods, true)

	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit")
	}

	// Verify MatchedPodsByIndex
	if len(got.MatchedPodsByIndex) != 1 || got.MatchedPodsByIndex[0] != 3 {
		t.Errorf("MatchedPodsByIndex = %v, want [3]", got.MatchedPodsByIndex)
	}

	// Verify HasPendingIPPods
	if !got.HasPendingIPPods {
		t.Error("HasPendingIPPods = false, want true")
	}

	// Verify Snapshot
	if len(got.Snapshot.Pods) != 2 {
		t.Errorf("Snapshot.Pods length = %d, want 2", len(got.Snapshot.Pods))
	}

	// Verify deep copy isolation: mutating returned value doesn't affect cache
	got.Desired[target] = DesiredPrefixSet{IPs: map[string]struct{}{"mutated": {}}}
	got2, _ := cache.Get(mapping)
	if _, hasMutated := got2.Desired[target].IPs["mutated"]; hasMutated {
		t.Error("Get should return deep copies; mutation of returned value affected cache")
	}
}

// ---------------------------------------------------------------------------
// TestCache_GenerationIsolation
// A cache entry for generation N should not be returned when querying generation N+1.
// ---------------------------------------------------------------------------
func TestCache_GenerationIsolation(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")

	mappingGen1 := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)
	mappingGen2 := cacheTestMapping("default", "web-mapping", 2,
		map[string]string{"app": "web-v2"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1", PrefixSetName: "test-cluster-default-web-mapping"}: {
			IPs: map[string]struct{}{"10.0.0.1/32": {}},
		},
	}

	// Store for generation 1
	cache.SetFromRecompute(mappingGen1, nil, desired, PodSnapshot{}, []int{1}, false)

	// Query with generation 2 → should miss
	if _, ok := cache.Get(mappingGen2); ok {
		t.Error("expected cache miss for different generation (gen 1 cached, gen 2 queried)")
	}

	// Query with generation 1 → should hit
	if _, ok := cache.Get(mappingGen1); !ok {
		t.Error("expected cache hit for matching generation")
	}
}

// ---------------------------------------------------------------------------
// TestCache_InvalidateNamespace
// InvalidateNamespace removes all entries in a namespace.
// ---------------------------------------------------------------------------
func TestCache_InvalidateNamespace(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")

	mapping1 := cacheTestMapping("target-ns", "mapping-1", 1,
		map[string]string{"app": "web"}, asgID)
	mapping2 := cacheTestMapping("target-ns", "mapping-2", 1,
		map[string]string{"app": "api"}, asgID)
	mappingOther := cacheTestMapping("other-ns", "mapping-3", 1,
		map[string]string{"app": "db"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}

	cache.SetFromRecompute(mapping1, nil, desired, PodSnapshot{}, []int{1}, false)
	cache.SetFromRecompute(mapping2, nil, desired, PodSnapshot{}, []int{1}, false)
	cache.SetFromRecompute(mappingOther, nil, desired, PodSnapshot{}, []int{1}, false)

	cache.InvalidateNamespace("target-ns")

	if _, ok := cache.Get(mapping1); ok {
		t.Error("expected cache miss for mapping-1 in invalidated namespace")
	}
	if _, ok := cache.Get(mapping2); ok {
		t.Error("expected cache miss for mapping-2 in invalidated namespace")
	}
	if _, ok := cache.Get(mappingOther); !ok {
		t.Error("expected cache hit for mapping-3 in other namespace")
	}
}

// ---------------------------------------------------------------------------
// TestCache_Delete
// Delete removes the cache entry entirely.
// ---------------------------------------------------------------------------
func TestCache_Delete(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	cache.SetFromRecompute(mapping, nil, desired, PodSnapshot{}, []int{1}, false)

	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	if _, ok := cache.Get(mapping); ok {
		t.Error("expected cache miss after Delete")
	}
}

// ---------------------------------------------------------------------------
// Phase 5 Lifecycle Fence Tests
// ---------------------------------------------------------------------------

// TestCache_GetWithVersion_MissReturnsVersionAndLifecycleFence verifies that
// GetWithVersion returns fence values even on cache miss so that callers can
// use them for CAS publish after recompute.
func TestCache_GetWithVersion_MissReturnsVersionAndLifecycleFence(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	_, version, lifecycleEpoch, hit := cache.GetWithVersion(mapping)
	if hit {
		t.Error("expected cache miss on empty cache")
	}
	// On empty cache, version and lifecycleEpoch should be zero (initial state)
	if version != 0 {
		t.Errorf("expected version=0 on initial miss, got %d", version)
	}
	if lifecycleEpoch != 0 {
		t.Errorf("expected lifecycleEpoch=0 on initial miss, got %d", lifecycleEpoch)
	}

	// After a Delete (which bumps lifecycle epoch), GetWithVersion should
	// return the bumped epoch even on cache miss.
	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	_, version2, lifecycleEpoch2, hit2 := cache.GetWithVersion(mapping)
	if hit2 {
		t.Error("expected cache miss after Delete")
	}
	if lifecycleEpoch2 <= lifecycleEpoch {
		t.Errorf("expected lifecycleEpoch to increase after Delete: before=%d, after=%d", lifecycleEpoch, lifecycleEpoch2)
	}
	_ = version2 // version also incremented, but lifecycle epoch is the key fence
}

// TestCache_SetFromRecomputeIfVersion_RejectsStaleAfterPodEventVersionAdvance
// verifies that a recompute publish is rejected when a pod event has advanced
// the version since the recompute started.
func TestCache_SetFromRecomputeIfVersion_RejectsStaleAfterPodEventVersionAdvance(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache with initial state
	initialDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	initialSnapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}
	cache.SetFromRecompute(mapping, nil, initialDesired, initialSnapshot, []int{1}, false)

	// Read current fences (simulates reconcile reading at start)
	_, expectedVersion, expectedLifecycleEpoch, _ := cache.GetWithVersion(mapping)

	// Simulate pod event advancing version mid-recompute
	newPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
	cache.OnPodAdd(mapping, newPod)

	// Attempt CAS publish with stale version — should be rejected
	staleDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}}, // missing pod-2
	}
	staleSnapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}

	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mapping, nil, staleDesired, staleSnapshot, []int{1}, false,
		expectedVersion, expectedLifecycleEpoch,
	)

	if committed {
		t.Error("SetFromRecomputeIfVersion should reject stale recompute after pod event version advance")
	}

	// Verify cache still has pod-2's IP (from OnPodAdd)
	got, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if _, hasIP := got.Desired[target].IPs["10.0.0.2/32"]; !hasIP {
		t.Error("cache should retain pod-2 IP from OnPodAdd; stale recompute must not overwrite")
	}
}

// TestCache_SetFromRecomputeIfVersion_RejectsAfterDeleteLifecycleEpochBump
// verifies that a recompute publish is rejected when Delete bumped the
// lifecycle epoch since the recompute started.
func TestCache_SetFromRecomputeIfVersion_RejectsAfterDeleteLifecycleEpochBump(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache
	initialDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	cache.SetFromRecompute(mapping, nil, initialDesired, PodSnapshot{}, []int{1}, false)

	// Read fences (simulates reconcile start)
	_, expectedVersion, expectedLifecycleEpoch, _ := cache.GetWithVersion(mapping)

	// Terminal cleanup: Delete bumps lifecycle epoch
	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	// Stale recompute attempts to publish
	staleDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}

	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mapping, nil, staleDesired, PodSnapshot{}, []int{1}, false,
		expectedVersion, expectedLifecycleEpoch,
	)

	if committed {
		t.Error("SetFromRecomputeIfVersion should reject publish when lifecycle epoch advanced by Delete")
	}

	// Cache should remain empty after Delete (stale publish must not repopulate)
	if _, ok := cache.Get(mapping); ok {
		t.Error("stale recompute must not repopulate cache after lifecycle epoch bump from Delete")
	}
}

// TestCache_Delete_BumpsLifecycleFence_AndClearsEntries verifies that Delete
// increments the lifecycle epoch fence while clearing all entries for the key.
func TestCache_Delete_BumpsLifecycleFence_AndClearsEntries(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	desired := map[ASGTarget]DesiredPrefixSet{
		{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	cache.SetFromRecompute(mapping, nil, desired, PodSnapshot{}, []int{1}, false)

	// Read fences before delete
	_, _, epochBefore, _ := cache.GetWithVersion(mapping)

	// Delete
	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	// Entry should be gone
	if _, ok := cache.Get(mapping); ok {
		t.Error("expected cache miss after Delete")
	}

	// Lifecycle epoch should be bumped (visible on subsequent GetWithVersion miss)
	_, _, epochAfter, _ := cache.GetWithVersion(mapping)
	if epochAfter <= epochBefore {
		t.Errorf("Delete must bump lifecycle epoch: before=%d, after=%d", epochBefore, epochAfter)
	}
}

// TestCache_DeleteRecreate_SameKeyGenerationReset_StalePublishRejected
// verifies the delete/recreate race: after a Delete and recreation with
// generation reset to 1, a stale in-flight recompute from the old object
// cannot repopulate the cache.
func TestCache_DeleteRecreate_SameKeyGenerationReset_StalePublishRejected(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")

	// Old mapping gen=5
	mappingOld := cacheTestMapping("default", "web-mapping", 5,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	// Seed cache for old mapping
	oldDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.OLD/32": {}}},
	}
	cache.SetFromRecompute(mappingOld, nil, oldDesired, PodSnapshot{}, []int{1}, false)

	// Stale reconcile reads fences (before deletion happens)
	_, staleVersion, staleEpoch, _ := cache.GetWithVersion(mappingOld)

	// Object is deleted (terminal cleanup)
	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	// Object is recreated with generation=1
	mappingNew := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web-v2"}, asgID)

	// Stale recompute from old object tries to publish
	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mappingOld, nil, oldDesired, PodSnapshot{}, []int{1}, false,
		staleVersion, staleEpoch,
	)
	if committed {
		t.Error("stale recompute from old generation must not repopulate cache after delete/recreate")
	}

	// New recompute for the recreated object should succeed
	newDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.NEW/32": {}}},
	}
	_, freshVersion, freshEpoch, _ := cache.GetWithVersion(mappingNew)
	committedNew, _, _ := cache.SetFromRecomputeIfVersion(
		mappingNew, nil, newDesired, PodSnapshot{}, []int{1}, false,
		freshVersion, freshEpoch,
	)
	if !committedNew {
		t.Error("fresh recompute for recreated object should succeed")
	}

	got, ok := cache.Get(mappingNew)
	if !ok {
		t.Fatal("expected cache hit for recreated mapping")
	}
	if _, hasNew := got.Desired[target].IPs["10.0.0.NEW/32"]; !hasNew {
		t.Error("cache should contain new mapping's IP")
	}
	if _, hasOld := got.Desired[target].IPs["10.0.0.OLD/32"]; hasOld {
		t.Error("cache must not contain old mapping's IP after delete/recreate")
	}
}

// TestCache_ConcurrentAccess_GetWithVersion_CASPublish extends concurrency
// tests to cover GetWithVersion + SetFromRecomputeIfVersion contention.
func TestCache_ConcurrentAccess_GetWithVersion_CASPublish(t *testing.T) {
	cache := NewDesiredStateCache("test-cluster")
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)

	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}
	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}

	// Seed initial state
	cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1}, false)

	var wg sync.WaitGroup
	iterations := 100

	// Concurrent GetWithVersion + CAS publish attempts
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, ver, epoch, _ := cache.GetWithVersion(mapping)
			cache.SetFromRecomputeIfVersion(mapping, nil, desired, snapshot, []int{1}, false, ver, epoch)
		}
	}()

	// Concurrent pod events (advance version)
	wg.Add(1)
	go func() {
		defer wg.Done()
		pod := cacheTestPod("default", "pod-x", map[string]string{"app": "web"}, "10.0.0.99")
		for i := 0; i < iterations; i++ {
			cache.OnPodAdd(mapping, pod)
			cache.OnPodDelete(mapping, pod)
		}
	}()

	// Concurrent Delete (advance lifecycle epoch)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})
			// Re-seed so other goroutines can keep operating
			cache.SetFromRecompute(mapping, nil, desired, snapshot, []int{1}, false)
		}
	}()

	wg.Wait()
	// No panic or race detected = pass (run with -race flag)
}

// ===========================================================================
// Phase 5: SetFromRecomputeArtifactsIfVersion Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestCache_SetFromRecomputeArtifactsIfVersion_MatchesLegacySetFromRecomputeIfVersionState
// The artifact-based CAS publish path must produce an identical cache entry
// to the legacy SetFromRecomputeIfVersion for the same inputs.
// ---------------------------------------------------------------------------
func TestCache_SetFromRecomputeArtifactsIfVersion_MatchesLegacySetFromRecomputeIfVersionState(t *testing.T) {
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)
	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	pods := []corev1.Pod{
		*cacheTestPod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
		*cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2"),
	}

	desired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}, "10.0.0.2/32": {}}},
	}
	snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
		{Namespace: "default", Name: "pod-2"}: {PodIP: "10.0.0.2"},
	}}
	matchedPods := []int{2}

	// Legacy path
	cacheLegacy := NewDesiredStateCache("test-cluster")
	_, legacyVer, legacyEpoch, _ := cacheLegacy.GetWithVersion(mapping)
	committedLegacy, _, _ := cacheLegacy.SetFromRecomputeIfVersion(
		mapping, pods, desired, snapshot, matchedPods, false,
		legacyVer, legacyEpoch,
	)
	if !committedLegacy {
		t.Fatal("legacy SetFromRecomputeIfVersion should commit on clean cache")
	}
	legacyState, legacyOk := cacheLegacy.Get(mapping)
	if !legacyOk {
		t.Fatal("expected cache hit after legacy commit")
	}

	// Artifact path
	cacheArtifact := NewDesiredStateCache("test-cluster")
	_, artVer, artEpoch, _ := cacheArtifact.GetWithVersion(mapping)
	artifacts := DesiredStateRecomputeArtifacts{
		Desired:            desired,
		Snapshot:           snapshot,
		MatchedPodsByIndex: matchedPods,
		HasPendingIPPods:   false,
		PodRules: map[PodIdentity]map[int]struct{}{
			{Namespace: "default", Name: "pod-1"}: {0: {}},
			{Namespace: "default", Name: "pod-2"}: {0: {}},
		},
		PendingIPCount: 0,
	}
	committedArt, _, _ := cacheArtifact.SetFromRecomputeArtifactsIfVersion(
		mapping, artifacts, artVer, artEpoch,
	)
	if !committedArt {
		t.Fatal("SetFromRecomputeArtifactsIfVersion should commit on clean cache")
	}
	artState, artOk := cacheArtifact.Get(mapping)
	if !artOk {
		t.Fatal("expected cache hit after artifact commit")
	}

	// Compare states
	if len(legacyState.Desired) != len(artState.Desired) {
		t.Errorf("Desired target count mismatch: legacy=%d, artifact=%d",
			len(legacyState.Desired), len(artState.Desired))
	}
	for tgt, legDPS := range legacyState.Desired {
		artDPS, ok := artState.Desired[tgt]
		if !ok {
			t.Errorf("target %v present in legacy but missing from artifact state", tgt)
			continue
		}
		if len(legDPS.IPs) != len(artDPS.IPs) {
			t.Errorf("IP count mismatch for target %v: legacy=%d, artifact=%d",
				tgt, len(legDPS.IPs), len(artDPS.IPs))
		}
	}
	if len(legacyState.Snapshot.Pods) != len(artState.Snapshot.Pods) {
		t.Errorf("Snapshot.Pods count mismatch: legacy=%d, artifact=%d",
			len(legacyState.Snapshot.Pods), len(artState.Snapshot.Pods))
	}
	if len(legacyState.MatchedPodsByIndex) != len(artState.MatchedPodsByIndex) {
		t.Errorf("MatchedPodsByIndex length mismatch: legacy=%d, artifact=%d",
			len(legacyState.MatchedPodsByIndex), len(artState.MatchedPodsByIndex))
	}
	if legacyState.HasPendingIPPods != artState.HasPendingIPPods {
		t.Errorf("HasPendingIPPods mismatch: legacy=%v, artifact=%v",
			legacyState.HasPendingIPPods, artState.HasPendingIPPods)
	}
}

// ---------------------------------------------------------------------------
// TestCache_SetFromRecomputeArtifactsIfVersion_RejectsStaleVersionAndLifecycleEpoch
// The artifact-based CAS must reject publishes when version or lifecycle
// epoch have advanced since the caller captured fences.
// ---------------------------------------------------------------------------
func TestCache_SetFromRecomputeArtifactsIfVersion_RejectsStaleVersionAndLifecycleEpoch(t *testing.T) {
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	mapping := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web"}, asgID)
	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	cache := NewDesiredStateCache("test-cluster")

	// Seed initial state
	initialDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	initialSnapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
		{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
	}}
	cache.SetFromRecompute(mapping, nil, initialDesired, initialSnapshot, []int{1}, false)

	// Capture fences
	_, capturedVer, capturedEpoch, _ := cache.GetWithVersion(mapping)

	t.Run("rejects after version advance from pod event", func(t *testing.T) {
		newPod := cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2")
		cache.OnPodAdd(mapping, newPod)

		staleArtifacts := DesiredStateRecomputeArtifacts{
			Desired:            initialDesired,
			Snapshot:           initialSnapshot,
			MatchedPodsByIndex: []int{1},
			HasPendingIPPods:   false,
			PodRules: map[PodIdentity]map[int]struct{}{
				{Namespace: "default", Name: "pod-1"}: {0: {}},
			},
			PendingIPCount: 0,
		}
		committed, _, _ := cache.SetFromRecomputeArtifactsIfVersion(
			mapping, staleArtifacts, capturedVer, capturedEpoch,
		)
		if committed {
			t.Error("SetFromRecomputeArtifactsIfVersion should reject stale version after pod event")
		}
	})

	t.Run("rejects after lifecycle epoch bump from Delete", func(t *testing.T) {
		cache2 := NewDesiredStateCache("test-cluster")
		cache2.SetFromRecompute(mapping, nil, initialDesired, initialSnapshot, []int{1}, false)
		_, ver, epoch, _ := cache2.GetWithVersion(mapping)

		// Delete bumps lifecycle epoch
		cache2.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

		staleArtifacts := DesiredStateRecomputeArtifacts{
			Desired:            initialDesired,
			Snapshot:           initialSnapshot,
			MatchedPodsByIndex: []int{1},
			HasPendingIPPods:   false,
			PodRules: map[PodIdentity]map[int]struct{}{
				{Namespace: "default", Name: "pod-1"}: {0: {}},
			},
			PendingIPCount: 0,
		}
		committed, _, _ := cache2.SetFromRecomputeArtifactsIfVersion(
			mapping, staleArtifacts, ver, epoch,
		)
		if committed {
			t.Error("SetFromRecomputeArtifactsIfVersion should reject after lifecycle epoch bump")
		}
	})
}

// ---------------------------------------------------------------------------
// TestCache_SetFromRecomputeArtifactsIfVersion_DeleteRecreateFenceStillRejectsStalePublish
// Validates that a delete/recreate with generation reset to 1 still rejects
// stale in-flight recomputes from the old object through the artifact path.
// ---------------------------------------------------------------------------
func TestCache_SetFromRecomputeArtifactsIfVersion_DeleteRecreateFenceStillRejectsStalePublish(t *testing.T) {
	asgID := makeASGResourceID("sub1", "rg1", "asg1")
	target := ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-web-mapping",
	}

	cache := NewDesiredStateCache("test-cluster")

	// Old mapping gen=5
	mappingOld := cacheTestMapping("default", "web-mapping", 5,
		map[string]string{"app": "web"}, asgID)

	oldDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.OLD/32": {}}},
	}
	cache.SetFromRecompute(mappingOld, nil, oldDesired, PodSnapshot{}, []int{1}, false)

	// Stale reconcile captures fences
	_, staleVer, staleEpoch, _ := cache.GetWithVersion(mappingOld)

	// Object deleted then recreated with generation=1
	cache.Delete(types.NamespacedName{Namespace: "default", Name: "web-mapping"})

	// Stale recompute from old object tries artifact-based publish
	staleArtifacts := DesiredStateRecomputeArtifacts{
		Desired:            oldDesired,
		Snapshot:           PodSnapshot{},
		MatchedPodsByIndex: []int{1},
		HasPendingIPPods:   false,
		PodRules:           map[PodIdentity]map[int]struct{}{},
		PendingIPCount:     0,
	}
	committed, _, _ := cache.SetFromRecomputeArtifactsIfVersion(
		mappingOld, staleArtifacts, staleVer, staleEpoch,
	)
	if committed {
		t.Error("artifact-based stale recompute must not repopulate cache after delete/recreate")
	}

	// Recreated mapping should be able to commit
	mappingNew := cacheTestMapping("default", "web-mapping", 1,
		map[string]string{"app": "web-v2"}, asgID)
	newDesired := map[ASGTarget]DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.NEW/32": {}}},
	}
	_, freshVer, freshEpoch, _ := cache.GetWithVersion(mappingNew)
	newArtifacts := DesiredStateRecomputeArtifacts{
		Desired:            newDesired,
		Snapshot:           PodSnapshot{},
		MatchedPodsByIndex: []int{1},
		HasPendingIPPods:   false,
		PodRules:           map[PodIdentity]map[int]struct{}{},
		PendingIPCount:     0,
	}
	committedNew, _, _ := cache.SetFromRecomputeArtifactsIfVersion(
		mappingNew, newArtifacts, freshVer, freshEpoch,
	)
	if !committedNew {
		t.Error("fresh artifact publish for recreated object should succeed")
	}

	got, ok := cache.Get(mappingNew)
	if !ok {
		t.Fatal("expected cache hit for recreated mapping")
	}
	if _, hasNew := got.Desired[target].IPs["10.0.0.NEW/32"]; !hasNew {
		t.Error("cache should contain new mapping's IP")
	}
	if _, hasOld := got.Desired[target].IPs["10.0.0.OLD/32"]; hasOld {
		t.Error("cache must not contain old mapping's IP after delete/recreate")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_SetFromRecomputeArtifactsIfVersion_ProducesIdenticalOutputToSetFromRecompute
// Verifies that the refactored artifact publish path produces cache entries
// identical to the older SetFromRecompute path, ensuring the internal helper
// refactor has no output drift.
// ---------------------------------------------------------------------------
func TestPhase5_SetFromRecomputeArtifactsIfVersion_ProducesIdenticalOutputToSetFromRecompute(t *testing.T) {
asgID := makeASGResourceID("sub1", "rg1", "asg1")
target := ASGTarget{
SubscriptionID: "sub1",
ResourceGroup:  "rg1",
ASGName:        "asg1",
FullResourceID: asgID,
PrefixSetName:  "test-cluster-default-parity-mapping",
}

mapping := cacheTestMapping("default", "parity-mapping", 1,
map[string]string{"app": "web"}, asgID)

pods := []corev1.Pod{
*cacheTestPod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
*cacheTestPod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2"),
}

desired := map[ASGTarget]DesiredPrefixSet{
target: {IPs: map[string]struct{}{
"10.0.0.1/32": {},
"10.0.0.2/32": {},
}},
}
snapshot := PodSnapshot{Pods: map[PodIdentity]PodMembership{
{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
{Namespace: "default", Name: "pod-2"}: {PodIP: "10.0.0.2"},
}}

// Path A: SetFromRecompute (legacy)
cacheA := NewDesiredStateCache("test-cluster")
cacheA.SetFromRecompute(mapping, pods, desired, snapshot, []int{2}, false)
gotA, okA := cacheA.Get(mapping)
if !okA {
t.Fatal("expected cache hit from SetFromRecompute")
}

// Path B: SetFromRecomputeArtifactsIfVersion (artifact path)
cacheB := NewDesiredStateCache("test-cluster")
_, ver, epoch, _ := cacheB.GetWithVersion(mapping)
artifacts := DesiredStateRecomputeArtifacts{
Desired:            desired,
Snapshot:           snapshot,
MatchedPodsByIndex: []int{2},
HasPendingIPPods:   false,
PodRules: map[PodIdentity]map[int]struct{}{
{Namespace: "default", Name: "pod-1"}: {0: {}},
{Namespace: "default", Name: "pod-2"}: {0: {}},
},
PendingIPCount: 0,
}
committed, _, _ := cacheB.SetFromRecomputeArtifactsIfVersion(mapping, artifacts, ver, epoch)
if !committed {
t.Fatal("expected artifact CAS commit to succeed")
}
gotB, okB := cacheB.Get(mapping)
if !okB {
t.Fatal("expected cache hit from SetFromRecomputeArtifactsIfVersion")
}

// Compare outputs
if len(gotA.Desired) != len(gotB.Desired) {
t.Errorf("desired map length mismatch: A=%d, B=%d", len(gotA.Desired), len(gotB.Desired))
}
for tgt, dpsA := range gotA.Desired {
dpsB, exists := gotB.Desired[tgt]
if !exists {
t.Errorf("target %v present in A but missing in B", tgt)
continue
}
if len(dpsA.IPs) != len(dpsB.IPs) {
t.Errorf("IP count mismatch for target %v: A=%d, B=%d", tgt, len(dpsA.IPs), len(dpsB.IPs))
}
for ip := range dpsA.IPs {
if _, has := dpsB.IPs[ip]; !has {
t.Errorf("IP %s in A but missing in B for target %v", ip, tgt)
}
}
}
if len(gotA.Snapshot.Pods) != len(gotB.Snapshot.Pods) {
t.Errorf("snapshot pod count mismatch: A=%d, B=%d", len(gotA.Snapshot.Pods), len(gotB.Snapshot.Pods))
}
if len(gotA.MatchedPodsByIndex) != len(gotB.MatchedPodsByIndex) {
t.Errorf("MatchedPodsByIndex length mismatch: A=%d, B=%d", len(gotA.MatchedPodsByIndex), len(gotB.MatchedPodsByIndex))
}
for i := range gotA.MatchedPodsByIndex {
if gotA.MatchedPodsByIndex[i] != gotB.MatchedPodsByIndex[i] {
t.Errorf("MatchedPodsByIndex[%d] mismatch: A=%d, B=%d", i, gotA.MatchedPodsByIndex[i], gotB.MatchedPodsByIndex[i])
}
}
if gotA.HasPendingIPPods != gotB.HasPendingIPPods {
t.Errorf("HasPendingIPPods mismatch: A=%v, B=%v", gotA.HasPendingIPPods, gotB.HasPendingIPPods)
}
}

// ---------------------------------------------------------------------------
// TestPhase5_CASFence_VersionAdvancesPreventsStalePublish
// Verifies that CAS fences correctly reject publishes from stale recomputes
// that captured fences before concurrent pod events. This ensures the
// lifecycle epoch + version fencing still works correctly after refactoring.
// ---------------------------------------------------------------------------
func TestPhase5_CASFence_VersionAdvancesPreventsStalePublish(t *testing.T) {
asgID := makeASGResourceID("sub1", "rg1", "asg1")
target := ASGTarget{
SubscriptionID: "sub1",
ResourceGroup:  "rg1",
ASGName:        "asg1",
FullResourceID: asgID,
PrefixSetName:  "test-cluster-default-fence-mapping",
}

mapping := cacheTestMapping("default", "fence-mapping", 1,
map[string]string{"app": "web"}, asgID)

cache := NewDesiredStateCache("test-cluster")

// Seed cache
cache.SetFromRecompute(mapping, nil,
map[ASGTarget]DesiredPrefixSet{
target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
},
PodSnapshot{Pods: map[PodIdentity]PodMembership{
{Namespace: "default", Name: "pod-1"}: {PodIP: "10.0.0.1"},
}}, []int{1}, false)

// Capture fences (simulating start of a recompute)
_, ver, epoch, _ := cache.GetWithVersion(mapping)

// Concurrent pod event advances the version
newPod := cacheTestPod("default", "pod-new", map[string]string{"app": "web"}, "10.0.0.2")
cache.OnPodAdd(mapping, newPod)

// Attempt to publish with stale fences — should be rejected
staleArtifacts := DesiredStateRecomputeArtifacts{
Desired:            map[ASGTarget]DesiredPrefixSet{target: {IPs: map[string]struct{}{"10.0.0.STALE/32": {}}}},
Snapshot:           PodSnapshot{},
MatchedPodsByIndex: []int{1},
HasPendingIPPods:   false,
PodRules:           map[PodIdentity]map[int]struct{}{},
PendingIPCount:     0,
}
committed, _, _ := cache.SetFromRecomputeArtifactsIfVersion(mapping, staleArtifacts, ver, epoch)
if committed {
t.Error("Phase 5: stale recompute with outdated version fence MUST be rejected by CAS")
}

// Verify cache still has the incrementally-updated state (not stale)
got, ok := cache.Get(mapping)
if !ok {
t.Fatal("expected cache hit")
}
if _, hasStale := got.Desired[target].IPs["10.0.0.STALE/32"]; hasStale {
t.Error("Phase 5: stale data must not be committed to cache")
}
if _, hasNew := got.Desired[target].IPs["10.0.0.2/32"]; !hasNew {
t.Error("Phase 5: incrementally-added pod IP must be preserved after rejected stale publish")
}
}
