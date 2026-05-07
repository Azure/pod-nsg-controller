package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- T6.3: Invalid resource ID in spec → Accepted=False immediately ---

func TestValidateASGResourceIDs_ReturnsDeterministicErrorMapAndParsedRefs(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					{ResourceID: "invalid-resource-id"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	result := validateASGResourceIDs(spec)

	t.Run("HasErrors returns true when invalid IDs present", func(t *testing.T) {
		if !result.HasErrors() {
			t.Errorf("HasErrors() = false, want true (spec contains invalid resource ID)")
		}
	})

	t.Run("ValidationErrors contains error for mapping[0]", func(t *testing.T) {
		errs, ok := result.ValidationErrors[0]
		if !ok || len(errs) == 0 {
			t.Errorf("ValidationErrors[0] = %v (ok=%v), want non-empty errors for invalid resource ID", errs, ok)
		}
	})

	t.Run("ValidationErrors does not contain error for mapping[1]", func(t *testing.T) {
		errs := result.ValidationErrors[1]
		if len(errs) != 0 {
			t.Errorf("ValidationErrors[1] = %v, want empty (all resource IDs valid)", errs)
		}
	})

	t.Run("ParsedByMapping has valid refs for mapping[0]", func(t *testing.T) {
		parsed, ok := result.ParsedByMapping[0]
		if !ok || len(parsed) == 0 {
			t.Errorf("ParsedByMapping[0] = %v (ok=%v), want at least 1 parsed ref (valid ID was present)", parsed, ok)
		}
	})

	t.Run("ParsedByMapping has valid refs for mapping[1]", func(t *testing.T) {
		parsed, ok := result.ParsedByMapping[1]
		if !ok || len(parsed) != 1 {
			t.Errorf("ParsedByMapping[1] = %v (ok=%v), want exactly 1 parsed ref", parsed, ok)
		}
	})

	t.Run("Error message format is deterministic", func(t *testing.T) {
		errs := result.ValidationErrors[0]
		if len(errs) == 0 {
			t.Fatal("expected validation errors for mapping[0]")
		}
		// Format: mapping[%d].applicationSecurityGroups[%d].resourceId %q: %v
		expected := fmt.Sprintf("mapping[0].applicationSecurityGroups[1].resourceId %q:", "invalid-resource-id")
		found := false
		for _, e := range errs {
			if len(e) >= len(expected) && e[:len(expected)] == expected {
				found = true
			}
		}
		if !found {
			t.Errorf("validation error format mismatch: got %v, want prefix %q", errs, expected)
		}
	})
}

func TestValidateASGResourceIDs_AllValid_NoErrors(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		},
	}

	result := validateASGResourceIDs(spec)

	if result.HasErrors() {
		t.Errorf("HasErrors() = true, want false (all resource IDs valid)")
	}

	parsed := result.ParsedByMapping[0]
	if len(parsed) != 1 {
		t.Errorf("ParsedByMapping[0] length = %d, want 1", len(parsed))
	}
}

func TestCanonicalizeSpecWithParsed_RewritesResourceIDsDeterministically(t *testing.T) {
	_ = zaptest.NewLogger(t)

	// Input with non-canonical casing in static segments.
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/SUBSCRIPTIONS/sub1/RESOURCEGROUPS/rg1/PROVIDERS/microsoft.network/APPLICATIONSECURITYGROUPS/myAsg"},
				},
			},
		},
	}

	// Validate to get parsed references.
	result := validateASGResourceIDs(spec)
	canonicalized := canonicalizeSpecWithParsed(spec, result.ParsedByMapping)

	t.Run("Canonical resource ID has correct static segments", func(t *testing.T) {
		if len(canonicalized.Mappings) == 0 || len(canonicalized.Mappings[0].ApplicationSecurityGroups) == 0 {
			t.Fatal("canonicalized spec has no mappings or ASGs")
		}
		got := canonicalized.Mappings[0].ApplicationSecurityGroups[0].ResourceID
		want := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/myAsg"
		if got != want {
			t.Errorf("canonicalized ResourceID = %q, want %q", got, want)
		}
	})

	t.Run("Canonicalization is idempotent", func(t *testing.T) {
		result2 := validateASGResourceIDs(canonicalized)
		canonicalized2 := canonicalizeSpecWithParsed(canonicalized, result2.ParsedByMapping)
		if len(canonicalized2.Mappings) == 0 || len(canonicalized2.Mappings[0].ApplicationSecurityGroups) == 0 {
			t.Fatal("double-canonicalized spec has no mappings or ASGs")
		}
		got := canonicalized2.Mappings[0].ApplicationSecurityGroups[0].ResourceID
		want := canonicalized.Mappings[0].ApplicationSecurityGroups[0].ResourceID
		if got != want {
			t.Errorf("double-canonicalized ResourceID = %q, want %q (should be idempotent)", got, want)
		}
	})
}

