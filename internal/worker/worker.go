package worker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	DefaultNamespace       = "anthropic-worker"
	DefaultTemplate        = "anthropic-worker-template"
	DefaultWorkdir         = "/workspace"
	DefaultAteapiAddr      = "api.ate-system.svc.cluster.local:443"
	DefaultSuspendTimeout  = 30 * time.Second
	DefaultResumeTimeout   = 60 * time.Second
	DefaultIdleTimeout     = 20 * time.Second
	DefaultPollInterval    = 5 * time.Second
	DefaultMaxActiveActors = 3
)

// Config defines the configuration for the Anthropic worker frontend server.
type Config struct {
	APIKey            string
	EnvironmentID     string
	EnvironmentKey    string
	AteapiAddr        string
	Namespace         string
	Template          string
	Workdir           string
	WorkerID          string
	Insecure          bool
	UnrestrictedPaths bool
	MaxFileBytes      int64
	IdleTimeout       time.Duration
	PollInterval      time.Duration
	MaxActiveActors   int
	Logger            *slog.Logger

	// AteClient can be provided to override the default gRPC client (e.g. in tests).
	AteClient ateapipb.ControlClient
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if c.Workdir == "" {
		if _, err := os.Stat(DefaultWorkdir); err == nil {
			c.Workdir = DefaultWorkdir
		} else {
			c.Workdir = "."
		}
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.MaxActiveActors <= 0 {
		c.MaxActiveActors = DefaultMaxActiveActors
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Validate ensures required configuration fields are set.
func (c *Config) Validate() error {
	c.Defaults()
	if c.EnvironmentID == "" {
		return errors.New("EnvironmentID is required")
	}
	if c.EnvironmentKey == "" {
		return errors.New("EnvironmentKey is required")
	}
	if c.APIKey == "" {
		return errors.New("APIKey is required")
	}
	return nil
}

// Server is the stateless frontend server that reconciles Anthropic work items
// with Substrate actors in the cluster.
type Server struct {
	cfg             Config
	ateClient       ateapipb.ControlClient
	ateConn         *grpc.ClientConn
	anthropicClient anthropic.Client
	logger          *slog.Logger
}

// NewServer initializes a new Server.
func NewServer(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	var (
		ateClient ateapipb.ControlClient
		ateConn   *grpc.ClientConn
		err       error
	)

	if cfg.AteClient != nil {
		ateClient = cfg.AteClient
	} else {
		logger.Info("Dialing Substrate ateapi", slog.String("addr", cfg.AteapiAddr), slog.Bool("insecure", cfg.Insecure))
		ateClient, ateConn, err = DialAteAPI(cfg.AteapiAddr, cfg.Insecure)
		if err != nil {
			return nil, fmt.Errorf("dialing ateapi: %w", err)
		}
	}

	antClient := anthropic.NewClient(option.WithAPIKey(cfg.APIKey))

	return &Server{
		cfg:             cfg,
		ateClient:       ateClient,
		ateConn:         ateConn,
		anthropicClient: antClient,
		logger:          logger,
	}, nil
}

func (s *Server) workRequestOptions() []option.RequestOption {
	var opts []option.RequestOption
	if s.cfg.EnvironmentKey != "" {
		opts = append(opts,
			option.WithHeaderDel("X-Api-Key"),
			option.WithAuthToken(s.cfg.EnvironmentKey),
		)
	}
	return opts
}

// Close releases any held connections.
func (s *Server) Close() error {
	if s.ateConn != nil {
		return s.ateConn.Close()
	}
	return nil
}

// Run starts the stateless work reconciliation loop.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("Starting Anthropic worker frontend server (stateless reconciler)",
		slog.String("environment_id", s.cfg.EnvironmentID),
		slog.String("namespace", s.cfg.Namespace),
		slog.String("template", s.cfg.Template),
		slog.String("workdir", s.cfg.Workdir),
		slog.Int("max_active_actors", s.cfg.MaxActiveActors),
		slog.Duration("idle_timeout", s.cfg.IdleTimeout),
		slog.Duration("poll_interval", s.cfg.PollInterval),
	)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("Anthropic worker frontend server stopped")
			return nil
		case <-ticker.C:
			if err := s.Reconcile(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				s.logger.Warn("Reconciliation error", slog.Any("error", err))
			}
		}
	}
}

