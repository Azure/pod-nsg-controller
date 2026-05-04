package phase5_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.uber.org/zap/zaptest"
)

// TestIntegration_Phase5_MultiMappingSharedPod_IndependentPrefixSets verifies that
// two PodASGMappings in the same namespace, both matching the same pod, produce
// distinct prefix sets in their respective ASGs without interfering with each other.
func TestIntegration_Phase5_MultiMappingSharedPod_IndependentPrefixSets(t *testing.T) {
	ctx := context.Background()
	sub := "sub-shared"
	rg := "rg-net"
	asgA := "asg-frontend"
	asgB := "asg-backend"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	pod := makePod("apps", "shared-pod", map[string]string{"role": "api", "tier": "web"}, "10.1.0.1")

	mappingA := makeMapping("apps", "frontend-map", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"role": "api"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgA)},
		},
	}})
	mappingB := makeMapping("apps", "backend-map", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"tier": "web"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgB)},
		},
	}})

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(pod, mappingA, mappingB).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Reconcile mapping A
	resultA, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "frontend-map", Namespace: "apps"},
	})
	if err != nil {
		t.Fatalf("reconcile mapping A: %v", err)
	}
	if resultA.RequeueAfter != 60*time.Second {
		t.Errorf("mapping A requeue: got %v, want 60s", resultA.RequeueAfter)
	}

	// Reconcile mapping B
	resultB, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "backend-map", Namespace: "apps"},
	})
	if err != nil {
		t.Fatalf("reconcile mapping B: %v", err)
	}
	if resultB.RequeueAfter != 60*time.Second {
		t.Errorf("mapping B requeue: got %v, want 60s", resultB.RequeueAfter)
	}

	// Verify prefix set A
	prefixSetNameA := model.OwnershipKey("test-cluster", "apps", "frontend-map")
	psA, err := fakeClient.Get(ctx, sub, rg, asgA, prefixSetNameA)
	if err != nil {
		t.Fatalf("get prefix set A: %v", err)
	}
	if len(psA.Properties.AddressPrefixes) != 1 || psA.Properties.AddressPrefixes[0] != "10.1.0.1" {
		t.Errorf("prefix set A IPs: got %v, want [10.1.0.1]", psA.Properties.AddressPrefixes)
	}

	// Verify prefix set B
	prefixSetNameB := model.OwnershipKey("test-cluster", "apps", "backend-map")
	psB, err := fakeClient.Get(ctx, sub, rg, asgB, prefixSetNameB)
	if err != nil {
		t.Fatalf("get prefix set B: %v", err)
	}
	if len(psB.Properties.AddressPrefixes) != 1 || psB.Properties.AddressPrefixes[0] != "10.1.0.1" {
		t.Errorf("prefix set B IPs: got %v, want [10.1.0.1]", psB.Properties.AddressPrefixes)
	}

	// Verify no cross-contamination: ASG A should not have prefix set B's name
	_, err = fakeClient.Get(ctx, sub, rg, asgA, prefixSetNameB)
	if !azure.IsNotFound(err) {
		t.Errorf("expected asgA to NOT have prefix set %s; err=%v", prefixSetNameB, err)
	}
	_, err = fakeClient.Get(ctx, sub, rg, asgB, prefixSetNameA)
	if !azure.IsNotFound(err) {
		t.Errorf("expected asgB to NOT have prefix set %s; err=%v", prefixSetNameA, err)
	}
}

