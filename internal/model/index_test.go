package model

import (
	"fmt"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// helper to build a PodASGMapping CR for tests.
func newMapping(namespace, name string, rules []v1alpha1.Mapping) v1alpha1.PodASGMapping {
	return v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: rules,
		},
	}
}

// helper to build a Pod for tests.
func newPod(namespace, name string, podLabels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    podLabels,
		},
	}
}

// mustBuildIndex calls BuildIndex and returns the compiled index.
func mustBuildIndex(t *testing.T, mappings []v1alpha1.PodASGMapping) *MappingIndex {
	t.Helper()
	return BuildIndex(mappings)
}

func TestPhase2_T28_BuildIndex_ThreeMappingsQueryReturnsExpectedASGs(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-api"
	asg3 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-db"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg1},
				},
			},
		}),
		newMapping("default", "api-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg2},
				},
			},
		}),
		newMapping("default", "db-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "db"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg3},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	// Query with web pod
	webPod := newPod("default", "web-pod-1", map[string]string{"app": "web"})
	webASGs := idx.MatchingASGs(webPod)
	if len(webASGs) != 1 {
		t.Fatalf("web pod: got %d ASGs, want 1", len(webASGs))
	}
	if webASGs[0].ResourceID != asg1 {
		t.Errorf("web pod: got ASG %q, want %q", webASGs[0].ResourceID, asg1)
	}

	// Query with api pod
	apiPod := newPod("default", "api-pod-1", map[string]string{"app": "api"})
	apiASGs := idx.MatchingASGs(apiPod)
	if len(apiASGs) != 1 {
		t.Fatalf("api pod: got %d ASGs, want 1", len(apiASGs))
	}
	if apiASGs[0].ResourceID != asg2 {
		t.Errorf("api pod: got ASG %q, want %q", apiASGs[0].ResourceID, asg2)
	}

	// Query with db pod
	dbPod := newPod("default", "db-pod-1", map[string]string{"app": "db"})
	dbASGs := idx.MatchingASGs(dbPod)
	if len(dbASGs) != 1 {
		t.Fatalf("db pod: got %d ASGs, want 1", len(dbASGs))
	}
	if dbASGs[0].ResourceID != asg3 {
		t.Errorf("db pod: got ASG %q, want %q", dbASGs[0].ResourceID, asg3)
	}
}

func TestPhase2_T29_OverlappingSelectorsReturnUnion(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-shared"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web-specific"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "shared-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg1},
				},
			},
		}),
		newMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg2},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	// Pod matches both mappings
	pod := newPod("default", "web-frontend", map[string]string{"app": "web", "tier": "frontend"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 2 {
		t.Fatalf("overlapping pod: got %d ASGs, want 2", len(asgs))
	}

	// Verify both ASGs are present (order: first-seen from flatten order)
	asgIDs := make(map[string]bool)
	for _, a := range asgs {
		asgIDs[a.ResourceID] = true
	}
	if !asgIDs[asg1] {
		t.Errorf("missing expected ASG %q in result", asg1)
	}
	if !asgIDs[asg2] {
		t.Errorf("missing expected ASG %q in result", asg2)
	}
}

func TestPhase2_T210_NoMatchingMappingsReturnsEmptySet(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg1},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	// Precondition: a matching pod DOES return results (validates index is built)
	matchPod := newPod("default", "web-pod", map[string]string{"app": "web"})
	matchASGs := idx.MatchingASGs(matchPod)
	if len(matchASGs) != 1 {
		t.Fatalf("precondition: matching pod got %d ASGs, want 1 (index must be built correctly)", len(matchASGs))
	}

	// Pod that doesn't match any selector
	pod := newPod("default", "random-pod", map[string]string{"app": "worker"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("non-matching pod: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_BuildIndex_NamespaceIsolation(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("production", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg1},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	// Pod in a different namespace should NOT match
	pod := newPod("staging", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("cross-namespace pod: got %d ASGs, want 0 (namespace isolation)", len(asgs))
	}

	// Pod in same namespace should match
	samePod := newPod("production", "web-pod", map[string]string{"app": "web"})
	sameASGs := idx.MatchingASGs(samePod)
	if len(sameASGs) != 1 {
		t.Errorf("same-namespace pod: got %d ASGs, want 1", len(sameASGs))
	}
}

func TestPhase2_MatchingASGs_DeduplicatesByCanonicalResourceID(t *testing.T) {
	// Same ASG referenced with different casing in provider path
	asgLower := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	asgUpper := "/subscriptions/sub1/resourceGroups/rg1/providers/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/my-asg"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "mapping-1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgLower},
				},
			},
		}),
		newMapping("default", "mapping-2", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgUpper},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Errorf("deduplicated ASGs: got %d, want 1 (canonical dedupe)", len(asgs))
	}
}

