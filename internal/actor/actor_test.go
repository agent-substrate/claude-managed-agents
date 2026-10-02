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

package actor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var rfc1123Pattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestActorName(t *testing.T) {
	testIDs := []string{
		"sesn_01EMNQKbnH4XXPnEWomkYLEB",
		"session_011CYP15aXyBfC52xQ9s67mn",
		"SESSION_ABC_123",
		"___special_chars&&&___",
		"123_abc",
		"",
		"session_a_very_long_session_id_with_more_than_sixty_three_characters_long_indeed_abcdef123456",
	}

	for _, id := range testIDs {
		name := ActorName(id)
		if !rfc1123Pattern.MatchString(name) {
			t.Errorf("ActorName(%q) = %q does not match RFC 1123 regex", id, name)
		}
		if len(name) > 63 {
			t.Errorf("ActorName(%q) = %q length = %d > 63", id, name, len(name))
		}
		if second := ActorName(id); second != name {
			t.Errorf("ActorName(%q) not deterministic: %q vs %q", id, name, second)
		}
	}
}

type fakeControlClient struct {
	ateapipb.ControlClient
	mu sync.Mutex

	createdAtespaces       map[string]*ateapipb.Atespace
	createdActors          map[string]*ateapipb.Actor
	createdEgressPolicies  map[string]*ateapipb.EgressPolicy
	resumedActors          map[string]int
	suspendedActors        map[string]int
	revertedActors         map[string]int
	deletedActors          map[string]int

	resumeErr  error
	createErr  error
	suspendErr error
	revertErr  error
}

func newFakeControlClient() *fakeControlClient {
	return &fakeControlClient{
		createdAtespaces:      make(map[string]*ateapipb.Atespace),
		createdActors:         make(map[string]*ateapipb.Actor),
		createdEgressPolicies: make(map[string]*ateapipb.EgressPolicy),
		resumedActors:         make(map[string]int),
		suspendedActors:       make(map[string]int),
		revertedActors:        make(map[string]int),
		deletedActors:         make(map[string]int),
	}
}

func (f *fakeControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := in.GetActor().GetAtespace() + "/" + in.GetActor().GetName()
	if _, ok := f.createdEgressPolicies[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "egress policy already exists")
	}
	policy := proto.Clone(in.GetEgressPolicy()).(*ateapipb.EgressPolicy)
	f.createdEgressPolicies[key] = policy
	return policy, nil
}

func (f *fakeControlClient) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if a, ok := f.createdActors[key]; ok {
		return proto.Clone(a).(*ateapipb.Actor), nil
	}
	return nil, status.Error(codes.NotFound, "actor not found")
}

func (f *fakeControlClient) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	key := in.GetActor().GetMetadata().GetAtespace() + "/" + in.GetActor().GetMetadata().GetName()
	if _, ok := f.createdActors[key]; ok {
		return nil, status.Error(codes.AlreadyExists, "actor already exists")
	}
	actor := proto.Clone(in.GetActor()).(*ateapipb.Actor)
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}
	f.createdActors[key] = actor
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (f *fakeControlClient) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if a, ok := f.createdActors[key]; ok {
		a.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	}
	f.suspendedActors[key]++
	return &ateapipb.SuspendActorResponse{}, nil
}

func (f *fakeControlClient) RevertActor(ctx context.Context, in *ateapipb.RevertActorRequest, opts ...grpc.CallOption) (*ateapipb.RevertActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revertErr != nil {
		return nil, f.revertErr
	}
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if a, ok := f.createdActors[key]; ok {
		a.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	}
	f.revertedActors[key]++
	return &ateapipb.RevertActorResponse{}, nil
}

func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	delete(f.createdActors, key)
	f.deletedActors[key]++
	return &ateapipb.Actor{}, nil
}

func (f *fakeControlClient) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if _, ok := f.createdActors[key]; !ok {
		return nil, status.Error(codes.NotFound, "actor not found")
	}
	f.createdActors[key].Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}
	f.resumedActors[key]++
	return &ateapipb.ResumeActorResponse{Actor: proto.Clone(f.createdActors[key]).(*ateapipb.Actor), Resumed: true}, nil
}

