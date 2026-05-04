package controller

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ============================================================
// T5.9: Pod status-only update (no label/IP change) → predicate filters
// ============================================================

func TestPhase5_PodPredicate_GenericEventFiltered(t *testing.T) {
	pred := PodEventPredicate()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	evt := event.GenericEvent{Object: pod}
	if pred.Generic(evt) {
		t.Error("expected PodEventPredicate to filter out generic events (return false), but got true")
	}
}

func TestPhase5_MappingPredicate_GenericEventFiltered(t *testing.T) {
	pred := MappingEventPredicate()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	evt := event.GenericEvent{Object: mapping}
	if pred.Generic(evt) {
		t.Error("expected MappingEventPredicate to filter out generic events (return false), but got true")
	}
}

// ============================================================
// T5.1: Pod created matching a mapping → MapPodToMappings returns reconcile request
// ============================================================

func TestPhase5_T51_MapPodToMappings_PodCreated_MatchingMapping(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	requests := r.MapPodToMappings(context.Background(), pod)

	if len(requests) == 0 {
		t.Fatal("expected MapPodToMappings to return reconcile requests for matching mapping, got 0")
	}

	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"}}
	found := false
	for _, req := range requests {
		if req == expectedReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected reconcile request %v in results, got %v", expectedReq, requests)
	}
}

// ============================================================
// T5.2: Pod deleted → MapPodToMappings returns reconcile request
// ============================================================

func TestPhase5_T52_MapPodToMappings_PodDeleted_MatchingMapping(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	requests := r.MapPodToMappings(context.Background(), pod)

	if len(requests) == 0 {
		t.Fatal("expected MapPodToMappings to return reconcile requests for deleted pod's mapping, got 0")
	}

	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"}}
	found := false
	for _, req := range requests {
		if req == expectedReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected reconcile request %v, got %v", expectedReq, requests)
	}
}

// ============================================================
// T5.3: Pod IP changes → predicate passes update, MapPodToMappings enqueues
// ============================================================

func TestPhase5_T53_MapPodToMappings_PodIPChanged_EnqueuesMapping(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	// Predicate check: IP change passes
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.2"},
	}

	pred := PodEventPredicate()
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
		t.Fatal("expected PodEventPredicate to pass pod IP change update")
	}

	// MapPodToMappings should enqueue the matching mapping
	requests := r.MapPodToMappings(context.Background(), newPod)
	if len(requests) == 0 {
		t.Fatal("expected MapPodToMappings to return reconcile request for pod with changed IP, got 0")
	}

	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"}}
	found := false
	for _, req := range requests {
		if req == expectedReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected reconcile request %v, got %v", expectedReq, requests)
	}
}

// ============================================================
// T5.4: Pod label changes away from match → old pod maps, new pod does not
// ============================================================

func TestPhase5_T54_MapPodToMappings_PodLabelChangeAway_UsesOldMatchOnly(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	// Predicate check: label change passes
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "api"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	pred := PodEventPredicate()
	if !pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}) {
		t.Fatal("expected PodEventPredicate to pass pod label change update")
	}

	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"}}
	oldRequests := r.MapPodToMappings(context.Background(), oldPod)
	foundOld := false
	for _, req := range oldRequests {
		if req == expectedReq {
			foundOld = true
			break
		}
	}
	if !foundOld {
		t.Errorf("expected old matching pod to enqueue %v, got %v", expectedReq, oldRequests)
	}

	newRequests := r.MapPodToMappings(context.Background(), newPod)
	if len(newRequests) != 0 {
		t.Errorf("expected new non-matching pod to enqueue no mappings, got %v", newRequests)
	}
}

