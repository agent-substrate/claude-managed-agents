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
	"encoding/json"
	"errors"
	"fmt"
	"io"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// Default constants for names and paths.
const (
	DefaultNamespace          = "ate-ant"
	DefaultTemplate           = "ate-ant-actor-template"
	DefaultReplicas = 3
	DefaultAteapiAddr         = "api.ate-system.svc.cluster.local:443"
	DefaultAtenetRouterAddr   = "atenet-router.ate-system.svc.cluster.local:80"
	DefaultValkeyImage        = "valkey/valkey:8"
	DefaultValkeyAddr         = "valkey.ate-ant.svc.cluster.local:6379"

	defaultWorkerPool               = "ate-ant-workerpool"
	defaultDeploymentName           = "ate-ant"
	defaultControllerDeploymentName = "ate-ant-controller"
	defaultSecretName               = "ate-ant-creds"
	defaultValkeyName               = "valkey"
	defaultValkeyPort               = 6379
	defaultValkeyStorageSize        = "10Gi"
)

// Config defines the parameters for generating Substrate and Kubernetes deployment manifests.
type Config struct {
	Namespace          string `json:"namespace"`
	Template           string `json:"template"`
	WorkerImage        string `json:"worker_image"`
	AteomImage         string `json:"ateom_image"`
	SnapshotsBucket    string `json:"snapshots_bucket"`
	Replicas           int32  `json:"replicas"`
	ControllerReplicas int32  `json:"controller_replicas"`
	EnvironmentID      string `json:"environment_id"`
	EnvironmentKey     string `json:"environment_key"`
	APIKey             string `json:"api_key"`
	BaseURL            string `json:"base_url"`
	AteapiAddr         string `json:"ateapi_addr"`
	AtenetRouterAddr   string `json:"atenet_router_addr"`
	ValkeyAddr         string `json:"valkey_addr"`
	ValkeyImage        string `json:"valkey_image"`
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if c.Replicas == 0 {
		c.Replicas = DefaultReplicas
	}
	if c.ControllerReplicas == 0 {
		c.ControllerReplicas = c.Replicas
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
	}
	if c.AtenetRouterAddr == "" {
		c.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if c.ValkeyImage == "" {
		c.ValkeyImage = DefaultValkeyImage
	}
	if c.ValkeyAddr == "" {
		if c.Namespace == DefaultNamespace {
			c.ValkeyAddr = DefaultValkeyAddr
		} else {
			c.ValkeyAddr = fmt.Sprintf("valkey.%s.svc.cluster.local:6379", c.Namespace)
		}
	}
}

// Validate ensures required fields are set before generating Kubernetes manifests.
func (c *Config) Validate() error {
	c.Defaults()
	if c.WorkerImage == "" {
		return errors.New("--worker-image is required")
	}
	if c.AteomImage == "" {
		return errors.New("--ateom-image is required")
	}
	return nil
}

// BuildManifests constructs all Kubernetes objects in apply order.
func BuildManifests(cfg Config) []any {
	cfg.Defaults()

	return []any{
		BuildNamespace(cfg),
		BuildSecret(cfg),
		BuildSecretsReaderRole(cfg),
		BuildSecretsReaderRoleBinding(cfg),
		BuildValkeyService(cfg),
		BuildValkeyStatefulSet(cfg),
		BuildWorkerPool(cfg),
		BuildControllerDeployment(cfg),
	}
}

// BuildNamespace constructs the corev1.Namespace manifest.
func BuildNamespace(cfg Config) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace},
	}
}

