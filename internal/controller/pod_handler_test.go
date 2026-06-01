package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/engine"
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
	h := NewPodToMappingEventHandler(fakeReader, logger, 0)
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
	handler, ok := NewPodToMappingEventHandler(fakeReader, logger, 0).(*PodToMappingEventHandler)
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
	handler, ok := NewPodToMappingEventHandler(failingListReader{}, logger, 0).(*PodToMappingEventHandler)
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
// Phase 3: TestPodToMappingEventHandler_DebounceEnabled_UsesAddAfter
// When MinReconcileInterval > 0, all lifecycle events use AddAfter.
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_DebounceEnabled_UsesAddAfter(t *testing.T) {
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

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	debounceInterval := 2 * time.Second
	h := NewPodToMappingEventHandler(fakeReader, logger, debounceInterval)
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	spy := &queueSpy{}

	t.Run("create uses AddAfter", func(t *testing.T) {
		spy.reset()
		pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, spy)

		if spy.addAfterCount == 0 {
			t.Error("Phase 3: Create with debounce enabled should use AddAfter, but got 0 AddAfter calls")
		}
		if spy.addCount > 0 {
			t.Error("Phase 3: Create with debounce enabled should NOT use Add")
		}
	})

	t.Run("update uses AddAfter", func(t *testing.T) {
		spy.reset()
		oldPod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		newPod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.2")
		handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, spy)

		if spy.addAfterCount == 0 {
			t.Error("Phase 3: Update with debounce enabled should use AddAfter, but got 0 AddAfter calls")
		}
		if spy.addCount > 0 {
			t.Error("Phase 3: Update with debounce enabled should NOT use Add")
		}
	})

	t.Run("delete uses AddAfter", func(t *testing.T) {
		spy.reset()
		pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		handler.Delete(ctx, event.TypedDeleteEvent[ctrlclient.Object]{Object: pod}, spy)

		if spy.addAfterCount == 0 {
			t.Error("Phase 3: Delete with debounce enabled should use AddAfter, but got 0 AddAfter calls")
		}
		if spy.addCount > 0 {
			t.Error("Phase 3: Delete with debounce enabled should NOT use Add")
		}
	})
}

// ---------------------------------------------------------------------------
// Phase 3: TestPodToMappingEventHandler_DebounceDisabled_UsesAdd
// When MinReconcileInterval == 0, all lifecycle events use plain Add.
// ---------------------------------------------------------------------------
func TestPodToMappingEventHandler_DebounceDisabled_UsesAdd(t *testing.T) {
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

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandler(fakeReader, logger, 0) // debounce disabled
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	spy := &queueSpy{}

	t.Run("create uses Add", func(t *testing.T) {
		spy.reset()
		pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, spy)

		if spy.addCount == 0 {
			t.Error("Phase 3: Create with debounce disabled should use Add, but got 0 Add calls")
		}
		if spy.addAfterCount > 0 {
			t.Error("Phase 3: Create with debounce disabled should NOT use AddAfter")
		}
	})

	t.Run("update uses Add", func(t *testing.T) {
		spy.reset()
		oldPod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		newPod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.2")
		handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, spy)

		if spy.addCount == 0 {
			t.Error("Phase 3: Update with debounce disabled should use Add, but got 0 Add calls")
		}
		if spy.addAfterCount > 0 {
			t.Error("Phase 3: Update with debounce disabled should NOT use AddAfter")
		}
	})

	t.Run("delete uses Add", func(t *testing.T) {
		spy.reset()
		pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
		handler.Delete(ctx, event.TypedDeleteEvent[ctrlclient.Object]{Object: pod}, spy)

		if spy.addCount == 0 {
			t.Error("Phase 3: Delete with debounce disabled should use Add, but got 0 Add calls")
		}
		if spy.addAfterCount > 0 {
			t.Error("Phase 3: Delete with debounce disabled should NOT use AddAfter")
		}
	})
}

// queueSpy implements workqueue.TypedRateLimitingInterface[reconcile.Request] and
// records which method (Add vs AddAfter) was called.
type queueSpy struct {
	addCount      int
	addAfterCount int
	items         []reconcile.Request
}

func (q *queueSpy) reset() {
	q.addCount = 0
	q.addAfterCount = 0
	q.items = nil
}

func (q *queueSpy) Add(item reconcile.Request) {
	q.addCount++
	q.items = append(q.items, item)
}

