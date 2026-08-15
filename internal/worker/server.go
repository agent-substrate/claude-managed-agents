package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	DefaultWorkerAddr = ":80"
)

// WorkRequest is the JSON payload accepted by the worker HTTP server.
type WorkRequest struct {
	SessionID      string `json:"session_id"`
	EnvironmentID  string `json:"environment_id,omitempty"`
	EnvironmentKey string `json:"environment_key,omitempty"`
}

// WorkResponse is the JSON payload returned by the worker HTTP server.
type WorkResponse struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Worker runs a lightweight HTTP server inside a Substrate actor container.
// On /work, it executes 'ant beta:worker run --session-id <session_id>'.
//
// Heartbeat Conflict Workaround:
// ------------------------------
// In Anthropic Managed Agents, the poller deployment runs `ant beta:worker poll --on-work ate-antworker`.
// When a work item is claimed, `poll` maintains an upstream heartbeat loop with Anthropic to keep
// the work lease alive while `--on-work` executes.
//
// Inside the Substrate actor microVM, `StartWork` runs `ant beta:worker run --work-id <id> --session-id <id>`.
// However, `ant beta:worker run` ALSO attempts to heartbeat the same `work-id` and sends
// `expected_last_heartbeat=NO_HEARTBEAT` on its first heartbeat (~10 seconds in).
//
// Because the poller already sent a heartbeat to Anthropic, Anthropic returns:
//
//	412 Precondition Failed: Heartbeat precondition failed: expected NO_HEARTBEAT, actual was <timestamp>
//
// This causes `ant beta:worker run` to believe another worker stole its lease, cancel its context,
// and shut down prematurely (~15 seconds after starting). In multi-turn sessions, this leaves subsequent
// tool calls stranded with no runner attached.
//
// To resolve this, Worker acts as a local reverse proxy for https://api.anthropic.com and starts
// `ant beta:worker run` with `--base-url http://127.0.0.1:<port>`:
//   - Heartbeats (`POST /v1/environments/.../work/.../heartbeat`) are intercepted locally and acknowledged
//     with HTTP 200 OK (`{"state":"active","lease_extended":true,"ttl_seconds":300}`).
//   - All real session event streams and tool execution results (`/v1/sessions/...`) are transparently
//     proxied to https://api.anthropic.com.
//   - The upstream poller (`poll`) continues to maintain the single, real lease with Anthropic until
//     the actor finishes and is suspended.
type Worker struct {
	logger       *slog.Logger
	antBinary    string
	localBaseURL string
	proxy        http.Handler
}

// NewWorker creates a new Worker instance with an Anthropic reverse proxy.
func NewWorker(logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	targetURL, _ := url.Parse("https://api.anthropic.com")
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
	}
	return &Worker{
		logger:       logger,
		antBinary:    "ant",
		localBaseURL: "http://127.0.0.1:80",
		proxy:        proxy,
	}
}

// Handler returns the http.Handler for the worker server.
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", w.handleWork)
	mux.HandleFunc("/healthz", w.handleHealthz)
	mux.HandleFunc("/readyz", w.handleHealthz)
	mux.HandleFunc("/v1/environments/", w.handleEnvironments)
	mux.HandleFunc("/v1/", w.handleProxy)
	return mux
}

// handleEnvironments intercepts worker heartbeats to avoid 412 conflicts with the poller.
// Non-heartbeat environment requests are forwarded to Anthropic via the reverse proxy.
func (w *Worker) handleEnvironments(rw http.ResponseWriter, req *http.Request) {
	if strings.HasSuffix(req.URL.Path, "/heartbeat") {
		nowStr := time.Now().UTC().Format(time.RFC3339)
		w.logger.Info("Intercepted local worker heartbeat, responding success",
			slog.String("path", req.URL.Path),
			slog.String("last_heartbeat", nowStr),
		)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		resp := fmt.Sprintf(`{"type":"work_heartbeat","state":"active","lease_extended":true,"last_heartbeat":%q,"ttl_seconds":300}`, nowStr)
		_, _ = rw.Write([]byte(resp))
		return
	}
	w.handleProxy(rw, req)
}

// handleProxy reverse-proxies other /v1/ requests (such as session events and tool results) to Anthropic.
func (w *Worker) handleProxy(rw http.ResponseWriter, req *http.Request) {
	if w.proxy != nil {
		w.proxy.ServeHTTP(rw, req)
		return
	}
	http.NotFound(rw, req)
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

	stdout, stderr, err := w.StartWork(context.Background(), payload)
	if err != nil {
		errMsg := err.Error()
		if stderr != "" {
			errMsg = stderr
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(rw).Encode(WorkResponse{
			Status: "error",
			Output: stdout,
			Error:  errMsg,
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
func (w *Worker) StartWork(ctx context.Context, req WorkRequest) (stdout string, stderr string, err error) {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		return "", "", errors.New("session_id is required")
	}

	bin := w.antBinary
	if bin == "" {
		bin = "ant"
	}

	baseURL := w.localBaseURL
	if baseURL == "" {
		baseURL = "http://127.0.0.1:80"
	}

	args := []string{
		"beta:worker", "run",
		"--unrestricted-paths",
		"--max-idle", "30s",
	}

	env := append(os.Environ(),
		"ANTHROPIC_SESSION_ID="+sessionID,
		"ANTHROPIC_WORK_ID="+sessionID,
		"ANTHROPIC_BASE_URL="+baseURL,
	)
	if req.EnvironmentID != "" {
		env = append(env, "ANTHROPIC_ENVIRONMENT_ID="+req.EnvironmentID)
	}
	if req.EnvironmentKey != "" {
		env = append(env, "ANTHROPIC_ENVIRONMENT_KEY="+req.EnvironmentKey)
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
	cmd.Env = env

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
	if strings.HasPrefix(addr, ":") {
		w.localBaseURL = "http://127.0.0.1" + addr
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
