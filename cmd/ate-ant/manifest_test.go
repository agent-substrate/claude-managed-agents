package main

import (
	"bytes"
	"strings"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func testConfig() Config {
	return Config{
		Namespace:       "anthropic-worker",
		Template:        "anthropic-worker-template",
		WorkerPool:      "anthropic-worker-template-workerpool",
		WorkerImage:     "gcr.io/test/worker@sha256:1111",
		AteomImage:      "gcr.io/test/ateom@sha256:2222",
		PauseImage:      DefaultPauseImage,
		SnapshotsBucket: "gs://test-bucket/snapshots/",
		Replicas:        3,
		WorkerReplicas:  1,
		EnvironmentID:   "env_test_123",
		EnvironmentKey:  "key_test_123",
	}
}

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
		"name: anthropic-environment-creds",
		"ANTHROPIC_ENVIRONMENT_ID: env_test_abc",
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

func TestValidate(t *testing.T) {
	cfg := testConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on valid config: %v", err)
	}

	cfg.WorkerImage = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate on missing worker image: want error, got nil")
	}

	cfg = testConfig()
	cfg.AteomImage = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate on missing ateom image: want error, got nil")
	}

	cfg = testConfig()
	cfg.SnapshotsBucket = ""
	if err := cfg.Validate(); err == nil {
		t.Error("Validate on missing snapshots bucket: want error, got nil")
	}
}

func TestBuildManifests(t *testing.T) {
	cfg := testConfig()
	objs := BuildManifests(cfg)
	if len(objs) != 5 {
		t.Fatalf("got %d manifests, want 5", len(objs))
	}

	ns := objs[0].(*corev1.Namespace)
	if ns.Name != "anthropic-worker" {
		t.Errorf("namespace = %q, want anthropic-worker", ns.Name)
	}

	secret := objs[1].(*corev1.Secret)
	if secret.Name != DefaultSecretName {
		t.Errorf("secret name = %q, want %q", secret.Name, DefaultSecretName)
	}
	if secret.StringData["ANTHROPIC_ENVIRONMENT_ID"] != "env_test_123" {
		t.Errorf("secret env id = %q", secret.StringData["ANTHROPIC_ENVIRONMENT_ID"])
	}

	pool := objs[2].(*atev1alpha1.WorkerPool)
	if pool.Name != "anthropic-worker-template-workerpool" || pool.Spec.Replicas != 3 {
		t.Errorf("workerpool = %+v", pool)
	}

	template := objs[3].(*atev1alpha1.ActorTemplate)
	if len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Image != cfg.WorkerImage {
		t.Errorf("template containers = %+v", template.Spec.Containers)
	}
	if len(template.Spec.Containers[0].Command) < 2 || template.Spec.Containers[0].Command[0] != "ate-ant" || template.Spec.Containers[0].Command[1] != "waker" {
		t.Errorf("template container command = %v, want ['ate-ant', 'waker', ...]", template.Spec.Containers[0].Command)
	}
	if len(template.Spec.Containers[0].Env) != 2 {
		t.Errorf("template container env count = %d, want 2", len(template.Spec.Containers[0].Env))
	}
	if template.Spec.Containers[0].Readyz == nil || template.Spec.Containers[0].Readyz.HTTPGet == nil || template.Spec.Containers[0].Readyz.HTTPGet.Path != "/healthz" || template.Spec.Containers[0].Readyz.HTTPGet.Port != 80 {
		t.Errorf("template container readyz = %+v, want path '/healthz' and port 80", template.Spec.Containers[0].Readyz)
	}
	if template.Spec.SnapshotsConfig.Location != cfg.SnapshotsBucket {
		t.Errorf("snapshots location = %q", template.Spec.SnapshotsConfig.Location)
	}

	deployment := objs[4].(*appsv1.Deployment)
	if deployment.Name != DefaultDeploymentName {
		t.Errorf("deployment name = %q", deployment.Name)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment containers count = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}
	workerContainer := deployment.Spec.Template.Spec.Containers[0]
	if len(workerContainer.Command) < 3 || workerContainer.Command[0] != "ant" || workerContainer.Command[1] != "beta:worker" || workerContainer.Command[2] != "poll" {
		t.Errorf("deployment container command = %v, want ['ant', 'beta:worker', 'poll', ...]", workerContainer.Command)
	}
}

func TestWriteManifests(t *testing.T) {
	cfg := testConfig()
	var buf bytes.Buffer
	if err := WriteManifests(&buf, BuildManifests(cfg)); err != nil {
		t.Fatalf("WriteManifests: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"kind: Namespace",
		"kind: Secret",
		"kind: WorkerPool",
		"kind: ActorTemplate",
		"apiVersion: ate.dev/v1alpha1",
		"kind: Deployment",
		"image: gcr.io/test/worker@sha256:1111",
		"location: gs://test-bucket/snapshots/",
		"ant",
		"beta:worker",
		"poll",
		"ate-ant",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestBuildManifestsWithAPIKey(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentKey = ""
	cfg.APIKey = "sk-ant-api03-test-key"

	objs := BuildManifests(cfg)
	secret := objs[1].(*corev1.Secret)
	if secret.StringData["ANTHROPIC_API_KEY"] != "sk-ant-api03-test-key" {
		t.Errorf("secret ANTHROPIC_API_KEY = %q", secret.StringData["ANTHROPIC_API_KEY"])
	}
	if _, ok := secret.StringData["ANTHROPIC_ENVIRONMENT_KEY"]; ok {
		t.Errorf("expected no ANTHROPIC_ENVIRONMENT_KEY in secret")
	}

	template := objs[3].(*atev1alpha1.ActorTemplate)
	foundAPIKey := false
	for _, env := range template.Spec.Containers[0].Env {
		if env.Name == "ANTHROPIC_API_KEY" && *env.Value == "sk-ant-api03-test-key" {
			foundAPIKey = true
		}
	}
	if !foundAPIKey {
		t.Errorf("expected ANTHROPIC_API_KEY in ActorTemplate env: %+v", template.Spec.Containers[0].Env)
	}
}
