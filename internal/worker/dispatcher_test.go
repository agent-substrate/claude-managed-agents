package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/queue"
	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

// newTestQueue returns a queue backed by an in-process Redis, plus the fake
// server: the queue exposes no depth accessor, so assertions about what it holds
// read the lists directly.
func newTestQueue(t *testing.T) (*queue.RedisQueue, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	q := queue.NewRedis(queue.RedisConfig{Client: client, Logger: discardLogger()})
	t.Cleanup(func() {
		_ = q.Close()
		_ = client.Close()
	})
	return q, server
}

// depth reports how many entries sit on one of the queue's lists, named without
// its prefix (e.g. "pending").
func depth(t *testing.T, server *miniredis.Miniredis, list string) int {
	t.Helper()
	entries, err := server.List(queue.KeyPrefix + ":" + list)
	if err != nil {
		// A list Redis has never held reads as empty.
		return 0
	}
	return len(entries)
}

// discardLogger keeps test output readable.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runDispatchers starts n dispatchers against the same queue. Each runs one
// work item at a time, so n is the concurrency under test.
func runDispatchers(t *testing.T, n int, cfg DispatcherConfig) {
	t.Helper()

	cfg.Logger = discardLogger()
	cfg.Dispatch.Logger = cfg.Logger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, n)

	for range n {
		dispatcher, err := NewDispatcher(cfg)
		if err != nil {
			t.Fatalf("NewDispatcher: %v", err)
		}
		go func() {
			defer func() { done <- struct{}{} }()
			_ = dispatcher.Run(ctx)
		}()
	}

	t.Cleanup(func() {
		cancel()
		for range n {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("dispatcher did not stop within 10s")
				return
			}
		}
	})
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recorder captures the dispatch calls a dispatcher makes.
type recorder struct {
	mu       sync.Mutex
	calls    []DispatchRequest
	inFlight int
	maxPeak  int

	// block, when non-nil, holds each call until it is closed.
	block chan struct{}
	// err is returned by every call.
	err error
}

func (r *recorder) dispatch(ctx context.Context, cfg Config, req DispatchRequest) error {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.inFlight++
	if r.inFlight > r.maxPeak {
		r.maxPeak = r.inFlight
	}
	block, err := r.block, r.err
	r.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-time.After(5 * time.Second):
		}
	}

	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return err
}

func (r *recorder) snapshot() []DispatchRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]DispatchRequest(nil), r.calls...)
}

func (r *recorder) peak() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxPeak
}