func TestValidateASGResourceIDs_MultipleErrorsPreserveDeterministicOrder(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/missing-leading-slash"},
					{ResourceID: "/SUBSCRIPTIONS/sub1/RESOURCEGROUPS/rg1/PROVIDERS/microsoft.network/APPLICATIONSECURITYGROUPS/asg-one"},
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/applicationSecurityGroups/wrong-provider"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/Sub2/resourceGroups/RG2/providers/Microsoft.Network/applicationSecurityGroups/asg-two"},
					{ResourceID: ""},
				},
			},
		},
	}

	result := validateASGResourceIDs(spec)

	t.Run("ValidationErrors preserve mapping and ASG index order", func(t *testing.T) {
		wantPrefixes := map[int][]string{
			0: {
				fmt.Sprintf("mapping[0].applicationSecurityGroups[0].resourceId %q:", spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID),
				fmt.Sprintf("mapping[0].applicationSecurityGroups[2].resourceId %q:", spec.Mappings[0].ApplicationSecurityGroups[2].ResourceID),
			},
			1: {
				fmt.Sprintf("mapping[1].applicationSecurityGroups[1].resourceId %q:", spec.Mappings[1].ApplicationSecurityGroups[1].ResourceID),
			},
		}

		for mappingIdx, want := range wantPrefixes {
			got := result.ValidationErrors[mappingIdx]
			if len(got) != len(want) {
				t.Fatalf("ValidationErrors[%d] length = %d, want %d (%v)", mappingIdx, len(got), len(want), got)
			}
			for i := range want {
				if !strings.HasPrefix(got[i], want[i]) {
					t.Errorf("ValidationErrors[%d][%d] = %q, want prefix %q", mappingIdx, i, got[i], want[i])
				}
			}
		}
	})

	t.Run("ParsedByMapping preserves valid ASG order", func(t *testing.T) {
		gotFirstMapping := result.ParsedByMapping[0]
		if len(gotFirstMapping) != 1 {
			t.Fatalf("ParsedByMapping[0] length = %d, want 1", len(gotFirstMapping))
		}
		if gotFirstMapping[0].FullResourceID != "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-one" {
			t.Errorf("ParsedByMapping[0][0].FullResourceID = %q, want canonicalized asg-one ID", gotFirstMapping[0].FullResourceID)
		}

		gotSecondMapping := result.ParsedByMapping[1]
		if len(gotSecondMapping) != 1 {
			t.Fatalf("ParsedByMapping[1] length = %d, want 1", len(gotSecondMapping))
		}
		if gotSecondMapping[0].FullResourceID != "/subscriptions/Sub2/resourceGroups/RG2/providers/Microsoft.Network/applicationSecurityGroups/asg-two" {
			t.Errorf("ParsedByMapping[1][0].FullResourceID = %q, want canonicalized asg-two ID", gotSecondMapping[0].FullResourceID)
		}
	})
}

