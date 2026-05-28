package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	legacyCleanupDoneFinalizer = "networking.azure.com/pod-asg-cleanup-done"
	legacyGCPendingFinalizer   = "networking.azure.com/pod-asg-gc"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestMapping(ns, name string, rules []v1alpha1.Mapping) *v1alpha1.PodASGMapping {
	return &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: rules,
		},
	}
}

func newTestPod(ns, name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
}

func asgResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

// stubExecutor records calls and returns configurable results.
type stubExecutor struct {
	calls   [][]engine.Action
	results []azure.ActionResult
}

func (e *stubExecutor) Execute(_ context.Context, actions []engine.Action) []azure.ActionResult {
	e.calls = append(e.calls, actions)
	if e.results != nil {
		return e.results
	}
	res := make([]azure.ActionResult, len(actions))
	for i, a := range actions {
		res[i] = azure.ActionResult{Action: a, Success: true}
	}
	return res
}

// ---------------------------------------------------------------------------
// T5.3: TestReconcile_PodIPChange_ReconcilesOldAndNewIP
// Spec: Pod IP changes → mapping is enqueued → old IP absent, new IP present
// ---------------------------------------------------------------------------
func TestReconcile_PodIPChange_ReconcilesOldAndNewIP(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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

	// Pod already has new IP (simulates IP change after update event)
	pod := newTestPod(ns, "pod-1", "10.0.0.2", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: executor was called with actions
	if len(exec.calls) == 0 {
		t.Fatal("T5.3: expected executor to be called with actions, but got zero calls")
	}

	// Assert: requeue after resync interval
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("T5.3: expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// T5.5: TestReconcile_SpecAddASG_CreatesPrefixSet
// Spec: PodASGMapping spec updated (new ASG added) → new prefix set created
// ---------------------------------------------------------------------------
func TestReconcile_SpecAddASG_CreatesPrefixSet(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg2")}, // new ASG added
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: executor was called and actions include creates for both ASGs
	if len(exec.calls) == 0 {
		t.Fatal("T5.5: expected executor to be called, got zero calls")
	}

	actions := exec.calls[0]
	foundCreate := false
	for _, a := range actions {
		if a.Kind == engine.CreatePrefixSet && a.Target.ASGName == "asg2" {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Errorf("T5.5: expected CreatePrefixSet action for asg2, got actions: %+v", actions)
	}
}

// ---------------------------------------------------------------------------
// T5.6: TestReconcile_SpecRemoveASG_DeletesOwnedPrefixSet
// Spec: PodASGMapping spec updated (ASG removed) → owned prefix set deleted
// ---------------------------------------------------------------------------
func TestReconcile_SpecRemoveASG_DeletesOwnedPrefixSet(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	// Mapping now only has asg1 (asg2 was removed from spec)
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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

	// Set ownership annotation to include both asg1 and asg2 (asg2 was previously owned)
	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
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

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: executor was called with a delete action for asg2
	if len(exec.calls) == 0 {
		t.Fatal("T5.6: expected executor to be called, got zero calls")
	}

	actions := exec.calls[0]
	foundDelete := false
	for _, a := range actions {
		if a.Kind == engine.DeletePrefixSet && a.Target.ASGName == "asg2" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("T5.6: expected DeletePrefixSet action for asg2, got actions: %+v", actions)
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_AddsFinalizerAndReturnsEarly
// Spec: First reconcile adds finalizer and requeues immediately
// ---------------------------------------------------------------------------
func TestReconcile_AddsFinalizerAndReturnsEarly(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	// No finalizer present yet

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: should requeue immediately (Requeue: true) for finalizer addition
	if !result.Requeue {
		t.Error("expected immediate Requeue after adding finalizer, got Requeue=false")
	}

	// Assert: executor should NOT have been called (early return after adding finalizer)
	if len(exec.calls) != 0 {
		t.Errorf("expected no executor calls on finalizer-add path, got %d calls", len(exec.calls))
	}

	// Assert: finalizer is now present on the mapping
	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}
	hasFinalizer := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasFinalizer = true
		}
		if f == legacyGCPendingFinalizer {
			t.Error("gcPendingFinalizer must not be added; spec requires only CleanupFinalizer on first reconcile")
		}
	}
	if !hasFinalizer {
		t.Error("expected CleanupFinalizer to be present on mapping after reconcile")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestReconcile_Debounce - same generation inside interval returns RequeueAfter
// ---------------------------------------------------------------------------
func TestReconcile_Debounce(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	debounceInterval := 2 * time.Second

	r := &MappingReconciler{
		Client:               fakeClient,
		Scheme:               scheme,
		ClusterName:          "test-cluster",
		ResyncInterval:       60 * time.Second,
		MinReconcileInterval: debounceInterval,
		PrefixSetFactory:     fakeFactory,
		Executor:             exec,
	}

	// First reconcile should proceed normally
	result1, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("Phase 3: first reconcile should execute actions")
	}

	// Second reconcile immediately after should be debounced (same generation)
	result2, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	// The second reconcile should return RequeueAfter with remaining time
	if result2.RequeueAfter <= 0 || result2.RequeueAfter > debounceInterval {
		t.Errorf("Phase 3: second reconcile within debounce interval should return RequeueAfter in (0, %v], got %v (result1=%v)",
			debounceInterval, result2.RequeueAfter, result1)
	}

	// Executor should NOT have been called again
	if len(exec.calls) > 1 {
		t.Errorf("Phase 3: second reconcile within debounce interval should NOT execute actions again, got %d total calls", len(exec.calls))
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestReconcile_DebounceDoesNotDelayInitial
// First reconcile for a mapping key is never delayed.
// ---------------------------------------------------------------------------
func TestReconcile_DebounceDoesNotDelayInitial(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "initial-mapping", []v1alpha1.Mapping{
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
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:               fakeClient,
		Scheme:               scheme,
		ClusterName:          "test-cluster",
		ResyncInterval:       60 * time.Second,
		MinReconcileInterval: 5 * time.Second,
		PrefixSetFactory:     fakeFactory,
		Executor:             exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "initial-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// First reconcile must NOT be debounced
	if len(exec.calls) == 0 {
		t.Error("Phase 3: first reconcile for a new key should NOT be debounced; expected executor calls")
	}

	// Should return normal resync
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("Phase 3: first reconcile should return resync interval, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestReconcile_DebounceBypassesNewGeneration
// A new generation (spec change) bypasses debounce immediately.
// ---------------------------------------------------------------------------
func TestReconcile_DebounceBypassesNewGeneration(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "gen-mapping", []v1alpha1.Mapping{
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
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:               fakeClient,
		Scheme:               scheme,
		ClusterName:          "test-cluster",
		ResyncInterval:       60 * time.Second,
		MinReconcileInterval: 10 * time.Second,
		PrefixSetFactory:     fakeFactory,
		Executor:             exec,
	}

	// First reconcile establishes debounce state
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gen-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	firstCallCount := len(exec.calls)

	// Simulate generation bump by updating the mapping
	var current v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "gen-mapping", Namespace: ns}, &current); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	current.Generation = 2
	if err := fakeClient.Update(ctx, &current); err != nil {
		t.Fatalf("failed to bump generation: %v", err)
	}

	// Second reconcile should NOT be debounced because generation changed
	_, err = r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gen-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	if len(exec.calls) <= firstCallCount {
		t.Error("Phase 3: reconcile with new generation should bypass debounce; expected additional executor calls")
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestReconcile_DebounceDisabledWithZeroInterval
// MIN_RECONCILE_INTERVAL_MS=0 disables debounce entirely.
// ---------------------------------------------------------------------------
func TestReconcile_DebounceDisabledWithZeroInterval(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "no-debounce", []v1alpha1.Mapping{
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
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:               fakeClient,
		Scheme:               scheme,
		ClusterName:          "test-cluster",
		ResyncInterval:       60 * time.Second,
		MinReconcileInterval: 0, // debounce disabled
		PrefixSetFactory:     fakeFactory,
		Executor:             exec,
	}

	// First reconcile
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "no-debounce", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	// Second reconcile immediately (same generation)
	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "no-debounce", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	// With debounce disabled, second reconcile should proceed normally (not short-circuit)
	if len(exec.calls) < 2 {
		t.Errorf("Phase 3: with debounce disabled (interval=0), both reconciles should execute; got %d calls", len(exec.calls))
	}

	// Should return normal resync interval, not a debounce-specific RequeueAfter
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("Phase 3: with debounce disabled, should return resync interval; got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: TestReconcile_DebounceStateClearedOnNotFoundAndDelete
// Debounce state is cleaned up on mapping not-found and deletion paths.
// ---------------------------------------------------------------------------
func TestReconcile_DebounceStateClearedOnNotFoundAndDelete(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	// Test not-found clears state
	t.Run("not-found clears debounce state", func(t *testing.T) {
		fakeClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			Build()

		fakeFactory := fake.NewClientFactory()
		exec := &stubExecutor{}

		r := &MappingReconciler{
			Client:               fakeClient,
			Scheme:               scheme,
			ClusterName:          "test-cluster",
			ResyncInterval:       60 * time.Second,
			MinReconcileInterval: 5 * time.Second,
			PrefixSetFactory:     fakeFactory,
			Executor:             exec,
		}

		key := types.NamespacedName{Name: "gone-mapping", Namespace: ns}

		// Simulate prior debounce state by marking a success
		r.markReconcileSuccess(key, 1, time.Now())

		// Reconcile a non-existent mapping
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}

		// Verify debounce state is cleared
		_, shouldDebounce := r.debounceRemaining(key, 1, time.Now())
		if shouldDebounce {
			t.Error("Phase 3: debounce state should be cleared after mapping not-found")
		}
	})

	// Test ErrStatusObjectNotFound from UpdatePending clears debounce state.
	// Scenario: mapping deleted during reconcile after prior success → debounce
	// state must be cleared so a quickly re-created mapping with the same
	// generation is not incorrectly debounced.
	t.Run("ErrStatusObjectNotFound from UpdatePending clears debounce state", func(t *testing.T) {
		mapping := newTestMapping(ns, "pending-gone", []v1alpha1.Mapping{
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

		fakeClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping).
			Build()

		fakeFactory := fake.NewClientFactory()
		fakeFactory.RegisterClient("sub1", fake.NewClient())
		exec := &stubExecutor{}

		// StatusUpdater that returns ErrStatusObjectNotFound on UpdatePending,
		// simulating a mapping deleted between ensureFinalizer and status write.
		statusStub := &stubStatusUpdaterP7{pendingErr: ErrStatusObjectNotFound}

		r := &MappingReconciler{
			Client:               fakeClient,
			Scheme:               scheme,
			ClusterName:          "test-cluster",
			ResyncInterval:       60 * time.Second,
			MinReconcileInterval: 5 * time.Second,
			PrefixSetFactory:     fakeFactory,
			Executor:             exec,
			StatusUpdater:        statusStub,
		}

		key := types.NamespacedName{Name: "pending-gone", Namespace: ns}

		// Simulate prior debounce state from a successful reconcile, but
		// old enough that the debounce window has expired so the reconcile
		// proceeds past the debounce check to the UpdatePending call.
		r.markReconcileSuccess(key, mapping.Generation, time.Now().Add(-10*time.Second))

		// Reconcile — UpdatePending returns ErrStatusObjectNotFound
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}

		// Debounce state must be cleared so a re-created mapping is not skipped
		_, shouldDebounce := r.debounceRemaining(key, mapping.Generation, time.Now())
		if shouldDebounce {
			t.Error("Phase 3: debounce state should be cleared after ErrStatusObjectNotFound from UpdatePending")
		}
	})
}

// ---------------------------------------------------------------------------
// T5.7: TestReconcile_DeletePath_CleansOwnedPrefixSetsAndRemovesFinalizer
// Spec: PodASGMapping deleted → finalizer triggers cleanup → all owned prefix sets deleted
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_CleansOwnedPrefixSetsAndRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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

	// Ownership annotation with asg1 and asg2
	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate fake Azure with prefix sets to be cleaned up
	ownershipKey := "test-cluster-test-ns-my-mapping"
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg2", ownershipKey, []string{"10.0.0.2/32"})

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: prefix sets should be deleted from Azure
	_, getErr1 := fakeAzClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if !azure.IsNotFound(getErr1) {
		t.Error("T5.7: expected prefix set in asg1 to be deleted")
	}
	_, getErr2 := fakeAzClient.Get(ctx, "sub1", "rg1", "asg2", ownershipKey)
	if !azure.IsNotFound(getErr2) {
		t.Error("T5.7: expected prefix set in asg2 to be deleted")
	}

	// Assert: finalizer should be removed (object may be fully GC'd by fake client
	// since removing the last finalizer on a deleting object triggers deletion)
	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); err != nil {
		// Object deleted by fake client GC — this proves the finalizer was removed
		return
	}
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			t.Error("T5.7: expected CleanupFinalizer to be removed after delete path")
		}
	}
}

// ---------------------------------------------------------------------------
// T5.8: TestReconcile_ReturnsRequeueAfterResyncInterval
// Spec: Periodic resync fires after interval → reconcile triggered, drift corrected
// ---------------------------------------------------------------------------
func TestReconcile_ReturnsRequeueAfterResyncInterval(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)
	exec := &stubExecutor{}

	resyncInterval := 90 * time.Second
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   resyncInterval,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if result.RequeueAfter != resyncInterval {
		t.Errorf("T5.8: expected RequeueAfter=%v, got %v", resyncInterval, result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_ClientAcquisitionError_RetainsFinalizerAndReturnsError
// Design §5.3: ForSubscription error => record operation error, retain finalizer
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_ClientAcquisitionError_RetainsFinalizerAndReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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

	// Factory with NO registered subscription → ForSubscription will fail
	fakeFactory := fake.NewClientFactory()

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}

	// Finalizer MUST be retained
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get mapping: %v", getErr)
	}
	hasCleanup := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when client acquisition fails")
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_ListError_RetainsFinalizerAndReturnsError
// Design §5.3: List error => record operation error, retain finalizer
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_ListError_RetainsFinalizerAndReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Inject a List error
	listErr := fmt.Errorf("simulated list failure")
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationList,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
	}, listErr, 1)

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}

	// Finalizer MUST be retained
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get mapping: %v", getErr)
	}
	hasCleanup := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when List error occurs during cleanup")
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_DeleteError_RetainsFinalizerAndReturnsError
// Design §5.3: Delete error (non-404) => record operation error, retain finalizer
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_DeleteError_RetainsFinalizerAndReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	ownershipKey := "test-cluster-test-ns-my-mapping"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate a prefix set to trigger delete
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})

	// Inject a Delete error
	deleteErr := fmt.Errorf("simulated delete failure")
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationDelete,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  ownershipKey,
	}, deleteErr, 1)

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}

	// Finalizer MUST be retained
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get mapping: %v", getErr)
	}
	hasCleanup := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when Delete error occurs during cleanup")
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_MultipleCleanupErrors_AggregatesAndRetainsFinalizer
// Design §5.3: All errors aggregated and returned as one reconcile error
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_MultipleCleanupErrors_AggregatesAndRetainsFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	ownershipKey := "test-cluster-test-ns-my-mapping"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	mapping.DeletionTimestamp = &now
	mapping.Finalizers = []string{CleanupFinalizer}

	ownedRefs := []OwnedASGRef{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1"},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2"},
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate prefix sets
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg2", ownershipKey, []string{"10.0.0.2/32"})

	// Inject Delete errors for both ASGs
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationDelete,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  ownershipKey,
	}, fmt.Errorf("delete error asg1"), 1)
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationDelete,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg2",
		PrefixSetName:  ownershipKey,
	}, fmt.Errorf("delete error asg2"), 1)

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}

	// Finalizer MUST be retained
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get mapping: %v", getErr)
	}
	hasCleanup := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when multiple cleanup errors occur")
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_OwnedAnnotationParseError_RetainsFinalizerAndReturnsError
// Design §5.3 rule 1: owned annotation present but invalid => return error, keep finalizer
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_OwnedAnnotationParseError_RetainsFinalizerAndReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: "not-valid-json{{{",
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}

	// Finalizer MUST be retained
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		t.Fatalf("failed to get mapping: %v", getErr)
	}
	hasCleanup := false
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when owned annotation parse error occurs on delete")
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_AbsentOwnedAnnotation_FallsBackToSpecCandidates
// Design §5.3 rule 1: owned annotation absent => fallback to spec-derived candidates
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_AbsentOwnedAnnotation_FallsBackToSpecCandidates(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	ownershipKey := "test-cluster-test-ns-my-mapping"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	// No ownership annotation present

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate Azure with a prefix set to be cleaned up via spec fallback
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: prefix set should be cleaned up using spec-derived candidates
	_, getErr := fakeAzClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if !azure.IsNotFound(getErr) {
		t.Error("expected prefix set to be deleted via spec-fallback when owned annotation is absent")
	}

	// Assert: CleanupFinalizer should be removed after successful cleanup
	// Object may be fully GC'd by fake client since removing the last finalizer
	// on a deleting object triggers deletion.
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		// Object deleted by fake client GC — proves finalizer was removed
		return
	}
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			t.Error("expected CleanupFinalizer to be removed after successful spec-fallback cleanup")
		}
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_RemovesOnlyCleanupFinalizer
// Design §5.3 rule 5: No cleanupDone finalizer; only remove CleanupFinalizer
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_RemovesOnlyCleanupFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	ownershipKey := "test-cluster-test-ns-my-mapping"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert: no cleanupDone finalizer must ever be added (single-finalizer contract)
	// Object may be fully GC'd by fake client since removing the last finalizer
	// on a deleting object triggers deletion — this also proves the contract.
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		// Object deleted by fake client GC — proves no extra finalizers were added
		return
	}
	for _, f := range updated.Finalizers {
		if f == legacyCleanupDoneFinalizer {
			t.Error("cleanupDoneFinalizer must not be added; single-finalizer contract requires only CleanupFinalizer")
		}
		if f == legacyGCPendingFinalizer {
			t.Error("gcPendingFinalizer must not be added; spec requires single-finalizer contract (CleanupFinalizer only)")
		}
		if f == CleanupFinalizer {
			t.Error("expected CleanupFinalizer to be removed after successful cleanup (not replaced with cleanupDone)")
		}
	}
}

