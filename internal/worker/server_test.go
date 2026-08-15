package worker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHTTPHandler(t *testing.T) {
	w := NewWorker(nil)
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
	if startResp.Status != "completed" {
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
	if startResp.Status != "completed" {
		t.Errorf("unexpected dispatch response: %+v", startResp)
	}
}

func TestWorkerStartWorkMissingIDs(t *testing.T) {
	w := NewWorker(nil)
	w.antBinary = "true"

	if _, _, err := w.StartWork(t.Context(), WorkRequest{SessionID: ""}); err == nil {
		t.Error("expected error when sessionID is empty, got nil")
	}
}

func TestWorkerStartWorkExecution(t *testing.T) {
	w := NewWorker(nil)
	w.antBinary = "true"

	if _, _, err := w.StartWork(t.Context(), WorkRequest{
		SessionID:      "sesn_test_123",
		EnvironmentID:  "env_test_123",
		EnvironmentKey: "key_test_123",
	}); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
}

func TestWorkerHeartbeatInterception(t *testing.T) {
	w := NewWorker(nil)
	server := httptest.NewServer(w.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/environments/env_123/work/sesn_123/heartbeat", "application/json", nil)
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST heartbeat status = %d, want 200", resp.StatusCode)
	}
	var res map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	if res["state"] != "active" || res["lease_extended"] != true {
		t.Errorf("unexpected heartbeat response: %v", res)
	}
}
