package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	azurefake "github.com/Azure/pod-nsg-controller/internal/azure/fake"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// fakeExecutor records Execute calls and returns preset results.
type fakeExecutor struct {
	calls   [][]engine.Action
	results []azure.ActionResult
}

func (f *fakeExecutor) Execute(_ context.Context, actions []engine.Action) []azure.ActionResult {
	f.calls = append(f.calls, actions)
	if f.results != nil {
		return f.results
	}
	results := make([]azure.ActionResult, len(actions))
	for i, a := range actions {
		results[i] = azure.ActionResult{Action: a, Success: true}
	}
	return results
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 scheme: %v", err)
	}
	return s
}

func newTestReconciler(t *testing.T, objs []client.Object, executor ActionExecutor, factory azure.AddressPrefixSetClientFactory) *MappingReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	cb := fakeclient.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.PodASGMapping{})
	if len(objs) > 0 {
		cb = cb.WithObjects(objs...)
	}
	fakeClient := cb.Build()
	return &MappingReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ClusterName:    "test-cluster",
		ResyncInterval: 60 * time.Second,
		Factory:        factory,
		Executor:       executor,
	}
}

func buildMapping(name, namespace string, mappings []v1alpha1.Mapping, finalizers []string, annotations map[string]string) *v1alpha1.PodASGMapping {
	m := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Finalizers:  finalizers,
			Annotations: annotations,
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: mappings,
		},
	}
	return m
}

func buildDeletingMapping(name, namespace string, mappings []v1alpha1.Mapping, finalizers []string, annotations map[string]string) *v1alpha1.PodASGMapping {
	m := buildMapping(name, namespace, mappings, finalizers, annotations)
	now := metav1.Now()
	m.DeletionTimestamp = &now
	return m
}

func buildPod(name, namespace, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: ip,
		},
	}
}

const testASGResourceID = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-1"
const testASGResourceID2 = "/subscriptions/sub-1/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-2"

// ============================================================
// T5.1: Pod created matching a mapping → reconcile creates prefix set
// ============================================================

func TestPhase5_T51_CreatePath_ReconcileCreatesPrefixSetInAzure(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		nil,
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0, got %v", result.RequeueAfter)
	}
	if got := len(executor.calls); got != 1 {
		t.Fatalf("expected executor to be called once, got %d", got)
	}

	actions := executor.calls[0]
	if len(actions) != 1 {
		t.Fatalf("expected exactly one action, got %d", len(actions))
	}
	if got := actions[0].Kind; got != engine.CreatePrefixSet {
		t.Errorf("expected action kind %q, got %q", engine.CreatePrefixSet, got)
	}
	if got := actions[0].DesiredIPs; len(got) != 1 || got[0] != "10.0.0.1" {
		t.Errorf("expected DesiredIPs [10.0.0.1], got %v", got)
	}

	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("getting updated mapping: %v", err)
	}
	if got := updated.Annotations[OwnedTargetsAnnotation]; got == "" {
		t.Error("expected owned targets annotation to be written")
	}
}

// ============================================================
// T5.2: Pod deleted → mapping is enqueued, IP removed from desired state
// ============================================================

func TestPhase5_T52_DeletePath_ReconcileRemovesIPFromDesiredAndEmitsUpdateOrDelete(t *testing.T) {
	// Setup: A mapping with selector app=web targeting asg-1.
	// Pod "web-1" previously matched and had IP 10.0.0.1 (prefix set exists in Azure).
	// Now the pod is deleted, so reconcile should produce an update removing the IP
	// or a delete if no IPs remain.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	// Pre-create a prefix set with the pod's IP
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	// No pods in the namespace (pod was deleted)
	r := newTestReconciler(t, []client.Object{mapping}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Reconcile should requeue
	if result.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0, got 0")
	}

	// Executor should have been called with a delete or update action (no IPs match)
	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called, got 0 calls")
	}

	actions := executor.calls[0]
	if len(actions) == 0 {
		t.Fatal("expected at least one action, got 0")
	}

	// With no matching pods, the prefix set should be deleted (empty desired state for that target)
	foundDelete := false
	for _, a := range actions {
		if a.Kind == engine.DeletePrefixSet {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected DeletePrefixSet action when no pods match, got actions: %+v", actions)
	}
}

// ============================================================
// T5.3: Pod IP changes → reconcile swaps old and new IP
// ============================================================

