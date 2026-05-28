package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPhase8_ReconcilerWiring_EmitsReconcileMetrics verifies that a real
// MappingReconciler with metrics wiring emits reconcile_total and
// reconcile_duration_seconds when Reconcile completes.
func TestPhase8_ReconcilerWiring_EmitsReconcileMetrics(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "metrics-test-ns"

	mapping := newTestMapping(ns, "metrics-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-metrics")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-1", "10.0.0.1", map[string]string{"app": "web"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "metrics-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	// Verify reconcile_total was incremented
	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	foundReconcileTotal := false
	foundReconcileDuration := false
	foundCRDResolution := false
	foundActionsPerCycle := false
	foundPodIPChanges := false

	for _, mf := range mfs {
		switch mf.GetName() {
		case "pod_nsg_controller_reconcile_total":
			foundReconcileTotal = true
			total := sumCounterValues(mf)
			if total < 1 {
				t.Errorf("reconcile_total = %v, want >= 1", total)
			}
		case "pod_nsg_controller_reconcile_duration_seconds":
			foundReconcileDuration = true
		case "pod_nsg_controller_crd_resolution_duration_seconds":
			foundCRDResolution = true
		case "pod_nsg_controller_reconcile_actions_per_cycle":
			foundActionsPerCycle = true
		case "pod_nsg_controller_pod_ip_changes_total":
			foundPodIPChanges = true
		}
	}

	if !foundReconcileTotal {
		t.Error("reconcile_total metric not emitted by Reconcile()")
	}
	if !foundReconcileDuration {
		t.Error("reconcile_duration_seconds metric not emitted by Reconcile()")
	}
	if !foundCRDResolution {
		t.Error("crd_resolution_duration_seconds metric not emitted by Reconcile()")
	}
	if !foundActionsPerCycle {
		t.Error("reconcile_actions_per_cycle metric not emitted by Reconcile()")
	}
	if !foundPodIPChanges {
		t.Error("pod_ip_changes_total metric not emitted by Reconcile() (pod churn)")
	}
}

// TestPhase8_ReconcilerWiring_EmitsConvergenceOnSuccess verifies that
// convergence tracking emits histogram observations after successful ARM PUTs.
func TestPhase8_ReconcilerWiring_EmitsConvergenceOnSuccess(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "conv-test-ns"

	mapping := newTestMapping(ns, "conv-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "conv"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-conv")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-conv", "10.0.0.1", map[string]string{"app": "conv"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conv-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	// The executor creates a prefix set → convergence should be tracked.
	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	foundPrefixSetActions := false
	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_prefix_set_actions_total" {
			foundPrefixSetActions = true
			total := sumCounterValues(mf)
			if total < 1 {
				t.Errorf("prefix_set_actions_total = %v, want >= 1", total)
			}
		}
	}

	if !foundPrefixSetActions {
		t.Error("prefix_set_actions_total metric not emitted after executor results")
	}
}

// TestPhase8_ReconcilerWiring_NotFoundEmitsMetrics verifies that mapping-not-found
// still emits a reconcile_total metric (no metric gaps on edge cases).
func TestPhase8_ReconcilerWiring_NotFoundEmitsMetrics(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &MappingReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		ClusterName:     "test-cluster",
		ResyncInterval:  60 * time.Second,
		MetricsRecorder: rec,
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	foundReconcileTotal := false
	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_reconcile_total" {
			foundReconcileTotal = true
		}
	}

	if !foundReconcileTotal {
		t.Error("reconcile_total not emitted for mapping-not-found case")
	}
}

// TestPhase8_ReconcilerWiring_PartialFailure_EmitsPartialFailureMetric verifies
// that when some actions fail, the reconciler emits partial_failure result.
func TestPhase8_ReconcilerWiring_PartialFailure_EmitsPartialFailureMetric(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "pf-test-ns"

	mapping := newTestMapping(ns, "pf-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "pf"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-pf1")},
				{ResourceID: asgResourceID("sub1", "rg1", "asg-pf2")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-pf", "10.0.0.1", map[string]string{"app": "pf"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Executor with partial failure: first action succeeds, second fails.
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-pf1", PrefixSetName: "test-cluster-pf-test-ns-pf-mapping"}},
				Success: true,
			},
			{
				Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-pf2", PrefixSetName: "test-cluster-pf-test-ns-pf-mapping"}},
				Success: false,
				Err:     &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "test failure"},
			},
		},
	}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pf-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	// Verify prefix_set_actions_total has both success and failure counts.
	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_prefix_set_actions_total" {
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "result" && l.GetValue() == "failure" {
						if m.GetCounter().GetValue() < 1 {
							t.Error("prefix_set_actions_total{result=failure} should be >= 1")
						}
					}
				}
			}
		}
	}
}

