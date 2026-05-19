package v1alpha1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
)

const (
	envtestK8sVersion = "1.31.x"
	crdName           = "podasgmappings.networking.azure.com"
)

var (
	envtestAssetsConfigured bool
	envtestSkipReason       = "envtest assets are not configured"
)

// TestMain points KUBEBUILDER_ASSETS at pre-fetched envtest binaries so the
// package test path stays local and does not acquire tools during go test.
func TestMain(m *testing.M) {
	oldAssets, hadOldAssets := os.LookupEnv("KUBEBUILDER_ASSETS")

	repoRoot, err := repoRootFromCWD()
	if err != nil {
		envtestSkipReason = fmt.Sprintf("failed to resolve repo root for envtest setup: %v", err)
		warnLog, _ := zap.NewProduction()
		warnLog.Warn("envtest setup issue; continuing so non-envtest tests can run", zap.String("reason", envtestSkipReason))
	} else if err := configureEnvtestAssets(repoRoot); err != nil {
		envtestSkipReason = fmt.Sprintf("failed to configure envtest assets: %v", err)
		warnLog, _ := zap.NewProduction()
		warnLog.Warn("envtest setup issue; continuing so non-envtest tests can run", zap.String("reason", envtestSkipReason))
	} else {
		envtestAssetsConfigured = true
	}

	code := m.Run()

	if hadOldAssets {
		_ = os.Setenv("KUBEBUILDER_ASSETS", oldAssets)
	} else {
		_ = os.Unsetenv("KUBEBUILDER_ASSETS")
	}

	os.Exit(code)
}

func requireEnvtestAssets(t *testing.T) {
	t.Helper()
	if !envtestAssetsConfigured {
		t.Skipf("skipping envtest-dependent test: %s", envtestSkipReason)
	}
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

	if err := os.Setenv("KUBEBUILDER_ASSETS", assetsPath); err != nil {
		return fmt.Errorf("set KUBEBUILDER_ASSETS: %w", err)
	}

	return nil
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

	return "", fmt.Errorf(
		"envtest assets for %s not found under %q; run 'make setup-envtest' first",
		envtestK8sVersion,
		assetsRoot,
	)
}

func hasEnvtestBinaries(assetsPath string) bool {
	for _, binary := range []string{"etcd", "kube-apiserver", "kubectl"} {
		if _, err := os.Stat(filepath.Join(assetsPath, binary)); err != nil {
			return false
		}
	}

	return true
}

// mustRepoRootFromThisFile returns the repository root by walking up from the
// test file directory. Fails the test if config/crd cannot be located.
func mustRepoRootFromThisFile(t *testing.T) string {
	t.Helper()
	// This test file lives at api/v1alpha1/types_test.go — repo root is two levels up.
	root, err := repoRootFromCWD()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	crdDir := filepath.Join(root, "config", "crd")
	if _, err := os.Stat(crdDir); os.IsNotExist(err) {
		t.Fatalf("CRD directory does not exist at %s — run 'make manifests' first", crdDir)
	}
	return root
}

// newValidPodASGMapping returns a PodASGMapping with all required fields populated.
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

func newValidPodASGMappingUnstructured(namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": v1alpha1.GroupVersion.String(),
			"kind":       "PodASGMapping",
			"metadata": map[string]any{
				"namespace": namespace,
				"name":      name,
			},
			"spec": map[string]any{
				"mappings": []any{
					map[string]any{
						"podSelector": map[string]any{
							"matchLabels": map[string]any{"app": "web"},
						},
						"applicationSecurityGroups": []any{
							map[string]any{
								"resourceId": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test",
							},
						},
					},
				},
			},
		},
	}
	obj.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("PodASGMapping"))
	return obj
}

// startPhase1Envtest bootstraps an envtest environment with the PodASGMapping CRD
// installed and returns the environment, rest config, and a typed client.
func startPhase1Envtest(t *testing.T) (*envtest.Environment, *rest.Config, client.Client) {
	t.Helper()
	requireEnvtestAssets(t)

	repoRoot := mustRepoRootFromThisFile(t)
	crdDir := filepath.Join(repoRoot, "config", "crd")
	log.SetLogger(zapr.NewLogger(zaptest.NewLogger(t)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}

	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("failed to create k8s client: %v", err)
	}

	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	})

	return testEnv, cfg, k8sClient
}

func getCRDVersion(
	t *testing.T,
	cfg *rest.Config,
	name string,
	version string,
) (*apiextensionsv1.CustomResourceDefinition, *apiextensionsv1.CustomResourceDefinitionVersion) {
	t.Helper()

	cs, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create apiextensions client: %v", err)
	}

	crd, err := cs.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), name, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("failed to get CRD %s: %v", name, err)
	}

	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == version {
			return crd, &crd.Spec.Versions[i]
		}
	}

	t.Fatalf("version %s not found in CRD %s", version, name)
	return nil, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func verbsForResourceInRBAC(content string, resource string) ([]string, bool) {
	lines := strings.Split(content, "\n")
	for i := range lines {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "resources:") || !strings.Contains(trimmed, fmt.Sprintf("%q", resource)) {
			continue
		}

		for j := i + 1; j < len(lines); j++ {
			trimmed = strings.TrimSpace(lines[j])
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "verbs:") {
				start := strings.Index(trimmed, "[")
				end := strings.LastIndex(trimmed, "]")
				if start == -1 || end == -1 || end <= start {
					return nil, false
				}

				rawVerbs := strings.Split(trimmed[start+1:end], ",")
				verbs := make([]string, 0, len(rawVerbs))
				for _, rawVerb := range rawVerbs {
					verb := strings.Trim(strings.TrimSpace(rawVerb), `"`)
					if verb != "" {
						verbs = append(verbs, verb)
					}
				}
				return verbs, true
			}
			if strings.HasPrefix(trimmed, "resources:") || strings.HasPrefix(trimmed, "- apiGroups:") || trimmed == "---" {
				break
			}
		}
	}

	return nil, false
}

