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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/spf13/cobra"
)

func newManifestCommand() *cobra.Command {
	cfg := manifest.Config{}
	var workerPools string

	cmd := &cobra.Command{
		Use:     "manifest",
		Aliases: []string{"deploy"},
		Short:   "Generate Kubernetes manifests to deploy the system",
		Long: `Manifest generates Kubernetes manifests for everything needed to run Anthropic's
self-hosted sandbox worker on a cluster with Agent Substrate installed:
the target namespace, Secret for credentials, a WorkerPool of pre-warmed workers,
and the worker daemon Deployments.

It prints YAML to stdout without touching the cluster; pipe to kubectl apply.

To generate the Substrate ActorTemplate manifest (for kubectl-ate), use
the "template" subcommand: ate-ant manifest template`,
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
			if cfg.BaseURL == "" {
				cfg.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
			}
			if workerPools != "" {
				if err := json.Unmarshal([]byte(workerPools), &cfg.WorkerPools); err != nil {
					return fmt.Errorf("parsing --worker-pools: %w", err)
				}
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
	cmd.Flags().Int32Var(&cfg.Replicas, "replicas", manifest.DefaultReplicas, "number of pre-warmed worker pool pods")
	cmd.Flags().Int32Var(&cfg.ControllerReplicas, "controller-replicas", 0, "number of controller daemon pods (defaults to --replicas)")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (e.g. env_...)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.APIKey, "anthropic-api-key", "", "Anthropic API key")
	cmd.Flags().StringVar(&cfg.BaseURL, "anthropic-base-url", "", "custom Anthropic API base URL (default $ANTHROPIC_BASE_URL, else the public API)")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet HTTP router endpoint")
	cmd.Flags().StringVar(&cfg.ValkeyAddr, "valkey-addr", manifest.DefaultValkeyAddr, "Valkey service endpoint")
	cmd.Flags().StringVar(&cfg.ValkeyImage, "valkey-image", manifest.DefaultValkeyImage, "Valkey container image")
	cmd.Flags().StringVar(&workerPools, "worker-pools", "", `JSON list of sized WorkerPools, one per group of identical worker nodes: [{"name":..., "replicas":..., "node_selector":{...}, "cpu_millicores":..., "memory_bytes":...}]. Overrides --replicas`)

	_ = cmd.MarkFlagRequired("worker-image")
	_ = cmd.MarkFlagRequired("ateom-image")

	cmd.AddCommand(newManifestTemplateCommand())

	return cmd
}

func newManifestTemplateCommand() *cobra.Command {
	cfg := manifest.Config{}

	cmd := &cobra.Command{
		Use:   "template",
		Short: "Generate Substrate ActorTemplate manifest",
		Long: `Generate Substrate ActorTemplate manifest to register with kubectl-ate.
It prints YAML to stdout without touching the cluster.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.EnvironmentID == "" {
				cfg.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
			}
			if cfg.EnvironmentKey == "" {
				cfg.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
			}
			if cfg.BaseURL == "" {
				cfg.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
			}
			if cfg.WorkerImage == "" {
				return errors.New("--worker-image is required")
			}
			if cfg.SnapshotsBucket == "" {
				return errors.New("--snapshots-bucket is required")
			}
			cfg.Defaults()
			return manifest.WriteActorTemplate(cmd.OutOrStdout(), manifest.BuildActorTemplate(cfg))
		},
	}

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace / atespace for the ActorTemplate")
	cmd.Flags().StringVar(&cfg.Template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.WorkerImage, "worker-image", "", "digest-pinned worker container image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.SnapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with prefix) for actor snapshots (e.g. gs://bucket/snapshots/)")
	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (e.g. env_...)")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.BaseURL, "anthropic-base-url", "", "custom Anthropic API base URL (default $ANTHROPIC_BASE_URL, else the public API)")
	cmd.Flags().Int64Var(&cfg.ActorCPUMillicores, "actor-cpu-millicores", manifest.DefaultActorCPUMillicores, "per-actor CPU limit")
	cmd.Flags().Int64Var(&cfg.ActorMemoryBytes, "actor-memory-bytes", manifest.DefaultActorMemoryBytes, "per-actor memory limit")

	_ = cmd.MarkFlagRequired("worker-image")
	_ = cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}
