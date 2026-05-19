package controller

import (
	"context"
	"errors"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type errorInjectingStatusWriter struct {
	crclient.SubResourceWriter
	updateErr          error
	updateCalls        *int
	passThroughUpdates int
}

func (w *errorInjectingStatusWriter) Create(ctx context.Context, obj crclient.Object, subResource crclient.Object, opts ...crclient.SubResourceCreateOption) error {
	return w.SubResourceWriter.Create(ctx, obj, subResource, opts...)
}

func (w *errorInjectingStatusWriter) Update(ctx context.Context, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
	if w.updateCalls != nil {
		*w.updateCalls = *w.updateCalls + 1
	}
	if w.passThroughUpdates > 0 {
		w.passThroughUpdates--
		return w.SubResourceWriter.Update(ctx, obj, opts...)
	}
	return w.updateErr
}

func (w *errorInjectingStatusWriter) Patch(ctx context.Context, obj crclient.Object, patch crclient.Patch, opts ...crclient.SubResourcePatchOption) error {
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

type postLoopGetInjectingClient struct {
	crclient.Client
	statusWriter    crclient.SubResourceWriter
	finalGetCall    int
	getCalls        *int
	finalGetErr     error
	finalGeneration int64
}

func (c *postLoopGetInjectingClient) Status() crclient.SubResourceWriter {
	return c.statusWriter
}

func (c *postLoopGetInjectingClient) Get(ctx context.Context, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
	if c.getCalls != nil {
		*c.getCalls = *c.getCalls + 1
	}
	call := 0
	if c.getCalls != nil {
		call = *c.getCalls
	}

	if c.finalGetCall > 0 && call == c.finalGetCall && c.finalGetErr != nil {
		return c.finalGetErr
	}

	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}

	if c.finalGetCall > 0 && call == c.finalGetCall && c.finalGeneration > 0 {
		if mapping, ok := obj.(*v1alpha1.PodASGMapping); ok {
			mapping.Generation = c.finalGeneration
		}
	}

	return nil
}

// TestPhase8_StatusUpdater_FinalStatusSuccess_InvokesConvergenceCommitter
// Design §3.5: UpdateAfterReconcile successful status update invokes committer.
func TestPhase8_StatusUpdater_FinalStatusSuccess_InvokesConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-success-ns"

	mapping := newTestMapping(ns, "su-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-su")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var committed bool
	var committedKey types.NamespacedName
	var committedGen int64
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
		committed = true
		committedKey = key
		committedGen = observedGeneration
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-su"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile() error: %v", err)
	}

	if !committed {
		t.Error("ConvergenceCommitter was NOT invoked on successful status write; want invoked")
	}
	if committedKey != key {
		t.Errorf("committed key = %v, want %v", committedKey, key)
	}
	if committedGen != 1 {
		t.Errorf("committed generation = %d, want 1", committedGen)
	}
}

// TestPhase8_StatusUpdater_FinalStatusSemanticNoop_InvokesConvergenceCommitter
// Design §3.5: statusSemanticEqual (no-op) still invokes committer.
func TestPhase8_StatusUpdater_FinalStatusSemanticNoop_InvokesConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-noop-ns"

	// Pre-populate mapping with existing status to trigger semantic no-op.
	mapping := newTestMapping(ns, "su-noop-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-noop")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}
	// Set conditions that match what ComputeStatus would produce for a successful
	// reconcile with the same parameters — triggers statusSemanticEqual path.
	mapping.Status = v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Converged",
				Message:            "All prefix sets converged",
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var committed bool
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
		committed = true
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-noop-mapping"}
	// Same results that would produce the same status → semantic no-op.
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-noop"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile() error: %v", err)
	}

	if !committed {
		t.Error("ConvergenceCommitter was NOT invoked on semantic no-op; want invoked")
	}
}