// ---------------------------------------------------------------------------
// Test helpers — assertion functions per design spec
// ---------------------------------------------------------------------------

// assertInvalidFieldCause checks that err is non-nil, references expectedFieldContains,
// and contains all requiredMessageTokens. Avoids brittle full-string comparisons.
func assertInvalidFieldCause(t *testing.T, err error, expectedFieldContains string, requiredMessageTokens ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("expected validation error for field containing %q, but got nil", expectedFieldContains)
		return
	}

	if !apierrors.IsInvalid(err) {
		t.Errorf("expected invalid error for field containing %q, got: %v", expectedFieldContains, err)
	}

	errMsg := err.Error()
	fieldMatched := strings.Contains(errMsg, expectedFieldContains)

	statusErr, ok := err.(*apierrors.StatusError)
	if ok && statusErr.ErrStatus.Details != nil {
		for _, cause := range statusErr.ErrStatus.Details.Causes {
			if strings.Contains(cause.Field, expectedFieldContains) {
				fieldMatched = true
				for _, token := range requiredMessageTokens {
					if !strings.Contains(cause.Message, token) && !strings.Contains(errMsg, token) {
						t.Errorf("expected validation cause for %q to contain token %q, got: %s",
							expectedFieldContains, token, cause.Message)
					}
				}
				break
			}
		}
	}

	if !fieldMatched {
		t.Errorf("expected error to reference field %q, got: %s", expectedFieldContains, errMsg)
	}

	for _, token := range requiredMessageTokens {
		if !strings.Contains(errMsg, token) {
			t.Errorf("expected error to contain token %q, got: %s", token, errMsg)
		}
	}
}

// assertCRDEstablished verifies the named CRD exists and has Established=True condition.
func assertCRDEstablished(t *testing.T, cfg *rest.Config, name string) {
	t.Helper()
	cs, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create apiextensions client: %v", err)
	}

	ctx := context.Background()
	var lastErr error
	var lastConditions []apiextensionsv1.CustomResourceDefinitionCondition

	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		crd, getErr := cs.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			lastErr = getErr
			return false, nil
		}

		lastErr = nil
		lastConditions = crd.Status.Conditions
		established := false
		namesAccepted := false
		for _, c := range crd.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
			if c.Type == apiextensionsv1.NamesAccepted && c.Status == apiextensionsv1.ConditionTrue {
				namesAccepted = true
			}
		}

		return established && namesAccepted, nil
	})
	if err == nil {
		return
	}
	if lastErr != nil {
		t.Fatalf("failed waiting for CRD %s to become Established: %v", name, lastErr)
	}
	t.Fatalf("CRD %s did not become Established within timeout; last conditions: %+v", name, lastConditions)
}

// assertPrinterColumn verifies a printer column exists with the expected name, type,
// jsonPath, and optional description substring. Matches by name, not list index.
func assertPrinterColumn(
	t *testing.T,
	cols []apiextensionsv1.CustomResourceColumnDefinition,
	name string,
	colType string,
	jsonPath string,
	descriptionContains string,
) {
	t.Helper()
	for _, col := range cols {
		if col.Name == name {
			if col.Type != colType {
				t.Errorf("printer column %q: got type %q, want %q", name, col.Type, colType)
			}
			if col.JSONPath != jsonPath {
				t.Errorf("printer column %q: got jsonPath %q, want %q", name, col.JSONPath, jsonPath)
			}
			if descriptionContains != "" && !strings.Contains(col.Description, descriptionContains) {
				t.Errorf("printer column %q: description %q does not contain %q",
					name, col.Description, descriptionContains)
			}
			return
		}
	}
	t.Errorf("printer column %q not found in CRD columns (have %d columns)", name, len(cols))
}

