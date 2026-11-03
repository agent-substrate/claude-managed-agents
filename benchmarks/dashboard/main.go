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

package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-substrate/claude-managed-agents/internal/actor"
	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	"github.com/valkey-io/valkey-go"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	appsv1 "k8s.io/api/apps/v1"
)

// Config configures the in-cluster benchmark dashboard server.
type Config struct {
	Addr             string
	Namespace        string
	AteapiAddr       string
	InsecureATE      bool
	CAFile           string
	ValkeyAddr       string
	ValkeyPassword   string
	DefaultWorkers   int
	DefaultControl   int
	MaxActorsInTable int
}

// Defaults populates default values on Config.
func (c *Config) Defaults() {
	if c.Addr == "" {
		c.Addr = cmp.Or(os.Getenv("DASHBOARD_ADDR"), ":8080")
	}
	if c.Namespace == "" {
		c.Namespace = cmp.Or(os.Getenv("ATE_NAMESPACE"), manifest.DefaultNamespace)
	}
	if c.AteapiAddr == "" {
		c.AteapiAddr = cmp.Or(os.Getenv("ATEAPI_ADDR"), manifest.DefaultAteapiAddr)
	}
	if c.ValkeyAddr == "" {
		c.ValkeyAddr = cmp.Or(os.Getenv("VALKEY_ADDR"), manifest.DefaultValkeyAddr)
	}
	if c.ValkeyPassword == "" {
		c.ValkeyPassword = os.Getenv("VALKEY_PASSWORD")
	}
	if c.DefaultWorkers <= 0 {
		c.DefaultWorkers = 50
	}
	if c.DefaultControl <= 0 {
		c.DefaultControl = 20
	}
	if c.MaxActorsInTable <= 0 {
		c.MaxActorsInTable = 150
	}
}

// ActorSummary holds counts of actors by lifecycle state.
type ActorSummary struct {
	Total          int            `json:"total"`
	Running        int            `json:"running"`
	Resuming       int            `json:"resuming"`
	Creating       int            `json:"creating"`
	Suspending     int            `json:"suspending"`
	Suspended      int            `json:"suspended"`
	Errored        int            `json:"errored"`
	Other          int            `json:"other"`
	ActiveTotal    int            `json:"active_total"`
	MultiplexRatio float64        `json:"multiplex_ratio"`
	StateBreakdown map[string]int `json:"state_breakdown"`
}

// WorkerSummary holds worker pool and controller replica metrics.
type WorkerSummary struct {
	WorkerPoolDesired int     `json:"workerpool_desired"`
	WorkerPoolReady   int     `json:"workerpool_ready"`
	WorkerPoolClaimed int     `json:"workerpool_claimed"`
	WorkerPoolIdle    int     `json:"workerpool_idle"`
	WorkerUtilization float64 `json:"worker_utilization_pct"`
	ControllerDesired int     `json:"controller_desired"`
	ControllerReady   int     `json:"controller_ready"`
}

// QueueSummary holds Valkey stream:events metrics.
type QueueSummary struct {
	Connected    bool  `json:"connected"`
	StreamLength int64 `json:"stream_length"`
	PendingCount int64 `json:"pending_count"`
	Lag          int64 `json:"lag"`
	EntriesRead  int64 `json:"entries_read"`
}

// ActorItem represents a single actor row in the dashboard table.
type ActorItem struct {
	Name       string `json:"name"`
	Atespace   string `json:"atespace"`
	State      string `json:"state"`
	WorkerPod  string `json:"worker_pod"`
	Version    int64  `json:"version"`
	AgeSeconds int64  `json:"age_seconds"`
	Age        string `json:"age"`
	HasError   bool   `json:"has_error"`
}

// DashboardStats is the JSON payload returned by GET /api/stats.
type DashboardStats struct {
	Timestamp string        `json:"timestamp"`
	Namespace string        `json:"namespace"`
	CollectMs float64       `json:"collect_ms"`
	Actors    ActorSummary  `json:"actors"`
	Workers   WorkerSummary `json:"workers"`
	Queue     QueueSummary  `json:"queue"`
	ActorList []ActorItem   `json:"actor_list"`
	Warnings  []string      `json:"warnings,omitempty"`
}

// Server serves the benchmark dashboard UI and JSON metrics API.
type Server struct {
	cfg       Config
	ateClient ateapipb.ControlClient
	vkClient  valkey.Client
	k8sClient *http.Client
	k8sToken  string
	logger    *slog.Logger

	rateMu     sync.Mutex
	lastWindow time.Time
	reqCount   int
}