// TestPhase8_StatusUpdater_FinalStatusNotFoundSentinel_DoesNotInvokeConvergenceCommitter
// Design §3.5: not-found sentinel exits skip convergence notification entirely.
func TestPhase8_StatusUpdater_FinalStatusNotFoundSentinel_DoesNotInvokeConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-error-ns"

	// Do NOT create the mapping → status update will fail with not-found sentinel.
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var committed bool
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
		committed = true
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-missing-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet},
			Success: true,
		},
	}

	// This should return ErrStatusObjectNotFound.
	_ = updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})

	if committed {
		t.Error("ConvergenceCommitter was invoked on not-found sentinel; want NOT invoked")
	}
}

// TestPhase8_StatusUpdater_FinalStatusStaleGenerationSentinel_DoesNotInvokeConvergenceCommitter
// Design §3.5: stale-generation sentinel exits skip convergence notification entirely.
func TestPhase8_StatusUpdater_FinalStatusStaleGenerationSentinel_DoesNotInvokeConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-stale-ns"

	mapping := newTestMapping(ns, "su-stale-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-stale")},
			},
		},
	})
	mapping.Generation = 2
	mapping.Finalizers = []string{CleanupFinalizer}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var committed bool
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
		committed = true
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-stale-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err != ErrStatusStaleGeneration {
		t.Fatalf("UpdateAfterReconcile() error = %v, want %v", err, ErrStatusStaleGeneration)
	}

	if committed {
		t.Error("ConvergenceCommitter was invoked on stale-generation sentinel; want NOT invoked")
	}
}

// TestPhase8_StatusUpdater_FinalStatusGenericWriteError_InvokesConvergenceCommitter
// Design §3.5: generic status write failures still notify convergence with outcome=error.
func TestPhase8_StatusUpdater_FinalStatusGenericWriteError_InvokesConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-write-error-ns"

	mapping := newTestMapping(ns, "su-write-error-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-write-error")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}

	baseClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updateCalls := 0
	injectedErr := errors.New("simulated status write failure")
	fakeClient := &conflictInjectingClient{
		Client: baseClient,
		statusWriter: &errorInjectingStatusWriter{
			SubResourceWriter: baseClient.Status(),
			updateErr:         injectedErr,
			updateCalls:       &updateCalls,
		},
	}

	updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var committed bool
	var gotOutcome StatusWriteOutcome
	var gotStatusErr error
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
		committed = true
		gotOutcome = outcome
		gotStatusErr = statusErr
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-write-error-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-write-error"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err == nil {
		t.Fatal("UpdateAfterReconcile() error = nil, want wrapped status write error")
	}

	if !committed {
		t.Fatal("ConvergenceCommitter was NOT invoked on generic status write failure; want invoked")
	}
	if gotOutcome != StatusWriteOutcomeError {
		t.Errorf("ConvergenceCommitter outcome = %q, want %q", gotOutcome, StatusWriteOutcomeError)
	}
	if gotStatusErr != injectedErr {
		t.Errorf("ConvergenceCommitter statusErr = %v, want %v", gotStatusErr, injectedErr)
	}
	if updateCalls != 1 {
		t.Errorf("status update calls = %d, want 1", updateCalls)
	}
}

