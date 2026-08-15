package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// DefaultPauseImage is the digest-pinned pause image recommended for Agent Substrate.
const DefaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// Default constants for names and paths.
const (
	DefaultNamespace        = "ate-antworker"
	DefaultTemplate         = "ate-antworker-actor-template"
	DefaultWorkerPool       = "ate-antworker-workerpool"
	DefaultDeploymentName   = "ate-antworker"
	DefaultConsumerName     = "ate-antworker-consumer"
	DefaultRedisName        = "ate-antworker-redis"
	DefaultSecretName       = "ate-antworker-creds"
	DefaultWorkdir          = "/workspace"
	DefaultAteapiAddr       = "api.ate-system.svc.cluster.local:443"
	DefaultAtenetRouterAddr = "atenet-router.ate-system.svc.cluster.local:80"
	DefaultRedisImage       = "redis:7.4-alpine"
	DefaultRedisStorage     = "5Gi"
	DefaultRedisPort        = 6379
)

// Config defines the parameters for generating Substrate and Kubernetes deployment manifests.
type Config struct {
	Namespace        string `json:"namespace"`
	Template         string `json:"template"`
	WorkerPool       string `json:"worker_pool"`
	WorkerImage      string `json:"worker_image"`
	AteomImage       string `json:"ateom_image"`
	PauseImage       string `json:"pause_image"`
	SnapshotsBucket  string `json:"snapshots_bucket"`
	Replicas         int32  `json:"replicas"`
	WorkerReplicas   int32  `json:"worker_replicas"`
	EnvironmentID    string `json:"environment_id"`
	EnvironmentKey   string `json:"environment_key"`
	APIKey           string `json:"api_key"`
	Workdir          string `json:"workdir"`
	AteapiAddr       string `json:"ateapi_addr"`
	AtenetRouterAddr string `json:"atenet_router_addr"`

	// RedisAddr points the queue at an existing Redis. When empty, a
	// single-replica Redis is generated alongside the workers.
	RedisAddr     string `json:"redis_addr"`
	RedisPassword string `json:"redis_password"`
	RedisImage    string `json:"redis_image"`
	RedisStorage  string `json:"redis_storage"`
	QueuePrefix   string `json:"queue_prefix"`

	ConsumerReplicas int32 `json:"consumer_replicas"`
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
		c.WorkerPool = DefaultWorkerPool
	}
	if c.PauseImage == "" {
		c.PauseImage = DefaultPauseImage
	}
	if c.Replicas == 0 {
		c.Replicas = 10
	}
	if c.WorkerReplicas == 0 {
		c.WorkerReplicas = 1
	}
	if c.Workdir == "" {
		c.Workdir = DefaultWorkdir
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
	}
	if c.AtenetRouterAddr == "" {
		c.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if c.RedisImage == "" {
		c.RedisImage = DefaultRedisImage
	}
	if c.RedisStorage == "" {
		c.RedisStorage = DefaultRedisStorage
	}
	if c.QueuePrefix == "" {
		c.QueuePrefix = queue.DefaultPrefix
	}
	if c.ConsumerReplicas == 0 {
		// Each consumer runs one work item at a time, so replicas are the
		// only concurrency dial. Stay under the pre-warmed worker pool.
		c.ConsumerReplicas = 8
	}
}

// BundlesRedis reports whether the manifests include a Redis of their own,
// which they do unless the queue points at an existing one.
func (c *Config) BundlesRedis() bool { return c.RedisAddr == "" }

