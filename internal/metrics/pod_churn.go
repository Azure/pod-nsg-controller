package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
)

// PodOperation represents a pod IP change operation.
type PodOperation string

const (
	PodOperationAdd    PodOperation = "add"
	PodOperationDelete PodOperation = "delete"
	PodOperationUpdate PodOperation = "update"
)

// PodIdentity uniquely identifies a pod.
type PodIdentity struct {
	Namespace string
	Name      string
	UID       string
}

// PodMembership stores the IP and target memberships for a pod.
type PodMembership struct {
	PodIP string
}

// MappingPodSnapshot is a point-in-time snapshot of pods for a mapping.
type MappingPodSnapshot struct {
	Pods map[PodIdentity]PodMembership
}

// PodDeltaSummary summarizes changes between two snapshots.
type PodDeltaSummary struct {
	Added   int
	Deleted int
	Updated int
	Total   int
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
	snapshot MappingPodSnapshot
	time     time.Time
}

// NewPodChurnTracker creates a new tracker.
func NewPodChurnTracker() *PodChurnTracker {
	return &PodChurnTracker{
		snapshots: make(map[types.NamespacedName]snapshotEntry),
	}
}

// ObserveSnapshot compares current snapshot against previous and returns the delta.
// It also records metrics on the PodChurnRecorder.
func (t *PodChurnTracker) ObserveSnapshot(rec *PodChurnRecorder, key types.NamespacedName, current MappingPodSnapshot, now time.Time) PodDeltaSummary {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev, hasPrev := t.snapshots[key]
	delta := computeDelta(prev.snapshot, current, hasPrev)

	// Record metrics
	if delta.Added > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationAdd)).Add(float64(delta.Added))
	}
	if delta.Deleted > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationDelete)).Add(float64(delta.Deleted))
	}
	if delta.Updated > 0 {
		rec.podIPChangesTotal.WithLabelValues(key.Namespace, key.Name, string(PodOperationUpdate)).Add(float64(delta.Updated))
	}

	// Update churn rate gauge (simple: changes / time-since-last)
	if hasPrev && !prev.time.IsZero() {
		elapsed := now.Sub(prev.time).Seconds()
		if elapsed > 0 {
			rate := float64(delta.Total) / elapsed
			rec.podChurnRate.WithLabelValues(key.Namespace, key.Name).Set(rate)
		}
	}

	t.snapshots[key] = snapshotEntry{snapshot: current, time: now}
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
