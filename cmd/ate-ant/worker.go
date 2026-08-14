package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-substrate/anthropic/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCommand() *cobra.Command {
	cfg := worker.Config{}

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the Anthropic worker frontend server on Agent Substrate",
		Long: `Worker runs the Anthropic Managed Agents self-hosted sandbox frontend server.
It long-polls Anthropic for work items, resumes or creates Substrate actors
for claimed sessions, runs the Anthropic tool executor within the actor sandbox,
and suspends the actor back to snapshot storage when the turn finishes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			server, err := worker.NewServer(cfg)
			if err != nil {
				return err
			}
			defer server.Close()

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				select {
				case <-sigChan:
					cancel()
				case <-ctx.Done():
				}
			}()

			return server.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID")
	cmd.Flags().StringVar(&cfg.EnvironmentKey, "anthropic-env-key", "", "Anthropic environment key")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", worker.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.Namespace, "namespace", worker.DefaultNamespace, "Substrate atespace / namespace")
	cmd.Flags().StringVar(&cfg.Template, "template", worker.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.Workdir, "workdir", worker.DefaultWorkdir, "Working directory for tool execution")
	cmd.Flags().StringVar(&cfg.WorkerID, "worker-id", "", "Unique worker identifier")
	cmd.Flags().BoolVar(&cfg.Insecure, "insecure", false, "Use plaintext gRPC connection to ateapi")

	return cmd
}