// TestIntegration_Phase5_NamespaceIsolation verifies that pods in namespace A
// do not affect the prefix set produced by a mapping in namespace B.
func TestIntegration_Phase5_NamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	sub := "sub-ns"
	rg := "rg-net"
	asgName := "asg-shared"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	// Both pods have matching labels, but are in different namespaces
	podA := makePod("ns-a", "pod-a", map[string]string{"app": "web"}, "10.0.1.1")
	podB := makePod("ns-b", "pod-b", map[string]string{"app": "web"}, "10.0.2.1")

	// Only namespace B has a mapping
	mappingB := makeMapping("ns-b", "web-map", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgName)},
		},
	}})

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(podA, podB, mappingB).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "web-map", Namespace: "ns-b"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	prefixSetName := model.OwnershipKey("test-cluster", "ns-b", "web-map")
	ps, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetName)
	if err != nil {
		t.Fatalf("get prefix set: %v", err)
	}

	// Only pod B's IP should be in the prefix set (namespace isolation)
	if len(ps.Properties.AddressPrefixes) != 1 {
		t.Fatalf("expected 1 IP, got %d: %v", len(ps.Properties.AddressPrefixes), ps.Properties.AddressPrefixes)
	}
	if ps.Properties.AddressPrefixes[0] != "10.0.2.1" {
		t.Errorf("expected 10.0.2.1, got %s", ps.Properties.AddressPrefixes[0])
	}
}

// TestIntegration_Phase5_FullLifecycle_CreateActiveDeleteCleanup exercises the complete
// lifecycle of a PodASGMapping through the reconciler:
// 1. Initial reconcile creates prefix set and owned-targets annotation
// 2. Spec mutation adds a second ASG, reconcile creates second prefix set
// 3. Mapping deletion triggers finalizer cleanup of both prefix sets
func TestIntegration_Phase5_FullLifecycle_CreateActiveDeleteCleanup(t *testing.T) {
	ctx := context.Background()
	sub := "sub-lifecycle"
	rg := "rg-net"
	asg1 := "asg-alpha"
	asg2 := "asg-beta"
	ns := "lifecycle-ns"
	mappingName := "lifecycle-map"
	prefixSetName := model.OwnershipKey("test-cluster", ns, mappingName)

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	mapping := makeMapping(ns, mappingName, []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asg1)},
		},
	}})
	pod := makePod(ns, "api-1", map[string]string{"app": "api"}, "10.2.0.1")

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping, pod).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Phase 1: Initial reconcile — creates prefix set in ASG1
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	ps1, err := fakeClient.Get(ctx, sub, rg, asg1, prefixSetName)
	if err != nil {
		t.Fatalf("get prefix set after initial reconcile: %v", err)
	}
	if len(ps1.Properties.AddressPrefixes) != 1 || ps1.Properties.AddressPrefixes[0] != "10.2.0.1" {
		t.Errorf("initial prefix set IPs: got %v, want [10.2.0.1]", ps1.Properties.AddressPrefixes)
	}

	// Verify finalizer was added
	var afterInitial v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mappingName, Namespace: ns}, &afterInitial); err != nil {
		t.Fatalf("get mapping after initial: %v", err)
	}
	hasFinalizer := false
	for _, f := range afterInitial.Finalizers {
		if f == controller.MappingCleanupFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Fatal("expected finalizer after initial reconcile")
	}

	// Phase 2: Mutate spec to add second ASG
	afterInitial.Spec.Mappings[0].ApplicationSecurityGroups = append(
		afterInitial.Spec.Mappings[0].ApplicationSecurityGroups,
		v1alpha1.ASGReference{ResourceID: asgResourceID(sub, rg, asg2)},
	)
	if err := k8sClient.Update(ctx, &afterInitial); err != nil {
		t.Fatalf("update mapping spec: %v", err)
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile after spec change: %v", err)
	}

	// Verify second prefix set was created
	ps2, err := fakeClient.Get(ctx, sub, rg, asg2, prefixSetName)
	if err != nil {
		t.Fatalf("get prefix set in ASG2: %v", err)
	}
	if len(ps2.Properties.AddressPrefixes) != 1 || ps2.Properties.AddressPrefixes[0] != "10.2.0.1" {
		t.Errorf("ASG2 prefix set IPs: got %v, want [10.2.0.1]", ps2.Properties.AddressPrefixes)
	}

	// Phase 3: Delete the mapping — the fake client sets DeletionTimestamp
	// when the object has a finalizer, so calling Delete will soft-delete.
	var current v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mappingName, Namespace: ns}, &current); err != nil {
		t.Fatalf("get mapping before delete: %v", err)
	}
	if err := k8sClient.Delete(ctx, &current); err != nil {
		t.Fatalf("delete mapping: %v", err)
	}

	// Object should still exist (finalizer prevents hard delete) but with DeletionTimestamp set
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	// Both prefix sets should be deleted
	_, err = fakeClient.Get(ctx, sub, rg, asg1, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("expected ASG1 prefix set to be deleted; err=%v", err)
	}
	_, err = fakeClient.Get(ctx, sub, rg, asg2, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("expected ASG2 prefix set to be deleted; err=%v", err)
	}
}

