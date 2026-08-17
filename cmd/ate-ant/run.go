package main

import (
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newRunCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the HTTP server inside a Substrate actor sandbox",
		Long: `Run runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /work with {session_id}, it forks:
ant beta:worker run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w, err := worker.NewWorker(worker.WorkerConfig{
				Addr: addr,
			})
			if err != nil {
				return err
			}
			return w.Run()
		},
	}
	cmd.Flags().StringVar(&addr, "addr", worker.DefaultWorkerAddr, "HTTP listen address")
	return cmd
}
