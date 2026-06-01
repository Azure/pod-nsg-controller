package engine

import (
	"sync"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// CachedDesiredState holds the cached output from a desired-state computation
// for a single PodASGMapping.
type CachedDesiredState struct {
	Desired            map[ASGTarget]DesiredPrefixSet
	Snapshot           PodSnapshot
	MatchedPodsByIndex []int
	HasPendingIPPods   bool
}

// DesiredStateCache is a concurrency-safe mapping-scoped cache that stores
// desired state plus status-driving derived data.
type DesiredStateCache struct {
	mu          sync.RWMutex
	clusterName string
	entries     map[cacheKey]*cacheEntry

	// keyVersions is a per-key version floor bumped by pod events and invalidation.
	keyVersions map[types.NamespacedName]uint64
	// lifecycleEpoch is a terminal lifecycle fence bumped on Delete; survives entry removal.
	lifecycleEpoch map[types.NamespacedName]uint64
	// knownGenerations tracks which generations have had entries committed for a key.
	knownGenerations map[types.NamespacedName]map[int64]struct{}
	// latestGeneration tracks the highest generation committed for a key.
	latestGeneration map[types.NamespacedName]int64
}

type cacheKey struct {
	key        types.NamespacedName
	generation int64
}

type cacheEntry struct {
	state   CachedDesiredState
	mapping *v1alpha1.PodASGMapping
	// podRules tracks which rule indices each pod contributes to,
	// enabling per-rule scoped IP removal on delete/update.
	podRules map[PodIdentity]map[int]struct{}
	// pendingIPCount tracks the number of matched pods without an IP.
	// This is used instead of scanning Snapshot.Pods because the snapshot
	// from a full recompute may exclude pods without IPs.
	pendingIPCount int
}

// NewDesiredStateCache creates a new desired-state cache for the given cluster.
func NewDesiredStateCache(clusterName string) *DesiredStateCache {
	return &DesiredStateCache{
		clusterName:      clusterName,
		entries:          make(map[cacheKey]*cacheEntry),
		keyVersions:      make(map[types.NamespacedName]uint64),
		lifecycleEpoch:   make(map[types.NamespacedName]uint64),
		knownGenerations: make(map[types.NamespacedName]map[int64]struct{}),
		latestGeneration: make(map[types.NamespacedName]int64),
	}
}

// Get returns the cached desired state for a mapping. Returns false if no
// cached entry exists for this mapping+generation. When the caller provides
// a non-empty UID, the cached entry's UID must match to prevent stale reads
// after a delete/recreate with the same namespaced name and generation.
func (c *DesiredStateCache) Get(mapping *v1alpha1.PodASGMapping) (CachedDesiredState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := cacheKey{
		key:        types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name},
		generation: mapping.Generation,
	}
	entry, ok := c.entries[key]
	if !ok {
		return CachedDesiredState{}, false
	}
	if mapping.UID != "" && entry.mapping != nil && entry.mapping.UID != mapping.UID {
		return CachedDesiredState{}, false
	}
	// Return deep copy to prevent mutation
	return deepCopyCachedState(entry.state), true
}

// SetFromRecompute stores the result of a full desired-state recomputation.
func (c *DesiredStateCache) SetFromRecompute(
	mapping *v1alpha1.PodASGMapping,
	pods []corev1.Pod,
	desired map[ASGTarget]DesiredPrefixSet,
	snapshot PodSnapshot,
	matchedPodsByIndex []int,
	hasPendingIPPods bool,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setFromRecomputeLocked(mapping, pods, desired, snapshot, matchedPodsByIndex, hasPendingIPPods)
}

