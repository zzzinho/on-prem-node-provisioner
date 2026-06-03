// Command onp-shutdown-agent powers its own node off when the controller marks
// the backing Machine ShuttingDown.
//
// It runs as a privileged DaemonSet on ONP-managed nodes. Unlike onp-wol-agent,
// this agent is k8s-aware: it joins the cluster as a controller-runtime manager,
// watches the single Machine whose spec.nodeName matches this node, and issues a
// graceful host shutdown (nsenter into PID 1, `systemctl poweroff`) when that
// Machine reaches ShuttingDown. There is no leader election — every node runs
// its own agent, each scoped to its own Machine.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	onpv1alpha1 "github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/logging"
	"github.com/zzzinho/on-prem-node-provisioner/internal/shutdownagent"
)

// scheme holds every type the manager's client must understand: the standard
// client-go types plus the onp.io/v1alpha1 CRDs.
var scheme = runtime.NewScheme()

func init() {
	// These registrations cannot fail for the static schemes we add, so a panic
	// here would only ever surface a programming error at startup.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := onpv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	var (
		nodeName    string
		metricsAddr string
		probeAddr   string
		logLevel    string
	)
	// NODE_NAME is set by the DaemonSet via the downward API (spec.nodeName); the
	// flag lets it be overridden for local runs.
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of the node this agent runs on (defaults to $NODE_NAME).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.StringVar(&logLevel, "log-level", "info", "Minimum log level: debug|info|warn|error.")
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

	if nodeName == "" {
		// Without a node name the agent cannot know which Machine is its own, and
		// a misidentified node could be powered off in error. Fail fast.
		log.Error(fmt.Errorf("node name is empty"), "set --node-name or the NODE_NAME env var")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// No leader election: each node runs its own agent for its own Machine.
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&shutdownagent.ShutdownReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		PowerOff: shutdownagent.SystemctlPowerOff,
		Recorder: mgr.GetEventRecorderFor("onp-shutdown-agent"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up shutdown reconciler")
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

	log.Info("starting shutdown-agent", "node", nodeName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
