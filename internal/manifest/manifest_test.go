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

package manifest

import (
	"bytes"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	atev1alpha1 "go.ate.dev/substrate/pkg/api/v1alpha1"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func testConfig() Config {
	return Config{
		Namespace:       "ate-ant",
		Template:        "ate-ant-actor-template",
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

	find[*corev1.ServiceAccount](t, objs, RouterClientServiceAccount)
	controller := find[*appsv1.Deployment](t, objs, defaultControllerDeploymentName)
	if got := controller.Spec.Template.Spec.ServiceAccountName; got != RouterClientServiceAccount {
		t.Errorf("controller serviceAccountName = %q, want %q", got, RouterClientServiceAccount)
	}
	var podCert, routerCA bool
	for _, v := range controller.Spec.Template.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.PodCertificate != nil && s.PodCertificate.SignerName == podIdentitySigner {
				podCert = true
			}
			if s.ClusterTrustBundle != nil && s.ClusterTrustBundle.SignerName != nil && *s.ClusterTrustBundle.SignerName == serviceDNSSigner {
				routerCA = true
			}
		}
	}
	if !podCert || !routerCA {
		t.Errorf("controller router mTLS volumes: podidentity cert = %v, servicedns trust bundle = %v; want both", podCert, routerCA)
	}

	ns := find[*corev1.Namespace](t, objs, "ate-ant")
	if ns.Name != "ate-ant" {
		t.Errorf("namespace = %q, want ate-ant", ns.Name)
	}

	secret := find[*corev1.Secret](t, objs, defaultSecretName)
	if secret.StringData["ANTHROPIC_ENVIRONMENT_ID"] != "env_test_123" {
		t.Errorf("secret env id = %q", secret.StringData["ANTHROPIC_ENVIRONMENT_ID"])
	}
	if secret.StringData["VALKEY_ADDR"] != DefaultValkeyAddr {
		t.Errorf("secret valkey addr = %q, want %q", secret.StringData["VALKEY_ADDR"], DefaultValkeyAddr)
	}

	valkeySvc := find[*corev1.Service](t, objs, defaultValkeyName)
	if len(valkeySvc.Spec.Ports) != 1 || valkeySvc.Spec.Ports[0].Port != 6379 {
		t.Errorf("valkey service ports = %+v", valkeySvc.Spec.Ports)
	}

	valkeySts := find[*appsv1.StatefulSet](t, objs, defaultValkeyName)
	if valkeySts.Spec.Replicas == nil || *valkeySts.Spec.Replicas != 1 {
		t.Errorf("valkey statefulset replicas = %v, want 1", valkeySts.Spec.Replicas)
	}
	if len(valkeySts.Spec.VolumeClaimTemplates) != 1 {
		t.Errorf("valkey statefulset volume claim templates count = %d, want 1", len(valkeySts.Spec.VolumeClaimTemplates))
	}

	pool := find[*atev1alpha1.WorkerPool](t, objs, defaultWorkerPool)
	if pool.Spec.Replicas != 3 {
		t.Errorf("workerpool = %+v", pool)
	}

	controllerDep := find[*appsv1.Deployment](t, objs, defaultControllerDeploymentName)
	if controllerDep.Spec.Replicas == nil || *controllerDep.Spec.Replicas != 3 {
		t.Errorf("controller deployment replicas = %v, want 3", *controllerDep.Spec.Replicas)
	}
	if len(controllerDep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("controller deployment containers count = %d, want 1", len(controllerDep.Spec.Template.Spec.Containers))
	}
	controllerContainer := controllerDep.Spec.Template.Spec.Containers[0]
	if len(controllerContainer.Command) < 2 || controllerContainer.Command[0] != "ate-ant" || controllerContainer.Command[1] != "controller" {
		t.Errorf("controller deployment container command = %v, want ['ate-ant', 'controller']", controllerContainer.Command)
	}
	if len(controllerContainer.EnvFrom) == 0 || controllerContainer.EnvFrom[0].SecretRef == nil || controllerContainer.EnvFrom[0].SecretRef.Name != defaultSecretName {
		t.Errorf("controller container envFrom = %+v, want secretRef %q", controllerContainer.EnvFrom, defaultSecretName)
	}
}

