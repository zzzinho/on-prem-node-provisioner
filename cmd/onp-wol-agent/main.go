// Command onp-wol-agent serves the /wake endpoint that broadcasts Wake-on-LAN
// magic packets on behalf of the controller.
//
// It runs as a hostNetwork DaemonSet on always-on nodes (the control plane):
// the magic packet is an L2 broadcast, so the sender must share the target's
// segment. The binary is intentionally k8s-free — it ships as a scratch image
// and imports only the standard library and the pure wol wire code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zzzinho/on-prem-node-provisioner/internal/agent"
	"github.com/zzzinho/on-prem-node-provisioner/internal/logging"
)

// shutdownTimeout bounds graceful drain of in-flight requests on SIGTERM.
const shutdownTimeout = 10 * time.Second

func main() {
	var (
		listenAddr string
		logLevel   string
	)
	flag.StringVar(&listenAddr, "listen-addr", ":9119", "Address the /wake endpoint binds to.")
	flag.StringVar(&logLevel, "log-level", "info", "Minimum log level: debug|info|warn|error.")
	flag.Parse()

	lvl, err := logging.ParseLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := logging.New(logging.Options{Level: lvl})

	if err := run(listenAddr, logger); err != nil {
		logger.Error("agent exited with error", "error", err)
		os.Exit(1)
	}
}

func run(listenAddr string, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: agent.Handler(nil, logger),
	}

	// signal.NotifyContext cancels ctx on the first SIGINT/SIGTERM and restores
	// default disposition on the second, so a stuck shutdown stays Ctrl-C-able.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("wol-agent listening", "addr", listenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		// ListenAndServe failed to bind (or otherwise returned) before any
		// signal arrived; ErrServerClosed cannot occur here since nothing has
		// called Shutdown yet.
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// Drain the goroutine's return value; after Shutdown it is ErrServerClosed,
	// which is the expected clean stop, not a failure.
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("wol-agent stopped")
	return nil
}