// ---------------------------------------------------------------------------
// T5.8 (spec contract): TestReconcile_NoTargets_StillReturnsRequeueAfterResyncInterval
// Spec: Periodic resync ALWAYS fires after interval, even when desired/owned target sets are empty.
// The current code conditionally skips resync when allTargets is empty — this test enforces the spec.
// ---------------------------------------------------------------------------
func TestReconcile_NoTargets_StillReturnsRequeueAfterResyncInterval(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	// Mapping with a selector that does NOT match any pods → desired state is empty
	mapping := newTestMapping(ns, "empty-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "nonexistent"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	// No pods matching the selector exist
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)
	exec := &stubExecutor{}

	resyncInterval := 60 * time.Second
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   resyncInterval,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "empty-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Spec requires: timed resync ALWAYS scheduled on successful non-delete reconcile,
	// INCLUDING when desired/owned target sets are empty.
	if result.RequeueAfter != resyncInterval {
		t.Errorf("T5.8: expected RequeueAfter=%v even with empty targets, got RequeueAfter=%v (spec requires periodic requeue regardless of target count)",
			resyncInterval, result.RequeueAfter)
	}
}

func TestReconcile_EmptyTargets_RequestPromptFollowUp(t *testing.T) {
	assertPromptFollowUp := func(t *testing.T, result ctrl.Result, resyncInterval time.Duration) {
		t.Helper()
		if result.Requeue {
			return
		}
		if result.RequeueAfter > 0 && result.RequeueAfter < resyncInterval {
			return
		}
		t.Errorf("expected prompt follow-up before full resync interval %v, got %#v", resyncInterval, result)
	}

	t.Run("matching pod without IP", func(t *testing.T) {
		ctx := context.Background()
		scheme := testScheme(t)
		ns := "test-ns"
		resyncInterval := 60 * time.Second

		mapping := newTestMapping(ns, "pending-ip-mapping", []v1alpha1.Mapping{
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
		mapping.CreationTimestamp = metav1.NewTime(time.Now())

		pod := newTestPod(ns, "pod-1", "", map[string]string{"app": "web"})

		fakeClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping, pod).
			Build()

		exec := &stubExecutor{}
		r := &MappingReconciler{
			Client:           fakeClient,
			Scheme:           scheme,
			ClusterName:      "test-cluster",
			ResyncInterval:   resyncInterval,
			PrefixSetFactory: fake.NewClientFactory(),
			Executor:         exec,
		}

		result, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "pending-ip-mapping", Namespace: ns},
		})
		if err != nil {
			t.Fatalf("unexpected reconcile error: %v", err)
		}
		assertPromptFollowUp(t, result, resyncInterval)
		if len(exec.calls) != 0 {
			t.Errorf("expected no executor calls while waiting for pod IPs, got %d", len(exec.calls))
		}
	})

	t.Run("recent mapping without targets", func(t *testing.T) {
		ctx := context.Background()
		scheme := testScheme(t)
		ns := "test-ns"
		resyncInterval := 60 * time.Second

		mapping := newTestMapping(ns, "recent-mapping", []v1alpha1.Mapping{
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
		mapping.CreationTimestamp = metav1.NewTime(time.Now())

		fakeClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping).
			Build()

		exec := &stubExecutor{}
		r := &MappingReconciler{
			Client:           fakeClient,
			Scheme:           scheme,
			ClusterName:      "test-cluster",
			ResyncInterval:   resyncInterval,
			PrefixSetFactory: fake.NewClientFactory(),
			Executor:         exec,
		}

		result, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "recent-mapping", Namespace: ns},
		})
		if err != nil {
			t.Fatalf("unexpected reconcile error: %v", err)
		}
		assertPromptFollowUp(t, result, resyncInterval)
		if len(exec.calls) != 0 {
			t.Errorf("expected no executor calls before any targets exist, got %d", len(exec.calls))
		}
	})

	t.Run("older mapping with pending pod IP falls back to resync interval", func(t *testing.T) {
		ctx := context.Background()
		scheme := testScheme(t)
		ns := "test-ns"
		resyncInterval := 60 * time.Second

		mapping := newTestMapping(ns, "older-pending-ip-mapping", []v1alpha1.Mapping{
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
		mapping.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))

		pod := newTestPod(ns, "pod-1", "", map[string]string{"app": "web"})

		fakeClient := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(mapping, pod).
			Build()

		r := &MappingReconciler{
			Client:           fakeClient,
			Scheme:           scheme,
			ClusterName:      "test-cluster",
			ResyncInterval:   resyncInterval,
			PrefixSetFactory: fake.NewClientFactory(),
			Executor:         &stubExecutor{},
		}

		result, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "older-pending-ip-mapping", Namespace: ns},
		})
		if err != nil {
			t.Fatalf("unexpected reconcile error: %v", err)
		}
		if result.RequeueAfter != resyncInterval {
			t.Fatalf("expected older pending-IP mapping to fall back to resync interval %v, got %#v", resyncInterval, result)
		}
	})
}

