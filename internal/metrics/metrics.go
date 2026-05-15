package metrics

import (
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Recorder holds references to all sub-recorders for the pod-nsg-controller metrics subsystem.
type Recorder struct {
	PodChurn    *PodChurnRecorder
	ARM         *ARMRecorder
	Convergence *ConvergenceRecorder
	Reconcile   *ReconcileRecorder
}

var (
	registerOnce sync.Once
	singleton    *Recorder
)

// MustRegister registers all metrics and panics on failure.
func MustRegister() *Recorder {
	r, err := Register()
	if err != nil {
		panic(err)
	}
	return r
}

// Register registers all metrics with the default Prometheus registry.
// It is safe to call multiple times; only the first call performs registration.
func Register() (*Recorder, error) {
	var regErr error
	registerOnce.Do(func() {
		singleton = &Recorder{
			PodChurn:    newPodChurnRecorder(),
			ARM:         newARMRecorder(),
			Convergence: newConvergenceRecorder(),
			Reconcile:   newReconcileRecorder(),
		}
	})
	return singleton, regErr
}

// RegisterWith registers all metrics with the given prometheus.Registerer.
// On AlreadyRegisteredError, it rebinds recorder fields to the existing collectors.
// On type mismatch, it returns an error.
func RegisterWith(reg prometheus.Registerer) (*Recorder, error) {
	rec := &Recorder{
		PodChurn:    newPodChurnRecorder(),
		ARM:         newARMRecorder(),
		Convergence: newConvergenceRecorder(),
		Reconcile:   newReconcileRecorder(),
	}

	if err := registerCollectors(reg, rec.PodChurn.Collectors(), func(i int, c prometheus.Collector) {
		rebindPodChurnCollector(rec.PodChurn, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.ARM.Collectors(), func(i int, c prometheus.Collector) {
		rebindARMCollector(rec.ARM, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.Convergence.Collectors(), func(i int, c prometheus.Collector) {
		rebindConvergenceCollector(rec.Convergence, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.Reconcile.Collectors(), func(i int, c prometheus.Collector) {
		rebindReconcileCollector(rec.Reconcile, i, c)
	}); err != nil {
		return nil, err
	}

	return rec, nil
}

// registerCollectors registers a slice of collectors, calling rebindFn with
// the existing collector on AlreadyRegisteredError. Returns error on type mismatch.
func registerCollectors(reg prometheus.Registerer, collectors []prometheus.Collector, rebindFn func(int, prometheus.Collector)) error {
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			var alreadyRegistered prometheus.AlreadyRegisteredError
			if errors.As(err, &alreadyRegistered) {
				rebindFn(i, alreadyRegistered.ExistingCollector)
				continue
			}
			return fmt.Errorf("registering collector: %w", err)
		}
	}
	return nil
}

func rebindPodChurnCollector(r *PodChurnRecorder, i int, c prometheus.Collector) {
	switch i {
	case 0:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.podIPChangesTotal = cv
		}
	case 1:
		if gv, ok := c.(*prometheus.GaugeVec); ok {
			r.podChurnRate = gv
		}
	}
}

func rebindARMCollector(r *ARMRecorder, i int, c prometheus.Collector) {
	switch i {
	case 0:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.requestsTotal = cv
		}
	case 1:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.requestDuration = hv
		}
	case 2:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.retriesTotal = cv
		}
	case 3:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.rateLimitDelays = cv
		}
	case 4:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.rateLimitDuration = hv
		}
	}
}

func rebindConvergenceCollector(r *ConvergenceRecorder, i int, c prometheus.Collector) {
	switch i {
	case 0:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.convergenceSeconds = hv
		}
	case 1:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.driftCorrections = cv
		}
	case 2:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.prefixSetActions = cv
		}
	}
}

func rebindReconcileCollector(r *ReconcileRecorder, i int, c prometheus.Collector) {
	switch i {
	case 0:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.reconcileDuration = hv
		}
	case 1:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.reconcileTotal = cv
		}
	case 2:
		if g, ok := c.(prometheus.Gauge); ok {
			r.reconcileQueueDepth = g
		}
	case 3:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.reconcileActionsPerCycle = hv
		}
	case 4:
		if g, ok := c.(prometheus.Gauge); ok {
			r.initialReconcileDuration = g
		}
	case 5:
		if g, ok := c.(prometheus.Gauge); ok {
			r.initialReconcileComplete = g
		}
	case 6:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.crdResolutionDuration = hv
		}
	}
}

// ResetForTesting resets the singleton for test isolation.
// Must only be called from tests.
func ResetForTesting() {
	registerOnce = sync.Once{}
	singleton = nil
}

// AllCollectors returns all prometheus.Collector instances for registration verification.
func (r *Recorder) AllCollectors() []prometheus.Collector {
	var all []prometheus.Collector
	all = append(all, r.PodChurn.Collectors()...)
	all = append(all, r.ARM.Collectors()...)
	all = append(all, r.Convergence.Collectors()...)
	all = append(all, r.Reconcile.Collectors()...)
	return all
}