// TestIntegration_Phase5_ClusterNameIsolation verifies that two reconcilers with
// different ClusterName values produce distinct prefix set names for the same
// mapping, ensuring multi-cluster safety.
func TestIntegration_Phase5_ClusterNameIsolation(t *testing.T) {
	ctx := context.Background()
	sub := "sub-cluster"
	rg := "rg-net"
	asgName := "asg-shared"
	ns := "default"
	mappingName := "web-map"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	mapping := makeMapping(ns, mappingName, []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgName)},
		},
	}})
	pod := makePod(ns, "web-1", map[string]string{"app": "web"}, "10.3.0.1")

	// Cluster A reconciler
	k8sClientA := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping.DeepCopy(), pod.DeepCopy()).
		Build()

	log := zaptest.NewLogger(t)
	executorA := azure.NewExecutor(log, factory, 4)
	reconcilerA := &controller.MappingReconciler{
		Client:         k8sClientA,
		Scheme:         newScheme(),
		ClusterName:    "cluster-east",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executorA,
	}

	// Cluster B reconciler (same Azure target, different cluster name)
	k8sClientB := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping.DeepCopy(), pod.DeepCopy()).
		Build()

	executorB := azure.NewExecutor(log, factory, 4)
	reconcilerB := &controller.MappingReconciler{
		Client:         k8sClientB,
		Scheme:         newScheme(),
		ClusterName:    "cluster-west",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executorB,
	}

	// Reconcile from both clusters
	_, err := reconcilerA.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile cluster A: %v", err)
	}

	_, err = reconcilerB.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile cluster B: %v", err)
	}

	// Verify distinct prefix sets exist
	prefixSetA := model.OwnershipKey("cluster-east", ns, mappingName)
	prefixSetB := model.OwnershipKey("cluster-west", ns, mappingName)

	if prefixSetA == prefixSetB {
		t.Fatal("cluster names should produce different prefix set names")
	}

	psA, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetA)
	if err != nil {
		t.Fatalf("get prefix set A: %v", err)
	}
	psB, err := fakeClient.Get(ctx, sub, rg, asgName, prefixSetB)
	if err != nil {
		t.Fatalf("get prefix set B: %v", err)
	}

	// Both should have the pod IP
	if len(psA.Properties.AddressPrefixes) != 1 || psA.Properties.AddressPrefixes[0] != "10.3.0.1" {
		t.Errorf("cluster A prefix set: got %v, want [10.3.0.1]", psA.Properties.AddressPrefixes)
	}
	if len(psB.Properties.AddressPrefixes) != 1 || psB.Properties.AddressPrefixes[0] != "10.3.0.1" {
		t.Errorf("cluster B prefix set: got %v, want [10.3.0.1]", psB.Properties.AddressPrefixes)
	}

	// Verify both coexist in the same ASG
	allSets, err := fakeClient.List(ctx, sub, rg, asgName)
	if err != nil {
		t.Fatalf("list prefix sets: %v", err)
	}
	if len(allSets) != 2 {
		t.Errorf("expected 2 prefix sets in ASG, got %d", len(allSets))
	}
}

