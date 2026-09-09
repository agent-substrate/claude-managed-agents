package main

import (
	"bytes"
	"testing"
)

func TestPollCommand(t *testing.T) {
	cmd := newPollCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "poll" {
		t.Errorf("cmd.Use = %q, want poll", cmd.Use)
	}
	if cmd.Flags().Lookup("handoff-ttl-seconds") == nil {
		t.Error("missing flag --handoff-ttl-seconds on poll command")
	}
	if cmd.Flags().Lookup("anthropic-base-url") == nil {
		t.Error("missing flag --anthropic-base-url on poll command")
	}
}

func TestRootCommandSubcommands(t *testing.T) {
	root := newRootCommand()

	expectedSubcommands := map[string]bool{
		"manifest":   false,
		"poll":       false,
		"run":        false,
		"controller": false,
		"version":    false,
	}

	for _, sub := range root.Commands() {
		if _, ok := expectedSubcommands[sub.Name()]; ok {
			expectedSubcommands[sub.Name()] = true
		}
	}

	for name, found := range expectedSubcommands {
		if !found {
			t.Errorf("expected subcommand %q on root command", name)
		}
	}
}
