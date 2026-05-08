package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
)

const (
	ConditionAccepted   = "Accepted"
	ConditionReconciled = "Reconciled"

	ReasonSpecValid          = "SpecValid"
	ReasonSpecInvalid        = "SpecInvalid"
	ReasonReconciling        = "Reconciling"
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	ReasonReconcileFailed    = "ReconcileFailed"
	ReasonValidationFailed   = "ValidationFailed"

	SyncStatePending = "Pending"
	SyncStateSynced  = "Synced"
	SyncStateError   = "Error"
)

// SetCondition upserts a condition by type on the given status.
// If the condition's Status, Reason, and Message are unchanged, LastTransitionTime is preserved.
func SetCondition(
	status *v1alpha1.PodASGMappingStatus,
	observedGeneration int64,
	condType string,
	value metav1.ConditionStatus,
	reason string,
	message string,
) {
	now := metav1.Now()

	for i, c := range status.Conditions {
		if c.Type == condType {
			// Preserve LastTransitionTime if status, reason, and message are unchanged.
			transitionTime := now
			if c.Status == value && c.Reason == reason && c.Message == message {
				transitionTime = c.LastTransitionTime
			}
			status.Conditions[i] = metav1.Condition{
				Type:               condType,
				Status:             value,
				ObservedGeneration: observedGeneration,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: transitionTime,
			}
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             value,
		ObservedGeneration: observedGeneration,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}