// setFromRecomputeLocked is the lock-held core of SetFromRecompute.
func (c *DesiredStateCache) setFromRecomputeLocked(
	mapping *v1alpha1.PodASGMapping,
	pods []corev1.Pod,
	desired map[ASGTarget]DesiredPrefixSet,
	snapshot PodSnapshot,
	matchedPodsByIndex []int,
	hasPendingIPPods bool,
) {
	key := cacheKey{
		key:        types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name},
		generation: mapping.Generation,
	}

	// Build per-pod rule contribution tracking
	podRules := make(map[PodIdentity]map[int]struct{})
	for pi := range pods {
		pod := &pods[pi]
		podID := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}
		matchedIndices := matchPodToRuleIndices(mapping, labels.Set(pod.Labels))
		for _, i := range matchedIndices {
			if podRules[podID] == nil {
				podRules[podID] = make(map[int]struct{})
			}
			podRules[podID][i] = struct{}{}
		}
	}

	// Deep copy caller-owned data to prevent external mutation from
	// corrupting cached state (the reconciler mutates desired after caching).
	copiedDesired := make(map[ASGTarget]DesiredPrefixSet, len(desired))
	for k, v := range desired {
		ips := make(map[string]struct{}, len(v.IPs))
		for ip := range v.IPs {
			ips[ip] = struct{}{}
		}
		copiedDesired[k] = DesiredPrefixSet{IPs: ips}
	}

	copiedSnapshot := PodSnapshot{Pods: make(map[PodIdentity]PodMembership, len(snapshot.Pods))}
	for k, v := range snapshot.Pods {
		copiedSnapshot.Pods[k] = v
	}

	copiedMatchedPods := make([]int, len(matchedPodsByIndex))
	copy(copiedMatchedPods, matchedPodsByIndex)

	// Count pending pods: pods tracked in podRules but absent from the
	// snapshot (ComputeDesiredStateWithSnapshot excludes pods without IPs).
	// Pending-pod tracking stays internal to cacheEntry.pendingIPCount so
	// the public Snapshot remains parity-compatible with ComputeDesiredStateWithSnapshot.
	pendingCount := 0
	for podID := range podRules {
		if _, inSnapshot := copiedSnapshot.Pods[podID]; !inSnapshot {
			pendingCount++
		}
	}
	// If caller indicates pending pods but none are tracked in podRules
	// (e.g., pods slice was nil), conservatively count at least 1.
	if hasPendingIPPods && pendingCount == 0 {
		pendingCount = 1
	}

	c.entries[key] = &cacheEntry{
		state: CachedDesiredState{
			Desired:            copiedDesired,
			Snapshot:           copiedSnapshot,
			MatchedPodsByIndex: copiedMatchedPods,
			HasPendingIPPods:   hasPendingIPPods,
		},
		mapping:        mapping,
		podRules:       podRules,
		pendingIPCount: pendingCount,
	}
}

// OnPodAdd incrementally updates the cache for a newly created pod.
func (c *DesiredStateCache) OnPodAdd(mapping *v1alpha1.PodASGMapping, pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}
	c.keyVersions[nsName]++

	key := cacheKey{
		key:        nsName,
		generation: mapping.Generation,
	}
	entry, ok := c.entries[key]
	if !ok {
		return
	}

	hasCIDR := pod.Status.PodIP != ""
	var cidr string
	if hasCIDR {
		cidr = toCIDR(pod.Status.PodIP)
	}

	prefixSetName := model.OwnershipKey(c.clusterName, mapping.Namespace, mapping.Name)
	matched := false
	podID := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}

	// Evaluate each rule's selector and only add IP to matching targets
	matchedIndices := matchPodToRuleIndices(mapping, labels.Set(pod.Labels))
	for _, i := range matchedIndices {
		matched = true
		rule := mapping.Spec.Mappings[i]

		// Track this pod's contribution to this rule
		if entry.podRules == nil {
			entry.podRules = make(map[PodIdentity]map[int]struct{})
		}
		if entry.podRules[podID] == nil {
			entry.podRules[podID] = make(map[int]struct{})
		}
		entry.podRules[podID][i] = struct{}{}

		// Only add CIDR if pod has an IP
		if hasCIDR {
			targets := resolveRuleTargets(rule, prefixSetName)
			for _, target := range targets {
				dps, exists := entry.state.Desired[target]
				if !exists {
					dps = DesiredPrefixSet{IPs: make(map[string]struct{})}
				}
				if dps.IPs == nil {
					dps.IPs = make(map[string]struct{})
				}
				dps.IPs[cidr] = struct{}{}
				entry.state.Desired[target] = dps
			}
		}

		// Increment MatchedPodsByIndex for this rule regardless of IP
		if i < len(entry.state.MatchedPodsByIndex) {
			entry.state.MatchedPodsByIndex[i]++
		}
	}

	// Update snapshot: only IP-bearing pods are stored in the public Snapshot
	// to maintain parity with ComputeDesiredStateWithSnapshot(). Pending-pod
	// tracking uses the internal pendingIPCount counter.
	if matched {
		if hasCIDR {
			if entry.state.Snapshot.Pods == nil {
				entry.state.Snapshot.Pods = make(map[PodIdentity]PodMembership)
			}
			entry.state.Snapshot.Pods[podID] = PodMembership{PodIP: pod.Status.PodIP}
		} else {
			entry.pendingIPCount++
			entry.state.HasPendingIPPods = true
		}
	}
}

