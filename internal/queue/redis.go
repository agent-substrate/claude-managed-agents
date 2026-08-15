package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultAddr is where the generated manifests put Redis.
const DefaultAddr = "ate-antworker-redis:6379"

// KeyPrefix namespaces every key the queue uses: <prefix>:pending and friends.
const KeyPrefix = "ate-antworker"

// Queue pacing. These describe how the queue talks to Redis, not a deployment
// choice, so they are fixed.
const (
	// maxAttempts is how many times an event is retried before it moves to the
	// dead-letter list.
	maxAttempts = 3

	// leaseTTL is how long a claimed event stays leased. Dispatchers renew it
	// while the work runs; when renewals stop, the event becomes reclaimable
	// this long afterwards. It doubles as the lifetime of the session's claim.
	leaseTTL = 5 * time.Minute

	// claimBlock is how long a claim blocks waiting for an event to arrive.
	claimBlock = 1 * time.Second

	// maxDeferPerClaim bounds how many events one Claim puts back because their
	// session is busy, so a queue full of one busy session cannot spin.
	maxDeferPerClaim = 8

	// busyBackoff is how long Claim waits after deferring that many events.
	busyBackoff = 100 * time.Millisecond
)

// RedisConfig configures the queue.
type RedisConfig struct {
	// Addr is the Redis host:port. Defaults to DefaultAddr.
	Addr string
	// Password authenticates to Redis. Empty means no auth.
	Password string

	Logger *slog.Logger

	// Client overrides the Redis client (e.g. in tests). When set, Addr and
	// Password are ignored and Close does not close the client.
	Client redis.UniversalClient
}

func (c *RedisConfig) defaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// RedisQueue is a durable work queue backed by Redis lists.
//
// Enqueue LPUSHes onto <prefix>:pending. A dispatcher claims the oldest entry by
// atomically moving it to <prefix>:inflight (BLMOVE ... RIGHT LEFT) and taking
// the event's session with SET NX on <prefix>:session:<id>, whose value is the
// claiming event's ID and whose TTL is the lease. An entry whose session is
// already taken goes back to the end of the pending list untouched.
//
// That one key is the whole ownership record: an in-flight entry is still owned
// exactly while its session key names it. A dispatcher keeps it alive by
// renewing the TTL, so one that stops — crashed, partitioned, or failed — lets
// Redis expire the key for us, and Maintain requeues whatever it finds
// unclaimed. That is the single retry path, so an unfinished work item needs no
// cooperation from the dispatcher that dropped it. Events that exhaust their
// attempts land on <prefix>:dead.
//
// Acknowledging removes the entry from the in-flight list by value, which is
// unambiguous because every payload carries a unique event ID.
type RedisQueue struct {
	client     redis.UniversalClient
	ownsClient bool
	log        *slog.Logger
}

// NewRedis creates the queue. It does not contact Redis; use Ping to verify
// connectivity.
func NewRedis(cfg RedisConfig) *RedisQueue {
	cfg.defaults()

	client := cfg.Client
	ownsClient := false
	if client == nil {
		client = redis.NewClient(&redis.Options{
			Addr:     cfg.Addr,
			Password: cfg.Password,
		})
		ownsClient = true
	}

	return &RedisQueue{
		client:     client,
		ownsClient: ownsClient,
		log:        cfg.Logger,
	}
}

// Ping verifies that Redis is reachable.
func (q *RedisQueue) Ping(ctx context.Context) error {
	return q.client.Ping(ctx).Err()
}

// Close closes the underlying Redis client if the queue created it.
func (q *RedisQueue) Close() error {
	if q.ownsClient {
		return q.client.Close()
	}
	return nil
}

func (q *RedisQueue) key(suffix string) string { return KeyPrefix + ":" + suffix }

func (q *RedisQueue) pendingKey() string         { return q.key("pending") }
func (q *RedisQueue) inflightKey() string        { return q.key("inflight") }
func (q *RedisQueue) deadKey() string            { return q.key("dead") }
func (q *RedisQueue) sessionKey(s string) string { return q.key("session:" + s) }

// Enqueue appends an event to the queue.
func (q *RedisQueue) Enqueue(ctx context.Context, ev Event) error {
	if ev.WorkID == "" {
		return errors.New("queue: work ID is required")
	}
	if ev.ID == "" {
		ev.ID = newID()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("queue: encoding event: %w", err)
	}
	if err := q.client.LPush(ctx, q.pendingKey(), payload).Err(); err != nil {
		return fmt.Errorf("queue: enqueueing %s: %w", ev.WorkID, err)
	}
	return nil
}

// deferScript puts a claimed entry back at the end of the pending list, for an
// event whose session is busy. The entry is unchanged, so it costs no attempt.
//
// KEYS: inflight, pending. ARGV: payload.
var deferScript = redis.NewScript(`
if redis.call('LREM', KEYS[1], 1, ARGV[1]) == 0 then
  return 0
end
redis.call('LPUSH', KEYS[2], ARGV[1])
return 1
`)