// BuildSecret constructs the corev1.Secret containing Anthropic environment credentials and Valkey address.
func BuildSecret(cfg Config) *corev1.Secret {
	stringData := map[string]string{
		"ANTHROPIC_ENVIRONMENT_ID": cfg.EnvironmentID,
		"VALKEY_ADDR":              cfg.ValkeyAddr,
	}
	if cfg.EnvironmentKey != "" {
		stringData["ANTHROPIC_ENVIRONMENT_KEY"] = cfg.EnvironmentKey
	}
	if cfg.APIKey != "" {
		stringData["ANTHROPIC_API_KEY"] = cfg.APIKey
	}
	if cfg.BaseURL != "" {
		stringData["ANTHROPIC_BASE_URL"] = cfg.BaseURL
	}

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName,
			Namespace: cfg.Namespace,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: stringData,
	}
}

// BuildSecretsReaderRole constructs a Role allowing ate-api-server to read secrets in this namespace.
func BuildSecretsReaderRole(cfg Config) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName + "-reader",
			Namespace: cfg.Namespace,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get"},
		}},
	}
}

// BuildSecretsReaderRoleBinding binds the secrets reader Role to ate-api-server serviceaccount in ate-system.
func BuildSecretsReaderRoleBinding(cfg Config) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultSecretName + "-reader",
			Namespace: cfg.Namespace,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "ate-api-server",
			Namespace: "ate-system",
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:      "Role",
			Name:      defaultSecretName + "-reader",
		},
	}
}

// BuildValkeyService constructs the Kubernetes Service for the Valkey instance.
func BuildValkeyService(cfg Config) *corev1.Service {
	labels := map[string]string{"app": defaultValkeyName}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultValkeyName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       defaultValkeyName,
				Port:       defaultValkeyPort,
				TargetPort: intstr.FromInt(defaultValkeyPort),
			}},
		},
	}
}

// BuildValkeyStatefulSet constructs the Kubernetes StatefulSet for a single-replica Valkey.
func BuildValkeyStatefulSet(cfg Config) *appsv1.StatefulSet {
	labels := map[string]string{"app": defaultValkeyName}
	replicas := int32(1)

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultValkeyName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: defaultValkeyName,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  defaultValkeyName,
						Image: cfg.ValkeyImage,
						Args:  []string{"valkey-server", "--save", "60 1", "--loglevel", "notice"},
						Ports: []corev1.ContainerPort{{
							Name:          defaultValkeyName,
							ContainerPort: defaultValkeyPort,
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data",
						}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "data",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(defaultValkeyStorageSize),
						},
					},
				},
			}},
		},
	}
}

// BuildWorkerPool constructs the Substrate WorkerPool CRD.
func BuildWorkerPool(cfg Config) *atev1alpha1.WorkerPool {
	return &atev1alpha1.WorkerPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: atev1alpha1.GroupVersion.String(),
			Kind:       "WorkerPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultWorkerPool,
			Namespace: cfg.Namespace,
			Labels: map[string]string{
				"workload": cfg.Template,
			},
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:     cfg.Replicas,
			WorkerImage:  cfg.AteomImage,
			SandboxClass: "gvisor",
		},
	}
}

// BuildActorTemplate constructs the protojson-shaped ateapipb.ActorTemplate for the Substrate API.
func BuildActorTemplate(cfg Config) *ateapipb.ActorTemplate {
	cfg.Defaults()

	var env []*ateapipb.EnvVar
	if cfg.EnvironmentKey != "" {
		env = append(env, &ateapipb.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_KEY",
			Value: cfg.EnvironmentKey,
		})
	}
	if cfg.EnvironmentID != "" {
		env = append(env, &ateapipb.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_ID",
			Value: cfg.EnvironmentID,
		})
	}
	if cfg.BaseURL != "" {
		env = append(env, &ateapipb.EnvVar{
			Name:  "ANTHROPIC_BASE_URL",
			Value: cfg.BaseURL,
		})
	}

	return &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: cfg.Namespace,
			Name:     cfg.Template,
		},
		WorkerSelector: &ateapipb.Selector{
			MatchLabels: map[string]string{
				"workload": cfg.Template,
			},
		},
		Containers: []*ateapipb.Container{{
			Name:    "worker",
			Image:   cfg.WorkerImage,
			Command: []string{"ate-ant", "run"},
			Env:     env,
			WakeupProbe: &ateapipb.ContainerWakeupProbe{
				HttpGet: &ateapipb.HTTPGetAction{
					Path: "/healthz",
					Port: 80,
				},
				TimeoutSeconds: 30,
			},
		}},
		SnapshotConfig: &ateapipb.SnapshotConfig{
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			StorageLocation: cfg.SnapshotsBucket,
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
			ConfigName:   "gvisor-default",
		},
	}
}

