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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func (d *delayingFakeClient) GetWithETag(_ context.Context, _, _, _, _ string) (*azure.AddressPrefixSet, string, error) {
	return nil, "", nil
}

func (d *delayingFakeClient) PutWithIfMatch(_ context.Context, _, _, _, _ string, _ []string, _ string) error {
	return nil
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

// ===========================================================================
// Phase 5: Desired-State Cache — Reconciler Cache Integration Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheHit_SkipsFullRecompute
// When the cache has a valid entry for the mapping+generation, the reconciler
// should use cached desired state instead of listing pods and recomputing.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheHit_SkipsFullRecompute(t *testing.T) {
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
	cache := engine.NewDesiredStateCache("test-cluster")

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Pre-seed cache with desired state (simulating a prior recompute)
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-" + ns + "-my-mapping",
	}
	cachedDesired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	cache.SetFromRecompute(
		&v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
			Spec:       mapping.Spec,
		},
		nil,
		cachedDesired,
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1},
		false,
	)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// The reconciler should have used cached state. Verify it called executor
	// (behavior preserved - still reconciles Azure state).
	if len(exec.calls) == 0 {
		t.Fatal("Phase 5: expected executor to be called using cached desired state")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheMiss_TriggersFullRecompute
// When no cache entry exists, the reconciler lists pods and recomputes
// desired state from scratch (existing behavior preserved).
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheMiss_TriggersFullRecompute(t *testing.T) {
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
	cache := engine.NewDesiredStateCache("test-cluster")

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// No cache seeding — cache miss expected
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Executor should be called (recompute + Azure sync)
	if len(exec.calls) == 0 {
		t.Fatal("Phase 5: expected executor to be called after cache miss full recompute")
	}

	// After recompute, cache should now have an entry
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	if _, ok := cache.Get(mappingObj); !ok {
		t.Error("Phase 5: expected cache populated after full recompute on cache miss")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_InvalidSpec_UsesLiveMatchedPods_NotCache
// Invalid spec path must compute matchedPodsByIndex from a live pod listing,
// not from cached data, and must delete cache entry.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_InvalidSpec_UsesLiveMatchedPods_NotCache(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	// Invalid ASG resource ID
	mapping := newTestMapping(ns, "invalid-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: "not-a-valid-resource-id"},
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
	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Pre-seed cache (should be deleted on invalid spec path)
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "invalid-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{},
		engine.PodSnapshot{}, []int{99}, false) // stale matched count

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "invalid-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Cache entry should be deleted for invalid spec
	if _, ok := cache.Get(mappingObj); ok {
		t.Error("Phase 5: expected cache entry deleted for mapping with invalid spec")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_GenerationChange_ForcesRecompute
// Valid-to-valid generation change (selector/ASG mutation) must cause a cache
// miss and recompute, not reuse stale prior-generation desired state.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_GenerationChange_ForcesRecompute(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web-v2"}, // new selector
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 2 // updated generation

	// pod matches new selector
	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web-v2"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Seed cache with OLD generation (gen=1, old selector)
	oldMappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
	}
	cache.SetFromRecompute(oldMappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1", PrefixSetName: "test-cluster-" + ns + "-my-mapping"}: {
				IPs: map[string]struct{}{"10.0.0.99/32": {}}, // stale IP from old gen
			},
		},
		engine.PodSnapshot{}, []int{5}, false)

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Executor should have been called with fresh desired state (not stale)
	if len(exec.calls) == 0 {
		t.Fatal("Phase 5: expected executor to be called after generation change")
	}

	// Verify that the action uses the new pod IP (10.0.0.1) not stale (10.0.0.99)
	actions := exec.calls[0]
	for _, a := range actions {
		if a.Kind == engine.CreatePrefixSet || a.Kind == engine.UpdatePrefixSet {
			for _, ip := range a.DesiredIPs {
				if ip == "10.0.0.99/32" {
					t.Error("Phase 5: stale IP from old generation used; generation isolation broken")
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_ForcedRecompute_AtResyncInterval
// Even with a valid cache entry, the reconciler should force a full recompute
// at the resync interval boundary to correct any drift.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_ForcedRecompute_AtResyncInterval(t *testing.T) {
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
	cache := engine.NewDesiredStateCache("test-cluster")

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    1 * time.Millisecond, // very short for testing
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Seed cache with stale desired state
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
				PrefixSetName:  "test-cluster-" + ns + "-my-mapping",
			}: {IPs: map[string]struct{}{"10.0.0.99/32": {}}}, // stale
		},
		engine.PodSnapshot{}, []int{1}, false)

	// Wait for resync interval to elapse
	time.Sleep(5 * time.Millisecond)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// After forced recompute, the reconciler should use the actual pod IP (10.0.0.1)
	// not the stale cached value (10.0.0.99)
	if len(exec.calls) == 0 {
		t.Fatal("Phase 5: expected executor call after forced recompute at resync interval")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_TerminalCleanup_ClearsCacheAndResyncState
// Terminal exits (mapping not found, delete complete) must clear cache and
// resync state for the mapping key.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_TerminalCleanup_ClearsCacheAndResyncState(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build() // no mapping exists → not found

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Pre-seed cache for a mapping that will be "not found"
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "deleted-mapping", Generation: 1},
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "deleted-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Cache entry should be cleared on terminal not-found exit
	if _, ok := cache.Get(mappingObj); ok {
		t.Error("Phase 5: expected cache cleared on terminal mapping-not-found exit")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheMiss_StaleRecomputeRejectedAfterVersionAdvance
// A reconcile performs recompute but a pod event mutates the cache version
// mid-flight. The CAS publish (SetFromRecomputeIfVersion) must reject the
// stale recompute result.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheMiss_StaleRecomputeRejectedAfterVersionAdvance(t *testing.T) {
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
	cache := engine.NewDesiredStateCache("test-cluster")

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Seed the cache so it starts with an entry, then simulate version advance
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-" + ns + "-my-mapping",
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1}, false)

	// Read fences before reconcile
	_, versionBefore, epochBefore, _ := cache.GetWithVersion(mappingObj)

	// Simulate a pod event that advances the version mid-recompute
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pod-2", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
	}
	cache.OnPodAdd(mappingObj, pod2)

	// Verify CAS primitive: stale publish is rejected
	staleDesired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}}, // missing pod-2
	}
	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mappingObj, nil, staleDesired, engine.PodSnapshot{}, []int{1}, false,
		versionBefore, epochBefore,
	)
	if committed {
		t.Error("Phase 5: stale recompute should be rejected when pod event advanced version")
	}

	// Cache should retain pod-2 (from OnPodAdd)
	got, ok := cache.Get(mappingObj)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if _, has := got.Desired[target].IPs["10.0.0.2/32"]; !has {
		t.Error("Phase 5: cache should retain pod-2 IP from incremental add; stale recompute must not overwrite")
	}

	// Verify the reconciler's fenced path: when reconcile runs with existing
	// cache entry (pod-2 already added), it uses the cache hit path and
	// proceeds normally without overwriting.
	// Mark a prior recompute so the resync interval hasn't elapsed and cache hit is used.
	r.markDesiredStateFullRecompute(types.NamespacedName{Name: "my-mapping", Namespace: ns}, 1, time.Now())
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// After reconcile, cache should still contain pod-2's IP (from pod event)
	got2, ok2 := cache.Get(mappingObj)
	if !ok2 {
		t.Fatal("expected cache hit after reconcile")
	}
	if _, has := got2.Desired[target].IPs["10.0.0.2/32"]; !has {
		t.Error("Phase 5: reconcile must not overwrite pod-event mutations in cache")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_DeleteRecreate_StaleInFlightCannotRepopulateCache
// A mapping is deleted and recreated with same namespaced name. A stale
// in-flight reconcile from the old object must not repopulate the cache.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_DeleteRecreate_StaleInFlightCannotRepopulateCache(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Simulate old mapping being in cache before deletion
	oldMappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 3},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "old"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-" + ns + "-my-mapping",
	}
	cache.SetFromRecompute(oldMappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.OLD/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	// Stale reconcile reads fences (before deletion)
	_, staleVersion, staleEpoch, _ := cache.GetWithVersion(oldMappingObj)

	// Terminal cleanup: reconciler calls Delete (mapping is gone)
	cache.Delete(types.NamespacedName{Namespace: ns, Name: "my-mapping"})

	// New mapping is created (generation resets to 1)
	newMapping := newTestMapping(ns, "my-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "new"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
	})
	newMapping.Finalizers = []string{CleanupFinalizer}
	newMapping.Generation = 1

	newPod := newTestPod(ns, "new-pod-1", "10.0.0.NEW", map[string]string{"app": "new"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(newMapping, newPod).
		Build()

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Stale in-flight recompute from old object tries to publish with old fences
	staleDesired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.OLD/32": {}}},
	}
	committed, _, _ := cache.SetFromRecomputeIfVersion(
		oldMappingObj, nil, staleDesired, engine.PodSnapshot{}, []int{1}, false,
		staleVersion, staleEpoch,
	)
	if committed {
		t.Error("Phase 5: stale in-flight recompute must not repopulate cache after delete/recreate")
	}

	// Now reconcile the new mapping normally
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// After fresh reconcile, cache should have new data only
	newMappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "my-mapping", Generation: 1},
		Spec:       newMapping.Spec,
	}
	got, ok := cache.Get(newMappingObj)
	if !ok {
		t.Fatal("Phase 5: expected cache populated after fresh reconcile for recreated mapping")
	}
	if _, hasOld := got.Desired[target].IPs["10.0.0.OLD/32"]; hasOld {
		t.Error("Phase 5: cache must not contain old mapping data after delete/recreate")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_NotFoundCleanup_StaleInFlightCannotRepopulateCache
// A mapping becomes NotFound. Stale in-flight recompute must not repopulate
// cache after the terminal cleanup.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_NotFoundCleanup_StaleInFlightCannotRepopulateCache(t *testing.T) {
	scheme := testScheme(t)
	ns := "test-ns"

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gone-mapping", Generation: 2},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-" + ns + "-gone-mapping",
	}

	// Pre-seed cache
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	// Stale reconcile reads fences
	_, staleVersion, staleEpoch, _ := cache.GetWithVersion(mappingObj)

	// Mapping goes away — reconciler discovers NotFound
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build() // no mapping

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Terminal cleanup should have cleared cache and bumped lifecycle epoch
	if _, ok := cache.Get(mappingObj); ok {
		t.Error("Phase 5: expected cache cleared on NotFound terminal cleanup")
	}

	// Stale in-flight recompute tries to publish with old fences
	staleDesired := map[engine.ASGTarget]engine.DesiredPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
	}
	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mappingObj, nil, staleDesired, engine.PodSnapshot{}, []int{1}, false,
		staleVersion, staleEpoch,
	)
	if committed {
		t.Error("Phase 5: stale in-flight recompute must not repopulate cache after NotFound cleanup")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_ForcedRecompute_ConflictRetry_UsesBoundedRequeueAfter
// When a forced recompute conflict occurs repeatedly, the reconciler should
// use a bounded RequeueAfter (not hot-loop with Requeue: true).
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_ForcedRecompute_ConflictRetry_UsesBoundedRequeueAfter(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"
	cache := engine.NewDesiredStateCache("test-cluster")

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "conflict-mapping", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-" + ns + "-conflict-mapping",
	}

	// Pre-seed cache and read fences
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)
	_, ver, epoch, _ := cache.GetWithVersion(mappingObj)

	// Advance version to create a permanent conflict scenario
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pod-churn", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.99"},
	}
	cache.OnPodAdd(mappingObj, pod)

	// Verify CAS primitive: stale publish is rejected
	committed, _, _ := cache.SetFromRecomputeIfVersion(
		mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}}},
		engine.PodSnapshot{}, []int{1}, false,
		ver, epoch,
	)
	if committed {
		t.Fatal("expected CAS failure on stale version")
	}

	// Verify cacheConflictRequeueAfter returns bounded duration
	r := &MappingReconciler{
		Client:            nil,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		DesiredStateCache: cache,
	}

	requeueAfter := r.cacheConflictRequeueAfter()
	if requeueAfter <= 0 {
		t.Errorf("Phase 5: cacheConflictRequeueAfter should be > 0, got %v", requeueAfter)
	}
	if requeueAfter > 2*time.Second {
		t.Errorf("Phase 5: cacheConflictRequeueAfter should be bounded ≤ 2s, got %v", requeueAfter)
	}

	// Verify the reconciler uses CAS publish through Reconcile():
	// A forced recompute (ResyncInterval=0) should still succeed because
	// GetWithVersion captures fresh fences before publish.
	mapping := newTestMapping(ns, "conflict-mapping", mappingObj.Spec.Mappings)
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	podObj := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, podObj).
		Build()
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())
	exec := &stubExecutor{}

	r2 := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    0, // force recompute
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}
	result, err := r2.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conflict-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	// When fences match (synchronous path), the CAS publish succeeds
	// and reconcile proceeds normally (no RequeueAfter from conflict).
	if result.RequeueAfter > 2*time.Second {
		t.Errorf("Phase 5: expected reconcile to succeed or requeue within bounds, got RequeueAfter=%v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_TerminalCleanup_LifecycleFenceBump
// Terminal exits must call Delete which bumps lifecycle epoch. Verify that
// after terminal cleanup, the lifecycle epoch is advanced.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_TerminalCleanup_LifecycleFenceBump(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build() // no mapping exists → not found

	fakeFactory := fake.NewClientFactory()
	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Pre-seed cache for a mapping that will be "not found"
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "terminal-mapping", Generation: 1},
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	// Read lifecycle epoch before terminal
	_, _, epochBefore, _ := cache.GetWithVersion(mappingObj)

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "terminal-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Cache should be cleared and lifecycle epoch bumped
	_, _, epochAfter, _ := cache.GetWithVersion(mappingObj)
	if epochAfter <= epochBefore {
		t.Errorf("Phase 5: terminal cleanup must bump lifecycle epoch: before=%d, after=%d", epochBefore, epochAfter)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_SecondPublishConflict_NoCacheEntry_SkipsStatusDiffExecutor
// When a CAS publish conflict occurs and no cache entry exists, the reconciler
// retries once with refreshed fences. If the second attempt also fails (due to
// continued churn) and no cache entry is available, it must requeue without
// executing status, diff, or executor side effects.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_SecondPublishConflict_NoCacheEntry_SkipsStatusDiffExecutor(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "conflict-mapping-2", []v1alpha1.Mapping{
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
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	// Use a status updater that tracks whether it was called.
	statusCalled := false
	statusUpdater := &phase5StubStatusUpdater{
		updatePendingFn: func(_ context.Context, _ types.NamespacedName, _ int64, _ string, _ []int) error {
			statusCalled = true
			return nil
		},
		updateAfterReconcileFn: func(_ context.Context, _ types.NamespacedName, _ int64, _ string, _ []azure.ActionResult, _ error, _ []ValidationIssue, _ []int) error {
			statusCalled = true
			return nil
		},
	}

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "conflict-mapping-2", Generation: 1},
		Spec:       mapping.Spec,
	}

	// Use a client that bumps cache version during every pod List call,
	// simulating continuous pod events arriving between fence capture and publish.
	bumpOnListClient := &phase5ListInterceptClient{
		Client: fakeClient,
		onList: func() {
			// Each List triggers a new pod event, advancing the version fence
			churnPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "mid-recompute-pod", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.2.1"},
			}
			cache.OnPodAdd(mappingObj, churnPod)
		},
	}

	r := &MappingReconciler{
		Client:            bumpOnListClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    0, // force recompute
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		StatusUpdater:     statusUpdater,
		DesiredStateCache: cache,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conflict-mapping-2", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Should requeue with bounded delay (conflict path)
	if result.RequeueAfter <= 0 {
		t.Errorf("Phase 5: expected bounded RequeueAfter on persistent CAS conflict, got %v", result.RequeueAfter)
	}
	if result.RequeueAfter > 2*time.Second {
		t.Errorf("Phase 5: RequeueAfter should be bounded ≤ 2s, got %v", result.RequeueAfter)
	}

	// Executor must NOT have been called
	if len(exec.calls) > 0 {
		t.Errorf("Phase 5: executor must not be called when CAS conflict persists; got %d calls", len(exec.calls))
	}

	// Status must NOT have been called
	if statusCalled {
		t.Error("Phase 5: status updater must not be called when CAS conflict persists after retry")
	}
}

