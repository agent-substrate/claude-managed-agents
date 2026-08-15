// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	var req worker.DispatchRequest
	var rcfg redisConfig

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Deploy and manage Anthropic workers on Agent Substrate",
		Long:          `ate-antworker queues Anthropic work items and runs them on Substrate actors.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueue(cmd.Context(), req, rcfg)
		},
	}

	registerRedisFlags(root, &rcfg)
	root.Flags().StringVar(&req.WorkID, "work-id", "", "Anthropic work ID (or $ANTHROPIC_WORK_ID)")
	root.Flags().StringVar(&req.SessionID, "session-id", "", "Anthropic session ID (or $ANTHROPIC_SESSION_ID)")

	root.AddCommand(newManifestCommand())
	root.AddCommand(newWorkerCommand())
	root.AddCommand(newQueueCommand())
	root.AddCommand(newDispatchCommand())

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("ate-antworker version %s\n", version)
		},
	})

	return root
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ate-antworker: %v\n", err)
		os.Exit(1)
	}
}