// BuildPollerDeployment constructs the Kubernetes Deployment for the poller daemon.
func BuildPollerDeployment(cfg Config) *appsv1.Deployment {
	labels := map[string]string{"app": defaultDeploymentName}
	replicas := int32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultDeploymentName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  defaultDeploymentName,
						Image: cfg.WorkerImage,
						Command: []string{
							"ate-ant",
							"poll",
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: defaultSecretName,
								},
							},
						}},
					}},
				},
			},
		},
	}
}

// BuildControllerDeployment constructs the Kubernetes Deployment for the session controller daemon.
func BuildControllerDeployment(cfg Config) *appsv1.Deployment {
	labels := map[string]string{"app": defaultControllerDeploymentName}
	replicas := cfg.ControllerReplicas

	expirationSeconds := int64(3600)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultControllerDeploymentName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  defaultControllerDeploymentName,
						Image: cfg.WorkerImage,
						Command: []string{
							"ate-ant",
							"controller",
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: defaultSecretName,
								},
							},
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "ateapi-token",
							MountPath: "/var/run/secrets/tokens",
							ReadOnly:  true,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "ateapi-token",
						VolumeSource: corev1.VolumeSource{
							Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{
									ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
										Audience:          "api.ate-system.svc",
										ExpirationSeconds: &expirationSeconds,
										Path:              "ateapi-token",
									},
								}},
							},
						},
					}},
				},
			},
		},
	}
}

// WriteActorTemplate writes a single Substrate ActorTemplate as protojson-shaped YAML.
func WriteActorTemplate(w io.Writer, tmpl *ateapipb.ActorTemplate) error {
	opts := protojson.MarshalOptions{
		UseProtoNames: false,
	}
	jsonBytes, err := opts.Marshal(tmpl)
	if err != nil {
		return fmt.Errorf("encoding actor template json: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return fmt.Errorf("decoding actor template json: %w", err)
	}
	delete(m, "status")
	prune(m)
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding actor template yaml: %w", err)
	}
	_, err = w.Write(data)
	return err
}

// WriteManifests writes the Kubernetes objects as a multi-document YAML stream.
func WriteManifests(w io.Writer, objs []any) error {
	for i, obj := range objs {
		var jsonBytes []byte
		var err error
		if m, ok := obj.(proto.Message); ok {
			jsonBytes, err = protojson.Marshal(m)
		} else {
			jsonBytes, err = json.Marshal(obj)
		}
		if err != nil {
			return fmt.Errorf("encoding json: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(jsonBytes, &m); err != nil {
			return fmt.Errorf("decoding json: %w", err)
		}
		delete(m, "status")
		prune(m)
		data, err := yaml.Marshal(m)
		if err != nil {
			return fmt.Errorf("encoding manifest: %w", err)
		}
		if i > 0 {
			if _, err := io.WriteString(w, "---\n"); err != nil {
				return err
			}
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func prune(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			cleaned := prune(child)
			if cleaned == nil {
				delete(val, k)
				continue
			}
			if m, ok := cleaned.(map[string]any); ok && len(m) == 0 {
				delete(val, k)
				continue
			}
			val[k] = cleaned
		}
		if len(val) == 0 {
			return nil
		}
		return val
	case []any:
		for i, elem := range val {
			val[i] = prune(elem)
		}
		return val
	default:
		return val
	}
}
