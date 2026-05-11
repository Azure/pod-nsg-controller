package azure

import (
	"net/http"
	"time"
)

// WithRetryPolicy sets the retry policy for the client.
func WithRetryPolicy(p RetryPolicy) AddressPrefixSetClientOption {
	return func(c *AddressPrefixSetClient) {
		c.retryPolicy = p
	}
}

// WithSubscriptionRateLimiter sets the per-subscription rate limiter for the client.
func WithSubscriptionRateLimiter(l SubscriptionRateLimiter) AddressPrefixSetClientOption {
	return func(c *AddressPrefixSetClient) {
		c.rateLimiter = l
	}
}

// ParseARMErrorWithContext parses an ARM error response, extracting metadata from
// HTTP headers (e.g., Retry-After) and the retry context.
func ParseARMErrorWithContext(statusCode int, body []byte, headers http.Header, retryCtx RetryContext) *ARMStatusError {
	armErr := parseARMError(statusCode, body)
	armErr.Operation = retryCtx.Operation
	armErr.RequestURL = retryCtx.URL

	if raValue := headers.Get("Retry-After"); raValue != "" {
		if d, ok := ParseRetryAfterValue(raValue, time.Now()); ok {
			armErr.RetryAfter = d
		}
	}

	return armErr
}
