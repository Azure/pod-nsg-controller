package azure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	pkgerrors "github.com/pkg/errors"
	"go.uber.org/zap"
)

// ActionResult holds the outcome of executing a single action.
type ActionResult struct {
	Action          engine.Action
	Success         bool
	Err             error
	CompletedAt     time.Time         // Timestamp when the ARM operation finished
	NoOp            bool              // True when a 412-retry recompute determined no ARM mutation was needed
	FinalActionKind engine.ActionKind // The terminal action kind after any recompute (may differ from Action.Kind)
}

// executionOutcome is the internal result of executeWithETagRetry, carrying
// richer semantics than a bare error so Execute() can populate ActionResult fields.
type executionOutcome struct {
	err             error
	noOp            bool              // 412 recompute found target already converged
	finalActionKind engine.ActionKind // the action kind that was actually applied (or original if no recompute)
}

// armOperation returns the ARM operation for a given action kind.
func armOperation(kind engine.ActionKind) ARMOperation {
	switch kind {
	case engine.CreatePrefixSet, engine.UpdatePrefixSet, engine.PatchPrefixSet:
		return ARMOperationPutPrefixSet
	case engine.DeletePrefixSet:
		return ARMOperationDeletePrefixSet
	default:
		return ARMOperation("Unknown")
	}
}

// Executor runs engine actions against Azure with bounded concurrency and ETag retry.
type Executor struct {
	log                   *zap.Logger
	factory               AddressPrefixSetClientFactory
	maxParallel           int
	maxRetries            int
	retryObserver         armRetryObserver
	patchThresholdPercent int
}

