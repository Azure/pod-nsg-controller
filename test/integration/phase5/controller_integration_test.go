package phase5_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.uber.org/zap/zaptest"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func asgResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func makePod(ns, name string, labels map[string]string, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

func makeMapping(ns, name string, rules []v1alpha1.Mapping) *v1alpha1.PodASGMapping {
	return &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.PodASGMappingSpec{Mappings: rules},
	}
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func buildReconciler(t *testing.T, k8sClient client.Client, factory *fake.ClientFactory) *controller.MappingReconciler {
	t.Helper()
	log := zaptest.NewLogger(t)
	executor := azure.NewExecutor(log, factory, 4)
	return &controller.MappingReconciler{
		Client:         k8sClient,
		Scheme:         newScheme(),
		ClusterName:    "test-cluster",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executor,
	}
}

// ---------------------------------------------------------------------------
// Test: Full Reconcile Loop — Pod Created → Desired → Diff → Execute → Azure
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_FullReconcileLoop_CreatesAzurePrefixSet(t *testing.T) {
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

	pod1 := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")
	pod2 := makePod("default", "web-2", map[string]string{"app": "web"}, "10.0.0.2")
	nonMatchPod := makePod("default", "db-1", map[string]string{"app": "db"}, "10.0.1.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod1, pod2, nonMatchPod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// First reconcile: adds finalizer
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	// The reconciler adds a finalizer and then proceeds with reconcileActive
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}

	// Verify Azure state: prefix set was created with matching IPs
	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("expected prefix set to exist, got error: %v", err)
	}
	if got == nil || got.Properties == nil {
		t.Fatal("prefix set has no properties")
	}

	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.1", "10.0.0.2"}
	if !strSliceEqual(gotIPs, wantIPs) {
		t.Errorf("IPs mismatch: got %v, want %v", gotIPs, wantIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Reconcile with drift correction (resync corrects Azure state)
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_ResyncCorrectsDrift(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Pre-populate Azure with drifted state (extra IP)
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1", "10.0.0.99"})

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})

	pod1 := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod1).
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

	// Verify Azure state corrected: only desired IP remains
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("expected prefix set to exist: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.1"}
	if !strSliceEqual(gotIPs, wantIPs) {
		t.Errorf("drift not corrected: got %v, want %v", gotIPs, wantIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Pod deleted → reconcile removes IP from prefix set
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodDeleted_RemovesIPFromPrefixSet(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Azure currently has two IPs
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1", "10.0.0.2"})

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})

	// Only one pod remains (web-2 was deleted)
	pod1 := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")

	// Pre-set the owned targets annotation
	ownedTargets := []controller.OwnedTarget{{
		ResourceID:    asgResourceID(sub, rg, asgName),
		PrefixSetName: prefixSetName,
	}}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod1).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify only the remaining pod's IP is in the prefix set
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set get error: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.1"}
	if !strSliceEqual(gotIPs, wantIPs) {
		t.Errorf("expected %v, got %v", wantIPs, gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Mapping deletion → finalizer cleanup deletes all owned prefix sets
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MappingDeletion_CleansUpAllOwnedPrefixSets(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName1 := "asg-1"
	asgName2 := "asg-2"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Pre-populate Azure with prefix sets in both ASGs
	_ = fakeAzClient.Put(ctx, sub, rg, asgName1, prefixSetName, []string{"10.0.0.1"})
	_ = fakeAzClient.Put(ctx, sub, rg, asgName2, prefixSetName, []string{"10.0.0.2"})

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

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for deletion path, got %v", result.RequeueAfter)
	}

	// Verify both prefix sets were deleted from Azure
	_, err = fakeAzClient.Get(ctx, sub, rg, asgName1, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("asg-1 prefix set should be deleted, got err=%v", err)
	}
	_, err = fakeAzClient.Get(ctx, sub, rg, asgName2, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("asg-2 prefix set should be deleted, got err=%v", err)
	}

	// Verify finalizer was removed. With fake client + DeletionTimestamp set,
	// removing the last finalizer causes the object to be garbage collected.
	var updated v1alpha1.PodASGMapping
	getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated)
	if getErr != nil {
		// Object was garbage-collected after finalizer removal — expected behavior
		return
	}
	for _, f := range updated.Finalizers {
		if f == controller.MappingCleanupFinalizer {
			t.Error("finalizer should have been removed after cleanup")
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Pod mapper — pod event enqueues matching mappings
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodMapper_EnqueuesMatchingMappings(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"

	mapping1 := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-web")},
			},
		},
	})
	mapping2 := makeMapping("default", "db-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "db"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-db")},
			},
		},
	})
	mapping3 := makeMapping("other-ns", "other-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-other")},
			},
		},
	})

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping1, mapping2, mapping3).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	// Pod with "app=web" in "default" namespace should only match web-map
	webPod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")
	requests := reconciler.MapPodToMappings(ctx, webPod)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d: %v", len(requests), requests)
	}
	if requests[0].NamespacedName.Name != "web-map" {
		t.Errorf("expected web-map enqueued, got %s", requests[0].NamespacedName.Name)
	}

	// Pod with "app=db" should match db-map
	dbPod := makePod("default", "db-1", map[string]string{"app": "db"}, "10.0.1.1")
	requests = reconciler.MapPodToMappings(ctx, dbPod)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request for db pod, got %d", len(requests))
	}
	if requests[0].NamespacedName.Name != "db-map" {
		t.Errorf("expected db-map enqueued, got %s", requests[0].NamespacedName.Name)
	}

	// Pod with no matching labels should enqueue nothing
	noMatchPod := makePod("default", "no-match", map[string]string{"app": "cache"}, "10.0.2.1")
	requests = reconciler.MapPodToMappings(ctx, noMatchPod)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for non-matching pod, got %d", len(requests))
	}
}

