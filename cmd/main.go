package main

import (
	"flag"
	"os"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
	"github.com/Azure/pod-nsg-controller/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
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
		os.Exit(1)
	}
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
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "pod-nsg-controller.azure.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	asgClient, err := azure.NewASGClient(cfg.SubscriptionID, cfg.ResourceGroup)
	if err != nil {
		setupLog.Error(err, "unable to create Azure ASG client")
		os.Exit(1)
	}

	nicClient, err := azure.NewNICClient(cfg.SubscriptionID, cfg.ResourceGroup)
	if err != nil {
		setupLog.Error(err, "unable to create Azure NIC client")
		os.Exit(1)
	}

	reconciler := &controller.PodReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		ASGClient: asgClient,
		NICClient: nicClient,
		Config:    cfg,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pod")
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

	setupLog.Info("starting manager",
		"subscriptionID", cfg.SubscriptionID,
		"resourceGroup", cfg.ResourceGroup,
		"nsgName", cfg.NSGName,
	)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