// OnPodDelete incrementally updates the cache for a deleted pod.
func (c *DesiredStateCache) OnPodDelete(mapping *v1alpha1.PodASGMapping, pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}
	c.keyVersions[nsName]++

	key := cacheKey{
		key:        nsName,
		generation: mapping.Generation,
	}
	entry, ok := c.entries[key]
	if !ok {
		return
	}

	hasCIDR := pod.Status.PodIP != ""
	var cidr string
	if hasCIDR {
		cidr = toCIDR(pod.Status.PodIP)
	}

	// Determine pending status: a pod is pending if tracked in podRules but
	// absent from the snapshot (snapshot only holds IP-bearing pods).
	podID := PodIdentity{Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID)}
	_, inPodRules := entry.podRules[podID]
	_, inSnapshot := entry.state.Snapshot.Pods[podID]
	wasPending := inPodRules && !inSnapshot
	if inSnapshot {
		delete(entry.state.Snapshot.Pods, podID)
	}

	prefixSetName := model.OwnershipKey(c.clusterName, mapping.Namespace, mapping.Name)

	// Determine which rules this pod matched (from podRules tracking or re-evaluate)
	matchedRules := entry.podRules[podID]

	// Per-rule scoped IP removal and MatchedPodsByIndex update
	// Use both tracked podRules and live selector evaluation to ensure correctness
	// even if tracking was incomplete.
	liveMatchedIndices := matchPodToRuleIndices(mapping, labels.Set(pod.Labels))
	liveMatchedSet := make(map[int]struct{}, len(liveMatchedIndices))
	for _, idx := range liveMatchedIndices {
		liveMatchedSet[idx] = struct{}{}
	}

	for i, rule := range mapping.Spec.Mappings {
		// Check if this pod contributed to this rule
		_, trackedMatch := matchedRules[i]
		_, selectorMatch := liveMatchedSet[i]
		if !trackedMatch && !selectorMatch {
			continue
		}

		// Decrement MatchedPodsByIndex
		if i < len(entry.state.MatchedPodsByIndex) && entry.state.MatchedPodsByIndex[i] > 0 {
			entry.state.MatchedPodsByIndex[i]--
		}

		// Only attempt IP removal if the pod had an IP
		if !hasCIDR {
			continue
		}

		// Per-rule check: does any OTHER pod that also contributes to this rule
		// still have the same IP?
		hasOtherContributor := false
		for otherPodID, otherRules := range entry.podRules {
			if otherPodID == podID {
				continue
			}
			if _, matchesThisRule := otherRules[i]; !matchesThisRule {
				continue
			}
			membership, inSnapshot := entry.state.Snapshot.Pods[otherPodID]
			if inSnapshot && membership.PodIP != "" && toCIDR(membership.PodIP) == cidr {
				hasOtherContributor = true
				break
			}
		}

		// Only remove IP from this rule's targets if no other contributor
		if !hasOtherContributor {
			targets := resolveRuleTargets(rule, prefixSetName)
			for _, target := range targets {
				if dps, exists := entry.state.Desired[target]; exists {
					delete(dps.IPs, cidr)
					entry.state.Desired[target] = dps
				}
			}
		}
	}

	// Remove pod from podRules tracking
	delete(entry.podRules, podID)

	// Update pending count using tracked counter instead of scanning snapshot.
	// Scanning snapshot alone is incorrect because the snapshot from a full
	// recompute may not include pods that had no IP at seed time.
	if wasPending && entry.pendingIPCount > 0 {
		entry.pendingIPCount--
	}
	entry.state.HasPendingIPPods = entry.pendingIPCount > 0
}

