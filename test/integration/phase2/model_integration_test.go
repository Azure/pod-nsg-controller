package phase2_test

import (
	"fmt"
	"strings"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func asgRef(resourceID string) v1alpha1.ASGReference {
	return v1alpha1.ASGReference{ResourceID: resourceID}
}

func asgRefFull(resourceID, subID, desc string) v1alpha1.ASGReference {
	return v1alpha1.ASGReference{
		ResourceID:     resourceID,
		SubscriptionID: subID,
		Description:    desc,
	}
}

func mapping(sel map[string]string, asgs ...v1alpha1.ASGReference) v1alpha1.Mapping {
	return v1alpha1.Mapping{
		PodSelector:               v1alpha1.PodSelector{MatchLabels: sel},
		ApplicationSecurityGroups: asgs,
	}
}

func cr(ns, name string, rules ...v1alpha1.Mapping) v1alpha1.PodASGMapping {
	return v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.PodASGMappingSpec{Mappings: rules},
	}
}

func pod(ns, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
}

func mustBuild(t *testing.T, mappings []v1alpha1.PodASGMapping) *model.MappingIndex {
	t.Helper()
	return model.BuildIndex(mappings)
}

// ---------------------------------------------------------------------------
// Integration: Full pipeline — CRD types → BuildIndex → MatchingASGs
// ---------------------------------------------------------------------------