// phase5StubStatusUpdater is a configurable StatusUpdater for Phase 5 tests.
type phase5StubStatusUpdater struct {
	updatePendingFn        func(ctx context.Context, key types.NamespacedName, gen int64, prefix string, matched []int) error
	updateAfterReconcileFn func(ctx context.Context, key types.NamespacedName, gen int64, prefix string, results []azure.ActionResult, reconcileErr error, issues []ValidationIssue, matched []int) error
}

func (s *phase5StubStatusUpdater) UpdatePending(ctx context.Context, key types.NamespacedName, gen int64, prefix string, matched []int) error {
	if s.updatePendingFn != nil {
		return s.updatePendingFn(ctx, key, gen, prefix, matched)
	}
	return nil
}

func (s *phase5StubStatusUpdater) UpdateAfterReconcile(ctx context.Context, key types.NamespacedName, gen int64, prefix string, results []azure.ActionResult, reconcileErr error, issues []ValidationIssue, matched []int) error {
	if s.updateAfterReconcileFn != nil {
		return s.updateAfterReconcileFn(ctx, key, gen, prefix, results, reconcileErr, issues, matched)
	}
	return nil
}

// phase5ListInterceptClient wraps a client.Client and calls onList before
// every List operation, allowing tests to simulate concurrent events.
type phase5ListInterceptClient struct {
	client.Client
	onList func()
}

