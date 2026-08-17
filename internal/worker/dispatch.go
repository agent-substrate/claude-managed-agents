package worker

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/life"
)

const (
	DefaultWorkTimeout = 10 * time.Minute
)

// Config defines the configuration for dispatching work to actors.
type Config struct {
	EnvironmentID  string
	EnvironmentKey string
	Logger         *slog.Logger
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
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

// Dispatcher coordinates actor dispatching.
type Dispatcher struct {
	lifeMgr   *life.Manager
	cfg       Config
	sessionID string
	workID    string
}

// NewDispatcher creates a new Dispatcher for a session using the provided life.Manager.
func NewDispatcher(lifeMgr *life.Manager, cfg Config, sessionID, workID string) *Dispatcher {
	cfg.Defaults()
	return &Dispatcher{
		lifeMgr:   lifeMgr,
		cfg:       cfg,
		sessionID: sessionID,
		workID:    workID,
	}
}

// Run runs the session on the Substrate actor.
func (d *Dispatcher) Run(ctx context.Context) error {
	if err := d.lifeMgr.Resume(ctx, d.sessionID); err != nil {
		return err
	}
	payload := WorkRequest{
		SessionID:      d.sessionID,
		WorkID:         d.workID,
		EnvironmentID:  d.cfg.EnvironmentID,
		EnvironmentKey: d.cfg.EnvironmentKey,
	}
	return d.lifeMgr.Notify(ctx, d.sessionID, payload)
}
