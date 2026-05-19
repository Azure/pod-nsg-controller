package phase8_test

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
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	"github.com/Azure/pod-nsg-controller/test/integration/testutil"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const envtestK8sVersion = "1.31.x"

// TestMain configures KUBEBUILDER_ASSETS for envtest before running tests.
func TestMain(m *testing.M) {
	repoRoot, err := repoRootFromCWD()
	if err != nil {
		os.Stderr.WriteString("failed to resolve repo root: " + err.Error() + "\n")
		os.Exit(1)
	}

	if err := configureEnvtestAssets(repoRoot); err != nil {
		os.Stderr.WriteString("failed to configure envtest assets: " + err.Error() + "\n")
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func repoRootFromCWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", "..")), nil
}

func configureEnvtestAssets(repoRoot string) error {
	if assets := strings.TrimSpace(os.Getenv("KUBEBUILDER_ASSETS")); assets != "" {
		if _, err := os.Stat(assets); err != nil {
			return fmt.Errorf("stat KUBEBUILDER_ASSETS %q: %w", assets, err)
		}
		return nil
	}
	assetsPath, err := localEnvtestAssetsPath(repoRoot)
	if err != nil {
		return err
	}
	return os.Setenv("KUBEBUILDER_ASSETS", assetsPath)
}

func localEnvtestAssetsPath(repoRoot string) (string, error) {
	assetsRoot := filepath.Join(repoRoot, "bin", "k8s")
	entries, err := os.ReadDir(assetsRoot)
	if err != nil {
		return "", fmt.Errorf("read envtest assets directory %q: %w", assetsRoot, err)
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

func hasEnvtestBinaries(assetsPath string) bool {
	for _, binary := range []string{"etcd", "kube-apiserver", "kubectl"} {
		if _, err := os.Stat(filepath.Join(assetsPath, binary)); err != nil {
			return false
		}
	}
	return true
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

type metricsEnvtestEnv struct {
	env         *envtest.Environment
	k8sClient   client.Client
	cancel      context.CancelFunc
	scheme      *runtime.Scheme
	mgr         ctrl.Manager
	fakeClient  *fake.Client
	fakeFactory *fake.ClientFactory
	registry    *prometheus.Registry
	recorder    *metrics.Recorder
}

func setupMetricsEnvtest(t *testing.T) *metricsEnvtestEnv {
	t.Helper()

	metrics.ResetForTesting()

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

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		env.Stop()
		t.Fatalf("metrics RegisterWith failed: %v", err)
	}

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	executor := azure.NewExecutor(zapLog, fakeFactory, 5)

	statusUpdater := controller.NewMappingStatusUpdater(mgr.GetClient(), ctrl.Log.WithName("status-updater"))

	controllerStart := time.Now()
	initialTracker := metrics.NewInitialReconcileTracker(controllerStart)
	podChurnTracker := metrics.NewPodChurnTracker()
	convergenceTracker := metrics.NewConvergenceTracker()

	// Wire convergence committer: the status updater invokes this callback after
	// each status resolution to commit staged convergence measurements.
	statusUpdater.SetConvergenceCommitter(func(
		key types.NamespacedName,
		observedGeneration int64,
		results []azure.ActionResult,
		outcome controller.StatusWriteOutcome,
		statusErr error,
	) {
		if outcome == controller.StatusWriteOutcomeError {
			return
		}
		for _, res := range results {
			if res.Success {
				convergenceTracker.CommitConvergence(rec.Convergence, key, res.Action.Target, observedGeneration)
			}
		}
	})

	reconciler := &controller.MappingReconciler{
		Client:             mgr.GetClient(),
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           executor,
		StatusUpdater:      statusUpdater,
		MetricsRecorder:    rec,
		PodChurnTracker:    podChurnTracker,
		ConvergenceTracker: convergenceTracker,
		InitialTracker:     initialTracker,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		env.Stop()
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	// Wire InitialReconcileInitializer to match production wiring (T8.11).
	initializer := controller.NewInitialReconcileInitializer(
		mgr.GetClient(),
		initialTracker,
		rec.Reconcile,
		ctrl.Log.WithName("initial-reconcile"),
	)
	if err := mgr.Add(initializer); err != nil {
		env.Stop()
		t.Fatalf("failed to add initial reconcile initializer: %v", err)
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

	return &metricsEnvtestEnv{
		env:         env,
		k8sClient:   mgr.GetClient(),
		cancel:      cancel,
		scheme:      scheme,
		mgr:         mgr,
		fakeClient:  fakeAzClient,
		fakeFactory: fakeFactory,
		registry:    reg,
		recorder:    rec,
	}
}

func teardownMetricsEnv(t *testing.T, te *metricsEnvtestEnv) {
	t.Helper()
	te.cancel()
	if err := te.env.Stop(); err != nil {
		t.Errorf("failed to stop envtest: %v", err)
	}
	metrics.ResetForTesting()
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// gatherMetricValue finds a specific counter metric value from gathered families.
func gatherCounterValue(mfs []*dto.MetricFamily, name string, labelFilter map[string]string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labelFilter) && m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gatherHistogramCount(mfs []*dto.MetricFamily, name string, labelFilter map[string]string) uint64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labelFilter) && m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func gatherGaugeValue(mfs []*dto.MetricFamily, name string, labelFilter map[string]string) (float64, bool) {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labelFilter) && m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func metricFamilyExists(mfs []*dto.MetricFamily, name string) bool {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return true
		}
	}
	return false
}

func sumCounterTotal(mfs []*dto.MetricFamily, name string) float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var total float64
		for _, m := range mf.GetMetric() {
			if m.GetCounter() != nil {
				total += m.GetCounter().GetValue()
			}
		}
		return total
	}
	return 0
}

func matchLabels(m *dto.Metric, filter map[string]string) bool {
	if len(filter) == 0 {
		return true
	}
	labels := make(map[string]string)
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Test 1: Full Reconcile Metrics Pipeline (envtest)
//
// Integration: MappingReconciler + engine + fake Azure + metrics + envtest
//
// Verifies that a full reconcile cycle with real K8s objects and fake Azure
// emits the complete set of Phase 8 metrics through the prometheus registry.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_FullReconcile_EmitsMetricsPipeline(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "metrics-integ-ns"

	// Create namespace
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Create mapping
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "integ-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "metrics-test"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-integ")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Create pod matching selector
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-integ-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "metrics-test"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	// Patch pod status with an IP
	pod.Status.PodIP = "10.0.0.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for reconcile to emit metrics
	waitForCondition(t, 30*time.Second, "reconcile_total metric emitted", func() bool {
		mfs, err := te.registry.Gather()
		if err != nil {
			return false
		}
		return sumCounterTotal(mfs, "pod_nsg_controller_reconcile_total") >= 1
	})

	// Gather final metrics
	mfs, err := te.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() failed: %v", err)
	}

	// T8.9: reconcile_total and reconcile_duration_seconds
	if !metricFamilyExists(mfs, "pod_nsg_controller_reconcile_total") {
		t.Error("reconcile_total not emitted")
	}
	if !metricFamilyExists(mfs, "pod_nsg_controller_reconcile_duration_seconds") {
		t.Error("reconcile_duration_seconds not emitted")
	}

	// T8.1: pod_ip_changes_total (pods were added)
	waitForCondition(t, 10*time.Second, "pod_ip_changes_total emitted", func() bool {
		mfs, _ = te.registry.Gather()
		return sumCounterTotal(mfs, "pod_nsg_controller_pod_ip_changes_total") > 0
	})
	addCount := gatherCounterValue(mfs, "pod_nsg_controller_pod_ip_changes_total",
		map[string]string{"operation": "add", "namespace": ns, "mapping": "integ-mapping"})
	if addCount < 1 {
		t.Errorf("pod_ip_changes_total{operation=add} = %v, want >= 1", addCount)
	}

	// T8.12: crd_resolution_duration_seconds emitted
	if !metricFamilyExists(mfs, "pod_nsg_controller_crd_resolution_duration_seconds") {
		t.Error("crd_resolution_duration_seconds not emitted")
	}

	// reconcile_actions_per_cycle should show at least 1 action
	if !metricFamilyExists(mfs, "pod_nsg_controller_reconcile_actions_per_cycle") {
		t.Error("reconcile_actions_per_cycle not emitted")
	}

	// prefix_set_actions_total should show create action
	if !metricFamilyExists(mfs, "pod_nsg_controller_prefix_set_actions_total") {
		t.Error("prefix_set_actions_total not emitted")
	}
}

