package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	DefaultNamespace        = "anthropic-worker"
	DefaultTemplate         = "anthropic-worker-template"
	DefaultWorkdir          = "/workspace"
	DefaultAteapiAddr       = "api.ate-system.svc.cluster.local:443"
	DefaultAtenetRouterAddr = "atenet-router.ate-system.svc.cluster.local:80"
	DefaultSuspendTimeout   = 30 * time.Second
	DefaultIdleTimeout      = 30 * time.Second
)

// Config defines the configuration for Substrate actor dispatching.
type Config struct {
	EnvironmentID    string
	EnvironmentKey   string
	APIKey           string
	AteapiAddr       string
	AtenetRouterAddr string
	Namespace        string
	Template         string
	Workdir          string
	Insecure         bool
	IdleTimeout      time.Duration
	Logger           *slog.Logger

	// AteClient can be provided to override the default gRPC client (e.g. in tests).
	AteClient ateapipb.ControlClient
}

// Defaults fills in default values for unset fields.
func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Template == "" {
		c.Template = DefaultTemplate
	}
	if c.Workdir == "" {
		if _, err := os.Stat(DefaultWorkdir); err == nil {
			c.Workdir = DefaultWorkdir
		} else {
			c.Workdir = "."
		}
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = DefaultAteapiAddr
	}
	if c.AtenetRouterAddr == "" {
		c.AtenetRouterAddr = DefaultAtenetRouterAddr
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Validate ensures required configuration fields are set.
func (c *Config) Validate() error {
	c.Defaults()
	if c.EnvironmentID == "" {
		return errors.New("EnvironmentID is required")
	}
	return nil
}

// DispatchRequest contains the work item details to dispatch to a Substrate actor.
type DispatchRequest struct {
	WorkID         string        `json:"work_id"`
	SessionID      string        `json:"session_id"`
	EnvironmentID  string        `json:"environment_id,omitempty"`
	EnvironmentKey string        `json:"environment_key,omitempty"`
	Wait           bool          `json:"wait"`
	IdleTimeout    time.Duration `json:"idle_timeout,omitempty"`
}

// DispatchWork receives a single Anthropic work item (e.g. from 'ant beta:worker poll --on-work')
// and dispatches it to the corresponding Substrate actor via the atenet router.
// If req.Wait is true, it waits for turn completion and suspends the actor before returning.
func DispatchWork(ctx context.Context, cfg Config, req DispatchRequest) error {
	cfg.Defaults()
	logger := cfg.Logger

	if req.WorkID == "" {
		return fmt.Errorf("work_id is required")
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = req.WorkID
	}

	aName := ActorName(sessionID)
	actorRef := &ateapipb.ObjectRef{
		Atespace: cfg.Namespace,
		Name:     aName,
	}

	logger.Info("Dispatching work item to Substrate actor",
		slog.String("work_id", req.WorkID),
		slog.String("session_id", sessionID),
		slog.String("actor", aName),
		slog.String("namespace", cfg.Namespace),
		slog.Bool("wait", req.Wait),
	)

	var (
		ateClient ateapipb.ControlClient
		conn      *grpc.ClientConn
		err       error
	)

	if cfg.AteClient != nil {
		ateClient = cfg.AteClient
	} else {
		ateClient, conn, err = DialAteAPI(cfg.AteapiAddr, cfg.Insecure)
		if err != nil {
			return fmt.Errorf("dialing ateapi at %s: %w", cfg.AteapiAddr, err)
		}
		defer conn.Close()
	}

	// Ensure actor exists and execute turn synchronously via waker
	if err := EnsureActorRegistered(ctx, ateClient, cfg, actorRef, req.WorkID, sessionID); err != nil {
		return fmt.Errorf("dispatching work to actor: %w", err)
	}

	logger.Info("Work item completed successfully on actor",
		slog.String("work_id", req.WorkID),
		slog.String("session_id", sessionID),
		slog.String("actor", aName),
	)

	if !req.Wait {
		return nil
	}

	// Turn completed; suspend actor to free WorkerPool slot
	logger.Info("Turn completed; suspending actor", slog.String("actor", aName))
	if err := SuspendActor(ctx, ateClient, actorRef); err != nil {
		return fmt.Errorf("suspending actor after turn completion: %w", err)
	}
	logger.Info("Actor suspended successfully", slog.String("actor", aName))
	return nil
}

// EnsureActorRegistered ensures the atespace and actor exist in Substrate,
// and notifies the actor via the Substrate atenet router (which automatically wakes/resumes it).
func EnsureActorRegistered(ctx context.Context, ateClient ateapipb.ControlClient, cfg Config, actorRef *ateapipb.ObjectRef, workID, sessionID string) error {
	_, err := ateClient.GetActor(ctx, &ateapipb.GetActorRequest{Actor: actorRef})
	if err == nil {
		return NotifyActorWork(ctx, cfg, actorRef, workID, sessionID)
	}

	if status.Code(err) == codes.NotFound {
		cfg.Logger.Info("Actor not found, registering new actor",
			slog.String("actor", actorRef.Name),
			slog.String("atespace", actorRef.Atespace),
		)

		if err := EnsureAtespace(ctx, ateClient, actorRef.Atespace); err != nil {
			return fmt.Errorf("ensuring atespace %q: %w", actorRef.Atespace, err)
		}

		_, err = ateClient.CreateActor(ctx, &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{
					Atespace: actorRef.Atespace,
					Name:     actorRef.Name,
				},
				ActorTemplateNamespace: cfg.Namespace,
				ActorTemplateName:      cfg.Template,
			},
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("creating actor %s: %w", actorRef.Name, err)
		}

		return NotifyActorWork(ctx, cfg, actorRef, workID, sessionID)
	}

	return err
}

