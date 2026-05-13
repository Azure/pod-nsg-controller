package controller

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeASGRef(subID, rg, name string) v1alpha1.ASGReference {
	return v1alpha1.ASGReference{
		ResourceID: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s", subID, rg, name),
	}
}

func makeSuccessResult(subID, rg, asgName, prefix string) azure.ActionResult {
	return azure.ActionResult{
		Action: engine.Action{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: subID,
				ResourceGroup:  rg,
				ASGName:        asgName,
				FullResourceID: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s", subID, rg, asgName),
				PrefixSetName:  prefix,
			},
		},
		Success: true,
	}
}

func makeFailedResult(subID, rg, asgName, prefix string, err error) azure.ActionResult {
	r := makeSuccessResult(subID, rg, asgName, prefix)
	r.Success = false
	r.Err = err
	return r
}

func conditionByType(status v1alpha1.PodASGMappingStatus, condType string) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// T6.1 — All actions succeed → Reconciled=True, all rows Synced
// ---------------------------------------------------------------------------

func TestComputeStatus_T61_AllActionsSucceed_ReconciledTrue_AllSynced(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	results := []azure.ActionResult{
		makeSuccessResult("sub1", "rg1", "asg1", prefix),
		makeSuccessResult("sub1", "rg1", "asg2", prefix),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{3, 2},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	// Conditions
	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionTrue {
		t.Errorf("Reconciled status = %q, want True", reconciled.Status)
	}

	accepted := conditionByType(got, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionTrue {
		t.Errorf("Accepted status = %q, want True", accepted.Status)
	}

	// MappingCount
	if got.MappingCount != 2 {
		t.Errorf("MappingCount = %d, want 2", got.MappingCount)
	}

	// All rows Synced
	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}
	for i, ms := range got.MappingStatuses {
		if ms.ASGSyncState != SyncStateSynced {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, SyncStateSynced)
		}
		if ms.Error != "" {
			t.Errorf("MappingStatuses[%d].Error = %q, want empty", i, ms.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.2 — Shared target failure marks all referencing mappings Error
// ---------------------------------------------------------------------------

func TestComputeStatus_T62_SharedTargetFailure_MarksAllReferencingMappingsError(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()

	// Two mappings share the same ASG target
	asgRef := makeASGRef("sub1", "rg1", "asg-shared")
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "a"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{asgRef},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "b"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{asgRef},
			},
		},
	}

	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg-shared", prefix, fmt.Errorf("Azure 429")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ReconcileErr:       fmt.Errorf("action failures: UpdatePrefixSet asg-shared: Azure 429"),
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled status = %q, want False", reconciled.Status)
	}

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}
	for i, ms := range got.MappingStatuses {
		if ms.ASGSyncState != SyncStateError {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, SyncStateError)
		}
		if ms.Error == "" {
			t.Errorf("MappingStatuses[%d].Error is empty, want failure message", i)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.2 supplement — failed ActionResults alone drive Reconciled=False
// ---------------------------------------------------------------------------

func TestComputeStatus_T62_FailedResultsWithoutReconcileErr_ReconciledFalse(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            []azure.ActionResult{makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("ARM 429"))},
		MatchedPodsByIndex: []int{1},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled status = %q, want False", reconciled.Status)
	}
	if reconciled.Reason != ReasonReconcileFailed {
		t.Errorf("Reconciled reason = %q, want %q", reconciled.Reason, ReasonReconcileFailed)
	}
	if reconciled.Message != "one or more actions failed" {
		t.Errorf("Reconciled message = %q, want %q", reconciled.Message, "one or more actions failed")
	}

	if len(got.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(got.MappingStatuses))
	}
	if got.MappingStatuses[0].ASGSyncState != SyncStateError {
		t.Errorf("MappingStatuses[0].ASGSyncState = %q, want %q", got.MappingStatuses[0].ASGSyncState, SyncStateError)
	}
	if got.MappingStatuses[0].Error == "" {
		t.Error("MappingStatuses[0].Error is empty, want failure message")
	}
}

// ---------------------------------------------------------------------------
// T6.2 — Multi-ASG mixed outcomes → Error with deterministic aggregated message
// ---------------------------------------------------------------------------