func (f *fakeControlClient) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var actors []*ateapipb.Actor
	for _, a := range f.createdActors {
		if in.Atespace == "" || a.GetMetadata().GetAtespace() == in.Atespace {
			actors = append(actors, proto.Clone(a).(*ateapipb.Actor))
		}
	}
	return &ateapipb.ListActorsResponse{Actors: actors}, nil
}

func (f *fakeControlClient) CreateAtespace(ctx context.Context, in *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetAtespace().GetMetadata().GetName()
	if _, ok := f.createdAtespaces[name]; ok {
		return nil, status.Error(codes.AlreadyExists, "atespace already exists")
	}
	atespace := in.GetAtespace()
	f.createdAtespaces[name] = atespace
	return atespace, nil
}

func (f *fakeControlClient) GetAtespace(ctx context.Context, in *ateapipb.GetAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetAtespace().GetName()
	if a, ok := f.createdAtespaces[name]; ok {
		return a, nil
	}
	return nil, status.Error(codes.NotFound, "atespace not found")
}

func TestManagerLifecycle(t *testing.T) {
	fakeClient := newFakeControlClient()
	mgr := NewManager(fakeClient, "test-template", "test-ns")

	sessionID := "session-lifecycle-test-01"
	name := ActorName(sessionID)

	// 1. Resume -> creates actor since it doesn't exist
	if err := mgr.Resume(context.Background(), sessionID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	fakeClient.mu.Lock()
	if _, ok := fakeClient.createdActors["test-ns/"+name]; !ok {
		t.Errorf("expected actor test-ns/%s to be created", name)
	}
	if policy, ok := fakeClient.createdEgressPolicies["test-ns/"+name]; !ok {
		t.Errorf("expected egress policy test-ns/%s to be created", name)
	} else if len(policy.GetRules()) == 0 {
		t.Errorf("expected egress policy to have rules")
	}
	fakeClient.mu.Unlock()

	// 2. Resume again -> calls ResumeActor
	if err := mgr.Resume(context.Background(), sessionID); err != nil {
		t.Fatalf("Resume again: %v", err)
	}

	fakeClient.mu.Lock()
	if fakeClient.resumedActors["test-ns/"+name] != 1 {
		t.Errorf("expected ResumeActor count = 1, got %d", fakeClient.resumedActors["test-ns/"+name])
	}
	fakeClient.mu.Unlock()

	// 3. Suspend immediately -> ignored due to 10s cooldown
	if err := mgr.Suspend(context.Background(), sessionID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	fakeClient.mu.Lock()
	if fakeClient.suspendedActors["test-ns/"+name] != 0 {
		t.Errorf("expected SuspendActor count = 0 (ignored), got %d", fakeClient.suspendedActors["test-ns/"+name])
	}
	fakeClient.mu.Unlock()

	// 4. Suspend after cooldown
	mgr.mu.Lock()
	mgr.lastResumed[name] = time.Now().Add(-defaultResumeCooldown - time.Second)
	mgr.mu.Unlock()

	if err := mgr.Suspend(context.Background(), sessionID); err != nil {
		t.Fatalf("Suspend after cooldown: %v", err)
	}

	fakeClient.mu.Lock()
	if fakeClient.suspendedActors["test-ns/"+name] != 1 {
		t.Errorf("expected SuspendActor count = 1, got %d", fakeClient.suspendedActors["test-ns/"+name])
	}
	fakeClient.mu.Unlock()
}

func TestManagerSuspendIdleActors(t *testing.T) {
	fakeClient := newFakeControlClient()
	actorName1 := "session-idle-test-01"
	actorName2 := "session-busy-test-02"

	fakeClient.createdActors["test-ns/"+actorName1] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     actorName1,
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}
	fakeClient.createdActors["test-ns/"+actorName2] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     actorName2,
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/idle" {
			http.NotFound(rw, req)
			return
		}

		host := req.Host
		rw.Header().Set("Content-Type", "application/json")
		if host == actorName1+".test-ns.actors.resources.substrate.ate.dev" {
			_ = json.NewEncoder(rw).Encode(IdleStatus{
				IdleDuration: "15s",
				LastActiveAt: time.Now().Add(-15 * time.Second).UTC().Format(time.RFC3339),
			})
		} else if host == actorName2+".test-ns.actors.resources.substrate.ate.dev" {
			_ = json.NewEncoder(rw).Encode(IdleStatus{
				IdleDuration: "0s",
				LastActiveAt: time.Now().UTC().Format(time.RFC3339),
			})
		} else {
			http.NotFound(rw, req)
		}
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)
	mgr.lastResumed[actorName1] = time.Now().Add(-25 * time.Second)
	mgr.lastResumed[actorName2] = time.Now()

	if err := mgr.suspendIdleActors(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("suspendIdleActors: %v", err)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()

	if fakeClient.suspendedActors["test-ns/"+actorName1] != 1 {
		t.Errorf("expected actor1 to be suspended, got %d", fakeClient.suspendedActors["test-ns/"+actorName1])
	}
	if fakeClient.suspendedActors["test-ns/"+actorName2] != 0 {
		t.Errorf("expected actor2 NOT to be suspended, got %d", fakeClient.suspendedActors["test-ns/"+actorName2])
	}
}

