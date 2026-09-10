package envworker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// heartbeatServer records the expected_last_heartbeat of every heartbeat it
// receives and answers each with the given status. A 200 reports the work as
// stopped so runHeartbeat returns after the first beat.
func heartbeatServer(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var expected []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/environments/env_123/work/work_1/heartbeat" {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			http.NotFound(rw, req)
			return
		}
		expected = append(expected, req.URL.Query().Get("expected_last_heartbeat"))
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"precondition_failed","message":"stale"}}`))
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"type":           "work_heartbeat",
			"last_heartbeat": "2026-08-16T22:00:03Z",
			"lease_extended": true,
			"state":          "stopped",
			"ttl_seconds":    120,
		})
	}))
	t.Cleanup(server.Close)
	return server, &expected
}

func runHeartbeatOnce(t *testing.T, server *httptest.Server, token string) bool {
	t.Helper()
	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
		option.WithMaxRetries(0),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := &anthropic.BetaSelfHostedWork{ID: "work_1", EnvironmentID: "env_123"}
	reqOpts, err := helperReqOpts("test-env-key", helperEnvironmentsWorker)
	if err != nil {
		t.Fatalf("helperReqOpts: %v", err)
	}
	return runHeartbeat(ctx, cancel, client, work, reqOpts, token, slog.Default())
}

func TestRunHeartbeatQuotesHandedOffToken(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusOK)

	if lost := runHeartbeatOnce(t, server, "2026-08-16T22:00:02Z"); lost {
		t.Error("a heartbeat that reports stopped must not count as a lost lease")
	}

	if len(*expected) != 1 || (*expected)[0] != "2026-08-16T22:00:02Z" {
		t.Errorf("expected_last_heartbeat values = %q, want the handed-off token", *expected)
	}
}

func TestRunHeartbeatClaimsLeaseWithoutToken(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusOK)

	runHeartbeatOnce(t, server, "")

	if len(*expected) != 1 || (*expected)[0] != noHeartbeatSentinel {
		t.Errorf("expected_last_heartbeat values = %q, want %q", *expected, noHeartbeatSentinel)
	}
}

func TestRunHeartbeatReportsLostLeaseOn412(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusPreconditionFailed)

	if lost := runHeartbeatOnce(t, server, "2026-08-16T22:00:02Z"); !lost {
		t.Error("a 412 on the first heartbeat must be reported as a lost lease")
	}
	if len(*expected) != 1 {
		t.Errorf("heartbeat calls = %d, want 1", len(*expected))
	}
}

func TestHandleItemOptionsEnvFallback(t *testing.T) {
	var expected []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/environments/env_123/work/work_1/heartbeat":
			expected = append(expected, req.URL.Query().Get("expected_last_heartbeat"))
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"type":           "work_heartbeat",
				"last_heartbeat": "2026-08-16T22:00:03Z",
				"lease_extended": true,
				"state":          "stopped",
				"ttl_seconds":    120,
			})
		case "/v1/environments/env_123/work/work_1/stop":
			rw.WriteHeader(http.StatusNoContent)
		default:
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"no"}}`))
		}
	}))
	defer server.Close()

	t.Setenv("ANTHROPIC_WORK_LAST_HEARTBEAT", "2026-08-16T22:00:02Z")

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
		option.WithMaxRetries(0),
	)
	worker := NewEnvironmentWorker(client, EnvironmentWorkerOptions{
		Tools:   []anthropic.BetaTool{},
		Workdir: t.TempDir(),
	})
	_ = worker.HandleItem(context.Background(), HandleItemOptions{
		WorkID:        "work_1",
		EnvironmentID: "env_123",
		SessionID:     "sesn_1",
		WorkSecret:    workSecret(t, "sessions-token-1"),
	})

	if len(expected) == 0 || expected[0] != "2026-08-16T22:00:02Z" {
		t.Errorf("expected_last_heartbeat values = %q, want the token from ANTHROPIC_WORK_LAST_HEARTBEAT first", expected)
	}
}

