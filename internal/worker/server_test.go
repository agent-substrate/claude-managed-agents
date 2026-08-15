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

	// 2. Dispatch session via JSON payload to /dispatch
	payload, _ := json.Marshal(WorkRequest{WorkID: "work_test_123", SessionID: "sesn_test_123"})
	resp, err = http.Post(server.URL+"/dispatch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /dispatch status = %d, want 200", resp.StatusCode)
	}
	var startResp WorkResponse
	_ = json.NewDecoder(resp.Body).Decode(&startResp)
	resp.Body.Close()
	if startResp.Status != "completed" {
		t.Errorf("unexpected dispatch response: %+v", startResp)
	}

	// 3. Missing work ID should return 400
	resp, err = http.Post(server.URL+"/dispatch", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("POST /dispatch empty: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /dispatch empty status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Missing work ID with only session ID should return 400
	payload, _ = json.Marshal(WorkRequest{SessionID: "sesn_test_123"})
	resp, err = http.Post(server.URL+"/dispatch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /dispatch with only session_id: %v", err)
	}
	// 5. Missing session ID with only work ID should return 400
	payload, _ = json.Marshal(WorkRequest{WorkID: "work_test_123"})
	resp, err = http.Post(server.URL+"/dispatch", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /dispatch with only work_id: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /dispatch with only work_id status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWorkerStartWorkMissingIDs(t *testing.T) {
	w := NewWorker(nil)
	if err := w.StartWork(t.Context(), "", "sesn_123"); err == nil {
		t.Error("expected error when workID is empty, got nil")
	}
	if err := w.StartWork(t.Context(), "work_123", ""); err == nil {
		t.Error("expected error when sessionID is empty, got nil")
	}
	if err := w.StartWork(t.Context(), "", ""); err == nil {
		t.Error("expected error when both IDs are empty, got nil")
	}
}

func TestWorkerStartWorkExecution(t *testing.T) {
	w := NewWorker(nil)
	w.antBinary = "true"

	if err := w.StartWork(t.Context(), "work_test_123", "sesn_test_123"); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
}
