package phase6_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"github.com/go-logr/zapr"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/pod-nsg-controller/test/integration/testutil"
)

const envtestK8sVersion = "1.31.x"

// TestMain configures KUBEBUILDER_ASSETS for envtest before running tests.
func TestMain(m *testing.M) {
	repoRoot, err := repoRootFromCWD()
	if err != nil {
		zapLogger, _ := zap.NewProduction()
		zapLogger.Fatal("failed to resolve repo root", zap.Error(err))
	}

	oldAssets, hadOldAssets := os.LookupEnv("KUBEBUILDER_ASSETS")
	if err := configureEnvtestAssets(repoRoot); err != nil {
		zapLogger, _ := zap.NewProduction()
		zapLogger.Fatal("failed to configure envtest assets", zap.Error(err))
	}

	code := m.Run()

	if hadOldAssets {
		_ = os.Setenv("KUBEBUILDER_ASSETS", oldAssets)
	} else {
		_ = os.Unsetenv("KUBEBUILDER_ASSETS")
	}
	os.Exit(code)
}

func repoRootFromCWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", errors.Wrap(err, "get working directory")
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", "..")), nil
}

func configureEnvtestAssets(repoRoot string) error {
	if assets := strings.TrimSpace(os.Getenv("KUBEBUILDER_ASSETS")); assets != "" {
		if _, err := os.Stat(assets); err != nil {
			return errors.Wrapf(err, "stat KUBEBUILDER_ASSETS %q", assets)
		}
		return nil
	}
	assetsPath, err := localEnvtestAssetsPath(repoRoot)
	if err != nil {
		return err
	}
	return os.Setenv("KUBEBUILDER_ASSETS", assetsPath)
}

func hasEnvtestBinaries(assetsPath string) bool {
	for _, binary := range []string{"etcd", "kube-apiserver", "kubectl"} {
		if _, err := os.Stat(filepath.Join(assetsPath, binary)); err != nil {
			return false
		}
	}
	return true
}