// ---------------------------------------------------------------------------
// Test 2: Pod Churn Across Multiple Reconciles (envtest)
//
// Integration: Pod watch → Reconcile trigger → engine.ComputeDesiredStateWithSnapshot
// → PodChurnTracker → PodChurnRecorder
//
// Verifies that adding/deleting pods through the real K8s API results in
// correct pod churn counters accumulating across reconcile cycles.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_PodChurn_MultipleReconciles(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "churn-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "churn-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "churn"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-churn")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Create 3 initial pods
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("churn-pod-%d", i),
				Namespace: ns,
				Labels:    map[string]string{"app": "churn"},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
			},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("create pod %d: %v", i, err)
		}
		pod.Status.PodIP = fmt.Sprintf("10.0.0.%d", i+1)
		pod.Status.PodIPs = []corev1.PodIP{{IP: pod.Status.PodIP}}
		pod.Status.Phase = corev1.PodRunning
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("update pod %d status: %v", i, err)
		}
	}

	// Wait for initial pod adds to be recorded
	waitForCondition(t, 30*time.Second, "pod_ip_changes_total{add} >= 3", func() bool {
		mfs, _ := te.registry.Gather()
		v := gatherCounterValue(mfs, "pod_nsg_controller_pod_ip_changes_total",
			map[string]string{"operation": "add", "namespace": ns, "mapping": "churn-mapping"})
		return v >= 3
	})

	// Delete 1 pod
	podToDelete := &corev1.Pod{}
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "churn-pod-0"}, podToDelete); err != nil {
		t.Fatalf("get pod to delete: %v", err)
	}
	if err := te.k8sClient.Delete(ctx, podToDelete); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	// Wait for delete to be reflected in metrics
	waitForCondition(t, 30*time.Second, "pod_ip_changes_total{delete} >= 1", func() bool {
		mfs, _ := te.registry.Gather()
		v := gatherCounterValue(mfs, "pod_nsg_controller_pod_ip_changes_total",
			map[string]string{"operation": "delete", "namespace": ns, "mapping": "churn-mapping"})
		return v >= 1
	})

	// Verify churn rate gauge exists (T8.13)
	mfs, _ := te.registry.Gather()
	if _, found := gatherGaugeValue(mfs, "pod_nsg_controller_pod_churn_rate",
		map[string]string{"namespace": ns, "mapping": "churn-mapping"}); !found {
		t.Error("pod_churn_rate gauge not emitted for churn-mapping")
	}
}