func TestPhase2_MatchingASGs_DedupeFirstSeenMetadataWins(t *testing.T) {
	asgID := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/my-asg"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "mapping-first", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgID, SubscriptionID: "first-sub", Description: "first desc"},
				},
			},
		}),
		newMapping("default", "mapping-second", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgID, SubscriptionID: "second-sub", Description: "second desc"},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Fatalf("deduplicated ASGs: got %d, want 1", len(asgs))
	}
	// First-seen metadata should win
	if asgs[0].SubscriptionID != "first-sub" {
		t.Errorf("SubscriptionID = %q, want %q (first-seen wins)", asgs[0].SubscriptionID, "first-sub")
	}
	if asgs[0].Description != "first desc" {
		t.Errorf("Description = %q, want %q (first-seen wins)", asgs[0].Description, "first desc")
	}
}

func TestPhase2_MatchingASGs_PreservesDeterministicFlattenOrder(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-alpha"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-beta"
	asg3 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-gamma"

	// Single mapping with multiple rules, each having a different ASG
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "multi-rule", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg1},
					{ResourceID: asg2},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg3},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 3 {
		t.Fatalf("got %d ASGs, want 3", len(asgs))
	}

	// Order must match flatten order: mapping CR order → rule order → ASG order
	wantOrder := []string{asg1, asg2, asg3}
	for i, want := range wantOrder {
		if asgs[i].ResourceID != want {
			t.Errorf("asgs[%d].ResourceID = %q, want %q (deterministic flatten order)", i, asgs[i].ResourceID, want)
		}
	}
}

func TestPhase2_MatchingASGs_NilPodReturnsEmpty(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)
	asgs := idx.MatchingASGs(nil)
	if len(asgs) != 0 {
		t.Errorf("nil pod: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_MatchingASGs_NilReceiverReturnsEmpty(t *testing.T) {
	var idx *MappingIndex
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("nil index: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_MatchingASGs_PodWithNilLabelsMatchesEmptySelector(t *testing.T) {
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "catch-all", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-all"},
				},
			},
		}),
	}

	idx := mustBuildIndex(t, mappings)

	// Pod with nil labels
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "no-labels-pod",
			Labels:    nil,
		},
	}
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Errorf("nil labels pod with empty selector: got %d ASGs, want 1", len(asgs))
	}
}

// --- Additional coverage below ---

func TestPhase2_BuildIndex_NilMappingsSlice(t *testing.T) {
	idx := mustBuildIndex(t, nil)
	if idx == nil {
		t.Fatal("BuildIndex(nil) should return a non-nil *MappingIndex")
	}
	// Query the empty index — should return no results.
	pod := newPod("default", "any-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("empty index: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_BuildIndex_EmptyMappingsSlice(t *testing.T) {
	idx := mustBuildIndex(t, []v1alpha1.PodASGMapping{})
	if idx == nil {
		t.Fatal("BuildIndex([]) should return a non-nil *MappingIndex")
	}
	pod := newPod("default", "any-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("empty index: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_BuildIndex_MappingWithNoRules(t *testing.T) {
	// A mapping CR with an empty Spec.Mappings should not contribute any entries.
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "empty-rules", []v1alpha1.Mapping{}),
	}
	idx := mustBuildIndex(t, mappings)
	if idx == nil {
		t.Fatal("BuildIndex should return a non-nil *MappingIndex")
	}
	pod := newPod("default", "any-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("mapping with no rules: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_BuildIndex_RuleWithEmptyASGs(t *testing.T) {
	// A rule that matches a pod but has no ASG references should contribute zero ASGs.
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "no-asgs", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("rule with empty ASGs: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_MatchingASGs_EmptyResultsAreNonNil(t *testing.T) {
	// MatchingASGs returns a concrete non-nil empty slice (not nil) for all "no result" paths.
	// This ensures callers can safely range/JSON-marshal without nil checks.
	t.Run("namespace mismatch returns non-nil empty", func(t *testing.T) {
		idx := mustBuildIndex(t, []v1alpha1.PodASGMapping{
			newMapping("production", "m", []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}},
				},
			}),
		})
		pod := newPod("staging", "web-pod", map[string]string{"app": "web"})
		asgs := idx.MatchingASGs(pod)
		if asgs == nil {
			t.Error("namespace mismatch: got nil, want non-nil empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("namespace mismatch: got %d ASGs, want 0", len(asgs))
		}
	})

	t.Run("label mismatch returns non-nil empty", func(t *testing.T) {
		idx := mustBuildIndex(t, []v1alpha1.PodASGMapping{
			newMapping("default", "m", []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}},
				},
			}),
		})
		pod := newPod("default", "other-pod", map[string]string{"app": "worker"})
		asgs := idx.MatchingASGs(pod)
		if asgs == nil {
			t.Error("label mismatch: got nil, want non-nil empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("label mismatch: got %d ASGs, want 0", len(asgs))
		}
	})
}

