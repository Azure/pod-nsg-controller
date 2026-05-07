package controller

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func statusTestScheme(t *testing.T) *runtime.Scheme {
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

func makeASGTarget(sub, rg, name string) engine.ASGTarget {
	return engine.ASGTarget{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        name,
		FullResourceID: "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/applicationSecurityGroups/" + name,
		PrefixSetName:  "test-cluster-default-my-mapping",
	}
}

// stubPodCountSource returns pre-configured pod counts.
type stubPodCountSource struct {
	counts map[string]int
}

func (s *stubPodCountSource) CountBySelectorHash(
	_ context.Context,
	_ string,
	_ v1alpha1.PodASGMappingSpec,
) (map[string]int, error) {
	return s.counts, nil
}

// ---------------------------------------------------------------------------
// T6.1: All actions succeed → Reconciled=True, all mappings Synced
// ---------------------------------------------------------------------------

func TestComputeStatus_T61_AllActionsSucceed_AllMappingsSynced(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
			Success: true,
		},
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")},
			Success: true,
		},
	}

	podCounts := map[string]int{
		SelectorHash(map[string]string{"app": "web"}): 3,
		SelectorHash(map[string]string{"app": "api"}): 2,
	}

	status := ComputeStatus(spec, results, podCounts)

	if len(status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(status.MappingStatuses))
	}

	for i, ms := range status.MappingStatuses {
		if ms.ASGSyncState != "Synced" {
			t.Errorf("mapping[%d]: expected ASGSyncState %q, got %q", i, "Synced", ms.ASGSyncState)
		}
		if ms.Error != "" {
			t.Errorf("mapping[%d]: expected no error, got %q", i, ms.Error)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.2: One mapping's ASG update fails → Reconciled=False, failed mapping Error
// ---------------------------------------------------------------------------

func TestComputeStatus_T62_OneMappingASGFails_OnlyReferencingMappingsError(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
			Success: false,
			Err:     &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "server error"},
		},
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")},
			Success: true,
		},
	}

	podCounts := map[string]int{}

	status := ComputeStatus(spec, results, podCounts)

	if len(status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(status.MappingStatuses))
	}

	// First mapping should be Error.
	if status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Errorf("mapping[0]: expected ASGSyncState %q, got %q", "Error", status.MappingStatuses[0].ASGSyncState)
	}
	if status.MappingStatuses[0].Error == "" {
		t.Error("mapping[0]: expected non-empty error message for failed ASG")
	}

	// Second mapping should be Synced.
	if status.MappingStatuses[1].ASGSyncState != "Synced" {
		t.Errorf("mapping[1]: expected ASGSyncState %q, got %q", "Synced", status.MappingStatuses[1].ASGSyncState)
	}
}

// ---------------------------------------------------------------------------
// T6.2 extension: Multi-ASG mapping — any failure yields Error
// ---------------------------------------------------------------------------

func TestComputeStatus_MultiASGMapping_AnyFailureYieldsError(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "multi"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
			Success: true,
		},
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")},
			Success: false,
			Err:     &azure.ARMStatusError{StatusCode: 409, ARMCode: "Conflict", Message: "conflict"},
		},
	}

	podCounts := map[string]int{}

	status := ComputeStatus(spec, results, podCounts)

	if len(status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(status.MappingStatuses))
	}
	if status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Errorf("expected ASGSyncState %q for multi-ASG partial failure, got %q", "Error", status.MappingStatuses[0].ASGSyncState)
	}
}

// ---------------------------------------------------------------------------
// T6.2 extension: Shared ASG failure propagates to all referencing mappings
// ---------------------------------------------------------------------------

func TestComputeStatus_SharedASGFailure_PropagatesToAllReferencingMappings(t *testing.T) {
	sharedASG := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/shared-asg"
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: sharedASG},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "api"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: sharedASG},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "shared-asg",
					FullResourceID: sharedASG,
					PrefixSetName:  "test-cluster-default-my-mapping",
				},
			},
			Success: false,
			Err:     &azure.ARMStatusError{StatusCode: 503, ARMCode: "ServiceUnavailable", Message: "unavailable"},
		},
	}

	podCounts := map[string]int{}

	status := ComputeStatus(spec, results, podCounts)

	if len(status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(status.MappingStatuses))
	}
	for i, ms := range status.MappingStatuses {
		if ms.ASGSyncState != "Error" {
			t.Errorf("mapping[%d]: expected ASGSyncState %q (shared ASG failure propagation), got %q", i, "Error", ms.ASGSyncState)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.5: matchedPods count is accurate
// ---------------------------------------------------------------------------

func TestComputeStatus_T65_MatchedPodsAccurate(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "web"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
			Success: true,
		},
	}

	expectedHash := SelectorHash(map[string]string{"app": "web"})
	podCounts := map[string]int{
		expectedHash: 7,
	}

	status := ComputeStatus(spec, results, podCounts)

	if len(status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(status.MappingStatuses))
	}
	if status.MappingStatuses[0].MatchedPods != 7 {
		t.Errorf("expected matchedPods=7, got %d", status.MappingStatuses[0].MatchedPods)
	}
}

// ---------------------------------------------------------------------------
// T6.6: selectorHash is deterministic
// ---------------------------------------------------------------------------

