package main

import (
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/spf13/cobra"
)

func newManifestCommand() *cobra.Command {
	cfg := manifest.Config{}

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
			return manifest.WriteManifests(cmd.OutOrStdout(), manifest.BuildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&cfg.Template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "digest-pinned worker container image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.AteomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool (e.g. ateom-gvisor)")
	cmd.Flags().StringVar(&cfg.SnapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with prefix) for actor snapshots (e.g. gs://bucket/snapshots/)")
	cmd.Flags().Int32Var(&cfg.Replicas, "replicas", manifest.DefaultReplicas, "number of pre-warmed worker pool pods")
	cmd.Flags().Int32Var(&cfg.ControllerReplicas, "controller-replicas", manifest.DefaultControllerReplicas, "number of controller daemon pods")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (e.g. env_...)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.APIKey, "anthropic-api-key", "", "Anthropic API key")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet HTTP router endpoint")
	cmd.Flags().StringVar(&cfg.ValkeyAddr, "valkey-addr", manifest.DefaultValkeyAddr, "Valkey service endpoint")
	cmd.Flags().StringVar(&cfg.ValkeyImage, "valkey-image", manifest.DefaultValkeyImage, "Valkey container image")

	_ = cmd.MarkFlagRequired("worker-image")
	_ = cmd.MarkFlagRequired("ateom-image")
	_ = cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}
