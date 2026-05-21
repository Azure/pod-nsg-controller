package metrics

import (
	"errors"
	"sync"

	pkgerrors "github.com/pkg/errors"
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

// MustRegister constructs the metrics recorder singleton and panics on failure.
// Note: this does NOT register collectors with any Prometheus registry.
// Use RegisterWith to register with a specific registry (e.g., controller-runtime's).
// This function is primarily useful in tests that read metrics directly from
// recorder fields without needing a Prometheus registry.
func MustRegister() *Recorder {
	r, err := Register()
	if err != nil {
		panic(err)
	}
	return r
}

// Register constructs the metrics recorder singleton. It is safe to call
// multiple times; only the first call creates the recorder.
//
// Note: this does NOT register collectors with any Prometheus registry.
// The returned recorder's collectors are usable for direct reads (e.g., in
// tests) but will not be scraped by Prometheus until registered via
// RegisterWith. Production code should use RegisterWith(ctrlmetrics.Registry).
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

// RegisterWith constructs a new recorder and registers all collectors with the
// given prometheus.Registerer. On AlreadyRegisteredError, it rebinds recorder
// fields to the existing collectors. On type mismatch, it returns an error.
// This is the correct entry point for production use.
func RegisterWith(reg prometheus.Registerer) (*Recorder, error) {
	rec := &Recorder{
		PodChurn:    newPodChurnRecorder(),
		ARM:         newARMRecorder(),
		Convergence: newConvergenceRecorder(),
		Reconcile:   newReconcileRecorder(),
	}

	if err := registerCollectors(reg, rec.PodChurn.Collectors(), func(i int, c prometheus.Collector) error {
		return rebindPodChurnCollector(rec.PodChurn, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.ARM.Collectors(), func(i int, c prometheus.Collector) error {
		return rebindARMCollector(rec.ARM, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.Convergence.Collectors(), func(i int, c prometheus.Collector) error {
		return rebindConvergenceCollector(rec.Convergence, i, c)
	}); err != nil {
		return nil, err
	}
	if err := registerCollectors(reg, rec.Reconcile.Collectors(), func(i int, c prometheus.Collector) error {
		return rebindReconcileCollector(rec.Reconcile, i, c)
	}); err != nil {
		return nil, err
	}

	return rec, nil
}

// registerCollectors registers a slice of collectors, calling rebindFn with
// the existing collector on AlreadyRegisteredError. Returns error on type
// mismatch or registration failure.
func registerCollectors(reg prometheus.Registerer, collectors []prometheus.Collector, rebindFn func(int, prometheus.Collector) error) error {
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			var alreadyRegistered prometheus.AlreadyRegisteredError
			if errors.As(err, &alreadyRegistered) {
				if rebindErr := rebindFn(i, alreadyRegistered.ExistingCollector); rebindErr != nil {
					return rebindErr
				}
				continue
			}
			return pkgerrors.Wrap(err, "registering collector")
		}
	}
	return nil
}

func rebindPodChurnCollector(r *PodChurnRecorder, i int, c prometheus.Collector) error {
	switch i {
	case 0:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.podIPChangesTotal = cv
			return nil
		}
	case 1:
		if gv, ok := c.(*prometheus.GaugeVec); ok {
			r.podChurnRate = gv
			return nil
		}
	}
	return pkgerrors.Errorf("rebind PodChurn collector %d: type mismatch (got %T)", i, c)
}

func rebindARMCollector(r *ARMRecorder, i int, c prometheus.Collector) error {
	switch i {
	case 0:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.requestsTotal = cv
			return nil
		}
	case 1:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.requestDuration = hv
			return nil
		}
	case 2:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.retriesTotal = cv
			return nil
		}
	case 3:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.rateLimitDelays = cv
			return nil
		}
	case 4:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.rateLimitDuration = hv
			return nil
		}
	}
	return pkgerrors.Errorf("rebind ARM collector %d: type mismatch (got %T)", i, c)
}

func rebindConvergenceCollector(r *ConvergenceRecorder, i int, c prometheus.Collector) error {
	switch i {
	case 0:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.convergenceSeconds = hv
			return nil
		}
	case 1:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.driftCorrections = cv
			return nil
		}
	case 2:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.prefixSetActions = cv
			return nil
		}
	}
	return pkgerrors.Errorf("rebind Convergence collector %d: type mismatch (got %T)", i, c)
}

func rebindReconcileCollector(r *ReconcileRecorder, i int, c prometheus.Collector) error {
	switch i {
	case 0:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.reconcileDuration = hv
			return nil
		}
	case 1:
		if cv, ok := c.(*prometheus.CounterVec); ok {
			r.reconcileTotal = cv
			return nil
		}
	case 2:
		if g, ok := c.(prometheus.Gauge); ok {
			r.reconcileQueueDepth = g
			return nil
		}
	case 3:
		if g, ok := c.(prometheus.Gauge); ok {
			r.reconcileInflight = g
			return nil
		}
	case 4:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.reconcileActionsPerCycle = hv
			return nil
		}
	case 5:
		if g, ok := c.(prometheus.Gauge); ok {
			r.initialReconcileDuration = g
			return nil
		}
	case 6:
		if g, ok := c.(prometheus.Gauge); ok {
			r.initialReconcileComplete = g
			return nil
		}
	case 7:
		if hv, ok := c.(*prometheus.HistogramVec); ok {
			r.crdResolutionDuration = hv
			return nil
		}
	}
	return pkgerrors.Errorf("rebind Reconcile collector %d: type mismatch (got %T)", i, c)
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
