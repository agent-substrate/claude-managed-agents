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

package event

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestManagerUnacknowledgedEventReplayed(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	mgr, err := New(ManagerConfig{
		Valkey: ValkeyConfig{
			Addr:         mini.Addr(),
			DisableCache: true,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()

	// Queue two events
	ev1, err := mgr.QueueResume(ctx, "session-1", "work-100", "2026-08-16T22:00:01Z")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}
	ev2, err := mgr.QueueResume(ctx, "session-2", "work-200", "")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}

	// Pop first event
	pop1, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop: %v", err)
	}
	if pop1.ID != ev1.ID {
		t.Fatalf("expected pop1 ID = %s, got %s", ev1.ID, pop1.ID)
	}

	// Do NOT acknowledge pop1 -> Pop must replay pop1 from PEL before moving to ev2
	popReplayed, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop replayed: %v", err)
	}
	if popReplayed.ID != ev1.ID {
		t.Fatalf("expected replayed pop ID = %s, got %s", ev1.ID, popReplayed.ID)
	}

	// Acknowledge pop1
	if err := mgr.ack(ctx, mgr.streamKey, pop1); err != nil {
		t.Fatalf("Ack pop1: %v", err)
	}

	// Now Pop moves on to ev2
	pop2, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop 2: %v", err)
	}
	if pop2.ID != ev2.ID {
		t.Fatalf("expected pop2 ID = %s, got %s", ev2.ID, pop2.ID)
	}

	// Acknowledge pop2
	if err := mgr.ack(ctx, mgr.streamKey, pop2); err != nil {
		t.Fatalf("Ack pop2: %v", err)
	}

	// Stream should now be empty of pending/new messages
	popEmpty, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop empty: %v", err)
	}
	if popEmpty != nil {
		t.Fatalf("expected nil after all events acknowledged, got %+v", popEmpty)
	}
}

