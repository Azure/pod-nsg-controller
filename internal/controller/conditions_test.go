package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// T6 conditions helper tests
// ---------------------------------------------------------------------------

// TestSetCondition_UpsertByType verifies that SetCondition inserts a new
// condition when one of that type does not exist, and updates (replaces) it
// when one already exists.
func TestSetCondition_UpsertByType(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	// Insert
	SetCondition(status, 1, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "spec ok")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after insert, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.Type != ConditionAccepted {
		t.Errorf("condition type = %q, want %q", c.Type, ConditionAccepted)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("condition status = %q, want %q", c.Status, metav1.ConditionTrue)
	}
	if c.Reason != ReasonSpecValid {
		t.Errorf("condition reason = %q, want %q", c.Reason, ReasonSpecValid)
	}
	if c.Message != "spec ok" {
		t.Errorf("condition message = %q, want %q", c.Message, "spec ok")
	}

	// Upsert (update existing)
	SetCondition(status, 2, ConditionAccepted, metav1.ConditionFalse, ReasonSpecInvalid, "bad id")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after upsert, got %d", len(status.Conditions))
	}
	c = status.Conditions[0]
	if c.Status != metav1.ConditionFalse {
		t.Errorf("upserted status = %q, want %q", c.Status, metav1.ConditionFalse)
	}
	if c.Reason != ReasonSpecInvalid {
		t.Errorf("upserted reason = %q, want %q", c.Reason, ReasonSpecInvalid)
	}
}

// TestSetCondition_SetsObservedGeneration verifies that the condition's
// ObservedGeneration field matches the supplied value.
func TestSetCondition_SetsObservedGeneration(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}
	var gen int64 = 42

	SetCondition(status, gen, ConditionReconciled, metav1.ConditionTrue, ReasonReconcileSucceeded, "")

	if len(status.Conditions) == 0 {
		t.Fatal("expected at least 1 condition, got 0")
	}
	if got := status.Conditions[0].ObservedGeneration; got != gen {
		t.Errorf("ObservedGeneration = %d, want %d", got, gen)
	}
}

// TestSetCondition_PreservesOtherConditions verifies that upserting one
// condition type does not remove or modify other condition types.
func TestSetCondition_PreservesOtherConditions(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, 1, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "ok")
	SetCondition(status, 1, ConditionReconciled, metav1.ConditionTrue, ReasonReconcileSucceeded, "done")

	if len(status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(status.Conditions))
	}

	// Now upsert Accepted — Reconciled must still exist.
	SetCondition(status, 2, ConditionAccepted, metav1.ConditionFalse, ReasonSpecInvalid, "bad")

	if len(status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions after upsert, got %d", len(status.Conditions))
	}

	found := false
	for _, c := range status.Conditions {
		if c.Type == ConditionReconciled {
			found = true
			if c.Status != metav1.ConditionTrue {
				t.Errorf("Reconciled condition was modified; status = %q", c.Status)
			}
		}
	}
	if !found {
		t.Error("Reconciled condition was removed during Accepted upsert")
	}
}

// TestSetCondition_SetsLastTransitionTime verifies that LastTransitionTime is
// always populated (non-zero) when a condition is set.
func TestSetCondition_SetsLastTransitionTime(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, 1, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "ok")

	if len(status.Conditions) == 0 {
		t.Fatal("expected at least 1 condition, got 0")
	}
	if status.Conditions[0].LastTransitionTime.IsZero() {
		t.Error("LastTransitionTime is zero, want non-zero")
	}
}

// TestSetCondition_MessageUpdatedOnUpsert verifies that the message field is
// replaced when upserting an existing condition.
func TestSetCondition_MessageUpdatedOnUpsert(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, 1, ConditionReconciled, metav1.ConditionFalse, ReasonReconcileFailed, "first error")
	SetCondition(status, 2, ConditionReconciled, metav1.ConditionFalse, ReasonReconcileFailed, "second error")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	if got := status.Conditions[0].Message; got != "second error" {
		t.Errorf("Message = %q, want %q", got, "second error")
	}
	if got := status.Conditions[0].ObservedGeneration; got != 2 {
		t.Errorf("ObservedGeneration = %d, want 2", got)
	}
}

// TestSetCondition_ZeroObservedGeneration verifies that ObservedGeneration=0
// is correctly stored (not treated as unset).
func TestSetCondition_ZeroObservedGeneration(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, 0, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "")

	if len(status.Conditions) == 0 {
		t.Fatal("expected at least 1 condition")
	}
	if got := status.Conditions[0].ObservedGeneration; got != 0 {
		t.Errorf("ObservedGeneration = %d, want 0", got)
	}
}

// TestSetCondition_PreservesLastTransitionTimeWhenUnchanged verifies that
// upserting a condition with the same status, reason, and message keeps the
// prior LastTransitionTime while still updating ObservedGeneration.
func TestSetCondition_PreservesLastTransitionTimeWhenUnchanged(t *testing.T) {
	oldTime := metav1.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	status := &v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               ConditionAccepted,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             ReasonSpecValid,
				Message:            "spec ok",
				LastTransitionTime: oldTime,
			},
		},
	}

	SetCondition(status, 2, ConditionAccepted, metav1.ConditionTrue, ReasonSpecValid, "spec ok")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	got := status.Conditions[0]
	if !got.LastTransitionTime.Equal(&oldTime) {
		t.Errorf("LastTransitionTime = %v, want preserved %v", got.LastTransitionTime, oldTime)
	}
	if got.ObservedGeneration != 2 {
		t.Errorf("ObservedGeneration = %d, want 2", got.ObservedGeneration)
	}
}

// TestSetCondition_UpdatesLastTransitionTimeWhenChanged verifies that
// LastTransitionTime advances when any of status, reason, or message changes.
func TestSetCondition_UpdatesLastTransitionTimeWhenChanged(t *testing.T) {
	oldTime := metav1.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	status := &v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               ConditionReconciled,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 1,
				Reason:             ReasonReconcileFailed,
				Message:            "first error",
				LastTransitionTime: oldTime,
			},
		},
	}

	SetCondition(status, 2, ConditionReconciled, metav1.ConditionFalse, ReasonReconcileFailed, "second error")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	got := status.Conditions[0]
	if got.LastTransitionTime.Equal(&oldTime) {
		t.Errorf("LastTransitionTime = %v, want a new transition time", got.LastTransitionTime)
	}
	if got.LastTransitionTime.IsZero() {
		t.Error("LastTransitionTime is zero, want non-zero")
	}
}
