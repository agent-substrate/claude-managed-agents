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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
)

type fakePagedControlClient struct {
	ateapipb.ControlClient
	pages map[string]*ateapipb.ListActorsResponse
}

func (f *fakePagedControlClient) ListActors(_ context.Context, in *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	if resp, ok := f.pages[in.GetPageToken()]; ok {
		return resp, nil
	}
	return &ateapipb.ListActorsResponse{}, nil
}

func TestDashboardCollectStatsPagination(t *testing.T) {
	client := &fakePagedControlClient{
		pages: map[string]*ateapipb.ListActorsResponse{
			"": {
				Actors: []*ateapipb.Actor{
					{
						Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "old-actor-1"},
						Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
					},
				},
				NextPageToken: "page-2",
			},
			"page-2": {
				Actors: []*ateapipb.Actor{
					{
						Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "new-actor-2"},
						Status: &ateapipb.ActorStatus{
							State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
							WorkerAssignment: &ateapipb.WorkerAssignment{WorkerPod: "worker-0"},
						},
					},
				},
				NextPageToken: "",
			},
		},
	}

	srv := NewServer(Config{Namespace: "ate-ant"}, client, nil, nil)
	stats := srv.collectStats(context.Background())

	if stats.Actors.Total != 2 {
		t.Fatalf("stats.Actors.Total = %d, want 2", stats.Actors.Total)
	}
	if stats.Actors.Suspended != 1 {
		t.Fatalf("stats.Actors.Suspended = %d, want 1", stats.Actors.Suspended)
	}
	if stats.Actors.Running != 1 {
		t.Fatalf("stats.Actors.Running = %d, want 1", stats.Actors.Running)
	}
	if stats.Workers.WorkerPoolClaimed != 1 {
		t.Fatalf("stats.Workers.WorkerPoolClaimed = %d, want 1", stats.Workers.WorkerPoolClaimed)
	}
}

func TestDashboardEndpointsAndSecurityHeaders(t *testing.T) {
	srv := NewServer(Config{Namespace: "ate-ant"}, nil, nil, nil)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantContent string
	}{
		{
			name:        "index page",
			method:      http.MethodGet,
			path:        "/",
			wantStatus:  http.StatusOK,
			wantContent: "Agent Substrate",
		},
		{
			name:        "styles",
			method:      http.MethodGet,
			path:        "/styles.css",
			wantStatus:  http.StatusOK,
			wantContent: "--page:",
		},
		{
			name:        "script",
			method:      http.MethodGet,
			path:        "/app.js",
			wantStatus:  http.StatusOK,
			wantContent: "/api/stats",
		},
		{
			name:        "healthz",
			method:      http.MethodGet,
			path:        "/healthz",
			wantStatus:  http.StatusOK,
			wantContent: "ok",
		},
		{
			name:       "post rejected",
			method:     http.MethodPost,
			path:       "/api/stats",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if tc.wantContent != "" && !strings.Contains(string(bodyBytes), tc.wantContent) {
				t.Errorf("body does not contain %q", tc.wantContent)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
			if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
				t.Errorf("Content-Security-Policy = %q, missing default-src 'self'", got)
			}
		})
	}

	t.Run("api stats json", func(t *testing.T) {
		resp, err := ts.Client().Get(ts.URL + "/api/stats")
		if err != nil {
			t.Fatalf("Get /api/stats: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var stats DashboardStats
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			t.Fatalf("Decode stats: %v", err)
		}
		if stats.Namespace != "ate-ant" {
			t.Errorf("Namespace = %q, want ate-ant", stats.Namespace)
		}
		if stats.Workers.WorkerPoolDesired != 0 {
			t.Errorf("WorkerPoolDesired = %d, want 0 outside the cluster", stats.Workers.WorkerPoolDesired)
		}
	})
}
