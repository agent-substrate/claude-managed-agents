package worker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	DefaultNamespace      = "anthropic-worker"
	DefaultTemplate       = "anthropic-worker-template"
	DefaultWorkdir        = "/workspace"
	DefaultAteapiAddr     = "api.ate-system.svc.cluster.local:443"
	DefaultSuspendTimeout = 30 * time.Second
)

// Config defines the configuration for the Anthropic worker frontend server.
type Config struct {
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
		c.Workdir = DefaultWorkdir
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
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
	return nil
}

// Server is the frontend server that polls Anthropic, manages Substrate actors,
// executes session worker items, and suspends actors once finished.
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

	antClient := anthropic.NewClient(option.WithAPIKey(cfg.EnvironmentKey))

	return &Server{
		cfg:             cfg,
		ateClient:       ateClient,
		ateConn:         ateConn,
		anthropicClient: antClient,
		logger:          logger,
	}, nil
}

// Close releases any held connections.
func (s *Server) Close() error {
	if s.ateConn != nil {
		return s.ateConn.Close()
	}
	return nil
}

// Run starts the worker polling loop. It blocks until ctx is cancelled or a fatal error occurs.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("Starting Anthropic worker frontend server",
		slog.String("environment_id", s.cfg.EnvironmentID),
		slog.String("namespace", s.cfg.Namespace),
		slog.String("template", s.cfg.Template),
		slog.String("workdir", s.cfg.Workdir),
	)

	poller := environments.NewWorkPoller(ctx, s.anthropicClient, environments.WorkPollerOptions{
		EnvironmentID:  s.cfg.EnvironmentID,
		EnvironmentKey: s.cfg.EnvironmentKey,
		WorkerID:       s.cfg.WorkerID,
		Logger:         s.logger,
	})
	defer poller.Close()

	for poller.Next() {
		work := poller.Current()
		if work == nil {
			continue
		}

		if err := s.handleWork(ctx, work); err != nil {
			s.logger.Error("Error handling work item",
				slog.String("work_id", work.ID),
				slog.Any("error", err),
			)
		}
	}

	if err := poller.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("poller error: %w", err)
	}

	s.logger.Info("Anthropic worker frontend server stopped")
	return nil
}

// handleWork handles a single work item: resumes the session's actor, executes the worker,
// and ensures the actor is suspended once done.
func (s *Server) handleWork(ctx context.Context, work *anthropic.BetaSelfHostedWork) error {
	sessionID := work.Data.ID
	if sessionID == "" {
		return fmt.Errorf("work item %s has empty session ID", work.ID)
	}

	actorName := ActorName(sessionID)
	actorRef := &ateapipb.ObjectRef{
		Atespace: s.cfg.Namespace,
		Name:     actorName,
	}

	// 1. Resume or create the actor
	s.logger.Info("Resuming actor for session",
		slog.String("actor", actorName),
		slog.String("session_id", sessionID),
	)
	if err := s.resumeOrRegisterActor(ctx, actorRef); err != nil {
		return fmt.Errorf("resuming actor %s: %w", actorName, err)
	}
	s.logger.Info("Actor is active", slog.String("actor", actorName))

	// 2. Execute worker and ensure the actor is suspended when finished
	defer func() {
		suspendCtx, cancel := context.WithTimeout(context.Background(), DefaultSuspendTimeout)
		defer cancel()

		s.logger.Info("Suspending actor after session work",
			slog.String("actor", actorName),
			slog.String("session_id", sessionID),
		)
		if err := s.suspendActor(suspendCtx, actorRef); err != nil {
			s.logger.Error("Failed to suspend actor",
				slog.String("actor", actorName),
				slog.Any("error", err),
			)
		} else {
			s.logger.Info("Actor suspended successfully", slog.String("actor", actorName))
		}
	}()

	s.logger.Info("Actor is active; waiting for session turn to complete in actor sandbox",
		slog.String("actor", actorName),
		slog.String("session_id", sessionID),
		slog.String("work_id", work.ID),
	)

	if err := s.waitForSessionIdle(ctx, sessionID); err != nil {
		s.logger.Warn("Session wait ended with error",
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return err
	}

	s.logger.Info("Session turn finished successfully", slog.String("session_id", sessionID))
	return nil
}

// waitForSessionIdle polls Anthropic until the session reaches an idle or terminated state.
func (s *Server) waitForSessionIdle(ctx context.Context, sessionID string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Wait at least a moment for the session to start processing
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}

	for {
		sess, err := s.anthropicClient.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
		if err != nil {
			s.logger.Warn("Failed to get session status",
				slog.String("session_id", sessionID),
				slog.Any("error", err),
			)
		} else {
			s.logger.Debug("Session status check",
				slog.String("session_id", sessionID),
				slog.String("status", string(sess.Status)),
			)

			if sess.Status == anthropic.BetaManagedAgentsSessionStatusIdle ||
				sess.Status == anthropic.BetaManagedAgentsSessionStatusTerminated {
				s.logger.Info("Session reached terminal turn state",
					slog.String("session_id", sessionID),
					slog.String("status", string(sess.Status)),
				)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resumeOrRegisterActor ensures the atespace and actor exist, and resumes the actor.
func (s *Server) resumeOrRegisterActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	// First, try resuming directly
	_, err := s.ateClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actorRef})
	if err == nil {
		return nil
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
		return nil
	}

	return err
}

// suspendActor suspends the specified actor in Substrate.
func (s *Server) suspendActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	_, err := s.ateClient.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: actorRef})
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

// ActorName returns a deterministic RFC 1123 compliant actor name for a given Anthropic session ID.
// It combines a sanitized readable prefix with a SHA-256 hash suffix to guarantee uniqueness and RFC 1123 conformance.
func ActorName(sessionID string) string {
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
