package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/alicebob/miniredis/v2"
)

func TestDispatchCommandHelp(t *testing.T) {
	cmd := newDispatchCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("dispatch --help failed: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"Dispatch acts as a hook target for 'ant beta:worker poll --on-work'",
		"--work-id",
		"--session-id",
		"--redis-addr",
		"--queue-prefix",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("dispatch help output missing %q:\n%s", expected, out)
		}
	}
}

func TestDispatchQueuesWorkItem(t *testing.T) {
	server := miniredis.RunT(t)

	cmd := newDispatchCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--work-id", "work_test_1",
		"--session-id", "sesn_test_1",
		"--redis-addr", server.Addr(),
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("dispatch with a queue: %v", err)
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

func TestRootCommandMissingWorkID(t *testing.T) {
	t.Setenv("ANTHROPIC_WORK_ID", "")
	t.Setenv("ANTHROPIC_SESSION_ID", "")
	cmd := newRootCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err == nil {
		t.Error("expected error when work ID is missing on root command, got nil")
	}
}
