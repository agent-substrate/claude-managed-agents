package main

import (
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCommand() *cobra.Command {
	var cfg worker.WorkerConfig

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the HTTP worker daemon inside a Substrate actor sandbox",
		Long: `Worker runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /work with {session_id}, it forks:
ant beta:worker run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := worker.NewWorker(cfg)
			if err != nil {
				return err
			}
			return w.Run()
		},
	}
	cmd.Flags().StringVar(&cfg.Addr, "addr", worker.DefaultWorkerAddr, "HTTP listen address")
	return cmd
}