func TestPhase5_T53_IPChange_ReconcileSwapsOldAndNewIP(t *testing.T) {
	// A pod previously had IP 10.0.0.1 (stored in Azure prefix set).
	// Now it has IP 10.0.0.2.
	// Reconcile should produce an update replacing old IP with new.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	pod := buildPod("web-1", "default", "10.0.0.2", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0, got 0")
	}

	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	if len(actions) == 0 {
		t.Fatal("expected at least one action")
	}

	// Should be an update with only the new IP
	foundUpdate := false
	for _, a := range actions {
		if a.Kind == engine.UpdatePrefixSet {
			foundUpdate = true
			if len(a.DesiredIPs) != 1 || a.DesiredIPs[0] != "10.0.0.2" {
				t.Errorf("expected DesiredIPs=[10.0.0.2], got %v", a.DesiredIPs)
			}
		}
	}
	if !foundUpdate {
		t.Errorf("expected UpdatePrefixSet action, got actions: %+v", actions)
	}
}

// ============================================================
// T5.4: Pod label changes away → reconcile removes IP
// ============================================================

func TestPhase5_T54_LabelChangeAway_ReconcileRemovesIP(t *testing.T) {
	// Pod previously matched (app=web) and had IP 10.0.0.1.
	// Now pod has labels app=api (no longer matches).
	// Reconcile should remove the IP (delete or update to empty).

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	// Pod labels no longer match
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "api"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	// Should delete the prefix set since no pods match
	foundDelete := false
	for _, a := range actions {
		if a.Kind == engine.DeletePrefixSet {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected DeletePrefixSet action when pod labels no longer match, got: %+v", actions)
	}
}

// ============================================================
// T5.5: PodASGMapping spec updated (new ASG added) → creates prefix set in new ASG
// ============================================================

func TestPhase5_T55_MappingSpecAddASG_CreatesPrefixSetInNewASG(t *testing.T) {
	// Mapping now has two ASG targets. asg-1 already has a prefix set, asg-2 is new.
	// Reconcile should create a new prefix set in asg-2.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	// asg-1 prefix set already exists with correct IPs
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{
				{ResourceID: testASGResourceID},
				{ResourceID: testASGResourceID2},
			},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{
				{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"},
			}),
		},
	)

	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	foundCreate := false
	for _, a := range actions {
		if a.Kind == engine.CreatePrefixSet && a.Target.ASGName == "asg-2" {
			foundCreate = true
			if len(a.DesiredIPs) != 1 || a.DesiredIPs[0] != "10.0.0.1" {
				t.Errorf("expected DesiredIPs=[10.0.0.1] for new ASG, got %v", a.DesiredIPs)
			}
		}
	}
	if !foundCreate {
		t.Errorf("expected CreatePrefixSet for asg-2, got actions: %+v", actions)
	}
}

// ============================================================
// T5.6: PodASGMapping spec updated (ASG removed) → deletes removed ASG prefix set
// ============================================================

func TestPhase5_T56_MappingSpecRemoveASG_DeletesRemovedASGPrefixSet_AndPrunesOwnedTargets(t *testing.T) {
	// Mapping previously had asg-1 and asg-2. Now it only has asg-1.
	// The prefix set in asg-2 should be deleted.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-2", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{
				{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"},
				{ResourceID: testASGResourceID2, PrefixSetName: "test-cluster-default-my-mapping"},
			}),
		},
	)

	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	foundDelete := false
	for _, a := range actions {
		if a.Kind == engine.DeletePrefixSet && a.Target.ASGName == "asg-2" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected DeletePrefixSet for asg-2, got actions: %+v", actions)
	}
}

// ============================================================
// T5.7: PodASGMapping deleted → finalizer triggers cleanup
// ============================================================