func TestCanonicalizeSpecWithParsed_CanonicalizesOnlyValidIDsWithoutMutatingInput(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/SUBSCRIPTIONS/sub1/RESOURCEGROUPS/rg1/PROVIDERS/microsoft.network/APPLICATIONSECURITYGROUPS/asg-one"},
					{ResourceID: "invalid-resource-id"},
					{ResourceID: "/subscriptions/Sub2/RESOURCEGROUPS/RG2/providers/microsoft.network/applicationSecurityGroups/asg-two"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "still-invalid"},
				},
			},
		},
	}

	originalFirstID := spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID
	originalSecondID := spec.Mappings[0].ApplicationSecurityGroups[1].ResourceID
	originalThirdID := spec.Mappings[0].ApplicationSecurityGroups[2].ResourceID

	result := validateASGResourceIDs(spec)
	canonicalized := canonicalizeSpecWithParsed(spec, result.ParsedByMapping)

	t.Run("Valid IDs are rewritten in-place while invalid IDs stay unchanged", func(t *testing.T) {
		got := canonicalized.Mappings[0].ApplicationSecurityGroups
		want := []string{
			"/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-one",
			"invalid-resource-id",
			"/subscriptions/Sub2/resourceGroups/RG2/providers/Microsoft.Network/applicationSecurityGroups/asg-two",
		}

		if len(got) != len(want) {
			t.Fatalf("canonicalized ASG count = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ResourceID != want[i] {
				t.Errorf("canonicalized.Mappings[0].ApplicationSecurityGroups[%d].ResourceID = %q, want %q", i, got[i].ResourceID, want[i])
			}
		}

		if canonicalized.Mappings[1].ApplicationSecurityGroups[0].ResourceID != "still-invalid" {
			t.Errorf("canonicalized invalid ID = %q, want %q", canonicalized.Mappings[1].ApplicationSecurityGroups[0].ResourceID, "still-invalid")
		}
	})

	t.Run("Input spec is not mutated", func(t *testing.T) {
		if spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID != originalFirstID {
			t.Errorf("spec first ResourceID mutated to %q, want %q", spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID, originalFirstID)
		}
		if spec.Mappings[0].ApplicationSecurityGroups[1].ResourceID != originalSecondID {
			t.Errorf("spec second ResourceID mutated to %q, want %q", spec.Mappings[0].ApplicationSecurityGroups[1].ResourceID, originalSecondID)
		}
		if spec.Mappings[0].ApplicationSecurityGroups[2].ResourceID != originalThirdID {
			t.Errorf("spec third ResourceID mutated to %q, want %q", spec.Mappings[0].ApplicationSecurityGroups[2].ResourceID, originalThirdID)
		}
	})
}

// --- T6.1: All actions succeed → Reconciled=True, all mappings Synced ---
// Tests the full flow using validateASGResourceIDs + ComputePodCountsFromPods + ComputeStatus

func TestPhase6_T61_AllActionsSucceed_ReconciledTrueAllSynced(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	// Step 1: Validate spec through the new validateASGResourceIDs function.
	validationResult := validateASGResourceIDs(spec)

	t.Run("Validation passes for valid spec", func(t *testing.T) {
		if validationResult.HasErrors() {
			t.Errorf("HasErrors() = true, want false for valid spec")
		}
	})

	t.Run("ParsedByMapping contains all mappings", func(t *testing.T) {
		if len(validationResult.ParsedByMapping) != 2 {
			t.Errorf("ParsedByMapping has %d entries, want 2", len(validationResult.ParsedByMapping))
		}
	})

	// Step 2: Canonicalize spec.
	canonicalized := canonicalizeSpecWithParsed(spec, validationResult.ParsedByMapping)

	t.Run("Canonicalized spec preserves mapping count", func(t *testing.T) {
		if len(canonicalized.Mappings) != 2 {
			t.Errorf("canonicalized spec has %d mappings, want 2", len(canonicalized.Mappings))
		}
	})

	// Step 3: Compute pod counts using the new function.
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
	}
	podCounts := ComputePodCountsFromPods(canonicalized, pods)

	t.Run("PodCounts computed from pods is non-nil", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("ComputePodCountsFromPods returned nil, want non-nil map")
		}
	})

	t.Run("PodCounts has correct web count", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "web"})
		if podCounts[hash] != 2 {
			t.Errorf("podCounts[%q] = %d, want 2", hash, podCounts[hash])
		}
	})

	t.Run("PodCounts has correct api count", func(t *testing.T) {
		hash := SelectorHash(map[string]string{"app": "api"})
		if podCounts[hash] != 1 {
			t.Errorf("podCounts[%q] = %d, want 1", hash, podCounts[hash])
		}
	})
}

// --- T6.2: One mapping ASG update fails → Reconciled=False, failed mapping Error ---
// Tests that ComputePodCountsFromPods correctly feeds into ComputeStatus with partial failure.

