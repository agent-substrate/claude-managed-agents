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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/event"
	"github.com/agent-substrate/claude-managed-agents/internal/runner"
	"github.com/alicebob/miniredis/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestControllerCommandFlags(t *testing.T) {
	cmd := newControllerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "controller" {
		t.Errorf("cmd.Use = %q, want controller", cmd.Use)
	}

	flags := []string{
		"namespace",
		"template",
		"ateapi-addr",
		"atenet-router-addr",
		"insecure",
		"gc-interval",
		"gc-idle-threshold",
		"anthropic-base-url",
		"handoff-ttl-seconds",
	}

	for _, name := range flags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s on controller command", name)
		}
	}
}

func TestRetry(t *testing.T) {
	ctx := t.Context()

	// 1. Success on first try
	var calls int
	err := retry(ctx, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}

	// 2. Success on 3rd attempt
	calls = 0
	err = retry(ctx, func() error {
		calls++
		if calls < 3 {
			return errors.New("temporary error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error on 3rd attempt, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}

	// 3. Exhausted attempts
	calls = 0
	err = retry(ctx, func() error {
		calls++
		return errors.New("permanent failure")
	})
	if err == nil || err.Error() != "permanent failure" {
		t.Fatalf("expected 'permanent failure', got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls before failing, got %d", calls)
	}
}

type stubResumer struct {
	resumeErr error
	notifyErr error
	resumed   []string
	notified  []any
}

func (s *stubResumer) Resume(ctx context.Context, sessionID string) error {
	s.resumed = append(s.resumed, sessionID)
	return s.resumeErr
}

func (s *stubResumer) Notify(ctx context.Context, sessionID string, payload any) error {
	s.notified = append(s.notified, payload)
	return s.notifyErr
}

func newTestEventManager(t *testing.T) (*event.Manager, *miniredis.Miniredis) {
	t.Helper()
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)
	em, err := event.New(event.ManagerConfig{
		Valkey: event.ValkeyConfig{
			Addr:         mini.Addr(),
			DisableCache: true,
		},
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("event.New: %v", err)
	}
	t.Cleanup(func() { em.Close() })
	return em, mini
}

// streamEvents reads every entry currently in the event stream.
func streamEvents(t *testing.T, mini *miniredis.Miniredis) []*event.Event {
	t.Helper()
	entries, err := mini.Stream("stream:events")
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	events := make([]*event.Event, 0, len(entries))
	for _, entry := range entries {
		var data string
		for i := 0; i+1 < len(entry.Values); i += 2 {
			if entry.Values[i] == "data" {
				data = entry.Values[i+1]
			}
		}
		if data == "" {
			t.Fatalf("stream entry %s has no data field", entry.ID)
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("unmarshaling entry %s: %v", entry.ID, err)
		}
		ev.ID = entry.ID
		events = append(events, &ev)
	}
	return events
}

func TestResumeRetryDelay(t *testing.T) {
	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second, // capped
		60 * time.Second,
	}
	for i, w := range want {
		attempts := i + 1
		if got := resumeRetryDelay(attempts); got != w {
			t.Errorf("resumeRetryDelay(%d) = %v, want %v", attempts, got, w)
		}
	}
	// Large attempt counts must stay capped and must not overflow.
	if got := resumeRetryDelay(1000); got != 60*time.Second {
		t.Errorf("resumeRetryDelay(1000) = %v, want 60s", got)
	}
	if got := resumeRetryDelay(0); got != 5*time.Second {
		t.Errorf("resumeRetryDelay(0) = %v, want 5s", got)
	}
}

func TestHandleResumeDefersOnResourceExhausted(t *testing.T) {
	ctx := t.Context()
	em, mini := newTestEventManager(t)

	ev, err := em.QueueResume(ctx, "session-1", "work-1", "hb-1")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}

	stub := &stubResumer{
		resumeErr: fmt.Errorf("resuming actor a-session-1: %w",
			status.Error(codes.ResourceExhausted, "no worker has room")),
	}

	before := time.Now()
	if err := handleResume(ctx, stub, em, ev); err != nil {
		t.Fatalf("handleResume: %v", err)
	}

	events := streamEvents(t, mini)
	if len(events) != 2 {
		t.Fatalf("stream holds %d events, want the original plus a deferred retry: %+v", len(events), events)
	}
	retry := events[1]
	if retry.Action != event.ActionResume || retry.SessionID != "session-1" || retry.WorkID != "work-1" || retry.LastHeartbeat != "hb-1" {
		t.Errorf("re-queued event lost fields: %+v", retry)
	}
	if retry.Attempts != 1 {
		t.Errorf("retry.Attempts = %d, want 1", retry.Attempts)
	}
	wantNotBefore := before.Add(resumeRetryBaseDelay)
	if retry.NotBefore.Before(wantNotBefore.Add(-time.Second)) || retry.NotBefore.After(wantNotBefore.Add(5*time.Second)) {
		t.Errorf("retry.NotBefore = %v, want about %v", retry.NotBefore, wantNotBefore)
	}
}

func TestHandleResumeSuccessQueuesSuspend(t *testing.T) {
	ctx := t.Context()
	em, mini := newTestEventManager(t)

	ev, err := em.QueueResume(ctx, "session-1", "work-1", "hb-1")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}

	stub := &stubResumer{}
	if err := handleResume(ctx, stub, em, ev); err != nil {
		t.Fatalf("handleResume: %v", err)
	}

	if len(stub.resumed) != 1 || stub.resumed[0] != "session-1" {
		t.Errorf("resumed = %v, want [session-1]", stub.resumed)
	}
	if len(stub.notified) != 1 {
		t.Fatalf("notified %d times, want 1", len(stub.notified))
	}
	payload, ok := stub.notified[0].(runner.WorkRequest)
	if !ok {
		t.Fatalf("notify payload is %T, want runner.WorkRequest", stub.notified[0])
	}
	if payload.SessionID != "session-1" || payload.WorkID != "work-1" || payload.LastHeartbeat != "hb-1" {
		t.Errorf("unexpected notify payload: %+v", payload)
	}

	events := streamEvents(t, mini)
	if len(events) != 2 {
		t.Fatalf("stream holds %d events, want the original plus a suspend: %+v", len(events), events)
	}
	if events[1].Action != event.ActionSuspend || events[1].SessionID != "session-1" {
		t.Errorf("expected follow-up suspend for session-1, got %+v", events[1])
	}
}

func TestHandleResumeOtherErrorReturns(t *testing.T) {
	ctx := t.Context()
	em, mini := newTestEventManager(t)

	ev, err := em.QueueResume(ctx, "session-1", "work-1", "hb-1")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}

	stub := &stubResumer{resumeErr: errors.New("boom")}
	if err := handleResume(ctx, stub, em, ev); err == nil {
		t.Fatal("handleResume returned nil, want the underlying error")
	}

	events := streamEvents(t, mini)
	if len(events) != 1 {
		t.Errorf("stream holds %d events, want only the original (in-place retry via PEL): %+v", len(events), events)
	}
}