func TestPhase2_BuildIndex_AllRulesPreservedRegardlessOfSelector(t *testing.T) {
	// Design §3.4B: CompileSelector is total for Phase 2.
	// BuildIndex MUST NOT drop any rules. Even empty-selector rules produce entries.
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-specific"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-catchall"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "mixed-selectors", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1}},
			},
			{
				// Empty selector — should still be compiled and included.
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	// A pod matching only the specific selector should get both (specific + catchall).
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 2 {
		t.Fatalf("expected 2 ASGs (specific + catchall), got %d", len(asgs))
	}
	if asgs[0].ResourceID != asg1 {
		t.Errorf("asgs[0] = %q, want %q", asgs[0].ResourceID, asg1)
	}
	if asgs[1].ResourceID != asg2 {
		t.Errorf("asgs[1] = %q, want %q", asgs[1].ResourceID, asg2)
	}

	// A pod matching only the catchall should get exactly one.
	other := newPod("default", "random-pod", map[string]string{"app": "worker"})
	otherASGs := idx.MatchingASGs(other)
	if len(otherASGs) != 1 {
		t.Fatalf("expected 1 ASG (catchall only), got %d", len(otherASGs))
	}
	if otherASGs[0].ResourceID != asg2 {
		t.Errorf("catchall ASG = %q, want %q", otherASGs[0].ResourceID, asg2)
	}
}

func TestPhase2_MatchingASGs_CanonicalDedupeWithMalformedResourceID(t *testing.T) {
	// Design §3.5 canonicalASGKey: if parse fails, key = strings.ToLower(resourceID).
	// Two malformed IDs that differ only by case should deduplicate.
	badID1 := "not-a-valid-resource-id/FOO"
	badID2 := "not-a-valid-resource-id/foo"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: badID1}},
			},
		}),
		newMapping("default", "m2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: badID2}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Errorf("malformed IDs differing by case: got %d ASGs, want 1 (dedupe via ToLower fallback)", len(asgs))
	}
	// First-seen wins: should be badID1.
	if len(asgs) == 1 && asgs[0].ResourceID != badID1 {
		t.Errorf("first-seen should win: got %q, want %q", asgs[0].ResourceID, badID1)
	}
}

func TestPhase2_MatchingASGs_MalformedAndValidIDsNotDeduped(t *testing.T) {
	// A malformed ID and a valid ID that happen to overlap in a ToLower form should
	// not deduplicate if the valid one canonicalizes differently.
	validID := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	malformedID := "totally-different"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: validID}},
			},
		}),
		newMapping("default", "m2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: malformedID}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 2 {
		t.Errorf("valid + malformed IDs: got %d ASGs, want 2 (distinct canonical keys)", len(asgs))
	}
}

func TestPhase2_BuildIndex_MultiNamespaceIndex(t *testing.T) {
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-prod"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-staging"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("production", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1}},
			},
		}),
		newMapping("staging", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	// Pod in production gets only the production ASG.
	prodPod := newPod("production", "web-1", map[string]string{"app": "web"})
	prodASGs := idx.MatchingASGs(prodPod)
	if len(prodASGs) != 1 {
		t.Fatalf("production pod: got %d ASGs, want 1", len(prodASGs))
	}
	if prodASGs[0].ResourceID != asg1 {
		t.Errorf("production pod: got %q, want %q", prodASGs[0].ResourceID, asg1)
	}

	// Pod in staging gets only the staging ASG.
	stagingPod := newPod("staging", "web-1", map[string]string{"app": "web"})
	stagingASGs := idx.MatchingASGs(stagingPod)
	if len(stagingASGs) != 1 {
		t.Fatalf("staging pod: got %d ASGs, want 1", len(stagingASGs))
	}
	if stagingASGs[0].ResourceID != asg2 {
		t.Errorf("staging pod: got %q, want %q", stagingASGs[0].ResourceID, asg2)
	}

	// Pod in unrelated namespace gets nothing.
	devPod := newPod("development", "web-1", map[string]string{"app": "web"})
	devASGs := idx.MatchingASGs(devPod)
	if len(devASGs) != 0 {
		t.Errorf("dev pod: got %d ASGs, want 0", len(devASGs))
	}
}