// NewExecutor creates an Executor with the given concurrency bound.
func NewExecutor(log *zap.Logger, factory AddressPrefixSetClientFactory, maxParallel int, opts ...ExecutorOption) *Executor {
	if maxParallel < 1 {
		maxParallel = 1
	}
	e := &Executor{
		log:                   log,
		factory:               factory,
		maxParallel:           maxParallel,
		maxRetries:            3,
		patchThresholdPercent: engine.DefaultPatchThresholdPercent,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Execute runs all actions with bounded parallelism and returns results in input order.
func (e *Executor) Execute(ctx context.Context, actions []engine.Action) []ActionResult {
	results := make([]ActionResult, len(actions))

	sem := make(chan struct{}, e.maxParallel)
	var wg sync.WaitGroup

	for i, action := range actions {
		wg.Add(1)
		go func(idx int, act engine.Action) {
			defer wg.Done()

			// Cancellable semaphore wait
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = ActionResult{Action: act, Success: false, Err: ctx.Err()}
				return
			}

			client, err := e.factory.ForSubscription(act.Target.SubscriptionID)
			if err != nil {
				results[idx] = ActionResult{Action: act, Success: false, Err: pkgerrors.Wrap(err, "getting client")}
				return
			}

			outcome := e.executeWithETagRetry(ctx, client, act)
			results[idx] = ActionResult{
				Action:          act,
				Success:         outcome.err == nil,
				Err:             outcome.err,
				CompletedAt:     time.Now(),
				NoOp:            outcome.noOp,
				FinalActionKind: outcome.finalActionKind,
			}
		}(i, action)
	}

	wg.Wait()
	return results
}

func (e *Executor) executeAction(ctx context.Context, client AddressPrefixSetAPI, action engine.Action) error {
	t := action.Target
	switch action.Kind {
	case engine.CreatePrefixSet, engine.UpdatePrefixSet:
		return client.Put(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName, action.DesiredIPs)
	case engine.PatchPrefixSet:
		return e.executePatchAction(ctx, client, action)
	case engine.DeletePrefixSet:
		return client.Delete(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
	default:
		return fmt.Errorf("unknown action kind: %s", action.Kind)
	}
}

// executePatchAction performs a read-modify-write patch: GET current state,
// apply add/remove delta, then PUT with If-Match for optimistic concurrency.
func (e *Executor) executePatchAction(ctx context.Context, client AddressPrefixSetAPI, action engine.Action) error {
	t := action.Target
	current, etag, err := client.GetWithETag(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
	if err != nil {
		return err
	}
	if current == nil {
		// Client returned (nil, _, nil) — treat as not-found so the retry
		// loop can recompute to a CreatePrefixSet action.
		return ErrNotFound
	}

	var currentIPs []string
	if current.Properties != nil {
		currentIPs = current.Properties.AddressPrefixes
	}

	merged := applyPatchDelta(currentIPs, action.AddIPs, action.RemoveIPs)
	return client.PutWithIfMatch(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName, merged, etag)
}

// applyPatchDelta applies add/remove operations to current IPs and returns a sorted deduplicated result.
func applyPatchDelta(current []string, addIPs, removeIPs []string) []string {
	set := make(map[string]struct{}, len(current)+len(addIPs))
	for _, ip := range current {
		set[ip] = struct{}{}
	}
	for _, ip := range removeIPs {
		delete(set, ip)
	}
	for _, ip := range addIPs {
		set[ip] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for ip := range set {
		result = append(result, ip)
	}
	sort.Strings(result)
	return result
}

func (e *Executor) executeWithETagRetry(ctx context.Context, client AddressPrefixSetAPI, action engine.Action) executionOutcome {
	finalKind := action.Kind
	var err error
	for attempt := 1; attempt <= e.maxRetries; attempt++ {
		// Check context before each attempt
		if ctx.Err() != nil {
			return executionOutcome{err: ctx.Err(), finalActionKind: finalKind}
		}

		err = e.executeAction(ctx, client, action)
		if err == nil {
			return executionOutcome{finalActionKind: finalKind}
		}

		// Patch-specific: if GetWithETag returned ErrNotFound, recompute to create/no-op.
		if action.Kind == engine.PatchPrefixSet && IsNotFound(err) {
			next, done, recomputeErr := e.recomputeSingleTargetActionViaDiff(action, nil, ErrNotFound)
			if recomputeErr != nil {
				return executionOutcome{err: pkgerrors.Wrap(recomputeErr, "patch recompute after not-found"), finalActionKind: finalKind}
			}
			if done {
				return executionOutcome{noOp: true, finalActionKind: finalKind}
			}
			if next != nil {
				action = *next
				finalKind = action.Kind
			}
			continue
		}

		if !IsPreconditionFailed(err) {
			return executionOutcome{err: err, finalActionKind: finalKind}
		}

		// Emit etag-conflict retry metric
		if e.retryObserver != nil {
			e.retryObserver.ObserveRetry(action.Target.SubscriptionID, string(armOperation(action.Kind)), "etag-conflict")
		}

		if attempt == e.maxRetries {
			e.log.Warn("ETag conflict, retries exhausted",
				zap.String("operation", string(armOperation(action.Kind))),
				zap.String("actionKind", string(action.Kind)),
				zap.String("prefixSetName", action.Target.PrefixSetName),
				zap.Int("attempt", attempt),
				zap.Int("maxRetries", e.maxRetries),
				zap.Error(err),
			)
			break
		}
		e.log.Warn("ETag conflict, retrying",
			zap.String("operation", string(armOperation(action.Kind))),
			zap.String("actionKind", string(action.Kind)),
			zap.String("prefixSetName", action.Target.PrefixSetName),
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", e.maxRetries),
			zap.Error(err),
		)

		// Check context before recompute
		if ctx.Err() != nil {
			return executionOutcome{err: ctx.Err(), finalActionKind: finalKind}
		}

		t := action.Target
		// Retry recompute is based on a single GET snapshot here, but the subsequent
		// Put performs its own internal GET to acquire the conditional ETag. If the
		// resource changes again between those calls, the retry can still succeed
		// with a newer ETag while applying DesiredIPs derived from this slightly
		// older snapshot. A later reconcile will converge any drift.
		current, getErr := client.Get(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
		next, done, recomputeErr := e.recomputeSingleTargetActionViaDiff(action, current, getErr)
		if recomputeErr != nil {
			return executionOutcome{err: pkgerrors.Wrap(recomputeErr, "recompute after 412"), finalActionKind: finalKind}
		}
		if done {
			// Target is already converged — no ARM mutation was applied.
			return executionOutcome{noOp: true, finalActionKind: finalKind}
		}
		if next != nil {
			action = *next
			finalKind = action.Kind
		}
	}
	return executionOutcome{err: err, finalActionKind: finalKind}
}

// recomputeSingleTargetActionViaDiff re-GETs the resource and recomputes the diff
// using engine.ComputeDiff to determine the next action.
func (e *Executor) recomputeSingleTargetActionViaDiff(action engine.Action, current *AddressPrefixSet, getErr error) (next *engine.Action, done bool, err error) {
	desired := buildSingleTargetDesired(action)
	actual, err := buildSingleTargetActual(action, current, getErr)
	if err != nil {
		return nil, false, pkgerrors.Wrap(err, "building actual state")
	}

	actions := engine.ComputeDiff(desired, actual, e.patchThresholdPercent)

	if len(actions) == 0 {
		return nil, true, nil
	}
	if len(actions) > 1 {
		return nil, false, fmt.Errorf("recompute produced %d actions, expected 0 or 1", len(actions))
	}

	return &actions[0], false, nil
}

// buildSingleTargetDesired builds a single-entry desired map for diff recomputation.
// For DELETE actions or PatchPrefixSet with empty DesiredIPs, returns an empty map
// so ComputeDiff converges to no-op when the resource doesn't exist.
func buildSingleTargetDesired(action engine.Action) map[engine.ASGTarget]engine.DesiredPrefixSet {
	if action.Kind == engine.DeletePrefixSet {
		return map[engine.ASGTarget]engine.DesiredPrefixSet{}
	}
	if action.Kind == engine.PatchPrefixSet && len(action.DesiredIPs) == 0 {
		return map[engine.ASGTarget]engine.DesiredPrefixSet{}
	}
	ips := make(map[string]struct{}, len(action.DesiredIPs))
	for _, ip := range action.DesiredIPs {
		ips[ip] = struct{}{}
	}
	return map[engine.ASGTarget]engine.DesiredPrefixSet{
		action.Target: {IPs: ips},
	}
}

// buildSingleTargetActual builds a single-entry actual map from Get result for diff recomputation.
func buildSingleTargetActual(action engine.Action, current *AddressPrefixSet, getErr error) (map[engine.ASGTarget]engine.ActualPrefixSet, error) {
	if getErr != nil {
		if errors.Is(getErr, ErrNotFound) {
			// Resource doesn't exist — empty actual map triggers Create
			return map[engine.ASGTarget]engine.ActualPrefixSet{}, nil
		}
		return nil, pkgerrors.Wrap(getErr, "Get failed")
	}
	if current == nil {
		// Defensive fallback for a violated client contract: treat nil current with
		// nil error as absent so retry recompute cannot turn a recreate into an
		// update against an assumed empty existing resource.
		return map[engine.ASGTarget]engine.ActualPrefixSet{}, nil
	}

	ips := make(map[string]struct{})
	if current.Properties != nil {
		for _, ip := range current.Properties.AddressPrefixes {
			ips[ip] = struct{}{}
		}
	}

	return map[engine.ASGTarget]engine.ActualPrefixSet{
		action.Target: {IPs: ips},
	}, nil
}