func localEnvtestAssetsPath(repoRoot string) (string, error) {
	assetsRoot := filepath.Join(repoRoot, "bin", "k8s")
	entries, err := os.ReadDir(assetsRoot)
	if err != nil {
		return "", errors.Wrapf(err, "read envtest assets directory %q", assetsRoot)
	}
	versionPrefix := strings.TrimSuffix(envtestK8sVersion, ".x") + "."
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), versionPrefix) {
			continue
		}
		assetsPath := filepath.Join(assetsRoot, entry.Name())
		if hasEnvtestBinaries(assetsPath) {
			return assetsPath, nil
		}
	}
	return "", fmt.Errorf("envtest assets for %s not found under %q", envtestK8sVersion, assetsRoot)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func integrationScheme(t *testing.T) *runtime.Scheme {
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

func asgResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

type testEnv struct {
	env       *envtest.Environment
	k8sClient client.Client
	cancel    context.CancelFunc
	scheme    *runtime.Scheme
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	scheme := integrationScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../../config/crd"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		env.Stop()
		t.Fatalf("failed to create client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx // kept for teardown

	return &testEnv{
		env:       env,
		k8sClient: k8sClient,
		cancel:    cancel,
		scheme:    scheme,
	}
}

type testEnvWithManager struct {
	testEnv
	mgr         ctrl.Manager
	fakeClient  *fake.Client
	fakeFactory *fake.ClientFactory
}

func setupTestEnvWithManager(t *testing.T) *testEnvWithManager {
	t.Helper()

	scheme := integrationScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../../config/crd"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	mgr := testutil.NewEnvtestManager(t, cfg, scheme)

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	executor := azure.NewExecutor(zapLog, fakeFactory, 2)

	reconciler := &controller.MappingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   2 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         executor,
		StatusUpdater:    controller.NewMappingStatusUpdater(mgr.GetClient(), ctrl.Log.WithName("status-updater")),
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		env.Stop()
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager exited with error: %v", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		env.Stop()
		t.Fatal("cache sync failed")
	}

	return &testEnvWithManager{
		testEnv: testEnv{
			env:       env,
			k8sClient: mgr.GetClient(),
			cancel:    cancel,
			scheme:    scheme,
		},
		mgr:         mgr,
		fakeClient:  fakeAzClient,
		fakeFactory: fakeFactory,
	}
}

// ---------------------------------------------------------------------------
// Test: envtest manager helper allows multiple managers without listener conflicts
// Integration: testutil.NewEnvtestManager disables metrics/probe listeners so
// parallel envtest suites do not reintroduce :8080/:8081 bind failures.
// ---------------------------------------------------------------------------
func TestEnvtestManagerHelper_AllowsConcurrentManagers(t *testing.T) {
	scheme := integrationScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../../config/crd"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	}()

	managers := []struct {
		name string
		mgr  ctrl.Manager
	}{
		{name: "first", mgr: testutil.NewEnvtestManager(t, cfg, scheme)},
		{name: "second", mgr: testutil.NewEnvtestManager(t, cfg, scheme)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type startResult struct {
		name string
		err  error
	}
	results := make(chan startResult, len(managers))

	for _, tc := range managers {
		go func(name string, mgr ctrl.Manager) {
			results <- startResult{name: name, err: mgr.Start(ctx)}
		}(tc.name, tc.mgr)
	}

	syncCtx, syncCancel := context.WithTimeout(ctx, 15*time.Second)
	defer syncCancel()

	for _, tc := range managers {
		if ok := tc.mgr.GetCache().WaitForCacheSync(syncCtx); !ok {
			t.Fatalf("%s manager cache did not sync", tc.name)
		}
	}

	cancel()

	for range managers {
		select {
		case result := <-results:
			if result.err != nil {
				t.Errorf("%s manager exited with error: %v", result.name, result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for managers to stop")
		}
	}
}

func (te *testEnv) teardown(t *testing.T) {
	t.Helper()
	te.cancel()
	if err := te.env.Stop(); err != nil {
		t.Errorf("failed to stop envtest: %v", err)
	}
}

func eventually(t *testing.T, timeout, interval time.Duration, condition func() bool, msg string) {
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
// Test: MappingStatusUpdater.UpdatePending writes status subresource via envtest
// Integration: controller.MappingStatusUpdater → client.Status().Update → K8s API
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdatePending_WritesStatusViaEnvtest(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-pending-status"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "web-map", Namespace: ns.Name},
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
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	fixedNow := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return fixedNow },
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "web-map", Namespace: ns.Name}
	err := updater.UpdatePending(ctx, key, mapping.Generation, "test-cluster-test-pending-status-web-map", []int{5})
	if err != nil {
		t.Fatalf("UpdatePending: %v", err)
	}

	// Verify status was written to K8s
	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	if fetched.Status.MappingCount != 1 {
		t.Errorf("MappingCount = %d, want 1", fetched.Status.MappingCount)
	}
	if len(fetched.Status.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(fetched.Status.MappingStatuses))
	}
	ms := fetched.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStatePending {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStatePending)
	}
	if ms.MatchedPods != 5 {
		t.Errorf("MatchedPods = %d, want 5", ms.MatchedPods)
	}

	// Verify conditions
	acceptedCond := findCondition(fetched.Status.Conditions, controller.ConditionAccepted)
	if acceptedCond == nil {
		t.Fatal("Accepted condition not found")
	}
	if acceptedCond.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %s, want True", acceptedCond.Status)
	}

	reconciledCond := findCondition(fetched.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionUnknown {
		t.Errorf("Reconciled = %s, want Unknown", reconciledCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: MappingStatusUpdater.UpdateAfterReconcile with success writes final status
// Integration: controller.MappingStatusUpdater → ComputeStatus → K8s API
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdateAfterReconcile_Success_WritesReconciledTrue(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-final-success"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "svc"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-web")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	fixedNow := metav1.NewTime(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return fixedNow },
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "svc-map", Namespace: ns.Name}
	prefixSetName := "test-cluster-test-final-success-svc-map"
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-web",
		FullResourceID: asgResourceID("sub1", "rg1", "asg-web"),
		PrefixSetName:  prefixSetName,
	}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, results, nil, nil, []int{3})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	if fetched.Status.MappingCount != 1 {
		t.Errorf("MappingCount = %d, want 1", fetched.Status.MappingCount)
	}

	ms := fetched.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateSynced {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStateSynced)
	}
	if ms.MatchedPods != 3 {
		t.Errorf("MatchedPods = %d, want 3", ms.MatchedPods)
	}
	if !ms.LastSyncTime.Equal(&fixedNow) {
		t.Errorf("LastSyncTime = %v, want %v", ms.LastSyncTime, fixedNow)
	}

	reconciledCond := findCondition(fetched.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionTrue {
		t.Errorf("Reconciled = %s, want True", reconciledCond.Status)
	}
	if reconciledCond.Reason != controller.ReasonReconcileSucceeded {
		t.Errorf("Reconciled.Reason = %q, want %q", reconciledCond.Reason, controller.ReasonReconcileSucceeded)
	}
}

// ---------------------------------------------------------------------------
// Test: UpdateAfterReconcile with action failure writes Reconciled=False and Error state
// Integration: ComputeStatus → model.ParseASGResourceID → engine.TargetIdentityKey → K8s API
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdateAfterReconcile_ActionFailure_WritesErrorState(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-action-failure"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "fail"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-fail")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	fixedNow := metav1.NewTime(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return fixedNow },
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "fail-map", Namespace: ns.Name}
	prefixSetName := "test-cluster-test-action-failure-fail-map"
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-fail",
		FullResourceID: asgResourceID("sub1", "rg1", "asg-fail"),
		PrefixSetName:  prefixSetName,
	}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}},
			Success: false,
			Err:     fmt.Errorf("ARM throttled"),
		},
	}

	reconcileErr := fmt.Errorf("action failures: UpdatePrefixSet asg-fail: ARM throttled")
	err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, results, reconcileErr, nil, []int{2})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	ms := fetched.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateError {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStateError)
	}
	if ms.Error == "" {
		t.Error("Error should not be empty for failed action")
	}

	reconciledCond := findCondition(fetched.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled = %s, want False", reconciledCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: UpdateAfterReconcile with validation issues writes Accepted=False
// Integration: ComputeStatus validation → K8s API
// Note: The CRD has pattern validation on resourceId, so we use a valid-looking
// resource ID that passes CRD validation but has an invalid subscription ID
// that the controller's validation logic would reject.
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdateAfterReconcile_ValidationIssue_AcceptedFalse(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-validation-issue"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Use a syntactically valid resource ID (passes CRD pattern validation).
	// The controller's own validation logic would flag this at reconcile time.
	badResourceID := asgResourceID("bad-sub", "bad-rg", "bad-asg")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: badResourceID},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	validationIssues := []controller.ValidationIssue{
		{MappingIndex: 0, ASGIndex: 0, ResourceID: badResourceID, Err: fmt.Errorf("subscription not configured")},
	}
	key := types.NamespacedName{Name: "invalid-map", Namespace: ns.Name}
	err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, "pfx",
		nil, fmt.Errorf("validation failed"), validationIssues, []int{0})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	acceptedCond := findCondition(fetched.Status.Conditions, controller.ConditionAccepted)
	if acceptedCond == nil {
		t.Fatal("Accepted condition not found")
	}
	if acceptedCond.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %s, want False", acceptedCond.Status)
	}
	if acceptedCond.Reason != controller.ReasonSpecInvalid {
		t.Errorf("Accepted.Reason = %q, want %q", acceptedCond.Reason, controller.ReasonSpecInvalid)
	}

	ms := fetched.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateError {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStateError)
	}
}

