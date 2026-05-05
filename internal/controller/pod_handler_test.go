package controller

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/go-logr/zapr"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/workqueue"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func podHandlerTestScheme(t *testing.T) *runtime.Scheme {
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

func makeMapping(ns, name string, matchLabels map[string]string, asgIDs ...string) *v1alpha1.PodASGMapping {
	refs := make([]v1alpha1.ASGReference, len(asgIDs))
	for i, id := range asgIDs {
		refs[i] = v1alpha1.ASGReference{ResourceID: id}
	}
	return &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: matchLabels},
					ApplicationSecurityGroups: refs,
				},
			},
		},
	}
}

func makePod(ns, name string, labels map[string]string, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Status:     corev1.PodStatus{PodIP: ip},
	}
}

func handlerASGResourceID(sub, rg, name string) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/applicationSecurityGroups/" + name
}

func containsRequest(reqs []reconcile.Request, ns, name string) bool {
	for _, r := range reqs {
		if r.NamespacedName == (types.NamespacedName{Namespace: ns, Name: name}) {
			return true
		}
	}
	return false
}

func newRequestQueue() workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
}

func drainQueue(t *testing.T, q workqueue.TypedRateLimitingInterface[reconcile.Request]) []reconcile.Request {
	t.Helper()

	reqs := make([]reconcile.Request, 0, q.Len())
	for q.Len() > 0 {
		req, shutdown := q.Get()
		if shutdown {
			t.Fatal("queue unexpectedly shut down")
		}
		reqs = append(reqs, req)
		q.Done(req)
	}
	return reqs
}

type failingListReader struct{}

func (failingListReader) Get(context.Context, types.NamespacedName, ctrlclient.Object, ...ctrlclient.GetOption) error {
	return nil
}

func (failingListReader) List(context.Context, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
	return assertiveListError{}
}

type assertiveListError struct{}

func (assertiveListError) Error() string {
	return "simulated list failure"
}

// ---------------------------------------------------------------------------
// T5.1: TestPodToMappingEventHandler_Create_EnqueuesMatchingMappings
// Spec: Pod created matching a mapping → mapping is enqueued
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_Create_EnqueuesMatchingMappings(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reqs) == 0 {
		t.Fatal("T5.1: expected at least one reconcile request for matching mapping, got none")
	}
	if !containsRequest(reqs, ns, "web-mapping") {
		t.Errorf("T5.1: expected request for web-mapping, got %+v", reqs)
	}
}

// ---------------------------------------------------------------------------
// T5.2: TestPodToMappingEventHandler_Delete_EnqueuesPreviouslyMatchingMappings
// Spec: Pod deleted → mapping is enqueued
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_Delete_EnqueuesPreviouslyMatchingMappings(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reqs) == 0 {
		t.Fatal("T5.2: expected at least one reconcile request for deleted pod's mapping, got none")
	}
	if !containsRequest(reqs, ns, "web-mapping") {
		t.Errorf("T5.2: expected request for web-mapping, got %+v", reqs)
	}
}

// ---------------------------------------------------------------------------
// T5.4: TestPodToMappingEventHandler_Update_UnionOldAndNewMatches
// Spec: Update enqueues union(old matches, new matches)
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_Update_UnionOldAndNewMatches(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mappingA := makeMapping(ns, "mapping-a",
		map[string]string{"app": "alpha"},
		handlerASGResourceID("sub1", "rg1", "asg-alpha"))

	mappingB := makeMapping(ns, "mapping-b",
		map[string]string{"app": "beta"},
		handlerASGResourceID("sub1", "rg1", "asg-beta"))

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingA, mappingB).
		Build()

	oldPod := makePod(ns, "migrating-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
	newPod := makePod(ns, "migrating-pod", map[string]string{"app": "beta"}, "10.0.0.1")

	oldReqs, err1 := MatchingMappingsForPod(ctx, fakeReader, oldPod)
	if err1 != nil {
		t.Fatalf("unexpected error matching old pod: %v", err1)
	}
	newReqs, err2 := MatchingMappingsForPod(ctx, fakeReader, newPod)
	if err2 != nil {
		t.Fatalf("unexpected error matching new pod: %v", err2)
	}

	union := UnionRequests(oldReqs, newReqs)

	if len(union) < 2 {
		t.Fatalf("T5.4: expected at least 2 requests in union (mapping-a + mapping-b), got %d: %+v", len(union), union)
	}
	if !containsRequest(union, ns, "mapping-a") {
		t.Errorf("T5.4: expected union to contain mapping-a (old match)")
	}
	if !containsRequest(union, ns, "mapping-b") {
		t.Errorf("T5.4: expected union to contain mapping-b (new match)")
	}
}