func TestComputeStatus_T62_MultiASGMixedOutcomes_AggregatesDeterministicError(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()

	// One mapping references two ASGs; one succeeds, one fails.
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "mixed"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					makeASGRef("sub1", "rg1", "asg-ok"),
					makeASGRef("sub1", "rg1", "asg-fail"),
				},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "clean"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg-ok")},
			},
		},
	}

	results := []azure.ActionResult{
		makeSuccessResult("sub1", "rg1", "asg-ok", prefix),
		makeFailedResult("sub1", "rg1", "asg-fail", prefix, fmt.Errorf("quota exceeded")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{2, 1},
		ReconcileErr:       fmt.Errorf("action failures: UpdatePrefixSet asg-fail: quota exceeded"),
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}

	// Row 0 has mixed outcomes → Error
	if got.MappingStatuses[0].ASGSyncState != SyncStateError {
		t.Errorf("MappingStatuses[0].ASGSyncState = %q, want %q", got.MappingStatuses[0].ASGSyncState, SyncStateError)
	}
	if got.MappingStatuses[0].Error == "" {
		t.Error("MappingStatuses[0].Error is empty, want aggregated failure message")
	}

	// Row 1 only references the successful ASG → Synced
	if got.MappingStatuses[1].ASGSyncState != SyncStateSynced {
		t.Errorf("MappingStatuses[1].ASGSyncState = %q, want %q", got.MappingStatuses[1].ASGSyncState, SyncStateSynced)
	}

	// Deterministic: call again and compare.
	got2 := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{2, 1},
		ReconcileErr:       fmt.Errorf("action failures: UpdatePrefixSet asg-fail: quota exceeded"),
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if got.MappingStatuses[0].Error != got2.MappingStatuses[0].Error {
		t.Errorf("error message not deterministic: %q vs %q",
			got.MappingStatuses[0].Error, got2.MappingStatuses[0].Error)
	}
}

// ---------------------------------------------------------------------------
// T6.3 — Invalid resource ID → Accepted=False immediately
// ---------------------------------------------------------------------------

func TestComputeStatus_T63_InvalidResourceID_AcceptedFalseImmediately(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "not-a-valid-id"}},
			},
		},
	}

	validationIssues := []ValidationIssue{
		{MappingIndex: 0, ASGIndex: 0, ResourceID: "not-a-valid-id", Err: fmt.Errorf("invalid resource ID")},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		ValidationIssues:   validationIssues,
		ReconcileErr:       fmt.Errorf("validation failed"),
		MatchedPodsByIndex: []int{0},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	accepted := conditionByType(got, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted status = %q, want False", accepted.Status)
	}

	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled status = %q, want False", reconciled.Status)
	}

	if len(got.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(got.MappingStatuses))
	}
	if got.MappingStatuses[0].ASGSyncState != SyncStateError {
		t.Errorf("MappingStatuses[0].ASGSyncState = %q, want %q", got.MappingStatuses[0].ASGSyncState, SyncStateError)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — lastSyncTime only advances on success (Synced rows)
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_LastSyncTime_OnlyUpdatedOnSynced(t *testing.T) {
	prefix := "cluster__ns__mapping"
	oldTime := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "ok"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg-ok")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "fail"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg-fail")},
			},
		},
	}

	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{ASGSyncState: SyncStateSynced, LastSyncTime: oldTime},
			{ASGSyncState: SyncStateSynced, LastSyncTime: oldTime},
		},
	}

	results := []azure.ActionResult{
		makeSuccessResult("sub1", "rg1", "asg-ok", prefix),
		makeFailedResult("sub1", "rg1", "asg-fail", prefix, fmt.Errorf("timeout")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ReconcileErr:       fmt.Errorf("action failures: timeout"),
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}

	// Row 0 succeeded → lastSyncTime should be updated to now.
	if !got.MappingStatuses[0].LastSyncTime.Equal(&now) {
		t.Errorf("Synced row lastSyncTime = %v, want %v", got.MappingStatuses[0].LastSyncTime, now)
	}

	// Row 1 failed → lastSyncTime should preserve previous value.
	if !got.MappingStatuses[1].LastSyncTime.Equal(&oldTime) {
		t.Errorf("Error row lastSyncTime = %v, want preserved %v", got.MappingStatuses[1].LastSyncTime, oldTime)
	}
}

// ---------------------------------------------------------------------------
// T6.5 — matchedPods count includes pods without IPs
// ---------------------------------------------------------------------------

