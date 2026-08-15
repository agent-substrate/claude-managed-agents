package main

import (
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCommand() *cobra.Command {
	var cfg worker.Config

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the HTTP worker daemon inside a Substrate actor sandbox",
		Long: `Worker runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /work with {session_id}, it forks:
ant beta:worker run.
Once it returns with a response, it suspends the actor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := worker.NewWorker(cfg)
			if err != nil {
				return err
			}
			defer w.Close()
			return w.Run()
		},
	}

	cmd.Flags().StringVar(&cfg.Addr, "addr", worker.DefaultWorkerAddr, "HTTP listen address")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", worker.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.Namespace, "namespace", worker.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	cmd.Flags().BoolVar(&cfg.Insecure, "insecure", false, "connect to ateapi without TLS")
	return cmd
}