func TestManagerSuspendIdleActors_Cooldown(t *testing.T) {
	fakeClient := newFakeControlClient()
	actorName := "session-cooldown-idle-01"

	fakeClient.createdActors["test-ns/"+actorName] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     actorName,
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(IdleStatus{
			IdleDuration: "15s",
			LastActiveAt: time.Now().Add(-15 * time.Second).UTC().Format(time.RFC3339),
		})
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)
	// Mark actor as recently resumed
	mgr.recordResume(actorName)

	if err := mgr.suspendIdleActors(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("suspendIdleActors: %v", err)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()

	// Should NOT be suspended because it was resumed within the 10s threshold
	if fakeClient.suspendedActors["test-ns/"+actorName] != 0 {
		t.Errorf("expected actor NOT to be suspended due to resume grace period, got %d", fakeClient.suspendedActors["test-ns/"+actorName])
	}
}

func TestManagerNotify(t *testing.T) {
	fakeClient := newFakeControlClient()
	sessionID := "session-notify-test-01"
	aName := ActorName(sessionID)

	var receivedHost, receivedPath, receivedTargetActor string
	var receivedBody map[string]string

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		receivedHost = req.Host
		receivedPath = req.URL.Path
		receivedTargetActor = req.Header.Get("ate-target-actor")
		_ = json.NewDecoder(req.Body).Decode(&receivedBody)

		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]string{"status": "accepted"})
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)

	payload := map[string]string{
		"session_id": sessionID,
		"work_id":    "work_123",
	}

	if err := mgr.Notify(context.Background(), sessionID, payload); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	expectedHost := aName + ".test-ns.actors.resources.substrate.ate.dev"
	if receivedHost != expectedHost {
		t.Errorf("Host = %q, want %q", receivedHost, expectedHost)
	}
	expectedTargetActor := "test-ns/" + aName
	if receivedTargetActor != expectedTargetActor {
		t.Errorf("ate-target-actor = %q, want %q", receivedTargetActor, expectedTargetActor)
	}
	if receivedPath != "/work" {
		t.Errorf("Path = %q, want /work", receivedPath)
	}
	if receivedBody["work_id"] != "work_123" {
		t.Errorf("received payload work_id = %q, want work_123", receivedBody["work_id"])
	}
}

func TestSuspendIgnoredWithinCooldown(t *testing.T) {
	fakeClient := newFakeControlClient()
	mgr := NewManager(fakeClient, "test-template", "test-ns")
	sessionID := "session-cooldown-01"
	aName := ActorName(sessionID)

	fakeClient.createdActors["test-ns/"+aName] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "test-ns", Name: aName},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	// 1. Resume sets timestamp to now
	if err := mgr.Resume(context.Background(), sessionID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// 2. Immediate suspend -> ignored
	actorRef := &ateapipb.ObjectRef{Atespace: "test-ns", Name: aName}
	if err := mgr.SuspendActor(context.Background(), actorRef); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}

	fakeClient.mu.Lock()
	if fakeClient.suspendedActors["test-ns/"+aName] != 0 {
		t.Errorf("expected 0 suspensions within 10s cooldown, got %d", fakeClient.suspendedActors["test-ns/"+aName])
	}
	fakeClient.mu.Unlock()

	// 3. Fast-forward resume timestamp > cooldown
	mgr.mu.Lock()
	mgr.lastResumed[aName] = time.Now().Add(-defaultResumeCooldown - time.Second)
	mgr.mu.Unlock()

	if err := mgr.SuspendActor(context.Background(), actorRef); err != nil {
		t.Fatalf("SuspendActor after cooldown: %v", err)
	}

	fakeClient.mu.Lock()
	if fakeClient.suspendedActors["test-ns/"+aName] != 1 {
		t.Errorf("expected 1 suspension after cooldown, got %d", fakeClient.suspendedActors["test-ns/"+aName])
	}
	fakeClient.mu.Unlock()
}

