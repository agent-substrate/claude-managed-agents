package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/actor"
	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/agent-substrate/claude-managed-agents/internal/runner"
	"github.com/agent-substrate/claude-managed-agents/internal/sessionmanager"
	"github.com/spf13/cobra"
)

func newControllerCommand() *cobra.Command {
	var (
		template         string
		namespace        string
		ateapiAddr       string
		atenetRouterAddr string
		insecure         bool
		gcInterval       time.Duration
		gcIdleThreshold  time.Duration
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

			am := actor.NewManager(ateClient, template, namespace).
				WithRouterAddr(atenetRouterAddr).
				WithLogger(logger)

			// Start background actor idle garbage collector.
			am.StartGC(ctx, gcInterval, gcIdleThreshold)

			sm, err := sessionmanager.New(sessionmanager.Options{
				Logger: logger,
				OnResume: func(ctx context.Context, event *sessionmanager.Event) error {
					if event.WorkID == "" {
						return nil
					}
					return retry(ctx, func() error {
						if err := am.Resume(ctx, event.SessionID); err != nil {
							return fmt.Errorf("resuming session %s: %w", event.SessionID, err)
						}
						payload := runner.WorkRequest{
							SessionID: event.SessionID,
							WorkID:    event.WorkID,
						}
						if err := am.Notify(ctx, event.SessionID, payload); err != nil {
							return fmt.Errorf("notifying session %s: %w", event.SessionID, err)
						}
						return nil
					})
				},
				OnSuspend: func(ctx context.Context, event *sessionmanager.Event) error {
					// Nothing to do here, we will garbage collect the actors.
					return nil
				},
			})
			if err != nil {
				return fmt.Errorf("creating session manager: %w", err)
			}
			defer sm.Close()

			return sm.Reconcile(ctx)
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	cmd.Flags().StringVar(&template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&atenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet router endpoint")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "connect to ateapi without TLS")
	cmd.Flags().DurationVar(&gcInterval, "gc-interval", actor.DefaultIdleCheckInterval, "interval between actor garbage collection sweeps")
	cmd.Flags().DurationVar(&gcIdleThreshold, "gc-idle-threshold", actor.DefaultIdleThreshold, "idle duration threshold before suspending an actor")

	return cmd
}

func retry(ctx context.Context, fn func() error) error {
	const maxRetries = 3
	var lastErr error
	backoff := 500 * time.Millisecond

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		if err := fn(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}
