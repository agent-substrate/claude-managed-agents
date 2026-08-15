package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis queue defaults.
const (
	DefaultAddr        = "ate-antworker-redis:6379"
	DefaultPrefix      = "ate-antworker"
	DefaultMaxAttempts = 3
)

// Queue pacing. These describe how the queue talks to Redis, not a deployment
// choice, so they are fixed.
const (
	// leaseTTL is how long a claimed event stays leased. Consumers renew it
	// while a turn runs; when renewals stop, the event becomes reclaimable this
	// long afterwards. It doubles as the lifetime of the session's claim.
	leaseTTL = 5 * time.Minute

	// claimBlock is how long a claim blocks waiting for an event to arrive.
	claimBlock = 5 * time.Second

	// maxDeferPerClaim bounds how many events one Claim puts back because their
	// session is busy, so a queue full of one busy session cannot spin.
	maxDeferPerClaim = 8

	// busyBackoff is how long Claim waits after deferring that many events.
	busyBackoff = time.Second
)

// RedisConfig configures the queue.
type RedisConfig struct {
	// Addr is the Redis host:port. Defaults to DefaultAddr.
	Addr string
	// Password authenticates to Redis. Empty means no auth.
	Password string
	// DB selects the Redis logical database.
	DB int
	// Prefix namespaces every key used by the queue. Defaults to DefaultPrefix.
	Prefix string
	// MaxAttempts is how many times an event is retried before it moves to the
	// dead-letter list. Defaults to DefaultMaxAttempts.
	MaxAttempts int
	// DedupTTL, when positive, drops events whose work ID was already enqueued
	// within this window. Disabled by default.
	DedupTTL time.Duration

	Logger *slog.Logger

	// Client overrides the Redis client (e.g. in tests). When set, Addr,
	// Password and DB are ignored and Close does not close the client.
	Client redis.UniversalClient
}

func (c *RedisConfig) defaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.Prefix == "" {
		c.Prefix = DefaultPrefix
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// RedisQueue is a durable work queue backed by Redis lists.
//
// Enqueue LPUSHes onto <prefix>:pending. A consumer claims the oldest entry by
// atomically moving it to <prefix>:inflight (BLMOVE ... RIGHT LEFT), taking the
// event's session with SET NX on <prefix>:session:<id> and recording a lease in
// <prefix>:leases. An entry whose session is already taken goes back to the end
// of the pending list untouched.
//
// Acknowledging removes the entry from the in-flight list by value, which is
// unambiguous because every payload carries a unique event ID. Maintain reclaims
// entries whose lease expired: that is the single retry path, so an unfinished
// turn needs no cooperation from the consumer that dropped it. Events that
// exhaust their attempts land on <prefix>:dead.
type RedisQueue struct {
	client      redis.UniversalClient
	ownsClient  bool
	log         *slog.Logger
	prefix      string
	maxAttempts int
	dedupTTL    time.Duration
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
			DB:       cfg.DB,
		})
		ownsClient = true
	}

	return &RedisQueue{
		client:      client,
		ownsClient:  ownsClient,
		log:         cfg.Logger,
		prefix:      cfg.Prefix,
		maxAttempts: cfg.MaxAttempts,
		dedupTTL:    cfg.DedupTTL,
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

func (q *RedisQueue) key(suffix string) string { return q.prefix + ":" + suffix }

func (q *RedisQueue) pendingKey() string         { return q.key("pending") }
func (q *RedisQueue) inflightKey() string        { return q.key("inflight") }
func (q *RedisQueue) leasesKey() string          { return q.key("leases") }
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
	if ev.EnqueuedAt.IsZero() {
		ev.EnqueuedAt = time.Now().UTC()
	}

	if q.dedupTTL > 0 {
		fresh, err := q.client.SetNX(ctx, q.key("seen:"+ev.WorkID), ev.ID, q.dedupTTL).Result()
		if err != nil {
			return fmt.Errorf("queue: deduplicating %s: %w", ev.WorkID, err)
		}
		if !fresh {
			return ErrDuplicate
		}
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

		lease := &Lease{
			Event:    ev,
			Deadline: time.Now().Add(leaseTTL),
			payload:  payload,
			token:    newID(),
		}

		// Take the session: one turn per sandbox at a time.
		free, err := q.client.SetNX(ctx, q.sessionKey(ev.Session()), lease.token, leaseTTL).Result()
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

		if err := q.client.HSet(ctx, q.leasesKey(), ev.ID, encodeLease(lease.token, lease.Deadline)).Err(); err != nil {
			// Leave the entry in flight; Maintain adopts and retries it.
			return nil, fmt.Errorf("queue: recording lease for %s: %w", ev.WorkID, err)
		}
		return lease, nil
	}

	// Everything seen belongs to a session that is already running a turn.
	sleep(ctx, busyBackoff)
	return nil, nil
}

// ackScript removes an in-flight entry, its lease, and its hold on the session.
// It reports how many entries were removed, which is 0 when the lease was
// already reclaimed.
//
// KEYS: inflight, leases, session. ARGV: payload, event ID, token.
var ackScript = redis.NewScript(`
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
if removed == 0 then
  return 0
end
redis.call('HDEL', KEYS[2], ARGV[2])
if redis.call('GET', KEYS[3]) == ARGV[3] then
  redis.call('DEL', KEYS[3])
end
return removed
`)

// Ack removes a completed event from the queue and frees its session.
func (q *RedisQueue) Ack(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return errors.New("queue: nil lease")
	}
	removed, err := ackScript.Run(ctx, q.client,
		[]string{q.inflightKey(), q.leasesKey(), q.sessionKey(lease.Event.Session())},
		lease.payload, lease.Event.ID, lease.token,
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: acknowledging %s: %w", lease.Event.WorkID, err)
	}
	if removed == 0 {
		return ErrLeaseLost
	}
	return nil
}

