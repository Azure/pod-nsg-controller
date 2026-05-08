package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap/zaptest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
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

func statusTestLogger(t *testing.T) logr.Logger {
	t.Helper()
	return zapr.NewLogger(zaptest.NewLogger(t))
}

type conflictInjectingClient struct {
	crclient.Client
	statusWriter crclient.SubResourceWriter
}

func (c *conflictInjectingClient) Status() crclient.SubResourceWriter {
	return c.statusWriter
}

type conflictInjectingStatusWriter struct {
	crclient.SubResourceWriter
	conflictsRemaining *int
	updateCalls        *int
}

func (w *conflictInjectingStatusWriter) Create(ctx context.Context, obj crclient.Object, subResource crclient.Object, opts ...crclient.SubResourceCreateOption) error {
	return w.SubResourceWriter.Create(ctx, obj, subResource, opts...)
}

func (w *conflictInjectingStatusWriter) Update(ctx context.Context, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
	*w.updateCalls = *w.updateCalls + 1
	if *w.conflictsRemaining > 0 {
		*w.conflictsRemaining--
		return apierrors.NewConflict(
			schema.GroupResource{Group: "networking.azure.com", Resource: "podasgmappings"},
			obj.GetName(),
			fmt.Errorf("simulated conflict"),
		)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *conflictInjectingStatusWriter) Patch(ctx context.Context, obj crclient.Object, patch crclient.Patch, opts ...crclient.SubResourcePatchOption) error {
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// ---------------------------------------------------------------------------
// TestUpdatePending_WritesPendingStatus
// ---------------------------------------------------------------------------

func TestUpdatePending_WritesPendingStatus(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "test-mapping", Namespace: "default"}
	err := updater.UpdatePending(context.Background(), key, 1, "cluster__default__test-mapping", []int{5})
	if err != nil {
		t.Fatalf("UpdatePending returned error: %v", err)
	}

	// Refetch and validate pending status was written.
	var fetched v1alpha1.PodASGMapping
	if err := fc.Get(context.Background(), key, &fetched); err != nil {
		t.Fatalf("failed to fetch mapping: %v", err)
	}

	if len(fetched.Status.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(fetched.Status.MappingStatuses))
	}
	if fetched.Status.MappingStatuses[0].ASGSyncState != SyncStatePending {
		t.Errorf("ASGSyncState = %q, want %q", fetched.Status.MappingStatuses[0].ASGSyncState, SyncStatePending)
	}

	accepted := conditionByType(fetched.Status, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition after UpdatePending")
	}
	if accepted.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %q, want True", accepted.Status)
	}
}

// ---------------------------------------------------------------------------
// TestUpdatePending_NotFoundReturnsErrStatusObjectNotFound
// ---------------------------------------------------------------------------