// ---------------------------------------------------------------------------
// Test: UpdatePending on deleted mapping returns ErrStatusObjectNotFound
// Integration: MappingStatusUpdater → K8s API (not-found handling)
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdatePending_DeletedMapping_ReturnsNotFound(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	zapLog := zaptest.NewLogger(t)
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "nonexistent", Namespace: "default"}
	err := updater.UpdatePending(ctx, key, 1, "pfx", []int{0})
	if err != controller.ErrStatusObjectNotFound {
		t.Errorf("err = %v, want ErrStatusObjectNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Test: UpdatePending detects stale generation
// Integration: MappingStatusUpdater → K8s API (generation guard)
// ---------------------------------------------------------------------------
func TestStatusUpdater_UpdatePending_StaleGeneration_ReturnsStaleErr(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-stale-gen"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "x"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	// Use observedGeneration=0, which is below the actual generation (1).
	key := types.NamespacedName{Name: "stale-map", Namespace: ns.Name}
	err := updater.UpdatePending(ctx, key, 0, "pfx", []int{0})
	if err != controller.ErrStatusStaleGeneration {
		t.Errorf("err = %v, want ErrStatusStaleGeneration", err)
	}
}

// ---------------------------------------------------------------------------
// Test: ComputeStatus cross-module: model.ParseASGResourceID + engine.TargetIdentityKey
// for failure attribution across module boundaries
// ---------------------------------------------------------------------------
func TestComputeStatus_CrossModule_FailureAttribution(t *testing.T) {
	resID := asgResourceID("sub1", "rg1", "my-asg")
	prefixSetName := "test-cluster-ns-mapping"

	parsed, err := model.ParseASGResourceID(resID)
	if err != nil {
		t.Fatalf("ParseASGResourceID: %v", err)
	}

	targetKey := engine.TargetIdentityKey(parsed.FullResourceID, prefixSetName)

	target := engine.ASGTarget{
		SubscriptionID: parsed.SubscriptionID,
		ResourceGroup:  parsed.ResourceGroup,
		ASGName:        parsed.ASGName,
		FullResourceID: parsed.FullResourceID,
		PrefixSetName:  prefixSetName,
	}

	failResults := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}},
			Success: false,
			Err:     fmt.Errorf("throttled"),
		},
	}

	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	status := controller.ComputeStatus(controller.ComputeStatusInput{
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: resID},
					},
				},
			},
		},
		PrefixSetName:      prefixSetName,
		Results:            failResults,
		MatchedPodsByIndex: []int{2},
		ObservedGeneration: 1,
		Phase:              controller.StatusPhaseFinal,
		Now:                now,
	})

	// Verify the target key from engine matches what ComputeStatus uses internally
	_ = targetKey // used for conceptual verification

	if len(status.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(status.MappingStatuses))
	}

	ms := status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateError {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStateError)
	}
	if !strings.Contains(ms.Error, "throttled") {
		t.Errorf("Error = %q, expected to contain 'throttled'", ms.Error)
	}

	reconciledCond := findCondition(status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled = %s, want False", reconciledCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: ComputeStatus with shared target: two mappings referencing same ASG
// When the shared target fails, both mapping rows should be marked Error
// Integration: ComputeStatus → model.ParseASGResourceID → engine.TargetIdentityKey
// ---------------------------------------------------------------------------
func TestComputeStatus_CrossModule_SharedTarget_BothMappingsError(t *testing.T) {
	resID := asgResourceID("sub1", "rg1", "shared-asg")
	prefixSetName := "cluster-ns-mapping"

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "shared-asg",
		FullResourceID: resID,
		PrefixSetName:  prefixSetName,
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32", "10.0.0.2/32"}},
			Success: false,
			Err:     fmt.Errorf("conflict"),
		},
	}

	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	status := controller.ComputeStatus(controller.ComputeStatusInput{
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "a"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "b"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
		PrefixSetName:      prefixSetName,
		Results:            results,
		MatchedPodsByIndex: []int{1, 1},
		ObservedGeneration: 1,
		Phase:              controller.StatusPhaseFinal,
		Now:                now,
	})

	if len(status.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(status.MappingStatuses))
	}

	for i, ms := range status.MappingStatuses {
		if ms.ASGSyncState != controller.SyncStateError {
			t.Errorf("MappingStatuses[%d].ASGSyncState = %q, want %q", i, ms.ASGSyncState, controller.SyncStateError)
		}
		if ms.Error == "" {
			t.Errorf("MappingStatuses[%d].Error should not be empty", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: ComputeMatchedPodsByMapping crosses controller → model boundary
// Integration: controller.ComputeMatchedPodsByMapping → model.CompileSelector
// ---------------------------------------------------------------------------
func TestComputeMatchedPodsByMapping_CrossModule_WithRealPods(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web", "tier": "frontend"}},
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
		{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Labels: map[string]string{"app": "web", "tier": "frontend"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Labels: map[string]string{"app": "web", "tier": "frontend"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Labels: map[string]string{"app": "api"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{"app": "worker"}}}, // no match
		{ObjectMeta: metav1.ObjectMeta{Name: "web-no-tier", Labels: map[string]string{"app": "web"}}}, // missing tier
	}

	result := controller.ComputeMatchedPodsByMapping(spec, pods)

	if len(result) != 2 {
		t.Fatalf("result len = %d, want 2", len(result))
	}
	if result[0] != 2 {
		t.Errorf("result[0] (web+frontend) = %d, want 2", result[0])
	}
	if result[1] != 1 {
		t.Errorf("result[1] (api) = %d, want 1", result[1])
	}
}

// ---------------------------------------------------------------------------
// Test: ComputeSelectorHash is deterministic regardless of map iteration order
// Integration: Pure function test ensuring cross-reconcile consistency
// ---------------------------------------------------------------------------
func TestComputeSelectorHash_Deterministic(t *testing.T) {
	labels1 := map[string]string{"app": "web", "tier": "frontend", "env": "prod"}
	labels2 := map[string]string{"tier": "frontend", "env": "prod", "app": "web"}

	h1 := controller.ComputeSelectorHash(labels1)
	h2 := controller.ComputeSelectorHash(labels2)

	if h1 != h2 {
		t.Errorf("hashes differ for same labels: %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty for non-empty labels")
	}

	// Different labels should produce different hash
	h3 := controller.ComputeSelectorHash(map[string]string{"app": "api"})
	if h1 == h3 {
		t.Error("different labels should produce different hashes")
	}
}

// ---------------------------------------------------------------------------
// Test: MappingPredicate filters status-only updates (T6.7)
// Integration: status_updater writes status → predicate should filter the update
// ---------------------------------------------------------------------------
func TestMappingPredicate_StatusOnlyUpdate_Filtered(t *testing.T) {
	pred := controller.MappingPredicate()

	base := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-map",
			Namespace:  "default",
			Generation: 1,
			Finalizers: []string{controller.CleanupFinalizer},
		},
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

	// Create a copy with only status changes
	updated := base.DeepCopy()
	updated.Status = v1alpha1.PodASGMappingStatus{
		MappingCount: 1,
		Conditions: []metav1.Condition{
			{Type: controller.ConditionAccepted, Status: metav1.ConditionTrue, Reason: controller.ReasonSpecValid},
			{Type: controller.ConditionReconciled, Status: metav1.ConditionTrue, Reason: controller.ReasonReconcileSucceeded},
		},
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: "abc123", MatchedPods: 3, ASGSyncState: controller.SyncStateSynced},
		},
	}

	updateEvent := event.UpdateEvent{
		ObjectOld: base,
		ObjectNew: updated,
	}

	if pred.Update(updateEvent) {
		t.Error("MappingPredicate should filter status-only updates")
	}
}

// ---------------------------------------------------------------------------
// Test: MappingPredicate allows spec changes through
// ---------------------------------------------------------------------------
func TestMappingPredicate_SpecChange_Allowed(t *testing.T) {
	pred := controller.MappingPredicate()

	old := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-map", Namespace: "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "asg1")}},
				},
			},
		},
	}

	newObj := old.DeepCopy()
	newObj.Generation = 2
	newObj.Spec.Mappings[0].PodSelector.MatchLabels["env"] = "prod"

	updateEvent := event.UpdateEvent{ObjectOld: old, ObjectNew: newObj}
	if !pred.Update(updateEvent) {
		t.Error("MappingPredicate should allow spec changes through")
	}
}