// Reconcile synchronizes Anthropic work queue items with Substrate running actors.
func (s *Server) Reconcile(ctx context.Context) error {
	// 1. Fetch current actor state from Substrate
	actorsResp, err := s.ateClient.ListActors(ctx, &ateapipb.ListActorsRequest{
		Atespace: s.cfg.Namespace,
	})
	if err != nil {
		return fmt.Errorf("listing substrate actors: %w", err)
	}

	clusterActors := make(map[string]*ateapipb.Actor)
	activeActorCount := 0
	for _, actor := range actorsResp.Actors {
		name := actor.Metadata.Name
		clusterActors[name] = actor
		if actor.Status != ateapipb.Actor_STATUS_SUSPENDED {
			activeActorCount++
		}
	}

	// 2. Fetch current work items from Anthropic
	page, err := s.anthropicClient.Beta.Environments.Work.List(
		ctx,
		s.cfg.EnvironmentID,
		anthropic.BetaEnvironmentWorkListParams{
			Limit: anthropic.Int(30),
		},
		s.workRequestOptions()...,
	)
	if err != nil {
		return fmt.Errorf("listing anthropic work: %w", err)
	}

	activeWorkBySession := make(map[string]*anthropic.BetaSelfHostedWork)
	for i := range page.Data {
		work := &page.Data[i]
		if work.Data.ID == "" {
			continue
		}
		if existing, exists := activeWorkBySession[work.Data.ID]; !exists || isNonTerminated(work.State) {
			if !exists || !isNonTerminated(existing.State) {
				activeWorkBySession[work.Data.ID] = work
			}
		}
	}

	now := time.Now()

	// 3. Suspend any running Substrate actors that are idle (>IdleTimeout without heartbeat) or terminated
	for _, actor := range clusterActors {
		if actor.Status != ateapipb.Actor_STATUS_RUNNING {
			continue
		}

		aName := actor.Metadata.Name
		actorRef := &ateapipb.ObjectRef{
			Atespace: s.cfg.Namespace,
			Name:     aName,
		}

		var matchingWork *anthropic.BetaSelfHostedWork
		for sessionID, work := range activeWorkBySession {
			if aName == actorName(sessionID) {
				matchingWork = work
				break
			}
		}

		shouldSuspend := false
		reason := ""

		if matchingWork == nil {
			shouldSuspend = true
			reason = "no work item found in Anthropic"
		} else if !isNonTerminated(matchingWork.State) {
			shouldSuspend = true
			reason = fmt.Sprintf("work state is %s", matchingWork.State)
		} else {
			lastActivity := parseWorkActivityTime(matchingWork)
			idleDuration := now.Sub(lastActivity)
			if idleDuration >= s.cfg.IdleTimeout {
				shouldSuspend = true
				reason = fmt.Sprintf("idle for %s (threshold %s)", idleDuration.Round(time.Millisecond), s.cfg.IdleTimeout)
			}
		}

		if shouldSuspend {
			s.logger.Info("Suspending idle/terminated actor",
				slog.String("actor", aName),
				slog.String("reason", reason),
			)
			if err := s.suspendActor(ctx, actorRef); err != nil {
				s.logger.Error("Failed to suspend actor",
					slog.String("actor", aName),
					slog.Any("error", err),
				)
			} else {
				s.logger.Info("Actor suspended successfully", slog.String("actor", aName))
				activeActorCount--
			}
		}
	}

	// 4. Resume or register actors for queued/active work items (up to MaxActiveActors limit)
	for sessionID, work := range activeWorkBySession {
		if !isNonTerminated(work.State) {
			continue
		}

		expectedName := actorName(sessionID)
		existingActor, existsInCluster := clusterActors[expectedName]

		// If actor is already running in Substrate, skip
		if existsInCluster && existingActor.Status != ateapipb.Actor_STATUS_SUSPENDED {
			continue
		}

		// Don't wake up anything that has been idle for more than IdleTimeout
		lastActivity := parseWorkActivityTime(work)
		if idleDuration := now.Sub(lastActivity); idleDuration >= s.cfg.IdleTimeout {
			s.logger.Debug("Skipping actor wake for idle work item",
				slog.String("session_id", sessionID),
				slog.String("work_id", work.ID),
				slog.Duration("idle_duration", idleDuration.Round(time.Millisecond)),
				slog.Duration("idle_timeout", s.cfg.IdleTimeout),
			)
			continue
		}

		// Enforce capacity limit
		if activeActorCount >= s.cfg.MaxActiveActors {
			s.logger.Debug("Worker pool at capacity, skipping actor resume",
				slog.String("session_id", sessionID),
				slog.Int("active_actors", activeActorCount),
				slog.Int("max_active_actors", s.cfg.MaxActiveActors),
			)
			break
		}

		actorRef := &ateapipb.ObjectRef{
			Atespace: s.cfg.Namespace,
			Name:     expectedName,
		}

		s.logger.Info("Waking actor for work item",
			slog.String("actor", expectedName),
			slog.String("session_id", sessionID),
			slog.String("work_id", work.ID),
			slog.String("state", string(work.State)),
		)

		if err := s.resumeOrRegisterActor(ctx, actorRef); err != nil {
			s.logger.Warn("Failed to resume actor for session",
				slog.String("actor", expectedName),
				slog.String("session_id", sessionID),
				slog.Any("error", err),
			)
			continue
		}

		s.logger.Info("Actor is active in Substrate", slog.String("actor", expectedName))
		activeActorCount++
	}

	return nil
}