// ---------------------------------------------------------------------------
// Test 3: Convergence Tracking End-to-End (envtest)
//
// Integration: Reconciler → engine diff → ConvergenceTracker.StartOrKeep →
// Executor.Execute → ConvergenceTracker.StageSuccessful →
// ConvergenceTracker.CommitConvergence → ConvergenceRecorder histogram
//
// Verifies that convergence duration is measured from detection to ARM success.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_Convergence_DetectionToARMSuccess(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "conv-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conv-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "conv"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-conv")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Create pod so diff produces a Create action
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conv-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "conv"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.1.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.1.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for convergence histogram to be recorded (T8.7)
	waitForCondition(t, 30*time.Second, "prefix_set_convergence_seconds emitted", func() bool {
		mfs, _ := te.registry.Gather()
		return metricFamilyExists(mfs, "pod_nsg_controller_prefix_set_convergence_seconds")
	})

	mfs, _ := te.registry.Gather()
	count := gatherHistogramCount(mfs, "pod_nsg_controller_prefix_set_convergence_seconds",
		map[string]string{"operation": "add"})
	if count < 1 {
		t.Errorf("prefix_set_convergence_seconds{operation=add} sample_count = %d, want >= 1", count)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Partial Failure Metrics Through Real Reconcile (envtest)
//
// Integration: MappingReconciler + fake Azure with injected errors + metrics
//
// Verifies that partial failures emit both success and failure action counts
// plus the correct reconcile result classification.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_PartialFailure_MetricsClassification(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "pf-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Mapping with two ASG targets — one will fail
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pf-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "pf"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-pf-ok")},
						{ResourceID: asgResourceID("sub1", "rg1", "asg-pf-fail")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Inject persistent error for one target
	ownershipKey := "test-cluster-" + ns + "-pf-mapping"
	te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg-pf-fail",
		PrefixSetName:  ownershipKey,
	}, &azure.ARMStatusError{
		StatusCode: 500,
		ARMCode:    "InternalServerError",
		Message:    "test partial failure",
	}, 10) // Persist through multiple retries

	// Create pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pf-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "pf"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.2.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.2.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for prefix_set_actions_total to include a failure (T8.10)
	waitForCondition(t, 30*time.Second, "prefix_set_actions_total{result=failure} >= 1", func() bool {
		mfs, _ := te.registry.Gather()
		return gatherCounterValue(mfs, "pod_nsg_controller_prefix_set_actions_total",
			map[string]string{"result": "failure"}) >= 1
	})

	mfs, _ := te.registry.Gather()

	// prefix_set_actions_total{result=success} >= 1
	successActions := gatherCounterValue(mfs, "pod_nsg_controller_prefix_set_actions_total",
		map[string]string{"result": "success"})
	if successActions < 1 {
		t.Errorf("prefix_set_actions_total{result=success} = %v, want >= 1", successActions)
	}

	// prefix_set_actions_total{result=failure} >= 1
	failActions := gatherCounterValue(mfs, "pod_nsg_controller_prefix_set_actions_total",
		map[string]string{"result": "failure"})
	if failActions < 1 {
		t.Errorf("prefix_set_actions_total{result=failure} = %v, want >= 1", failActions)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Drift Detection Metrics (envtest)
//
// Integration: MappingReconciler + fake Azure with pre-existing stale data +
// metrics ConvergenceRecorder.IncrementDriftCorrections
//
// Verifies that when Azure has stale IPs not in the desired state,
// prefix_set_drift_corrections_total is incremented.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_DriftDetection_EmitsDriftMetric(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "drift-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drift-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "drift"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-drift")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Pre-populate Azure with stale IPs (10.0.0.99 is drift)
	ownershipKey := "test-cluster-" + ns + "-drift-mapping"
	if err := te.fakeClient.Put(ctx, "sub1", "rg1", "asg-drift", ownershipKey,
		[]string{"10.0.3.1/32", "10.0.0.99/32"}); err != nil {
		t.Fatalf("pre-populate Azure: %v", err)
	}

	// Create pod with only 10.0.3.1 — 10.0.0.99 is stale
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drift-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "drift"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.3.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.3.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// T8.8: Wait for drift corrections metric
	waitForCondition(t, 30*time.Second, "prefix_set_drift_corrections_total >= 1", func() bool {
		mfs, _ := te.registry.Gather()
		return sumCounterTotal(mfs, "pod_nsg_controller_prefix_set_drift_corrections_total") >= 1
	})

	mfs, _ := te.registry.Gather()
	driftTotal := sumCounterTotal(mfs, "pod_nsg_controller_prefix_set_drift_corrections_total")
	if driftTotal < 1 {
		t.Errorf("prefix_set_drift_corrections_total = %v, want >= 1", driftTotal)
	}
}

