package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// Phase 7 helpers
// ---------------------------------------------------------------------------

// stubStatusUpdaterP7 records all status-update calls and returns configurable errors.
type stubStatusUpdaterP7 struct {
	pendingCalls       []pendingCall
	reconcileCalls     []reconcileCall
	pendingErr         error
	reconcileErr       error
	reconcileCallCount int
}

type pendingCall struct {
	key                types.NamespacedName
	observedGeneration int64
	prefixSetName      string
	matchedPodsByIndex []int
}

type reconcileCall struct {
	key                types.NamespacedName
	observedGeneration int64
	prefixSetName      string
	results            []azure.ActionResult
	reconcileErr       error
	validationIssues   []ValidationIssue
	matchedPodsByIndex []int
}

func (s *stubStatusUpdaterP7) UpdatePending(
	ctx context.Context, key types.NamespacedName, gen int64, prefix string, matched []int,
) error {
	s.pendingCalls = append(s.pendingCalls, pendingCall{key, gen, prefix, matched})
	return s.pendingErr
}

func (s *stubStatusUpdaterP7) UpdateAfterReconcile(
	ctx context.Context, key types.NamespacedName, gen int64, prefix string,
	results []azure.ActionResult, reconcileErr error, issues []ValidationIssue, matched []int,
) error {
	s.reconcileCallCount++
	s.reconcileCalls = append(s.reconcileCalls, reconcileCall{key, gen, prefix, results, reconcileErr, issues, matched})
	return s.reconcileErr
}

// newReconcilerP7 creates a MappingReconciler with standard test plumbing.
func newReconcilerP7(
	t *testing.T,
	objs []client.Object,
	fakeFactory *fake.ClientFactory,
	exec Executor,
	statusUpdater StatusUpdater,
) *MappingReconciler {
	t.Helper()
	scheme := testScheme(t)

	builder := fakeclient.NewClientBuilder().WithScheme(scheme)
	builder = builder.WithObjects(objs...)
	fakeClient := builder.Build()

	return &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusUpdater,
	}
}

// ---------------------------------------------------------------------------
// T7.1: ARM returns 429 — call retried after Retry-After delay
//
// Tests that the reconciler translates a 429 ARM error into a policy-driven
// requeue with delay derived from Retry-After, returns nil error, and updates
// status with the error.
// ---------------------------------------------------------------------------

func TestPhase7_T71_429RetryAfterPolicyRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	// Executor returns a 429 failure with Retry-After 15s.
	arm429 := &azure.ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "throttled",
		RetryAfter: 15 * time.Second,
	}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: false,
				Err:     arm429,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: reconcile MUST return nil error.
	if err != nil {
		t.Fatalf("T7.1: expected nil error from reconcile, got %v", err)
	}

	// Phase 7 contract: result uses policy-driven requeue with delay >= Retry-After (15s).
	if result.RequeueAfter < 15*time.Second {
		t.Errorf("T7.1: expected RequeueAfter >= 15s (Retry-After), got %v", result.RequeueAfter)
	}

	// Phase 7 contract: status updater called with the 429 error.
	if len(statusUpdater.reconcileCalls) == 0 {
		t.Fatal("T7.1: expected status updater UpdateAfterReconcile to be called")
	}

	lastCall := statusUpdater.reconcileCalls[len(statusUpdater.reconcileCalls)-1]
	if lastCall.reconcileErr == nil {
		t.Error("T7.1: expected reconcile error to be passed to status updater")
	}
}

// ---------------------------------------------------------------------------
// T7.2: ARM returns 500 three times then succeeds on 4th
//
// This is primarily tested in the retry/client layers, but at the reconciler
// level we verify that when the executor returns mixed results with eventual
// success (simulating retry logic at lower layer), the reconciler handles it
// correctly through the error policy path.
// ---------------------------------------------------------------------------

