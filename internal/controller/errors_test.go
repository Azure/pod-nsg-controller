package controller

import (
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/engine"
	"go.uber.org/zap/zaptest"
)

// -----------------------------------------------------------------------
// T7.4 / T7.5 – Classify action results
// -----------------------------------------------------------------------

func TestClassifyActionResults_MixedResults(t *testing.T) {
	_ = zaptest.NewLogger(t)

	results := []azure.ActionResult{
		{Action: makeTestAction("target-1", engine.UpdatePrefixSet), Success: true, Err: nil},
		{Action: makeTestAction("target-2", engine.UpdatePrefixSet), Success: false, Err: &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}},
		{Action: makeTestAction("target-3", engine.UpdatePrefixSet), Success: true, Err: nil},
		{Action: makeTestAction("target-4", engine.UpdatePrefixSet), Success: false, Err: &azure.ARMStatusError{StatusCode: 429, ARMCode: "TooManyRequests", Message: "throttled"}},
		{Action: makeTestAction("target-5", engine.UpdatePrefixSet), Success: false, Err: &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"}},
	}

	summary := ClassifyActionResults(results)

	// 3 failures total
	if len(summary.Failures) != 3 {
		t.Errorf("ClassifyActionResults() Failures count = %d, want 3", len(summary.Failures))
	}

	// 500 and 429 are retriable
	if summary.RetriableCount != 2 {
		t.Errorf("ClassifyActionResults() RetriableCount = %d, want 2", summary.RetriableCount)
	}

	// 403 is non-retriable
	if summary.NonRetriableCount != 1 {
		t.Errorf("ClassifyActionResults() NonRetriableCount = %d, want 1", summary.NonRetriableCount)
	}
}

func TestClassifyActionResults_AllSuccess(t *testing.T) {
	_ = zaptest.NewLogger(t)

	results := []azure.ActionResult{
		{Action: makeTestAction("target-1", engine.UpdatePrefixSet), Success: true, Err: nil},
		{Action: makeTestAction("target-2", engine.UpdatePrefixSet), Success: true, Err: nil},
	}

	summary := ClassifyActionResults(results)

	if len(summary.Failures) != 0 {
		t.Errorf("ClassifyActionResults() Failures count = %d, want 0", len(summary.Failures))
	}
	if summary.RetriableCount != 0 {
		t.Errorf("ClassifyActionResults() RetriableCount = %d, want 0", summary.RetriableCount)
	}
	if summary.NonRetriableCount != 0 {
		t.Errorf("ClassifyActionResults() NonRetriableCount = %d, want 0", summary.NonRetriableCount)
	}
}

func TestClassifyActionResults_404ASGNotFound_IsNonRetriable(t *testing.T) {
	_ = zaptest.NewLogger(t)

	armErr := &azure.ARMStatusError{
		StatusCode: 404,
		ARMCode:    "ApplicationSecurityGroupNotFound",
		Message:    "ASG not found",
	}

	results := []azure.ActionResult{
		{Action: makeTestAction("target-1", engine.UpdatePrefixSet), Success: false, Err: armErr},
	}

	summary := ClassifyActionResults(results)

	if summary.NonRetriableCount != 1 {
		t.Errorf("ClassifyActionResults() NonRetriableCount = %d, want 1 for 404-ASG-not-found", summary.NonRetriableCount)
	}
	if summary.RetriableCount != 0 {
		t.Errorf("ClassifyActionResults() RetriableCount = %d, want 0 for 404-ASG-not-found", summary.RetriableCount)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("ClassifyActionResults() Failures count = %d, want 1", len(summary.Failures))
	}
	if summary.Failures[0].Class != ErrorClassNonRetriable {
		t.Errorf("ClassifyActionResults() Failures[0].Class = %q, want %q", summary.Failures[0].Class, ErrorClassNonRetriable)
	}
	if summary.Failures[0].Reason != "404-ASG-not-found" {
		t.Errorf("ClassifyActionResults() Failures[0].Reason = %q, want %q", summary.Failures[0].Reason, "404-ASG-not-found")
	}
}