func TestUpdatePending_NotFoundReturnsErrStatusObjectNotFound(t *testing.T) {
	scheme := statusTestScheme(t)

	// No objects in the fake client → Get will return NotFound.
	fc := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "gone", Namespace: "default"}
	err := updater.UpdatePending(context.Background(), key, 1, "prefix", []int{0})

	if err == nil {
		t.Fatal("expected error for missing object, got nil")
	}
	if err != ErrStatusObjectNotFound {
		t.Errorf("error = %v, want ErrStatusObjectNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// TestUpdatePending_StaleGenerationReturnsErrStatusStaleGeneration
// ---------------------------------------------------------------------------

func TestUpdatePending_StaleGenerationReturnsErrStatusStaleGeneration(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-mapping",
			Namespace:  "default",
			Generation: 5, // Current generation is 5
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "test-mapping", Namespace: "default"}
	// Pass observedGeneration=3, which is stale compared to current generation=5.
	err := updater.UpdatePending(context.Background(), key, 3, "prefix", []int{0})

	if err == nil {
		t.Fatal("expected error for stale generation, got nil")
	}
	if err != ErrStatusStaleGeneration {
		t.Errorf("error = %v, want ErrStatusStaleGeneration", err)
	}
}

// ---------------------------------------------------------------------------
// TestUpdatePending_ConflictRetry
// ---------------------------------------------------------------------------

func TestUpdatePending_ConflictRetry(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "retry-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "retry"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	conflictsRemaining := 1
	updateCalls := 0
	cc := &conflictInjectingClient{
		Client: fc,
		statusWriter: &conflictInjectingStatusWriter{
			SubResourceWriter:  fc.Status(),
			conflictsRemaining: &conflictsRemaining,
			updateCalls:        &updateCalls,
		},
	}
	updater := &MappingStatusUpdater{
		Client:      cc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "retry-mapping", Namespace: "default"}
	err := updater.UpdatePending(context.Background(), key, 1, "prefix", []int{2})
	if err != nil {
		t.Fatalf("UpdatePending returned error: %v", err)
	}
	if updateCalls != 2 {
		t.Errorf("status update calls = %d, want 2", updateCalls)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}
	if len(fetched.Status.MappingStatuses) == 0 {
		t.Error("expected pending status to be written, got empty MappingStatuses")
	}
	if len(fetched.Status.MappingStatuses) > 0 && fetched.Status.MappingStatuses[0].ASGSyncState != SyncStatePending {
		t.Errorf("ASGSyncState = %q, want %q", fetched.Status.MappingStatuses[0].ASGSyncState, SyncStatePending)
	}
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_ConflictRetry_StopsAtMaxAttempts
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_ConflictRetry_StopsAtMaxAttempts(t *testing.T) {
	scheme := statusTestScheme(t)
	prefix := "cluster__default__mapping"
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "final-conflict-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	conflictsRemaining := 3
	updateCalls := 0
	cc := &conflictInjectingClient{
		Client: fc,
		statusWriter: &conflictInjectingStatusWriter{
			SubResourceWriter:  fc.Status(),
			conflictsRemaining: &conflictsRemaining,
			updateCalls:        &updateCalls,
		},
	}
	updater := &MappingStatusUpdater{
		Client:      cc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 2,
	}

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1",
					PrefixSetName:  prefix,
				},
			},
			Success: true,
		},
	}

	key := types.NamespacedName{Name: "final-conflict-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(context.Background(), key, 1, prefix, results, nil, nil, []int{1})
	if err == nil {
		t.Fatal("expected conflict error after exhausting retries, got nil")
	}
	if !apierrors.IsConflict(err) {
		t.Errorf("error = %v, want conflict", err)
	}
	if updateCalls != 2 {
		t.Errorf("status update calls = %d, want 2", updateCalls)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}
	if fetched.Status.MappingCount != 0 {
		t.Errorf("MappingCount = %d, want 0 after failed status updates", fetched.Status.MappingCount)
	}
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_WritesFinalStatus
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_WritesFinalStatus(t *testing.T) {
	scheme := statusTestScheme(t)
	prefix := "cluster__default__mapping"
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "final-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1",
					PrefixSetName:  prefix,
				},
			},
			Success: true,
		},
	}

	key := types.NamespacedName{Name: "final-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(),
		key,
		1,
		prefix,
		results,
		nil, // no reconcile error
		nil, // no validation issues
		[]int{3},
	)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned error: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}

	if fetched.Status.MappingCount != 1 {
		t.Errorf("MappingCount = %d, want 1", fetched.Status.MappingCount)
	}
	if len(fetched.Status.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(fetched.Status.MappingStatuses))
	}
	if fetched.Status.MappingStatuses[0].ASGSyncState != SyncStateSynced {
		t.Errorf("ASGSyncState = %q, want %q", fetched.Status.MappingStatuses[0].ASGSyncState, SyncStateSynced)
	}

	reconciled := conditionByType(fetched.Status, ConditionReconciled)
	if reconciled == nil {
		t.Fatal("missing Reconciled condition")
	}
	if reconciled.Status != metav1.ConditionTrue {
		t.Errorf("Reconciled = %q, want True", reconciled.Status)
	}
}

