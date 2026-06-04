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
// Phase 5: Desired-State Cache — Recompute Artifacts Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_ParityWithExistingOutputs
// The single-pass artifact builder must produce identical results to the
// existing multi-pass approach: ComputeDesiredStateWithSnapshot, ComputeMatchedPodsByMapping,
// and hasPendingIPPods detection.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_ParityWithExistingOutputs(t *testing.T) {
	mapping := makeMapping("default", "mapping-1", []v1alpha1.Mapping{
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
				MatchLabels: map[string]string{"tier": "backend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-2")},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
		makePod("default", "web-pod-2", map[string]string{"app": "web", "tier": "backend"}, "10.0.0.2"),
		makePod("default", "backend-pod", map[string]string{"tier": "backend"}, "10.0.0.3"),
		makePod("default", "unrelated-pod", map[string]string{"app": "db"}, "10.0.0.4"),
		// Pod without IP (pending)
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "pending-pod",
				Labels:    map[string]string{"app": "web"},
			},
			Status: corev1.PodStatus{PodIP: ""},
		},
	}

	// Compute using existing multi-pass approach
	existingDesired, existingSnapshot := ComputeDesiredStateWithSnapshot("test-cluster", []v1alpha1.PodASGMapping{mapping}, pods)

	// Compute using the new single-pass artifact builder
	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// Parity check 1: Desired state must be identical
	if len(artifacts.Desired) != len(existingDesired) {
		t.Errorf("artifacts.Desired has %d targets, existing has %d", len(artifacts.Desired), len(existingDesired))
	}
	for target, existingDPS := range existingDesired {
		artifactDPS, ok := artifacts.Desired[target]
		if !ok {
			t.Errorf("target %v present in existing desired but missing from artifacts", target)
			continue
		}
		if len(existingDPS.IPs) != len(artifactDPS.IPs) {
			t.Errorf("IP count mismatch for target %v: existing=%d, artifacts=%d",
				target, len(existingDPS.IPs), len(artifactDPS.IPs))
		}
		for ip := range existingDPS.IPs {
			if _, ok := artifactDPS.IPs[ip]; !ok {
				t.Errorf("IP %s present in existing but missing from artifacts for target %v", ip, target)
			}
		}
	}

	// Parity check 2: Snapshot must be identical
	if len(artifacts.Snapshot.Pods) != len(existingSnapshot.Pods) {
		t.Errorf("artifacts.Snapshot.Pods has %d entries, existing has %d",
			len(artifacts.Snapshot.Pods), len(existingSnapshot.Pods))
	}
	for id, member := range existingSnapshot.Pods {
		artMember, ok := artifacts.Snapshot.Pods[id]
		if !ok {
			t.Errorf("pod %v present in existing snapshot but missing from artifacts", id)
			continue
		}
		if artMember.PodIP != member.PodIP {
			t.Errorf("pod %v IP mismatch: existing=%s, artifacts=%s", id, member.PodIP, artMember.PodIP)
		}
	}

	// Parity check 3: MatchedPodsByIndex must reflect per-rule counts
	// Rule 0 (app=web) matches: web-pod-1, web-pod-2, pending-pod = 3
	// Rule 1 (tier=backend) matches: web-pod-2, backend-pod = 2
	if len(artifacts.MatchedPodsByIndex) != 2 {
		t.Fatalf("MatchedPodsByIndex length = %d, want 2", len(artifacts.MatchedPodsByIndex))
	}
	if artifacts.MatchedPodsByIndex[0] != 3 {
		t.Errorf("MatchedPodsByIndex[0] = %d, want 3 (web-pod-1, web-pod-2, pending-pod)", artifacts.MatchedPodsByIndex[0])
	}
	if artifacts.MatchedPodsByIndex[1] != 2 {
		t.Errorf("MatchedPodsByIndex[1] = %d, want 2 (web-pod-2, backend-pod)", artifacts.MatchedPodsByIndex[1])
	}

	// Parity check 4: HasPendingIPPods must be true (pending-pod has no IP)
	if !artifacts.HasPendingIPPods {
		t.Error("artifacts.HasPendingIPPods = false, want true (pending-pod has no IP)")
	}

	// Parity check 5: PendingIPCount
	if artifacts.PendingIPCount != 1 {
		t.Errorf("artifacts.PendingIPCount = %d, want 1", artifacts.PendingIPCount)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_PodRulesContributorCoverage
// Verifies that PodRules correctly tracks which rule indices each pod contributes to,
// including multi-rule/multi-target scenarios and shared-IP pods.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_PodRulesContributorCoverage(t *testing.T) {
	mapping := makeMapping("default", "mapping-multi", []v1alpha1.Mapping{
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
				MatchLabels: map[string]string{"tier": "backend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-2")},
			},
		},
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"region": "east"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-3")},
			},
		},
	})
	pods := []corev1.Pod{
		// Matches rule 0 only
		makePod("default", "web-only", map[string]string{"app": "web"}, "10.0.0.1"),
		// Matches rules 0 and 1
		makePod("default", "web-backend", map[string]string{"app": "web", "tier": "backend"}, "10.0.0.2"),
		// Matches all three rules
		makePod("default", "all-match", map[string]string{"app": "web", "tier": "backend", "region": "east"}, "10.0.0.3"),
		// Same IP as web-only but different pod (shared IP scenario)
		makePod("default", "shared-ip-pod", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// Verify PodRules is populated
	if artifacts.PodRules == nil {
		t.Fatal("artifacts.PodRules is nil")
	}

	tests := []struct {
		name          string
		podName       string
		expectedRules map[int]struct{}
	}{
		{
			name:          "web-only matches rule 0",
			podName:       "web-only",
			expectedRules: map[int]struct{}{0: {}},
		},
		{
			name:          "web-backend matches rules 0 and 1",
			podName:       "web-backend",
			expectedRules: map[int]struct{}{0: {}, 1: {}},
		},
		{
			name:          "all-match matches rules 0, 1, and 2",
			podName:       "all-match",
			expectedRules: map[int]struct{}{0: {}, 1: {}, 2: {}},
		},
		{
			name:          "shared-ip-pod matches rule 0",
			podName:       "shared-ip-pod",
			expectedRules: map[int]struct{}{0: {}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			podID := PodIdentity{Namespace: "default", Name: tc.podName}
			rules, ok := artifacts.PodRules[podID]
			if !ok {
				t.Fatalf("PodRules missing entry for pod %s", tc.podName)
			}
			if len(rules) != len(tc.expectedRules) {
				t.Errorf("pod %s has %d rule entries, want %d", tc.podName, len(rules), len(tc.expectedRules))
			}
			for idx := range tc.expectedRules {
				if _, ok := rules[idx]; !ok {
					t.Errorf("pod %s missing expected rule index %d", tc.podName, idx)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_NoPods_EmptyArtifacts
// Edge case: no pods produces empty artifacts.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_NoPods_EmptyArtifacts(t *testing.T) {
	mapping := makeMapping("default", "mapping-1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
			},
		},
	})

	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, nil)

	if len(artifacts.Desired) != 0 {
		t.Errorf("expected empty Desired for no pods, got %d entries", len(artifacts.Desired))
	}
	if len(artifacts.Snapshot.Pods) != 0 {
		t.Errorf("expected empty Snapshot for no pods, got %d entries", len(artifacts.Snapshot.Pods))
	}
	if artifacts.HasPendingIPPods {
		t.Error("HasPendingIPPods should be false when no pods")
	}
	if artifacts.PendingIPCount != 0 {
		t.Errorf("PendingIPCount = %d, want 0", artifacts.PendingIPCount)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_InvalidASGResourceID_Skipped
// Edge case: invalid ASG resource IDs are silently skipped (no panic).
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_InvalidASGResourceID_Skipped(t *testing.T) {
	mapping := makeMapping("default", "mapping-1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "not-a-valid-resource-id"},
			},
		},
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-valid")},
			},
		},
	})
	pods := []corev1.Pod{
		makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1"),
	}

	// Should not panic, rule 0's invalid ASG is skipped, rule 1 still works
	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// Rule 1 with valid ASG should produce desired state
	if len(artifacts.Desired) != 1 {
		t.Errorf("expected 1 target from valid ASG rule, got %d", len(artifacts.Desired))
	}

	// Both rules match the pod (same selector), so MatchedPodsByIndex should be [1, 1]
	if len(artifacts.MatchedPodsByIndex) != 2 {
		t.Fatalf("MatchedPodsByIndex length = %d, want 2", len(artifacts.MatchedPodsByIndex))
	}
	if artifacts.MatchedPodsByIndex[0] != 1 {
		t.Errorf("MatchedPodsByIndex[0] = %d, want 1", artifacts.MatchedPodsByIndex[0])
	}
	if artifacts.MatchedPodsByIndex[1] != 1 {
		t.Errorf("MatchedPodsByIndex[1] = %d, want 1", artifacts.MatchedPodsByIndex[1])
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

// ---------------------------------------------------------------------------
// TestPhase5_RecomputeArtifacts_PendingIPPods_TrackedInArtifacts
// ComputeDesiredStateRecomputeArtifacts must correctly track pending-IP pods
// (matched pods with no IP) in both HasPendingIPPods and PendingIPCount.
// This ensures parity with the incremental cache pending tracking.
// ---------------------------------------------------------------------------
func TestPhase5_RecomputeArtifacts_PendingIPPods_TrackedInArtifacts(t *testing.T) {
	mapping := makeMapping("default", "pending-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: makeASGResourceID("sub-1", "rg-1", "asg-1")},
			},
		},
	})

	pods := []corev1.Pod{
		makePod("default", "pod-with-ip", map[string]string{"app": "web"}, "10.0.0.1"),
		// Two pods without IP (pending scheduling)
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-pending-1", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: ""},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-pending-2", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: ""},
		},
	}

	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	if !artifacts.HasPendingIPPods {
		t.Error("HasPendingIPPods should be true when pods without IPs match selectors")
	}
	if artifacts.PendingIPCount != 2 {
		t.Errorf("PendingIPCount = %d, want 2", artifacts.PendingIPCount)
	}

	// Only the pod with IP should be in the snapshot
	if len(artifacts.Snapshot.Pods) != 1 {
		t.Errorf("Snapshot.Pods length = %d, want 1 (only pods with IP)", len(artifacts.Snapshot.Pods))
	}

	// All 3 matched pods should be in PodRules
	if len(artifacts.PodRules) != 3 {
		t.Errorf("PodRules length = %d, want 3 (all matched pods including pending)", len(artifacts.PodRules))
	}

	// MatchedPodsByIndex[0] should be 3 (all three pods match the single rule)
	if len(artifacts.MatchedPodsByIndex) != 1 || artifacts.MatchedPodsByIndex[0] != 3 {
		t.Errorf("MatchedPodsByIndex = %v, want [3]", artifacts.MatchedPodsByIndex)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_RecomputeArtifacts_SharedIPContributors_MultipleRules
// When multiple pods share the same IP (e.g. host-network) across multiple
// rules, the artifacts must correctly track per-pod-per-rule contributions
// so that the incremental cache can determine when an IP is safe to remove.
// ---------------------------------------------------------------------------
func TestPhase5_RecomputeArtifacts_SharedIPContributors_MultipleRules(t *testing.T) {
	asgID1 := makeASGResourceID("sub-1", "rg-1", "asg-frontend")
	asgID2 := makeASGResourceID("sub-1", "rg-1", "asg-all")

	mapping := makeMapping("default", "shared-ip-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"role": "frontend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID1}},
		},
		{
			// Matches ALL pods (superset selector)
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"tier": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID2}},
		},
	})

	// Two pods with the SAME IP (host-network scenario) but different labels
	pods := []corev1.Pod{
		makePod("default", "pod-a", map[string]string{"role": "frontend", "tier": "web"}, "192.168.1.1"),
		makePod("default", "pod-b", map[string]string{"role": "backend", "tier": "web"}, "192.168.1.1"),
	}

	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// pod-a matches both rules, pod-b matches only rule 1 (tier=web)
	podAID := PodIdentity{Namespace: "default", Name: "pod-a"}
	podBID := PodIdentity{Namespace: "default", Name: "pod-b"}

	if rules, ok := artifacts.PodRules[podAID]; !ok || len(rules) != 2 {
		t.Errorf("pod-a should match 2 rules, got PodRules[pod-a]=%v", artifacts.PodRules[podAID])
	}
	if rules, ok := artifacts.PodRules[podBID]; !ok || len(rules) != 1 {
		t.Errorf("pod-b should match 1 rule (tier=web), got PodRules[pod-b]=%v", artifacts.PodRules[podBID])
	}

	// Both pods should be in the snapshot with same IP
	if len(artifacts.Snapshot.Pods) != 2 {
		t.Errorf("Snapshot.Pods length = %d, want 2", len(artifacts.Snapshot.Pods))
	}

	// The shared IP should appear in both target's desired state
	foundInASGAll := false
	for tgt, dps := range artifacts.Desired {
		if tgt.ASGName == "asg-all" {
			if _, has := dps.IPs["192.168.1.1/32"]; has {
				foundInASGAll = true
			}
		}
	}
	if !foundInASGAll {
		t.Error("shared IP 192.168.1.1/32 should appear in asg-all target (both pods contribute)")
	}
}

