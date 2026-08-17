package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestManifestCommand(t *testing.T) {
	cmd := newManifestCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--worker-image", "gcr.io/test/worker@sha256:123456",
		"--ateom-image", "gcr.io/test/ateom@sha256:789012",
		"--snapshots-bucket", "gs://my-bucket/snapshots/",
		"--namespace", "test-ns",
		"--anthropic-env-id", "env_test_abc",
		"--anthropic-env-key", "key_test_xyz",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("manifest command execute: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"kind: Namespace",
		"name: test-ns",
		"kind: Secret",
		"name: ate-antworker-creds",
		"ANTHROPIC_ENVIRONMENT_ID: env_test_abc",
		"kind: Service",
		"name: valkey",
		"kind: StatefulSet",
		"kind: WorkerPool",
		"kind: ActorTemplate",
		"kind: Deployment",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("manifest output missing %q:\n%s", expected, out)
		}
	}
}

func TestManifestCommandMissingRequiredFlags(t *testing.T) {
	cmd := newManifestCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err == nil {
		t.Error("expected error when required flags are missing, got nil")
	}
}

