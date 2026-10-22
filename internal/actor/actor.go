// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package actor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	DefaultIdleCheckInterval = 1 * time.Second
	DefaultIdleThreshold     = 3 * time.Second

	defaultSuspendTimeout = 30 * time.Second
	defaultNotifyTimeout  = 5 * time.Second
	defaultResumeCooldown = 3 * time.Second
)

// IdleStatus is the JSON payload returned by an actor's /idle HTTP endpoint.
type IdleStatus struct {
	IdleDuration string `json:"idle_duration"`
	LastActiveAt string `json:"last_active_at"`
}

// Manager coordinates the lifecycle of Substrate actors (creation, resume, suspend, GC).
type Manager struct {
	client     ateapipb.ControlClient
	template   string
	atespace   string
	routerAddr string
	// routerTransport dials atenet-router; nil uses http.DefaultTransport.
	routerTransport http.RoundTripper
	egress          EgressConfig
	logger          *slog.Logger

	mu          sync.Mutex
	lastResumed map[string]time.Time
}

// NewManager creates a new actor lifecycle Manager.
func NewManager(client ateapipb.ControlClient, template, atespace string) *Manager {
	if atespace == "" {
		atespace = manifest.DefaultNamespace
	}
	if template == "" {
		template = manifest.DefaultTemplate
	}
	return &Manager{
		client:      client,
		template:    template,
		atespace:    atespace,
		routerAddr:  manifest.DefaultAtenetRouterAddr,
		egress:      EgressConfig{CredentialURI: manifest.EnvironmentKeyCredentialURI(atespace)},
		logger:      slog.Default(),
		lastResumed: make(map[string]time.Time),
	}
}

// Atespace returns the configured atespace/namespace.
func (m *Manager) Atespace() string {
	return m.atespace
}

// RouterAddr returns the configured atenet router address.
func (m *Manager) RouterAddr() string {
	return m.routerAddr
}

// WithRouterAddr sets the atenet router address.
func (m *Manager) WithRouterAddr(routerAddr string) *Manager {
	if routerAddr != "" {
		m.routerAddr = routerAddr
	}
	return m
}

// WithRouterTransport sets the HTTP transport used to reach actors through
// atenet-router, e.g. the mTLS transport from RouterTransportFromFiles.
func (m *Manager) WithRouterTransport(t http.RoundTripper) *Manager {
	if t != nil {
		m.routerTransport = t
	}
	return m
}

// routerURL returns the router URL for path, defaulting to https:// when the
// configured address has no scheme.
func (m *Manager) routerURL(path string) string {
	routerAddr := m.routerAddr
	if !strings.HasPrefix(routerAddr, "http://") && !strings.HasPrefix(routerAddr, "https://") {
		routerAddr = "https://" + routerAddr
	}
	return strings.TrimSuffix(routerAddr, "/") + path
}

func (m *Manager) routerClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: m.routerTransport, Timeout: timeout}
}

// WithEgress sets the egress policy configuration applied to new actors.
// Empty fields keep their defaults.
func (m *Manager) WithEgress(cfg EgressConfig) *Manager {
	if cfg.AnthropicBaseURL != "" {
		m.egress.AnthropicBaseURL = cfg.AnthropicBaseURL
	}
	if cfg.CredentialURI != "" {
		m.egress.CredentialURI = cfg.CredentialURI
	}
	return m
}

// WithLogger sets the logger for the Manager.
func (m *Manager) WithLogger(logger *slog.Logger) *Manager {
	if logger != nil {
		m.logger = logger
	}
	return m
}

func (m *Manager) recordResume(actorName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastResumed == nil {
		m.lastResumed = make(map[string]time.Time)
	}
	m.lastResumed[actorName] = time.Now()
}