// ---------------------------------------------------------------------------
// T6.7 — Status-only update does not modify spec or metadata
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_StatusOnlyMutation_DoesNotChangeSpecOrMetadata(t *testing.T) {
	scheme := statusTestScheme(t)
	prefix := "cluster__default__mapping"

	originalSpec := v1alpha1.PodASGMappingSpec{
		Mappings: []v1alpha1.Mapping{
			{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "immutable"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
			},
		},
	}

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "immutable-mapping",
			Namespace:   "default",
			Generation:  1,
			Labels:      map[string]string{"original": "true"},
			Annotations: map[string]string{"note": "do-not-change"},
		},
		Spec: originalSpec,
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	results := []azure.ActionResult{
		{
			Action: engine.Action{
				Kind: engine.UpdatePrefixSet,
				Target: engine.ASGTarget{
					SubscriptionID: "sub1",
					ResourceGroup:  "rg1",
					ASGName:        "asg1",
					FullResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1",
					PrefixSetName:  prefix,
				},
			},
			Success: true,
		},
	}

	key := types.NamespacedName{Name: "immutable-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(),
		key,
		1,
		prefix,
		results,
		nil,
		nil,
		[]int{1},
	)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned error: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}

	// Spec must not have changed.
	if len(fetched.Spec.Mappings) != len(originalSpec.Mappings) {
		t.Fatalf("Spec.Mappings len changed: got %d, want %d", len(fetched.Spec.Mappings), len(originalSpec.Mappings))
	}
	if fetched.Spec.Mappings[0].PodSelector.MatchLabels["app"] != "immutable" {
		t.Errorf("Spec.Mappings[0] label changed")
	}

	// Labels and annotations must not have changed.
	if fetched.Labels["original"] != "true" {
		t.Errorf("Labels changed: %v", fetched.Labels)
	}
	if fetched.Annotations["note"] != "do-not-change" {
		t.Errorf("Annotations changed: %v", fetched.Annotations)
	}

	// But status should have been updated (at minimum, MappingCount should reflect spec).
	if fetched.Status.MappingCount != 1 {
		t.Errorf("MappingCount = %d, want 1 (status was not updated)", fetched.Status.MappingCount)
	}

	_ = fmt.Sprintf("verified T6.7: status-only mutation")
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_NotFoundReturnsErrStatusObjectNotFound
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_NotFoundReturnsErrStatusObjectNotFound(t *testing.T) {
	scheme := statusTestScheme(t)

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "gone", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(), key, 1, "prefix",
		nil, nil, nil, []int{0},
	)

	if err == nil {
		t.Fatal("expected error for missing object, got nil")
	}
	if err != ErrStatusObjectNotFound {
		t.Errorf("error = %v, want ErrStatusObjectNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_StaleGenerationReturnsErrStatusStaleGeneration
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_StaleGenerationReturnsErrStatusStaleGeneration(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "stale-mapping",
			Namespace:  "default",
			Generation: 10,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "stale-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(), key, 5, "prefix", // observedGeneration=5 < current=10
		nil, nil, nil, []int{0},
	)

	if err == nil {
		t.Fatal("expected error for stale generation, got nil")
	}
	if err != ErrStatusStaleGeneration {
		t.Errorf("error = %v, want ErrStatusStaleGeneration", err)
	}
}

// ---------------------------------------------------------------------------
// TestNewMappingStatusUpdater_Defaults
// ---------------------------------------------------------------------------

func TestNewMappingStatusUpdater_Defaults(t *testing.T) {
	scheme := statusTestScheme(t)
	fc := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	logger := statusTestLogger(t)

	updater := NewMappingStatusUpdater(fc, logger)

	if updater.Client != fc {
		t.Error("Client not set correctly")
	}
	if updater.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", updater.MaxAttempts)
	}
	if updater.Now == nil {
		t.Error("Now func is nil")
	}
	// Verify Now func produces a non-zero time.
	ts := updater.Now()
	if ts.IsZero() {
		t.Error("Now() returned zero time")
	}
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_ValidationIssues_WritesAcceptedFalse
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_ValidationIssues_WritesAcceptedFalse(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "invalid-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "not-a-valid-id"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	validationIssues := []ValidationIssue{
		{MappingIndex: 0, ASGIndex: 0, ResourceID: "not-a-valid-id", Err: fmt.Errorf("invalid resource ID")},
	}

	key := types.NamespacedName{Name: "invalid-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(), key, 1, "prefix",
		nil,                             // no results
		fmt.Errorf("validation failed"), // reconcileErr
		validationIssues,
		[]int{0},
	)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned error: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}

	accepted := conditionByType(fetched.Status, ConditionAccepted)
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %q, want False", accepted.Status)
	}

	if len(fetched.Status.MappingStatuses) != 1 {
		t.Fatalf("MappingStatuses len = %d, want 1", len(fetched.Status.MappingStatuses))
	}
	if fetched.Status.MappingStatuses[0].ASGSyncState != SyncStateError {
		t.Errorf("ASGSyncState = %q, want %q", fetched.Status.MappingStatuses[0].ASGSyncState, SyncStateError)
	}
}

