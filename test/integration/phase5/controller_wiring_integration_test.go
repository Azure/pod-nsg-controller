package phase5_test

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
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"go.uber.org/zap/zaptest"

	"github.com/Azure/pod-nsg-controller/test/integration/testutil"
	"github.com/go-logr/zapr"
)

const envtestK8sVersion = "1.31.x"

// TestMain configures KUBEBUILDER_ASSETS for envtest before running tests.
func TestMain(m *testing.M) {
	repoRoot, err := repoRootFromCWD()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve repo root: %v\n", err)
		os.Exit(1)
	}

	oldAssets, hadOldAssets := os.LookupEnv("KUBEBUILDER_ASSETS")
	if err := configureEnvtestAssets(repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "failed to configure envtest assets: %v\n", err)
		os.Exit(1)
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
	env         *envtest.Environment
	k8sClient   client.Client
	mgr         ctrl.Manager
	cancel      context.CancelFunc
	fakeClient  *fake.Client
	fakeFactory *fake.ClientFactory
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()

	scheme := integrationScheme(t)
	zapLog := zaptest.NewLogger(t)

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../../config/crd"},
		Scheme:            scheme,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	ctrl.SetLogger(zapr.NewLogger(zapLog))

	mgr := testutil.NewEnvtestManager(t, cfg, scheme)

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	executor := azure.NewExecutor(zapLog, fakeFactory, 2)

	reconciler := &controller.MappingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   2 * time.Second, // short for testing
		PrefixSetFactory: fakeFactory,
		Executor:         executor,
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

	// Wait for cache sync
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		env.Stop()
		t.Fatal("cache sync failed")
	}

	return &testEnv{
		env:         env,
		k8sClient:   mgr.GetClient(),
		mgr:         mgr,
		cancel:      cancel,
		fakeClient:  fakeAzClient,
		fakeFactory: fakeFactory,
	}
}

func (te *testEnv) teardown(t *testing.T) {
	t.Helper()
	te.cancel()
	if err := te.env.Stop(); err != nil {
		t.Errorf("failed to stop envtest: %v", err)
	}
}

// eventually polls a condition until timeout.
func eventually(t *testing.T, timeout, interval time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Errorf("timed out waiting for condition: %s", msg)
}

// ---------------------------------------------------------------------------
// T5.1, T5.2, T5.4: TestPhase5_PodCreateDeleteAndLabelChange_TriggerReconcile
// ---------------------------------------------------------------------------
func TestPhase5_PodCreateDeleteAndLabelChange_TriggerReconcile(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-pod-events"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "web-mapping", Namespace: ns.Name},
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

	// T5.1: Create pod matching the mapping
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod-1",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("T5.1: failed to create pod: %v", err)
	}

	// Wait for finalizer to appear on mapping (indicates reconcile happened)
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "web-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		for _, f := range m.Finalizers {
			if f == controller.CleanupFinalizer {
				return true
			}
		}
		return false
	}, "T5.1: expected finalizer to appear on mapping after pod create")

	// T5.2: Delete the pod → reconcile should remove IP
	if err := te.k8sClient.Delete(ctx, pod); err != nil {
		t.Fatalf("T5.2: failed to delete pod: %v", err)
	}

	// Assert: eventually the prefix set in Azure should not contain the pod IP
	ownershipKey := "test-cluster-" + ns.Name + "-web-mapping"
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if azure.IsNotFound(err) {
			return true // prefix set deleted entirely
		}
		if err != nil {
			return false
		}
		if ps.Properties == nil {
			return true
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return false // IP still present
			}
		}
		return true
	}, "T5.2: expected pod IP to be removed from prefix set after pod delete")
}