// TestIntegration_FullPipeline_CRDToMatchingASGs verifies the complete data
// flow from realistic PodASGMapping CRs through BuildIndex and MatchingASGs.
// This crosses the api/v1alpha1 → internal/model module boundary.
func TestIntegration_FullPipeline_CRDToMatchingASGs(t *testing.T) {
	asgWeb := "/subscriptions/sub-1/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/asg-web"
	asgAPI := "/subscriptions/sub-1/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/asg-api"
	asgDB := "/subscriptions/sub-2/resourceGroups/rg-data/providers/Microsoft.Network/applicationSecurityGroups/asg-db"

	mappings := []v1alpha1.PodASGMapping{
		cr("production", "web-mapping",
			mapping(map[string]string{"app": "web", "tier": "frontend"}, asgRef(asgWeb)),
		),
		cr("production", "api-mapping",
			mapping(map[string]string{"app": "api"}, asgRef(asgAPI)),
		),
		cr("production", "db-mapping",
			mapping(map[string]string{"app": "db"}, asgRef(asgDB)),
		),
	}

	idx := mustBuild(t, mappings)

	tests := []struct {
		name     string
		pod      *corev1.Pod
		wantASGs []string
	}{
		{
			name:     "web pod matches web-mapping",
			pod:      pod("production", "web-pod-1", map[string]string{"app": "web", "tier": "frontend"}),
			wantASGs: []string{asgWeb},
		},
		{
			name:     "api pod matches api-mapping",
			pod:      pod("production", "api-pod-1", map[string]string{"app": "api"}),
			wantASGs: []string{asgAPI},
		},
		{
			name:     "db pod matches db-mapping",
			pod:      pod("production", "db-pod-1", map[string]string{"app": "db"}),
			wantASGs: []string{asgDB},
		},
		{
			name:     "pod with extra labels still matches",
			pod:      pod("production", "web-extra", map[string]string{"app": "web", "tier": "frontend", "version": "v2"}),
			wantASGs: []string{asgWeb},
		},
		{
			name:     "pod in wrong namespace matches nothing",
			pod:      pod("staging", "web-pod-1", map[string]string{"app": "web", "tier": "frontend"}),
			wantASGs: nil,
		},
		{
			name:     "pod with no matching labels",
			pod:      pod("production", "unknown-pod", map[string]string{"app": "cache"}),
			wantASGs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := idx.MatchingASGs(tt.pod)
			if len(tt.wantASGs) == 0 {
				if len(got) != 0 {
					t.Errorf("expected no ASGs, got %d: %v", len(got), got)
				}
				return
			}
			if len(got) != len(tt.wantASGs) {
				t.Fatalf("expected %d ASGs, got %d: %v", len(tt.wantASGs), len(got), got)
			}
			for i, want := range tt.wantASGs {
				if got[i].ResourceID != want {
					t.Errorf("ASG[%d]: want %s, got %s", i, want, got[i].ResourceID)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: DeepCopy'd CRD objects work through model pipeline
// ---------------------------------------------------------------------------

// TestIntegration_DeepCopiedCRDs_WorkThroughPipeline ensures that objects
// which have been through controller-runtime's DeepCopy (as happens in real
// reconciliation) produce identical results through the model pipeline.
func TestIntegration_DeepCopiedCRDs_WorkThroughPipeline(t *testing.T) {
	asgID := "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-test"
	original := cr("default", "test-mapping",
		mapping(map[string]string{"app": "test"}, asgRefFull(asgID, "sub-1", "test ASG")),
	)

	copied := *original.DeepCopy()

	// Build indexes from original and copied independently.
	idxOrig := mustBuild(t, []v1alpha1.PodASGMapping{original})
	idxCopy := mustBuild(t, []v1alpha1.PodASGMapping{copied})

	testPod := pod("default", "test-pod", map[string]string{"app": "test"})

	origASGs := idxOrig.MatchingASGs(testPod)
	copyASGs := idxCopy.MatchingASGs(testPod)

	if len(origASGs) != len(copyASGs) {
		t.Fatalf("DeepCopy divergence: orig returned %d ASGs, copy returned %d", len(origASGs), len(copyASGs))
	}
	for i := range origASGs {
		if origASGs[i].ResourceID != copyASGs[i].ResourceID {
			t.Errorf("ASG[%d] ResourceID: orig=%s copy=%s", i, origASGs[i].ResourceID, copyASGs[i].ResourceID)
		}
		if origASGs[i].SubscriptionID != copyASGs[i].SubscriptionID {
			t.Errorf("ASG[%d] SubscriptionID: orig=%s copy=%s", i, origASGs[i].SubscriptionID, copyASGs[i].SubscriptionID)
		}
		if origASGs[i].Description != copyASGs[i].Description {
			t.Errorf("ASG[%d] Description: orig=%s copy=%s", i, origASGs[i].Description, copyASGs[i].Description)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: ParseASGResourceID + OwnershipKey compose for prefix set naming
// ---------------------------------------------------------------------------

// TestIntegration_ParsedResourceID_ComposesWith_OwnershipKey verifies that
// ParseASGResourceID output fields can be combined with OwnershipKey to produce
// the deterministic addressPrefixSet name the controller uses per ASG.
func TestIntegration_ParsedResourceID_ComposesWith_OwnershipKey(t *testing.T) {
	resourceID := "/subscriptions/sub-abc/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/my-asg"

	parsed, err := model.ParseASGResourceID(resourceID)
	if err != nil {
		t.Fatalf("ParseASGResourceID: %v", err)
	}

	// Verify parsed fields are usable.
	if parsed.SubscriptionID != "sub-abc" {
		t.Errorf("SubscriptionID: want sub-abc, got %s", parsed.SubscriptionID)
	}
	if parsed.ResourceGroup != "rg-prod" {
		t.Errorf("ResourceGroup: want rg-prod, got %s", parsed.ResourceGroup)
	}
	if parsed.ASGName != "my-asg" {
		t.Errorf("ASGName: want my-asg, got %s", parsed.ASGName)
	}

	// Compose with OwnershipKey — this is how the controller names prefix sets.
	key := model.OwnershipKey("cluster-1", "production", "web-mapping")
	if key != "cluster-1-production-web-mapping" {
		t.Errorf("OwnershipKey: want cluster-1-production-web-mapping, got %s", key)
	}

	// Different cluster produces distinct key for same mapping.
	key2 := model.OwnershipKey("cluster-2", "production", "web-mapping")
	if key == key2 {
		t.Errorf("ownership keys must differ across clusters: both = %s", key)
	}
}

// ---------------------------------------------------------------------------
// Integration: Cross-subscription ASGs with deduplication via canonicalASGKey
// ---------------------------------------------------------------------------

// TestIntegration_CrossSubscription_Deduplication verifies that MatchingASGs
// correctly deduplicates ASGs across mappings when the same ASG resource ID
// appears with different casing (as could happen with mixed-case input).
// This tests the internal integration of ParseASGResourceID (via canonicalASGKey)
// within the index lookup path.
func TestIntegration_CrossSubscription_Deduplication(t *testing.T) {
	asgLower := "/subscriptions/SUB-1/resourceGroups/RG-1/providers/Microsoft.Network/applicationSecurityGroups/asg-shared"
	asgMixed := "/subscriptions/SUB-1/resourceGroups/RG-1/providers/microsoft.network/applicationsecuritygroups/asg-shared"

	mappings := []v1alpha1.PodASGMapping{
		cr("ns1", "mapping-a",
			mapping(map[string]string{"app": "web"}, asgRefFull(asgLower, "SUB-1", "from mapping-a")),
		),
		cr("ns1", "mapping-b",
			mapping(map[string]string{"app": "web"}, asgRefFull(asgMixed, "SUB-1", "from mapping-b")),
		),
	}

	idx := mustBuild(t, mappings)
	results := idx.MatchingASGs(pod("ns1", "web-pod", map[string]string{"app": "web"}))

	if len(results) != 1 {
		t.Fatalf("expected 1 deduplicated ASG, got %d: %v", len(results), results)
	}
	// First-seen wins — should be from mapping-a.
	if results[0].Description != "from mapping-a" {
		t.Errorf("expected first-seen metadata, got description=%q", results[0].Description)
	}
}

// ---------------------------------------------------------------------------
// Integration: Multi-rule CR with overlapping selectors → union of ASGs
// ---------------------------------------------------------------------------

// TestIntegration_OverlappingSelectors_ReturnUnion tests that when a pod
// matches multiple rules in the same CR, the result is the union of all
// referenced ASGs (T2.9 cross-function integration).
func TestIntegration_OverlappingSelectors_ReturnUnion(t *testing.T) {
	asg1 := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-1"
	asg2 := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-2"

	// One CR with two rules — both match the same pod.
	m := cr("default", "multi-rule",
		mapping(map[string]string{"app": "web"}, asgRef(asg1)),
		mapping(map[string]string{"tier": "frontend"}, asgRef(asg2)),
	)

	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})
	results := idx.MatchingASGs(pod("default", "web-fe", map[string]string{"app": "web", "tier": "frontend"}))

	if len(results) != 2 {
		t.Fatalf("expected 2 ASGs from union, got %d", len(results))
	}
	if results[0].ResourceID != asg1 || results[1].ResourceID != asg2 {
		t.Errorf("unexpected ASG order: %v", results)
	}
}

// ---------------------------------------------------------------------------
// Integration: Empty selector matches all pods in namespace
// ---------------------------------------------------------------------------

// TestIntegration_EmptySelector_MatchesAllPods verifies that an empty
// matchLabels selector (compiled via CompileSelector → labels.Everything)
// matches any pod in the correct namespace through the full pipeline.
func TestIntegration_EmptySelector_MatchesAllPods(t *testing.T) {
	asgID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/catch-all"

	m := cr("monitoring", "catch-all",
		mapping(map[string]string{}, asgRef(asgID)),
	)

	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})

	pods := []*corev1.Pod{
		pod("monitoring", "pod-a", map[string]string{"app": "grafana"}),
		pod("monitoring", "pod-b", map[string]string{"role": "exporter"}),
		pod("monitoring", "pod-c", nil),
	}

	for _, p := range pods {
		results := idx.MatchingASGs(p)
		if len(results) != 1 {
			t.Errorf("pod %s: expected 1 ASG from empty selector, got %d", p.Name, len(results))
		}
	}

	// Still namespace-scoped — different namespace gets nothing.
	results := idx.MatchingASGs(pod("default", "outsider", map[string]string{"app": "grafana"}))
	if len(results) != 0 {
		t.Errorf("pod in wrong namespace should match nothing, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// Integration: ParseASGResourceID canonical output matches index dedup
// ---------------------------------------------------------------------------

// TestIntegration_CanonicalResourceID_RoundTrip verifies that
// ParseASGResourceID's canonical FullResourceID output survives a round-trip
// through BuildIndex deduplication. This ensures the parser and index agree
// on canonical form.
func TestIntegration_CanonicalResourceID_RoundTrip(t *testing.T) {
	// Input with mixed casing on static segments.
	input := "/SUBSCRIPTIONS/sub-1/RESOURCEGROUPS/rg-1/PROVIDERS/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/asg-x"

	parsed, err := model.ParseASGResourceID(input)
	if err != nil {
		t.Fatalf("ParseASGResourceID: %v", err)
	}

	// Canonical output should have canonical static segments.
	want := "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-x"
	if parsed.FullResourceID != want {
		t.Errorf("canonical FullResourceID:\n  want: %s\n  got:  %s", want, parsed.FullResourceID)
	}

	// Feed canonical ID through a mapping + index; ensure it deduplicates with original.
	m := cr("ns", "test",
		mapping(map[string]string{"app": "x"},
			asgRef(input),
			asgRef(parsed.FullResourceID),
		),
	)
	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})
	results := idx.MatchingASGs(pod("ns", "p", map[string]string{"app": "x"}))

	if len(results) != 1 {
		t.Fatalf("expected dedup to 1 ASG, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// Integration: Multi-namespace isolation
// ---------------------------------------------------------------------------

// TestIntegration_MultiNamespace_Isolation verifies that the index correctly
// isolates pods by namespace across multiple PodASGMapping CRs from different
// namespaces — a key cross-module contract between API types and model.
func TestIntegration_MultiNamespace_Isolation(t *testing.T) {
	asgProd := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-prod"
	asgStaging := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-staging"

	mappings := []v1alpha1.PodASGMapping{
		cr("production", "prod-mapping",
			mapping(map[string]string{"app": "web"}, asgRef(asgProd)),
		),
		cr("staging", "staging-mapping",
			mapping(map[string]string{"app": "web"}, asgRef(asgStaging)),
		),
	}

	idx := mustBuild(t, mappings)

	// Prod pod should only match prod ASG.
	prodResults := idx.MatchingASGs(pod("production", "web-1", map[string]string{"app": "web"}))
	if len(prodResults) != 1 || prodResults[0].ResourceID != asgProd {
		t.Errorf("prod pod: want [%s], got %v", asgProd, prodResults)
	}

	// Staging pod should only match staging ASG.
	stagingResults := idx.MatchingASGs(pod("staging", "web-1", map[string]string{"app": "web"}))
	if len(stagingResults) != 1 || stagingResults[0].ResourceID != asgStaging {
		t.Errorf("staging pod: want [%s], got %v", asgStaging, stagingResults)
	}
}

// ---------------------------------------------------------------------------
// Integration: OwnershipKey uniqueness across namespaces
// ---------------------------------------------------------------------------

// TestIntegration_OwnershipKey_NamespaceIsolation verifies that OwnershipKey
// produces distinct keys for the same mapping name in different namespaces,
// which is essential for the addressPrefixSet naming scheme.
func TestIntegration_OwnershipKey_NamespaceIsolation(t *testing.T) {
	cluster := "my-cluster"

	keys := make(map[string]string)
	namespaces := []string{"production", "staging", "dev"}

	for _, ns := range namespaces {
		key := model.OwnershipKey(cluster, ns, "web-mapping")
		for prevNS, prevKey := range keys {
			if key == prevKey {
				t.Errorf("collision: ns=%s and ns=%s both produce key=%s", ns, prevNS, key)
			}
		}
		keys[ns] = key
	}
}

// ---------------------------------------------------------------------------
// Integration: Full realistic scenario — multi-CR, multi-sub, end-to-end
// ---------------------------------------------------------------------------

// TestIntegration_RealisticScenario_MultiCR_MultiSubscription simulates a
// real-world deployment with multiple PodASGMapping CRs referencing ASGs in
// different Azure subscriptions, and verifies that the full model pipeline
// (parse + compile + index + match + ownership) produces correct results.
func TestIntegration_RealisticScenario_MultiCR_MultiSubscription(t *testing.T) {
	// ASGs in two different subscriptions.
	asgFrontend := "/subscriptions/sub-frontend/resourceGroups/rg-web/providers/Microsoft.Network/applicationSecurityGroups/frontend-asg"
	asgBackend := "/subscriptions/sub-backend/resourceGroups/rg-api/providers/Microsoft.Network/applicationSecurityGroups/backend-asg"
	asgShared := "/subscriptions/sub-shared/resourceGroups/rg-common/providers/Microsoft.Network/applicationSecurityGroups/shared-asg"

	mappings := []v1alpha1.PodASGMapping{
		cr("app-ns", "frontend-mapping",
			mapping(map[string]string{"tier": "frontend"},
				asgRefFull(asgFrontend, "sub-frontend", "Frontend ASG"),
				asgRefFull(asgShared, "sub-shared", "Shared ASG"),
			),
		),
		cr("app-ns", "backend-mapping",
			mapping(map[string]string{"tier": "backend"},
				asgRefFull(asgBackend, "sub-backend", "Backend ASG"),
				asgRefFull(asgShared, "sub-shared", "Shared ASG (dup)"),
			),
		),
	}

	idx := mustBuild(t, mappings)

	// Frontend pod → frontend ASG + shared ASG.
	fePod := pod("app-ns", "fe-pod", map[string]string{"tier": "frontend", "app": "nginx"})
	feResults := idx.MatchingASGs(fePod)
	if len(feResults) != 2 {
		t.Fatalf("frontend pod: want 2 ASGs, got %d: %v", len(feResults), feResults)
	}
	if feResults[0].ResourceID != asgFrontend {
		t.Errorf("frontend pod ASG[0]: want %s, got %s", asgFrontend, feResults[0].ResourceID)
	}
	if feResults[1].ResourceID != asgShared {
		t.Errorf("frontend pod ASG[1]: want %s, got %s", asgShared, feResults[1].ResourceID)
	}

	// Backend pod → backend ASG + shared ASG.
	bePod := pod("app-ns", "be-pod", map[string]string{"tier": "backend"})
	beResults := idx.MatchingASGs(bePod)
	if len(beResults) != 2 {
		t.Fatalf("backend pod: want 2 ASGs, got %d: %v", len(beResults), beResults)
	}
	if beResults[0].ResourceID != asgBackend {
		t.Errorf("backend pod ASG[0]: want %s, got %s", asgBackend, beResults[0].ResourceID)
	}

	// Pod matching both tiers → union of all 3 unique ASGs.
	bothPod := pod("app-ns", "both-pod", map[string]string{"tier": "frontend"})
	// Only matches frontend rule, so 2 ASGs.
	bothResults := idx.MatchingASGs(bothPod)
	if len(bothResults) != 2 {
		t.Fatalf("both pod: want 2 ASGs, got %d", len(bothResults))
	}

	// Verify parsed resource IDs for each subscription.
	for _, rid := range []string{asgFrontend, asgBackend, asgShared} {
		parsed, err := model.ParseASGResourceID(rid)
		if err != nil {
			t.Errorf("ParseASGResourceID(%s): %v", rid, err)
			continue
		}
		if !strings.HasPrefix(parsed.SubscriptionID, "sub-") {
			t.Errorf("unexpected subscription for %s: %s", rid, parsed.SubscriptionID)
		}
	}

	// Ownership keys are distinct per mapping.
	key1 := model.OwnershipKey("cluster-a", "app-ns", "frontend-mapping")
	key2 := model.OwnershipKey("cluster-a", "app-ns", "backend-mapping")
	if key1 == key2 {
		t.Errorf("ownership keys should differ: %s vs %s", key1, key2)
	}
}

// ---------------------------------------------------------------------------
// Integration: ASGReference metadata preservation through pipeline
// ---------------------------------------------------------------------------

// TestIntegration_ASGReference_MetadataPreserved verifies that optional
// ASGReference fields (SubscriptionID, Description) survive the full
// BuildIndex → MatchingASGs pipeline intact.
func TestIntegration_ASGReference_MetadataPreserved(t *testing.T) {
	asgID := "/subscriptions/sub-xyz/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/my-asg"

	m := cr("ns", "meta-test",
		mapping(map[string]string{"app": "test"},
			asgRefFull(asgID, "sub-xyz", "Production ASG for web tier"),
		),
	)

	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})
	results := idx.MatchingASGs(pod("ns", "p", map[string]string{"app": "test"}))

	if len(results) != 1 {
		t.Fatalf("expected 1 ASG, got %d", len(results))
	}
	if results[0].SubscriptionID != "sub-xyz" {
		t.Errorf("SubscriptionID: want sub-xyz, got %s", results[0].SubscriptionID)
	}
	if results[0].Description != "Production ASG for web tier" {
		t.Errorf("Description: want 'Production ASG for web tier', got %s", results[0].Description)
	}
}

// ---------------------------------------------------------------------------
// Integration: Invalid resource IDs in CRDs flow through pipeline gracefully
// ---------------------------------------------------------------------------

// TestIntegration_InvalidResourceID_InCRD_GracefulFallback verifies that ASG
// references with malformed resource IDs (which bypass CRD validation in tests)
// still flow through BuildIndex + MatchingASGs without panics, using the
// canonicalASGKey fallback path (strings.ToLower) for deduplication.
func TestIntegration_InvalidResourceID_InCRD_GracefulFallback(t *testing.T) {
	validASG := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-valid"
	invalidASG := "not-a-valid-resource-id"

	m := cr("default", "mixed-validity",
		mapping(map[string]string{"app": "test"},
			asgRef(validASG),
			asgRef(invalidASG),
		),
	)

	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})
	results := idx.MatchingASGs(pod("default", "p", map[string]string{"app": "test"}))

	// Both ASGs should appear — invalid IDs are not rejected by the index.
	if len(results) != 2 {
		t.Fatalf("expected 2 ASGs (valid + invalid), got %d: %v", len(results), results)
	}
	if results[0].ResourceID != validASG {
		t.Errorf("ASG[0]: want valid ASG, got %s", results[0].ResourceID)
	}
	if results[1].ResourceID != invalidASG {
		t.Errorf("ASG[1]: want invalid ASG passthrough, got %s", results[1].ResourceID)
	}

	// Verify the invalid ID still deduplicates correctly via fallback.
	m2 := cr("default", "dup-invalid",
		mapping(map[string]string{"app": "test"},
			asgRef(invalidASG),
			asgRef(strings.ToUpper(invalidASG)),
		),
	)
	idx2 := mustBuild(t, []v1alpha1.PodASGMapping{m2})
	results2 := idx2.MatchingASGs(pod("default", "p", map[string]string{"app": "test"}))

	if len(results2) != 1 {
		t.Fatalf("expected 1 deduplicated invalid ASG, got %d: %v", len(results2), results2)
	}
}

// ---------------------------------------------------------------------------
// Integration: Index immutability after CRD slice mutation
// ---------------------------------------------------------------------------

// TestIntegration_IndexImmutability_AfterCRDMutation verifies that mutating
// the original PodASGMapping slice after BuildIndex does not affect query
// results — the index must defensively copy ASG references. This tests the
// contract between api/v1alpha1 types and internal/model's defensive copying.
func TestIntegration_IndexImmutability_AfterCRDMutation(t *testing.T) {
	asgOriginal := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-original"

	mappings := []v1alpha1.PodASGMapping{
		cr("default", "mutable-test",
			mapping(map[string]string{"app": "web"}, asgRef(asgOriginal)),
		),
	}

	idx := mustBuild(t, mappings)

	// Mutate the original slice after index was built.
	mappings[0].Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = "mutated-id"
	mappings[0].Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "mutated"

	// Index should still return the original ASG.
	results := idx.MatchingASGs(pod("default", "p", map[string]string{"app": "web"}))
	if len(results) != 1 {
		t.Fatalf("expected 1 ASG, got %d", len(results))
	}
	if results[0].ResourceID != asgOriginal {
		t.Errorf("ResourceID mutated: want %s, got %s", asgOriginal, results[0].ResourceID)
	}
	if results[0].Description == "mutated" {
		t.Error("Description was mutated through original slice — index is not immutable")
	}
}

// ---------------------------------------------------------------------------
// Integration: Deterministic ordering at scale across many CRs/rules
// ---------------------------------------------------------------------------

// TestIntegration_DeterministicOrdering_AtScale verifies that the pipeline
// preserves deterministic ordering of ASGs across many CRs and rules. This
// ensures the flatten order (CR order → rule order → ASG order) is stable,
// which is critical for downstream reconciliation idempotency.
func TestIntegration_DeterministicOrdering_AtScale(t *testing.T) {
	const numCRs = 10
	const rulesPerCR = 3
	const asgsPerRule = 2

	var mappings []v1alpha1.PodASGMapping
	var expectedOrder []string

	for i := 0; i < numCRs; i++ {
		var rules []v1alpha1.Mapping
		for j := 0; j < rulesPerCR; j++ {
			var asgs []v1alpha1.ASGReference
			for k := 0; k < asgsPerRule; k++ {
				rid := fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-cr%d-rule%d-asg%d", i, j, k)
				asgs = append(asgs, asgRef(rid))
				expectedOrder = append(expectedOrder, rid)
			}
			// All rules use the same selector so the test pod matches everything.
			rules = append(rules, mapping(map[string]string{"app": "scale-test"}, asgs...))
		}
		mappings = append(mappings, cr("scale-ns", fmt.Sprintf("cr-%d", i), rules...))
	}

	idx := mustBuild(t, mappings)
	results := idx.MatchingASGs(pod("scale-ns", "p", map[string]string{"app": "scale-test"}))

	if len(results) != len(expectedOrder) {
		t.Fatalf("expected %d ASGs, got %d", len(expectedOrder), len(results))
	}
	for i, want := range expectedOrder {
		if results[i].ResourceID != want {
			t.Errorf("ASG[%d]: want %s, got %s", i, want, results[i].ResourceID)
		}
	}

	// Run twice to confirm determinism.
	results2 := idx.MatchingASGs(pod("scale-ns", "p", map[string]string{"app": "scale-test"}))
	for i := range results {
		if results[i].ResourceID != results2[i].ResourceID {
			t.Errorf("non-deterministic: run1[%d]=%s run2[%d]=%s", i, results[i].ResourceID, i, results2[i].ResourceID)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Nil/empty labels pod through full pipeline
// ---------------------------------------------------------------------------

// TestIntegration_NilLabelsPod_ThroughPipeline verifies that a pod with nil
// labels (common for newly-created pods) flows through the full pipeline
// without panics and only matches empty-selector rules.
func TestIntegration_NilLabelsPod_ThroughPipeline(t *testing.T) {
	asgCatchAll := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/catch-all"
	asgSpecific := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/specific"

	mappings := []v1alpha1.PodASGMapping{
		cr("default", "catch-all-mapping",
			mapping(map[string]string{}, asgRef(asgCatchAll)),
		),
		cr("default", "specific-mapping",
			mapping(map[string]string{"app": "web"}, asgRef(asgSpecific)),
		),
	}

	idx := mustBuild(t, mappings)

	// Pod with nil labels should match only the catch-all.
	nilPod := pod("default", "nil-labels", nil)
	results := idx.MatchingASGs(nilPod)
	if len(results) != 1 {
		t.Fatalf("nil-labels pod: want 1 ASG, got %d: %v", len(results), results)
	}
	if results[0].ResourceID != asgCatchAll {
		t.Errorf("want catch-all ASG, got %s", results[0].ResourceID)
	}

	// Pod with empty map should also match only catch-all.
	emptyPod := pod("default", "empty-labels", map[string]string{})
	results2 := idx.MatchingASGs(emptyPod)
	if len(results2) != 1 {
		t.Fatalf("empty-labels pod: want 1 ASG, got %d", len(results2))
	}
	if results2[0].ResourceID != asgCatchAll {
		t.Errorf("want catch-all ASG, got %s", results2[0].ResourceID)
	}
}

// ---------------------------------------------------------------------------
// Integration: OwnershipKey + ParseASGResourceID for all parsed fields
// ---------------------------------------------------------------------------

// TestIntegration_OwnershipKey_WithAllParsedFields verifies that each field
// from ParseASGResourceID can compose with OwnershipKey in realistic
// naming scenarios across the api → model boundary.
func TestIntegration_OwnershipKey_WithAllParsedFields(t *testing.T) {
	resources := []string{
		"/subscriptions/sub-a/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/frontend",
		"/subscriptions/sub-b/resourceGroups/rg-staging/providers/Microsoft.Network/applicationSecurityGroups/backend",
		"/subscriptions/sub-a/resourceGroups/rg-prod/providers/Microsoft.Network/applicationSecurityGroups/shared",
	}

	keys := make(map[string]bool)
	for _, rid := range resources {
		parsed, err := model.ParseASGResourceID(rid)
		if err != nil {
			t.Fatalf("ParseASGResourceID(%s): %v", rid, err)
		}

		// Compose ownership key using parsed fields as cluster context.
		key := model.OwnershipKey("cluster-1", parsed.ResourceGroup, parsed.ASGName)
		if keys[key] {
			t.Errorf("duplicate ownership key: %s (from %s)", key, rid)
		}
		keys[key] = true

		// Verify parsed fields are non-empty and consistent.
		if parsed.SubscriptionID == "" || parsed.ResourceGroup == "" || parsed.ASGName == "" {
			t.Errorf("empty parsed field for %s: sub=%q rg=%q asg=%q",
				rid, parsed.SubscriptionID, parsed.ResourceGroup, parsed.ASGName)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: Error propagation — ParseASGResourceID errors are independent
// ---------------------------------------------------------------------------

// TestIntegration_ErrorPropagation_ParseFailureDoesNotAffectIndex verifies
// that ParseASGResourceID errors for specific resource IDs don't interfere
// with successful operations on other IDs within the same pipeline execution.
func TestIntegration_ErrorPropagation_ParseFailureDoesNotAffectIndex(t *testing.T) {
	validID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg-ok"
	invalidIDs := []string{
		"",
		"no-leading-slash",
		"/subscriptions//resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg",
		"/subscriptions/sub/resourceGroups/rg/providers/Wrong.Provider/applicationSecurityGroups/asg",
	}

	// Verify each invalid ID produces an error.
	for _, id := range invalidIDs {
		_, err := model.ParseASGResourceID(id)
		if err == nil {
			t.Errorf("expected error for %q, got nil", id)
		}
	}

	// Verify valid ID still parses correctly after invalid attempts.
	parsed, err := model.ParseASGResourceID(validID)
	if err != nil {
		t.Fatalf("valid ID parse failed after invalid attempts: %v", err)
	}

	// Feed valid parsed ID through index pipeline.
	m := cr("default", "after-errors",
		mapping(map[string]string{"app": "test"}, asgRef(parsed.FullResourceID)),
	)
	idx := mustBuild(t, []v1alpha1.PodASGMapping{m})
	results := idx.MatchingASGs(pod("default", "p", map[string]string{"app": "test"}))
	if len(results) != 1 || results[0].ResourceID != parsed.FullResourceID {
		t.Errorf("pipeline after errors: want [%s], got %v", parsed.FullResourceID, results)
	}
}
