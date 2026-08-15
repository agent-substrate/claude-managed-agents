package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/spf13/cobra"
)

// queueOptions holds the Redis queue flags shared by 'dispatch' and 'consume'.
type queueOptions struct {
	Addr        string
	Password    string
	DB          int
	Prefix      string
	MaxAttempts int
	DedupTTL    time.Duration
}

// registerQueueFlags adds the Redis queue flags to a command.
func registerQueueFlags(cmd *cobra.Command, opts *queueOptions) {
	cmd.Flags().StringVar(&opts.Addr, "redis-addr", queue.DefaultAddr, "Redis host:port backing the durable work queue")
	cmd.Flags().StringVar(&opts.Password, "redis-password", "", "Redis password (or $REDIS_PASSWORD)")
	cmd.Flags().IntVar(&opts.DB, "redis-db", 0, "Redis logical database")
	cmd.Flags().StringVar(&opts.Prefix, "queue-prefix", queue.DefaultPrefix, "prefix for the queue's Redis keys")
	cmd.Flags().IntVar(&opts.MaxAttempts, "max-attempts", queue.DefaultMaxAttempts, "attempts before a work item is moved to the dead-letter list")
	cmd.Flags().DurationVar(&opts.DedupTTL, "dedup-ttl", 0, "drop work items whose work ID was enqueued within this window (0 disables)")
}

// Open connects the queue and verifies Redis is reachable.
func (o queueOptions) Open(ctx context.Context) (*queue.RedisQueue, error) {
	password := o.Password
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}

	q := queue.NewRedis(queue.RedisConfig{
		Addr:        o.Addr,
		Password:    password,
		DB:          o.DB,
		Prefix:      o.Prefix,
		MaxAttempts: o.MaxAttempts,
		DedupTTL:    o.DedupTTL,
	})

	if err := q.Ping(ctx); err != nil {
		q.Close()
		return nil, fmt.Errorf("connecting to Redis at %s: %w", o.Addr, err)
	}
	return q, nil
}
