package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// ConvergenceOperation classifies the type of convergence operation.
type ConvergenceOperation string

const (
	ConvergenceOpAdd    ConvergenceOperation = "add"
	ConvergenceOpDelete ConvergenceOperation = "delete"
	ConvergenceOpUpdate ConvergenceOperation = "update"
)

// TargetDelta records which IPs were added/removed for a target.
type TargetDelta struct {
	AddedIPs   map[string]struct{}
	RemovedIPs map[string]struct{}
}

// ConvergenceRecorder records convergence Prometheus metrics.
type ConvergenceRecorder struct {
	convergenceSeconds   *prometheus.HistogramVec
	driftCorrections     *prometheus.CounterVec
	prefixSetActions     *prometheus.CounterVec
}

func newConvergenceRecorder() *ConvergenceRecorder {
	return &ConvergenceRecorder{
		convergenceSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "prefix_set_convergence_seconds",
			Help:      "Time from pod IP change detection to ARM PUT success",
			Buckets:   prometheus.DefBuckets,
		}, []string{"subscription_id", "resource_group", "asg_name", "operation"}),
		driftCorrections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "prefix_set_drift_corrections_total",
			Help:      "Drift corrections applied",
		}, []string{"subscription_id", "resource_group", "asg_name"}),
		prefixSetActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "prefix_set_actions_total",
			Help:      "Prefix-set actions executed by operation and result",
		}, []string{"operation", "result"}),
	}
}

// Collectors returns all collectors for this recorder.
func (r *ConvergenceRecorder) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.convergenceSeconds, r.driftCorrections, r.prefixSetActions}
}

// ObserveConvergence records a convergence histogram observation.
func (r *ConvergenceRecorder) ObserveConvergence(target engine.ASGTarget, op ConvergenceOperation, duration time.Duration) {
	r.convergenceSeconds.WithLabelValues(
		target.SubscriptionID,
		target.ResourceGroup,
		target.ASGName,
		string(op),
	).Observe(duration.Seconds())
}

// IncrementDriftCorrections increments the drift correction counter for a target.
func (r *ConvergenceRecorder) IncrementDriftCorrections(target engine.ASGTarget) {
	r.driftCorrections.WithLabelValues(
		target.SubscriptionID,
		target.ResourceGroup,
		target.ASGName,
	).Inc()
}

// RecordPrefixSetAction records a prefix-set action outcome.
func (r *ConvergenceRecorder) RecordPrefixSetAction(operation, result string) {
	r.prefixSetActions.WithLabelValues(operation, result).Inc()
}

// convergenceToken tracks a pending convergence measurement.
type convergenceToken struct {
	op         ConvergenceOperation
	detectedAt time.Time
	completedAt *time.Time
}

// ConvergenceTracker tracks pending convergence measurements.
type ConvergenceTracker struct {
	mu      sync.Mutex
	pending map[convergenceKey]convergenceToken
}

type convergenceKey struct {
	mappingKey types.NamespacedName
	targetKey  string
}

// NewConvergenceTracker creates a new ConvergenceTracker.
func NewConvergenceTracker() *ConvergenceTracker {
	return &ConvergenceTracker{
		pending: make(map[convergenceKey]convergenceToken),
	}
}

// StartOrKeep starts tracking convergence for a target, or keeps the existing start time.
func (t *ConvergenceTracker) StartOrKeep(key types.NamespacedName, target engine.ASGTarget, op ConvergenceOperation, detectedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := convergenceKey{mappingKey: key, targetKey: target.FullResourceID}
	if _, exists := t.pending[ck]; !exists {
		t.pending[ck] = convergenceToken{op: op, detectedAt: detectedAt}
	}
}

// StageSuccessfulAction stages a successful ARM action time for later commit.
func (t *ConvergenceTracker) StageSuccessfulAction(key types.NamespacedName, target engine.ASGTarget, op ConvergenceOperation, completedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := convergenceKey{mappingKey: key, targetKey: target.FullResourceID}
	if token, exists := t.pending[ck]; exists {
		token.completedAt = &completedAt
		t.pending[ck] = token
	}
}

// CommitConvergence commits convergence measurements to the histogram.
// The recorded duration spans from change detection to ARM PUT success
// (captured by StageSuccessfulAction), not from the status write.
func (t *ConvergenceTracker) CommitConvergence(rec *ConvergenceRecorder, key types.NamespacedName, target engine.ASGTarget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ck := convergenceKey{mappingKey: key, targetKey: target.FullResourceID}
	if token, exists := t.pending[ck]; exists && token.completedAt != nil {
		duration := token.completedAt.Sub(token.detectedAt)
		rec.ObserveConvergence(target, token.op, duration)
		delete(t.pending, ck)
	}
}

// Forget removes all pending tokens for a mapping key.
func (t *ConvergenceTracker) Forget(key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ck := range t.pending {
		if ck.mappingKey == key {
			delete(t.pending, ck)
		}
	}
}

// ClassifyConvergenceOperation classifies a target delta into a convergence operation.
func ClassifyConvergenceOperation(delta TargetDelta) (ConvergenceOperation, bool) {
	hasAdded := len(delta.AddedIPs) > 0
	hasRemoved := len(delta.RemovedIPs) > 0
	switch {
	case hasAdded && hasRemoved:
		return ConvergenceOpUpdate, true
	case hasAdded:
		return ConvergenceOpAdd, true
	case hasRemoved:
		return ConvergenceOpDelete, true
	default:
		return "", false
	}
}
