// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/actor"
	"github.com/agent-substrate/claude-managed-agents/internal/event"
	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/agent-substrate/claude-managed-agents/internal/poller"
	"github.com/agent-substrate/claude-managed-agents/internal/runner"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
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
		pcfg             poller.Config
	)

	cmd := &cobra.Command{
		Use:           "controller",
		Short:         "Run the unified controller: poll Anthropic for work and dispatch lifecycle events to Agent Substrate actors",
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

			em, err := event.New(event.ManagerConfig{
				Logger: logger,
				OnResume: func(ctx context.Context, ev *event.Event) error {
					if ev.WorkID == "" {
						return nil
					}
					return retry(ctx, func() error {
						if err := am.Resume(ctx, ev.SessionID); err != nil {
							return fmt.Errorf("resuming session %s: %w", ev.SessionID, err)
						}
						payload := runner.WorkRequest{
							SessionID:     ev.SessionID,
							WorkID:        ev.WorkID,
							LastHeartbeat: ev.LastHeartbeat,
						}
						if err := am.Notify(ctx, ev.SessionID, payload); err != nil {
							return fmt.Errorf("notifying session %s: %w", ev.SessionID, err)
						}
						return nil
					})
				},
				OnSuspend: func(ctx context.Context, ev *event.Event) error {
					// Nothing to do here, we will garbage collect the actors.
					return nil
				},
			})
			if err != nil {
				return fmt.Errorf("creating event manager: %w", err)
			}
			defer em.Close()

			pcfg.Defaults()
			p, err := poller.New(pcfg)
			if err != nil {
				return fmt.Errorf("creating poller: %w", err)
			}

			g, gctx := errgroup.WithContext(ctx)

			// 1. Run poller loop to ingest work items and queue resume events.
			g.Go(func() error {
				return p.Poll(gctx, func(ctx context.Context, claim poller.Claim) error {
					sessID := claim.Work.Data.ID
					workID := claim.Work.ID
					_, err := em.QueueResume(ctx, sessID, workID, claim.LastHeartbeat)
					return err
				})
			})

			// 2. Run controller reconcile loop to process events from Valkey.
			g.Go(func() error {
				return em.Reconcile(gctx)
			})

			return g.Wait()
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	cmd.Flags().StringVar(&template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&ateapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&atenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet router endpoint")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "connect to ateapi without TLS")
	cmd.Flags().DurationVar(&gcInterval, "gc-interval", actor.DefaultIdleCheckInterval, "interval between actor garbage collection sweeps")
	cmd.Flags().DurationVar(&gcIdleThreshold, "gc-idle-threshold", actor.DefaultIdleThreshold, "idle duration threshold before suspending an actor")
	cmd.Flags().StringVar(&pcfg.BaseURL, "anthropic-base-url", "",
		"custom Anthropic API base URL (default $ANTHROPIC_BASE_URL, else the public API)")
	cmd.Flags().Int64Var(&pcfg.HandoffTTLSeconds, "handoff-ttl-seconds", poller.DefaultHandoffTTLSeconds,
		"lease the poller takes on each work item before queueing it: the time the controller has to start the sandbox before the lease lapses and the item is requeued")

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
