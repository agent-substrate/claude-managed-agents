package main

import (
	"context"
	"fmt"

	"github.com/agent-substrate/claude-managed-agents/internal/event"
	"github.com/agent-substrate/claude-managed-agents/internal/poller"
	"github.com/spf13/cobra"
)

func newPollCommand() *cobra.Command {
	var cfg poller.Config

	cmd := &cobra.Command{
		Use:           "poll",
		Short:         "Poll Anthropic for self-hosted environment work items and queue them in the event manager",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg.Defaults()
			em, err := event.New(event.ManagerConfig{
				Logger: cfg.Logger,
			})
			if err != nil {
				return fmt.Errorf("creating event manager: %w", err)
			}
			defer em.Close()

			p, err := poller.New(cfg)
			if err != nil {
				return fmt.Errorf("creating poller: %w", err)
			}
			return p.Poll(ctx, func(ctx context.Context, claim poller.Claim) error {
				sessID := claim.Work.Data.ID
				workID := claim.Work.ID
				_, err := em.QueueResume(ctx, sessID, workID, claim.LastHeartbeat)
				return err
			})
		},
	}

	cmd.Flags().Int64Var(&cfg.HandoffTTLSeconds, "handoff-ttl-seconds", poller.DefaultHandoffTTLSeconds,
		"lease the poller takes on each work item before queueing it: the time the controller has to start the sandbox before the lease lapses and the item is requeued")

	return cmd
}
