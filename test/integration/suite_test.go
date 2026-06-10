package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	"github.com/Azure/pod-nsg-controller/test/integration/testutil"
	"github.com/go-logr/zapr"
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
	return filepath.Clean(filepath.Join(wd, "..", "..")), nil
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
// integrationEnv — envtest harness for integration tests
// ---------------------------------------------------------------------------

type integrationEnv struct {
	env              *envtest.Environment
	scheme           *runtime.Scheme
	mgr              ctrl.Manager
	k8sClient        client.Client
	mgrCtx           context.Context
	mgrCancel        context.CancelFunc
	mgrDone          chan error
	fakeFactory      *fake.ClientFactory
	fakeClientsBySub map[string]*fake.Client
	desiredCache     *engine.DesiredStateCache
	zapLog           *zap.Logger
	opts             integrationEnvOptions
}

type integrationEnvOptions struct {
	ClusterName             string
	ResyncInterval          time.Duration
	MaxConcurrentReconciles int
	MaxConcurrentAzureReads int
	Subscriptions           []string
}

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

func setupIntegrationEnv(t *testing.T, opts integrationEnvOptions) *integrationEnv {
	t.Helper()

	metrics.ResetForTesting()

	if opts.ClusterName == "" {
		opts.ClusterName = "test-cluster"
	}
	if opts.ResyncInterval == 0 {
		opts.ResyncInterval = 60 * time.Second
	}
	if opts.MaxConcurrentReconciles == 0 {
		opts.MaxConcurrentReconciles = 1
	}
	if opts.MaxConcurrentAzureReads == 0 {
		opts.MaxConcurrentAzureReads = 5
	}
	if len(opts.Subscriptions) == 0 {
		opts.Subscriptions = []string{"sub1"}
	}

	scheme := integrationScheme(t)
	zapLog := zaptest.NewLogger(t)
	ctrl.SetLogger(zapr.NewLogger(zapLog))

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../config/crd"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	mgr := testutil.NewEnvtestManager(t, cfg, scheme)

	fakeFactory := fake.NewClientFactory()
	fakeClients := make(map[string]*fake.Client)
	for _, sub := range opts.Subscriptions {
		c := fake.NewClient()
		fakeFactory.RegisterClient(sub, c)
		fakeClients[sub] = c
	}

	desiredCache := engine.NewDesiredStateCache(opts.ClusterName)

	ie := &integrationEnv{
		env:              env,
		scheme:           scheme,
		mgr:              mgr,
		k8sClient:        mgr.GetClient(),
		fakeFactory:      fakeFactory,
		fakeClientsBySub: fakeClients,
		desiredCache:     desiredCache,
		zapLog:           zapLog,
		opts:             opts,
	}

	return ie
}

func (e *integrationEnv) startManager(t *testing.T, exec controller.Executor) {
	t.Helper()

	statusUpdater := controller.NewMappingStatusUpdater(e.mgr.GetClient(), ctrl.Log.WithName("status-updater"))

	reconciler := &controller.MappingReconciler{
		Client:                  e.mgr.GetClient(),
		Scheme:                  e.scheme,
		ClusterName:             e.opts.ClusterName,
		ResyncInterval:          e.opts.ResyncInterval,
		MaxConcurrentReconciles: e.opts.MaxConcurrentReconciles,
		AzureReadSem:            make(chan struct{}, e.opts.MaxConcurrentAzureReads),
		PrefixSetFactory:        e.fakeFactory,
		Executor:                exec,
		StatusUpdater:           statusUpdater,
		DesiredStateCache:       e.desiredCache,
	}

	if err := reconciler.SetupWithManager(e.mgr); err != nil {
		e.env.Stop()
		t.Fatalf("failed to setup reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.mgrCtx = ctx
	e.mgrCancel = cancel
	e.mgrDone = make(chan error, 1)

	go func() {
		e.mgrDone <- e.mgr.Start(ctx)
	}()

	if !e.mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		e.env.Stop()
		t.Fatal("cache sync failed")
	}
}

func (e *integrationEnv) teardown(t *testing.T) {
	t.Helper()
	if e.mgrCancel != nil {
		e.mgrCancel()
		<-e.mgrDone
	}
	if err := e.env.Stop(); err != nil {
		t.Errorf("failed to stop envtest: %v", err)
	}
	metrics.ResetForTesting()
}

func (e *integrationEnv) triggerReconcile(t *testing.T, key types.NamespacedName) {
	t.Helper()

	var mapping v1alpha1.PodASGMapping
	if err := e.k8sClient.Get(context.Background(), key, &mapping); err != nil {
		t.Fatalf("triggerReconcile: get mapping: %v", err)
	}
	patch := client.MergeFrom(mapping.DeepCopy())
	// Toggle a test-only finalizer to trigger MappingPredicate (watches finalizer changes).
	const triggerFinalizer = "test.pod-nsg-controller.io/reconcile-trigger"
	hasTrigger := false
	for _, f := range mapping.Finalizers {
		if f == triggerFinalizer {
			hasTrigger = true
			break
		}
	}
	if hasTrigger {
		filtered := make([]string, 0, len(mapping.Finalizers)-1)
		for _, f := range mapping.Finalizers {
			if f != triggerFinalizer {
				filtered = append(filtered, f)
			}
		}
		mapping.Finalizers = filtered
	} else {
		mapping.Finalizers = append(mapping.Finalizers, triggerFinalizer)
	}
	if err := e.k8sClient.Patch(context.Background(), &mapping, patch); err != nil {
		t.Fatalf("triggerReconcile: patch mapping: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func eventually(t *testing.T, timeout, interval time.Duration, desc string, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastMsg string
	for time.Now().Before(deadline) {
		ok, msg := fn()
		if ok {
			return
		}
		lastMsg = msg
		time.Sleep(interval)
	}
	t.Fatalf("timed out waiting for %s: %s", desc, lastMsg)
}

func asgResourceID(sub, rg, asg string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/applicationSecurityGroups/" + asg
}

func createNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

func createPodWithIP(t *testing.T, c client.Client, ns, name, ip string, labels map[string]string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
		},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod %s/%s: %v", ns, name, err)
	}
	pod.Status.PodIP = ip
	pod.Status.PodIPs = []corev1.PodIP{{IP: ip}}
	pod.Status.Phase = corev1.PodRunning
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status %s/%s: %v", ns, name, err)
	}
}