// TestPhase8_StatusUpdater_TerminalConflictPostLoopRefetchPaths covers the
// post-loop re-fetch branches used after final conflict exhaustion.
func TestPhase8_StatusUpdater_TerminalConflictPostLoopRefetchPaths(t *testing.T) {
	t.Run("generation advance returns stale sentinel without convergence notification", func(t *testing.T) {
		scheme := testScheme(t)
		ns := "su-postloop-stale-ns"

		mapping := newTestMapping(ns, "su-postloop-stale-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-postloop-stale")},
				},
			},
		})
		mapping.Generation = 1
		mapping.Finalizers = []string{CleanupFinalizer}

		baseClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping).
			WithStatusSubresource(mapping).
			Build()

		updateCalls := 0
		getCalls := 0
		conflictsRemaining := 2
		fakeClient := &postLoopGetInjectingClient{
			Client: baseClient,
			statusWriter: &conflictInjectingStatusWriter{
				SubResourceWriter:  baseClient.Status(),
				conflictsRemaining: &conflictsRemaining,
				updateCalls:        &updateCalls,
			},
			finalGetCall:    3,
			getCalls:        &getCalls,
			finalGeneration: 2,
		}

		updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))
		updater.MaxAttempts = 2

		var committed bool
		updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
			committed = true
		})

		key := types.NamespacedName{Namespace: ns, Name: "su-postloop-stale-mapping"}
		results := []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-postloop-stale"}},
				Success: true,
			},
		}

		err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
		if err != ErrStatusStaleGeneration {
			t.Fatalf("UpdateAfterReconcile() error = %v, want %v", err, ErrStatusStaleGeneration)
		}
		if committed {
			t.Error("ConvergenceCommitter was invoked after stale generation was detected post-conflict; want NOT invoked")
		}
		if updateCalls != 2 {
			t.Errorf("status update calls = %d, want 2", updateCalls)
		}
		if getCalls != 3 {
			t.Errorf("mapping Get calls = %d, want 3", getCalls)
		}
	})

	t.Run("transient post-loop refetch failure preserves original conflict error", func(t *testing.T) {
		scheme := testScheme(t)
		ns := "su-postloop-refetch-error-ns"

		mapping := newTestMapping(ns, "su-postloop-refetch-error-mapping", []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg-postloop-refetch")},
				},
			},
		})
		mapping.Generation = 1
		mapping.Finalizers = []string{CleanupFinalizer}

		baseClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping).
			WithStatusSubresource(mapping).
			Build()

		updateCalls := 0
		getCalls := 0
		conflictsRemaining := 2
		refetchErr := errors.New("transient refetch failure")
		fakeClient := &postLoopGetInjectingClient{
			Client: baseClient,
			statusWriter: &conflictInjectingStatusWriter{
				SubResourceWriter:  baseClient.Status(),
				conflictsRemaining: &conflictsRemaining,
				updateCalls:        &updateCalls,
			},
			finalGetCall: 3,
			getCalls:     &getCalls,
			finalGetErr:  refetchErr,
		}

		updater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))
		updater.MaxAttempts = 2

		var committed bool
		var gotOutcome StatusWriteOutcome
		var gotStatusErr error
		updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult, outcome StatusWriteOutcome, statusErr error) {
			committed = true
			gotOutcome = outcome
			gotStatusErr = statusErr
		})

		key := types.NamespacedName{Namespace: ns, Name: "su-postloop-refetch-error-mapping"}
		results := []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-postloop-refetch"}},
				Success: true,
			},
		}

		err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
		if err == nil {
			t.Fatal("UpdateAfterReconcile() error = nil, want wrapped conflict error")
		}
		if !apierrors.IsConflict(err) {
			t.Errorf("UpdateAfterReconcile() error = %v, want conflict", err)
		}
		if errors.Is(err, refetchErr) {
			t.Errorf("UpdateAfterReconcile() error = %v, want original conflict error, not %v", err, refetchErr)
		}
		if !committed {
			t.Fatal("ConvergenceCommitter was NOT invoked after terminal conflict exhaustion; want invoked")
		}
		if gotOutcome != StatusWriteOutcomeError {
			t.Errorf("ConvergenceCommitter outcome = %q, want %q", gotOutcome, StatusWriteOutcomeError)
		}
		if gotStatusErr == nil || !apierrors.IsConflict(gotStatusErr) {
			t.Errorf("ConvergenceCommitter statusErr = %v, want conflict error", gotStatusErr)
		}
		if updateCalls != 2 {
			t.Errorf("status update calls = %d, want 2", updateCalls)
		}
		if getCalls != 3 {
			t.Errorf("mapping Get calls = %d, want 3", getCalls)
		}
	})
}
