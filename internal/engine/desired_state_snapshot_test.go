package engine

import (
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPhase8_ComputeDesiredStateWithSnapshot_ReturnsPodSnapshot verifies that
// ComputeDesiredStateWithSnapshot returns both the desired state map and a
// PodSnapshot reflecting all matched pods.
func TestPhase8_ComputeDesiredStateWithSnapshot_ReturnsPodSnapshot(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "pod-2", map[string]string{"app": "web"}, "10.0.0.2"),
		makePod("default", "pod-3", map[string]string{"app": "other"}, "10.0.0.3"),
	}

	desired, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	// Desired state should still work as before
	if len(desired) == 0 {
		t.Fatal("expected non-empty desired state")
	}

	// Snapshot should contain matched pods (pod-1 and pod-2, not pod-3)
	if len(snapshot.Pods) != 2 {
		t.Errorf("snapshot.Pods length = %d, want 2", len(snapshot.Pods))
	}

	// Verify pod identities are correct
	foundPod1 := false
	foundPod2 := false
	for id, member := range snapshot.Pods {
		if id.Name == "pod-1" && id.Namespace == "default" && member.PodIP == "10.0.0.1" {
			foundPod1 = true
		}
		if id.Name == "pod-2" && id.Namespace == "default" && member.PodIP == "10.0.0.2" {
			foundPod2 = true
		}
	}
	if !foundPod1 {
		t.Error("snapshot missing pod-1")
	}
	if !foundPod2 {
		t.Error("snapshot missing pod-2")
	}
}

// TestPhase8_ComputeDesiredStateWithSnapshot_EmptyPodsReturnsEmptySnapshot verifies
// that no matched pods produce an empty PodSnapshot.
func TestPhase8_ComputeDesiredStateWithSnapshot_EmptyPodsReturnsEmptySnapshot(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
		}),
	}
	// No pods match
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "other"}, "10.0.0.1"),
	}

	_, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	if len(snapshot.Pods) != 0 {
		t.Errorf("snapshot.Pods length = %d, want 0 for no matching pods", len(snapshot.Pods))
	}
}

// TestPhase8_ComputeDesiredStateWithSnapshot_MultipleMappingsAggregatesSnapshot verifies
// that pods matched across multiple mapping rules are correctly deduplicated in the snapshot.
func TestPhase8_ComputeDesiredStateWithSnapshot_MultipleMappingsAggregatesSnapshot(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-multi", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-2")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	_, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	// Pod should appear once in snapshot (deduplicated), not once per target
	if len(snapshot.Pods) != 1 {
		t.Errorf("snapshot.Pods length = %d, want 1 (deduplicated across targets)", len(snapshot.Pods))
	}
}

// TestPhase8_ComputeDesiredStateWithSnapshot_BackwardCompatibility verifies that
// ComputeDesiredState (without snapshot) still works and produces the same desired state.
func TestPhase8_ComputeDesiredStateWithSnapshot_BackwardCompatibility(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	desiredOnly := ComputeDesiredState("test-cluster", mappings, pods)
	desiredWithSnapshot, _ := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	if len(desiredOnly) != len(desiredWithSnapshot) {
		t.Errorf("ComputeDesiredState result count = %d, ComputeDesiredStateWithSnapshot = %d; want equal",
			len(desiredOnly), len(desiredWithSnapshot))
	}

	for target, dps := range desiredOnly {
		withSnap, ok := desiredWithSnapshot[target]
		if !ok {
			t.Errorf("target %v present in ComputeDesiredState but missing from ComputeDesiredStateWithSnapshot", target)
			continue
		}
		if len(dps.IPs) != len(withSnap.IPs) {
			t.Errorf("IP set length mismatch for target %v: %d vs %d", target, len(dps.IPs), len(withSnap.IPs))
		}
	}
}

// TestPhase8_ComputeDesiredStateWithSnapshot_PodWithoutIPExcluded verifies that
// pods without an IP are excluded from the snapshot.
func TestPhase8_ComputeDesiredStateWithSnapshot_PodWithoutIPExcluded(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
		// Pod without IP (pending scheduling)
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pod-pending",
				Labels:    map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{PodIP: ""},
		},
	}

	_, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	// Only pod-1 should be in snapshot (pod-pending has no IP)
	if len(snapshot.Pods) != 1 {
		t.Errorf("snapshot.Pods length = %d, want 1 (exclude pods without IP)", len(snapshot.Pods))
	}
}

