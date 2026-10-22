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

package envworker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"
)

const (
	heartbeatDefault    = 30 * time.Second
	heartbeatFloor      = 1 * time.Second
	noHeartbeatSentinel = "NO_HEARTBEAT"
	stopTimeout         = 10 * time.Second
)

type helperTag string

const (
	helperEnvironmentsWorker helperTag = "environments-worker"
)

func withHelperTag(v helperTag) option.RequestOption {
	return option.WithHeader("x-stainless-helper", string(v))
}

func bearerReqOpts(environmentKey string) ([]option.RequestOption, error) {
	if environmentKey == "" {
		return nil, errors.New("environments: environment key is required")
	}
	return []option.RequestOption{
		option.WithHeaderDel("X-Api-Key"),
		option.WithAuthToken(environmentKey),
	}, nil
}

func helperReqOpts(environmentKey string, helper helperTag) ([]option.RequestOption, error) {
	opts, err := bearerReqOpts(environmentKey)
	if err != nil {
		return nil, err
	}
	return append(opts, withHelperTag(helper)), nil
}

// EnvironmentWorkerOptions configures an EnvironmentWorker.
type EnvironmentWorkerOptions struct {
	EnvironmentID  string
	EnvironmentKey string
	Workdir        string
	// AllowedRoots are directories outside Workdir the file tools may also
	// reach (agenttoolset.AgentToolContext.AllowedRoots). The SDK no longer
	// supports UnrestrictedPaths; list "/" to reach the whole filesystem.
	AllowedRoots   []string
	MaxFileBytes   int64
	Tools          []anthropic.BetaTool
	ToolsFunc      func(env *agenttoolset.AgentToolContext) []anthropic.BetaTool
	MaxIdle        *time.Duration
	RequestOptions []option.RequestOption
	Logger         *slog.Logger
}

// HandleItemOptions configures a work item execution.
type HandleItemOptions struct {
	WorkID                string
	EnvironmentID         string
	SessionID             string
	EnvironmentKey        string
	ExpectedLastHeartbeat string
}

// EnvironmentWorker executes session work items with tool dispatch and heartbeat management.
type EnvironmentWorker struct {
	client anthropic.Client
	opts   EnvironmentWorkerOptions
}

// NewEnvironmentWorker creates a new EnvironmentWorker instance.
func NewEnvironmentWorker(client anthropic.Client, opts EnvironmentWorkerOptions) *EnvironmentWorker {
	if opts.Workdir == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Workdir = wd
		} else {
			opts.Workdir = "."
		}
	}
	return &EnvironmentWorker{client: client, opts: opts}
}

// HandleItem executes a work item in-process: sets up skills, runs session tool runner,
// maintains lease heartbeats in the background, and force-stops on completion unless lease was lost.
func (w *EnvironmentWorker) HandleItem(ctx context.Context, opts HandleItemOptions) error {
	workID := cmp.Or(opts.WorkID, os.Getenv("ANTHROPIC_WORK_ID"))
	environmentID := cmp.Or(opts.EnvironmentID, os.Getenv("ANTHROPIC_ENVIRONMENT_ID"))
	sessionID := cmp.Or(opts.SessionID, os.Getenv("ANTHROPIC_SESSION_ID"))
	environmentKey := cmp.Or(opts.EnvironmentKey, w.opts.EnvironmentKey, os.Getenv("ANTHROPIC_ENVIRONMENT_KEY"))
	expectedLastHeartbeat := cmp.Or(opts.ExpectedLastHeartbeat, os.Getenv("ANTHROPIC_WORK_LAST_HEARTBEAT"))

	for _, req := range []struct{ name, val, env string }{
		{"work_id", workID, "ANTHROPIC_WORK_ID"},
		{"environment_id", environmentID, "ANTHROPIC_ENVIRONMENT_ID"},
		{"session_id", sessionID, "ANTHROPIC_SESSION_ID"},
		{"environment_key", environmentKey, "ANTHROPIC_ENVIRONMENT_KEY"},
	} {
		if req.val == "" {
			return fmt.Errorf("EnvironmentWorker.HandleItem: %s is required — pass it in HandleItemOptions or set %s", req.name, req.env)
		}
	}

	work := &anthropic.BetaSelfHostedWork{
		ID:            workID,
		EnvironmentID: environmentID,
		Data: anthropic.BetaSessionWorkData{
			ID: sessionID,
		},
	}

	log := w.opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("work_id", work.ID), slog.String("session_id", sessionID))

	helperOpts, err := helperReqOpts(environmentKey, helperEnvironmentsWorker)
	if err != nil {
		return err
	}
	hbStopOpts := make([]option.RequestOption, 0, len(w.opts.RequestOptions)+len(helperOpts))
	hbStopOpts = append(hbStopOpts, w.opts.RequestOptions...)
	hbStopOpts = append(hbStopOpts, helperOpts...)

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	hbDone := make(chan struct{})
	var leaseLost bool
	go func() {
		defer close(hbDone)
		leaseLost = runHeartbeat(sessCtx, sessCancel, w.client, work, hbStopOpts, expectedLastHeartbeat, log)
	}()

	env := &agenttoolset.AgentToolContext{
		Workdir:      w.opts.Workdir,
		AllowedRoots: w.opts.AllowedRoots,
		MaxFileBytes: w.opts.MaxFileBytes,
	}

	if err := env.SetupSkills(sessCtx, w.client, sessionID, hbStopOpts...); err != nil {
		log.Warn("skill setup failed", slog.Any("error", err))
	}
	defer func() {
		if err := env.Cleanup(); err != nil {
			log.Warn("skill cleanup failed", slog.Any("error", err))
		}
	}()

	var (
		tools      []anthropic.BetaTool
		closeTools bool
	)
	switch {
	case w.opts.ToolsFunc != nil:
		tools = w.opts.ToolsFunc(env)
		closeTools = true
	case w.opts.Tools != nil:
		tools = w.opts.Tools
	default:
		tools = agenttoolset.BetaAgentToolset20260401(env)
		closeTools = true
	}
	if closeTools {
		defer agenttoolset.CloseAll(tools)
	}

	runnerBearerOpts, err := bearerReqOpts(environmentKey)
	if err != nil {
		return err
	}
	runnerReqOpts := make([]option.RequestOption, 0, len(w.opts.RequestOptions)+len(runnerBearerOpts))
	runnerReqOpts = append(runnerReqOpts, w.opts.RequestOptions...)
	runnerReqOpts = append(runnerReqOpts, runnerBearerOpts...)

	runner := w.client.Beta.Sessions.Events.NewToolRunner(sessCtx, sessionID, anthropic.SessionToolRunnerOptions{
		Tools:          tools,
		MaxIdle:        w.opts.MaxIdle,
		Logger:         log,
		RequestOptions: runnerReqOpts,
	})
	for runner.Next() {
		call := runner.Current()
		log.Info("dispatched tool",
			slog.String("tool", call.Name),
			slog.Bool("is_error", call.IsError),
			slog.Bool("posted", call.Posted))
	}
	var runErr error
	if err := runner.Err(); err != nil &&
		!errors.Is(err, anthropic.ErrSessionTerminated) &&
		!errors.Is(err, anthropic.ErrIdleTimeout) {
		log.Warn("session tool runner exited with error", slog.Any("error", err))
		runErr = err
	}
	_ = runner.Close()

	sessCancel()
	<-hbDone

	if leaseLost {
		log.Info("lease lost; released without stopping it")
		return runErr
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := stopWork(stopCtx, w.client, work.ID,
		anthropic.BetaEnvironmentWorkStopParams{
			EnvironmentID: work.EnvironmentID,
			BetaSelfHostedWorkStopRequest: anthropic.BetaSelfHostedWorkStopRequestParam{
				Force: param.NewOpt(true),
			},
		},
		hbStopOpts...,
	); err != nil && !isStatus(err, 409) {
		log.Warn("force-stop on exit failed", slog.Any("error", err))
	}

	return runErr
}