// ---------------------------------------------------------------------------
// Test 6: Mapping Deletion Cleans Up Metric State (envtest)
//
// Integration: MappingReconciler + PodChurnTracker.ForgetWithDelete +
// ConvergenceTracker.Forget
//
// Verifies that deleting a mapping cleans up per-mapping metric state so
// stale gauge series are removed.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_MappingDeletion_CleansUpMetricState(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "cleanup-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "cleanup"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-cleanup")},
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
			Name:      "cleanup-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "cleanup"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.4.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.4.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for initial reconcile to process this mapping
	waitForCondition(t, 30*time.Second, "reconcile_total > 0", func() bool {
		mfs, _ := te.registry.Gather()
		return sumCounterTotal(mfs, "pod_nsg_controller_reconcile_total") > 0
	})

	// Verify churn gauge was created
	waitForCondition(t, 10*time.Second, "pod_churn_rate for cleanup-mapping exists", func() bool {
		mfs, _ := te.registry.Gather()
		_, found := gatherGaugeValue(mfs, "pod_nsg_controller_pod_churn_rate",
			map[string]string{"namespace": ns, "mapping": "cleanup-mapping"})
		return found
	})

	// Delete the mapping
	toDelete := &v1alpha1.PodASGMapping{}
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "cleanup-mapping"}, toDelete); err != nil {
		t.Fatalf("get mapping for deletion: %v", err)
	}
	if err := te.k8sClient.Delete(ctx, toDelete); err != nil {
		t.Fatalf("delete mapping: %v", err)
	}

	// Wait for cleanup — the gauge series should be deleted.
	// After ForgetWithDelete, the gauge is removed from the vec.
	waitForCondition(t, 30*time.Second, "pod_churn_rate for cleanup-mapping removed", func() bool {
		mfs, _ := te.registry.Gather()
		_, found := gatherGaugeValue(mfs, "pod_nsg_controller_pod_churn_rate",
			map[string]string{"namespace": ns, "mapping": "cleanup-mapping"})
		return !found
	})
}