// isNonTerminated returns true if the work item is queued, starting, or active.
func isNonTerminated(state anthropic.BetaSelfHostedWorkState) bool {
	return state == anthropic.BetaSelfHostedWorkStateActive ||
		state == anthropic.BetaSelfHostedWorkStateQueued ||
		state == anthropic.BetaSelfHostedWorkStateStarting
}

// parseWorkActivityTime extracts the latest activity timestamp from a work item.
// It prioritizes LatestHeartbeatAt, then StartedAt, then CreatedAt, falling back to time.Now().
func parseWorkActivityTime(work *anthropic.BetaSelfHostedWork) time.Time {
	if work.LatestHeartbeatAt != "" {
		if t, err := time.Parse(time.RFC3339, work.LatestHeartbeatAt); err == nil {
			return t
		}
	}
	if work.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, work.StartedAt); err == nil {
			return t
		}
	}
	if work.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, work.CreatedAt); err == nil {
			return t
		}
	}
	return time.Now()
}

// resumeOrRegisterActor ensures the atespace and actor exist, resumes the actor,
// and waits until the actor reaches STATUS_RUNNING.
func (s *Server) resumeOrRegisterActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	_, err := s.ateClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actorRef})
	if err == nil {
		return s.waitForActorResumed(ctx, actorRef)
	}

	// If actor doesn't exist yet, create it and retry resume
	if status.Code(err) == codes.NotFound {
		s.logger.Info("Actor not found, registering new actor",
			slog.String("actor", actorRef.Name),
			slog.String("atespace", actorRef.Atespace),
		)

		if err := s.ensureAtespace(ctx, actorRef.Atespace); err != nil {
			return fmt.Errorf("ensuring atespace %q: %w", actorRef.Atespace, err)
		}

		_, err = s.ateClient.CreateActor(ctx, &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{
					Atespace: actorRef.Atespace,
					Name:     actorRef.Name,
				},
				ActorTemplateNamespace: s.cfg.Namespace,
				ActorTemplateName:      s.cfg.Template,
			},
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("creating actor %s: %w", actorRef.Name, err)
		}

		_, err = s.ateClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actorRef})
		if err != nil {
			return fmt.Errorf("resuming actor %s after creation: %w", actorRef.Name, err)
		}
		return s.waitForActorResumed(ctx, actorRef)
	}

	return err
}

// waitForActorResumed polls Substrate until the actor reaches STATUS_RUNNING.
func (s *Server) waitForActorResumed(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	waitCtx, cancel := context.WithTimeout(ctx, DefaultResumeTimeout)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		actor, err := s.ateClient.GetActor(waitCtx, &ateapipb.GetActorRequest{Actor: actorRef})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				select {
				case <-waitCtx.Done():
					return fmt.Errorf("timed out waiting for actor %s: %w", actorRef.Name, waitCtx.Err())
				case <-ticker.C:
					continue
				}
			}
			return fmt.Errorf("getting actor status for %s: %w", actorRef.Name, err)
		}

		switch actor.Status {
		case ateapipb.Actor_STATUS_RUNNING:
			return nil
		case ateapipb.Actor_STATUS_CRASHED:
			return fmt.Errorf("actor %s crashed while resuming", actorRef.Name)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for actor %s to reach STATUS_RUNNING (current status: %s): %w", actorRef.Name, actor.Status, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// suspendActor suspends the specified actor in Substrate.
func (s *Server) suspendActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	suspendCtx, cancel := context.WithTimeout(ctx, DefaultSuspendTimeout)
	defer cancel()

	_, err := s.ateClient.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{Actor: actorRef})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	return nil
}

// ensureAtespace creates the Atespace if it doesn't already exist.
func (s *Server) ensureAtespace(ctx context.Context, atespace string) error {
	_, err := s.ateClient.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{
			Metadata: &ateapipb.ResourceMetadata{
				Name: atespace,
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	return nil
}

// actorName returns a deterministic RFC 1123 compliant actor name for a given Anthropic session ID.
// It combines a sanitized readable prefix with a SHA-256 hash suffix to guarantee uniqueness and RFC 1123 conformance.
func actorName(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	hashSuffix := fmt.Sprintf("%x", h[:8]) // 16 hex chars

	// Clean the session ID into lowercase alphanumeric + hyphens
	var b strings.Builder
	for _, r := range strings.ToLower(sessionID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	prefix := strings.Trim(b.String(), "-")
	if len(prefix) > 40 {
		prefix = prefix[:40]
		prefix = strings.TrimRight(prefix, "-")
	}

	if prefix == "" {
		return "session-" + hashSuffix
	}
	return fmt.Sprintf("%s-%s", prefix, hashSuffix)
}

// DialAteAPI dials the Substrate ateapi gRPC service.
func DialAteAPI(endpoint string, insecureConn bool) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if insecureConn {
		creds = insecure.NewCredentials()
	} else {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, err
	}

	return ateapipb.NewControlClient(conn), conn, nil
}