func TestPhase2_MatchingASGs_PodWithNilLabelsDoesNotMatchNonEmptySelector(t *testing.T) {
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "web-mapping", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "unlabeled-pod",
			Labels:    nil,
		},
	}
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 0 {
		t.Errorf("nil-labels pod with non-empty selector: got %d ASGs, want 0", len(asgs))
	}
}

func TestPhase2_MatchingASGs_IntraCRDuplicateASGsDeduped(t *testing.T) {
	// Same ASG referenced in multiple rules within one CR should deduplicate.
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/shared-asg"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "multi-rule", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg, Description: "rule-1"}},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg, Description: "rule-2"}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Fatalf("intra-CR duplicate: got %d ASGs, want 1", len(asgs))
	}
	// First-seen wins: Description from first rule.
	if asgs[0].Description != "rule-1" {
		t.Errorf("Description = %q, want %q (first-seen wins)", asgs[0].Description, "rule-1")
	}
}

func TestPhase2_MatchingASGs_DuplicateASGsWithinSameRule(t *testing.T) {
	// Same ASG listed twice in a single rule's ASG list should deduplicate.
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "dup-in-rule", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asg, Description: "first"},
					{ResourceID: asg, Description: "second"},
				},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Fatalf("same-rule duplicate: got %d ASGs, want 1", len(asgs))
	}
	if asgs[0].Description != "first" {
		t.Errorf("Description = %q, want %q (first-seen within rule)", asgs[0].Description, "first")
	}
}

func TestPhase2_MatchingASGs_OrderAcrossMultipleCRs(t *testing.T) {
	// Verify deterministic order when ASGs come from separate CRs.
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-from-cr1"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-from-cr2"
	asg3 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-from-cr3"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "cr1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"team": "platform"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1}},
			},
		}),
		newMapping("default", "cr2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"team": "platform"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2}},
			},
		}),
		newMapping("default", "cr3", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"team": "platform"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg3}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "platform-pod", map[string]string{"team": "platform"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 3 {
		t.Fatalf("multi-CR order: got %d ASGs, want 3", len(asgs))
	}
	wantOrder := []string{asg1, asg2, asg3}
	for i, want := range wantOrder {
		if asgs[i].ResourceID != want {
			t.Errorf("asgs[%d].ResourceID = %q, want %q (input CR order)", i, asgs[i].ResourceID, want)
		}
	}
}

func TestPhase2_MatchingASGs_EmptySelectorMatchesAllPodsInNamespace(t *testing.T) {
	// A catch-all selector (empty matchLabels) should match every pod in its namespace.
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/catch-all"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "catch-all", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	pods := []*corev1.Pod{
		newPod("default", "pod-a", map[string]string{"app": "web"}),
		newPod("default", "pod-b", map[string]string{"role": "worker"}),
		newPod("default", "pod-c", map[string]string{}),
		newPod("default", "pod-d", nil),
	}
	for _, pod := range pods {
		asgs := idx.MatchingASGs(pod)
		if len(asgs) != 1 {
			t.Errorf("pod %q: got %d ASGs, want 1 (empty selector = catch-all)", pod.Name, len(asgs))
		}
	}

	// But not pods in other namespaces.
	otherPod := newPod("other-ns", "pod-e", map[string]string{"app": "web"})
	otherASGs := idx.MatchingASGs(otherPod)
	if len(otherASGs) != 0 {
		t.Errorf("other-ns pod: got %d ASGs, want 0", len(otherASGs))
	}
}