func TestPhase5_T57_MappingDeleting_FinalizerCleansUnionOfSpecAndPersistedTargets(t *testing.T) {
	// Mapping is being deleted (has DeletionTimestamp).
	// It should clean up all owned prefix sets (union of spec targets + persisted annotation targets).

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-2", "test-cluster-default-my-mapping", []string{"10.0.0.2"})

	executor := &fakeExecutor{}

	// Mapping has asg-1 in spec but asg-2 in persisted annotation only (previously owned)
	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{
				{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"},
				{ResourceID: testASGResourceID2, PrefixSetName: "test-cluster-default-my-mapping"},
			}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Should not requeue on successful deletion
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after successful deletion cleanup, got RequeueAfter=%v", result.RequeueAfter)
	}

	// Both prefix sets should have been deleted via the executor or directly
	// Verify the Azure state
	_, getErr1 := fakeClient.Get(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping")
	if !azure.IsNotFound(getErr1) {
		t.Errorf("expected prefix set in asg-1 to be deleted, got err: %v", getErr1)
	}

	_, getErr2 := fakeClient.Get(context.Background(), "sub-1", "rg-1", "asg-2", "test-cluster-default-my-mapping")
	if !azure.IsNotFound(getErr2) {
		t.Errorf("expected prefix set in asg-2 to be deleted, got err: %v", getErr2)
	}

	// Finalizer should have been removed from the mapping
	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err == nil {
		for _, f := range updated.Finalizers {
			if f == MappingCleanupFinalizer {
				t.Error("expected finalizer to be removed after cleanup")
			}
		}
	}
}

// ============================================================
// T5.8: Periodic resync fires after interval
// ============================================================

func TestPhase5_T58_PeriodicResync_ReturnsConfiguredRequeueAfterAndCorrectsDrift(t *testing.T) {
	// After a successful reconcile, Result.RequeueAfter should equal configured interval.
	// If Azure state has drifted, reconcile should correct it.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	// Azure has drifted: contains an extra IP "10.0.0.99" not in desired state
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1", "10.0.0.99"})

	executor := &fakeExecutor{}
	resyncInterval := 120 * time.Second

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	scheme := newTestScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(mapping, pod).WithStatusSubresource(&v1alpha1.PodASGMapping{}).Build()
	r := &MappingReconciler{
		Client:         fakeK8s,
		Scheme:         scheme,
		ClusterName:    "test-cluster",
		ResyncInterval: resyncInterval,
		Factory:        factory,
		Executor:       executor,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	// Assert RequeueAfter equals configured interval
	if result.RequeueAfter != resyncInterval {
		t.Errorf("expected RequeueAfter=%v, got %v", resyncInterval, result.RequeueAfter)
	}

	// Assert drift was corrected (update action issued to remove extra IP)
	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called to correct drift")
	}

	actions := executor.calls[0]
	foundCorrection := false
	for _, a := range actions {
		if a.Kind == engine.UpdatePrefixSet {
			foundCorrection = true
			// Should only contain the valid IP
			if len(a.DesiredIPs) != 1 || a.DesiredIPs[0] != "10.0.0.1" {
				t.Errorf("expected drift correction to DesiredIPs=[10.0.0.1], got %v", a.DesiredIPs)
			}
		}
	}
	if !foundCorrection {
		t.Errorf("expected UpdatePrefixSet to correct drift, got actions: %+v", actions)
	}
}

func TestPhase5_Reconcile_EnsuresFinalizerBeforeActiveFlow(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		nil,
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter > 0, got %v", result.RequeueAfter)
	}

	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("getting updated mapping: %v", err)
	}

	hasFinalizer := false
	for _, finalizer := range updated.Finalizers {
		if finalizer == MappingCleanupFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected reconcile to add cleanup finalizer")
	}
}

