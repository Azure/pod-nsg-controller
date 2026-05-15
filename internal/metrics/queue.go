package metrics

import (
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"k8s.io/client-go/util/workqueue"
)

// instrumentedQueue wraps a TypedRateLimitingInterface to track queue depth.
type instrumentedQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	rec *ReconcileRecorder
}

func (q *instrumentedQueue) Add(item reconcile.Request) {
	q.TypedRateLimitingInterface.Add(item)
	q.rec.SetQueueDepth(q.Len())
}

func (q *instrumentedQueue) Get() (reconcile.Request, bool) {
	item, shutdown := q.TypedRateLimitingInterface.Get()
	q.rec.SetQueueDepth(q.Len())
	return item, shutdown
}

func (q *instrumentedQueue) Done(item reconcile.Request) {
	q.TypedRateLimitingInterface.Done(item)
	q.rec.SetQueueDepth(q.Len())
}

func (q *instrumentedQueue) AddRateLimited(item reconcile.Request) {
	q.TypedRateLimitingInterface.AddRateLimited(item)
	q.rec.SetQueueDepth(q.Len())
}

// NewInstrumentedQueueFactory returns a function that creates an instrumented
// work queue which updates reconcile_queue_depth after each operation.
func NewInstrumentedQueueFactory(rec *ReconcileRecorder) func(
	controllerName string,
	rateLimiter workqueue.TypedRateLimiter[reconcile.Request],
) workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return func(controllerName string, rateLimiter workqueue.TypedRateLimiter[reconcile.Request]) workqueue.TypedRateLimitingInterface[reconcile.Request] {
		inner := workqueue.NewTypedRateLimitingQueue(rateLimiter)
		return &instrumentedQueue{
			TypedRateLimitingInterface: inner,
			rec:                        rec,
		}
	}
}
