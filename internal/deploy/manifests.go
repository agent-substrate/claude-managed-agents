// Package deploy generates Kubernetes manifests and Agent Substrate CRDs for Anthropic worker deployments.
package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// DefaultPauseImage is the digest-pinned pause image recommended for Agent Substrate.
const DefaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// DefaultWorkerCommand is the default entrypoint for the prebuilt ant worker container.
const DefaultWorkerCommand = "ant"

// Default constants for names and namespaces.
const (
	DefaultNamespace      = "anthropic-worker"
	DefaultTemplate       = "anthropic-worker-template"
	DefaultWorkerPool     = "anthropic-worker-template-workerpool"
	DefaultDeploymentName = "anthropic-worker"
	DefaultSecretName     = "anthropic-environment-creds"
	DefaultServiceName    = "anthropic-worker"
	DefaultPort           = 8080
	DefaultWorkdir        = "/workspace"
)

// Config defines the parameters for generating Substrate and Kubernetes deployment manifests.
type Config struct {
	Namespace         string            `json:"namespace"`
	Template          string            `json:"template"`
	WorkerPool        string            `json:"worker_pool"`
	WorkerImage       string            `json:"worker_image"`
	AteomImage        string            `json:"ateom_image"`
	PauseImage        string            `json:"pause_image"`
	SnapshotsBucket   string            `json:"snapshots_bucket"`
	Replicas          int32             `json:"replicas"`
	WorkerReplicas    int32             `json:"worker_replicas"`
	Port              int32             `json:"port"`
	WorkerCommand     []string          `json:"worker_command"`
	PoolLabels        map[string]string `json:"pool_labels"`
	EnvironmentID     string            `json:"environment_id"`
	EnvironmentKey    string            `json:"environment_key"`
	WebhookSigningKey string            `json:"webhook_signing_key"`
	Workdir           string            `json:"workdir"`
	WorkerMode        string            `json:"worker_mode"`
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if c.WorkerPool == "" {
		c.WorkerPool = c.Template + "-workerpool"
	}
	if c.PauseImage == "" {
		c.PauseImage = DefaultPauseImage
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.Replicas == 0 {
		c.Replicas = 3
	}
	if c.WorkerReplicas == 0 {
		c.WorkerReplicas = 1
	}
	if len(c.WorkerCommand) == 0 {
		c.WorkerCommand = []string{"ant", "beta:worker", "poll", "--workdir", c.Workdir}
	}
	if c.PoolLabels == nil {
		c.PoolLabels = map[string]string{"workload": c.Template}
	}
	if c.Workdir == "" {
		c.Workdir = DefaultWorkdir
	}
	if c.WorkerMode == "" {
		c.WorkerMode = "poll"
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
	objs := []any{
		BuildNamespace(cfg),
		BuildSecret(cfg),
		BuildWorkerPool(cfg),
		BuildActorTemplate(cfg),
		BuildWorkerDeployment(cfg),
		BuildWorkerService(cfg),
	}
	return objs
}

// BuildNamespace constructs the corev1.Namespace manifest.
func BuildNamespace(cfg Config) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace},
	}
}

// BuildSecret constructs the corev1.Secret containing Anthropic environment credentials.
func BuildSecret(cfg Config) *corev1.Secret {
	stringData := map[string]string{
		"ANTHROPIC_ENVIRONMENT_ID":  cfg.EnvironmentID,
		"ANTHROPIC_ENVIRONMENT_KEY": cfg.EnvironmentKey,
	}
	if cfg.WebhookSigningKey != "" {
		stringData["ANTHROPIC_WEBHOOK_SIGNING_KEY"] = cfg.WebhookSigningKey
	}

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultSecretName,
			Namespace: cfg.Namespace,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: stringData,
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
			Name:      cfg.WorkerPool,
			Namespace: cfg.Namespace,
			Labels:    cfg.PoolLabels,
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:   cfg.Replicas,
			AteomImage: cfg.AteomImage,
		},
	}
}

// BuildActorTemplate constructs the Substrate ActorTemplate CRD.
func BuildActorTemplate(cfg Config) *atev1alpha1.ActorTemplate {
	portStr := fmt.Sprintf("%d", cfg.Port)
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
			PauseImage: cfg.PauseImage,
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: cfg.PoolLabels,
			},
			Containers: []atev1alpha1.Container{{
				Name:    "worker",
				Image:   cfg.WorkerImage,
				Command: cfg.WorkerCommand,
				Env: []atev1alpha1.EnvVar{
					{
						Name:  "PORT",
						Value: &portStr,
					},
					{
						Name:  "WORK_DIR",
						Value: &cfg.Workdir,
					},
					{
						Name:  "WORKER_MODE",
						Value: &cfg.WorkerMode,
					},
				},
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet: &atev1alpha1.HTTPGetAction{
						Path: "/readyz",
						Port: cfg.Port,
					},
				},
			}},
			SnapshotsConfig: atev1alpha1.SnapshotsConfig{
				Location: cfg.SnapshotsBucket,
			},
		},
	}
}

// BuildWorkerDeployment constructs the Kubernetes Deployment for the worker daemon.
func BuildWorkerDeployment(cfg Config) *appsv1.Deployment {
	labels := map[string]string{"app": DefaultDeploymentName}
	replicas := cfg.WorkerReplicas

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultDeploymentName,
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
						Name:  DefaultDeploymentName,
						Image: cfg.WorkerImage,
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: DefaultSecretName,
								},
							},
						}},
						Env: []corev1.EnvVar{
							{
								Name:  "PORT",
								Value: fmt.Sprintf("%d", cfg.Port),
							},
							{
								Name:  "WORK_DIR",
								Value: cfg.Workdir,
							},
							{
								Name:  "WORKER_MODE",
								Value: cfg.WorkerMode,
							},
						},
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: cfg.Port,
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(cfg.Port),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstr.FromInt32(cfg.Port),
								},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       5,
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "workspace",
							MountPath: cfg.Workdir,
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "workspace",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}
}

// BuildWorkerService constructs the Kubernetes Service for health/webhook endpoints.
func BuildWorkerService(cfg Config) *corev1.Service {
	labels := map[string]string{"app": DefaultDeploymentName}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultServiceName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       cfg.Port,
				TargetPort: intstr.FromInt32(cfg.Port),
			}},
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