func TestPhase5_Reconcile_StatusPlaceholderUpdaterInvoked(t *testing.T) {
	tests := []struct {
		name              string
		seedActual        bool
		wantExecutorCalls int
		wantResultCount   int
	}{
		{
			name:              "no_actions_path",
			seedActual:        true,
			wantExecutorCalls: 0,
			wantResultCount:   0,
		},
		{
			name:              "actions_success_path",
			seedActual:        false,
			wantExecutorCalls: 1,
			wantResultCount:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := azurefake.NewClientFactory()
			fakeClient := azurefake.NewClient()
			factory.RegisterClient("sub-1", fakeClient)
			if tt.seedActual {
				_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})
			}

			executor := &fakeExecutor{}
			mapping := buildMapping("my-mapping", "default",
				[]v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
				[]string{MappingCleanupFinalizer},
				map[string]string{
					OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
				},
			)
			pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

			called := 0
			var gotMapping types.NamespacedName
			gotResultCount := -1

			r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
			r.StatusUpdater = func(_ context.Context, mapping *v1alpha1.PodASGMapping, results []azure.ActionResult) error {
				called++
				gotMapping = types.NamespacedName{Name: mapping.Name, Namespace: mapping.Namespace}
				gotResultCount = len(results)
				return nil
			}

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("unexpected reconcile error: %v", err)
			}
			if result.RequeueAfter == 0 {
				t.Errorf("expected RequeueAfter > 0, got %v", result.RequeueAfter)
			}
			if called != 1 {
				t.Errorf("expected status updater to be called once, got %d", called)
			}
			if gotMapping != (types.NamespacedName{Name: "my-mapping", Namespace: "default"}) {
				t.Errorf("expected updater mapping %v, got %v", types.NamespacedName{Name: "my-mapping", Namespace: "default"}, gotMapping)
			}
			if gotResultCount != tt.wantResultCount {
				t.Errorf("expected updater result count %d, got %d", tt.wantResultCount, gotResultCount)
			}
			if got := len(executor.calls); got != tt.wantExecutorCalls {
				t.Errorf("expected executor calls %d, got %d", tt.wantExecutorCalls, got)
			}
		})
	}
}

func TestPhase5_Reconcile_StatusPlaceholderUpdaterErrorReturned(t *testing.T) {
	tests := []struct {
		name       string
		seedActual bool
	}{
		{name: "no_actions_path", seedActual: true},
		{name: "actions_success_path", seedActual: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := azurefake.NewClientFactory()
			fakeClient := azurefake.NewClient()
			factory.RegisterClient("sub-1", fakeClient)
			if tt.seedActual {
				_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})
			}

			executor := &fakeExecutor{}
			mapping := buildMapping("my-mapping", "default",
				[]v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
				[]string{MappingCleanupFinalizer},
				map[string]string{
					OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
				},
			)
			pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

			wantErr := errors.New("status updater failed")
			r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
			r.StatusUpdater = func(_ context.Context, _ *v1alpha1.PodASGMapping, _ []azure.ActionResult) error {
				return wantErr
			}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
			})
			if err == nil {
				t.Fatal("expected reconcile error from status updater, got nil")
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("expected reconcile error to wrap %v, got %v", wantErr, err)
			}
		})
	}
}

func TestPhase5_ReconcileDeleting_AzureDeleteFails_RetainsFinalizer(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})
	_ = fakeClient.InjectError(azurefake.InjectKey{
		Operation:      azurefake.OperationDelete,
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
		PrefixSetName:  "test-cluster-default-my-mapping",
	}, errors.New("azure delete failed"), 1)

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, &fakeExecutor{}, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error when Azure delete fails, got nil")
	}

	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("getting updated mapping: %v", err)
	}

	hasFinalizer := false
	for _, finalizer := range updated.Finalizers {
		if finalizer == MappingCleanupFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected finalizer to be retained when Azure delete fails")
	}
}

// ============================================================
// T5.8 additional: Mapping predicate uses deep equal for spec, finalizers, deletion timestamp
// ============================================================

func TestPhase5_MappingPredicate_UpdateUsesDeepEqualForSpecFinalizersDeletionTimestamp(t *testing.T) {
	pred := MappingEventPredicate()

	t.Run("spec_change_passes", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
			},
		}
		newMapping := oldMapping.DeepCopy()
		newMapping.Spec.Mappings[0].PodSelector.MatchLabels["app"] = "api"

		evt := makeUpdateEvent(oldMapping, newMapping)
		if !pred.Update(evt) {
			t.Error("expected predicate to pass for spec change")
		}
	})

	t.Run("status_only_change_blocked", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
			},
		}
		newMapping := oldMapping.DeepCopy()
		newMapping.Status.MappingCount = 5

		evt := makeUpdateEvent(oldMapping, newMapping)
		if pred.Update(evt) {
			t.Error("expected predicate to block status-only change")
		}
	})

	t.Run("finalizer_change_passes", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
			},
		}
		newMapping := oldMapping.DeepCopy()
		newMapping.Finalizers = []string{MappingCleanupFinalizer}

		evt := makeUpdateEvent(oldMapping, newMapping)
		if !pred.Update(evt) {
			t.Error("expected predicate to pass for finalizer change")
		}
	})

	t.Run("deletion_timestamp_change_passes", func(t *testing.T) {
		oldMapping := &v1alpha1.PodASGMapping{
			ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
			Spec: v1alpha1.PodASGMappingSpec{
				Mappings: []v1alpha1.Mapping{{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				}},
			},
		}
		newMapping := oldMapping.DeepCopy()
		now := metav1.Now()
		newMapping.DeletionTimestamp = &now

		evt := makeUpdateEvent(oldMapping, newMapping)
		if !pred.Update(evt) {
			t.Error("expected predicate to pass for deletion timestamp change")
		}
	})
}

