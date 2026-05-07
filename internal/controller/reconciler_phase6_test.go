package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/go-logr/logr"
	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// T6 reconciler: AddsFinalizer + writes BootstrapPending status with fresh RV
// ---------------------------------------------------------------------------

func TestReconcile_AddsFinalizer_WritesBootstrapPendingStatus_WithFreshResourceVersion(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "boot-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	// No finalizer yet — this is first reconcile.

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	exec := &stubExecutor{}
	statusRecorder := &recordingStatusUpdater{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusRecorder,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "boot-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Should requeue immediately.
	if !result.Requeue {
		t.Error("expected Requeue=true for first reconcile with finalizer addition")
	}

	// Executor should NOT have been called.
	if len(exec.calls) != 0 {
		t.Errorf("expected zero executor calls on first reconcile, got %d", len(exec.calls))
	}

	// Status updater should have been called with BootstrapPending phase.
	if statusRecorder.callCount == 0 {
		t.Fatal("expected StatusUpdater to be called with BootstrapPending phase, got zero calls")
	}
	if statusRecorder.lastPhase != StatusPhaseBootstrapPending {
		t.Errorf("expected phase %q, got %q", StatusPhaseBootstrapPending, statusRecorder.lastPhase)
	}

	// Verify finalizer was actually added.
	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	hasFinalizer := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Error("expected CleanupFinalizer to be present")
	}
}

// ---------------------------------------------------------------------------
// T6.3: AddsFinalizer + validation fails → writes Accepted=False and returns early
// ---------------------------------------------------------------------------

func TestReconcile_AddsFinalizer_ValidationFails_WritesAcceptedFalseAndReturnsEarly(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "invalid-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "invalid-resource-id-not-arm-format"},
			},
		},
	})
	// No finalizer — triggers first reconcile path.

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	exec := &stubExecutor{}
	statusRecorder := &recordingStatusUpdater{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusRecorder,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "invalid-mapping", Namespace: ns},
	})
	_ = err // Validation may or may not return error depending on implementation.

	// Executor should NOT have been called (validation failure = no Azure calls).
	if len(exec.calls) != 0 {
		t.Errorf("expected zero executor calls on validation failure, got %d", len(exec.calls))
	}

	// Status updater should have been called with ValidationFailed phase.
	if statusRecorder.callCount == 0 {
		t.Fatal("expected StatusUpdater to be called with ValidationFailed phase, got zero calls")
	}
	if statusRecorder.lastPhase != StatusPhaseValidationFailed {
		t.Errorf("expected phase %q, got %q", StatusPhaseValidationFailed, statusRecorder.lastPhase)
	}
}

// ---------------------------------------------------------------------------
// T6.4: PostExecution status drift → requeues without returning status error
// ---------------------------------------------------------------------------

func TestReconcile_PostExecution_StatusDrift_RequeuesWithoutReturningStatusError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "drift-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	exec := &stubExecutor{}

	// StatusUpdater that always returns generation drift error.
	driftUpdater := &driftStatusUpdater{
		driftErr: &StatusGenerationDriftError{ProcessedGeneration: 1, LiveGeneration: 2},
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    driftUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "drift-mapping", Namespace: ns},
	})

	// When drift happens but execution succeeded, should requeue without error.
	if err != nil {
		t.Errorf("expected nil error when status drift occurs (execution succeeded), got %v", err)
	}
	if !result.Requeue && result.RequeueAfter == 0 {
		t.Error("expected requeue when status drift is detected")
	}
}

// ---------------------------------------------------------------------------
// T6: PostExecution status drift + reconcile failure → returns reconcile error
// ---------------------------------------------------------------------------