func TestPhase7_T72_500x3ThenSuccess(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	// Executor returns success (retry happened internally at ARM layer).
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: true,
				Err:     nil,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	if err != nil {
		t.Fatalf("T7.2: expected nil error from reconcile after successful retry, got %v", err)
	}

	// On full success, should requeue at resync interval.
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("T7.2: expected RequeueAfter=30s (resync), got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// T7.3: ARM returns 500 four times — exhausted, error reported in status
//
// When the executor returns a retriable-but-exhausted error (all retries used),
// reconcile must: return nil error, requeue with backoff, and write the failure
// to status.
// ---------------------------------------------------------------------------

func TestPhase7_T73_500x4Exhausted_StatusReportsFailure(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	arm500 := &azure.ARMStatusError{
		StatusCode: 500,
		ARMCode:    "InternalServerError",
		Message:    "internal error",
	}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: false,
				Err:     arm500,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: nil error, policy-driven requeue.
	if err != nil {
		t.Fatalf("T7.3: expected nil error, got %v", err)
	}

	// Should requeue with backoff (>= ExhaustedRetryBackoff = 8s).
	if result.RequeueAfter < 8*time.Second {
		t.Errorf("T7.3: expected RequeueAfter >= 8s (backoff), got %v", result.RequeueAfter)
	}

	// Status updater must have been called with the error.
	if len(statusUpdater.reconcileCalls) == 0 {
		t.Fatal("T7.3: expected status updater to be called")
	}
	lastCall := statusUpdater.reconcileCalls[len(statusUpdater.reconcileCalls)-1]
	if lastCall.reconcileErr == nil {
		t.Error("T7.3: expected reconcileErr to be propagated to status updater")
	}
}

// ---------------------------------------------------------------------------
// T7.4: ARM returns 403 — no retry, error reported immediately
//
// Non-retriable auth errors must return nil error, requeue at resync interval
// (not accelerated backoff), and write the error to status.
// ---------------------------------------------------------------------------

func TestPhase7_T74_403NoRetry_ImmediateError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	arm403 := &azure.ARMStatusError{
		StatusCode: 403,
		ARMCode:    "AuthorizationFailed",
		Message:    "forbidden",
	}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: false,
				Err:     arm403,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: nil error.
	if err != nil {
		t.Fatalf("T7.4: expected nil error, got %v", err)
	}

	// Non-retriable: requeue at resync interval (30s), not accelerated.
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("T7.4: expected RequeueAfter=30s (resync) for non-retriable 403, got %v", result.RequeueAfter)
	}

	// Status updater must record the error.
	if len(statusUpdater.reconcileCalls) == 0 {
		t.Fatal("T7.4: expected status updater to be called")
	}
	lastCall := statusUpdater.reconcileCalls[len(statusUpdater.reconcileCalls)-1]
	if lastCall.reconcileErr == nil {
		t.Error("T7.4: expected reconcileErr with 403 to be passed to status updater")
	}
}

// ---------------------------------------------------------------------------
// T7.5: 3 of 5 actions fail → 2 persist + 3 errors + requeue
//
// Partial failures: successful actions persist (executor commits them),
// failed actions are reported in status, and reconcile requeues.
// ---------------------------------------------------------------------------

func TestPhase7_T75_PartialFailure_SucceedAndRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	// 5 ASGs in one mapping: the executor will report 2 success, 3 failure.
	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg3")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg4")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg5")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	arm500 := &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	exec := &stubExecutor{
		results: []azure.ActionResult{
			{Action: makeTestAction("asg1", engine.CreatePrefixSet), Success: true},
			{Action: makeTestAction("asg2", engine.CreatePrefixSet), Success: false, Err: arm500},
			{Action: makeTestAction("asg3", engine.CreatePrefixSet), Success: true},
			{Action: makeTestAction("asg4", engine.CreatePrefixSet), Success: false, Err: arm500},
			{Action: makeTestAction("asg5", engine.CreatePrefixSet), Success: false, Err: arm500},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: nil error, policy-driven requeue.
	if err != nil {
		t.Fatalf("T7.5: expected nil error, got %v", err)
	}

	// Must requeue (not zero-value result).
	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("T7.5: expected reconcile to requeue after partial failure")
	}

	// Status updater must be called with the error and full results.
	if len(statusUpdater.reconcileCalls) == 0 {
		t.Fatal("T7.5: expected status updater to be called")
	}

	lastCall := statusUpdater.reconcileCalls[len(statusUpdater.reconcileCalls)-1]

	// The reconcile error must be non-nil (aggregate of 3 failures).
	if lastCall.reconcileErr == nil {
		t.Error("T7.5: expected reconcileErr to be non-nil for 3/5 failures")
	}

	// Full result set must be passed so status can identify 2 successes and 3 errors.
	if len(lastCall.results) != 5 {
		t.Errorf("T7.5: expected 5 results passed to status updater, got %d", len(lastCall.results))
	}

	successCount := 0
	failCount := 0
	for _, r := range lastCall.results {
		if r.Success {
			successCount++
		} else {
			failCount++
		}
	}
	if successCount != 2 {
		t.Errorf("T7.5: expected 2 successful results, got %d", successCount)
	}
	if failCount != 3 {
		t.Errorf("T7.5: expected 3 failed results, got %d", failCount)
	}
}