// OnPodUpdate incrementally updates the cache for a pod update. Returns false
// when the mutation cannot be safely applied and the caller must invalidate.
func (c *DesiredStateCache) OnPodUpdate(mapping *v1alpha1.PodASGMapping, oldPod, newPod *corev1.Pod) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}

	key := cacheKey{
		key:        nsName,
		generation: mapping.Generation,
	}
	entry, ok := c.entries[key]
	if !ok {
		// If entries exist for a different generation, bump version and return
		// false so the caller invalidates stale generation data.
		for k := range c.entries {
			if k.key == nsName {
				c.keyVersions[nsName]++
				return false
			}
		}
		// True cache miss (no entry for any generation): bump version so any
		// in-flight recompute that captured fences before this event will fail
		// the CAS check; return true (safe no-op) to avoid unnecessary
		// invalidation churn — the fence advance is sufficient protection.
		c.keyVersions[nsName]++
		return true
	}

	c.keyVersions[nsName]++

	oldIP := oldPod.Status.PodIP
	newIP := newPod.Status.PodIP

	oldCIDR := ""
	if oldIP != "" {
		oldCIDR = toCIDR(oldIP)
	}
	newCIDR := ""
	if newIP != "" {
		newCIDR = toCIDR(newIP)
	}

	prefixSetName := model.OwnershipKey(c.clusterName, mapping.Namespace, mapping.Name)
	podID := PodIdentity{Namespace: newPod.Namespace, Name: newPod.Name, UID: string(newPod.UID)}

	// Initialize podRules tracking if needed
	if entry.podRules == nil {
		entry.podRules = make(map[PodIdentity]map[int]struct{})
	}

	anyNewMatch := false

	// Determine which rules each pod version matches using shared helper
	oldMatchedIndices := matchPodToRuleIndices(mapping, labels.Set(oldPod.Labels))
	newMatchedIndices := matchPodToRuleIndices(mapping, labels.Set(newPod.Labels))

	oldMatchedSet := make(map[int]struct{}, len(oldMatchedIndices))
	for _, idx := range oldMatchedIndices {
		oldMatchedSet[idx] = struct{}{}
	}
	newMatchedSet := make(map[int]struct{}, len(newMatchedIndices))
	for _, idx := range newMatchedIndices {
		newMatchedSet[idx] = struct{}{}
	}

	// Build union of all affected rule indices
	affectedRules := make(map[int]struct{})
	for idx := range oldMatchedSet {
		affectedRules[idx] = struct{}{}
	}
	for idx := range newMatchedSet {
		affectedRules[idx] = struct{}{}
	}

	// Evaluate rules against old and new labels to determine which targets are affected
	for i, rule := range mapping.Spec.Mappings {
		if _, affected := affectedRules[i]; !affected {
			continue
		}

		_, oldMatches := oldMatchedSet[i]
		_, newMatches := newMatchedSet[i]

		if newMatches {
			anyNewMatch = true
		}

		// Update MatchedPodsByIndex for selector transitions
		if !oldMatches && newMatches {
			if i < len(entry.state.MatchedPodsByIndex) {
				entry.state.MatchedPodsByIndex[i]++
			}
			// Track new rule contribution
			if entry.podRules[podID] == nil {
				entry.podRules[podID] = make(map[int]struct{})
			}
			entry.podRules[podID][i] = struct{}{}
		} else if oldMatches && !newMatches {
			if i < len(entry.state.MatchedPodsByIndex) && entry.state.MatchedPodsByIndex[i] > 0 {
				entry.state.MatchedPodsByIndex[i]--
			}
			// Remove rule contribution tracking
			if entry.podRules[podID] != nil {
				delete(entry.podRules[podID], i)
			}
		}

		targets := resolveRuleTargets(rule, prefixSetName)
		for _, target := range targets {
			dps, exists := entry.state.Desired[target]
			if !exists {
				dps = DesiredPrefixSet{IPs: make(map[string]struct{})}
			}
			if dps.IPs == nil {
				dps.IPs = make(map[string]struct{})
			}

			// Remove old IP if old pod matched this rule
			if oldMatches && oldCIDR != "" {
				// Per-rule check: only remove if no other contributor for this rule
				hasOtherContributor := false
				for otherPodID, otherRules := range entry.podRules {
					if otherPodID == podID {
						continue
					}
					if _, matchesThisRule := otherRules[i]; !matchesThisRule {
						continue
					}
					membership, inSnapshot := entry.state.Snapshot.Pods[otherPodID]
					if inSnapshot && membership.PodIP != "" && toCIDR(membership.PodIP) == oldCIDR {
						hasOtherContributor = true
						break
					}
				}
				if !hasOtherContributor {
					delete(dps.IPs, oldCIDR)
				}
			}
			// Add new IP if new pod matches this rule
			if newMatches && newCIDR != "" {
				dps.IPs[newCIDR] = struct{}{}
			}

			entry.state.Desired[target] = dps
		}
	}

	// Determine whether this pod was previously pending (tracked in podRules
	// but absent from snapshot, since snapshot only holds IP-bearing pods).
	_, inPodRules := entry.podRules[podID]
	_, inSnapshot := entry.state.Snapshot.Pods[podID]
	wasPending := inPodRules && !inSnapshot

	// Update snapshot membership: only IP-bearing matched pods are stored
	// in the public Snapshot to maintain parity with ComputeDesiredStateWithSnapshot.
	if entry.state.Snapshot.Pods == nil {
		entry.state.Snapshot.Pods = make(map[PodIdentity]PodMembership)
	}
	nowPending := false
	if anyNewMatch {
		if newIP != "" {
			entry.state.Snapshot.Pods[podID] = PodMembership{PodIP: newIP}
		} else {
			// Pod has no IP: keep out of snapshot (parity with ComputeDesiredStateWithSnapshot)
			delete(entry.state.Snapshot.Pods, podID)
			nowPending = true
		}
	} else {
		// Pod no longer matches any rule — remove from snapshot
		delete(entry.state.Snapshot.Pods, podID)
		// Clean up podRules for this pod
		delete(entry.podRules, podID)
	}

	// Update pending count using tracked counter instead of scanning snapshot.
	if wasPending && !nowPending && entry.pendingIPCount > 0 {
		entry.pendingIPCount--
	} else if !wasPending && nowPending {
		entry.pendingIPCount++
	}
	entry.state.HasPendingIPPods = entry.pendingIPCount > 0

	return true
}