func TestManagerQueueAndPop(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	// Connection error on invalid Valkey address
	if _, err := New(ManagerConfig{Valkey: ValkeyConfig{Addr: "127.0.0.1:0"}}); err == nil {
		t.Error("expected error for invalid ValkeyAddr, got nil")
	}

	mgr, err := New(ManagerConfig{
		Valkey: ValkeyConfig{
			Addr:         mini.Addr(),
			DisableCache: true,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()

	// QueueResume
	ev1, err := mgr.QueueResume(ctx, "session-1", "work-100", "2026-08-16T22:00:01Z")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}
	if ev1.SessionID != "session-1" || ev1.WorkID != "work-100" || ev1.LastHeartbeat != "2026-08-16T22:00:01Z" || ev1.Action != ActionResume || ev1.ID == "" {
		t.Errorf("unexpected ev1: %+v", ev1)
	}

	// QueueSuspend
	ev2, err := mgr.QueueSuspend(ctx, "session-2")
	if err != nil {
		t.Fatalf("QueueSuspend: %v", err)
	}
	if ev2.SessionID != "session-2" || ev2.Action != ActionSuspend || ev2.ID == "" {
		t.Errorf("unexpected ev2: %+v", ev2)
	}

	// Pop first event (ev1)
	pop1, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop 1: %v", err)
	}
	if pop1 == nil || pop1.SessionID != "session-1" || pop1.WorkID != "work-100" || pop1.Action != ActionResume || pop1.ID != ev1.ID {
		t.Fatalf("unexpected pop1: %+v", pop1)
	}

	// Calling Pop before ACK should return the pending unACKed event (ev1)
	pop1Pending, err := mgr.pop(ctx, mgr.streamKey)
	if err != nil {
		t.Fatalf("Pop pending: %v", err)
	}
	if pop1Pending == nil || pop1Pending.ID != ev1.ID {
		t.Fatalf("expected pending ev1 replayed before ACK, got %+v", pop1Pending)
	}

	// Acknowledge ev1
	if err := mgr.ack(ctx, mgr.streamKey, pop1); err != nil {
		t.Fatalf("Ack ev1: %v", err)
	}

	// Pop the suspend event (ev2) from the suspend stream
	pop2, err := mgr.pop(ctx, mgr.suspendKey)
	if err != nil {
		t.Fatalf("Pop 2: %v", err)
	}
	if pop2 == nil || pop2.SessionID != "session-2" || pop2.Action != ActionSuspend || pop2.ID != ev2.ID {
		t.Fatalf("unexpected pop2: %+v", pop2)
	}

	// Acknowledge ev2
	if err := mgr.ack(ctx, mgr.suspendKey, pop2); err != nil {
		t.Fatalf("Ack ev2: %v", err)
	}

	// Pop when all entries are acknowledged returns nil on both streams
	for _, stream := range []string{mgr.streamKey, mgr.suspendKey} {
		pop3, err := mgr.pop(ctx, stream)
		if err != nil {
			t.Fatalf("Pop 3 on %s: %v", stream, err)
		}
		if pop3 != nil {
			t.Fatalf("expected nil pop on empty stream %s, got %+v", stream, pop3)
		}
	}
}

func TestManagerReconcile(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	var (
		resumedSessions []string
		resumedWorks    []string
		suspendedEvents []string
		mu              sync.Mutex
	)

	mgr, err := New(ManagerConfig{
		Valkey: ValkeyConfig{
			Addr:         mini.Addr(),
			DisableCache: true,
		},
		PollInterval: 10 * time.Millisecond,
		OnResume: func(ctx context.Context, event *Event) error {
			mu.Lock()
			defer mu.Unlock()
			resumedSessions = append(resumedSessions, event.SessionID)
			resumedWorks = append(resumedWorks, event.WorkID)
			return nil
		},
		OnSuspend: func(ctx context.Context, event *Event) error {
			mu.Lock()
			defer mu.Unlock()
			suspendedEvents = append(suspendedEvents, event.SessionID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- mgr.Reconcile(ctx)
	}()

	// Queue events
	if _, err := mgr.QueueResume(ctx, "session-resume-1", "work-1", ""); err != nil {
		t.Fatalf("QueueResume: %v", err)
	}
	if _, err := mgr.QueueSuspend(ctx, "session-suspend-1"); err != nil {
		t.Fatalf("QueueSuspend: %v", err)
	}

	// Wait for events to be processed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		resumeCount := len(resumedSessions)
		suspendCount := len(suspendedEvents)
		mu.Unlock()

		if resumeCount >= 1 && suspendCount >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	if len(resumedSessions) != 1 || resumedSessions[0] != "session-resume-1" || resumedWorks[0] != "work-1" {
		t.Errorf("unexpected resumed: sessions=%v, works=%v", resumedSessions, resumedWorks)
	}
	if len(suspendedEvents) != 1 || suspendedEvents[0] != "session-suspend-1" {
		t.Errorf("unexpected suspended: %v", suspendedEvents)
	}
	mu.Unlock()

	// Cancel context and verify clean exit
	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Reconcile did not exit in time after context cancellation")
	}
}

func TestManagerReconcile_MultipleConsumersDeduplication(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processedCount int32

	// Create 3 consumers sharing the same Valkey instance
	numConsumers := 3
	consumers := make([]*Manager, numConsumers)
	for i := 0; i < numConsumers; i++ {
		c, err := New(ManagerConfig{
			Valkey: ValkeyConfig{
				Addr:         mini.Addr(),
				DisableCache: true,
			},
			PollInterval: 10 * time.Millisecond,
			OnResume: func(ctx context.Context, event *Event) error {
				atomic.AddInt32(&processedCount, 1)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("New consumer %d: %v", i, err)
		}
		defer c.Close()

		consumers[i] = c
	}

	for i := 0; i < numConsumers; i++ {
		go func(c *Manager) {
			_ = c.Reconcile(ctx)
		}(consumers[i])
	}

	// Queue 10 events
	for i := 1; i <= 10; i++ {
		_, err := consumers[0].QueueResume(ctx, fmt.Sprintf("session-%d", i), fmt.Sprintf("work-%d", i), "")
		if err != nil {
			t.Fatalf("QueueResume %d: %v", i, err)
		}
	}

	// Wait for processing to complete
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&processedCount) == 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if count := atomic.LoadInt32(&processedCount); count != 10 {
		t.Errorf("processedCount = %d, want exactly 10 (each event popped and processed once)", count)
	}
}

// A resume that keeps failing, as when no worker has room, must not hold up
// suspend events, since those free the workers the resume needs.
func TestManagerReconcile_SuspendNotBlockedByFailingResume(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	suspended := make(chan string, 1)
	mgr, err := New(ManagerConfig{
		Valkey:       ValkeyConfig{Addr: mini.Addr(), DisableCache: true},
		PollInterval: 10 * time.Millisecond,
		OnResume: func(ctx context.Context, event *Event) error {
			return errors.New("no worker has room for the actor")
		},
		OnSuspend: func(ctx context.Context, event *Event) error {
			suspended <- event.SessionID
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	if _, err := mgr.QueueResume(ctx, "session-blocked", "work-1", ""); err != nil {
		t.Fatalf("QueueResume: %v", err)
	}
	if _, err := mgr.QueueSuspend(ctx, "session-running"); err != nil {
		t.Fatalf("QueueSuspend: %v", err)
	}
	go func() { _ = mgr.Reconcile(ctx) }()

	select {
	case got := <-suspended:
		if got != "session-running" {
			t.Fatalf("suspended %q, want session-running", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("suspend event was not processed while a resume kept failing")
	}
}