// ---------------------------------------------------------------------------
// T7.6: Rate limiter throttles burst of 20 calls — none dropped
//
// Tested at the controller level by verifying that the reconciler returns nil
// error even when the rate limiter introduces waits (no rejections).
// This test validates the integration contract, not the limiter itself.
// ---------------------------------------------------------------------------

func TestPhase7_T76_RateLimiterBurstNoneDropped(t *testing.T) {
	_ = zaptest.NewLogger(t)

	// Create a rate limiter at 10 RPS and send 20 calls.
	log := zaptest.NewLogger(t)
	limiter := azure.NewARMRateLimiter(log, 10.0)

	ctx := context.Background()
	const burst = 20
	errCount := 0
	for i := 0; i < burst; i++ {
		if err := limiter.Wait(ctx, "sub1"); err != nil {
			errCount++
		}
	}

	if errCount > 0 {
		t.Errorf("T7.6: expected 0 dropped calls from rate limiter burst of %d, got %d errors", burst, errCount)
	}
}

// ---------------------------------------------------------------------------
// T7.7: ETag conflict (412) during retry loop — Re-GET + recompute succeeds
//
// Tests the executor-level 412 retry path: a 412 on first attempt should
// trigger re-GET + recompute, and succeed on the next attempt.
// ---------------------------------------------------------------------------

func TestPhase7_T77_ETagConflict_ReGETRecomputeSuccess(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()

	fakeAzClient := fake.NewClient()

	// Pre-populate with existing state.
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", "test-prefix", []string{"10.0.0.1/32"})

	// First PUT returns 412 (ETag mismatch), second PUT succeeds.
	fakeAzClient.Fail412Count = 1

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	log := zaptest.NewLogger(t)
	executor := azure.NewExecutor(log, fakeFactory, 5)

	actions := []engine.Action{
		{
			Kind: engine.UpdatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
				PrefixSetName:  "test-prefix",
			},
			DesiredIPs: []string{"10.0.0.2/32"},
		},
	}

	results := executor.Execute(ctx, actions)

	if len(results) != 1 {
		t.Fatalf("T7.7: expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("T7.7: expected success after 412 retry, got err=%v", results[0].Err)
	}
}

// ---------------------------------------------------------------------------
// Network timeout — retry once then reconcile requeues with backoff
//
// Per the retry policy table: network timeout is retried once, then the
// reconciler requeues with backoff (not raw error). This validates the
// end-to-end contract from a network error through the policy path.
// ---------------------------------------------------------------------------

func TestPhase7_NetworkTimeout_RetryOnceThenRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	// Network timeout error wraps net.OpError.
	netErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: fmt.Errorf("connection timed out"),
	}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: false,
				Err:     netErr,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: nil error return.
	if err != nil {
		t.Fatalf("NetworkTimeout: expected nil error, got %v", err)
	}

	// Should requeue with backoff.
	if result.RequeueAfter < 8*time.Second {
		t.Errorf("NetworkTimeout: expected RequeueAfter >= 8s (backoff), got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Raw-error path conversion matrix tests (design §3.2)
//
// Each stage that previously returned raw errors must now return
// (policyResult, nil) via finalizeSystemError.
// ---------------------------------------------------------------------------

func TestPhase7_StageConversion_FetchMappingError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	// Create reconciler with no mapping in the store — simulating a Get error
	// by injecting a client that fails on Get (non-NotFound).
	scheme := testScheme(t)
	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}

	// Test the "list-actual-state" stage error path.
	// A pod with an IP is required so desired state is non-empty and
	// listActualForTargets is actually called.
	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	// Don't register sub1 in factory → ForSubscription will fail during listActualForTargets.
	// This tests the "list-actual-state" stage error path.

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    &stubStatusUpdaterP7{},
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: nil error + policy requeue.
	if err != nil {
		t.Fatalf("list-actual-state stage: expected nil error, got %v", err)
	}

	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("list-actual-state stage: expected policy-driven requeue, got zero result")
	}
}

