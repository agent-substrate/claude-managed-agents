// Package worker implements the Anthropic self-hosted sandbox worker for Agent Substrate.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/agent-substrate/substrate-anthropic/internal/config"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"
)

// Worker represents a self-hosted sandbox worker running in an Agent Substrate container.
type Worker struct {
	cfg     *config.Config
	client  anthropic.Client
	worker  *environments.EnvironmentWorker
	logger  *slog.Logger
	server  *http.Server
	customs []anthropic.BetaTool
	mu      sync.Mutex
	ready   bool
}

// Option configures a Worker instance.
type Option func(*Worker)

// WithCustomTools adds custom tools to the toolset available to Claude.
func WithCustomTools(tools ...anthropic.BetaTool) Option {
	return func(w *Worker) {
		w.customs = append(w.customs, tools...)
	}
}

// WithLogger sets the logger for the worker.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Worker) {
		w.logger = logger
	}
}

// New creates and initializes an Anthropic EnvironmentWorker.
func New(cfg *config.Config, opts ...Option) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	w := &Worker{
		cfg:    cfg,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}

	clientOpts := []option.RequestOption{
		option.WithAuthToken(cfg.EnvironmentKey),
	}
	if cfg.WebhookSigningKey != "" {
		clientOpts = append(clientOpts, option.WithWebhookKey(cfg.WebhookSigningKey))
	}

	client := anthropic.NewClient(clientOpts...)
	w.client = client

	workerOpts := environments.EnvironmentWorkerOptions{
		EnvironmentID:     cfg.EnvironmentID,
		EnvironmentKey:    cfg.EnvironmentKey,
		WorkerID:          cfg.WorkerID,
		Workdir:           cfg.Workdir,
		UnrestrictedPaths: cfg.UnrestrictedPaths,
		MaxFileBytes:      cfg.MaxFileBytes,
		Logger:            w.logger,
		ToolsFunc: func(env *agenttoolset.AgentToolContext) []anthropic.BetaTool {
			tools := agenttoolset.BetaAgentToolset20260401(env)
			if len(w.customs) > 0 {
				tools = append(tools, w.customs...)
			}
			return tools
		},
	}

	w.worker = environments.NewEnvironmentWorker(client, workerOpts)
	return w, nil
}

// Handler returns an http.Handler that serves health checks, stats, and webhook endpoints.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, "ok\n")
	})

	mux.HandleFunc("GET /readyz", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		ready := w.ready
		w.mu.Unlock()

		rw.Header().Set("Content-Type", "text/plain")
		if ready {
			rw.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(rw, "ready\n")
		} else {
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(rw, "initializing\n")
		}
	})

	mux.HandleFunc("GET /stats", w.handleStats)
	mux.HandleFunc("POST /webhook", w.handleWebhook)

	return mux
}

// handleStats returns current queue depth from Anthropic API.
func (w *Worker) handleStats(rw http.ResponseWriter, r *http.Request) {
	stats, err := w.client.Beta.Environments.Work.Stats(
		r.Context(),
		w.cfg.EnvironmentID,
		anthropic.BetaEnvironmentWorkStatsParams{},
	)
	if err != nil {
		w.logger.Error("failed to query environment work stats", "error", err)
		http.Error(rw, `{"error":"failed to fetch work stats"}`, http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"environment_id": w.cfg.EnvironmentID,
		"depth":          stats.Depth,
		"pending":        stats.Pending,
	})
}

// handleWebhook handles incoming webhook events from Anthropic Managed Agents.
func (w *Worker) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	if w.cfg.WebhookSigningKey == "" {
		http.Error(rw, `{"error":"webhook mode not configured"}`, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}

	event, err := w.client.Beta.Webhooks.Unwrap(body, r.Header)
	if err != nil {
		w.logger.Warn("webhook signature verification failed", "error", err)
		http.Error(rw, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	if event.Data.Type != "session.status_run_started" {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]string{"status": "ignored"})
		return
	}

	// Detach from r.Context() since the session might outlive webhook delivery timeout.
	go func() {
		ctx := context.Background()
		poller := environments.NewWorkPoller(ctx, w.client, environments.WorkPollerOptions{
			EnvironmentID:      w.cfg.EnvironmentID,
			EnvironmentKey:     w.cfg.EnvironmentKey,
			BlockMs:            param.Null[int64](),
			ReclaimOlderThanMs: param.NewOpt[int64](2000),
			Drain:              true,
		})
		defer poller.Close()

		for poller.Next() {
			item := poller.Current()
			w.logger.Info("claimed session from webhook", "work_id", item.ID, "session_id", item.Data.ID)
			if err := w.worker.HandleItem(ctx, environments.HandleItemOptions{
				WorkID:         item.ID,
				EnvironmentID:  item.EnvironmentID,
				SessionID:      item.Data.ID,
				EnvironmentKey: w.cfg.EnvironmentKey,
			}); err != nil {
				w.logger.Error("failed to handle work item", "work_id", item.ID, "error", err)
			}
		}
		if err := poller.Err(); err != nil {
			w.logger.Error("error draining work queue", "error", err)
		}
	}()

	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]string{"status": "accepted"})
}

// Run starts the worker lifecycle, including the HTTP server and work poller.
func (w *Worker) Run(ctx context.Context) error {
	// Ensure workdir exists
	if err := os.MkdirAll(w.cfg.Workdir, 0o755); err != nil {
		return fmt.Errorf("failed to create workdir %q: %w", w.cfg.Workdir, err)
	}

	// Start HTTP server for healthz, readyz, stats, webhook
	server := &http.Server{
		Addr:              w.cfg.ListenAddr,
		Handler:           w.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	w.server = server

	serverErr := make(chan error, 1)
	go func() {
		w.logger.Info("starting HTTP server", "addr", w.cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Mark worker as ready
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.ready = false
		w.mu.Unlock()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	w.logger.Info("substrate anthropic worker ready",
		"mode", w.cfg.Mode,
		"env_id", w.cfg.EnvironmentID,
		"workdir", w.cfg.Workdir,
	)

	// In webhook-only mode, keep running and await cancellation
	if w.cfg.Mode == config.ModeWebhook {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-serverErr:
			return fmt.Errorf("http server failed: %w", err)
		}
	}

	// In poll or hybrid mode, run the worker poller
	pollerErr := make(chan error, 1)
	go func() {
		if err := w.worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			pollerErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErr:
		return fmt.Errorf("http server failed: %w", err)
	case err := <-pollerErr:
		return fmt.Errorf("worker poller failed: %w", err)
	}
}
