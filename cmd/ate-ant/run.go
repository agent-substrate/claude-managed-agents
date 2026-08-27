package main

import (
	"github.com/agent-substrate/claude-managed-agents/internal/runner"
	"github.com/spf13/cobra"
)

func newRunCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the HTTP server inside a Substrate actor sandbox",
		Long: `Run runs a lightweight HTTP server inside a Substrate actor container.
When it receives POST /work with {session_id, work_id, last_heartbeat}, it
services the work item in-process: it runs the session's tool calls and
continues the work-item lease from the given heartbeat token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := runner.NewRunner(runner.RunnerConfig{
				Addr: addr,
			})
			if err != nil {
				return err
			}
			return r.Run()
		},
	}
	cmd.Flags().StringVar(&addr, "addr", runner.DefaultRunnerAddr, "HTTP listen address")
	return cmd
}
