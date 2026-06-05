package azure

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"strconv"
	"time"
)

// jitterRng is a package-local random source for retry jitter, seeded from
// crypto/rand to ensure non-deterministic backoff across controller
// restarts and replicas.
var jitterRng = newCryptoSeededRand()

func newCryptoSeededRand() *mathrand.Rand {
	var seed int64
	if err := binary.Read(rand.Reader, binary.LittleEndian, &seed); err != nil {
		seed = time.Now().UnixNano()
	}
	return mathrand.New(mathrand.NewSource(seed))
}

// ARMOperation identifies the ARM operation being performed.
type ARMOperation string

const (
	ARMOperationGetPrefixSet    ARMOperation = "GetPrefixSet"
	ARMOperationPutPrefixSet    ARMOperation = "PutPrefixSet"
	ARMOperationDeletePrefixSet ARMOperation = "DeletePrefixSet"
	ARMOperationListPrefixSets  ARMOperation = "ListPrefixSets"
	ARMOperationPollLRO         ARMOperation = "PollLRO"
)

// RetryPolicy configures retry behavior for ARM calls.
type RetryPolicy struct {
	MaxRetries        int           // retries after initial attempt; default 3
	NetworkMaxRetries int           // retries after initial network failure; default 1
	BaseDelay         time.Duration // default 1s
	MaxDelay          time.Duration // default 8s
	JitterFactor      float64       // default 0.2
}

// RetryDecision indicates whether a retry should be attempted.
type RetryDecision struct {
	Retry       bool
	Delay       time.Duration
	RetryReason string
}

// RetryContext provides contextual info for retry logging.
type RetryContext struct {
	Operation      ARMOperation
	Method         string
	URL            string
	SubscriptionID string
	ResourceGroup  string
	ASGName        string
	PrefixSetName  string
	SkipMetrics    bool // If true, doRequest suppresses armObserver metrics for successful (2xx) responses only; transport errors and non-2xx responses are still emitted.
}

// DefaultRetryPolicy returns the default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:        3,
		NetworkMaxRetries: 1,
		BaseDelay:         1 * time.Second,
		MaxDelay:          8 * time.Second,
		JitterFactor:      0.2,
	}
}

// DecideRetry determines whether to retry based on the error and attempt number.
// attemptNumber is 1-based total call count. retriesUsed = attemptNumber - 1.
// Retry is allowed only when retriesUsed < MaxRetries (or NetworkMaxRetries for network errors).
func DecideRetry(err error, attemptNumber int, p RetryPolicy) RetryDecision {
	if err == nil {
		return RetryDecision{Retry: false}
	}

	retriesUsed := attemptNumber - 1

	var armErr *ARMStatusError
	if errors.As(err, &armErr) {
		// 412 is owned by executor, not generic retry.
		if armErr.StatusCode == 412 {
			return RetryDecision{Retry: false}
		}

		if isRetriableStatusCode(armErr.StatusCode) {
			if retriesUsed >= p.MaxRetries {
				return RetryDecision{Retry: false}
			}
			delay := computeBackoff(retriesUsed, p)
			reason := "server-error"
			if armErr.StatusCode == 429 && armErr.RetryAfter > 0 {
				delay = armErr.RetryAfter
				reason = "429-retry-after"
			}
			return RetryDecision{Retry: true, Delay: delay, RetryReason: reason}
		}

		// Non-retriable ARM errors (401, 403, 404, 409, etc.)
		return RetryDecision{Retry: false}
	}

	// Network errors use NetworkMaxRetries.
	if isNetworkError(err) {
		if retriesUsed >= p.NetworkMaxRetries {
			return RetryDecision{Retry: false}
		}
		delay := computeBackoff(retriesUsed, p)
		return RetryDecision{Retry: true, Delay: delay, RetryReason: "network"}
	}

	return RetryDecision{Retry: false}
}

func computeBackoff(retriesUsed int, p RetryPolicy) time.Duration {
	delay := float64(p.BaseDelay) * math.Pow(2, float64(retriesUsed))
	if delay > float64(p.MaxDelay) {
		delay = float64(p.MaxDelay)
	}
	if p.JitterFactor > 0 {
		jitter := delay * p.JitterFactor * (jitterRng.Float64()*2 - 1)
		delay += jitter
	}
	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

func isRetriableStatusCode(statusCode int) bool {
	return statusCode == 408 || statusCode == 429 || statusCode >= 500
}

func isNetworkError(err error) bool {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return false
}

// ParseRetryAfterValue parses a Retry-After header value (delta-seconds or HTTP-date).
func ParseRetryAfterValue(value string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	// http.ParseTime tries RFC1123, RFC850, and ANSI C asctime formats.
	t, err := http.ParseTime(value)
	if err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// ExtractRetryAfterHint extracts a Retry-After duration from an error.
func ExtractRetryAfterHint(err error) time.Duration {
	var armErr *ARMStatusError
	if errors.As(err, &armErr) {
		return armErr.RetryAfter
	}
	return 0
}

// IsRetriableARM returns true if the error is retriable (408, 429, 5xx, network).
func IsRetriableARM(err error) bool {
	var armErr *ARMStatusError
	if errors.As(err, &armErr) {
		return isRetriableStatusCode(armErr.StatusCode)
	}
	return isNetworkError(err)
}

// IsAuthFailure returns true if the error is a 401 or 403.
func IsAuthFailure(err error) bool {
	var armErr *ARMStatusError
	if !errors.As(err, &armErr) {
		return false
	}
	return armErr.StatusCode == 401 || armErr.StatusCode == 403
}

// IsParentASGNotFound returns true if the error indicates the parent ASG is missing.
func IsParentASGNotFound(err error) bool {
	var armErr *ARMStatusError
	if !errors.As(err, &armErr) {
		return false
	}
	if armErr.StatusCode != 404 {
		return false
	}
	switch armErr.ARMCode {
	case "ApplicationSecurityGroupNotFound",
		"ParentResourceNotFound",
		"ResourceGroupNotFound",
		"SubscriptionNotFound":
		return true
	}
	return false
}

// IsChildPrefixSetNotFound returns true if the error indicates a child prefix set is missing.
func IsChildPrefixSetNotFound(err error) bool {
	var armErr *ARMStatusError
	if !errors.As(err, &armErr) {
		return false
	}
	if armErr.StatusCode != 404 {
		return false
	}
	return armErr.ARMCode == "AddressPrefixSetNotFound"
}
