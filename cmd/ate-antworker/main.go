// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	cfg := worker.Config{}
	var req worker.DispatchRequest

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Deploy and manage Anthropic workers on Agent Substrate",
		Long: `ate-antworker dispatches Anthropic work items to Substrate actors.
When executed without subcommands, it serves the dispatch command (for 'ant beta:worker poll --on-work ate-antworker').`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDispatch(cmd.Context(), cfg, req)
		},
	}

	registerDispatchFlags(root, &cfg, &req)

	root.AddCommand(newManifestCommand())
	root.AddCommand(newWorkerCommand())
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
	if len(os.Args) > 0 && filepath.Base(os.Args[0]) == "ate-antworker-dispatch" {
		cmd := newDispatchCommand()
		if err := cmd.Execute(); err != nil {
			fmt.Fprintf(os.Stderr, "ate-antworker-dispatch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	root := newRootCommand()
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ate-antworker: %v\n", err)
		os.Exit(1)
	}
}