func TestBuildActorTemplate(t *testing.T) {
	cfg := testConfig()
	tmpl := BuildActorTemplate(cfg)

	if tmpl.GetMetadata().GetName() != cfg.Template {
		t.Errorf("template name = %q, want %q", tmpl.GetMetadata().GetName(), cfg.Template)
	}
	if tmpl.GetMetadata().GetAtespace() != cfg.Namespace {
		t.Errorf("template atespace = %q, want %q", tmpl.GetMetadata().GetAtespace(), cfg.Namespace)
	}
	if tmpl.GetWorkerSelector().GetMatchLabels()["workload"] != cfg.Template {
		t.Errorf("worker selector workload label = %q, want %q", tmpl.GetWorkerSelector().GetMatchLabels()["workload"], cfg.Template)
	}
	if len(tmpl.Containers) != 1 || tmpl.Containers[0].Image != cfg.WorkerImage {
		t.Errorf("template containers = %+v", tmpl.Containers)
	}
	if len(tmpl.Containers[0].Command) < 2 || tmpl.Containers[0].Command[0] != "ate-ant" || tmpl.Containers[0].Command[1] != "run" {
		t.Errorf("template container command = %v, want ['ate-ant', 'run']", tmpl.Containers[0].Command)
	}
	if tmpl.Containers[0].WakeupProbe == nil || tmpl.Containers[0].WakeupProbe.HttpGet == nil ||
		tmpl.Containers[0].WakeupProbe.HttpGet.Path != "/healthz" || tmpl.Containers[0].WakeupProbe.HttpGet.Port != 80 {
		t.Errorf("template container wakeup probe = %+v, want /healthz on 80", tmpl.Containers[0].WakeupProbe)
	}
	if tmpl.GetSnapshotConfig().GetStorageLocation() != cfg.SnapshotsBucket {
		t.Errorf("snapshots location = %q, want %q", tmpl.GetSnapshotConfig().GetStorageLocation(), cfg.SnapshotsBucket)
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
		"kind: Deployment",
		"name: ate-ant",
		"ate-ant",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kind: ActorTemplate") {
		t.Errorf("BuildManifests output should not contain ActorTemplate CRD")
	}
}

func TestWriteActorTemplate(t *testing.T) {
	cfg := testConfig()
	var buf bytes.Buffer
	if err := WriteActorTemplate(&buf, BuildActorTemplate(cfg)); err != nil {
		t.Fatalf("WriteActorTemplate: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"name: ate-ant-actor-template",
		"atespace: ate-ant",
		"image: gcr.io/test/worker@sha256:1111",
		"storageLocation: gs://test-bucket/snapshots/",
		"path: /healthz",
		"port: 80",
		"ANTHROPIC_ENVIRONMENT_ID",
		"env_test_123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "status:") {
		t.Errorf("output should not contain status fields:\n%s", out)
	}
	if strings.Contains(out, cfg.EnvironmentKey) {
		t.Errorf("ActorTemplate should not embed the real environment key:\n%s", out)
	}
	tmpl := BuildActorTemplate(cfg)
	if got := actorProtoEnv(tmpl, "ANTHROPIC_ENVIRONMENT_KEY"); got != EnvironmentKeyPlaceholder {
		t.Errorf("ActorTemplate ANTHROPIC_ENVIRONMENT_KEY = %q, want placeholder %q", got, EnvironmentKeyPlaceholder)
	}
	if got := actorProtoEnv(tmpl, "SSL_CERT_FILE"); got != TrustBundleFile {
		t.Errorf("ActorTemplate SSL_CERT_FILE = %q, want %q", got, TrustBundleFile)
	}
	if len(tmpl.Volumes) != 1 || tmpl.Volumes[0].GetSystemInfo() == nil {
		t.Fatalf("ActorTemplate volumes = %+v, want one systemInfo trust bundle volume", tmpl.Volumes)
	}
	mounts := tmpl.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].Name != tmpl.Volumes[0].Name {
		t.Errorf("worker volume mounts = %+v, want the trust bundle volume", mounts)
	}
}

