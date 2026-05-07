package controller

import (
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// T6 Condition helper tests
// ---------------------------------------------------------------------------

func TestSetCondition_AddsCondition(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{}

	SetCondition(status, ConditionAccepted, metav1.ConditionTrue, "SpecValid", "spec is valid")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.Type != ConditionAccepted {
		t.Errorf("expected condition type %q, got %q", ConditionAccepted, c.Type)
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("expected condition status %q, got %q", metav1.ConditionTrue, c.Status)
	}
	if c.Reason != "SpecValid" {
		t.Errorf("expected reason %q, got %q", "SpecValid", c.Reason)
	}
	if c.Message != "spec is valid" {
		t.Errorf("expected message %q, got %q", "spec is valid", c.Message)
	}
}

func TestSetCondition_UpdatesExistingConditionByType(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:    ConditionAccepted,
				Status:  metav1.ConditionTrue,
				Reason:  "SpecValid",
				Message: "ok",
			},
		},
	}

	SetCondition(status, ConditionAccepted, metav1.ConditionFalse, "InvalidSpec", "bad resource ID")

	if len(status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(status.Conditions))
	}
	c := status.Conditions[0]
	if c.Status != metav1.ConditionFalse {
		t.Errorf("expected updated status %q, got %q", metav1.ConditionFalse, c.Status)
	}
	if c.Reason != "InvalidSpec" {
		t.Errorf("expected updated reason %q, got %q", "InvalidSpec", c.Reason)
	}
	if c.Message != "bad resource ID" {
		t.Errorf("expected updated message %q, got %q", "bad resource ID", c.Message)
	}
}

func TestSetCondition_PreservesOtherConditionTypes(t *testing.T) {
	status := &v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:    ConditionAccepted,
				Status:  metav1.ConditionTrue,
				Reason:  "SpecValid",
				Message: "ok",
			},
		},
	}

	SetCondition(status, ConditionReconciled, metav1.ConditionTrue, "AllSynced", "all mappings synced")

	if len(status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(status.Conditions))
	}

	// Verify the original condition is still there.
	found := false
	for _, c := range status.Conditions {
		if c.Type == ConditionAccepted && c.Status == metav1.ConditionTrue {
			found = true
			break
		}
	}
	if !found {
		t.Error("Accepted condition was not preserved after adding Reconciled condition")
	}
}