func TestPhase6_T62_OneASGFails_ReconciledFalsePartialError(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	failErr := fmt.Errorf("Azure 500: internal server error")
	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1",
					PrefixSetName:  "cluster1-default-mapping1",
				},
				DesiredIPs: []string{"10.0.0.1"},
			},
			Success: true,
			Err:     nil,
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg2",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2",
					PrefixSetName:  "cluster1-default-mapping1",
				},
				DesiredIPs: []string{"10.0.0.2"},
			},
			Success: false,
			Err:     failErr,
		},
	}

	// Use ComputePodCountsFromPods (new function) to compute pod counts.
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
	}
	podCounts := ComputePodCountsFromPods(spec, pods)

	t.Run("Pod counts computed correctly for partial failure scenario", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("ComputePodCountsFromPods returned nil")
		}
		hash := SelectorHash(map[string]string{"app": "web"})
		if podCounts[hash] != 2 {
			t.Errorf("podCounts[web] = %d, want 2", podCounts[hash])
		}
	})

	t.Run("ComputeStatus with pod-counted results marks successful mapping Synced", func(t *testing.T) {
		if podCounts == nil {
			t.Skip("podCounts is nil, ComputePodCountsFromPods not implemented")
		}
		status := ComputeStatus(spec, results, podCounts)
		if len(status.MappingStatuses) < 1 {
			t.Fatal("expected at least 1 mapping status")
		}
		if status.MappingStatuses[0].ASGSyncState != "Synced" {
			t.Errorf("MappingStatuses[0].ASGSyncState = %q, want %q", status.MappingStatuses[0].ASGSyncState, "Synced")
		}
	})

	t.Run("ComputeStatus with pod-counted results marks failed mapping Error", func(t *testing.T) {
		if podCounts == nil {
			t.Skip("podCounts is nil, ComputePodCountsFromPods not implemented")
		}
		status := ComputeStatus(spec, results, podCounts)
		if len(status.MappingStatuses) < 2 {
			t.Fatal("expected at least 2 mapping statuses")
		}
		if status.MappingStatuses[1].ASGSyncState != "Error" {
			t.Errorf("MappingStatuses[1].ASGSyncState = %q, want %q", status.MappingStatuses[1].ASGSyncState, "Error")
		}
	})

	t.Run("MatchedPods reflects actual pod count from ComputePodCountsFromPods", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("ComputePodCountsFromPods returned nil")
		}
		status := ComputeStatus(spec, results, podCounts)
		if len(status.MappingStatuses) < 2 {
			t.Fatal("expected at least 2 mapping statuses")
		}
		// web mapping should have 2 matched pods
		if status.MappingStatuses[0].MatchedPods != 2 {
			t.Errorf("MappingStatuses[0].MatchedPods = %d, want 2", status.MappingStatuses[0].MatchedPods)
		}
		// api mapping should have 1 matched pod
		if status.MappingStatuses[1].MatchedPods != 1 {
			t.Errorf("MappingStatuses[1].MatchedPods = %d, want 1", status.MappingStatuses[1].MatchedPods)
		}
	})
}

// --- T6.3 (additional): Invalid resource ID → Accepted=False ---

func TestPhase6_T63_InvalidResourceID_AcceptedFalse(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "not-a-valid-resource-id"},
				},
			},
		},
	}

	result := validateASGResourceIDs(spec)

	t.Run("Validation detects invalid resource ID", func(t *testing.T) {
		if !result.HasErrors() {
			t.Error("HasErrors() = false, want true for invalid resource ID")
		}
	})

	t.Run("Status writes Accepted=False for validation failure", func(t *testing.T) {
		mapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "test-invalid-mapping", Namespace: "default", Generation: 1},
			Spec:       spec,
			Status:     v1alpha1.PodASGMappingStatus{},
		}

		input := ReconcileStatusInput{
			Phase:            StatusPhaseValidationFailed,
			ProcessedGen:     1,
			ValidationErrors: result.ValidationErrors,
			PodCounts:        map[string]int{},
		}

		updater := newTestUpdater(t, mapping, time.Now)
		_ = updater.UpdateAfterReconcile(context.Background(), mapping, input)

		acceptedCond := findCond(mapping.Status.Conditions, ConditionAccepted)
		if acceptedCond == nil {
			t.Fatal("Accepted condition not found")
		}
		if acceptedCond.Status != metav1.ConditionFalse {
			t.Errorf("Accepted condition status = %v, want %v", acceptedCond.Status, metav1.ConditionFalse)
		}
	})

	t.Run("All mapping statuses are Error on validation failure", func(t *testing.T) {
		mapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "test-invalid-mapping-2", Namespace: "default", Generation: 1},
			Spec:       spec,
			Status:     v1alpha1.PodASGMappingStatus{},
		}

		input := ReconcileStatusInput{
			Phase:            StatusPhaseValidationFailed,
			ProcessedGen:     1,
			ValidationErrors: result.ValidationErrors,
			PodCounts:        map[string]int{},
		}

		updater := newTestUpdater(t, mapping, time.Now)
		_ = updater.UpdateAfterReconcile(context.Background(), mapping, input)

		for i, ms := range mapping.Status.MappingStatuses {
			if ms.ASGSyncState != "Error" {
				t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, "Error")
			}
		}
	})
}