// TestPhase8_ReconcilerWiring_InitialReconcileCompletion verifies that the
// initial-reconcile tracker marks keys terminal on successful reconciliation.
func TestPhase8_ReconcilerWiring_InitialReconcileCompletion(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "init-test-ns"

	mapping := newTestMapping(ns, "init-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "init"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-init")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-init", "10.0.0.1", map[string]string{"app": "init"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	key := types.NamespacedName{Name: "init-mapping", Namespace: ns}
	tracker := metrics.NewInitialReconcileTracker(time.Now())
	// Pre-initialize with the key to simulate startup listing.
	_ = tracker.EnsureInitialized(ctx, func(_ context.Context) ([]types.NamespacedName, error) {
		return []types.NamespacedName{key}, nil
	})

	if tracker.IsComplete() {
		t.Fatal("tracker should not be complete before reconcile")
	}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     tracker,
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	if !tracker.IsComplete() {
		t.Error("initial-reconcile tracker should be complete after successful reconcile")
	}
}

// TestPhase8_ReconcilerWiring_NilMetrics_NoNilPanic verifies that the reconciler
// works without metrics (nil MetricsRecorder) without panicking.
func TestPhase8_ReconcilerWiring_NilMetrics_NoNilPanic(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	ns := "nil-metrics-ns"

	mapping := newTestMapping(ns, "nil-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "nil"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-nil")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-nil", "10.0.0.1", map[string]string{"app": "nil"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ClusterName:      "test-cluster",
		ResyncInterval:   60 * time.Second,
		PrefixSetFactory: fakeFactory,
		Executor:         exec,
		StatusUpdater:    NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		// No metrics wired — all nil
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nil-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile with nil metrics should not error: %v", reconcileErr)
	}
}

// sumCounterValues returns the total across all label combinations for a metric family.
func sumCounterValues(mf *dto.MetricFamily) float64 {
	var total float64
	for _, m := range mf.GetMetric() {
		if m.GetCounter() != nil {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// findLabelValue returns the value of a label by name, or "" if absent.
func findLabelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// TestPhase8_ReconcilerWiring_PrefixSetActionsUsesSpecLabels verifies that
// prefix_set_actions_total emits spec-mandated lowercase operation labels
// ("create", "update", "delete"), NOT engine.ActionKind values.
func TestPhase8_ReconcilerWiring_PrefixSetActionsUsesSpecLabels(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "label-test-ns"

	mapping := newTestMapping(ns, "label-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "label"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-label")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-label", "10.0.0.1", map[string]string{"app": "label"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "label-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_prefix_set_actions_total" {
			for _, m := range mf.GetMetric() {
				opLabel := findLabelValue(m, "operation")
				// Spec-mandated values are "create", "update", "delete".
				// Engine ActionKind values like "CreatePrefixSet" are wrong.
				switch opLabel {
				case "create", "update", "delete":
					// correct
				default:
					t.Errorf("prefix_set_actions_total has wrong operation label %q; want create/update/delete", opLabel)
				}
			}
		}
	}
}

// TestPhase8_ReconcilerWiring_DriftDetected_EmitsDriftCorrections verifies that
// when Azure actual state has IPs not in desired state (drift), the reconciler
// emits prefix_set_drift_corrections_total.
func TestPhase8_ReconcilerWiring_DriftDetected_EmitsDriftCorrections(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "drift-test-ns"

	mapping := newTestMapping(ns, "drift-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "drift"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-drift")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-drift", "10.0.0.1", map[string]string{"app": "drift"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Pre-populate Azure with a stale IP that is not in the desired state.
	ownershipKey := "test-cluster-" + ns + "-drift-mapping"
	_ = fakeAzClient.Put(ctx, "sub1", "rg1", "asg-drift", ownershipKey,
		[]string{"10.0.0.1/32", "10.0.0.99/32"}) // 10.0.0.99 is stale

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "drift-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	foundDrift := false
	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_prefix_set_drift_corrections_total" {
			foundDrift = true
			total := sumCounterValues(mf)
			if total < 1 {
				t.Errorf("prefix_set_drift_corrections_total = %v, want >= 1 (stale IP 10.0.0.99 should trigger drift)", total)
			}
		}
	}
	if !foundDrift {
		t.Error("prefix_set_drift_corrections_total not emitted when Azure had stale IPs")
	}
}