// TestIntegration_Phase5_PodMapperToReconciler_E2E verifies the full flow from
// MapPodToMappings producing reconcile requests to the reconciler processing them
// and writing the correct state to Azure.
func TestIntegration_Phase5_PodMapperToReconciler_E2E(t *testing.T) {
	ctx := context.Background()
	sub := "sub-mapper"
	rg := "rg-net"
	asgName := "asg-app"

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	mapping1 := makeMapping("default", "map-alpha", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgName)},
		},
	}})
	mapping2 := makeMapping("default", "map-beta", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"env": "prod"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgName)},
		},
	}})

	// This pod matches both mappings
	pod := makePod("default", "web-prod", map[string]string{"app": "web", "env": "prod"}, "10.4.0.1")

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping1, mapping2, pod).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Step 1: MapPodToMappings should return requests for both mappings
	requests := reconciler.MapPodToMappings(ctx, pod)
	if len(requests) != 2 {
		t.Fatalf("expected 2 reconcile requests from mapper, got %d", len(requests))
	}

	// Sort requests for deterministic assertion
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].Name < requests[j].Name
	})
	if requests[0].Name != "map-alpha" || requests[1].Name != "map-beta" {
		t.Errorf("unexpected requests: %v", requests)
	}

	// Step 2: Execute reconcile for each request
	for _, req := range requests {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req.NamespacedName})
		if err != nil {
			t.Fatalf("reconcile %s: %v", req.Name, err)
		}
	}

	// Step 3: Both mappings should have created their own prefix sets
	psAlpha, err := fakeClient.Get(ctx, sub, rg, asgName, model.OwnershipKey("test-cluster", "default", "map-alpha"))
	if err != nil {
		t.Fatalf("get alpha prefix set: %v", err)
	}
	psBeta, err := fakeClient.Get(ctx, sub, rg, asgName, model.OwnershipKey("test-cluster", "default", "map-beta"))
	if err != nil {
		t.Fatalf("get beta prefix set: %v", err)
	}

	if psAlpha.Properties.AddressPrefixes[0] != "10.4.0.1" {
		t.Errorf("alpha prefix set: got %v, want [10.4.0.1]", psAlpha.Properties.AddressPrefixes)
	}
	if psBeta.Properties.AddressPrefixes[0] != "10.4.0.1" {
		t.Errorf("beta prefix set: got %v, want [10.4.0.1]", psBeta.Properties.AddressPrefixes)
	}
}

// TestIntegration_Phase5_OwnedTargetAnnotation_EnablesOrphanCleanup verifies that
// the owned-targets annotation persisted from a prior reconcile cycle is used to
// detect and clean up orphaned prefix sets when the mapping spec changes.
func TestIntegration_Phase5_OwnedTargetAnnotation_EnablesOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	sub := "sub-orphan"
	rg := "rg-net"
	asgOld := "asg-old"
	asgNew := "asg-new"
	ns := "default"
	mappingName := "evolving-map"
	prefixSetName := model.OwnershipKey("test-cluster", ns, mappingName)

	fakeClient := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub, fakeClient)

	// Seed Azure: the old ASG has a prefix set from a prior cycle
	if err := fakeClient.Put(ctx, sub, rg, asgOld, prefixSetName, []string{"10.5.0.1"}); err != nil {
		t.Fatalf("seed old prefix set: %v", err)
	}

	// Mapping spec now points to the NEW ASG only, but annotation remembers old
	ownedTargets, _ := json.Marshal([]controller.OwnedTarget{
		{ResourceID: asgResourceID(sub, rg, asgOld), PrefixSetName: prefixSetName},
	})

	mapping := makeMapping(ns, mappingName, []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub, rg, asgNew)},
		},
	}})
	mapping.Finalizers = []string{controller.MappingCleanupFinalizer}
	mapping.Annotations = map[string]string{
		controller.OwnedTargetsAnnotation: string(ownedTargets),
	}

	pod := makePod(ns, "web-1", map[string]string{"app": "web"}, "10.5.0.1")

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping, pod).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mappingName, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Old ASG prefix set should be deleted (orphan cleanup via read scope union)
	_, err = fakeClient.Get(ctx, sub, rg, asgOld, prefixSetName)
	if !azure.IsNotFound(err) {
		t.Errorf("expected old ASG prefix set to be deleted (orphan cleanup); err=%v", err)
	}

	// New ASG prefix set should exist
	psNew, err := fakeClient.Get(ctx, sub, rg, asgNew, prefixSetName)
	if err != nil {
		t.Fatalf("get new ASG prefix set: %v", err)
	}
	if len(psNew.Properties.AddressPrefixes) != 1 || psNew.Properties.AddressPrefixes[0] != "10.5.0.1" {
		t.Errorf("new prefix set IPs: got %v, want [10.5.0.1]", psNew.Properties.AddressPrefixes)
	}

	// Annotation should now reflect only the new target
	var updated v1alpha1.PodASGMapping
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mappingName, Namespace: ns}, &updated); err != nil {
		t.Fatalf("get updated mapping: %v", err)
	}
	raw := updated.Annotations[controller.OwnedTargetsAnnotation]
	var targets []controller.OwnedTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		t.Fatalf("unmarshal owned targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("expected 1 owned target, got %d", len(targets))
	}
	if targets[0].ResourceID != asgResourceID(sub, rg, asgNew) {
		t.Errorf("owned target resource ID: got %s, want %s", targets[0].ResourceID, asgResourceID(sub, rg, asgNew))
	}
}

