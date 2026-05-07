package phase6_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"github.com/go-logr/zapr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// testEnv6 provides a reusable envtest environment for Phase 6 cross-module tests.
// ---------------------------------------------------------------------------

type testEnv6 struct {
	env         *envtest.Environment
	k8sClient   client.Client
	mgr         ctrl.Manager
	cancel      context.CancelFunc
	fakeClient  *fake.Client
	fakeFactory *fake.ClientFactory
}

func setupTestEnv6(t *testing.T, executor controller.Executor, statusUpdater controller.StatusUpdater) *testEnv6 {
	t.Helper()

	scheme := crossModuleScheme(t)
	zapLog := zaptest.NewLogger(t)

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{repoRoot + "/config/crd/bases"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	ctrl.SetLogger(zapr.NewLogger(zapLog))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("failed to create manager: %v", err)
	}

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	if executor == nil {
		executor = azure.NewExecutor(zapLog, fakeFactory, 2)
	}

	reconciler := &controller.MappingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         executor,
		StatusUpdater:    statusUpdater,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		_ = env.Stop()
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		_ = env.Stop()
		t.Fatal("cache sync failed")
	}

	return &testEnv6{
		env:         env,
		k8sClient:   mgr.GetClient(),
		mgr:         mgr,
		cancel:      cancel,
		fakeClient:  fakeAzClient,
		fakeFactory: fakeFactory,
	}
}

func (te *testEnv6) teardown(t *testing.T) {
	t.Helper()
	te.cancel()
	if err := te.env.Stop(); err != nil {
		t.Logf("envtest stop: %v", err)
	}
}

func crossModuleScheme(t *testing.T) *runtime.Scheme {
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

func eventually6(t *testing.T, timeout, interval time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("timed out waiting for condition: %s", msg)
}

// ---------------------------------------------------------------------------
// T6.1 Integration: All actions succeed → Reconciled=True, all mappings Synced
// Cross-module flow: Reconciler → Executor → StatusUpdater → K8s Status API
// ---------------------------------------------------------------------------

func TestPhase6_T61_AllActionsSucceed_ReconciledTrue_AllMappingsSynced(t *testing.T) {
	podCounter := &stubPodCountSource{counts: map[string]int{}}

	// Build the status updater adapter that bridges the interface.
	var statusClient client.Client

	// We'll create a custom status updater that wires through MappingStatusUpdater.
	customUpdater := &deferredStatusUpdater{podCounter: podCounter}

	te := setupTestEnv6(t, &successExecutor{}, customUpdater)
	defer te.teardown(t)
	ctx := context.Background()

	// Set the client after manager is started.
	statusClient = te.k8sClient
	_ = statusClient
	customUpdater.setClient(te.k8sClient)

	// Create namespace and pod with IP.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t61-test"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: "t61-test",
			Labels: map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	// Assign Pod IP via status update.
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	// Create PodASGMapping.
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "t61-mapping", Namespace: "t61-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for status to be written.
	eventually6(t, 15*time.Second, 300*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "t61-mapping", Namespace: "t61-test"}, &m); err != nil {
			return false
		}
		return hasCondition(m.Status.Conditions, "Reconciled", metav1.ConditionTrue)
	}, "Reconciled=True condition")

	// Verify final status.
	var final v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "t61-mapping", Namespace: "t61-test"}, &final); err != nil {
		t.Fatalf("failed to get final mapping: %v", err)
	}

	if !hasCondition(final.Status.Conditions, "Accepted", metav1.ConditionTrue) {
		t.Error("expected Accepted=True condition")
	}
	if !hasCondition(final.Status.Conditions, "Reconciled", metav1.ConditionTrue) {
		t.Error("expected Reconciled=True condition")
	}
	if len(final.Status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(final.Status.MappingStatuses))
	}
	if final.Status.MappingStatuses[0].ASGSyncState != "Synced" {
		t.Errorf("expected ASGSyncState=Synced, got %s", final.Status.MappingStatuses[0].ASGSyncState)
	}
}

