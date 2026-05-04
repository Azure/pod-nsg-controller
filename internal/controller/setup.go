package controller

import (
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// SetupWithManager registers the controller with the manager.
func (r *MappingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PodASGMapping{}, builder.WithPredicates(MappingEventPredicate())).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.MapPodToMappings),
			builder.WithPredicates(PodEventPredicate()),
		).
		Complete(r)
}