func TestManagerStartGC_Success(t *testing.T) {
	fakeClient := newFakeControlClient()
	actorName1 := "session-gc-test-01"

	fakeClient.createdActors["test-ns/"+actorName1] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     actorName1,
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(IdleStatus{
			IdleDuration: "15s",
			LastActiveAt: time.Now().Add(-15 * time.Second).UTC().Format(time.RFC3339),
		})
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)
	mgr.lastResumed[actorName1] = time.Now().Add(-defaultResumeCooldown - time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr.StartGC(ctx, 10*time.Millisecond, 1*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fakeClient.mu.Lock()
		count := fakeClient.suspendedActors["test-ns/"+actorName1]
		fakeClient.mu.Unlock()
		if count >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()
	if fakeClient.suspendedActors["test-ns/"+actorName1] != 1 {
		t.Fatalf("expected actor to be suspended by GC, got %d", fakeClient.suspendedActors["test-ns/"+actorName1])
	}
}

func TestManagerSuspendIfIdle(t *testing.T) {
	fakeClient := newFakeControlClient()
	sessionID := "session-suspend-if-idle-01"
	aName := ActorName(sessionID)

	fakeClient.createdActors["test-ns/"+aName] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     aName,
		},
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	var calls int
	var callsMu sync.Mutex
	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		callsMu.Lock()
		calls++
		c := calls
		callsMu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		// First check reports busy ("0s"), second check reports idle ("100ms")
		idleStr := "0s"
		if c >= 2 {
			idleStr = "100ms"
		}
		_ = json.NewEncoder(rw).Encode(IdleStatus{
			IdleDuration: idleStr,
			LastActiveAt: time.Now().UTC().Format(time.RFC3339),
		})
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)
	mgr.lastResumed[aName] = time.Now().Add(-1 * time.Second)

	ctx := context.Background()

	// 1. Busy actor -> not suspended
	suspended, err := mgr.SuspendIfIdle(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("SuspendIfIdle (busy): %v", err)
	}
	if suspended {
		t.Fatalf("expected suspended = false when actor is busy")
	}

	// 2. Idle actor -> suspended
	suspended, err = mgr.SuspendIfIdle(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("SuspendIfIdle (idle): %v", err)
	}
	if !suspended {
		t.Fatalf("expected suspended = true when actor is idle")
	}

	// 3. Already suspended actor -> returns true immediately without another SuspendActor RPC
	suspended, err = mgr.SuspendIfIdle(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("SuspendIfIdle (already suspended): %v", err)
	}
	if !suspended {
		t.Fatalf("expected suspended = true when actor is already suspended")
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()
	if fakeClient.suspendedActors["test-ns/"+aName] != 1 {
		t.Fatalf("expected actor to be suspended once, got %d", fakeClient.suspendedActors["test-ns/"+aName])
	}
}

func TestManagerSuspendIfIdle_UnreachableReverts(t *testing.T) {
	fakeClient := newFakeControlClient()
	sessionID := "session-unreachable-revert"
	aName := ActorName(sessionID)

	fakeClient.createdActors["test-ns/"+aName] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "test-ns", Name: aName},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	}

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusBadGateway)
	}))
	defer routerServer.Close()

	mgr := NewManager(fakeClient, "test-template", "test-ns").WithRouterAddr(routerServer.URL)
	mgr.lastResumed[aName] = time.Now().Add(-10 * time.Second)

	ctx := context.Background()

	// /idle returns 502 -> RevertActor is called
	done, err := mgr.SuspendIfIdle(ctx, sessionID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("SuspendIfIdle (revert): %v", err)
	}
	if !done {
		t.Fatalf("expected done = true when unreachable actor is reverted")
	}
	if fakeClient.revertedActors["test-ns/"+aName] != 1 {
		t.Fatalf("expected actor to be reverted once, got %d", fakeClient.revertedActors["test-ns/"+aName])
	}
}