// ===========================================================================
// Phase 5: Desired-State Cache — Snapshot Extraction Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateWithSnapshot_HelperExtraction_PreservesParity
// After extracting shared evaluation logic, ComputeDesiredStateWithSnapshot
// should still produce identical results for desired state and snapshot.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateWithSnapshot_HelperExtraction_PreservesParity(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-2")},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"tier": "backend"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-3")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-pod-2", map[string]string{"app": "web", "tier": "backend"}, "10.0.0.2"),
		makePod("default", "backend-pod", map[string]string{"tier": "backend"}, "10.0.0.3"),
		makePod("default", "unrelated-pod", map[string]string{"app": "db"}, "10.0.0.4"),
	}

	desired, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	// Verify desired state has correct targets
	if len(desired) == 0 {
		t.Fatal("expected non-empty desired state")
	}

	// Count unique pods in snapshot: web-pod-1, web-pod-2, backend-pod
	// (web-pod-2 matches both rules but should be deduplicated)
	if len(snapshot.Pods) != 3 {
		t.Errorf("snapshot.Pods length = %d, want 3 (web-pod-1, web-pod-2, backend-pod)", len(snapshot.Pods))
	}

	// Verify the shared evaluation logic: web-pod-2 matches both selectors
	// and contributes to asg-1, asg-2 (via app=web) and asg-3 (via tier=backend)
	foundWebPod2 := false
	for id, member := range snapshot.Pods {
		if id.Name == "web-pod-2" {
			foundWebPod2 = true
			if member.PodIP != "10.0.0.2" {
				t.Errorf("web-pod-2 IP = %q, want 10.0.0.2", member.PodIP)
			}
		}
	}
	if !foundWebPod2 {
		t.Error("snapshot missing web-pod-2 which matches multiple selectors")
	}

	// Verify parity: ComputeDesiredState produces same desired state
	desiredOnly := ComputeDesiredState("test-cluster", mappings, pods)
	if len(desiredOnly) != len(desired) {
		t.Errorf("desired state count mismatch: ComputeDesiredState=%d vs WithSnapshot=%d",
			len(desiredOnly), len(desired))
	}
	for target, dps := range desiredOnly {
		withSnap, ok := desired[target]
		if !ok {
			t.Errorf("target %v present in ComputeDesiredState but missing from WithSnapshot", target)
			continue
		}
		if len(dps.IPs) != len(withSnap.IPs) {
			t.Errorf("IP set length mismatch for target %v: %d vs %d",
				target, len(dps.IPs), len(withSnap.IPs))
		}
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateWithSnapshot_PendingIPPods_Tracked
// Pods without IP should be excluded from desired IPs but tracked for
// pending-IP detection in the cache.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateWithSnapshot_PendingIPPods_Tracked(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		makeMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
				},
			},
		}),
	}
	pods := []corev1.Pod{
		makePod("default", "pod-with-ip", map[string]string{"app": "web"}, "10.0.0.1"),
		// Pods without IPs (pending scheduling)
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pod-no-ip-1",
				Labels:    map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{PodIP: ""},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pod-no-ip-2",
				Labels:    map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{PodIP: ""},
		},
	}

	desired, snapshot := ComputeDesiredStateWithSnapshot("test-cluster", mappings, pods)

	// Only pod-with-ip should contribute to desired IPs
	for _, dps := range desired {
		if len(dps.IPs) != 1 {
			t.Errorf("desired IPs = %d, want 1 (only pod with IP contributes)", len(dps.IPs))
		}
		if _, ok := dps.IPs["10.0.0.1/32"]; !ok {
			t.Errorf("desired IPs should contain 10.0.0.1/32, got %v", dps.IPs)
		}
	}

	// Snapshot should only contain the pod with IP
	if len(snapshot.Pods) != 1 {
		t.Errorf("snapshot.Pods = %d, want 1 (only pods with IP in snapshot)", len(snapshot.Pods))
	}
}