// EnsureAtespace creates the atespace if it doesn't already exist.
//
// ate-api authorizes calls before checking existence, and CreateAtespace
// requires global owner, so look the atespace up first: an atespace-scoped
// controller can then run against an atespace provisioned by an operator.
func (m *Manager) EnsureAtespace(ctx context.Context) error {
	_, err := m.client.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{
		Atespace: &ateapipb.ObjectRef{Name: m.atespace},
	})
	if err == nil {
		return nil
	}
	_, err = m.client.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{
			Metadata: &ateapipb.ResourceMetadata{
				Name: m.atespace,
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	return nil
}

// Resume ensures the actor for sessionID exists (creating it if needed) and resumes it if suspended.
func (m *Manager) Resume(ctx context.Context, sessionID string) error {
	actorRef := &ateapipb.ObjectRef{
		Atespace: m.atespace,
		Name:     ActorName(sessionID),
	}

	actor, err := m.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: actorRef})
	if status.Code(err) == codes.NotFound {
		m.logger.Info("Actor not found, creating new actor",
			slog.String("actor", actorRef.Name),
			slog.String("atespace", actorRef.Atespace),
		)
		if err := m.EnsureAtespace(ctx); err != nil {
			return fmt.Errorf("ensuring atespace %q: %w", actorRef.Atespace, err)
		}
		_, err = m.client.CreateActor(ctx, &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{
					Atespace: actorRef.Atespace,
					Name:     actorRef.Name,
				},
				ActorTemplate: &ateapipb.ObjectRef{
					Atespace: m.atespace,
					Name:     m.template,
				},
			},
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("creating actor %s: %w", actorRef.Name, err)
		}

		// The egress policy must exist before the actor is notified of work:
		// it is what injects the Anthropic environment key into the worker's
		// API calls, so without it the worker cannot authenticate.
		policy, err := BuildEgressPolicy(actorRef.Atespace, m.egress)
		if err != nil {
			return fmt.Errorf("building egress policy for actor %s: %w", actorRef.Name, err)
		}
		_, err = m.client.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
			Actor:        actorRef,
			EgressPolicy: policy,
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("creating egress policy for actor %s: %w", actorRef.Name, err)
		}

		m.recordResume(actorRef.Name)
		return nil
	} else if err != nil {
		return err
	}

	if actor != nil {
		if _, err := m.client.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actorRef}); err != nil {
			m.logger.Debug("ResumeActor result", slog.String("actor", actorRef.Name), slog.Any("error", err))
		}
		m.recordResume(actorRef.Name)
	}
	return nil
}

// Notify sends an HTTP POST request to the actor's /work endpoint via the Substrate router,
// retrying transient 5xx errors with backoff while the actor is warming up.
func (m *Manager) Notify(ctx context.Context, sessionID string, payload any) error {
	url := m.routerURL("/work")

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling work payload: %w", err)
	}

	timeoutMs := fmt.Sprintf("%d", defaultNotifyTimeout.Milliseconds())
	actorName := ActorName(sessionID)
	host := fmt.Sprintf("%s.%s.actors.resources.substrate.ate.dev", actorName, m.atespace)

	client := m.routerClient(defaultNotifyTimeout)

	type workResponse struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", m.atespace, actorName))
		req.Header.Set("x-envoy-upstream-rq-timeout-ms", timeoutMs)
		req.Header.Set("x-envoy-upstream-rq-per-try-timeout-ms", timeoutMs)
		req.Host = host

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("posting to router at %s (host %s): %w", url, host, err)
			continue
		}

		var workResp workResponse
		_ = json.NewDecoder(resp.Body).Decode(&workResp)
		resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("worker daemon via router at %s returned status %d", url, resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 400 || workResp.Status == "error" {
			if workResp.Error != "" {
				return fmt.Errorf("worker daemon via router at %s returned status %d: %s", url, resp.StatusCode, workResp.Error)
			}
			return fmt.Errorf("worker daemon via router at %s returned status %d", url, resp.StatusCode)
		}

		m.recordResume(actorName)
		m.logger.Info("Successfully notified actor of work item via router",
			slog.String("actor", actorName),
			slog.String("session_id", sessionID),
			slog.String("status", workResp.Status),
		)
		return nil
	}

	return lastErr
}

// Suspend suspends the actor for sessionID.
func (m *Manager) Suspend(ctx context.Context, sessionID string) error {
	actorRef := &ateapipb.ObjectRef{
		Atespace: m.atespace,
		Name:     ActorName(sessionID),
	}
	return m.SuspendActor(ctx, actorRef)
}

// SuspendActor suspends an actor by its ObjectRef. If the actor was resumed
// within defaultResumeCooldown, the suspension request is ignored.
func (m *Manager) SuspendActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	m.mu.Lock()
	lastResume, ok := m.lastResumed[actorRef.Name]
	m.mu.Unlock()

	if ok && time.Since(lastResume) < defaultResumeCooldown {
		m.logger.Debug("Ignoring suspend for actor within resume cooldown",
			slog.String("actor", actorRef.Name),
			slog.Duration("since_resume", time.Since(lastResume)),
			slog.Duration("cooldown", defaultResumeCooldown),
		)
		return nil
	}

	return m.suspendActor(ctx, actorRef)
}

