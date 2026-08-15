package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/spf13/cobra"
)

// redisOptions holds the Redis flags shared by 'queue' and 'dispatch'.
type redisOptions struct {
	Addr     string
	Password string
}

// registerRedisFlags adds the Redis flags to a command.
func registerRedisFlags(cmd *cobra.Command, opts *redisOptions) {
	cmd.Flags().StringVar(&opts.Addr, "redis-addr", queue.DefaultAddr, "Redis host:port backing the durable work queue")
	cmd.Flags().StringVar(&opts.Password, "redis-password", "", "Redis password (or $REDIS_PASSWORD)")
}

// Open connects the queue and verifies Redis is reachable.
func (o redisOptions) Open(ctx context.Context) (*queue.RedisQueue, error) {
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
