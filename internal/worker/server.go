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

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	cfg          Config
	antBinary    string
	localBaseURL string
	proxy        http.Handler
	ateConn      *grpc.ClientConn
}

// NewWorker creates a new Worker instance with an Anthropic reverse proxy.
// It initializes cfg.AteClient if not already provided.
func NewWorker(cfg Config) (*Worker, error) {
	cfg.Defaults()
	var ateConn *grpc.ClientConn
	if cfg.AteClient == nil {
		client, conn, err := DialAteAPI(cfg.AteapiAddr, cfg.Insecure)
		if err != nil {
			return nil, fmt.Errorf("dialing ateapi at %s: %w", cfg.AteapiAddr, err)
		}
		cfg.AteClient = client
		ateConn = conn
	}

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
		cfg:          cfg,
		antBinary:    "ant",
		localBaseURL: localBaseURL,
		proxy:        proxy,
		ateConn:      ateConn,
	}, nil
}

// Close releases any background connections maintained by Worker.
func (w *Worker) Close() error {
	if w.ateConn != nil {
		return w.ateConn.Close()
	}
	return nil
}

func (w *Worker) logger() *slog.Logger {
	if w != nil && w.cfg.Logger != nil {
		return w.cfg.Logger
	}
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

// handleWork parses the session ID and starts 'ant beta:worker run'.
// Once the response has been returned to the client, it suspends the actor.
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
		if flusher, ok := rw.(http.Flusher); ok {
			flusher.Flush()
		}
		if err := w.suspendActor(sessionID); err != nil {
			w.logger().Error("Failed to suspend actor after error response", slog.Any("error", err))
		}
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(WorkResponse{
		Status: "completed",
		Output: stdout,
	})
	if flusher, ok := rw.(http.Flusher); ok {
		flusher.Flush()
	}
	if err := w.suspendActor(sessionID); err != nil {
		w.logger().Error("Failed to suspend actor after success response", slog.Any("error", err))
	}
}

func (w *Worker) suspendActor(sessionID string) error {
	actorName := ActorName(sessionID)
	actorRef := &ateapipb.ObjectRef{
		Atespace: w.cfg.Namespace,
		Name:     actorName,
	}

	w.logger().Info("Suspending actor after returning response",
		slog.String("session_id", sessionID),
		slog.String("actor", actorName),
		slog.String("namespace", w.cfg.Namespace),
	)

	if w.cfg.AteClient == nil {
		return fmt.Errorf("ateapi client not initialized")
	}

	suspendCtx, cancel := context.WithTimeout(context.Background(), DefaultSuspendTimeout)
	defer cancel()

	_, err := w.cfg.AteClient.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{Actor: actorRef})
	if err != nil && status.Code(err) != codes.NotFound {
		w.logger().Error("Failed to suspend actor",
			slog.String("actor", actorName),
			slog.Any("error", err),
		)
		return fmt.Errorf("suspending actor %s: %w", actorName, err)
	}
	w.logger().Info("Actor suspended successfully", slog.String("actor", actorName))
	return nil
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

// Run starts the HTTP server using w.cfg.Addr with 10-minute timeouts.
func (w *Worker) Run() error {
	addr := w.cfg.Addr
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
