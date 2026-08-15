package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/spf13/cobra"
)

// queueOptions holds the Redis flags shared by 'dispatch' and 'consume'.
type queueOptions struct {
	Addr     string
	Password string
}

// registerQueueFlags adds the Redis flags to a command.
func registerQueueFlags(cmd *cobra.Command, opts *queueOptions) {
	cmd.Flags().StringVar(&opts.Addr, "redis-addr", queue.DefaultAddr, "Redis host:port backing the durable work queue")
	cmd.Flags().StringVar(&opts.Password, "redis-password", "", "Redis password (or $REDIS_PASSWORD)")
}

// Open connects the queue and verifies Redis is reachable.
func (o queueOptions) Open(ctx context.Context) (*queue.RedisQueue, error) {
	password := o.Password
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}

	q := queue.NewRedis(queue.RedisConfig{Addr: o.Addr, Password: password})

	if err := q.Ping(ctx); err != nil {
		q.Close()
		return nil, fmt.Errorf("connecting to Redis at %s: %w", o.Addr, err)
	}
	return q, nil
}
