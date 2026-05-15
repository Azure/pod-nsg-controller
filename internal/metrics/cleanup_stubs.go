package metrics

import "k8s.io/apimachinery/pkg/types"

// HasPending returns true if the tracker has any pending convergence tokens for the given key.
// This is used by cleanup paths to verify state has been cleared.
func (t *ConvergenceTracker) HasPending(key types.NamespacedName) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ck := range t.pending {
		if ck.mappingKey == key {
			return true
		}
	}
	return false
}

// HasSnapshot returns true if the tracker has a snapshot for the given key.
// This is used by cleanup paths to verify state has been cleared.
func (t *PodChurnTracker) HasSnapshot(key types.NamespacedName) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.snapshots[key]
	return ok
}

// ForgetWithDelete removes the snapshot for a mapping key and deletes the
// associated pod_churn_rate gauge series from the recorder.
// Counters remain cumulative (not deleted).
func (t *PodChurnTracker) ForgetWithDelete(rec *PodChurnRecorder, key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.snapshots, key)
	if rec != nil {
		rec.podChurnRate.DeleteLabelValues(key.Namespace, key.Name)
	}
}
