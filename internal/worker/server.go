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
// On /work, it executes 'ant beta:worker run --session-id <session_id>'
// and suspends the actor once execution finishes to snapshot state and free the worker runner.
type Worker struct {
	addr         string
	namespace    string
	antBinary    string
	localBaseURL string
	proxy        http.Handler
}

// NewWorker creates a new Worker instance with an Anthropic reverse proxy and ateapi client for self-suspension.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	cfg.Defaults()

	targetURL, _ := url.Parse("https://api.anthropic.com")
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
	}
	localBaseURL := "http://127.0.0.1" + cfg.Addr
	if !strings.HasPrefix(cfg.Addr, ":") {
		localBaseURL = "http://" + cfg.Addr
	}
	return &Worker{
		addr:         cfg.Addr,
		antBinary:    "ant",
		localBaseURL: localBaseURL,
		proxy:        proxy,
	}, nil
}

func (w *Worker) logger() *slog.Logger {
	return slog.Default()
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
		w.logger().Info("Intercepted local worker heartbeat, responding success",
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

// handleWork parses the session ID, returns 200 accepted immediately,
// runs 'ant beta:worker run' in the background, and suspends the actor when done.
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

	if _, _, err := w.StartWork(context.Background(), payload); err != nil {
		w.logger().Error("Background ant worker process failed",
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "accepted",
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
		"--unrestricted-paths", // TODO(jbd): Make this flag optional.
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
