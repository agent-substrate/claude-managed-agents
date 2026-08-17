package manifest

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
	if len(objs) != 10 {
		t.Fatalf("got %d manifests, want 10", len(objs))
	}

	ns := find[*corev1.Namespace](t, objs, "ate-antworker")
	if ns.Name != "ate-antworker" {
		t.Errorf("namespace = %q, want ate-antworker", ns.Name)
	}

	secret := find[*corev1.Secret](t, objs, DefaultSecretName)
	if secret.StringData["ANTHROPIC_ENVIRONMENT_ID"] != "env_test_123" {
		t.Errorf("secret env id = %q", secret.StringData["ANTHROPIC_ENVIRONMENT_ID"])
	}
	if secret.StringData["VALKEY_ADDR"] != DefaultValkeyAddr {
		t.Errorf("secret valkey addr = %q, want %q", secret.StringData["VALKEY_ADDR"], DefaultValkeyAddr)
	}

	valkeySvc := find[*corev1.Service](t, objs, DefaultValkeyName)
	if len(valkeySvc.Spec.Ports) != 1 || valkeySvc.Spec.Ports[0].Port != 6379 {
		t.Errorf("valkey service ports = %+v", valkeySvc.Spec.Ports)
	}

	valkeySts := find[*appsv1.StatefulSet](t, objs, DefaultValkeyName)
	if valkeySts.Spec.Replicas == nil || *valkeySts.Spec.Replicas != 1 {
		t.Errorf("valkey statefulset replicas = %v, want 1", valkeySts.Spec.Replicas)
	}
	if len(valkeySts.Spec.VolumeClaimTemplates) != 1 {
		t.Errorf("valkey statefulset volume claim templates count = %d, want 1", len(valkeySts.Spec.VolumeClaimTemplates))
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
	if len(template.Spec.Containers[0].Env) == 0 {
		t.Errorf("template container env is empty, expected secretKeyRefs")
	}
	if template.Spec.Containers[0].Readyz == nil || template.Spec.Containers[0].Readyz.HTTPGet == nil || template.Spec.Containers[0].Readyz.HTTPGet.Path != "/healthz" || template.Spec.Containers[0].Readyz.HTTPGet.Port != 80 {
		t.Errorf("template container readyz = %+v, want path '/healthz' and port 80", template.Spec.Containers[0].Readyz)
	}
	if template.Spec.SnapshotsConfig.Location != cfg.SnapshotsBucket {
		t.Errorf("snapshots location = %q", template.Spec.SnapshotsConfig.Location)
	}

	deployment := find[*appsv1.Deployment](t, objs, DefaultDeploymentName)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Errorf("deployment replicas = %v, want 1", *deployment.Spec.Replicas)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment containers count = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}
	workerContainer := deployment.Spec.Template.Spec.Containers[0]
	if len(workerContainer.Command) < 1 || workerContainer.Command[0] != "ate-antworker" {
		t.Errorf("deployment container command = %v, want ['ate-antworker']", workerContainer.Command)
	}
	if len(workerContainer.EnvFrom) == 0 || workerContainer.EnvFrom[0].SecretRef == nil || workerContainer.EnvFrom[0].SecretRef.Name != DefaultSecretName {
		t.Errorf("worker container envFrom = %+v, want secretRef %q", workerContainer.EnvFrom, DefaultSecretName)
	}

	consumerDep := find[*appsv1.Deployment](t, objs, DefaultConsumerDeploymentName)
	if consumerDep.Spec.Replicas == nil || *consumerDep.Spec.Replicas != 1 {
		t.Errorf("consumer deployment replicas = %v, want 1", *consumerDep.Spec.Replicas)
	}
	if len(consumerDep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("consumer deployment containers count = %d, want 1", len(consumerDep.Spec.Template.Spec.Containers))
	}
	consumerContainer := consumerDep.Spec.Template.Spec.Containers[0]
	if len(consumerContainer.Command) < 2 || consumerContainer.Command[0] != "ate-antworker" || consumerContainer.Command[1] != "consumer" {
		t.Errorf("consumer deployment container command = %v, want ['ate-antworker', 'consumer']", consumerContainer.Command)
	}
	if len(consumerContainer.EnvFrom) == 0 || consumerContainer.EnvFrom[0].SecretRef == nil || consumerContainer.EnvFrom[0].SecretRef.Name != DefaultSecretName {
		t.Errorf("consumer container envFrom = %+v, want secretRef %q", consumerContainer.EnvFrom, DefaultSecretName)
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
		"name: ate-antworker",
		"image: gcr.io/test/worker@sha256:1111",
		"location: gs://test-bucket/snapshots/",
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
	foundAPIKey := false
	for _, env := range template.Spec.Containers[0].Env {
		if env.Name == "ANTHROPIC_API_KEY" {
			foundAPIKey = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Key != "ANTHROPIC_API_KEY" {
				t.Errorf("unexpected ANTHROPIC_API_KEY env: %+v", env)
			}
		}
	}
	if !foundAPIKey {
		t.Errorf("ANTHROPIC_API_KEY not found in ActorTemplate container env: %+v", template.Spec.Containers[0].Env)
	}
}