// -----------------------------------------------------------------------
// T7.5 – 3 of 5 actions fail → 2 succeed and persist, 3 report errors, reconcile requeued
// -----------------------------------------------------------------------
func TestPhase7_T75_PartialFailure_DecideRequeueFromActionSummary(t *testing.T) {
	_ = zaptest.NewLogger(t)

	t.Run("retriable_failures_requeue_with_backoff", func(t *testing.T) {
		summary := ActionFailureSummary{
			Failures: []ClassifiedActionFailure{
				{Class: ErrorClassRetriable, Reason: "500"},
				{Class: ErrorClassRetriable, Reason: "500"},
				{Class: ErrorClassRetriable, Reason: "429"},
			},
			RetriableCount:    3,
			NonRetriableCount: 0,
			MaxRetryAfterHint: 0,
		}

		policy := RequeuePolicy{
			ResyncInterval:            30 * time.Second,
			ExhaustedRetryBackoff:     8 * time.Second,
			MaxRetryAfterRequeueDelay: 5 * time.Minute,
		}

		result := DecideRequeueFromActionSummary(summary, policy)

		if result.RequeueAfter < 8*time.Second {
			t.Errorf("DecideRequeueFromActionSummary() RequeueAfter = %v, want >= 8s for retriable", result.RequeueAfter)
		}
	})

	t.Run("only_non_retriable_requeue_at_resync", func(t *testing.T) {
		summary := ActionFailureSummary{
			Failures: []ClassifiedActionFailure{
				{Class: ErrorClassNonRetriable, Reason: "403"},
			},
			RetriableCount:    0,
			NonRetriableCount: 1,
			MaxRetryAfterHint: 0,
		}

		policy := RequeuePolicy{
			ResyncInterval:            30 * time.Second,
			ExhaustedRetryBackoff:     8 * time.Second,
			MaxRetryAfterRequeueDelay: 5 * time.Minute,
		}

		result := DecideRequeueFromActionSummary(summary, policy)

		if result.RequeueAfter != 30*time.Second {
			t.Errorf("DecideRequeueFromActionSummary() RequeueAfter = %v, want %v for non-retriable only",
				result.RequeueAfter, 30*time.Second)
		}
	})
}

// -----------------------------------------------------------------------
// DecideRequeueFromActionSummary: Retry-After hint priority
// -----------------------------------------------------------------------
func TestDecideRequeueFromActionSummary_RetryAfterHintPriority(t *testing.T) {
	_ = zaptest.NewLogger(t)

	summary := ActionFailureSummary{
		Failures: []ClassifiedActionFailure{
			{Class: ErrorClassRetriable, Reason: "429", RetryAfterHint: 30 * time.Second},
			{Class: ErrorClassRetriable, Reason: "500"},
		},
		RetriableCount:    2,
		NonRetriableCount: 0,
		MaxRetryAfterHint: 30 * time.Second,
	}

	policy := RequeuePolicy{
		ResyncInterval:            30 * time.Second,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}

	result := DecideRequeueFromActionSummary(summary, policy)

	// Retry-After hint (30s) should take priority over default backoff (8s).
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("DecideRequeueFromActionSummary() RequeueAfter = %v, want 30s (Retry-After hint)", result.RequeueAfter)
	}
}

func TestDecideRequeueFromActionSummary_RetryAfterHintClamped(t *testing.T) {
	_ = zaptest.NewLogger(t)

	summary := ActionFailureSummary{
		Failures: []ClassifiedActionFailure{
			{Class: ErrorClassRetriable, Reason: "429", RetryAfterHint: 10 * time.Minute},
		},
		RetriableCount:    1,
		NonRetriableCount: 0,
		MaxRetryAfterHint: 10 * time.Minute,
	}

	policy := RequeuePolicy{
		ResyncInterval:            30 * time.Second,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}

	result := DecideRequeueFromActionSummary(summary, policy)

	// Should be clamped to MaxRetryAfterRequeueDelay.
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("DecideRequeueFromActionSummary() RequeueAfter = %v, want 5m (clamped)", result.RequeueAfter)
	}
}

// -----------------------------------------------------------------------
// DecideRequeueFromSystemError
// -----------------------------------------------------------------------
func TestDecideRequeueFromSystemError_TransientBackoff(t *testing.T) {
	_ = zaptest.NewLogger(t)

	transientErr := &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "oops"}

	policy := RequeuePolicy{
		ResyncInterval:            30 * time.Second,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}

	result := DecideRequeueFromSystemError(transientErr, policy)

	if result.RequeueAfter < 8*time.Second {
		t.Errorf("DecideRequeueFromSystemError(500) RequeueAfter = %v, want >= 8s", result.RequeueAfter)
	}
}

