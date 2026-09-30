// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
		"--namespace", "test-ns",
		"--anthropic-env-id", "env_test_abc",
		"--anthropic-env-key", "key_test_xyz",
		"--anthropic-base-url", "http://managed-agents.example.svc.cluster.local:8400",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("manifest command execute: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"kind: Namespace",
		"name: test-ns",
		"kind: Secret",
		"name: ate-ant-creds",
		"ANTHROPIC_ENVIRONMENT_ID: env_test_abc",
		"ANTHROPIC_BASE_URL: http://managed-agents.example.svc.cluster.local:8400",
		"kind: Service",
		"name: valkey",
		"kind: StatefulSet",
		"kind: WorkerPool",
		"kind: Deployment",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("manifest output missing %q:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "kind: ActorTemplate") {
		t.Errorf("manifest output should not contain kind: ActorTemplate")
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

func TestManifestTemplateCommand(t *testing.T) {
	cmd := newManifestCommand()

	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"template",
		"--worker-image", "gcr.io/test/worker@sha256:123456",
		"--snapshots-bucket", "gs://my-bucket/snapshots/",
		"--namespace", "test-ns",
		"--anthropic-env-id", "env_test_abc",
		"--anthropic-env-key", "key_test_xyz",
		"--anthropic-base-url", "http://managed-agents.example.svc.cluster.local:8400",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("manifest template command execute: %v", err)
	}

	out := buf.String()
	for _, expected := range []string{
		"name: ate-ant-actor-template",
		"atespace: test-ns",
		"image: gcr.io/test/worker@sha256:123456",
		"storageLocation: gs://my-bucket/snapshots/",
		"path: /healthz",
		"port: 80",
		"ANTHROPIC_ENVIRONMENT_ID",
		"env_test_abc",
		"ANTHROPIC_ENVIRONMENT_KEY",
		"key_test_xyz",
		"ANTHROPIC_BASE_URL",
		"http://managed-agents.example.svc.cluster.local:8400",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("manifest template output missing %q:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "status:") {
		t.Errorf("manifest template output should not contain status fields:\n%s", out)
	}
}

func TestManifestTemplateCommandMissingRequiredFlags(t *testing.T) {
	cmd := newManifestCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"template"})

	if err := cmd.Execute(); err == nil {
		t.Error("expected error when required flags are missing, got nil")
	}
}