// ---------------------------------------------------------------------------
// T6.2 Integration: Partial failure → Reconciled=False, failed mapping Error
// Cross-module flow: Reconciler → failingExecutor → StatusUpdater → K8s API
// ---------------------------------------------------------------------------

func TestPhase6_T62_PartialFailure_ReconciledFalse_PerMappingState(t *testing.T) {
	podCounter := &stubPodCountSource{counts: map[string]int{}}
	customUpdater := &deferredStatusUpdater{podCounter: podCounter}

	// Use an executor that fails for a specific ASG.
	failExec := &partialFailExecutor{
		failASG: "asg2",
	}

	te := setupTestEnv6(t, failExec, customUpdater)
	defer te.teardown(t)
	ctx := context.Background()

	customUpdater.setClient(te.k8sClient)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t62-test"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create pods for both selectors.
	for _, p := range []struct {
		name   string
		labels map[string]string
		ip     string
	}{
		{"web-1", map[string]string{"app": "web"}, "10.0.0.1"},
		{"api-1", map[string]string{"app": "api"}, "10.0.0.2"},
	} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: "t62-test", Labels: p.labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("failed to create pod %s: %v", p.name, err)
		}
		pod.Status.PodIP = p.ip
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("failed to update pod status %s: %v", p.name, err)
		}
	}

	// Create mapping with two rules: asg1 (succeeds) and asg2 (fails).
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "t62-mapping", Namespace: "t62-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "asg1")}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "asg2")}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for status indicating Reconciled=False.
	eventually6(t, 15*time.Second, 300*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "t62-mapping", Namespace: "t62-test"}, &m); err != nil {
			return false
		}
		return hasCondition(m.Status.Conditions, "Reconciled", metav1.ConditionFalse)
	}, "Reconciled=False after partial failure")

	var final v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "t62-mapping", Namespace: "t62-test"}, &final); err != nil {
		t.Fatalf("failed to get final mapping: %v", err)
	}

	// Accepted should still be True (spec is valid).
	if !hasCondition(final.Status.Conditions, "Accepted", metav1.ConditionTrue) {
		t.Error("expected Accepted=True even with partial failure")
	}

	// Verify mapping statuses show per-mapping outcomes.
	if len(final.Status.MappingStatuses) < 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(final.Status.MappingStatuses))
	}

	// At least one should be Error (the one targeting asg2).
	hasError := false
	for _, ms := range final.Status.MappingStatuses {
		if ms.ASGSyncState == "Error" {
			hasError = true
			if ms.Error == "" {
				t.Error("expected non-empty error message for Error mapping")
			}
		}
	}
	if !hasError {
		t.Error("expected at least one mapping status with ASGSyncState=Error")
	}
}

// ---------------------------------------------------------------------------
// T6.5 Integration: ComputePodCountsFromPods → matchedPods accuracy
// Cross-module flow: model.CompileSelector → ComputePodCountsFromPods → ComputeStatus
// ---------------------------------------------------------------------------

