package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestNewPollerValidation(t *testing.T) {
	t.Setenv("ANTHROPIC_ENVIRONMENT_ID", "")
	_, err := New(Config{})
	if err == nil {
		t.Error("expected error when environment_id is empty, got nil")
	}

	p, err := New(Config{EnvironmentID: "env_test_123"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.cfg.EnvironmentID != "env_test_123" {
		t.Errorf("EnvironmentID = %q, want 'env_test_123'", p.cfg.EnvironmentID)
	}
}

func TestPollOnce(t *testing.T) {
	var (
		apiKeyHeader string
		envKeyHeader string
		callCount    int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&callCount, 1)
		apiKeyHeader = req.Header.Get("x-api-key")
		envKeyHeader = req.Header.Get("x-anthropic-environment-key")

		if req.URL.Path != "/v1/environments/env_123/work/poll" {
			http.NotFound(rw, req)
			return
		}

		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"id":             "work_abc_123",
			"environment_id": "env_123",
			"type":           "work",
			"state":          "active",
			"data": map[string]any{
				"id":   "sesn_xyz_456",
				"type": "session",
			},
			"created_at":          "2026-08-16T22:00:00Z",
			"acknowledged_at":     "2026-08-16T22:00:01Z",
			"latest_heartbeat_at": "2026-08-16T22:00:01Z",
			"started_at":          "2026-08-16T22:00:01Z",
			"secret":              "sec_test_secret",
		})
	}))
	defer server.Close()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
		option.WithHeader("x-anthropic-environment-key", "test-env-key"),
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	item, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if item.ID != "work_abc_123" {
		t.Errorf("ID = %q, want 'work_abc_123'", item.ID)
	}
	if item.Data.ID != "sesn_xyz_456" {
		t.Errorf("Data.ID = %q, want 'sesn_xyz_456'", item.Data.ID)
	}
	if apiKeyHeader != "test-api-key" {
		t.Errorf("x-api-key = %q, want 'test-api-key'", apiKeyHeader)
	}
	if envKeyHeader != "test-env-key" {
		t.Errorf("x-anthropic-environment-key = %q, want 'test-env-key'", envKeyHeader)
	}
}

func TestPollOnceWithEnvDefaultsSuppressed(t *testing.T) {
	var (
		apiKeyHeader string
		authHeader   string
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		apiKeyHeader = req.Header.Get("x-api-key")
		authHeader = req.Header.Get("authorization")

		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"id":             "work_abc_123",
			"environment_id": "env_123",
			"type":           "work",
			"state":          "active",
			"data": map[string]any{
				"id":   "sesn_xyz_456",
				"type": "session",
			},
		})
	}))
	defer server.Close()

	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-bearer-token")

	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(server.URL),
		option.WithAPIKey("valid-api-key"),
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if apiKeyHeader != "valid-api-key" {
		t.Errorf("x-api-key = %q, want 'valid-api-key'", apiKeyHeader)
	}
	if authHeader != "" {
		t.Errorf("expected no Authorization header, got %q", authHeader)
	}
}

func TestPollOnceNotFoundOrEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	item, err := p.PollOnce(context.Background())
	if !errors.Is(err, ErrNoWork) {
		t.Errorf("PollOnce err = %v, want ErrNoWork", err)
	}
	if item != nil {
		t.Errorf("expected item nil, got %+v", item)
	}
}

func TestComplete(t *testing.T) {
	var requestedPath string

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		requestedPath = req.URL.Path
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"id":             "work_123",
			"environment_id": "env_123",
			"type":           "work",
			"state":          "stopped",
			"data": map[string]any{
				"id":   "sesn_123",
				"type": "session",
			},
			"created_at":          "2026-08-16T22:00:00Z",
			"acknowledged_at":     "2026-08-16T22:00:01Z",
			"latest_heartbeat_at": "2026-08-16T22:00:01Z",
			"started_at":          "2026-08-16T22:00:01Z",
			"stopped_at":          "2026-08-16T22:05:00Z",
		})
	}))
	defer server.Close()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := p.Complete(context.Background(), "work_123", false)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if requestedPath != "/v1/environments/env_123/work/work_123/stop" {
		t.Errorf("requested path = %q, want '/v1/environments/env_123/work/work_123/stop'", requestedPath)
	}
	if res.State != anthropic.BetaSelfHostedWorkStateStopped {
		t.Errorf("State = %v, want stopped", res.State)
	}
}

