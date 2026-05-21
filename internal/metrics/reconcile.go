package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
)

// ReconcileResult classifies the outcome of a reconcile cycle.
type ReconcileResult string

const (
	ReconcileResultSuccess        ReconcileResult = "success"
	ReconcileResultPartialFailure ReconcileResult = "partial_failure"
	ReconcileResultError          ReconcileResult = "error"
	ReconcileResultRequeue        ReconcileResult = "requeue"
)

// ReconcileRecorder records reconciliation Prometheus metrics.
type ReconcileRecorder struct {
	reconcileDuration        *prometheus.HistogramVec
	reconcileTotal           *prometheus.CounterVec
	reconcileQueueDepth      prometheus.Gauge
	reconcileInflight        prometheus.Gauge
	reconcileActionsPerCycle *prometheus.HistogramVec
	initialReconcileDuration prometheus.Gauge
	initialReconcileComplete prometheus.Gauge
	crdResolutionDuration    *prometheus.HistogramVec
}

func newReconcileRecorder() *ReconcileRecorder {
	return &ReconcileRecorder{
		reconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "reconcile_duration_seconds",
			Help:      "Per-reconcile wall-clock duration",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"namespace", "mapping", "result"}),
		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "reconcile_total",
			Help:      "Total reconciliations by outcome",
		}, []string{"namespace", "mapping", "result"}),
		reconcileQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "pod_nsg_controller",
			Name:      "reconcile_queue_depth",
			Help:      "Number of pending items in the reconcile work queue (excludes in-flight items being processed)",
		}),
		reconcileInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "pod_nsg_controller",
			Name:      "reconcile_inflight",
			Help:      "Number of items currently being processed (between Get and Done)",
		}),
		reconcileActionsPerCycle: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "reconcile_actions_per_cycle",
			Help:      "Number of ARM actions emitted per reconcile cycle",
			Buckets:   []float64{0, 1, 2, 5, 10, 25, 50, 100},
		}, []string{"namespace", "mapping"}),
		initialReconcileDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "pod_nsg_controller",
			Name:      "initial_reconcile_duration_seconds",
			Help:      "Wall-clock time from controller start to completion of the first full reconciliation",
		}),
		initialReconcileComplete: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "pod_nsg_controller",
			Name:      "initial_reconcile_complete",
			Help:      "Set to 1 once the initial reconciliation pass has finished",
		}),
		crdResolutionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "crd_resolution_duration_seconds",
			Help:      "Time to resolve a CRD update into desired-state actions",
		}, []string{"namespace", "mapping", "operation"}),
	}
}

// Collectors returns all collectors for this recorder.
func (r *ReconcileRecorder) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		r.reconcileDuration,
		r.reconcileTotal,
		r.reconcileQueueDepth,
		r.reconcileInflight,
		r.reconcileActionsPerCycle,
		r.initialReconcileDuration,
		r.initialReconcileComplete,
		r.crdResolutionDuration,
	}
}

// ObserveReconcile records a reconcile cycle completion.
func (r *ReconcileRecorder) ObserveReconcile(namespace, mapping string, result ReconcileResult, duration time.Duration) {
	r.reconcileDuration.WithLabelValues(namespace, mapping, string(result)).Observe(duration.Seconds())
	r.reconcileTotal.WithLabelValues(namespace, mapping, string(result)).Inc()
}

// ObserveActionsPerCycle records the number of ARM actions in a reconcile cycle.
func (r *ReconcileRecorder) ObserveActionsPerCycle(namespace, mapping string, count int) {
	r.reconcileActionsPerCycle.WithLabelValues(namespace, mapping).Observe(float64(count))
}

// SetQueueDepth sets the current pending queue depth gauge.
// This reflects only items waiting to be processed (pending), not items
// currently being processed (in-flight). See SetInflight for in-flight count.
func (r *ReconcileRecorder) SetQueueDepth(depth int) {
	r.reconcileQueueDepth.Set(float64(depth))
}

// IncInflight increments the in-flight gauge when an item is dequeued for processing.
func (r *ReconcileRecorder) IncInflight() {
	r.reconcileInflight.Inc()
}

// DecInflight decrements the in-flight gauge when processing is complete.
func (r *ReconcileRecorder) DecInflight() {
	r.reconcileInflight.Dec()
}