func TestPhase5_T54_PodWatchHandler_LabelChangeAway_EnqueuesOldMatch(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-mapping",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}
	eventHandler := handler.EnqueueRequestsFromMapFunc(r.MapPodToMappings)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "api"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	eventHandler.Update(context.Background(), event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}, queue)

	if got := queue.Len(); got != 1 {
		t.Fatalf("expected exactly one queued request from old-match/new-miss update, got %d", got)
	}

	req, shutdown := queue.Get()
	if shutdown {
		t.Fatal("queue unexpectedly shut down")
	}
	queue.Done(req)

	want := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-mapping", Namespace: "default"}}
	if req != want {
		t.Fatalf("expected queued request %v, got %v", want, req)
	}
}

func TestPhase5_PodWatchHandler_LabelSwitchBetweenMappings_EnqueuesOldAndNewMatches(t *testing.T) {
	scheme := newTestScheme(t)

	mappingWeb := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-web", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	mappingAPI := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-api", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingWeb, mappingAPI).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}
	eventHandler := handler.EnqueueRequestsFromMapFunc(r.MapPodToMappings)
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default", Labels: map[string]string{"app": "api"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	eventHandler.Update(context.Background(), event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}, queue)

	if got := queue.Len(); got != 2 {
		t.Fatalf("expected two queued requests from label switch between mappings, got %d", got)
	}

	gotRequests := map[reconcile.Request]bool{}
	for i := 0; i < 2; i++ {
		req, shutdown := queue.Get()
		if shutdown {
			t.Fatal("queue unexpectedly shut down")
		}
		queue.Done(req)
		gotRequests[req] = true
	}

	wantWeb := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-web", Namespace: "default"}}
	wantAPI := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-api", Namespace: "default"}}
	if !gotRequests[wantWeb] || !gotRequests[wantAPI] {
		t.Fatalf("expected queued requests %v and %v, got %v", wantWeb, wantAPI, gotRequests)
	}
}

// ============================================================
// T5.10: Multiple PodASGMappings, pod matches one → MapPodToMappings returns only matching
// ============================================================

func TestPhase5_T510_MapPodToMappings_MultipleMappings_OnlyMatchingReturned(t *testing.T) {
	scheme := newTestScheme(t)

	mappingWeb := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mapping-web",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	mappingAPI := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mapping-api",
			Namespace: "default",
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingWeb, mappingAPI).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	requests := r.MapPodToMappings(context.Background(), pod)

	webReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-web", Namespace: "default"}}
	apiReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-api", Namespace: "default"}}

	foundWeb := false
	foundAPI := false
	for _, req := range requests {
		if req == webReq {
			foundWeb = true
		}
		if req == apiReq {
			foundAPI = true
		}
	}

	if !foundWeb {
		t.Error("expected mapping-web to be in MapPodToMappings results for pod with app=web")
	}
	if foundAPI {
		t.Error("expected mapping-api NOT to be in MapPodToMappings results for pod with app=web")
	}
}

// ============================================================
// T5.5: PodASGMapping spec updated → MappingEventPredicate passes
// ============================================================

func TestPhase5_T55_MappingPredicate_SpecChange_Passes(t *testing.T) {
	pred := MappingEventPredicate()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	newMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
				},
			},
		},
	}

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if !pred.Update(evt) {
		t.Error("expected MappingEventPredicate to pass spec change update")
	}
}

// ============================================================
// T5.6: PodASGMapping spec updated (ASG removed) → predicate passes
// ============================================================

func TestPhase5_T56_MappingPredicate_ASGRemoved_Passes(t *testing.T) {
	pred := MappingEventPredicate()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector: v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{
					{ResourceID: testASGResourceID},
					{ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg2"},
				},
			}},
		},
	}

	newMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if !pred.Update(evt) {
		t.Error("expected MappingEventPredicate to pass when ASG is removed from spec")
	}
}

// ============================================================
// T5.7: PodASGMapping deleted → MappingEventPredicate passes delete
// ============================================================

func TestPhase5_T57_MappingPredicate_Delete_Passes(t *testing.T) {
	pred := MappingEventPredicate()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	evt := event.DeleteEvent{Object: mapping}
	if !pred.Delete(evt) {
		t.Error("expected MappingEventPredicate to pass delete events")
	}
}

