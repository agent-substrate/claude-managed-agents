// Command substrate-anthropic-worker runs the Anthropic self-hosted sandbox worker
// on Agent Substrate. It handles polling work items, tool execution inside the sandbox,
// and health/readiness endpoints for Kubernetes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-substrate/substrate-anthropic/internal/config"
	"github.com/agent-substrate/substrate-anthropic/internal/worker"
)

func main() {
	var (
		envID      = flag.String("env-id", "", "Anthropic environment ID (or ANTHROPIC_ENVIRONMENT_ID env var)")
		workdir    = flag.String("workdir", "", "Working directory for tool execution (or WORK_DIR env var)")
		listen     = flag.String("listen", "", "Address to serve health checks and webhooks (or PORT / LISTEN_ADDR)")
		mode       = flag.String("mode", "", "Worker mode: poll, webhook, or hybrid (or WORKER_MODE env var)")
		unrestPath = flag.Bool("unrestricted-paths", false, "Allow tools to access files outside workdir")
	)
	flag.Parse()

	// Initialize structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Load configuration from environment, overridden by explicit CLI flags
	cfg := config.LoadFromEnv()
	if *envID != "" {
		cfg.EnvironmentID = *envID
	}
	if *workdir != "" {
		cfg.Workdir = *workdir
	}
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
	}
	if *unrestPath {
		cfg.UnrestrictedPaths = true
	}

	if err := cfg.Validate(); err != nil {
		logger.Error("configuration validation failed", "error", err)
		fmt.Fprintf(os.Stderr, "substrate-anthropic-worker: configuration error: %v\n", err)
		os.Exit(1)
	}

	logger.Info("starting substrate anthropic worker", "config", cfg.String())

	// Create worker instance
	w, err := worker.New(cfg, worker.WithLogger(logger))
	if err != nil {
		logger.Error("failed to create worker", "error", err)
		os.Exit(1)
	}

	// Setup graceful shutdown on SIGINT / SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := w.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("worker terminated with error", "error", err)
		os.Exit(1)
	}

	logger.Info("worker shutdown complete")
}