// NewServer constructs a new dashboard Server.
func NewServer(cfg Config, ateClient ateapipb.ControlClient, vkClient valkey.Client, logger *slog.Logger) *Server {
	cfg.Defaults()
	if logger == nil {
		logger = slog.Default()
	}
	k8sHTTP, token := newInClusterK8sClient()
	return &Server{
		cfg:        cfg,
		ateClient:  ateClient,
		vkClient:   vkClient,
		k8sClient:  k8sHTTP,
		k8sToken:   token,
		logger:     logger,
		lastWindow: time.Now(),
	}
}

func newInClusterK8sClient() (*http.Client, string) {
	const (
		tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		caFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, ""
	}
	caBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, ""
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, ""
	}
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, strings.TrimSpace(string(tokenBytes))
}

// Handler returns the HTTP handler with security headers, method allow-listing, and rate limiting.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/styles.css", s.handleStyles)
	mux.HandleFunc("/app.js", s.handleAppJS)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/healthz", s.handleHealthz)

	// TODO(security): Add OAuth2/OIDC authentication when exposing dashboard outside trusted cluster networks.
	return s.securityMiddleware(mux)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("X-Frame-Options", "DENY")
		rw.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		rw.Header().Set("Cache-Control", "no-store")

		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if !s.allowRequest() {
			http.Error(rw, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(rw, req)
	})
}

func (s *Server) allowRequest() bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	if now.Sub(s.lastWindow) >= time.Second {
		s.lastWindow = now
		s.reqCount = 0
	}
	s.reqCount++
	return s.reqCount <= 100
}

func (s *Server) handleHealthz(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleIndex(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(rw, req)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(indexHTML))
}

func (s *Server) handleStyles(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "text/css; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(stylesCSS))
}

func (s *Server) handleAppJS(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte(appJS))
}

func (s *Server) handleStats(rw http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
	defer cancel()

	stats := s.collectStats(ctx)
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(stats)
}