// ============================================================
// T5.8: Verify PodEventPredicate passes create events
// ============================================================

func TestPhase5_T58_PodPredicate_Create_Passes(t *testing.T) {
	pred := PodEventPredicate()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	evt := event.CreateEvent{Object: pod}
	if !pred.Create(evt) {
		t.Error("expected PodEventPredicate to pass create events")
	}
}

// ============================================================
// T5.9: Pod status-only update → predicate filters (expanded)
// ============================================================

func TestPhase5_T59_PodPredicate_StatusOnlyUpdate_Blocked(t *testing.T) {
	pred := PodEventPredicate()

	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodSucceeded},
	}

	evt := event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}
	if pred.Update(evt) {
		t.Error("expected PodEventPredicate to block status-only update (no label/IP change)")
	}
}

// ============================================================
// MappingPredicate: status-only update blocked
// ============================================================

func TestPhase5_MappingPredicate_StatusOnlyUpdate_Blocked(t *testing.T) {
	pred := MappingEventPredicate()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
		Status: v1alpha1.PodASGMappingStatus{MappingCount: 1},
	}

	newMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
		Status: v1alpha1.PodASGMappingStatus{MappingCount: 5},
	}

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if pred.Update(evt) {
		t.Error("expected MappingEventPredicate to block status-only update")
	}
}

// ============================================================
// MapPodToMappings: non-pod object returns empty
// ============================================================

func TestPhase5_MapPodToMappings_NonPodObject_ReturnsEmpty(t *testing.T) {
	scheme := newTestScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "default"},
	}

	requests := r.MapPodToMappings(context.Background(), configMap)
	if len(requests) != 0 {
		t.Errorf("expected MapPodToMappings to return empty for non-pod object, got %v", requests)
	}
}

func TestPhase5_MapPodToMappings_EmptyNamespace_ReturnsEmpty(t *testing.T) {
	scheme := newTestScheme(t)
	fakeK8s := fakeclient.NewClientBuilder().WithScheme(scheme).Build()
	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: "",
			Labels:    map[string]string{"app": "web"},
		},
	}

	requests := r.MapPodToMappings(context.Background(), pod)
	if len(requests) != 0 {
		t.Errorf("expected MapPodToMappings to return empty for pod with empty namespace, got %v", requests)
	}
}

func TestPhase5_MapPodToMappings_CrossNamespace_DoesNotEnqueueOtherNamespace(t *testing.T) {
	scheme := newTestScheme(t)

	mappingA := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-a", Namespace: "ns-a"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	mappingB := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-b", Namespace: "ns-b"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingA, mappingB).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "ns-a", Labels: map[string]string{"app": "web"}},
	}

	requests := r.MapPodToMappings(context.Background(), pod)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-a", Namespace: "ns-a"}}
	unwanted := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-b", Namespace: "ns-b"}}

	foundWanted := false
	foundUnwanted := false
	for _, req := range requests {
		if req == want {
			foundWanted = true
		}
		if req == unwanted {
			foundUnwanted = true
		}
	}
	if !foundWanted {
		t.Errorf("expected request %v, got %v", want, requests)
	}
	if foundUnwanted {
		t.Errorf("did not expect cross-namespace request %v in %v", unwanted, requests)
	}
}

// ============================================================
// MapPodToMappings: pod with no labels → namespace-wide fallback
// ============================================================

func TestPhase5_MapPodToMappings_PodNoLabels_FallbackEnqueuesAll(t *testing.T) {
	scheme := newTestScheme(t)

	mapping1 := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	mapping2 := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-2", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping1, mapping2).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-pod", Namespace: "default"},
	}

	requests := r.MapPodToMappings(context.Background(), pod)

	if len(requests) < 2 {
		t.Fatalf("expected MapPodToMappings to return all namespace mappings as fallback for pod with no labels, got %d: %v", len(requests), requests)
	}

	req1 := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-1", Namespace: "default"}}
	req2 := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-2", Namespace: "default"}}

	found1, found2 := false, false
	for _, req := range requests {
		if req == req1 {
			found1 = true
		}
		if req == req2 {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Errorf("expected both mapping-1 and mapping-2 in results, got %v", requests)
	}
}