// ---------------------------------------------------------------------------
// Test 7: Convergence Committer Invoked via Status Updater (envtest)
//
// Integration: MappingReconciler → StatusUpdater.SetConvergenceCommitter →
// reconcile success → status write → convergence committed
//
// Verifies the full pipeline: reconciler wires the convergence committer
// into the status updater, and after a successful reconcile the convergence
// observation is committed through the callback (T8.7 end-to-end).
// ---------------------------------------------------------------------------

func TestPhase8_Integration_ConvergenceCommitter_InvokedOnStatusWrite(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "committer-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "committer-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "committer"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-committer")},
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
			Name:      "committer-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "committer"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.6.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.6.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for reconcile to complete and convergence to be emitted.
	// The convergence_seconds histogram is the proof that the full pipeline worked:
	// detection → ARM success → commit (before or after status write).
	waitForCondition(t, 30*time.Second, "prefix_set_convergence_seconds observed", func() bool {
		mfs, _ := te.registry.Gather()
		count := gatherHistogramCount(mfs, "pod_nsg_controller_prefix_set_convergence_seconds", nil)
		return count >= 1
	})

	// Also verify the mapping status was updated (convergence callback was invoked)
	var updated v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "committer-mapping"}, &updated); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	// Status should have conditions set after reconcile
	if len(updated.Status.Conditions) == 0 {
		t.Error("mapping status conditions should be set after successful reconcile")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Initial Reconcile Completion Lifecycle (envtest)
//
// Integration: InitialReconcileInitializer → InitialReconcileTracker →
// MappingReconciler.MarkTerminal → ReconcileRecorder.SetInitialReconcileComplete
//
// Verifies T8.11: Controller starts, reconciles all CRs, then
// initial_reconcile_duration_seconds is set to a positive value and
// initial_reconcile_complete is set to 1.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_InitialReconcileCompletion(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "initial-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Create a mapping so the initial reconcile tracker has work to track.
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "initial-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "initial"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-initial")},
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
			Name:      "initial-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "initial"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.7.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.7.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for reconcile_total to confirm the reconcile happened.
	waitForCondition(t, 30*time.Second, "reconcile_total >= 1", func() bool {
		mfs, _ := te.registry.Gather()
		return sumCounterTotal(mfs, "pod_nsg_controller_reconcile_total") >= 1
	})

	// T8.11: initial_reconcile_complete should eventually become 1.
	waitForCondition(t, 30*time.Second, "initial_reconcile_complete == 1", func() bool {
		mfs, _ := te.registry.Gather()
		v, found := gatherGaugeValue(mfs, "pod_nsg_controller_initial_reconcile_complete", nil)
		return found && v == 1
	})

	// Verify initial_reconcile_duration_seconds is positive
	mfs, _ := te.registry.Gather()
	dur, found := gatherGaugeValue(mfs, "pod_nsg_controller_initial_reconcile_duration_seconds", nil)
	if !found {
		t.Error("initial_reconcile_duration_seconds not emitted")
	} else if dur <= 0 {
		t.Errorf("initial_reconcile_duration_seconds = %v, want > 0", dur)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Queue Depth Instrumentation During Real Reconciliation (envtest)
//
// Integration: SetupWithManager → NewInstrumentedQueueFactory → reconcile requests
// → queue Add/Get/Done → reconcile_queue_depth gauge
//
// Verifies that the instrumented queue correctly reports queue depth during
// actual controller-runtime reconciliation.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_QueueDepth_InstrumentedDuringReconcile(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "qdepth-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Create mapping
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qdepth-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "qdepth"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-qdepth")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	// Create pod to trigger reconciliation
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qdepth-pod-1",
			Namespace: ns,
			Labels:    map[string]string{"app": "qdepth"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.8.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.8.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait for reconcile to complete (queue was used)
	waitForCondition(t, 30*time.Second, "reconcile_total >= 1", func() bool {
		mfs, _ := te.registry.Gather()
		return sumCounterTotal(mfs, "pod_nsg_controller_reconcile_total") >= 1
	})

	// The reconcile_queue_depth gauge should exist and have been set at some point.
	mfs, _ := te.registry.Gather()
	if !metricFamilyExists(mfs, "pod_nsg_controller_reconcile_queue_depth") {
		t.Error("reconcile_queue_depth metric not emitted; instrumented queue may not be wired")
	}
}

// ---------------------------------------------------------------------------
// Test 10: Multiple Mappings Emit Isolated Metrics (envtest)
//
// Integration: Multiple PodASGMapping CRs → independent reconcile cycles →
// metrics labeled per-mapping
//
// Verifies that metrics from different mappings don't interfere and are
// correctly scoped by namespace/mapping labels.
// ---------------------------------------------------------------------------

func TestPhase8_Integration_MultipleMappings_IsolatedMetrics(t *testing.T) {
	te := setupMetricsEnvtest(t)
	defer teardownMetricsEnv(t, te)

	ctx := context.Background()
	ns := "multi-integ-ns"

	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Create two mappings with different selectors
	for _, name := range []string{"mapping-alpha", "mapping-beta"} {
		mapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{
					{
						PodSelector: v1alpha1.PodSelector{
							MatchLabels: map[string]string{"app": name},
						},
						ApplicationSecurityGroups: []v1alpha1.ASGReference{
							{ResourceID: asgResourceID("sub1", "rg1", "asg-"+name)},
						},
					},
				},
			},
		}
		if err := te.k8sClient.Create(ctx, mapping); err != nil {
			t.Fatalf("create mapping %s: %v", name, err)
		}
	}

	// Create pods for each mapping
	for i, name := range []string{"mapping-alpha", "mapping-beta"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("multi-pod-%d", i),
				Namespace: ns,
				Labels:    map[string]string{"app": name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
			},
		}
		if err := te.k8sClient.Create(ctx, pod); err != nil {
			t.Fatalf("create pod for %s: %v", name, err)
		}
		pod.Status.PodIP = fmt.Sprintf("10.0.9.%d", i+1)
		pod.Status.PodIPs = []corev1.PodIP{{IP: pod.Status.PodIP}}
		pod.Status.Phase = corev1.PodRunning
		if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
			t.Fatalf("update pod status for %s: %v", name, err)
		}
	}

	// Wait for both mappings to emit pod_ip_changes_total (proves full reconcile
	// path through engine snapshot and pod churn tracker for each mapping).
	waitForCondition(t, 30*time.Second, "pod_ip_changes_total for mapping-alpha", func() bool {
		mfs, _ := te.registry.Gather()
		return gatherCounterValue(mfs, "pod_nsg_controller_pod_ip_changes_total",
			map[string]string{"namespace": ns, "mapping": "mapping-alpha", "operation": "add"}) >= 1
	})
	waitForCondition(t, 30*time.Second, "pod_ip_changes_total for mapping-beta", func() bool {
		mfs, _ := te.registry.Gather()
		return gatherCounterValue(mfs, "pod_nsg_controller_pod_ip_changes_total",
			map[string]string{"namespace": ns, "mapping": "mapping-beta", "operation": "add"}) >= 1
	})

	mfs, _ := te.registry.Gather()

	// Verify reconcile_total has entries for both mappings
	alphaReconcile := gatherCounterValue(mfs, "pod_nsg_controller_reconcile_total",
		map[string]string{"namespace": ns, "mapping": "mapping-alpha"})
	betaReconcile := gatherCounterValue(mfs, "pod_nsg_controller_reconcile_total",
		map[string]string{"namespace": ns, "mapping": "mapping-beta"})

	if alphaReconcile < 1 {
		t.Errorf("reconcile_total{mapping=mapping-alpha} = %v, want >= 1", alphaReconcile)
	}
	if betaReconcile < 1 {
		t.Errorf("reconcile_total{mapping=mapping-beta} = %v, want >= 1", betaReconcile)
	}
}
