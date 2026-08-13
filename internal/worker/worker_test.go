package worker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate-anthropic/internal/config"
)

func TestNewWorkerInvalidConfig(t *testing.T) {
	_, err := New(&config.Config{})
	if err == nil {
		t.Error("expected error for empty config, got nil")
	}
}

func TestNewWorkerValidConfig(t *testing.T) {
	cfg := &config.Config{
		EnvironmentID:  "env_test_01",
		EnvironmentKey: "anthropic_env_key_test",
		Workdir:        t.TempDir(),
		ListenAddr:     "127.0.0.1:8080",
		Mode:           config.ModePoll,
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create worker: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil worker")
	}
}

func TestWorkerHealthzAndReadyz(t *testing.T) {
	cfg := &config.Config{
		EnvironmentID:  "env_test_01",
		EnvironmentKey: "anthropic_env_key_test",
		Workdir:        t.TempDir(),
		ListenAddr:     "127.0.0.1:8080",
		Mode:           config.ModePoll,
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create worker: %v", err)
	}

	handler := w.Handler()

	// Test healthz
	reqHealth := httptest.NewRequest("GET", "http://127.0.0.1:8080/healthz", nil)
	recHealth := httptest.NewRecorder()
	handler.ServeHTTP(recHealth, reqHealth)

	if recHealth.Code != http.StatusOK {
		t.Errorf("healthz code = %d, want %d", recHealth.Code, http.StatusOK)
	}
	if body := strings.TrimSpace(recHealth.Body.String()); body != "ok" {
		t.Errorf("healthz body = %q, want %q", body, "ok")
	}

	// Test readyz before ready
	reqReady1 := httptest.NewRequest("GET", "http://127.0.0.1:8080/readyz", nil)
	recReady1 := httptest.NewRecorder()
	handler.ServeHTTP(recReady1, reqReady1)
	if recReady1.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before ready code = %d, want %d", recReady1.Code, http.StatusServiceUnavailable)
	}

	// Mark ready
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()

	// Test readyz after ready
	reqReady2 := httptest.NewRequest("GET", "http://127.0.0.1:8080/readyz", nil)
	recReady2 := httptest.NewRecorder()
	handler.ServeHTTP(recReady2, reqReady2)
	if recReady2.Code != http.StatusOK {
		t.Errorf("readyz after ready code = %d, want %d", recReady2.Code, http.StatusOK)
	}
}
