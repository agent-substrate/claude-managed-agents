package worker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHTTPHandler(t *testing.T) {
	w, err := NewWorker(WorkerConfig{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.antBinary = "true"

	server := httptest.NewServer(w.Handler())
	defer server.Close()

	// 1. Health check
	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Dispatch work item via JSON payload to /work
	payload, _ := json.Marshal(WorkRequest{SessionID: "sesn_01UgEbQopdaDjL8DxCvjMwat"})
	resp, err = http.Post(server.URL+"/work", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /work: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /work status = %d, want 200", resp.StatusCode)
	}
	var startResp WorkResponse
	_ = json.NewDecoder(resp.Body).Decode(&startResp)
	resp.Body.Close()
	if startResp.Status != "accepted" {
		t.Errorf("unexpected dispatch response: %+v", startResp)
	}

	// 3. Missing session ID should return 400
	resp, err = http.Post(server.URL+"/work", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST /work empty: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /work empty status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Work item with session_id alone should succeed
	payload, _ = json.Marshal(WorkRequest{SessionID: "sesn_01UgEbQopdaDjL8DxCvjMwat"})
	resp, err = http.Post(server.URL+"/work", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /work with session_id: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /work with session_id status = %d, want 200", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&startResp)
	resp.Body.Close()
	if startResp.Status != "accepted" {
		t.Errorf("unexpected dispatch response: %+v", startResp)
	}
}

func TestWorkerStartWorkMissingIDs(t *testing.T) {
	w, err := NewWorker(WorkerConfig{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.antBinary = "true"

	if _, _, err := w.StartWork(t.Context(), WorkRequest{SessionID: ""}); err == nil {
		t.Error("expected error when sessionID is empty, got nil")
	}
}

func TestWorkerStartWorkExecution(t *testing.T) {
	w, err := NewWorker(WorkerConfig{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.antBinary = "true"

	if _, _, err := w.StartWork(t.Context(), WorkRequest{
		SessionID:      "sesn_test_123",
		EnvironmentID:  "env_test_123",
		EnvironmentKey: "key_test_123",
	}); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
}

func TestWorkerSingleExecutionConcurrency(t *testing.T) {
	w, err := NewWorker(WorkerConfig{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.antBinary = "true"

	server := httptest.NewServer(w.Handler())
	defer server.Close()

	// Dispatch multiple concurrent requests
	for i := 0; i < 5; i++ {
		payload, _ := json.Marshal(WorkRequest{SessionID: "sesn_concurrent_test"})
		resp, err := http.Post(server.URL+"/work", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST /work: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /work status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestWorkerIdleEndpoint(t *testing.T) {
	w, err := NewWorker(WorkerConfig{})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.antBinary = "true"

	server := httptest.NewServer(w.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/idle")
	if err != nil {
		t.Fatalf("GET /idle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /idle status = %d, want 200", resp.StatusCode)
	}

	var idle IdleStatus
	if err := json.NewDecoder(resp.Body).Decode(&idle); err != nil {
		t.Fatalf("decode /idle response: %v", err)
	}
	resp.Body.Close()

	if idle.IdleDuration == "" {
		t.Errorf("expected non-empty idle_duration")
	}
	if idle.LastActiveAt == "" {
		t.Errorf("expected non-empty last_active_at")
	}

	// 2. Dispatch work -> actor is marked running, /idle returns 0s
	w.workMu.Lock() // block background work so it stays running
	payload, _ := json.Marshal(WorkRequest{SessionID: "sesn_idle_test"})
	resp, err = http.Post(server.URL+"/work", "application/json", bytes.NewReader(payload))
	if err != nil {
		w.workMu.Unlock()
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(server.URL + "/idle")
	if err != nil {
		w.workMu.Unlock()
		t.Fatalf("GET /idle while running: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&idle)
	resp.Body.Close()
	w.workMu.Unlock()

	if idle.IdleDuration != "0s" {
		t.Errorf("expected idle_duration = '0s' while running, got %q", idle.IdleDuration)
	}
}