func runHeartbeat(ctx context.Context, cancel context.CancelFunc, client anthropic.Client, work *anthropic.BetaSelfHostedWork, reqOpts []option.RequestOption, expectedLastHeartbeat string, log *slog.Logger) (lost bool) {
	interval := heartbeatDefault
	ttl := heartbeatDefault
	last := cmp.Or(expectedLastHeartbeat, noHeartbeatSentinel)
	lastSuccess := time.Now()

	beat := func() bool {
		beatOpts := append(reqOpts[:len(reqOpts):len(reqOpts)], option.WithRequestTimeout(interval))
		resp, err := client.Beta.Environments.Work.Heartbeat(
			ctx,
			work.ID,
			anthropic.BetaEnvironmentWorkHeartbeatParams{
				EnvironmentID:         work.EnvironmentID,
				ExpectedLastHeartbeat: param.NewOpt(last),
			},
			beatOpts...,
		)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			if isStatus(err, 412) {
				log.Warn("heartbeat precondition failed; lease reclaimed by another worker",
					slog.Any("error", err))
				lost = true
				return false
			}
			if isFatal4xx(err) {
				log.Error("permanent heartbeat failure", slog.Any("error", err))
				return false
			}
			if stale := time.Since(lastSuccess); stale > ttl {
				log.Error("heartbeat staleness ceiling exceeded; lease presumed expired",
					slog.Duration("since_last_success", stale),
					slog.Duration("ttl", ttl),
					slog.Any("error", err))
				lost = true
				return false
			}
			log.Warn("transient heartbeat failure", slog.Any("error", err))
			return true
		}
		last = resp.LastHeartbeat
		lastSuccess = time.Now()
		if resp.TTLSeconds > 0 {
			ttl = max(time.Duration(resp.TTLSeconds)*time.Second, heartbeatFloor)
			interval = clampDur(ttl/2, heartbeatFloor, heartbeatDefault)
		}
		switch resp.State {
		case anthropic.BetaSelfHostedWorkHeartbeatResponseStateStopping,
			anthropic.BetaSelfHostedWorkHeartbeatResponseStateStopped:
			log.Info("heartbeat reports shutdown", slog.String("state", string(resp.State)))
			return false
		}
		if !resp.LeaseExtended {
			log.Warn("lease not extended; shutting down")
			return false
		}
		return true
	}

	if !beat() {
		cancel()
		return lost
	}
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return lost
		case <-t.C:
		}
		if !beat() {
			cancel()
			return lost
		}
	}
}

func clampDur(v, lo, hi time.Duration) time.Duration {
	return max(lo, min(hi, v))
}

func stopWork(ctx context.Context, client anthropic.Client, workID string, params anthropic.BetaEnvironmentWorkStopParams, opts ...option.RequestOption) error {
	var raw *http.Response
	opts = append(opts, option.WithResponseBodyInto(&raw))
	_, err := client.Beta.Environments.Work.Stop(ctx, workID, params, opts...)
	if raw != nil && raw.Body != nil {
		_ = raw.Body.Close()
	}
	return err
}

func isStatus(err error, code int) bool {
	var apiErr *anthropic.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func isFatal4xx(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	c := apiErr.StatusCode
	return c >= 400 && c < 500 && c != 408 && c != 409 && c != 429
}