func (s *Server) collectStats(ctx context.Context) DashboardStats {
	start := time.Now()
	now := time.Now().UTC()

	stats := DashboardStats{
		Timestamp: now.Format(time.RFC3339),
		Namespace: s.cfg.Namespace,
		Actors: ActorSummary{
			StateBreakdown: make(map[string]int),
		},
		Workers: WorkerSummary{
			WorkerPoolDesired: s.cfg.DefaultWorkers,
			WorkerPoolReady:   s.cfg.DefaultWorkers,
			ControllerDesired: s.cfg.DefaultControl,
			ControllerReady:   s.cfg.DefaultControl,
		},
		ActorList: []ActorItem{},
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)

	addWarning := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		stats.Warnings = append(stats.Warnings, msg)
	}

	// 1. Query Kubernetes deployments for WorkerPool and Controller replica counts.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wDesired, wReady, cDesired, cReady, ok := s.fetchK8sDeployments(ctx)
		if ok {
			mu.Lock()
			stats.Workers.WorkerPoolDesired = wDesired
			stats.Workers.WorkerPoolReady = wReady
			stats.Workers.ControllerDesired = cDesired
			stats.Workers.ControllerReady = cReady
			mu.Unlock()
		}
	}()

	// 2. Query ateapi.ListActors for actor counts, states, and claimed worker pods.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.ateClient == nil {
			addWarning("ateapi client not configured")
			return
		}
		resp, err := s.ateClient.ListActors(ctx, &ateapipb.ListActorsRequest{
			Atespace: s.cfg.Namespace,
		})
		if err != nil {
			s.logger.Warn("ListActors failed", slog.Any("error", err))
			addWarning("unavailable: ateapi ListActors")
			return
		}

		var (
			summary      = ActorSummary{StateBreakdown: make(map[string]int)}
			activeItems  []ActorItem
			dormantItems []ActorItem
			claimedPods  = make(map[string]struct{})
		)

		for _, a := range resp.GetActors() {
			if a == nil || a.GetMetadata() == nil {
				continue
			}
			summary.Total++
			st := a.GetStatus().GetState()
			stName := strings.TrimPrefix(st.String(), "ACTOR_STATE_")
			summary.StateBreakdown[stName]++

			pod := a.GetStatus().GetWorkerAssignment().GetWorkerPod()
			if pod != "" {
				claimedPods[pod] = struct{}{}
			}

			var ageSec int64
			if ct := a.GetMetadata().GetCreateTime(); ct != nil && ct.IsValid() {
				d := now.Sub(ct.AsTime())
				if d > 0 {
					ageSec = int64(d.Seconds())
				}
			}

			item := ActorItem{
				Name:       a.GetMetadata().GetName(),
				Atespace:   a.GetMetadata().GetAtespace(),
				State:      stName,
				WorkerPod:  pod,
				Version:    a.GetMetadata().GetVersion(),
				AgeSeconds: ageSec,
				Age:        formatAge(ageSec),
				HasError:   a.GetStatus().GetCrash() != nil,
			}

			switch st {
			case ateapipb.ActorState_ACTOR_STATE_RUNNING:
				summary.Running++
				summary.ActiveTotal++
				activeItems = append(activeItems, item)
			case ateapipb.ActorState_ACTOR_STATE_RESUMING, ateapipb.ActorState_ACTOR_STATE_REVERTING:
				summary.Resuming++
				summary.ActiveTotal++
				activeItems = append(activeItems, item)
			case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
				summary.Suspending++
				summary.ActiveTotal++
				activeItems = append(activeItems, item)
			case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
				summary.Suspended++
				dormantItems = append(dormantItems, item)
			case ateapipb.ActorState_ACTOR_STATE_CRASHED:
				summary.Errored++
				activeItems = append(activeItems, item)
			default:
				summary.Other++
				activeItems = append(activeItems, item)
			}
		}

		slices.SortFunc(activeItems, func(a, b ActorItem) int {
			if a.State != b.State {
				return strings.Compare(a.State, b.State)
			}
			return strings.Compare(a.Name, b.Name)
		})
		slices.SortFunc(dormantItems, func(a, b ActorItem) int {
			if a.AgeSeconds != b.AgeSeconds {
				return cmp.Compare(a.AgeSeconds, b.AgeSeconds)
			}
			return strings.Compare(a.Name, b.Name)
		})

		combined := activeItems
		if len(combined) < s.cfg.MaxActorsInTable && len(dormantItems) > 0 {
			rem := s.cfg.MaxActorsInTable - len(combined)
			if rem > len(dormantItems) {
				rem = len(dormantItems)
			}
			combined = append(combined, dormantItems[:rem]...)
		} else if len(combined) > s.cfg.MaxActorsInTable {
			combined = combined[:s.cfg.MaxActorsInTable]
		}

		mu.Lock()
		stats.Actors = summary
		stats.ActorList = combined
		claimed = len(claimedPods)
		if claimed < summary.ActiveTotal {
			claimed = summary.ActiveTotal
		}
		mu.Unlock()
	}()

	// 3. Query Valkey stream:events metrics.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.vkClient == nil {
			return
		}
		q := s.fetchQueueSummary(ctx)
		mu.Lock()
		stats.Queue = q
		mu.Unlock()
	}()

	wg.Wait()

	stats.Workers.WorkerPoolClaimed = claimed
	idle := stats.Workers.WorkerPoolReady - claimed
	if idle < 0 {
		idle = 0
	}
	stats.Workers.WorkerPoolIdle = idle
	if stats.Workers.WorkerPoolReady > 0 {
		stats.Workers.WorkerUtilization = float64(claimed) * 100.0 / float64(stats.Workers.WorkerPoolReady)
		stats.Actors.MultiplexRatio = float64(stats.Actors.Total) / float64(stats.Workers.WorkerPoolReady)
	}
	stats.CollectMs = float64(time.Since(start).Microseconds()) / 1000.0
	return stats
}

func (s *Server) fetchK8sDeployments(ctx context.Context) (wDesired, wReady, cDesired, cReady int, ok bool) {
	if s.k8sClient == nil || s.k8sToken == "" {
		return 0, 0, 0, 0, false
	}
	url := fmt.Sprintf("https://kubernetes.default.svc/apis/apps/v1/namespaces/%s/deployments", s.cfg.Namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	req.Header.Set("Authorization", "Bearer "+s.k8sToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.k8sClient.Do(req)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, 0, 0, false
	}

	var list appsv1.DeploymentList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return 0, 0, 0, 0, false
	}

	wDesired, wReady = s.cfg.DefaultWorkers, s.cfg.DefaultWorkers
	cDesired, cReady = s.cfg.DefaultControl, s.cfg.DefaultControl
	foundAny := false
	for _, d := range list.Items {
		switch d.Name {
		case "ate-ant-workerpool":
			if d.Spec.Replicas != nil {
				wDesired = int(*d.Spec.Replicas)
			}
			wReady = int(d.Status.ReadyReplicas)
			foundAny = true
		case "ate-ant-controller":
			if d.Spec.Replicas != nil {
				cDesired = int(*d.Spec.Replicas)
			}
			cReady = int(d.Status.ReadyReplicas)
			foundAny = true
		}
	}
	return wDesired, wReady, cDesired, cReady, foundAny
}

