package queue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestQueue returns a queue backed by an in-process Redis, plus the fake
// server so tests can inspect keys and advance its clock.
func newTestQueue(t *testing.T, cfg RedisConfig) (*RedisQueue, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	cfg.Client = redis.NewClient(&redis.Options{Addr: server.Addr()})
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	q := NewRedis(cfg)
	t.Cleanup(func() {
		_ = q.Close()
		_ = cfg.Client.Close()
	})
	return q, server
}

// depths reports how many entries sit on each of the queue's lists.
func depths(t *testing.T, q *RedisQueue) (pending, inflight, dead int64) {
	t.Helper()
	ctx := context.Background()
	for _, l := range []struct {
		key string
		out *int64
	}{
		{q.pendingKey(), &pending},
		{q.inflightKey(), &inflight},
		{q.deadKey(), &dead},
	} {
		n, err := q.client.LLen(ctx, l.key).Result()
		if err != nil {
			t.Fatalf("LLEN %s: %v", l.key, err)
		}
		*l.out = n
	}
	return pending, inflight, dead
}

func mustEnqueue(t *testing.T, q *RedisQueue, workID, sessionID string) {
	t.Helper()
	if err := q.Enqueue(context.Background(), Event{WorkID: workID, SessionID: sessionID}); err != nil {
		t.Fatalf("Enqueue(%s): %v", workID, err)
	}
}

func mustClaim(t *testing.T, q *RedisQueue) *Lease {
	t.Helper()
	lease, err := q.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease == nil {
		t.Fatal("Claim returned no lease, want one")
	}
	return lease
}

// killConsumer drops the claim on a leased event, standing in for a consumer
// that stopped renewing it — crashed, partitioned, or simply gone.
func killConsumer(t *testing.T, q *RedisQueue, lease *Lease) {
	t.Helper()
	if err := q.client.Del(context.Background(), q.sessionKey(lease.Event.Session())).Err(); err != nil {
		t.Fatalf("dropping claim: %v", err)
	}
}

func TestEnqueueClaimAck(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")

	if pending, _, _ := depths(t, q); pending != 1 {
		t.Errorf("pending = %d, want 1", pending)
	}

	lease := mustClaim(t, q)
	if lease.Event.WorkID != "work_1" || lease.Event.SessionID != "sesn_1" {
		t.Errorf("claimed %+v, want work_1/sesn_1", lease.Event)
	}
	if lease.Event.ID == "" {
		t.Error("claimed event has no ID")
	}

	if pending, inflight, _ := depths(t, q); pending != 0 || inflight != 1 {
		t.Errorf("pending = %d in-flight = %d, want 0 and 1", pending, inflight)
	}

	if err := q.Ack(ctx, lease); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	if _, inflight, _ := depths(t, q); inflight != 0 {
		t.Errorf("in-flight = %d after ack, want 0", inflight)
	}

	// Double ack is reported as a lost lease rather than silently succeeding.
	if err := q.Ack(ctx, lease); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("second Ack error = %v, want ErrLeaseLost", err)
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	q, _ := newTestQueue(t, RedisConfig{})

	lease, err := q.Claim(context.Background())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease != nil {
		t.Errorf("Claim on empty queue = %+v, want nil", lease)
	}
}

func TestClaimIsFIFO(t *testing.T) {
	q, _ := newTestQueue(t, RedisConfig{})

	for _, id := range []string{"work_1", "work_2", "work_3"} {
		mustEnqueue(t, q, id, "sesn_"+id)
	}

	for _, want := range []string{"work_1", "work_2", "work_3"} {
		lease := mustClaim(t, q)
		if lease.Event.WorkID != want {
			t.Errorf("claimed %s, want %s", lease.Event.WorkID, want)
		}
	}
}

func TestClaimTakesOneEventPerSession(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	mustEnqueue(t, q, "work_2", "sesn_1")
	mustEnqueue(t, q, "work_3", "sesn_2")

	first := mustClaim(t, q)
	if first.Event.WorkID != "work_1" {
		t.Fatalf("claimed %s, want work_1", first.Event.WorkID)
	}

	// work_2 shares the session and must not be handed out; work_3 is free to
	// run in parallel, and the busy item does not block it.
	second := mustClaim(t, q)
	if second.Event.WorkID != "work_3" {
		t.Fatalf("claimed %s, want work_3: a busy session must not block other sessions", second.Event.WorkID)
	}

	if lease, err := q.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	} else if lease != nil {
		t.Fatalf("claimed %s while its session was busy, want nothing", lease.Event.WorkID)
	}

	// Deferring must not cost the event an attempt or lose it.
	if pending, _, dead := depths(t, q); pending != 1 || dead != 0 {
		t.Errorf("pending = %d dead = %d, want work_2 still pending", pending, dead)
	}

	// Once the first work item is acknowledged, the session frees up.
	if err := q.Ack(ctx, first); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	third := mustClaim(t, q)
	if third.Event.WorkID != "work_2" || third.Event.Attempts != 0 {
		t.Errorf("claimed %+v, want work_2 on its first attempt", third.Event)
	}
}