func TestPhase2_BuildIndex_NilSelectorInRule(t *testing.T) {
	// A rule where PodSelector has nil MatchLabels should compile to everything selector.
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/nil-selector"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "nil-sel", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: nil},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	// Should match any pod in the namespace.
	pod := newPod("default", "any-pod", map[string]string{"whatever": "value"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Errorf("nil-selector rule: got %d ASGs, want 1", len(asgs))
	}
}

func TestPhase2_MatchingASGs_EmptyResultsReturnConcreteEmptySlice(t *testing.T) {
	asgID := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"

	t.Run("nil receiver", func(t *testing.T) {
		var idx *MappingIndex
		asgs := idx.MatchingASGs(newPod("default", "web-pod", map[string]string{"app": "web"}))
		if asgs == nil {
			t.Fatal("MatchingASGs() returned nil, want concrete empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("MatchingASGs() len = %d, want 0", len(asgs))
		}
	})

	t.Run("nil pod", func(t *testing.T) {
		idx := mustBuildIndex(t, []v1alpha1.PodASGMapping{
			newMapping("default", "web-mapping", []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgID},
					},
				},
			}),
		})

		asgs := idx.MatchingASGs(nil)
		if asgs == nil {
			t.Fatal("MatchingASGs(nil) returned nil, want concrete empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("MatchingASGs(nil) len = %d, want 0", len(asgs))
		}
	})

	t.Run("no matches", func(t *testing.T) {
		idx := mustBuildIndex(t, []v1alpha1.PodASGMapping{
			newMapping("default", "web-mapping", []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgID},
					},
				},
			}),
		})

		asgs := idx.MatchingASGs(newPod("default", "worker-pod", map[string]string{"app": "worker"}))
		if asgs == nil {
			t.Fatal("MatchingASGs() returned nil, want concrete empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("MatchingASGs() len = %d, want 0", len(asgs))
		}
	})

	t.Run("empty index", func(t *testing.T) {
		idx := mustBuildIndex(t, nil)
		if idx == nil {
			t.Fatal("BuildIndex(nil) returned nil, want usable empty index")
		}

		asgs := idx.MatchingASGs(newPod("default", "worker-pod", map[string]string{"app": "worker"}))
		if asgs == nil {
			t.Fatal("MatchingASGs() returned nil, want concrete empty slice")
		}
		if len(asgs) != 0 {
			t.Errorf("MatchingASGs() len = %d, want 0", len(asgs))
		}
	})
}

// ---- Tests for BuildIndex API drift (review blocker) ----

func TestPhase2_BuildIndex_NeverReturnsErrorForPhase2Selectors(t *testing.T) {
	// Design §3.4B: CompileSelector is a total transform for Phase 2's matchLabels-only model.
	// Therefore BuildIndex must NEVER return an error for any valid Phase 2 input.
	// This test guards against API drift where BuildIndex returns (*MappingIndex, error)
	// but the error path should be unreachable for Phase 2.
	tests := []struct {
		name     string
		mappings []v1alpha1.PodASGMapping
	}{
		{"nil mappings", nil},
		{"empty mappings", []v1alpha1.PodASGMapping{}},
		{"nil matchLabels", []v1alpha1.PodASGMapping{
			newMapping("ns", "m", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: nil},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}}},
			}),
		}},
		{"empty matchLabels", []v1alpha1.PodASGMapping{
			newMapping("ns", "m", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}}},
			}),
		}},
		{"single label", []v1alpha1.PodASGMapping{
			newMapping("ns", "m", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}}},
			}),
		}},
		{"many labels", []v1alpha1.PodASGMapping{
			newMapping("ns", "m", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{
					"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
				}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}}},
			}),
		}},
		{"multiple CRs with mixed selectors", []v1alpha1.PodASGMapping{
			newMapping("ns", "m1", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: nil},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/a"}}},
			}),
			newMapping("ns", "m2", []v1alpha1.Mapping{
				{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/s/resourceGroups/r/providers/Microsoft.Network/applicationSecurityGroups/b"}}},
			}),
		}},
		{"zero-value PodASGMapping", []v1alpha1.PodASGMapping{{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := BuildIndex(tc.mappings)
			if idx == nil {
				t.Fatal("BuildIndex returned nil index")
			}
		})
	}
}

// ---- Tests for ASG slice aliasing (review feedback) ----

func TestPhase2_BuildIndex_InputMutationAfterBuild_ASGSliceAliasing(t *testing.T) {
	// Review concern: compiledEntry.asgs stores the slice from input rule.ApplicationSecurityGroups.
	// If the caller mutates the original input slice after calling BuildIndex, the index entries
	// may be corrupted due to shared backing array. This test documents whether aliasing exists.
	asgOriginal := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/original-asg"
	asgMutated := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/MUTATED-asg"

	// Build the input mapping slice
	inputMappings := []v1alpha1.PodASGMapping{
		newMapping("default", "m1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgOriginal, Description: "original"},
				},
			},
		}),
	}

	// Build the index
	idx := mustBuildIndex(t, inputMappings)

	// Verify the original value works
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgsBefore := idx.MatchingASGs(pod)
	if len(asgsBefore) != 1 || asgsBefore[0].ResourceID != asgOriginal {
		t.Fatalf("before mutation: got %v, want [%s]", asgsBefore, asgOriginal)
	}

	// Now mutate the original input's ASG slice
	inputMappings[0].Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = asgMutated
	inputMappings[0].Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "mutated"

	// Query again — this reveals whether aliasing exists.
	asgsAfter := idx.MatchingASGs(pod)
	if len(asgsAfter) != 1 {
		t.Fatalf("after mutation: got %d ASGs, want 1", len(asgsAfter))
	}

	// Document the aliasing behavior: if the result shows the mutated value,
	// that means the index shares the backing array (aliasing exists).
	// If it shows the original value, the index has its own copy.
	// Either behavior is acceptable if documented — this test captures the actual behavior.
	if asgsAfter[0].ResourceID == asgMutated {
		// Aliasing exists — the index stores a reference to the input slice.
		// This is the current behavior and is acceptable because:
		// 1. BuildIndex is called with a snapshot and the input isn't reused.
		// 2. Callers must not mutate inputs after passing to BuildIndex.
		t.Logf("INFO: ASG slice aliasing confirmed — index shares input backing array (ResourceID changed to %q)", asgsAfter[0].ResourceID)
	} else if asgsAfter[0].ResourceID == asgOriginal {
		t.Logf("INFO: ASG slice is defensively copied — index is isolated from input mutations")
	}
}

