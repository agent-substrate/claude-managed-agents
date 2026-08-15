package durbuffer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
)

var ErrNoEvents = errors.New("no events available to consume")

const (
	streamKey = "ate-antworker"
	groupName = "ate-antworker"
)

type Buffer struct {
	client valkey.Client
}

type Event struct {
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id"`
}

func (b *Buffer) workIDKey() string {
	return streamKey + ":work_ids"
}

// New initializes the Valkey buffer and creates a Consumer Group if it doesn't exist.
func New(addrs []string, password string) (*Buffer, error) {
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  addrs,
		Password:     password,
		DisableCache: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Valkey: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// MKSTREAM creates the stream automatically if it doesn't exist.
	cmd := client.B().XgroupCreate().Key(streamKey).Group(groupName).Id("0").Mkstream().Build()
	if err := client.Do(ctx, cmd).Error(); err != nil {
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			client.Close()
			return nil, fmt.Errorf("failed to create consumer group: %w", err)
		}
	}

	return &Buffer{
		client: client,
	}, nil
}

// Put writes the unacknowledged event payload directly to Valkey's durable stream.
func (b *Buffer) Put(ctx context.Context, event *Event) (string, error) {
	if event == nil {
		return "", errors.New("cannot put nil event")
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("failed to marshal event: %w", err)
	}

	cmd := b.client.B().Xadd().Key(streamKey).Id("*").FieldValue().FieldValue("payload", string(payload)).Build()
	id, err := b.client.Do(ctx, cmd).ToString()
	if err != nil {
		return "", fmt.Errorf("failed to write to stream: %w", err)
	}

	if event.WorkID != "" {
		hsetCmd := b.client.B().Hset().Key(b.workIDKey()).FieldValue().FieldValue(event.WorkID, id).Build()
		if err := b.client.Do(ctx, hsetCmd).Error(); err != nil {
			return id, fmt.Errorf("failed to record work_id index: %w", err)
		}
	}

	return id, nil
}

// Next fetches unacknowledged events for a specific worker from the consumer group.
func (b *Buffer) Next(ctx context.Context, consumerName string, count int64) ([]*Event, error) {
	cmd := b.client.B().Xreadgroup().Group(groupName, consumerName).Count(count).Streams().Key(streamKey).Id(">").Build()

	res, err := b.client.Do(ctx, cmd).AsXRead()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, ErrNoEvents
		}
		return nil, fmt.Errorf("failed reading from group: %w", err)
	}

	messages, ok := res[streamKey]
	if !ok || len(messages) == 0 {
		return nil, ErrNoEvents
	}

	var events []*Event
	for _, msg := range messages {
		var ev Event
		if payload, ok := msg.FieldValues["payload"]; ok {
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
		} else {
			ev.SessionID = msg.FieldValues["session_id"]
			ev.WorkID = msg.FieldValues["work_id"]
		}
		events = append(events, &ev)
	}

	if len(events) == 0 {
		return nil, ErrNoEvents
	}

	return events, nil
}

// Ack explicitly acknowledges events by workID, removing them from the Pending Entries List (PEL).
func (b *Buffer) Ack(ctx context.Context, workIDs ...string) error {
	if len(workIDs) == 0 {
		return nil
	}

	hmgetCmd := b.client.B().Hmget().Key(b.workIDKey()).Field(workIDs...).Build()
	values, err := b.client.Do(ctx, hmgetCmd).AsStrSlice()
	if err != nil {
		return fmt.Errorf("failed to lookup message IDs for work IDs: %w", err)
	}

	var messageIDs []string
	for i, val := range values {
		if val != "" {
			messageIDs = append(messageIDs, val)
		} else if i < len(workIDs) && strings.Contains(workIDs[i], "-") {
			// Fallback in case a raw stream message ID was passed.
			messageIDs = append(messageIDs, workIDs[i])
		}
	}

	if len(messageIDs) > 0 {
		cmd := b.client.B().Xack().Key(streamKey).Group(groupName).Id(messageIDs...).Build()
		if err := b.client.Do(ctx, cmd).Error(); err != nil {
			return fmt.Errorf("failed to ack message(s): %w", err)
		}
	}

	hdelCmd := b.client.B().Hdel().Key(b.workIDKey()).Field(workIDs...).Build()
	_ = b.client.Do(ctx, hdelCmd).Error()

	return nil
}

// Close gracefully closes the Valkey client connection.
func (b *Buffer) Close() {
	b.client.Close()
}
