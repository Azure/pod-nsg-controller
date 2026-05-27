package controller

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	"github.com/pkg/errors"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// PodToMappingEventHandler enqueues PodASGMapping reconcile requests
// when pod events occur.
type PodToMappingEventHandler struct {
	Reader               client.Reader
	Logger               logr.Logger
	MinReconcileInterval time.Duration

	matcherCache *podMappingMatcherCache
}

// NewPodToMappingEventHandler creates a new event handler.
func NewPodToMappingEventHandler(reader client.Reader, logger logr.Logger, minReconcileInterval time.Duration) handler.EventHandler {
	return &PodToMappingEventHandler{
		Reader:               reader,
		Logger:               logger,
		MinReconcileInterval: minReconcileInterval,
		matcherCache: &podMappingMatcherCache{
			byNamespace: make(map[string]compiledPodMappingRequests),
		},
	}
}

// Create enqueues matches for a new pod.
func (h *PodToMappingEventHandler) Create(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		return
	}
	reqs, err := h.matchingMappingsForPod(ctx, pod)
	if err != nil {
		h.Logger.Error(err, "failed to match mappings for created pod",
			"pod", pod.Name, "podNamespace", pod.Namespace)
		return
	}
	for _, req := range reqs {
		h.enqueueRequest(q, req)
	}
}

// Update enqueues the union of matches for old and new pod.
func (h *PodToMappingEventHandler) Update(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
	newPod, ok2 := e.ObjectNew.(*corev1.Pod)
	if !ok1 || !ok2 {
		return
	}

	oldReqs, err1 := h.matchingMappingsForPod(ctx, oldPod)
	if err1 != nil {
		h.Logger.Error(err1, "failed to match mappings for old pod",
			"oldPodName", oldPod.Name, "podNamespace", oldPod.Namespace)
	}
	newReqs, err2 := h.matchingMappingsForPod(ctx, newPod)
	if err2 != nil {
		h.Logger.Error(err2, "failed to match mappings for new pod",
			"newPodName", newPod.Name, "podNamespace", newPod.Namespace)
	}

	union := UnionRequests(oldReqs, newReqs)
	for _, req := range union {
		h.enqueueRequest(q, req)
	}
}

// Delete enqueues matches for a deleted pod.
func (h *PodToMappingEventHandler) Delete(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, ok := e.Object.(*corev1.Pod)
	if !ok {
		return
	}
	reqs, err := h.matchingMappingsForPod(ctx, pod)
	if err != nil {
		h.Logger.Error(err, "failed to match mappings for deleted pod",
			"pod", pod.Name, "podNamespace", pod.Namespace)
		return
	}
	for _, req := range reqs {
		h.enqueueRequest(q, req)
	}
}

// enqueueRequest adds a reconcile request to the queue, using AddAfter when
// debounce is enabled to coalesce rapid pod events.
func (h *PodToMappingEventHandler) enqueueRequest(q workqueue.TypedRateLimitingInterface[reconcile.Request], req reconcile.Request) {
	if h.MinReconcileInterval > 0 {
		q.AddAfter(req, h.MinReconcileInterval)
	} else {
		q.Add(req)
	}
}

