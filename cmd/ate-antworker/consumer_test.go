package main

import (
	"bytes"
	"testing"
)

func TestConsumerCommandFlags(t *testing.T) {
	cmd := newConsumerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "consumer" {
		t.Errorf("cmd.Use = %q, want consumer", cmd.Use)
	}

	flags := []string{
		"namespace",
		"template",
		"ateapi-addr",
		"atenet-router-addr",
		"insecure",
	}

	for _, name := range flags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s on consumer command", name)
		}
	}
}