func TestComputeStatus_T65_MatchedPods_IncludesNoIPMatchingPods(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: ""}, // no IP yet
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.3"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"app": "other"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.4"},
		},
	}

	got := ComputeMatchedPodsByMapping(spec, pods)

	if got == nil {
		t.Fatal("ComputeMatchedPodsByMapping returned nil")
	}
	if len(got) != 2 {
		t.Fatalf("result len = %d, want 2", len(got))
	}

	// Mapping 0 ("app=web") should match 2 pods (including the one without IP).
	if got[0] != 2 {
		t.Errorf("matched pods for mapping 0 = %d, want 2", got[0])
	}

	// Mapping 1 ("app=api") should match 1 pod.
	if got[1] != 1 {
		t.Errorf("matched pods for mapping 1 = %d, want 1", got[1])
	}
}

// ---------------------------------------------------------------------------
// T6.6 — selectorHash is deterministic regardless of map iteration order
// ---------------------------------------------------------------------------

func TestComputeStatus_T66_SelectorHash_DeterministicForMapOrder(t *testing.T) {
	// Go maps have non-deterministic iteration; same labels must produce same hash.
	labels1 := map[string]string{"app": "web", "env": "prod", "team": "platform"}
	labels2 := map[string]string{"team": "platform", "app": "web", "env": "prod"}

	h1 := ComputeSelectorHash(labels1)
	h2 := ComputeSelectorHash(labels2)

	if h1 == "" {
		t.Fatal("ComputeSelectorHash returned empty string")
	}

	if h1 != h2 {
		t.Errorf("hashes differ for same labels: %q vs %q", h1, h2)
	}

	// Stability: call multiple times.
	for i := 0; i < 10; i++ {
		h := ComputeSelectorHash(labels1)
		if h != h1 {
			t.Fatalf("hash not stable on iteration %d: %q vs %q", i, h, h1)
		}
	}

	// Different labels must produce different hash.
	labelsOther := map[string]string{"app": "api", "env": "prod", "team": "platform"}
	hOther := ComputeSelectorHash(labelsOther)
	if hOther == h1 {
		t.Errorf("different labels produced the same hash: %q", hOther)
	}
}

// ---------------------------------------------------------------------------
// Pending phase — all rows Pending, Accepted=True, Reconciled=Unknown
// ---------------------------------------------------------------------------

func TestComputeStatus_PendingPhase_AllRowsPendingReconciledUnknown(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		MatchedPodsByIndex: []int{3, 1},
		ObservedGeneration: 1,
		Phase:              StatusPhasePending,
		Now:                now,
	})

	// Conditions
	accepted := conditionByType(got, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %q, want True", accepted.Status)
	}

	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionUnknown {
		t.Errorf("Reconciled = %q, want Unknown", reconciled.Status)
	}
	if reconciled.Reason != ReasonReconciling {
		t.Errorf("Reconciled reason = %q, want %q", reconciled.Reason, ReasonReconciling)
	}

	// All rows Pending
	for i, ms := range got.MappingStatuses {
		if ms.ASGSyncState != SyncStatePending {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, SyncStatePending)
		}
		if ms.Error != "" {
			t.Errorf("MappingStatuses[%d].Error = %q, want empty", i, ms.Error)
		}
	}

	if got.MappingCount != 2 {
		t.Errorf("MappingCount = %d, want 2", got.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// T6.4 supplement — Pending phase preserves lastSyncTime from previous status
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_PendingPhasePreservesLastSyncTime(t *testing.T) {
	oldTime := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
		},
	}

	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{ASGSyncState: SyncStateSynced, LastSyncTime: oldTime},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		MatchedPodsByIndex: []int{2},
		ObservedGeneration: 2,
		Phase:              StatusPhasePending,
		Now:                now,
	})

	if len(got.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(got.MappingStatuses))
	}
	if !got.MappingStatuses[0].LastSyncTime.Equal(&oldTime) {
		t.Errorf("Pending row lastSyncTime = %v, want preserved %v", got.MappingStatuses[0].LastSyncTime, oldTime)
	}
}

// ---------------------------------------------------------------------------
// Systemic failure — reconcileErr + no results + no validation issues + Final
// ---------------------------------------------------------------------------