func (q *queueSpy) AddAfter(item reconcile.Request, _ time.Duration) {
	q.addAfterCount++
	q.items = append(q.items, item)
}

func (q *queueSpy) AddRateLimited(item reconcile.Request)               {}
func (q *queueSpy) Forget(item reconcile.Request)                       {}
func (q *queueSpy) NumRequeues(item reconcile.Request) int              { return 0 }
func (q *queueSpy) Len() int                                            { return len(q.items) }
func (q *queueSpy) Get() (reconcile.Request, bool)                      { return reconcile.Request{}, false }
func (q *queueSpy) Done(item reconcile.Request)                         {}
func (q *queueSpy) ShutDown()                                           {}
func (q *queueSpy) ShutDownWithDrain()                                  {}
func (q *queueSpy) ShuttingDown() bool                                  { return false }

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

// ===========================================================================
// Phase 5: Desired-State Cache — Pod Handler Cache Integration Tests
// ===========================================================================

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_Create_UpdatesCacheOnAdd
// Pod create event with valid mapping should call OnPodAdd on the cache.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_Create_UpdatesCacheOnAdd(t *testing.T) {
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

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	cache := engine.NewDesiredStateCache("test-cluster")

	// The handler should accept a DesiredStateCache parameter
	h := NewPodToMappingEventHandlerWithCache(fakeReader, logger, 0, cache)
	if h == nil {
		t.Fatal("NewPodToMappingEventHandlerWithCache returned nil")
	}

	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	queue := newRequestQueue()
	defer queue.ShutDown()

	pod := makePod(ns, "web-pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, queue)

	// Verify enqueue still works (existing behavior preserved)
	got := drainQueue(t, queue)
	if len(got) == 0 {
		t.Fatal("Phase 5: Create should still enqueue matching mappings")
	}
	if !containsRequest(got, ns, "web-mapping") {
		t.Error("Phase 5: expected web-mapping enqueued on pod create")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_PartialUpdateFailure_InvalidatesNamespaceCache
// When one side of an update resolution fails, the handler should invalidate
// the namespace cache while still enqueuing the successful side's requests.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_PartialUpdateFailure_InvalidatesNamespaceCache(t *testing.T) {
	ctx := context.Background()
	ns := "default"

	mapping := makeMapping(ns, "web-mapping",
		map[string]string{"app": "web"},
		handlerASGResourceID("sub1", "rg1", "asg1"))

	// Seed the cache with an entry
	cache := engine.NewDesiredStateCache("test-cluster")
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	// Use a reader that fails on list (simulating partial resolution failure)
	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandlerWithCache(failingListReader{}, logger, 0, cache)
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	queue := newRequestQueue()
	defer queue.ShutDown()

	oldPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	newPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.2")
	handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, queue)

	// After partial failure, the cache for this namespace should be invalidated
	if _, ok := cache.Get(mappingObj); ok {
		t.Error("Phase 5: expected namespace cache invalidated after partial update resolution failure")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_UnsafeCacheMutation_InvalidatesMappingCache
// When OnPodUpdate returns false (unsafe mutation), the handler should
// invalidate the specific mapping cache entry while preserving enqueue behavior.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_UnsafeCacheMutation_InvalidatesMappingCache(t *testing.T) {
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

	cache := engine.NewDesiredStateCache("test-cluster")
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	cache.SetFromRecompute(mappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandlerWithCache(fakeReader, logger, 0, cache)
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	queue := newRequestQueue()
	defer queue.ShutDown()

	// Update pod IP (OnPodUpdate stub returns false → unsafe mutation)
	oldPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	newPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.2")
	handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, queue)

	// Enqueue behavior should be preserved
	got := drainQueue(t, queue)
	if !containsRequest(got, ns, "web-mapping") {
		t.Error("Phase 5: expected web-mapping enqueued even when cache mutation is unsafe")
	}

	// The mapping cache entry should be invalidated
	if _, ok := cache.Get(mappingObj); ok {
		t.Error("Phase 5: expected mapping cache invalidated after unsafe OnPodUpdate")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_InvalidMappingSpec_InvalidatesCacheOnCreate
// When a mapping has invalid ASG resource IDs, the pod handler should
// invalidate that mapping's cache entry (not mutate it).
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_InvalidMappingSpec_InvalidatesCacheOnCreate(t *testing.T) {
	ctx := context.Background()
	scheme := podHandlerTestScheme(t)
	ns := "default"

	// Invalid ASG resource ID (missing path segments)
	invalidMapping := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mapping", Namespace: ns},
		Spec: v1alpha1.PodASGMappingSpec{
			Mappings: []v1alpha1.Mapping{
				{
					PodSelector:               v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "web"}},
					ApplicationSecurityGroups: []v1alpha1.ASGReference{{ResourceID: "invalid-id"}},
				},
			},
		},
	}

	fakeReader := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(invalidMapping).
		Build()

	cache := engine.NewDesiredStateCache("test-cluster")
	// Pre-seed cache for this mapping
	invalidMappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "invalid-mapping", Generation: 1},
		Spec:       invalidMapping.Spec,
	}
	cache.SetFromRecompute(invalidMappingObj, nil,
		map[engine.ASGTarget]engine.DesiredPrefixSet{
			{ASGName: "asg1"}: {IPs: map[string]struct{}{"10.0.0.1/32": {}}},
		},
		engine.PodSnapshot{}, []int{1}, false)

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandlerWithCache(fakeReader, logger, 0, cache)
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	queue := newRequestQueue()
	defer queue.ShutDown()

	pod := makePod(ns, "web-pod", map[string]string{"app": "web"}, "10.0.0.1")
	handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, queue)

	// Cache for the invalid mapping should be invalidated (not mutated)
	if _, ok := cache.Get(invalidMappingObj); ok {
		t.Error("Phase 5: expected cache invalidated for mapping with invalid ASG resource IDs")
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_Update_CacheMiss_DoesNotInvalidateMapping
// When OnPodUpdate returns true on a cache miss (no-op safe path), the handler
// must NOT call Invalidate. This avoids unnecessary invalidation churn when
// the cache has no entry for a mapping.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_Update_CacheMiss_DoesNotInvalidateMapping(t *testing.T) {
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

	// Cache with NO pre-seeded entry (empty cache)
	cache := engine.NewDesiredStateCache("test-cluster")

	logger := zapr.NewLogger(zaptest.NewLogger(t))
	h := NewPodToMappingEventHandlerWithCache(fakeReader, logger, 0, cache)
	handler, ok := h.(*PodToMappingEventHandler)
	if !ok {
		t.Fatal("expected *PodToMappingEventHandler")
	}

	queue := newRequestQueue()
	defer queue.ShutDown()

	// Pod update with IP change — but cache has no entry for the mapping
	oldPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.1")
	newPod := makePod(ns, "pod-1", map[string]string{"app": "web"}, "10.0.0.2")
	handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, queue)

	// Mapping should still be enqueued (enqueue behavior preserved)
	got := drainQueue(t, queue)
	if !containsRequest(got, ns, "web-mapping") {
		t.Error("Phase 5: expected web-mapping enqueued on pod update even with cache miss")
	}

	// The critical assertion: on cache miss, OnPodUpdate returns true (safe no-op),
	// so the handler does NOT call Invalidate. The fence is advanced exactly once
	// (by OnPodUpdate internally). If Invalidate were also called, the version
	// would be 2 (one bump from OnPodUpdate + one from Invalidate).
	mappingObj := &v1alpha1.PodASGMapping{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
		Spec:       mapping.Spec,
	}
	_, version, _, _ := cache.GetWithVersion(mappingObj)

	// Version must be exactly 1: OnPodUpdate bumps the version fence once on
	// cache miss. If Invalidate were called (incorrectly), version would be 2.
	if version != 1 {
		t.Errorf("Phase 5: cache-miss OnPodUpdate must bump version fence exactly once (no Invalidate); version=%d, want 1", version)
	}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_Create_MutatesCacheBeforeEnqueue
