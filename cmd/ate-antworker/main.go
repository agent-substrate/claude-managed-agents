// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/life"
	"github.com/agent-substrate/claude-managed-agents/internal/poller"
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	var (
		cfg              worker.Config
		template         string
		namespace        string
		ateapiAddr       string
		atenetRouterAddr string
		insecure         bool
	)

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Dispatch Claude Managed Agents work items onto Agent Substrate Actors",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg.Defaults()

			ateClient, conn, err := life.DialATE(ateapiAddr, insecure)
			if err != nil {
				return fmt.Errorf("dialing ateapi at %s: %w", ateapiAddr, err)
			}
			defer conn.Close()

			lifeMgr := life.NewManager(ateClient, template, namespace).
				WithRouterAddr(atenetRouterAddr).
				WithLogger(cfg.Logger)
			lifeMgr.StartGC(ctx, life.DefaultIdleCheckInterval, life.DefaultIdleThreshold)

			p, err := poller.New(poller.Config{
				EnvironmentID:  cfg.EnvironmentID,
				EnvironmentKey: cfg.EnvironmentKey,
				Logger:         cfg.Logger,
			})
			if err != nil {
				return fmt.Errorf("creating poller: %w", err)
			}
			return p.Poll(ctx, func(ctx context.Context, item *anthropic.BetaSelfHostedWork) error {
				sessID := item.Data.ID
				workID := item.ID

				dispatcher := worker.NewDispatcher(lifeMgr, cfg, sessID, workID)
				return dispatcher.Run(ctx)
			})
		},
	}

	root.Flags().StringVar(&namespace, "namespace", life.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	root.Flags().StringVar(&template, "template", life.DefaultTemplate, "Substrate ActorTemplate name")
	root.Flags().StringVar(&ateapiAddr, "ateapi-addr", life.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	root.Flags().StringVar(&atenetRouterAddr, "atenet-router-addr", life.DefaultAtenetRouterAddr, "Substrate atenet router endpoint")
	root.Flags().StringVar(&cfg.EnvironmentID, "environment-id", "", "Anthropic environment ID")
	root.Flags().StringVar(&cfg.EnvironmentKey, "environment-key", "", "Anthropic environment key")
	root.Flags().BoolVar(&insecure, "insecure", false, "connect to ateapi without TLS")

	root.AddCommand(newManifestCommand())
	root.AddCommand(newWorkerCommand())
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
