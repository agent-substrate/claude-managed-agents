package main

import (
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newWorkerCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the HTTP worker daemon inside a Substrate actor sandbox",
		Long: `Worker runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /dispatch with {work_id, session_id}, it forks:
ant beta:worker run --work-id <work_id> --session-id <session_id>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return worker.NewWorker(nil).Run(addr)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", worker.DefaultWorkerAddr, "HTTP listen address")
	return cmd
}
