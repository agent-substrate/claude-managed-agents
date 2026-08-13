package main

import (
	"github.com/agent-substrate/substrate-anthropic/internal/deploy"
	"github.com/spf13/cobra"
)

func newDeployCommand() *cobra.Command {
	cfg := deploy.Config{}

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Generate Kubernetes and Agent Substrate manifests for deployment",
		Long: `Deploy generates Kubernetes manifests for everything needed to run Anthropic's
self-hosted sandbox worker on a cluster with Agent Substrate installed:
the target namespace, Secret for credentials, a WorkerPool of pre-warmed workers,
the ActorTemplate for sandboxed execution, and the worker daemon Deployment and Service.

It prints YAML to stdout without touching the cluster; pipe to kubectl apply.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			return deploy.WriteManifests(cmd.OutOrStdout(), deploy.BuildManifests(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", deploy.DefaultNamespace, "Kubernetes namespace to deploy into")
	cmd.Flags().StringVar(&cfg.Template, "template", deploy.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.WorkerPool, "workerpool", "", "Substrate WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "digest-pinned worker container image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.AteomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool (e.g. ateom-gvisor)")
	cmd.Flags().StringVar(&cfg.PauseImage, "pause-image", deploy.DefaultPauseImage, "digest-pinned pause image for the sandbox container")
	cmd.Flags().StringVar(&cfg.SnapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with prefix) for actor snapshots (e.g. gs://bucket/snapshots/)")
	cmd.Flags().Int32Var(&cfg.Replicas, "replicas", 3, "number of pre-warmed worker pool pods")
	cmd.Flags().Int32Var(&cfg.WorkerReplicas, "worker-replicas", 1, "number of worker daemon deployment replicas")
	cmd.Flags().Int32Var(&cfg.Port, "port", deploy.DefaultPort, "port for health probes and webhooks")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (e.g. env_...)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.WebhookSigningKey, "anthropic-webhook-signing-key", "", "Anthropic webhook signing key (optional)")
	cmd.Flags().StringVar(&cfg.Workdir, "workdir", deploy.DefaultWorkdir, "working directory for tool execution")
	cmd.Flags().StringVar(&cfg.WorkerMode, "worker-mode", "poll", "worker mode: poll, webhook, or hybrid")
	cmd.Flags().StringSliceVar(&cfg.WorkerCommand, "worker-command", nil, "worker container entrypoint (defaults to ant beta:worker poll --workdir <workdir>)")

	_ = cmd.MarkFlagRequired("worker-image")
	_ = cmd.MarkFlagRequired("ateom-image")
	_ = cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}