// ---------------------------------------------------------------------------
// Test: Pod mapper — unlabeled pod enqueues all namespace mappings
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodMapper_UnlabeledPodEnqueuesAllInNamespace(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"

	mapping1 := makeMapping("default", "map-a", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "a"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-a")},
			},
		},
	})
	mapping2 := makeMapping("default", "map-b", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "b"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-b")},
			},
		},
	})

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping1, mapping2).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	// Pod with nil labels (tombstone/delete scenario)
	unlabeledPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "deleted-pod"},
	}
	requests := reconciler.MapPodToMappings(ctx, unlabeledPod)

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests for unlabeled pod, got %d: %v", len(requests), requests)
	}

	names := make([]string, len(requests))
	for i, r := range requests {
		names[i] = r.NamespacedName.Name
	}
	sort.Strings(names)
	if names[0] != "map-a" || names[1] != "map-b" {
		t.Errorf("expected [map-a, map-b], got %v", names)
	}
}

// ---------------------------------------------------------------------------
// Test: StatusPlaceholderUpdater error propagation across boundaries
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_StatusUpdaterError_PropagatesFromReconcile(t *testing.T) {
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

	statusErr := fmt.Errorf("simulated status update failure")
	reconciler := &controller.MappingReconciler{
		Client:         k8sClient,
		Scheme:         s,
		ClusterName:    "test-cluster",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executor,
		StatusUpdater: func(_ context.Context, _ *v1alpha1.PodASGMapping, _ []azure.ActionResult) error {
			return statusErr
		},
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected error from status updater, got nil")
	}
	if !containsSubstring(err.Error(), "status placeholder") {
		t.Errorf("error should mention status placeholder, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: Multi-ASG mapping creates prefix sets in all referenced ASGs
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MultiASGMapping_CreatesInAllASGs(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asg1 := "asg-frontend"
	asg2 := "asg-backend"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	mapping := makeMapping("default", "multi-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asg1)},
				{ResourceID: asgResourceID(sub, rg, asg2)},
			},
		},
	})

	pod := makePod("default", "web-1", map[string]string{"tier": "web"}, "10.0.0.5")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "multi-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	prefixSetName := model.OwnershipKey("test-cluster", "default", "multi-map")

	// Verify prefix set exists in both ASGs
	got1, err := fakeAzClient.Get(ctx, sub, rg, asg1, prefixSetName)
	if err != nil {
		t.Fatalf("asg1 prefix set missing: %v", err)
	}
	if len(got1.Properties.AddressPrefixes) != 1 || got1.Properties.AddressPrefixes[0] != "10.0.0.5" {
		t.Errorf("asg1 IPs wrong: got %v", got1.Properties.AddressPrefixes)
	}

	got2, err := fakeAzClient.Get(ctx, sub, rg, asg2, prefixSetName)
	if err != nil {
		t.Fatalf("asg2 prefix set missing: %v", err)
	}
	if len(got2.Properties.AddressPrefixes) != 1 || got2.Properties.AddressPrefixes[0] != "10.0.0.5" {
		t.Errorf("asg2 IPs wrong: got %v", got2.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: Cross-subscription mapping uses correct factory clients
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_CrossSubscription_UsesCorrectClients(t *testing.T) {
	ctx := context.Background()
	sub1 := "sub-001"
	sub2 := "sub-002"
	rg := "rg-net"
	asg1 := "asg-in-sub1"
	asg2 := "asg-in-sub2"

	client1 := fake.NewClient()
	client2 := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub1, client1)
	factory.RegisterClient(sub2, client2)

	mapping := makeMapping("default", "cross-sub", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "svc"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub1, rg, asg1)},
				{ResourceID: asgResourceID(sub2, rg, asg2)},
			},
		},
	})

	pod := makePod("default", "svc-1", map[string]string{"app": "svc"}, "10.1.0.1")

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping, pod).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cross-sub", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	prefixSetName := model.OwnershipKey("test-cluster", "default", "cross-sub")

	// Verify prefix set in sub1's client
	got1, err := client1.Get(ctx, sub1, rg, asg1, prefixSetName)
	if err != nil {
		t.Fatalf("sub1 prefix set missing: %v", err)
	}
	if got1.Properties.AddressPrefixes[0] != "10.1.0.1" {
		t.Errorf("sub1 wrong IP: %v", got1.Properties.AddressPrefixes)
	}

	// Verify prefix set in sub2's client
	got2, err := client2.Get(ctx, sub2, rg, asg2, prefixSetName)
	if err != nil {
		t.Fatalf("sub2 prefix set missing: %v", err)
	}
	if got2.Properties.AddressPrefixes[0] != "10.1.0.1" {
		t.Errorf("sub2 wrong IP: %v", got2.Properties.AddressPrefixes)
	}
}

