package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
	"github.com/Azure/pod-nsg-controller/internal/controller"
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
		_, _ = fmt.Fprintf(os.Stderr, "failed to initialize zap logger: %v\n", err)
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

	prefixSetFactory := azure.NewClientFactory(zapLog.With(zap.String("component", "azure-client-factory")))
	executor := azure.NewExecutor(zapLog.With(zap.String("component", "azure-executor")), prefixSetFactory, 5)

	statusUpdater := controller.NewMappingStatusUpdater(mgr.GetClient(), time.Now)

	reconciler := &controller.MappingReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		ClusterName:          cfg.ClusterName,
		DefaultSubscription:  cfg.SubscriptionID,
		DefaultResourceGroup: cfg.ResourceGroup,
		ResyncInterval:       cfg.ResyncInterval,
		PrefixSetFactory:     prefixSetFactory,
		Executor:             executor,
		StatusUpdater:        statusUpdater,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MappingReconciler")
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
