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
	DefaultWakerAddr = ":80"
)

// WorkRequest is the JSON payload accepted by the waker HTTP server.
type WorkRequest struct {
	WorkID    string `json:"work_id"`
	SessionID string `json:"session_id,omitempty"`
}

// WorkResponse is the JSON payload returned by the waker HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Waker runs a lightweight HTTP server inside a Substrate actor container.
// On /dispatch, it executes 'ant beta:worker run --work-id <work_id> --session-id <session_id>'.
type Waker struct {
	logger    *slog.Logger
	antBinary string
}

// NewWaker creates a new Waker instance.
func NewWaker(logger *slog.Logger) *Waker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Waker{
		logger:    logger,
		antBinary: "ant",
	}
}

// Handler returns the http.Handler for the waker server.
func (w *Waker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/dispatch", w.handleDispatch)
	mux.HandleFunc("/start", w.handleDispatch) // backwards compatibility alias
	mux.HandleFunc("/healthz", w.handleHealthz)
	mux.HandleFunc("/readyz", w.handleHealthz)
	return mux
}

// handleDispatch parses the work ID / session ID and starts 'ant beta:worker run'.
func (w *Waker) handleDispatch(rw http.ResponseWriter, req *http.Request) {
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
func (w *Waker) handleHealthz(rw http.ResponseWriter, req *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "ok",
	})
}

// StartWork executes 'ant beta:worker run --work-id <workID> --session-id <sessionID>' synchronously.
func (w *Waker) StartWork(ctx context.Context, workID, sessionID string) error {
	if workID == "" || sessionID == "" {
		return errors.New("work_id and session_id are required")
	}

	bin := w.antBinary
	if bin == "" {
		bin = "ant"
	}

	args := []string{"beta:worker", "run", "--work-id", workID, "--session-id", sessionID}
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
func (w *Waker) Run(addr string) error {
	if addr == "" {
		addr = DefaultWakerAddr
	}
	return http.ListenAndServe(addr, w.Handler())
}