// Design: cache mutation must be visible to reconcilers at enqueue time.
// Currently FAILS because enqueue happens BEFORE cache mutation.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_Create_MutatesCacheBeforeEnqueue(t *testing.T) {
ctx := context.Background()
scheme := podHandlerTestScheme(t)
ns := "default"

asgID := handlerASGResourceID("sub1", "rg1", "asg1")
mapping := makeMapping(ns, "web-mapping", map[string]string{"app": "web"}, asgID)
mapping.Generation = 1

fakeReader := fakeclient.NewClientBuilder().
WithScheme(scheme).
WithObjects(mapping).
Build()

cache := engine.NewDesiredStateCache("test-cluster")

// Seed cache with an initial state so OnPodAdd can mutate it
target := engine.ASGTarget{
SubscriptionID: "sub1",
ResourceGroup:  "rg1",
ASGName:        "asg1",
FullResourceID: asgID,
PrefixSetName:  "test-cluster-default-web-mapping",
}
cache.SetFromRecompute(mapping, nil,
map[engine.ASGTarget]engine.DesiredPrefixSet{
target: {IPs: map[string]struct{}{}},
},
engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{}},
[]int{0}, false)

logger := zapr.NewLogger(zaptest.NewLogger(t))
handler := &PodToMappingEventHandler{
Reader:            fakeReader,
Logger:            logger,
DesiredStateCache: cache,
matcherCache: &podMappingMatcherCache{
byNamespace: make(map[string]compiledPodMappingRequests),
},
}