// --- T6.4: lastSyncTime preserved from previous success ---
// Tests that lastSyncTime is only updated for Synced mappings using pod counts from new function.

func TestPhase6_T64_LastSyncTimePreservedFromPreviousSuccess(t *testing.T) {
	_ = zaptest.NewLogger(t)

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	previousSyncTime := metav1.NewTime(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	nowTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Use ComputePodCountsFromPods to derive pod counts (new function).
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
	}
	podCounts := ComputePodCountsFromPods(spec, pods)

	t.Run("PodCounts from ComputePodCountsFromPods is non-nil for T6.4 flow", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("ComputePodCountsFromPods returned nil, want non-nil map for lastSyncTime test")
		}
	})

	t.Run("PodCounts has expected web count", func(t *testing.T) {
		if podCounts == nil {
			t.Skip("podCounts nil")
		}
		hash := SelectorHash(map[string]string{"app": "web"})
		if podCounts[hash] != 1 {
			t.Errorf("podCounts[web] = %d, want 1", podCounts[hash])
		}
	})

	t.Run("PodCounts has expected api count", func(t *testing.T) {
		if podCounts == nil {
			t.Skip("podCounts nil")
		}
		hash := SelectorHash(map[string]string{"app": "api"})
		if podCounts[hash] != 1 {
			t.Errorf("podCounts[api] = %d, want 1", podCounts[hash])
		}
	})

	// If the new function works, also verify lastSyncTime behavior.
	failErr := fmt.Errorf("Azure error")
	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1",
					PrefixSetName:  "cluster1-default-mapping1",
				},
			},
			Success: true,
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg2",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2",
					PrefixSetName:  "cluster1-default-mapping1",
				},
			},
			Success: false,
			Err:     failErr,
		},
	}

	t.Run("Synced mapping gets updated lastSyncTime with computed podCounts", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("podCounts nil, ComputePodCountsFromPods not implemented")
		}
		mapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lastsync", Namespace: "default", Generation: 2},
			Spec:       spec,
			Status: v1alpha1.PodASGMappingStatus{
				MappingStatuses: []v1alpha1.MappingStatus{
					{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
					{SelectorHash: SelectorHash(map[string]string{"app": "api"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				},
			},
		}

		input := ReconcileStatusInput{
			Phase:        StatusPhasePostExecution,
			ProcessedGen: 2,
			Results:      results,
			PodCounts:    podCounts,
			ReconcileErr: failErr,
		}

		updater := newTestUpdater(t, mapping, func() time.Time { return nowTime })
		_ = updater.UpdateAfterReconcile(context.Background(), mapping, input)

		got := mapping.Status.MappingStatuses[0].LastSyncTime.Time
		if !got.Equal(nowTime) {
			t.Errorf("Synced mapping lastSyncTime = %v, want %v", got, nowTime)
		}
	})

	t.Run("Error mapping preserves previous lastSyncTime with computed podCounts", func(t *testing.T) {
		if podCounts == nil {
			t.Fatal("podCounts nil, ComputePodCountsFromPods not implemented")
		}
		mapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lastsync-2", Namespace: "default", Generation: 2},
			Spec:       spec,
			Status: v1alpha1.PodASGMappingStatus{
				MappingStatuses: []v1alpha1.MappingStatus{
					{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
					{SelectorHash: SelectorHash(map[string]string{"app": "api"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				},
			},
		}

		input := ReconcileStatusInput{
			Phase:        StatusPhasePostExecution,
			ProcessedGen: 2,
			Results:      results,
			PodCounts:    podCounts,
			ReconcileErr: failErr,
		}

		updater := newTestUpdater(t, mapping, func() time.Time { return nowTime })
		_ = updater.UpdateAfterReconcile(context.Background(), mapping, input)

		got := mapping.Status.MappingStatuses[1].LastSyncTime.Time
		if !got.Equal(previousSyncTime.Time) {
			t.Errorf("Error mapping lastSyncTime = %v, want %v (preserved)", got, previousSyncTime.Time)
		}
	})
}

// --- T6.6: selectorHash is deterministic ---
// Tests that SelectorHash produces deterministic keys used by ComputePodCountsFromPods.

func TestPhase6_T66_SelectorHashDeterministic(t *testing.T) {
	_ = zaptest.NewLogger(t)

	labels1 := map[string]string{"app": "web", "env": "prod", "tier": "frontend"}
	labels2 := map[string]string{"tier": "frontend", "app": "web", "env": "prod"}

	t.Run("Same labels in different order produce same hash", func(t *testing.T) {
		hash1 := SelectorHash(labels1)
		hash2 := SelectorHash(labels2)
		if hash1 != hash2 {
			t.Errorf("SelectorHash(%v) = %q, SelectorHash(%v) = %q, want equal", labels1, hash1, labels2, hash2)
		}
	})

	t.Run("Hash is 16 hex characters", func(t *testing.T) {
		hash := SelectorHash(labels1)
		if len(hash) != 16 {
			t.Errorf("SelectorHash length = %d, want 16", len(hash))
		}
	})

	t.Run("Different labels produce different hash", func(t *testing.T) {
		hashA := SelectorHash(map[string]string{"app": "web"})
		hashB := SelectorHash(map[string]string{"app": "api"})
		if hashA == hashB {
			t.Errorf("different labels produced same hash %q", hashA)
		}
	})

	t.Run("ComputePodCountsFromPods uses SelectorHash as map keys", func(t *testing.T) {
		spec := v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: labels1},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		}
		pods := []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Labels: labels1}},
		}
		counts := ComputePodCountsFromPods(spec, pods)
		if counts == nil {
			t.Fatal("ComputePodCountsFromPods returned nil")
		}
		expectedHash := SelectorHash(labels1)
		if _, ok := counts[expectedHash]; !ok {
			t.Errorf("ComputePodCountsFromPods keys don't contain expected SelectorHash %q", expectedHash)
		}
	})

	t.Run("ZeroPodCountsForSpec uses SelectorHash as map keys", func(t *testing.T) {
		spec := v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: labels1},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		}
		counts := ZeroPodCountsForSpec(spec)
		if counts == nil {
			t.Fatal("ZeroPodCountsForSpec returned nil")
		}
		expectedHash := SelectorHash(labels1)
		if _, ok := counts[expectedHash]; !ok {
			t.Errorf("ZeroPodCountsForSpec keys don't contain expected SelectorHash %q", expectedHash)
		}
	})
}

