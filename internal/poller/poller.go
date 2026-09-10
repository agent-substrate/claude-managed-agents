package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	defaultPollInterval = 1 * time.Second

	// DefaultHandoffTTLSeconds is the lease the poller establishes before
	// handing a work item to the controller: the time the controller has to
	// restore the session's actor and start the worker before the lease
	// lapses and the item is requeued.
	DefaultHandoffTTLSeconds int64 = 120

	// pollReclaimOlderThanMs is the server default for reclaim_older_than_ms.
	// The poller acks every item right after the poll, so the window only
	// needs to cover the poll-to-ack round trip; it is stated explicitly so
	// that dependence is visible.
	pollReclaimOlderThanMs int64 = 5000

	// noHeartbeat is the expected_last_heartbeat value that claims a lease
	// nobody has heartbeated yet.
	noHeartbeat = "NO_HEARTBEAT"
)

// Config configures the Poller client and lifecycle.
type Config struct {
	Client         *anthropic.Client
	BaseURL        string
	EnvironmentID  string
	EnvironmentKey string
	PollInterval   time.Duration
	BlockMs        int64
	// HandoffTTLSeconds is the desired_ttl_seconds of the first heartbeat the
	// poller sends after acking an item; see DefaultHandoffTTLSeconds.
	HandoffTTLSeconds int64
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
	if c.BaseURL == "" {
		c.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = defaultPollInterval
	}
	if c.HandoffTTLSeconds <= 0 {
		c.HandoffTTLSeconds = DefaultHandoffTTLSeconds
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Poller polls Anthropic for self-hosted environment work items
// and completes/stops items using the Anthropic Go SDK.
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
		if cfg.EnvironmentKey == "" {
			return nil, errors.New("environment_key is required (set ANTHROPIC_ENVIRONMENT_KEY)")
		}
		opts := []option.RequestOption{
			option.WithoutEnvironmentDefaults(),
			option.WithAuthToken(cfg.EnvironmentKey),
		}
		if cfg.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(cfg.BaseURL))
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
	params := anthropic.BetaEnvironmentWorkPollParams{
		ReclaimOlderThanMs: param.NewOpt(pollReclaimOlderThanMs),
	}
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

// Lease acknowledges a polled work item and sends its first heartbeat, so the
// item is no longer reclaimable and holds a lease of HandoffTTLSeconds. It
// returns the last_heartbeat value the heartbeat response carried: the next
// heartbeat, from whichever process continues the lease, must quote it as
// expected_last_heartbeat.
func (p *Poller) Lease(ctx context.Context, item *anthropic.BetaSelfHostedWork) (string, error) {
	if item == nil || item.ID == "" {
		return "", errors.New("work_id is required")
	}
	if _, err := p.client.Beta.Environments.Work.Ack(ctx, item.ID, anthropic.BetaEnvironmentWorkAckParams{
		EnvironmentID: p.cfg.EnvironmentID,
	}); err != nil {
		return "", fmt.Errorf("acking work %s: %w", item.ID, err)
	}
	res, err := p.client.Beta.Environments.Work.Heartbeat(ctx, item.ID, anthropic.BetaEnvironmentWorkHeartbeatParams{
		EnvironmentID:         p.cfg.EnvironmentID,
		DesiredTTLSeconds:     param.NewOpt(p.cfg.HandoffTTLSeconds),
		ExpectedLastHeartbeat: param.NewOpt(noHeartbeat),
	})
	if err == nil && (!res.LeaseExtended || res.LastHeartbeat == "") {
		err = fmt.Errorf("lease not established (state %q)", res.State)
	}
	if err != nil {
		// An acked item that is never heartbeated is not requeued by the
		// server, so release it rather than leave the session waiting on it.
		p.release(item.ID)
		return "", fmt.Errorf("first heartbeat for work %s: %w", item.ID, err)
	}
	return res.LastHeartbeat, nil
}

// release best-effort force-stops a work item the poller acked but cannot hand
// over. It uses its own context so a cancelled poll loop does not skip it.
func (p *Poller) release(workID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, workID, true); err != nil {
		p.cfg.Logger.Warn("Releasing un-leased work item failed",
			slog.String("work_id", workID),
			slog.Any("error", err),
		)
	}
}

// Claim is a work item the poller has polled, acked, and heartbeated once.
type Claim struct {
	// Work is the polled item. Its Secret field carries the per-delivery
	// secret from the poll response.
	Work *anthropic.BetaSelfHostedWork
	// LastHeartbeat is the token the process that continues the lease must
	// send as its first expected_last_heartbeat.
	LastHeartbeat string
}

// Handler is invoked for each claimed work item.
type Handler func(ctx context.Context, claim Claim) error

// Poll runs a continuous polling loop using the Anthropic SDK.
// For each claimed item, it invokes handler and marks completion.
func (p *Poller) Poll(ctx context.Context, handler Handler) error {
	p.cfg.Logger.Info("Starting Anthropic SDK work polling loop",
		slog.String("environment_id", p.cfg.EnvironmentID),
		slog.Duration("poll_interval", p.cfg.PollInterval),
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

		// Ack and lease before enqueueing. An item whose ack failed is left
		// alone and is requeued by the server after the reclaim window.
		lastHeartbeat, err := p.Lease(ctx, item)
		if err != nil {
			p.cfg.Logger.Warn("Leasing work item failed, skipping",
				slog.String("work_id", item.ID),
				slog.String("session_id", item.Data.ID),
				slog.Any("error", err),
			)
			continue
		}

		p.cfg.Logger.Info("Claimed work item via Anthropic SDK",
			slog.String("work_id", item.ID),
			slog.String("session_id", item.Data.ID),
			slog.Int64("handoff_ttl_seconds", p.cfg.HandoffTTLSeconds),
		)

		if err := handler(ctx, Claim{Work: item, LastHeartbeat: lastHeartbeat}); err != nil {
			p.cfg.Logger.Error("Work item processing failed",
				slog.String("work_id", item.ID),
				slog.Any("error", err),
			)
			continue
		}
	}
}