// ---------------------------------------------------------------------------
// T1.1 — JSON round-trip: serialize PodASGMapping to JSON and back, expect equality.
// ---------------------------------------------------------------------------
func TestPhase1_T11_JSONRoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.1: JSON round-trip test")

	t.Run("FullyPopulatedObject", func(t *testing.T) {
		original := newValidPodASGMapping("default", "roundtrip")
		original.TypeMeta = metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "PodASGMapping",
		}
		original.Labels = map[string]string{"team": "networking"}
		original.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID = "00000000-0000-0000-0000-000000000000"
		original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "primary application security group"
		now := metav1.NewTime(time.Unix(1_700_000_000, 0).UTC())
		original.Status = v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Accepted",
					Status:             metav1.ConditionTrue,
					Reason:             "ValidationSucceeded",
					Message:            "mapping accepted",
					LastTransitionTime: now,
				},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{
					SelectorHash: "hash-1",
					MatchedPods:  3,
					ASGSyncState: "Synced",
					LastSyncTime: now,
					Error:        "transient sync warning",
				},
			},
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("failed to marshal PodASGMapping to JSON: %v", err)
		}

		restored := &v1alpha1.PodASGMapping{}
		if err := json.Unmarshal(data, restored); err != nil {
			t.Fatalf("failed to unmarshal PodASGMapping from JSON: %v", err)
		}

		restoredData, err := json.Marshal(restored)
		if err != nil {
			t.Fatalf("failed to marshal restored PodASGMapping to JSON: %v", err)
		}
		if !bytes.Equal(restoredData, data) {
			t.Errorf("T1.1 round-trip JSON mismatch: got %s, want %s", restoredData, data)
		}
		if got := restored.Status.MappingStatuses[0].Error; got != "transient sync warning" {
			t.Errorf("T1.1 mappingStatuses[0].error: got %q, want %q", got, "transient sync warning")
		}
	})

	t.Run("MinimalObjectOmitsEmptyFields", func(t *testing.T) {
		minimal := newValidPodASGMapping("default", "minimal-roundtrip")

		data, err := json.Marshal(minimal)
		if err != nil {
			t.Fatalf("failed to marshal minimal PodASGMapping to JSON: %v", err)
		}

		if bytes.Contains(data, []byte(`"subscriptionId"`)) {
			t.Errorf("expected minimal object JSON to omit subscriptionId, got %s", data)
		}
		if bytes.Contains(data, []byte(`"description"`)) {
			t.Errorf("expected minimal object JSON to omit description, got %s", data)
		}

		restored := &v1alpha1.PodASGMapping{}
		if err := json.Unmarshal(data, restored); err != nil {
			t.Fatalf("failed to unmarshal minimal PodASGMapping from JSON: %v", err)
		}
		if got := len(restored.Status.Conditions); got != 0 {
			t.Errorf("expected restored minimal object to have 0 conditions, got %d", got)
		}
		if got := len(restored.Status.MappingStatuses); got != 0 {
			t.Errorf("expected restored minimal object to have 0 mappingStatuses, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------------
// T1.2 — Empty spec.mappings rejected by minItems CRD validation.
// ---------------------------------------------------------------------------
func TestPhase1_T12_RejectsZeroMappingsViaAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.2: Zero mappings must be rejected by API server CRD validation")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	testCases := []struct {
		name     string
		mappings []v1alpha1.Mapping
	}{
		{
			name:     "nil mappings",
			mappings: nil,
		},
		{
			name:     "empty mappings",
			mappings: []v1alpha1.Mapping{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &v1alpha1.PodASGMapping{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      strings.ReplaceAll(tc.name, " ", "-"),
				},
				Spec: v1alpha1.PodASGMappingSpec{
					Mappings: tc.mappings,
				},
			}

			err := k8sClient.Create(ctx, obj)
			assertInvalidFieldCause(t, err, "mappings", "spec.mappings")
		})
	}
}

// ---------------------------------------------------------------------------
// T1.3 — Invalid resourceId pattern rejected by CRD validation.
// ---------------------------------------------------------------------------
func TestPhase1_T13_RejectsInvalidResourceIDViaAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.3: Invalid resourceId must be rejected by API server CRD validation")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	testCases := []struct {
		name       string
		resourceID string
	}{
		{
			name:       "arbitrary string",
			resourceID: "not-a-valid-azure-resource-id",
		},
		{
			name:       "missing leading slash",
			resourceID: "subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test",
		},
		{
			name:       "wrong resource type",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/networkSecurityGroups/asg-test",
		},
		{
			name:       "missing asg name",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := newValidPodASGMapping("default", strings.ReplaceAll(tc.name, " ", "-"))
			obj.Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = tc.resourceID

			err := k8sClient.Create(ctx, obj)
			assertInvalidFieldCause(t, err, "resourceId")
		})
	}
}

// ---------------------------------------------------------------------------
// T1.4 — Valid PodASGMapping accepted by API server.
// ---------------------------------------------------------------------------
func TestPhase1_T14_AcceptsValidObjectViaAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.4: Valid PodASGMapping must be accepted by API server")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	obj := newValidPodASGMapping("default", "valid-obj")
	obj.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID = "00000000-0000-0000-0000-000000000000"
	obj.Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "valid description"

	err := k8sClient.Create(ctx, obj)
	if err != nil {
		t.Errorf("T1.4: expected valid PodASGMapping to be accepted, but got error: %v", err)
	}

	fetched := &v1alpha1.PodASGMapping{}
	key := client.ObjectKey{Namespace: obj.Namespace, Name: obj.Name}
	if err := k8sClient.Get(ctx, key, fetched); err != nil {
		t.Fatalf("T1.4: expected created PodASGMapping to be retrievable, but got error: %v", err)
	}
	if fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID != obj.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID {
		t.Errorf("T1.4 subscriptionId: got %q, want %q",
			fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID,
			obj.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID)
	}
	if fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].Description != obj.Spec.Mappings[0].ApplicationSecurityGroups[0].Description {
		t.Errorf("T1.4 description: got %q, want %q",
			fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].Description,
			obj.Spec.Mappings[0].ApplicationSecurityGroups[0].Description)
	}
}