// ---------------------------------------------------------------------------
// T5.3: TestPhase5_PodIPChange_UpdatesPrefixSet
// ---------------------------------------------------------------------------
func TestPhase5_PodIPChange_UpdatesPrefixSet(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ip-change"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ip-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Simulate IP assignment via status update
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-ip-mapping"

	// Wait for first reconcile to pick up the IP
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil {
			return false
		}
		if ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "T5.3: expected 10.0.0.1 to appear in prefix set")

	// Now change the IP
	pod.Status.PodIP = "10.0.0.99"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod IP: %v", err)
	}

	// Assert: new IP present, old IP absent
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		hasNew := false
		hasOld := false
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.99/32" {
				hasNew = true
			}
			if ip == "10.0.0.1/32" {
				hasOld = true
			}
		}
		return hasNew && !hasOld
	}, "T5.3: expected old IP absent and new IP present in prefix set after IP change")
}

// ---------------------------------------------------------------------------
// T5.4: TestPhase5_PodLabelChangeAway_RemovesIPFromPrefixSet
// ---------------------------------------------------------------------------
func TestPhase5_PodLabelChangeAway_RemovesIPFromPrefixSet(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-label-change-away"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "label-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "label-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-label-mapping"

	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "T5.4: expected matching pod IP in prefix set before label change")

	var currentPod corev1.Pod
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "label-pod", Namespace: ns.Name}, &currentPod); err != nil {
		t.Fatalf("failed to get pod before label update: %v", err)
	}
	currentPod.Labels = map[string]string{"app": "other"}
	if err := te.k8sClient.Update(ctx, &currentPod); err != nil {
		t.Fatalf("failed to update pod labels: %v", err)
	}

	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if azure.IsNotFound(err) {
			return true
		}
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return false
			}
		}
		return true
	}, "T5.4: expected pod IP to be removed after label changes away from the mapping selector")
}

// ---------------------------------------------------------------------------
// T5.5, T5.6: TestPhase5_MappingSpecMutation_CreateAndDelete
// ---------------------------------------------------------------------------
func TestPhase5_MappingSpecMutation_CreateAndDelete(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-spec-mutation"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "spec-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-spec-mapping"

	// Wait for initial prefix set in asg1
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "T5.5: expected initial prefix set in asg1")

	// T5.5: Add new ASG to mapping spec
	var current v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "spec-mapping", Namespace: ns.Name}, &current); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	current.Spec.Mappings[0].ApplicationSecurityGroups = append(
		current.Spec.Mappings[0].ApplicationSecurityGroups,
		v1alpha1.ASGReference{ResourceID: asgResourceID("sub1", "rg1", "asg2")},
	)
	if err := te.k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("T5.5: failed to update mapping spec: %v", err)
	}

	// Assert: new prefix set created in asg2
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg2", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "T5.5: expected prefix set to be created in asg2")

	// T5.6: Remove asg2 from spec
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "spec-mapping", Namespace: ns.Name}, &current); err != nil {
		t.Fatalf("failed to re-get mapping: %v", err)
	}
	current.Spec.Mappings[0].ApplicationSecurityGroups = current.Spec.Mappings[0].ApplicationSecurityGroups[:1]
	if err := te.k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("T5.6: failed to update mapping spec: %v", err)
	}

	// Assert: prefix set in asg2 deleted
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		_, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg2", ownershipKey)
		return azure.IsNotFound(err)
	}, "T5.6: expected prefix set in asg2 to be deleted after ASG removal from spec")
}

// ---------------------------------------------------------------------------
// T5.7: TestPhase5_MappingDeletion_FinalizerCleanup
// ---------------------------------------------------------------------------
func TestPhase5_MappingDeletion_FinalizerCleanup(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-deletion"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "del-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-del-mapping"

	// Wait for prefix set to be created
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "T5.7: expected prefix set to exist before deletion")

	// Delete the mapping
	if err := te.k8sClient.Delete(ctx, mapping); err != nil {
		t.Fatalf("T5.7: failed to delete mapping: %v", err)
	}

	// Assert: mapping is actually deleted (finalizer removed)
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "del-mapping", Namespace: ns.Name}, &m)
		return err != nil // not found → deleted successfully
	}, "T5.7: expected mapping to be fully deleted (finalizer removed)")

	// Assert: prefix set in Azure is deleted
	_, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if !azure.IsNotFound(err) {
		t.Error("T5.7: expected prefix set to be deleted from Azure after mapping deletion")
	}
}