func TestPhase5_MapPodToMappings_EmptySelectorRuleMatchesAnyLabeledPod(t *testing.T) {
	scheme := newTestScheme(t)

	catchAllMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-all", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	specificMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-api", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "api"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(catchAllMapping, specificMapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-1",
			Namespace: "default",
			Labels:    map[string]string{"role": "worker"},
		},
	}

	requests := r.MapPodToMappings(context.Background(), pod)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-all", Namespace: "default"}}
	unwanted := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-api", Namespace: "default"}}

	foundWanted := false
	foundUnwanted := false
	for _, req := range requests {
		if req == want {
			foundWanted = true
		}
		if req == unwanted {
			foundUnwanted = true
		}
	}

	if !foundWanted {
		t.Errorf("expected empty-selector mapping request %v, got %v", want, requests)
	}
	if foundUnwanted {
		t.Errorf("did not expect unrelated mapping request %v in %v", unwanted, requests)
	}
}

func TestPhase5_MappingPredicate_AnnotationOnlyUpdate_Blocked(t *testing.T) {
	pred := MappingEventPredicate()

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
	newMapping.Annotations = map[string]string{
		OwnedTargetsAnnotation: mustMarshalOwnedTargets([]OwnedTarget{{ResourceID: testASGResourceID, PrefixSetName: "test-cluster-default-m1"}}),
	}

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if pred.Update(evt) {
		t.Error("expected MappingEventPredicate to block annotation-only update")
	}
}

// ============================================================
// Pod predicate: delete event passes
// ============================================================

func TestPhase5_PodPredicate_Delete_Passes(t *testing.T) {
	pred := PodEventPredicate()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	evt := event.DeleteEvent{Object: pod}
	if !pred.Delete(evt) {
		t.Error("expected PodEventPredicate to pass delete events")
	}
}

// ============================================================
// Mapping predicate: create event passes
// ============================================================

func TestPhase5_MappingPredicate_Create_Passes(t *testing.T) {
	pred := MappingEventPredicate()

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	evt := event.CreateEvent{Object: mapping}
	if !pred.Create(evt) {
		t.Error("expected MappingEventPredicate to pass create events")
	}
}

// ============================================================
// Pod predicate: update with non-pod objects falls through safely (returns true)
// ============================================================

func TestPhase5_PodPredicate_UpdateNonPodObject_ReturnsTrue(t *testing.T) {
	pred := PodEventPredicate()

	oldCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "default"}}
	newCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "default"}}

	evt := event.UpdateEvent{ObjectOld: oldCM, ObjectNew: newCM}
	if !pred.Update(evt) {
		t.Error("expected PodEventPredicate to return true for non-pod objects (safety fallback)")
	}
}

// ============================================================
// Mapping predicate: update with non-mapping objects falls through safely (returns true)
// ============================================================

func TestPhase5_MappingPredicate_UpdateNonMappingObject_ReturnsTrue(t *testing.T) {
	pred := MappingEventPredicate()

	oldCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "default"}}
	newCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm1", Namespace: "default"}}

	evt := event.UpdateEvent{ObjectOld: oldCM, ObjectNew: newCM}
	if !pred.Update(evt) {
		t.Error("expected MappingEventPredicate to return true for non-mapping objects (safety fallback)")
	}
}

// ============================================================
// Mapping predicate: DeletionTimestamp both non-nil with different values passes
// ============================================================

func TestPhase5_MappingPredicate_DeletionTimestampBothNonNilDifferent_Passes(t *testing.T) {
	pred := MappingEventPredicate()

	time1 := metav1.Now()
	time2 := metav1.NewTime(time1.Add(5 * time.Second))

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "m1",
			Namespace:         "default",
			DeletionTimestamp: &time1,
			Finalizers:        []string{MappingCleanupFinalizer},
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	newMapping := oldMapping.DeepCopy()
	newMapping.DeletionTimestamp = &time2

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if !pred.Update(evt) {
		t.Error("expected MappingEventPredicate to pass when DeletionTimestamp values differ")
	}
}