func workItemJSON(workID, sessionID string) map[string]any {
	return map[string]any{
		"id":             workID,
		"environment_id": "env_123",
		"type":           "work",
		"state":          "active",
		"data": map[string]any{
			"id":   sessionID,
			"type": "session",
		},
		"created_at":          "2026-08-16T22:00:00Z",
		"acknowledged_at":     "2026-08-16T22:00:01Z",
		"latest_heartbeat_at": "2026-08-16T22:00:01Z",
		"started_at":          "2026-08-16T22:00:01Z",
		"secret":              "sec_test_secret",
	}
}

// TestPollLoopLeaseHandoff proves a poll is followed by an ack and then a
// first heartbeat that claims the lease with NO_HEARTBEAT and the configured
// TTL, and that the handler receives the last_heartbeat the server returned.
func TestPollLoopLeaseHandoff(t *testing.T) {
	var (
		mu        sync.Mutex
		sequence  []string
		pollCalls int32
		hbQuery   url.Values
	)
	const serverLastHeartbeat = "2026-08-16T22:00:02.123456Z"

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")

		switch req.URL.Path {
		case "/v1/environments/env_123/work/poll":
			calls := atomic.AddInt32(&pollCalls, 1)
			if calls == 1 {
				if got := req.URL.Query().Get("reclaim_older_than_ms"); got != "5000" {
					t.Errorf("reclaim_older_than_ms = %q, want 5000", got)
				}
				mu.Lock()
				sequence = append(sequence, "poll")
				mu.Unlock()
				_ = json.NewEncoder(rw).Encode(workItemJSON("work_first_1", "sesn_first_1"))
				return
			}
			rw.WriteHeader(http.StatusNotFound)
		case "/v1/environments/env_123/work/work_first_1/ack":
			mu.Lock()
			sequence = append(sequence, "ack")
			mu.Unlock()
			_ = json.NewEncoder(rw).Encode(workItemJSON("work_first_1", "sesn_first_1"))
		case "/v1/environments/env_123/work/work_first_1/heartbeat":
			mu.Lock()
			sequence = append(sequence, "heartbeat")
			hbQuery = req.URL.Query()
			mu.Unlock()
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"type":           "work_heartbeat",
				"last_heartbeat": serverLastHeartbeat,
				"lease_extended": true,
				"state":          "active",
				"ttl_seconds":    120,
			})
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			http.NotFound(rw, req)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.cfg.HandoffTTLSeconds != DefaultHandoffTTLSeconds {
		t.Fatalf("HandoffTTLSeconds = %d, want default %d", p.cfg.HandoffTTLSeconds, DefaultHandoffTTLSeconds)
	}

	handled := make(chan Claim, 1)
	go func() {
		_ = p.Poll(ctx, func(ctx context.Context, claim Claim) error {
			handled <- claim
			return nil
		})
	}()

	var claim Claim
	select {
	case claim = <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for work item to be handled")
	}
	cancel()

	if claim.Work.ID != "work_first_1" {
		t.Errorf("handled work ID = %q, want 'work_first_1'", claim.Work.ID)
	}
	if claim.Work.Secret != "sec_test_secret" {
		t.Errorf("handled work secret = %q, want the poll response's secret", claim.Work.Secret)
	}
	if claim.LastHeartbeat != serverLastHeartbeat {
		t.Errorf("LastHeartbeat = %q, want %q", claim.LastHeartbeat, serverLastHeartbeat)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"poll", "ack", "heartbeat"}
	if len(sequence) != len(want) {
		t.Fatalf("request sequence = %v, want %v", sequence, want)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("request sequence = %v, want %v", sequence, want)
		}
	}
	if got := hbQuery.Get("expected_last_heartbeat"); got != "NO_HEARTBEAT" {
		t.Errorf("expected_last_heartbeat = %q, want NO_HEARTBEAT", got)
	}
	if got := hbQuery.Get("desired_ttl_seconds"); got != "120" {
		t.Errorf("desired_ttl_seconds = %q, want 120", got)
	}
}