// ---------------------------------------------------------------------------
// T1.5 — CRD YAML registers cleanly in envtest cluster with correct shape.
// ---------------------------------------------------------------------------
func TestPhase1_T15_CRDYAMLRegistersInEnvtest(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.5: CRD YAML must apply cleanly with correct shape including printer columns")

	_, cfg, _ := startPhase1Envtest(t)

	// T1.5a — CRD is Established
	t.Run("CRD_Established", func(t *testing.T) {
		assertCRDEstablished(t, cfg, crdName)
	})

	// T1.5b — CRD file exists on disk
	t.Run("CRD_FileExists", func(t *testing.T) {
		repoRoot := mustRepoRootFromThisFile(t)
		crdFile := filepath.Join(repoRoot, "config", "crd", "podasgmapping.yaml")
		info, err := os.Stat(crdFile)
		if err != nil {
			t.Errorf("CRD file not found at %s: %v", crdFile, err)
		} else if info.Size() == 0 {
			t.Errorf("CRD file at %s is empty", crdFile)
		}
	})

	// T1.5c — Printer columns match spec exactly
	t.Run("PrinterColumns", func(t *testing.T) {
		_, version := getCRDVersion(t, cfg, crdName, v1alpha1.GroupVersion.Version)
		cols := version.AdditionalPrinterColumns
		if len(cols) != 2 {
			t.Errorf("expected exactly 2 printer columns, got %d", len(cols))
		}

		assertPrinterColumn(t, cols, "Mappings", "integer", ".status.mappingCount", "Number of mapping rules")
		assertPrinterColumn(t, cols, "Age", "date", ".metadata.creationTimestamp", "")
	})

	t.Run("Discovery", func(t *testing.T) {
		discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
		if err != nil {
			t.Fatalf("failed to create discovery client: %v", err)
		}
		resourceList, err := discoveryClient.ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
		if err != nil {
			t.Fatalf("failed to discover resources for %s: %v", v1alpha1.GroupVersion.String(), err)
		}

		found := false
		for _, resource := range resourceList.APIResources {
			if resource.Name == "podasgmappings" {
				found = true
				if resource.Kind != "PodASGMapping" {
					t.Errorf("discovered kind: got %q, want %q", resource.Kind, "PodASGMapping")
				}
				if !resource.Namespaced {
					t.Errorf("discovered resource should be namespaced")
				}
				if resource.SingularName != "podasgmapping" {
					t.Errorf("discovered singular name: got %q, want %q", resource.SingularName, "podasgmapping")
				}
				break
			}
		}
		if !found {
			t.Fatalf("podasgmappings resource not found in discovery for %s", v1alpha1.GroupVersion.String())
		}
	})

	t.Run("SchemaContract", func(t *testing.T) {
		crd, version := getCRDVersion(t, cfg, crdName, v1alpha1.GroupVersion.Version)
		if crd.Spec.Group != v1alpha1.GroupVersion.Group {
			t.Errorf("CRD group: got %q, want %q", crd.Spec.Group, v1alpha1.GroupVersion.Group)
		}
		if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
			t.Errorf("CRD scope: got %q, want %q", crd.Spec.Scope, apiextensionsv1.NamespaceScoped)
		}
		if version.Subresources == nil || version.Subresources.Status == nil {
			t.Errorf("expected status subresource to be enabled for %s", v1alpha1.GroupVersion.Version)
		}
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			t.Fatalf("expected openAPIV3 schema for version %s", v1alpha1.GroupVersion.Version)
		}

		rootSchema := version.Schema.OpenAPIV3Schema
		if !containsString(rootSchema.Required, "spec") {
			t.Errorf("root schema required fields: expected %q, got %v", "spec", rootSchema.Required)
		}

		specSchema := rootSchema.Properties["spec"]
		mappingsSchema := specSchema.Properties["mappings"]
		if mappingsSchema.MinItems == nil || *mappingsSchema.MinItems != 1 {
			t.Errorf("spec.mappings minItems: got %v, want %d", mappingsSchema.MinItems, 1)
		}

		mappingSchema := mappingsSchema.Items.Schema
		if mappingSchema == nil {
			t.Fatalf("expected schema for spec.mappings items")
		}

		podSelectorSchema := mappingSchema.Properties["podSelector"]
		if !containsString(podSelectorSchema.Required, "matchLabels") {
			t.Errorf("podSelector required fields: expected %q, got %v", "matchLabels", podSelectorSchema.Required)
		}

		asgListSchema := mappingSchema.Properties["applicationSecurityGroups"]
		if asgListSchema.MinItems == nil || *asgListSchema.MinItems != 1 {
			t.Errorf("applicationSecurityGroups minItems: got %v, want %d", asgListSchema.MinItems, 1)
		}

		asgReferenceSchema := asgListSchema.Items.Schema
		if asgReferenceSchema == nil {
			t.Fatalf("expected schema for applicationSecurityGroups items")
		}

		resourceIDSchema := asgReferenceSchema.Properties["resourceId"]
		wantPattern := "^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Network/applicationSecurityGroups/[^/]+$"
		if resourceIDSchema.Pattern != wantPattern {
			t.Errorf("resourceId pattern: got %q, want %q", resourceIDSchema.Pattern, wantPattern)
		}
	})

	// T1.5 — CRD resource names match spec (kind, plural, singular, listKind)
	t.Run("CRD_Names", func(t *testing.T) {
		crd, _ := getCRDVersion(t, cfg, crdName, v1alpha1.GroupVersion.Version)

		if got := crd.Spec.Names.Kind; got != "PodASGMapping" {
			t.Errorf("CRD names.kind: got %q, want %q", got, "PodASGMapping")
		}
		if got := crd.Spec.Names.Plural; got != "podasgmappings" {
			t.Errorf("CRD names.plural: got %q, want %q", got, "podasgmappings")
		}
		if got := crd.Spec.Names.Singular; got != "podasgmapping" {
			t.Errorf("CRD names.singular: got %q, want %q", got, "podasgmapping")
		}
		if got := crd.Spec.Names.ListKind; got != "PodASGMappingList" {
			t.Errorf("CRD names.listKind: got %q, want %q", got, "PodASGMappingList")
		}
	})

	// T1.5 — v1alpha1 version flags: served=true, storage=true
	t.Run("VersionServedAndStorage", func(t *testing.T) {
		_, version := getCRDVersion(t, cfg, crdName, v1alpha1.GroupVersion.Version)

		if !version.Served {
			t.Error("v1alpha1 version should be served")
		}
		if !version.Storage {
			t.Error("v1alpha1 version should be storage")
		}
	})
}

// ---------------------------------------------------------------------------
// T1.6 — Typed client CRUD with registered scheme.
// ---------------------------------------------------------------------------
func TestPhase1_T16_TypedClientCRUDWithRegisteredScheme(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.6: Typed client must support Create, Get, List, Delete")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()
	ns := "default"

	// --- CREATE ---
	obj := newValidPodASGMapping(ns, "crud-test")
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("T1.6 Create: unexpected error: %v", err)
	}

	// --- GET ---
	fetched := &v1alpha1.PodASGMapping{}
	key := client.ObjectKey{Namespace: ns, Name: "crud-test"}
	if err := k8sClient.Get(ctx, key, fetched); err != nil {
		t.Fatalf("T1.6 Get: unexpected error: %v", err)
	}
	if fetched.Name != "crud-test" {
		t.Errorf("T1.6 Get: got name %q, want %q", fetched.Name, "crud-test")
	}
	if len(fetched.Spec.Mappings) != 1 {
		t.Errorf("T1.6 Get: got %d mappings, want 1", len(fetched.Spec.Mappings))
	}

	// --- LIST ---
	list := &v1alpha1.PodASGMappingList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		t.Fatalf("T1.6 List: unexpected error: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("T1.6 List: got %d items, want %d", len(list.Items), 1)
	}

	// --- DELETE ---
	if err := k8sClient.Delete(ctx, obj); err != nil {
		t.Fatalf("T1.6 Delete: unexpected error: %v", err)
	}

	// Verify deletion
	err := k8sClient.Get(ctx, key, &v1alpha1.PodASGMapping{})
	if err == nil {
		t.Errorf("T1.6 Delete verification: expected NotFound error after Delete, but Get succeeded")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("T1.6 Delete verification: expected NotFound error, got %v", err)
	}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		t.Fatalf("T1.6 List after delete: unexpected error: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("T1.6 List after delete: got %d items, want %d", len(list.Items), 0)
	}
}

