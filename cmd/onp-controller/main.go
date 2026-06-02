// Command onp-controller runs the ONP controller manager.
//
// It wires up the manager (scheme, metrics and health endpoints, signal
// handling), registers the WoL power provider, and runs the Machine reconciler
// that drives the wake path from the onp.io/wake-now annotation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	onpv1alpha1 "github.com/zzzinho/on-prem-node-provisioner/api/v1alpha1"
	"github.com/zzzinho/on-prem-node-provisioner/internal/controller"
	"github.com/zzzinho/on-prem-node-provisioner/internal/logging"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power"
	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
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
		metricsAddr      string
		probeAddr        string
		leaderElect      bool
		bootTimeout      time.Duration
		wolAgentEndpoint string
		logLevel         string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election to ensure a single active controller.")
	flag.StringVar(&logLevel, "log-level", "info", "Minimum log level: debug|info|warn|error.")
	flag.DurationVar(&bootTimeout, "boot-timeout", 10*time.Minute, "How long to wait for a node to report Ready after power-on before failing.")
	flag.StringVar(&wolAgentEndpoint, "wol-agent-endpoint", "http://onp-wol-agent:9119", "Base URL of the onp-wol-agent that broadcasts magic packets.")
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

	// Register the power providers this controller knows about. Phase 1 ships
	// WoL only; a new provider is one Register call, no reconciler change.
	registry := power.NewRegistry()
	wolProvider := power.NewWoLProvider(wol.NewClient(wolAgentEndpoint, nil))
	if err := registry.Register(wolProvider); err != nil {
		log.Error(err, "unable to register power provider", "provider", wolProvider.Name())
		os.Exit(1)
	}

	// Index Machines by spec.nodeName so a Node event can fan out to the
	// Machine that links it without assuming the names match.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &onpv1alpha1.Machine{}, controller.IndexMachineNodeName,
		func(o client.Object) []string {
			m, ok := o.(*onpv1alpha1.Machine)
			if !ok || m.Spec.NodeName == "" {
				return nil
			}
			return []string{m.Spec.NodeName}
		}); err != nil {
		log.Error(err, "unable to set up machine field indexer")
		os.Exit(1)
	}

	if err := (&controller.MachineReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Registry:    registry,
		BootTimeout: bootTimeout,
		Recorder:    mgr.GetEventRecorderFor("onp-controller"),
		Clock:       clock.RealClock{},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up machine reconciler")
		os.Exit(1)
	}

	if err := (&controller.NodePoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("onp-controller"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up nodepool reconciler")
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
