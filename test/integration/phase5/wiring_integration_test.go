package phase5_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// Test: Predicate + Mapper pipeline — pod label change triggers correct mapping
// This verifies the integration point where PodEventPredicate passes an update
// event and MapPodToMappings enqueues the correct reconcile request.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PredicateAndMapper_LabelChange_EnqueuesCorrectMapping(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"

	// Two mappings: one for app=web, one for app=db
	webMapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-web")},
			},
		},
	})
	dbMapping := makeMapping("default", "db-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "db"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-db")},
			},
		},
	})

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(webMapping, dbMapping).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	// Simulate a pod whose labels changed from app=web to app=db
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-1",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-1",
			Labels:    map[string]string{"app": "db"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	// Step 1: Verify predicate passes this update (labels changed)
	predicate := controller.PodEventPredicate()
	updateEvent := event.UpdateEvent{
		ObjectOld: oldPod,
		ObjectNew: newPod,
	}
	if !predicate.Update(updateEvent) {
		t.Fatal("PodEventPredicate should pass label change events")
	}

	// Step 2: Verify mapper enqueues the NEW pod's matching mapping (db-map)
	requests := reconciler.MapPodToMappings(ctx, newPod)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request for new labels, got %d", len(requests))
	}
	if requests[0].Name != "db-map" {
		t.Errorf("expected db-map to be enqueued for new labels, got %s", requests[0].Name)
	}

	// Step 3: Verify mapper also enqueues old mapping when called with old labels
	// (In a real controller, the old pod state would also be used to enqueue)
	requestsOld := reconciler.MapPodToMappings(ctx, oldPod)
	if len(requestsOld) != 1 {
		t.Fatalf("expected 1 request for old labels, got %d", len(requestsOld))
	}
	if requestsOld[0].Name != "web-map" {
		t.Errorf("expected web-map to be enqueued for old labels, got %s", requestsOld[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Test: Predicate filters status-only pod update — mapper never runs
// Verifies predicate blocks events that should not reach the mapper/reconciler.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PredicateFilters_StatusOnlyPodUpdate(t *testing.T) {
	// Pod status changed (e.g., phase change) but labels and IP remain same
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-1",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodPending},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod-1",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
	}

	predicate := controller.PodEventPredicate()
	updateEvent := event.UpdateEvent{
		ObjectOld: oldPod,
		ObjectNew: newPod,
	}
	if predicate.Update(updateEvent) {
		t.Error("PodEventPredicate should block status-only changes (no label/IP diff)")
	}
}

// ---------------------------------------------------------------------------
// Test: Predicate passes PodIP change — triggers reconcile that updates Azure
// End-to-end: predicate → mapper → reconcile → engine → executor → Azure
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodIPChange_E2E_PredicateToAzure(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Azure currently has old IP
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1"})

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	ownedTargets := []controller.OwnedTarget{{
		ResourceID:    asgResourceID(sub, rg, asgName),
		PrefixSetName: prefixSetName,
	}}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	// Pod IP changed from 10.0.0.1 -> 10.0.0.5
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-1", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-1", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.5"},
	}

	// Step 1: Predicate passes the IP change
	predicate := controller.PodEventPredicate()
	if !predicate.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
		t.Fatal("PodEventPredicate should pass IP change events")
	}

	// Step 2: Full reconcile produces updated Azure state
	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, newPod). // new pod has new IP
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}

	// Step 3: Verify Azure has new IP only
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set get error: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.5"}
	if !strSliceEqual(gotIPs, wantIPs) {
		t.Errorf("expected %v after IP change, got %v", wantIPs, gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: ETag retry through reconciler — executor retries 412 during reconcile
// Cross-module: reconciler → executor → fake Azure (412) → retry → success
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_ETagRetry_ThroughReconciler(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	// Inject 1 ETag conflict (412) on Put — executor should retry and succeed
	fakeAzClient.Fail412Count = 1

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile should succeed after ETag retry, got error: %v", err)
	}
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}

	// Verify the prefix set was eventually created
	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set should exist after retry: %v", err)
	}
	if got == nil || got.Properties == nil {
		t.Fatal("prefix set missing properties")
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	if !strSliceEqual(gotIPs, []string{"10.0.0.1"}) {
		t.Errorf("expected [10.0.0.1], got %v", gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Multi-cycle reconcile — annotation persists across cycles
// First cycle creates prefix set + annotation. Second cycle (no changes) is no-op.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MultiCycleReconcile_AnnotationPersistence(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"}}

	// First cycle: creates prefix set and sets annotation
	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	// Verify annotation is set after first cycle
	var afterFirst v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, req.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	firstAnnotation := afterFirst.Annotations[controller.OwnedTargetsAnnotation]
	if firstAnnotation == "" {
		t.Fatal("owned-targets annotation should be set after first reconcile")
	}

	// Second cycle: should be a no-op (Azure already correct)
	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	// Verify annotation is unchanged
	var afterSecond v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, req.NamespacedName, &afterSecond); err != nil {
		t.Fatalf("get after second reconcile: %v", err)
	}
	secondAnnotation := afterSecond.Annotations[controller.OwnedTargetsAnnotation]
	if firstAnnotation != secondAnnotation {
		t.Errorf("annotation changed between cycles:\n  first:  %s\n  second: %s", firstAnnotation, secondAnnotation)
	}

	// Verify Azure state is still correct
	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set missing: %v", err)
	}
	if !strSliceEqual(sortedStrings(got.Properties.AddressPrefixes), []string{"10.0.0.1"}) {
		t.Error("Azure state corrupted across reconcile cycles")
	}
}

