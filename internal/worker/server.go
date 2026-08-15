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
)

const (
	DefaultWorkerAddr = ":80"
)

// WorkRequest is the JSON payload accepted by the worker HTTP server.
type WorkRequest struct {
	SessionID string `json:"session_id"`
}

// WorkResponse is the JSON payload returned by the worker HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Worker runs a lightweight HTTP server inside a Substrate actor container.
// On /work, it executes 'ant beta:worker run --session-id <session_id>'.
type Worker struct {
	logger    *slog.Logger
	antBinary string
}

// NewWorker creates a new Worker instance.
func NewWorker(logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		logger:    logger,
		antBinary: "ant",
	}
}

// Handler returns the http.Handler for the worker server.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", w.handleWork)
	mux.HandleFunc("/healthz", w.handleHealthz)
	mux.HandleFunc("/readyz", w.handleHealthz)
	return mux
}

// handleWork parses the session ID and starts 'ant beta:worker run'.
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

	stdout, stderr, err := w.StartWork(req.Context(), sessionID)
	if err != nil {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Output: stdout,
			Error:  err.Error(),
		})
		return
	}

	if stderr != "" {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Output: stdout,
			Error:  stderr,
		})
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "completed",
		Output: stdout,
	})
}

// handleHealthz returns OK status.
func (w *Worker) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "ok",
	})
}

// StartWork executes 'ant beta:worker run --session-id <sessionID>' synchronously.
func (w *Worker) StartWork(ctx context.Context, sessionID string) (stdout string, stderr string, err error) {
	if sessionID == "" {
		return "", "", errors.New("session_id is required")
	}

	bin := w.antBinary
	if bin == "" {
		bin = "ant"
	}

	args := []string{
		"beta:worker", "run",
		"--work-id", sessionID,
		"--session-id", sessionID,
		"--unrestricted-paths",
		"--max-idle", "10m",
	}
	if envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID"); envID != "" {
		args = append(args, "--environment-id", envID)
	}
	if envKey := os.Getenv("ANTHROPIC_ENVIRONMENT_KEY"); envKey != "" {
		args = append(args, "--environment-key", envKey)
	}

	w.logger.Info("Running ant worker process",
		slog.String("binary", bin),
		slog.Any("args", args),
		slog.String("session_id", sessionID),
	)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &errBuf)
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	err = cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	w.logger.Info("ant worker process finished",
		slog.String("session_id", sessionID),
		slog.String("stdout", stdout),
		slog.String("stderr", stderr),
		slog.Any("error", err),
	)
	return stdout, stderr, err
}

// Run starts the HTTP server on addr with 10-minute timeouts.
func (w *Worker) Run(addr string) error {
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