func TestComputeStatus_SystemicFailure_AllRowsError(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		MatchedPodsByIndex: []int{1, 2},
		ReconcileErr:       fmt.Errorf("failed to list actual state"),
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	reconciled := conditionByType(got, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled = %q, want False", reconciled.Status)
	}

	for i, ms := range got.MappingStatuses {
		if ms.ASGSyncState != SyncStateError {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, SyncStateError)
		}
		if ms.Error == "" {
			t.Errorf("MappingStatuses[%d].Error is empty, want systemic error message", i)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.3 supplement — mixed valid/invalid: rows WITHOUT issues also get Error
// when Accepted=False
// ---------------------------------------------------------------------------

func TestComputeStatus_T63_MixedValidInvalid_AllRowsError(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "good"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "not-valid"}},
			},
		},
	}

	validationIssues := []ValidationIssue{
		{MappingIndex: 1, ASGIndex: 0, ResourceID: "not-valid", Err: fmt.Errorf("invalid resource ID")},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		ValidationIssues:   validationIssues,
		ReconcileErr:       fmt.Errorf("validation failed"),
		MatchedPodsByIndex: []int{2, 0},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	accepted := conditionByType(got, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %q, want False", accepted.Status)
	}

	// Both rows should be Error when Accepted=False, even the valid one.
	for i, ms := range got.MappingStatuses {
		if ms.ASGSyncState != SyncStateError {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, SyncStateError)
		}
		if ms.Error == "" {
			t.Errorf("MappingStatuses[%d].Error is empty, want non-empty", i)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.6 — ComputeSelectorHash edge cases
// ---------------------------------------------------------------------------

func TestComputeSelectorHash_EmptyLabels(t *testing.T) {
	h := ComputeSelectorHash(map[string]string{})
	if h != "" {
		t.Errorf("ComputeSelectorHash(empty) = %q, want empty string", h)
	}
}

func TestComputeSelectorHash_NilLabels(t *testing.T) {
	h := ComputeSelectorHash(nil)
	if h != "" {
		t.Errorf("ComputeSelectorHash(nil) = %q, want empty string", h)
	}
}

func TestComputeSelectorHash_SingleKey(t *testing.T) {
	h := ComputeSelectorHash(map[string]string{"app": "web"})
	if h == "" {
		t.Fatal("ComputeSelectorHash returned empty string for single-key map")
	}
	// Must be stable
	h2 := ComputeSelectorHash(map[string]string{"app": "web"})
	if h != h2 {
		t.Errorf("not stable: %q vs %q", h, h2)
	}
}

// ---------------------------------------------------------------------------
// T6.5 — ComputeMatchedPodsByMapping edge cases
// ---------------------------------------------------------------------------

func TestComputeMatchedPodsByMapping_EmptyPodList(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}}},
		},
	}
	got := ComputeMatchedPodsByMapping(spec, nil)
	if len(got) != 1 {
		t.Fatalf("result len = %d, want 1", len(got))
	}
	if got[0] != 0 {
		t.Errorf("matched = %d, want 0 for empty pod list", got[0])
	}
}

func TestComputeMatchedPodsByMapping_EmptyMatchLabels_MatchesAll(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{}}},
		},
	}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "p2", Labels: map[string]string{"app": "api"}}},
	}
	got := ComputeMatchedPodsByMapping(spec, pods)
	if len(got) != 1 {
		t.Fatalf("result len = %d, want 1", len(got))
	}
	if got[0] != 2 {
		t.Errorf("matched = %d, want 2 (empty selector matches all)", got[0])
	}
}

func TestComputeMatchedPodsByMapping_EmptyMappings(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{Mappings: nil}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: map[string]string{"app": "web"}}},
	}
	got := ComputeMatchedPodsByMapping(spec, pods)
	if len(got) != 0 {
		t.Errorf("result len = %d, want 0 for empty mappings", len(got))
	}
}

// ---------------------------------------------------------------------------
// SelectorHash is populated on mapping status rows
// ---------------------------------------------------------------------------

func TestComputeStatus_SelectorHashPopulatedOnRows(t *testing.T) {
	now := metav1.Now()
	prefix := "cluster__ns__mapping"

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web", "env": "prod"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            []azure.ActionResult{makeSuccessResult("sub1", "rg1", "asg1", prefix)},
		MatchedPodsByIndex: []int{1},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(got.MappingStatuses))
	}

	expectedHash := ComputeSelectorHash(map[string]string{"app": "web", "env": "prod"})
	if got.MappingStatuses[0].SelectorHash != expectedHash {
		t.Errorf("SelectorHash = %q, want %q", got.MappingStatuses[0].SelectorHash, expectedHash)
	}
}

// ---------------------------------------------------------------------------
// T6.5 — MatchedPods values propagated to status rows
// ---------------------------------------------------------------------------