// ---------------------------------------------------------------------------
// Test: Azure execution error stops reconcile (error propagation)
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_AzureError_PropagatesFromReconcile(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Inject error for PUT
	_ = fakeAzClient.InjectError(fake.InjectKey{
		Operation:     fake.OperationPut,
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}, fmt.Errorf("transient ARM failure"), 5) // more than maxRetries

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

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected error from failed Azure execution, got nil")
	}
}

// ---------------------------------------------------------------------------
// Test: Reconcile on non-existent mapping returns without error
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MappingNotFound_ReturnsCleanly(t *testing.T) {
	ctx := context.Background()

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error for missing mapping, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for missing mapping, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test: Full cycle — pod IP change reflected in Azure after reconcile
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_PodIPChange_UpdatesAzurePrefixSet(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Azure has old IP
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1"})

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, asgName)},
			},
		},
	})
	// Existing annotation
	ownedTargets := []controller.OwnedTarget{{
		ResourceID:    asgResourceID(sub, rg, asgName),
		PrefixSetName: prefixSetName,
	}}
	annotationJSON, _ := json.Marshal(ownedTargets)
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(annotationJSON),
	}

	// Pod has new IP (simulating IP change after reschedule)
	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.99")

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

	// Verify new IP is in Azure
	got, err := fakeAzClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("prefix set get error: %v", err)
	}
	gotIPs := sortedStrings(got.Properties.AddressPrefixes)
	wantIPs := []string{"10.0.0.99"}
	if !strSliceEqual(gotIPs, wantIPs) {
		t.Errorf("expected %v after IP change, got %v", wantIPs, gotIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: Engine integration — desired state feeds correct data to diff
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_DesiredState_To_Diff_Pipeline(t *testing.T) {
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"
	clusterName := "test-cluster"

	mapping := v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-map"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{
						{ResourceID: asgResourceID(sub, rg, asgName)},
					},
				},
			},
		},
	}
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-1", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-2", Labels: map[string]string{"app": "web"}},
			Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}

	desired := engine.ComputeDesiredState(clusterName, []v1alpha1.PodASGMapping{mapping}, pods)

	// Simulate actual state has only one IP (partial sync)
	prefixSetName := model.OwnershipKey(clusterName, "default", "web-map")
	target := engine.ASGTarget{
		SubscriptionID: sub,
		ResourceGroup:  rg,
		ASGName:        asgName,
		FullResourceID: asgResourceID(sub, rg, asgName),
		PrefixSetName:  prefixSetName,
	}
	actual := map[engine.ASGTarget]engine.ActualPrefixSet{
		target: {IPs: map[string]struct{}{"10.0.0.1": {}}},
	}

	actions := engine.ComputeDiff(desired, actual)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action (update), got %d", len(actions))
	}
	if actions[0].Kind != engine.UpdatePrefixSet {
		t.Errorf("expected UpdatePrefixSet, got %s", actions[0].Kind)
	}
	wantIPs := []string{"10.0.0.1", "10.0.0.2"}
	if !strSliceEqual(actions[0].DesiredIPs, wantIPs) {
		t.Errorf("expected IPs %v, got %v", wantIPs, actions[0].DesiredIPs)
	}
}