// ---------------------------------------------------------------------------
// TestUpdatePending_MatchedPodsWrittenToStatus
// ---------------------------------------------------------------------------

func TestUpdatePending_MatchedPodsWrittenToStatus(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pods-mapping",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "pods-mapping", Namespace: "default"}
	err := updater.UpdatePending(context.Background(), key, 1, "prefix", []int{10, 3})
	if err != nil {
		t.Fatalf("UpdatePending returned error: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}

	if len(fetched.Status.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(fetched.Status.MappingStatuses))
	}
	if fetched.Status.MappingStatuses[0].MatchedPods != 10 {
		t.Errorf("MappingStatuses[0].MatchedPods = %d, want 10", fetched.Status.MappingStatuses[0].MatchedPods)
	}
	if fetched.Status.MappingStatuses[1].MatchedPods != 3 {
		t.Errorf("MappingStatuses[1].MatchedPods = %d, want 3", fetched.Status.MappingStatuses[1].MatchedPods)
	}
}

// ---------------------------------------------------------------------------
// T6.4 — Updater preserves LastSyncTime on reorder by selector hash
// End-to-end updater test: pre-seed status with [A, B], then update with
// reordered spec [B, A] where both fail. LastSyncTime must follow selector.
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_T64_PreservesLastSyncTimeOnReorderBySelectorHash(t *testing.T) {
	scheme := statusTestScheme(t)
	prefix := "cluster__default__reorder"

	labelsA := map[string]string{"app": "alpha"}
	labelsB := map[string]string{"app": "beta"}
	hashA := ComputeSelectorHash(labelsA)
	hashB := ComputeSelectorHash(labelsB)

	timeA := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	timeB := metav1.NewTime(time.Date(2025, 2, 1, 10, 0, 0, 0, time.UTC))

	// Original spec had [A, B]; status has their LastSyncTimes.
	// Now reorder spec to [B, A].
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "reorder-mapping",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsB},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: labelsA},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
		Status: v1alpha1.PodASGMappingStatus{
			MappingCount: 2,
			MappingStatuses: []v1alpha1.MappingStatus{
				{SelectorHash: hashA, ASGSyncState: SyncStateSynced, LastSyncTime: timeA, MatchedPods: 1},
				{SelectorHash: hashB, ASGSyncState: SyncStateSynced, LastSyncTime: timeB, MatchedPods: 1},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	// Both fail → should preserve times by selector hash
	results := []azure.ActionResult{
		makeFailedResult("sub1", "rg1", "asg2", prefix, fmt.Errorf("timeout")),
		makeFailedResult("sub1", "rg1", "asg1", prefix, fmt.Errorf("timeout")),
	}

	key := types.NamespacedName{Name: "reorder-mapping", Namespace: "default"}
	err := updater.UpdateAfterReconcile(
		context.Background(), key, 2, prefix,
		results,
		fmt.Errorf("action failures"),
		nil,
		[]int{1, 1},
	)
	if err != nil {
		t.Fatalf("UpdateAfterReconcile returned error: %v", err)
	}

	var fetched v1alpha1.PodASGMapping
	if fetchErr := fc.Get(context.Background(), key, &fetched); fetchErr != nil {
		t.Fatalf("fetch error: %v", fetchErr)
	}

	if len(fetched.Status.MappingStatuses) != 2 {
		t.Fatalf("MappingStatuses len = %d, want 2", len(fetched.Status.MappingStatuses))
	}

	// Row 0 is now B → must have timeB
	if !fetched.Status.MappingStatuses[0].LastSyncTime.Equal(&timeB) {
		t.Errorf("Row 0 (B) LastSyncTime = %v, want %v (by selector hash)",
			fetched.Status.MappingStatuses[0].LastSyncTime, timeB)
	}

	// Row 1 is now A → must have timeA
	if !fetched.Status.MappingStatuses[1].LastSyncTime.Equal(&timeA) {
		t.Errorf("Row 1 (A) LastSyncTime = %v, want %v (by selector hash)",
			fetched.Status.MappingStatuses[1].LastSyncTime, timeA)
	}
}

// ---------------------------------------------------------------------------
// TestStatusSemanticEqual
// ---------------------------------------------------------------------------

func TestStatusSemanticEqual(t *testing.T) {
	now := metav1.Now()
	earlier := metav1.NewTime(now.Add(-time.Hour))

	base := v1alpha1.PodASGMappingStatus{
		MappingCount: 1,
		Conditions: []metav1.Condition{
			{
				Type:               ConditionAccepted,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonSpecValid,
				ObservedGeneration: 1,
				LastTransitionTime: earlier,
			},
		},
		MappingStatuses: []v1alpha1.MappingStatus{
			{SelectorHash: "abc", MatchedPods: 3, ASGSyncState: SyncStateSynced, LastSyncTime: earlier},
		},
	}

	// Same content, different LastTransitionTime → equal.
	sameDiffTime := base.DeepCopy()
	sameDiffTime.Conditions[0].LastTransitionTime = now
	if !statusSemanticEqual(base, *sameDiffTime) {
		t.Error("expected equal when only LastTransitionTime differs")
	}

	// Different MappingCount → not equal.
	diffCount := base.DeepCopy()
	diffCount.MappingCount = 2
	if statusSemanticEqual(base, *diffCount) {
		t.Error("expected not equal when MappingCount differs")
	}

	// Different condition status → not equal.
	diffCondStatus := base.DeepCopy()
	diffCondStatus.Conditions[0].Status = metav1.ConditionFalse
	if statusSemanticEqual(base, *diffCondStatus) {
		t.Error("expected not equal when condition Status differs")
	}

	// Different condition reason → not equal.
	diffCondReason := base.DeepCopy()
	diffCondReason.Conditions[0].Reason = "OtherReason"
	if statusSemanticEqual(base, *diffCondReason) {
		t.Error("expected not equal when condition Reason differs")
	}

	// Different ObservedGeneration → not equal.
	diffGen := base.DeepCopy()
	diffGen.Conditions[0].ObservedGeneration = 2
	if statusSemanticEqual(base, *diffGen) {
		t.Error("expected not equal when ObservedGeneration differs")
	}

	// Different MatchedPods → not equal.
	diffPods := base.DeepCopy()
	diffPods.MappingStatuses[0].MatchedPods = 5
	if statusSemanticEqual(base, *diffPods) {
		t.Error("expected not equal when MatchedPods differs")
	}

	// Different ASGSyncState → not equal.
	diffState := base.DeepCopy()
	diffState.MappingStatuses[0].ASGSyncState = SyncStateError
	if statusSemanticEqual(base, *diffState) {
		t.Error("expected not equal when ASGSyncState differs")
	}

	// Different LastSyncTime → not equal.
	diffSync := base.DeepCopy()
	diffSync.MappingStatuses[0].LastSyncTime = now
	if statusSemanticEqual(base, *diffSync) {
		t.Error("expected not equal when LastSyncTime differs")
	}

	// Different number of conditions → not equal.
	diffCondLen := base.DeepCopy()
	diffCondLen.Conditions = append(diffCondLen.Conditions, metav1.Condition{Type: "Extra"})
	if statusSemanticEqual(base, *diffCondLen) {
		t.Error("expected not equal when condition count differs")
	}

	// Both empty → equal.
	if !statusSemanticEqual(v1alpha1.PodASGMappingStatus{}, v1alpha1.PodASGMappingStatus{}) {
		t.Error("expected equal for two empty statuses")
	}
}

// ---------------------------------------------------------------------------
// TestUpdatePending_SkipsWriteWhenStatusUnchanged
// ---------------------------------------------------------------------------

func TestUpdatePending_SkipsWriteWhenStatusUnchanged(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "noop-pending",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	key := types.NamespacedName{Name: "noop-pending", Namespace: "default"}

	// First call: writes pending status.
	if err := updater.UpdatePending(context.Background(), key, 1, "prefix", []int{2}); err != nil {
		t.Fatalf("first UpdatePending returned error: %v", err)
	}

	// Track writes via a conflict-injecting client with 0 remaining conflicts.
	updateCalls := 0
	conflictsRemaining := 0
	cc := &conflictInjectingClient{
		Client: fc,
		statusWriter: &conflictInjectingStatusWriter{
			SubResourceWriter:  fc.Status(),
			conflictsRemaining: &conflictsRemaining,
			updateCalls:        &updateCalls,
		},
	}
	updater.Client = cc

	// Second call with same inputs: should skip the write.
	if err := updater.UpdatePending(context.Background(), key, 1, "prefix", []int{2}); err != nil {
		t.Fatalf("second UpdatePending returned error: %v", err)
	}

	if updateCalls != 0 {
		t.Errorf("status update calls = %d, want 0 (should have been skipped)", updateCalls)
	}
}

// ---------------------------------------------------------------------------
// TestUpdateAfterReconcile_SkipsWriteWhenValidationStatusUnchanged
// ---------------------------------------------------------------------------

func TestUpdateAfterReconcile_SkipsWriteWhenValidationStatusUnchanged(t *testing.T) {
	scheme := statusTestScheme(t)
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "noop-validation",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "bad"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "not-a-valid-id"}},
				},
			},
		},
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mapping).WithObjects(mapping).Build()
	updater := &MappingStatusUpdater{
		Client:      fc,
		Logger:      statusTestLogger(t),
		Now:         metav1.Now,
		MaxAttempts: 3,
	}

	validationIssues := []ValidationIssue{
		{MappingIndex: 0, ASGIndex: 0, ResourceID: "not-a-valid-id", Err: fmt.Errorf("invalid resource ID")},
	}

	key := types.NamespacedName{Name: "noop-validation", Namespace: "default"}

	// First call: writes validation failure status.
	if err := updater.UpdateAfterReconcile(
		context.Background(), key, 1, "prefix",
		nil, fmt.Errorf("validation failed"), validationIssues, []int{0},
	); err != nil {
		t.Fatalf("first UpdateAfterReconcile returned error: %v", err)
	}

	// Track writes.
	updateCalls := 0
	conflictsRemaining := 0
	cc := &conflictInjectingClient{
		Client: fc,
		statusWriter: &conflictInjectingStatusWriter{
			SubResourceWriter:  fc.Status(),
			conflictsRemaining: &conflictsRemaining,
			updateCalls:        &updateCalls,
		},
	}
	updater.Client = cc

	// Second call with same validation failure: should skip the write.
	if err := updater.UpdateAfterReconcile(
		context.Background(), key, 1, "prefix",
		nil, fmt.Errorf("validation failed"), validationIssues, []int{0},
	); err != nil {
		t.Fatalf("second UpdateAfterReconcile returned error: %v", err)
	}

	if updateCalls != 0 {
		t.Errorf("status update calls = %d, want 0 (should have been skipped)", updateCalls)
	}
}