func TestClaimBacksOffWhenEverySessionIsBusy(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	mustClaim(t, q)

	for range maxDeferPerClaim + 2 {
		mustEnqueue(t, q, "work_more", "sesn_1")
	}

	start := time.Now()
	lease, err := q.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if lease != nil {
		t.Fatalf("claimed %s, want nothing while the session is busy", lease.Event.WorkID)
	}
	if elapsed := time.Since(start); elapsed < busyBackoff {
		t.Errorf("Claim returned after %s, want it to back off for %s instead of spinning", elapsed, busyBackoff)
	}
}

func TestMaintainRetriesUnclaimedWork(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	lease := mustClaim(t, q)

	// A live claim is left alone.
	if err := q.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if pending, inflight, _ := depths(t, q); pending != 0 || inflight != 1 {
		t.Fatalf("pending = %d in-flight = %d, want the item still claimed", pending, inflight)
	}

	killConsumer(t, q, lease)

	if err := q.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	retried := mustClaim(t, q)
	if retried.Event.WorkID != "work_1" || retried.Event.Attempts != 1 {
		t.Errorf("retried %+v, want work_1 having used one attempt", retried.Event)
	}
}

func TestMaintainDeadLettersAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")

	for range maxAttempts {
		lease := mustClaim(t, q)
		killConsumer(t, q, lease)
		if err := q.Maintain(ctx); err != nil {
			t.Fatalf("Maintain: %v", err)
		}
	}

	if pending, inflight, dead := depths(t, q); dead != 1 || pending != 0 || inflight != 0 {
		t.Fatalf("pending = %d in-flight = %d dead = %d, want the item dead-lettered", pending, inflight, dead)
	}

	raw, err := q.client.LIndex(ctx, q.deadKey(), 0).Result()
	if err != nil {
		t.Fatalf("reading dead list: %v", err)
	}
	var dead Event
	if err := json.Unmarshal([]byte(raw), &dead); err != nil {
		t.Fatalf("decoding dead event: %v", err)
	}
	if dead.WorkID != "work_1" || dead.Attempts != maxAttempts {
		t.Errorf("dead event = %+v, want work_1 with %d attempts", dead, maxAttempts)
	}
}

func TestMaintainRetriesWorkWhoseSessionMovedOn(t *testing.T) {
	ctx := context.Background()
	q, _ := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	mustEnqueue(t, q, "work_2", "sesn_1")

	first := mustClaim(t, q)
	killConsumer(t, q, first)

	// The session is free again, so the next work item takes it while work_1 is
	// still sitting in flight. work_1 must still count as abandoned: its session
	// key exists, but it names work_2.
	second := mustClaim(t, q)
	if second.Event.WorkID != "work_2" {
		t.Fatalf("claimed %s, want work_2", second.Event.WorkID)
	}

	if err := q.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	pending, inflight, _ := depths(t, q)
	if pending != 1 || inflight != 1 {
		t.Fatalf("pending = %d in-flight = %d, want work_1 requeued and work_2 still claimed", pending, inflight)
	}
}

func TestExtendKeepsTheClaimAlive(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	lease := mustClaim(t, q)

	// Renewing before the TTL lapses keeps the work item claimed.
	server.FastForward(leaseTTL - time.Minute)
	if err := q.Extend(ctx, lease); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	server.FastForward(leaseTTL - time.Minute)

	if err := q.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if _, inflight, _ := depths(t, q); inflight != 1 {
		t.Errorf("in-flight = %d, want the renewed claim left alone", inflight)
	}

	// Once the claim lapses and the work item is reclaimed, the old holder can
	// no longer renew it.
	killConsumer(t, q, lease)
	if err := q.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if err := q.Extend(ctx, lease); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Extend after reclaim = %v, want ErrLeaseLost", err)
	}
}

func TestAckFreesTheSession(t *testing.T) {
	ctx := context.Background()
	q, server := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	lease := mustClaim(t, q)

	if !server.Exists(q.sessionKey("sesn_1")) {
		t.Fatal("claim did not take the session")
	}
	if err := q.Ack(ctx, lease); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if server.Exists(q.sessionKey("sesn_1")) {
		t.Error("ack left the session taken")
	}
}

func TestClaimLapsesOnItsOwn(t *testing.T) {
	q, server := newTestQueue(t, RedisConfig{})

	mustEnqueue(t, q, "work_1", "sesn_1")
	mustClaim(t, q)

	// Nothing renews the session hold, so it lapses on its own even if no
	// consumer is left to run maintenance.
	server.FastForward(leaseTTL + time.Minute)
	if server.Exists(q.sessionKey("sesn_1")) {
		t.Error("session hold outlived the lease")
	}
}

func TestEnqueueRequiresWorkID(t *testing.T) {
	q, _ := newTestQueue(t, RedisConfig{})
	if err := q.Enqueue(context.Background(), Event{SessionID: "sesn_1"}); err == nil {
		t.Error("Enqueue without a work ID succeeded, want an error")
	}
}

func TestEventSessionFallsBackToWorkID(t *testing.T) {
	if got := (Event{WorkID: "work_1"}).Session(); got != "work_1" {
		t.Errorf("Session() = %q, want work_1", got)
	}
	if got := (Event{WorkID: "work_1", SessionID: "sesn_1"}).Session(); got != "sesn_1" {
		t.Errorf("Session() = %q, want sesn_1", got)
	}
}
