package azure

import (
	"time"

	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

// armRequestObserver is the local interface matching metrics.ARMObserver.
type armRequestObserver interface {
	ObserveRequest(subscriptionID, operation, statusCode string, duration time.Duration)
}

// armRetryObserver is the local interface matching metrics.ARMRetryObserver.
type armRetryObserver interface {
	ObserveRetry(subscriptionID, operation, retryReason string)
}

// armRateLimitObserver is the local interface matching metrics.ARMRateLimitObserver.
type armRateLimitObserver interface {
	ObserveRateLimitDelay(subscriptionID string, delay time.Duration)
}

// WithARMMetrics adds metrics instrumentation to the AddressPrefixSetClient.
// The observer records every HTTP attempt; the retry observer records retry decisions.
func WithARMMetrics(obs metrics.ARMObserver, retryObs metrics.ARMRetryObserver) AddressPrefixSetClientOption {
	return func(c *AddressPrefixSetClient) {
		c.armObserver = obs
		c.armRetryObserver = retryObs
	}
}

// WithFactoryARMMetrics adds ARM metrics observers to clients created by the factory.
func WithFactoryARMMetrics(obs metrics.ARMObserver, retryObs metrics.ARMRetryObserver) ClientFactoryOption {
	return func(f *ClientFactory) {
		f.armObserver = obs
		f.armRetryObserver = retryObs
	}
}

// ExecutorOption configures an Executor.
type ExecutorOption func(*Executor)

// WithExecutorRetryMetrics adds retry metrics to the executor's ETag retry path.
func WithExecutorRetryMetrics(obs metrics.ARMRetryObserver) ExecutorOption {
	return func(e *Executor) {
		e.retryObserver = obs
	}
}

// WithRateLimitMetrics adds rate-limit delay metrics to the ARMRateLimiter.
// Emits only when delay > 0.
func WithRateLimitMetrics(obs metrics.ARMRateLimitObserver) ARMRateLimiterOption {
	return func(l *ARMRateLimiter) {
		l.rateLimitObserver = obs
	}
}