// Invalidate removes the cached entry for the given mapping key.
func (c *DesiredStateCache) Invalidate(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.keyVersions[key]++

	for k := range c.entries {
		if k.key == key {
			delete(c.entries, k)
		}
	}
}

// InvalidateNamespace removes all cached entries for the given namespace.
func (c *DesiredStateCache) InvalidateNamespace(namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.entries {
		if k.key.Namespace == namespace {
			c.keyVersions[k.key]++
			delete(c.entries, k)
		}
	}
}

// Delete removes the cached entry and any associated state for the given key.
// It bumps the lifecycle epoch fence so stale in-flight recomputes cannot
// repopulate the cache after terminal cleanup.
func (c *DesiredStateCache) Delete(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lifecycleEpoch[key]++
	c.keyVersions[key]++

	for k := range c.entries {
		if k.key == key {
			delete(c.entries, k)
		}
	}

	// Clear generation-rollover tracking for this key.
	delete(c.knownGenerations, key)
	delete(c.latestGeneration, key)
}

// GetWithVersion returns the cached desired state along with the current
// mutation version and lifecycle epoch fences. These fences are used by
// SetFromRecomputeIfVersion for CAS publish semantics. Returns fences even
// on cache miss (from retained keyVersions/lifecycleEpoch maps). UID validation
// is applied when the caller provides a non-empty UID to prevent stale reads.
func (c *DesiredStateCache) GetWithVersion(mapping *v1alpha1.PodASGMapping) (CachedDesiredState, uint64, uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}
	currentVersion := c.keyVersions[nsName]
	currentLifecycleEpoch := c.lifecycleEpoch[nsName]

	key := cacheKey{key: nsName, generation: mapping.Generation}
	entry, ok := c.entries[key]
	if !ok {
		return CachedDesiredState{}, currentVersion, currentLifecycleEpoch, false
	}
	if mapping.UID != "" && entry.mapping != nil && entry.mapping.UID != mapping.UID {
		return CachedDesiredState{}, currentVersion, currentLifecycleEpoch, false
	}
	return deepCopyCachedState(entry.state), currentVersion, currentLifecycleEpoch, true
}

