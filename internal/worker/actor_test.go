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
	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
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
		name := actorName(id)
		if !rfc1123Pattern.MatchString(name) {
			t.Errorf("ActorName(%q) = %q does not match RFC 1123 regex", id, name)
		}
		if len(name) > 63 {
			t.Errorf("ActorName(%q) = %q length = %d > 63", id, name, len(name))
		}
		// Must be deterministic
		if second := actorName(id); second != name {
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

func TestRunActorRegistrationAndNotification(t *testing.T) {
	fakeClient := newFakeControlClient()

	mockRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "completed"})
	}))
	defer mockRouter.Close()

	cfg := Config{
		ATEClient:        fakeClient,
		AtenetRouterAddr: mockRouter.Listener.Addr().String(),
		Namespace:        "ate-antworker",
		Template:         "ate-antworker-actor-template",
	}
	cfg.Defaults()

	ctx := context.Background()
	sessionID := "session-test-001"
	aName := actorName(sessionID)

	// 1. Initial run: Actor does not exist yet -> should create atespace, create actor, and notify router
	if err := run(ctx, fakeClient, cfg, sessionID); err != nil {
		t.Fatalf("run: %v", err)
	}

	key := "ate-antworker/" + aName
	if _, ok := fakeClient.createdAtespaces["ate-antworker"]; !ok {
		t.Errorf("expected Atespace 'ate-antworker' to be created")
	}
	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Errorf("expected Actor %q to be created", key)
	}

	// 2. Subsequent run: Actor already exists -> should notify router directly without recreating
	if err := run(ctx, fakeClient, cfg, sessionID); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestEnsureActorErrorPropagation(t *testing.T) {
	fakeClient := newFakeControlClient()
	fakeClient.createErr = status.Error(codes.Internal, "internal substrate error")

	cfg := Config{ATEClient: fakeClient}
	cfg.Defaults()

	err := run(context.Background(), fakeClient, cfg, "session-test")
	if err == nil || !errors.Is(err, fakeClient.createErr) {
		if status.Code(err) != codes.Internal {
			t.Errorf("expected internal error, got %v", err)
		}
	}
}

func TestSuspendActor(t *testing.T) {
	fakeClient := newFakeControlClient()
	sessionID := "session-suspend-01"
	aName := actorName(sessionID)
	key := "ate-antworker/" + aName

	actorRef := &ateapipb.ObjectRef{
		Atespace: "ate-antworker",
		Name:     aName,
	}

	fakeClient.createdActors[key] = &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-antworker", Name: aName},
		Status:   ateapipb.Actor_STATUS_RUNNING,
	}

	cfg := Config{
		Namespace: "ate-antworker",
		ATEClient: fakeClient,
	}
	cfg.Defaults()

	if err := suspendActor(context.Background(), fakeClient, cfg, actorRef, sessionID); err != nil {
		t.Fatalf("suspendActor: %v", err)
	}

	if fakeClient.suspendedActors[key] != 1 {
		t.Errorf("suspended count = %d, want 1", fakeClient.suspendedActors[key])
	}
	if fakeClient.createdActors[key].Status != ateapipb.Actor_STATUS_SUSPENDED {
		t.Errorf("actor status = %s, want STATUS_SUSPENDED", fakeClient.createdActors[key].Status)
	}
}

func TestDispatcherLifecycle(t *testing.T) {
	fakeClient := newFakeControlClient()

	mockRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}))
	defer mockRouter.Close()

	mr := miniredis.RunT(t)
	vc, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{mr.Addr()},
		DisableCache: true,
	})
	if err != nil {
		t.Fatalf("valkey.NewClient: %v", err)
	}
	defer vc.Close()

	cfg := Config{
		ATEClient:        fakeClient,
		AtenetRouterAddr: mockRouter.Listener.Addr().String(),
		Namespace:        "ate-antworker",
		Template:         "ate-antworker-actor-template",
		ValkeyClient:     vc,
	}

	sessionID := "sesn_test_dispatcher_01"
	aName := actorName(sessionID)
	key := "ate-antworker/" + aName

	d, err := NewDispatcher(context.Background(), cfg, sessionID)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// d.Run should consume the session event (enqueued in NewDispatcher), execute, clean up (suspend actor), and return immediately
	if err := d.Run(ctx); err != nil {
		t.Fatalf("d.Run: %v", err)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()
	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Errorf("expected actor %q to be created", key)
	}
	if fakeClient.suspendedActors[key] < 1 {
		t.Errorf("expected actor %q to be suspended via Cleanup, got %d", key, fakeClient.suspendedActors[key])
	}
}
