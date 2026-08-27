package envworker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
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

// TestRunHeartbeatQuotesHandedOffToken proves the first heartbeat sends the
// supplied token as expected_last_heartbeat instead of claiming the lease.
func TestRunHeartbeatQuotesHandedOffToken(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusOK)

	if lost := runHeartbeatOnce(t, server, "2026-08-16T22:00:02Z"); lost {
		t.Error("a heartbeat that reports stopped must not count as a lost lease")
	}

	if len(*expected) != 1 || (*expected)[0] != "2026-08-16T22:00:02Z" {
		t.Errorf("expected_last_heartbeat values = %q, want the handed-off token", *expected)
	}
}

// TestRunHeartbeatClaimsLeaseWithoutToken proves the behaviour without a token
// is unchanged: the first heartbeat sends the NO_HEARTBEAT sentinel.
func TestRunHeartbeatClaimsLeaseWithoutToken(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusOK)

	runHeartbeatOnce(t, server, "")

	if len(*expected) != 1 || (*expected)[0] != noHeartbeatSentinel {
		t.Errorf("expected_last_heartbeat values = %q, want %q", *expected, noHeartbeatSentinel)
	}
}

// TestRunHeartbeatReportsLostLeaseOn412 proves a precondition failure on the
// handed-off token is reported as a lost lease, so the caller leaves the item
// to whoever holds it now.
func TestRunHeartbeatReportsLostLeaseOn412(t *testing.T) {
	server, expected := heartbeatServer(t, http.StatusPreconditionFailed)

	if lost := runHeartbeatOnce(t, server, "2026-08-16T22:00:02Z"); !lost {
		t.Error("a 412 on the first heartbeat must be reported as a lost lease")
	}
	if len(*expected) != 1 {
		t.Errorf("heartbeat calls = %d, want 1", len(*expected))
	}
}

// TestHandleItemOptionsEnvFallback proves the token falls back to
// ANTHROPIC_WORK_LAST_HEARTBEAT like the other options fall back to theirs,
// by observing what the first heartbeat sends.
func TestHandleItemOptionsEnvFallback(t *testing.T) {
	var expected []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/environments/env_123/work/work_1/heartbeat":
			expected = append(expected, req.URL.Query().Get("expected_last_heartbeat"))
			// Report stopped so the session runner is cancelled straight away.
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
			// Skill setup and the event stream are not under test; let them
			// fail fast.
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
		WorkID:         "work_1",
		EnvironmentID:  "env_123",
		SessionID:      "sesn_1",
		EnvironmentKey: "test-env-key",
	})

	if len(expected) == 0 || expected[0] != "2026-08-16T22:00:02Z" {
		t.Errorf("expected_last_heartbeat values = %q, want the token from ANTHROPIC_WORK_LAST_HEARTBEAT first", expected)
	}
}
