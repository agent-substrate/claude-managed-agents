package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

var (
	// ErrNoWork is returned when a poll attempt yields no available work item.
	ErrNoWork = errors.New("no work available")
)

const (
	DefaultPollInterval      = 1 * time.Second
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultHeartbeatTTL      = 300 // 5 minutes
)

// Config configures the Poller client and lifecycle.
type Config struct {
	Client            *anthropic.Client
	APIKey            string
	EnvironmentID     string
	EnvironmentKey    string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTTL      int64
	BlockMs           int64
	HTTPClient        *http.Client
	Logger            *slog.Logger
}

// Defaults populates default configuration values for unset fields.
func (c *Config) Defaults() {
	if c.EnvironmentID == "" {
		c.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	}
	if c.EnvironmentKey == "" {
		c.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.HeartbeatTTL <= 0 {
		c.HeartbeatTTL = DefaultHeartbeatTTL
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Poller polls Anthropic for self-hosted environment work items,
// renews leases with periodic heartbeats, and completes/stops items using the Anthropic Go SDK.
type Poller struct {
	client *anthropic.Client
	cfg    Config
}

// New creates a new Poller using the Anthropic Go SDK.
func New(cfg Config) (*Poller, error) {
	cfg.Defaults()
	if cfg.EnvironmentID == "" {
		return nil, errors.New("environment_id is required (set ANTHROPIC_ENVIRONMENT_ID)")
	}

	client := cfg.Client
	if client == nil {
		opts := []option.RequestOption{
			option.WithoutEnvironmentDefaults(),
		}
		if cfg.EnvironmentKey != "" {
			opts = append(opts, option.WithAuthToken(cfg.EnvironmentKey))
		}
		if cfg.HTTPClient != nil {
			opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
		}
		c := anthropic.NewClient(opts...)
		client = &c
	}
	return &Poller{
		client: client,
		cfg:    cfg,
	}, nil
}

// PollOnce long-polls and claims the next available work item for the configured environment using the Anthropic SDK.
// If no work is available, it returns ErrNoWork.
func (p *Poller) PollOnce(ctx context.Context) (*anthropic.BetaSelfHostedWork, error) {
	params := anthropic.BetaEnvironmentWorkPollParams{}
	if p.cfg.BlockMs > 0 {
		params.BlockMs = param.NewOpt(p.cfg.BlockMs)
	}

	res, err := p.client.Beta.Environments.Work.Poll(ctx, p.cfg.EnvironmentID, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			if apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusNoContent || apiErr.StatusCode == http.StatusRequestTimeout {
				return nil, ErrNoWork
			}
		}
		return nil, fmt.Errorf("polling work for environment %s: %w", p.cfg.EnvironmentID, err)
	}

	if res == nil || res.ID == "" {
		return nil, ErrNoWork
	}

	return res, nil
}

// Heartbeat records a heartbeat with Anthropic to extend the work lease.
func (p *Poller) Heartbeat(ctx context.Context, workID string, expectedLastHeartbeat string) (*anthropic.BetaSelfHostedWorkHeartbeatResponse, error) {
	if workID == "" {
		return nil, errors.New("work_id is required")
	}

	params := anthropic.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID:     p.cfg.EnvironmentID,
		DesiredTTLSeconds: param.NewOpt(p.cfg.HeartbeatTTL),
	}
	if expectedLastHeartbeat != "" {
		params.ExpectedLastHeartbeat = param.NewOpt(expectedLastHeartbeat)
	}

	return p.client.Beta.Environments.Work.Heartbeat(ctx, workID, params)
}

// Complete marks a claimed work item as completed/stopped using the Anthropic SDK.
func (p *Poller) Complete(ctx context.Context, workID string, force bool) (*anthropic.BetaSelfHostedWork, error) {
	if workID == "" {
		return nil, errors.New("work_id is required")
	}

	return p.client.Beta.Environments.Work.Stop(ctx, workID, anthropic.BetaEnvironmentWorkStopParams{
		EnvironmentID: p.cfg.EnvironmentID,
		BetaSelfHostedWorkStopRequest: anthropic.BetaSelfHostedWorkStopRequestParam{
			Force: param.NewOpt(force),
		},
	})
}

// StartHeartbeat begins a background goroutine that periodically calls Heartbeat
// for the given workID until the returned stop func is invoked or ctx is canceled.
func (p *Poller) StartHeartbeat(ctx context.Context, workID string) (stop func(), errs <-chan error) {
	errChan := make(chan error, 16)
	stopCtx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(errChan)

		ticker := time.NewTicker(p.cfg.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCtx.Done():
				return
			case <-ticker.C:
				_, err := p.Heartbeat(stopCtx, workID, "")
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						p.cfg.Logger.Warn("Work heartbeat renewal failed",
							slog.String("work_id", workID),
							slog.Any("error", err),
						)
						select {
						case errChan <- err:
						default:
						}
					}
					continue
				}
			}
		}
	}()

	stopFunc := func() {
		cancel()
		wg.Wait()
	}

	return stopFunc, errChan
}

// Handler is invoked for each claimed work item.
type Handler func(ctx context.Context, item *anthropic.BetaSelfHostedWork) error

// Poll runs a continuous polling loop using the Anthropic SDK.
// For each claimed item, it maintains background heartbeats, invokes handler, and marks completion.
func (p *Poller) Poll(ctx context.Context, handler Handler) error {
	p.cfg.Logger.Info("Starting Anthropic SDK work polling loop",
		slog.String("environment_id", p.cfg.EnvironmentID),
		slog.Duration("poll_interval", p.cfg.PollInterval),
		slog.Duration("heartbeat_interval", p.cfg.HeartbeatInterval),
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		item, err := p.PollOnce(ctx)
		if err != nil {
			if errors.Is(err, ErrNoWork) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(p.cfg.PollInterval):
					continue
				}
			}

			p.cfg.Logger.Warn("Polling work failed, backing off",
				slog.Any("error", err),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.cfg.PollInterval):
				continue
			}
		}

		p.cfg.Logger.Info("Claimed work item via Anthropic SDK",
			slog.String("work_id", item.ID),
			slog.String("session_id", item.Data.ID),
		)

		stopHeartbeat, _ := p.StartHeartbeat(ctx, item.ID)

		handlerErr := handler(ctx, item)
		stopHeartbeat()

		if handlerErr != nil {
			p.cfg.Logger.Error("Work item processing failed",
				slog.String("work_id", item.ID),
				slog.Any("error", handlerErr),
			)
			continue
		}
	}
}
