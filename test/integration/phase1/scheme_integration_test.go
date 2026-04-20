package phase1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap/zaptest"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
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

// buildFullScheme reproduces the exact scheme composition from cmd/main.go init().
func buildFullScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme failed: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme failed: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("v1alpha1.AddToScheme failed: %v", err)
	}
	return s
}

// startFullEnvtest starts an envtest environment with the full scheme from
// cmd/main.go, installs the PodASGMapping CRD, and returns a typed client.
func startFullEnvtest(t *testing.T) (*envtest.Environment, *rest.Config, client.Client, *runtime.Scheme) {
	t.Helper()
	repoRoot, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}
	crdDir := filepath.Join(repoRoot, "config", "crd")
	if _, err := os.Stat(crdDir); os.IsNotExist(err) {
		t.Fatalf("CRD directory not found at %s — run 'make manifests' first", crdDir)
	}

	log.SetLogger(zapr.NewLogger(zaptest.NewLogger(t)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	s := buildFullScheme(t)
	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("failed to create k8s client: %v", err)
	}

	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	})

	return testEnv, cfg, k8sClient, s
}

func newValidPodASGMapping(namespace, name string) *v1alpha1.PodASGMapping {
	return &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{
							ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test",
						},
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Integration: Full scheme composition from cmd/main.go works with envtest
// ---------------------------------------------------------------------------

func TestIntegration_FullSchemeWithEnvtest(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: full scheme (clientgoscheme + corev1 + v1alpha1) with envtest")

	_, _, k8sClient, s := startFullEnvtest(t)
	ctx := context.Background()

	t.Run("SchemeKnowsBothCoreAndCRDTypes", func(t *testing.T) {
		// Core type: Pod
		_, err := s.New(corev1.SchemeGroupVersion.WithKind("Pod"))
		if err != nil {
			t.Errorf("scheme does not know Pod: %v", err)
		}
		// CRD type: PodASGMapping
		_, err = s.New(v1alpha1.GroupVersion.WithKind("PodASGMapping"))
		if err != nil {
			t.Errorf("scheme does not know PodASGMapping: %v", err)
		}
	})

	t.Run("CRDAndCoreTypesCoexist", func(t *testing.T) {
		// Create a PodASGMapping
		mapping := newValidPodASGMapping("default", "coexist-test")
		if err := k8sClient.Create(ctx, mapping); err != nil {
			t.Fatalf("failed to create PodASGMapping: %v", err)
		}

		// Create a ConfigMap (core type) in the same namespace
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "coexist-cm",
			},
			Data: map[string]string{"key": "value"},
		}
		if err := k8sClient.Create(ctx, cm); err != nil {
			t.Fatalf("failed to create ConfigMap: %v", err)
		}

		// Verify both can be fetched
		fetchedMapping := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coexist-test"}, fetchedMapping); err != nil {
			t.Errorf("failed to get PodASGMapping: %v", err)
		}

		fetchedCM := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "coexist-cm"}, fetchedCM); err != nil {
			t.Errorf("failed to get ConfigMap: %v", err)
		}

		// Verify they are independent
		if fetchedMapping.Name != "coexist-test" {
			t.Errorf("PodASGMapping name: got %q, want %q", fetchedMapping.Name, "coexist-test")
		}
		if fetchedCM.Data["key"] != "value" {
			t.Errorf("ConfigMap data: got %q, want %q", fetchedCM.Data["key"], "value")
		}
	})

	t.Run("ListCRDDoesNotReturnCoreTypes", func(t *testing.T) {
		mappingList := &v1alpha1.PodASGMappingList{}
		if err := k8sClient.List(ctx, mappingList, client.InNamespace("default")); err != nil {
			t.Fatalf("failed to list PodASGMappings: %v", err)
		}
		for _, item := range mappingList.Items {
			if item.Kind == "ConfigMap" || item.Kind == "Pod" {
				t.Errorf("PodASGMappingList should not contain core types, found kind %q", item.Kind)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Controller-runtime manager creation with full scheme + CRD
// ---------------------------------------------------------------------------

func TestIntegration_ManagerCreationWithCRD(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: controller-runtime manager with full scheme and CRD")

	_, cfg, _, s := startFullEnvtest(t)

	// Create a manager the same way cmd/main.go does, but with a random metrics port
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	t.Run("ManagerSchemeRecognizesCRDType", func(t *testing.T) {
		_, err := mgr.GetScheme().New(v1alpha1.GroupVersion.WithKind("PodASGMapping"))
		if err != nil {
			t.Errorf("manager scheme does not recognize PodASGMapping: %v", err)
		}
	})

	t.Run("ManagerClientCRUD", func(t *testing.T) {
		// Start manager in background for cache sync
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		startErrCh := make(chan error, 1)
		go func() {
			startErrCh <- mgr.Start(ctx)
		}()

		// Wait for cache sync; if it fails, distinguish a Start error from a plain sync timeout.
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			select {
			case err := <-startErrCh:
				t.Fatalf("manager Start failed: %v", err)
			default:
				t.Fatal("cache sync failed")
			}
		}

		// Use manager's client (cached) to create and get PodASGMapping
		mgrClient := mgr.GetClient()
		obj := newValidPodASGMapping("default", "mgr-crud")
		if err := mgrClient.Create(ctx, obj); err != nil {
			t.Fatalf("manager client Create failed: %v", err)
		}

		fetched := &v1alpha1.PodASGMapping{}
		// Retry Get to allow cache to sync the newly created object
		var getErr error
		for i := 0; i < 20; i++ {
			getErr = mgrClient.Get(ctx, types.NamespacedName{
				Namespace: "default", Name: "mgr-crud",
			}, fetched)
			if getErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if getErr != nil {
			t.Fatalf("manager client Get failed after retries: %v", getErr)
		}
		if len(fetched.Spec.Mappings) != 1 {
			t.Errorf("manager client Get: got %d mappings, want 1", len(fetched.Spec.Mappings))
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Manager cache watches PodASGMapping resources
// ---------------------------------------------------------------------------

func TestIntegration_CacheInformerForCRD(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: manager cache informer for PodASGMapping")

	_, cfg, _, s := startFullEnvtest(t)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  s,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErrCh := make(chan error, 1)
	go func() {
		startErrCh <- mgr.Start(ctx)
	}()

	// Wait for cache sync; if it fails, distinguish a Start error from a plain sync timeout.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		select {
		case err := <-startErrCh:
			t.Fatalf("manager Start failed: %v", err)
		default:
			t.Fatal("cache sync failed")
		}
	}

	t.Run("GetInformerForCRDType", func(t *testing.T) {
		// Getting an informer for PodASGMapping should succeed
		_, err := mgr.GetCache().GetInformer(ctx, &v1alpha1.PodASGMapping{})
		if err != nil {
			t.Errorf("failed to get informer for PodASGMapping: %v", err)
		}
	})

	t.Run("CacheListReflectsCreatedObjects", func(t *testing.T) {
		// Use a direct client to create (bypass cache for write)
		directClient, err := client.New(cfg, client.Options{Scheme: s})
		if err != nil {
			t.Fatalf("failed to create direct client: %v", err)
		}

		obj := newValidPodASGMapping("default", "cache-test")
		if err := directClient.Create(ctx, obj); err != nil {
			t.Fatalf("direct client Create failed: %v", err)
		}

		// Wait for the cache to pick up the object
		var listResult v1alpha1.PodASGMappingList
		var found bool
		for i := 0; i < 30; i++ {
			if err := mgr.GetCache().List(ctx, &listResult); err != nil {
				t.Fatalf("cache List failed: %v", err)
			}
			for _, item := range listResult.Items {
				if item.Name == "cache-test" {
					found = true
					break
				}
			}
			if found {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !found {
			t.Error("cache did not reflect the created PodASGMapping within timeout")
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Namespace-scoped isolation for PodASGMapping
// ---------------------------------------------------------------------------

func TestIntegration_NamespaceScopedIsolation(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: PodASGMapping namespace isolation")

	_, _, k8sClient, _ := startFullEnvtest(t)
	ctx := context.Background()

	// Create two namespaces
	for _, nsName := range []string{"ns-alpha", "ns-beta"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("failed to create namespace %s: %v", nsName, err)
		}
	}

	// Create PodASGMapping in ns-alpha
	objAlpha := newValidPodASGMapping("ns-alpha", "ns-isolation")
	if err := k8sClient.Create(ctx, objAlpha); err != nil {
		t.Fatalf("failed to create PodASGMapping in ns-alpha: %v", err)
	}

	// Create a differently-named PodASGMapping in ns-beta
	objBeta := newValidPodASGMapping("ns-beta", "ns-isolation-beta")
	if err := k8sClient.Create(ctx, objBeta); err != nil {
		t.Fatalf("failed to create PodASGMapping in ns-beta: %v", err)
	}

	t.Run("ListInAlphaDoesNotIncludeBeta", func(t *testing.T) {
		list := &v1alpha1.PodASGMappingList{}
		if err := k8sClient.List(ctx, list, client.InNamespace("ns-alpha")); err != nil {
			t.Fatalf("list in ns-alpha failed: %v", err)
		}
		if len(list.Items) != 1 {
			t.Errorf("ns-alpha: got %d items, want 1", len(list.Items))
		}
		if list.Items[0].Name != "ns-isolation" {
			t.Errorf("ns-alpha: got name %q, want %q", list.Items[0].Name, "ns-isolation")
		}
	})

	t.Run("ListInBetaDoesNotIncludeAlpha", func(t *testing.T) {
		list := &v1alpha1.PodASGMappingList{}
		if err := k8sClient.List(ctx, list, client.InNamespace("ns-beta")); err != nil {
			t.Fatalf("list in ns-beta failed: %v", err)
		}
		if len(list.Items) != 1 {
			t.Errorf("ns-beta: got %d items, want 1", len(list.Items))
		}
		if list.Items[0].Name != "ns-isolation-beta" {
			t.Errorf("ns-beta: got name %q, want %q", list.Items[0].Name, "ns-isolation-beta")
		}
	})

	t.Run("CrossNamespaceListReturnsAll", func(t *testing.T) {
		list := &v1alpha1.PodASGMappingList{}
		if err := k8sClient.List(ctx, list); err != nil {
			t.Fatalf("cross-namespace list failed: %v", err)
		}
		names := map[string]bool{}
		for _, item := range list.Items {
			names[item.Namespace+"/"+item.Name] = true
		}
		if !names["ns-alpha/ns-isolation"] {
			t.Error("cross-namespace list missing ns-alpha/ns-isolation")
		}
		if !names["ns-beta/ns-isolation-beta"] {
			t.Error("cross-namespace list missing ns-beta/ns-isolation-beta")
		}
	})

	t.Run("SameNameDifferentNamespaces", func(t *testing.T) {
		objAlpha2 := newValidPodASGMapping("ns-alpha", "shared-name")
		if err := k8sClient.Create(ctx, objAlpha2); err != nil {
			t.Fatalf("create shared-name in ns-alpha: %v", err)
		}
		objBeta2 := newValidPodASGMapping("ns-beta", "shared-name")
		if err := k8sClient.Create(ctx, objBeta2); err != nil {
			t.Fatalf("create shared-name in ns-beta: %v", err)
		}

		fetchAlpha := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ns-alpha", Name: "shared-name"}, fetchAlpha); err != nil {
			t.Fatalf("get ns-alpha/shared-name: %v", err)
		}
		fetchBeta := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "ns-beta", Name: "shared-name"}, fetchBeta); err != nil {
			t.Fatalf("get ns-beta/shared-name: %v", err)
		}

		if fetchAlpha.UID == fetchBeta.UID {
			t.Error("same-named objects in different namespaces should have different UIDs")
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Status subresource isolation (spec update vs status update)
// across the full scheme boundary
// ---------------------------------------------------------------------------

func TestIntegration_StatusSubresourceWithFullScheme(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: status subresource with full scheme")

	_, _, k8sClient, _ := startFullEnvtest(t)
	ctx := context.Background()

	obj := newValidPodASGMapping("default", "status-integration")
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	t.Run("StatusUpdateDoesNotChangeSpec", func(t *testing.T) {
		fresh := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "status-integration"}, fresh); err != nil {
			t.Fatalf("Get failed: %v", err)
		}

		fresh.Status = v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: fresh.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "AllSynced",
					Message:            "all mappings synced",
				},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{
					SelectorHash: "abc123",
					MatchedPods:  5,
					ASGSyncState: "Synced",
					LastSyncTime: metav1.Now(),
				},
			},
		}

		if err := k8sClient.Status().Update(ctx, fresh); err != nil {
			t.Fatalf("Status().Update failed: %v", err)
		}

		final := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "status-integration"}, final); err != nil {
			t.Fatalf("Get after status update failed: %v", err)
		}

		// Status should be set
		if len(final.Status.Conditions) != 1 {
			t.Errorf("expected 1 condition, got %d", len(final.Status.Conditions))
		}
		if final.Status.MappingStatuses[0].MatchedPods != 5 {
			t.Errorf("matchedPods: got %d, want 5", final.Status.MappingStatuses[0].MatchedPods)
		}

		// Spec should be unchanged
		if len(final.Spec.Mappings) != 1 {
			t.Errorf("spec changed after status update: got %d mappings, want 1", len(final.Spec.Mappings))
		}
		if final.Spec.Mappings[0].PodSelector.MatchLabels["app"] != "web" {
			t.Errorf("spec label changed after status update: got %q, want %q",
				final.Spec.Mappings[0].PodSelector.MatchLabels["app"], "web")
		}
	})

	t.Run("SpecUpdateDoesNotClearStatus", func(t *testing.T) {
		fresh := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "status-integration"}, fresh); err != nil {
			t.Fatalf("Get failed: %v", err)
		}

		// Modify spec
		fresh.Spec.Mappings[0].PodSelector.MatchLabels["tier"] = "frontend"
		if err := k8sClient.Update(ctx, fresh); err != nil {
			t.Fatalf("spec Update failed: %v", err)
		}

		final := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "status-integration"}, final); err != nil {
			t.Fatalf("Get after spec update failed: %v", err)
		}

		// Spec should reflect the change
		if final.Spec.Mappings[0].PodSelector.MatchLabels["tier"] != "frontend" {
			t.Errorf("spec label not updated: got %q, want %q",
				final.Spec.Mappings[0].PodSelector.MatchLabels["tier"], "frontend")
		}

		// Status should still be present
		if len(final.Status.Conditions) != 1 {
			t.Errorf("status cleared after spec update: got %d conditions, want 1", len(final.Status.Conditions))
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: DeepCopy fidelity through API server pipeline
// Verifies zz_generated.deepcopy.go works correctly with objects that have
// been round-tripped through the API server (serialization/deserialization).
// ---------------------------------------------------------------------------

func TestIntegration_DeepCopyThroughAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: deepcopy mutation independence through API server pipeline")

	_, _, k8sClient, _ := startFullEnvtest(t)
	ctx := context.Background()

	// Create a fully-populated object
	obj := newValidPodASGMapping("default", "deepcopy-pipeline")
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Set status via the status subresource
	fresh := &v1alpha1.PodASGMapping{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "deepcopy-pipeline"}, fresh); err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	fresh.Status = v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: fresh.Generation,
				LastTransitionTime: metav1.Now(),
				Reason:             "AllSynced",
				Message:            "all mappings synced",
			},
		},
		MappingStatuses: []v1alpha1.MappingStatus{
			{
				SelectorHash: "abc123",
				MatchedPods:  5,
				ASGSyncState: "Synced",
				LastSyncTime: metav1.Now(),
			},
		},
	}
	if err := k8sClient.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("Status().Update failed: %v", err)
	}

	t.Run("DeepCopyFromAPIServerIsMutationIndependent", func(t *testing.T) {
		// Fetch the full object from API server
		original := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "deepcopy-pipeline"}, original); err != nil {
			t.Fatalf("Get original failed: %v", err)
		}

		// DeepCopy it
		copied := original.DeepCopy()

		// Mutate the copy
		copied.Spec.Mappings[0].PodSelector.MatchLabels["app"] = "mutated"
		copied.Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "mutated-desc"
		if len(copied.Status.Conditions) > 0 {
			copied.Status.Conditions[0].Message = "mutated-message"
		}
		if len(copied.Status.MappingStatuses) > 0 {
			copied.Status.MappingStatuses[0].MatchedPods = 999
		}

		// Verify original is untouched
		if original.Spec.Mappings[0].PodSelector.MatchLabels["app"] != "web" {
			t.Errorf("original spec mutated: got label %q, want %q",
				original.Spec.Mappings[0].PodSelector.MatchLabels["app"], "web")
		}
		if original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description != "" {
			t.Errorf("original description mutated: got %q, want empty",
				original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description)
		}
		if len(original.Status.Conditions) > 0 && original.Status.Conditions[0].Message != "all mappings synced" {
			t.Errorf("original status condition mutated: got %q, want %q",
				original.Status.Conditions[0].Message, "all mappings synced")
		}
		if len(original.Status.MappingStatuses) > 0 && original.Status.MappingStatuses[0].MatchedPods != 5 {
			t.Errorf("original status matchedPods mutated: got %d, want 5",
				original.Status.MappingStatuses[0].MatchedPods)
		}
	})

	t.Run("DeepCopiedObjectCanBeUpdatedIndependently", func(t *testing.T) {
		original := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "deepcopy-pipeline"}, original); err != nil {
			t.Fatalf("Get failed: %v", err)
		}

		// DeepCopy and modify
		copied := original.DeepCopy()
		copied.Spec.Mappings[0].PodSelector.MatchLabels["tier"] = "frontend"

		// Update the copy on the API server
		if err := k8sClient.Update(ctx, copied); err != nil {
			t.Fatalf("Update copied object failed: %v", err)
		}

		// Verify the update took effect
		fetched := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "deepcopy-pipeline"}, fetched); err != nil {
			t.Fatalf("Get after update failed: %v", err)
		}
		if fetched.Spec.Mappings[0].PodSelector.MatchLabels["tier"] != "frontend" {
			t.Errorf("update via deep copy not persisted: got %q, want %q",
				fetched.Spec.Mappings[0].PodSelector.MatchLabels["tier"], "frontend")
		}

		// Original in-memory object should still have stale resourceVersion
		if original.ResourceVersion == fetched.ResourceVersion {
			t.Error("original should have stale resourceVersion after copy was updated")
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Discovery API alignment with Go scheme registration
// Verifies groupversion_info.go ↔ config/crd/podasgmapping.yaml ↔ discovery
// ---------------------------------------------------------------------------

func TestIntegration_DiscoveryAPIAlignment(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: discovery API alignment with Go scheme registration")

	_, cfg, _, _ := startFullEnvtest(t)

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create discovery client: %v", err)
	}

	t.Run("CRDGroupVersionMatchesGoConstants", func(t *testing.T) {
		// The Go code defines Group="networking.azure.com", Version="v1alpha1"
		expectedGV := v1alpha1.GroupVersion.String() // "networking.azure.com/v1alpha1"

		resources, err := dc.ServerResourcesForGroupVersion(expectedGV)
		if err != nil {
			t.Fatalf("discovery failed for %s: %v", expectedGV, err)
		}

		var found bool
		for _, r := range resources.APIResources {
			if r.Kind == "PodASGMapping" {
				found = true
				if r.Name != "podasgmappings" {
					t.Errorf("resource name: got %q, want %q", r.Name, "podasgmappings")
				}
				if r.SingularName != "podasgmapping" {
					t.Errorf("singular name: got %q, want %q", r.SingularName, "podasgmapping")
				}
				if !r.Namespaced {
					t.Error("PodASGMapping should be namespace-scoped")
				}

				// Verify status subresource is exposed
				var hasStatus bool
				for _, sub := range resources.APIResources {
					if sub.Name == "podasgmappings/status" {
						hasStatus = true
						break
					}
				}
				if !hasStatus {
					t.Error("status subresource not found in discovery")
				}
				break
			}
		}
		if !found {
			t.Errorf("PodASGMapping kind not found in discovery for %s", expectedGV)
		}
	})

	t.Run("GroupVersionListIncludesNetworkingAzure", func(t *testing.T) {
		groups, err := dc.ServerGroups()
		if err != nil {
			t.Fatalf("failed to get server groups: %v", err)
		}

		var found bool
		for _, g := range groups.Groups {
			if g.Name == "networking.azure.com" {
				found = true
				// Verify v1alpha1 is listed
				var hasVersion bool
				for _, v := range g.Versions {
					if v.Version == "v1alpha1" {
						hasVersion = true
						break
					}
				}
				if !hasVersion {
					t.Error("v1alpha1 version not listed in networking.azure.com group")
				}
				break
			}
		}
		if !found {
			t.Error("networking.azure.com group not found in server groups")
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Generation tracking across spec/status boundary
// Verifies that metadata.generation increments on spec changes but NOT on
// status-only changes — confirming the status subresource registration is
// correctly wired between CRD YAML and the API server.
// ---------------------------------------------------------------------------

func TestIntegration_GenerationTracking(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: generation tracking across spec/status subresource boundary")

	_, _, k8sClient, _ := startFullEnvtest(t)
	ctx := context.Background()

	obj := newValidPodASGMapping("default", "gen-track")
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Fetch initial generation
	initial := &v1alpha1.PodASGMapping{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gen-track"}, initial); err != nil {
		t.Fatalf("Get initial failed: %v", err)
	}
	initialGen := initial.Generation

	t.Run("SpecUpdateIncrementsGeneration", func(t *testing.T) {
		fresh := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gen-track"}, fresh); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		fresh.Spec.Mappings[0].PodSelector.MatchLabels["env"] = "prod"
		if err := k8sClient.Update(ctx, fresh); err != nil {
			t.Fatalf("Update failed: %v", err)
		}

		updated := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gen-track"}, updated); err != nil {
			t.Fatalf("Get after update failed: %v", err)
		}
		if updated.Generation <= initialGen {
			t.Errorf("generation should increment on spec change: got %d, initial was %d",
				updated.Generation, initialGen)
		}
	})

	t.Run("StatusUpdateDoesNotIncrementGeneration", func(t *testing.T) {
		fresh := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gen-track"}, fresh); err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		genBeforeStatus := fresh.Generation

		fresh.Status = v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: fresh.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "Synced",
					Message:            "all good",
				},
			},
		}
		if err := k8sClient.Status().Update(ctx, fresh); err != nil {
			t.Fatalf("Status().Update failed: %v", err)
		}

		afterStatus := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gen-track"}, afterStatus); err != nil {
			t.Fatalf("Get after status update failed: %v", err)
		}
		if afterStatus.Generation != genBeforeStatus {
			t.Errorf("generation should NOT change on status update: got %d, expected %d",
				afterStatus.Generation, genBeforeStatus)
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Concurrent multi-client CRD access
// Verifies that multiple independent clients can safely operate on
// PodASGMapping resources through the full scheme pipeline.
// ---------------------------------------------------------------------------

func TestIntegration_ConcurrentMultiClientAccess(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: concurrent multi-client CRD access")

	_, cfg, _, s := startFullEnvtest(t)
	ctx := context.Background()

	// Create three independent clients (simulating multiple controllers)
	clients := make([]client.Client, 3)
	for i := range clients {
		c, err := client.New(cfg, client.Options{Scheme: s})
		if err != nil {
			t.Fatalf("failed to create client %d: %v", i, err)
		}
		clients[i] = c
	}

	t.Run("ConcurrentCreatesInDifferentNamespaces", func(t *testing.T) {
		// Create namespaces
		for i := 0; i < 3; i++ {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("concurrent-ns-%d", i)}}
			if err := clients[0].Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("create namespace %d: %v", i, err)
			}
		}

		// Each client creates objects in its own namespace concurrently
		errCh := make(chan error, 3)
		for i := 0; i < 3; i++ {
			go func(idx int) {
				obj := newValidPodASGMapping(fmt.Sprintf("concurrent-ns-%d", idx), "concurrent-obj")
				errCh <- clients[idx].Create(ctx, obj)
			}(i)
		}

		for i := 0; i < 3; i++ {
			if err := <-errCh; err != nil {
				t.Errorf("concurrent create %d failed: %v", i, err)
			}
		}

		// Verify all objects exist
		for i := 0; i < 3; i++ {
			fetched := &v1alpha1.PodASGMapping{}
			if err := clients[i].Get(ctx, types.NamespacedName{
				Namespace: fmt.Sprintf("concurrent-ns-%d", i),
				Name:      "concurrent-obj",
			}, fetched); err != nil {
				t.Errorf("get from client %d failed: %v", i, err)
			}
		}
	})

	t.Run("CrossClientVisibility", func(t *testing.T) {
		// Client 0 creates an object
		obj := newValidPodASGMapping("default", "cross-client-vis")
		if err := clients[0].Create(ctx, obj); err != nil {
			t.Fatalf("client 0 create failed: %v", err)
		}

		// Client 1 and 2 should see it
		for i := 1; i <= 2; i++ {
			fetched := &v1alpha1.PodASGMapping{}
			if err := clients[i].Get(ctx, types.NamespacedName{
				Namespace: "default", Name: "cross-client-vis",
			}, fetched); err != nil {
				t.Errorf("client %d cannot see object created by client 0: %v", i, err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Integration: Error propagation — validation errors from API server use
// proper status error types that cross the client boundary correctly.
// ---------------------------------------------------------------------------

func TestIntegration_ValidationErrorPropagation(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Integration: validation error propagation across client boundary")

	_, _, k8sClient, _ := startFullEnvtest(t)
	ctx := context.Background()

	t.Run("InvalidResourceIDReturnsStatusError", func(t *testing.T) {
		obj := newValidPodASGMapping("default", "bad-rid")
		obj.Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = "invalid"

		err := k8sClient.Create(ctx, obj)
		if err == nil {
			t.Fatal("expected error for invalid resourceId, got nil")
		}

		// Verify the error is a proper StatusError (not a transport or generic error)
		statusErr, ok := err.(*apierrors.StatusError)
		if !ok {
			t.Fatalf("expected *StatusError, got %T: %v", err, err)
		}

		// Verify the status code is 422 Unprocessable Entity (Invalid)
		if statusErr.ErrStatus.Code != 422 {
			t.Errorf("status code: got %d, want 422", statusErr.ErrStatus.Code)
		}

		// Verify it has causes pointing to the invalid field
		if statusErr.ErrStatus.Details == nil || len(statusErr.ErrStatus.Details.Causes) == 0 {
			t.Error("expected status error to have field-level causes")
		}
	})

	t.Run("EmptyMappingsReturnsStatusError", func(t *testing.T) {
		obj := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "empty-map"},
			Spec:       v1alpha1.PodASGMappingSpec{Mappings: []v1alpha1.Mapping{}},
		}

		err := k8sClient.Create(ctx, obj)
		if err == nil {
			t.Fatal("expected error for empty mappings, got nil")
		}

		if !apierrors.IsInvalid(err) {
			t.Errorf("expected IsInvalid=true, got false for error: %v", err)
		}
	})

	t.Run("NotFoundReturnsProperError", func(t *testing.T) {
		fetched := &v1alpha1.PodASGMapping{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "does-not-exist"}, fetched)
		if err == nil {
			t.Fatal("expected NotFound error, got nil")
		}
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected IsNotFound=true, got false for error: %v", err)
		}
	})

	t.Run("ConflictOnStaleUpdate", func(t *testing.T) {
		// Create an object
		obj := newValidPodASGMapping("default", "conflict-test")
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("Create failed: %v", err)
		}

		// Get two copies
		copy1 := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "conflict-test"}, copy1); err != nil {
			t.Fatalf("Get copy1 failed: %v", err)
		}
		copy2 := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "conflict-test"}, copy2); err != nil {
			t.Fatalf("Get copy2 failed: %v", err)
		}

		// Update copy1
		copy1.Spec.Mappings[0].PodSelector.MatchLabels["env"] = "prod"
		if err := k8sClient.Update(ctx, copy1); err != nil {
			t.Fatalf("Update copy1 failed: %v", err)
		}

		// Update copy2 with stale resourceVersion — should conflict
		copy2.Spec.Mappings[0].PodSelector.MatchLabels["env"] = "staging"
		err := k8sClient.Update(ctx, copy2)
		if err == nil {
			t.Fatal("expected conflict error on stale update, got nil")
		}
		if !apierrors.IsConflict(err) {
			t.Errorf("expected IsConflict=true, got false for error: %v", err)
		}
	})
}