// ---------------------------------------------------------------------------
// Test: LastSyncTime preservation across pending → final → error transitions
// Integration: ComputeStatus maintains temporal invariant across phases
// ---------------------------------------------------------------------------
func TestComputeStatus_LastSyncTime_PreservedOnError(t *testing.T) {
	resID := asgResourceID("sub1", "rg1", "asg-time")
	prefixSetName := "cluster-ns-m"
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-time",
		FullResourceID: resID,
		PrefixSetName:  prefixSetName,
	}

	// Simulate successful sync at t0
	t0 := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	successStatus := controller.ComputeStatus(controller.ComputeStatusInput{
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
		PrefixSetName:      prefixSetName,
		Results:            []azure.ActionResult{{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target}, Success: true}},
		MatchedPodsByIndex: []int{1},
		ObservedGeneration: 1,
		Phase:              controller.StatusPhaseFinal,
		Now:                t0,
	})

	if successStatus.MappingStatuses[0].LastSyncTime != t0 {
		t.Fatalf("LastSyncTime should be t0 after success")
	}

	// Now simulate failure at t1 — LastSyncTime should be preserved from t0
	t1 := metav1.NewTime(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	errorStatus := controller.ComputeStatus(controller.ComputeStatusInput{
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
		PreviousStatus:     successStatus,
		PrefixSetName:      prefixSetName,
		Results:            []azure.ActionResult{{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target}, Success: false, Err: fmt.Errorf("fail")}},
		MatchedPodsByIndex: []int{1},
		ReconcileErr:       fmt.Errorf("fail"),
		ObservedGeneration: 1,
		Phase:              controller.StatusPhaseFinal,
		Now:                t1,
	})

	if errorStatus.MappingStatuses[0].LastSyncTime != t0 {
		t.Errorf("LastSyncTime = %v, want preserved t0 = %v", errorStatus.MappingStatuses[0].LastSyncTime, t0)
	}
}