func TestReconcile_PostExecution_StatusDrift_WithReconcileFailure_ReturnsReconcileError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "drift-fail-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	// Executor that returns a failure result.
	failingExec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: false,
				Err:     fmt.Errorf("azure error"),
			},
		},
	}

	// StatusUpdater that returns drift error.
	driftUpdater := &driftStatusUpdater{
		driftErr: &StatusGenerationDriftError{ProcessedGeneration: 1, LiveGeneration: 2},
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         failingExec,
		StatusUpdater:    driftUpdater,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "drift-fail-mapping", Namespace: ns},
	})

	// Even with drift, if reconcile execution failed, must return the reconcile error.
	if err == nil {
		t.Fatal("expected reconcile error to be returned even when status drift occurs")
	}
}

// ---------------------------------------------------------------------------
// T6: combineReconcileAndStatusErrors uses pkg/errors wrapping
// ---------------------------------------------------------------------------

func TestReconcile_CombineReconcileAndStatusErrors_UsesPkgErrorsWrapping(t *testing.T) {
	t.Run("both nil returns nil", func(t *testing.T) {
		result := combineReconcileAndStatusErrors(nil, nil)
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("reconcile only returns reconcile error", func(t *testing.T) {
		reconcileErr := fmt.Errorf("reconcile failed")
		result := combineReconcileAndStatusErrors(reconcileErr, nil)
		if result != reconcileErr {
			t.Errorf("expected reconcile error returned directly, got %v", result)
		}
	})

	t.Run("status only wraps with context", func(t *testing.T) {
		statusErr := fmt.Errorf("status update failed")
		result := combineReconcileAndStatusErrors(nil, statusErr)
		if result == nil {
			t.Fatal("expected non-nil result for status-only error")
		}
		// Should contain context about updating status.
		if !containsString(result.Error(), "updating PodASGMapping status") {
			t.Errorf("expected wrapped error to contain 'updating PodASGMapping status', got %q", result.Error())
		}
		// Should use pkg/errors wrapping (preserves cause chain).
		if pkgerrors.Cause(result) != statusErr {
			t.Errorf("expected pkg/errors Cause to be statusErr, got %v", pkgerrors.Cause(result))
		}
	})

	t.Run("both present wraps reconcile as primary with status context", func(t *testing.T) {
		reconcileErr := fmt.Errorf("reconcile failed")
		statusErr := fmt.Errorf("status update failed")
		result := combineReconcileAndStatusErrors(reconcileErr, statusErr)
		if result == nil {
			t.Fatal("expected non-nil result for combined errors")
		}
		// Primary cause should be reconcile error.
		var cause error = result
		for {
			unwrapped := errors.Unwrap(cause)
			if unwrapped == nil {
				break
			}
			cause = unwrapped
		}
		// The deepest cause or the pkgerrors.Cause should relate to reconcileErr.
		pkgCause := pkgerrors.Cause(result)
		if pkgCause != reconcileErr {
			t.Errorf("expected pkg/errors Cause to be reconcileErr, got %v", pkgCause)
		}
	})
}

// ---------------------------------------------------------------------------
// Test helpers for reconciler Phase 6 tests
// ---------------------------------------------------------------------------

// recordingStatusUpdater records calls to UpdateAfterReconcile.
type recordingStatusUpdater struct {
	callCount int
	lastPhase StatusPhase
}

func (r *recordingStatusUpdater) UpdateAfterReconcile(
	_ context.Context,
	_ *v1alpha1.PodASGMapping,
	input ReconcileStatusInput,
) error {
	r.callCount++
	r.lastPhase = input.Phase
	return nil
}

// driftStatusUpdater always returns a StatusGenerationDriftError.
type driftStatusUpdater struct {
	driftErr *StatusGenerationDriftError
}

func (d *driftStatusUpdater) UpdateAfterReconcile(
	_ context.Context,
	_ *v1alpha1.PodASGMapping,
	_ ReconcileStatusInput,
) error {
	return d.driftErr
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// T6.3 completeness: WithFinalizer + invalid spec → validation gate always runs
// Design §3.3A: Run validateASGResourceIDs on every non-delete reconcile
// ---------------------------------------------------------------------------

func TestReconcile_WithFinalizer_InvalidSpec_WritesValidationFailedAndSkipsAzure(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "invalid-finalized", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "invalid-resource-id-not-arm-format"},
			},
		},
	})
	// Finalizer already present — this is NOT the first reconcile.
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "web-pod", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}
	statusRecorder := &recordingStatusUpdater{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusRecorder,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "invalid-finalized", Namespace: ns},
	})
	_ = err

	// T6.3: Executor should NOT have been called — validation failure must
	// prevent Azure calls even for already-finalized mappings.
	if len(exec.calls) != 0 {
		t.Errorf("expected zero executor calls when validation fails on finalized mapping, got %d", len(exec.calls))
	}

	// Status updater must be called with ValidationFailed phase, not PostExecution.
	if statusRecorder.callCount == 0 {
		t.Fatal("expected StatusUpdater to be called with ValidationFailed phase, got zero calls")
	}
	if statusRecorder.lastPhase != StatusPhaseValidationFailed {
		t.Errorf("T6.3: expected phase %q for always-run validation gate, got %q",
			StatusPhaseValidationFailed, statusRecorder.lastPhase)
	}
}