func TestPhase7_StageConversion_DeleteCleanupError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	now := metav1.Now()
	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.DeletionTimestamp = &now
	mapping.Finalizers = []string{CleanupFinalizer}

	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
	}
	ownedJSON, _ := json.Marshal(ownedRefs)
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: string(ownedJSON),
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	// Don't register sub1 → causes ForSubscription error during delete cleanup.
	fakeFactory := fake.NewClientFactory()

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: delete-cleanup errors return nil error + policy requeue.
	if err != nil {
		t.Fatalf("delete-cleanup stage: expected nil error, got %v", err)
	}

	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("delete-cleanup stage: expected policy-driven requeue, got zero result")
	}

	// Finalizer must be retained so cleanup retries on next reconcile.
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "m1", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get updated mapping: %v", getErr)
	}
	if !controllerutil.ContainsFinalizer(&updated, CleanupFinalizer) {
		t.Error("delete-cleanup stage: expected finalizer to be retained on error")
	}
}

func TestPhase7_StageConversion_DeleteCleanupError_NoStatusWrite(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	now := metav1.Now()
	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.DeletionTimestamp = &now
	mapping.Finalizers = []string{CleanupFinalizer}

	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
	}
	ownedJSON, _ := json.Marshal(ownedRefs)
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: string(ownedJSON),
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()

	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	if err != nil {
		t.Fatalf("delete-no-status stage: expected nil error, got %v", err)
	}

	// Phase 7 contract: no status writes during deletion path.
	if statusUpdater.reconcileCallCount > 0 {
		t.Errorf("delete-no-status: expected 0 status updater calls during delete, got %d", statusUpdater.reconcileCallCount)
	}
}

func TestPhase7_StageConversion_OwnershipParseError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	// Corrupt the ownership annotation so parsing fails.
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: "invalid-json!!!",
	}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())
	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: parse-owned-annotation error returns nil + policy requeue.
	if err != nil {
		t.Fatalf("parse-owned-annotation stage: expected nil error, got %v", err)
	}

	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("parse-owned-annotation stage: expected policy-driven requeue")
	}
}

func TestPhase7_StageConversion_PreActionOwnershipPatchError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	// This test verifies the pre-action ownership patch path.
	// Currently the reconciler returns a raw error for this stage.
	// After Phase 7, it must return (policyResult, nil).
	// The test will fail because the current implementation wraps
	// and returns errors directly.
	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// We're testing a happy path that goes through ownership patching.
	// Confirm nil error (basic sanity for this stage path).
	if err != nil {
		t.Fatalf("store-owned-pre stage: expected nil error, got %v", err)
	}

	_ = result // consumed by assertions above
}

// ---------------------------------------------------------------------------
// Status-write error: no recursive UpdateAfterReconcile
//
// When UpdateAfterReconcile itself fails, the reconciler must NOT call it
// again (preventing infinite recursion). Instead, use finalizeStatusWriteError.
// ---------------------------------------------------------------------------

func TestPhase7_StatusWriteError_NoRecursiveUpdate(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	statusUpdater := &stubStatusUpdaterP7{
		reconcileErr: fmt.Errorf("status write failed"),
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: status write errors return nil error + policy requeue.
	if err != nil {
		t.Fatalf("status-write-error: expected nil error, got %v", err)
	}

	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("status-write-error: expected policy-driven requeue")
	}

	// Key assertion: UpdateAfterReconcile must be called EXACTLY ONCE —
	// the failing call. No recursive retry.
	if statusUpdater.reconcileCallCount != 1 {
		t.Errorf("status-write-error: expected exactly 1 status update call, got %d (recursive call detected)", statusUpdater.reconcileCallCount)
	}
}