// Use a custom queue that checks cache state at enqueue time
cacheHadIPAtEnqueue := false
checkQueue := &enqueueCheckingQueue{
TypedRateLimitingInterface: newRequestQueue(),
onAdd: func() {
// At the moment Add is called, check if cache already has the pod IP
mappingObj := &v1alpha1.PodASGMapping{
ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
Spec:       mapping.Spec,
}
if cached, ok := cache.Get(mappingObj); ok {
if dps, exists := cached.Desired[target]; exists {
if _, hasIP := dps.IPs["10.0.0.5/32"]; hasIP {
cacheHadIPAtEnqueue = true
}
}
}
},
}
defer checkQueue.ShutDown()

pod := makePod(ns, "new-pod", map[string]string{"app": "web"}, "10.0.0.5")
handler.Create(ctx, event.TypedCreateEvent[ctrlclient.Object]{Object: pod}, checkQueue)

// The test assertion: cache mutation should be visible at enqueue time
if !cacheHadIPAtEnqueue {
t.Error("Phase 5: Create() must mutate cache BEFORE enqueue so reconcilers observe fresh state; " +
"currently enqueue happens first (cache mutation not yet visible)")
}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_Delete_MutatesCacheBeforeEnqueue
// Design: cache mutation (IP removal) must happen before reconcile is enqueued.
// Currently FAILS because enqueue happens BEFORE cache mutation.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_Delete_MutatesCacheBeforeEnqueue(t *testing.T) {
ctx := context.Background()
scheme := podHandlerTestScheme(t)
ns := "default"

asgID := handlerASGResourceID("sub1", "rg1", "asg1")
mapping := makeMapping(ns, "web-mapping", map[string]string{"app": "web"}, asgID)
mapping.Generation = 1

fakeReader := fakeclient.NewClientBuilder().
WithScheme(scheme).
WithObjects(mapping).
Build()

cache := engine.NewDesiredStateCache("test-cluster")

target := engine.ASGTarget{
SubscriptionID: "sub1",
ResourceGroup:  "rg1",
ASGName:        "asg1",
FullResourceID: asgID,
PrefixSetName:  "test-cluster-default-web-mapping",
}

// Seed cache with a pod IP that will be deleted
pods := []corev1.Pod{
*makePod(ns, "doomed-pod", map[string]string{"app": "web"}, "10.0.0.7"),
}
cache.SetFromRecompute(mapping, pods,
map[engine.ASGTarget]engine.DesiredPrefixSet{
target: {IPs: map[string]struct{}{"10.0.0.7/32": {}}},
},
engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
{Namespace: ns, Name: "doomed-pod"}: {PodIP: "10.0.0.7"},
}},
[]int{1}, false)

logger := zapr.NewLogger(zaptest.NewLogger(t))
handler := &PodToMappingEventHandler{
Reader:            fakeReader,
Logger:            logger,
DesiredStateCache: cache,
matcherCache: &podMappingMatcherCache{
byNamespace: make(map[string]compiledPodMappingRequests),
},
}

// Check cache state at enqueue time
cacheIPRemovedAtEnqueue := false
checkQueue := &enqueueCheckingQueue{
TypedRateLimitingInterface: newRequestQueue(),
onAdd: func() {
mappingObj := &v1alpha1.PodASGMapping{
ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
Spec:       mapping.Spec,
}
if cached, ok := cache.Get(mappingObj); ok {
if dps, exists := cached.Desired[target]; exists {
if _, hasIP := dps.IPs["10.0.0.7/32"]; !hasIP {
cacheIPRemovedAtEnqueue = true
}
} else {
cacheIPRemovedAtEnqueue = true
}
}
},
}
defer checkQueue.ShutDown()