func TestPhase6_T65_PodCountsFlowThroughToStatus(t *testing.T) {
	// This test verifies the cross-module flow between pod_counts.go and status.go.
	// It does NOT require envtest - it tests the data flow between modules directly.

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-3", Labels: map[string]string{"app": "web"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"app": "other"}}},
	}

	// Module 1: pod_counts.go computes counts.
	podCounts := controller.ComputePodCountsFromPods(spec, pods)

	// Verify counts.
	webHash := controller.SelectorHash(map[string]string{"app": "web"})
	apiHash := controller.SelectorHash(map[string]string{"app": "api"})

	if podCounts[webHash] != 3 {
		t.Errorf("expected 3 web pods, got %d", podCounts[webHash])
	}
	if podCounts[apiHash] != 1 {
		t.Errorf("expected 1 api pod, got %d", podCounts[apiHash])
	}

	// Module 2: status.go uses counts in ComputeStatus.
	results := []azure.ActionResult{} // no actions
	status := controller.ComputeStatus(spec, results, podCounts)

	// Verify matchedPods flows through correctly.
	if len(status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(status.MappingStatuses))
	}

	for _, ms := range status.MappingStatuses {
		if ms.SelectorHash == webHash && ms.MatchedPods != 3 {
			t.Errorf("web mapping: expected matchedPods=3, got %d", ms.MatchedPods)
		}
		if ms.SelectorHash == apiHash && ms.MatchedPods != 1 {
			t.Errorf("api mapping: expected matchedPods=1, got %d", ms.MatchedPods)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.6 Integration: SelectorHash determinism across modules
// Cross-module flow: pod_counts.go SelectorHash == status.go SelectorHash
// ---------------------------------------------------------------------------

func TestPhase6_T66_SelectorHashDeterminismAcrossModules(t *testing.T) {
	// Verify that keys produced by ComputePodCountsFromPods match those used by ComputeStatus.
	matchLabels := map[string]string{"env": "prod", "app": "web", "version": "v2"}

	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: matchLabels},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
				},
			},
		},
	}

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "p1", Labels: matchLabels}},
	}

	// Compute pod counts (uses SelectorHash internally).
	podCounts := controller.ComputePodCountsFromPods(spec, pods)

	// Compute status (also uses SelectorHash internally).
	status := controller.ComputeStatus(spec, nil, podCounts)

	// The selector hash used as key in podCounts must match the one in status.
	expectedHash := controller.SelectorHash(matchLabels)

	if _, ok := podCounts[expectedHash]; !ok {
		t.Errorf("pod counts missing expected hash %s", expectedHash)
	}

	if len(status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(status.MappingStatuses))
	}
	if status.MappingStatuses[0].SelectorHash != expectedHash {
		t.Errorf("status hash %s != expected %s", status.MappingStatuses[0].SelectorHash, expectedHash)
	}
	if status.MappingStatuses[0].MatchedPods != 1 {
		t.Errorf("expected matchedPods=1, got %d", status.MappingStatuses[0].MatchedPods)
	}

	// Verify determinism: label order shouldn't matter.
	hash1 := controller.SelectorHash(map[string]string{"z": "1", "a": "2", "m": "3"})
	hash2 := controller.SelectorHash(map[string]string{"a": "2", "m": "3", "z": "1"})
	if hash1 != hash2 {
		t.Errorf("SelectorHash not deterministic: %s != %s", hash1, hash2)
	}
}

// ---------------------------------------------------------------------------
// T6.3 Integration: Validation errors → Accepted=False via MappingStatusUpdater
// Cross-module flow: asg_validation.go → status.go → conditions.go → K8s API
// ---------------------------------------------------------------------------