func TestDecideRequeueFromSystemError_AuthAndASGNotFound_ResyncOnly(t *testing.T) {
	_ = zaptest.NewLogger(t)

	tests := []struct {
		name string
		err  error
	}{
		{"403 auth error", &azure.ARMStatusError{StatusCode: 403, ARMCode: "AuthorizationFailed", Message: "forbidden"}},
		{"404 ASG not found", &azure.ARMStatusError{StatusCode: 404, ARMCode: "ApplicationSecurityGroupNotFound", Message: "not found"}},
	}

	policy := RequeuePolicy{
		ResyncInterval:            30 * time.Second,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := DecideRequeueFromSystemError(tc.err, policy)

			if result.RequeueAfter != 30*time.Second {
				t.Errorf("DecideRequeueFromSystemError(%s) RequeueAfter = %v, want %v (resync only)",
					tc.name, result.RequeueAfter, 30*time.Second)
			}
		})
	}
}

func TestDefaultRequeuePolicy(t *testing.T) {
	_ = zaptest.NewLogger(t)
	resync := 30 * time.Second
	policy := DefaultRequeuePolicy(resync)

	if policy.ResyncInterval != resync {
		t.Errorf("DefaultRequeuePolicy().ResyncInterval = %v, want %v", policy.ResyncInterval, resync)
	}
	if policy.ExhaustedRetryBackoff != 8*time.Second {
		t.Errorf("DefaultRequeuePolicy().ExhaustedRetryBackoff = %v, want 8s", policy.ExhaustedRetryBackoff)
	}
	if policy.MaxRetryAfterRequeueDelay != 5*time.Minute {
		t.Errorf("DefaultRequeuePolicy().MaxRetryAfterRequeueDelay = %v, want 5m", policy.MaxRetryAfterRequeueDelay)
	}
}

func TestDecideRequeueFromSystemError_RetryAfterHintPriorityAndClamp(t *testing.T) {
	_ = zaptest.NewLogger(t)

	policy := RequeuePolicy{
		ResyncInterval:            30 * time.Second,
		ExhaustedRetryBackoff:     8 * time.Second,
		MaxRetryAfterRequeueDelay: 5 * time.Minute,
	}

	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "uses larger retry-after hint",
			err: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				Message:    "throttled",
				RetryAfter: 30 * time.Second,
			},
			want: 30 * time.Second,
		},
		{
			name: "clamps retry-after hint to policy maximum",
			err: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				Message:    "throttled",
				RetryAfter: 10 * time.Minute,
			},
			want: 5 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRequeueFromSystemError(tc.err, policy)
			if got.RequeueAfter != tc.want {
				t.Errorf("DecideRequeueFromSystemError() RequeueAfter = %v, want %v", got.RequeueAfter, tc.want)
			}
		})
	}
}

