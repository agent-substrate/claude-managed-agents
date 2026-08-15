package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func registerDispatchFlags(cmd *cobra.Command, cfg *worker.Config, req *worker.DispatchRequest) {
	cmd.Flags().StringVar(&req.WorkID, "work-id", "", "Anthropic work ID (or $ANTHROPIC_WORK_ID)")
	cmd.Flags().StringVar(&req.SessionID, "session-id", "", "Anthropic session ID (or $ANTHROPIC_SESSION_ID)")
	cmd.Flags().BoolVar(&req.Wait, "wait", true, "Wait for turn completion and automatically suspend the actor")
	cmd.Flags().DurationVar(&req.IdleTimeout, "idle-timeout", worker.DefaultIdleTimeout, "Max idle duration before considering turn complete")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", worker.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", worker.DefaultAtenetRouterAddr, "Substrate atenet HTTP router endpoint")
	cmd.Flags().StringVar(&cfg.Namespace, "namespace", worker.DefaultNamespace, "Substrate atespace / namespace")
	cmd.Flags().StringVar(&cfg.Template, "template", worker.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().BoolVar(&cfg.Insecure, "insecure", false, "Use plaintext gRPC connection to ateapi")
}

func runDispatch(ctx context.Context, cfg worker.Config, req worker.DispatchRequest) error {
	// 1. Read from environment variables if not passed via flags
	if req.WorkID == "" {
		req.WorkID = os.Getenv("ANTHROPIC_WORK_ID")
	}
	if req.SessionID == "" {
		req.SessionID = os.Getenv("ANTHROPIC_SESSION_ID")
	}
	if req.EnvironmentID == "" {
		req.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	}
	if req.EnvironmentKey == "" {
		req.EnvironmentKey = os.Getenv("ANTHROPIC_ENVIRONMENT_KEY")
	}

	// 2. If work ID still missing, try reading JSON payload from stdin
	if req.WorkID == "" {
		stat, err := os.Stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			data, err := io.ReadAll(os.Stdin)
			if err == nil && len(data) > 0 {
				var payload struct {
					ID   string `json:"id"`
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if err := json.Unmarshal(data, &payload); err == nil {
					req.WorkID = payload.ID
					if payload.Data.ID != "" {
						req.SessionID = payload.Data.ID
					}
				}
			}
		}
	}

	if req.WorkID == "" {
		return fmt.Errorf("work ID is required (set ANTHROPIC_WORK_ID or pass --work-id)")
	}

	if cfg.EnvironmentID == "" {
		cfg.EnvironmentID = req.EnvironmentID
	}
	if cfg.EnvironmentKey == "" {
		cfg.EnvironmentKey = req.EnvironmentKey
	}

	if ctx == nil {
		ctx = context.Background()
	}

	return worker.DispatchWork(ctx, cfg, req)
}

func newDispatchCommand() *cobra.Command {
	cfg := worker.Config{}
	var req worker.DispatchRequest

	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Dispatch a single Anthropic work item to a Substrate actor",
		Long: `Dispatch acts as a hook target for 'ant beta:worker poll --on-work'.
It receives work details via environment variables (or stdin JSON),
ensures the Substrate actor exists, and wakes it via the Substrate router.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDispatch(cmd.Context(), cfg, req)
		},
	}

	registerDispatchFlags(cmd, &cfg, &req)
	return cmd
}