// ===========================================================================
// Phase 5: Recompute Artifacts — Multi-Rule Same Target Deduplication
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_MultiRuleSameTarget_IPNotDuplicated
// When multiple rules resolve to the same ASG target and the same pod matches
// both rules, the IP must appear exactly once in the target's desired set.
// This validates target resolution deduplication in the artifact builder.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_MultiRuleSameTarget_IPNotDuplicated(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-shared")

	// Two rules with different selectors but pointing to the SAME ASG
	mapping := makeMapping("default", "multi-rule-same-asg", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"tier": "frontend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	// Pod matches BOTH rules
	pods := []corev1.Pod{
		makePod("default", "dual-match-pod", map[string]string{"app": "web", "tier": "frontend"}, "10.0.0.1"),
	}

	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// The target should exist with the IP
	target := ASGTarget{
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-shared",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-multi-rule-same-asg",
	}

	dps, exists := artifacts.Desired[target]
	if !exists {
		t.Fatal("expected target in artifacts.Desired")
	}

	// IP must appear exactly once (not duplicated due to matching both rules)
	if len(dps.IPs) != 1 {
		t.Errorf("expected exactly 1 IP in target (deduplicated), got %d: %v", len(dps.IPs), dps.IPs)
	}
	if _, has := dps.IPs["10.0.0.1/32"]; !has {
		t.Errorf("expected 10.0.0.1/32 in target IPs, got %v", dps.IPs)
	}

	// Pod should be tracked in PodRules for both rule indices
	podID := PodIdentity{Namespace: "default", Name: "dual-match-pod"}
	rules, ok := artifacts.PodRules[podID]
	if !ok {
		t.Fatal("pod should be tracked in PodRules")
	}
	if _, has := rules[0]; !has {
		t.Error("pod should be tracked under rule 0")
	}
	if _, has := rules[1]; !has {
		t.Error("pod should be tracked under rule 1")
	}

	// MatchedPodsByIndex: both rules match 1 pod each
	if len(artifacts.MatchedPodsByIndex) != 2 {
		t.Fatalf("MatchedPodsByIndex length = %d, want 2", len(artifacts.MatchedPodsByIndex))
	}
	if artifacts.MatchedPodsByIndex[0] != 1 {
		t.Errorf("MatchedPodsByIndex[0] = %d, want 1", artifacts.MatchedPodsByIndex[0])
	}
	if artifacts.MatchedPodsByIndex[1] != 1 {
		t.Errorf("MatchedPodsByIndex[1] = %d, want 1", artifacts.MatchedPodsByIndex[1])
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_ComputeDesiredStateRecomputeArtifacts_IncrementalParity_DeleteOneOfTwoContributors
// After a full recompute with two pods contributing an IP, removing one pod via
// the incremental cache path must keep the IP (since the other pod still contributes).
// This validates the parity contract between recompute and incremental paths.
// ---------------------------------------------------------------------------
func TestPhase5_ComputeDesiredStateRecomputeArtifacts_IncrementalParity_DeleteOneOfTwoContributors(t *testing.T) {
	asgID := makeASGResourceID("sub-1", "rg-1", "asg-1")
	mapping := makeMapping("default", "contrib-parity", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
		},
	})

	target := ASGTarget{
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-default-contrib-parity",
	}

	// Two pods with the same IP
	podA := makePod("default", "pod-a", map[string]string{"app": "web"}, "192.168.1.1")
	podB := makePod("default", "pod-b", map[string]string{"app": "web"}, "192.168.1.1")

	pods := []corev1.Pod{podA, podB}
	artifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, pods)

	// Commit artifacts to cache
	cache := NewDesiredStateCache("test-cluster")
	_, ver, epoch, _ := cache.GetWithVersion(&mapping)
	committed, _, _ := cache.SetFromRecomputeArtifactsIfVersion(&mapping, artifacts, ver, epoch)
	if !committed {
		t.Fatal("artifact commit should succeed")
	}

	// Delete pod-a incrementally
	cache.OnPodDelete(&mapping, &podA)

	got, ok := cache.Get(&mapping)
	if !ok {
		t.Fatal("expected cache hit after delete")
	}

	// IP must be retained (pod-b still contributes)
	if _, hasIP := got.Desired[target].IPs["192.168.1.1/32"]; !hasIP {
		t.Errorf("shared IP must be retained when one contributor remains; got IPs: %v", got.Desired[target].IPs)
	}

	// Compare with fresh recompute using only pod-b
	freshArtifacts := ComputeDesiredStateRecomputeArtifacts("test-cluster", mapping, []corev1.Pod{podB})
	if _, hasIP := freshArtifacts.Desired[target].IPs["192.168.1.1/32"]; !hasIP {
		t.Error("fresh recompute with pod-b must include the shared IP")
	}

	// MatchedPodsByIndex parity
	if got.MatchedPodsByIndex[0] != freshArtifacts.MatchedPodsByIndex[0] {
		t.Errorf("MatchedPodsByIndex parity: incremental=%d, recompute=%d",
			got.MatchedPodsByIndex[0], freshArtifacts.MatchedPodsByIndex[0])
	}
}
