package controller

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"golang.org/x/time/rate"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
		r.MinReconcileInterval,
	)

	name := fmt.Sprintf("podasgmapping-%d", atomic.AddInt64(&controllerSeq, 1))

	opts := ctrlcontroller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}

	// Wire rate limiter for the controller queue (default per-item backoff).
	opts.RateLimiter = newMappingControllerRateLimiter(r.MinReconcileInterval)

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

// newMappingControllerRateLimiter returns a bucket-backed typed rate limiter
// for the mapping controller queue. It composes a per-item token-bucket limiter
// (to cap same-key throughput without cross-key interference) with a per-item
// exponential failure backoff limiter (for retry spacing on errors).
func newMappingControllerRateLimiter(minInterval time.Duration) workqueue.TypedRateLimiter[reconcile.Request] {
	// When debounce is disabled (zero interval), preserve the default
	// controller-runtime limiter (shared 10 QPS / 100 burst + exponential
	// backoff). This avoids changing retry behavior for unrelated keys.
	if minInterval <= 0 {
		return workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]()
	}

	// Compose a per-item token-bucket limiter (to cap same-key throughput
	// without cross-key interference) with exponential failure backoff.
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		&perItemBucketRateLimiter{
			limiters: make(map[reconcile.Request]*rate.Limiter),
			rate:     rate.Every(minInterval),
			burst:    1,
		},
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, 1000*time.Second),
	)
}

// perItemBucketRateLimiter provides per-key token bucket rate limiting,
// avoiding cross-key serialization that a shared TypedBucketRateLimiter causes.
type perItemBucketRateLimiter struct {
	mu       sync.Mutex
	limiters map[reconcile.Request]*rate.Limiter
	rate     rate.Limit
	burst    int
}

func (l *perItemBucketRateLimiter) When(item reconcile.Request) time.Duration {
	l.mu.Lock()
	limiter, ok := l.limiters[item]
	if !ok {
		limiter = rate.NewLimiter(l.rate, l.burst)
		l.limiters[item] = limiter
	}
	l.mu.Unlock()

	return limiter.Reserve().Delay()
}

func (l *perItemBucketRateLimiter) NumRequeues(_ reconcile.Request) int {
	return 0
}

// Forget is a no-op: the per-key token bucket must persist across successful
// reconciles to enforce the minimum interval between events for the same key.
// Limiters are lazily created and retained for the controller lifetime (bounded
// by the number of distinct PodASGMapping keys, typically small).
func (l *perItemBucketRateLimiter) Forget(_ reconcile.Request) {}
