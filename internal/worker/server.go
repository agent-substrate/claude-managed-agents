package worker

import (
	"context"
	"encoding/json"
	"errors"
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
	WorkID    string `json:"work_id"`
	SessionID string `json:"session_id,omitempty"`
}

// WorkResponse is the JSON payload returned by the worker HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Worker runs a lightweight HTTP server inside a Substrate actor container.
// On /dispatch, it executes 'ant beta:worker run --work-id <work_id> --session-id <session_id>'.
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
	mux.HandleFunc("/dispatch", w.handleDispatch)
	mux.HandleFunc("/healthz", w.handleHealthz)
	mux.HandleFunc("/readyz", w.handleHealthz)
	return mux
}

// handleDispatch parses the work ID / session ID and starts 'ant beta:worker run'.
func (w *Worker) handleDispatch(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		http.Error(rw, `{"error": "method not allowed, use POST"}`, http.StatusMethodNotAllowed)
		return
	}

	var payload WorkRequest
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&payload)
	}

	workID := strings.TrimSpace(payload.WorkID)
	sessionID := strings.TrimSpace(payload.SessionID)

	if workID == "" || sessionID == "" {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Error:  "work_id and session_id are required",
		})
		return
	}

	if err := w.StartWork(req.Context(), workID, sessionID); err != nil {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Error:  err.Error(),
		})
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "completed",
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

// StartWork executes 'ant beta:worker run --work-id <workID> --session-id <sessionID>' synchronously.
func (w *Worker) StartWork(ctx context.Context, workID, sessionID string) error {
	if workID == "" || sessionID == "" {
		return errors.New("work_id and session_id are required")
	}

	bin := w.antBinary
	if bin == "" {
		bin = "ant"
	}

	args := []string{
		"beta:worker", "run",
		"--work-id", workID,
		"--session-id", sessionID,
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
		slog.String("work_id", workID),
		slog.String("session_id", sessionID),
	)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	return cmd.Run()
}

// Run starts the HTTP server on addr.
func (w *Worker) Run(addr string) error {
	if addr == "" {
		addr = DefaultWorkerAddr
	}
	return http.ListenAndServe(addr, w.Handler())
}
