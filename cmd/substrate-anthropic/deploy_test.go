package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDeployCommand(t *testing.T) {
	cmd := newDeployCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--worker-image", "gcr.io/test/worker@sha256:123456",
		"--ateom-image", "gcr.io/test/ateom@sha256:789012",
		"--snapshots-bucket", "gs://my-bucket/snapshots/",
		"--namespace", "test-ns",
		"--env-id", "env_test_abc",
		"--env-key", "key_test_xyz",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("deploy command execute: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"kind: Namespace",
		"name: test-ns",
		"kind: Secret",
		"name: anthropic-environment-creds",
		"ANTHROPIC_ENVIRONMENT_ID: env_test_abc",
		"kind: WorkerPool",
		"kind: ActorTemplate",
		"kind: Deployment",
		"kind: Service",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("manifest output missing %q:\n%s", expected, out)
		}
	}
}

func TestDeployCommandMissingRequiredFlags(t *testing.T) {
	cmd := newDeployCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err == nil {
		t.Error("expected error when required flags are missing, got nil")
	}
}