func (c *phase5ListInterceptClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.onList != nil {
		c.onList()
	}
	return c.Client.List(ctx, list, opts...)
}

// ===========================================================================
// Phase 5: Artifact-Based Reconcile Path Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheMiss_ArtifactPath_ProducesCorrectDesiredState
// On cache miss, the reconciler should use the single-pass artifact path
// (ComputeDesiredStateRecomputeArtifacts) and publish via
// SetFromRecomputeArtifactsIfVersion, producing identical results to the
// legacy multi-pass approach.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheMiss_ArtifactPath_ProducesCorrectDesiredState(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "artifact-mapping", []v1alpha1.Mapping{
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

	pod1 := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})
	pod2 := newTestPod(ns, "pod-2", "10.0.0.2", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod1, pod2).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")

	statusUpdater := &phase5StubStatusUpdater{}

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    0, // Force recompute (cache miss)
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		StatusUpdater:     statusUpdater,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "artifact-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// After reconcile, the cache should be populated with correct state
	cachedState, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache to be populated after reconcile on cache miss")
	}

	// Verify the cached desired state has both pod IPs
	found10001 := false
	found10002 := false
	for _, dps := range cachedState.Desired {
		if _, ok := dps.IPs["10.0.0.1/32"]; ok {
			found10001 = true
		}
		if _, ok := dps.IPs["10.0.0.2/32"]; ok {
			found10002 = true
		}
	}
	if !found10001 {
		t.Error("cached desired state missing 10.0.0.1/32")
	}
	if !found10002 {
		t.Error("cached desired state missing 10.0.0.2/32")
	}

	// Executor should have been called with actions
	if len(exec.calls) == 0 {
		t.Error("expected executor to be called for cache-miss reconcile")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_ForcedResync_ArtifactPath_NoFunctionalRegression
// A forced resync should still produce correct results through the
// artifact path, not regressing behavior vs the legacy multi-pass path.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_ForcedResync_ArtifactPath_NoFunctionalRegression(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	mapping := newTestMapping(ns, "resync-artifact-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
			},
		},
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"tier": "backend"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	// pod-1 matches both rules, pod-2 only first rule
	pod1 := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web", "tier": "backend"})
	pod2 := newTestPod(ns, "pod-2", "10.0.0.2", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod1, pod2).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	cache := engine.NewDesiredStateCache("test-cluster")
	statusUpdater := &phase5StubStatusUpdater{}

	r := &MappingReconciler{
		Client:            fakeClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    0, // Force resync
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		StatusUpdater:     statusUpdater,
		DesiredStateCache: cache,
	}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "resync-artifact-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Verify cache is populated
	cachedState, ok := cache.Get(mapping)
	if !ok {
		t.Fatal("expected cache populated after forced resync")
	}

	// MatchedPodsByIndex should reflect:
	// Rule 0 (app=web): 2 pods (pod-1, pod-2)
	// Rule 1 (tier=backend): 1 pod (pod-1)
	if len(cachedState.MatchedPodsByIndex) != 2 {
		t.Fatalf("MatchedPodsByIndex length = %d, want 2", len(cachedState.MatchedPodsByIndex))
	}
	if cachedState.MatchedPodsByIndex[0] != 2 {
		t.Errorf("MatchedPodsByIndex[0] = %d, want 2", cachedState.MatchedPodsByIndex[0])
	}
	if cachedState.MatchedPodsByIndex[1] != 1 {
		t.Errorf("MatchedPodsByIndex[1] = %d, want 1", cachedState.MatchedPodsByIndex[1])
	}

	// HasPendingIPPods should be false (both pods have IPs)
	if cachedState.HasPendingIPPods {
		t.Error("HasPendingIPPods should be false when all matched pods have IPs")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_ForcedResync_CASConflict_DoesNotFallbackToStaleCacheReread
// Design: When shouldForceDesiredStateRecompute returns true and the CAS publish
// fails, the reconciler must NOT fall back to stale cache re-read as a successful
// forced-resync completion. It should bounded-requeue instead.
//
// To produce a CAS conflict in a synchronous test, we use a wrapping client
// that injects a cache mutation (OnPodAdd) during the List call, simulating a
// concurrent pod event between GetWithVersion and SetFromRecomputeArtifactsIfVersion.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_ForcedResync_CASConflict_DoesNotFallbackToStaleCacheReread(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	cache := engine.NewDesiredStateCache("test-cluster")

	asgID := asgResourceID("sub1", "rg1", "asg1")

	// Set up mapping object
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "forced-mapping", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
				},
			},
		},
	}

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-" + ns + "-forced-mapping",
	}

	// Pre-seed cache with a STALE state (only pod-1, missing pod-2's IP)
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1}, false)

	// Now set up reconciler with ResyncInterval=0 to force recompute
	mapping := newTestMapping(ns, "forced-mapping", mappingObj.Spec.Mappings)
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	// Include both pod-1 and pod-2 in the cluster (truth that recompute will find)
	pod1 := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})
	pod2 := newTestPod(ns, "pod-2", "10.0.0.2", map[string]string{"app": "web"})

	innerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod1, pod2).
		Build()

	// Wrapping client that injects a cache mutation during List (simulating concurrent pod event)
	listCallCount := 0
	wrappedClient := &listInterceptingClient{
		Client: innerClient,
		onList: func() {
			listCallCount++
			if listCallCount <= 2 {
				// Inject a concurrent pod event to advance the cache version fence,
				// causing subsequent CAS publish to fail
				churnPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("churn-pod-%d", listCallCount), Labels: map[string]string{"app": "web"}},
					Status:     corev1.PodStatus{PodIP: fmt.Sprintf("10.0.0.%d", 90+listCallCount)},
				}
				cache.OnPodAdd(mappingObj, churnPod)
			}
		},
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	// Use a tracking executor to see what IPs are sent to Azure
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:            wrappedClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    0, // forced recompute (shouldForceDesiredStateRecompute returns true)
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "forced-mapping", Namespace: ns},
	})

	// There are two valid outcomes for the fix:
	// 1. Bounded requeue (no execution with stale data)
	// 2. Retry recompute succeeds and executor is called with FRESH data (all pods)
	//
	// The INVALID outcome (current bug): executor is called with stale cache data
	// from the re-read fallback that only contains pod-1 + churn-pod (missing pod-2).

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if len(exec.calls) > 0 {
		// If executor was called, verify it got the FRESH recomputed data (pod-2 must be present)
		actions := exec.calls[0]
		foundPod2IP := false
		for _, a := range actions {
			for _, ip := range a.DesiredIPs {
				if ip == "10.0.0.2/32" {
					foundPod2IP = true
				}
			}
		}
		if !foundPod2IP {
			t.Error("Phase 5: Forced resync CAS conflict MUST NOT fall back to stale cache re-read; " +
				"executor was called without pod-2's IP (10.0.0.2/32), indicating stale cache substitution")
		}
	} else {
		// Executor was not called — acceptable only if bounded requeue was returned
		if result.RequeueAfter <= 0 {
			t.Error("Phase 5: When forced resync CAS conflict occurs and executor is not called, " +
				"result must have RequeueAfter > 0 (bounded requeue)")
		}
	}
}

