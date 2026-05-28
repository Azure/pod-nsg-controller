# Scope Report: Phase 3 - Pod Event Debouncing

## Spec alignment
- Source: `docs/PERFORMANCE-IMPROVEMENTS.md`
- Relevant section: **Phase 3: Pod Event Debouncing** (Design/Changes/Acceptance Criteria, lines ~166-225; config appendix line ~442)
- Deliverables in spec:
  - Add `MinReconcileIntervalMs` / `MIN_RECONCILE_INTERVAL_MS` (default `2000`)
  - Configure a rate-limited controller work queue
  - Add per-mapping minimum reconcile interval logic (`lastReconcileTime`, `RequeueAfter` remaining)

## Repository/package layout (relevant)
- `cmd/`: app wiring (`cmd/main.go`) injects config into `MappingReconciler`
- `internal/config/`: env parsing/validation (`Load()`)
- `internal/controller/`: watch wiring (`setup.go`), pod event enqueue (`pod_handler.go`), reconcile loop (`mapping_reconciler.go`), predicates/error policy
- `internal/metrics/`: instrumented queue wrapper (`queue.go`) already supports delayed queue semantics (`AddAfter`)
- `api/`: CRD types
- `test/integration/`: envtest flow coverage for pod-triggered reconciles (`test/integration/phase5/controller_wiring_integration_test.go`)

## Root cause analysis
Pod-triggered reconciles are effectively immediate and unthrottled for a mapping key:

1. `PodPredicate()` allows create/delete and update when Pod IP or labels change (`internal/controller/predicates.go`), so churn events pass through by design.
2. `PodToMappingEventHandler` enqueues via `q.Add(req)` for create/update/delete (`internal/controller/pod_handler.go`: `Create`, `Update`, `Delete`), with no debounce delay.
3. `SetupWithManager()` does not set a custom queue rate limiter; it relies on controller-runtime defaults (`internal/controller/setup.go`).
4. `MappingReconciler.Reconcile()` has no per-key minimum interval gate; successful runs return prompt follow-up or `ResyncInterval`, but frequent pod events still trigger repeated full reconcile cycles (`internal/controller/mapping_reconciler.go`).
5. `internal/config/config.go` currently has no `MinReconcileIntervalMs` field/env var.

Net effect: bursts of pod events can repeatedly enqueue/process the same mapping key before meaningful state changes accumulate.

## Affected code path trace
1. Watch wiring: `MappingReconciler.SetupWithManager()` registers
   - `For(&PodASGMapping{}, MappingPredicate())`
   - `Watches(&Pod{}, podHandler, PodPredicate())`
2. Pod event handling: `PodToMappingEventHandler.Create/Update/Delete` computes matching mappings and immediately `q.Add(...)`.
3. Reconcile execution: `MappingReconciler.Reconcile()` processes each dequeued request and typically schedules next periodic run with `RequeueAfter: r.ResyncInterval`.
4. Error path requeue behavior (`DefaultRequeuePolicy` / `DecideRequeue...`) is separate and must remain intact.

## Interfaces/contracts involved
- `handler.EventHandler` contract in `PodToMappingEventHandler` (`Create`, `Update`, `Delete`, `Generic`)
- `workqueue.TypedRateLimitingInterface[reconcile.Request]` (`Add`, `AddAfter`, `AddRateLimited`)
- `reconcile.Reconciler` contract (`Reconcile(ctx, req) (ctrl.Result, error)`)
- Existing policy contract in `errors.go` for system/action-failure requeue decisions

## Callers and dependencies
- `cmd/main.go` constructs `MappingReconciler`; any new debounce field on the reconciler must be wired here from config.
- `setup.go` owns controller options (`MaxConcurrentReconciles`, queue factory), so queue debouncing/rate limiting is centralized here.
- `pod_handler.go` is the only pod-watch event handler for mapping reconciliation.

