# Pod-NSG-Controller — Performance Improvement Specification

> **Development Approach:** Spec-Driven Development using Test-Driven Development (TDD).
> Every phase defines acceptance criteria expressed as failing tests **before** implementation begins.

---

## Table of Contents

- [Motivation](#motivation)
- [Benchmark Baseline](#benchmark-baseline)
- [Phase 1: Parallel Mapping Reconciliation](#phase-1-parallel-mapping-reconciliation)
- [Phase 2: Parallel Azure GET Calls](#phase-2-parallel-azure-get-calls)
- [Phase 3: Pod Event Debouncing](#phase-3-pod-event-debouncing)
- [Phase 4: Incremental Diff and Patch](#phase-4-incremental-diff-and-patch)
- [Phase 5: Desired-State Cache](#phase-5-desired-state-cache)
- [Phase 6: Tunable ARM Concurrency](#phase-6-tunable-arm-concurrency)
- [Appendix A: Benchmark Methodology](#appendix-a-benchmark-methodology)
- [Appendix B: Configuration Reference](#appendix-b-configuration-reference)

---

## Motivation

Load testing on the eastus2euap cluster (300 pod ops/min sustained for 10 minutes, 3,000 total
operations) revealed the following reconciliation latency profile:

| Metric | Pod Create → ASG Update | Pod IP → ASG Update |
|--------|------------------------|---------------------|
| P50    | 1.7s                   | 7.1s                |
| P95    | 12.9s                  | 28.0s               |
| P99    | 32.4s                  | 36.6s               |
| Avg    | 3.0s                   | 9.9s                |

Key observations:

1. **Single reconciler worker** — `SetupWithManager` uses controller-runtime defaults
   (1 worker). Multiple PodASGMappings serialize, doubling latency.
2. **Sequential Azure GETs** — `listActualForTargets()` fetches each ASG prefix set
   sequentially; each GET takes 200-500ms.
3. **No event debouncing** — every pod IP change immediately enqueues a reconcile.
   At 300 ops/min the controller reconciles ~80 times per mapping over 10 min.
4. **Full-state replacement** — every PUT sends the entire IP list even when a single
   IP changed, increasing payload size and ETag conflict probability.
5. **Full desired-state recomputation** — every reconcile lists all pods in the namespace
   and rebuilds the desired state from scratch.

The improvements below are ordered by implementation effort and expected impact.
Each phase is independent and can be merged separately.

---

## Benchmark Baseline

All performance targets reference this baseline measured on the eastus2euap cluster:

- **Cluster:** 3 worker nodes (Standard_D4s_v5), 56 IPs/NIC, Azure CNI transparent bridge
- **Controller:** Single replica, `MaxConcurrentActions=5`, `ResyncInterval=60s`
- **Workload:** 300 pod ops/min (150 creates + 150 deletes per min) for 10 minutes
- **Mappings:** 2 PodASGMappings (frontend, backend), each selecting ~50% of pods
- **Peak concurrent pods:** 264

| Metric         | Baseline Value |
|----------------|----------------|
| P50 latency    | 1.7s           |
| P95 latency    | 12.9s          |
| P99 latency    | 32.4s          |
| Reconcile cycles/mapping | ~80 in 10 min |
| Reconcile interval P50   | 8.4s     |
| ETag conflicts | 3              |
| ARM throttles  | 0              |
| Synced %       | 85-87%         |

---

## Phase 1: Parallel Mapping Reconciliation

### Problem

`SetupWithManager` in `internal/controller/setup.go:27-31` uses `ctrl.NewControllerManagedBy(mgr)`
without setting `MaxConcurrentReconciles`. The controller-runtime default is 1 worker, meaning
reconciliations for different PodASGMappings serialize. With 2 mappings, one must wait for the
other to finish its full reconcile cycle (pod list + Azure GETs + diff + Azure PUTs).

### Design

Add a `MaxConcurrentReconciles` option to the controller builder in `SetupWithManager`. Make
it configurable via the `Config` struct with a sensible default.

Each worker is assigned a distinct PodASGMapping key from the work queue. Controller-runtime
guarantees that at most one worker holds a given key at a time — if a second reconcile is
enqueued for a mapping that is already in-flight, it waits in the queue until the first
completes. Workers therefore never process the same mapping concurrently; the parallelism
is strictly across *different* mappings.

### Changes

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `MaxConcurrentReconciles int` field, env var `MAX_CONCURRENT_RECONCILES`, default `5` |
| `internal/controller/mapping_reconciler.go` | Add `MaxConcurrentReconciles int` field to `MappingReconciler` struct |
| `internal/controller/setup.go` | Pass `controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}` to the builder |
| `cmd/main.go` | Wire `cfg.MaxConcurrentReconciles` into the reconciler |

### Acceptance Criteria (Tests First)

1. **`internal/config/config_test.go`** — `TestLoad_MaxConcurrentReconciles`:
   - With `MAX_CONCURRENT_RECONCILES=10` → `cfg.MaxConcurrentReconciles == 10`
   - With no env var → `cfg.MaxConcurrentReconciles == 5` (default)
   - With `MAX_CONCURRENT_RECONCILES=0` → error
   - With `MAX_CONCURRENT_RECONCILES=-1` → error

2. **`internal/controller/setup_test.go`** — `TestSetupWithManager_ConcurrentReconciles`:
   - Verify the controller is created with the configured `MaxConcurrentReconciles`
   - Two PodASGMappings reconcile concurrently (not sequentially)
   - A single PodASGMapping is never reconciled by more than one worker at a time: enqueue two rapid reconcile requests for the same mapping and verify the second does not start until the first completes (serialization guarantee)

### Performance Target

| Metric | Baseline | Target |
|--------|----------|--------|
| P50 latency | 1.7s | ≤1.5s |
| P95 latency | 12.9s | ≤7.0s |
| Synced % | 85% | ≥92% |

---

## Phase 2: Parallel Azure GET Calls

### Problem

`listActualForTargets()` in `internal/controller/mapping_reconciler.go:449-483` iterates
over all ASG targets sequentially, issuing one Azure GET per target. Each GET incurs
200-500ms of network latency. With N ASG targets per mapping, this adds N×200ms
to every reconcile cycle before the diff can even begin.

### Design

Fan out GET calls using a bounded goroutine pool (reuse the existing `maxParallel`
concurrency pattern from the `Executor`). Collect results with error aggregation.

### Changes

| File | Change |
|------|--------|
| `internal/controller/mapping_reconciler.go` | Rewrite `listActualForTargets()` to use `sync.WaitGroup` + semaphore channel for parallel GETs |

### Acceptance Criteria (Tests First)

1. **`internal/controller/mapping_reconciler_test.go`** — `TestListActualForTargets_Parallel`:
   - Mock Azure client with artificial 100ms delay per GET
   - 5 targets → total wall time < 300ms (proves parallelism), not ~500ms (sequential)
   - Error on one target returns error while still fetching remaining targets

2. **`internal/controller/mapping_reconciler_test.go`** — `TestListActualForTargets_NotFoundSkipped`:
   - Targets with `ErrNotFound` responses are omitted from the result map (existing behavior preserved)

### Performance Target

| Metric | Baseline | Target |
|--------|----------|--------|
| Reconcile interval P50 | 8.4s | ≤6.0s |
| P95 latency | (after Phase 1) ≤7.0s | ≤5.0s |

---

## Phase 3: Pod Event Debouncing

### Problem

Every pod create/update/delete event that changes an IP immediately enqueues a reconcile
for the matching PodASGMapping(s). At 300 ops/min (5/s), the controller-runtime work queue
deduplicates by key but still processes reconciles as fast as they're dequeued. This results
in ~80 reconcile cycles per mapping over 10 minutes — many of which recompute the same
desired state and find no diff because the ARM call from the previous cycle already
covered the change.

### Design

Introduce a configurable debounce window. When a pod event enqueues a reconcile, apply a
`RequeueAfter` delay instead of immediate requeue. This coalesces bursts of pod events
into fewer, larger reconcile batches.

Two mechanisms:

1. **Rate-limited work queue** — Replace the default work queue with a
   `workqueue.NewTypedRateLimitingQueue` using a `BucketRateLimiter` that limits
   reconciles to a configurable rate (e.g., 2/s per mapping key).

2. **Minimum reconcile interval** — In the reconciler, track `lastReconcileTime` per
   mapping and skip early requeues within a configurable `MinReconcileInterval` (default 2s),
   returning `RequeueAfter: remaining`.

### Changes

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `MinReconcileIntervalMs int` field, env var `MIN_RECONCILE_INTERVAL_MS`, default `2000` |
| `internal/controller/setup.go` | Configure rate-limited work queue in the controller builder |
| `internal/controller/mapping_reconciler.go` | Add `lastReconcileTime sync.Map`, skip/requeue if within `MinReconcileInterval` |

### Acceptance Criteria (Tests First)

1. **`internal/config/config_test.go`** — `TestLoad_MinReconcileInterval`:
   - With `MIN_RECONCILE_INTERVAL_MS=500` → `cfg.MinReconcileIntervalMs == 500`
   - With no env var → default `2000`

2. **`internal/controller/mapping_reconciler_test.go`** — `TestReconcile_Debounce`:
   - Enqueue 10 reconcile requests for the same mapping within 100ms
   - Verify only 1-2 actual reconcile cycles execute (not 10)
   - All pod changes are captured in the final reconcile state

3. **`internal/controller/mapping_reconciler_test.go`** — `TestReconcile_DebounceDoesNotDelayInitial`:
   - First reconcile for a new mapping executes immediately (no debounce penalty)

### Performance Target

| Metric | Baseline | Target |
|--------|----------|--------|
| Reconcile cycles/mapping/10min | ~80 | ≤40 |
| ARM GETs/min | ~16 | ≤8 |
| P50 latency | ≤1.5s | ≤2.5s (acceptable trade-off) |
| P95 latency | ≤5.0s | ≤4.0s |

> **Note:** P50 may increase slightly because events are batched, but P95/P99 improve
> because each reconcile processes more changes per cycle, reducing overall ARM load.

---

## Phase 4: Incremental Diff and Patch

### Problem

`ComputeDiff()` in `internal/engine/diff.go:9-87` performs a full-state diff: if any IP
differs between desired and actual, it generates an `UpdatePrefixSet` action with **all**
desired IPs. The `Put()` call replaces the entire prefix set contents. At 264 concurrent
pods, every update sends 264 IPs even if only 1 changed.

This causes:
- Larger PUT payloads (higher ARM processing time)
- Higher ETag conflict probability (any concurrent change invalidates the full PUT)
- Redundant data transfer

### Design

Extend the diff engine to compute **incremental changes**: which IPs to add and which to
remove. Introduce a new action kind `PatchPrefixSet` that carries only the delta.

If the ARM API does not support PATCH operations on AddressPrefixSet, the implementation
falls back to a read-modify-write pattern:
1. GET current prefix set (with ETag)
2. Apply delta (add/remove IPs) locally
3. PUT the modified set (with `If-Match: ETag`)

This still reduces ETag conflicts because the read-modify-write uses the freshest state,
not a stale desired state computed from the informer cache.

### Changes

| File | Change |
|------|--------|
| `internal/engine/types.go` | Add `PatchPrefixSet ActionKind`, add `AddIPs`/`RemoveIPs` fields to `Action` |
| `internal/engine/diff.go` | When actual exists and differs from desired, compute IP-level delta; emit `PatchPrefixSet` if delta is smaller than full replacement threshold (configurable, default 50%) |
| `internal/azure/executor.go` | Handle `PatchPrefixSet`: GET → apply delta → PUT with ETag |
| `internal/azure/address_prefix_set_client.go` | Add `GetWithETag()` method returning `(AddressPrefixSet, etag string, error)` for use in read-modify-write |

### Acceptance Criteria (Tests First)

1. **`internal/engine/diff_test.go`** — `TestComputeDiff_IncrementalPatch`:
   - Desired: {A, B, C, D}, Actual: {A, B, E} → `PatchPrefixSet` with `AddIPs: [C, D]`, `RemoveIPs: [E]`
   - Delta is 3 ops vs full replacement of 4 IPs

2. **`internal/engine/diff_test.go`** — `TestComputeDiff_FallbackToFullReplace`:
   - Desired: {X, Y, Z}, Actual: {A, B, C} → `UpdatePrefixSet` (full replacement, delta > 50%)

3. **`internal/engine/diff_test.go`** — `TestComputeDiff_NoDiffNoPatch`:
   - Desired == Actual → no action emitted (existing behavior preserved)

4. **`internal/azure/executor_test.go`** — `TestExecutor_PatchPrefixSet`:
   - Mock GET returns {A, B, E} with ETag
   - Executor applies delta: adds C, D; removes E → PUT {A, B, C, D} with `If-Match`
   - Verify PUT payload and ETag header

5. **`internal/azure/executor_test.go`** — `TestExecutor_PatchETagConflictRetry`:
   - First GET returns ETag1, PUT gets 412, retry GET returns ETag2, retry PUT succeeds
   - Delta is recomputed against fresh state on each retry

### Performance Target

| Metric | Baseline | Target |
|--------|----------|--------|
| PUT payload size (264 pods) | 264 IPs | ≤10 IPs (avg) |
| ETag conflicts/10min | 3 | ≤1 |
| P99 latency | 32.4s | ≤15.0s |

---

## Phase 5: Desired-State Cache

### Problem

`ComputeDesiredState()` in `internal/engine/desired_state.go:19-91` rebuilds the complete
`map[ASGTarget]DesiredPrefixSet` by iterating over all pods on every reconcile cycle. At 264
pods with 2 mappings and ~40 reconcile cycles over 10 minutes, this means listing and
processing 264×40 = 10,560 pod evaluations unnecessarily. The pod informer already has a
cached list; the bottleneck is the O(pods × selectors) label matching.

### Design

Maintain an in-memory index that maps pod namespace/name to its current IP and matching
ASG targets. Update the index incrementally from informer watch events (create/update/delete).
Use the index to produce the desired state in O(1) per reconcile instead of O(pods).

Fall back to full recomputation on resync interval (every 60s) to correct any drift.

### Changes

| File | Change |
|------|--------|
| `internal/engine/desired_state_cache.go` (new) | `DesiredStateCache` struct: `OnPodAdd/Update/Delete`, `GetDesiredState()`, `Invalidate()` |
| `internal/engine/desired_state_cache_test.go` (new) | Unit tests for incremental updates, cache correctness, invalidation |
| `internal/controller/pod_handler.go` | On pod create/update/delete, update the cache in addition to enqueuing reconcile |
| `internal/controller/mapping_reconciler.go` | On reconcile, call `cache.GetDesiredState()` instead of `engine.ComputeDesiredState()`. On resync (no events), call `cache.Invalidate()` to force full recomputation |

### Acceptance Criteria (Tests First)

1. **`internal/engine/desired_state_cache_test.go`** — `TestCache_IncrementalAdd`:
   - Add 3 pods → `GetDesiredState()` returns correct IP mappings
   - Add 1 more pod → state updated without full recomputation (verify via call counter)

2. **`internal/engine/desired_state_cache_test.go`** — `TestCache_PodDelete`:
   - Add 3 pods, delete 1 → `GetDesiredState()` excludes deleted pod's IP

3. **`internal/engine/desired_state_cache_test.go`** — `TestCache_PodIPChange`:
   - Pod update with new IP → old IP removed, new IP present in desired state

4. **`internal/engine/desired_state_cache_test.go`** — `TestCache_Invalidate`:
   - After `Invalidate()`, next `GetDesiredState()` triggers full recomputation

5. **`internal/engine/desired_state_cache_test.go`** — `TestCache_ConcurrentAccess`:
   - 10 goroutines adding/removing pods concurrently → no data races (run with `-race`)

### Performance Target

| Metric | Baseline | Target |
|--------|----------|--------|
| Reconcile cycle CPU time (264 pods) | O(pods) | O(1) amortized |
| Memory overhead | None | ~1KB per pod (IP + target refs) |

---

## Phase 6: Tunable ARM Concurrency

### Problem

`internal/config/config.go:37-41` defaults `MaxConcurrentActions=5` and `ARMRateLimitRPS=10`.
These are conservative for clusters with many ASG targets. The executor in
`internal/azure/executor.go:42` uses `maxParallel` but is initialized only once at startup.

### Design

Make ARM concurrency dynamically tunable without controller restart. Add metrics to expose
ARM call latency and queue depth so operators can tune values based on observed load.

### Changes

| File | Change |
|------|--------|
| `internal/config/config.go` | Increase default `MaxConcurrentActions` to `10`, `ARMRateLimitRPS` to `20` |
| `internal/azure/executor.go` | Add Prometheus metrics: `arm_call_duration_seconds` (histogram), `arm_concurrent_actions` (gauge), `arm_etag_conflicts_total` (counter) |
| `internal/azure/address_prefix_set_client.go` | Add Prometheus metrics: `arm_request_duration_seconds` by operation (GET/PUT/DELETE) |
| `docs/SPECIFICATION.md` | Update Appendix C configuration reference with new defaults |

### Acceptance Criteria (Tests First)

1. **`internal/config/config_test.go`** — `TestLoad_Defaults`:
   - No env vars → `MaxConcurrentActions == 10`, `ARMRateLimitRPS == 20`

2. **`internal/azure/executor_test.go`** — `TestExecutor_Metrics`:
   - Execute 5 actions → `arm_call_duration_seconds` histogram has 5 observations
   - Execute with 1 ETag conflict → `arm_etag_conflicts_total` counter incremented

3. **`internal/azure/address_prefix_set_client_test.go`** — `TestClient_RequestMetrics`:
   - GET/PUT/DELETE → `arm_request_duration_seconds` has observations per operation label

### Performance Target

| Metric | Baseline (5 actions) | Target (10 actions) |
|--------|---------------------|---------------------|
| Max parallel ARM calls | 5 | 10 |
| ARM rate limit | 10 RPS | 20 RPS |
| Executor throughput | ~5 actions/s | ~10 actions/s |

---

## Appendix A: Benchmark Methodology

### Load Test Script

Located at `test/loadtest/churn-test.sh` (parallelized version):

- **Target:** 300 pod ops/min (150 creates + 150 deletes) for 10 minutes
- **Parallelism:** 20 concurrent `kubectl` workers via `xargs -P`
- **Pod image:** `registry.k8s.io/pause:3.9` (minimal, ~500KB)
- **Monitoring:** PodASGMapping status polled every 2s, pod IPs tracked every 3s
- **Analysis:** `test/loadtest/analyze-churn.sh` computes latency percentiles from
  event timestamps and status snapshots

### How to Run

```bash
export KUBECONFIG=/path/to/kubeconfig.yaml

# Run churn test
bash test/loadtest/churn-test.sh

# Analyze results
bash test/loadtest/analyze-churn.sh /tmp/churn-test-<timestamp>
```

### Measuring Reconciliation Latency

Two latency metrics are computed:

1. **Pod Create → ASG Status Update:** Time from `kubectl apply` to when
   `PodASGMapping.status.matchedPods` count changes in the next status poll.
   Includes pod scheduling, IP assignment, informer propagation, reconcile cycle,
   and ARM call latency.

2. **Pod IP Assigned → ASG Status Update:** Time from when the pod's IP is first
   observed (via polling) to when the status changes. Isolates controller + ARM
   latency from pod scheduling overhead.

---

## Appendix B: Configuration Reference

All environment variables added by this specification:

| Variable | Type | Default | Phase | Description |
|----------|------|---------|-------|-------------|
| `MAX_CONCURRENT_RECONCILES` | int | `5` | 1 | Max parallel PodASGMapping reconciliations |
| `MIN_RECONCILE_INTERVAL_MS` | int | `2000` | 3 | Minimum time between reconciles for the same mapping (ms) |
| `MAX_CONCURRENT_ACTIONS` | int | `10` | 6 | Max parallel ARM mutations per reconcile (was `5`) |
| `ARM_RATE_LIMIT_RPS` | float | `20` | 6 | Per-subscription ARM call rate limit (was `10`) |

Existing variables (unchanged):

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `AZURE_SUBSCRIPTION_ID` | string | (required) | Default Azure subscription |
| `AZURE_RESOURCE_GROUP` | string | (required) | Default Azure resource group |
| `CLUSTER_NAME` | string | (required) | Unique cluster identity for ownership keys |
| `RESYNC_INTERVAL_SECONDS` | int | `60` | Periodic full resync interval |
| `AZURE_CLIENT_ID` | string | (optional) | Managed identity client ID for multi-identity VMs |

---

## Summary: Expected Cumulative Impact

| Metric | Baseline | After All Phases | Improvement |
|--------|----------|-----------------|-------------|
| P50 latency | 1.7s | ≤2.0s | Comparable (batching trade-off) |
| P95 latency | 12.9s | ≤4.0s | **3.2× faster** |
| P99 latency | 32.4s | ≤10.0s | **3.2× faster** |
| Reconcile cycles/10min | ~80 | ≤40 | **50% fewer ARM calls** |
| ETag conflicts/10min | 3 | ≤1 | **3× fewer** |
| Synced % | 85% | ≥95% | **+10pp** |
| PUT payload (264 pods) | 264 IPs | ≤10 IPs avg | **96% smaller** |