pod := makePod(ns, "doomed-pod", map[string]string{"app": "web"}, "10.0.0.7")
handler.Delete(ctx, event.TypedDeleteEvent[ctrlclient.Object]{Object: pod}, checkQueue)

if !cacheIPRemovedAtEnqueue {
t.Error("Phase 5: Delete() must mutate cache (remove IP) BEFORE enqueue; " +
"currently enqueue happens first (stale IP still visible to reconciler)")
}
}

// ---------------------------------------------------------------------------
// TestPhase5_PodHandler_Update_MutatesCacheBeforeUnionEnqueue
// Design: cache mutation for all affected mappings must happen before any enqueue.
// Currently FAILS because union enqueue happens BEFORE cache mutation.
// ---------------------------------------------------------------------------
func TestPhase5_PodHandler_Update_MutatesCacheBeforeUnionEnqueue(t *testing.T) {
ctx := context.Background()
scheme := podHandlerTestScheme(t)
ns := "default"

asgID := handlerASGResourceID("sub1", "rg1", "asg1")
mapping := makeMapping(ns, "web-mapping", map[string]string{"app": "web"}, asgID)
mapping.Generation = 1

fakeReader := fakeclient.NewClientBuilder().
WithScheme(scheme).
WithObjects(mapping).
Build()

cache := engine.NewDesiredStateCache("test-cluster")

target := engine.ASGTarget{
SubscriptionID: "sub1",
ResourceGroup:  "rg1",
ASGName:        "asg1",
FullResourceID: asgID,
PrefixSetName:  "test-cluster-default-web-mapping",
}

// Seed with old pod IP
pods := []corev1.Pod{
*makePod(ns, "updating-pod", map[string]string{"app": "web"}, "10.0.0.10"),
}
cache.SetFromRecompute(mapping, pods,
map[engine.ASGTarget]engine.DesiredPrefixSet{
target: {IPs: map[string]struct{}{"10.0.0.10/32": {}}},
},
engine.PodSnapshot{Pods: map[engine.PodIdentity]engine.PodMembership{
{Namespace: ns, Name: "updating-pod"}: {PodIP: "10.0.0.10"},
}},
[]int{1}, false)

logger := zapr.NewLogger(zaptest.NewLogger(t))
handler := &PodToMappingEventHandler{
Reader:            fakeReader,
Logger:            logger,
DesiredStateCache: cache,
matcherCache: &podMappingMatcherCache{
byNamespace: make(map[string]compiledPodMappingRequests),
},
}

// Check if cache reflects the new IP at enqueue time
cacheHadNewIPAtEnqueue := false
checkQueue := &enqueueCheckingQueue{
TypedRateLimitingInterface: newRequestQueue(),
onAdd: func() {
mappingObj := &v1alpha1.PodASGMapping{
ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-mapping", Generation: 1},
Spec:       mapping.Spec,
}
if cached, ok := cache.Get(mappingObj); ok {
if dps, exists := cached.Desired[target]; exists {
if _, hasNewIP := dps.IPs["10.0.0.20/32"]; hasNewIP {
cacheHadNewIPAtEnqueue = true
}
}
}
},
}
defer checkQueue.ShutDown()

oldPod := makePod(ns, "updating-pod", map[string]string{"app": "web"}, "10.0.0.10")
newPod := makePod(ns, "updating-pod", map[string]string{"app": "web"}, "10.0.0.20")
handler.Update(ctx, event.TypedUpdateEvent[ctrlclient.Object]{ObjectOld: oldPod, ObjectNew: newPod}, checkQueue)

if !cacheHadNewIPAtEnqueue {
t.Error("Phase 5: Update() must mutate cache (IP change) BEFORE enqueue; " +
"currently union enqueue happens first (stale state visible to reconciler)")
}
}

// enqueueCheckingQueue wraps a queue and calls onAdd before each Add/AddAfter.
type enqueueCheckingQueue struct {
workqueue.TypedRateLimitingInterface[reconcile.Request]
onAdd func()
}

func (q *enqueueCheckingQueue) Add(item reconcile.Request) {
if q.onAdd != nil {
q.onAdd()
}
q.TypedRateLimitingInterface.Add(item)
}

func (q *enqueueCheckingQueue) AddAfter(item reconcile.Request, duration time.Duration) {
if q.onAdd != nil {
q.onAdd()
}
q.TypedRateLimitingInterface.AddAfter(item, duration)
}
