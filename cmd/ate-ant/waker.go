package main

import (
	"github.com/agent-substrate/anthropic/internal/worker"
	"github.com/spf13/cobra"
)

func newWakerCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "waker",
		Short: "Run the HTTP waker daemon inside a Substrate actor sandbox",
		Long: `Waker runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /dispatch with {work_id, session_id}, it forks:
ant beta:worker run --work-id <work_id> --session-id <session_id>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return worker.NewWaker(nil).Run(addr)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", worker.DefaultWakerAddr, "HTTP listen address")
	return cmd
}