// SetFromRecomputeIfVersion commits a recompute result only if the provided
// expectedVersion and expectedLifecycleEpoch match the current fences.
// Returns (committed, currentVersion, currentLifecycleEpoch).
func (c *DesiredStateCache) SetFromRecomputeIfVersion(
	mapping *v1alpha1.PodASGMapping,
	pods []corev1.Pod,
	desired map[ASGTarget]DesiredPrefixSet,
	snapshot PodSnapshot,
	matchedPodsByIndex []int,
	hasPendingIPPods bool,
	expectedVersion uint64,
	expectedLifecycleEpoch uint64,
) (committed bool, currentVersion uint64, currentLifecycleEpoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}
	currentVersion = c.keyVersions[nsName]
	currentLifecycleEpoch = c.lifecycleEpoch[nsName]

	if expectedLifecycleEpoch != currentLifecycleEpoch {
		return false, currentVersion, currentLifecycleEpoch
	}
	if expectedVersion != currentVersion {
		return false, currentVersion, currentLifecycleEpoch
	}

	// UID guard: if an existing entry was committed by a different UID (from a
	// prior incarnation that committed before its Delete ran), reject the publish
	// to prevent the old reconcile from overwriting the new incarnation's state.
	key := cacheKey{key: nsName, generation: mapping.Generation}
	if existing, exists := c.entries[key]; exists && mapping.UID != "" &&
		existing.mapping != nil && existing.mapping.UID != mapping.UID {
		return false, currentVersion, currentLifecycleEpoch
	}

	// Generation-rollover guard: reject commits for generations older than the
	// latest committed generation. This prevents stale recomputes from leaking
	// entries for superseded spec versions.
	if latest, ok := c.latestGeneration[nsName]; ok && mapping.Generation < latest {
		return false, currentVersion, currentLifecycleEpoch
	}

	// Fences match — commit using inline logic (avoid double-lock via SetFromRecompute).
	c.setFromRecomputeLocked(mapping, pods, desired, snapshot, matchedPodsByIndex, hasPendingIPPods)

	// Update generation-rollover tracking and prune older generation entries.
	if c.knownGenerations[nsName] == nil {
		c.knownGenerations[nsName] = make(map[int64]struct{})
	}
	c.knownGenerations[nsName][mapping.Generation] = struct{}{}
	if mapping.Generation > c.latestGeneration[nsName] {
		c.latestGeneration[nsName] = mapping.Generation
		// Prune entries for older generations of this key.
		for k := range c.entries {
			if k.key == nsName && k.generation < mapping.Generation {
				delete(c.entries, k)
			}
		}
	}

	return true, currentVersion, currentLifecycleEpoch
}

// SetFromRecomputeArtifactsIfVersion commits a recompute result from pre-built
// artifacts only if the provided expectedVersion and expectedLifecycleEpoch
// match the current fences. This avoids re-matching pods to build podRules
// since the artifacts already contain them.
// Returns (committed, currentVersion, currentLifecycleEpoch).
func (c *DesiredStateCache) SetFromRecomputeArtifactsIfVersion(
	mapping *v1alpha1.PodASGMapping,
	artifacts DesiredStateRecomputeArtifacts,
	expectedVersion uint64,
	expectedLifecycleEpoch uint64,
) (committed bool, currentVersion uint64, currentLifecycleEpoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nsName := types.NamespacedName{Namespace: mapping.Namespace, Name: mapping.Name}
	currentVersion = c.keyVersions[nsName]
	currentLifecycleEpoch = c.lifecycleEpoch[nsName]

	if expectedLifecycleEpoch != currentLifecycleEpoch {
		return false, currentVersion, currentLifecycleEpoch
	}
	if expectedVersion != currentVersion {
		return false, currentVersion, currentLifecycleEpoch
	}

	// UID guard: reject if an existing entry was committed by a different UID.
	key := cacheKey{key: nsName, generation: mapping.Generation}
	if existing, exists := c.entries[key]; exists && mapping.UID != "" &&
		existing.mapping != nil && existing.mapping.UID != mapping.UID {
		return false, currentVersion, currentLifecycleEpoch
	}

	// Generation-rollover guard: reject commits for generations older than the
	// latest committed generation.
	if latest, ok := c.latestGeneration[nsName]; ok && mapping.Generation < latest {
		return false, currentVersion, currentLifecycleEpoch
	}

	c.entries[key] = buildCacheEntryFromArtifacts(mapping, artifacts)

	// Update generation-rollover tracking and prune older generation entries.
	if c.knownGenerations[nsName] == nil {
		c.knownGenerations[nsName] = make(map[int64]struct{})
	}
	c.knownGenerations[nsName][mapping.Generation] = struct{}{}
	if mapping.Generation > c.latestGeneration[nsName] {
		c.latestGeneration[nsName] = mapping.Generation
		for k := range c.entries {
			if k.key == nsName && k.generation < mapping.Generation {
				delete(c.entries, k)
			}
		}
	}

	return true, currentVersion, currentLifecycleEpoch
}