// workSecret encodes a work item secret payload the way the poll response
// carries it: URL-safe base64 of a JSON object with a sessions_token.
func workSecret(t *testing.T, sessionsToken string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"sessions_token": sessionsToken, "ingress_token": "unused"})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// itemServer answers the heartbeat (reporting the work as stopped) and the
// stop call of work_1, recording the Authorization and x-api-key headers of
// every request.
func itemServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var auth []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		auth = append(auth, req.Header.Get("authorization")+"|"+req.Header.Get("x-api-key"))
		rw.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/environments/env_123/work/work_1/heartbeat":
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"type":           "work_heartbeat",
				"last_heartbeat": "2026-08-16T22:00:03Z",
				"lease_extended": true,
				"state":          "stopped",
				"ttl_seconds":    120,
			})
		case "/v1/environments/env_123/work/work_1/stop":
			rw.WriteHeader(http.StatusNoContent)
		default:
			rw.WriteHeader(http.StatusNotFound)
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"no"}}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, &auth
}

func handleItemWith(t *testing.T, server *httptest.Server, opts HandleItemOptions) error {
	t.Helper()
	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(server.URL),
		option.WithAPIKey("parent-api-key"),
		option.WithMaxRetries(0),
	)
	worker := NewEnvironmentWorker(client, EnvironmentWorkerOptions{
		Tools:   []anthropic.BetaTool{},
		Workdir: t.TempDir(),
	})
	opts.WorkID = "work_1"
	opts.EnvironmentID = "env_123"
	opts.SessionID = "sesn_1"
	return worker.HandleItem(context.Background(), opts)
}

func TestHandleItemAuthenticatesWithSessionsTokenFromWorkSecret(t *testing.T) {
	server, auth := itemServer(t)

	_ = handleItemWith(t, server, HandleItemOptions{WorkSecret: workSecret(t, "sessions-token-1")})

	if len(*auth) < 2 {
		t.Fatalf("requests = %q, want a heartbeat and a stop", *auth)
	}
	for _, got := range *auth {
		if got != "Bearer sessions-token-1|" {
			t.Errorf("authorization|x-api-key = %q, want the sessions token as bearer and no api key", got)
		}
	}
}

func TestHandleItemRequiresSessionsToken(t *testing.T) {
	t.Setenv("ANTHROPIC_WORK_SECRET", "")
	server, auth := itemServer(t)

	err := handleItemWith(t, server, HandleItemOptions{WorkSecret: "not-a-payload"})
	if err == nil {
		t.Fatal("expected an error when the work secret carries no sessions token")
	}
	if len(*auth) != 0 {
		t.Errorf("requests = %q, want none without a sessions token", *auth)
	}
}

func TestSessionsTokenFromSecret(t *testing.T) {
	padded := base64.URLEncoding.EncodeToString([]byte(`{"sessions_token":"tok"}`))
	for _, tc := range []struct{ name, secret, want string }{
		{"raw url base64", workSecret(t, "tok"), "tok"},
		{"padded base64", padded, "tok"},
		{"empty", "", ""},
		{"not base64", "%%%", ""},
		{"not json", base64.RawURLEncoding.EncodeToString([]byte("plain")), ""},
		{"no token", base64.RawURLEncoding.EncodeToString([]byte(`{"ingress_token":"x"}`)), ""},
	} {
		if got := sessionsTokenFromSecret(tc.secret); got != tc.want {
			t.Errorf("%s: sessionsTokenFromSecret = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHandleItemValidation(t *testing.T) {
	client := anthropic.NewClient(option.WithAPIKey("test-key"))
	worker := NewEnvironmentWorker(client, EnvironmentWorkerOptions{})

	t.Setenv("ANTHROPIC_WORK_ID", "")
	t.Setenv("ANTHROPIC_ENVIRONMENT_ID", "")
	t.Setenv("ANTHROPIC_SESSION_ID", "")
	t.Setenv("ANTHROPIC_WORK_SECRET", "")

	err := worker.HandleItem(context.Background(), HandleItemOptions{})
	if err == nil {
		t.Fatal("expected error when options are empty, got nil")
	}
}

func TestBearerReqOptsValidation(t *testing.T) {
	if _, err := bearerReqOpts(""); err == nil {
		t.Fatal("expected error for empty sessions token")
	}
	opts, err := bearerReqOpts("secret-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 2 {
		t.Errorf("expected 2 options, got %d", len(opts))
	}
}