func (s *Server) fetchQueueSummary(ctx context.Context) QueueSummary {
	var q QueueSummary
	const (
		streamKey = "stream:events"
		groupName = "event"
	)

	xlenCmd := s.vkClient.B().Xlen().Key(streamKey).Build()
	if n, err := s.vkClient.Do(ctx, xlenCmd).AsInt64(); err == nil {
		q.Connected = true
		q.StreamLength = n
	} else if valkey.IsValkeyNil(err) {
		q.Connected = true
	}

	xpendingCmd := s.vkClient.B().Xpending().Key(streamKey).Group(groupName).Build()
	if arr, err := s.vkClient.Do(ctx, xpendingCmd).ToArray(); err == nil && len(arr) > 0 {
		q.Connected = true
		if p, err := arr[0].AsInt64(); err == nil {
			q.PendingCount = p
		}
	}

	xinfoCmd := s.vkClient.B().XinfoGroups().Key(streamKey).Build()
	if groups, err := s.vkClient.Do(ctx, xinfoCmd).ToArray(); err == nil {
		q.Connected = true
		for _, g := range groups {
			kv, err := g.AsMap()
			if err != nil {
				continue
			}
			if nameMsg, ok := kv["name"]; ok {
				if name, _ := nameMsg.ToString(); name != groupName {
					continue
				}
			}
			if pMsg, ok := kv["pending"]; ok {
				q.PendingCount, _ = pMsg.AsInt64()
			}
			if lMsg, ok := kv["lag"]; ok {
				q.Lag, _ = lMsg.AsInt64()
			}
			if rMsg, ok := kv["entries-read"]; ok {
				q.EntriesRead, _ = rMsg.AsInt64()
			}
		}
	}

	return q
}

func formatAge(sec int64) string {
	if sec <= 0 {
		return "0s"
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%dm", sec/3600, (sec%3600)/60)
}

// The dashboard frontend lives in web/ and is compiled into the binary.
var (
	//go:embed web/index.html
	indexHTML string
	//go:embed web/styles.css
	stylesCSS string
	//go:embed web/app.js
	appJS string
)

func main() {
	var cfg Config
	flag.StringVar(&cfg.Addr, "addr", "", "HTTP listen address (default :8080)")
	flag.StringVar(&cfg.Namespace, "namespace", manifest.DefaultNamespace, "Substrate atespace / Kubernetes namespace")
	flag.StringVar(&cfg.AteapiAddr, "ateapi-addr", manifest.DefaultAteapiAddr, "Substrate ateapi gRPC endpoint")
	flag.BoolVar(&cfg.InsecureATE, "insecure-ate", false, "Connect to ateapi without TLS")
	flag.StringVar(&cfg.CAFile, "servicedns-ca-file", actor.DefaultServiceDNSCAFile, "Trust bundle that verifies ateapi's serving certificate")
	flag.StringVar(&cfg.ValkeyAddr, "valkey-addr", "", "Valkey address (default $VALKEY_ADDR)")
	flag.IntVar(&cfg.DefaultWorkers, "default-workers", 50, "Fallback WorkerPool size when outside Kubernetes")
	flag.IntVar(&cfg.DefaultControl, "default-controllers", 20, "Fallback Controller replica count when outside Kubernetes")
	flag.Parse()

	cfg.Defaults()
	logger := slog.Default()

	ateClient, conn, err := actor.DialATE(cfg.AteapiAddr, cfg.InsecureATE, cfg.CAFile)
	if err != nil {
		logger.Error("Failed to dial ateapi", slog.String("addr", cfg.AteapiAddr), slog.Any("error", err))
		os.Exit(1)
	}
	defer conn.Close()

	var vkClient valkey.Client
	if cfg.ValkeyAddr != "" {
		opt := valkey.ClientOption{
			InitAddress:  []string{cfg.ValkeyAddr},
			DisableCache: true,
		}
		if cfg.ValkeyPassword != "" {
			opt.Password = cfg.ValkeyPassword
		}
		if c, err := valkey.NewClient(opt); err == nil {
			vkClient = c
			defer vkClient.Close()
		} else {
			logger.Warn("Could not connect to Valkey (continuing without queue stats)", slog.Any("error", err))
		}
	}

	srv := NewServer(cfg, ateClient, vkClient, logger)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
	}()

	logger.Info("Starting benchmark dashboard server",
		slog.String("addr", cfg.Addr),
		slog.String("namespace", cfg.Namespace),
		slog.String("ateapi", cfg.AteapiAddr),
		slog.String("valkey", cfg.ValkeyAddr),
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("Dashboard server exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}
