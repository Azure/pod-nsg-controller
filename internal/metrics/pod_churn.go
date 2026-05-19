package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/pod-nsg-controller/internal/engine"
)

// podChurnWindow is the fixed window for computing weighted churn rate.
const podChurnWindow = 60 * time.Second

// PodOperation represents a pod IP change operation.
type PodOperation string

const (
	PodOperationAdd    PodOperation = "add"
	PodOperationDelete PodOperation = "delete"
	PodOperationUpdate PodOperation = "update"
)

// PodIdentity is an alias for engine.PodIdentity, allowing direct consumption
// of the engine's snapshot types without conversion.
type PodIdentity = engine.PodIdentity

// PodMembership is an alias for engine.PodMembership.
type PodMembership = engine.PodMembership

// MappingPodSnapshot is an alias for engine.PodSnapshot, the authoritative
// pod snapshot produced by the desired-state engine.
type MappingPodSnapshot = engine.PodSnapshot

// PodDeltaSummary summarizes changes between two snapshots.
type PodDeltaSummary struct {
	Added   int
	Deleted int
	Updated int
	Total   int
}

// churnInterval records a change interval for windowed rate computation.
type churnInterval struct {
	start   time.Time
	end     time.Time
	changes int
}

// PodChurnRecorder records pod churn Prometheus metrics.
type PodChurnRecorder struct {
	podIPChangesTotal *prometheus.CounterVec
	podChurnRate      *prometheus.GaugeVec
}

func newPodChurnRecorder() *PodChurnRecorder {
	return &PodChurnRecorder{
		podIPChangesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "pod_ip_changes_total",
			Help:      "Total pod IP changes detected by the controller",
		}, []string{"namespace", "mapping", "operation"}),
		podChurnRate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "pod_nsg_controller",
			Name:      "pod_churn_rate",
			Help:      "Pod IP changes per second over a sliding window",
		}, []string{"namespace", "mapping"}),
	}
}

// Collectors returns all collectors for this recorder.
func (r *PodChurnRecorder) Collectors() []prometheus.Collector {
	return []prometheus.Collector{r.podIPChangesTotal, r.podChurnRate}
}

// PodChurnTracker tracks pod snapshot state across reconcile cycles.
type PodChurnTracker struct {
	mu        sync.Mutex
	snapshots map[types.NamespacedName]snapshotEntry
}

type snapshotEntry struct {
	snapshot   MappingPodSnapshot
	observedAt time.Time
	history    []churnInterval
}

// NewPodChurnTracker creates a new tracker.
func NewPodChurnTracker() *PodChurnTracker {
	return &PodChurnTracker{
		snapshots: make(map[types.NamespacedName]snapshotEntry),
	}
}

// ObserveSnapshot compares current snapshot against previous and returns the delta.
// It also records metrics on the PodChurnRecorder using a fixed-window weighted-overlap algorithm.
func (t *PodChurnTracker) ObserveSnapshot(rec *PodChurnRecorder, key types.NamespacedName, current MappingPodSnapshot, now time.Time) PodDeltaSummary {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev, hasPrev := t.snapshots[key]
	delta := computeDelta(prev.snapshot, current, hasPrev)

	// Record counters
	if delta.Added > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationAdd)).Add(float64(delta.Added))
	}
	if delta.Deleted > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationDelete)).Add(float64(delta.Deleted))
	}
	if delta.Updated > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationUpdate)).Add(float64(delta.Updated))
	}

	// Build updated history
	history := prev.history
	if hasPrev && !prev.observedAt.IsZero() && now.After(prev.observedAt) && delta.Total > 0 {
		history = append(history, churnInterval{
			start:   prev.observedAt,
			end:     now,
			changes: delta.Total,
		})
	}

	// Prune intervals fully outside the window
	windowStart := now.Add(-podChurnWindow)
	pruned := history[:0]
	for _, iv := range history {
		if iv.end.After(windowStart) {
			pruned = append(pruned, iv)
		}
	}
	history = pruned

	// Compute weighted churn rate
	var weightedChanges float64
	for _, iv := range history {
		intervalDuration := iv.end.Sub(iv.start).Seconds()
		if intervalDuration <= 0 {
			continue
		}
		// Overlap of interval with [windowStart, now]
		overlapStart := iv.start
		if overlapStart.Before(windowStart) {
			overlapStart = windowStart
		}
		overlapEnd := iv.end
		if overlapEnd.After(now) {
			overlapEnd = now
		}
		overlapDuration := overlapEnd.Sub(overlapStart).Seconds()
		if overlapDuration <= 0 {
			continue
		}
		weightedChanges += float64(iv.changes) * (overlapDuration / intervalDuration)
	}

	rate := weightedChanges / podChurnWindow.Seconds()
	rec.podChurnRate.WithLabelValues(key.Namespace, key.Name).Set(rate)

	t.snapshots[key] = snapshotEntry{snapshot: current, observedAt: now, history: history}
	return delta
}

// Forget removes the snapshot for a mapping key.
func (t *PodChurnTracker) Forget(key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.snapshots, key)
}

func computeDelta(prev, current MappingPodSnapshot, hasPrev bool) PodDeltaSummary {
	if !hasPrev {
		// First time seeing this mapping - all pods are "add"
		added := len(current.Pods)
		return PodDeltaSummary{Added: added, Total: added}
	}

	var added, deleted, updated int

	// Check for additions and updates
	for id, curMember := range current.Pods {
		if prevMember, exists := prev.Pods[id]; !exists {
			added++
		} else if prevMember.PodIP != curMember.PodIP {
			updated++
		}
	}

	// Check for deletions
	for id := range prev.Pods {
		if _, exists := current.Pods[id]; !exists {
			deleted++
		}
	}

	return PodDeltaSummary{
		Added:   added,
		Deleted: deleted,
		Updated: updated,
		Total:   added + deleted + updated,
	}
}