// ============================================================
// T5.9 (reconciler integration): Reconcile NOT triggered for status-only mapping update
// (tested via predicate — already in predicate test above)
// ============================================================

// ============================================================
// Malformed owned targets annotation fails deletion and retains finalizer
// ============================================================

func TestPhase5_ReconcileDeleting_MalformedOwnedTargetsAnnotationFailsAndRetainsFinalizer(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: "this is not valid json{{{",
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	// Should return an error (malformed annotation)
	if err == nil {
		t.Fatal("expected reconcile error for malformed owned targets annotation, got nil")
	}

	// Finalizer should still be present
	var updated v1alpha1.PodASGMapping
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); getErr == nil {
		hasFinalizer := false
		for _, f := range updated.Finalizers {
			if f == MappingCleanupFinalizer {
				hasFinalizer = true
			}
		}
		if !hasFinalizer {
			t.Error("expected finalizer to be retained when annotation is malformed")
		}
	}
}

func TestPhase5_ReconcileDeleting_AzureListFails_RetainsFinalizer(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	if err := fakeClient.InjectError(azurefake.InjectKey{
		Operation:      azurefake.OperationList,
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
	}, errors.New("azure list failed"), 1); err != nil {
		t.Fatalf("injecting list error: %v", err)
	}

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, &fakeExecutor{}, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error when Azure list fails, got nil")
	}

	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("getting updated mapping: %v", err)
	}

	hasFinalizer := false
	for _, finalizer := range updated.Finalizers {
		if finalizer == MappingCleanupFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("expected finalizer to be retained when Azure list fails")
	}
}

func TestPhase5_FinalizerCleanup_ExactNameSkipsListFallback(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	exactName := "test-cluster-default-my-mapping"
	if err := fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", exactName, []string{"10.0.0.1"}); err != nil {
		t.Fatalf("seeding fake Azure state: %v", err)
	}
	if err := fakeClient.InjectError(azurefake.InjectKey{
		Operation:      azurefake.OperationList,
		SubscriptionID: "sub-1",
		ResourceGroup:  "rg-1",
		ASGName:        "asg-1",
	}, errors.New("unexpected list call"), 1); err != nil {
		t.Fatalf("injecting list error: %v", err)
	}

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: exactName}}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, &fakeExecutor{}, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error when cleanup resolves exact name via get fast path, got: %v", err)
	}

	if _, err := fakeClient.Get(context.Background(), "sub-1", "rg-1", "asg-1", exactName); !azure.IsNotFound(err) {
		t.Errorf("expected exact-name prefix set to be deleted, got err: %v", err)
	}
}

// ============================================================
// Finalizer cleanup: list miss still deletes by target name
// ============================================================

func TestPhase5_FinalizerCleanup_ListMiss_StillDeletesByTargetName(t *testing.T) {
	// When List doesn't find the prefix set (case mismatch or timing), the cleanup
	// should still attempt Delete by target name.

	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	// Don't pre-create the prefix set — simulates a list miss

	executor := &fakeExecutor{}

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})

	// Should succeed (NotFound on delete is tolerated)
	if err != nil {
		t.Fatalf("expected no error when delete returns NotFound, got: %v", err)
	}

	// Finalizer should be removed
	var updated v1alpha1.PodASGMapping
	if getErr := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); getErr == nil {
		for _, f := range updated.Finalizers {
			if f == MappingCleanupFinalizer {
				t.Error("expected finalizer to be removed after cleanup")
			}
		}
	}
}