// TestIntegration_Phase5_FactoryRoutesMultipleSubscriptions_AcrossReconcileCycles
// verifies that the Azure client factory correctly routes to different subscription
// clients across multiple reconcile cycles for the same reconciler.
func TestIntegration_Phase5_FactoryRoutesMultipleSubscriptions_AcrossReconcileCycles(t *testing.T) {
	ctx := context.Background()
	sub1 := "sub-east"
	sub2 := "sub-west"
	rg := "rg-net"
	asg1 := "asg-east"
	asg2 := "asg-west"
	ns := "multi-sub"

	fakeClient1 := fake.NewClient()
	fakeClient2 := fake.NewClient()
	factory := fake.NewClientFactory()
	factory.RegisterClient(sub1, fakeClient1)
	factory.RegisterClient(sub2, fakeClient2)

	mapping1 := makeMapping(ns, "east-map", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"region": "east"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub1, rg, asg1)},
		},
	}})
	mapping2 := makeMapping(ns, "west-map", []v1alpha1.Mapping{{
		PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"region": "west"}},
		ApplicationSecurityGroups: []v1alpha1.ASGReference{
			{ResourceID: asgResourceID(sub2, rg, asg2)},
		},
	}})

	podEast := makePod(ns, "pod-east", map[string]string{"region": "east"}, "10.6.1.1")
	podWest := makePod(ns, "pod-west", map[string]string{"region": "west"}, "10.6.2.1")

	k8sClient := fakeclient.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mapping1, mapping2, podEast, podWest).
		Build()

	reconciler := buildReconciler(t, k8sClient, factory)

	// Reconcile east mapping
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "east-map", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile east: %v", err)
	}

	// Reconcile west mapping
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "west-map", Namespace: ns},
	})
	if err != nil {
		t.Fatalf("reconcile west: %v", err)
	}

	// East client should have east prefix set
	psEast, err := fakeClient1.Get(ctx, sub1, rg, asg1, model.OwnershipKey("test-cluster", ns, "east-map"))
	if err != nil {
		t.Fatalf("get east prefix set: %v", err)
	}
	if psEast.Properties.AddressPrefixes[0] != "10.6.1.1" {
		t.Errorf("east IPs: got %v, want [10.6.1.1]", psEast.Properties.AddressPrefixes)
	}

	// West client should have west prefix set
	psWest, err := fakeClient2.Get(ctx, sub2, rg, asg2, model.OwnershipKey("test-cluster", ns, "west-map"))
	if err != nil {
		t.Fatalf("get west prefix set: %v", err)
	}
	if psWest.Properties.AddressPrefixes[0] != "10.6.2.1" {
		t.Errorf("west IPs: got %v, want [10.6.2.1]", psWest.Properties.AddressPrefixes)
	}

	// Verify no cross-routing: east client should NOT have west data
	_, err = fakeClient1.Get(ctx, sub1, rg, asg2, model.OwnershipKey("test-cluster", ns, "west-map"))
	if !azure.IsNotFound(err) {
		t.Errorf("east client should not have west prefix set")
	}
	_, err = fakeClient2.Get(ctx, sub2, rg, asg1, model.OwnershipKey("test-cluster", ns, "east-map"))
	if !azure.IsNotFound(err) {
		t.Errorf("west client should not have east prefix set")
	}
}