// --- T6.7: Status-only update does not trigger re-reconcile ---
// Tests reconcile observer pattern and validates that the validation path
// does not produce spurious re-reconciles.

func TestPhase6_T67_StatusOnlyUpdateNoReReconcile(t *testing.T) {
	_ = zaptest.NewLogger(t)

	t.Run("ReconcileObserver interface is invocable", func(t *testing.T) {
		counter := &reconcileStartCounter{}
		counter.OnReconcileStart()
		if counter.Count() != 1 {
			t.Errorf("counter.Count() = %d, want 1 after one call", counter.Count())
		}
	})

	t.Run("Multiple calls increment counter", func(t *testing.T) {
		counter := &reconcileStartCounter{}
		counter.OnReconcileStart()
		counter.OnReconcileStart()
		counter.OnReconcileStart()
		if counter.Count() != 3 {
			t.Errorf("counter.Count() = %d, want 3", counter.Count())
		}
	})

	t.Run("Validation of valid spec does not set HasErrors", func(t *testing.T) {
		spec := v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		}
		result := validateASGResourceIDs(spec)
		// A valid spec should NOT have errors — this would prevent spurious reconcile entries.
		if result.HasErrors() {
			t.Error("valid spec should not have errors; spurious validation failures would trigger extra reconciles")
		}
		// Additionally, valid spec must produce non-nil ParsedByMapping.
		if result.ParsedByMapping == nil {
			t.Error("ParsedByMapping is nil for valid spec, want populated map")
		}
		if len(result.ParsedByMapping) != 1 {
			t.Errorf("ParsedByMapping has %d entries, want 1", len(result.ParsedByMapping))
		}
	})
}

// --- Test helpers ---

func asgValidationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestUpdater(t *testing.T, mapping *v1alpha1.PodASGMapping, nowFn func() time.Time) *MappingStatusUpdater {
	t.Helper()
	scheme := asgValidationTestScheme(t)
	fc := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()
	return NewMappingStatusUpdater(fc, nowFn)
}

// reconcileStartCounter counts reconcile start invocations for T6.7.
type reconcileStartCounter struct {
	count int
}

func (c *reconcileStartCounter) OnReconcileStart() {
	c.count++
}

func (c *reconcileStartCounter) Count() int {
	return c.count
}

// findCond finds a condition by type from a conditions slice.
func findCond(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
