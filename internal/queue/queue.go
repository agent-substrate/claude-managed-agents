// Package queue provides a durable work queue for the Anthropic work items
// fetched by 'ant beta:worker poll'.
//
// The poller hook ('ate-antworker dispatch') enqueues an event and returns
// immediately; a consumer ('ate-antworker consume') claims the event, resumes
// the Substrate actor, runs the work, and acknowledges it. Because the event
// outlives the short-lived hook process, a crash between the poll and the work
// no longer loses the work item.
//
// Delivery is at-least-once. An event stays in the in-flight list until it is
// acknowledged; a consumer holds it under a lease that it renews while the work
// runs. Anything that stops the work — a failure, a killed pod, a lost network —
// stops the renewals, and the event is reclaimed once the lease expires.
//
// A claim also takes the event's session, and Claim never hands out two events
// for one session at a time, so two consumers cannot run work against the same
// sandbox concurrently.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Errors returned by the queue.
var (
	// ErrDuplicate is returned by Enqueue when deduplication is enabled and an
	// event with the same work ID was already enqueued within the dedup window.
	ErrDuplicate = errors.New("queue: duplicate work item")

	// ErrLeaseLost is returned by Ack and Extend when the lease is no longer
	// held, because it expired and the event was reclaimed.
	ErrLeaseLost = errors.New("queue: lease no longer held")
)

// Event is a single Anthropic work item waiting to be dispatched to an actor.
//
// Events are stored as JSON. Secrets are deliberately not part of the payload:
// the consumer reads its own Anthropic credentials from its environment rather
// than out of the queue.
type Event struct {
	// ID uniquely identifies this queue entry. It also keeps every serialized
	// payload distinct, which the bookkeeping relies on to remove exactly one
	// entry from the in-flight list.
	ID string `json:"id"`

	WorkID    string `json:"work_id"`
	SessionID string `json:"session_id,omitempty"`

	// Attempts counts how many times the event was claimed and left unfinished.
	// Exhausting them moves the event to the dead-letter list.
	Attempts int `json:"attempts"`
}

// Session returns the session the event belongs to, falling back to the work ID
// when the poller did not report one. It matches the fallback used when deriving
// the Substrate actor name.
func (e Event) Session() string {
	if e.SessionID != "" {
		return e.SessionID
	}
	return e.WorkID
}

// Lease is a claimed event, held by one consumer along with its session, until
// it is acknowledged or the lease expires.
type Lease struct {
	Event    Event
	Deadline time.Time

	// payload is the exact serialized entry sitting in the in-flight list.
	// Removing it by value is what acknowledges the event.
	payload string
	// token identifies the holder of both the lease and the session.
	token string
}

// newID returns a random identifier for an event or lease token.
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails catastrophically; fall back to the clock so
		// enqueueing never breaks on it.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// sleep waits for d unless ctx is canceled first.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