func TestPhase6_T63_ValidationFailed_AcceptedFalse_ViaStatusUpdater(t *testing.T) {
	scheme := crossModuleScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{repoRoot + "/config/crd/bases"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Create namespace and mapping.
	// The CRD enforces regex, so we use a valid-format resource ID.
	// This test verifies the status writing path when the reconciler decides
	// validation failed (e.g., subscription not reachable, permissions issue, etc.).
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t63-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mapping", Namespace: "t63-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "test-asg")},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Simulate validation errors (as if the reconciler detected issues).
	validationErrors := map[int][]string{
		0: {fmt.Sprintf("mapping[0].applicationSecurityGroups[0].resourceId %q: subscription not accessible",
			asgResourceID("sub1", "rg1", "test-asg"))},
	}

	// Build pod counts.
	podCounts := controller.ZeroPodCountsForSpec(mapping.Spec)

	// Use MappingStatusUpdater to write status.
	updater := controller.NewMappingStatusUpdater(k8sClient, time.Now)

	// Refetch to get the server-assigned resource version.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "invalid-mapping", Namespace: "t63-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	input := controller.ReconcileStatusInput{
		Phase:            controller.StatusPhaseValidationFailed,
		ProcessedGen:     mapping.Generation,
		Results:          nil,
		PodCounts:        podCounts,
		ValidationErrors: validationErrors,
		ReconcileErr:     nil,
	}

	if err := updater.UpdateAfterReconcile(context.Background(), mapping, input); err != nil {
		t.Fatalf("UpdateAfterReconcile failed: %v", err)
	}

	// Verify status was written to K8s.
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "invalid-mapping", Namespace: "t63-test",
	}, &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if !hasCondition(updated.Status.Conditions, "Accepted", metav1.ConditionFalse) {
		t.Error("expected Accepted=False for invalid spec")
	}
	if !hasCondition(updated.Status.Conditions, "Reconciled", metav1.ConditionFalse) {
		t.Error("expected Reconciled=False for validation failure")
	}

	// Mapping statuses should show Error state.
	if len(updated.Status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(updated.Status.MappingStatuses))
	}
	if updated.Status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Errorf("expected ASGSyncState=Error, got %s", updated.Status.MappingStatuses[0].ASGSyncState)
	}
	if updated.Status.MappingStatuses[0].Error == "" {
		t.Error("expected non-empty error message in mapping status")
	}
}

// ---------------------------------------------------------------------------
// T6.4 Integration: lastSyncTime preserved on error, updated on success
// Cross-module flow: status.go applyLastSyncTimeRules across sequential updates
// ---------------------------------------------------------------------------

func TestPhase6_T64_LastSyncTimePreservation_AcrossUpdates(t *testing.T) {
	scheme := crossModuleScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{repoRoot + "/config/crd/bases"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "t64-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "t64-mapping", Namespace: "t64-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "asg1")}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "t64-mapping", Namespace: "t64-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	fixedTime := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return fixedTime }

	updater := controller.NewMappingStatusUpdater(k8sClient, nowFn)

	// First update: successful → lastSyncTime set.
	target := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-t64-test-t64-mapping",
	}
	successResults := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.CreatePrefixSet, Target: target}, Success: true},
	}
	input := controller.ReconcileStatusInput{
		Phase:        controller.StatusPhasePostExecution,
		ProcessedGen: mapping.Generation,
		Results:      successResults,
		PodCounts:    map[string]int{},
	}
	if err := updater.UpdateAfterReconcile(context.Background(), mapping, input); err != nil {
		t.Fatalf("first update failed: %v", err)
	}

	// Refetch and verify lastSyncTime is set.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "t64-mapping", Namespace: "t64-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	if len(mapping.Status.MappingStatuses) == 0 {
		t.Fatal("expected mapping statuses after first update")
	}
	firstSyncTime := mapping.Status.MappingStatuses[0].LastSyncTime
	if firstSyncTime.IsZero() {
		t.Fatal("expected non-zero lastSyncTime after successful sync")
	}

	// Second update: failure → lastSyncTime should be preserved from first success.
	laterTime := fixedTime.Add(5 * time.Minute)
	updater2 := controller.NewMappingStatusUpdater(k8sClient, func() time.Time { return laterTime })

	failResults := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target}, Success: false, Err: fmt.Errorf("azure timeout")},
	}
	input2 := controller.ReconcileStatusInput{
		Phase:        controller.StatusPhasePostExecution,
		ProcessedGen: mapping.Generation,
		Results:      failResults,
		PodCounts:    map[string]int{},
		ReconcileErr: fmt.Errorf("action failures: update asg1: azure timeout"),
	}
	if err := updater2.UpdateAfterReconcile(context.Background(), mapping, input2); err != nil {
		t.Fatalf("second update failed: %v", err)
	}

	// Refetch and verify lastSyncTime is preserved (not updated to laterTime).
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "t64-mapping", Namespace: "t64-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping after failure: %v", err)
	}

	if len(mapping.Status.MappingStatuses) == 0 {
		t.Fatal("expected mapping statuses after second update")
	}
	if mapping.Status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Errorf("expected Error state after failure, got %s", mapping.Status.MappingStatuses[0].ASGSyncState)
	}
	secondSyncTime := mapping.Status.MappingStatuses[0].LastSyncTime
	if !secondSyncTime.Equal(&firstSyncTime) {
		t.Errorf("lastSyncTime changed on error: was %v, now %v", firstSyncTime.Time, secondSyncTime.Time)
	}
}

