# Scope Report: Phase 2 - Parallel Azure GET Calls

## Spec alignment
- Source: `docs/PERFORMANCE-IMPROVEMENTS.md`
- Relevant section: **Phase 2: Parallel Azure GET Calls** (Design/Changes/Acceptance Criteria, lines ~127-163)
- Declared deliverable: rewrite `listActualForTargets()` in `internal/controller/mapping_reconciler.go` to use `sync.WaitGroup` + bounded semaphore and preserve not-found skip behavior.

## Repository/package layout (relevant to this change)
- `cmd/`: app entrypoints (`cmd/main.go` wires controller/executor/factory/config)
- `internal/controller/`: reconcile flow and target actual-state fetch (`mapping_reconciler.go`)
- `internal/azure/`: Azure client factory + API interfaces + concrete GET implementation
- `internal/config/`: concurrency/env config (`MAX_CONCURRENT_ACTIONS`, `MAX_CONCURRENT_RECONCILES`)
- `api/`: CRD types (`api/v1alpha1`)
- `test/integration/`: phase-based integration suites (no dedicated Phase 2 parallel GET integration test currently)

## Root cause analysis
`MappingReconciler.listActualForTargets()` is currently serial and fail-fast:

1. Iterates targets one-by-one (`for target := range targets`) in `internal/controller/mapping_reconciler.go` (lines ~780-807).
2. Performs one Azure `Get` per target synchronously.
3. Returns immediately on first non-404 error, aborting remaining target GETs.

This makes reconcile latency scale with target count (`N * GET latency`) and prevents partial progress/error aggregation across targets.

## Affected code path trace
1. `MappingReconciler.Reconcile()` → builds `allTargets` and calls `listActualForTargets()` (`internal/controller/mapping_reconciler.go`, ~251-257).
2. `listActualForTargets()`:
   - acquires subscription-scoped client via `AddressPrefixSetClientFactory.ForSubscription`
   - calls `AddressPrefixSetAPI.Get(...)` per target
   - skips `azure.IsNotFound(err)`; otherwise returns wrapped error.
3. `Reconcile()` on error calls `finalizeSystemError(..., stage="list-actual-state", allowStatusWrite=true)` (`internal/controller/error_policy.go`), which logs and applies requeue policy.

## Interfaces/contracts involved
- `internal/azure/interfaces.go`
  - `AddressPrefixSetClientFactory.ForSubscription(subscriptionID)`
  - `AddressPrefixSetAPI.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)`
- Current functional contract in controller path:
  - 404/not-found is non-fatal and omitted from `actual` map.
  - non-404 error fails reconcile stage `list-actual-state`.

## Existing patterns to reuse
- Bounded parallel worker pattern already exists in `internal/azure/executor.go`:
  - `sem := make(chan struct{}, maxParallel)`
  - `sync.WaitGroup`
  - context-aware semaphore acquisition with `select` on `ctx.Done()`
- Error aggregation pattern exists in controller (`aggregateDeleteCleanupErrors`, `aggregateActionFailures` in `mapping_reconciler.go`), so Phase 2 can follow same style for deterministic multi-error reporting.

## Logging/error handling context
- Controller layer primarily uses `fmt.Errorf("...: %w", err)` and defers policy/logging to `finalizeSystemError()` (`logger.Error(..., "stage", "...")`).
- Azure client layer uses `github.com/pkg/errors` wrappers and `*zap.Logger`.
- Phase 2 changes should keep controller-level wrapping style consistent; do not silently drop non-404 errors.

## Existing tests and gaps
- `internal/controller/mapping_reconciler_test.go`
  - Extensive reconcile coverage, but **no direct unit test** for `listActualForTargets()` parallelism/aggregation.
- `internal/controller/mapping_reconciler_error_policy_test.go`
  - Covers `list-actual-state` stage error policy behavior (requeue + nil reconcile error), useful regression coverage.
- `internal/azure/executor_test.go`
  - Demonstrates concurrency-bound testing style (peak concurrency/semaphore), useful reference.

## Recommended existing files to modify
1. `internal/controller/mapping_reconciler.go`
   - Rewrite `listActualForTargets()` with bounded parallel GET fan-out.
   - Preserve:
     - per-subscription client acquisition behavior
     - `ErrNotFound` skip semantics
     - reconcile-stage error propagation on non-404 failures.
   - Add multi-error aggregation for parallel failures.
2. `internal/controller/mapping_reconciler_test.go`
   - Add `TestListActualForTargets_Parallel` (wall-time based + non-fatal completion of all GET attempts when one fails).
   - Add `TestListActualForTargets_NotFoundSkipped` (preserve current omit-on-not-found behavior).
3. (Optional but recommended) `internal/controller/mapping_reconciler_error_policy_test.go`
   - Extend an existing `list-actual-state` case if aggregate error shape changes materially.

## New files assessment
- **No new files required.**
- Existing controller module and test module are the correct extension points for this phase.

## Risks / what could break
- Concurrent map writes in `actual`/error collection if mutex protection is missed.
- Nondeterministic error ordering after parallelization may make brittle string-equality tests fail.
- Context cancellation handling while waiting on semaphore/goroutines.
- Burst GET concurrency can increase rate-limiter waits; must remain bounded.
- Subscription client acquisition in parallel path must avoid redundant races and preserve factory semantics.

## Scope conclusion
Phase 2 is localized to controller actual-state fetch behavior and tests. The root-cause fix fits cleanly in existing files, with no architectural/module split needed.
