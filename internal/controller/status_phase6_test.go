package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// T6.1 extension: ComputeStatus sets MappingCount from ProcessedSpec
// ---------------------------------------------------------------------------

func TestComputeStatus_SetsMappingCount_FromProcessedSpec(t *testing.T) {
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
			{
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "worker"},
				},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg3"},
				},
			},
		},
	}

	results := []azure.ActionResult{
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")}, Success: true},
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")}, Success: true},
		{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg3")}, Success: true},
	}

	status := ComputeStatus(spec, results, map[string]int{})

	if status.MappingCount != 3 {
		t.Errorf("expected MappingCount=3, got %d", status.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// T6.3 extension: buildValidationFailedStatus sets MappingCount
// ---------------------------------------------------------------------------

func TestBuildValidationFailedStatus_SetsMappingCount(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "invalid-id"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "also-invalid"},
				},
			},
		},
	}

	validationErrors := map[int][]string{
		0: {"invalid resource ID format"},
		1: {"invalid resource ID format"},
	}

	status := buildValidationFailedStatus(spec, validationErrors, map[string]int{})

	if status.MappingCount != 2 {
		t.Errorf("expected MappingCount=2, got %d", status.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// T6.3 extension: buildBootstrapPendingStatus sets MappingCount
// ---------------------------------------------------------------------------

func TestBuildBootstrapPendingStatus_SetsMappingCount(t *testing.T) {
	spec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
				},
			},
			{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			},
		},
	}

	status := buildBootstrapPendingStatus(spec, map[string]int{})

	if status.MappingCount != 2 {
		t.Errorf("expected MappingCount=2, got %d", status.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// T6.4: StatusUpdater generation drift returns typed error
// ---------------------------------------------------------------------------

func TestStatusUpdater_PostExecution_GenerationDrift_ReturnsTypedError(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	// Create mapping with Generation=3 (simulating a spec update during reconcile).
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "drift-mapping",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
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

	// Input was processed at Generation=2, but mapping is now at Generation=3.
	input := ReconcileStatusInput{
		Phase:        StatusPhasePostExecution,
		ProcessedGen: 2, // stale generation
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")}, Success: true},
		},
		PodCounts: map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)

	// Should return a StatusGenerationDriftError.
	var driftErr *StatusGenerationDriftError
	if !errors.As(err, &driftErr) {
		t.Fatalf("expected *StatusGenerationDriftError, got %v (type %T)", err, err)
	}
	if driftErr.ProcessedGeneration != 2 {
		t.Errorf("expected ProcessedGeneration=2, got %d", driftErr.ProcessedGeneration)
	}
	if driftErr.LiveGeneration != 3 {
		t.Errorf("expected LiveGeneration=3, got %d", driftErr.LiveGeneration)
	}
}

// ---------------------------------------------------------------------------
// T6.4: lastSyncTime carry-forward by identity (safe, schema-compatible)
// ---------------------------------------------------------------------------

func TestStatusUpdater_T64_LastSyncTime_CarryForwardByIdentity(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	nowTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "identity-mapping",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{
					Type:               ConditionReconciled,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 2,
				},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{SelectorHash: SelectorHash(map[string]string{"app": "api"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, func() time.Time { return nowTime })

	// mapping[0] (web) succeeds → lastSyncTime updated to now.
	// mapping[1] (api) fails → lastSyncTime should be carried forward by identity.
	input := ReconcileStatusInput{
		Phase:                      StatusPhasePostExecution,
		ProcessedGen:               2,
		ProcessedSpec:              mapping.Spec,
		PreviousMappingStatuses:    mapping.Status.MappingStatuses,
		PreviousObservedGeneration: 2,
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")}, Success: true},
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")}, Success: false, Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"}},
		},
		PodCounts: map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if len(updated.Status.MappingStatuses) != 2 {
		t.Fatalf("expected 2 mapping statuses, got %d", len(updated.Status.MappingStatuses))
	}

	// Synced mapping should have lastSyncTime = now.
	expectedNow := metav1.NewTime(nowTime)
	if !updated.Status.MappingStatuses[0].LastSyncTime.Equal(&expectedNow) {
		t.Errorf("synced mapping: expected lastSyncTime=%v, got %v", expectedNow, updated.Status.MappingStatuses[0].LastSyncTime)
	}

	// Error mapping should carry forward by identity (not by index).
	// With identity-based carry-forward, the identity must match exactly.
	identity := MappingIdentity(mapping.Spec.Mappings[1])
	if identity == "" {
		t.Fatal("MappingIdentity returned empty string; stub not yet implemented")
	}

	if !updated.Status.MappingStatuses[1].LastSyncTime.Equal(&previousSyncTime) {
		t.Errorf("error mapping: expected carried-forward lastSyncTime=%v, got %v",
			previousSyncTime, updated.Status.MappingStatuses[1].LastSyncTime)
	}
}

// ---------------------------------------------------------------------------
// T6.4: No carry-forward when identity is ambiguous (duplicate identities)
// ---------------------------------------------------------------------------

func TestStatusUpdater_T64_NoCarryForward_WhenIdentityAmbiguous(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	nowTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Two mappings with IDENTICAL selectors and ASGs (ambiguous identity).
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ambiguous-mapping",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{Type: ConditionReconciled, Status: metav1.ConditionTrue, ObservedGeneration: 2},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, func() time.Time { return nowTime })

	// Both mappings fail → ambiguous identity, should NOT carry forward.
	input := ReconcileStatusInput{
		Phase:                      StatusPhasePostExecution,
		ProcessedGen:               2,
		ProcessedSpec:              mapping.Spec,
		PreviousMappingStatuses:    mapping.Status.MappingStatuses,
		PreviousObservedGeneration: 2,
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")}, Success: false, Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"}},
		},
		PodCounts: map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	// With ambiguous identities, carry-forward should NOT happen.
	// LastSyncTime should be zero for Error mappings (no carry-forward).
	for i, ms := range updated.Status.MappingStatuses {
		if ms.ASGSyncState == "Error" && !ms.LastSyncTime.IsZero() {
			t.Errorf("mapping[%d]: expected zero LastSyncTime for ambiguous identity Error mapping, got %v",
				i, ms.LastSyncTime)
		}
	}
}

