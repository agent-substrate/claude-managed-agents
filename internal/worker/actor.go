package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	DefaultNamespace  = "ate-antworker"
	DefaultTemplate   = "ate-antworker-actor-template"
	DefaultAteapiAddr = "api.ate-system.svc.cluster.local:443"
)

func run(ctx context.Context, ateClient ateapipb.ControlClient, cfg Config, sessionID string) error {
	actorRef := &ateapipb.ObjectRef{
		Atespace: cfg.Namespace,
		Name:     actorName(sessionID),
	}

	_, err := ateClient.GetActor(ctx, &ateapipb.GetActorRequest{Actor: actorRef})
	if status.Code(err) == codes.NotFound {
		cfg.Logger.Info("Actor not found, creating new actor",
			slog.String("actor", actorRef.Name),
			slog.String("atespace", actorRef.Atespace),
		)
		if err := ensureAtespace(ctx, ateClient, actorRef.Atespace); err != nil {
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

	} else if err != nil {
		return err
	}
	if _, err := ateClient.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: actorRef}); err != nil {
		cfg.Logger.Debug("ResumeActor result", slog.String("actor", actorRef.Name), slog.Any("error", err))
	}
	return notify(ctx, cfg, actorRef, sessionID)
}

// ensureAtespace creates the Atespace if it doesn't already exist.
func ensureAtespace(ctx context.Context, ateClient ateapipb.ControlClient, atespace string) error {
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

// actorName returns a deterministic RFC 1123 compliant actor name for a given Anthropic session ID.
func actorName(sessionID string) string {
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

// notify sends an HTTP POST request to the actor via the Substrate atenet router.
func notify(ctx context.Context, cfg Config, actorRef *ateapipb.ObjectRef, sessionID string) error {
	routerAddr := cfg.AtenetRouterAddr
	if !strings.HasPrefix(routerAddr, "http://") && !strings.HasPrefix(routerAddr, "https://") {
		routerAddr = "http://" + routerAddr
	}
	url := fmt.Sprintf("%s/work", routerAddr)

	payload := WorkRequest{
		SessionID:      sessionID,
		EnvironmentID:  cfg.EnvironmentID,
		EnvironmentKey: cfg.EnvironmentKey,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling work payload: %w", err)
	}

	timeoutMs := fmt.Sprintf("%d", DefaultWorkTimeout.Milliseconds())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-envoy-upstream-rq-timeout-ms", timeoutMs)
	req.Header.Set("x-envoy-upstream-rq-per-try-timeout-ms", timeoutMs)
	req.Host = fmt.Sprintf("%s.%s.actors.resources.substrate.ate.dev", actorRef.Name, actorRef.Atespace)

	client := &http.Client{
		Timeout: DefaultWorkTimeout,
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to router at %s (host %s): %w", url, req.Host, err)
	}
	defer resp.Body.Close()

	var workResp WorkResponse
	_ = json.NewDecoder(resp.Body).Decode(&workResp)

	if resp.StatusCode >= 400 || workResp.Status == "error" {
		return fmt.Errorf("worker daemon via router at %s returned status %d: %s (output: %s)", url, resp.StatusCode, workResp.Error, workResp.Output)
	}

	cfg.Logger.Info("Successfully notified actor of work item via router",
		slog.String("actor", actorRef.Name),
		slog.String("session_id", sessionID),
		slog.String("status", workResp.Status),
	)

	return nil
}

func suspendActor(ctx context.Context, ateClient ateapipb.ControlClient, cfg Config, actorRef *ateapipb.ObjectRef, sessionID string) error {
	suspendCtx, cancel := context.WithTimeout(ctx, DefaultSuspendTimeout)
	defer cancel()

	_, err := ateClient.SuspendActor(suspendCtx, &ateapipb.SuspendActorRequest{Actor: actorRef})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("suspending actor %s: %w", actorRef.Name, err)
	}
	cfg.Logger.Info("Actor suspended successfully", slog.String("actor", actorRef.Name))
	return nil
}

// dialATE dials the Substrate ateapi gRPC service.
func dialATE(endpoint string, insecureConn bool) (ateapipb.ControlClient, *grpc.ClientConn, error) {
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
