package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/life"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	cfg.Defaults()

	if cfg.Logger == nil {
		t.Error("logger = nil, want a default logger")
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
	key := in.GetActor().GetMetadata().Atespace + "/" + in.GetActor().GetMetadata().Name
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
		if in.Atespace == "" || a.Metadata.Atespace == in.Atespace {
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

func TestDispatcherLifecycle(t *testing.T) {
	fakeClient := newFakeControlClient()

	mockRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer mockRouter.Close()

	cfg := Config{}

	sessionID := "sesn_test_dispatcher_01"
	aName := life.ActorName(sessionID)
	key := "ate-antworker/" + aName

	lifeMgr := life.NewManager(fakeClient, "ate-antworker-actor-template", "ate-antworker").
		WithRouterAddr(mockRouter.Listener.Addr().String()).
		WithLogger(cfg.Logger)

	d := NewDispatcher(lifeMgr, cfg, sessionID, "work_test_01")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.Run(ctx); err != nil {
		t.Fatalf("d.Run: %v", err)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()
	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Errorf("expected actor %q to be created", key)
	}
}