// ---------------------------------------------------------------------------
// Test: RequeueAfter honors configured ResyncInterval
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_ResyncInterval_HonoredInResult(t *testing.T) {
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

	customInterval := 120 * time.Second
	reconciler := &controller.MappingReconciler{
		Client:         k8sClient,
		Scheme:         s,
		ClusterName:    "test-cluster",
		ResyncInterval: customInterval,
		Factory:        factory,
		Executor:       executor,
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != customInterval {
		t.Errorf("expected RequeueAfter=%v, got %v", customInterval, result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Test: No-op reconcile (no diff) still updates owned-targets annotation
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_NoOpReconcile_UpdatesOwnedTargetsAnnotation(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"
	asgName := "my-asg"

	fakeAzClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeAzClient)

	prefixSetName := model.OwnershipKey("test-cluster", "default", "web-map")

	// Azure already has the correct state
	_ = fakeAzClient.Put(ctx, sub, rg, asgName, prefixSetName, []string{"10.0.0.1"})

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

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify annotation was set
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "web-map", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated mapping: %v", err)
	}
	raw := updated.Annotations[controller.OwnedTargetsAnnotation]
	if raw == "" {
		t.Fatal("expected owned-targets annotation to be set")
	}
	var targets []controller.OwnedTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].PrefixSetName != prefixSetName {
		t.Errorf("expected prefixSetName %q, got %q", prefixSetName, targets[0].PrefixSetName)
	}
}

// ---------------------------------------------------------------------------
// Test: Predicates integration — pod mapper + predicate pipeline correctness
// (verifies MapPodToMappings is compatible with EnqueueRequestsFromMapFunc contract)
// ---------------------------------------------------------------------------

func TestIntegration_Phase5_MapPodToMappings_ReturnsReconcileRequests(t *testing.T) {
	ctx := context.Background()
	sub := "sub-001"
	rg := "rg-net"

	mapping := makeMapping("default", "web-map", []v1alpha1.Mapping{
		{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: asgResourceID(sub, rg, "asg-web")},
			},
		},
	})

	s := newScheme()
	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(s).
		WithObjects(mapping).
		WithStatusSubresource(&v1alpha1.PodASGMapping{}).
		Build()

	factory := fake.NewClientFactory()
	reconciler := buildReconciler(t, k8sClient, factory)

	pod := makePod("default", "web-1", map[string]string{"app": "web"}, "10.0.0.1")
	requests := reconciler.MapPodToMappings(ctx, pod)

	// Verify output matches reconcile.Request contract
	for _, r := range requests {
		if r.NamespacedName == (types.NamespacedName{}) {
			t.Error("empty NamespacedName in request")
		}
	}
	if len(requests) != 1 {
		t.Errorf("expected 1 reconcile request, got %d", len(requests))
	}

	// Verify it's a valid reconcile.Request the controller framework would accept
	var _ []reconcile.Request = requests
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func sortedStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func mustMarshalOwnedTargets(targets []controller.OwnedTarget) string {
	data, err := json.Marshal(targets)
	if err != nil {
		panic(err)
	}
	return string(data)
}
