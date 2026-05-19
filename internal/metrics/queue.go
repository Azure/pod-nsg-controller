package metrics

import (
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// instrumentedQueue wraps a TypedRateLimitingInterface to track queue depth.
type instrumentedQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]
	rec      *ReconcileRecorder
	stopOnce sync.Once
	stopCh   chan struct{}
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

func (q *instrumentedQueue) AddAfter(item reconcile.Request, duration time.Duration) {
	q.TypedRateLimitingInterface.AddAfter(item, duration)
	if duration <= 0 {
		// Zero or negative delay: item is immediately ready
		q.rec.SetQueueDepth(q.Len())
	}
	// Positive delay: do not inflate depth; sampler will detect when ready
}

func (q *instrumentedQueue) AddRateLimited(item reconcile.Request) {
	q.TypedRateLimitingInterface.AddRateLimited(item)
	q.rec.SetQueueDepth(q.Len())
}

func (q *instrumentedQueue) ShutDown() {
	q.stopOnce.Do(func() { close(q.stopCh) })
	q.TypedRateLimitingInterface.ShutDown()
}

func (q *instrumentedQueue) ShutDownWithDrain() {
	q.stopOnce.Do(func() { close(q.stopCh) })
	q.TypedRateLimitingInterface.ShutDownWithDrain()
}

// startDepthSampler periodically samples queue Len() to detect delayed items
// becoming ready (promotion from delayed queue to ready queue).
func (q *instrumentedQueue) startDepthSampler() {
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-q.stopCh:
				return
			case <-ticker.C:
				q.rec.SetQueueDepth(q.Len())
			}
		}
	}()
}

// NewInstrumentedQueueFactory returns a function that creates an instrumented
// work queue which updates reconcile_queue_depth after each operation.
func NewInstrumentedQueueFactory(rec *ReconcileRecorder) func(
	controllerName string,
	rateLimiter workqueue.TypedRateLimiter[reconcile.Request],
) workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return func(controllerName string, rateLimiter workqueue.TypedRateLimiter[reconcile.Request]) workqueue.TypedRateLimitingInterface[reconcile.Request] {
		inner := workqueue.NewTypedRateLimitingQueue(rateLimiter)
		q := &instrumentedQueue{
			TypedRateLimitingInterface: inner,
			rec:                        rec,
			stopCh:                     make(chan struct{}),
		}
		q.startDepthSampler()
		return q
	}
}