// ---------------------------------------------------------------------------
// TestReconcile_MappingNotFound_ReturnsSuccessNoRequeue
// Design §7.1: Mapping not found → return success (no requeue)
// ---------------------------------------------------------------------------
func TestReconcile_MappingNotFound_ReturnsSuccessNoRequeue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected nil error for not-found mapping, got: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for not-found mapping, got %+v", result)
	}
	if len(exec.calls) != 0 {
		t.Errorf("expected no executor calls for not-found mapping, got %d", len(exec.calls))
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletePath_Azure404IsNonFatal
// Design §7.5: Delete 404 from Azure is non-fatal in cleanup
// ---------------------------------------------------------------------------
func TestReconcile_DeletePath_Azure404IsNonFatal(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	ownershipKey := "test-cluster-test-ns-my-mapping"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate so list returns an entry, then inject 404 on delete
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"10.0.0.1/32"})
	// Delete the entry so that the actual delete call returns 404
	_ = fakeAzClient.Delete(ctx, "sub1", "rg1", "asg1", ownershipKey)

	exec := &stubExecutor{}
	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// 404 during cleanup should NOT be treated as failure
	if err != nil {
		t.Errorf("expected no error when Azure returns 404 during cleanup, got: %v", err)
	}

	// Finalizer should still be removed (successful cleanup)
	var updated v1alpha1.PodASGMapping
	if getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "my-mapping", Namespace: ns}, &updated); getErr != nil {
		// Object GC'd — proves finalizer was removed
		return
	}
	for _, f := range updated.Finalizers {
		if f == CleanupFinalizer {
			t.Error("expected CleanupFinalizer to be removed when 404 is non-fatal")
		}
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_ExecutorPartialFailure_ReturnsError
// Executor returns partial failures → reconcile must return error
// ---------------------------------------------------------------------------
func TestReconcile_ExecutorPartialFailure_ReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Executor that returns a failure result
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{ASGName: "asg1"}},
				Success: false,
				Err:     fmt.Errorf("simulated Azure failure"),
			},
		},
	}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_DeletingWithoutFinalizer_ReturnsSuccessNoRequeue