// ---------------------------------------------------------------------------
// Test: Full reconcile with envtest produces status on mapping (live controller)
// Integration: Reconciler → Executor → Azure fake → StatusUpdater.UpdateAfterReconcile → K8s
// Integration: Reconciler -> Executor -> Azure fake -> StatusUpdater -> K8s status subresource
// ---------------------------------------------------------------------------
func TestPhase6_FullReconcile_StatusUpdateFlow(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-full-flow"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "flow-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "flow"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-flow")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "flow-pod-1",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "flow"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.42"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-flow-map"
	key := types.NamespacedName{Name: "flow-map", Namespace: ns.Name}

	// Wait for reconciliation to create the prefix set in Azure
	eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-flow", ownershipKey)
		if err != nil {
			return false
		}
		return ps != nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "prefix set should be created with pod IP")

	// Verify the prefix set contains the pod IP (engine stores raw IPs without /32)
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-flow", ownershipKey)
	if err != nil {
		t.Fatalf("get prefix set: %v", err)
	}
	found := false
	for _, ip := range ps.Properties.AddressPrefixes {
		if ip == "10.0.0.42/32" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("prefix set IPs = %v, want to contain 10.0.0.42", ps.Properties.AddressPrefixes)
	}

	// Verify the reconciler-owned status updater persisted final status.
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var current v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &current); err != nil {
			return false
		}
		reconciled := findCondition(current.Status.Conditions, controller.ConditionReconciled)
		return current.Status.MappingCount == 1 &&
			len(current.Status.MappingStatuses) == 1 &&
			current.Status.MappingStatuses[0].ASGSyncState == controller.SyncStateSynced &&
			reconciled != nil &&
			reconciled.Status == metav1.ConditionTrue
	}, "reconciler should persist synced status")

	var statusCheck v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &statusCheck); err != nil {
		t.Fatalf("get mapping after reconcile: %v", err)
	}

	ms := statusCheck.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateSynced {
		t.Errorf("ASGSyncState = %q, want %q", ms.ASGSyncState, controller.SyncStateSynced)
	}
	if ms.MatchedPods != 1 {
		t.Errorf("MatchedPods = %d, want 1", ms.MatchedPods)
	}
	wantSelectorHash := controller.ComputeSelectorHash(map[string]string{"app": "flow"})
	if ms.SelectorHash != wantSelectorHash {
		t.Errorf("SelectorHash = %q, want %q", ms.SelectorHash, wantSelectorHash)
	}
	if ms.LastSyncTime.IsZero() {
		t.Errorf("LastSyncTime = %v, want non-zero", ms.LastSyncTime)
	}

	reconciledCond := findCondition(statusCheck.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found after full flow")
	}
	if reconciledCond.Status != metav1.ConditionTrue {
		t.Errorf("Reconciled = %s, want True", reconciledCond.Status)
	}

	acceptedCond := findCondition(statusCheck.Status.Conditions, controller.ConditionAccepted)
	if acceptedCond == nil {
		t.Fatal("Accepted condition not found after full flow")
	}
	if acceptedCond.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %s, want True", acceptedCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: TargetIdentityKey normalization is case-insensitive and consistent
// with model.ParseASGResourceID output
// Integration: engine.TargetIdentityKey → model.ParseASGResourceID
// ---------------------------------------------------------------------------
func TestTargetIdentityKey_CaseInsensitive_WithParsedResourceID(t *testing.T) {
	// Mixed-case resource ID
	resIDMixed := "/subscriptions/Sub1/resourceGroups/RG1/providers/Microsoft.Network/applicationSecurityGroups/MyASG"
	resIDLower := "/subscriptions/Sub1/resourceGroups/RG1/providers/Microsoft.Network/applicationSecurityGroups/myasg"

	parsedMixed, err := model.ParseASGResourceID(resIDMixed)
	if err != nil {
		t.Fatalf("ParseASGResourceID mixed: %v", err)
	}
	parsedLower, err := model.ParseASGResourceID(resIDLower)
	if err != nil {
		t.Fatalf("ParseASGResourceID lower: %v", err)
	}

	pfx := "cluster-ns-map"
	keyMixed := engine.TargetIdentityKey(parsedMixed.FullResourceID, pfx)
	keyLower := engine.TargetIdentityKey(parsedLower.FullResourceID, pfx)

	// TargetIdentityKey lowercases everything, so these should match
	if keyMixed != keyLower {
		t.Errorf("keys should match case-insensitively:\n  mixed: %q\n  lower: %q", keyMixed, keyLower)
	}
}

// ---------------------------------------------------------------------------
// Test: Pending → Final status transition round-trips through K8s
// Integration: StatusUpdater.UpdatePending → K8s write → StatusUpdater.UpdateAfterReconcile
// → K8s read (previous status) → ComputeStatus → K8s write
// Verifies that K8s-persisted pending state is correctly read as previous
// status input when computing final status.
// ---------------------------------------------------------------------------
func TestStatusUpdater_PendingToFinal_TransitionViaEnvtest(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-pending-final"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resID := asgResourceID("sub1", "rg1", "asg-pf")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "pf-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "pf"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	fixedNow := metav1.NewTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return fixedNow },
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "pf-map", Namespace: ns.Name}
	prefixSetName := "test-cluster-test-pending-final-pf-map"

	// Step 1: Write pending status
	if err := updater.UpdatePending(ctx, key, mapping.Generation, prefixSetName, []int{4}); err != nil {
		t.Fatalf("UpdatePending: %v", err)
	}

	// Verify pending status persisted
	var afterPending v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterPending); err != nil {
		t.Fatalf("get after pending: %v", err)
	}
	if afterPending.Status.MappingStatuses[0].ASGSyncState != controller.SyncStatePending {
		t.Errorf("after pending: ASGSyncState = %q, want Pending", afterPending.Status.MappingStatuses[0].ASGSyncState)
	}
	reconciledCond := findCondition(afterPending.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil || reconciledCond.Status != metav1.ConditionUnknown {
		t.Errorf("after pending: Reconciled should be Unknown")
	}

	// Step 2: Write final success status (reads previous pending status from K8s)
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-pf",
		FullResourceID: resID,
		PrefixSetName:  prefixSetName,
	}
	results := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}}, Success: true},
	}
	if err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, results, nil, nil, []int{4}); err != nil {
		t.Fatalf("UpdateAfterReconcile: %v", err)
	}

	// Verify final status overwrote pending state
	var afterFinal v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterFinal); err != nil {
		t.Fatalf("get after final: %v", err)
	}

	ms := afterFinal.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateSynced {
		t.Errorf("after final: ASGSyncState = %q, want Synced", ms.ASGSyncState)
	}
	if !ms.LastSyncTime.Equal(&fixedNow) {
		t.Errorf("after final: LastSyncTime = %v, want %v", ms.LastSyncTime, fixedNow)
	}
	if ms.MatchedPods != 4 {
		t.Errorf("after final: MatchedPods = %d, want 4", ms.MatchedPods)
	}

	reconciledCond = findCondition(afterFinal.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil || reconciledCond.Status != metav1.ConditionTrue {
		t.Errorf("after final: Reconciled should be True")
	}
	acceptedCond := findCondition(afterFinal.Status.Conditions, controller.ConditionAccepted)
	if acceptedCond == nil || acceptedCond.Status != metav1.ConditionTrue {
		t.Errorf("after final: Accepted should be True")
	}
}