func TestPhase1_DeepCopyPreservesNestedFields(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: generated deepcopy should isolate nested fields")

	now := metav1.NewTime(time.Unix(1_700_000_100, 0).UTC())
	original := newValidPodASGMapping("default", "deepcopy")
	original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "original description"
	original.Status = v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Initial",
				Message:            "ready",
				LastTransitionTime: now,
			},
		},
		MappingStatuses: []v1alpha1.MappingStatus{
			{
				SelectorHash: "hash-1",
				MatchedPods:  1,
				ASGSyncState: "Synced",
				LastSyncTime: now,
			},
		},
	}

	t.Run("PodASGMapping", func(t *testing.T) {
		deepCopied := original.DeepCopy()
		if deepCopied == nil {
			t.Fatalf("DeepCopy returned nil")
		}
		if deepCopied == original {
			t.Errorf("DeepCopy returned the same pointer")
		}

		deepCopied.Spec.Mappings[0].PodSelector.MatchLabels["app"] = "api"
		deepCopied.Spec.Mappings[0].ApplicationSecurityGroups[0].Description = "changed description"
		deepCopied.Status.Conditions[0].Reason = "Mutated"
		deepCopied.Status.MappingStatuses[0].ASGSyncState = "Pending"

		if original.Spec.Mappings[0].PodSelector.MatchLabels["app"] != "web" {
			t.Errorf("original pod selector was mutated: got %q, want %q",
				original.Spec.Mappings[0].PodSelector.MatchLabels["app"], "web")
		}
		if original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description != "original description" {
			t.Errorf("original ASG description was mutated: got %q, want %q",
				original.Spec.Mappings[0].ApplicationSecurityGroups[0].Description, "original description")
		}
		if original.Status.Conditions[0].Reason != "Initial" {
			t.Errorf("original condition reason was mutated: got %q, want %q",
				original.Status.Conditions[0].Reason, "Initial")
		}
		if original.Status.MappingStatuses[0].ASGSyncState != "Synced" {
			t.Errorf("original mapping status was mutated: got %q, want %q",
				original.Status.MappingStatuses[0].ASGSyncState, "Synced")
		}
	})

	t.Run("PodASGMappingList", func(t *testing.T) {
		list := &v1alpha1.PodASGMappingList{
			Items: []v1alpha1.PodASGMapping{*original},
		}

		deepCopied := list.DeepCopy()
		if deepCopied == nil {
			t.Fatalf("PodASGMappingList DeepCopy returned nil")
		}
		if deepCopied == list {
			t.Errorf("PodASGMappingList DeepCopy returned the same pointer")
		}

		deepCopied.Items[0].Spec.Mappings[0].PodSelector.MatchLabels["app"] = "batch"
		if list.Items[0].Spec.Mappings[0].PodSelector.MatchLabels["app"] != "web" {
			t.Errorf("original list item was mutated: got %q, want %q",
				list.Items[0].Spec.Mappings[0].PodSelector.MatchLabels["app"], "web")
		}
	})
}

// ---------------------------------------------------------------------------
// T1.3 extended — Additional resourceId pattern edge cases not in the base table.
// ---------------------------------------------------------------------------
func TestPhase1_T13_ResourceIDPatternAdditionalEdgeCases(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.3 extended: Additional resourceId pattern boundary testing")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		resourceID string
		wantErr    bool
	}{
		{
			name:       "empty string",
			resourceID: "",
			wantErr:    true,
		},
		{
			name:       "trailing slash",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test/",
			wantErr:    true,
		},
		{
			name:       "extra path segment after asg name",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test/extra",
			wantErr:    true,
		},
		{
			name:       "wrong provider namespace Microsoft.Compute",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Compute/applicationSecurityGroups/asg-test",
			wantErr:    true,
		},
		{
			name:       "lowercase microsoft.network provider namespace",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/microsoft.network/applicationSecurityGroups/asg-test",
			wantErr:    true,
		},
		{
			name:       "uppercase MICROSOFT.NETWORK provider namespace",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/MICROSOFT.NETWORK/applicationSecurityGroups/asg-test",
			wantErr:    true,
		},
		{
			name:       "missing resourceGroups segment",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/providers/Microsoft.Network/applicationSecurityGroups/asg-test",
			wantErr:    true,
		},
		{
			name:       "bare asg name without path",
			resourceID: "my-asg",
			wantErr:    true,
		},
		{
			name:       "url instead of arm path",
			resourceID: "https://management.azure.com/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-test",
			wantErr:    true,
		},
		{
			name:       "valid with hyphens underscores and numbers in names",
			resourceID: "/subscriptions/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/resourceGroups/rg-my_group-123/providers/Microsoft.Network/applicationSecurityGroups/asg-web_frontend-01",
			wantErr:    false,
		},
		{
			name:       "valid minimal subscription guid",
			resourceID: "/subscriptions/a/resourceGroups/b/providers/Microsoft.Network/applicationSecurityGroups/c",
			wantErr:    false,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := newValidPodASGMapping("default", fmt.Sprintf("rid-ext-edge-%d", i))
			obj.Spec.Mappings[0].ApplicationSecurityGroups[0].ResourceID = tc.resourceID

			err := k8sClient.Create(ctx, obj)
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error for resourceId %q, but create succeeded", tc.resourceID)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected resourceId %q to be accepted, but got error: %v", tc.resourceID, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T1.2 extended — Empty applicationSecurityGroups list rejected by minItems=1.
// ---------------------------------------------------------------------------
func TestPhase1_T12_RejectsEmptyASGListViaAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.2 extended: Empty applicationSecurityGroups list must be rejected")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	obj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "empty-asg-list",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{}, // empty — should be rejected by minItems=1
				},
			},
		},
	}

	err := k8sClient.Create(ctx, obj)
	assertInvalidFieldCause(t, err, "applicationSecurityGroups")
}

