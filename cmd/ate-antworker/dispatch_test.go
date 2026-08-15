package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
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
		"Dispatch drains the durable Redis queue",
		"--redis-addr",
		"--ateapi-addr",
		"--namespace",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("dispatch help output missing %q:\n%s", expected, out)
		}
	}
}

func TestDispatchCommandFailsWithoutRedis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := newDispatchCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// Port 1 has nothing listening, so connecting fails fast.
	cmd.SetArgs([]string{"--redis-addr", "127.0.0.1:1"})

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("dispatch with an unreachable Redis succeeded, want an error")
	}
}