// SetInitialReconcileComplete marks initial reconciliation as complete.
func (r *ReconcileRecorder) SetInitialReconcileComplete(duration time.Duration) {
	r.initialReconcileDuration.Set(duration.Seconds())
	r.initialReconcileComplete.Set(1)
}

// ObserveCRDResolution records the duration of CRD resolution.
func (r *ReconcileRecorder) ObserveCRDResolution(namespace, mapping, operation string, duration time.Duration) {
	r.crdResolutionDuration.WithLabelValues(namespace, mapping, operation).Observe(duration.Seconds())
}

// DeleteForMapping removes all per-mapping metric series for the given
// namespace/mapping. This prevents unbounded cardinality growth when
// PodASGMapping resources are deleted.
func (r *ReconcileRecorder) DeleteForMapping(namespace, mapping string) {
	labels := prometheus.Labels{"namespace": namespace, "mapping": mapping}
	r.reconcileDuration.DeletePartialMatch(labels)
	r.reconcileTotal.DeletePartialMatch(labels)
	r.reconcileActionsPerCycle.DeletePartialMatch(labels)
	r.crdResolutionDuration.DeletePartialMatch(labels)
}

// CRDResolutionDuration returns the CRD resolution histogram vec for direct access in tests.
func (r *ReconcileRecorder) CRDResolutionDuration() *prometheus.HistogramVec {
	return r.crdResolutionDuration
}

// InitialReconcileTracker tracks the progress of initial reconciliation at startup.
type InitialReconcileTracker struct {
	mu              sync.Mutex
	controllerStart time.Time
	initialKeys     map[types.NamespacedName]struct{}
	completedKeys   map[types.NamespacedName]struct{}
	initialized     bool
	complete        bool
}

// NewInitialReconcileTracker creates a tracker with the given controller start time.
func NewInitialReconcileTracker(controllerStartAt time.Time) *InitialReconcileTracker {
	return &InitialReconcileTracker{
		controllerStart: controllerStartAt,
		initialKeys:     make(map[types.NamespacedName]struct{}),
		completedKeys:   make(map[types.NamespacedName]struct{}),
	}
}

// EnsureInitialized initializes the set of keys that must be reconciled at startup.
// The listFn should return all PodASGMapping keys existing at startup.
// Safe to call multiple times; only the first successful call takes effect.
// On error, subsequent calls will retry.
func (t *InitialReconcileTracker) EnsureInitialized(ctx context.Context, listFn func(context.Context) ([]types.NamespacedName, error)) error {
	t.mu.Lock()
	if t.initialized {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	keys, err := listFn(ctx)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.initialized {
		return nil // another goroutine won the race
	}
	for _, k := range keys {
		t.initialKeys[k] = struct{}{}
	}
	t.initialized = true
	if len(t.initialKeys) == 0 {
		t.complete = true
	}
	return nil
}

// MarkTerminal marks a key as having reached a terminal state in reconciliation.
// If terminal is false, the key is not marked (non-terminal outcome).
func (t *InitialReconcileTracker) MarkTerminal(rec *ReconcileRecorder, key types.NamespacedName, terminal bool) {
	if !terminal {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.complete || !t.initialized {
		return
	}
	if _, isInitialKey := t.initialKeys[key]; !isInitialKey {
		return
	}
	t.completedKeys[key] = struct{}{}
	if len(t.completedKeys) >= len(t.initialKeys) {
		t.complete = true
		duration := time.Since(t.controllerStart)
		rec.SetInitialReconcileComplete(duration)
	}
}

// IsComplete returns whether initial reconciliation has completed.
func (t *InitialReconcileTracker) IsComplete() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.complete
}

// IsInitialized returns whether the tracker has been initialized with the
// startup key set. If false, MarkTerminal calls are no-ops.
func (t *InitialReconcileTracker) IsInitialized() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initialized
}

// Forget removes a key from tracking (e.g., mapping deleted).
func (t *InitialReconcileTracker) Forget(key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.initialKeys, key)
	delete(t.completedKeys, key)
	// Check if removal makes us complete
	if t.initialized && !t.complete && len(t.initialKeys) > 0 && len(t.completedKeys) >= len(t.initialKeys) {
		t.complete = true
	}
}