// ---------------------------------------------------------------------------
// Phase 1 extended — Required CRD fields are rejected when omitted entirely.
// ---------------------------------------------------------------------------
func TestPhase1_RejectsMissingRequiredFieldsViaAPIServer(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1 extended: missing required CRD fields must be rejected by API server validation")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	testCases := []struct {
		name          string
		expectedField string
		mutate        func(obj *unstructured.Unstructured)
	}{
		{
			name:          "missing spec",
			expectedField: "spec",
			mutate: func(obj *unstructured.Unstructured) {
				delete(obj.Object, "spec")
			},
		},
		{
			name:          "missing mappings field",
			expectedField: "mappings",
			mutate: func(obj *unstructured.Unstructured) {
				spec := obj.Object["spec"].(map[string]any)
				delete(spec, "mappings")
			},
		},
		{
			name:          "missing matchLabels",
			expectedField: "matchLabels",
			mutate: func(obj *unstructured.Unstructured) {
				spec := obj.Object["spec"].(map[string]any)
				mappings := spec["mappings"].([]any)
				firstMapping := mappings[0].(map[string]any)
				podSelector := firstMapping["podSelector"].(map[string]any)
				delete(podSelector, "matchLabels")
			},
		},
		{
			name:          "missing resourceId",
			expectedField: "resourceId",
			mutate: func(obj *unstructured.Unstructured) {
				spec := obj.Object["spec"].(map[string]any)
				mappings := spec["mappings"].([]any)
				firstMapping := mappings[0].(map[string]any)
				asgRefs := firstMapping["applicationSecurityGroups"].([]any)
				firstASG := asgRefs[0].(map[string]any)
				delete(firstASG, "resourceId")
			},
		},
	}

	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			obj := newValidPodASGMappingUnstructured("default", fmt.Sprintf("missing-required-%d", i))
			tc.mutate(obj)

			err := k8sClient.Create(ctx, obj)
			assertInvalidFieldCause(t, err, tc.expectedField, "Required value")
		})
	}
}

// ---------------------------------------------------------------------------
// T1.4 extended — Valid multi-mapping with cross-subscription ASG references.
// ---------------------------------------------------------------------------
func TestPhase1_T14_AcceptsMultiMappingWithCrossSubscription(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.4 extended: Multi-mapping object with cross-subscription ASG references accepted")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()

	obj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "multi-mapping-cross-sub",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web", "tier": "frontend"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{
							ResourceID:     "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-one/providers/Microsoft.Network/applicationSecurityGroups/asg-web",
							SubscriptionID: "11111111-1111-1111-1111-111111111111",
							Description:    "Web frontend ASG in subscription 1",
						},
						{
							ResourceID:     "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg-two/providers/Microsoft.Network/applicationSecurityGroups/asg-shared",
							SubscriptionID: "22222222-2222-2222-2222-222222222222",
						},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "api"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{
							ResourceID:  "/subscriptions/33333333-3333-3333-3333-333333333333/resourceGroups/rg-three/providers/Microsoft.Network/applicationSecurityGroups/asg-api",
							Description: "API backend ASG in subscription 3",
						},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("expected multi-mapping cross-subscription PodASGMapping to be accepted, got error: %v", err)
	}

	// Verify the object round-trips correctly through the API server
	fetched := &v1alpha1.PodASGMapping{}
	key := client.ObjectKey{Namespace: "default", Name: "multi-mapping-cross-sub"}
	if err := k8sClient.Get(ctx, key, fetched); err != nil {
		t.Fatalf("failed to get created multi-mapping object: %v", err)
	}
	if got := len(fetched.Spec.Mappings); got != 2 {
		t.Errorf("fetched mappings count: got %d, want 2", got)
	}
	if got := len(fetched.Spec.Mappings[0].ApplicationSecurityGroups); got != 2 {
		t.Errorf("mapping[0] ASG count: got %d, want 2", got)
	}
	if got := fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].SubscriptionID; got != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("mapping[0] ASG[0] SubscriptionID: got %q, want %q", got, "11111111-1111-1111-1111-111111111111")
	}
	if got := fetched.Spec.Mappings[0].ApplicationSecurityGroups[0].Description; got != "Web frontend ASG in subscription 1" {
		t.Errorf("mapping[0] ASG[0] Description: got %q, want %q", got, "Web frontend ASG in subscription 1")
	}
	if got := fetched.Spec.Mappings[1].ApplicationSecurityGroups[0].Description; got != "API backend ASG in subscription 3" {
		t.Errorf("mapping[1] ASG[0] Description: got %q, want %q", got, "API backend ASG in subscription 3")
	}
}

