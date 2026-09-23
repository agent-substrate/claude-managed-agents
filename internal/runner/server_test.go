// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunnerHTTPHandler(t *testing.T) {
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.handleItem = func(context.Context, WorkRequest) error { return nil }

	server := httptest.NewServer(r.Handler())
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

func TestRunnerStartWorkMissingIDs(t *testing.T) {
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.handleItem = func(context.Context, WorkRequest) error { return nil }

	if err := r.StartWork(t.Context(), WorkRequest{SessionID: ""}); err == nil {
		t.Error("expected error when sessionID is empty, got nil")
	}
}

func TestRunnerStartWorkExecution(t *testing.T) {
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	var got WorkRequest
	r.handleItem = func(_ context.Context, req WorkRequest) error {
		got = req
		return nil
	}

	if err := r.StartWork(t.Context(), WorkRequest{
		SessionID:     "sesn_test_123",
		WorkID:        "work_test_123",
		LastHeartbeat: "2026-08-16T22:00:01Z",
	}); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
	if got.SessionID != "sesn_test_123" || got.WorkID != "work_test_123" || got.LastHeartbeat != "2026-08-16T22:00:01Z" {
		t.Errorf("handleItem received %+v", got)
	}

	// A missing work id falls back to the session id, as before.
	if err := r.StartWork(t.Context(), WorkRequest{SessionID: "sesn_test_123"}); err != nil {
		t.Fatalf("StartWork: %v", err)
	}
	if got.WorkID != "sesn_test_123" {
		t.Errorf("WorkID = %q, want session id fallback", got.WorkID)
	}
}

func TestRunnerSingleExecutionConcurrency(t *testing.T) {
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.handleItem = func(context.Context, WorkRequest) error { return nil }

	server := httptest.NewServer(r.Handler())
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

func TestRunnerIdleEndpoint(t *testing.T) {
	r, err := NewRunner(RunnerConfig{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.handleItem = func(context.Context, WorkRequest) error { return nil }

	server := httptest.NewServer(r.Handler())
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
	r.workMu.Lock() // block background work so it stays running
	payload, _ := json.Marshal(WorkRequest{SessionID: "sesn_idle_test"})
	resp, err = http.Post(server.URL+"/work", "application/json", bytes.NewReader(payload))
	if err != nil {
		r.workMu.Unlock()
		t.Fatalf("POST /work: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(server.URL + "/idle")
	if err != nil {
		r.workMu.Unlock()
		t.Fatalf("GET /idle while running: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&idle)
	resp.Body.Close()
	r.workMu.Unlock()

	if idle.IdleDuration != "0s" {
		t.Errorf("expected idle_duration = '0s' while running, got %q", idle.IdleDuration)
	}
}