func TestClassifyActionResults_PartialFailures_PreservesSuccessfulActions(t *testing.T) {
	_ = zaptest.NewLogger(t)

	success1 := makeTestAction("target-1", engine.UpdatePrefixSet)
	success2 := makeTestAction("target-3", engine.UpdatePrefixSet)
	failure1 := makeTestAction("target-2", engine.UpdatePrefixSet)
	failure2 := makeTestAction("target-4", engine.UpdatePrefixSet)
	failure3 := makeTestAction("target-5", engine.UpdatePrefixSet)

	results := []azure.ActionResult{
		{Action: success1, Success: true},
		{
			Action:  failure1,
			Success: false,
			Err: &azure.ARMStatusError{
				StatusCode: 500,
				ARMCode:    "InternalServerError",
				Message:    "oops",
			},
		},
		{Action: success2, Success: true},
		{
			Action:  failure2,
			Success: false,
			Err: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				Message:    "throttled",
				RetryAfter: 20 * time.Second,
			},
		},
		{
			Action:  failure3,
			Success: false,
			Err: &azure.ARMStatusError{
				StatusCode: 403,
				ARMCode:    "AuthorizationFailed",
				Message:    "forbidden",
			},
		},
	}

	summary := ClassifyActionResults(results)

	if got, want := len(summary.Failures), 3; got != want {
		t.Errorf("len(summary.Failures) = %d, want %d", got, want)
	}
	if got, want := summary.RetriableCount, 2; got != want {
		t.Errorf("summary.RetriableCount = %d, want %d", got, want)
	}
	if got, want := summary.NonRetriableCount, 1; got != want {
		t.Errorf("summary.NonRetriableCount = %d, want %d", got, want)
	}
	if got, want := summary.MaxRetryAfterHint, 20*time.Second; got != want {
		t.Errorf("summary.MaxRetryAfterHint = %v, want %v", got, want)
	}

	for _, failure := range summary.Failures {
		if failure.Result.Action.Target.ASGName == success1.Target.ASGName {
			t.Errorf("summary.Failures unexpectedly included successful action target %q", success1.Target.ASGName)
		}
		if failure.Result.Action.Target.ASGName == success2.Target.ASGName {
			t.Errorf("summary.Failures unexpectedly included successful action target %q", success2.Target.ASGName)
		}
	}
}

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Phase 7 Acceptance: T7.5 Partial Failure Report
// ---------------------------------------------------------------------------

func TestPhase7_T75_BuildPartialFailureReport_MixedResults(t *testing.T) {
	_ = zaptest.NewLogger(t)

	results := []azure.ActionResult{
		{Action: makeTestAction("asg-1", engine.UpdatePrefixSet), Err: nil},
		{Action: makeTestAction("asg-2", engine.UpdatePrefixSet), Err: nil},
		{
			Action: makeTestAction("asg-3", engine.UpdatePrefixSet),
			Err: &azure.ARMStatusError{
				StatusCode: 500,
				ARMCode:    "InternalServerError",
				Message:    "fail-1",
			},
		},
		{
			Action: makeTestAction("asg-4", engine.UpdatePrefixSet),
			Err: &azure.ARMStatusError{
				StatusCode: 403,
				ARMCode:    "AuthorizationFailed",
				Message:    "fail-2",
			},
		},
		{
			Action: makeTestAction("asg-5", engine.UpdatePrefixSet),
			Err: &azure.ARMStatusError{
				StatusCode: 429,
				ARMCode:    "TooManyRequests",
				Message:    "fail-3",
				RetryAfter: 5 * time.Second,
			},
		},
	}

	policy := RequeuePolicy{
		ResyncInterval:        10 * time.Second,
		ExhaustedRetryBackoff: 60 * time.Second,
	}

	report := BuildPartialFailureReport(results, v1alpha1.PodASGMappingSpec{}, "test-prefix", policy)

	// T7.5 acceptance: 2 of 5 succeeded.
	if len(report.SucceededTargets) != 2 {
		t.Errorf("T7.5: SucceededTargets = %d, want 2", len(report.SucceededTargets))
	}

	// T7.5 acceptance: 3 of 5 failed.
	if len(report.FailedTargets) != 3 {
		t.Errorf("T7.5: FailedTargets = %d, want 3", len(report.FailedTargets))
	}

	// T7.5 acceptance: RequeueResult should be non-zero (requeue for transient errors).
	if report.RequeueResult.RequeueAfter == 0 && !report.RequeueResult.Requeue {
		t.Errorf("T7.5: RequeueResult is zero, want requeue for transient errors")
	}

	// T7.5 acceptance: Summary should categorize failures.
	totalFailed := report.Summary.RetriableCount + report.Summary.NonRetriableCount
	if totalFailed != 3 {
		t.Errorf("T7.5: Summary total failed (retriable+nonretriable) = %d, want 3", totalFailed)
	}
}

func makeTestAction(asgName string, kind engine.ActionKind) engine.Action {
	return engine.Action{
		Kind: kind,
		Target: engine.ASGTarget{
			SubscriptionID: "test-sub",
			ResourceGroup:  "test-rg",
			ASGName:        asgName,
			FullResourceID: fmt.Sprintf("/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/applicationSecurityGroups/%s", asgName),
			PrefixSetName:  "test-prefix",
		},
		DesiredIPs: []string{"10.0.0.1/32"},
	}
}