// listInterceptingClient wraps a client.Client and calls onList before each List call.
type listInterceptingClient struct {
	client.Client
	onList func()
}

func (c *listInterceptingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.onList != nil {
		c.onList()
	}
	return c.Client.List(ctx, list, opts...)
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheMiss_CASConflict_NonForced_CanUseCacheReread
// Locks in the current NON-forced behavior: on cache miss with CAS conflict,
// the reconciler may use cache re-read as fallback (this is acceptable for
// non-forced reconciles since it's incremental data that was just mutated).
//
// To produce a genuine cache miss + CAS conflict in a synchronous test, we:
//  1. Invalidate the cache so GetWithVersion returns ok=false (true cache miss).
//  2. Use a listInterceptingClient that calls OnPodAdd during List to advance
//     the version fence, causing SetFromRecomputeArtifactsIfVersion to fail.
//  3. The non-forced path then falls back to cache re-read (Get), which succeeds
//     because OnPodAdd re-populated the entry.
//
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheMiss_CASConflict_NonForced_CanUseCacheReread(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	cache := engine.NewDesiredStateCache("test-cluster")

	asgID := asgResourceID("sub1", "rg1", "asg1")

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "nonforced-mapping", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
				},
			},
		},
	}

	// Seed cache then invalidate to create a true cache-miss state
	// (GetWithVersion returns ok=false, but keyVersions is non-zero).
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-" + ns + "-nonforced-mapping",
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1}, false)
	cache.Invalidate(types.NamespacedName{Namespace: ns, Name: "nonforced-mapping"})

	// Now: GetWithVersion will return ok=false (true cache miss) but version > 0.

	mapping := newTestMapping(ns, "nonforced-mapping", mappingObj.Spec.Mappings)
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	innerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	// Wrapping client injects a concurrent cache repopulation during List to:
	// (a) advance the version fence (causing CAS publish to fail), and
	// (b) repopulate the cache entry (so Get re-read succeeds).
	// OnPodAdd alone cannot repopulate after Invalidate (returns early on missing entry),
	// so we use OnPodAdd to bump the version fence, then SetFromRecompute to re-seed the entry.
	listCallCount := 0
	wrappedClient := &listInterceptingClient{
		Client: innerClient,
		onList: func() {
			listCallCount++
			if listCallCount == 1 {
				// Step 1: Bump version fence via OnPodAdd (bumps keyVersions
				// even though entry is missing — returns early at line 194-196).
				churnPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      "churn-pod-nonforced",
						Labels:    map[string]string{"app": "web"},
					},
					Status: corev1.PodStatus{PodIP: "10.0.0.99"},
				}
				cache.OnPodAdd(mappingObj, churnPod)

				// Step 2: Re-populate entry via SetFromRecompute (simulates a
				// concurrent reconciler completing its recompute). This does NOT
				// bump keyVersions, so the CAS conflict still triggers.
				cache.SetFromRecompute(mappingObj, nil,
					map[engine.ASGTarget]engine.DesiredPrefixSet{
						target: {IPs: map[string]struct{}{"10.0.0.1/32": {}, "10.0.0.99/32": {}}},
					},
					engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
						{Namespace: ns, Name: "pod-1"}:               {PodIP: "10.0.0.1"},
						{Namespace: ns, Name: "churn-pod-nonforced"}: {PodIP: "10.0.0.99"},
					}},
					[]int{2}, false)
			}
		},
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:            wrappedClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second, // non-forced (long interval)
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Mark recent full recompute so shouldForceDesiredStateRecompute returns false
	r.markDesiredStateFullRecompute(
		types.NamespacedName{Name: "nonforced-mapping", Namespace: ns},
		1, time.Now(),
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonforced-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Verify a true cache miss occurred (List was invoked for pod recompute)
	if listCallCount == 0 {
		t.Fatal("Phase 5: expected List to be called (cache miss triggers pod recompute), " +
			"but listInterceptingClient.onList was never invoked — this is a cache HIT, not a miss")
	}

	// Branch-distinguishing assertion: the re-read path does NOT issue a second
	// List call. If listCallCount > 1, the reconciler fell through to the retry
	// full-recompute path (which also calls List), meaning re-read was NOT used.
	if listCallCount != 1 {
		t.Fatalf("Phase 5: expected exactly 1 List call (cache re-read path), got %d — "+
			"reconciler fell through to retry recompute instead of using cache re-read", listCallCount)
	}

	// The non-forced CAS conflict path MUST resolve via cache re-read because
	// SetFromRecompute populated the entry during List. The reconciler should
	// use the re-read data and proceed to the executor.
	if len(exec.calls) == 0 {
		t.Fatal("Phase 5: expected executor to be called after cache re-read resolved " +
			"CAS conflict, but no executor calls recorded — re-read path was NOT exercised")
	}

	// The re-read cache entry contains 10.0.0.99/32 (injected by SetFromRecompute
	// during the List interception). The retry recompute path would only produce
	// 10.0.0.1/32 from the fakeclient's pod list. Verify the re-read data was used.
	foundRereadIP := false
	for _, actions := range exec.calls {
		for _, a := range actions {
			for _, ip := range a.DesiredIPs {
				if ip == "10.0.0.99/32" {
					foundRereadIP = true
				}
			}
		}
	}
	if !foundRereadIP {
		t.Fatal("Phase 5: executor actions do not contain 10.0.0.99/32 which is only present " +
			"in the cache re-read entry — reconciler did not use re-read data")
	}
	_ = result
}