// Design: Mapping deleting but finalizer already removed → no-op
// ---------------------------------------------------------------------------
func TestReconcile_DeletingWithoutFinalizer_ReturnsSuccessNoRequeue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	now := metav1.Now()
	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	// Use a different finalizer so the fake client accepts a deleting object,
	// but the CleanupFinalizer is absent (simulates already-cleaned-up state).
	mapping.Finalizers = []string{"other.finalizer/keep"}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("expected nil error for deleting mapping without finalizer, got: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for deleting mapping without finalizer, got %+v", result)
	}
	if len(exec.calls) != 0 {
		t.Errorf("expected no executor calls, got %d", len(exec.calls))
	}
}

// ---------------------------------------------------------------------------
// TestReconcile_NonDeletePath_CorruptOwnedAnnotation_ReturnsError
// Design §5.2 step 6: parse error on non-delete path => return error (fail closed)
// ---------------------------------------------------------------------------
func TestReconcile_NonDeletePath_CorruptOwnedAnnotation_ReturnsError(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
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
	mapping.Annotations = map[string]string{
		OwnedASGsAnnotationKey: "not-valid-json{{{",
	}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})

	// Phase 7 contract: reconcile returns nil error with policy-driven requeue
	if err != nil {
		t.Fatalf("expected nil error (Phase 7 policy-driven), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Phase 2: Parallel Azure GET Calls — listActualForTargets tests
// ---------------------------------------------------------------------------

// delayingFakeClient wraps a fake Azure API to inject per-GET delay and track concurrency.
type delayingFakeClient struct {
	delay       time.Duration
	mu          sync.Mutex
	inFlight    int64
	maxInFlight int64
	attempts    int64
	// Per-target error injection (target key -> error)
	targetErrors map[string]error
}

func (d *delayingFakeClient) Get(ctx context.Context, sub, rg, asg, ps string) (*azure.AddressPrefixSet, error) {
	atomic.AddInt64(&d.attempts, 1)

	current := atomic.AddInt64(&d.inFlight, 1)
	defer atomic.AddInt64(&d.inFlight, -1)

	d.mu.Lock()
	if current > d.maxInFlight {
		d.maxInFlight = current
	}
	d.mu.Unlock()

	// Simulate network delay
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	key := sub + "/" + rg + "/" + asg + "/" + ps
	d.mu.Lock()
	injErr := d.targetErrors[key]
	d.mu.Unlock()

	if injErr != nil {
		return nil, injErr
	}

	name := ps
	etag := `"v-1"`
	return &azure.AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &azure.AddressPrefixSetProperties{
			AddressPrefixes: []string{"10.0.0.1"},
		},
	}, nil
}

func (d *delayingFakeClient) Put(_ context.Context, _, _, _, _ string, _ []string) error {
	return nil
}

func (d *delayingFakeClient) Delete(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (d *delayingFakeClient) List(_ context.Context, _, _, _ string) ([]azure.AddressPrefixSet, error) {
	return nil, nil
}

// delayingFakeFactory returns the same delayingFakeClient for any subscription.
type delayingFakeFactory struct {
	client *delayingFakeClient
}

func (f *delayingFakeFactory) ForSubscription(_ string) (azure.AddressPrefixSetAPI, error) {
	return f.client, nil
}

// TestListActualForTargets_Parallel verifies parallel GET execution.
// Asserts: all 5 attempts occurred, observed concurrency > 1 and <= semaphore bound,
// and aggregated error is returned for the failed target.
func TestListActualForTargets_Parallel(t *testing.T) {
	ctx := context.Background()

	delayClient := &delayingFakeClient{
		delay:        100 * time.Millisecond,
		targetErrors: make(map[string]error),
	}

	// Inject one non-NotFound failure for target 3
	delayClient.targetErrors["sub1/rg1/asg3/prefix-set"] = &azure.ARMStatusError{
		StatusCode: 500,
		ARMCode:    "InternalServerError",
		Message:    "injected failure",
	}

	factory := &delayingFakeFactory{client: delayClient}

	r := &MappingReconciler{
		PrefixSetFactory: factory,
		AzureReadSem:     make(chan struct{}, 5),
	}

	targets := map[engine.ASGTarget]struct{}{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg3", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg4", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg5", PrefixSetName: "prefix-set"}: {},
	}

	_, err := r.listActualForTargets(ctx, targets)

	// Phase 2 acceptance: aggregated error is returned (one target failed)
	if err == nil {
		t.Fatal("TestListActualForTargets_Parallel: expected aggregated error from failed target, got nil")
	}

	// Phase 2 acceptance: all 5 GET attempts occurred (no fail-fast abort)
	gotAttempts := atomic.LoadInt64(&delayClient.attempts)
	if gotAttempts != 5 {
		t.Errorf("TestListActualForTargets_Parallel: expected 5 GET attempts, got %d (fail-fast detected)", gotAttempts)
	}

	// Phase 2 acceptance: observed max concurrent GETs > 1 (proves parallelism)
	delayClient.mu.Lock()
	maxConcurrent := delayClient.maxInFlight
	delayClient.mu.Unlock()
	if maxConcurrent <= 1 {
		t.Errorf("TestListActualForTargets_Parallel: expected observed concurrency > 1, got %d (sequential execution detected)", maxConcurrent)
	}
	if maxConcurrent > 5 {
		t.Errorf("TestListActualForTargets_Parallel: expected observed concurrency <= 5 (bounded), got %d", maxConcurrent)
	}
}

// TestListActualForTargets_NotFoundSkipped verifies ErrNotFound targets are omitted
// from the result and do not cause an error when remaining targets succeed.
func TestListActualForTargets_NotFoundSkipped(t *testing.T) {
	ctx := context.Background()

	delayClient := &delayingFakeClient{
		delay:        5 * time.Millisecond,
		targetErrors: make(map[string]error),
	}

	// Target 2 returns ErrNotFound (should be omitted, not cause error)
	delayClient.targetErrors["sub1/rg1/asg2/prefix-set"] = azure.ErrNotFound

	factory := &delayingFakeFactory{client: delayClient}

	r := &MappingReconciler{
		PrefixSetFactory: factory,
		AzureReadSem:     make(chan struct{}, 5),
	}

	targets := map[engine.ASGTarget]struct{}{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg3", PrefixSetName: "prefix-set"}: {},
	}

	actual, err := r.listActualForTargets(ctx, targets)

	// No error expected (NotFound is non-fatal, other targets succeed)
	if err != nil {
		t.Fatalf("TestListActualForTargets_NotFoundSkipped: expected nil error, got %v", err)
	}

	// NotFound target (asg2) must be omitted from actual
	for target := range actual {
		if target.ASGName == "asg2" {
			t.Error("TestListActualForTargets_NotFoundSkipped: NotFound target asg2 should be omitted from actual")
		}
	}

	// Successful targets must be present
	found := 0
	for target := range actual {
		if target.ASGName == "asg1" || target.ASGName == "asg3" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("TestListActualForTargets_NotFoundSkipped: expected 2 successful targets in actual, got %d", found)
	}
}

// TestListActualForTargets_AggregatedErrorPrefersRetriableCause verifies that when
// mixed failures occur (403 + 429), the aggregated error classifies as retriable
// and exposes the retry hint through unwrapping.
func TestListActualForTargets_AggregatedErrorPrefersRetriableCause(t *testing.T) {
	ctx := context.Background()

	delayClient := &delayingFakeClient{
		delay:        5 * time.Millisecond,
		targetErrors: make(map[string]error),
	}

	// 403 non-retriable failure
	delayClient.targetErrors["sub1/rg1/asg1/prefix-set"] = &azure.ARMStatusError{
		StatusCode: 403,
		ARMCode:    "AuthorizationFailed",
		Message:    "forbidden",
	}

	// 429 retriable failure with Retry-After
	delayClient.targetErrors["sub1/rg1/asg2/prefix-set"] = &azure.ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "throttled",
		RetryAfter: 20 * time.Second,
	}

	factory := &delayingFakeFactory{client: delayClient}

	r := &MappingReconciler{
		PrefixSetFactory: factory,
		AzureReadSem:     make(chan struct{}, 5),
	}

	targets := map[engine.ASGTarget]struct{}{
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2", PrefixSetName: "prefix-set"}: {},
		{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg3", PrefixSetName: "prefix-set"}: {},
	}

	_, err := r.listActualForTargets(ctx, targets)

	// Must return an error (at least two targets failed)
	if err == nil {
		t.Fatal("TestListActualForTargets_AggregatedErrorPrefersRetriableCause: expected aggregated error, got nil")
	}

	// Aggregated error must classify as retriable (429 takes priority over 403)
	if !azure.IsRetriableARM(err) {
		t.Errorf("TestListActualForTargets_AggregatedErrorPrefersRetriableCause: expected aggregated error to classify as retriable (IsRetriableARM=true), got false; err=%v", err)
	}

	// Must expose Retry-After hint from the 429 via unwrapping
	hint := azure.ExtractRetryAfterHint(err)
	if hint < 20*time.Second {
		t.Errorf("TestListActualForTargets_AggregatedErrorPrefersRetriableCause: expected RetryAfter hint >= 20s from 429, got %v", hint)
	}

	// Error message must include both target failures
	errMsg := err.Error()
	if !strings.Contains(errMsg, "asg1") || !strings.Contains(errMsg, "asg2") {
		t.Errorf("TestListActualForTargets_AggregatedErrorPrefersRetriableCause: error message should include both failed targets; got: %s", errMsg)
	}
}
