package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/rakyll/sessionflight"
	"github.com/valkey-io/valkey-go"
	"google.golang.org/grpc"
)

const (
	DefaultAtenetRouterAddr = "atenet-router.ate-system.svc.cluster.local:80"
	DefaultSuspendTimeout   = 30 * time.Second
	DefaultWorkTimeout      = 10 * time.Minute
)

// Config defines the configuration for Substrate actor dispatching.
type Config struct {
	AteapiAddr       string
	AtenetRouterAddr string
	Namespace        string
	Template         string
	Insecure         bool
	Logger           *slog.Logger
	EnvironmentID    string
	EnvironmentKey   string

	ValkeyClient valkey.Client

	// ATEClient can be provided to override the default gRPC client (e.g. in tests).
	ATEClient ateapipb.ControlClient
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
	}
	if c.AtenetRouterAddr == "" {
		c.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if c.EnvironmentID == "" {
		c.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	}
	if c.EnvironmentKey == "" {
		c.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Dispatcher coordinates actor dispatching with a single long-lived sessionflight.Runner.
type Dispatcher struct {
	cfg         Config
	sessionID   string
	ateClient   ateapipb.ControlClient
	conn        *grpc.ClientConn
	runner      *sessionflight.Runner
	cleanupDone chan struct{}
}

// NewDispatcher creates a new Dispatcher with a single sessionflight.Runner instance.
func NewDispatcher(ctx context.Context, cfg Config, sessionID string) (*Dispatcher, error) {
	cfg.Defaults()
	if cfg.ValkeyClient == nil {
		return nil, fmt.Errorf("valkey client is required")
	}

	var (
		ateClient ateapipb.ControlClient
		conn      *grpc.ClientConn
		err       error
	)

	if cfg.ATEClient != nil {
		ateClient = cfg.ATEClient
	} else {
		ateClient, conn, err = dialATE(cfg.AteapiAddr, cfg.Insecure)
		if err != nil {
			return nil, fmt.Errorf("dialing ateapi at %s: %w", cfg.AteapiAddr, err)
		}
	}

	d := &Dispatcher{
		cfg:         cfg,
		sessionID:   sessionID,
		ateClient:   ateClient,
		conn:        conn,
		cleanupDone: make(chan struct{}),
	}

	runner, err := sessionflight.New(ctx, sessionflight.Config{
		Client: cfg.ValkeyClient,
		Prefix: fmt.Sprintf("sessionflight:%s", sessionID),
		Logger: cfg.Logger,
		Handler: func(ctx context.Context, e *sessionflight.Event) error {
			return run(ctx, ateClient, cfg, e.SessionID)
		},
		Cleanup: func(ctx context.Context, e *sessionflight.Event, runErr error) {
			actorRef := &ateapipb.ObjectRef{
				Name:     actorName(e.SessionID),
				Atespace: cfg.Namespace,
			}
			if err := suspendActor(ctx, ateClient, cfg, actorRef, e.SessionID); err != nil {
				cfg.Logger.Error("Failed to suspend actor",
					slog.String("actor", actorRef.Name),
					slog.String("session_id", e.SessionID),
					slog.Any("error", err),
				)
			}
			close(d.cleanupDone)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initializing sessionflight runner: %w", err)
	}
	if _, err := runner.Enqueue(ctx, sessionID); err != nil {
		return nil, fmt.Errorf("enqueuing session %s: %w", sessionID, err)
	}
	d.runner = runner
	return d, nil
}

// Run runs the consumer loop of the sessionflight runner.
// Run cancels the runner and returns once Cleanup for the session has finished.
func (d *Dispatcher) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-runCtx.Done():
		case <-d.cleanupDone:
			cancel()
		}
	}()

	return d.runner.Run(runCtx)
}

// Close releases any dialed gRPC connections.
func (d *Dispatcher) Close() error {
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}
