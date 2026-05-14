package phase7_test

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
	"github.com/Azure/pod-nsg-controller/internal/testing/controllertest"
	"github.com/go-logr/zapr"
	"github.com/pkg/errors"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/pod-nsg-controller/test/integration/testutil"
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

type testEnvWithManager struct {
	env         *envtest.Environment
	k8sClient   client.Client
	cancel      context.CancelFunc
	scheme      *runtime.Scheme
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

	executor := azure.NewExecutor(zapLog, fakeFactory, 5)

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
		env:         env,
		k8sClient:   mgr.GetClient(),
		cancel:      cancel,
		scheme:      scheme,
		mgr:         mgr,
		fakeClient:  fakeAzClient,
		fakeFactory: fakeFactory,
	}
}

func teardownEnv(t *testing.T, te *testEnvWithManager) {
	t.Helper()
	te.cancel()
	if err := te.env.Stop(); err != nil {
		t.Errorf("failed to stop envtest: %v", err)
	}
}

// waitForCondition polls until a condition is true or timeout expires.
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

// ---------------------------------------------------------------------------
// Test 1: Executor → FakeClient Error Injection → Error Classification Pipeline
//
// Integration: azure.Executor + fake.Client + controller.ClassifyActionResults
// + controller.DecideRequeueFromActionSummary
//
// Verifies that errors injected at the Azure client level flow correctly
// through the executor and are properly classified by the controller layer.
// ---------------------------------------------------------------------------