func createMapping(t *testing.T, c client.Client, ns, name string, spec v1alpha1.PodASGMappingSpec) {
	t.Helper()
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: spec,
	}
	if err := c.Create(context.Background(), mapping); err != nil {
		t.Fatalf("create mapping %s/%s: %v", ns, name, err)
	}
}

func deleteMapping(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
	}
	if err := c.Delete(context.Background(), mapping); err != nil {
		t.Fatalf("delete mapping %s/%s: %v", ns, name, err)
	}
}

func deletePod(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
	}
	if err := c.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s/%s: %v", ns, name, err)
	}
}

func (e *integrationEnv) restartManager(t *testing.T, exec controller.Executor) {
	t.Helper()

	e.mgrCancel()
	select {
	case err := <-e.mgrDone:
		if err != nil {
			t.Logf("old manager stopped with: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for old manager to stop")
	}

	cfg := e.env.Config
	mgr := testutil.NewEnvtestManager(t, cfg, e.scheme)
	e.mgr = mgr
	e.k8sClient = mgr.GetClient()

	freshCache := engine.NewDesiredStateCache(e.opts.ClusterName)
	e.desiredCache = freshCache

	statusUpdater := controller.NewMappingStatusUpdater(mgr.GetClient(), ctrl.Log.WithName("status-updater"))
	reconciler := &controller.MappingReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  e.scheme,
		ClusterName:             e.opts.ClusterName,
		ResyncInterval:          e.opts.ResyncInterval,
		MaxConcurrentReconciles: e.opts.MaxConcurrentReconciles,
		AzureReadSem:            make(chan struct{}, e.opts.MaxConcurrentAzureReads),
		PrefixSetFactory:        e.fakeFactory,
		Executor:                exec,
		StatusUpdater:           statusUpdater,
		DesiredStateCache:       freshCache,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup reconciler on restart: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.mgrCtx = ctx
	e.mgrCancel = cancel
	e.mgrDone = make(chan error, 1)

	go func() {
		e.mgrDone <- mgr.Start(ctx)
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		t.Fatal("cache sync failed after restart")
	}
}

// blockingExecutor blocks in Execute until released or context canceled.
type blockingExecutor struct {
	delegate    controller.Executor
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func newBlockingExecutor(delegate controller.Executor) *blockingExecutor {
	return &blockingExecutor{
		delegate: delegate,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (b *blockingExecutor) Execute(ctx context.Context, actions []engine.Action) []azure.ActionResult {
	b.startedOnce.Do(func() {
		close(b.started)
	})

	select {
	case <-b.release:
		return b.delegate.Execute(ctx, actions)
	case <-ctx.Done():
		results := make([]azure.ActionResult, len(actions))
		for i, a := range actions {
			results[i] = azure.ActionResult{
				Action:  a,
				Success: false,
				Err:     ctx.Err(),
			}
		}
		return results
	}
}