// buildCacheEntryFromArtifacts constructs a cache entry with deep-copied data
// from recompute artifacts, preventing external mutation of cached state.
func buildCacheEntryFromArtifacts(
	mapping *v1alpha1.PodASGMapping,
	artifacts DesiredStateRecomputeArtifacts,
) *cacheEntry {
	copiedDesired := make(map[ASGTarget]DesiredPrefixSet, len(artifacts.Desired))
	for k, v := range artifacts.Desired {
		ips := make(map[string]struct{}, len(v.IPs))
		for ip := range v.IPs {
			ips[ip] = struct{}{}
		}
		copiedDesired[k] = DesiredPrefixSet{IPs: ips}
	}

	copiedSnapshot := PodSnapshot{Pods: make(map[PodIdentity]PodMembership, len(artifacts.Snapshot.Pods))}
	for k, v := range artifacts.Snapshot.Pods {
		copiedSnapshot.Pods[k] = v
	}

	copiedMatchedPods := make([]int, len(artifacts.MatchedPodsByIndex))
	copy(copiedMatchedPods, artifacts.MatchedPodsByIndex)

	copiedPodRules := make(map[PodIdentity]map[int]struct{}, len(artifacts.PodRules))
	for podID, rules := range artifacts.PodRules {
		rulesCopy := make(map[int]struct{}, len(rules))
		for idx := range rules {
			rulesCopy[idx] = struct{}{}
		}
		copiedPodRules[podID] = rulesCopy
	}

	return &cacheEntry{
		state: CachedDesiredState{
			Desired:            copiedDesired,
			Snapshot:           copiedSnapshot,
			MatchedPodsByIndex: copiedMatchedPods,
			HasPendingIPPods:   artifacts.HasPendingIPPods,
		},
		mapping:        mapping,
		podRules:       copiedPodRules,
		pendingIPCount: artifacts.PendingIPCount,
	}
}

func deepCopyCachedState(state CachedDesiredState) CachedDesiredState {
	cp := CachedDesiredState{
		HasPendingIPPods: state.HasPendingIPPods,
	}

	if state.Desired != nil {
		cp.Desired = make(map[ASGTarget]DesiredPrefixSet, len(state.Desired))
		for k, v := range state.Desired {
			ips := make(map[string]struct{}, len(v.IPs))
			for ip := range v.IPs {
				ips[ip] = struct{}{}
			}
			cp.Desired[k] = DesiredPrefixSet{IPs: ips}
		}
	}

	if state.Snapshot.Pods != nil {
		cp.Snapshot = PodSnapshot{Pods: make(map[PodIdentity]PodMembership, len(state.Snapshot.Pods))}
		for k, v := range state.Snapshot.Pods {
			cp.Snapshot.Pods[k] = v
		}
	}

	if state.MatchedPodsByIndex != nil {
		cp.MatchedPodsByIndex = make([]int, len(state.MatchedPodsByIndex))
		copy(cp.MatchedPodsByIndex, state.MatchedPodsByIndex)
	}

	return cp
}
