// Command ate-antworker provides CLI utilities to generate deployment manifests
// and manage Anthropic workers on Agent Substrate.
package main

import (
	"fmt"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
	"github.com/valkey-io/valkey-go"
)

var version = "0.1.0"

func newRootCommand() *cobra.Command {
	var (
		cfg worker.Config
	)

	root := &cobra.Command{
		Use:           "ate-antworker",
		Short:         "Dispatch Claude Managed Agents work items onto Agent Substrate Actors",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			var sessionID string
			if sessionID == "" {
				sessionID = os.Getenv("ANTHROPIC_SESSION_ID")
			}
			if sessionID == "" {
				sessionID = os.Getenv("ANTHROPIC_WORK_ID")
			}
			if sessionID == "" {
				return fmt.Errorf("session ID is required")
			}

			vc, err := newValKeyClient()
			if err != nil {
				return fmt.Errorf("failed to create Valkey client: %v", err)
			}
			defer vc.Close()

			cfg.ValkeyClient = vc
			cfg.Defaults()
			dispatcher, err := worker.NewDispatcher(ctx, cfg, sessionID)
			if err != nil {
				return fmt.Errorf("failed to create a dispatcher: %v", err)
			}
			defer dispatcher.Close()

			return dispatcher.Run(ctx)
		},
	}

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

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ate-antworker: %v\n", err)
		os.Exit(1)
	}
}

func newValKeyClient() (valkey.Client, error) {
	valkeyAddr := os.Getenv("VALKEY_ADDR")
	if valkeyAddr == "" {
		valkeyAddr = "valkey.ate-antworker.svc.cluster.local:6379"
	}
	vc, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{valkeyAddr},
		Password:     os.Getenv("VALKEY_PASSWORD"),
		DisableCache: true,
	})
	if err != nil {
		return nil, fmt.Errorf("valkey NewClient failed at %s: %w", valkeyAddr, err)
	}
	return vc, nil
}