// SuspendActor suspends the specified actor in Substrate.
func SuspendActor(ctx context.Context, ateClient ateapipb.ControlClient, actorRef *ateapipb.ObjectRef) error {
	suspendCtx, cancel := context.WithTimeout(ctx, DefaultSuspendTimeout)
	defer cancel()

	_, err := ateClient.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{Actor: actorRef})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	return nil
}

// EnsureAtespace creates the Atespace if it doesn't already exist.
func EnsureAtespace(ctx context.Context, ateClient ateapipb.ControlClient, atespace string) error {
	_, err := ateClient.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{
			Metadata: &ateapipb.ResourceMetadata{
				Name: atespace,
			},
		},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	return nil
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

// DialAteAPI dials the Substrate ateapi gRPC service.
func DialAteAPI(endpoint string, insecureConn bool) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if insecureConn {
		creds = insecure.NewCredentials()
	} else {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, err
	}

	return ateapipb.NewControlClient(conn), conn, nil
}

// NotifyActorWork sends an HTTP POST request to the actor via the Substrate atenet router.
func NotifyActorWork(ctx context.Context, cfg Config, actorRef *ateapipb.ObjectRef, workID, sessionID string) error {
	routerAddr := cfg.AtenetRouterAddr
	if !strings.HasPrefix(routerAddr, "http://") && !strings.HasPrefix(routerAddr, "https://") {
		routerAddr = "http://" + routerAddr
	}
	url := fmt.Sprintf("%s/dispatch", routerAddr)

	payload := map[string]string{
		"work_id":    workID,
		"session_id": sessionID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling work payload: %w", err)
	}

	backoff := wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Steps:    6,
		Cap:      2 * time.Second,
	}

	var lastErr error
	err = wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Host = fmt.Sprintf("%s.%s.actors.resources.substrate.ate.dev", actorRef.Name, actorRef.Atespace)

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("posting to router at %s (host %s): %w", url, req.Host, err)
			return false, nil
		}
		resp.Body.Close()

		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("waker via router at %s returned status %d", url, resp.StatusCode)
			return false, nil
		}

		cfg.Logger.Info("Successfully notified actor of work item via router",
			slog.String("actor", actorRef.Name),
			slog.String("work_id", workID),
			slog.String("session_id", sessionID),
			slog.String("router", cfg.AtenetRouterAddr),
			slog.String("host", req.Host),
		)
		return true, nil
	})

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if lastErr != nil {
			return lastErr
		}
		return err
	}
	return nil
}
