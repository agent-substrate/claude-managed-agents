package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/spf13/cobra"
)

// redisConfig holds the Redis flags shared by 'queue' and 'dispatch'.
type redisConfig struct {
	Addr     string
	Password string
}

// registerRedisFlags adds the Redis flags to a command.
func registerRedisFlags(cmd *cobra.Command, cfg *redisConfig) {
	cmd.Flags().StringVar(&cfg.Addr, "redis-addr", queue.DefaultAddr, "Redis host:port backing the durable work queue")
	cmd.Flags().StringVar(&cfg.Password, "redis-password", "", "Redis password (or $REDIS_PASSWORD)")
}

// Open connects the queue and verifies Redis is reachable, retrying for up to 30 seconds
// to tolerate startup ordering when pods boot concurrently.
func (c redisConfig) Open(ctx context.Context) (*queue.RedisQueue, error) {
	addr := c.Addr
	if addr == "" {
		addr = queue.DefaultAddr
	}
	password := c.Password
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}

	slog.Info("Connecting to Redis", slog.String("addr", addr))

	q := queue.NewRedis(queue.RedisConfig{Addr: addr, Password: password})

	var lastErr error
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := q.Ping(ctx); err == nil {
			slog.Info("Connected to Redis", slog.String("addr", addr))
			return q, nil
		} else {
			lastErr = err
			slog.Debug("Waiting for Redis to become reachable", slog.String("addr", addr), slog.Any("error", err))
		}

		select {
		case <-ctx.Done():
			q.Close()
			return nil, ctx.Err()
		case <-timeout:
			q.Close()
			return nil, fmt.Errorf("connecting to Redis at %s (timed out after 30s): %w", addr, lastErr)
		case <-ticker.C:
		}
	}
}