## Existing patterns to reuse
- Requeue timing pattern already exists in reconciler (`promptRequeueAfter`, `RequeueAfter` usage in `mapping_reconciler.go`).
- Queue instrumentation wrapper already handles delayed queue semantics (`internal/metrics/queue.go`, `queue_delayed_test.go`), so rate-limited queue changes should preserve this path.
- Config parsing style: env -> typed parse -> validation using `errors.Wrap`/`fmt.Errorf` (`internal/config/config.go`).
- Controller logging style: `log.FromContext(...).WithValues(...)` and structured key/value fields.

## Existing tests to extend
- `internal/config/config_test.go`
  - Add `TestLoad_MinReconcileInterval` per spec (env value + default)
- `internal/controller/mapping_reconciler_test.go`
  - Add `TestReconcile_Debounce`
  - Add `TestReconcile_DebounceDoesNotDelayInitial`
  - Ensure existing prompt-follow-up/resync tests still hold for non-debounce paths
- `internal/controller/setup_metrics_test.go` (and/or a setup-focused test)
  - Extend to validate queue/rate-limiter wiring path remains instrumented when metrics are enabled
- `internal/controller/pod_handler_test.go`
  - Optional extension if enqueue method changes (`Add` vs `AddAfter`) or handler behavior is refactored
- `test/integration/phase5/controller_wiring_integration_test.go`
  - Optional integration coverage for burst pod events collapsing into fewer reconcile executions

## Recommended existing files to modify
1. `internal/config/config.go`
   - Add `MinReconcileIntervalMs int`
   - Parse `MIN_RECONCILE_INTERVAL_MS` with default `2000`, validate `>= 0` or `>= 1` per chosen contract
2. `internal/config/config_test.go`
   - Add targeted load tests for new env var/default/validation behavior
3. `internal/controller/mapping_reconciler.go`
   - Add per-key `lastReconcileTime` tracking and minimum-interval gate in `Reconcile()`
   - Ensure first reconcile for a key is immediate
   - Ensure cleanup paths clear debounce state on mapping deletion/not-found to avoid stale growth
4. `internal/controller/setup.go`
   - Configure controller queue rate limiter consistent with Phase 3 design
   - Preserve metrics queue instrumentation (`metrics.NewInstrumentedQueueFactory`) when enabled
5. `cmd/main.go`
   - Wire new config value(s) into `MappingReconciler` fields used by debounce logic
6. `internal/controller/mapping_reconciler_test.go`
   - Add debounce unit tests and update assertions where timing semantics intentionally change

## New files assessment
- **No new files required.**
- All required behavior fits naturally in existing config/controller wiring and existing test modules.

## Logging and error-handling context near changes
- Pod handler list failures are wrapped (`errors.Wrapf`) and logged via `h.Logger.Error(...)` (`pod_handler.go`).
- Reconcile path uses structured logr logging and centralized error-policy finalization (`error_policy.go` + `errors.go`).
- Debounce guard should return explicit `ctrl.Result{RequeueAfter: ...}` without swallowing errors, and should not bypass existing error-policy paths.

## Risk assessment
1. **Behavioral regression on first reconcile:** debounce must not delay initial processing for new keys (explicit spec criterion).
2. **Interaction with existing requeue policies:** minimum-interval gating must not break action/system-error backoff behavior.
3. **Prompt follow-up logic conflicts:** existing short follow-up behavior for empty targets/pending IPs must remain coherent with new debounce window.
4. **State retention/leak risk:** per-key `lastReconcileTime` map requires cleanup on terminal paths (delete/not-found) to prevent unbounded growth.
5. **Queue metrics correctness:** custom rate limiter must not break instrumented queue depth/inflight reporting semantics.
6. **Test fragility:** timing-sensitive debounce tests can be flaky if wall-clock assumptions are too strict; prefer deterministic windows/assertions.

## Scope conclusion
Phase 3 is localized to existing `config`, `controller setup`, and `mapping reconciler` modules, with test extensions in existing test files. No new module boundaries are required.