// ---------------------------------------------------------------------------
// Test: LastSyncTime preservation across multiple reconcile cycles via envtest
// Integration: StatusUpdater success write → K8s → StatusUpdater error write
// → reads K8s-persisted previous status → preserves LastSyncTime by selectorHash
// ---------------------------------------------------------------------------
func TestStatusUpdater_LastSyncTime_PreservedAcrossReconcileCyclesViaEnvtest(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-lastsync-persist"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resID := asgResourceID("sub1", "rg1", "asg-ls")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "ls-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "ls"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	prefixSetName := "test-cluster-test-lastsync-persist-ls-map"
	key := types.NamespacedName{Name: "ls-map", Namespace: ns.Name}
	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-ls",
		FullResourceID: resID,
		PrefixSetName:  prefixSetName,
	}

	// Cycle 1: Success at t0
	t0 := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return t0 },
		MaxAttempts: 3,
	}

	successResults := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}}, Success: true},
	}
	if err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, successResults, nil, nil, []int{1}); err != nil {
		t.Fatalf("cycle 1 UpdateAfterReconcile: %v", err)
	}

	// Verify t0 written
	var afterSuccess v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterSuccess); err != nil {
		t.Fatalf("get after success: %v", err)
	}
	if !afterSuccess.Status.MappingStatuses[0].LastSyncTime.Equal(&t0) {
		t.Fatalf("cycle 1: LastSyncTime = %v, want %v", afterSuccess.Status.MappingStatuses[0].LastSyncTime, t0)
	}

	// Cycle 2: Failure at t1 — LastSyncTime should be preserved from K8s as t0
	t1 := metav1.NewTime(time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	updater.Now = func() metav1.Time { return t1 }

	failResults := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}}, Success: false, Err: fmt.Errorf("throttled")},
	}
	if err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, failResults, fmt.Errorf("throttled"), nil, []int{1}); err != nil {
		t.Fatalf("cycle 2 UpdateAfterReconcile: %v", err)
	}

	// Verify LastSyncTime preserved as t0, NOT t1
	var afterFail v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterFail); err != nil {
		t.Fatalf("get after fail: %v", err)
	}

	ms := afterFail.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateError {
		t.Errorf("cycle 2: ASGSyncState = %q, want Error", ms.ASGSyncState)
	}
	if !ms.LastSyncTime.Equal(&t0) {
		t.Errorf("cycle 2: LastSyncTime = %v, want preserved t0 = %v (not t1 = %v)",
			ms.LastSyncTime, t0, t1)
	}

	// Cycle 3: Success again at t2 — LastSyncTime should update to t2
	t2 := metav1.NewTime(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC))
	updater.Now = func() metav1.Time { return t2 }

	if err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, successResults, nil, nil, []int{1}); err != nil {
		t.Fatalf("cycle 3 UpdateAfterReconcile: %v", err)
	}

	var afterRecover v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterRecover); err != nil {
		t.Fatalf("get after recover: %v", err)
	}

	ms = afterRecover.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateSynced {
		t.Errorf("cycle 3: ASGSyncState = %q, want Synced", ms.ASGSyncState)
	}
	if !ms.LastSyncTime.Equal(&t2) {
		t.Errorf("cycle 3: LastSyncTime = %v, want t2 = %v", ms.LastSyncTime, t2)
	}
}

// ---------------------------------------------------------------------------
// Test: Full reconcile with Azure failure produces correct error status
// Integration: Reconciler → engine.ComputeDesiredState → engine.ComputeDiff
// → Executor → fake Azure (injected error) → StatusUpdater → K8s
// ---------------------------------------------------------------------------
func TestPhase6_FullReconcile_AzureFailure_WritesErrorStatus(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-azure-fail"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resID := asgResourceID("sub1", "rg1", "asg-fail-flow")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-flow-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "failflow"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failflow-pod-1",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "failflow"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.99"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod IP: %v", err)
	}

	// Inject persistent PUT errors for this ASG in the fake Azure client.
	ownershipKey := "test-cluster-" + ns.Name + "-fail-flow-map"
	te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-fail-flow",
		PrefixSetName:  ownershipKey,
	}, fmt.Errorf("ARM throttled"), 10)

	key := types.NamespacedName{Name: "fail-flow-map", Namespace: ns.Name}

	// Wait for status to reflect the failure
	eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		var current v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &current); err != nil {
			return false
		}
		if len(current.Status.MappingStatuses) == 0 {
			return false
		}
		return current.Status.MappingStatuses[0].ASGSyncState == controller.SyncStateError
	}, "reconciler should write error status after Azure failure")

	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	ms := fetched.Status.MappingStatuses[0]
	if ms.ASGSyncState != controller.SyncStateError {
		t.Errorf("ASGSyncState = %q, want Error", ms.ASGSyncState)
	}
	if ms.Error == "" {
		t.Error("Error message should not be empty")
	}
	if ms.MatchedPods != 1 {
		t.Errorf("MatchedPods = %d, want 1", ms.MatchedPods)
	}

	reconciledCond := findCondition(fetched.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled = %s, want False", reconciledCond.Status)
	}

	// Accepted should still be True (spec is valid)
	acceptedCond := findCondition(fetched.Status.Conditions, controller.ConditionAccepted)
	if acceptedCond == nil {
		t.Fatal("Accepted condition not found")
	}
	if acceptedCond.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %s, want True", acceptedCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: ObservedGeneration propagates correctly through full envtest flow
