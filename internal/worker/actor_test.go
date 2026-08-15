package worker

import (
	"context"
	"encoding/json"
	"errors"
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
		// Must be deterministic
		if second := ActorName(id); second != name {
			t.Errorf("ActorName(%q) not deterministic: %q vs %q", id, name, second)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	cfg.Defaults()

	if cfg.Namespace != DefaultNamespace {
		t.Errorf("namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.Template != DefaultTemplate {
		t.Errorf("template = %q, want %q", cfg.Template, DefaultTemplate)
	}
	if cfg.AteapiAddr != DefaultAteapiAddr {
		t.Errorf("ateapiAddr = %q, want %q", cfg.AteapiAddr, DefaultAteapiAddr)
	}
	if cfg.AtenetRouterAddr != DefaultAtenetRouterAddr {
		t.Errorf("atenetRouterAddr = %q, want %q", cfg.AtenetRouterAddr, DefaultAtenetRouterAddr)
	}
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
		return a, nil
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
	actor := in.GetActor()
	actor.Status = ateapipb.Actor_STATUS_RUNNING
	f.createdActors[key] = actor
	return actor, nil
}

func (f *fakeControlClient) UpdateActor(ctx context.Context, in *ateapipb.UpdateActorRequest, opts ...grpc.CallOption) (*ateapipb.UpdateActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) SuspendActor(ctx context.Context, in *ateapipb.SuspendActorRequest, opts ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.suspendErr != nil {
		return nil, f.suspendErr
	}
	key := in.GetActor().Atespace + "/" + in.GetActor().Name
	if a, ok := f.createdActors[key]; ok {
		if a.Status != ateapipb.Actor_STATUS_RUNNING {
			return nil, status.Errorf(codes.FailedPrecondition, "MarkSuspendingStep prerequisite not met for Actor: %s (got: %s, want STATUS_RUNNING)", in.GetActor().Name, a.Status)
		}
		a.Status = ateapipb.Actor_STATUS_SUSPENDED
	}
	f.suspendedActors[key]++
	return &ateapipb.SuspendActorResponse{}, nil
}

func (f *fakeControlClient) PauseActor(ctx context.Context, in *ateapipb.PauseActorRequest, opts ...grpc.CallOption) (*ateapipb.PauseActorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
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
	return &ateapipb.ResumeActorResponse{Actor: f.createdActors[key], Resumed: true}, nil
}

func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) GetActorSnapshot(ctx context.Context, in *ateapipb.GetActorSnapshotRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshot, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) ListActorSnapshots(ctx context.Context, in *ateapipb.ListActorSnapshotsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) TagActorSnapshot(ctx context.Context, in *ateapipb.TagActorSnapshotRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) UpdateActorSnapshotTag(ctx context.Context, in *ateapipb.UpdateActorSnapshotTagRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) DeleteActorSnapshotTag(ctx context.Context, in *ateapipb.DeleteActorSnapshotTagRequest, opts ...grpc.CallOption) (*ateapipb.ActorSnapshotTag, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) ListActors(ctx context.Context, in *ateapipb.ListActorsRequest, opts ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var actors []*ateapipb.Actor
	for _, a := range f.createdActors {
		if in.Atespace == "" || a.Metadata.Atespace == in.Atespace {
			actors = append(actors, a)
		}
	}
	return &ateapipb.ListActorsResponse{Actors: actors}, nil
}

func (f *fakeControlClient) ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
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
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func (f *fakeControlClient) DeleteAtespace(ctx context.Context, in *ateapipb.DeleteAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	return nil, status.Error(codes.Unimplemented, "unimplemented")
}

func TestResumeAndSuspendActorLifecycle(t *testing.T) {
	fakeClient := newFakeControlClient()

	mockRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
	}))
	defer mockRouter.Close()

	cfg := Config{
		AteClient:        fakeClient,
		AtenetRouterAddr: mockRouter.Listener.Addr().String(),
		Namespace:        "ate-antworker",
		Template:         "ate-antworker-actor-template",
	}
	cfg.Defaults()

	ctx := context.Background()
	actorRef := &ateapipb.ObjectRef{
		Atespace: "ate-antworker",
		Name:     "session-test-001",
	}

	// 1. Initial Registration: Actor does not exist yet -> should create atespace, create actor, and notify router
	if err := EnsureActorRegistered(ctx, fakeClient, cfg, actorRef, "work-test-001", "session-test-001"); err != nil {
		t.Fatalf("EnsureActorRegistered: %v", err)
	}

	key := "ate-antworker/session-test-001"
	if _, ok := fakeClient.createdAtespaces["ate-antworker"]; !ok {
		t.Errorf("expected Atespace 'ate-antworker' to be created")
	}
	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Errorf("expected Actor %q to be created", key)
	}

	// 2. Suspend Actor
	fakeClient.createdActors[key].Status = ateapipb.Actor_STATUS_RUNNING
	if err := SuspendActor(ctx, fakeClient, actorRef); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if fakeClient.suspendedActors[key] != 1 {
		t.Errorf("suspended count = %d, want 1", fakeClient.suspendedActors[key])
	}

	// 3. Subsequent Notification: Actor already exists -> should notify router directly without error
	if err := EnsureActorRegistered(ctx, fakeClient, cfg, actorRef, "work-test-002", "session-test-001"); err != nil {
		t.Fatalf("second EnsureActorRegistered: %v", err)
	}
}

func TestEnsureActorErrorPropagation(t *testing.T) {
	fakeClient := newFakeControlClient()
	fakeClient.createErr = status.Error(codes.Internal, "internal substrate error")

	cfg := Config{AteClient: fakeClient}
	cfg.Defaults()

	actorRef := &ateapipb.ObjectRef{
		Atespace: "ate-antworker",
		Name:     "session-test",
	}

	err := EnsureActorRegistered(context.Background(), fakeClient, cfg, actorRef, "work-test", "session-test")
	if err == nil || !errors.Is(err, fakeClient.createErr) {
		if status.Code(err) != codes.Internal {
			t.Errorf("expected internal error, got %v", err)
		}
	}
}

func TestDispatchWork(t *testing.T) {
	fakeClient := newFakeControlClient()

	mockRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
	}))
	defer mockRouter.Close()

	cfg := Config{
		AteClient:        fakeClient,
		AtenetRouterAddr: mockRouter.Listener.Addr().String(),
		Namespace:        "ate-antworker",
		Template:         "ate-antworker-actor-template",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessionID := "sesn_test_dispatch_01"
	aName := ActorName(sessionID)
	key := "ate-antworker/" + aName

	if err := DispatchWork(ctx, cfg, sessionID); err != nil {
		t.Fatalf("DispatchWork: %v", err)
	}

	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Fatalf("expected actor %q to be created", key)
	}
	if fakeClient.suspendedActors[key] != 1 {
		t.Errorf("expected actor %q to be suspended after the work item, count=%d", key, fakeClient.suspendedActors[key])
	}
}