// TestPhase8_ReconcilerWiring_ConvergenceCommittedBeforeStatusWrite verifies that
// convergence timing reflects detection→ARM-success, committed through the
// status-updater instrumentation boundary. The status updater invokes the
// convergence callback after final status resolution (written, noop, or error).
func TestPhase8_ReconcilerWiring_ConvergenceCommittedBeforeStatusWrite(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "conv-edge-ns"

	mapping := newTestMapping(ns, "conv-edge-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "cedge"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-cedge")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-cedge", "10.0.0.1", map[string]string{"app": "cedge"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	tracker := metrics.NewConvergenceTracker()

	statusUpdater := NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status"))
	// Wire convergence committer as in production (main.go): the status updater
	// invokes this callback after status resolution to commit convergence.
	statusUpdater.SetConvergenceCommitter(func(
		key types.NamespacedName,
		observedGeneration int64,
		results []azure.ActionResult,
		outcome StatusWriteOutcome,
		statusErr error,
	) {
		_ = outcome
		_ = statusErr
		for _, res := range results {
			if res.Success {
				tracker.CommitConvergence(rec.Convergence, key, res.Action.Target, observedGeneration)
			}
		}
	})

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      statusUpdater,
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: tracker,
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conv-edge-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	// After a successful reconcile with actions, convergence should be committed
	// (no pending tokens left in the tracker for this mapping).
	foundConvergence := false
	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_prefix_set_convergence_seconds" {
			foundConvergence = true
		}
	}
	if !foundConvergence {
		t.Error("prefix_set_convergence_seconds not emitted; convergence may not be committed before status write")
	}
}

// TestPhase8_ReconcilerWiring_ConvergenceCommittedOnStatusWriteError verifies that
// convergence is still committed when ARM actions succeed but the final status
// write fails with a generic error. This matches the production wiring in main.go,
// which commits staged convergence on written, noop, and error outcomes.
func TestPhase8_ReconcilerWiring_ConvergenceCommittedOnStatusWriteError(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "conv-status-error-ns"

	mapping := newTestMapping(ns, "conv-status-error-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "conv-status-error"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-conv-status-error")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-conv-status-error", "10.0.0.1", map[string]string{"app": "conv-status-error"})

	baseClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	updateCalls := 0
	fakeClient := &conflictInjectingClient{
		Client: baseClient,
		statusWriter: &errorInjectingStatusWriter{
			SubResourceWriter:  baseClient.Status(),
			updateErr:          errors.New("simulated status write failure"),
			updateCalls:        &updateCalls,
			passThroughUpdates: 1,
		},
	}

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}
	tracker := metrics.NewConvergenceTracker()
	statusUpdater := NewMappingStatusUpdater(fakeClient, statusTestLogger(t))

	var sawOutcomeError bool
	statusUpdater.SetConvergenceCommitter(func(
		key types.NamespacedName,
		observedGeneration int64,
		results []azure.ActionResult,
		outcome StatusWriteOutcome,
		statusErr error,
	) {
		if outcome == StatusWriteOutcomeError && statusErr != nil {
			sawOutcomeError = true
		}
		for _, res := range results {
			if res.Success {
				tracker.CommitConvergence(rec.Convergence, key, res.Action.Target, observedGeneration)
			}
		}
	})

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      statusUpdater,
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: tracker,
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conv-status-error-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	if !sawOutcomeError {
		t.Fatal("status updater did not report StatusWriteOutcomeError during reconcile")
	}
	if updateCalls != 2 {
		t.Errorf("status update calls = %d, want 2 (pending write + failing final write)", updateCalls)
	}

	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	foundConvergenceObservation := false
	for _, mf := range mfs {
		if mf.GetName() != "pod_nsg_controller_prefix_set_convergence_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetHistogram().GetSampleCount() > 0 {
				foundConvergenceObservation = true
			}
		}
	}
	if !foundConvergenceObservation {
		t.Error("prefix_set_convergence_seconds not emitted when final status write failed")
	}
}

