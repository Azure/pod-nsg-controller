package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ARMObserver is the interface components use to report ARM request metrics.
type ARMObserver interface {
	ObserveRequest(subscriptionID, operation, statusCode string, duration time.Duration)
}

// ARMRetryObserver is the interface for reporting ARM retry metrics.
type ARMRetryObserver interface {
	ObserveRetry(subscriptionID, operation, retryReason string)
}

// ARMRateLimitObserver is the interface for reporting rate limit metrics.
type ARMRateLimitObserver interface {
	ObserveRateLimitDelay(subscriptionID string, delay time.Duration)
}

// ARMRecorder records ARM call Prometheus metrics.
type ARMRecorder struct {
	requestsTotal      *prometheus.CounterVec
	requestDuration    *prometheus.HistogramVec
	retriesTotal       *prometheus.CounterVec
	rateLimitDelays    *prometheus.CounterVec
	rateLimitDuration  *prometheus.HistogramVec
}

func newARMRecorder() *ARMRecorder {
	return &ARMRecorder{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "arm_requests_total",
			Help:      "Total ARM API calls by operation and response status",
		}, []string{"subscription_id", "operation", "status_code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "arm_request_duration_seconds",
			Help:      "ARM call latency distribution",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"subscription_id", "operation"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "arm_retries_total",
			Help:      "Total retry attempts by reason",
		}, []string{"subscription_id", "operation", "retry_reason"}),
		rateLimitDelays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pod_nsg_controller",
			Name:      "arm_rate_limit_delays_total",
			Help:      "Times a call was delayed by the per-subscription rate limiter",
		}, []string{"subscription_id"}),
		rateLimitDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pod_nsg_controller",
			Name:      "arm_rate_limit_delay_seconds",
			Help:      "Duration of rate-limiter-imposed delays",
		}, []string{"subscription_id"}),
	}
}

// Collectors returns all collectors for this recorder.
func (r *ARMRecorder) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		r.requestsTotal,
		r.requestDuration,
		r.retriesTotal,
		r.rateLimitDelays,
		r.rateLimitDuration,
	}
}

// ObserveRequest records a completed ARM request.
func (r *ARMRecorder) ObserveRequest(subscriptionID, operation, statusCode string, duration time.Duration) {
	r.requestsTotal.WithLabelValues(subscriptionID, operation, statusCode).Inc()
	r.requestDuration.WithLabelValues(subscriptionID, operation).Observe(duration.Seconds())
}

// ObserveRetry records a retry attempt.
func (r *ARMRecorder) ObserveRetry(subscriptionID, operation, retryReason string) {
	r.retriesTotal.WithLabelValues(subscriptionID, operation, retryReason).Inc()
}

// ObserveRateLimitDelay records a rate limiter delay.
func (r *ARMRecorder) ObserveRateLimitDelay(subscriptionID string, delay time.Duration) {
	r.rateLimitDelays.WithLabelValues(subscriptionID).Inc()
	r.rateLimitDuration.WithLabelValues(subscriptionID).Observe(delay.Seconds())
}