// ============================================================
// Mapping predicate: DeletionTimestamp both non-nil with same value is blocked
// (no spec/finalizer/deletionTimestamp change)
// ============================================================

func TestPhase5_MappingPredicate_DeletionTimestampBothNonNilSame_Blocked(t *testing.T) {
	pred := MappingEventPredicate()

	ts := metav1.Now()

	oldMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "m1",
			Namespace:         "default",
			DeletionTimestamp: &ts,
			Finalizers:        []string{MappingCleanupFinalizer},
		},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}
	newMapping := oldMapping.DeepCopy()

	evt := event.UpdateEvent{ObjectOld: oldMapping, ObjectNew: newMapping}
	if pred.Update(evt) {
		t.Error("expected MappingEventPredicate to block when DeletionTimestamp values are the same (no effective change)")
	}
}

// ============================================================
// Pod predicate: table-driven comprehensive scenarios
// ============================================================

func TestPhase5_PodPredicate_UpdateScenarios(t *testing.T) {
	tests := []struct {
		name      string
		oldPod    *corev1.Pod
		newPod    *corev1.Pod
		wantAllow bool
	}{
		{
			name: "label_added",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			wantAllow: true,
		},
		{
			name: "label_removed",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			wantAllow: true,
		},
		{
			name: "label_value_changed",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "api"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			wantAllow: true,
		},
		{
			name: "ip_assigned_from_empty",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: ""},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			wantAllow: true,
		},
		{
			name: "ip_cleared",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: ""},
			},
			wantAllow: true,
		},
		{
			name: "conditions_change_only",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			},
			wantAllow: false,
		},
		{
			name: "annotation_change_only",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "web"}, Annotations: map[string]string{"note": "hi"}},
				Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
			},
			wantAllow: false,
		},
	}

	pred := PodEventPredicate()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := event.UpdateEvent{ObjectOld: tt.oldPod, ObjectNew: tt.newPod}
			got := pred.Update(evt)
			if got != tt.wantAllow {
				t.Errorf("PodEventPredicate.Update() = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

// ============================================================
// MapPodToMappings: pod with empty (non-nil) labels map → namespace-wide fallback
// ============================================================

func TestPhase5_MapPodToMappings_PodEmptyLabelsMap_FallbackEnqueuesAll(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "mapping-1", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	// Pod with empty map (non-nil) should trigger fallback
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ghost-pod",
			Namespace: "default",
			Labels:    map[string]string{},
		},
	}

	requests := r.MapPodToMappings(context.Background(), pod)
	if len(requests) == 0 {
		t.Fatal("expected MapPodToMappings to return all namespace mappings for pod with empty labels map, got 0")
	}

	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapping-1", Namespace: "default"}}
	found := false
	for _, req := range requests {
		if req == expectedReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected mapping-1 in results, got %v", requests)
	}
}

// ============================================================
// MapPodToMappings: mapping with empty MatchLabels (wildcard) matches any pod
// ============================================================

func TestPhase5_MapPodToMappings_WildcardSelector_MatchesAnyPod(t *testing.T) {
	scheme := newTestScheme(t)

	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "wildcard-mapping", Namespace: "default"},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{{
				PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{}},
				ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: testASGResourceID}},
			}},
		},
	}

	fakeK8s := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	r := &MappingReconciler{Client: fakeK8s, Scheme: scheme}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "any-pod",
			Namespace: "default",
			Labels:    map[string]string{"team": "infra"},
		},
	}

	requests := r.MapPodToMappings(context.Background(), pod)
	expectedReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "wildcard-mapping", Namespace: "default"}}
	found := false
	for _, req := range requests {
		if req == expectedReq {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected wildcard mapping to match any labeled pod, got %v", requests)
	}
}