func TestComputeStatus_T65_MatchedPodsValuesOnRows(t *testing.T) {
	now := metav1.Now()
	prefix := "cluster__ns__mapping"

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            []azure.ActionResult{makeSuccessResult("sub1", "rg1", "asg1", prefix), makeSuccessResult("sub1", "rg1", "asg2", prefix)},
		MatchedPodsByIndex: []int{5, 0},
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if got.MappingStatuses[0].MatchedPods != 5 {
		t.Errorf("MappingStatuses[0].MatchedPods = %d, want 5", got.MappingStatuses[0].MatchedPods)
	}
	if got.MappingStatuses[1].MatchedPods != 0 {
		t.Errorf("MappingStatuses[1].MatchedPods = %d, want 0", got.MappingStatuses[1].MatchedPods)
	}
}

// ---------------------------------------------------------------------------
// Zero mappings edge case
// ---------------------------------------------------------------------------

func TestComputeStatus_ZeroMappings(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{Mappings: nil}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if got.MappingCount != 0 {
		t.Errorf("MappingCount = %d, want 0", got.MappingCount)
	}
	if len(got.MappingStatuses) != 0 {
		t.Errorf("MappingStatuses len = %d, want 0", len(got.MappingStatuses))
	}
}

// ---------------------------------------------------------------------------
// T6.4 — lastSyncTime preserved for validation-failure rows
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_LastSyncTimePreservedOnValidationFailure(t *testing.T) {
	oldTime := metav1.NewTime(time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "not-valid"}},
			},
		},
	}

	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{ASGSyncState: SyncStateSynced, LastSyncTime: oldTime},
		},
	}

	validationIssues := []ValidationIssue{
		{MappingIndex: 0, ASGIndex: 0, ResourceID: "not-valid", Err: fmt.Errorf("invalid")},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		ValidationIssues:   validationIssues,
		ReconcileErr:       fmt.Errorf("validation failed"),
		MatchedPodsByIndex: []int{0},
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if !got.MappingStatuses[0].LastSyncTime.Equal(&oldTime) {
		t.Errorf("Validation-failure row lastSyncTime = %v, want preserved %v",
			got.MappingStatuses[0].LastSyncTime, oldTime)
	}
}

// ---------------------------------------------------------------------------
// T6.1 — ObservedGeneration propagated to conditions
// ---------------------------------------------------------------------------

func TestComputeStatus_ObservedGenerationOnConditions(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()
	var gen int64 = 7

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            []azure.ActionResult{makeSuccessResult("sub1", "rg1", "asg1", prefix)},
		MatchedPodsByIndex: []int{1},
		ObservedGeneration: gen,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	for _, condType := range []string{ConditionAccepted, ConditionReconciled} {
		c := conditionByType(got, condType)
		if c == nil {
			t.Errorf("missing %s condition", condType)
			continue
		}
		if c.ObservedGeneration != gen {
			t.Errorf("%s.ObservedGeneration = %d, want %d", condType, c.ObservedGeneration, gen)
		}
	}
}

// ---------------------------------------------------------------------------
// MatchedPodsByIndex shorter than Mappings: out-of-range defaults to 0
// ---------------------------------------------------------------------------

