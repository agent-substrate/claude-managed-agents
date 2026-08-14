package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWakerCommandHelp(t *testing.T) {
	cmd := newWakerCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("waker --help failed: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"Waker runs a lightweight HTTP server inside a Substrate actor container",
		"--addr",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("waker help output missing %q:\n%s", expected, out)
		}
	}
}