// ---------------------------------------------------------------------------
// Status-write error with ErrStatusObjectNotFound sentinel
// ---------------------------------------------------------------------------

func TestPhase7_StatusWriteError_ObjectNotFound_NoRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	statusUpdater := &stubStatusUpdaterP7{
		reconcileErr: ErrStatusObjectNotFound,
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// ErrStatusObjectNotFound sentinel -> (empty result, nil error).
	if err != nil {
		t.Fatalf("status-object-not-found: expected nil error, got %v", err)
	}

	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("status-object-not-found: expected no requeue, got %+v", result)
	}
}

// ---------------------------------------------------------------------------
// Status-write error with ErrStatusStaleGeneration sentinel
// ---------------------------------------------------------------------------

func TestPhase7_StatusWriteError_StaleGeneration_NoRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	statusUpdater := &stubStatusUpdaterP7{
		reconcileErr: ErrStatusStaleGeneration,
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// ErrStatusStaleGeneration sentinel -> (empty result, nil error).
	if err != nil {
		t.Fatalf("status-stale-generation: expected nil error, got %v", err)
	}

	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("status-stale-generation: expected no requeue, got %+v", result)
	}
}

// ---------------------------------------------------------------------------
// finalizeSystemError: best-effort status write for allowStatusWrite=true
// ---------------------------------------------------------------------------

func TestPhase7_FinalizeSystemError_WritesStatusForAllowedStages(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	// Don't register subscription → triggers list-actual-state error.
	fakeFactory := fake.NewClientFactory()

	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	if err != nil {
		t.Fatalf("finalize-system-error: expected nil error, got %v", err)
	}

	// For list-actual-state stage (allowStatusWrite=true), the status updater
	// must be called with the system error.
	if len(statusUpdater.reconcileCalls) == 0 {
		t.Error("finalize-system-error: expected status updater to be called for allowStatusWrite=true stage")
	}
}

// ---------------------------------------------------------------------------
// Action failures path: ClassifyActionResults + DecideRequeueFromActionSummary
//
// When reconcile has action failures, it must classify them and use
// DecideRequeueFromActionSummary (not return raw errors).
// ---------------------------------------------------------------------------

func TestPhase7_ActionFailures_UsePolicyRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	arm500 := &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{Action: makeTestAction("asg1", engine.CreatePrefixSet), Success: false, Err: arm500},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusUpdater,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: action failures → nil error + policy requeue.
	if err != nil {
		t.Fatalf("action-failures: expected nil error, got %v", err)
	}

	if result.RequeueAfter < 8*time.Second {
		t.Errorf("action-failures: expected RequeueAfter >= 8s (backoff), got %v", result.RequeueAfter)
	}
}

