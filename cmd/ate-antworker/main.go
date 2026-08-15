// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Deploy and manage Anthropic workers on Agent Substrate",
		Long:          `ate-antworker queues Anthropic work items and runs them on Substrate actors.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newManifestCommand())
	root.AddCommand(newWorkerCommand())
	root.AddCommand(newDispatchCommand())
	root.AddCommand(newConsumeCommand())

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