// TestPollLoopSkipsItemWhenAckFails proves an item whose ack fails is neither
// heartbeated nor handed to the handler.
func TestPollLoopSkipsItemWhenAckFails(t *testing.T) {
	var (
		pollCalls      int32
		heartbeatCalls int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1/environments/env_123/work/poll":
			if atomic.AddInt32(&pollCalls, 1) == 1 {
				_ = json.NewEncoder(rw).Encode(workItemJSON("work_bad_1", "sesn_bad_1"))
				return
			}
			rw.WriteHeader(http.StatusNotFound)
		case "/v1/environments/env_123/work/work_bad_1/ack":
			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
		case "/v1/environments/env_123/work/work_bad_1/heartbeat":
			atomic.AddInt32(&heartbeatCalls, 1)
			rw.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(rw, req)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
		option.WithMaxRetries(0),
	)
	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handled := make(chan Claim, 1)
	go func() {
		_ = p.Poll(ctx, func(ctx context.Context, claim Claim) error {
			handled <- claim
			return nil
		})
	}()

	select {
	case claim := <-handled:
		t.Fatalf("handler received %q although its ack failed", claim.Work.ID)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()

	if atomic.LoadInt32(&pollCalls) < 2 {
		t.Errorf("poll calls = %d, want the loop to keep polling after the failed ack", pollCalls)
	}
	if atomic.LoadInt32(&heartbeatCalls) != 0 {
		t.Errorf("heartbeat calls = %d, want 0 after a failed ack", heartbeatCalls)
	}
}

// TestLeaseReleasesItemWhenHeartbeatFails proves an acked item whose first
// heartbeat fails is force-stopped rather than left acked and unheartbeated.
func TestLeaseReleasesItemWhenHeartbeatFails(t *testing.T) {
	var (
		mu       sync.Mutex
		sequence []string
		stopBody string
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/v1/environments/env_123/work/work_hb_1/ack":
			sequence = append(sequence, "ack")
			_ = json.NewEncoder(rw).Encode(workItemJSON("work_hb_1", "sesn_hb_1"))
		case "/v1/environments/env_123/work/work_hb_1/heartbeat":
			sequence = append(sequence, "heartbeat")
			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = rw.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
		case "/v1/environments/env_123/work/work_hb_1/stop":
			sequence = append(sequence, "stop")
			var body bytes.Buffer
			_, _ = body.ReadFrom(req.Body)
			stopBody = body.String()
			_ = json.NewEncoder(rw).Encode(workItemJSON("work_hb_1", "sesn_hb_1"))
		default:
			http.NotFound(rw, req)
		}
	}))
	defer server.Close()

	client := anthropic.NewClient(
		option.WithBaseURL(server.URL),
		option.WithAPIKey("test-api-key"),
		option.WithMaxRetries(0),
	)
	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	item := &anthropic.BetaSelfHostedWork{ID: "work_hb_1"}
	if _, err := p.Lease(context.Background(), item); err == nil {
		t.Fatal("Lease: expected error when the first heartbeat fails")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"ack", "heartbeat", "stop"}
	if len(sequence) != len(want) {
		t.Fatalf("request sequence = %v, want %v", sequence, want)
	}
	for i := range want {
		if sequence[i] != want[i] {
			t.Fatalf("request sequence = %v, want %v", sequence, want)
		}
	}
	if !strings.Contains(stopBody, `"force":true`) {
		t.Errorf("stop body = %s, want force:true", stopBody)
	}
}