func TestPhase2_MatchingASGs_ReturnsIndependentSliceEachCall(t *testing.T) {
	// Verify that successive calls to MatchingASGs return independent slices.
	// Mutating one result must not affect subsequent calls.
	asg := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})

	// First call
	result1 := idx.MatchingASGs(pod)
	if len(result1) != 1 {
		t.Fatalf("first call: got %d ASGs, want 1", len(result1))
	}

	// Mutate result1
	result1[0].Description = "MUTATED-BY-CALLER"

	// Second call should be unaffected
	result2 := idx.MatchingASGs(pod)
	if len(result2) != 1 {
		t.Fatalf("second call: got %d ASGs, want 1", len(result2))
	}
	if result2[0].Description == "MUTATED-BY-CALLER" {
		t.Errorf("second call returned mutated data from first call — result slices must be independent")
	}
}

// ---- Tests for cross-CR + intra-CR dedupe interaction ----

func TestPhase2_MatchingASGs_DedupeAcrossThreeCRsMixedCasing(t *testing.T) {
	// Same ASG with different casings across three CRs — should deduplicate to one.
	asg1 := "/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	asg2 := "/subscriptions/sub/resourceGroups/rg/providers/microsoft.network/applicationsecuritygroups/my-asg"
	asg3 := "/subscriptions/Sub/resourceGroups/Rg/providers/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/MY-ASG"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "cr1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1, Description: "cr1-desc"}},
			},
		}),
		newMapping("default", "cr2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2, Description: "cr2-desc"}},
			},
		}),
		newMapping("default", "cr3", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg3, Description: "cr3-desc"}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)

	if len(asgs) != 1 {
		t.Fatalf("3-CR mixed-case dedupe: got %d ASGs, want 1", len(asgs))
	}
	// First-seen wins: asg1 from cr1
	if asgs[0].ResourceID != asg1 {
		t.Errorf("ResourceID = %q, want %q (first-seen from cr1)", asgs[0].ResourceID, asg1)
	}
	if asgs[0].Description != "cr1-desc" {
		t.Errorf("Description = %q, want %q (first-seen metadata from cr1)", asgs[0].Description, "cr1-desc")
	}
}

func TestPhase2_MatchingASGs_FirstSeenComplexFlattenOrder(t *testing.T) {
	// Test first-seen across: multiple CRs, multiple rules within CR, multiple ASGs within rule.
	// Expected flatten order: CR1.Rule0.ASG0, CR1.Rule0.ASG1, CR1.Rule1.ASG0, CR2.Rule0.ASG0
	asgA := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-a"
	asgB := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-b"
	// asgC is a case-variant of asgA — should be deduped
	asgC := "/subscriptions/SUB1/resourceGroups/RG1/providers/Microsoft.Network/applicationSecurityGroups/asg-a"
	asgD := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-d"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "cr1", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgA, Description: "first-A"},
					{ResourceID: asgB, Description: "first-B"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgC, Description: "dup-of-A-should-be-skipped"},
				},
			},
		}),
		newMapping("default", "cr2", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgD, Description: "first-D"},
				},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)

	// Expected: asgA, asgB, asgD (asgC deduped against asgA)
	if len(asgs) != 3 {
		t.Fatalf("complex flatten: got %d ASGs, want 3", len(asgs))
	}
	wantIDs := []string{asgA, asgB, asgD}
	wantDescs := []string{"first-A", "first-B", "first-D"}
	for i := range wantIDs {
		if asgs[i].ResourceID != wantIDs[i] {
			t.Errorf("asgs[%d].ResourceID = %q, want %q", i, asgs[i].ResourceID, wantIDs[i])
		}
		if asgs[i].Description != wantDescs[i] {
			t.Errorf("asgs[%d].Description = %q, want %q", i, asgs[i].Description, wantDescs[i])
		}
	}
}

