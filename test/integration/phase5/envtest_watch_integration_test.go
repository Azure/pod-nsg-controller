package phase5_test

import (
	"context"
	"encoding/json"
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
	"github.com/Azure/pod-nsg-controller/internal/model"
	"github.com/go-logr/zapr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"go.uber.org/zap/zaptest"
)

const phase5EnvtestK8sVersion = "1.31.x"

func TestMain(m *testing.M) {
	repoRoot, err := phase5RepoRootFromCWD()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve repo root: %v\n", err)
		os.Exit(1)
	}

	oldAssets, hadOldAssets := os.LookupEnv("KUBEBUILDER_ASSETS")
	if err := configurePhase5EnvtestAssets(repoRoot); err != nil {
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

func phase5RepoRootFromCWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", "..")), nil
}

func configurePhase5EnvtestAssets(repoRoot string) error {
	if assets := strings.TrimSpace(os.Getenv("KUBEBUILDER_ASSETS")); assets != "" {
		if _, err := os.Stat(assets); err != nil {
			return fmt.Errorf("stat KUBEBUILDER_ASSETS %q: %w", assets, err)
		}
		return nil
	}

	assetsPath, err := phase5LocalEnvtestAssetsPath(repoRoot)
	if err != nil {
		return err
	}
	return os.Setenv("KUBEBUILDER_ASSETS", assetsPath)
}

func phase5LocalEnvtestAssetsPath(repoRoot string) (string, error) {
	assetsRoot := filepath.Join(repoRoot, "bin", "k8s")
	entries, err := os.ReadDir(assetsRoot)
	if err != nil {
		return "", fmt.Errorf("read envtest assets directory %q: %w", assetsRoot, err)
	}

	versionPrefix := strings.TrimSuffix(phase5EnvtestK8sVersion, ".x") + "."
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), versionPrefix) {
			continue
		}

		assetsPath := filepath.Join(assetsRoot, entry.Name())
		if phase5HasEnvtestBinaries(assetsPath) {
			return assetsPath, nil
		}
	}

	return "", fmt.Errorf("envtest assets for %s not found under %q", phase5EnvtestK8sVersion, assetsRoot)
}

func phase5HasEnvtestBinaries(assetsPath string) bool {
	for _, binary := range []string{"etcd", "kube-apiserver", "kubectl"} {
		if _, err := os.Stat(filepath.Join(assetsPath, binary)); err != nil {
			return false
		}
	}
	return true
}

func startPhase5Envtest(t *testing.T) (*envtest.Environment, *rest.Config, client.Client, *runtime.Scheme) {
	t.Helper()

	repoRoot, err := phase5RepoRootFromCWD()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
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
		t.Fatalf("start envtest: %v", err)
	}

	scheme := newScheme()
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create envtest client: %v", err)
	}

	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	return testEnv, cfg, k8sClient, scheme
}

func TestIntegration_Phase5_EnvtestPodLabelChangeAway_TriggersWatchReconcile(t *testing.T) {
	t.Run("update away from match deletes persisted prefix set", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := zaptest.NewLogger(t)
		subscriptionID := "sub-001"
		resourceGroup := "rg-net"
		asgName := "asg-web"
		namespaceName := "phase5-watch"
		mappingName := "web-map"
		prefixSetName := model.OwnershipKey("test-cluster", namespaceName, mappingName)

		_, cfg, directClient, scheme := startPhase5Envtest(t)

		fakeAzureClient := fake.NewClient()
		factory := fake.NewClientFactory()
		factory.RegisterClient(subscriptionID, fakeAzureClient)
		executor := azure.NewExecutor(logger, factory, 4)

		reconciler := &controller.MappingReconciler{
			Client:         nil,
			Scheme:         scheme,
			ClusterName:    "test-cluster",
			ResyncInterval: 10 * time.Minute,
			Factory:        factory,
			Executor:       executor,
		}

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		if err != nil {
			t.Fatalf("create manager: %v", err)
		}
		reconciler.Client = mgr.GetClient()
		if err := reconciler.SetupWithManager(mgr); err != nil {
			t.Fatalf("setup reconciler with manager: %v", err)
		}

		ownedTargets, err := json.Marshal([]controller.OwnedTarget{{
			ResourceID:    asgResourceID(subscriptionID, resourceGroup, asgName),
			PrefixSetName: prefixSetName,
		}})
		if err != nil {
			t.Fatalf("marshal owned targets: %v", err)
		}

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
		if err := directClient.Create(ctx, namespace); err != nil {
			t.Fatalf("create namespace: %v", err)
		}

		mapping := makeMapping(namespaceName, mappingName, []v1alpha1.Mapping{{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(subscriptionID, resourceGroup, asgName)},
			},
		}})
		mapping.Finalizers = []string{controller.MappingCleanupFinalizer}
		mapping.Annotations = map[string]string{
			controller.OwnedTargetsAnnotation: string(ownedTargets),
		}
		if err := directClient.Create(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		pod := makePod(namespaceName, "web-1", map[string]string{"app": "web"}, "10.0.0.1")
		pod.Spec.Containers = []corev1.Container{{
			Name:  "app",
			Image: "example.invalid/test:latest",
		}}
		if err := directClient.Create(ctx, pod); err != nil {
			t.Fatalf("create pod: %v", err)
		}

		if err := fakeAzureClient.Put(ctx, subscriptionID, resourceGroup, asgName, prefixSetName, []string{"10.0.0.1"}); err != nil {
			t.Fatalf("seed Azure prefix set: %v", err)
		}

		startErrCh := make(chan error, 1)
		go func() {
			startErrCh <- mgr.Start(ctx)
		}()

		if !mgr.GetCache().WaitForCacheSync(ctx) {
			select {
			case err := <-startErrCh:
				t.Fatalf("manager start failed: %v", err)
			default:
				t.Fatal("manager cache sync failed")
			}
		}

		var currentPod corev1.Pod
		if err := directClient.Get(ctx, types.NamespacedName{Namespace: namespaceName, Name: "web-1"}, &currentPod); err != nil {
			t.Fatalf("get pod before update: %v", err)
		}
		currentPod.Labels = map[string]string{"app": "db"}
		if err := directClient.Update(ctx, &currentPod); err != nil {
			t.Fatalf("update pod labels away from match: %v", err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			_, err := fakeAzureClient.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
			if azure.IsNotFound(err) {
				return
			}
			if err != nil {
				t.Fatalf("get prefix set after pod update: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}

		prefixSet, err := fakeAzureClient.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
		if err != nil {
			t.Fatalf("prefix set should have been deleted after pod update, got error: %v", err)
		}
		t.Errorf("expected watch-driven reconcile to delete prefix set after labels changed away; remaining prefixes: %v", prefixSet.Properties.AddressPrefixes)
	})
}