func TestComputeStatus_MatchedPodsByIndexShorterThanMappings(t *testing.T) {
	prefix := "cluster__ns__mapping"
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "a"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "b"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PrefixSetName:      prefix,
		Results:            []azure.ActionResult{makeSuccessResult("sub1", "rg1", "asg1", prefix), makeSuccessResult("sub1", "rg1", "asg2", prefix)},
		MatchedPodsByIndex: []int{3}, // only 1 element for 2 mappings
		ObservedGeneration: 1,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if got.MappingStatuses[0].MatchedPods != 3 {
		t.Errorf("MappingStatuses[0].MatchedPods = %d, want 3", got.MappingStatuses[0].MatchedPods)
	}
	if got.MappingStatuses[1].MatchedPods != 0 {
		t.Errorf("MappingStatuses[1].MatchedPods = %d, want 0 (out of range)", got.MappingStatuses[1].MatchedPods)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — LastSyncTime preserved by selector hash on reorder
// Design §5.1: selector-hash-based preservation replaces index-based.
// When mappings [A, B] become [B, A], LastSyncTime must follow the selector.
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_LastSyncTimePreserved_BySelectorHash_OnReorder(t *testing.T) {
	prefix := "cluster__ns__mapping"
	timeA := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	timeB := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	labelsA := map[string]string{"app": "alpha"}
	labelsB := map[string]string{"app": "beta"}

	// Previous order: [A, B]
	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: ComputeSelectorHash(labelsA), ASGSyncState: SyncStateSynced, LastSyncTime: timeA},
			{SelectorHash: ComputeSelectorHash(labelsB), ASGSyncState: SyncStateSynced, LastSyncTime: timeB},
		},
	}

	// New order: [B, A] — reordered
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsB},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsA},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
		},
	}

	// Both fail → should preserve prior LastSyncTime by selector hash
	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg2", prefix, fmt.Errorf("timeout")),
		makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("timeout")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ReconcileErr:       fmt.Errorf("all failed"),
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}

	// Row 0 is now B → must have B's previous time (timeB), not A's (timeA)
	if !got.MappingStatuses[0].LastSyncTime.Equal(&timeB) {
		t.Errorf("Reordered row 0 (B) LastSyncTime = %v, want %v (preserved by selector hash)", got.MappingStatuses[0].LastSyncTime, timeB)
	}

	// Row 1 is now A → must have A's previous time (timeA), not B's (timeB)
	if !got.MappingStatuses[1].LastSyncTime.Equal(&timeA) {
		t.Errorf("Reordered row 1 (A) LastSyncTime = %v, want %v (preserved by selector hash)", got.MappingStatuses[1].LastSyncTime, timeA)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — LastSyncTime preserved by selector hash on insert
// When mappings [A, B] become [A, C, B], B's timestamp must follow its hash.
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_LastSyncTimePreserved_BySelectorHash_OnInsert(t *testing.T) {
	prefix := "cluster__ns__mapping"
	timeA := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	timeB := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	labelsA := map[string]string{"app": "alpha"}
	labelsB := map[string]string{"app": "beta"}
	labelsC := map[string]string{"app": "gamma"}

	// Previous: [A, B]
	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: ComputeSelectorHash(labelsA), ASGSyncState: SyncStateSynced, LastSyncTime: timeA},
			{SelectorHash: ComputeSelectorHash(labelsB), ASGSyncState: SyncStateSynced, LastSyncTime: timeB},
		},
	}

	// New: [A, C, B] — C inserted in the middle
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsA},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsC},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg3")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsB},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	// All fail → preserve times
	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("err")),
		makeFailedResult("sub1", "rg1", "asg3", prefix, fmt.Errorf("err")),
		makeFailedResult("sub1", "rg1", "asg2", prefix, fmt.Errorf("err")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1, 1},
		ReconcileErr:       fmt.Errorf("all failed"),
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 3 {
		t.Fatalf("MappingStatuses len = %d, want 3", len(got.MappingStatuses))
	}

	// Row 0 (A) → timeA
	if !got.MappingStatuses[0].LastSyncTime.Equal(&timeA) {
		t.Errorf("Row 0 (A) LastSyncTime = %v, want %v", got.MappingStatuses[0].LastSyncTime, timeA)
	}

	// Row 1 (C) → zero (no previous time, new mapping)
	if !got.MappingStatuses[1].LastSyncTime.IsZero() {
		t.Errorf("Row 1 (C) LastSyncTime = %v, want zero (new mapping)", got.MappingStatuses[1].LastSyncTime)
	}

	// Row 2 (B) → timeB (must follow selector hash, not index)
	if !got.MappingStatuses[2].LastSyncTime.Equal(&timeB) {
		t.Errorf("Row 2 (B) LastSyncTime = %v, want %v (preserved by selector hash, not index)", got.MappingStatuses[2].LastSyncTime, timeB)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — LastSyncTime preserved by selector hash on delete
// When mappings [A, B, C] become [A, C], C's timestamp must follow its hash.
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_LastSyncTimePreserved_BySelectorHash_OnDelete(t *testing.T) {
	prefix := "cluster__ns__mapping"
	timeA := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	timeB := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))
	timeC := metav1.NewTime(time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	labelsA := map[string]string{"app": "alpha"}
	labelsB := map[string]string{"app": "beta"}
	labelsC := map[string]string{"app": "gamma"}

	// Previous: [A, B, C]
	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: ComputeSelectorHash(labelsA), ASGSyncState: SyncStateSynced, LastSyncTime: timeA},
			{SelectorHash: ComputeSelectorHash(labelsB), ASGSyncState: SyncStateSynced, LastSyncTime: timeB},
			{SelectorHash: ComputeSelectorHash(labelsC), ASGSyncState: SyncStateSynced, LastSyncTime: timeC},
		},
	}

	// New: [A, C] — B deleted
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsA},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsC},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg3")},
			},
		},
	}

	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("err")),
		makeFailedResult("sub1", "rg1", "asg3", prefix, fmt.Errorf("err")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ReconcileErr:       fmt.Errorf("all failed"),
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}

	// Row 0 (A) → timeA
	if !got.MappingStatuses[0].LastSyncTime.Equal(&timeA) {
		t.Errorf("Row 0 (A) LastSyncTime = %v, want %v", got.MappingStatuses[0].LastSyncTime, timeA)
	}

	// Row 1 (C) → timeC (must use selector hash, not index; index 1 would give timeB)
	if !got.MappingStatuses[1].LastSyncTime.Equal(&timeC) {
		t.Errorf("Row 1 (C) LastSyncTime = %v, want %v (preserved by selector hash, not index)", got.MappingStatuses[1].LastSyncTime, timeC)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — Duplicate selector hash preserves deterministically via FIFO
// ---------------------------------------------------------------------------

func TestComputeStatus_T64_DuplicateSelectorHash_PreservesDeterministically_FIFO(t *testing.T) {
	prefix := "cluster__ns__mapping"
	timeX := metav1.NewTime(time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC))
	time1 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	time2 := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))

	// Same labels → same selector hash
	sameLabels := map[string]string{"app": "web"}
	hash := ComputeSelectorHash(sameLabels)
	otherLabels := map[string]string{"app": "other"}

	// Previous: [other, web, web] — different hash at index 0 shifts indices
	previousStatus := v1alpha1.PodASGMappingStatus{
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: ComputeSelectorHash(otherLabels), ASGSyncState: SyncStateSynced, LastSyncTime: timeX},
			{SelectorHash: hash, ASGSyncState: SyncStateSynced, LastSyncTime: time1},
			{SelectorHash: hash, ASGSyncState: SyncStateSynced, LastSyncTime: time2},
		},
	}

	// New: [web, web] — the "other" mapping was removed; two duplicate hash rows remain
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: sameLabels},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg1")},
			},
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: sameLabels},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{makeASGRef("sub1", "rg1", "asg2")},
			},
		},
	}

	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("err")),
		makeFailedResult("sub1", "rg1", "asg2", prefix, fmt.Errorf("err")),
	}

	got := ComputeStatus(ComputeStatusInput{
		Spec:               spec,
		PreviousStatus:     previousStatus,
		PrefixSetName:      prefix,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ReconcileErr:       fmt.Errorf("all failed"),
		ObservedGeneration: 2,
		Phase:              StatusPhaseFinal,
		Now:                now,
	})

	if len(got.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(got.MappingStatuses))
	}

	// FIFO by selector hash: first row pops time1, second row pops time2
	// (index-based would give timeX and time1 — wrong)
	if !got.MappingStatuses[0].LastSyncTime.Equal(&time1) {
		t.Errorf("Duplicate hash row 0 LastSyncTime = %v, want %v (FIFO first from selector hash bucket)", got.MappingStatuses[0].LastSyncTime, time1)
	}
	if !got.MappingStatuses[1].LastSyncTime.Equal(&time2) {
		t.Errorf("Duplicate hash row 1 LastSyncTime = %v, want %v (FIFO second from selector hash bucket)", got.MappingStatuses[1].LastSyncTime, time2)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — buildPreviousLastSyncBySelector unit test
// ---------------------------------------------------------------------------

func TestBuildPreviousLastSyncBySelector(t *testing.T) {
	time1 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	time2 := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))
	time3 := metav1.NewTime(time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC))

	prev := []v1alpha1.MappingStatus{
		{SelectorHash: "hash-a", LastSyncTime: time1},
		{SelectorHash: "hash-b", LastSyncTime: time2},
		{SelectorHash: "hash-a", LastSyncTime: time3}, // duplicate hash-a
	}

	bySelector := buildPreviousLastSyncBySelector(prev)

	if bySelector == nil {
		t.Fatal("buildPreviousLastSyncBySelector returned nil, want non-nil map")
	}

	aQueue, ok := bySelector["hash-a"]
	if !ok {
		t.Fatal("missing key 'hash-a' in result")
	}
	if len(aQueue) != 2 {
		t.Fatalf("hash-a queue len = %d, want 2", len(aQueue))
	}
	if !aQueue[0].Equal(&time1) {
		t.Errorf("hash-a[0] = %v, want %v", aQueue[0], time1)
	}
	if !aQueue[1].Equal(&time3) {
		t.Errorf("hash-a[1] = %v, want %v", aQueue[1], time3)
	}

	bQueue, ok := bySelector["hash-b"]
	if !ok {
		t.Fatal("missing key 'hash-b' in result")
	}
	if len(bQueue) != 1 {
		t.Fatalf("hash-b queue len = %d, want 1", len(bQueue))
	}
	if !bQueue[0].Equal(&time2) {
		t.Errorf("hash-b[0] = %v, want %v", bQueue[0], time2)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — popPreviousLastSync unit test
// ---------------------------------------------------------------------------

func TestPopPreviousLastSync(t *testing.T) {
	time1 := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	time2 := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))

	bySelector := map[string][]metav1.Time{
		"hash-a": {time1, time2},
	}

	// First pop: should return time1
	got1, ok1 := popPreviousLastSync(bySelector, "hash-a")
	if !ok1 {
		t.Fatal("popPreviousLastSync returned false for existing key, want true")
	}
	if !got1.Equal(&time1) {
		t.Errorf("first pop = %v, want %v", got1, time1)
	}

	// Second pop: should return time2
	got2, ok2 := popPreviousLastSync(bySelector, "hash-a")
	if !ok2 {
		t.Fatal("popPreviousLastSync returned false on second pop, want true")
	}
	if !got2.Equal(&time2) {
		t.Errorf("second pop = %v, want %v", got2, time2)
	}

	// Third pop: queue exhausted, should return false
	_, ok3 := popPreviousLastSync(bySelector, "hash-a")
	if ok3 {
		t.Error("popPreviousLastSync returned true for exhausted queue, want false")
	}

	// Non-existent key
	_, ok4 := popPreviousLastSync(bySelector, "hash-nonexistent")
	if ok4 {
		t.Error("popPreviousLastSync returned true for non-existent key, want false")
	}
}

