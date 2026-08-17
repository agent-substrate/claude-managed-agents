package worker

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
	DefaultWorkerAddr  = ":80"
	DefaultWorkTimeout = 10 * time.Minute
)

// WorkRequest is the JSON payload accepted by the worker HTTP server.
type WorkRequest struct {
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id,omitempty"`
}

// WorkResponse is the JSON payload returned by the worker HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// IdleStatus is the JSON payload returned by the /idle HTTP endpoint.
type IdleStatus struct {
	IdleDuration string `json:"idle_duration"`
	LastActiveAt string `json:"last_active_at"`
}

// WorkerConfig defines configuration for the in-actor worker daemon.
type WorkerConfig struct {
	Addr string
}

// Defaults fills in default values for unset fields.
func (c *WorkerConfig) Defaults() {
	if c.Addr == "" {
		c.Addr = DefaultWorkerAddr
	}
}

// Worker runs a lightweight HTTP server inside a Substrate actor container.
// On /work, it executes 'ant beta:worker run' directly against Anthropic API.
type Worker struct {
	addr      string
	antBinary string

	// workMu ensures that only one 'ant beta:worker run' subprocess executes at a time.
	workMu sync.Mutex

	stateMu      sync.RWMutex
	running      bool
	lastActiveAt time.Time
}

// NewWorker creates a new Worker instance.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	cfg.Defaults()

	return &Worker{
		addr:         cfg.Addr,
		antBinary:    "ant",
		lastActiveAt: time.Now(),
	}, nil
}

func (w *Worker) logger() *slog.Logger {
	return slog.Default()
}

// Handler returns the http.Handler for the worker server.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", w.handleWork)
	mux.HandleFunc("/idle", w.handleIdle)
	mux.HandleFunc("/healthz", w.handleHealthz)
	mux.HandleFunc("/readyz", w.handleHealthz)
	return mux
}

// handleWork parses the session ID, marks the actor as running, returns 200 accepted immediately,
// and runs 'ant beta:worker run' in a background goroutine.
func (w *Worker) handleWork(rw http.ResponseWriter, req *http.Request) {
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

	w.stateMu.Lock()
	w.running = true
	w.stateMu.Unlock()

	go func() {
		w.workMu.Lock()
		defer w.workMu.Unlock()

		defer func() {
			w.stateMu.Lock()
			w.running = false
			w.lastActiveAt = time.Now()
			w.stateMu.Unlock()
		}()

		if _, _, err := w.StartWork(context.Background(), payload); err != nil {
			w.logger().Error("Background ant worker process failed",
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

// handleIdle returns how long the worker server has been idle.
func (w *Worker) handleIdle(rw http.ResponseWriter, req *http.Request) {
	w.stateMu.RLock()
	running := w.running
	lastActive := w.lastActiveAt
	w.stateMu.RUnlock()

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
func (w *Worker) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "ok",
	})
}

// StartWork executes 'ant beta:worker run' synchronously.
func (w *Worker) StartWork(ctx context.Context, req WorkRequest) (stdout string, stderr string, err error) {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return "", "", errors.New("session_id is required")
	}

	bin := w.antBinary
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

	w.logger().Info("Running ant worker process",
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
	w.logger().Info("ant worker process finished",
		slog.String("session_id", sessionID),
		slog.String("stdout", stdout),
		slog.String("stderr", stderr),
		slog.Any("error", err),
	)
	return stdout, stderr, err
}

// Run starts the HTTP server using w.addr with 10-minute timeouts.
func (w *Worker) Run() error {
	addr := w.addr
	if addr == "" {
		addr = DefaultWorkerAddr
	}
	server := &http.Server{
		Addr:         addr,
		Handler:      w.Handler(),
		ReadTimeout:  DefaultWorkTimeout,
		WriteTimeout: DefaultWorkTimeout,
		IdleTimeout:  DefaultWorkTimeout,
	}
	return server.ListenAndServe()
}