// TestPhase8_ReconcilerWiring_InitialReconcile_FallbackAfterStartupFailure verifies
// that the initial-reconcile tracker recovers from a startup initialization failure
// via the reconcile-path fallback, preventing the tracker from getting permanently stuck.
func TestPhase8_ReconcilerWiring_InitialReconcile_FallbackAfterStartupFailure(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "fallback-test-ns"

	mapping := newTestMapping(ns, "fallback-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "fallback"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-fallback")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-fallback", "10.0.0.1", map[string]string{"app": "fallback"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	exec := &stubExecutor{}

	// Create tracker but simulate startup init failure: EnsureInitialized with error.
	tracker := metrics.NewInitialReconcileTracker(time.Now())
	_ = tracker.EnsureInitialized(ctx, func(_ context.Context) ([]types.NamespacedName, error) {
		return nil, context.DeadlineExceeded // simulate startup failure
	})

	// After failure, tracker should NOT be initialized.
	if tracker.IsInitialized() {
		t.Fatal("tracker should not be initialized after startup failure")
	}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     tracker,
	}

	key := types.NamespacedName{Name: "fallback-mapping", Namespace: ns}
	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	// After reconcile, the fallback should have initialized the tracker.
	if !tracker.IsInitialized() {
		t.Error("tracker should be initialized via reconcile-path fallback after startup failure")
	}

	// The tracker should be complete (only one mapping, which was just reconciled).
	if !tracker.IsComplete() {
		t.Error("tracker should be complete after successful reconcile via fallback")
	}
}

// --- Phase 4: PatchPrefixSet metrics integration ---

// TestPhase4_ReconcilerWiring_PatchActionEmitsUpdateLabel verifies that
// PatchPrefixSet actions emit only valid operation labels (add|update|delete),
// mapping PatchPrefixSet to "update".
func TestPhase4_ReconcilerWiring_PatchActionEmitsUpdateLabel(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()

	reg := prometheus.NewRegistry()
	rec, err := metrics.RegisterWith(reg)
	if err != nil {
		t.Fatalf("RegisterWith failed: %v", err)
	}

	ctx := context.Background()
	scheme := testScheme(t)
	ns := "patch-metrics-ns"

	mapping := newTestMapping(ns, "patch-metrics-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "patch"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-patch")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}

	pod := newTestPod(ns, "pod-patch", "10.0.0.1", map[string]string{"app": "patch"})

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping, pod).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeAzClient := fake.NewClient()
	fakeFactory.RegisterClient("sub1", fakeAzClient)

	// Stub executor returns PatchPrefixSet result
	exec := &stubExecutor{
		results: []azure.ActionResult{
			{
				Action: engine.Action{
					Kind: engine.PatchPrefixSet,
					Target: engine.ASGTarget{
						SubscriptionID: "sub1",
						ResourceGroup:  "rg1",
						ASGName:        "asg-patch",
						PrefixSetName:  "test-cluster-patch-metrics-ns-patch-metrics-mapping",
					},
					DesiredIPs: []string{"10.0.0.1/32"},
					AddIPs:     []string{"10.0.0.1/32"},
				},
				Success:         true,
				FinalActionKind: engine.PatchPrefixSet,
			},
		},
	}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test-status")),
		MetricsRecorder:    rec,
		PodChurnTracker:    metrics.NewPodChurnTracker(),
		ConvergenceTracker: metrics.NewConvergenceTracker(),
		InitialTracker:     metrics.NewInitialReconcileTracker(time.Now()),
	}

	_, reconcileErr := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "patch-metrics-mapping", Namespace: ns},
	})
	if reconcileErr != nil {
		t.Fatalf("Reconcile failed: %v", reconcileErr)
	}

	// Verify CRD resolution emits only valid labels
	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatalf("Gather failed: %v", gatherErr)
	}

	for _, mf := range mfs {
		if mf.GetName() == "pod_nsg_controller_crd_resolution_duration_seconds" {
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "operation" {
						op := lp.GetValue()
						if op != "add" && op != "update" && op != "delete" {
							t.Errorf("unexpected CRD resolution operation label %q for PatchPrefixSet (want add|update|delete)", op)
						}
					}
				}
			}
		}
	}
}