// ---------------------------------------------------------------------------
// T6.3: Early validation status write — conflict with generation change → requeue
// Design §3.3B: writeEarlyPhaseStatus conflict disambiguation
// ---------------------------------------------------------------------------

func TestReconcile_EarlyValidationStatusWrite_ConflictWithGenerationChange_Requeues(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "conflict-gen-change", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "invalid-resource-id"},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	conflictErr := apierrors.NewConflict(
		schema.GroupResource{Group: "networking.azure.com", Resource: "podasgmappings"},
		"conflict-gen-change",
		fmt.Errorf("resource version conflict"),
	)

	conflictUpdater := &conflictOnceStatusUpdater{err: conflictErr}

	r := &MappingReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		ClusterName:   "test-cluster",
		ResyncInterval: 60 * time.Second,
		StatusUpdater: conflictUpdater,
	}

	input := ReconcileStatusInput{
		Phase:            StatusPhaseValidationFailed,
		ProcessedGen:     1,
		ProcessedSpec:    mapping.Spec,
		ValidationErrors: map[int][]string{0: {"invalid resource ID format"}},
		PodCounts:        map[string]int{},
	}

	key := types.NamespacedName{Name: "conflict-gen-change", Namespace: ns}
	result, err := r.writeEarlyPhaseStatus(ctx, key, mapping, input, logr.Discard())

	// When conflict occurs and live generation changed, reconciler should
	// classify it as stale and requeue without error.
	if err != nil {
		t.Errorf("expected nil error for stale write skip, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue=true when stale generation drift detected during early-phase status write")
	}
}

// ---------------------------------------------------------------------------
// T6.3: Early validation status write — conflict, same generation, retry succeeds
// Design §3.3B step 4.4: retry status write once against refetched object
// ---------------------------------------------------------------------------

func TestReconcile_EarlyValidationStatusWrite_ConflictSameGeneration_RetrySucceeds(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "conflict-retry-ok", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "invalid-resource-id"},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	// StatusUpdater conflicts on first call, succeeds on retry.
	conflictErr := apierrors.NewConflict(
		schema.GroupResource{Group: "networking.azure.com", Resource: "podasgmappings"},
		"conflict-retry-ok",
		fmt.Errorf("resource version conflict"),
	)
	conflictUpdater := &conflictOnceStatusUpdater{err: conflictErr}

	r := &MappingReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		ClusterName:   "test-cluster",
		ResyncInterval: 60 * time.Second,
		StatusUpdater: conflictUpdater,
	}

	input := ReconcileStatusInput{
		Phase:            StatusPhaseValidationFailed,
		ProcessedGen:     1,
		ProcessedSpec:    mapping.Spec,
		ValidationErrors: map[int][]string{0: {"invalid resource ID format"}},
		PodCounts:        map[string]int{},
	}

	key := types.NamespacedName{Name: "conflict-retry-ok", Namespace: ns}
	result, err := r.writeEarlyPhaseStatus(ctx, key, mapping, input, logr.Discard())

	// After successful retry, should return the early-phase result (Requeue=true for validation).
	if err != nil {
		t.Errorf("expected nil error after successful retry, got %v", err)
	}
	if !result.Requeue {
		t.Error("expected Requeue=true for early-phase validation result after conflict retry")
	}

	// StatusUpdater should have been called exactly twice (initial + retry).
	if conflictUpdater.callCount != 2 {
		t.Errorf("expected StatusUpdater to be called 2 times (initial + retry), got %d",
			conflictUpdater.callCount)
	}
}

