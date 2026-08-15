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
		Namespace:       "ate-antworker",
		Template:        "ate-antworker-actor-template",
		WorkerImage:     "gcr.io/test/worker@sha256:1111",
		AteomImage:      "gcr.io/test/ateom@sha256:2222",
		SnapshotsBucket: "gs://test-bucket/snapshots/",
		Replicas:        3,
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
		"name: ate-antworker-creds",
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

// find returns the generated object of type T with the given name.
func find[T any](t *testing.T, objs []any, name string) T {
	t.Helper()
	for _, obj := range objs {
		typed, ok := obj.(T)
		if !ok {
			continue
		}
		if meta, ok := obj.(interface{ GetName() string }); ok && meta.GetName() != name {
			continue
		}
		return typed
	}
	var zero T
	t.Fatalf("no %T named %q in generated manifests", zero, name)
	return zero
}

func TestBuildManifests(t *testing.T) {
	cfg := testConfig()
	objs := BuildManifests(cfg)
	if len(objs) != 5 {
		t.Fatalf("got %d manifests, want 5", len(objs))
	}

	ns := find[*corev1.Namespace](t, objs, "ate-antworker")
	if ns.Name != "ate-antworker" {
		t.Errorf("namespace = %q, want ate-antworker", ns.Name)
	}

	secret := find[*corev1.Secret](t, objs, DefaultSecretName)
	if secret.StringData["ANTHROPIC_ENVIRONMENT_ID"] != "env_test_123" {
		t.Errorf("secret env id = %q", secret.StringData["ANTHROPIC_ENVIRONMENT_ID"])
	}

	pool := find[*atev1alpha1.WorkerPool](t, objs, DefaultWorkerPool)
	if pool.Spec.Replicas != 3 {
		t.Errorf("workerpool = %+v", pool)
	}

	template := find[*atev1alpha1.ActorTemplate](t, objs, cfg.Template)
	if len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Image != cfg.WorkerImage {
		t.Errorf("template containers = %+v", template.Spec.Containers)
	}
	if len(template.Spec.Containers[0].Command) < 2 || template.Spec.Containers[0].Command[0] != "ate-antworker" || template.Spec.Containers[0].Command[1] != "worker" {
		t.Errorf("template container command = %v, want ['ate-antworker', 'worker', ...]", template.Spec.Containers[0].Command)
	}
	if len(template.Spec.Containers[0].Env) != 0 {
		t.Errorf("template container env count = %d, want 0", len(template.Spec.Containers[0].Env))
	}
	if template.Spec.Containers[0].Readyz == nil || template.Spec.Containers[0].Readyz.HTTPGet == nil || template.Spec.Containers[0].Readyz.HTTPGet.Path != "/healthz" || template.Spec.Containers[0].Readyz.HTTPGet.Port != 80 {
		t.Errorf("template container readyz = %+v, want path '/healthz' and port 80", template.Spec.Containers[0].Readyz)
	}
	if template.Spec.SnapshotsConfig.Location != cfg.SnapshotsBucket {
		t.Errorf("snapshots location = %q", template.Spec.SnapshotsConfig.Location)
	}

	deployment := find[*appsv1.Deployment](t, objs, DefaultDeploymentName)
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment containers count = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}
	workerContainer := deployment.Spec.Template.Spec.Containers[0]
	if len(workerContainer.Command) < 3 || workerContainer.Command[0] != "ant" || workerContainer.Command[1] != "beta:worker" || workerContainer.Command[2] != "poll" {
		t.Errorf("deployment container command = %v, want ['ant', 'beta:worker', 'poll', ...]", workerContainer.Command)
	}

	poll := strings.Join(workerContainer.Command, " ")
	if !strings.Contains(poll, "--on-work ate-antworker") {
		t.Errorf("poller command = %v, want --on-work ate-antworker", workerContainer.Command)
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
		"ate-antworker",
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
	secret := find[*corev1.Secret](t, objs, DefaultSecretName)
	if secret.StringData["ANTHROPIC_API_KEY"] != "sk-ant-api03-test-key" {
		t.Errorf("secret ANTHROPIC_API_KEY = %q", secret.StringData["ANTHROPIC_API_KEY"])
	}
	if _, ok := secret.StringData["ANTHROPIC_ENVIRONMENT_KEY"]; ok {
		t.Errorf("expected no ANTHROPIC_ENVIRONMENT_KEY in secret")
	}

	template := find[*atev1alpha1.ActorTemplate](t, objs, cfg.Template)
	if len(template.Spec.Containers[0].Env) != 0 {
		t.Errorf("expected no env in ActorTemplate: %+v", template.Spec.Containers[0].Env)
	}
}