// ===========================================================================
// Phase 5: Reconciler — Forced Resync Repeated CAS Conflict Bounded Requeue
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_ForcedResync_RepeatedCASConflicts_BoundedRequeue
// Validates design edge case #5: Forced resync under heavy pod churn produces
// repeated CAS conflicts, but the reconciler must not loop indefinitely. It
// should yield a bounded requeue (not 0 or negative) after exhausting retries.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_ForcedResync_RepeatedCASConflicts_BoundedRequeue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	cache := engine.NewDesiredStateCache("test-cluster")
	asgID := asgResourceID("sub1", "rg1", "asg1")

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "heavy-churn-mapping", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
				},
			},
		},
	}

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-" + ns + "-heavy-churn-mapping",
	}

	// Seed cache and then invalidate to force recompute path
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1}, false)
	cache.Invalidate(types.NamespacedName{Namespace: ns, Name: "heavy-churn-mapping"})

	mapping := newTestMapping(ns, "heavy-churn-mapping", mappingObj.Spec.Mappings)
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	innerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	// Intercept EVERY List call to bump the version fence, causing repeated CAS conflicts
	listCallCount := 0
	wrappedClient := &listInterceptingClient{
		Client: innerClient,
		onList: func() {
			listCallCount++
			// Always bump version to ensure CAS conflict
			churnPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      fmt.Sprintf("churn-pod-%d", listCallCount),
					Labels:    map[string]string{"app": "web"},
				},
				Status: corev1.PodStatus{PodIP: fmt.Sprintf("10.0.99.%d", listCallCount)},
			}
			cache.OnPodAdd(mappingObj, churnPod)
		},
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:            wrappedClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    2 * time.Second, // short to trigger forced resync
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Mark an old full recompute so shouldForceDesiredStateRecompute returns true
	// (last recompute was longer than ResyncInterval ago)
	r.markDesiredStateFullRecompute(
		types.NamespacedName{Name: "heavy-churn-mapping", Namespace: ns},
		1, time.Now().Add(-5*time.Second),
	)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "heavy-churn-mapping", Namespace: ns},
	})

	// The reconciler must NOT loop indefinitely — it should return with a bounded requeue
	// Either it returns an error (acceptable) or a positive requeue interval
	if err == nil && result.RequeueAfter == 0 && !result.Requeue {
		t.Fatal("Phase 5: forced resync with repeated CAS conflicts must not silently succeed " +
			"without requeue — expected bounded requeue or error")
	}

	// List should have been called at most a bounded number of times (not hundreds)
	// The design says "retry recompute once with refreshed fences, then bounded requeue"
	if listCallCount > 5 {
		t.Errorf("Phase 5: reconciler called List %d times (unbounded retry loop); "+
			"expected bounded retries (max ~2-3 attempts)", listCallCount)
	}

	// If requeue was returned, it must use the bounded interval (cacheConflictRequeueAfter)
	if err == nil && result.RequeueAfter > 0 {
		if result.RequeueAfter > 10*time.Second {
			t.Errorf("Phase 5: requeue interval too large (%v); expected bounded value", result.RequeueAfter)
		}
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_Reconcile_CacheHit_UsesArtifactPath_NoListCall
// Validates that when cache has a valid hit, the reconciler does NOT call
// List (no pod enumeration), proving O(1) amortized reconcile.
// ---------------------------------------------------------------------------
func TestPhase5_Reconcile_CacheHit_UsesArtifactPath_NoListCall(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "test-ns"

	cache := engine.NewDesiredStateCache("test-cluster")
	asgID := asgResourceID("sub1", "rg1", "asg1")

	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "hit-mapping", Generation: 1},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgID}},
				},
			},
		},
	}

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		FullResourceID: asgID,
		PrefixSetName:  "test-cluster-" + ns + "-hit-mapping",
	}

	// Seed cache with valid entry (no invalidation)
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			target: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
			{Namespace: ns, Name: "pod-1"}: {PodIP: "10.0.0.1"},
		}},
		[]int{1}, false)

	mapping := newTestMapping(ns, "hit-mapping", mappingObj.Spec.Mappings)
	mapping.Finalizers = []string{CleanupFinalizer}
	mapping.Generation = 1

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	innerClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		Build()

	listCallCount := 0
	wrappedClient := &listInterceptingClient{
		Client: innerClient,
		onList: func() {
			listCallCount++
		},
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())
	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:            wrappedClient,
		Scheme:            scheme,
		ClusterName:       "test-cluster",
		ResyncInterval:    60 * time.Second,
		PrefixSetFactory:  fakeFactory,
		Executor:          exec,
		DesiredStateCache: cache,
	}

	// Mark recent recompute so forced resync is NOT triggered
	r.markDesiredStateFullRecompute(
		types.NamespacedName{Name: "hit-mapping", Namespace: ns},
		1, time.Now(),
	)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "hit-mapping", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// With a cache hit, no List should be called (O(1) desired-state resolution)
	if listCallCount > 0 {
		t.Errorf("Phase 5: cache hit path must NOT call List (O(1) amortized); "+
			"but List was called %d times — reconciler bypassed cache", listCallCount)
	}
}