func TestPhase5_FinalizerCleanup_UsesCasePreservedPrefixSetNameFromList(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	casePreservedName := "Test-Cluster-Default-My-Mapping"
	if err := fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", casePreservedName, []string{"10.0.0.1"}); err != nil {
		t.Fatalf("seeding fake Azure state: %v", err)
	}

	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)

	r := newTestReconciler(t, []client.Object{mapping}, &fakeExecutor{}, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error when cleanup resolves case-preserved name, got: %v", err)
	}

	if _, err := fakeClient.Get(context.Background(), "sub-1", "rg-1", "asg-1", casePreservedName); !azure.IsNotFound(err) {
		t.Errorf("expected case-preserved prefix set to be deleted, got err: %v", err)
	}

	var updated v1alpha1.PodASGMapping
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-mapping", Namespace: "default"}, &updated); err == nil {
		for _, f := range updated.Finalizers {
			if f == MappingCleanupFinalizer {
				t.Error("expected finalizer to be removed after case-preserved cleanup")
			}
		}
	}
}

// ============================================================
// Helper: makeUpdateEvent for predicate tests
// ============================================================

func makeUpdateEvent(oldObj, newObj client.Object) event.UpdateEvent {
	return event.UpdateEvent{
		ObjectOld: oldObj,
		ObjectNew: newObj,
	}
}

func mustMarshalOwnedTargets(targets []OwnedTarget) string {
	data, err := json.Marshal(targets)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// ============================================================
// Reconcile: mapping not found (404) returns empty result with no error
// ============================================================

func TestPhase5_Reconcile_MappingNotFound_ReturnsEmptyResultNoError(t *testing.T) {
	factory := azurefake.NewClientFactory()
	executor := &fakeExecutor{}
	// No mapping created in the fake client
	r := newTestReconciler(t, nil, executor, factory)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error for not-found mapping, got: %v", err)
	}
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected empty result (no requeue), got %+v", result)
	}
	if len(executor.calls) != 0 {
		t.Errorf("expected no executor calls, got %d", len(executor.calls))
	}
}

// ============================================================
// Reconcile deleting path: mapping without finalizer short-circuits (no cleanup)
// ============================================================

func TestPhase5_ReconcileDeleting_NoControllerFinalizer_ShortCircuitsNoCleanup(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	// Pre-create a prefix set — it should NOT be deleted since our finalizer is absent
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}
	// Has a different finalizer (not the controller's cleanup finalizer)
	mapping := buildDeletingMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{"some-other-controller/finalizer"}, // Not our finalizer
		nil,
	)

	r := newTestReconciler(t, []client.Object{mapping}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after deletion without our finalizer, got %v", result.RequeueAfter)
	}

	// Prefix set should still exist (not deleted since our finalizer was absent)
	ps, getErr := fakeClient.Get(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping")
	if getErr != nil {
		t.Errorf("expected prefix set to still exist (no cleanup without our finalizer), got err: %v", getErr)
	}
	if ps == nil {
		t.Error("expected prefix set to still exist")
	}
}

// ============================================================
// Reconcile: multiple pods matching → combined IP set in prefix set
// ============================================================

func TestPhase5_Reconcile_MultiplePods_CombinedIPSet(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		nil,
	)
	pod1 := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})
	pod2 := buildPod("web-2", "default", "10.0.0.2", map[string]string{"app": "web"})
	pod3 := buildPod("api-1", "default", "10.0.0.3", map[string]string{"app": "api"}) // does NOT match

	r := newTestReconciler(t, []client.Object{mapping, pod1, pod2, pod3}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0")
	}
	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Kind != engine.CreatePrefixSet {
		t.Errorf("expected CreatePrefixSet, got %s", actions[0].Kind)
	}
	// Should contain both matching pod IPs but not the non-matching pod
	ips := actions[0].DesiredIPs
	if len(ips) != 2 {
		t.Fatalf("expected 2 IPs in DesiredIPs, got %d: %v", len(ips), ips)
	}
	ipSet := make(map[string]bool)
	for _, ip := range ips {
		ipSet[ip] = true
	}
	if !ipSet["10.0.0.1"] || !ipSet["10.0.0.2"] {
		t.Errorf("expected IPs [10.0.0.1, 10.0.0.2], got %v", ips)
	}
	if ipSet["10.0.0.3"] {
		t.Errorf("did not expect non-matching pod IP 10.0.0.3 in DesiredIPs, got %v", ips)
	}
}

// ============================================================
// Reconcile: executor action failure returns error
// ============================================================

