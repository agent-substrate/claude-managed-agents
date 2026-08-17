package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/valkey-io/valkey-go"
)

const (
	DefaultPollInterval = 1 * time.Second
	DefaultQueueKey     = "queue:events"
)

// Action represents the lifecycle action to take for a session.
type Action string

const (
	ActionResume  Action = "resume"
	ActionSuspend Action = "suspend"
)

// Event represents a session lifecycle event in the queue.
type Event struct {
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
	QueueKey       string
	Logger         *slog.Logger
}

// Manager coordinates session lifecycle events using a Valkey queue.
type Manager struct {
	vkClient     valkey.Client
	logger       *slog.Logger
	pollInterval time.Duration
	queueKey     string

	OnSuspend func(ctx context.Context, event *Event) error
	OnResume  func(ctx context.Context, event *Event) error
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
	queueKey := o.QueueKey
	if queueKey == "" {
		queueKey = DefaultQueueKey
	}

	return &Manager{
		vkClient:     vkClient,
		logger:       logger,
		pollInterval: pollInterval,
		queueKey:     queueKey,
	}, nil
}

// Close closes the underlying Valkey client.
func (m *Manager) Close() error {
	if m.vkClient != nil {
		m.vkClient.Close()
	}
	return nil
}

// QueueResume pushes a "resume" event to the queue.
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
	cmd := m.vkClient.B().Lpush().Key(m.queueKey).Element(string(data)).Build()
	if err := m.vkClient.Do(ctx, cmd).Error(); err != nil {
		return nil, fmt.Errorf("sessionmanager: pushing resume event to %s: %w", m.queueKey, err)
	}
	return event, nil
}

// QueueSuspend pushes a "suspend" event to the queue.
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
	cmd := m.vkClient.B().Lpush().Key(m.queueKey).Element(string(data)).Build()
	if err := m.vkClient.Do(ctx, cmd).Error(); err != nil {
		return nil, fmt.Errorf("sessionmanager: pushing suspend event to %s: %w", m.queueKey, err)
	}
	return event, nil
}

// Pop pops the next event from the queue. Returns nil if the queue is empty.
func (m *Manager) Pop(ctx context.Context) (*Event, error) {
	cmd := m.vkClient.B().Rpop().Key(m.queueKey).Build()
	val, err := m.vkClient.Do(ctx, cmd).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, err
	}
	var event Event
	if err := json.Unmarshal([]byte(val), &event); err != nil {
		return nil, fmt.Errorf("sessionmanager: unmarshaling event: %w", err)
	}
	return &event, nil
}

// Run continuously pops events from the queue and dispatches them to OnResume/OnSuspend.
func (m *Manager) Run(ctx context.Context) error {
	m.logger.Info("Starting session lifecycle manager queue consumer",
		slog.Duration("poll_interval", m.pollInterval),
		slog.String("queue_key", m.queueKey),
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		event, err := m.Pop(ctx)
		if err != nil {
			m.logger.Error("Failed to pop event from queue", slog.Any("error", err))
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

		switch event.Action {
		case ActionResume:
			if m.OnResume != nil {
				if err := m.OnResume(ctx, event); err != nil {
					m.logger.Error("OnResume failed",
						slog.String("session_id", event.SessionID),
						slog.String("work_id", event.WorkID),
						slog.Any("error", err),
					)
				}
			}
		case ActionSuspend:
			if m.OnSuspend != nil {
				if err := m.OnSuspend(ctx, event); err != nil {
					m.logger.Error("OnSuspend failed",
						slog.String("session_id", event.SessionID),
						slog.Any("error", err),
					)
				}
			}
		default:
			m.logger.Warn("Unknown lifecycle event action",
				slog.String("action", string(event.Action)),
				slog.String("session_id", event.SessionID),
			)
		}
	}
}
