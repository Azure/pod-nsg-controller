package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// InitialReconcileInitializer is a manager.Runnable that initializes the
// initial reconcile tracker at startup. Only the leader runs this.
type InitialReconcileInitializer struct {
	Reader  client.Reader
	Tracker *metrics.InitialReconcileTracker
	Rec     *metrics.ReconcileRecorder
	Log     logr.Logger
}

// NewInitialReconcileInitializer creates a new InitialReconcileInitializer.
func NewInitialReconcileInitializer(
	reader client.Reader,
	tracker *metrics.InitialReconcileTracker,
	rec *metrics.ReconcileRecorder,
	log logr.Logger,
) *InitialReconcileInitializer {
	return &InitialReconcileInitializer{
		Reader:  reader,
		Tracker: tracker,
		Rec:     rec,
		Log:     log,
	}
}

// Start initializes the tracker with the set of existing PodASGMapping keys.
// It implements manager.Runnable.
func (i *InitialReconcileInitializer) Start(ctx context.Context) error {
	listFn := func(ctx context.Context) ([]types.NamespacedName, error) {
		var mappingList v1alpha1.PodASGMappingList
		if i.Reader == nil {
			return nil, nil
		}
		if err := i.Reader.List(ctx, &mappingList); err != nil {
			return nil, err
		}
		keys := make([]types.NamespacedName, len(mappingList.Items))
		for idx, m := range mappingList.Items {
			keys[idx] = types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
		}
		return keys, nil
	}

	err := i.Tracker.EnsureInitialized(ctx, listFn)
	if err != nil {
		i.Log.Error(err, "failed to initialize startup tracker, deferring to reconcile fallback")
		return nil // non-fatal: reconcile fallback path handles this
	}

	// If empty startup set, emit completion immediately
	if i.Tracker.IsComplete() {
		i.Rec.SetInitialReconcileComplete(time.Since(i.Tracker.ControllerStart()))
	}

	return nil
}

// NeedLeaderElection returns true — only the leader emits startup metrics.
func (i *InitialReconcileInitializer) NeedLeaderElection() bool {
	return true
}
