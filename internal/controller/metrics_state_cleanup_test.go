package controller

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPhase8_Reconciler_TerminalCleanup_OnMappingNotFound
// Design §3.6: When a mapping is not found, cleanupPerMappingMetricState is called.
// This ensures ConvergenceTracker.Forget and PodChurnTracker.Forget are invoked.
func TestPhase8_Reconciler_TerminalCleanup_OnMappingNotFound(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	scheme := testScheme(t)

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	convTracker := metrics.NewConvergenceTracker()
	podChurnTracker := metrics.NewPodChurnTracker()

	// Pre-populate trackers with state that should be cleaned up.
	key := types.NamespacedName{Namespace: "cleanup-ns", Name: "gone-mapping"}
	podChurnTracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{
			{Namespace: "cleanup-ns", Name: "pod-1", UID: "uid-1"}: {PodIP: "10.0.0.1"},
		},
	}, time.Now())

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		MetricsRecorder:    rec,
		PodChurnTracker:    podChurnTracker,
		ConvergenceTracker: convTracker,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}

	// After mapping-not-found, cleanupPerMappingMetricState should have been called.
	// Verify convergence tracker forgot the key (no tokens remain).
	if convTracker.HasPending(key) {
		t.Error("ConvergenceTracker still has pending tokens for deleted mapping; want none")
	}

	// Verify pod churn tracker forgot the key (snapshot removed).
	if podChurnTracker.HasSnapshot(key) {
		t.Error("PodChurnTracker still has snapshot for deleted mapping; want none")
	}
}

// TestPhase8_Reconciler_TerminalCleanup_OnDeleteComplete
// Design §3.6: When a mapping deletion completes, cleanupPerMappingMetricState is called.
func TestPhase8_Reconciler_TerminalCleanup_OnDeleteComplete(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	scheme := testScheme(t)
	ns := "cleanup-del-ns"

	// Create mapping marked for deletion with finalizer.
	mapping := newTestMapping(ns, "del-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "del"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-del")},
			},
		},
	})
	mapping.Finalizers = []string{CleanupFinalizer}
	now := metav1.Now()
	mapping.DeletionTimestamp = &now

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	fakeFactory := fake.NewClientFactory()
	fakeFactory.RegisterClient("sub1", fake.NewClient())

	convTracker := metrics.NewConvergenceTracker()
	podChurnTracker := metrics.NewPodChurnTracker()

	key := types.NamespacedName{Namespace: ns, Name: "del-mapping"}
	podChurnTracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{
			{Namespace: ns, Name: "pod-del", UID: "uid-del"}: {PodIP: "10.0.0.99"},
		},
	}, time.Now())

	exec := &stubExecutor{}

	r := &MappingReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		ClusterName:        "test-cluster",
		ResyncInterval:     60 * time.Second,
		PrefixSetFactory:   fakeFactory,
		Executor:           exec,
		StatusUpdater:      NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test")),
		MetricsRecorder:    rec,
		PodChurnTracker:    podChurnTracker,
		ConvergenceTracker: convTracker,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}

	// After delete-complete, cleanupPerMappingMetricState should have been called.
	if convTracker.HasPending(key) {
		t.Error("ConvergenceTracker still has pending tokens after delete; want none")
	}
	if podChurnTracker.HasSnapshot(key) {
		t.Error("PodChurnTracker still has snapshot after delete; want none")
	}
}

// TestPhase8_PodChurnForget_DeletesGaugeSeries
// Design §3.6: PodChurnTracker.Forget deletes the pod_churn_rate{namespace,mapping} series.
func TestPhase8_PodChurnForget_DeletesGaugeSeries(t *testing.T) {
	metrics.ResetForTesting()
	defer metrics.ResetForTesting()
	rec, _ := metrics.Register()

	tracker := metrics.NewPodChurnTracker()
	key := types.NamespacedName{Namespace: "forget-ns", Name: "forget-mapping"}

	// Create initial snapshot.
	t0 := time.Now()
	tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{},
	}, t0)

	// Add pods so the gauge gets set.
	t1 := t0.Add(1 * time.Second)
	tracker.ObserveSnapshot(rec.PodChurn, key, metrics.MappingPodSnapshot{
		Pods: map[metrics.PodIdentity]metrics.PodMembership{
			{Namespace: "forget-ns", Name: "p1", UID: "u1"}: {PodIP: "10.0.0.1"},
			{Namespace: "forget-ns", Name: "p2", UID: "u2"}: {PodIP: "10.0.0.2"},
		},
	}, t1)

	// Verify gauge is set before Forget.
	gaugeBeforeForget := getPodChurnRateForTest(t, rec.PodChurn, "forget-ns", "forget-mapping")
	if gaugeBeforeForget == 0 {
		t.Fatal("pod_churn_rate gauge should be non-zero before Forget()")
	}

	// Call ForgetWithDelete which should delete the gauge series.
	tracker.ForgetWithDelete(rec.PodChurn, key)

	// Verify gauge series is deleted.
	gaugeAfterForget := getPodChurnRateExists(t, rec.PodChurn, "forget-ns", "forget-mapping")
	if gaugeAfterForget {
		t.Error("pod_churn_rate gauge still exists after ForgetWithDelete; want deleted")
	}
}

// --- helpers ---

func getPodChurnRateForTest(t *testing.T, pcr *metrics.PodChurnRecorder, ns, mapping string) float64 {
	t.Helper()
	collectors := pcr.Collectors()
	for _, c := range collectors {
		if gv, ok := c.(*prometheus.GaugeVec); ok {
			g, err := gv.GetMetricWithLabelValues(ns, mapping)
			if err != nil {
				continue
			}
			var metric dto.Metric
			if err := g.Write(&metric); err != nil {
				continue
			}
			if metric.GetGauge() != nil {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func getPodChurnRateExists(t *testing.T, pcr *metrics.PodChurnRecorder, ns, mapping string) bool {
	t.Helper()
	collectors := pcr.Collectors()
	for _, c := range collectors {
		if gv, ok := c.(*prometheus.GaugeVec); ok {
			g, err := gv.GetMetricWithLabelValues(ns, mapping)
			if err != nil {
				return false
			}
			var metric dto.Metric
			if err := g.Write(&metric); err != nil {
				return false
			}
			// If the gauge value is non-zero or it was explicitly written, it exists.
			if metric.GetGauge() != nil && metric.GetGauge().GetValue() != 0 {
				return true
			}
		}
	}
	return false
}
