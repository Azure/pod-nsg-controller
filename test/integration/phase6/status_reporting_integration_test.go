package phase6_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/go-logr/zapr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// T6.7: Status subresource update does NOT trigger re-reconcile
// ---------------------------------------------------------------------------

func TestPhase6_T67_StatusSubresourceUpdate_DoesNotTriggerReconcile(t *testing.T) {
	scheme := integrationScheme(t)
	logger := zapr.NewLogger(zaptest.NewLogger(t))
	ctrl.SetLogger(logger)

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	// Track reconcile count.
	var reconcileCount int64
	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	// Create a counting executor.
	countingExecutor := &countingExecutor{count: &reconcileCount}

	innerUpdater := controller.NewMappingStatusUpdater(
		mgr.GetClient(),
		time.Now,
	)
	statusUpdater := &statusUpdaterAdapter{inner: innerUpdater}

	reconciler := &controller.MappingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         countingExecutor,
		StatusUpdater:    statusUpdater,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	// Wait for manager cache to sync.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache sync failed")
	}

	k8sClient := mgr.GetClient()

	// Create namespace.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "phase6-test"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create PodASGMapping.
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-mapping",
			Namespace: "phase6-test",
		},
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
	if err := k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for initial reconcile(s) to complete.
	time.Sleep(3 * time.Second)

	// Record reconcile count after initial reconcile stabilizes.
	initialCount := atomic.LoadInt64(&reconcileCount)

	// Now perform a status-only update.
	var latest v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name: "test-mapping", Namespace: "phase6-test",
	}, &latest); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}

	latest.Status.MappingStatuses = []v1alpha1.MappingStatus{
		{SelectorHash: "abcdef1234567890", MatchedPods: 99, ASGSyncState: "Synced"},
	}
	if err := k8sClient.Status().Update(ctx, &latest); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Wait and check that no additional reconcile was triggered.
	time.Sleep(2 * time.Second)

	finalCount := atomic.LoadInt64(&reconcileCount)
	if finalCount > initialCount {
		t.Errorf("status-only update triggered %d additional reconcile(s); expected 0",
			finalCount-initialCount)
	}
}

// ---------------------------------------------------------------------------
// Integration: Full status wiring writes conditions and mapping statuses
// ---------------------------------------------------------------------------

func TestPhase6_StatusWiring_MainStyleConstruction_WritesConditionsAndMappingStatuses(t *testing.T) {
	scheme := integrationScheme(t)
	logger := zapr.NewLogger(zaptest.NewLogger(t))
	ctrl.SetLogger(logger)

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	innerUpdater2 := controller.NewMappingStatusUpdater(
		mgr.GetClient(),
		time.Now,
	)
	statusUpdater := &statusUpdaterAdapter{inner: innerUpdater2}

	// Simple executor that always succeeds.
	exec := &successExecutor{}

	reconciler := &controller.MappingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    statusUpdater,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache sync failed")
	}

	k8sClient := mgr.GetClient()

	// Create namespace and pod.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "phase6-wiring"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: "phase6-wiring",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Create mapping.
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wiring-mapping",
			Namespace: "phase6-wiring",
		},
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
	if err := k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for reconcile to complete and write status.
	time.Sleep(5 * time.Second)

	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}

	// Verify conditions are written.
	if len(updated.Status.Conditions) == 0 {
		t.Error("expected conditions to be set after reconcile, got none")
	}

	// Verify mapping statuses are written.
	if len(updated.Status.MappingStatuses) == 0 {
		t.Error("expected mapping statuses to be written after reconcile, got none")
	}

	// Verify Accepted=True condition exists.
	foundAccepted := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "Accepted" && c.Status == metav1.ConditionTrue {
			foundAccepted = true
		}
	}
	if !foundAccepted {
		t.Error("expected Accepted=True condition after successful reconcile")
	}
}

// ---------------------------------------------------------------------------
// Test helpers for integration tests
// ---------------------------------------------------------------------------

type stubPodCountSource struct {
	counts map[string]int
}

func (s *stubPodCountSource) CountBySelectorHash(
	_ context.Context,
	_ string,
	_ v1alpha1.PodASGMappingSpec,
) (map[string]int, error) {
	return s.counts, nil
}

// statusUpdaterAdapter wraps MappingStatusUpdater to satisfy the existing
// StatusUpdater interface while Phase 6 is not yet fully wired.
type statusUpdaterAdapter struct {
	inner *controller.MappingStatusUpdater
}

func (a *statusUpdaterAdapter) UpdateAfterReconcile(
	ctx context.Context,
	mapping *v1alpha1.PodASGMapping,
	input controller.ReconcileStatusInput,
) error {
	return a.inner.UpdateAfterReconcile(ctx, mapping, input)
}

type countingExecutor struct {
	count *int64
}

func (e *countingExecutor) Execute(_ context.Context, actions []engine.Action) []azure.ActionResult {
	atomic.AddInt64(e.count, 1)
	results := make([]azure.ActionResult, len(actions))
	for i, a := range actions {
		results[i] = azure.ActionResult{Action: a, Success: true}
	}
	return results
}

type successExecutor struct{}

func (e *successExecutor) Execute(_ context.Context, actions []engine.Action) []azure.ActionResult {
	results := make([]azure.ActionResult, len(actions))
	for i, a := range actions {
		results[i] = azure.ActionResult{Action: a, Success: true}
	}
	return results
}
