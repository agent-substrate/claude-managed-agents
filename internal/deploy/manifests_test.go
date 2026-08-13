package deploy

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
		WorkerImage:     "example.com/worker@sha256:1111",
		AteomImage:      "example.com/ateom@sha256:2222",
		PauseImage:      DefaultPauseImage,
		SnapshotsBucket: "gs://test-bucket/snapshots/",
		Replicas:        3,
		WorkerReplicas:  1,
		Port:            8080,
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

func TestBuildManifests(t *testing.T) {
	cfg := testConfig()
	objs := BuildManifests(cfg)
	if len(objs) != 6 {
		t.Fatalf("got %d manifests, want 6", len(objs))
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
	if template.Spec.SnapshotsConfig.Location != cfg.SnapshotsBucket {
		t.Errorf("snapshots location = %q", template.Spec.SnapshotsConfig.Location)
	}

	deployment := objs[4].(*appsv1.Deployment)
	if deployment.Name != DefaultDeploymentName {
		t.Errorf("deployment name = %q", deployment.Name)
	}

	service := objs[5].(*corev1.Service)
	if service.Name != DefaultServiceName {
		t.Errorf("service name = %q", service.Name)
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
		"kind: Service",
		"image: example.com/worker@sha256:1111",
		"location: gs://test-bucket/snapshots/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