func TestPhase5_Reconcile_ExecutorActionFailure_ReturnsError(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	failErr := errors.New("azure PUT timed out")
	executor := &fakeExecutor{
		results: []azure.ActionResult{{
			Action:  engine.Action{Kind: engine.CreatePrefixSet, Target: engine.ASGTarget{ASGName: "asg-1", PrefixSetName: "test-cluster-default-my-mapping"}},
			Success: false,
			Err:     failErr,
		}},
	}

	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		nil,
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error when executor action fails, got nil")
	}
	if !errors.Is(err, failErr) {
		t.Errorf("expected error to wrap %v, got %v", failErr, err)
	}
}

// ============================================================
// Active reconcile: malformed owned-target annotation returns error
// ============================================================

func TestPhase5_ReconcileActive_MalformedOwnedTargetsAnnotation_ReturnsError(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: "not valid json [[[",
		},
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error for malformed annotation in active path, got nil")
	}
}

// ============================================================
// Reconcile: no matching pods, no pre-existing state → no actions, clean pass
// ============================================================

func TestPhase5_Reconcile_NoMatchingPods_NoPriorState_NoActions(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		nil,
	)
	// Pod exists but doesn't match the selector
	pod := buildPod("api-1", "default", "10.0.0.1", map[string]string{"app": "api"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 even when no actions")
	}
	if len(executor.calls) != 0 {
		t.Errorf("expected no executor calls when no pods match and no prior state, got %d", len(executor.calls))
	}
}

// ============================================================
// Reconcile: zero resync interval still works (edge boundary)
// ============================================================

func TestPhase5_Reconcile_ZeroResyncInterval_ReturnsZeroRequeueAfter(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	scheme := newTestScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(mapping, pod).WithStatusSubresource(&v1alpha1.PodASGMapping{}).Build()
	r := &MappingReconciler{
		Client:         fakeK8s,
		Scheme:         scheme,
		ClusterName:    "test-cluster",
		ResyncInterval: 0, // zero interval boundary
		Factory:        factory,
		Executor:       executor,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected RequeueAfter=0 for zero interval, got %v", result.RequeueAfter)
	}
}

// ============================================================
// Reconcile: nil StatusUpdater defaults to noop (no panic)
// ============================================================

func TestPhase5_Reconcile_NilStatusUpdater_DefaultsToNoop(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)
	_ = fakeClient.Put(context.Background(), "sub-1", "rg-1", "asg-1", "test-cluster-default-my-mapping", []string{"10.0.0.1"})

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	// StatusUpdater is nil by default from newTestReconciler
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("expected no error with nil StatusUpdater, got: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0")
	}
}

// ============================================================
// Reconcile: factory error for unknown subscription
// ============================================================

func TestPhase5_Reconcile_FactoryUnknownSubscription_ReturnsError(t *testing.T) {
	// Factory has no client registered for the subscription referenced in the mapping
	factory := azurefake.NewClientFactory()
	// Intentionally NOT registering "sub-1"

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		map[string]string{
			OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-my-mapping"}}),
		},
	)
	pod := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})

	r := newTestReconciler(t, []client.Object{mapping, pod}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected reconcile error when factory can't provide client for subscription, got nil")
	}
}

// ============================================================
// Reconcile: pods with empty IP are excluded from desired state
// ============================================================

func TestPhase5_Reconcile_PodWithEmptyIP_ExcludedFromDesired(t *testing.T) {
	factory := azurefake.NewClientFactory()
	fakeClient := azurefake.NewClient()
	factory.RegisterClient("sub-1", fakeClient)

	executor := &fakeExecutor{}
	mapping := buildMapping("my-mapping", "default",
		[]v1alpha1.Mapping{{
			PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
			ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
		}},
		[]string{MappingCleanupFinalizer},
		nil,
	)
	podWithIP := buildPod("web-1", "default", "10.0.0.1", map[string]string{"app": "web"})
	podNoIP := buildPod("web-2", "default", "", map[string]string{"app": "web"}) // No IP yet

	r := newTestReconciler(t, []client.Object{mapping, podWithIP, podNoIP}, executor, factory)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if len(executor.calls) == 0 {
		t.Fatal("expected executor to be called")
	}

	actions := executor.calls[0]
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	// Should only have the pod with an IP
	ips := actions[0].DesiredIPs
	for _, ip := range ips {
		if ip == "" {
			t.Error("empty IP should not be in DesiredIPs")
		}
	}
	if len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Errorf("expected DesiredIPs=[10.0.0.1], got %v", ips)
	}
}