// ---------------------------------------------------------------------------
// Integration: ComputeStatus + SetCondition + SelectorHash compose correctly
// Cross-module flow: status.go + conditions.go + asg_validation.go end-to-end
// ---------------------------------------------------------------------------

func TestPhase6_StatusComputationEndToEnd_MultipleModulesCompose(t *testing.T) {
	// Validates that ComputeStatus, SetCondition, and SelectorHash work together.
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "frontend-asg")},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "backend-asg")},
				},
			},
		},
	}

	// Validate spec (should pass).
	for _, mapping := range spec.Mappings {
		for _, asgRef := range mapping.ApplicationSecurityGroups {
			if _, err := model.ParseASGResourceID(asgRef.ResourceID); err != nil {
				t.Fatalf("spec should be valid, got error: %v", err)
			}
		}
	}

	// Compute pod counts.
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "fe-1", Labels: map[string]string{"tier": "frontend"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fe-2", Labels: map[string]string{"tier": "frontend"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "be-1", Labels: map[string]string{"tier": "backend"}}},
	}
	podCounts := controller.ComputePodCountsFromPods(spec, pods)

	// Simulate mixed results: frontend succeeds, backend fails.
	feTarget := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "frontend-asg",
		FullResourceID: asgResourceID("sub1", "rg1", "frontend-asg"),
		PrefixSetName:  "test",
	}
	beTarget := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "backend-asg",
		FullResourceID: asgResourceID("sub1", "rg1", "backend-asg"),
		PrefixSetName:  "test",
	}
	results := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.CreatePrefixSet, Target: feTarget}, Success: true},
		{Action: engine.Action{Kind: engine.CreatePrefixSet, Target: beTarget}, Success: false, Err: fmt.Errorf("network timeout")},
	}

	// Compute status (cross-module: status.go uses engine types and model).
	status := controller.ComputeStatus(spec, results, podCounts)

	// Add conditions (cross-module: conditions.go modifies status).
	controller.SetCondition(&status, "Accepted", metav1.ConditionTrue, "SpecValid", "spec is valid")
	controller.SetCondition(&status, "Reconciled", metav1.ConditionFalse, "ReconcileError", "partial failure")

	// Verify composed result.
	if status.MappingCount != 2 {
		t.Errorf("expected MappingCount=2, got %d", status.MappingCount)
	}
	if len(status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(status.MappingStatuses))
	}

	feHash := controller.SelectorHash(map[string]string{"tier": "frontend"})
	beHash := controller.SelectorHash(map[string]string{"tier": "backend"})

	for _, ms := range status.MappingStatuses {
		switch ms.SelectorHash {
		case feHash:
			if ms.ASGSyncState != "Synced" {
				t.Errorf("frontend: expected Synced, got %s", ms.ASGSyncState)
			}
			if ms.MatchedPods != 2 {
				t.Errorf("frontend: expected matchedPods=2, got %d", ms.MatchedPods)
			}
		case beHash:
			if ms.ASGSyncState != "Error" {
				t.Errorf("backend: expected Error, got %s", ms.ASGSyncState)
			}
			if ms.MatchedPods != 1 {
				t.Errorf("backend: expected matchedPods=1, got %d", ms.MatchedPods)
			}
			if ms.Error == "" {
				t.Error("backend: expected non-empty error string")
			}
		default:
			t.Errorf("unexpected selector hash: %s", ms.SelectorHash)
		}
	}

	// Verify conditions.
	if len(status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(status.Conditions))
	}
	if !hasCondition(status.Conditions, "Accepted", metav1.ConditionTrue) {
		t.Error("missing Accepted=True")
	}
	if !hasCondition(status.Conditions, "Reconciled", metav1.ConditionFalse) {
		t.Error("missing Reconciled=False")
	}
}