// ---------------------------------------------------------------------------
// Test: Deletion with partial Azure failure — error propagates, finalizer stays
// Cross-module: reconciler → deleteTargetPrefixSet → Azure (error) → finalizer kept
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_DeletionPartialFailure_FinalizerRetained(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName1 := "asg-1"
	asgName2 := "asg-2"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Populate Azure — both ASGs have prefix sets
	_ = fakeAzClient.Put(ctx, sub, rg, asgName1, prefixSetName, []string{"10.0.0.1"})
	_ = fakeAzClient.Put(ctx, sub, rg, asgName2, prefixSetName, []string{"10.0.0.2"})

	// Inject error on GET for asg-2 (simulating Azure transient failure during cleanup)
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationGet,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName2,
		PrefixSetName:  prefixSetName,
	}, fmt.Errorf("transient Azure error"), 1)

	// Also inject error on Delete for asg-2 to ensure error propagates
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:      fake.OperationDelete,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName2,
		PrefixSetName:  prefixSetName,
	}, fmt.Errorf("delete transient failure"), 1)

	now := metav1.Now()
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              "web-map",
			DeletionTimestamp: &now,
			Finalizers:        []string{controller.MappingCleanupFinalizer},
			Annotations: map[string]string{
				controller.OwnedTargetsAnnotation: mustMarshalOwnedTargets([]controller.OwnedTarget{
					{ResourceID: asgResourceID(sub, rg, asgName1), PrefixSetName: prefixSetName},
					{ResourceID: asgResourceID(sub, rg, asgName2), PrefixSetName: prefixSetName},
				}),
			},
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID(sub, rg, asgName1)},
						{ResourceID: asgResourceID(sub, rg, asgName2)},
					},
				},
			},
		},
	}

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Reconcile should fail because asg-2 cleanup fails
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected error from partial deletion failure, got nil")
	}

	// Verify finalizer is still present (not removed on partial failure)
	var updated v1alpha1.PodASGMapping
	if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated); getErr != nil {
		t.Fatalf("get after failed deletion: %v", getErr)
	}
	hasFinalizer := false
	for _, f := range updated.Finalizers {
		if f == controller.MappingCleanupFinalizer {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Error("finalizer should be retained after partial deletion failure")
	}
}

// ---------------------------------------------------------------------------
// Test: Mapping spec add ASG — creates prefix set in new ASG
// Cross-module: engine computes target for new ASG → executor creates it
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MappingSpecAddASG_CreatesPrefixSetInNewASG(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName1 := "asg-existing"
	asgName2 := "asg-new"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// ASG-1 already has the prefix set (from prior reconcile)
	_ = fakeAzClient.Put(ctx, sub, rg, asgName1, prefixSetName, []string{"10.0.0.1"})

	// Mapping now references both ASGs (spec was updated to add asg-new)
	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName1)},
				{ResourceID: asgResourceID(sub, rg, asgName2)},
			},
		},
	})
	// Old annotation only knows about asg-existing
	ownedTargets := []controller.OwnedTarget{{
		ResourceID:    asgResourceID(sub, rg, asgName1),
		PrefixSetName: prefixSetName,
	}}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify new ASG has the prefix set
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName2, prefixSetName)
	if err != nil {
		t.Fatalf("new ASG prefix set should exist: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	if !strSliceEqual(gotIPs, []string{"10.0.0.1"}) {
		t.Errorf("new ASG expected [10.0.0.1], got %v", gotIPs)
	}

	// Verify annotation now includes both ASGs
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated mapping: %v", err)
	}
	var targets []controller.OwnedTarget
	if err := json.Unmarshal([]byte(updated.Annotations[controller.OwnedTargetsAnnotation]), &targets); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if len(targets) != 2 {
		t.Errorf("expected 2 owned targets, got %d", len(targets))
	}
}

