package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"regexp"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	anthropic "github.com/anthropics/anthropic-sdk-go"
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

func TestConfigValidate(t *testing.T) {
	cfg := Config{}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty Config, got nil")
	}

	cfg.EnvironmentID = "env_123"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing EnvironmentKey, got nil")
	}

	cfg.EnvironmentKey = "key_123"
	if err := cfg.Validate(); err != nil {
		t.Errorf("unexpected error for valid Config: %v", err)
	}

	if cfg.Namespace != DefaultNamespace {
		t.Errorf("namespace = %q, want %q", cfg.Namespace, DefaultNamespace)
	}
	if cfg.Template != DefaultTemplate {
		t.Errorf("template = %q, want %q", cfg.Template, DefaultTemplate)
	}
	if cfg.Workdir != DefaultWorkdir {
		t.Errorf("workdir = %q, want %q", cfg.Workdir, DefaultWorkdir)
	}
	if cfg.AteapiAddr != DefaultAteapiAddr {
		t.Errorf("ateapiAddr = %q, want %q", cfg.AteapiAddr, DefaultAteapiAddr)
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
	return nil, status.Error(codes.Unimplemented, "unimplemented")
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

	cfg := Config{
		EnvironmentID:  "env_test_123",
		EnvironmentKey: "key_test_123",
		AteClient:      fakeClient,
		Namespace:      "anthropic-worker",
		Template:       "anthropic-worker-template",
	}

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	ctx := context.Background()
	actorRef := &ateapipb.ObjectRef{
		Atespace: "anthropic-worker",
		Name:     "session-test-001",
	}

	// 1. Initial Resume: Actor does not exist yet -> should create atespace, create actor, and resume
	if err := server.resumeOrRegisterActor(ctx, actorRef); err != nil {
		t.Fatalf("resumeOrRegisterActor: %v", err)
	}

	key := "anthropic-worker/session-test-001"
	if _, ok := fakeClient.createdAtespaces["anthropic-worker"]; !ok {
		t.Errorf("expected Atespace 'anthropic-worker' to be created")
	}
	if _, ok := fakeClient.createdActors[key]; !ok {
		t.Errorf("expected Actor %q to be created", key)
	}
	if fakeClient.resumedActors[key] != 1 {
		t.Errorf("resumed count = %d, want 1", fakeClient.resumedActors[key])
	}

	// 2. Suspend Actor
	if err := server.suspendActor(ctx, actorRef); err != nil {
		t.Fatalf("suspendActor: %v", err)
	}
	if fakeClient.suspendedActors[key] != 1 {
		t.Errorf("suspended count = %d, want 1", fakeClient.suspendedActors[key])
	}

	// 3. Subsequent Resume: Actor already exists -> should resume directly without error
	if err := server.resumeOrRegisterActor(ctx, actorRef); err != nil {
		t.Fatalf("second resumeOrRegisterActor: %v", err)
	}
	if fakeClient.resumedActors[key] != 2 {
		t.Errorf("resumed count = %d, want 2", fakeClient.resumedActors[key])
	}
}

func TestHandleWorkEmptySessionID(t *testing.T) {
	fakeClient := newFakeControlClient()
	cfg := Config{
		EnvironmentID:  "env_test_123",
		EnvironmentKey: "key_test_123",
		AteClient:      fakeClient,
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	work := &anthropic.BetaSelfHostedWork{
		ID:            "work_123",
		EnvironmentID: "env_test_123",
		Data:          anthropic.BetaSessionWorkData{}, // empty ID
	}

	err = server.handleWork(context.Background(), work)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
}

func TestResumeActorErrorPropagation(t *testing.T) {
	fakeClient := newFakeControlClient()
	fakeClient.resumeErr = status.Error(codes.Internal, "internal substrate error")

	cfg := Config{
		EnvironmentID:  "env_test_123",
		EnvironmentKey: "key_test_123",
		AteClient:      fakeClient,
	}
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close()

	actorRef := &ateapipb.ObjectRef{
		Atespace: "anthropic-worker",
		Name:     "session-test",
	}

	err = server.resumeOrRegisterActor(context.Background(), actorRef)
	if err == nil || !errors.Is(err, fakeClient.resumeErr) {
		if status.Code(err) != codes.Internal {
			t.Errorf("expected internal error, got %v", err)
		}
	}
}