// ---------------------------------------------------------------------------
// T6.4: No carry-forward when PreviousObservedGeneration differs from ProcessedGen
// ---------------------------------------------------------------------------

func TestStatusUpdater_T64_NoCarryForward_WhenPreviousObservedGenerationDiffers(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))
	nowTime := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "gen-mismatch-mapping",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{Type: ConditionReconciled, Status: metav1.ConditionTrue, ObservedGeneration: 1}, // old gen
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	fakeClient := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		WithObjects(mapping).
		Build()

	updater := NewMappingStatusUpdater(fakeClient, func() time.Time { return nowTime })

	// ProcessedGen=3 but PreviousObservedGeneration=1 (mismatch → no carry-forward).
	input := ReconcileStatusInput{
		Phase:                      StatusPhasePostExecution,
		ProcessedGen:               3,
		ProcessedSpec:              mapping.Spec,
		PreviousMappingStatuses:    mapping.Status.MappingStatuses,
		PreviousObservedGeneration: 1, // differs from ProcessedGen
		Results: []azure.ActionResult{
			{Action: engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")}, Success: false, Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"}},
		},
		PodCounts: map[string]int{},
	}

	err := updater.UpdateAfterReconcile(ctx, mapping, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if len(updated.Status.MappingStatuses) != 1 {
		t.Fatalf("expected 1 mapping status, got %d", len(updated.Status.MappingStatuses))
	}

	// With PreviousObservedGeneration != ProcessedGen, carry-forward is disabled.
	// Error mapping should have zero LastSyncTime.
	ms := updated.Status.MappingStatuses[0]
	if ms.ASGSyncState != "Error" {
		t.Fatalf("expected ASGSyncState=Error, got %q", ms.ASGSyncState)
	}
	if !ms.LastSyncTime.IsZero() {
		t.Errorf("expected zero LastSyncTime when PreviousObservedGeneration != ProcessedGen, got %v",
			ms.LastSyncTime)
	}
}

