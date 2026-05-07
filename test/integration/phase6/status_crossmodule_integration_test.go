package phase6_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
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
// Integration: ObservedGeneration correctly tracked in conditions after reconcile
// Cross-module flow: Reconciler → StatusUpdater → SetConditionForGeneration → K8s API
// ---------------------------------------------------------------------------

func TestPhase6_ObservedGeneration_TrackedInConditionsAfterReconcile(t *testing.T) {
	podCounter := &stubPodCountSource{counts: map[string]int{}}
	customUpdater := &deferredStatusUpdater{podCounter: podCounter}

	te := setupTestEnv6(t, &successExecutor{}, customUpdater)
	defer te.teardown(t)
	ctx := context.Background()

	customUpdater.setClient(te.k8sClient)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "observedgen-test"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: "observedgen-test",
			Labels: map[string]string{"app": "web"},
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

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "observedgen-mapping", Namespace: "observedgen-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "asg1")}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	// Wait for Reconciled=True indicating successful reconcile.
	eventually6(t, 15*time.Second, 300*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "observedgen-mapping", Namespace: "observedgen-test"}, &m); err != nil {
			return false
		}
		return hasCondition(m.Status.Conditions, "Reconciled", metav1.ConditionTrue)
	}, "Reconciled=True condition")

	var final v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "observedgen-mapping", Namespace: "observedgen-test"}, &final); err != nil {
		t.Fatalf("failed to get final mapping: %v", err)
	}

	// Verify ObservedGeneration is set correctly on both conditions.
	for _, c := range final.Status.Conditions {
		if c.Type == "Accepted" {
			if c.ObservedGeneration != final.Generation {
				t.Errorf("Accepted condition: expected ObservedGeneration=%d, got %d",
					final.Generation, c.ObservedGeneration)
			}
		}
		if c.Type == "Reconciled" {
			if c.ObservedGeneration != final.Generation {
				t.Errorf("Reconciled condition: expected ObservedGeneration=%d, got %d",
					final.Generation, c.ObservedGeneration)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: MappingCount set correctly through full reconciler loop (T61/T62)
// Cross-module flow: Reconciler → StatusUpdater → ComputeStatus → K8s Status API
// ---------------------------------------------------------------------------

func TestPhase6_MappingCount_SetCorrectlyInSuccessPath(t *testing.T) {
	podCounter := &stubPodCountSource{counts: map[string]int{}}
	customUpdater := &deferredStatusUpdater{podCounter: podCounter}

	te := setupTestEnv6(t, &successExecutor{}, customUpdater)
	defer te.teardown(t)
	ctx := context.Background()

	customUpdater.setClient(te.k8sClient)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mappingcount-test"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Create pods for each selector.
	for _, p := range []struct {
		name   string
		labels map[string]string
		ip     string
	}{
		{"fe-1", map[string]string{"tier": "frontend"}, "10.0.0.1"},
		{"be-1", map[string]string{"tier": "backend"}, "10.0.0.2"},
		{"be-2", map[string]string{"tier": "backend"}, "10.0.0.3"},
	} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: p.name, Namespace: "mappingcount-test", Labels: p.labels},
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

	// Create mapping with 2 rules.
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mc-mapping", Namespace: "mappingcount-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "frontend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "frontend-asg")}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "backend"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "backend-asg")}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	eventually6(t, 15*time.Second, 300*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "mc-mapping", Namespace: "mappingcount-test"}, &m); err != nil {
			return false
		}
		return hasCondition(m.Status.Conditions, "Reconciled", metav1.ConditionTrue)
	}, "Reconciled=True condition")

	var final v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "mc-mapping", Namespace: "mappingcount-test"}, &final); err != nil {
		t.Fatalf("failed to get final mapping: %v", err)
	}

	// Verify MappingCount matches number of spec mappings.
	if final.Status.MappingCount != 2 {
		t.Errorf("expected MappingCount=2, got %d", final.Status.MappingCount)
	}

	// Verify correct number of mapping statuses.
	if len(final.Status.MappingStatuses) != 2 {
		t.Errorf("expected 2 MappingStatuses, got %d", len(final.Status.MappingStatuses))
	}

	// Verify matchedPods is populated correctly for at least the backend mapping (2 pods).
	beHash := controller.SelectorHash(map[string]string{"tier": "backend"})
	for _, ms := range final.Status.MappingStatuses {
		if ms.SelectorHash == beHash && ms.MatchedPods != 2 {
			t.Errorf("backend mapping: expected matchedPods=2, got %d", ms.MatchedPods)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: MappingCount preserved during partial failure path
// Cross-module flow: Reconciler → failingExecutor → StatusUpdater → K8s Status API
// ---------------------------------------------------------------------------

func TestPhase6_MappingCount_PreservedInPartialFailurePath(t *testing.T) {
	podCounter := &stubPodCountSource{counts: map[string]int{}}
	customUpdater := &deferredStatusUpdater{podCounter: podCounter}
	failExec := &partialFailExecutor{failASG: "fail-asg"}

	te := setupTestEnv6(t, failExec, customUpdater)
	defer te.teardown(t)
	ctx := context.Background()

	customUpdater.setClient(te.k8sClient)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mcfail-test"}}
	if err := te.k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-1", Namespace: "mcfail-test",
			Labels: map[string]string{"app": "fail-app"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	if err := te.k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}
	pod.Status.PodIP = "10.0.0.1"
	if err := te.k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mcfail-mapping", Namespace: "mcfail-test"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "fail-app"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: asgResourceID("sub1", "rg1", "fail-asg")}},
				},
			},
		},
	}
	if err := te.k8sClient.Create(ctx, mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}

	eventually6(t, 15*time.Second, 300*time.Millisecond, func() bool {
		var m v1alpha1.PodASGMapping
		if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "mcfail-mapping", Namespace: "mcfail-test"}, &m); err != nil {
			return false
		}
		return m.Status.MappingCount > 0
	}, "MappingCount to be set")

	var final v1alpha1.PodASGMapping
	if err := te.k8sClient.Get(ctx, types.NamespacedName{Name: "mcfail-mapping", Namespace: "mcfail-test"}, &final); err != nil {
		t.Fatalf("failed to get final mapping: %v", err)
	}

	if final.Status.MappingCount != 1 {
		t.Errorf("expected MappingCount=1 even with failure, got %d", final.Status.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// Integration: Generation drift error propagation across status updater boundary
// Cross-module flow: StatusUpdater detects drift → returns StatusGenerationDriftError
// ---------------------------------------------------------------------------

func TestPhase6_GenerationDrift_ReturnsTypedError_AcrossModuleBoundary(t *testing.T) {
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

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "drift-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "drift-mapping", Namespace: "drift-test"},
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

	// Refetch to get server-assigned generation.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "drift-mapping", Namespace: "drift-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	actualGeneration := mapping.Generation

	// Simulate drift: pass a stale generation to the status updater.
	updater := controller.NewMappingStatusUpdater(k8sClient, time.Now)
	input := controller.ReconcileStatusInput{
		Phase:        controller.StatusPhasePostExecution,
		ProcessedGen: actualGeneration + 1, // Stale: processed a future generation
		Results:      []azure.ActionResult{},
		PodCounts:    map[string]int{},
	}

	statusErr := updater.UpdateAfterReconcile(context.Background(), mapping, input)

	// Verify the error is a StatusGenerationDriftError.
	if statusErr == nil {
		t.Fatal("expected StatusGenerationDriftError, got nil")
	}

	var driftErr *controller.StatusGenerationDriftError
	if !isStatusGenerationDriftError(statusErr, &driftErr) {
		t.Fatalf("expected *StatusGenerationDriftError, got %T: %v", statusErr, statusErr)
	}

	if driftErr.ProcessedGeneration != actualGeneration+1 {
		t.Errorf("expected ProcessedGeneration=%d, got %d", actualGeneration+1, driftErr.ProcessedGeneration)
	}
	if driftErr.LiveGeneration != actualGeneration {
		t.Errorf("expected LiveGeneration=%d, got %d", actualGeneration, driftErr.LiveGeneration)
	}
}

// ---------------------------------------------------------------------------
// Integration: Status write failure propagates correctly through reconciler
// Cross-module flow: Reconciler → StatusUpdater (fails) → combined error returned
// ---------------------------------------------------------------------------

func TestPhase6_StatusWriteFailure_PropagatesError(t *testing.T) {
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

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "statuserr-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "statuserr-mapping", Namespace: "statuserr-test"},
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

	// Refetch mapping.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "statuserr-mapping", Namespace: "statuserr-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	// Use an updater with a WRONG client (nil) to force a status write failure.
	// Actually, we use a stale resource version by writing from a copy with bad RV.
	// This simulates a conflict error propagating through the updater boundary.
	staleCopy := mapping.DeepCopy()
	staleCopy.ResourceVersion = "999999"

	updater := controller.NewMappingStatusUpdater(k8sClient, time.Now)
	input := controller.ReconcileStatusInput{
		Phase:        controller.StatusPhasePostExecution,
		ProcessedGen: staleCopy.Generation,
		Results:      []azure.ActionResult{},
		PodCounts:    map[string]int{},
	}

	statusErr := updater.UpdateAfterReconcile(context.Background(), staleCopy, input)

	// The status write should fail with a conflict error (stale resourceVersion).
	if statusErr == nil {
		t.Fatal("expected status write to fail with conflict error, got nil")
	}
}

// ---------------------------------------------------------------------------
// Integration: Identity-based lastSyncTime carry-forward via direct updater
// Cross-module flow: MappingIdentity → applyLastSyncTimeByIdentity → K8s Status API
// ---------------------------------------------------------------------------

func TestPhase6_IdentityBasedCarryForward_AcrossUpdaterCycles(t *testing.T) {
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

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "identity-cf-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	spec := v1alpha1.PodASGMappingSpec{
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
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "identity-cf-mapping", Namespace: "identity-cf-test"},
		Spec:       spec,
	}
	if err := k8sClient.Create(context.Background(), mapping); err != nil {
		t.Fatalf("failed to create mapping: %v", err)
	}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "identity-cf-mapping", Namespace: "identity-cf-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	firstTime := time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC)

	// First cycle: both succeed.
	target1 := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-identity-cf-test-identity-cf-mapping",
	}
	target2 := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg2",
		FullResourceID: asgResourceID("sub1", "rg1", "asg2"),
		PrefixSetName:  "test-cluster-identity-cf-test-identity-cf-mapping",
	}

	updater := controller.NewMappingStatusUpdater(k8sClient, func() time.Time { return firstTime })
	input := controller.ReconcileStatusInput{
		Phase:         controller.StatusPhasePostExecution,
		ProcessedGen:  mapping.Generation,
		ProcessedSpec: spec,
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.CreatePrefixSet, Target: target1}, Success: true},
			{Action: engine.Action{Kind: engine.CreatePrefixSet, Target: target2}, Success: true},
		},
		PodCounts: map[string]int{
			controller.SelectorHash(map[string]string{"app": "web"}): 1,
			controller.SelectorHash(map[string]string{"app": "api"}): 2,
		},
	}
	if err := updater.UpdateAfterReconcile(context.Background(), mapping, input); err != nil {
		t.Fatalf("first update failed: %v", err)
	}

	// Refetch to get updated status.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "identity-cf-mapping", Namespace: "identity-cf-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch after first update: %v", err)
	}

	// Verify both have LastSyncTime set.
	if len(mapping.Status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(mapping.Status.MappingStatuses))
	}
	for i, ms := range mapping.Status.MappingStatuses {
		if ms.LastSyncTime.IsZero() {
			t.Errorf("mapping[%d]: expected non-zero LastSyncTime after success", i)
		}
	}

	// Second cycle: first mapping fails, second succeeds. Use identity-based carry-forward.
	secondTime := firstTime.Add(10 * time.Minute)
	previousStatuses := append([]v1alpha1.MappingStatus(nil), mapping.Status.MappingStatuses...)
	previousObservedGen := observedGenForCondition(mapping.Status.Conditions, "Reconciled")

	updater2 := controller.NewMappingStatusUpdater(k8sClient, func() time.Time { return secondTime })
	input2 := controller.ReconcileStatusInput{
		Phase:                      controller.StatusPhasePostExecution,
		ProcessedGen:               mapping.Generation,
		ProcessedSpec:              spec,
		PreviousMappingStatuses:    previousStatuses,
		PreviousObservedGeneration: previousObservedGen,
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target1}, Success: false, Err: fmt.Errorf("timeout")},
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target2}, Success: true},
		},
		PodCounts: map[string]int{
			controller.SelectorHash(map[string]string{"app": "web"}): 1,
			controller.SelectorHash(map[string]string{"app": "api"}): 2,
		},
		ReconcileErr: fmt.Errorf("action failures: update asg1: timeout"),
	}
	if err := updater2.UpdateAfterReconcile(context.Background(), mapping, input2); err != nil {
		t.Fatalf("second update failed: %v", err)
	}

	// Refetch and verify.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "identity-cf-mapping", Namespace: "identity-cf-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch after second update: %v", err)
	}

	if len(mapping.Status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses after second update, got %d", len(mapping.Status.MappingStatuses))
	}

	webHash := controller.SelectorHash(map[string]string{"app": "web"})
	apiHash := controller.SelectorHash(map[string]string{"app": "api"})

	for _, ms := range mapping.Status.MappingStatuses {
		switch ms.SelectorHash {
		case webHash:
			// Failed mapping: LastSyncTime should be carried forward from first cycle.
			if ms.ASGSyncState != "Error" {
				t.Errorf("web mapping: expected Error state, got %s", ms.ASGSyncState)
			}
			if !ms.LastSyncTime.Time.Equal(firstTime) {
				t.Errorf("web mapping: expected LastSyncTime=%v (carried forward), got %v",
					firstTime, ms.LastSyncTime.Time)
			}
		case apiHash:
			// Succeeded mapping: LastSyncTime should be updated to secondTime.
			if ms.ASGSyncState != "Synced" {
				t.Errorf("api mapping: expected Synced state, got %s", ms.ASGSyncState)
			}
			if !ms.LastSyncTime.Time.Equal(secondTime) {
				t.Errorf("api mapping: expected LastSyncTime=%v (updated), got %v",
					secondTime, ms.LastSyncTime.Time)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Integration: LastSyncTime updated on re-sync success after prior error
// Cross-module flow: StatusUpdater → applyLastSyncTimeByIdentity → K8s API → verify
// ---------------------------------------------------------------------------

func TestPhase6_LastSyncTime_UpdatedOnResyncSuccess(t *testing.T) {
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

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "resync-test"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "resync-mapping", Namespace: "resync-test"},
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
		Name: "resync-mapping", Namespace: "resync-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch mapping: %v", err)
	}

	target := engine.ASGTarget{
		SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg1",
		FullResourceID: asgResourceID("sub1", "rg1", "asg1"),
		PrefixSetName:  "test-cluster-resync-test-resync-mapping",
	}

	// Cycle 1: failure.
	t1 := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	updater1 := controller.NewMappingStatusUpdater(k8sClient, func() time.Time { return t1 })
	input1 := controller.ReconcileStatusInput{
		Phase:         controller.StatusPhasePostExecution,
		ProcessedGen:  mapping.Generation,
		ProcessedSpec: mapping.Spec,
		Results:       []azure.ActionResult{{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target}, Success: false, Err: fmt.Errorf("fail")}},
		PodCounts:     map[string]int{},
		ReconcileErr:  fmt.Errorf("action failures"),
	}
	if err := updater1.UpdateAfterReconcile(context.Background(), mapping, input1); err != nil {
		t.Fatalf("cycle 1 update failed: %v", err)
	}

	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "resync-mapping", Namespace: "resync-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch after cycle 1: %v", err)
	}

	if mapping.Status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Fatalf("expected Error state after cycle 1, got %s", mapping.Status.MappingStatuses[0].ASGSyncState)
	}

	// Cycle 2: success → LastSyncTime should be updated.
	t2 := t1.Add(5 * time.Minute)
	previousStatuses := append([]v1alpha1.MappingStatus(nil), mapping.Status.MappingStatuses...)
	prevObservedGen := observedGenForCondition(mapping.Status.Conditions, "Reconciled")

	updater2 := controller.NewMappingStatusUpdater(k8sClient, func() time.Time { return t2 })
	input2 := controller.ReconcileStatusInput{
		Phase:                      controller.StatusPhasePostExecution,
		ProcessedGen:               mapping.Generation,
		ProcessedSpec:              mapping.Spec,
		PreviousMappingStatuses:    previousStatuses,
		PreviousObservedGeneration: prevObservedGen,
		Results:                    []azure.ActionResult{{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: target}, Success: true}},
		PodCounts:                  map[string]int{},
	}
	if err := updater2.UpdateAfterReconcile(context.Background(), mapping, input2); err != nil {
		t.Fatalf("cycle 2 update failed: %v", err)
	}

	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name: "resync-mapping", Namespace: "resync-test",
	}, mapping); err != nil {
		t.Fatalf("failed to refetch after cycle 2: %v", err)
	}

	if mapping.Status.MappingStatuses[0].ASGSyncState != "Synced" {
		t.Errorf("expected Synced after cycle 2, got %s", mapping.Status.MappingStatuses[0].ASGSyncState)
	}
	if !mapping.Status.MappingStatuses[0].LastSyncTime.Time.Equal(t2) {
		t.Errorf("expected LastSyncTime=%v after success, got %v", t2, mapping.Status.MappingStatuses[0].LastSyncTime.Time)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func isStatusGenerationDriftError(err error, target **controller.StatusGenerationDriftError) bool {
	driftErr, ok := err.(*controller.StatusGenerationDriftError)
	if ok && target != nil {
		*target = driftErr
	}
	return ok
}

func observedGenForCondition(conditions []metav1.Condition, condType string) int64 {
	for _, c := range conditions {
		if c.Type == condType {
			return c.ObservedGeneration
		}
	}
	return 0
}
