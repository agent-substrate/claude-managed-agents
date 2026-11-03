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

// Command loadgen benchmarks Claude Managed Agents on Agent Substrate at scale
// (e.g., 10,000 sessions multiplexed across a 1,000-2,000 pod WorkerPool on a
// 500-node GKE cluster).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/actor"
	"github.com/agent-substrate/claude-managed-agents/internal/event"
	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/agent-substrate/claude-managed-agents/internal/runner"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/spf13/cobra"
	"github.com/valkey-io/valkey-go"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	ModeValkey    = "valkey"
	ModeDirect    = "direct"
	ModeAnthropic = "anthropic"
	ModeBoth      = "both"
)

// Config holds all load generator parameters.
type Config struct {
	Mode             string
	Sessions         int
	Concurrency      int
	TurnsPerSession  int
	RateLimitPerSec  float64
	SessionPrefix    string
	TurnInterval     time.Duration
	ActiveHold       time.Duration
	ReportInterval   time.Duration
	Notify           bool
	SuspendAfterTurn bool
	WaitDrain        bool
	DrainTimeout     time.Duration
	JSONOut          string

	// Substrate / Valkey settings
	Namespace        string
	Template         string
	AteapiAddr       string
	AtenetRouterAddr string
	RouterClientCert string
	Insecure         bool
	CAFile           string
	ValkeyAddr       string
	ValkeyPassword   string

	// Anthropic API settings
	EnvironmentID string
	APIKey        string
	BaseURL       string
	AgentID       string
	Model         string
	Prompt        string
}

