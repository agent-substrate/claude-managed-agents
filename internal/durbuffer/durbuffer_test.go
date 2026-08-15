package durbuffer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestErrNoEvents(t *testing.T) {
	if !errors.Is(ErrNoEvents, ErrNoEvents) {
		t.Errorf("ErrNoEvents identity check failed")
	}
}

func TestNewInvalidAddress(t *testing.T) {
	_, err := New(nil, "")
	if err == nil {
		t.Error("expected error with empty addresses, got nil")
	}
}

func TestEventJSON(t *testing.T) {
	ev := &Event{
		SessionID: "sesn_123",
		WorkID:    "work_456",
	}

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}

	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal event: %v", err)
	}

	if decoded.SessionID != ev.SessionID || decoded.WorkID != ev.WorkID {
		t.Errorf("got %+v, want %+v", decoded, ev)
	}
}

func TestPutNilEvent(t *testing.T) {
	b := &Buffer{}
	_, err := b.Put(context.Background(), nil)
	if err == nil {
		t.Error("expected error when putting nil event, got nil")
	}
}

func TestBufferLifecycleWithMiniRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	buf, err := New([]string{mr.Addr()}, "")
	if err != nil {
		t.Fatalf("New buffer failed: %v", err)
	}
	defer buf.Close()

	ev := &Event{
		SessionID: "sesn_lifecycle_001",
		WorkID:    "work_lifecycle_001",
	}

	id, err := buf.Put(ctx, ev)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if id == "" {
		t.Errorf("expected non-empty message ID from Put")
	}

	// Consume the event
	events, err := buf.Next(ctx, "worker-1", 10)
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].SessionID != ev.SessionID || events[0].WorkID != ev.WorkID {
		t.Errorf("got event %+v, want %+v", events[0], ev)
	}

	// Reading again should return ErrNoEvents
	_, err = buf.Next(ctx, "worker-1", 10)
	if !errors.Is(err, ErrNoEvents) {
		t.Errorf("expected ErrNoEvents, got %v", err)
	}

	// Acknowledge by work ID
	if err := buf.Ack(ctx, ev.WorkID); err != nil {
		t.Fatalf("Ack failed: %v", err)
	}
}

func TestBufferMultipleEventsWithMiniRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	buf, err := New([]string{mr.Addr()}, "")
	if err != nil {
		t.Fatalf("New buffer failed: %v", err)
	}
	defer buf.Close()

	events := []*Event{
		{SessionID: "sesn_1", WorkID: "work_1"},
		{SessionID: "sesn_2", WorkID: "work_2"},
		{SessionID: "sesn_3", WorkID: "work_3"},
	}

	for _, ev := range events {
		if _, err := buf.Put(ctx, ev); err != nil {
			t.Fatalf("Put(%+v) failed: %v", ev, err)
		}
	}

	// Batch read count 2
	batch1, err := buf.Next(ctx, "consumer-a", 2)
	if err != nil {
		t.Fatalf("Next(2) batch1 failed: %v", err)
	}
	if len(batch1) != 2 {
		t.Fatalf("expected 2 events in batch1, got %d", len(batch1))
	}
	if batch1[0].WorkID != "work_1" || batch1[1].WorkID != "work_2" {
		t.Errorf("unexpected batch1 events: %+v, %+v", batch1[0], batch1[1])
	}

	// Batch read remaining
	batch2, err := buf.Next(ctx, "consumer-a", 2)
	if err != nil {
		t.Fatalf("Next(2) batch2 failed: %v", err)
	}
	if len(batch2) != 1 {
		t.Fatalf("expected 1 event in batch2, got %d", len(batch2))
	}
	if batch2[0].WorkID != "work_3" {
		t.Errorf("unexpected batch2 event: %+v", batch2[0])
	}

	// Ack all work IDs
	if err := buf.Ack(ctx, "work_1", "work_2", "work_3"); err != nil {
		t.Fatalf("Ack batch failed: %v", err)
	}
}

func TestBufferReopenExistingGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	buf1, err := New([]string{mr.Addr()}, "")
	if err != nil {
		t.Fatalf("New buf1 failed: %v", err)
	}

	if _, err := buf1.Put(ctx, &Event{SessionID: "sesn_shared", WorkID: "work_shared"}); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	buf1.Close()

	// Re-open buffer with the same group name (should handle BUSYGROUP cleanly)
	buf2, err := New([]string{mr.Addr()}, "")
	if err != nil {
		t.Fatalf("New buf2 failed: %v", err)
	}
	defer buf2.Close()

	events, err := buf2.Next(ctx, "worker-2", 1)
	if err != nil {
		t.Fatalf("Next failed on re-opened buffer: %v", err)
	}
	if len(events) != 1 || events[0].WorkID != "work_shared" {
		t.Errorf("unexpected events: %+v", events)
	}
}

func TestBufferEmptyStream(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	buf, err := New([]string{mr.Addr()}, "")
	if err != nil {
		t.Fatalf("New buffer failed: %v", err)
	}
	defer buf.Close()

	_, err = buf.Next(ctx, "worker-1", 5)
	if !errors.Is(err, ErrNoEvents) {
		t.Errorf("expected ErrNoEvents for empty stream, got %v", err)
	}
}

func TestBufferWithPassword(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("my-secret-pw")
	ctx := context.Background()

	// Should fail with wrong password
	_, err := New([]string{mr.Addr()}, "wrong-pw")
	if err == nil {
		t.Error("expected error with wrong password, got nil")
	}

	// Should succeed with correct password
	buf, err := New([]string{mr.Addr()}, "my-secret-pw")
	if err != nil {
		t.Fatalf("New with correct password failed: %v", err)
	}
	defer buf.Close()

	if _, err := buf.Put(ctx, &Event{SessionID: "sesn_auth", WorkID: "work_auth"}); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	events, err := buf.Next(ctx, "worker-auth", 1)
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if len(events) != 1 || events[0].WorkID != "work_auth" {
		t.Errorf("unexpected events: %+v", events)
	}
}
