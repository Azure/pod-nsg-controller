# Scope Report — Phase 4: Incremental Diff and Patch

## Problem Scope and Root Cause

The current reconcile path is full-replacement end-to-end:

1. `internal/controller/mapping_reconciler.go` (`Reconcile`) calls `engine.ComputeDiff(desired, actual)`.
2. `internal/engine/diff.go` (`ComputeDiff`) emits `UpdatePrefixSet` whenever desired and actual IP sets differ.
3. `internal/azure/executor.go` (`executeAction`) handles `UpdatePrefixSet` by calling `AddressPrefixSetAPI.Put(...)` with `action.DesiredIPs` (full list).
4. `internal/azure/address_prefix_set_client.go` (`Put`) sends full `addressPrefixSet` payload.

So even a 1-IP change becomes a full list PUT. There is no delta action kind and no delta fields on `engine.Action`, which is the primary code-level root cause for missing incremental patch behavior.

## Related Spec Sections

- `docs/PERFORMANCE-IMPROVEMENTS.md` — **Phase 4: Incremental Diff and Patch** (Problem/Design/Changes/Acceptance Criteria/Targets).
- `docs/repoDesigns/00-architecture-overview.md` — explicitly states current design uses full desired-state convergence without incremental patches.
- `docs/repoDesigns/03-azure-client.md` and `06-error-handling.md` — executor retry flow and single-target recompute behavior that Phase 4 extends.

## Repository Structure (relevant to this phase)

- `cmd/` — controller wiring (`cmd/main.go`)
- `internal/engine/` — desired/actual diff model and action generation
- `internal/azure/` — ARM client, executor, retry behavior, fake clients
- `internal/controller/` — reconcile loop, metrics/status mapping from action kinds
- `api/` — CRD types (not directly impacted for this phase)
- `test/integration/phase4/` — integration coverage of diff+executor pipeline

## Affected Code Path (trace)

- `MappingReconciler.Reconcile` (`internal/controller/mapping_reconciler.go`)
  - `listActualForTargets(...)`
  - `engine.ComputeDiff(...)`
  - `Executor.Execute(...)`
- `Executor.Execute` / `executeWithETagRetry` / `executeAction` (`internal/azure/executor.go`)
- `AddressPrefixSetClient.Get`/`Put` (`internal/azure/address_prefix_set_client.go`)
- `engine.Action` and `engine.ComputeDiff` (`internal/engine/types.go`, `internal/engine/diff.go`)

## Existing Interfaces/Contracts Impacted

1. `engine.ActionKind` and `engine.Action` are consumed across controller, executor, status, metrics, and tests.
2. `azure.AddressPrefixSetAPI` is implemented by:
   - real client (`internal/azure/address_prefix_set_client.go`)
   - fake client (`internal/azure/fake/fake_client.go`)
   - many executor test stubs (`internal/azure/executor_test.go`).
3. Controller metrics and classification logic currently assume only create/update/delete kinds:
   - `internal/controller/mapping_reconciler.go` (`actionKindToOperationLabel`, convergence staging)
   - `internal/controller/reconcile_metrics.go` (`ClassifyCRDResolutionOperation`).

## Recommended Existing Files to Modify

### Core implementation

1. `internal/engine/types.go`
   - Add `PatchPrefixSet` action kind.
   - Extend `Action` with delta payload (`AddIPs`, `RemoveIPs`) while preserving existing fields for create/update/delete compatibility.

2. `internal/engine/diff.go`
   - Compute delta (`add`, `remove`) for existing resources.
   - Emit `PatchPrefixSet` for small deltas and `UpdatePrefixSet` fallback for large deltas (threshold default 50% per performance spec).
   - Keep deterministic ordering.

3. `internal/azure/executor.go`
   - Add `PatchPrefixSet` execution path (GET current, apply delta, PUT merged set).
   - Ensure 412 retry recompute handles patch actions correctly and preserves no-op semantics.
   - Update `armOperation(...)` mapping for patch actions.

4. `internal/azure/address_prefix_set_client.go`
   - Add `GetWithETag(...)` (or equivalent method used by executor patch path per spec).
   - Reuse existing GET/ETag extraction logic; avoid duplicating parsing rules.

5. `internal/azure/interfaces.go`
   - Extend `AddressPrefixSetAPI` contract for new patch-read primitive.

### Controller/metrics compatibility (required to avoid regressions)

6. `internal/controller/mapping_reconciler.go`
   - Map `PatchPrefixSet` to `"update"` metric label in `actionKindToOperationLabel`.
   - Ensure drift/convergence bookkeeping treats patch as corrective update.

7. `internal/controller/reconcile_metrics.go`
   - Include `PatchPrefixSet` in CRD resolution operation classification as `"update"`.

## Existing Tests to Extend

1. `internal/engine/diff_test.go`
   - Add spec Phase 4 diff tests:
     - incremental patch emission
     - fallback to full update
     - no-diff no-action behavior.

2. `internal/azure/executor_test.go`
   - Add `PatchPrefixSet` execution coverage:
     - delta application correctness
     - ETag conflict retry with re-GET/recompute.

3. `internal/azure/address_prefix_set_client_test.go`
   - Add tests for new `GetWithETag` contract and ETag handling.

4. `test/integration/phase4/executor_integration_test.go`
   - Extend full pipeline coverage to include patch-producing diffs and executor application.

5. `internal/controller/reconcile_metrics_test.go` and/or `internal/controller/reconciler_metrics_integration_test.go`
   - Ensure `PatchPrefixSet` still emits allowed operation labels (`create|update|delete`) and does not surface as `"unknown"`.

## Need for New Files

No new production files are strictly required. Existing engine, executor, Azure client, controller metrics, and test modules are the correct extension points for this phase.

## Risk Assessment

1. **Action-kind propagation risk:** adding a new kind without updating controller metric/classification switches causes `"unknown"` labels and behavioral drift.
2. **Retry correctness risk:** patch + 412 handling must avoid stale-merge behavior; recompute path in executor must remain deterministic and idempotent.
3. **Interface churn risk:** extending `AddressPrefixSetAPI` requires updating all fake/stub implementations used by unit tests.
4. **Behavior compatibility risk:** create/delete/update semantics and existing Phase 4/7 retry behavior must remain unchanged for non-patch actions.
