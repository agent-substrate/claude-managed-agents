package main

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/agent-substrate/claude-managed-agents/internal/worker"
	"github.com/spf13/cobra"
)

func newDispatchCommand() *cobra.Command {
	cfg := worker.Config{}
	var ropts redisOptions

	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Dispatch queued Anthropic work items to Substrate actors",
		Long: `Dispatch drains the durable Redis queue filled by 'ate-antworker queue'.

For each work item it takes an exclusive lock on the session, ensures the
Substrate actor exists, wakes it via the Substrate router, waits for the work to
finish, suspends the actor, and only then acknowledges the item. A work item that
fails is retried, and one whose dispatcher dies is reclaimed when its lease
expires, so no polled work item is lost.

Run it as its own Deployment, scaled with replicas.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			q, err := ropts.Open(ctx)
			if err != nil {
				return err
			}
			defer q.Close()

			dispatcher, err := worker.NewDispatcher(worker.DispatcherConfig{
				Queue:    q,
				Dispatch: cfg,
			})
			if err != nil {
				return err
			}

			// A clean shutdown is not a failure.
			if err := dispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	registerRedisFlags(cmd, &ropts)

	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", worker.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", worker.DefaultAtenetRouterAddr, "Substrate atenet HTTP router endpoint")
	cmd.Flags().StringVar(&cfg.Namespace, "namespace", worker.DefaultNamespace, "Substrate atespace / namespace")
	cmd.Flags().StringVar(&cfg.Template, "template", worker.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().BoolVar(&cfg.Insecure, "insecure", false, "Use plaintext gRPC connection to ateapi")

	return cmd
}