func TestStatusUpdater_GenerationDrift_ZeroProcessedGenerationDoesNotDrift(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_ = logger

	selectorLabels := map[string]string{"app": "web"}
	selectorHash := SelectorHash(selectorLabels)

	scheme := statusTestScheme(t)
	ctx := context.Background()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zero-gen-mapping",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: selectorLabels},
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
	err := updater.UpdateAfterReconcile(ctx, mapping, ReconcileStatusInput{
		Phase:        StatusPhasePostExecution,
		ProcessedGen: 0,
		ProcessedSpec: v1alpha1.PodASGMappingSpec{
			Mappings: append([]v1alpha1.Mapping(nil), mapping.Spec.Mappings...),
		},
		Results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: true,
			},
		},
		PodCounts: map[string]int{
			selectorHash: 2,
		},
	})

	var driftErr *StatusGenerationDriftError
	if errors.As(err, &driftErr) {
		t.Fatalf("expected no generation drift error, got %v", driftErr)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
		t.Fatalf("failed to get updated mapping: %v", err)
	}

	if got, want := updated.Status.MappingStatuses[0].MatchedPods, 2; got != want {
		t.Errorf("matchedPods = %d, want %d", got, want)
	}
}

func TestStatusUpdater_GenerationDrift_ZeroLiveGenerationReturnsTypedError(t *testing.T) {
	scheme := statusTestScheme(t)
	ctx := context.Background()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zero-live-gen-mapping",
			Namespace:  "default",
			Generation: 0,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
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
	err := updater.UpdateAfterReconcile(ctx, mapping, ReconcileStatusInput{
		Phase:        StatusPhasePostExecution,
		ProcessedGen: 2,
		ProcessedSpec: v1alpha1.PodASGMappingSpec{
			Mappings: append([]v1alpha1.Mapping(nil), mapping.Spec.Mappings...),
		},
		Results: []azure.ActionResult{
			{
				Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
				Success: true,
			},
		},
	})

	var driftErr *StatusGenerationDriftError
	if !errors.As(err, &driftErr) {
		t.Fatalf("expected *StatusGenerationDriftError, got %v (type %T)", err, err)
	}
	if driftErr.ProcessedGeneration != 2 {
		t.Errorf("expected ProcessedGeneration=2, got %d", driftErr.ProcessedGeneration)
	}
	if driftErr.LiveGeneration != 0 {
		t.Errorf("expected LiveGeneration=0, got %d", driftErr.LiveGeneration)
	}
}

