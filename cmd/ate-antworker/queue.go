package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

// enqueueTimeout bounds how long the poller hook may spend queueing a work item.
const enqueueTimeout = 10 * time.Second

func runQueue(ctx context.Context, req worker.DispatchRequest, ropts redisOptions) error {
	// Read from environment variables if not passed via flags
	if req.WorkID == "" {
		req.WorkID = os.Getenv("ANTHROPIC_WORK_ID")
	}
	if req.SessionID == "" {
		req.SessionID = os.Getenv("ANTHROPIC_SESSION_ID")
	}

	if req.WorkID == "" {
		return fmt.Errorf("work ID is required (set ANTHROPIC_WORK_ID or pass --work-id)")
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// Persist the work item and return: resuming the actor is the consumer's
	// job, so this process dying cannot drop the work item.
	ctx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()

	q, err := ropts.Open(ctx)
	if err != nil {
		return err
	}
	defer q.Close()

	return worker.EnqueueWork(ctx, q, req, nil)
}

func newQueueCommand() *cobra.Command {
	var req worker.DispatchRequest
	var ropts redisOptions

	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Queue a single Anthropic work item for dispatch to a Substrate actor",
		Long: `Queue acts as a hook target for 'ant beta:worker poll --on-work'.
It receives work details via environment variables or flags, appends the work
item to the durable queue, and returns immediately.

'ate-antworker dispatch' claims the work item from there, resumes the Substrate
actor, and runs it, so a work item survives this process dying between the poll
and the work being done.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runQueue(cmd.Context(), req, ropts)
		},
	}

	registerRedisFlags(cmd, &ropts)
	cmd.Flags().StringVar(&req.WorkID, "work-id", "", "Anthropic work ID (or $ANTHROPIC_WORK_ID)")
	cmd.Flags().StringVar(&req.SessionID, "session-id", "", "Anthropic session ID (or $ANTHROPIC_SESSION_ID)")
	return cmd
}