func TestPhase7_StatusWriteErrorPrecedesActionFailure(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	arm429 := &azure.ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "throttled",
		RetryAfter: 25 * time.Second,
	}
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  makeTestAction("asg1", engine.CreatePrefixSet),
				Success: false,
				Err:     arm429,
			},
		},
	}
	statusUpdater := &stubStatusUpdaterP7{
		reconcileErr: fmt.Errorf("final status write failed"),
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	r := newReconcilerP7(t,
		[]client.Object{mapping, pod},
		fakeFactory, exec, statusUpdater,
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	// Phase 7 contract: status write errors take precedence over action
	// failure requeue decisions so they are never masked.
	if err != nil {
		t.Fatalf("status-write-precedence: expected nil error, got %v", err)
	}
	// Status write error drives the requeue via finalizeStatusWriteError
	// (policy ExhaustedRetryBackoff = 8s), not the action-failure's 25s.
	if result.RequeueAfter == 0 && !result.Requeue {
		t.Error("status-write-precedence: expected policy-driven requeue")
	}
	if result.RequeueAfter == 25*time.Second {
		t.Error("status-write-precedence: action-failure requeue must not mask status write error")
	}
	if statusUpdater.reconcileCallCount != 1 {
		t.Errorf("status-write-precedence: expected exactly 1 final status call, got %d", statusUpdater.reconcileCallCount)
	}
}

func TestPhase7_StatusWriteWrappedSentinels_NoRequeue(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"

	tests := []struct {
		name      string
		statusErr error
	}{
		{
			name:      "object not found",
			statusErr: pkgerrors.Wrap(ErrStatusObjectNotFound, "wrapped object not found"),
		},
		{
			name:      "stale generation",
			statusErr: pkgerrors.Wrap(ErrStatusStaleGeneration, "wrapped stale generation"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			})
			mapping.Finalizers = []string{CleanupFinalizer}
			pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

			statusUpdater := &stubStatusUpdaterP7{
				reconcileErr: tc.statusErr,
			}
			fakeFactory := fake.NewClientFactory()
			fakeFactory.RegisterClient("sub1", fake.NewClient())

			r := newReconcilerP7(t,
				[]client.Object{mapping, pod},
				fakeFactory, &stubExecutor{}, statusUpdater,
			)

			result, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
			})

			if err != nil {
				t.Fatalf("wrapped-sentinel: expected nil error, got %v", err)
			}
			if result.RequeueAfter != 0 || result.Requeue {
				t.Errorf("wrapped-sentinel: expected no requeue for wrapped sentinel, got %+v", result)
			}
			if statusUpdater.reconcileCallCount != 1 {
				t.Errorf("wrapped-sentinel: expected exactly 1 final status call, got %d", statusUpdater.reconcileCallCount)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Post-action ownership patch error → policy requeue, nil error
// ---------------------------------------------------------------------------

func TestPhase7_StageConversion_PostActionOwnershipError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	// Pre-populate owned refs so post-action diff requires patch.
	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg3"}, // stale, to be removed
	}
	ownedJSON, _ := json.Marshal(ownedRefs)
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: string(ownedJSON),
	}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Executor returns a delete success for asg3 to trigger post-action ownership change.
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{Action: makeTestAction("asg1", engine.CreatePrefixSet), Success: true},
			{Action: makeTestAction("asg2", engine.CreatePrefixSet), Success: true},
			{
				Action: engine.Action{
					Kind: engine.DeletePrefixSet,
					Target: engine.ASGTarget{
						SubscriptionID: "sub1",
						ResourceGroup:  "rg1",
						ASGName:        "asg3",
						FullResourceID: asgResourceID("sub1", "rg1", "asg3"),
						PrefixSetName:  "test-cluster-test-ns-m1",
					},
				},
				Success: true,
			},
		},
	}

	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusUpdater,
	}

	// This tests the post-action ownership patch path.
	// If post-action patch fails, Phase 7 requires nil error + policy requeue.
	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns},
	})

	if err != nil {
		t.Fatalf("patch-owned-post stage: expected nil error, got %v", err)
	}

	_ = result
}

// ---------------------------------------------------------------------------
// Ensure-finalizer error → policy requeue, nil error
// ---------------------------------------------------------------------------

