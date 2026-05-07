package controller

import (
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// T6 extension: SetConditionForGeneration sets ObservedGeneration
// ---------------------------------------------------------------------------

func TestSetConditionForGeneration_SetsObservedGeneration(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetConditionForGeneration(status, ConditionReconciled, metav1.ConditionTrue, "AllSynced", "all synced", 5)

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.ObservedGeneration != 5 {
		t.Errorf("expected ObservedGeneration=5, got %d", c.ObservedGeneration)
	}
	if c.Type != ConditionReconciled {
		t.Errorf("expected condition type %q, got %q", ConditionReconciled, c.Type)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("expected status True, got %q", c.Status)
	}
}

// ---------------------------------------------------------------------------
// T6 extension: SetCondition preserves ObservedGeneration when updating existing
// ---------------------------------------------------------------------------

func TestSetCondition_PreservesObservedGenerationWhenUpdatingExisting(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               ConditionReconciled,
				Status:             metav1.ConditionTrue,
				Reason:             "AllSynced",
				Message:            "ok",
				ObservedGeneration: 7,
			},
		},
	}

	// Update the condition without explicitly passing generation.
	SetCondition(status, ConditionReconciled, metav1.ConditionFalse, "ReconcileError", "something failed")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %q", c.Status)
	}
	// Should preserve the existing ObservedGeneration=7.
	if c.ObservedGeneration != 7 {
		t.Errorf("expected ObservedGeneration to be preserved as 7, got %d", c.ObservedGeneration)
	}
}

// ---------------------------------------------------------------------------
// T6 extension: SetCondition on new condition defaults ObservedGeneration to 0
// ---------------------------------------------------------------------------

func TestSetCondition_NewCondition_DefaultObservedGenerationZero(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, ConditionAccepted, metav1.ConditionTrue, "SpecValid", "spec is valid")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.ObservedGeneration != 0 {
		t.Errorf("expected ObservedGeneration=0 for new condition without generation context, got %d",
			c.ObservedGeneration)
	}
}

// ---------------------------------------------------------------------------
// T6 extension: SetCondition transition time uses apimeta (only updates on change)
// ---------------------------------------------------------------------------

func TestSetCondition_TransitionTimeDelegatesToApimeta(t *testing.T) {
	// First, set a condition to True.
	status := &v1alpha1.PodASGMappingStatus{}
	SetCondition(status, ConditionReconciled, metav1.ConditionTrue, "AllSynced", "all synced")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	firstTransitionTime := status.Conditions[0].LastTransitionTime

	// Update the condition with the SAME status (True→True).
	// Per apimeta convention, LastTransitionTime should NOT change.
	SetCondition(status, ConditionReconciled, metav1.ConditionTrue, "StillSynced", "still synced")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after same-status update, got %d", len(status.Conditions))
	}
	secondTransitionTime := status.Conditions[0].LastTransitionTime

	if !firstTransitionTime.Equal(&secondTransitionTime) {
		t.Errorf("LastTransitionTime should not change for same status transition; first=%v, second=%v",
			firstTransitionTime, secondTransitionTime)
	}
}