// ---------------------------------------------------------------------------
// T5.4: TestPodToMappingEventHandler_Update_LabelChangeAway_EnqueuesOldMatch
// Spec: Pod label changes (no longer matches) → mapping enqueued
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_Update_LabelChangeAway_EnqueuesOldMatch(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	oldPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	newPod := makePod(ns, "pod-1", map[string]string{"app": "other"}, "10.0.0.1")

	oldReqs, _ := MatchingMappingsForPod(ctx, fakeReader, oldPod)
	newReqs, _ := MatchingMappingsForPod(ctx, fakeReader, newPod)

	union := UnionRequests(oldReqs, newReqs)

	if !containsRequest(union, ns, "web-mapping") {
		t.Error("T5.4: expected web-mapping to be enqueued when pod label changes away from matching")
	}
}

// ---------------------------------------------------------------------------
// TestMatchingMappingsForPod_MultipleRulesSameMapping_DedupesSingleRequest
// Multiple rules in the same mapping match the same pod → single request
// ---------------------------------------------------------------------------
func TestMatchingMappingsForPod_MultipleRulesSameMapping_DedupesSingleRequest(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	// Mapping with two rules that both match same pod labels
	mapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-rule", Namespace: ns},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: handlerASGResourceID("sub1", "rg1", "asg1")}},
				},
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: handlerASGResourceID("sub1", "rg1", "asg2")}},
				},
			},
		},
	}

	pod := makePod(ns, "web-pod", map[string]string{"app": "web"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reqs) != 1 {
		t.Errorf("expected exactly 1 deduplicated request for multi-rule mapping, got %d: %+v", len(reqs), reqs)
	}
}

func TestPodMappingCacheKey_OrderIndependentWithoutListResourceVersion(t *testing.T) {
	t.Run("same logical mappings in different orders", func(t *testing.T) {
		first := makeMapping("default", "first", map[string]string{"app": "web"}, handlerASGResourceID("sub1", "rg1", "asg1"))
		first.ResourceVersion = "7"
		first.Generation = 2

		second := makeMapping("default", "second", map[string]string{"app": "api"}, handlerASGResourceID("sub1", "rg1", "asg2"))
		second.ResourceVersion = "11"
		second.Generation = 4

		left := &v1alpha1.PodASGMappingList{
			Items: []v1alpha1.PodASGMapping{*first, *second},
		}
		right := &v1alpha1.PodASGMappingList{
			Items: []v1alpha1.PodASGMapping{*second, *first},
		}

		gotLeft := podMappingCacheKey(left)
		gotRight := podMappingCacheKey(right)

		if gotLeft != gotRight {
			t.Errorf("podMappingCacheKey() should be order-independent when list resourceVersion is empty, got %q and %q", gotLeft, gotRight)
		}
	})
}