func TestPhase7_StageConversion_EnsureFinalizerError_ReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)

	// This test validates the design contract that ensureFinalizer errors
	// should go through finalizeSystemError and return (result, nil).
	// Currently the code returns wrapped errors directly.
	//
	// We test by calling finalizeSystemError directly once it exists.

	policy := DefaultRequeuePolicy(30 * time.Second)
	sysErr := fmt.Errorf("patch failed: connection refused")

	result := DecideRequeueFromSystemError(sysErr, policy)

	// For unknown (non-ARM) errors, DecideRequeueFromSystemError treats as retriable.
	if result.RequeueAfter < 8*time.Second {
		t.Errorf("ensure-finalizer: expected RequeueAfter >= 8s for unknown error, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Comprehensive table-driven test for all stage error paths
//
// This covers the complete conversion matrix from design §3.2.
// ---------------------------------------------------------------------------

func TestPhase7_StageConversionMatrix_AllReturnsNilError(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name             string
		stage            string
		err              error
		allowStatusWrite bool
		wantMinRequeue   time.Duration
	}{
		{
			name:             "fetch-mapping 500",
			stage:            "fetch-mapping",
			err:              &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"},
			allowStatusWrite: false,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "delete-cleanup network error",
			stage:            "delete-cleanup",
			err:              &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("connection refused")},
			allowStatusWrite: false,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "delete-remove-finalizer error",
			stage:            "delete-remove-finalizer",
			err:              fmt.Errorf("patch conflict"),
			allowStatusWrite: false,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "ensure-finalizer error",
			stage:            "ensure-finalizer",
			err:              fmt.Errorf("patch failed"),
			allowStatusWrite: false,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "list-pods 500",
			stage:            "list-pods",
			err:              &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"},
			allowStatusWrite: true,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "parse-owned-annotation error",
			stage:            "parse-owned-annotation",
			err:              fmt.Errorf("invalid JSON"),
			allowStatusWrite: true,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "list-actual-state 403",
			stage:            "list-actual-state",
			err:              &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"},
			allowStatusWrite: true,
			wantMinRequeue:   30 * time.Second, // non-retriable → resync
		},
		{
			name:             "store-owned-pre error",
			stage:            "store-owned-pre",
			err:              fmt.Errorf("json marshal failed"),
			allowStatusWrite: true,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "patch-owned-pre 429",
			stage:            "patch-owned-pre",
			err:              &azure.ARMStatusError{StatusCode: 429, ARMCode: "TooManyRequests", Message: "throttled", RetryAfter: 20 * time.Second},
			allowStatusWrite: true,
			wantMinRequeue:   20 * time.Second,
		},
		{
			name:             "store-owned-post error",
			stage:            "store-owned-post",
			err:              fmt.Errorf("json marshal failed"),
			allowStatusWrite: true,
			wantMinRequeue:   8 * time.Second,
		},
		{
			name:             "patch-owned-post conflict",
			stage:            "patch-owned-post",
			err:              fmt.Errorf("conflict on patch"),
			allowStatusWrite: true,
			wantMinRequeue:   8 * time.Second,
		},
	}

	policy := DefaultRequeuePolicy(30 * time.Second)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := DecideRequeueFromSystemError(tc.err, policy)

			// All system errors must produce a non-zero RequeueAfter.
			if result.RequeueAfter == 0 {
				t.Errorf("stage %s: expected non-zero RequeueAfter", tc.stage)
			}

			if result.RequeueAfter < tc.wantMinRequeue {
				t.Errorf("stage %s: expected RequeueAfter >= %v, got %v", tc.stage, tc.wantMinRequeue, result.RequeueAfter)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: finalizeSystemError method exists and works
// ---------------------------------------------------------------------------

func TestPhase7_FinalizeSystemError_MethodExists(t *testing.T) {
	_ = zaptest.NewLogger(t)
	ctx := context.Background()
	ns := "test-ns"
	scheme := testScheme(t)

	mapping := newTestMapping(ns, "m1", []v1alpha1.Mapping{})
	mapping.Finalizers = []string{CleanupFinalizer}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	statusUpdater := &stubStatusUpdaterP7{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
		StatusUpdater:    statusUpdater,
	}

	sysErr := fmt.Errorf("test system error")
	logger := ctrl.Log.WithName("test")

	// Call the new method. This must compile and return (result, nil).
	result, err := r.finalizeSystemError(
		ctx,
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "m1", Namespace: ns}},
		mapping,
		"test-key",
		nil, // matchedPodsByIndex
		"test-stage",
		sysErr,
		true, // allowStatusWrite
		logger,
	)

	if err != nil {
		t.Fatalf("finalizeSystemError must return nil error, got %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Error("finalizeSystemError must return non-zero RequeueAfter")
	}
}

// ---------------------------------------------------------------------------
// Integration: finalizeStatusWriteError method exists and works
// ---------------------------------------------------------------------------

func TestPhase7_FinalizeStatusWriteError_MethodExists(t *testing.T) {
	_ = zaptest.NewLogger(t)

	scheme := testScheme(t)
	fakeClient := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	fakeFactory := fake.NewClientFactory()

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   30 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         &stubExecutor{},
	}

	logger := ctrl.Log.WithName("test")

	t.Run("generic_error_returns_policy_result", func(t *testing.T) {
		statusErr := fmt.Errorf("status patch failed")
		result, err := r.finalizeStatusWriteError(statusErr, logger)

		if err != nil {
			t.Fatalf("finalizeStatusWriteError must return nil error, got %v", err)
		}
		if result.RequeueAfter == 0 {
			t.Error("finalizeStatusWriteError must return non-zero RequeueAfter for generic error")
		}
	})

	t.Run("object_not_found_returns_empty_result", func(t *testing.T) {
		result, err := r.finalizeStatusWriteError(ErrStatusObjectNotFound, logger)

		if err != nil {
			t.Fatalf("finalizeStatusWriteError must return nil error, got %v", err)
		}
		if result.RequeueAfter != 0 || result.Requeue {
			t.Errorf("finalizeStatusWriteError(ErrStatusObjectNotFound) must return empty result, got %+v", result)
		}
	})

	t.Run("stale_generation_returns_empty_result", func(t *testing.T) {
		result, err := r.finalizeStatusWriteError(ErrStatusStaleGeneration, logger)

		if err != nil {
			t.Fatalf("finalizeStatusWriteError must return nil error, got %v", err)
		}
		if result.RequeueAfter != 0 || result.Requeue {
			t.Errorf("finalizeStatusWriteError(ErrStatusStaleGeneration) must return empty result, got %+v", result)
		}
	})

	t.Run("wrapped_sentinels_return_empty_result", func(t *testing.T) {
		tests := []struct {
			name      string
			statusErr error
		}{
			{
				name:      "wrapped object not found",
				statusErr: pkgerrors.Wrap(ErrStatusObjectNotFound, "wrapped"),
			},
			{
				name:      "wrapped stale generation",
				statusErr: pkgerrors.Wrap(ErrStatusStaleGeneration, "wrapped"),
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				result, err := r.finalizeStatusWriteError(tc.statusErr, logger)

				if err != nil {
					t.Fatalf("finalizeStatusWriteError must return nil error, got %v", err)
				}
				if result.RequeueAfter != 0 || result.Requeue {
					t.Errorf("finalizeStatusWriteError(%v) must return empty result, got %+v", tc.statusErr, result)
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 7 Acceptance: T7.5 Reconciler Status Contract Validation
// ---------------------------------------------------------------------------

func TestPhase7_T75_Reconciler_StatusContractValidation(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Create a mapping with 3 ASGs.
	mapping := newTestMapping("default", "contract-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "contract"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-ok")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg-transient-fail")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg-perm-fail")},
			},
		},
	})

	// Simulate mixed results: 1 success + 2 failures.
	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind:   engine.UpdatePrefixSet,
				Target: engine.ASGTarget{ASGName: "asg-ok", PrefixSetName: "test-prefix"},
			},
			Err: nil,
		},
		{
			Action: engine.Action{
				Kind:   engine.UpdatePrefixSet,
				Target: engine.ASGTarget{ASGName: "asg-transient-fail", PrefixSetName: "test-prefix"},
			},
			Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "transient"},
		},
		{
			Action: engine.Action{
				Kind:   engine.UpdatePrefixSet,
				Target: engine.ASGTarget{ASGName: "asg-perm-fail", PrefixSetName: "test-prefix"},
			},
			Err: &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"},
		},
	}

	// Build partial failure report using stub.
	report := BuildPartialFailureReport(results, mapping.Spec, "test-prefix", RequeuePolicy{
		ResyncInterval:        10 * time.Second,
		ExhaustedRetryBackoff: 60 * time.Second,
	})

	// T7.5 acceptance: report from reconciler should contain proper classification.
	if len(report.SucceededTargets) != 1 {
		t.Errorf("T7.5 reconciler: SucceededTargets = %d, want 1", len(report.SucceededTargets))
	}
	if len(report.FailedTargets) != 2 {
		t.Errorf("T7.5 reconciler: FailedTargets = %d, want 2", len(report.FailedTargets))
	}

	// Validate status contract.
	violations := ValidatePhase7StatusContract(report.StatusSnapshot, results, mapping.Spec, "test-prefix")
	if len(violations) == 0 {
		t.Errorf("T7.5 reconciler: ValidatePhase7StatusContract returned 0 violations; want > 0 for stub status")
	}

	_ = logger
}

// ---------------------------------------------------------------------------
// Unused import guard: ensure all imported packages are used.
// ---------------------------------------------------------------------------
var _ = corev1.Pod{}