func (m *Manager) suspendActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	suspendCtx, cancel := context.WithTimeout(ctx, defaultSuspendTimeout)
	defer cancel()

	_, err := m.client.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{Actor: actorRef})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("suspending actor %s: %w", actorRef.Name, err)
	}
	m.logger.Info("Actor suspended successfully", slog.String("actor", actorRef.Name))
	return nil
}

// actorIdleStatus queries an actor's /idle endpoint via the Substrate router.
func (m *Manager) actorIdleStatus(ctx context.Context, actorRef *ateapipb.ObjectRef) (*IdleStatus, error) {
	url := m.routerURL("/idle")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", actorRef.Atespace, actorRef.Name))
	req.Host = fmt.Sprintf("%s.%s.actors.resources.substrate.ate.dev", actorRef.Name, actorRef.Atespace)

	client := m.routerClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting /idle from router at %s (host %s): %w", url, req.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("actor /idle returned status %d", resp.StatusCode)
	}

	var idleResp IdleStatus
	if err := json.NewDecoder(resp.Body).Decode(&idleResp); err != nil {
		return nil, fmt.Errorf("decoding /idle response: %w", err)
	}
	return &idleResp, nil
}

func (m *Manager) revertActor(ctx context.Context, actorRef *ateapipb.ObjectRef) error {
	opCtx, cancel := context.WithTimeout(ctx, defaultSuspendTimeout)
	defer cancel()

	_, err := m.client.RevertActor(opCtx, &ateapipb.RevertActorRequest{Actor: actorRef})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("reverting actor %s: %w", actorRef.Name, err)
	}
	m.logger.Info("Reverted unreachable actor to suspended state", slog.String("actor", actorRef.Name))
	return nil
}

func (m *Manager) checkAndSuspendActor(ctx context.Context, actorRef *ateapipb.ObjectRef, threshold time.Duration) (bool, error) {
	m.mu.Lock()
	lastResume, ok := m.lastResumed[actorRef.Name]
	m.mu.Unlock()

	if ok && time.Since(lastResume) < threshold {
		m.logger.Debug("Skipping idle check: actor was resumed recently",
			slog.String("actor", actorRef.Name),
			slog.Duration("since_resume", time.Since(lastResume)),
		)
		return false, nil
	}

	idleResp, err := m.actorIdleStatus(ctx, actorRef)
	if err != nil {
		// Re-check lastResumed in case a new Resume/Notify arrived while /idle was in flight.
		m.mu.Lock()
		lastResume, ok = m.lastResumed[actorRef.Name]
		m.mu.Unlock()
		if ok && time.Since(lastResume) < threshold {
			return false, nil
		}

		m.logger.Warn("Actor /idle check failed, reverting unreachable actor",
			slog.String("actor", actorRef.Name),
			slog.Any("error", err),
		)
		if err := m.revertActor(ctx, actorRef); err != nil {
			return false, err
		}
		return true, nil
	}

	dur, err := time.ParseDuration(idleResp.IdleDuration)
	if err != nil {
		return false, fmt.Errorf("parsing idle duration %q: %w", idleResp.IdleDuration, err)
	}

	if dur < threshold {
		return false, nil
	}

	// Re-check lastResumed in case a new Resume/Notify arrived while /idle was in flight.
	m.mu.Lock()
	lastResume, ok = m.lastResumed[actorRef.Name]
	m.mu.Unlock()
	if ok && time.Since(lastResume) < threshold {
		return false, nil
	}

	m.logger.Info("Suspending idle actor",
		slog.String("actor", actorRef.Name),
		slog.String("idle_duration", idleResp.IdleDuration),
		slog.String("threshold", threshold.String()),
	)
	if err := m.suspendActor(ctx, actorRef); err != nil {
		return false, err
	}
	return true, nil
}

// SuspendIfIdle checks if the actor for sessionID has been idle for at least
// threshold and suspends it if so. It returns true if the actor is now
// suspended (or was already suspended / not found), and false if the actor is
// still active or below the idle threshold.
func (m *Manager) SuspendIfIdle(ctx context.Context, sessionID string, threshold time.Duration) (bool, error) {
	if threshold <= 0 {
		threshold = DefaultIdleThreshold
	}

	actorRef := &ateapipb.ObjectRef{
		Atespace: m.atespace,
		Name:     ActorName(sessionID),
	}

	actor, err := m.client.GetActor(ctx, &ateapipb.GetActorRequest{Actor: actorRef})
	if status.Code(err) == codes.NotFound {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting actor %s: %w", actorRef.Name, err)
	}
	if actor.GetStatus() != nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return true, nil
	}

	return m.checkAndSuspendActor(ctx, actorRef, threshold)
}

