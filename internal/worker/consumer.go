package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
)

// Consumer tuning. These are deliberately not configurable: they describe how
// the consumer paces itself against Redis and the actors, not a deployment
// choice. Scale with the number of consumer replicas instead.
const (
	// heartbeatInterval is how often a running work item renews its lease. It
	// must stay well under the queue's lease TTL.
	heartbeatInterval = time.Minute

	// maintenanceInterval is how often a consumer reclaims expired leases,
	// checked between claims. This is the retry path, so it also bounds how
	// quickly a failed work item is retried.
	maintenanceInterval = 30 * time.Second

	// claimBackoff is how long the consumer waits before retrying after the
	// queue itself failed, so a Redis outage does not become a hot loop.
	claimBackoff = time.Second
)

// ConsumerConfig configures a Consumer.
type ConsumerConfig struct {
	// Queue is the durable queue to consume from. Required.
	Queue *queue.RedisQueue

	// Dispatch configures how work reaches Substrate actors.
	Dispatch Config

	Logger *slog.Logger

	// dispatch overrides how a work item is run. Tests set it; production uses
	// DispatchWork.
	dispatch func(ctx context.Context, cfg Config, req DispatchRequest) error
}

func (c *ConsumerConfig) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.dispatch == nil {
		c.dispatch = DispatchWork
	}
	c.Dispatch.Defaults()
}

// Consumer drains the durable queue: it claims a polled work item, resumes the
// Substrate actor for its session, waits for the work to finish, suspends the
// actor, and only then acknowledges the work item.
//
// One consumer runs one work item at a time. Concurrency is the number of
// consumer replicas, which keeps the unit of scaling and the unit of failure the
// same thing: a consumer is either working on exactly one work item or on none.
//
// It never puts work back itself. A work item that fails, or a consumer that
// dies, simply stops renewing the lease, and queue maintenance retries the work
// item once it expires. That is the only retry path, which is why the consumer can be
// killed at any point without losing or duplicating work.
type Consumer struct {
	cfg ConsumerConfig
	log *slog.Logger
	q   *queue.RedisQueue
}

// NewConsumer creates a Consumer.
func NewConsumer(cfg ConsumerConfig) (*Consumer, error) {
	if cfg.Queue == nil {
		return nil, errors.New("consumer: queue is required")
	}
	cfg.defaults()
	return &Consumer{cfg: cfg, log: cfg.Logger, q: cfg.Queue}, nil
}

// Run consumes work items one at a time until ctx is canceled. A work item
// already underway finishes first, so shutdown is bounded by the work, not by a
// timer.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("Starting queue consumer",
		slog.String("namespace", c.cfg.Dispatch.Namespace),
	)

	// The work outlives shutdown: cancelling Run stops further claims, but the
	// work item in progress runs to completion rather than dying mid-tool-call.
	workCtx := context.WithoutCancel(ctx)

	var lastMaintenance time.Time

	for ctx.Err() == nil {
		// Reclaiming expired leases rides along on the claim loop rather than
		// running beside it: a claim blocks for only a few seconds, so an idle
		// consumer keeps to the schedule, and a busy one catches up the moment
		// its work item ends. Work it would have reclaimed while every consumer
		// was busy had nowhere to run anyway.
		if time.Since(lastMaintenance) >= maintenanceInterval {
			if err := c.q.Maintain(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("Queue maintenance failed", slog.Any("error", err))
			}
			lastMaintenance = time.Now()
		}

		lease, err := c.q.Claim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Error("Claiming work item from queue failed", slog.Any("error", err))
				sleep(ctx, claimBackoff)
			}
			continue
		}
		if lease == nil {
			continue
		}
		c.handle(workCtx, lease)
	}

	c.log.Info("Queue consumer stopped")
	return ctx.Err()
}

// handle runs one work item and acknowledges it. A failure is left to the lease
// to resolve: the work item stays in flight until maintenance retries it.
func (c *Consumer) handle(ctx context.Context, lease *queue.Lease) {
	ev := lease.Event
	log := c.log.With(
		slog.String("work_id", ev.WorkID),
		slog.String("session_id", ev.Session()),
		slog.Int("attempt", ev.Attempts+1),
	)

	stop := c.heartbeat(ctx, lease, log)
	defer stop()

	log.Info("Dispatching queued work item")
	start := time.Now()

	err := c.cfg.dispatch(ctx, c.cfg.Dispatch, DispatchRequest{
		WorkID:    ev.WorkID,
		SessionID: ev.Session(),
		// Wait for the work to finish and suspend the actor, freeing its
		// WorkerPool slot.
		Wait: true,
	})
	if err != nil {
		log.Error("Queued work item failed; leaving it to be reclaimed",
			slog.Any("error", err), slog.Duration("took", time.Since(start)))
		return
	}

	if err := c.q.Ack(ctx, lease); err != nil {
		// The work item already ran; a lost lease here means it may run again.
		log.Error("Acknowledging completed work item failed", slog.Any("error", err))
		return
	}
	log.Info("Queued work item completed", slog.Duration("took", time.Since(start)))
}

// heartbeat renews the lease while a work item is running. The returned
// function stops it; once renewals stop, the work item becomes reclaimable.
func (c *Consumer) heartbeat(ctx context.Context, lease *queue.Lease, log *slog.Logger) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			if err := c.q.Extend(ctx, lease); err != nil {
				log.Warn("Extending work item lease failed", slog.Any("error", err))
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// sleep waits for d unless ctx is canceled first.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// EnqueueWork records a polled work item in the durable queue. The poller hook
// calls it and returns immediately, leaving the actor resume to a consumer.
func EnqueueWork(ctx context.Context, q *queue.RedisQueue, req DispatchRequest, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if req.WorkID == "" {
		return fmt.Errorf("work_id is required")
	}

	ev := queue.Event{
		WorkID:    req.WorkID,
		SessionID: req.SessionID,
	}

	if err := q.Enqueue(ctx, ev); err != nil {
		return fmt.Errorf("enqueueing work item %s: %w", ev.WorkID, err)
	}

	logger.Info("Queued work item for dispatch",
		slog.String("work_id", ev.WorkID),
		slog.String("session_id", ev.Session()),
	)
	return nil
}
