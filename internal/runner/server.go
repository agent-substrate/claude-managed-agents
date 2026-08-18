package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRunnerAddr = ":80"
	DefaultRunTimeout = 10 * time.Minute
)

// WorkRequest is the JSON payload accepted by the runner HTTP server.
type WorkRequest struct {
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id,omitempty"`
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
// On /work, it executes 'ant beta:worker run' directly against Anthropic API.
type Runner struct {
	addr      string
	antBinary string

	// workMu ensures that only one 'ant beta:worker run' subprocess executes at a time.
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
		antBinary:    "ant",
		lastActiveAt: time.Now(),
	}, nil
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
// and runs 'ant beta:worker run' in a background goroutine.
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

		if _, _, err := r.StartWork(context.Background(), payload); err != nil {
			r.logger().Error("Background ant worker process failed",
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

// StartWork executes 'ant beta:worker run' synchronously.
func (r *Runner) StartWork(ctx context.Context, req WorkRequest) (stdout string, stderr string, err error) {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return "", "", errors.New("session_id is required")
	}

	bin := r.antBinary
	if bin == "" {
		bin = "ant"
	}

	workID := strings.TrimSpace(req.WorkID)
	if workID == "" {
		workID = sessionID
	}

	args := []string{
		"beta:worker", "run",
		"--unrestricted-paths", // TODO(jbd): Make this flag optional.
		"--max-idle", "1s",
	}

	env := append(os.Environ(),
		"ANTHROPIC_SESSION_ID="+sessionID,
		"ANTHROPIC_WORK_ID="+workID,
	)

	r.logger().Info("Running ant worker process",
		slog.String("binary", bin),
		slog.Any("args", args),
		slog.String("session_id", sessionID),
	)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	cmd.Stdin = os.Stdin
	cmd.Env = env

	err = cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	r.logger().Info("ant worker process finished",
		slog.String("session_id", sessionID),
		slog.String("stdout", stdout),
		slog.String("stderr", stderr),
		slog.Any("error", err),
	)
	return stdout, stderr, err
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
		ReadTimeout:  DefaultRunTimeout,
		WriteTimeout: DefaultRunTimeout,
		IdleTimeout:  DefaultRunTimeout,
	}
	return server.ListenAndServe()
}