// Generic is a no-op.
func (h *PodToMappingEventHandler) Generic(_ context.Context, _ event.TypedGenericEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

type compiledPodMappingRequest struct {
	namespacedName types.NamespacedName
	selectors      []labels.Selector
}

type compiledPodMappingRequests struct {
	cacheKey string
	entries  []compiledPodMappingRequest
}

type podMappingMatcherCache struct {
	mu          sync.RWMutex
	byNamespace map[string]compiledPodMappingRequests
}

// MatchingMappingsForPod returns reconcile requests for all PodASGMappings
// whose selectors match the given pod.
func MatchingMappingsForPod(ctx context.Context, c client.Reader, pod *corev1.Pod) ([]reconcile.Request, error) {
	mappingList, err := listPodASGMappings(ctx, c, pod.Namespace)
	if err != nil {
		return nil, err
	}
	return matchingMappingsFromList(&mappingList, pod, nil), nil
}

func (h *PodToMappingEventHandler) matchingMappingsForPod(ctx context.Context, pod *corev1.Pod) ([]reconcile.Request, error) {
	mappingList, err := listPodASGMappings(ctx, h.Reader, pod.Namespace)
	if err != nil {
		return nil, err
	}
	return matchingMappingsFromList(&mappingList, pod, h.matcherCache), nil
}

func listPodASGMappings(ctx context.Context, c client.Reader, namespace string) (v1alpha1.PodASGMappingList, error) {
	var mappingList v1alpha1.PodASGMappingList
	if err := c.List(ctx, &mappingList, client.InNamespace(namespace)); err != nil {
		return v1alpha1.PodASGMappingList{}, errors.Wrapf(err, "listing PodASGMappings in namespace %s", namespace)
	}
	return mappingList, nil
}

func matchingMappingsFromList(mappingList *v1alpha1.PodASGMappingList, pod *corev1.Pod, cache *podMappingMatcherCache) []reconcile.Request {
	var compiled compiledPodMappingRequests
	if cache != nil {
		compiled = cache.getOrBuild(pod.Namespace, podMappingCacheKey(mappingList), mappingList.Items)
	} else {
		compiled = compiledPodMappingRequests{entries: compilePodMappingRequests(mappingList.Items)}
	}
	return matchingCompiledPodMappingRequests(compiled.entries, pod)
}

func (c *podMappingMatcherCache) getOrBuild(namespace, cacheKey string, mappings []v1alpha1.PodASGMapping) compiledPodMappingRequests {
	c.mu.RLock()
	cached, ok := c.byNamespace[namespace]
	c.mu.RUnlock()
	if ok && cached.cacheKey == cacheKey {
		return cached
	}

	compiled := compiledPodMappingRequests{
		cacheKey: cacheKey,
		entries:  compilePodMappingRequests(mappings),
	}

	c.mu.Lock()
	c.byNamespace[namespace] = compiled
	c.mu.Unlock()

	return compiled
}

func podMappingCacheKey(mappingList *v1alpha1.PodASGMappingList) string {
	if mappingList.ResourceVersion != "" {
		return mappingList.ResourceVersion
	}

	parts := make([]string, 0, len(mappingList.Items))
	for i := range mappingList.Items {
		m := &mappingList.Items[i]
		parts = append(parts,
			m.Namespace+"/"+m.Name+"@"+m.ResourceVersion+"#"+strconv.FormatInt(m.Generation, 10))
	}
	sort.Strings(parts)

	var key strings.Builder
	for _, part := range parts {
		key.WriteString(part)
		key.WriteByte(';')
	}
	return key.String()
}

func compilePodMappingRequests(mappings []v1alpha1.PodASGMapping) []compiledPodMappingRequest {
	entries := make([]compiledPodMappingRequest, 0, len(mappings))
	for i := range mappings {
		m := &mappings[i]
		entry := compiledPodMappingRequest{
			namespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name},
			selectors:      make([]labels.Selector, 0, len(m.Spec.Mappings)),
		}

		for _, rule := range m.Spec.Mappings {
			selector, err := model.CompileSelector(rule.PodSelector)
			if err != nil {
				continue
			}
			entry.selectors = append(entry.selectors, selector)
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].namespacedName.String() < entries[j].namespacedName.String()
	})
	return entries
}

func matchingCompiledPodMappingRequests(entries []compiledPodMappingRequest, pod *corev1.Pod) []reconcile.Request {
	reqs := make([]reconcile.Request, 0, len(entries))
	podLabels := labels.Set(pod.Labels)
	for i := range entries {
		entry := &entries[i]
		for _, selector := range entry.selectors {
			if selector.Matches(podLabels) {
				reqs = append(reqs, reconcile.Request{NamespacedName: entry.namespacedName})
				break
			}
		}
	}
	return reqs
}

// UnionRequests merges two request slices, deduplicating by NamespacedName.
func UnionRequests(left, right []reconcile.Request) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{})
	var result []reconcile.Request

	for _, r := range left {
		if _, ok := seen[r.NamespacedName]; !ok {
			seen[r.NamespacedName] = struct{}{}
			result = append(result, r)
		}
	}
	for _, r := range right {
		if _, ok := seen[r.NamespacedName]; !ok {
			seen[r.NamespacedName] = struct{}{}
			result = append(result, r)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].NamespacedName.String() < result[j].NamespacedName.String()
	})
	return result
}