// extendScript pushes out a lease deadline and the matching session hold, but
// only for the current holder.
//
// KEYS: leases, session. ARGV: event ID, token, new deadline millis, TTL millis.
var extendScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], ARGV[1])
if not cur then
  return 0
end
local prefix = ARGV[2] .. '|'
if string.sub(cur, 1, string.len(prefix)) ~= prefix then
  return 0
end
redis.call('HSET', KEYS[1], ARGV[1], prefix .. ARGV[3])
if redis.call('GET', KEYS[2]) == ARGV[2] then
  redis.call('PEXPIRE', KEYS[2], ARGV[4])
end
return 1
`)

// Extend keeps a lease and its session alive while the turn is still running.
// Consumers call it on a heartbeat; when the calls stop, the event is reclaimed.
func (q *RedisQueue) Extend(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return errors.New("queue: nil lease")
	}
	deadline := time.Now().Add(leaseTTL)

	held, err := extendScript.Run(ctx, q.client,
		[]string{q.leasesKey(), q.sessionKey(lease.Event.Session())},
		lease.Event.ID, lease.token, deadline.UnixMilli(), leaseTTL.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: extending lease for %s: %w", lease.Event.WorkID, err)
	}
	if held == 0 {
		return ErrLeaseLost
	}
	lease.Deadline = deadline
	return nil
}

// reclaimScript retries an in-flight entry whose lease expired, or dead-letters
// it, and frees the session it was holding.
//
// KEYS: inflight, leases, session, pending, dead.
// ARGV: old payload, event ID, token, new payload, dead flag ("1" or "0").
var reclaimScript = redis.NewScript(`
if redis.call('LREM', KEYS[1], 1, ARGV[1]) == 0 then
  return 0
end
redis.call('HDEL', KEYS[2], ARGV[2])
if redis.call('GET', KEYS[3]) == ARGV[3] then
  redis.call('DEL', KEYS[3])
end
if ARGV[5] == '1' then
  redis.call('LPUSH', KEYS[5], ARGV[4])
else
  redis.call('LPUSH', KEYS[4], ARGV[4])
end
return 1
`)

// Maintain reclaims events whose lease expired, retrying them or dead-lettering
// them once they run out of attempts. It is the only retry path, so consumers
// must call it periodically.
//
// An in-flight entry with no lease record belongs to a consumer that died
// between claiming and recording it. It is adopted with a fresh deadline so the
// next pass retries it through the normal expiry path, rather than being
// reclaimed underneath a consumer that is merely slow.
func (q *RedisQueue) Maintain(ctx context.Context) error {
	payloads, err := q.client.LRange(ctx, q.inflightKey(), 0, -1).Result()
	if err != nil {
		return fmt.Errorf("queue: listing in-flight events: %w", err)
	}
	leases, err := q.client.HGetAll(ctx, q.leasesKey()).Result()
	if err != nil {
		return fmt.Errorf("queue: listing leases: %w", err)
	}

	now := time.Now()
	live := make(map[string]bool, len(payloads))

	for _, payload := range payloads {
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		live[ev.ID] = true

		raw, ok := leases[ev.ID]
		if !ok {
			if err := q.client.HSet(ctx, q.leasesKey(), ev.ID,
				encodeLease("adopted", now.Add(leaseTTL))).Err(); err != nil {
				return fmt.Errorf("queue: adopting in-flight event %s: %w", ev.WorkID, err)
			}
			continue
		}

		token, deadline, err := decodeLease(raw)
		if err != nil || now.Before(deadline) {
			continue
		}
		if err := q.reclaim(ctx, ev, payload, token); err != nil {
			return err
		}
	}

	// Drop lease records whose event is no longer in flight.
	for id := range leases {
		if !live[id] {
			if err := q.client.HDel(ctx, q.leasesKey(), id).Err(); err != nil {
				return fmt.Errorf("queue: pruning lease %s: %w", id, err)
			}
		}
	}
	return nil
}

func (q *RedisQueue) reclaim(ctx context.Context, ev Event, payload, token string) error {
	next := ev
	next.Attempts++

	dead := "0"
	if next.Attempts >= q.maxAttempts {
		dead = "1"
	}

	retry, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("queue: encoding event: %w", err)
	}

	moved, err := reclaimScript.Run(ctx, q.client,
		[]string{q.inflightKey(), q.leasesKey(), q.sessionKey(ev.Session()), q.pendingKey(), q.deadKey()},
		payload, ev.ID, token, retry, dead,
	).Int64()
	if err != nil {
		return fmt.Errorf("queue: reclaiming %s: %w", ev.WorkID, err)
	}
	if moved == 0 {
		// Another consumer reclaimed it first.
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
	q.log.Info("Lease expired on unfinished work item; requeued",
		slog.String("work_id", ev.WorkID),
		slog.String("session_id", ev.Session()),
		slog.Int("attempts", next.Attempts),
	)
	return nil
}

// encodeLease renders a lease record as "<token>|<deadline unix millis>".
func encodeLease(token string, deadline time.Time) string {
	return token + "|" + strconv.FormatInt(deadline.UnixMilli(), 10)
}

func decodeLease(raw string) (token string, deadline time.Time, err error) {
	token, millis, ok := strings.Cut(raw, "|")
	if !ok {
		return "", time.Time{}, fmt.Errorf("queue: malformed lease record %q", raw)
	}
	ms, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("queue: malformed lease deadline %q: %w", raw, err)
	}
	return token, time.UnixMilli(ms), nil
}
