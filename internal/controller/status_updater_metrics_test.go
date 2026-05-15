package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPhase8_StatusUpdater_FinalStatusSuccess_InvokesConvergenceCommitter
// Design §3.5: UpdateAfterReconcile successful status update invokes committer.
func TestPhase8_StatusUpdater_FinalStatusSuccess_InvokesConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-success-ns"

	mapping := newTestMapping(ns, "su-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-su")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test"))

	var committed bool
	var committedKey types.NamespacedName
	var committedGen int64
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult) {
		committed = true
		committedKey = key
		committedGen = observedGeneration
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-su"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile() error: %v", err)
	}

	if !committed {
		t.Error("ConvergenceCommitter was NOT invoked on successful status write; want invoked")
	}
	if committedKey != key {
		t.Errorf("committed key = %v, want %v", committedKey, key)
	}
	if committedGen != 1 {
		t.Errorf("committed generation = %d, want 1", committedGen)
	}
}

// TestPhase8_StatusUpdater_FinalStatusSemanticNoop_InvokesConvergenceCommitter
// Design §3.5: statusSemanticEqual (no-op) still invokes committer.
func TestPhase8_StatusUpdater_FinalStatusSemanticNoop_InvokesConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-noop-ns"

	// Pre-populate mapping with existing status to trigger semantic no-op.
	mapping := newTestMapping(ns, "su-noop-mapping", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{
				MatchLabels: map[string]string{"app": "web"},
			},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID("sub1", "rg1", "asg-noop")},
			},
		},
	})
	mapping.Generation = 1
	mapping.Finalizers = []string{CleanupFinalizer}
	// Set conditions that match what ComputeStatus would produce for a successful
	// reconcile with the same parameters — triggers statusSemanticEqual path.
	mapping.Status = v1alpha1.PodASGMappingStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Converged",
				Message:            "All prefix sets converged",
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		WithStatusSubresource(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test"))

	var committed bool
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult) {
		committed = true
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-noop-mapping"}
	// Same results that would produce the same status → semantic no-op.
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: engine.ASGTarget{SubscriptionID: "sub1", ResourceGroup: "rg1", ASGName: "asg-noop"}},
			Success: true,
		},
	}

	err := updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})
	if err != nil {
		t.Fatalf("UpdateAfterReconcile() error: %v", err)
	}

	if !committed {
		t.Error("ConvergenceCommitter was NOT invoked on semantic no-op; want invoked")
	}
}

// TestPhase8_StatusUpdater_FinalStatusError_DoesNotInvokeConvergenceCommitter
// Design §3.5: Status write error → committer is NOT invoked.
func TestPhase8_StatusUpdater_FinalStatusError_DoesNotInvokeConvergenceCommitter(t *testing.T) {
	scheme := testScheme(t)
	ns := "su-error-ns"

	// Do NOT create the mapping → status update will fail with not-found sentinel.
	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, ctrl.Log.WithName("test"))

	var committed bool
	updater.SetConvergenceCommitter(func(key types.NamespacedName, observedGeneration int64, results []azure.ActionResult) {
		committed = true
	})

	key := types.NamespacedName{Namespace: ns, Name: "su-missing-mapping"}
	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.CreatePrefixSet},
			Success: true,
		},
	}

	// This should return ErrStatusObjectNotFound.
	_ = updater.UpdateAfterReconcile(context.Background(), key, 1, "test-cluster-ns-mapping", results, nil, nil, []int{1})

	if committed {
		t.Error("ConvergenceCommitter was invoked on status error; want NOT invoked")
	}
}
