package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsumeCommandHelp(t *testing.T) {
	cmd := newConsumeCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("consume --help failed: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"Consume drains the durable Redis queue",
		"--redis-addr",
		"--queue-prefix",
		"--max-attempts",
		"--ateapi-addr",
		"--namespace",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("consume help output missing %q:\n%s", expected, out)
		}
	}
}

func TestConsumeCommandFailsWithoutRedis(t *testing.T) {
	cmd := newConsumeCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// Port 1 has nothing listening, so connecting fails fast.
	cmd.SetArgs([]string{"--redis-addr", "127.0.0.1:1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("consume with an unreachable Redis succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "connecting to Redis") {
		t.Errorf("error = %v, want it to report the Redis connection failure", err)
	}
}
