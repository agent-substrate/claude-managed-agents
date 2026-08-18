package sessionmanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/valkey-io/valkey-go"
)

const (
	DefaultPollInterval = 1 * time.Second
	DefaultStreamKey    = "stream:events"
	DefaultGroupName    = "sessionmanager"
)

// Action represents the lifecycle action to take for a session.
type Action string

const (
	ActionResume  Action = "resume"
	ActionSuspend Action = "suspend"
)

// Event represents a session lifecycle event in the stream.
type Event struct {
	ID        string    `json:"id,omitempty"`
	Action    Action    `json:"action"`
	SessionID string    `json:"session_id"`
	WorkID    string    `json:"work_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Options configures a session Manager.
type Options struct {
	Client         valkey.Client
	ValkeyAddr     string
	ValkeyPassword string
	DisableCache   bool
	PollInterval   time.Duration
	StreamKey      string
	QueueKey       string // alias for StreamKey (backwards compatibility)
	GroupName      string
	ConsumerName   string
	Logger         *slog.Logger
	OnSuspend      func(ctx context.Context, event *Event) error
	OnResume       func(ctx context.Context, event *Event) error
}

// Manager coordinates session lifecycle events using Valkey Streams.
type Manager struct {
	vkClient     valkey.Client
	logger       *slog.Logger
	pollInterval time.Duration
	streamKey    string
	groupName    string
	consumerName string

	onSuspend func(ctx context.Context, event *Event) error
	onResume  func(ctx context.Context, event *Event) error
}

func defaultConsumerName() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "consumer"
	}
	return fmt.Sprintf("%s-%s", hostname, hex.EncodeToString(buf[:]))
}

// New creates a new session Manager.
func New(o Options) (*Manager, error) {
	vkClient := o.Client
	if vkClient == nil {
		if o.ValkeyAddr == "" {
			o.ValkeyAddr = os.Getenv("VALKEY_ADDR")
		}
		if o.ValkeyAddr == "" {
			o.ValkeyAddr = manifest.DefaultValkeyAddr
		}
		if o.ValkeyPassword == "" {
			o.ValkeyPassword = os.Getenv("VALKEY_PASSWORD")
		}

		clientOpt := valkey.ClientOption{
			InitAddress:  []string{o.ValkeyAddr},
			DisableCache: o.DisableCache,
		}
		if o.ValkeyPassword != "" {
			clientOpt.Password = o.ValkeyPassword
		}

		var err error
		vkClient, err = valkey.NewClient(clientOpt)
		if err != nil {
			return nil, fmt.Errorf("sessionmanager: connecting to valkey at %s: %w", o.ValkeyAddr, err)
		}
	}

	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pollInterval := o.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	streamKey := o.StreamKey
	if streamKey == "" {
		streamKey = o.QueueKey
	}
	if streamKey == "" {
		streamKey = DefaultStreamKey
	}
	groupName := o.GroupName
	if groupName == "" {
		groupName = DefaultGroupName
	}
	consumerName := o.ConsumerName
	if consumerName == "" {
		consumerName = defaultConsumerName()
	}

	return &Manager{
		vkClient:     vkClient,
		logger:       logger,
		pollInterval: pollInterval,
		streamKey:    streamKey,
		groupName:    groupName,
		consumerName: consumerName,
		onSuspend:    o.OnSuspend,
		onResume:     o.OnResume,
	}, nil
}

// Close closes the underlying Valkey client.
func (m *Manager) Close() error {
	if m.vkClient != nil {
		m.vkClient.Close()
	}
	return nil
}

// ensureGroup creates the consumer group on the stream if it does not already exist.
func (m *Manager) ensureGroup(ctx context.Context) error {
	cmd := m.vkClient.B().XgroupCreate().Key(m.streamKey).Group(m.groupName).Id("0").Mkstream().Build()
	err := m.vkClient.Do(ctx, cmd).Error()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("sessionmanager: creating consumer group %s on %s: %w", m.groupName, m.streamKey, err)
	}
	return nil
}

// QueueResume pushes a "resume" event to the stream.
func (m *Manager) QueueResume(ctx context.Context, sessionID, workID string) (*Event, error) {
	if sessionID == "" {
		return nil, errors.New("sessionmanager: session_id is required")
	}
	event := &Event{
		Action:    ActionResume,
		SessionID: sessionID,
		WorkID:    workID,
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("sessionmanager: marshaling resume event: %w", err)
	}
	cmd := m.vkClient.B().Xadd().Key(m.streamKey).Id("*").FieldValue().FieldValue("data", string(data)).Build()
	id, err := m.vkClient.Do(ctx, cmd).ToString()
	if err != nil {
		return nil, fmt.Errorf("sessionmanager: pushing resume event to stream %s: %w", m.streamKey, err)
	}
	event.ID = id
	return event, nil
}

// QueueSuspend pushes a "suspend" event to the stream.
func (m *Manager) QueueSuspend(ctx context.Context, sessionID string) (*Event, error) {
	if sessionID == "" {
		return nil, errors.New("sessionmanager: session_id is required")
	}
	event := &Event{
		Action:    ActionSuspend,
		SessionID: sessionID,
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("sessionmanager: marshaling suspend event: %w", err)
	}
	cmd := m.vkClient.B().Xadd().Key(m.streamKey).Id("*").FieldValue().FieldValue("data", string(data)).Build()
	id, err := m.vkClient.Do(ctx, cmd).ToString()
	if err != nil {
		return nil, fmt.Errorf("sessionmanager: pushing suspend event to stream %s: %w", m.streamKey, err)
	}
	event.ID = id
	return event, nil
}

func (m *Manager) parseEntry(entry valkey.XRangeEntry) (*Event, error) {
	dataStr, ok := entry.FieldValues["data"]
	if !ok {
		action := entry.FieldValues["action"]
		sessionID := entry.FieldValues["session_id"]
		workID := entry.FieldValues["work_id"]
		if sessionID != "" {
			return &Event{
				ID:        entry.ID,
				Action:    Action(action),
				SessionID: sessionID,
				WorkID:    workID,
			}, nil
		}
		return nil, fmt.Errorf("sessionmanager: missing data field in stream entry %s", entry.ID)
	}
	var event Event
	if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
		return nil, fmt.Errorf("sessionmanager: unmarshaling event from stream entry %s: %w", entry.ID, err)
	}
	event.ID = entry.ID
	return &event, nil
}

// pop reads the next event from the stream for this consumer.
// It first checks for any unacknowledged pending events for this consumer,
// and if none exist, reads the next new event from the stream.
// Returns nil if the stream has no pending or new events.
func (m *Manager) pop(ctx context.Context) (*Event, error) {
	if err := m.ensureGroup(ctx); err != nil {
		return nil, err
	}

	// 1. Check pending messages for this consumer (ID: "0")
	pendingCmd := m.vkClient.B().Xreadgroup().Group(m.groupName, m.consumerName).Count(1).Streams().Key(m.streamKey).Id("0").Build()
	res, err := m.vkClient.Do(ctx, pendingCmd).AsXRead()
	if err == nil && len(res[m.streamKey]) > 0 {
		return m.parseEntry(res[m.streamKey][0])
	}
	if err != nil && !valkey.IsValkeyNil(err) {
		return nil, fmt.Errorf("sessionmanager: reading pending stream entries: %w", err)
	}

	// 2. Read new messages for this consumer group (ID: ">")
	newCmd := m.vkClient.B().Xreadgroup().Group(m.groupName, m.consumerName).Count(1).Streams().Key(m.streamKey).Id(">").Build()
	res, err = m.vkClient.Do(ctx, newCmd).AsXRead()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionmanager: reading new stream entries: %w", err)
	}
	entries := res[m.streamKey]
	if len(entries) == 0 {
		return nil, nil
	}
	return m.parseEntry(entries[0])
}

// ack acknowledges an event in the stream so it is removed from the pending entries list (PEL).
func (m *Manager) ack(ctx context.Context, event *Event) error {
	if event == nil || event.ID == "" {
		return nil
	}
	return m.ackID(ctx, event.ID)
}

// ackID acknowledges an event by its stream entry ID.
func (m *Manager) ackID(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	cmd := m.vkClient.B().Xack().Key(m.streamKey).Group(m.groupName).Id(id).Build()
	return m.vkClient.Do(ctx, cmd).Error()
}

// Reconcile continuously pops events from the stream, dispatches them to OnResume/OnSuspend,
// and enforces that each event is acknowledged before moving on to the next.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.logger.Info("Starting session lifecycle manager stream consumer",
		slog.Duration("poll_interval", m.pollInterval),
		slog.String("stream_key", m.streamKey),
		slog.String("group", m.groupName),
		slog.String("consumer", m.consumerName),
	)

	if err := m.ensureGroup(ctx); err != nil {
		return fmt.Errorf("sessionmanager: ensuring consumer group: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		event, err := m.pop(ctx)
		if err != nil {
			m.logger.Error("Failed to pop event from stream", slog.Any("error", err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.pollInterval):
				continue
			}
		}

		if event == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.pollInterval):
				continue
			}
		}

		var handleErr error
		switch event.Action {
		case ActionResume:
			if m.onResume != nil {
				handleErr = m.onResume(ctx, event)
			}
		case ActionSuspend:
			if m.onSuspend != nil {
				handleErr = m.onSuspend(ctx, event)
			}
		default:
			m.logger.Warn("Unknown lifecycle event action",
				slog.String("action", string(event.Action)),
				slog.String("id", event.ID),
				slog.String("session_id", event.SessionID),
			)
		}

		if handleErr != nil {
			m.logger.Error("Processing lifecycle event failed",
				slog.String("id", event.ID),
				slog.String("action", string(event.Action)),
				slog.String("session_id", event.SessionID),
				slog.Any("error", handleErr),
			)
			// Do not ACK on failure so the event remains in PEL. Wait before retrying.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.pollInterval):
			}
			continue
		}

		// Acknowledge the event after successful processing before proceeding to the next
		if err := m.ack(ctx, event); err != nil {
			m.logger.Error("Failed to acknowledge event in stream",
				slog.String("id", event.ID),
				slog.String("session_id", event.SessionID),
				slog.Any("error", err),
			)
		}
	}
}