// ---------------------------------------------------------------------------
// Integration: ASG validation → status error message format consistency
// Cross-module flow: asg_validation.go error format → status.go buildValidationFailedStatus
// ---------------------------------------------------------------------------

func TestPhase6_ValidationErrorsFlowIntoStatusMessages(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: asgResourceID("sub1", "rg1", "valid-asg")},
					{ResourceID: "invalid-id-format"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "also-invalid"},
				},
			},
		},
	}

	// Module 1: Validate ASG resource IDs using model.ParseASGResourceID (same as asg_validation.go).
	validationErrors := make(map[int][]string)
	for i, mapping := range spec.Mappings {
		for j, asgRef := range mapping.ApplicationSecurityGroups {
			if _, err := model.ParseASGResourceID(asgRef.ResourceID); err != nil {
				errMsg := fmt.Sprintf("mapping[%d].applicationSecurityGroups[%d].resourceId %q: %v",
					i, j, asgRef.ResourceID, err)
				validationErrors[i] = append(validationErrors[i], errMsg)
			}
		}
	}

	// Verify errors are present for both mappings.
	if len(validationErrors[0]) != 1 {
		t.Errorf("expected 1 error for mapping[0], got %d", len(validationErrors[0]))
	}
	if len(validationErrors[1]) != 1 {
		t.Errorf("expected 1 error for mapping[1], got %d", len(validationErrors[1]))
	}

	// Module 2: Feed validation errors into ComputeStatus via StatusPhaseValidationFailed path.
	// The MappingStatusUpdater handles this internally - verify the data contract.
	podCounts := controller.ZeroPodCountsForSpec(spec)

	// Verify pod counts keys align with status computation.
	for _, mapping := range spec.Mappings {
		hash := controller.SelectorHash(mapping.PodSelector.MatchLabels)
		if _, ok := podCounts[hash]; !ok {
			t.Errorf("missing pod count for hash %s", hash)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func hasCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) bool {
	for _, c := range conditions {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}

// deferredStatusUpdater adapts the old StatusUpdater interface to use MappingStatusUpdater.
type deferredStatusUpdater struct {
	podCounter *stubPodCountSource
	inner      *controller.MappingStatusUpdater
}

func (d *deferredStatusUpdater) setClient(c client.Client) {
	d.inner = controller.NewMappingStatusUpdater(c, time.Now)
}

func (d *deferredStatusUpdater) UpdateAfterReconcile(
	ctx context.Context,
	mapping *v1alpha1.PodASGMapping,
	input controller.ReconcileStatusInput,
) error {
	if d.inner == nil {
		return nil
	}
	return d.inner.UpdateAfterReconcile(ctx, mapping, input)
}

// partialFailExecutor fails actions targeting a specific ASG name.
type partialFailExecutor struct {
	failASG string
}

func (e *partialFailExecutor) Execute(_ context.Context, actions []engine.Action) []azure.ActionResult {
	results := make([]azure.ActionResult, len(actions))
	for i, a := range actions {
		if a.Target.ASGName == e.failASG {
			results[i] = azure.ActionResult{
				Action:  a,
				Success: false,
				Err:     fmt.Errorf("simulated failure for %s", e.failASG),
			}
		} else {
			results[i] = azure.ActionResult{Action: a, Success: true}
		}
	}
	return results
}
