# Scope Report: Phase 5 - Desired-State Cache

## Spec alignment
- Source: `docs/PERFORMANCE-IMPROVEMENTS.md`
- Relevant section: **Phase 5: Desired-State Cache** (lines ~297-348)
- Related problem statement: repeated full desired-state recomputation (lines ~45-46, 301-305)

## Repository/package layout (relevant)
- `cmd/`: process bootstrap and dependency wiring (`cmd/main.go`)
- `api/v1alpha1`: CRD types (`PodASGMapping`)
- `internal/engine`: desired-state computation, snapshot, cache implementation
- `internal/controller`: reconcile flow, pod event handling, cache-first orchestration
- `internal/model`: selector compilation and ASG resource parsing used by engine/controller
- `test/integration/phase5`: cross-module and controller wiring integration coverage for Phase 5

## Root cause analysis
The baseline bottleneck is the full recompute path:

1. `internal/engine/desired_state.go:23` (`ComputeDesiredState`) iterates namespace pods and selector rules for every reconcile.
2. In reconcile misses/resync paths, `internal/controller/mapping_reconciler.go:265-274` still does full pod list + recompute.
3. Recompute work is O(pods × mapping-rules), so pod churn can repeatedly trigger expensive selector matching.

Phase 5’s cache path is intended to avoid this by incrementally mutating desired state from pod watch events and only using full recompute as controlled fallback/resync.

## Affected code path trace
1. Pod watch events enter `PodToMappingEventHandler` (`internal/controller/pod_handler.go`):
   - `Create` → `DesiredStateCache.OnPodAdd`
   - `Update` → `DesiredStateCache.OnPodUpdate` / invalidate fallback
   - `Delete` → `DesiredStateCache.OnPodDelete`
2. Reconcile reads cache in `MappingReconciler.Reconcile`:
   - cache read/fences: `DesiredStateCache.GetWithVersion` (`mapping_reconciler.go:245-263`)
   - cache miss/forced resync: list pods + `engine.ComputeDesiredStateRecomputeArtifacts` (`:266-277`)
   - CAS publish: `SetFromRecomputeArtifactsIfVersion` (`:287-335`)
3. Resync forcing is controlled by `shouldForceDesiredStateRecompute` and state in `desiredStateResyncState` (`mapping_reconciler.go:1288-1324`).

## Interfaces/contracts involved
- `MappingReconciler` implements controller-runtime reconcile contract (`Reconcile(ctx, req)`).
- `PodToMappingEventHandler` implements controller-runtime typed event handler contract (`Create/Update/Delete/Generic`).
- `DesiredStateCache` contract consumed by both controller and pod handler:
  - read/CAS publish: `GetWithVersion`, `SetFromRecomputeArtifactsIfVersion`
  - incremental updates: `OnPodAdd`, `OnPodUpdate`, `OnPodDelete`
  - invalidation/terminal cleanup: `Invalidate`, `InvalidateNamespace`, `Delete`
- `StatusUpdater` depends on `matchedPodsByIndex` produced by cache/recompute paths and must remain behaviorally consistent.

## Logging and error-handling patterns nearby
- Controller uses `logr` (`logger := log.FromContext(ctx)`), with `logger.V(1).Info` for cache-path decisions and `logger.Error` for failures.
- Pod handler wraps list failures with `pkg/errors` (`errors.Wrapf` in `listPodASGMappings`), then logs via `logr`.
- Reconciler routes operational errors through staged handlers (`finalizeSystemError`) rather than silent fallback.

## Existing modules to modify (prefer existing)
1. `internal/controller/mapping_reconciler.go`
   - cache-first read/fallback/resync/CAS conflict behavior lives here
   - resync force policy and bounded conflict requeue live here
2. `internal/controller/pod_handler.go`
   - cache mutation ordering and invalidation behavior on pod events
3. `internal/engine/desired_state_cache.go`
   - cache state model, incremental mutation correctness, CAS fence semantics
4. `internal/engine/desired_state_snapshot.go`
   - full recompute artifact source-of-truth that must stay parity-compatible with incremental mutations

## Existing tests to extend
- `internal/engine/desired_state_cache_test.go`
- `internal/engine/desired_state_snapshot_test.go`
- `internal/controller/pod_handler_test.go`
- `internal/controller/mapping_reconciler_test.go`
- `test/integration/phase5/controller_wiring_integration_test.go`
- `test/integration/phase5/cross_module_integration_test.go`

## New files assessment
- **Production:** Adds `internal/engine/desired_state_cache.go` (desired-state cache implementation).
- **Tests/docs:** Adds/extends Phase 5 unit + integration coverage and this scope report.
## Related spec sections
- `docs/PERFORMANCE-IMPROVEMENTS.md`:
  - Problem framing for full recompute (`~45-46`, `~301-305`)
  - Phase 5 design and changes (`~307-323`)
  - Phase 5 acceptance criteria (`~324-341`)
  - Phase 5 performance target (`~344-348`)

## Risk assessment
- Drift/parity risk between incremental cache mutations and full recompute artifacts.
- Stale publish risk under high pod churn without strict CAS fence handling.
- Generation/UID lifecycle race risk (delete/recreate with same namespaced name).
- Status correctness risk if `matchedPodsByIndex` diverges between cache-hit and recompute paths.
- Over-invalidation risk can degrade back to frequent full recomputes and erase performance gains.