// ---------------------------------------------------------------------------
// T5.8: TestPhase5_PeriodicResync_CorrectsDrift
// ---------------------------------------------------------------------------
func TestPhase5_PeriodicResync_CorrectsDrift(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-resync"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "resync-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resync-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-resync-mapping"

	// Wait for initial reconcile
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "T5.8: expected initial prefix set")

	// Inject drift: overwrite the prefix set with wrong IPs
	err := te.fakeClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"99.99.99.99"})
	if err != nil {
		t.Fatalf("failed to inject drift: %v", err)
	}

	// Assert: resync corrects drift within ResyncInterval (2s) + buffer
	eventually(t, 10*time.Second, 500*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		hasCorrectIP := false
		hasDriftIP := false
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				hasCorrectIP = true
			}
			if ip == "99.99.99.99" {
				hasDriftIP = true
			}
		}
		return hasCorrectIP && !hasDriftIP
	}, "T5.8: expected resync to correct drift (restore 10.0.0.1, remove 99.99.99.99)")
}

// ---------------------------------------------------------------------------
// T5.9: TestPhase5_PodStatusOnlyUpdate_DoesNotTrigger
// ---------------------------------------------------------------------------
func TestPhase5_PodStatusOnlyUpdate_DoesNotTrigger(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-status-only"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "status-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "status-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-status-mapping"

	// Wait for initial reconcile
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil
	}, "T5.9: expected initial reconcile to complete")

	// Inject drift to detect if reconcile fires
	err := te.fakeClient.Put(ctx, "sub1", "rg1", "asg1", ownershipKey, []string{"99.99.99.99"})
	if err != nil {
		t.Fatalf("failed to inject marker: %v", err)
	}

	// Now do a status-only update (change phase, not IP or labels)
	pod.Status.Phase = corev1.PodSucceeded
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	// Assert: drift should NOT be corrected immediately (predicate filtered the status-only update)
	// Wait a brief period and check that the drift marker remains
	time.Sleep(1 * time.Second)
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if err != nil {
		t.Fatalf("T5.9: failed to get prefix set: %v", err)
	}
	if ps.Properties != nil {
		hasDrift := false
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "99.99.99.99" {
				hasDrift = true
			}
		}
		if !hasDrift {
			t.Error("T5.9: drift marker was corrected, suggesting predicate did NOT filter status-only update")
		}
	}
}

// ---------------------------------------------------------------------------
// T5.10: TestPhase5_MultipleMappings_OnlyMatchingMappingAffected
// ---------------------------------------------------------------------------
func TestPhase5_MultipleMappings_OnlyMatchingMappingAffected(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-multi-mapping"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	webMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "web-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-web")},
					},
				},
			},
		},
	}
	dbMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "db-mapping", Namespace: ns.Name},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "db"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg-db")},
					},
				},
			},
		},
	}

	if err := te.k8sClient.Create(ctx, webMapping); err != nil {
		t.Fatalf("failed to create web-mapping: %v", err)
	}
	if err := te.k8sClient.Create(ctx, dbMapping); err != nil {
		t.Fatalf("failed to create db-mapping: %v", err)
	}

	// Create a web pod only
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to set pod IP: %v", err)
	}

	webOwnerKey := "test-cluster-" + ns.Name + "-web-mapping"
	dbOwnerKey := "test-cluster-" + ns.Name + "-db-mapping"

	// Assert: web mapping's prefix set should have the pod IP
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-web", webOwnerKey)
		if err != nil || ps.Properties == nil {
			return false
		}
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				return true
			}
		}
		return false
	}, "T5.10: expected web pod IP in asg-web prefix set")

	// Assert: db mapping's prefix set should NOT contain the web pod IP
	// (db-mapping may not even have a prefix set created, or if reconciled it should be empty)
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg-db", dbOwnerKey)
	if err == nil && ps.Properties != nil {
		for _, ip := range ps.Properties.AddressPrefixes {
			if ip == "10.0.0.1/32" {
				t.Error("T5.10: web pod IP should NOT appear in asg-db prefix set")
			}
		}
	}
	// If not-found, that's also correct — db-mapping shouldn't have created a prefix set for web pods
}

