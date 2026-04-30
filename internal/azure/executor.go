package azure

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Azure/pod-nsg-controller/internal/engine"
	"go.uber.org/zap"
)

// ActionResult holds the outcome of executing a single action.
type ActionResult struct {
	Action  engine.Action
	Success bool
	Err     error
}

// armVerb returns the ARM HTTP verb for a given action kind.
func armVerb(kind engine.ActionKind) string {
	switch kind {
	case engine.CreatePrefixSet, engine.UpdatePrefixSet:
		return "PUT"
	case engine.DeletePrefixSet:
		return "DELETE"
	default:
		return "UNKNOWN"
	}
}

// Executor runs engine actions against Azure with bounded concurrency and ETag retry.
type Executor struct {
	log         *zap.Logger
	factory     AddressPrefixSetClientFactory
	maxParallel int
	maxRetries  int
}

// NewExecutor creates an Executor with the given concurrency bound.
func NewExecutor(log *zap.Logger, factory AddressPrefixSetClientFactory, maxParallel int) *Executor {
	if maxParallel < 1 {
		maxParallel = 1
	}
	return &Executor{
		log:         log,
		factory:     factory,
		maxParallel: maxParallel,
		maxRetries:  3,
	}
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
				results[idx] = ActionResult{Action: act, Success: false, Err: fmt.Errorf("getting client: %w", err)}
				return
			}

			err = e.executeWithETagRetry(ctx, client, act)
			results[idx] = ActionResult{
				Action:  act,
				Success: err == nil,
				Err:     err,
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
	case engine.DeletePrefixSet:
		return client.Delete(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
	default:
		return fmt.Errorf("unknown action kind: %s", action.Kind)
	}
}

func (e *Executor) executeWithETagRetry(ctx context.Context, client AddressPrefixSetAPI, action engine.Action) error {
	var err error
	for attempt := 1; attempt <= e.maxRetries; attempt++ {
		// Check context before each attempt
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err = e.executeAction(ctx, client, action)
		if err == nil {
			return nil
		}
		if !IsPreconditionFailed(err) {
			return err
		}

		if attempt == e.maxRetries {
			e.log.Warn("ETag conflict, retries exhausted",
				zap.String("operation", armVerb(action.Kind)),
				zap.String("actionKind", string(action.Kind)),
				zap.String("prefixSetName", action.Target.PrefixSetName),
				zap.Int("attempt", attempt),
				zap.Int("maxRetries", e.maxRetries),
				zap.Error(err),
			)
			break
		}
		e.log.Warn("ETag conflict, retrying",
			zap.String("operation", armVerb(action.Kind)),
			zap.String("actionKind", string(action.Kind)),
			zap.String("prefixSetName", action.Target.PrefixSetName),
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", e.maxRetries),
			zap.Error(err),
		)

		// Check context before recompute
		if ctx.Err() != nil {
			return ctx.Err()
		}

		t := action.Target
		// Retry recompute is based on a single GET snapshot here, but the subsequent
		// Put performs its own internal GET to acquire the conditional ETag. If the
		// resource changes again between those calls, the retry can still succeed
		// with a newer ETag while applying DesiredIPs derived from this slightly
		// older snapshot. A later reconcile will converge any drift.
		current, getErr := client.Get(ctx, t.SubscriptionID, t.ResourceGroup, t.ASGName, t.PrefixSetName)
		next, done, recomputeErr := recomputeSingleTargetActionViaDiff(action, current, getErr)
		if recomputeErr != nil {
			return fmt.Errorf("recompute after 412: %w", recomputeErr)
		}
		if done {
			return nil
		}
		if next != nil {
			action = *next
		}
	}
	return err
}

// recomputeSingleTargetActionViaDiff re-GETs the resource and recomputes the diff
// using engine.ComputeDiff to determine the next action.
func recomputeSingleTargetActionViaDiff(action engine.Action, current *AddressPrefixSet, getErr error) (next *engine.Action, done bool, err error) {
	desired := buildSingleTargetDesired(action)
	actual, err := buildSingleTargetActual(action, current, getErr)
	if err != nil {
		return nil, false, fmt.Errorf("building actual state: %w", err)
	}

	actions := engine.ComputeDiff(desired, actual)

	if len(actions) == 0 {
		return nil, true, nil
	}
	if len(actions) > 1 {
		return nil, false, fmt.Errorf("recompute produced %d actions, expected 0 or 1", len(actions))
	}

	return &actions[0], false, nil
}

// buildSingleTargetDesired builds a single-entry desired map for diff recomputation.
// For DELETE actions, returns an empty map so ComputeDiff produces DeletePrefixSet.
func buildSingleTargetDesired(action engine.Action) map[engine.ASGTarget]engine.DesiredPrefixSet {
	if action.Kind == engine.DeletePrefixSet {
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
		return nil, fmt.Errorf("Get failed: %w", getErr)
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