// ---------------------------------------------------------------------------
// Phase 7 Acceptance: T7.5 Status Contract Validation
// ---------------------------------------------------------------------------

func TestPhase7_T75_StatusContract_ConditionsAndMappingRows(t *testing.T) {
	now := metav1.Now()

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-ok"},
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-fail"},
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-perm"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-ok",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-ok",
					PrefixSetName:  "test-prefix",
				},
				DesiredIPs: []string{"10.0.0.1"},
			},
			Err: nil,
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-fail",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-fail",
					PrefixSetName:  "test-prefix",
				},
				DesiredIPs: []string{"10.0.0.2"},
			},
			Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "transient"},
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-perm",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg-perm",
					PrefixSetName:  "test-prefix",
				},
				DesiredIPs: []string{"10.0.0.3"},
			},
			Err: &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"},
		},
	}

	// Validate Phase 7 status contract against the partial failure results.
	// The status passed here is deliberately incomplete (missing Reconciled
	// condition and per-ASG MappingStatuses), so the validator should detect
	// violations.
	violations := ValidatePhase7StatusContract(
		v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Accepted",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: now,
				},
			},
		},
		results,
		spec,
		"test-prefix",
	)

	// T7.5 acceptance: the incomplete status above must trigger violations
	// for missing Reconciled=False condition and missing per-ASG sync states.
	if len(violations) == 0 {
		t.Fatalf("T7.5: ValidatePhase7StatusContract returned 0 violations; want > 0 for incomplete status")
	}

	for _, v := range violations {
		t.Logf("violation: field=%s got=%s want=%s msg=%s", v.Field, v.Got, v.Want, v.Message)
	}

	// Assert the specific violations we expect from an incomplete status:
	// 1. Missing Reconciled condition (results contain failures).
	// 2. MappingStatuses count mismatch (0 rows vs 1 spec mapping).
	hasReconciledViolation := false
	hasMappingStatusesViolation := false
	for _, v := range violations {
		switch v.Field {
		case "conditions[Reconciled]":
			hasReconciledViolation = true
			if v.Got != "<missing>" {
				t.Errorf("T7.5: Reconciled violation: expected Got=<missing>, got %q", v.Got)
			}
		case "mappingStatuses":
			hasMappingStatusesViolation = true
			if v.Got != "0 rows" {
				t.Errorf("T7.5: mappingStatuses violation: expected Got=\"0 rows\", got %q", v.Got)
			}
		}
	}
	if !hasReconciledViolation {
		t.Error("T7.5: expected violation for missing Reconciled condition")
	}
	if !hasMappingStatusesViolation {
		t.Error("T7.5: expected violation for mismatched mappingStatuses count")
	}
}