// suspendIdleActors lists all actors in the atespace, queries their /idle endpoint,
// and suspends any actor that has been idle for longer than the threshold.
func (m *Manager) suspendIdleActors(ctx context.Context, threshold time.Duration) error {
	resp, err := m.client.ListActors(ctx, &ateapipb.ListActorsRequest{
		Atespace: m.atespace,
	})
	if err != nil {
		return fmt.Errorf("listing actors in atespace %q: %w", m.atespace, err)
	}

	for _, actor := range resp.GetActors() {
		if actor == nil || actor.GetMetadata() == nil {
			continue
		}
		if actor.GetStatus() != nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
			continue
		}

		actorRef := &ateapipb.ObjectRef{
			Atespace: actor.GetMetadata().GetAtespace(),
			Name:     actor.GetMetadata().GetName(),
		}

		m.mu.Lock()
		_, ok := m.lastResumed[actorRef.Name]
		m.mu.Unlock()

		if !ok {
			// First time seeing this actor -> grant a full threshold grace period
			m.recordResume(actorRef.Name)
			continue
		}

		if _, err := m.checkAndSuspendActor(ctx, actorRef, threshold); err != nil {
			m.logger.Debug("Failed to check/suspend idle actor",
				slog.String("actor", actorRef.Name),
				slog.Any("error", err),
			)
		}
	}
	return nil
}

// StartGC starts a background loop that periodically scans for idle actors in the atespace
// and suspends them.
func (m *Manager) StartGC(ctx context.Context, checkInterval, idleThreshold time.Duration) {
	if checkInterval <= 0 {
		checkInterval = DefaultIdleCheckInterval
	}
	if idleThreshold <= 0 {
		idleThreshold = DefaultIdleThreshold
	}

	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		m.logger.Info("Starting actor idle garbage collector",
			slog.Duration("check_interval", checkInterval),
			slog.Duration("idle_threshold", idleThreshold),
			slog.String("namespace", m.atespace),
		)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.suspendIdleActors(ctx, idleThreshold); err != nil {
					m.logger.Warn("Actor GC error checking actors", slog.Any("error", err))
				}
			}
		}
	}()
}

// ActorName returns a deterministic RFC 1123 compliant actor name for a given Anthropic session ID.
func ActorName(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	hashSuffix := fmt.Sprintf("%x", h[:8]) // 16 hex chars

	// Clean the session ID into lowercase alphanumeric + hyphens
	var b strings.Builder
	for _, r := range strings.ToLower(sessionID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	prefix := strings.Trim(b.String(), "-")
	if len(prefix) > 40 {
		prefix = prefix[:40]
		prefix = strings.TrimRight(prefix, "-")
	}

	if prefix == "" {
		return "session-" + hashSuffix
	}
	return fmt.Sprintf("%s-%s", prefix, hashSuffix)
}

const (
	defaultTokenPath = "/var/run/secrets/tokens/ateapi-token"
	// ateapiServerName is the DNS name in ate-api's serving certificate.
	ateapiServerName = "api.ate-system.svc"
)

type tokenAuth struct {
	tokenFile string
}

func (t tokenAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	data, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, nil
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, nil
	}
	return map[string]string{
		"authorization": "Bearer " + token,
	}, nil
}

func (t tokenAuth) RequireTransportSecurity() bool {
	return false
}

// DialATE dials the Substrate ateapi gRPC service. Unless insecureConn is set,
// it verifies ate-api's serving certificate against the trust bundle at caFile
// before sending the ServiceAccount bearer token.
func DialATE(endpoint string, insecureConn bool, caFile string) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	dialOpts := []grpc.DialOption{}
	if insecureConn {
		creds = insecure.NewCredentials()
	} else {
		var err error
		creds, err = credentials.NewClientTLSFromFile(caFile, "")
		if err != nil {
			return nil, nil, fmt.Errorf("loading the ate-api trust bundle (mount the servicedns trust bundle or pass --servicedns-ca-file): %w", err)
		}
		// grpc verifies the server certificate against the connection's
		// authority. ate-api's certificate names api.ate-system.svc, not the
		// dialed address (api.ate-system.svc.cluster.local or a port-forward).
		dialOpts = append(dialOpts, grpc.WithAuthority(ateapiServerName))
	}
	dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))

	if _, err := os.Stat(defaultTokenPath); err == nil {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenAuth{tokenFile: defaultTokenPath}))
	}

	conn, err := grpc.NewClient(endpoint, dialOpts...)
	if err != nil {
		return nil, nil, err
	}

	return ateapipb.NewControlClient(conn), conn, nil
}
