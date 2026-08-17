package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/agent-substrate/claude-managed-agents/internal/actor"
	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/agent-substrate/claude-managed-agents/internal/sessionmanager"
	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newControllerCommand() *cobra.Command {
	var (
		template         string
		namespace        string
		ateapiAddr       string
		atenetRouterAddr string
		insecure         bool
	)

	cmd := &cobra.Command{
		Use:           "controller",
		Short:         "Consume lifecycle events from the session manager and dispatch to Agent Substrate actors",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			logger := slog.Default()

			ateClient, conn, err := actor.DialATE(ateapiAddr, insecure)
			if err != nil {
				return fmt.Errorf("dialing ateapi at %s: %w", ateapiAddr, err)
			}
			defer conn.Close()

			actorMgr := actor.NewManager(ateClient, template, namespace).
				WithRouterAddr(atenetRouterAddr).
				WithLogger(logger)

			sessionManager, err := sessionmanager.New(sessionmanager.Options{
				Logger: logger,
			})
			if err != nil {
				return fmt.Errorf("creating session manager: %w", err)
			}
			defer sessionManager.Close()

			sessionManager.OnResume = func(ctx context.Context, event *sessionmanager.Event) error {
				if event.WorkID == "" {
					return nil
				}
				if err := actorMgr.Resume(ctx, event.SessionID); err != nil {
					return fmt.Errorf("resuming session %s: %w", event.SessionID, err)
				}
				payload := worker.WorkRequest{
					SessionID: event.SessionID,
					WorkID:    event.WorkID,
				}
				if err := actorMgr.Notify(ctx, event.SessionID, payload); err != nil {
					return fmt.Errorf("notifying session %s: %w", event.SessionID, err)
				}
				return nil
			}

			// TODO(jbd): Start the garbage collector.

			sessionManager.OnSuspend = func(ctx context.Context, event *sessionmanager.Event) error {
				if err := actorMgr.Suspend(ctx, event.SessionID); err != nil {
					return fmt.Errorf("suspending session %s: %w", event.SessionID, err)
				}
				return nil
			}
			return sessionManager.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	cmd.Flags().StringVar(&template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&atenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet router endpoint")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "connect to ateapi without TLS")

	return cmd
}
