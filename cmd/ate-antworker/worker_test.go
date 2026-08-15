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
		"Worker runs a lightweight HTTP server inside a Substrate actor container",
		"--addr",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("worker help output missing %q:\n%s", expected, out)
		}
	}
}
