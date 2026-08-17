package main

import (
	"bytes"
	"testing"
)

func TestControllerCommandFlags(t *testing.T) {
	cmd := newControllerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "controller" {
		t.Errorf("cmd.Use = %q, want controller", cmd.Use)
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
			t.Errorf("missing flag --%s on controller command", name)
		}
	}
}
