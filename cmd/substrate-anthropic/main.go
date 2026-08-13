// Command substrate-anthropic provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0"

func main() {
	root := &cobra.Command{
		Use:           "substrate-anthropic",
		Short:         "Deploy and manage Anthropic workers on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newDeployCommand())

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("substrate-anthropic version %s\n", version)
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "substrate-anthropic: %v\n", err)
		os.Exit(1)
	}
}