func TestStatusUpdater_UsesInputPodCountsWithoutCallingPodCountSource(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_ = logger

	selectorLabels := map[string]string{"app": "web"}
	selectorHash := SelectorHash(selectorLabels)

	testCases := []struct {
		name      string
		phase     StatusPhase
		wantState string
	}{
		{
			name:      "post execution",
			phase:     StatusPhasePostExecution,
			wantState: "Synced",
		},
		{
			name:      "validation failed",
			phase:     StatusPhaseValidationFailed,
			wantState: "Error",
		},
		{
			name:      "bootstrap pending",
			phase:     StatusPhaseBootstrapPending,
			wantState: "Pending",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := statusTestScheme(t)
			ctx := context.Background()

			mapping := &v1alpha1.PodASGMapping{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "pod-counts-mapping",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: v1alpha1.PodASGMappingSpec{
					Mappings: []v1alpha1.Mapping{
						{
							PodSelector: v1alpha1.PodSelector{MatchLabels: selectorLabels},
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
				Phase:        tc.phase,
				ProcessedGen: 1,
				ProcessedSpec: v1alpha1.PodASGMappingSpec{
					Mappings: append([]v1alpha1.Mapping(nil), mapping.Spec.Mappings...),
				},
				PodCounts: map[string]int{
					selectorHash: 4,
				},
			}
			if tc.phase == StatusPhasePostExecution {
				input.Results = []azure.ActionResult{
					{
						Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
						Success: true,
					},
				}
			}
			if tc.phase == StatusPhaseValidationFailed {
				input.ValidationErrors = map[int][]string{
					0: {"invalid resource ID format"},
				}
			}

			if err := updater.UpdateAfterReconcile(ctx, mapping, input); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var updated v1alpha1.PodASGMapping
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
				t.Fatalf("failed to get updated mapping: %v", err)
			}

			if got, want := updated.Status.MappingStatuses[0].MatchedPods, 4; got != want {
				t.Errorf("matchedPods = %d, want %d", got, want)
			}
			if got, want := updated.Status.MappingStatuses[0].ASGSyncState, tc.wantState; got != want {
				t.Errorf("ASGSyncState = %q, want %q", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Snapshot helper tests: captureProcessedStatusSnapshot deep-copies inputs
// Design §3.3: "Snapshot performs deep-copy semantics for spec/status slices and map."
// ---------------------------------------------------------------------------

func TestCaptureProcessedStatusSnapshot_DeepCopiesInputs(t *testing.T) {
	_ = zaptest.NewLogger(t)

	originalPodCounts := map[string]int{
		"hash-web": 3,
		"hash-api": 5,
	}
	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "snapshot-test",
			Namespace:  "default",
			Generation: 7,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
					},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			Conditions: []metav1.Condition{
				{Type: ConditionReconciled, Status: metav1.ConditionTrue, ObservedGeneration: 6},
			},
			MappingStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: "hash-web", ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{SelectorHash: "hash-api", ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	snapshot := captureProcessedStatusSnapshot(mapping, originalPodCounts)

	// Verify snapshot captured the correct generation.
	if snapshot.Generation != 7 {
		t.Errorf("expected Generation=7, got %d", snapshot.Generation)
	}

	// Verify snapshot captured the spec mappings.
	if len(snapshot.Spec.Mappings) != 2 {
		t.Errorf("expected 2 spec mappings in snapshot, got %d", len(snapshot.Spec.Mappings))
	}

	// Verify snapshot captured previous mapping statuses.
	if len(snapshot.PreviousMappingStatuses) != 2 {
		t.Errorf("expected 2 previous mapping statuses in snapshot, got %d", len(snapshot.PreviousMappingStatuses))
	}

	// Verify snapshot captured PreviousObservedGen from Reconciled condition.
	if snapshot.PreviousObservedGen != 6 {
		t.Errorf("expected PreviousObservedGen=6, got %d", snapshot.PreviousObservedGen)
	}

	// Verify pod counts are captured.
	if snapshot.PodCounts["hash-web"] != 3 {
		t.Errorf("expected PodCounts[hash-web]=3, got %d", snapshot.PodCounts["hash-web"])
	}
	if snapshot.PodCounts["hash-api"] != 5 {
		t.Errorf("expected PodCounts[hash-api]=5, got %d", snapshot.PodCounts["hash-api"])
	}

	// Deep-copy verification: mutate original mapping AFTER snapshot.
	mapping.Generation = 99
	mapping.Spec.Mappings = nil
	mapping.Status.MappingStatuses = nil
	originalPodCounts["hash-web"] = 999

	// Snapshot must NOT be affected by mutations to the original.
	if snapshot.Generation != 7 {
		t.Errorf("snapshot Generation mutated: expected 7, got %d", snapshot.Generation)
	}
	if len(snapshot.Spec.Mappings) != 2 {
		t.Errorf("snapshot Spec.Mappings mutated: expected 2, got %d", len(snapshot.Spec.Mappings))
	}
	if len(snapshot.PreviousMappingStatuses) != 2 {
		t.Errorf("snapshot PreviousMappingStatuses mutated: expected 2, got %d", len(snapshot.PreviousMappingStatuses))
	}
	if snapshot.PodCounts["hash-web"] != 3 {
		t.Errorf("snapshot PodCounts mutated: expected 3, got %d", snapshot.PodCounts["hash-web"])
	}
}

// ---------------------------------------------------------------------------
// Snapshot helper tests: buildPostExecutionStatusInput uses snapshot fields
// Design §3.3C-4: "Build ReconcileStatusInput only from snapshot + execution outcomes"
// ---------------------------------------------------------------------------

func TestBuildPostExecutionStatusInput_UsesSnapshotFields(t *testing.T) {
	_ = zaptest.NewLogger(t)

	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))

	snapshot := processedStatusSnapshot{
		Generation: 5,
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
					},
				},
			},
		},
		PreviousMappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: "hash-web", ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
		},
		PreviousObservedGen: 4,
		PodCounts: map[string]int{
			"hash-web": 3,
		},
	}

	results := []azure.ActionResult{
		{
			Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
			Success: true,
		},
	}
	reconcileErr := errors.New("partial failure")

	input := buildPostExecutionStatusInput(snapshot, results, reconcileErr)

	// Must use StatusPhasePostExecution.
	if input.Phase != StatusPhasePostExecution {
		t.Errorf("expected Phase=%q, got %q", StatusPhasePostExecution, input.Phase)
	}

	// Must use snapshot.Generation as ProcessedGen.
	if input.ProcessedGen != 5 {
		t.Errorf("expected ProcessedGen=5 (from snapshot), got %d", input.ProcessedGen)
	}

	// Must use snapshot.Spec as ProcessedSpec.
	if len(input.ProcessedSpec.Mappings) != 1 {
		t.Errorf("expected 1 ProcessedSpec mapping (from snapshot), got %d", len(input.ProcessedSpec.Mappings))
	}

	// Must use snapshot.PreviousMappingStatuses.
	if len(input.PreviousMappingStatuses) != 1 {
		t.Errorf("expected 1 PreviousMappingStatuses (from snapshot), got %d", len(input.PreviousMappingStatuses))
	}

	// Must use snapshot.PreviousObservedGen.
	if input.PreviousObservedGeneration != 4 {
		t.Errorf("expected PreviousObservedGeneration=4 (from snapshot), got %d", input.PreviousObservedGeneration)
	}

	// Must use snapshot.PodCounts.
	if input.PodCounts["hash-web"] != 3 {
		t.Errorf("expected PodCounts[hash-web]=3 (from snapshot), got %d", input.PodCounts["hash-web"])
	}

	// Must include results.
	if len(input.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(input.Results))
	}

	// Must include reconcileErr.
	if input.ReconcileErr == nil {
		t.Error("expected ReconcileErr to be set from argument")
	} else if input.ReconcileErr.Error() != "partial failure" {
		t.Errorf("expected ReconcileErr message %q, got %q", "partial failure", input.ReconcileErr.Error())
	}
}