func TestComputeStatus_T66_SelectorHashDeterministic(t *testing.T) {
	labels1 := map[string]string{"app": "web", "env": "prod", "tier": "frontend"}
	labels2 := map[string]string{"tier": "frontend", "app": "web", "env": "prod"}

	hash1 := SelectorHash(labels1)
	hash2 := SelectorHash(labels2)

	if hash1 == "" {
		t.Fatal("SelectorHash returned empty string")
	}
	if len(hash1) != 16 {
		t.Errorf("expected selectorHash length 16 (truncated hex), got %d", len(hash1))
	}
	if hash1 != hash2 {
		t.Errorf("same labels in different order produced different hashes: %q vs %q", hash1, hash2)
	}

	// Different labels → different hash.
	labels3 := map[string]string{"app": "api", "env": "prod", "tier": "frontend"}
	hash3 := SelectorHash(labels3)
	if hash3 == hash1 {
		t.Error("different labels produced same hash")
	}
}

// ---------------------------------------------------------------------------
// T6.4: Status preserves lastSyncTime from previous success
// ---------------------------------------------------------------------------

func TestStatusUpdater_T64_LastSyncTimeUpdatesOnlyOnSynced(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	previousSyncTime := metav1.NewTime(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	nowTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "api"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			MappingStatuses: []v1alpha1.MappingStatus{
				{ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, func() time.Time { return nowTime })

	// Mapping[0] succeeds, mapping[1] fails.
	input := ReconcileStatusInput{
		Phase:        StatusPhasePostExecution,
		ProcessedGen: 2,
		Results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: true,
			},
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")},
				Success: false,
				Err:     &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"},
			},
		},
		PodCounts: map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned unexpected error: %v", err)
	}

	// Re-fetch mapping to check status.
	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if len(updated.Status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(updated.Status.MappingStatuses))
	}

	// Synced mapping should have updated lastSyncTime to nowTime.
	syncedMS := updated.Status.MappingStatuses[0]
	expectedNow := metav1.NewTime(nowTime)
	if !syncedMS.LastSyncTime.Equal(&expectedNow) {
		t.Errorf("synced mapping: expected lastSyncTime=%v, got %v", expectedNow, syncedMS.LastSyncTime)
	}

	// Error mapping should preserve previous lastSyncTime.
	errorMS := updated.Status.MappingStatuses[1]
	if !errorMS.LastSyncTime.Equal(&previousSyncTime) {
		t.Errorf("error mapping: expected preserved lastSyncTime=%v, got %v", previousSyncTime, errorMS.LastSyncTime)
	}
}

// ---------------------------------------------------------------------------
// Status builders: Validation failure → all mappings Error
// ---------------------------------------------------------------------------

func TestStatusBuilders_ValidationFailed_AllMappingsError(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "invalid-resource-id"},
					},
				},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, time.Now)

	input := ReconcileStatusInput{
		Phase:        StatusPhaseValidationFailed,
		ProcessedGen: 1,
		ValidationErrors: map[int][]string{
			0: {"invalid ASG resource ID format: invalid-resource-id"},
		},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	// Verify Accepted=False condition.
	foundAccepted := false
	for _, c := range updated.Status.Conditions {
		if c.Type == ConditionAccepted {
			foundAccepted = true
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected Accepted=False, got %q", c.Status)
			}
			if c.Reason != "InvalidSpec" {
				t.Errorf("expected reason %q, got %q", "InvalidSpec", c.Reason)
			}
		}
	}
	if !foundAccepted {
		t.Error("expected Accepted condition to be set")
	}

	// Verify all mappings are Error.
	if len(updated.Status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(updated.Status.MappingStatuses))
	}
	if updated.Status.MappingStatuses[0].ASGSyncState != "Error" {
		t.Errorf("expected ASGSyncState %q, got %q", "Error", updated.Status.MappingStatuses[0].ASGSyncState)
	}
}

// ---------------------------------------------------------------------------
// Status builders: Bootstrap pending → all mappings Pending
// ---------------------------------------------------------------------------

func TestStatusBuilders_BootstrapPending_AllMappingsPending(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, time.Now)

	input := ReconcileStatusInput{
		Phase:        StatusPhaseBootstrapPending,
		ProcessedGen: 1,
		PodCounts:    map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	// Verify Accepted=True, Reconciled=False with BootstrapPending.
	foundReconciled := false
	for _, c := range updated.Status.Conditions {
		if c.Type == ConditionReconciled {
			foundReconciled = true
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected Reconciled=False, got %q", c.Status)
			}
			if c.Reason != "BootstrapPending" {
				t.Errorf("expected reason %q, got %q", "BootstrapPending", c.Reason)
			}
		}
	}
	if !foundReconciled {
		t.Error("expected Reconciled condition to be set")
	}

	// Verify all mappings are Pending.
	if len(updated.Status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(updated.Status.MappingStatuses))
	}
	if updated.Status.MappingStatuses[0].ASGSyncState != "Pending" {
		t.Errorf("expected ASGSyncState %q, got %q", "Pending", updated.Status.MappingStatuses[0].ASGSyncState)
	}
}

// ---------------------------------------------------------------------------
// StatusUpdater uses status subresource (client.Status().Update)
// ---------------------------------------------------------------------------

func TestStatusUpdater_UsesStatusSubresource(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{
						MatchLabels: map[string]string{"app": "web"},
					},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, time.Now)

	input := ReconcileStatusInput{
		Phase:        StatusPhasePostExecution,
		ProcessedGen: 1,
		Results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: true,
			},
		},
		PodCounts: map[string]int{},
	}

	if err := updater.UpdateAfterReconcile(ctx, mapping, input); err != nil {
		t.Fatalf("UpdateAfterReconcile returned unexpected error: %v", err)
	}

	// Verify that status was actually written (non-zero mapping statuses).
	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if len(updated.Status.MappingStatuses) == 0 {
		t.Error("expected status subresource to be updated with mapping statuses, but got none")
	}
	if len(updated.Status.Conditions) == 0 {
		t.Error("expected status subresource to be updated with conditions, but got none")
	}
}