// ---------------------------------------------------------------------------
// T1.6 extended — Update spec and write to status subresource.
// ---------------------------------------------------------------------------
func TestPhase1_T16_UpdateAndStatusSubresource(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("T1.6 extended: Update existing object and write to status subresource")

	_, _, k8sClient := startPhase1Envtest(t)
	ctx := context.Background()
	ns := "default"

	// Create an initial object
	obj := newValidPodASGMapping(ns, "update-status-test")
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}

	key := client.ObjectKey{Namespace: ns, Name: "update-status-test"}

	t.Run("Update_Spec", func(t *testing.T) {
		// Get fresh copy for update
		fetched := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, key, fetched); err != nil {
			t.Fatalf("Get before update: unexpected error: %v", err)
		}

		// Add a second mapping
		fetched.Spec.Mappings = append(fetched.Spec.Mappings, v1alpha1.Mapping{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "api"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{
					ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-test/providers/Microsoft.Network/applicationSecurityGroups/asg-api",
				},
			},
		})

		if err := k8sClient.Update(ctx, fetched); err != nil {
			t.Fatalf("Update: unexpected error: %v", err)
		}

		// Verify update persisted
		updated := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, key, updated); err != nil {
			t.Fatalf("Get after update: unexpected error: %v", err)
		}
		if got := len(updated.Spec.Mappings); got != 2 {
			t.Errorf("after Update: got %d mappings, want 2", got)
		}
	})

	t.Run("StatusSubresource_Write", func(t *testing.T) {
		// Get fresh copy for status update
		fresh := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, key, fresh); err != nil {
			t.Fatalf("Get for status update: unexpected error: %v", err)
		}

		fresh.Status = v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: fresh.Generation,
					LastTransitionTime: metav1.Now(),
					Reason:             "Synced",
					Message:            "All ASGs in sync",
				},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{
					SelectorHash: "hash-abc",
					MatchedPods:  3,
					ASGSyncState: "Synced",
					LastSyncTime: metav1.Now(),
				},
			},
		}

		if err := k8sClient.Status().Update(ctx, fresh); err != nil {
			t.Fatalf("Status().Update: unexpected error: %v", err)
		}

		// Verify status persisted
		withStatus := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, key, withStatus); err != nil {
			t.Fatalf("Get after status update: unexpected error: %v", err)
		}
		if got := len(withStatus.Status.Conditions); got != 1 {
			t.Fatalf("after Status Update: got %d conditions, want 1", got)
		}
		if got := withStatus.Status.Conditions[0].Type; got != "Ready" {
			t.Errorf("Status condition type: got %q, want %q", got, "Ready")
		}
		if got := len(withStatus.Status.MappingStatuses); got != 1 {
			t.Fatalf("after Status Update: got %d mappingStatuses, want 1", got)
		}
		if got := withStatus.Status.MappingStatuses[0].MatchedPods; got != 3 {
			t.Errorf("MappingStatus matchedPods: got %d, want 3", got)
		}
	})

	t.Run("StatusWriteDoesNotClobberSpec", func(t *testing.T) {
		// After a status write, spec should be unchanged
		final := &v1alpha1.PodASGMapping{}
		if err := k8sClient.Get(ctx, key, final); err != nil {
			t.Fatalf("Get final: unexpected error: %v", err)
		}
		// Spec should still have 2 mappings from Update_Spec
		if got := len(final.Spec.Mappings); got != 2 {
			t.Errorf("spec should be unchanged after status update: got %d mappings, want 2", got)
		}
		// Status should still be there from StatusSubresource_Write
		if got := len(final.Status.Conditions); got != 1 {
			t.Errorf("status conditions should persist: got %d, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 1 — GroupVersion constants match spec-required values.
// ---------------------------------------------------------------------------
func TestPhase1_GroupVersionConstants(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: GroupVersion constants validation")

	if got := v1alpha1.GroupVersion.Group; got != "networking.azure.com" {
		t.Errorf("GroupVersion.Group: got %q, want %q", got, "networking.azure.com")
	}
	if got := v1alpha1.GroupVersion.Version; got != "v1alpha1" {
		t.Errorf("GroupVersion.Version: got %q, want %q", got, "v1alpha1")
	}
	if got := v1alpha1.GroupVersion.String(); got != "networking.azure.com/v1alpha1" {
		t.Errorf("GroupVersion.String(): got %q, want %q", got, "networking.azure.com/v1alpha1")
	}
}

// ---------------------------------------------------------------------------
// Phase 1 — AddToScheme registers expected GVKs for typed client usage.
// ---------------------------------------------------------------------------
func TestPhase1_SchemeRegistrationGVKs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: Scheme registration GVK validation")

	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme failed: %v", err)
	}

	t.Run("PodASGMapping_GVK", func(t *testing.T) {
		gvks, _, err := s.ObjectKinds(&v1alpha1.PodASGMapping{})
		if err != nil {
			t.Fatalf("ObjectKinds(PodASGMapping) error: %v", err)
		}
		found := false
		for _, gvk := range gvks {
			if gvk.Group == "networking.azure.com" && gvk.Version == "v1alpha1" && gvk.Kind == "PodASGMapping" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected GVK networking.azure.com/v1alpha1/PodASGMapping not found in %v", gvks)
		}
	})

	t.Run("PodASGMappingList_GVK", func(t *testing.T) {
		gvks, _, err := s.ObjectKinds(&v1alpha1.PodASGMappingList{})
		if err != nil {
			t.Fatalf("ObjectKinds(PodASGMappingList) error: %v", err)
		}
		found := false
		for _, gvk := range gvks {
			if gvk.Group == "networking.azure.com" && gvk.Version == "v1alpha1" && gvk.Kind == "PodASGMappingList" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected GVK networking.azure.com/v1alpha1/PodASGMappingList not found in %v", gvks)
		}
	})

	t.Run("NewFromGVK", func(t *testing.T) {
		// Scheme.New should create the correct typed object from GVK
		obj, err := s.New(v1alpha1.GroupVersion.WithKind("PodASGMapping"))
		if err != nil {
			t.Fatalf("Scheme.New(PodASGMapping) error: %v", err)
		}
		if _, ok := obj.(*v1alpha1.PodASGMapping); !ok {
			t.Errorf("Scheme.New returned %T, want *v1alpha1.PodASGMapping", obj)
		}
	})

	t.Run("AddToSchemeIdempotent", func(t *testing.T) {
		// Calling AddToScheme twice should not error or corrupt the scheme
		if err := v1alpha1.AddToScheme(s); err != nil {
			t.Fatalf("second AddToScheme call failed: %v", err)
		}
		gvks, _, err := s.ObjectKinds(&v1alpha1.PodASGMapping{})
		if err != nil {
			t.Fatalf("ObjectKinds after second AddToScheme: %v", err)
		}
		if len(gvks) == 0 {
			t.Error("no GVKs after second AddToScheme")
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 1 — RBAC manifest covers podasgmappings with correct verbs.
// ---------------------------------------------------------------------------
func TestPhase1_RBACManifestCoverage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: RBAC manifest validation")

	repoRoot := mustRepoRootFromThisFile(t)
	rbacPath := filepath.Join(repoRoot, "config", "rbac", "rbac.yaml")

	data, err := os.ReadFile(rbacPath)
	if err != nil {
		t.Fatalf("failed to read RBAC manifest at %s: %v", rbacPath, err)
	}
	content := string(data)

	t.Run("ReferencesPodasgmappings", func(t *testing.T) {
		if !strings.Contains(content, "podasgmappings") {
			t.Error("RBAC manifest does not reference podasgmappings resource")
		}
	})

	t.Run("ReferencesStatusSubresource", func(t *testing.T) {
		if !strings.Contains(content, "podasgmappings/status") {
			t.Error("RBAC manifest does not reference podasgmappings/status resource")
		}
	})

	t.Run("ReferencesAPIGroup", func(t *testing.T) {
		if !strings.Contains(content, "networking.azure.com") {
			t.Error("RBAC manifest does not reference networking.azure.com API group")
		}
	})

	t.Run("MainResourceVerbs", func(t *testing.T) {
		verbs, ok := verbsForResourceInRBAC(content, "podasgmappings")
		if !ok {
			t.Fatal("RBAC manifest missing verbs for podasgmappings resource")
		}
		for _, verb := range []string{"get", "list", "watch"} {
			if !containsString(verbs, verb) {
				t.Errorf("RBAC manifest missing required verb %q for podasgmappings", verb)
			}
		}
	})

	t.Run("StatusSubresourceVerbs", func(t *testing.T) {
		verbs, ok := verbsForResourceInRBAC(content, "podasgmappings/status")
		if !ok {
			t.Fatal("RBAC manifest missing verbs for podasgmappings/status resource")
		}
		for _, verb := range []string{"get", "patch", "update"} {
			if !containsString(verbs, verb) {
				t.Errorf("RBAC manifest missing required verb %q for podasgmappings/status", verb)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 1 — CRD YAML file contains expected structural anchors.
// ---------------------------------------------------------------------------
func TestPhase1_CRDYAMLFileStructure(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: CRD YAML file structural validation")

	repoRoot := mustRepoRootFromThisFile(t)
	crdPath := filepath.Join(repoRoot, "config", "crd", "podasgmapping.yaml")

	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("failed to read CRD YAML at %s: %v", crdPath, err)
	}
	content := string(data)

	requiredAnchors := []struct {
		name   string
		substr string
	}{
		{"apiVersion", "apiextensions.k8s.io/v1"},
		{"kind", "kind: CustomResourceDefinition"},
		{"CRD name", "name: podasgmappings.networking.azure.com"},
		{"group", "group: networking.azure.com"},
		{"scope", "scope: Namespaced"},
		{"version name", "name: v1alpha1"},
		{"served flag", "served: true"},
		{"storage flag", "storage: true"},
		{"status subresource", "status: {}"},
		{"minItems on mappings", "minItems: 1"},
		{"resourceId pattern", "pattern:"},
		{"spec required", "- spec"},
		{"matchLabels required", "- matchLabels"},
		{"resourceId required", "- resourceId"},
	}

	for _, anchor := range requiredAnchors {
		t.Run(anchor.name, func(t *testing.T) {
			if !strings.Contains(content, anchor.substr) {
				t.Errorf("CRD YAML missing expected content %q (%s)", anchor.substr, anchor.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 1 — Scheme registration in cmd/main.go pattern is reproducible.
// This test verifies that the scheme used in main.go (clientgoscheme +
// corev1 + v1alpha1) can be assembled and recognizes PodASGMapping.
// ---------------------------------------------------------------------------
func TestPhase1_MainSchemePattern(t *testing.T) {
	logger := zaptest.NewLogger(t)
	logger.Info("Phase 1: cmd/main.go scheme pattern validation")

	// Reproduce the same scheme setup as cmd/main.go init()
	s := runtime.NewScheme()

	// These mirror cmd/main.go's init() calls
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme(clientgoscheme) failed: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme(corev1) failed: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme(v1alpha1) failed: %v", err)
	}

	// Verify a core type from main.go's scheme composition is known.
	if _, err := s.New(corev1.SchemeGroupVersion.WithKind("Pod")); err != nil {
		t.Fatalf("Scheme.New(Pod) failed: %v", err)
	}

	// Verify PodASGMapping is known to the scheme
	obj, err := s.New(v1alpha1.GroupVersion.WithKind("PodASGMapping"))
	if err != nil {
		t.Fatalf("Scheme.New(PodASGMapping) failed: %v", err)
	}
	pam, ok := obj.(*v1alpha1.PodASGMapping)
	if !ok {
		t.Fatalf("Scheme.New returned %T, want *v1alpha1.PodASGMapping", obj)
	}

	// Verify the created object implements runtime.Object (DeepCopyObject)
	copied := pam.DeepCopyObject()
	if copied == nil {
		t.Error("DeepCopyObject returned nil")
	}
	if _, ok := copied.(*v1alpha1.PodASGMapping); !ok {
		t.Errorf("DeepCopyObject returned %T, want *v1alpha1.PodASGMapping", copied)
	}
}