// QueueAddr returns the Redis endpoint the poller and consumers connect to.
func (c *Config) QueueAddr() string {
	if c.RedisAddr != "" {
		return c.RedisAddr
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", DefaultRedisName, c.Namespace, DefaultRedisPort)
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

func newManifestCommand() *cobra.Command {
	cfg := Config{}

	cmd := &cobra.Command{
		Use:     "manifest",
		Aliases: []string{"deploy"},
		Short:   "Generate Kubernetes and Agent Substrate manifests",
		Long: `Manifest generates Kubernetes manifests for everything needed to run Anthropic's
self-hosted sandbox worker on a cluster with Agent Substrate installed:
the target namespace, Secret for credentials, a WorkerPool of pre-warmed workers,
the ActorTemplate for sandboxed execution, and the worker daemon Deployment.

It prints YAML to stdout without touching the cluster; pipe to kubectl apply.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.EnvironmentID == "" {
				cfg.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
			}
			if cfg.EnvironmentKey == "" {
				cfg.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
			}
			if cfg.APIKey == "" {
				cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			return WriteManifests(cmd.OutOrStdout(), BuildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&cfg.Template, "template", DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.WorkerPool, "workerpool", "", "Substrate WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "digest-pinned worker container image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.AteomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool (e.g. ateom-gvisor)")
	cmd.Flags().StringVar(&cfg.PauseImage, "pause-image", DefaultPauseImage, "digest-pinned pause image for the sandbox container")
	cmd.Flags().StringVar(&cfg.SnapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with prefix) for actor snapshots (e.g. gs://bucket/snapshots/)")
	cmd.Flags().Int32Var(&cfg.Replicas, "replicas", 10, "number of pre-warmed worker pool pods")
	cmd.Flags().Int32Var(&cfg.WorkerReplicas, "worker-replicas", 1, "number of worker daemon deployment replicas")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (e.g. env_...)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.APIKey, "anthropic-api-key", "", "Anthropic API key")
	cmd.Flags().StringVar(&cfg.Workdir, "workdir", DefaultWorkdir, "working directory for tool execution")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", DefaultAtenetRouterAddr, "Substrate atenet HTTP router endpoint")
	cmd.Flags().StringVar(&cfg.RedisAddr, "redis-addr", "", "existing Redis host:port for the work queue (default: generate a single-replica Redis)")
	cmd.Flags().StringVar(&cfg.RedisPassword, "redis-password", "", "Redis password, stored in the credentials Secret")
	cmd.Flags().StringVar(&cfg.RedisImage, "redis-image", DefaultRedisImage, "container image for the generated Redis")
	cmd.Flags().StringVar(&cfg.RedisStorage, "redis-storage", DefaultRedisStorage, "persistent volume size for the generated Redis")
	cmd.Flags().StringVar(&cfg.QueuePrefix, "queue-prefix", queue.DefaultPrefix, "prefix for the queue's Redis keys")
	cmd.Flags().Int32Var(&cfg.ConsumerReplicas, "consumer-replicas", 8, "number of queue consumer replicas; each runs one work item at a time")

	_ = cmd.MarkFlagRequired("worker-image")
	_ = cmd.MarkFlagRequired("ateom-image")
	_ = cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}

// BuildManifests constructs all Kubernetes and Substrate objects in apply order.
func BuildManifests(cfg Config) []any {
	cfg.Defaults()

	objs := []any{
		BuildNamespace(cfg),
		BuildSecret(cfg),
	}
	if cfg.BundlesRedis() {
		objs = append(objs, BuildRedisService(cfg), BuildRedisStatefulSet(cfg))
	}
	return append(objs,
		BuildWorkerPool(cfg),
		BuildActorTemplate(cfg),
		BuildWorkerDeployment(cfg),
		BuildConsumerDeployment(cfg),
	)
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
		"ANTHROPIC_ENVIRONMENT_ID": cfg.EnvironmentID,
	}
	if cfg.EnvironmentKey != "" {
		stringData["ANTHROPIC_ENVIRONMENT_KEY"] = cfg.EnvironmentKey
	}
	if cfg.APIKey != "" {
		stringData["ANTHROPIC_API_KEY"] = cfg.APIKey
	}
	if cfg.RedisPassword != "" {
		stringData["REDIS_PASSWORD"] = cfg.RedisPassword
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
			Labels: map[string]string{
				"workload": cfg.Template,
			},
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:   cfg.Replicas,
			AteomImage: cfg.AteomImage,
		},
	}
}

// BuildActorTemplate constructs the Substrate ActorTemplate CRD.
func BuildActorTemplate(cfg Config) *atev1alpha1.ActorTemplate {
	var envVars []atev1alpha1.EnvVar
	if cfg.EnvironmentID != "" {
		envVars = append(envVars, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_ID",
			Value: &cfg.EnvironmentID,
		})
	}
	if cfg.EnvironmentKey != "" {
		envVars = append(envVars, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_ENVIRONMENT_KEY",
			Value: &cfg.EnvironmentKey,
		})
	}
	if cfg.APIKey != "" {
		envVars = append(envVars, atev1alpha1.EnvVar{
			Name:  "ANTHROPIC_API_KEY",
			Value: &cfg.APIKey,
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
			PauseImage: cfg.PauseImage,
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"workload": cfg.Template,
				},
			},
			Containers: []atev1alpha1.Container{{
				Name:    "worker",
				Image:   cfg.WorkerImage,
				Command: []string{"ate-antworker", "worker"},
				Env:     envVars,
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

// BuildRedisService constructs the Service fronting the bundled Redis.
func BuildRedisService(cfg Config) *corev1.Service {
	labels := map[string]string{"app": DefaultRedisName}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultRedisName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       "redis",
				Port:       DefaultRedisPort,
				TargetPort: intstr.FromInt32(DefaultRedisPort),
			}},
		},
	}
}

// BuildRedisStatefulSet constructs the single-replica Redis backing the durable
// work queue. Append-only persistence is enabled so queued work items survive a
// restart of the pod.
//
// One replica is enough to make the queue durable, but it is not highly
// available: point --redis-addr at a managed or replicated Redis if the queue
// must survive losing a node.
func BuildRedisStatefulSet(cfg Config) *appsv1.StatefulSet {
	labels := map[string]string{"app": DefaultRedisName}
	replicas := int32(1)

	args := []string{
		"redis-server",
		"--appendonly", "yes",
		"--dir", "/data",
	}
	var env []corev1.EnvVar
	if cfg.RedisPassword != "" {
		// Kubernetes expands $(REDIS_PASSWORD) from the container's env.
		args = append(args, "--requirepass", "$(REDIS_PASSWORD)")
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: DefaultSecretName},
					Key:                  "REDIS_PASSWORD",
				},
			},
		})
	}

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultRedisName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: DefaultRedisName,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "redis",
						Image: cfg.RedisImage,
						Args:  args,
						Env:   env,
						Ports: []corev1.ContainerPort{{
							Name:          "redis",
							ContainerPort: DefaultRedisPort,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt32(DefaultRedisPort),
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/data",
						}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(cfg.RedisStorage),
						},
					},
				},
			}},
		},
	}
}

// BuildConsumerDeployment constructs the Deployment that drains the queue: it
// resumes the actor for each queued work item, runs it, and suspends the actor.
func BuildConsumerDeployment(cfg Config) *appsv1.Deployment {
	labels := map[string]string{"app": DefaultConsumerName}
	replicas := cfg.ConsumerReplicas

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConsumerName,
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
						Name:  DefaultConsumerName,
						Image: cfg.WorkerImage,
						Command: []string{
							"ate-antworker", "consume",
							"--redis-addr", cfg.QueueAddr(),
							"--queue-prefix", cfg.QueuePrefix,
							"--ateapi-addr", cfg.AteapiAddr,
							"--atenet-router-addr", cfg.AtenetRouterAddr,
							"--namespace", cfg.Namespace,
							"--template", cfg.Template,
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: DefaultSecretName,
								},
							},
						}},
					}},
				},
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
						Command: []string{
							"ant", "beta:worker", "poll",
							// The hook only queues the work item; the consumer
							// Deployment resumes the actor and runs the work item.
							"--on-work", fmt.Sprintf("ate-antworker dispatch --redis-addr %s --queue-prefix %s",
								cfg.QueueAddr(), cfg.QueuePrefix),
						},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: DefaultSecretName,
								},
							},
						}},
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
