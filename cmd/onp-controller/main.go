// Command onp-controller runs the ONP controller manager.
//
// In M2.0 it only wires up the manager: scheme registration, metrics and
// health endpoints, and signal handling. Reconcilers are added in later
// sub-slices; for now a started manager that serves /healthz and /readyz is
// the observable checkpoint.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	onpv1alpha1 "github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/logging"
)

// scheme holds every type the manager's client must understand: the standard
// client-go types plus the onp.io/v1alpha1 CRDs.
var scheme = runtime.NewScheme()

func init() {
	// These registrations cannot fail for the static schemes we add, so a
	// panic here would only ever surface a programming error at startup.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := onpv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		leaderElect bool
		bootTimeout time.Duration
		logLevel    string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election to ensure a single active controller.")
	flag.StringVar(&logLevel, "log-level", "info", "Minimum log level: debug|info|warn|error.")
	// bootTimeout is defined now but unused until the reconciler lands in M2.2.
	flag.DurationVar(&bootTimeout, "boot-timeout", 10*time.Minute, "How long to wait for a node to report Ready after power-on before failing.")
	flag.Parse()

	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// One JSON slog logger backs everything: controller-runtime via logr and
	// client-go via klog both route through the same handler.
	logger := logging.New(logging.Options{Level: lvl})
	ctrl.SetLogger(logging.Logr(logger))
	klog.SetSlogLogger(logger)
	log := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "onp-controller.onp.io",
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