// ---------------------------------------------------------------------------
// TestMatchingMappingsForPod_ListError_ReturnsError
// ---------------------------------------------------------------------------
func TestMatchingMappingsForPod_ListError_ReturnsError(t *testing.T) {
	ctx := context.Background()

	pod := makePod("default", "pod-1", map[string]string{"app": "web"}, "10.0.0.1")

	reqs, err := MatchingMappingsForPod(ctx, failingListReader{}, pod)
	if err == nil {
		t.Fatal("expected non-nil error when List fails")
	}
	if reqs != nil {
		t.Errorf("expected nil requests on list error, got %v", reqs)
	}
	if !strings.Contains(err.Error(), "listing PodASGMappings") {
		t.Errorf("error should mention listing PodASGMappings, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// T5.10: TestPodToMappingEventHandler_MultipleMappings_OnlyMatchingEnqueued
// Spec: Multiple PodASGMappings in same namespace, pod matches one → only that one enqueued
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_MultipleMappings_OnlyMatchingEnqueued(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mappingWeb := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg-web"))

	mappingDB := makeMapping(ns, "db-mapping",
		map[string]string{"app": "db"},
		handlerASGResourceID("sub1", "rg1", "asg-db"))

	pod := makePod(ns, "web-pod", map[string]string{"app": "web"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingWeb, mappingDB).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reqs) != 1 {
		t.Fatalf("T5.10: expected exactly 1 request (only matching mapping), got %d: %+v", len(reqs), reqs)
	}
	if !containsRequest(reqs, ns, "web-mapping") {
		t.Errorf("T5.10: expected request for web-mapping, got %+v", reqs)
	}
	if containsRequest(reqs, ns, "db-mapping") {
		t.Error("T5.10: db-mapping should NOT be enqueued for a web pod")
	}
}

// ---------------------------------------------------------------------------
// TestNewPodToMappingEventHandler_ReturnsNonNil
// Verify the constructor returns a usable handler
// ---------------------------------------------------------------------------
func TestNewPodToMappingEventHandler_ReturnsNonNil(t *testing.T) {
	scheme := podHandlerTestScheme(t)
	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		Build()

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandler(fakeReader, logger)
	if h == nil {
		t.Error("NewPodToMappingEventHandler returned nil, expected a valid handler.EventHandler")
	}
}

// ---------------------------------------------------------------------------
// TestPodToMappingEventHandler_EnqueueLifecycleEvents
// Verify create/update/delete events enqueue reconcile requests through the queue.
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_EnqueueLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mappingA := makeMapping(ns, "mapping-a",
		map[string]string{"app": "alpha"},
		handlerASGResourceID("sub1", "rg1", "asg-alpha"))
	mappingB := makeMapping(ns, "mapping-b",
		map[string]string{"app": "beta"},
		handlerASGResourceID("sub1", "rg1", "asg-beta"))

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mappingA, mappingB).
		Build()

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	handler, ok := NewPodToMappingEventHandler(fakeReader, logger).(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	t.Run("create enqueues matching mapping", func(t *testing.T) {
		queue := newRequestQueue()
		defer queue.ShutDown()

		pod := makePod(ns, "alpha-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
		handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, queue)

		got := drainQueue(t, queue)
		if len(got) != 1 {
			t.Fatalf("Create() enqueued %d requests, want 1", len(got))
		}
		if got[0].NamespacedName != (types.NamespacedName{Namespace: ns, Name: "mapping-a"}) {
			t.Errorf("Create() enqueued %v, want default/mapping-a", got[0].NamespacedName)
		}
	})

	t.Run("update enqueues union of old and new matches", func(t *testing.T) {
		queue := newRequestQueue()
		defer queue.ShutDown()

		oldPod := makePod(ns, "moving-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
		newPod := makePod(ns, "moving-pod", map[string]string{"app": "beta"}, "10.0.0.2")
		handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, queue)

		got := drainQueue(t, queue)
		if len(got) != 2 {
			t.Fatalf("Update() enqueued %d requests, want 2", len(got))
		}
		if !containsRequest(got, ns, "mapping-a") {
			t.Errorf("Update() queue missing old match mapping-a: %+v", got)
		}
		if !containsRequest(got, ns, "mapping-b") {
			t.Errorf("Update() queue missing new match mapping-b: %+v", got)
		}
	})

	t.Run("delete enqueues previously matching mapping", func(t *testing.T) {
		queue := newRequestQueue()
		defer queue.ShutDown()

		pod := makePod(ns, "beta-pod", map[string]string{"app": "beta"}, "10.0.0.2")
		handler.Delete(ctx, event.TypedDeleteEvent[ctrlclient.Object]{Object: pod}, queue)

		got := drainQueue(t, queue)
		if len(got) != 1 {
			t.Fatalf("Delete() enqueued %d requests, want 1", len(got))
		}
		if got[0].NamespacedName != (types.NamespacedName{Namespace: ns, Name: "mapping-b"}) {
			t.Errorf("Delete() enqueued %v, want default/mapping-b", got[0].NamespacedName)
		}
	})

	t.Run("generic does not enqueue", func(t *testing.T) {
		queue := newRequestQueue()
		defer queue.ShutDown()

		pod := makePod(ns, "ignored-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
		handler.Generic(ctx, event.TypedGenericEvent[ctrlclient.Object]{Object: pod}, queue)

		if queue.Len() != 0 {
			t.Errorf("Generic() enqueued %d requests, want 0", queue.Len())
		}
	})
}

// ---------------------------------------------------------------------------
// TestPodToMappingEventHandler_ListError_DoesNotEnqueue
// Verify list failures are handled without queueing stale requests.
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_ListError_DoesNotEnqueue(t *testing.T) {
	ctx := context.Background()
	logger := zapr.NewLogger(zaptest.NewLogger(t))
	handler, ok := NewPodToMappingEventHandler(failingListReader{}, logger).(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	tests := []struct {
		name string
		run  func(workqueue.TypedRateLimitingInterface[reconcile.Request])
	}{
		{
			name: "create",
			run: func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				pod := makePod("default", "alpha-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
				handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, queue)
			},
		},
		{
			name: "update",
			run: func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				oldPod := makePod("default", "alpha-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
				newPod := makePod("default", "alpha-pod", map[string]string{"app": "beta"}, "10.0.0.2")
				handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, queue)
			},
		},
		{
			name: "delete",
			run: func(queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				pod := makePod("default", "alpha-pod", map[string]string{"app": "alpha"}, "10.0.0.1")
				handler.Delete(ctx, event.TypedDeleteEvent[ctrlclient.Object]{Object: pod}, queue)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queue := newRequestQueue()
			defer queue.ShutDown()

			tc.run(queue)

			if queue.Len() != 0 {
				t.Errorf("%s queued %d requests after list failure, want 0", tc.name, queue.Len())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestUnionRequests_Deduplicates
// ---------------------------------------------------------------------------
func TestUnionRequests_Deduplicates(t *testing.T) {
	left := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "b"}},
	}
	right := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "b"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "c"}},
	}

	union := UnionRequests(left, right)

	if len(union) != 3 {
		t.Fatalf("expected 3 deduplicated requests, got %d: %+v", len(union), union)
	}
}

// Ensure unused imports are consumed.
var _ = event.CreateEvent{}

// ---------------------------------------------------------------------------
// TestUnionRequests_NilAndEmptyInputs
// Boundary: nil and empty slices produce valid empty results
// ---------------------------------------------------------------------------
func TestUnionRequests_NilAndEmptyInputs(t *testing.T) {
	tests := []struct {
		name  string
		left  []reconcile.Request
		right []reconcile.Request
		want  int
	}{
		{
			name:  "both nil",
			left:  nil,
			right: nil,
			want:  0,
		},
		{
			name:  "left nil right has items",
			left:  nil,
			right: []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}}},
			want:  1,
		},
		{
			name:  "left has items right nil",
			left:  []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}}},
			right: nil,
			want:  1,
		},
		{
			name:  "both empty",
			left:  []reconcile.Request{},
			right: []reconcile.Request{},
			want:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := UnionRequests(tc.left, tc.right)
			if len(got) != tc.want {
				t.Errorf("UnionRequests(%v, %v) = %d items, want %d", tc.left, tc.right, len(got), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestUnionRequests_DeterministicOrder
// Union results must be sorted deterministically for request stability
// ---------------------------------------------------------------------------
func TestUnionRequests_DeterministicOrder(t *testing.T) {
	left := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "z"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "a"}},
	}
	right := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "m"}},
	}

	union := UnionRequests(left, right)
	if len(union) != 3 {
		t.Fatalf("expected 3 items, got %d", len(union))
	}

	// Verify sorted order
	for i := 0; i < len(union)-1; i++ {
		if union[i].NamespacedName.String() > union[i+1].NamespacedName.String() {
			t.Errorf("UnionRequests result not sorted: %q > %q at indices %d, %d",
				union[i].NamespacedName.String(), union[i+1].NamespacedName.String(), i, i+1)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMatchingMappingsForPod_NoMatchingLabels_ReturnsEmpty
// Pod labels don't match any mapping selectors → empty result
// ---------------------------------------------------------------------------
func TestMatchingMappingsForPod_NoMatchingLabels_ReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	// Pod has different labels
	pod := makePod(ns, "db-pod", map[string]string{"app": "db"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for non-matching pod, got %d: %+v", len(reqs), reqs)
	}
}

// ---------------------------------------------------------------------------
// TestMatchingMappingsForPod_PodWithNoLabels_ReturnsEmpty
// Edge case: Pod has nil labels → should not match label selectors
// ---------------------------------------------------------------------------
func TestMatchingMappingsForPod_PodWithNoLabels_ReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	// Pod with nil labels
	pod := makePod(ns, "no-label-pod", nil, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for pod with nil labels, got %d: %+v", len(reqs), reqs)
	}
}

// ---------------------------------------------------------------------------
// TestMatchingMappingsForPod_CrossNamespaceIsolation
// Mappings in different namespace do NOT match pods in another namespace
// ---------------------------------------------------------------------------
func TestMatchingMappingsForPod_CrossNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)

	// Mapping in namespace "team-a"
	mapping := makeMapping("team-a", "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	// Pod in namespace "team-b" with matching labels
	pod := makePod("team-b", "web-pod", map[string]string{"app": "web"}, "10.0.0.1")

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mapping).
		Build()

	reqs, err := MatchingMappingsForPod(ctx, fakeReader, pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("expected 0 requests for pod in different namespace, got %d: %+v", len(reqs), reqs)
	}
}