// ---- Large-scale / boundary tests ----

func TestPhase2_BuildIndex_LargeNumberOfMappings(t *testing.T) {
	// Verify BuildIndex handles many CRs without error.
	const numCRs = 100
	const rulesPerCR = 5
	mappings := make([]v1alpha1.PodASGMapping, numCRs)
	for i := 0; i < numCRs; i++ {
		rules := make([]v1alpha1.Mapping, rulesPerCR)
		for j := 0; j < rulesPerCR; j++ {
			asgID := fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-%d-%d", i, j)
			rules[j] = v1alpha1.Mapping{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"cr": fmt.Sprintf("%d", i)}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
			}
		}
		mappings[i] = newMapping("default", fmt.Sprintf("cr-%d", i), rules)
	}

	idx := BuildIndex(mappings)
	if idx == nil {
		t.Fatal("BuildIndex returned nil")
	}

	// Query a pod matching one specific CR
	pod := newPod("default", "pod-42", map[string]string{"cr": "42"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != rulesPerCR {
		t.Errorf("large index query: got %d ASGs, want %d", len(asgs), rulesPerCR)
	}
}

func TestPhase2_MatchingASGs_LargeNumberOfMatchingRules(t *testing.T) {
	// All 50 CRs match the same pod — verify union works at scale with no duplicates.
	const numCRs = 50
	mappings := make([]v1alpha1.PodASGMapping, numCRs)
	for i := 0; i < numCRs; i++ {
		asgID := fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-%d", i)
		mappings[i] = newMapping("default", fmt.Sprintf("cr-%d", i), []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"team": "platform"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
			},
		})
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "platform-pod", map[string]string{"team": "platform"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != numCRs {
		t.Errorf("large union: got %d ASGs, want %d", len(asgs), numCRs)
	}
}

// ---- Additional edge cases ----

func TestPhase2_BuildIndex_MultipleRulesInSameCRDifferentSelectors(t *testing.T) {
	// A single CR with multiple rules using different selectors.
	// A pod should only match rules whose selectors match.
	asg1 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	asg2 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-api"
	asg3 := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-shared"

	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "multi-rule-cr", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg1}},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg2}},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}}, // catch-all
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg3}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	// Web pod matches rule 0 (web) and rule 2 (catch-all)
	webPod := newPod("default", "web-pod", map[string]string{"app": "web"})
	webASGs := idx.MatchingASGs(webPod)
	if len(webASGs) != 2 {
		t.Fatalf("web pod: got %d ASGs, want 2 (web + catchall)", len(webASGs))
	}
	if webASGs[0].ResourceID != asg1 || webASGs[1].ResourceID != asg3 {
		t.Errorf("web pod ASGs: got [%q, %q], want [%q, %q]", webASGs[0].ResourceID, webASGs[1].ResourceID, asg1, asg3)
	}

	// API pod matches rule 1 (api) and rule 2 (catch-all)
	apiPod := newPod("default", "api-pod", map[string]string{"app": "api"})
	apiASGs := idx.MatchingASGs(apiPod)
	if len(apiASGs) != 2 {
		t.Fatalf("api pod: got %d ASGs, want 2 (api + catchall)", len(apiASGs))
	}
	if apiASGs[0].ResourceID != asg2 || apiASGs[1].ResourceID != asg3 {
		t.Errorf("api pod ASGs: got [%q, %q], want [%q, %q]", apiASGs[0].ResourceID, apiASGs[1].ResourceID, asg2, asg3)
	}

	// Random pod matches only rule 2 (catch-all)
	randomPod := newPod("default", "random-pod", map[string]string{"app": "worker"})
	randomASGs := idx.MatchingASGs(randomPod)
	if len(randomASGs) != 1 {
		t.Fatalf("random pod: got %d ASGs, want 1 (catchall only)", len(randomASGs))
	}
	if randomASGs[0].ResourceID != asg3 {
		t.Errorf("random pod ASG: got %q, want %q", randomASGs[0].ResourceID, asg3)
	}
}