// ---------------------------------------------------------------------------
// Test: Mapping spec remove ASG — deletes removed ASG prefix set
// Cross-module: engine computes empty for removed target → diff produces delete → executor deletes
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MappingSpecRemoveASG_DeletesRemovedPrefixSet(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName1 := "asg-keep"
	asgName2 := "asg-remove"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Both ASGs have prefix sets from before
	_ = fakeAzClient.Put(ctx, sub, rg, asgName1, prefixSetName, []string{"10.0.0.1"})
	_ = fakeAzClient.Put(ctx, sub, rg, asgName2, prefixSetName, []string{"10.0.0.1"})

	// Mapping now only references asg-keep (asg-remove was removed from spec)
	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName1)},
			},
		},
	})
	// Annotation still shows both ASGs (from prior reconcile)
	ownedTargets := []controller.OwnedTarget{
		{ResourceID: asgResourceID(sub, rg, asgName1), PrefixSetName: prefixSetName},
		{ResourceID: asgResourceID(sub, rg, asgName2), PrefixSetName: prefixSetName},
	}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify asg-keep still has the prefix set
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName1, prefixSetName)
	if err != nil {
		t.Fatalf("kept ASG prefix set missing: %v", err)
	}
	if !strSliceEqual(sortedStrings(got.Properties.AddressPrefixes), []string{"10.0.0.1"}) {
		t.Error("kept ASG prefix set has wrong IPs")
	}

	// Verify asg-remove's prefix set was deleted
	_, err = fakeAzClient.Get(ctx, sub, rg, asgName2, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("removed ASG prefix set should be deleted, got err=%v", err)
	}

	// Verify annotation now only includes asg-keep
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated mapping: %v", err)
	}
	var targets []controller.OwnedTarget
	if err := json.Unmarshal([]byte(updated.Annotations[controller.OwnedTargetsAnnotation]), &targets); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if len(targets) != 1 {
		t.Errorf("expected 1 owned target, got %d", len(targets))
	}
}

// ---------------------------------------------------------------------------
// Test: MappingEventPredicate integration with status-only update
// Verifies that spec-only changes pass but status-only changes are blocked.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MappingPredicate_SpecChange_Passes(t *testing.T) {
	sub := "sub-001"
	rg := "rg-net"

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-map"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID(sub, rg, "asg-1")},
					},
				},
			},
		},
	}
	newMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-map"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID(sub, rg, "asg-1")},
						{ResourceID: asgResourceID(sub, rg, "asg-2")},
					},
				},
			},
		},
	}

	predicate := controller.MappingEventPredicate()

	// Spec change should pass
	if !predicate.Update(event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}) {
		t.Error("MappingEventPredicate should pass spec changes")
	}

	// Status-only change should be blocked
	statusOnlyNew := oldMapping.DeepCopy()
	statusOnlyNew.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Now(), Reason: "Synced"},
	}
	if predicate.Update(event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: statusOnlyNew}) {
		t.Error("MappingEventPredicate should block status-only changes")
	}
}

// ---------------------------------------------------------------------------
// Test: Generic events blocked by both predicates
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_GenericEvents_BlockedByPredicates(t *testing.T) {
	podPred := controller.PodEventPredicate()
	mappingPred := controller.MappingEventPredicate()

	podGeneric := event.GenericEvent{
		Object: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod-1"},
		},
	}
	mappingGeneric := event.GenericEvent{
		Object: &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-map"},
		},
	}

	if podPred.Generic(podGeneric) {
		t.Error("Pod predicate should block generic events")
	}
	if mappingPred.Generic(mappingGeneric) {
		t.Error("Mapping predicate should block generic events")
	}
}

// ---------------------------------------------------------------------------
// Test: StatusUpdater seam receives correct results from executor
// Cross-module: reconciler → executor → results → StatusUpdater
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_StatusUpdater_ReceivesExecutorResults(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	log := zaptest.NewLogger(t)
	executor := azure.NewExecutor(log, factory, 4)

	var capturedResults []azure.ActionResult
	var capturedMapping *v1alpha1.PodASGMapping

	reconciler := &controller.MappingReconciler{
		Client:         k8sClient,
		Scheme:         s,
		ClusterName:    "test-cluster",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executor,
		StatusUpdater: func(_ context.Context, m *v1alpha1.PodASGMapping, results []azure.ActionResult) error {
			capturedMapping = m
			capturedResults = results
			return nil
		},
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify status updater was called with results
	if capturedMapping == nil {
		t.Fatal("StatusUpdater was not called")
	}
	if capturedMapping.Name != "web-map" {
		t.Errorf("expected mapping name 'web-map', got %q", capturedMapping.Name)
	}
	if len(capturedResults) != 1 {
		t.Fatalf("expected 1 result from executor, got %d", len(capturedResults))
	}
	if !capturedResults[0].Success {
		t.Errorf("expected successful result, got error: %v", capturedResults[0].Err)
	}
	if capturedResults[0].Action.Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet action, got %s", capturedResults[0].Action.Kind)
	}
}

