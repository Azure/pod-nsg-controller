package controller

import (
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Standard condition types for PodASGMapping.
const (
	ConditionAccepted   = "Accepted"
	ConditionReconciled = "Reconciled"
)

// SetCondition sets or updates a condition on the given PodASGMappingStatus.
// It preserves the existing ObservedGeneration when updating an existing condition.
// For newly added conditions without generation context, ObservedGeneration defaults to 0.
func SetCondition(
	status *v1alpha1.PodASGMappingStatus,
	condType string,
	value metav1.ConditionStatus,
	reason string,
	message string,
) {
	// Look up existing ObservedGeneration to preserve it.
	var observedGen int64
	existing := apimeta.FindStatusCondition(status.Conditions, condType)
	if existing != nil {
		observedGen = existing.ObservedGeneration
	}

	newCondition := metav1.Condition{
		Type:               condType,
		Status:             value,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGen,
	}

	apimeta.SetStatusCondition(&status.Conditions, newCondition)
}

// SetConditionForGeneration sets or updates a condition with an explicit ObservedGeneration.
func SetConditionForGeneration(
	status *v1alpha1.PodASGMappingStatus,
	condType string,
	value metav1.ConditionStatus,
	reason string,
	message string,
	observedGeneration int64,
) {
	newCondition := metav1.Condition{
		Type:               condType,
		Status:             value,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	}

	apimeta.SetStatusCondition(&status.Conditions, newCondition)
}
