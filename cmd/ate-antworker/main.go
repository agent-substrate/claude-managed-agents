// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/poller"
	"github.com/agent-substrate/claude-managed-agents/internal/sessionmanager"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	var cfg poller.Config

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Dispatch Claude Managed Agents work items onto Agent Substrate Actors",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg.Defaults()
			sessionManager, err := sessionmanager.New(sessionmanager.Options{
				Logger: cfg.Logger,
			})
			if err != nil {
				return fmt.Errorf("creating session manager: %w", err)
			}
			defer sessionManager.Close()

			p, err := poller.New(cfg)
			if err != nil {
				return fmt.Errorf("creating poller: %w", err)
			}
			return p.Poll(ctx, func(ctx context.Context, item *anthropic.BetaSelfHostedWork) error {
				sessID := item.Data.ID
				workID := item.ID
				_, err := sessionManager.QueueResume(ctx, sessID, workID)
				return err
			})
		},
	}

	root.AddCommand(newManifestCommand())
	root.AddCommand(newWorkerCommand())
	root.AddCommand(newConsumerCommand())
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