func TestBuildManifestsWithAPIKey(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentKey = ""
	cfg.APIKey = "sk-ant-api03-test-key"

	objs := BuildManifests(cfg)
	secret := find[*corev1.Secret](t, objs, defaultSecretName)
	if secret.StringData["ANTHROPIC_API_KEY"] != "sk-ant-api03-test-key" {
		t.Errorf("secret ANTHROPIC_API_KEY = %q", secret.StringData["ANTHROPIC_API_KEY"])
	}
	if _, ok := secret.StringData["ANTHROPIC_ENVIRONMENT_KEY"]; ok {
		t.Errorf("expected no ANTHROPIC_ENVIRONMENT_KEY in secret")
	}

	tmpl := BuildActorTemplate(cfg)
	for _, env := range tmpl.Containers[0].Env {
		if env.Name == "ANTHROPIC_API_KEY" {
			t.Errorf("expected no ANTHROPIC_API_KEY in ActorTemplate container env, found: %+v", env)
		}
	}
}

func TestBuildManifestsWithBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "http://managed-agents.example.svc.cluster.local:8400"

	objs := BuildManifests(cfg)
	secret := find[*corev1.Secret](t, objs, defaultSecretName)
	if secret.StringData["ANTHROPIC_BASE_URL"] != cfg.BaseURL {
		t.Errorf("secret ANTHROPIC_BASE_URL = %q, want %q", secret.StringData["ANTHROPIC_BASE_URL"], cfg.BaseURL)
	}
	tmpl := BuildActorTemplate(cfg)
	if got := actorProtoEnv(tmpl, "ANTHROPIC_BASE_URL"); got != cfg.BaseURL {
		t.Errorf("ActorTemplate ANTHROPIC_BASE_URL = %q, want %q", got, cfg.BaseURL)
	}

	objs = BuildManifests(testConfig())
	secret = find[*corev1.Secret](t, objs, defaultSecretName)
	if _, ok := secret.StringData["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("expected no ANTHROPIC_BASE_URL in secret when unset")
	}
	tmpl = BuildActorTemplate(testConfig())
	if got := actorProtoEnv(tmpl, "ANTHROPIC_BASE_URL"); got != "" {
		t.Errorf("expected no ANTHROPIC_BASE_URL in ActorTemplate container env when unset, got %q", got)
	}
}

func actorProtoEnv(template *ateapipb.ActorTemplate, name string) string {
	for _, env := range template.Containers[0].Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

func TestBuildCredentialProviderPolicy(t *testing.T) {
	cfg := testConfig()
	cfg.Namespace = "team-ns"

	secret := BuildSecret(cfg)
	if secret.Labels[EgressCredentialLabel] != "true" {
		t.Errorf("secret labels = %v, want %s=true", secret.Labels, EgressCredentialLabel)
	}

	cm := find[*corev1.ConfigMap](t, BuildManifests(cfg), CredentialProviderPolicyConfigMap)
	if cm.Namespace != CredentialProviderNamespace || cm.Name != CredentialProviderPolicyConfigMap {
		t.Errorf("configmap = %s/%s", cm.Namespace, cm.Name)
	}
	var got credentialProviderPolicy
	if err := yaml.Unmarshal([]byte(cm.Data[CredentialProviderPolicyKey]), &got); err != nil {
		t.Fatalf("decoding policy: %v\n%s", err, cm.Data[CredentialProviderPolicyKey])
	}
	if len(got.Policies) != 1 {
		t.Fatalf("policies = %+v, want one", got.Policies)
	}
	p := got.Policies[0]
	if p.Atespace != "team-ns" || len(p.AllowedNamespaces) != 1 || p.AllowedNamespaces[0] != "team-ns" {
		t.Errorf("grant = %+v, want atespace and namespace team-ns", p)
	}
	if len(p.SecretSelector.MatchLabels) != 1 || p.SecretSelector.MatchLabels[EgressCredentialLabel] != "true" {
		t.Errorf("secret selector = %+v, want %s=true", p.SecretSelector.MatchLabels, EgressCredentialLabel)
	}
}