func TestDispatcherDispatchesAndAcksQueuedWork(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t)
	rec := &recorder{}

	runDispatchers(t, 1, DispatcherConfig{Queue: q, dispatch: rec.dispatch})

	if err := q.Enqueue(ctx, queue.Event{WorkID: "work_1", SessionID: "sesn_1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, "the work item to be dispatched", func() bool { return len(rec.snapshot()) == 1 })

	got := rec.snapshot()[0]
	if got.WorkID != "work_1" || got.SessionID != "sesn_1" {
		t.Errorf("dispatched %+v, want work_1/sesn_1", got)
	}
	if !got.Wait {
		t.Error("dispatch request Wait = false, want true so the actor is suspended once the work is done")
	}

	waitFor(t, "the queue to drain", func() bool {
		return depth(t, server, "pending") == 0 && depth(t, server, "inflight") == 0
	})
}

func TestDispatcherLeavesFailedWorkInFlight(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t)
	rec := &recorder{err: errors.New("actor unreachable")}

	runDispatchers(t, 1, DispatcherConfig{Queue: q, dispatch: rec.dispatch})

	if err := q.Enqueue(ctx, queue.Event{WorkID: "work_1", SessionID: "sesn_1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, "the work item to fail", func() bool { return len(rec.snapshot()) == 1 })

	// A failed work item is neither acknowledged nor put back by the dispatcher:
	// it stays leased until maintenance reclaims it, which is the same path a
	// killed consumer takes.
	waitFor(t, "the failed work item to stay in flight", func() bool {
		return depth(t, server, "inflight") == 1 &&
			depth(t, server, "pending") == 0 &&
			depth(t, server, "dead") == 0
	})
}

func TestDispatchersSerializeWorkPerSession(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t)
	rec := &recorder{block: make(chan struct{})}

	// Two dispatchers and two work items for one session: with idle capacity to
	// spare, only the session itself can hold the second one back.
	runDispatchers(t, 2, DispatcherConfig{Queue: q, dispatch: rec.dispatch})

	for _, id := range []string{"work_1", "work_2"} {
		if err := q.Enqueue(ctx, queue.Event{WorkID: id, SessionID: "sesn_1"}); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}

	waitFor(t, "the first work item to start", func() bool { return len(rec.snapshot()) >= 1 })
	waitFor(t, "the second work item to stay pending", func() bool {
		return depth(t, server, "pending") == 1 && depth(t, server, "inflight") == 1
	})

	if n := len(rec.snapshot()); n != 1 {
		t.Fatalf("concurrent dispatches for one session = %d, want 1", n)
	}

	close(rec.block)

	// Acknowledging the first work item frees the session, and the second runs.
	waitFor(t, "both work items to be dispatched", func() bool { return len(rec.snapshot()) == 2 })
	if peak := rec.peak(); peak != 1 {
		t.Errorf("peak concurrent work items for one session = %d, want 1", peak)
	}

	waitFor(t, "the queue to drain", func() bool {
		return depth(t, server, "pending") == 0 &&
			depth(t, server, "inflight") == 0 &&
			depth(t, server, "dead") == 0
	})
}

func TestDispatcherRunsOneWorkItemAtATime(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t)
	rec := &recorder{block: make(chan struct{})}

	runDispatchers(t, 1, DispatcherConfig{Queue: q, dispatch: rec.dispatch})

	for _, id := range []string{"sesn_1", "sesn_2", "sesn_3"} {
		if err := q.Enqueue(ctx, queue.Event{WorkID: "work_" + id, SessionID: id}); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}

	// Three unrelated sessions, so nothing but the dispatcher itself limits them.
	waitFor(t, "a work item to start", func() bool { return len(rec.snapshot()) == 1 })
	time.Sleep(200 * time.Millisecond)

	if peak := rec.peak(); peak != 1 {
		t.Errorf("peak concurrent work items for one dispatcher = %d, want 1", peak)
	}
	close(rec.block)
}

func TestDispatchersScaleWithReplicas(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t)
	rec := &recorder{block: make(chan struct{})}

	runDispatchers(t, 3, DispatcherConfig{Queue: q, dispatch: rec.dispatch})

	for _, id := range []string{"sesn_1", "sesn_2", "sesn_3"} {
		if err := q.Enqueue(ctx, queue.Event{WorkID: "work_" + id, SessionID: id}); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}

	// Throughput comes from replicas: three dispatchers run three sessions at once.
	waitFor(t, "all three sessions to run at once", func() bool { return rec.peak() == 3 })
	close(rec.block)
}

func TestDispatcherRequiresQueue(t *testing.T) {
	if _, err := NewDispatcher(DispatcherConfig{}); err == nil {
		t.Error("NewDispatcher without a queue succeeded, want an error")
	}
}

func TestEnqueueWork(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t)

	if err := EnqueueWork(ctx, q, DispatchRequest{WorkID: "work_1", SessionID: "sesn_1"}, discardLogger()); err != nil {
		t.Fatalf("EnqueueWork: %v", err)
	}

	if got := depth(t, server, "pending"); got != 1 {
		t.Errorf("pending = %d, want 1", got)
	}

	if err := EnqueueWork(ctx, q, DispatchRequest{SessionID: "sesn_1"}, discardLogger()); err == nil {
		t.Error("EnqueueWork without a work ID succeeded, want an error")
	}
}