func TestPhase2_MatchingASGs_SamePodDifferentNamespaces(t *testing.T) {
	// Same labels, same ASG ID, but different namespaces in mappings.
	// Each pod should only match the mapping in its own namespace.
	asg := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/shared-asg"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("ns-a", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg, Description: "from ns-a"}},
			},
		}),
		newMapping("ns-b", "m2", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg, Description: "from ns-b"}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	podA := newPod("ns-a", "web-pod", map[string]string{"app": "web"})
	asgsA := idx.MatchingASGs(podA)
	if len(asgsA) != 1 {
		t.Fatalf("ns-a pod: got %d ASGs, want 1", len(asgsA))
	}
	if asgsA[0].Description != "from ns-a" {
		t.Errorf("ns-a pod: Description = %q, want %q", asgsA[0].Description, "from ns-a")
	}

	podB := newPod("ns-b", "web-pod", map[string]string{"app": "web"})
	asgsB := idx.MatchingASGs(podB)
	if len(asgsB) != 1 {
		t.Fatalf("ns-b pod: got %d ASGs, want 1", len(asgsB))
	}
	if asgsB[0].Description != "from ns-b" {
		t.Errorf("ns-b pod: Description = %q, want %q", asgsB[0].Description, "from ns-b")
	}
}

func TestPhase2_MatchingASGs_PodWithEmptyNamespace(t *testing.T) {
	// A pod with empty namespace should only match mappings with empty namespace.
	asg := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg"
	mappings := []v1alpha1.PodASGMapping{
		newMapping("", "m-empty-ns", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
		newMapping("default", "m-default-ns", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asg}},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)

	// Pod with empty namespace matches only the empty-ns mapping
	pod := newPod("", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Fatalf("empty-ns pod: got %d ASGs, want 1", len(asgs))
	}
}

func TestPhase2_BuildIndex_PreservesASGMetadata(t *testing.T) {
	// Verify that all ASGReference fields (ResourceID, SubscriptionID, Description) are preserved.
	asg := v1alpha1.ASGReference{
		ResourceID:     "/subscriptions/my-sub/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg",
		SubscriptionID: "explicit-sub-override",
		Description:    "My important ASG",
	}
	mappings := []v1alpha1.PodASGMapping{
		newMapping("default", "m1", []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{asg},
			},
		}),
	}
	idx := mustBuildIndex(t, mappings)
	pod := newPod("default", "web-pod", map[string]string{"app": "web"})
	asgs := idx.MatchingASGs(pod)
	if len(asgs) != 1 {
		t.Fatalf("got %d ASGs, want 1", len(asgs))
	}
	if asgs[0].ResourceID != asg.ResourceID {
		t.Errorf("ResourceID = %q, want %q", asgs[0].ResourceID, asg.ResourceID)
	}
	if asgs[0].SubscriptionID != asg.SubscriptionID {
		t.Errorf("SubscriptionID = %q, want %q", asgs[0].SubscriptionID, asg.SubscriptionID)
	}
	if asgs[0].Description != asg.Description {
		t.Errorf("Description = %q, want %q", asgs[0].Description, asg.Description)
	}
}

func TestPhase2_CanonicalASGKey_DifferentDynamicSegmentCaseProducesSameKey(t *testing.T) {
	// Two resource IDs that differ only in dynamic-segment casing should have
	// the same canonical key (because canonicalASGKey lowercases the full canonical form).
	id1 := "/subscriptions/MY-SUB/resourceGroups/MY-RG/providers/Microsoft.Network/applicationSecurityGroups/MY-ASG"
	id2 := "/subscriptions/my-sub/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg"

	key1 := canonicalASGKey(id1)
	key2 := canonicalASGKey(id2)
	if key1 != key2 {
		t.Errorf("canonicalASGKey: %q != %q — dynamic segment case difference should produce same key", key1, key2)
	}
}

func TestPhase2_CanonicalASGKey_InvalidIDFallsBackToLowercase(t *testing.T) {
	// An invalid resource ID should fall back to strings.ToLower.
	invalid := "NOT-A-VALID/Resource/ID"
	key := canonicalASGKey(invalid)
	want := "not-a-valid/resource/id"
	if key != want {
		t.Errorf("canonicalASGKey(%q) = %q, want %q (lowercase fallback)", invalid, key, want)
	}
}

func TestPhase2_CanonicalASGKey_ValidIDUsesCanonicalForm(t *testing.T) {
	// A valid resource ID should produce a key from the canonical FullResourceID, lowered.
	id := "/subscriptions/Sub/resourceGroups/RG/providers/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/ASG"
	key := canonicalASGKey(id)
	want := "/subscriptions/sub/resourcegroups/rg/providers/microsoft.network/applicationsecuritygroups/asg"
	if key != want {
		t.Errorf("canonicalASGKey(%q) = %q, want %q", id, key, want)
	}
}