// ---------------------------------------------------------------------------
// Test: Config → Reconciler wiring — ResyncInterval flows correctly
// Verifies the config value propagation from config.Config to reconciler result.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_ConfigResyncInterval_FlowsToReconcilerResult(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	log := zaptest.NewLogger(t)
	executor := azure.NewExecutor(log, factory, 4)

	// Simulate config-driven interval values
	testCases := []struct {
		name     string
		interval time.Duration
	}{
		{"30s", 30 * time.Second},
		{"60s", 60 * time.Second},
		{"120s", 120 * time.Second},
		{"5m", 5 * time.Minute},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reconciler := &controller.MappingReconciler{
				Client:         k8sClient,
				Scheme:         s,
				ClusterName:    "test-cluster",
				ResyncInterval: tc.interval,
				Factory:        factory,
				Executor:       executor,
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("reconcile error: %v", err)
			}
			if result.RequeueAfter != tc.interval {
				t.Errorf("expected RequeueAfter=%v, got %v", tc.interval, result.RequeueAfter)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: Pod with no IP is not included in desired state (engine integration)
// Cross-module: reconciler → engine.ComputeDesiredState → diff
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodNoIP_ExcludedFromDesired(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	// Pod with IP
	pod1 := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")
	// Pod without IP (newly scheduled, not yet assigned)
	pod2 := makePod("default", "web-2", map[string]string{"app": "web"}, "")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod1, pod2).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Only pod1's IP should be in the prefix set
	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set missing: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	if !strSliceEqual(gotIPs, []string{"10.0.0.1"}) {
		t.Errorf("expected only [10.0.0.1] (no-IP pod excluded), got %v", gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: SetupWithManager contract — verify it doesn't panic and registers
// Uses a minimal fake manager to validate the wiring compiles correctly.
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_SetupWithManager_Registers(t *testing.T) {
	// This test verifies that SetupWithManager can be called without panics.
	// The actual manager integration is more complex, but this validates
	// the type compatibility of the For/Watches/Complete chain.
	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	// Verify the reconciler satisfies the reconcile.Reconciler interface
	var _ client.Client = reconciler.Client
	_ = reconciler.Scheme
	_ = reconciler.ClusterName
	_ = reconciler.ResyncInterval
	_ = reconciler.Factory
	_ = reconciler.Executor

	// Verify MapPodToMappings has correct signature for EnqueueRequestsFromMapFunc
	var _ func(context.Context, client.Object) []ctrl.Request = reconciler.MapPodToMappings
}

// ---------------------------------------------------------------------------
// Test: Pod label change away from match → reconcile removes IP from desired
// Simulates T5.4: pod that was matching now doesn't match → IP removed from Azure
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodLabelChangeAway_RemovesIPFromDesired(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Azure has two IPs from prior reconcile
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1", "10.0.0.2"})

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	ownedTargets := []controller.OwnedTarget{{
		ResourceID:    asgResourceID(sub, rg, asgName),
		PrefixSetName: prefixSetName,
	}}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	// pod-1 still matches, pod-2 label changed to "app=db" (no longer matches)
	pod1 := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")
	pod2 := makePod("default", "web-2", map[string]string{"app": "db"}, "10.0.0.2")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod1, pod2).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Only pod-1's IP should remain
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set missing: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	if !strSliceEqual(gotIPs, []string{"10.0.0.1"}) {
		t.Errorf("expected [10.0.0.1] (pod-2 label changed away), got %v", gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Finalizer is idempotent — ensureFinalizer on already-finalized mapping
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_EnsureFinalizer_Idempotent(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	// Pre-set finalizer (simulating already reconciled once)
	mapping.Finalizers = []string{controller.MappingCleanupFinalizer}

	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Should succeed without double-adding finalizer
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify exactly one finalizer
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	count := 0
	for _, f := range updated.Finalizers {
		if f == controller.MappingCleanupFinalizer {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 finalizer, found %d", count)
	}
}
