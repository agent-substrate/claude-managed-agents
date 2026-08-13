package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkerCommandHelp(t *testing.T) {
	cmd := newWorkerCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("worker --help failed: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"Worker runs the Anthropic Managed Agents self-hosted sandbox frontend server",
		"--anthropic-env-id",
		"--anthropic-env-key",
		"--ateapi-addr",
		"--namespace",
		"--template",
		"--workdir",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("worker help output missing %q:\n%s", expected, out)
		}
	}
}

func TestWorkerCommandMissingCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_ENVIRONMENT_ID", "")
	t.Setenv("ANTHROPIC_ENVIRONMENT_KEY", "")
	cmd := newWorkerCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	// Without env vars or flags, it should fail validation
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when credentials missing, got nil")
	}
}
