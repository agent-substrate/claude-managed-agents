package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/alicebob/miniredis/v2"
)

func TestQueueCommandHelp(t *testing.T) {
	cmd := newQueueCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("queue --help failed: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"Queue acts as a hook target for 'ant beta:worker poll --on-work'",
		"--work-id",
		"--session-id",
		"--redis-addr",
		"--redis-password",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("queue help output missing %q:\n%s", expected, out)
		}
	}
}

func TestQueueStoresWorkItem(t *testing.T) {
	server := miniredis.RunT(t)

	cmd := newQueueCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--work-id", "work_test_1",
		"--session-id", "sesn_test_1",
		"--redis-addr", server.Addr(),
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("queue command: %v", err)
	}

	// The hook must only enqueue: no actor is touched, so this succeeds with no
	// Substrate cluster reachable at all.
	q := queue.NewRedis(queue.RedisConfig{Addr: server.Addr()})
	defer q.Close()

	lease, err := q.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease == nil {
		t.Fatal("no work item was queued")
	}
	if lease.Event.WorkID != "work_test_1" || lease.Event.SessionID != "sesn_test_1" {
		t.Errorf("queued %+v, want work_test_1/sesn_test_1", lease.Event)
	}
}

func TestQueueMissingWorkID(t *testing.T) {
	t.Setenv("ANTHROPIC_WORK_ID", "")
	t.Setenv("ANTHROPIC_SESSION_ID", "")
	cmd := newQueueCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	// It must fail on the missing work ID, before ever reaching Redis.
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when the work ID is missing, got nil")
	}
	if !strings.Contains(err.Error(), "work ID is required") {
		t.Errorf("error = %v, want it to name the missing work ID", err)
	}
}

func TestRedisConfigOpenSuccess(t *testing.T) {
	server := miniredis.RunT(t)

	rcfg := redisConfig{Addr: server.Addr()}
	q, err := rcfg.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer q.Close()

	if err := q.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestRedisConfigOpenCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rcfg := redisConfig{Addr: "127.0.0.1:1"}
	_, err := rcfg.Open(ctx)
	if err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
}
