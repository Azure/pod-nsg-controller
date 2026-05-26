package main

import (
	"flag"
	"os"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
	"github.com/Azure/pod-nsg-controller/internal/controller"
	"github.com/Azure/pod-nsg-controller/internal/metrics"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var healthProbeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.Parse()

	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.TimeKey = "timestamp"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapLog, err := zapCfg.Build()
	if err != nil {
		_, _ = os.Stderr.WriteString("failed to initialize zap logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = zapLog.Sync() }()
	logger := zapr.NewLogger(zapLog)
	ctrl.SetLogger(logger)

	setupLog := ctrl.Log.WithName("setup")

	cfg, err := config.Load()
	if err != nil {
		setupLog.Error(err, "unable to load configuration")
		os.Exit(1)
	}

	// Register metrics with the controller-runtime Prometheus registry.
	rec, err := metrics.RegisterWith(ctrlmetrics.Registry)
	if err != nil {
		setupLog.Error(err, "unable to register metrics")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "pod-nsg-controller.azure.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	rateLimiter := azure.NewARMRateLimiter(
		zapLog.With(zap.String("component", "azure-rate-limiter")),
		cfg.ARMRateLimitRPS,
		azure.WithRateLimitMetrics(rec.ARM),
	)

	prefixSetFactory := azure.NewClientFactory(
		zapLog.With(zap.String("component", "azure-client-factory")),
		azure.WithFactoryRetryPolicy(azure.DefaultRetryPolicy()),
		azure.WithFactorySubscriptionRateLimiter(rateLimiter),
		azure.WithFactoryARMRecorder(rec.ARM),
	)

	executor := azure.NewExecutor(
		zapLog.With(zap.String("component", "azure-executor")),
		prefixSetFactory,
		cfg.MaxConcurrentActions,
		azure.WithExecutorRetryMetrics(rec.ARM),
	)

	statusUpdater := controller.NewMappingStatusUpdater(mgr.GetClient(), ctrl.Log.WithName("status-updater"))

	controllerStart := time.Now()
	initialTracker := metrics.NewInitialReconcileTracker(controllerStart)
	podChurnTracker := metrics.NewPodChurnTracker()
	convergenceTracker := metrics.NewConvergenceTracker()

	// Wire convergence committer: the status updater invokes this callback after
	// each status resolution (written, noop, or error). The callback commits
	// staged convergence measurements for all outcomes because the ARM action
	// already succeeded—the convergence duration spans detection to ARM PUT
	// success, not to status write. Only not-found/stale-generation sentinels
	// suppress notification (handled inside the status updater itself).
	statusUpdater.SetConvergenceCommitter(func(
		key types.NamespacedName,
		observedGeneration int64,
		results []azure.ActionResult,
		outcome controller.StatusWriteOutcome,
		_ error,
	) {
		_ = outcome // commit on all outcomes: written, noop, and error
		for _, res := range results {
			if res.Success {
				if res.NoOp {
					// No ARM mutation occurred; forget the stale token so its
					// detectedAt does not inflate a future real convergence.
					convergenceTracker.ForgetTarget(key, res.Action.Target, observedGeneration)
				} else {
					convergenceTracker.CommitConvergence(rec.Convergence, key, res.Action.Target, observedGeneration)
				}
			}
		}
	})

	reconciler := &controller.MappingReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		ClusterName:             cfg.ClusterName,
		DefaultSubscription:     cfg.SubscriptionID,
		DefaultResourceGroup:    cfg.ResourceGroup,
		ResyncInterval:          cfg.ResyncInterval,
		MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		AzureReadSem:            make(chan struct{}, cfg.MaxConcurrentAzureReads),
		PrefixSetFactory:        prefixSetFactory,
		Executor:                executor,
		StatusUpdater:           statusUpdater,
		MetricsRecorder:      rec,
		PodChurnTracker:      podChurnTracker,
		ConvergenceTracker:   convergenceTracker,
		InitialTracker:       initialTracker,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MappingReconciler")
		os.Exit(1)
	}

	// Register the initial-reconcile initializer as a manager Runnable.
	initializer := controller.NewInitialReconcileInitializer(
		mgr.GetAPIReader(),
		initialTracker,
		rec.Reconcile,
		ctrl.Log.WithName("initial-reconcile"),
	)
	if err := mgr.Add(initializer); err != nil {
		setupLog.Error(err, "unable to add initial reconcile initializer")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	startupFields := []interface{}{
		"clusterName", cfg.ClusterName,
		"resyncIntervalSeconds", int(cfg.ResyncInterval.Seconds()),
	}
	if cfg.SubscriptionID != "" {
		startupFields = append(startupFields, "subscriptionID", cfg.SubscriptionID)
	}
	if cfg.ResourceGroup != "" {
		startupFields = append(startupFields, "resourceGroup", cfg.ResourceGroup)
	}
	setupLog.Info("starting manager", startupFields...)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
