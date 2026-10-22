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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateConfig(t *testing.T) {
	cfg := Config{Mode: ModeValkey}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate valid valkey config: %v", err)
	}
	if cfg.Sessions != 5000 {
		t.Errorf("default Sessions = %d, want 5000", cfg.Sessions)
	}

	cfg = Config{Mode: "invalid"}
	if err := cfg.Validate(); err == nil {
		t.Errorf("Validate invalid mode: expected error, got nil")
	}

	t.Setenv("ANTHROPIC_ENVIRONMENT_ID", "")
	cfg = Config{Mode: ModeAnthropic}
	if err := cfg.Validate(); err == nil {
		t.Errorf("Validate anthropic mode without env ID: expected error, got nil")
	}
}

func TestRecorderAndReport(t *testing.T) {
	rec := NewRecorder()
	rec.Record("op_a", 10*time.Millisecond, nil)
	rec.Record("op_a", 20*time.Millisecond, nil)
	rec.Record("op_a", 30*time.Millisecond, fmt.Errorf("failed"))

	cfg := Config{
		Mode:            ModeDirect,
		Sessions:        3,
		TurnsPerSession: 1,
		Concurrency:     2,
	}
	rep := rec.Report(cfg)
	if len(rep.Operations) != 1 {
		t.Fatalf("len(Operations) = %d, want 1", len(rep.Operations))
	}
	op := rep.Operations[0]
	if op.Total != 3 || op.Successes != 2 || op.Errors != 1 {
		t.Errorf("op counts = %+v, want total=3, successes=2, errors=1", op)
	}
	if op.MinMs != 10 || op.MaxMs != 30 {
		t.Errorf("op min/max = %v/%v, want 10/30", op.MinMs, op.MaxMs)
	}

	var buf bytes.Buffer
	PrintSummary(&buf, rep)
	if !strings.Contains(buf.String(), "op_a") {
		t.Errorf("PrintSummary output missing op_a:\n%s", buf.String())
	}
}

func TestRunValkeyMode(t *testing.T) {
	mr := miniredis.RunT(t)

	cfg := Config{
		Mode:             ModeValkey,
		Sessions:         25,
		Concurrency:      5,
		TurnsPerSession:  2,
		SuspendAfterTurn: true,
		ValkeyAddr:       mr.Addr(),
		SessionPrefix:    "testvk",
	}
	cfg.Defaults()

	rec := NewRecorder()
	if err := runValkeyMode(context.Background(), cfg, rec, slog.Default()); err != nil {
		t.Fatalf("runValkeyMode: %v", err)
	}

	rep := rec.Report(cfg)
	if len(rep.Operations) != 2 {
		t.Fatalf("got %d operations, want 2: %+v", len(rep.Operations), rep.Operations)
	}
	for _, op := range rep.Operations {
		if op.Successes != 50 || op.Errors != 0 {
			t.Errorf("operation %s: got successes=%d errors=%d, want 50/0", op.Operation, op.Successes, op.Errors)
		}
	}
}

type testControlServer struct {
	ateapipb.UnimplementedControlServer
	mu       sync.Mutex
	actors   map[string]*ateapipb.Actor
	resumes  int
	suspends int
}

func newTestControlServer() *testControlServer {
	return &testControlServer{
		actors: make(map[string]*ateapipb.Actor),
	}
}

func (s *testControlServer) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	return req.GetAtespace(), nil
}

func (s *testControlServer) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := req.GetActor().GetAtespace() + "/" + req.GetActor().GetName()
	if a, ok := s.actors[key]; ok {
		return a, nil
	}
	return nil, status.Error(codes.NotFound, "not found")
}

func (s *testControlServer) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := req.GetActor().GetMetadata().GetAtespace() + "/" + req.GetActor().GetMetadata().GetName()
	s.actors[key] = req.GetActor()
	return req.GetActor(), nil
}

func (s *testControlServer) CreateActorEgressPolicy(ctx context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	return req.GetEgressPolicy(), nil
}

func (s *testControlServer) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumes++
	return &ateapipb.ResumeActorResponse{
		Actor: &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{
			Atespace: req.GetActor().GetAtespace(),
			Name:     req.GetActor().GetName(),
		}},
		Resumed: true,
	}, nil
}

func (s *testControlServer) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspends++
	return &ateapipb.SuspendActorResponse{}, nil
}

func TestRunDirectMode(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	ctrl := newTestControlServer()
	ateapipb.RegisterControlServer(grpcSrv, ctrl)
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.Stop()

	var notifyCalls atomic.Int64
	routerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/work" {
			notifyCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
			return
		}
		http.NotFound(w, r)
	}))
	defer routerSrv.Close()

	cfg := Config{
		Mode:             ModeDirect,
		Sessions:         20,
		Concurrency:      4,
		TurnsPerSession:  2,
		Notify:           true,
		SuspendAfterTurn: true,
		AteapiAddr:       lis.Addr().String(),
		AtenetRouterAddr: routerSrv.URL,
		Insecure:         true,
		SessionPrefix:    "testdirect",
	}
	cfg.Defaults()

	rec := NewRecorder()
	if err := runDirectMode(context.Background(), cfg, rec, slog.Default()); err != nil {
		t.Fatalf("runDirectMode: %v", err)
	}

	if got := notifyCalls.Load(); got != 40 {
		t.Errorf("notifyCalls = %d, want 40", got)
	}
	ctrl.mu.Lock()
	if len(ctrl.actors) != 20 {
		t.Errorf("created actors = %d, want 20", len(ctrl.actors))
	}
	if ctrl.resumes != 20 {
		t.Errorf("resumed actors = %d, want 20 (turn 2)", ctrl.resumes)
	}
	if ctrl.suspends != 40 {
		t.Errorf("suspended actors = %d, want 40", ctrl.suspends)
	}
	ctrl.mu.Unlock()
}

func TestRunAnthropicMode(t *testing.T) {
	var createdSessions atomic.Int64
	var sentEvents atomic.Int64

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/agents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "agent_bench_123",
				"type": "agent",
				"name": "Agent Substrate 10k Benchmark Agent",
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/sessions"):
			id := createdSessions.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   fmt.Sprintf("sesn_bench_%d", id),
				"type": "session",
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/v1/sessions/") && strings.HasSuffix(r.URL.Path, "/events"):
			sentEvents.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	cfg := Config{
		Mode:            ModeAnthropic,
		Sessions:        10,
		Concurrency:     3,
		TurnsPerSession: 2,
		EnvironmentID:   "env_bench_test",
		APIKey:          "sk-ant-test",
		BaseURL:         apiSrv.URL,
	}
	cfg.Defaults()

	rec := NewRecorder()
	if err := runAnthropicMode(context.Background(), cfg, rec, slog.Default()); err != nil {
		t.Fatalf("runAnthropicMode: %v", err)
	}

	if got := createdSessions.Load(); got != 10 {
		t.Errorf("createdSessions = %d, want 10", got)
	}
	if got := sentEvents.Load(); got != 10 {
		t.Errorf("sentEvents = %d, want 10", got)
	}
}