func TestExecutor_ErrorClassification_Pipeline(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	executor := azure.NewExecutor(zapLog, factory, 5)

	target := engine.ASGTarget{
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "test-cluster-ns-map",
	}

	tests := []struct {
		name              string
		injectedErr       error
		wantRetriable     int
		wantNonRetriable  int
		wantFailureReason string
	}{
		{
			name: "429_TooManyRequests_classified_as_retriable",
			injectedErr: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				Message:    "Rate limit exceeded",
				RetryAfter: 30 * time.Second,
			},
			wantRetriable:     1,
			wantNonRetriable:  0,
			wantFailureReason: "429",
		},
		{
			name: "500_ServerError_classified_as_retriable",
			injectedErr: &azure.ARMStatusError{
				StatusCode: 500,
				ARMCode:    "InternalServerError",
				Message:    "Internal error",
			},
			wantRetriable:     1,
			wantNonRetriable:  0,
			wantFailureReason: "5xx",
		},
		{
			name: "403_Forbidden_classified_as_non_retriable",
			injectedErr: &azure.ARMStatusError{
				StatusCode: 403,
				ARMCode:    "AuthorizationFailed",
				Message:    "Forbidden",
			},
			wantRetriable:     0,
			wantNonRetriable:  1,
			wantFailureReason: "403",
		},
		{
			name: "404_ParentASGNotFound_classified_as_non_retriable",
			injectedErr: &azure.ARMStatusError{
				StatusCode: 404,
				ARMCode:    "ApplicationSecurityGroupNotFound",
				Message:    "ASG not found",
			},
			wantRetriable:     0,
			wantNonRetriable:  1,
			wantFailureReason: "404-ASG-not-found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeAz.ClearInjectedErrors()

			// Inject error for the PUT operation
			fakeAz.InjectError(fake.InjectKey{
				Operation:      fake.OperationPut,
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "test-cluster-ns-map",
			}, tc.injectedErr, 1)

			actions := []engine.Action{
				{Kind: engine.CreatePrefixSet, Target: target, DesiredIPs: []string{"10.0.0.1/32"}},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			results := executor.Execute(ctx, actions)

			// Verify executor reports failure
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d", len(results))
			}
			if results[0].Success {
				t.Fatal("expected failure but got success")
			}

			// Classify through controller layer
			summary := controller.ClassifyActionResults(results)

			if summary.RetriableCount != tc.wantRetriable {
				t.Errorf("RetriableCount = %d, want %d", summary.RetriableCount, tc.wantRetriable)
			}
			if summary.NonRetriableCount != tc.wantNonRetriable {
				t.Errorf("NonRetriableCount = %d, want %d", summary.NonRetriableCount, tc.wantNonRetriable)
			}
			if len(summary.Failures) != 1 {
				t.Fatalf("expected 1 failure, got %d", len(summary.Failures))
			}
			if summary.Failures[0].Reason != tc.wantFailureReason {
				t.Errorf("Reason = %q, want %q", summary.Failures[0].Reason, tc.wantFailureReason)
			}

			// Verify requeue decision
			policy := controller.DefaultRequeuePolicy(60 * time.Second)
			result := controller.DecideRequeueFromActionSummary(summary, policy)

			if tc.wantRetriable > 0 {
				if result.RequeueAfter < 8*time.Second {
					t.Errorf("RequeueAfter = %v, want >= 8s for retriable", result.RequeueAfter)
				}
			} else {
				if result.RequeueAfter != 60*time.Second {
					t.Errorf("RequeueAfter = %v, want 60s (resync) for non-retriable", result.RequeueAfter)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 2: T7.5 — Partial Failure Pipeline (3/5 fail, 2 succeed)
//
// Integration: azure.Executor + fake.Client + controller.ClassifyActionResults
//
// Verifies that when some actions succeed and others fail, the successful
// ones persist their state while failures are properly classified.
// ---------------------------------------------------------------------------

func TestExecutor_PartialFailure_Pipeline(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	executor := azure.NewExecutor(zapLog, factory, 5)

	// Set up 5 actions targeting different prefix sets under the same ASG
	actions := make([]engine.Action, 5)
	for i := 0; i < 5; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d/32", i+1)},
		}
	}

	// Inject 500 errors for actions 0, 2, 4 (3 failures)
	for _, idx := range []int{0, 2, 4} {
		fakeAz.InjectError(fake.InjectKey{
			Operation:      fake.OperationPut,
			SubscriptionID: "sub1",
			ResourceGroup:  "rg1",
			ASGName:        "asg1",
			PrefixSetName:  fmt.Sprintf("ps-%d", idx),
		}, &azure.ARMStatusError{
			StatusCode: 500,
			ARMCode:    "InternalServerError",
			Message:    "transient failure",
		}, 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := executor.Execute(ctx, actions)

	// Count successes and failures
	successCount := 0
	failCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		} else {
			failCount++
		}
	}

	if successCount != 2 {
		t.Errorf("success count = %d, want 2", successCount)
	}
	if failCount != 3 {
		t.Errorf("fail count = %d, want 3", failCount)
	}

	// Verify successful actions persisted state in fake client
	for _, idx := range []int{1, 3} {
		ps, err := fakeAz.Get(ctx, "sub1", "rg1", "asg1", fmt.Sprintf("ps-%d", idx))
		if err != nil {
			t.Errorf("expected ps-%d to exist after successful Put, got error: %v", idx, err)
			continue
		}
		if ps.Properties == nil || len(ps.Properties.AddressPrefixes) != 1 {
			t.Errorf("ps-%d: expected 1 IP, got %v", idx, ps)
		}
	}

	// Classify through controller error classification
	summary := controller.ClassifyActionResults(results)

	if summary.RetriableCount != 3 {
		t.Errorf("RetriableCount = %d, want 3 (500s are retriable)", summary.RetriableCount)
	}

	// Requeue decision should trigger fast retry
	policy := controller.DefaultRequeuePolicy(60 * time.Second)
	result := controller.DecideRequeueFromActionSummary(summary, policy)

	if result.RequeueAfter < 8*time.Second {
		t.Errorf("RequeueAfter = %v, want >= 8s for retriable failures", result.RequeueAfter)
	}
	if result.RequeueAfter > 5*time.Minute {
		t.Errorf("RequeueAfter = %v, want <= 5m (clamped max)", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test 3: 429 Retry-After Hint Propagation Through Full Pipeline
//
// Integration: azure.ARMStatusError → azure.ExtractRetryAfterHint →
// controller.ClassifyActionResults → controller.DecideRequeueFromActionSummary
//
// Verifies that Retry-After hints from 429 responses propagate through error
// classification to influence requeue timing.
// ---------------------------------------------------------------------------

func TestRetryAfterHint_Propagation_Pipeline(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	executor := azure.NewExecutor(zapLog, factory, 5)

	// Inject 429 with a 30-second Retry-After
	fakeAz.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps1",
	}, &azure.ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "Rate limited",
		RetryAfter: 30 * time.Second,
	}, 1)

	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := executor.Execute(ctx, actions)
	summary := controller.ClassifyActionResults(results)

	// Verify Retry-After hint is captured
	if summary.MaxRetryAfterHint != 30*time.Second {
		t.Errorf("MaxRetryAfterHint = %v, want 30s", summary.MaxRetryAfterHint)
	}

	// Verify requeue uses the Retry-After hint (30s > default 8s)
	policy := controller.DefaultRequeuePolicy(60 * time.Second)
	result := controller.DecideRequeueFromActionSummary(summary, policy)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want 30s (Retry-After hint)", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Retry-After Hint Clamped at MaxRetryAfterRequeueDelay
//
// Integration: azure.ARMStatusError → controller pipeline
//
// Verifies that excessively large Retry-After values are clamped to 5 minutes.
// ---------------------------------------------------------------------------

func TestRetryAfterHint_ClampedAt5Minutes(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	executor := azure.NewExecutor(zapLog, factory, 5)

	// Inject 429 with a 10-minute Retry-After (exceeds 5m max)
	fakeAz.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps1",
	}, &azure.ARMStatusError{
		StatusCode: 429,
		ARMCode:    "TooManyRequests",
		Message:    "Rate limited",
		RetryAfter: 10 * time.Minute,
	}, 1)

	actions := []engine.Action{
		{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  "ps1",
			},
			DesiredIPs: []string{"10.0.0.1/32"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	results := executor.Execute(ctx, actions)
	summary := controller.ClassifyActionResults(results)
	policy := controller.DefaultRequeuePolicy(60 * time.Second)
	result := controller.DecideRequeueFromActionSummary(summary, policy)

	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("RequeueAfter = %v, want 5m (clamped)", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test 5: Retry + Error Classification Chain Consistency
//
// Integration: azure.DecideRetry + azure.IsRetriableARM +
// controller.ClassifyActionResults
//
// Verifies that what DecideRetry considers retriable is consistent with what
// ClassifyActionResults classifies as retriable — ensuring no classification
// drift between the retry and error classifier layers.
// ---------------------------------------------------------------------------

func TestRetry_ErrorClassification_Consistency(t *testing.T) {
	policy := azure.DefaultRetryPolicy()

	tests := []struct {
		name           string
		err            error
		wantRetry      bool
		wantRetriable  bool
		wantClassLabel controller.ErrorClass
	}{
		{
			name:           "429_consistent_retriable",
			err:            &azure.ARMStatusError{StatusCode: 429, ARMCode: "TooManyRequests"},
			wantRetry:      true,
			wantRetriable:  true,
			wantClassLabel: controller.ErrorClassRetriable,
		},
		{
			name:           "500_consistent_retriable",
			err:            &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError"},
			wantRetry:      true,
			wantRetriable:  true,
			wantClassLabel: controller.ErrorClassRetriable,
		},
		{
			name:           "502_consistent_retriable",
			err:            &azure.ARMStatusError{StatusCode: 502, ARMCode: "BadGateway"},
			wantRetry:      true,
			wantRetriable:  true,
			wantClassLabel: controller.ErrorClassRetriable,
		},
		{
			name:           "403_consistent_non_retriable",
			err:            &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed"},
			wantRetry:      false,
			wantRetriable:  false,
			wantClassLabel: controller.ErrorClassNonRetriable,
		},
		{
			name:           "404_ParentASG_consistent_non_retriable",
			err:            &azure.ARMStatusError{StatusCode: 404, ARMCode: "ApplicationSecurityGroupNotFound"},
			wantRetry:      false,
			wantRetriable:  false,
			wantClassLabel: controller.ErrorClassNonRetriable,
		},
		{
			name:           "412_not_retried_by_generic_retry",
			err:            &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed"},
			wantRetry:      false,
			wantRetriable:  false,
			wantClassLabel: controller.ErrorClassNonRetriable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Check retry layer
			decision := azure.DecideRetry(tc.err, 1, policy)
			if decision.Retry != tc.wantRetry {
				t.Errorf("DecideRetry.Retry = %v, want %v", decision.Retry, tc.wantRetry)
			}

			// Check IsRetriableARM
			if azure.IsRetriableARM(tc.err) != tc.wantRetriable {
				t.Errorf("IsRetriableARM = %v, want %v", azure.IsRetriableARM(tc.err), tc.wantRetriable)
			}

			// Check classifier
			results := []azure.ActionResult{{
				Action:  engine.Action{Kind: engine.CreatePrefixSet},
				Success: false,
				Err:     tc.err,
			}}
			summary := controller.ClassifyActionResults(results)
			if len(summary.Failures) != 1 {
				t.Fatalf("expected 1 failure, got %d", len(summary.Failures))
			}
			if summary.Failures[0].Class != tc.wantClassLabel {
				t.Errorf("ClassifyActionResults class = %q, want %q",
					summary.Failures[0].Class, tc.wantClassLabel)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 6: Rate Limiter + Executor Concurrent Actions
//
// Integration: azure.ARMRateLimiter + azure.Executor
//
// Verifies that the rate limiter can work alongside the executor's concurrent
// action processing without deadlocks or dropped requests.
// ---------------------------------------------------------------------------

func TestRateLimiter_WithExecutor_NoDrop(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	// Create rate limiter with high RPS to not block test
	limiter := azure.NewARMRateLimiter(zapLog, 100)

	// Ensure rate limiter works for concurrent calls
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Fire 20 concurrent rate limiter waits
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			errCh <- limiter.Wait(ctx, "sub1")
		}()
	}

	for i := 0; i < 20; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("Wait returned error: %v", err)
		}
	}

	// Now verify executor also works with same factory
	executor := azure.NewExecutor(zapLog, factory, 5)
	actions := make([]engine.Action, 10)
	for i := 0; i < 10; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d/32", i+1)},
		}
	}

	results := executor.Execute(ctx, actions)
	for i, r := range results {
		if !r.Success {
			t.Errorf("action %d failed: %v", i, r.Err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 7: DecideRequeueFromSystemError + ARM Error Types
//
// Integration: azure.ARMStatusError + azure error classifiers +
// controller.DecideRequeueFromSystemError
//
// Verifies system-level error handling for pre-action errors
// (e.g., listActualForTargets failures) produces correct requeue.
// ---------------------------------------------------------------------------

func TestSystemError_RequeueDecision_Pipeline(t *testing.T) {
	policy := controller.DefaultRequeuePolicy(60 * time.Second)

	tests := []struct {
		name           string
		err            error
		wantRequeueMin time.Duration
		wantRequeueMax time.Duration
	}{
		{
			name: "429_system_error_fast_requeue",
			err: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				RetryAfter: 15 * time.Second,
			},
			wantRequeueMin: 15 * time.Second,
			wantRequeueMax: 15 * time.Second,
		},
		{
			name: "500_system_error_backoff_requeue",
			err: &azure.ARMStatusError{
				StatusCode: 500,
				ARMCode:    "InternalServerError",
			},
			wantRequeueMin: 8 * time.Second,
			wantRequeueMax: 8 * time.Second,
		},
		{
			name: "403_auth_system_error_resync",
			err: &azure.ARMStatusError{
				StatusCode: 403,
				ARMCode:    "AuthorizationFailed",
			},
			wantRequeueMin: 60 * time.Second,
			wantRequeueMax: 60 * time.Second,
		},
		{
			name: "404_parent_ASG_system_error_resync",
			err: &azure.ARMStatusError{
				StatusCode: 404,
				ARMCode:    "ApplicationSecurityGroupNotFound",
			},
			wantRequeueMin: 60 * time.Second,
			wantRequeueMax: 60 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := controller.DecideRequeueFromSystemError(tc.err, policy)
			if result.RequeueAfter < tc.wantRequeueMin {
				t.Errorf("RequeueAfter = %v, want >= %v", result.RequeueAfter, tc.wantRequeueMin)
			}
			if result.RequeueAfter > tc.wantRequeueMax {
				t.Errorf("RequeueAfter = %v, want <= %v", result.RequeueAfter, tc.wantRequeueMax)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test 8: Envtest — Full Reconcile with Transient 500 Errors
//
// Integration: envtest + MappingReconciler + Executor + FakeClient + StatusUpdater
//
// Creates a PodASGMapping and matching pods, injects 500 errors for certain
// Put operations, and verifies the reconciler eventually converges when
// errors clear.
// ---------------------------------------------------------------------------

func TestEnvtest_Reconcile_TransientErrors_EventualConvergence(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer teardownEnv(t, te)

	ctx := context.Background()
	ns := "test-transient"

	// Create namespace
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create pod with IP
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: ns,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Image: "nginx",
			}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	// Set pod IP via status update
	pod.Status.PodIP = "10.0.0.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := model.OwnershipKey("test-cluster", ns, "transient-mapping")

	// Inject 500 errors that will clear after 2 calls
	te.fakeClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  ownershipKey,
	}, &azure.ARMStatusError{
		StatusCode: 500,
		ARMCode:    "InternalServerError",
		Message:    "transient",
	}, 2) // fail first 2 reconcile attempts

	// Create mapping
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "transient-mapping",
			Namespace: ns,
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
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for eventual convergence: the prefix set should exist once errors clear
	waitForCondition(t, 30*time.Second, "prefix set created after transient errors", func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		if err != nil {
			return false
		}
		return ps != nil && ps.Properties != nil && len(ps.Properties.AddressPrefixes) > 0
	})

	// Verify the prefix set has the correct IP
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if err != nil {
		t.Fatalf("expected prefix set to exist: %v", err)
	}
	if len(ps.Properties.AddressPrefixes) != 1 || ps.Properties.AddressPrefixes[0] != "10.0.0.1/32" {
		t.Errorf("prefix set IPs = %v, want [10.0.0.1]", ps.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Envtest — Partial Failure with Status Reporting
//
// Integration: envtest + MappingReconciler + Executor + FakeClient +
// StatusUpdater + Error Classification
//
// Creates a mapping with 2 ASG targets, injects permanent error on one,
// verifies the other succeeds and status reflects mixed results.
// ---------------------------------------------------------------------------

func TestEnvtest_Reconcile_PartialFailure_StatusReflectsErrors(t *testing.T) {
	te := setupTestEnvWithManager(t)
	defer teardownEnv(t, te)

	ctx := context.Background()
	ns := "test-partial-fail"

	// Register a second subscription client
	fakeAz2 := fake.NewClient()
	te.fakeFactory.RegisterClient("sub2", fakeAz2)

	// Create namespace
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	if err := te.k8sClient.Create(ctx, nsObj); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: ns,
			Labels:    map[string]string{"role": "api"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
		},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.1.1"
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.1.1"}}
	pod.Status.Phase = corev1.PodRunning
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	ownershipKey := model.OwnershipKey("test-cluster", ns, "partial-mapping")

	// Inject permanent 403 on sub2 for all operations
	fakeAz2.InjectError(fake.InjectKey{
		Operation:      fake.OperationPut,
		SubscriptionID: "sub2",
		ResourceGroup:  "rg2",
		ASGName:        "asg2",
		PrefixSetName:  ownershipKey,
	}, &azure.ARMStatusError{
		StatusCode: 403,
		ARMCode:    "AuthorizationFailed",
		Message:    "Forbidden",
	}, 100) // permanent

	// Create mapping targeting two ASGs in different subscriptions
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partial-mapping",
			Namespace: ns,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"role": "api"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID("sub1", "rg1", "asg1")},
						{ResourceID: asgResourceID("sub2", "rg2", "asg2")},
					},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for the successful target (sub1/asg1) to be created
	waitForCondition(t, 30*time.Second, "sub1 prefix set created", func() bool {
		ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
		return err == nil && ps != nil
	})

	// Verify sub1 prefix set has the right IP
	ps, err := te.fakeClient.Get(ctx, "sub1", "rg1", "asg1", ownershipKey)
	if err != nil {
		t.Fatalf("sub1 prefix set should exist: %v", err)
	}
	if len(ps.Properties.AddressPrefixes) != 1 || ps.Properties.AddressPrefixes[0] != "10.0.1.1/32" {
		t.Errorf("sub1 prefix set IPs = %v, want [10.0.1.1]", ps.Properties.AddressPrefixes)
	}

	// Verify sub2 prefix set was NOT created (403 error)
	_, err = fakeAz2.Get(ctx, "sub2", "rg2", "asg2", ownershipKey)
	if err == nil {
		t.Error("sub2 prefix set should not exist (403 error)")
	}

	// Verify status shows Reconciled=False (action failures)
	waitForCondition(t, 15*time.Second, "status updated with Reconciled=False", func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "partial-mapping", Namespace: ns}, &m); err != nil {
			return false
		}
		for _, c := range m.Status.Conditions {
			if c.Type == "Reconciled" && c.Status == metav1.ConditionFalse {
				return true
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// Test 10: Error Type Hierarchy — ARMStatusError From Azure Package to Controller
//
// Integration: azure.ARMStatusError + azure.Is* helpers + controller.classifySingleFailure
//
// Verifies the error type hierarchy is correctly maintained across package
// boundaries when errors are wrapped with pkg/errors.
// ---------------------------------------------------------------------------

func TestErrorTypeHierarchy_AcrossPackages(t *testing.T) {
	// Create error in azure package
	original := &azure.ARMStatusError{
		StatusCode: 404,
		ARMCode:    "ParentResourceNotFound",
		Message:    "Parent not found",
	}

	// Wrap with pkg/errors (as real code does)
	wrapped := errors.Wrap(original, "GET prefix set")
	doubleWrapped := errors.Wrap(wrapped, "listActualForTargets")

	// Verify azure classifiers still work through wrapping
	if !azure.IsParentASGNotFound(doubleWrapped) {
		t.Error("IsParentASGNotFound should detect parent ASG through double wrapping")
	}
	if !azure.IsNotFound(doubleWrapped) {
		t.Error("IsNotFound should detect 404 through double wrapping")
	}
	if azure.IsRetriableARM(doubleWrapped) {
		t.Error("IsRetriableARM should return false for 404")
	}
	if azure.IsAuthFailure(doubleWrapped) {
		t.Error("IsAuthFailure should return false for 404")
	}

	// Verify controller requeue decision with wrapped error
	policy := controller.DefaultRequeuePolicy(60 * time.Second)
	result := controller.DecideRequeueFromSystemError(doubleWrapped, policy)

	if result.RequeueAfter != 60*time.Second {
		t.Errorf("RequeueAfter = %v, want 60s (resync for parent ASG not found)", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test 11: Mixed Error Types in Parallel Executor
//
// Integration: azure.Executor (concurrent) + fake.Client + controller error pipeline
//
// Verifies error classification is correct when the executor processes
// multiple actions concurrently with different failure modes.
// ---------------------------------------------------------------------------

func TestExecutor_ConcurrentMixedErrors_Classification(t *testing.T) {
	zapLog := zaptest.NewLogger(t)
	factory := fake.NewClientFactory()
	fakeAz := fake.NewClient()
	factory.RegisterClient("sub1", fakeAz)

	executor := azure.NewExecutor(zapLog, factory, 5)

	// Action 0: 429 (retriable)
	fakeAz.InjectError(fake.InjectKey{
		Operation: fake.OperationPut, SubscriptionID: "sub1",
		ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps-0",
	}, &azure.ARMStatusError{StatusCode: 429, ARMCode: "TooManyRequests", RetryAfter: 20 * time.Second}, 1)

	// Action 1: succeeds
	// Action 2: 403 (non-retriable)
	fakeAz.InjectError(fake.InjectKey{
		Operation: fake.OperationPut, SubscriptionID: "sub1",
		ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps-2",
	}, &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed"}, 1)

	// Action 3: 404 parent ASG not found (non-retriable)
	fakeAz.InjectError(fake.InjectKey{
		Operation: fake.OperationPut, SubscriptionID: "sub1",
		ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps-3",
	}, &azure.ARMStatusError{StatusCode: 404, ARMCode: "ApplicationSecurityGroupNotFound"}, 1)

	// Action 4: 500 (retriable)
	fakeAz.InjectError(fake.InjectKey{
		Operation: fake.OperationPut, SubscriptionID: "sub1",
		ResourceGroup: "rg1", ASGName: "asg1", PrefixSetName: "ps-4",
	}, &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError"}, 1)

	actions := make([]engine.Action, 5)
	for i := 0; i < 5; i++ {
		actions[i] = engine.Action{
			Kind: engine.CreatePrefixSet,
			Target: engine.ASGTarget{
				SubscriptionID: "sub1",
				ResourceGroup:  "rg1",
				ASGName:        "asg1",
				PrefixSetName:  fmt.Sprintf("ps-%d", i),
			},
			DesiredIPs: []string{fmt.Sprintf("10.0.0.%d/32", i+1)},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results := executor.Execute(ctx, actions)
	summary := controller.ClassifyActionResults(results)

	// Expected: 2 retriable (429 + 500), 2 non-retriable (403 + 404-ASG)
	if summary.RetriableCount != 2 {
		t.Errorf("RetriableCount = %d, want 2", summary.RetriableCount)
	}
	if summary.NonRetriableCount != 2 {
		t.Errorf("NonRetriableCount = %d, want 2", summary.NonRetriableCount)
	}

	// Verify the 429's Retry-After hint is captured as max
	if summary.MaxRetryAfterHint != 20*time.Second {
		t.Errorf("MaxRetryAfterHint = %v, want 20s", summary.MaxRetryAfterHint)
	}

	// Verify requeue uses the Retry-After hint since retriable > 0
	policy := controller.DefaultRequeuePolicy(60 * time.Second)
	result := controller.DecideRequeueFromActionSummary(summary, policy)
	if result.RequeueAfter != 20*time.Second {
		t.Errorf("RequeueAfter = %v, want 20s (max Retry-After hint)", result.RequeueAfter)
	}

	// Verify the one successful action persisted
	ps, err := fakeAz.Get(ctx, "sub1", "rg1", "asg1", "ps-1")
	if err != nil {
		t.Errorf("ps-1 should exist after successful Put: %v", err)
	}
	if ps.Properties == nil || len(ps.Properties.AddressPrefixes) != 1 {
		t.Errorf("ps-1 IPs wrong: %v", ps)
	}
}

// ---------------------------------------------------------------------------
// Test 12: Rate Limiter Context Cancellation
//
// Integration: azure.ARMRateLimiter + context cancellation
//
// Verifies that rate limiter properly handles context cancellation during
// wait, which is critical for graceful controller shutdown.
// ---------------------------------------------------------------------------

func TestRateLimiter_ContextCancellation(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	// Very low RPS to force throttling
	limiter := azure.NewARMRateLimiter(zapLog, 1)

	ctx, cancel := context.WithCancel(context.Background())

	// First call succeeds (consumes burst token)
	if err := limiter.Wait(ctx, "sub1"); err != nil {
		t.Fatalf("first Wait should succeed: %v", err)
	}

	// Cancel context before second call
	cancel()

	// Second call should fail with context error
	err := limiter.Wait(ctx, "sub1")
	if err == nil {
		t.Error("Wait should return error after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// Test 13: Rate Limiter Per-Subscription Isolation
//
// Integration: azure.ARMRateLimiter across multiple subscriptions
//
// Verifies that rate limiting for one subscription doesn't block another.
// ---------------------------------------------------------------------------

func TestRateLimiter_PerSubscription_Isolation(t *testing.T) {
	zapLog := zaptest.NewLogger(t)

	// 1 RPS per subscription
	limiter := azure.NewARMRateLimiter(zapLog, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Consume burst for sub1
	if err := limiter.Wait(ctx, "sub1"); err != nil {
		t.Fatalf("sub1 first Wait failed: %v", err)
	}

	// sub2 should still be able to proceed immediately
	start := time.Now()
	if err := limiter.Wait(ctx, "sub2"); err != nil {
		t.Fatalf("sub2 Wait failed: %v", err)
	}
	elapsed := time.Since(start)

	// sub2 should not be blocked by sub1's rate limit
	if elapsed > 500*time.Millisecond {
		t.Errorf("sub2 Wait took %v, expected near-instant (subscription isolation)", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Test: T7.5 Integration — Status Contract After Partial Failure
//
// Validates that the Phase 7 partial failure report correctly categorizes
// successes and failures, and that the status contract validator detects
// violations in incomplete status snapshots.
// ---------------------------------------------------------------------------

func TestPhase7_T75_Integration_ValidateStatusContract(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set, skipping envtest integration test")
	}

	zapLog := zaptest.NewLogger(t)

	asgOk := "asg-ok-integration"
	asgFail := "asg-fail-integration"
	asgPerm := "asg-perm-integration"

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: asgOk,
					FullResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgOk),
					PrefixSetName:  "pod-nsg-controller",
				},
				DesiredIPs: []string{"10.0.0.1/32"},
			},
			Err: nil,
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: asgFail,
					FullResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgFail),
					PrefixSetName:  "pod-nsg-controller",
				},
				DesiredIPs: []string{"10.0.0.2/32"},
			},
			Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "transient"},
		},
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: asgPerm,
					FullResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgPerm),
					PrefixSetName:  "pod-nsg-controller",
				},
				DesiredIPs: []string{"10.0.0.3/32"},
			},
			Err: &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"},
		},
	}

	report := controllertest.BuildPartialFailureReport(results, v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "contract-int"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgOk)},
				{ResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgFail)},
				{ResourceID: fmt.Sprintf("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/%s", asgPerm)},
			},
		}},
	}, "pod-nsg-controller", controller.RequeuePolicy{})

	// T7.5 integration: 1 succeeded, 2 failed.
	if len(report.SucceededTargets) != 1 {
		t.Errorf("T7.5 integration: SucceededTargets = %d, want 1", len(report.SucceededTargets))
	}
	if len(report.FailedTargets) != 2 {
		t.Errorf("T7.5 integration: FailedTargets = %d, want 2", len(report.FailedTargets))
	}

	violations := controllertest.ValidatePhase7StatusContract(
		report.StatusSnapshot, results,
		v1alpha1.PodASGMappingSpec{}, "pod-nsg-controller",
	)
	if len(violations) == 0 {
		t.Errorf("T7.5 integration: ValidatePhase7StatusContract returned 0 violations; want > 0")
	}

	_ = zapLog
}