// ---------------------------------------------------------------------------
// TestPhase5_MappingDeletion_CleanupFailureRetainsFinalizer
// Design §5.3: Cleanup error → finalizer retained, mapping not deleted
// ---------------------------------------------------------------------------
func TestPhase5_MappingDeletion_CleanupFailureRetainsFinalizer(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-cleanup-fail"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-mapping", Namespace: ns.Name},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: ns.Name,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := "test-cluster-" + ns.Name + "-fail-mapping"

	// Wait for prefix set to be created
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	}, "expected prefix set to exist before deletion")

	// Inject persistent delete errors
	_ = te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationDelete,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  ownershipKey,
	}, fmt.Errorf("persistent delete failure"), 10)

	// Delete the mapping
	if err := te.k8sClient.Delete(ctx, mapping); err != nil {
		t.Fatalf("failed to delete mapping: %v", err)
	}

	// Wait a few reconcile cycles
	time.Sleep(5 * time.Second)

	// Assert: mapping still exists with CleanupFinalizer retained
	var m v1alpha1.PodASGMapping
	err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "fail-mapping", Namespace: ns.Name}, &m)
	if err != nil {
		t.Fatalf("expected mapping to still exist (finalizer retained), but got error: %v", err)
	}

	hasCleanup := false
	for _, f := range m.Finalizers {
		if f == controller.CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when cleanup fails")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_DeleteWithCorruptOwnedAnnotation_FinalizerRetained
// Design §5.3 rule 1: corrupt owned annotation on delete → error, finalizer retained
// ---------------------------------------------------------------------------
func TestPhase5_DeleteWithCorruptOwnedAnnotation_FinalizerRetained(t *testing.T) {
	te := setupTestEnv(t)
	defer te.teardown(t)
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-corrupt-annot"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "corrupt-mapping", Namespace: ns.Name},
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

	// Wait for finalizer
	eventually(t, 10*time.Second, 200*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "corrupt-mapping", Namespace: ns.Name}, &m); err != nil {
			return false
		}
		for _, f := range m.Finalizers {
			if f == controller.CleanupFinalizer {
				return true
			}
		}
		return false
	}, "expected finalizer to be added")

	// Corrupt the owned annotation
	var current v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "corrupt-mapping", Namespace: ns.Name}, &current); err != nil {
		t.Fatalf("failed to get mapping: %v", err)
	}
	if current.Annotations == nil {
		current.Annotations = make(map[string]string)
	}
	current.Annotations[controller.OwnedASGsAnnotationKey] = "not-valid-json{{{"
	if err := te.k8sClient.Update(ctx, &current); err != nil {
		t.Fatalf("failed to corrupt annotation: %v", err)
	}

	// Delete the mapping
	if err := te.k8sClient.Delete(ctx, &current); err != nil {
		t.Fatalf("failed to delete mapping: %v", err)
	}

	// Wait a few reconcile cycles
	time.Sleep(5 * time.Second)

	// Assert: mapping still exists with CleanupFinalizer retained
	var m v1alpha1.PodASGMapping
	err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "corrupt-mapping", Namespace: ns.Name}, &m)
	if err != nil {
		t.Fatalf("expected mapping to still exist (corrupt annotation should block cleanup), got: %v", err)
	}

	hasCleanup := false
	for _, f := range m.Finalizers {
		if f == controller.CleanupFinalizer {
			hasCleanup = true
		}
	}
	if !hasCleanup {
		t.Error("expected CleanupFinalizer to be retained when owned annotation is corrupt during delete")
	}
}