type pagedControlClient struct {
	ateapipb.ControlClient
	pages map[string]*ateapipb.ListActorsResponse
	calls int
}

func (p *pagedControlClient) ListActors(_ context.Context, in *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	p.calls++
	if resp, ok := p.pages[in.GetPageToken()]; ok {
		return resp, nil
	}
	return &ateapipb.ListActorsResponse{}, nil
}

func TestListAllActorsPagination(t *testing.T) {
	client := &pagedControlClient{
		pages: map[string]*ateapipb.ListActorsResponse{
			"": {
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "actor-1"}},
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "actor-2"}},
				},
				NextPageToken: "page-2",
			},
			"page-2": {
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "actor-3"}},
				},
				NextPageToken: "page-3",
			},
			"page-3": {
				Actors: []*ateapipb.Actor{
					{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-ant", Name: "actor-4"}},
				},
				NextPageToken: "",
			},
		},
	}

	got, err := ListAllActors(context.Background(), client, "ate-ant")
	if err != nil {
		t.Fatalf("ListAllActors: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4", len(got))
	}
	if client.calls != 3 {
		t.Fatalf("ListActors calls = %d, want 3", client.calls)
	}
}

func TestManagerCrashedActorRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("resume crashed actor reverts and resumes", func(t *testing.T) {
		fakeClient := newFakeControlClient()
		sessionID := "session-crashed-resume"
		aName := ActorName(sessionID)
		key := "test-ns/" + aName

		fakeClient.createdActors[key] = &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "test-ns", Name: aName},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
		}

		mgr := NewManager(fakeClient, "test-template", "test-ns")
		if err := mgr.Resume(ctx, sessionID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if fakeClient.revertedActors[key] != 1 {
			t.Fatalf("revertedActors = %d, want 1", fakeClient.revertedActors[key])
		}
		if fakeClient.resumedActors[key] != 1 {
			t.Fatalf("resumedActors = %d, want 1", fakeClient.resumedActors[key])
		}
	})

	t.Run("resume falls back to recreate when snapshot restore fails", func(t *testing.T) {
		fakeClient := newFakeControlClient()
		fakeClient.resumeErr = status.Error(codes.Internal, "runsc restore: exit status 128")
		sessionID := "session-unrestorable-snapshot"
		aName := ActorName(sessionID)
		key := "test-ns/" + aName

		fakeClient.createdActors[key] = &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "test-ns", Name: aName},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
		}

		mgr := NewManager(fakeClient, "test-template", "test-ns")
		if err := mgr.Resume(ctx, sessionID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if fakeClient.revertedActors[key] != 1 {
			t.Fatalf("revertedActors = %d, want 1", fakeClient.revertedActors[key])
		}
		if fakeClient.deletedActors[key] != 1 {
			t.Fatalf("deletedActors = %d, want 1", fakeClient.deletedActors[key])
		}
		if got := fakeClient.createdActors[key].GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			t.Fatalf("recreated actor state = %v, want RUNNING", got)
		}
	})

	t.Run("gc and SuspendIfIdle revert crashed actors immediately", func(t *testing.T) {
		fakeClient := newFakeControlClient()
		sessionID := "session-crashed-gc"
		aName := ActorName(sessionID)
		key := "test-ns/" + aName

		fakeClient.createdActors[key] = &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "test-ns", Name: aName},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
		}

		mgr := NewManager(fakeClient, "test-template", "test-ns")
		if err := mgr.suspendIdleActors(ctx, 10*time.Second); err != nil {
			t.Fatalf("suspendIdleActors: %v", err)
		}
		if fakeClient.revertedActors[key] != 1 {
			t.Fatalf("revertedActors = %d, want 1", fakeClient.revertedActors[key])
		}
	})
}