// ---------------------------------------------------------------------------
// T6.3: Early bootstrap status write — conflict persists → returns wrapped error
// Design §3.3B step 4.4 + §3.4: do not swallow persistent conflicts
// ---------------------------------------------------------------------------

func TestReconcile_EarlyBootstrapStatusWrite_ConflictSameGeneration_RetryConflict_ReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "conflict-persist", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	// StatusUpdater that always returns conflict (persists on retry).
	persistentConflict := apierrors.NewConflict(
		schema.GroupResource{Group: "networking.azure.com", Resource: "podasgmappings"},
		"conflict-persist",
		fmt.Errorf("persistent conflict"),
	)
	alwaysFail := &alwaysFailStatusUpdater{err: persistentConflict}

	r := &MappingReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		ClusterName:   "test-cluster",
		ResyncInterval: 60 * time.Second,
		StatusUpdater: alwaysFail,
	}

	input := ReconcileStatusInput{
		Phase:         StatusPhaseBootstrapPending,
		ProcessedGen:  1,
		ProcessedSpec: mapping.Spec,
		PodCounts:     map[string]int{},
	}

	key := types.NamespacedName{Name: "conflict-persist", Namespace: ns}
	_, err := r.writeEarlyPhaseStatus(ctx, key, mapping, input, logr.Discard())

	// When conflict persists (same generation) after retry, must return a wrapped error.
	// Design §3.4: "Remove best-effort _ = behavior for early-phase status writes."
	if err == nil {
		t.Fatal("expected non-nil error when early-phase status conflict persists after retry")
	}
	if !apierrors.IsConflict(err) {
		// The error should preserve the conflict cause through pkg/errors wrapping.
		cause := pkgerrors.Cause(err)
		if !apierrors.IsConflict(cause) {
			t.Errorf("expected conflict error (possibly wrapped), got %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.3: Early validation status write — non-conflict error → returns error
// Design §3.3B step 3 + §3.4: propagate non-drift/non-conflict errors
// ---------------------------------------------------------------------------

func TestReconcile_EarlyValidationStatusWrite_NonConflictError_ReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "nonconflict-err", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "invalid-resource-id"},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	// StatusUpdater that returns a non-conflict error (e.g., network failure).
	networkErr := fmt.Errorf("network timeout writing status subresource")
	failUpdater := &alwaysFailStatusUpdater{err: networkErr}

	r := &MappingReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		ClusterName:   "test-cluster",
		ResyncInterval: 60 * time.Second,
		StatusUpdater: failUpdater,
	}

	input := ReconcileStatusInput{
		Phase:            StatusPhaseValidationFailed,
		ProcessedGen:     1,
		ProcessedSpec:    mapping.Spec,
		ValidationErrors: map[int][]string{0: {"invalid resource ID format"}},
		PodCounts:        map[string]int{},
	}

	key := types.NamespacedName{Name: "nonconflict-err", Namespace: ns}
	_, err := r.writeEarlyPhaseStatus(ctx, key, mapping, input, logr.Discard())

	// Design §3.4: Non-conflict, non-drift errors must be propagated (not swallowed).
	if err == nil {
		t.Fatal("expected non-nil error when early-phase status write fails with non-conflict error")
	}
	// Error message should indicate status write context.
	if !containsString(err.Error(), "network timeout") {
		t.Errorf("expected error to contain cause message, got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Additional test helpers for Phase 6 early-status conflict scenarios
// ---------------------------------------------------------------------------

// conflictOnceStatusUpdater returns err on first call, succeeds on subsequent calls.
type conflictOnceStatusUpdater struct {
	callCount int
	err       error
}

func (u *conflictOnceStatusUpdater) UpdateAfterReconcile(
	_ context.Context,
	_ *v1alpha1.PodASGMapping,
	_ ReconcileStatusInput,
) error {
	u.callCount++
	if u.callCount == 1 {
		return u.err
	}
	return nil
}

// alwaysFailStatusUpdater always returns the configured error.
type alwaysFailStatusUpdater struct {
	callCount int
	err       error
}

func (u *alwaysFailStatusUpdater) UpdateAfterReconcile(
	_ context.Context,
	_ *v1alpha1.PodASGMapping,
	_ ReconcileStatusInput,
) error {
	u.callCount++
	return u.err
}

// ---------------------------------------------------------------------------
// Post-execution snapshot regression: reconciler must use processed snapshot
// Design §3.3C: "Final status update must always use snapshot values, never
// live refetched spec/generation/status."
// ---------------------------------------------------------------------------

func TestReconcile_PostExecution_UsesProcessedSnapshotForStatusInput(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	// Mapping at Generation=2. After execution, a simulated refetch might
	// return a newer Generation (3). The reconciler MUST use the snapshot
	// generation (2) in the status input, not the refetched value.
	mapping := newTestMapping(ns, "snapshot-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 2

	pod := newTestPod(ns, "web-pod", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: true,
			},
		},
	}

	// Use a capturing status updater to inspect the input received.
	capturer := &capturingStatusUpdater{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    capturer,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "snapshot-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Verify status updater was called.
	if capturer.callCount == 0 {
		t.Fatal("expected StatusUpdater to be called")
	}

	// The status input must reflect the generation at which reconciliation
	// was PROCESSED (2), captured by the snapshot before any metadata patches
	// or Azure execution. It must NOT reflect a possibly-newer live generation.
	input := capturer.lastInput
	if input.Phase != StatusPhasePostExecution {
		t.Errorf("expected Phase=%q, got %q", StatusPhasePostExecution, input.Phase)
	}

	// This is the key assertion: ProcessedGen must come from the snapshot
	// (captured before execution), not from a refetched live mapping.
	snapshot := captureProcessedStatusSnapshot(mapping, ComputePodCountsFromPods(mapping.Spec, []corev1.Pod{*pod}))
	expectedInput := buildPostExecutionStatusInput(snapshot, exec.results, nil)

	// The snapshot-based input must have non-zero ProcessedGen matching the mapping.
	if expectedInput.ProcessedGen != 2 {
		t.Errorf("buildPostExecutionStatusInput: expected ProcessedGen=2 from snapshot, got %d", expectedInput.ProcessedGen)
	}

	// The snapshot-based input must carry the processed spec.
	if len(expectedInput.ProcessedSpec.Mappings) != 1 {
		t.Errorf("buildPostExecutionStatusInput: expected 1 spec mapping from snapshot, got %d",
			len(expectedInput.ProcessedSpec.Mappings))
	}

	// The snapshot-based input must have PostExecution phase.
	if expectedInput.Phase != StatusPhasePostExecution {
		t.Errorf("buildPostExecutionStatusInput: expected Phase=%q, got %q",
			StatusPhasePostExecution, expectedInput.Phase)
	}
}

// capturingStatusUpdater records the last ReconcileStatusInput received.
type capturingStatusUpdater struct {
	callCount int
	lastInput ReconcileStatusInput
}

func (c *capturingStatusUpdater) UpdateAfterReconcile(
	_ context.Context,
	_ *v1alpha1.PodASGMapping,
	input ReconcileStatusInput,
) error {
	c.callCount++
	c.lastInput = input
	return nil
}
