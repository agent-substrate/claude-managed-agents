// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/envworker"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"
)

const (
	DefaultRunnerAddr = ":80"
	defaultRunTimeout = 10 * time.Minute

	// workerMaxIdle is how long the worker keeps the session open after the
	// agent's turn ends before exiting and releasing the actor.
	workerMaxIdle = 1 * time.Second
)

// WorkRequest is the JSON payload accepted by the runner HTTP server.
type WorkRequest struct {
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id,omitempty"`
	// LastHeartbeat is the lease token the poller obtained when it acked the
	// work item and sent its first heartbeat. The worker quotes it as its
	// first expected_last_heartbeat, continuing that lease instead of
	// claiming a new one. Empty means the worker claims the lease itself.
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

// WorkResponse is the JSON payload returned by the runner HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// IdleStatus is the JSON payload returned by the /idle HTTP endpoint.
type IdleStatus struct {
	IdleDuration string `json:"idle_duration"`
	LastActiveAt string `json:"last_active_at"`
}

// RunnerConfig defines configuration for the in-actor runner daemon.
type RunnerConfig struct {
	Addr string
}

// Defaults fills in default values for unset fields.
func (c *RunnerConfig) Defaults() {
	if c.Addr == "" {
		c.Addr = DefaultRunnerAddr
	}
}

// Runner runs a lightweight HTTP server inside a Substrate actor container.
// On /work, it services the work item in-process with the SDK's environment
// worker: it runs the session's tool calls against the Anthropic API while
// heartbeating the work-item lease.
type Runner struct {
	addr string

	// handleItem services one work item; tests replace it.
	handleItem func(ctx context.Context, req WorkRequest) error

	// workMu ensures that only one work item is serviced at a time.
	workMu sync.Mutex

	stateMu      sync.RWMutex
	running      bool
	lastActiveAt time.Time
}

// NewRunner creates a new Runner instance.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	cfg.Defaults()

	return &Runner{
		addr:         cfg.Addr,
		handleItem:   handleItem,
		lastActiveAt: time.Now(),
	}, nil
}

// handleItem runs one work item's session with the in-process environment worker.
// The environment id and key come from ANTHROPIC_ENVIRONMENT_ID and
// ANTHROPIC_ENVIRONMENT_KEY, which the actor template injects. The key is only
// a placeholder: the Substrate egress gateway replaces it with the real key
// from the ate-ant-creds Secret on the way out. The worker does
// not require ANTHROPIC_API_KEY. The tools are the standard agent toolset rooted
// at the process working directory, with the file tools allowed outside it.
// The lease token and the environment key never reach the logs.
func handleItem(ctx context.Context, req WorkRequest) error {
	workdir, err := os.Getwd()
	if err != nil {
		workdir = "."
	}
	env := newToolContext(workdir)
	toolset := agenttoolset.BetaAgentToolset20260401(env)
	defer agenttoolset.CloseAll(toolset)

	maxIdle := workerMaxIdle
	worker := envworker.NewEnvironmentWorker(anthropic.NewClient(), envworker.EnvironmentWorkerOptions{
		Tools:        toolset,
		Workdir:      workdir,
		AllowedRoots: env.AllowedRoots,
		MaxIdle:      &maxIdle,
		Logger:       slog.Default(),
	})
	return worker.HandleItem(ctx, envworker.HandleItemOptions{
		WorkID:                req.WorkID,
		SessionID:             req.SessionID,
		ExpectedLastHeartbeat: req.LastHeartbeat,
	})
}

// newToolContext returns the agent toolset context for a session rooted at
// workdir. The actor's gVisor sandbox is the security boundary, so the file
// tools may reach the whole filesystem. anthropic-sdk-go rejects every tool
// call (bash included) when the deprecated UnrestrictedPaths is set;
// AllowedRoots "/" is its replacement.
func newToolContext(workdir string) *agenttoolset.AgentToolContext {
	return &agenttoolset.AgentToolContext{
		Workdir:      workdir,
		AllowedRoots: []string{"/"},
	}
}

func (r *Runner) logger() *slog.Logger {
	return slog.Default()
}

// Handler returns the http.Handler for the runner server.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", r.handleWork)
	mux.HandleFunc("/idle", r.handleIdle)
	mux.HandleFunc("/healthz", r.handleHealthz)
	mux.HandleFunc("/readyz", r.handleHealthz)
	return mux
}

// handleWork parses the session ID, marks the actor as running, returns 200 accepted immediately,
// and services the work item in a background goroutine.
func (r *Runner) handleWork(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		http.Error(rw, `{"error": "method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload WorkRequest
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&payload)
	}

	sessionID := strings.TrimSpace(payload.SessionID)
	if sessionID == "" {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Error:  "session_id is required",
		})
		return
	}

	r.stateMu.Lock()
	r.running = true
	r.lastActiveAt = time.Now()
	r.stateMu.Unlock()

	go func() {
		r.workMu.Lock()
		defer r.workMu.Unlock()

		defer func() {
			r.stateMu.Lock()
			r.running = false
			r.lastActiveAt = time.Now()
			r.stateMu.Unlock()
		}()

		if err := r.StartWork(context.Background(), payload); err != nil {
			r.logger().Error("Background worker failed",
				slog.String("session_id", sessionID),
				slog.Any("error", err),
			)
		}
	}()

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "accepted",
	})
}

// handleIdle returns how long the runner server has been idle.
func (r *Runner) handleIdle(rw http.ResponseWriter, req *http.Request) {
	r.stateMu.RLock()
	running := r.running
	lastActive := r.lastActiveAt
	r.stateMu.RUnlock()

	var (
		idleDur       time.Duration
		lastActiveStr string
	)
	if running {
		idleDur = 0
		lastActiveStr = time.Now().UTC().Format(time.RFC3339)
	} else {
		idleDur = time.Since(lastActive)
		lastActiveStr = lastActive.UTC().Format(time.RFC3339)
	}

	resp := IdleStatus{
		IdleDuration: idleDur.String(),
		LastActiveAt: lastActiveStr,
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(resp)
}

// handleHealthz returns OK status.
func (r *Runner) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "ok",
	})
}

// StartWork services one work item synchronously: it runs the session's tool
// calls in-process while heartbeating the lease, and returns when the session
// goes idle, is stopped, or the lease is lost.
func (r *Runner) StartWork(ctx context.Context, req WorkRequest) error {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return errors.New("session_id is required")
	}

	workID := strings.TrimSpace(req.WorkID)
	if workID == "" {
		workID = sessionID
	}

	req = WorkRequest{
		SessionID:     sessionID,
		WorkID:        workID,
		LastHeartbeat: strings.TrimSpace(req.LastHeartbeat),
	}

	r.logger().Info("Running worker",
		slog.String("session_id", sessionID),
		slog.String("work_id", workID),
		slog.Bool("lease_handoff", req.LastHeartbeat != ""),
	)

	err := r.handleItem(ctx, req)
	r.logger().Info("Worker finished",
		slog.String("session_id", sessionID),
		slog.String("work_id", workID),
		slog.Any("error", err),
	)
	return err
}

// Run starts the HTTP server using r.addr with 10-minute timeouts.
func (r *Runner) Run() error {
	addr := r.addr
	if addr == "" {
		addr = DefaultRunnerAddr
	}
	server := &http.Server{
		Addr:         addr,
		Handler:      r.Handler(),
		ReadTimeout:  defaultRunTimeout,
		WriteTimeout: defaultRunTimeout,
		IdleTimeout:  defaultRunTimeout,
	}
	return server.ListenAndServe()
}
