// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	var (
		cfg       worker.Config
		workID    string
		sessionID string
	)

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Deploy and manage Anthropic workers on Agent Substrate",
		Long:          `ate-antworker executes Anthropic work items on Substrate actors.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDispatch(cmd.Context(), cfg, workID, sessionID)
		},
	}

	root.Flags().StringVar(&workID, "work-id", "", "Anthropic work ID")
	root.Flags().StringVar(&sessionID, "session-id", "", "Anthropic session ID")
	root.Flags().StringVar(&cfg.Namespace, "namespace", worker.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	root.Flags().StringVar(&cfg.Template, "template", worker.DefaultTemplate, "Substrate ActorTemplate name")
	root.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", worker.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	root.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", worker.DefaultAtenetRouterAddr, "Substrate atenet router endpoint")
	root.Flags().StringVar(&cfg.EnvironmentID, "environment-id", "", "Anthropic environment ID")
	root.Flags().StringVar(&cfg.EnvironmentKey, "environment-key", "", "Anthropic environment key")
	root.Flags().BoolVar(&cfg.Insecure, "insecure", false, "connect to ateapi without TLS")

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

func runDispatch(ctx context.Context, cfg worker.Config, workID, sessionID string) error {
	if sessionID == "" {
		sessionID = os.Getenv("ANTHROPIC_SESSION_ID")
	}
	if sessionID == "" {
		sessionID = os.Getenv("ANTHROPIC_WORK_ID")
	}
	if sessionID == "" {
		sessionID = workID
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required (set ANTHROPIC_SESSION_ID / ANTHROPIC_WORK_ID or pass --session-id)")
	}

	cfg.Defaults()
	return worker.DispatchWork(ctx, cfg, sessionID)
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ate-antworker: %v\n", err)
		os.Exit(1)
	}
}
