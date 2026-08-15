package main

import (
	"bytes"
	"strings"
	"testing"
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
		"--wait",
		"--ateapi-addr",
		"--atenet-router-addr",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("dispatch help output missing %q:\n%s", expected, out)
		}
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
