package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// DefaultPauseImage is the digest-pinned pause image recommended for Agent Substrate.
const defaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// Default constants for names and paths.
const (
	DefaultNamespace          = "ate-ant"
	DefaultTemplate           = "ate-ant-actor-template"
	DefaultControllerReplicas = 1
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
		c.Replicas = 10
	}
	if c.ControllerReplicas == 0 {
		c.ControllerReplicas = DefaultControllerReplicas
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

// Validate ensures required fields are set before generating manifests.
func (c *Config) Validate() error {
	c.Defaults()
	if c.WorkerImage == "" {
		return errors.New("--worker-image is required")
	}
	if c.AteomImage == "" {
		return errors.New("--ateom-image is required")
	}
	if c.SnapshotsBucket == "" {
		return errors.New("--snapshots-bucket is required")
	}
	return nil
}

// BuildManifests constructs all Kubernetes and Substrate objects in apply order.
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
		BuildActorTemplate(cfg),
		BuildPollerDeployment(cfg),
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
			AteomImage:   cfg.AteomImage,
			SandboxClass: "gvisor",
		},
	}
}

// BuildActorTemplate constructs the Substrate ActorTemplate CRD.
func BuildActorTemplate(cfg Config) *atev1alpha1.ActorTemplate {
	var env []atev1alpha1.EnvVar
	if cfg.APIKey != "" {
		env = append(env, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_API_KEY",
			Value: &cfg.APIKey,
		})
	}
	if cfg.EnvironmentKey != "" {
		env = append(env, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_KEY",
			Value: &cfg.EnvironmentKey,
		})
	}
	if cfg.EnvironmentID != "" {
		env = append(env, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_ID",
			Value: &cfg.EnvironmentID,
		})
	}

	return &atev1alpha1.ActorTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: atev1alpha1.GroupVersion.String(),
			Kind:       "ActorTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Template,
			Namespace: cfg.Namespace,
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"workload": cfg.Template,
				},
			},
			Containers: []atev1alpha1.Container{{
				Name:    "worker",
				Image:   cfg.WorkerImage,
				Command: []string{"ate-ant", "run"},
				Env:     env,
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet: &atev1alpha1.HTTPGetAction{
						Path: "/healthz",
						Port: 80,
					},
				},
			}},
			SnapshotsConfig: atev1alpha1.SnapshotsConfig{
				Location: cfg.SnapshotsBucket,
			},
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

// WriteManifests writes the Kubernetes objects as a multi-document YAML stream.
func WriteManifests(w io.Writer, objs []any) error {
	for i, obj := range objs {
		jsonBytes, err := json.Marshal(obj)
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
