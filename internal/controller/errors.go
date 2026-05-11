package controller

import (
	"errors"
	"fmt"
	"time"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ErrorClass classifies reconcile errors.
type ErrorClass string

const (
	ErrorClassRetriable    ErrorClass = "Retriable"
	ErrorClassNonRetriable ErrorClass = "NonRetriable"
)

// ClassifiedActionFailure wraps an action result with classification.
type ClassifiedActionFailure struct {
	Result         azure.ActionResult
	Class          ErrorClass
	Reason         string
	RetryAfterHint time.Duration
}

// ActionFailureSummary aggregates classified action failures.
type ActionFailureSummary struct {
	Failures          []ClassifiedActionFailure
	RetriableCount    int
	NonRetriableCount int
	MaxRetryAfterHint time.Duration
}

// RequeuePolicy configures requeue behavior based on error classification.
type RequeuePolicy struct {
	ResyncInterval            time.Duration
	ExhaustedRetryBackoff     time.Duration
	MaxRetryAfterRequeueDelay time.Duration
}

// DefaultRequeuePolicy returns the default requeue policy.
func DefaultRequeuePolicy(resyncInterval time.Duration) RequeuePolicy {
	return RequeuePolicy{
		ResyncInterval:            resyncInterval,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}
}

// ClassifyActionResults classifies action results into retriable and non-retriable.
func ClassifyActionResults(results []azure.ActionResult) ActionFailureSummary {
	var summary ActionFailureSummary
	for _, r := range results {
		if r.Success {
			continue
		}
		failure := classifySingleFailure(r)
		summary.Failures = append(summary.Failures, failure)
		switch failure.Class {
		case ErrorClassRetriable:
			summary.RetriableCount++
		case ErrorClassNonRetriable:
			summary.NonRetriableCount++
		}
		if failure.RetryAfterHint > summary.MaxRetryAfterHint {
			summary.MaxRetryAfterHint = failure.RetryAfterHint
		}
	}
	return summary
}

func classifySingleFailure(r azure.ActionResult) ClassifiedActionFailure {
	failure := ClassifiedActionFailure{
		Result: r,
	}

	if r.Err == nil {
		failure.Class = ErrorClassNonRetriable
		failure.Reason = "unknown"
		return failure
	}

	// Parent ASG not found is non-retriable (check before generic retriable).
	if azure.IsParentASGNotFound(r.Err) {
		failure.Class = ErrorClassNonRetriable
		failure.Reason = "404-ASG-not-found"
		return failure
	}

	// Retriable: 429, 5xx, network errors.
	if azure.IsRetriableARM(r.Err) {
		failure.Class = ErrorClassRetriable
		failure.RetryAfterHint = azure.ExtractRetryAfterHint(r.Err)
		var armErr *azure.ARMStatusError
		if errors.As(r.Err, &armErr) {
			if armErr.StatusCode == 429 {
				failure.Reason = "429"
			} else {
				failure.Reason = fmt.Sprintf("%dxx", armErr.StatusCode/100)
			}
		} else {
			failure.Reason = "network"
		}
		return failure
	}

	// Auth failures (401, 403) are non-retriable.
	if azure.IsAuthFailure(r.Err) {
		failure.Class = ErrorClassNonRetriable
		var armErr *azure.ARMStatusError
		if errors.As(r.Err, &armErr) {
			failure.Reason = fmt.Sprintf("%d", armErr.StatusCode)
		} else {
			failure.Reason = "auth"
		}
		return failure
	}

	failure.Class = ErrorClassNonRetriable
	failure.Reason = "unknown"
	return failure
}

// DecideRequeueFromActionSummary determines the requeue result from classified failures.
func DecideRequeueFromActionSummary(summary ActionFailureSummary, p RequeuePolicy) ctrl.Result {
	if summary.RetriableCount == 0 && summary.NonRetriableCount == 0 {
		return ctrl.Result{RequeueAfter: p.ResyncInterval}
	}

	if summary.RetriableCount > 0 {
		delay := p.ExhaustedRetryBackoff
		if summary.MaxRetryAfterHint > delay {
			delay = summary.MaxRetryAfterHint
		}
		if p.MaxRetryAfterRequeueDelay > 0 && delay > p.MaxRetryAfterRequeueDelay {
			delay = p.MaxRetryAfterRequeueDelay
		}
		return ctrl.Result{RequeueAfter: delay}
	}

	// Only non-retriable failures.
	return ctrl.Result{RequeueAfter: p.ResyncInterval}
}

// DecideRequeueFromSystemError determines the requeue result from a system-level error.
func DecideRequeueFromSystemError(err error, p RequeuePolicy) ctrl.Result {
	if err == nil {
		return ctrl.Result{RequeueAfter: p.ResyncInterval}
	}

	// Non-retriable: auth failure or parent ASG not found.
	if azure.IsAuthFailure(err) || azure.IsParentASGNotFound(err) {
		return ctrl.Result{RequeueAfter: p.ResyncInterval}
	}

	// Retriable: 429, 5xx, network.
	if azure.IsRetriableARM(err) {
		delay := p.ExhaustedRetryBackoff
		hint := azure.ExtractRetryAfterHint(err)
		if hint > delay {
			delay = hint
		}
		if p.MaxRetryAfterRequeueDelay > 0 && delay > p.MaxRetryAfterRequeueDelay {
			delay = p.MaxRetryAfterRequeueDelay
		}
		return ctrl.Result{RequeueAfter: delay}
	}

	// Default: treat as retriable with backoff.
	return ctrl.Result{RequeueAfter: p.ExhaustedRetryBackoff}
}
