package life

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
	mu sync.Mutex

	createdAtespaces map[string]*ateapipb.Atespace
	createdActors    map[string]*ateapipb.Actor
	resumedActors    map[string]int
	suspendedActors  map[string]int

	resumeErr  error
	createErr  error
	suspendErr error
}

func newFakeControlClient() *fakeControlClient {
	return &fakeControlClient{
		createdAtespaces: make(map[string]*ateapipb.Atespace),
		createdActors:    make(map[string]*ateapipb.Actor),
		resumedActors:    make(map[string]int),
		suspendedActors:  make(map[string]int),
	}
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
	actor.Status = ateapipb.Actor_STATUS_RUNNING
	f.createdActors[key] = actor
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (f *fakeControlClient) UpdateActor(ctx context.Context, in *ateapipb.UpdateActorRequest, opts ...grpc.CallOption) (*ateapipb.UpdateActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if a, ok := f.createdActors[key]; ok {
		a.Status = ateapipb.Actor_STATUS_SUSPENDED
	}
	f.suspendedActors[key]++
	return &ateapipb.SuspendActorResponse{}, nil
}

func (f *fakeControlClient) PauseActor(ctx context.Context, in *ateapipb.PauseActorRequest, opts ...grpc.CallOption) (*ateapipb.PauseActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
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
	f.createdActors[key].Status = ateapipb.Actor_STATUS_RUNNING
	f.resumedActors[key]++
	return &ateapipb.ResumeActorResponse{Actor: proto.Clone(f.createdActors[key]).(*ateapipb.Actor), Resumed: true}, nil
}

func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) GetActorSnapshot(ctx context.Context, in *ateapipb.GetActorSnapshotRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshot, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) ListActorSnapshots(ctx context.Context, in *ateapipb.ListActorSnapshotsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) TagActorSnapshot(ctx context.Context, in *ateapipb.TagActorSnapshotRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) UpdateActorSnapshotTag(ctx context.Context, in *ateapipb.UpdateActorSnapshotTagRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) DeleteActorSnapshotTag(ctx context.Context, in *ateapipb.DeleteActorSnapshotTagRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
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

func (f *fakeControlClient) ListAtespaces(ctx context.Context, in *ateapipb.ListAtespacesRequest, opts ...grpc.CallOption) (*ateapipb.ListAtespacesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
}

func (f *fakeControlClient) DeleteAtespace(ctx context.Context, in *ateapipb.DeleteAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in fake")
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
	mgr.lastResumed[name] = time.Now().Add(-DefaultResumeCooldown - time.Second)
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
		Status: ateapipb.Actor_STATUS_RUNNING,
	}
	fakeClient.createdActors["test-ns/"+actorName2] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: "test-ns",
			Name:     actorName2,
		},
		Status: ateapipb.Actor_STATUS_RUNNING,
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

	if err := mgr.SuspendIdleActors(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("SuspendIdleActors: %v", err)
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

func TestManagerNotify(t *testing.T) {
	fakeClient := newFakeControlClient()
	sessionID := "session-notify-test-01"
	aName := ActorName(sessionID)

	var receivedHost, receivedPath string
	var receivedBody map[string]string

	routerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		receivedHost = req.Host
		receivedPath = req.URL.Path
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
		Status:   ateapipb.Actor_STATUS_RUNNING,
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
	mgr.lastResumed[aName] = time.Now().Add(-DefaultResumeCooldown - time.Second)
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
