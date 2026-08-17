package poller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestPollLoop(t *testing.T) {
	var (
		pollCalls int32
		stopCalls int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")

		switch req.URL.Path {
		case "/v1/environments/env_123/work/poll":
			calls := atomic.AddInt32(&pollCalls, 1)
			if calls == 1 {
				_ = json.NewEncoder(rw).Encode(map[string]any{
					"id":             "work_first_1",
					"environment_id": "env_123",
					"type":           "work",
					"state":          "active",
					"data": map[string]any{
						"id":   "sesn_first_1",
						"type": "session",
					},
					"created_at":          "2026-08-16T22:00:00Z",
					"acknowledged_at":     "2026-08-16T22:00:01Z",
					"latest_heartbeat_at": "2026-08-16T22:00:01Z",
					"started_at":          "2026-08-16T22:00:01Z",
				})
				return
			}
			rw.WriteHeader(http.StatusNotFound)
		case "/v1/environments/env_123/work/work_first_1/stop":
			atomic.AddInt32(&stopCalls, 1)
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"id":             "work_first_1",
				"environment_id": "env_123",
				"type":           "work",
				"state":          "stopped",
				"data": map[string]any{
					"id":   "sesn_first_1",
					"type": "session",
				},
				"created_at":          "2026-08-16T22:00:00Z",
				"acknowledged_at":     "2026-08-16T22:00:01Z",
				"latest_heartbeat_at": "2026-08-16T22:00:01Z",
				"started_at":          "2026-08-16T22:00:01Z",
				"stopped_at":          "2026-08-16T22:05:00Z",
			})
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
	)

	p, err := New(Config{
		Client:        &client,
		EnvironmentID: "env_123",
		PollInterval:  10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	handled := make(chan string, 1)
	go func() {
		_ = p.Poll(ctx, func(ctx context.Context, item *anthropic.BetaSelfHostedWork) error {
			handled <- item.ID
			return nil
		})
	}()

	select {
	case id := <-handled:
		if id != "work_first_1" {
			t.Errorf("handled work ID = %q, want 'work_first_1'", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for work item to be handled")
	}

	time.Sleep(50 * time.Millisecond)
	cancel()
}