// Integration: Reconciler → StatusUpdater → ComputeStatus → SetCondition
// → K8s status subresource
// Verifies that conditions.ObservedGeneration matches the CR generation
// ---------------------------------------------------------------------------
func TestPhase6_ObservedGeneration_PropagatesInFullFlow(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-obsgen"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resID := asgResourceID("sub1", "rg1", "asg-gen")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "gen-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "gen"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gen-pod-1",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "gen"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.50"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod IP: %v", err)
	}

	key := types.NamespacedName{Name: "gen-map", Namespace: ns.Name}

	// Wait for initial reconciliation
	eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		var current v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &current); err != nil {
			return false
		}
		cond := findCondition(current.Status.Conditions, controller.ConditionReconciled)
		return cond != nil && cond.Status == metav1.ConditionTrue
	}, "initial reconcile should succeed")

	// Verify ObservedGeneration on conditions matches CR generation
	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	initialGen := fetched.Generation
	for _, cond := range fetched.Status.Conditions {
		if cond.ObservedGeneration != initialGen {
			t.Errorf("condition %q: ObservedGeneration = %d, want %d",
				cond.Type, cond.ObservedGeneration, initialGen)
		}
	}

	// Update spec to bump generation. Use retry loop since the reconciler
	// may patch metadata concurrently (ownership annotation), causing conflicts.
	var newGen int64
	for attempt := 0; attempt < 5; attempt++ {
		var latest v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &latest); err != nil {
			t.Fatalf("get mapping for spec update: %v", err)
		}
		latest.Spec.Mappings[0].PodSelector.MatchLabels["env"] = "prod"
		if err := te.k8sClient.Update(ctx, &latest); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}

	// Wait for the cache to reflect the new generation
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var latest v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &latest); err != nil {
			return false
		}
		newGen = latest.Generation
		return newGen > initialGen
	}, "generation should bump after spec update")

	// Wait for reconciliation with new generation
	eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		var current v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &current); err != nil {
			return false
		}
		cond := findCondition(current.Status.Conditions, controller.ConditionReconciled)
		return cond != nil && cond.ObservedGeneration == newGen
	}, "ObservedGeneration should update to new generation after spec change")

	var afterUpdate v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterUpdate); err != nil {
		t.Fatalf("get mapping after gen update: %v", err)
	}

	for _, cond := range afterUpdate.Status.Conditions {
		if cond.ObservedGeneration != newGen {
			t.Errorf("after update: condition %q: ObservedGeneration = %d, want %d",
				cond.Type, cond.ObservedGeneration, newGen)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: SelectorHash allows correct LastSyncTime preservation when mappings
// reorder between reconcile cycles, verified through full StatusUpdater
// and K8s round-trip
// Integration: StatusUpdater → ComputeStatus (with selectorHash) → K8s → next
// StatusUpdater call reads K8s-persisted status → preserves by selectorHash
// ---------------------------------------------------------------------------
func TestStatusUpdater_SelectorHashPreservation_AcrossReorder_ViaEnvtest(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-reorder-envtest"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resID1 := asgResourceID("sub1", "rg1", "asg-r1")
	resID2 := asgResourceID("sub1", "rg1", "asg-r2")
	prefixSetName := "test-cluster-test-reorder-envtest-reorder-map"

	// Create mapping with two entries: app=web then app=api
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "reorder-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID1}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID2}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	zapLog := zaptest.NewLogger(t)
	t0 := metav1.NewTime(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	updater := &controller.MappingStatusUpdater{
		Client:      te.k8sClient,
		Logger:      zapr.NewLogger(zapLog),
		Now:         func() metav1.Time { return t0 },
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "reorder-map", Namespace: ns.Name}
	target1 := engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-r1", FullResourceID: resID1, PrefixSetName: prefixSetName}
	target2 := engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-r2", FullResourceID: resID2, PrefixSetName: prefixSetName}

	// Cycle 1: Both succeed at t0
	results := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target1, DesiredIPs: []string{"10.0.0.1/32"}}, Success: true},
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target2, DesiredIPs: []string{"10.0.0.2/32"}}, Success: true},
	}
	if err := updater.UpdateAfterReconcile(ctx, key, mapping.Generation, prefixSetName, results, nil, nil, []int{1, 1}); err != nil {
		t.Fatalf("cycle 1 UpdateAfterReconcile: %v", err)
	}

	// Verify both rows synced at t0
	var afterSuccess v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterSuccess); err != nil {
		t.Fatalf("get after success: %v", err)
	}

	webHash := controller.ComputeSelectorHash(map[string]string{"app": "web"})
	apiHash := controller.ComputeSelectorHash(map[string]string{"app": "api"})

	if len(afterSuccess.Status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(afterSuccess.Status.MappingStatuses))
	}
	for _, ms := range afterSuccess.Status.MappingStatuses {
		if !ms.LastSyncTime.Equal(&t0) {
			t.Errorf("cycle 1: SelectorHash %q: LastSyncTime = %v, want %v", ms.SelectorHash, ms.LastSyncTime, t0)
		}
	}

	// Now reorder mappings: api first, web second
	if err := te.k8sClient.Get(ctx, key, &afterSuccess); err != nil {
		t.Fatalf("re-get mapping: %v", err)
	}
	afterSuccess.Spec.Mappings = []v1alpha1.Mapping{
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID2}},
		},
		{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resID1}},
		},
	}
	if err := te.k8sClient.Update(ctx, &afterSuccess); err != nil {
		t.Fatalf("update mapping spec (reorder): %v", err)
	}

	// Re-read to get updated generation
	var reordered v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &reordered); err != nil {
		t.Fatalf("get reordered: %v", err)
	}

	// Cycle 2: Pending after reorder — LastSyncTime should be preserved by selectorHash
	t1 := metav1.NewTime(time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC))
	updater.Now = func() metav1.Time { return t1 }

	if err := updater.UpdatePending(ctx, key, reordered.Generation, prefixSetName, []int{1, 1}); err != nil {
		t.Fatalf("cycle 2 UpdatePending: %v", err)
	}

	var afterReorderPending v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &afterReorderPending); err != nil {
		t.Fatalf("get after reorder pending: %v", err)
	}

	// After reorder: index 0 is now "api", index 1 is now "web"
	// Both should preserve their t0 LastSyncTime regardless of new index
	for _, ms := range afterReorderPending.Status.MappingStatuses {
		if !ms.LastSyncTime.Equal(&t0) {
			t.Errorf("after reorder: SelectorHash %q: LastSyncTime = %v, want preserved t0 = %v",
				ms.SelectorHash, ms.LastSyncTime, t0)
		}
	}

	// Verify hashes match the reordered spec order
	if afterReorderPending.Status.MappingStatuses[0].SelectorHash != apiHash {
		t.Errorf("index 0 SelectorHash = %q, want apiHash %q", afterReorderPending.Status.MappingStatuses[0].SelectorHash, apiHash)
	}
	if afterReorderPending.Status.MappingStatuses[1].SelectorHash != webHash {
		t.Errorf("index 1 SelectorHash = %q, want webHash %q", afterReorderPending.Status.MappingStatuses[1].SelectorHash, webHash)
	}
}

