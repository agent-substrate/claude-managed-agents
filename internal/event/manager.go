package event

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
	defaultPollInterval = 1 * time.Second
	defaultStreamKey    = "stream:events"
	defaultGroupName    = "event"
)

// Action represents the lifecycle action to take for a session.
type Action string

const (
	ActionResume  Action = "resume"
	ActionSuspend Action = "suspend"
)

// Event represents a session lifecycle event in the stream.
type Event struct {
	ID        string `json:"id,omitempty"`
	Action    Action `json:"action"`
	SessionID string `json:"session_id"`
	WorkID    string `json:"work_id,omitempty"`
	// LastHeartbeat is the lease token from the poller's first heartbeat on
	// WorkID; the worker that services the item must quote it as its first
	// expected_last_heartbeat.
	LastHeartbeat string    `json:"last_heartbeat,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ValkeyConfig configures the Valkey client connection.
type ValkeyConfig struct {
	Client       valkey.Client
	Addr         string
	Password     string
	DisableCache bool
}

// ManagerConfig configures a session Manager.
type ManagerConfig struct {
	Valkey       ValkeyConfig
	PollInterval time.Duration
	StreamKey    string
	QueueKey     string // alias for StreamKey (backwards compatibility)
	GroupName    string
	ConsumerName string
	Logger       *slog.Logger
	OnSuspend    func(ctx context.Context, event *Event) error
	OnResume     func(ctx context.Context, event *Event) error
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
func New(cfg ManagerConfig) (*Manager, error) {
	vkClient := cfg.Valkey.Client
	if vkClient == nil {
		addr := cfg.Valkey.Addr
		if addr == "" {
			addr = os.Getenv("VALKEY_ADDR")
		}
		if addr == "" {
			addr = manifest.DefaultValkeyAddr
		}
		password := cfg.Valkey.Password
		if password == "" {
			password = os.Getenv("VALKEY_PASSWORD")
		}

		clientOpt := valkey.ClientOption{
			InitAddress:  []string{addr},
			DisableCache: cfg.Valkey.DisableCache,
		}
		if password != "" {
			clientOpt.Password = password
		}

		var err error
		vkClient, err = valkey.NewClient(clientOpt)
		if err != nil {
			return nil, fmt.Errorf("event: connecting to valkey at %s: %w", addr, err)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	streamKey := cfg.StreamKey
	if streamKey == "" {
		streamKey = cfg.QueueKey
	}
	if streamKey == "" {
		streamKey = defaultStreamKey
	}
	groupName := cfg.GroupName
	if groupName == "" {
		groupName = defaultGroupName
	}
	consumerName := cfg.ConsumerName
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
		onSuspend:    cfg.OnSuspend,
		onResume:     cfg.OnResume,
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
		return fmt.Errorf("event: creating consumer group %s on %s: %w", m.groupName, m.streamKey, err)
	}
	return nil
}

// QueueResume pushes a "resume" event to the stream. lastHeartbeat is the
// lease token for workID (see Event.LastHeartbeat).
func (m *Manager) QueueResume(ctx context.Context, sessionID, workID, lastHeartbeat string) (*Event, error) {
	if sessionID == "" {
		return nil, errors.New("event: session_id is required")
	}
	event := &Event{
		Action:        ActionResume,
		SessionID:     sessionID,
		WorkID:        workID,
		LastHeartbeat: lastHeartbeat,
		CreatedAt:     time.Now().UTC(),
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("event: marshaling resume event: %w", err)
	}
	cmd := m.vkClient.B().Xadd().Key(m.streamKey).Id("*").FieldValue().FieldValue("data", string(data)).Build()
	id, err := m.vkClient.Do(ctx, cmd).ToString()
	if err != nil {
		return nil, fmt.Errorf("event: pushing resume event to stream %s: %w", m.streamKey, err)
	}
	event.ID = id
	return event, nil
}

// QueueSuspend pushes a "suspend" event to the stream.
func (m *Manager) QueueSuspend(ctx context.Context, sessionID string) (*Event, error) {
	if sessionID == "" {
		return nil, errors.New("event: session_id is required")
	}
	event := &Event{
		Action:    ActionSuspend,
		SessionID: sessionID,
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("event: marshaling suspend event: %w", err)
	}
	cmd := m.vkClient.B().Xadd().Key(m.streamKey).Id("*").FieldValue().FieldValue("data", string(data)).Build()
	id, err := m.vkClient.Do(ctx, cmd).ToString()
	if err != nil {
		return nil, fmt.Errorf("event: pushing suspend event to stream %s: %w", m.streamKey, err)
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
		lastHeartbeat := entry.FieldValues["last_heartbeat"]
		if sessionID != "" {
			return &Event{
				ID:            entry.ID,
				Action:        Action(action),
				SessionID:     sessionID,
				WorkID:        workID,
				LastHeartbeat: lastHeartbeat,
			}, nil
		}
		return nil, fmt.Errorf("event: missing data field in stream entry %s", entry.ID)
	}
	var event Event
	if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
		return nil, fmt.Errorf("event: unmarshaling event from stream entry %s: %w", entry.ID, err)
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
		return nil, fmt.Errorf("event: reading pending stream entries: %w", err)
	}

	// 2. Read new messages for this consumer group (ID: ">")
	newCmd := m.vkClient.B().Xreadgroup().Group(m.groupName, m.consumerName).Count(1).Streams().Key(m.streamKey).Id(">").Build()
	res, err = m.vkClient.Do(ctx, newCmd).AsXRead()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("event: reading new stream entries: %w", err)
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
		return fmt.Errorf("event: ensuring consumer group: %w", err)
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