// Claim leases the oldest event whose session is free, blocking briefly for one
// to arrive. It returns (nil, nil) when nothing is claimable.
func (q *RedisQueue) Claim(ctx context.Context) (*Lease, error) {
	for deferred := 0; deferred < maxDeferPerClaim; {
		payload, err := q.client.BLMove(ctx, q.pendingKey(), q.inflightKey(), "RIGHT", "LEFT", claimBlock).Result()
		switch {
		case errors.Is(err, redis.Nil):
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("queue: claiming event: %w", err)
		}

		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// Undecodable entries can never be dispatched; park them for
			// inspection instead of reclaiming them forever.
			q.client.LRem(ctx, q.inflightKey(), 1, payload)
			q.client.LPush(ctx, q.deadKey(), payload)
			return nil, fmt.Errorf("queue: decoding event, moved to dead list: %w", err)
		}

		// Take the session, which is also this entry's claim on itself: one
		// work item per sandbox at a time, held for as long as the TTL is
		// renewed.
		free, err := q.client.SetNX(ctx, q.sessionKey(ev.Session()), ev.ID, leaseTTL).Result()
		if err != nil {
			return nil, fmt.Errorf("queue: claiming session %s: %w", ev.Session(), err)
		}
		if !free {
			if err := deferScript.Run(ctx, q.client,
				[]string{q.inflightKey(), q.pendingKey()}, payload).Err(); err != nil {
				return nil, fmt.Errorf("queue: deferring %s: %w", ev.WorkID, err)
			}
			deferred++
			continue
		}
		return &Lease{Event: ev, payload: payload}, nil
	}

	// Everything seen belongs to a session that is already running a work item.
	sleep(ctx, busyBackoff)
	return nil, nil
}

// ackScript removes an in-flight entry and releases the session it holds. It
// reports how many entries were removed, which is 0 when the entry was already
// reclaimed.
//
// KEYS: inflight, session. ARGV: payload, event ID.
var ackScript = redis.NewScript(`
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
if removed == 0 then
  return 0
end
if redis.call('GET', KEYS[2]) == ARGV[2] then
  redis.call('DEL', KEYS[2])
end
return removed
`)

// Ack removes a completed event from the queue and frees its session.
func (q *RedisQueue) Ack(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return errors.New("queue: nil lease")
	}
	removed, err := ackScript.Run(ctx, q.client,
		[]string{q.inflightKey(), q.sessionKey(lease.Event.Session())},
		lease.payload, lease.Event.ID,
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: acknowledging %s: %w", lease.Event.WorkID, err)
	}
	if removed == 0 {
		return ErrLeaseLost
	}
	return nil
}

// extendScript pushes out the session TTL, but only while it still names this
// event.
//
// KEYS: session. ARGV: event ID, TTL millis.
var extendScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

// Extend keeps a claim alive while the work is still running. Dispatchers call it
// on a heartbeat; when the calls stop, the claim expires and Maintain requeues
// the event.
func (q *RedisQueue) Extend(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return errors.New("queue: nil lease")
	}

	held, err := extendScript.Run(ctx, q.client,
		[]string{q.sessionKey(lease.Event.Session())},
		lease.Event.ID, leaseTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: extending claim on %s: %w", lease.Event.WorkID, err)
	}
	if held == 0 {
		return ErrLeaseLost
	}
	return nil
}

// reclaimScript retries an unclaimed in-flight entry, or dead-letters it. It
// never touches the session key: an entry only reaches here once the key stopped
// naming it, so the key is either gone or already owned by a newer claim.
//
// KEYS: inflight, pending, dead.
// ARGV: old payload, new payload, dead flag ("1" or "0").
var reclaimScript = redis.NewScript(`
if redis.call('LREM', KEYS[1], 1, ARGV[1]) == 0 then
  return 0
end
if ARGV[3] == '1' then
  redis.call('LPUSH', KEYS[3], ARGV[2])
else
  redis.call('LPUSH', KEYS[2], ARGV[2])
end
return 1
`)

// Maintain requeues in-flight events that nothing claims any more, and
// dead-letters those that ran out of attempts. It is the only retry path, so
// dispatchers must call it periodically.
//
// An event is unclaimed once its session key stops naming it: the dispatcher let
// the TTL lapse, or the session has since been claimed by a different event.
// Both mean the same thing — no live dispatcher is working on this entry.
func (q *RedisQueue) Maintain(ctx context.Context) error {
	payloads, err := q.client.LRange(ctx, q.inflightKey(), 0, -1).Result()
	if err != nil {
		return fmt.Errorf("queue: listing in-flight events: %w", err)
	}

	for _, payload := range payloads {
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}

		owner, err := q.client.Get(ctx, q.sessionKey(ev.Session())).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("queue: reading session %s: %w", ev.Session(), err)
		}
		if owner == ev.ID {
			continue
		}
		if err := q.reclaim(ctx, ev, payload); err != nil {
			return err
		}
	}
	return nil
}

func (q *RedisQueue) reclaim(ctx context.Context, ev Event, payload string) error {
	next := ev
	next.Attempts++

	dead := "0"
	if next.Attempts >= maxAttempts {
		dead = "1"
	}

	retry, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("queue: encoding event: %w", err)
	}

	moved, err := reclaimScript.Run(ctx, q.client,
		[]string{q.inflightKey(), q.pendingKey(), q.deadKey()},
		payload, retry, dead,
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: reclaiming %s: %w", ev.WorkID, err)
	}
	if moved == 0 {
		// Another dispatcher reclaimed it first.
		return nil
	}

	if dead == "1" {
		q.log.Warn("Work item exhausted its attempts; moved to dead-letter list",
			slog.String("work_id", ev.WorkID),
			slog.String("session_id", ev.Session()),
			slog.Int("attempts", next.Attempts),
		)
		return nil
	}
	q.log.Info("Claim lapsed on unfinished work item; requeued",
		slog.String("work_id", ev.WorkID),
		slog.String("session_id", ev.Session()),
		slog.Int("attempts", next.Attempts),
	)
	return nil
}