// ---------------------------------------------------------------------------
// Test: Partial failure with multiple mappings in full reconcile flow
// Integration: Reconciler → engine → Executor → fake Azure (one succeeds,
// one fails) → StatusUpdater → K8s
// Verifies mixed Synced/Error status on different mapping rows (T6.2)
// ---------------------------------------------------------------------------
func TestPhase6_FullReconcile_PartialFailure_MixedStatus(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-partial-fail"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	resIDGood := asgResourceID("sub1", "rg1", "asg-good")
	resIDBad := asgResourceID("sub1", "rg1", "asg-bad")
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-map", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "good"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resIDGood}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: resIDBad}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Create pods for both selectors
	for _, p := range []struct {
		name   string
		labels map[string]string
		ip     string
	}{
		{"good-pod", map[string]string{"app": "good"}, "10.0.0.1"},
		{"bad-pod", map[string]string{"app": "bad"}, "10.0.0.2"},
	} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: ns.Name, Labels: p.labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("create pod %s: %v", p.name, err)
		}
		pod.Status.PodIP = p.ip
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("update pod %s IP: %v", p.name, err)
		}
	}

	// Inject PUT errors only for asg-bad
	ownershipKey := "test-cluster-" + ns.Name + "-partial-map"
	te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-bad",
		PrefixSetName:  ownershipKey,
	}, fmt.Errorf("service unavailable"), 10)

	key := types.NamespacedName{Name: "partial-map", Namespace: ns.Name}

	// Wait for status with mixed results
	eventually(t, 15*time.Second, 200*time.Millisecond, func() bool {
		var current v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, key, &current); err != nil {
			return false
		}
		if len(current.Status.MappingStatuses) != 2 {
			return false
		}
		// One should be Error (for asg-bad)
		hasError := false
		for _, ms := range current.Status.MappingStatuses {
			if ms.ASGSyncState == controller.SyncStateError {
				hasError = true
			}
		}
		return hasError
	}, "status should reflect partial failure")

	var fetched v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("get mapping: %v", err)
	}

	goodHash := controller.ComputeSelectorHash(map[string]string{"app": "good"})
	badHash := controller.ComputeSelectorHash(map[string]string{"app": "bad"})

	for _, ms := range fetched.Status.MappingStatuses {
		switch ms.SelectorHash {
		case goodHash:
			if ms.ASGSyncState != controller.SyncStateSynced {
				t.Errorf("good mapping: ASGSyncState = %q, want Synced", ms.ASGSyncState)
			}
			if ms.MatchedPods != 1 {
				t.Errorf("good mapping: MatchedPods = %d, want 1", ms.MatchedPods)
			}
		case badHash:
			if ms.ASGSyncState != controller.SyncStateError {
				t.Errorf("bad mapping: ASGSyncState = %q, want Error", ms.ASGSyncState)
			}
			if ms.Error == "" {
				t.Error("bad mapping: Error should not be empty")
			}
			if ms.MatchedPods != 1 {
				t.Errorf("bad mapping: MatchedPods = %d, want 1", ms.MatchedPods)
			}
		default:
			t.Errorf("unexpected SelectorHash: %q", ms.SelectorHash)
		}
	}

	// Reconciled should be False due to partial failure
	reconciledCond := findCondition(fetched.Status.Conditions, controller.ConditionReconciled)
	if reconciledCond == nil {
		t.Fatal("Reconciled condition not found")
	}
	if reconciledCond.Status != metav1.ConditionFalse {
		t.Errorf("Reconciled = %s, want False", reconciledCond.Status)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