// Defaults populates default values for unset fields.
func (c *Config) Defaults() {
	if c.Mode == "" {
		c.Mode = ModeValkey
	}
	if c.Sessions <= 0 {
		c.Sessions = 5000
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 100
	}
	if c.TurnsPerSession <= 0 {
		c.TurnsPerSession = 1
	}
	if c.SessionPrefix == "" {
		c.SessionPrefix = fmt.Sprintf("bench-%d", time.Now().Unix())
	}
	if c.ReportInterval <= 0 {
		c.ReportInterval = 5 * time.Second
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 30 * time.Minute
	}
	if c.Namespace == "" {
		c.Namespace = manifest.DefaultNamespace
	}
	if c.Template == "" {
		c.Template = manifest.DefaultTemplate
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = manifest.DefaultAteapiAddr
	}
	if c.AtenetRouterAddr == "" {
		c.AtenetRouterAddr = manifest.DefaultAtenetRouterAddr
	}
	if c.ValkeyAddr == "" {
		if env := os.Getenv("VALKEY_ADDR"); env != "" {
			c.ValkeyAddr = env
		} else {
			c.ValkeyAddr = manifest.DefaultValkeyAddr
		}
	}
	if c.ValkeyPassword == "" {
		c.ValkeyPassword = os.Getenv("VALKEY_PASSWORD")
	}
	if c.EnvironmentID == "" {
		c.EnvironmentID = os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if c.BaseURL == "" {
		c.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if c.Model == "" {
		if env := os.Getenv("ANTHROPIC_MODEL"); env != "" {
			c.Model = env
		} else {
			c.Model = string(anthropic.BetaManagedAgentsModelClaudeOpus5)
		}
	}
	if c.Prompt == "" {
		c.Prompt = "Run `uname -a` in bash and show the output."
	}
}

// Validate checks that required parameters for the selected mode are valid.
func (c *Config) Validate() error {
	c.Defaults()
	switch c.Mode {
	case ModeValkey, ModeDirect, ModeAnthropic, ModeBoth:
	default:
		return fmt.Errorf("invalid --mode %q: must be one of %s, %s, %s, or %s",
			c.Mode, ModeValkey, ModeDirect, ModeAnthropic, ModeBoth)
	}
	if (c.Mode == ModeAnthropic || c.Mode == ModeBoth) && c.EnvironmentID == "" {
		return errors.New("--anthropic-env-id (or ANTHROPIC_ENVIRONMENT_ID) is required in anthropic/both mode")
	}
	return nil
}

// OperationSummary holds aggregated statistics for a single operation type.
type OperationSummary struct {
	Operation  string  `json:"operation"`
	Total      int     `json:"total"`
	Successes  int     `json:"successes"`
	Errors     int     `json:"errors"`
	MinMs      float64 `json:"min_ms"`
	MeanMs     float64 `json:"mean_ms"`
	P50Ms      float64 `json:"p50_ms"`
	P90Ms      float64 `json:"p90_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
	Throughput float64 `json:"throughput_ops_per_sec"`
}

// BenchmarkReport is the complete benchmark output structure.
type BenchmarkReport struct {
	Mode            string             `json:"mode"`
	Sessions        int                `json:"sessions"`
	TurnsPerSession int                `json:"turns_per_session"`
	Concurrency     int                `json:"concurrency"`
	StartedAt       time.Time          `json:"started_at"`
	CompletedAt     time.Time          `json:"completed_at"`
	TotalDuration   string             `json:"total_duration"`
	TotalDurationS  float64            `json:"total_duration_seconds"`
	Operations      []OperationSummary `json:"operations"`
}

type opBucket struct {
	mu        sync.Mutex
	durations []time.Duration
	successes int
	errors    int
}

// Recorder collects thread-safe latency measurements per operation type.
type Recorder struct {
	mu        sync.Mutex
	startedAt time.Time
	buckets   map[string]*opBucket
	completed atomic.Int64
	failed    atomic.Int64
}

// NewRecorder creates a new metrics Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		startedAt: time.Now(),
		buckets:   make(map[string]*opBucket),
	}
}

// Record logs the duration and error outcome of an operation.
func (r *Recorder) Record(op string, d time.Duration, err error) {
	r.mu.Lock()
	b, ok := r.buckets[op]
	if !ok {
		b = &opBucket{}
		r.buckets[op] = b
	}
	r.mu.Unlock()

	b.mu.Lock()
	b.durations = append(b.durations, d)
	if err != nil {
		b.errors++
		r.failed.Add(1)
	} else {
		b.successes++
		r.completed.Add(1)
	}
	b.mu.Unlock()
}

// Report computes percentile statistics across all recorded operations.
func (r *Recorder) Report(cfg Config) BenchmarkReport {
	now := time.Now()
	totalElapsed := now.Sub(r.startedAt)
	elapsedSec := math.Max(totalElapsed.Seconds(), 0.000001)

	r.mu.Lock()
	ops := make([]string, 0, len(r.buckets))
	for k := range r.buckets {
		ops = append(ops, k)
	}
	slices.Sort(ops)

	summaries := make([]OperationSummary, 0, len(ops))
	for _, op := range ops {
		b := r.buckets[op]
		b.mu.Lock()
		samples := slices.Clone(b.durations)
		succ := b.successes
		errs := b.errors
		b.mu.Unlock()

		slices.Sort(samples)
		total := len(samples)
		sum := OperationSummary{
			Operation:  op,
			Total:      total,
			Successes:  succ,
			Errors:     errs,
			Throughput: float64(total) / elapsedSec,
		}
		if total > 0 {
			var totalDur time.Duration
			for _, s := range samples {
				totalDur += s
			}
			sum.MinMs = durMs(samples[0])
			sum.MeanMs = durMs(totalDur) / float64(total)
			sum.P50Ms = durMs(percentile(samples, 0.50))
			sum.P90Ms = durMs(percentile(samples, 0.90))
			sum.P95Ms = durMs(percentile(samples, 0.95))
			sum.P99Ms = durMs(percentile(samples, 0.99))
			sum.MaxMs = durMs(samples[total-1])
		}
		summaries = append(summaries, sum)
	}
	r.mu.Unlock()

	return BenchmarkReport{
		Mode:            cfg.Mode,
		Sessions:        cfg.Sessions,
		TurnsPerSession: cfg.TurnsPerSession,
		Concurrency:     cfg.Concurrency,
		StartedAt:       r.startedAt.UTC(),
		CompletedAt:     now.UTC(),
		TotalDuration:   totalElapsed.Round(time.Millisecond).String(),
		TotalDurationS:  totalElapsed.Seconds(),
		Operations:      summaries,
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func durMs(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// PrintSummary formats the BenchmarkReport as a human-readable table.
func PrintSummary(w io.Writer, rep BenchmarkReport) {
	fmt.Fprintf(w, "\n========================================================================================================\n")
	fmt.Fprintf(w, "Benchmark Summary (mode=%s, sessions=%d, turns=%d, concurrency=%d, duration=%s)\n",
		rep.Mode, rep.Sessions, rep.TurnsPerSession, rep.Concurrency, rep.TotalDuration)
	fmt.Fprintf(w, "========================================================================================================\n")
	fmt.Fprintf(w, "%-20s %8s %8s %7s %9s %9s %9s %9s %9s %9s\n",
		"OPERATION", "TOTAL", "OK", "ERR", "OPS/SEC", "MEAN(ms)", "P50(ms)", "P90(ms)", "P99(ms)", "MAX(ms)")
	fmt.Fprintf(w, "--------------------------------------------------------------------------------------------------------\n")
	for _, op := range rep.Operations {
		fmt.Fprintf(w, "%-20s %8d %8d %7d %9.2f %9.2f %9.2f %9.2f %9.2f %9.2f\n",
			op.Operation, op.Total, op.Successes, op.Errors, op.Throughput,
			op.MeanMs, op.P50Ms, op.P90Ms, op.P99Ms, op.MaxMs)
	}
	fmt.Fprintf(w, "========================================================================================================\n")
}

// runWorkerPool executes work items across a fixed pool of worker goroutines with optional rate limiting.
func runWorkerPool(ctx context.Context, totalItems, concurrency int, rateLimit float64, fn func(ctx context.Context, idx int) error) error {
	if concurrency > totalItems && totalItems > 0 {
		concurrency = totalItems
	}
	jobs := make(chan int, concurrency*2)

	var limiter <-chan time.Time
	var ticker *time.Ticker
	if rateLimit > 0 {
		interval := time.Duration(float64(time.Second) / rateLimit)
		if interval < time.Microsecond {
			interval = time.Microsecond
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		limiter = ticker.C
	}

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter:
					}
				}
				_ = fn(ctx, idx)
			}
		}()
	}

	for i := 0; i < totalItems; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return ctx.Err()
}

// runValkeyMode injects session resume (and optional suspend) events into Valkey stream:events
// and optionally waits for the controller consumer group to drain all 10k session events.
func runValkeyMode(ctx context.Context, cfg Config, rec *Recorder, logger *slog.Logger) error {
	em, err := event.New(event.ManagerConfig{
		Valkey: event.ValkeyConfig{
			Addr:         cfg.ValkeyAddr,
			Password:     cfg.ValkeyPassword,
			DisableCache: true,
		},
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("connecting to valkey at %s: %w", cfg.ValkeyAddr, err)
	}
	defer em.Close()

	for turn := 1; turn <= cfg.TurnsPerSession; turn++ {
		if turn > 1 && cfg.TurnInterval > 0 {
			logger.Info("Waiting between turns", slog.Int("next_turn", turn), slog.Duration("interval", cfg.TurnInterval))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.TurnInterval):
			}
		}

		logger.Info("Injecting session resume events into Valkey stream",
			slog.Int("turn", turn),
			slog.Int("sessions", cfg.Sessions),
			slog.Int("concurrency", cfg.Concurrency),
		)

		err := runWorkerPool(ctx, cfg.Sessions, cfg.Concurrency, cfg.RateLimitPerSec, func(ctx context.Context, i int) error {
			sessID := fmt.Sprintf("sesn_%s_%05d", cfg.SessionPrefix, i)
			workID := fmt.Sprintf("work_%s_t%d_%05d", cfg.SessionPrefix, turn, i)
			hb := fmt.Sprintf("hb_%s_t%d_%05d", cfg.SessionPrefix, turn, i)

			start := time.Now()
			_, qErr := em.QueueResume(ctx, sessID, workID, hb)
			rec.Record("valkey_queue_resume", time.Since(start), qErr)
			if qErr != nil {
				return qErr
			}

			if cfg.SuspendAfterTurn {
				sStart := time.Now()
				_, sErr := em.QueueSuspend(ctx, sessID)
				rec.Record("valkey_queue_suspend", time.Since(sStart), sErr)
				return sErr
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if cfg.WaitDrain {
		if err := waitValkeyStreamDrain(ctx, cfg, rec, logger); err != nil {
			return err
		}
	}
	return nil
}

func waitValkeyStreamDrain(ctx context.Context, cfg Config, rec *Recorder, logger *slog.Logger) error {
	vkOpt := valkey.ClientOption{
		InitAddress:  []string{cfg.ValkeyAddr},
		DisableCache: true,
	}
	if cfg.ValkeyPassword != "" {
		vkOpt.Password = cfg.ValkeyPassword
	}
	vk, err := valkey.NewClient(vkOpt)
	if err != nil {
		return fmt.Errorf("connecting to valkey for drain check: %w", err)
	}
	defer vk.Close()

	drainStart := time.Now()
	drainCtx, cancel := context.WithTimeout(ctx, cfg.DrainTimeout)
	defer cancel()

	logger.Info("Waiting for controller consumer group to drain stream:events",
		slog.Duration("timeout", cfg.DrainTimeout),
	)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-drainCtx.Done():
			rec.Record("valkey_stream_drain", time.Since(drainStart), drainCtx.Err())
			return fmt.Errorf("waiting for stream drain: %w", drainCtx.Err())
		case <-ticker.C:
			pendingCmd := vk.B().Xpending().Key("stream:events").Group("event").Build()
			arr, pErr := vk.Do(drainCtx, pendingCmd).ToArray()
			if pErr != nil {
				continue
			}
			if len(arr) > 0 {
				pendingCount, _ := arr[0].AsInt64()
				if pendingCount == 0 {
					rec.Record("valkey_stream_drain", time.Since(drainStart), nil)
					logger.Info("Valkey stream pending queue drained", slog.Duration("drain_duration", time.Since(drainStart)))
					return nil
				}
				logger.Info("Valkey stream draining in progress", slog.Int64("pending_events", pendingCount))
			}
		}
	}
}

// runDirectMode drives Substrate actor lifecycle directly via ateapi and atenet-router.
// This exercises actor creation, warmup, HTTP /work routing, snapshot suspension, and
// snapshot resumption across 10,000 sessions multiplexed onto the warm WorkerPool.
func runDirectMode(ctx context.Context, cfg Config, rec *Recorder, logger *slog.Logger) error {
	ateClient, conn, err := actor.DialATE(cfg.AteapiAddr, cfg.Insecure, cfg.CAFile)
	if err != nil {
		return fmt.Errorf("dialing ateapi at %s: %w", cfg.AteapiAddr, err)
	}
	defer conn.Close()

	certFile := cfg.RouterClientCert
	if _, err := os.Stat(certFile); certFile != "" && err != nil {
		logger.Warn("Router client certificate not found; calling atenet-router without mTLS",
			slog.String("path", certFile), slog.Any("error", err))
		certFile = ""
	}
	routerTransport, err := actor.RouterTransportFromFiles(certFile, cfg.CAFile, actor.DefaultRouterServerName)
	if err != nil {
		return fmt.Errorf("configuring atenet-router mTLS: %w", err)
	}

	am := actor.NewManager(ateClient, cfg.Template, cfg.Namespace).
		WithRouterAddr(cfg.AtenetRouterAddr).
		WithRouterTransport(routerTransport).
		WithLogger(logger)

	if err := am.EnsureAtespace(ctx); err != nil {
		logger.Warn("EnsureAtespace returned error (continuing)", slog.Any("error", err))
	}

	for turn := 1; turn <= cfg.TurnsPerSession; turn++ {
		if turn > 1 && cfg.TurnInterval > 0 {
			logger.Info("Waiting between turns", slog.Int("next_turn", turn), slog.Duration("interval", cfg.TurnInterval))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.TurnInterval):
			}
		}

		opResume := "actor_resume_cold"
		if turn > 1 {
			opResume = "actor_resume_warm"
		}

		logger.Info("Running direct Substrate actor load wave",
			slog.Int("turn", turn),
			slog.Int("sessions", cfg.Sessions),
			slog.Int("concurrency", cfg.Concurrency),
			slog.Bool("notify", cfg.Notify),
			slog.Bool("suspend_after_turn", cfg.SuspendAfterTurn),
		)

		err := runWorkerPool(ctx, cfg.Sessions, cfg.Concurrency, cfg.RateLimitPerSec, func(ctx context.Context, i int) error {
			sessID := fmt.Sprintf("sesn_%s_%05d", cfg.SessionPrefix, i)
			workID := fmt.Sprintf("work_%s_t%d_%05d", cfg.SessionPrefix, turn, i)

			rStart := time.Now()
			rErr := am.Resume(ctx, sessID)
			rec.Record(opResume, time.Since(rStart), rErr)
			if rErr != nil {
				return rErr
			}

			if cfg.Notify {
				nStart := time.Now()
				nErr := am.Notify(ctx, sessID, runner.WorkRequest{
					SessionID:     sessID,
					WorkID:        workID,
					LastHeartbeat: "NO_HEARTBEAT",
				})
				rec.Record("actor_notify", time.Since(nStart), nErr)
				if nErr != nil {
					return nErr
				}
			}

			if cfg.ActiveHold > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(cfg.ActiveHold):
				}
			}

			if cfg.SuspendAfterTurn {
				sStart := time.Now()
				actorRef := &ateapipb.ObjectRef{
					Atespace: cfg.Namespace,
					Name:     actor.ActorName(sessID),
				}
				_, sErr := ateClient.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: actorRef})
				if status.Code(sErr) == codes.NotFound {
					sErr = nil
				}
				rec.Record("actor_suspend", time.Since(sStart), sErr)
				return sErr
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// runAnthropicMode creates 10,000 sessions via the Anthropic Managed Agents API
// and optionally submits follow-up turns per session.
func runAnthropicMode(ctx context.Context, cfg Config, rec *Recorder, logger *slog.Logger) error {
	var opts []option.RequestOption
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	client := anthropic.NewClient(opts...)

	agentID := cfg.AgentID
	if agentID == "" {
		start := time.Now()
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name: "Agent Substrate 10k Benchmark Agent",
			Model: anthropic.BetaManagedAgentsModelConfigParams{
				ID: anthropic.BetaManagedAgentsModel(cfg.Model),
			},
			Tools: []anthropic.BetaAgentNewParamsToolUnion{
				{
					OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
						Type: "agent_toolset_20260401",
						DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
							PermissionPolicy: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParamsPermissionPolicyUnion{
								OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{
									Type: "always_allow",
								},
							},
						},
					},
				},
			},
		})
		rec.Record("anthropic_create_agent", time.Since(start), err)
		if err != nil {
			return fmt.Errorf("creating benchmark agent: %w", err)
		}
		agentID = agent.ID
		logger.Info("Created benchmark agent", slog.String("agent_id", agentID))
	}

	sessionIDs := make([]string, cfg.Sessions)

	logger.Info("Creating Anthropic sessions",
		slog.Int("sessions", cfg.Sessions),
		slog.Int("concurrency", cfg.Concurrency),
		slog.String("environment_id", cfg.EnvironmentID),
		slog.String("agent_id", agentID),
	)

	err := runWorkerPool(ctx, cfg.Sessions, cfg.Concurrency, cfg.RateLimitPerSec, func(ctx context.Context, i int) error {
		params := anthropic.BetaSessionNewParams{
			Agent: anthropic.BetaSessionNewParamsAgentUnion{
				OfString: param.NewOpt(agentID),
			},
			EnvironmentID: cfg.EnvironmentID,
			InitialEvents: []anthropic.BetaSessionNewParamsInitialEventUnion{
				{
					OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
						Type: "user.message",
						Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
							{
								OfText: &anthropic.BetaManagedAgentsTextBlockParam{
									Type: "text",
									Text: fmt.Sprintf("[session %d turn 1] %s", i, cfg.Prompt),
								},
							},
						},
					},
				},
			},
		}
		start := time.Now()
		sess, sErr := client.Beta.Sessions.New(ctx, params)
		rec.Record("anthropic_create_session", time.Since(start), sErr)
		if sErr != nil {
			return sErr
		}
		sessionIDs[i] = sess.ID
		return nil
	})
	if err != nil {
		return err
	}

	for turn := 2; turn <= cfg.TurnsPerSession; turn++ {
		if cfg.TurnInterval > 0 {
			logger.Info("Waiting before sending follow-up turn", slog.Int("turn", turn), slog.Duration("interval", cfg.TurnInterval))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(cfg.TurnInterval):
			}
		}

		err := runWorkerPool(ctx, cfg.Sessions, cfg.Concurrency, cfg.RateLimitPerSec, func(ctx context.Context, i int) error {
			sessID := sessionIDs[i]
			if sessID == "" {
				return nil
			}
			start := time.Now()
			_, eErr := client.Beta.Sessions.Events.Send(ctx, sessID, anthropic.BetaSessionEventSendParams{
				Events: []anthropic.BetaManagedAgentsEventParamsUnion{
					{
						OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
							Type: "user.message",
							Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
								{
									OfText: &anthropic.BetaManagedAgentsTextBlockParam{
										Type: "text",
										Text: fmt.Sprintf("[session %d turn %d] %s", i, turn, cfg.Prompt),
									},
								},
							},
						},
					},
				},
			})
			rec.Record("anthropic_send_turn", time.Since(start), eErr)
			return eErr
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func newRootCommand() *cobra.Command {
	cfg := Config{
		Notify:           true,
		SuspendAfterTurn: true,
	}

	cmd := &cobra.Command{
		Use:           "loadgen",
		Short:         "Benchmark load generator for 5,000 actors on Agent Substrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			logger := slog.Default()
			rec := NewRecorder()

			// Periodic live progress reporter.
			progDone := make(chan struct{})
			go func() {
				defer close(progDone)
				ticker := time.NewTicker(cfg.ReportInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						logger.Info("Loadgen progress",
							slog.Int64("completed_ops", rec.completed.Load()),
							slog.Int64("failed_ops", rec.failed.Load()),
							slog.Duration("elapsed", time.Since(rec.startedAt).Round(time.Second)),
						)
					}
				}
			}()

			var runErr error
			switch cfg.Mode {
			case ModeValkey:
				runErr = runValkeyMode(ctx, cfg, rec, logger)
			case ModeDirect:
				runErr = runDirectMode(ctx, cfg, rec, logger)
			case ModeAnthropic:
				runErr = runAnthropicMode(ctx, cfg, rec, logger)
			case ModeBoth:
				if err := runValkeyMode(ctx, cfg, rec, logger); err != nil {
					runErr = err
				} else {
					runErr = runAnthropicMode(ctx, cfg, rec, logger)
				}
			}

			stop()
			<-progDone

			rep := rec.Report(cfg)
			PrintSummary(cmd.OutOrStdout(), rep)

			if cfg.JSONOut != "" {
				data, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling JSON report: %w", err)
				}
				if err := os.WriteFile(cfg.JSONOut, data, 0o644); err != nil {
					return fmt.Errorf("writing JSON report to %s: %w", cfg.JSONOut, err)
				}
				logger.Info("Wrote JSON benchmark report", slog.String("path", cfg.JSONOut))
			}

			return runErr
		},
	}

	cmd.Flags().StringVar(&cfg.Mode, "mode", ModeValkey, "benchmark mode: valkey, direct, anthropic, or both")
	cmd.Flags().IntVar(&cfg.Sessions, "sessions", 5000, "total number of actor sessions to benchmark")
	cmd.Flags().IntVar(&cfg.Concurrency, "concurrency", 100, "number of concurrent worker goroutines")
	cmd.Flags().IntVar(&cfg.TurnsPerSession, "turns-per-session", 1, "number of turns to execute per session")
	cmd.Flags().Float64Var(&cfg.RateLimitPerSec, "rate", 0, "maximum operations per second (0 for unlimited)")
	cmd.Flags().StringVar(&cfg.SessionPrefix, "session-prefix", "", "session ID prefix (defaults to timestamped bench-<ts>)")
	cmd.Flags().DurationVar(&cfg.TurnInterval, "turn-interval", 0, "wait duration between consecutive turns")
	cmd.Flags().DurationVar(&cfg.ActiveHold, "active-hold", 0, "duration to hold actor active before suspending in direct mode")
	cmd.Flags().DurationVar(&cfg.ReportInterval, "report-interval", 5*time.Second, "interval between progress log messages")
	cmd.Flags().BoolVar(&cfg.Notify, "notify", true, "send HTTP POST /work via atenet-router in direct mode")
	cmd.Flags().BoolVar(&cfg.SuspendAfterTurn, "suspend-after-turn", true, "suspend actor after each turn to multiplex 10k sessions over the warm WorkerPool")
	cmd.Flags().BoolVar(&cfg.WaitDrain, "wait-drain", false, "wait for Valkey stream consumer group to drain all pending events")
	cmd.Flags().DurationVar(&cfg.DrainTimeout, "drain-timeout", 30*time.Minute, "timeout when waiting for Valkey stream drain")
	cmd.Flags().StringVar(&cfg.JSONOut, "json-out", "", "optional path to write JSON benchmark report")

	cmd.Flags().StringVar(&cfg.Namespace, "namespace", manifest.DefaultNamespace, "Kubernetes namespace / Substrate atespace")
	cmd.Flags().StringVar(&cfg.Template, "template", manifest.DefaultTemplate, "Substrate ActorTemplate name")
	cmd.Flags().StringVar(&cfg.AteapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	cmd.Flags().StringVar(&cfg.AtenetRouterAddr, "atenet-router-addr", manifest.DefaultAtenetRouterAddr, "Substrate atenet router HTTP endpoint")
	cmd.Flags().StringVar(&cfg.RouterClientCert, "router-client-cert", actor.DefaultRouterClientCertFile,
		"podidentity credential bundle presented to atenet-router for mTLS in direct mode (skipped if absent)")
	cmd.Flags().BoolVar(&cfg.Insecure, "insecure", false, "connect to ateapi without TLS")
	cmd.Flags().StringVar(&cfg.CAFile, "servicedns-ca-file", actor.DefaultServiceDNSCAFile,
		"trust bundle that verifies ateapi's and atenet-router's serving certificates")
	cmd.Flags().StringVar(&cfg.ValkeyAddr, "valkey-addr", "", "Valkey endpoint (default $VALKEY_ADDR or valkey.ate-ant.svc.cluster.local:6379)")

	cmd.Flags().StringVar(&cfg.EnvironmentID, "anthropic-env-id", "", "Anthropic environment ID (default $ANTHROPIC_ENVIRONMENT_ID)")
	cmd.Flags().StringVar(&cfg.APIKey, "anthropic-api-key", "", "Anthropic API key (default $ANTHROPIC_API_KEY)")
	cmd.Flags().StringVar(&cfg.BaseURL, "anthropic-base-url", "", "custom Anthropic API base URL (default $ANTHROPIC_BASE_URL)")
	cmd.Flags().StringVar(&cfg.AgentID, "agent-id", "", "existing Anthropic agent ID to reuse")
	cmd.Flags().StringVar(&cfg.Model, "model", "", "Anthropic model for the benchmark agent")
	cmd.Flags().StringVar(&cfg.Prompt, "prompt", "Run `uname -a` in bash and show the output.", "prompt to execute in each session")

	return cmd
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
}
