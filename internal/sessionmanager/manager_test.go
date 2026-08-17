package sessionmanager

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

func TestManagerQueueAndPop(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	// Connection error on invalid Valkey address
	if _, err := New(Options{ValkeyAddr: "127.0.0.1:0"}); err == nil {
		t.Error("expected error for invalid ValkeyAddr, got nil")
	}

	mgr, err := New(Options{
		ValkeyAddr:   mini.Addr(),
		DisableCache: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx := context.Background()

	// QueueResume
	ev1, err := mgr.QueueResume(ctx, "session-1", "work-100")
	if err != nil {
		t.Fatalf("QueueResume: %v", err)
	}
	if ev1.SessionID != "session-1" || ev1.WorkID != "work-100" || ev1.Action != ActionResume {
		t.Errorf("unexpected ev1: %+v", ev1)
	}

	// QueueSuspend
	ev2, err := mgr.QueueSuspend(ctx, "session-2")
	if err != nil {
		t.Fatalf("QueueSuspend: %v", err)
	}
	if ev2.SessionID != "session-2" || ev2.Action != ActionSuspend {
		t.Errorf("unexpected ev2: %+v", ev2)
	}

	// Pop first event (FIFO: ev1)
	pop1, err := mgr.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop 1: %v", err)
	}
	if pop1 == nil || pop1.SessionID != "session-1" || pop1.WorkID != "work-100" || pop1.Action != ActionResume {
		t.Fatalf("unexpected pop1: %+v", pop1)
	}

	// Pop second event (FIFO: ev2)
	pop2, err := mgr.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop 2: %v", err)
	}
	if pop2 == nil || pop2.SessionID != "session-2" || pop2.Action != ActionSuspend {
		t.Fatalf("unexpected pop2: %+v", pop2)
	}

	// Pop when empty returns nil
	pop3, err := mgr.Pop(ctx)
	if err != nil {
		t.Fatalf("Pop 3: %v", err)
	}
	if pop3 != nil {
		t.Fatalf("expected nil pop on empty queue, got %+v", pop3)
	}
}

func TestManagerRun(t *testing.T) {
	mini, err := miniredis.Run()
	if err != nil {
		t.Fatalf("starting miniredis: %v", err)
	}
	t.Cleanup(mini.Close)

	mgr, err := New(Options{
		ValkeyAddr:   mini.Addr(),
		DisableCache: true,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer mgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		resumedSessions []string
		resumedWorks    []string
		suspendedEvents []string
		mu              sync.Mutex
	)

	mgr.OnResume = func(ctx context.Context, event *Event) error {
		mu.Lock()
		defer mu.Unlock()
		resumedSessions = append(resumedSessions, event.SessionID)
		resumedWorks = append(resumedWorks, event.WorkID)
		return nil
	}

	mgr.OnSuspend = func(ctx context.Context, event *Event) error {
		mu.Lock()
		defer mu.Unlock()
		suspendedEvents = append(suspendedEvents, event.SessionID)
		return nil
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- mgr.Run(ctx)
	}()

	// Queue events
	if _, err := mgr.QueueResume(ctx, "session-resume-1", "work-1"); err != nil {
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
		t.Fatal("Run did not exit in time after context cancellation")
	}
}

func TestManagerRun_MultipleConsumersDeduplication(t *testing.T) {
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
		c, err := New(Options{
			ValkeyAddr:   mini.Addr(),
			DisableCache: true,
			PollInterval: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("New consumer %d: %v", i, err)
		}
		defer c.Close()

		c.OnResume = func(ctx context.Context, event *Event) error {
			atomic.AddInt32(&processedCount, 1)
			return nil
		}
		consumers[i] = c
	}

	for i := 0; i < numConsumers; i++ {
		go func(c *Manager) {
			_ = c.Run(ctx)
		}(consumers[i])
	}

	// Queue 10 events
	for i := 1; i <= 10; i++ {
		_, err := consumers[0].QueueResume(ctx, fmt.Sprintf("session-%d", i), fmt.Sprintf("work-%d", i))
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


