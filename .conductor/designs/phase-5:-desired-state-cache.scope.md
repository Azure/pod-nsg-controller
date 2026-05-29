# Phase 5: Desired-State Cache — Scoping Report

## Scope Summary

Phase 5 addresses a performance bottleneck where desired state is rebuilt from all pods on each reconcile. In this codebase, the Phase 5 architecture is already present (cache, pod-event incremental mutation, cache-first reconcile, periodic forced recompute), so the scope for any additional Phase 5 work should be focused on **existing cache/reconciler/pod-handler modules**, not new modules.

## Root Cause Analysis

The baseline bottleneck originates in full recomputation:

- `internal/engine/desired_state.go` → `ComputeDesiredState(...)`
- `internal/engine/desired_state_snapshot.go` → `ComputeDesiredStateWithSnapshot(...)`

Both require iterating namespace pods and selector rules. Historically, reconcile triggered this path repeatedly, producing O(pods × selectors) work on frequent reconcile cycles.

In current code, this root cause is mitigated by a cache-first path in `MappingReconciler.Reconcile(...)`, but correctness and performance now depend on event-driven cache mutation and forced resync behavior staying aligned.

## Affected Code Paths (File + Function)

1. **Reconcile desired-state resolution**
   - `internal/controller/mapping_reconciler.go`
   - `func (r *MappingReconciler) Reconcile(...)`
   - Key flow:
     - cache lookup: `r.DesiredStateCache.Get(&mapping)`
     - fallback recompute: `ComputeDesiredStateWithSnapshot(...)`
     - cache seed: `SetFromRecompute(...)`
     - periodic full recompute gating: `shouldForceDesiredStateRecompute(...)`, `markDesiredStateFullRecompute(...)`

2. **Pod watch event → cache mutation**
   - `internal/controller/pod_handler.go`
   - `Create(...)`, `Update(...)`, `Delete(...)`
   - Key flow:
     - enqueue reconcile requests
     - mutate/invalidate cache via `OnPodAdd`, `OnPodUpdate`, `OnPodDelete`, `Invalidate`, `InvalidateNamespace`

3. **Cache data model + incremental update logic**
   - `internal/engine/desired_state_cache.go`
   - `Get(...)`, `SetFromRecompute(...)`, `OnPodAdd(...)`, `OnPodUpdate(...)`, `OnPodDelete(...)`, `Invalidate(...)`, `InvalidateNamespace(...)`

4. **Shared helper contract for parity**
   - `internal/engine/desired_state_snapshot.go`
   - `matchPodToRuleIndices(...)`, `resolveRuleTargets(...)`
   - These helpers define selector/target evaluation behavior reused by cache logic.

5. **Wiring and ownership of shared cache instance**
   - `internal/controller/setup.go` → `SetupWithManager(...)`
   - `cmd/main.go` → cache construction/injection with `engine.NewDesiredStateCache(...)`

## Interfaces / Contracts in Play

- `MappingReconciler` is controller-runtime reconcile entrypoint.
- `PodToMappingEventHandler` implements controller-runtime event handler contract.
- `Executor` and `StatusUpdater` interfaces in `mapping_reconciler.go` are downstream contracts that consume desired-state outputs.
- `DesiredStateCache` is the internal contract between pod-event handling and reconcile.
- Generation-scoped cache keying (`types.NamespacedName + generation`) is a correctness boundary (prevents stale spec reuse).

## Callers and Dependencies

- `cmd/main.go` creates one shared `DesiredStateCache`.
- `setup.go` passes the same cache to:
  - pod watch handler (`NewPodToMappingEventHandlerWithCache`)
  - reconciler (`MappingReconciler.DesiredStateCache`)
- controller-runtime watch events invoke pod handler methods; reconcile queue invokes `Reconcile`.

## Logging and Error Handling Patterns Nearby

- Controller/reconciler paths use `logr` (`logger.V(1).Info`, `logger.Error`).
- Pod handler also uses `logr` for event-resolution failures.
- Error wrapping style uses `github.com/pkg/errors` (`errors.Wrapf`, `pkgerrors.Wrapf`) in relevant paths (e.g., mapping list and Azure target fetch fanout).
- Engine cache module is intentionally pure/in-memory and does not log.

## Existing Modules to Modify (Preferred)

If Phase 5 behavior changes are needed, prioritize these existing files:

1. `internal/engine/desired_state_cache.go`
2. `internal/controller/pod_handler.go`
3. `internal/controller/mapping_reconciler.go`
4. `internal/engine/desired_state_snapshot.go` (only if selector/target parity changes are required)
5. `internal/controller/setup.go` and `cmd/main.go` (only for wiring/lifecycle adjustments)

## Existing Tests to Extend

1. `internal/engine/desired_state_cache_test.go`
2. `internal/controller/pod_handler_test.go`
3. `internal/controller/mapping_reconciler_test.go`
4. `test/integration/phase5/controller_wiring_integration_test.go`
5. `test/integration/phase5/cross_module_integration_test.go`
6. `internal/engine/desired_state_snapshot_test.go` (if parity/helper behavior changes)

## New Files Assessment

No new files are required for scoped Phase 5 follow-up work. The repository already contains the dedicated cache module and focused unit/integration tests for this phase.

## Related Spec Sections

- `docs/PERFORMANCE-IMPROVEMENTS.md`
  - “Phase 5: Desired-State Cache” (Problem, Design, Changes, Acceptance Criteria)
  - Problem statement references `internal/engine/desired_state.go` recomputation behavior.
  - Change list maps directly to existing implementation locations listed above.

## Risk Assessment

1. **Stale desired state risk**
   - If pod-event mutations miss edge cases (label transition, IP transition, invalid mapping resolution), stale IP membership may persist until forced recompute.
2. **Generation isolation regressions**
   - Incorrect cache keying or invalidation could leak old-generation desired state into new spec generations.
3. **Resync drift-correction regressions**
   - If forced recompute timing/state tracking breaks, periodic drift correction may stop working.
4. **Concurrency/race risk**
   - Cache mutation/read paths are concurrent (watch events + reconcile); lock discipline and deep-copy boundaries are critical.
5. **Status/metrics coupling risk**
   - `MatchedPodsByIndex`, snapshot, and pending-IP flags are consumed by status/metrics paths; cache changes can alter reported status semantics if parity is lost.
