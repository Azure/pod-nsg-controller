package controller

import (
	"fmt"
	"sync/atomic"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
)

// controllerSeq provides unique controller names to avoid prometheus metrics
// registration collisions when multiple managers are created in the same process
// (e.g., envtest-based integration tests).
var controllerSeq int64

// SetupWithManager registers the MappingReconciler watches.
func (r *MappingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	podHandler := NewPodToMappingEventHandler(
		mgr.GetClient(),
		ctrl.Log.WithName("pod-handler"),
	)

	name := fmt.Sprintf("podasgmapping-%d", atomic.AddInt64(&controllerSeq, 1))

	opts := ctrlcontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}

	// Wire instrumented queue factory if metrics are available.
	if r.MetricsRecorder != nil {
		opts.NewQueue = metrics.NewInstrumentedQueueFactory(r.MetricsRecorder.Reconcile)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(opts).
		For(&v1alpha1.PodASGMapping{}, builder.WithPredicates(MappingPredicate())).
		Watches(&corev1.Pod{}, podHandler, builder.WithPredicates(PodPredicate())).
		Complete(r)
}