// ---------------------------------------------------------------------------
// T6.4: No carry-forward when previous status length differs
// ---------------------------------------------------------------------------

func TestStatusUpdater_T64_NoCarryForward_WhenPreviousStatusLengthDiffers(t *testing.T) {
	logger := zaptest.NewLogger(t)
	_ = logger

	previousSyncTime := metav1.NewTime(time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC))

	testCases := []struct {
		name             string
		previousStatuses []v1alpha1.MappingStatus
	}{
		{
			name: "previous shorter than current spec",
			previousStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
		{
			name: "previous longer than current spec",
			previousStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: SelectorHash(map[string]string{"app": "web"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{SelectorHash: SelectorHash(map[string]string{"app": "api"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
				{SelectorHash: SelectorHash(map[string]string{"app": "extra"}), ASGSyncState: "Synced", LastSyncTime: previousSyncTime},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := statusTestScheme(t)
			ctx := context.Background()

			mapping := &v1alpha1.PodASGMapping{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "length-mismatch-mapping",
					Namespace:  "default",
					Generation: 4,
				},
				Spec: v1alpha1.PodASGMappingSpec{
					Mappings: []v1alpha1.Mapping{
						{
							PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
							ApplicationSecurityGroups: []v1alpha1.ASGReference{
								{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"},
							},
						},
						{
							PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
							ApplicationSecurityGroups: []v1alpha1.ASGReference{
								{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
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
			err := updater.UpdateAfterReconcile(ctx, mapping, ReconcileStatusInput{
				Phase:                      StatusPhasePostExecution,
				ProcessedGen:               4,
				ProcessedSpec:              mapping.Spec,
				PreviousMappingStatuses:    tc.previousStatuses,
				PreviousObservedGeneration: 4,
				Results: []azure.ActionResult{
					{
						Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg1")},
						Success: false,
						Err:     &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"},
					},
					{
						Action:  engine.Action{Kind: engine.UpdatePrefixSet, Target: makeASGTarget("sub1", "rg1", "asg2")},
						Success: false,
						Err:     &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalError", Message: "fail"},
					},
				},
				PodCounts: map[string]int{},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var updated v1alpha1.PodASGMapping
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(mapping), &updated); err != nil {
				t.Fatalf("failed to get updated mapping: %v", err)
			}

			for i, ms := range updated.Status.MappingStatuses {
				if got, want := ms.ASGSyncState, "Error"; got != want {
					t.Errorf("mapping[%d] ASGSyncState = %q, want %q", i, got, want)
				}
				if !ms.LastSyncTime.IsZero() {
					t.Errorf("mapping[%d] LastSyncTime = %v, want zero", i, ms.LastSyncTime)
				}
			}
		})
	}
}
