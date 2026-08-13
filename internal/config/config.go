// Package config manages configuration for the Substrate Anthropic worker.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultWorkdir is the standard working directory in self-hosted sandboxes.
const DefaultWorkdir = "/workspace"

// DefaultListenAddr is the default port for health probes and webhooks.
const DefaultListenAddr = ":8080"

// Mode represents the worker operating mode.
type Mode string

const (
	// ModePoll runs continuous long-polling on the Anthropic work queue.
	ModePoll Mode = "poll"
	// ModeWebhook starts an HTTP server listening for Anthropic webhooks.
	ModeWebhook Mode = "webhook"
	// ModeHybrid runs both the poller and the webhook receiver.
	ModeHybrid Mode = "hybrid"
)

// Config holds runtime configuration for the Anthropic worker.
type Config struct {
	// EnvironmentID is the Anthropic environment ID (e.g. "env_...").
	EnvironmentID string `json:"environment_id"`

	// EnvironmentKey is the bearer token used to authenticate to Anthropic.
	EnvironmentKey string `json:"-"`

	// WebhookSigningKey is used to verify incoming webhook signatures.
	WebhookSigningKey string `json:"-"`

	// Workdir is the root filesystem path where agent operations execute.
	Workdir string `json:"workdir"`

	// WorkerID is a unique identifier reported for observability.
	WorkerID string `json:"worker_id"`

	// ListenAddr is the TCP address to bind the HTTP server for healthz/webhook.
	ListenAddr string `json:"listen_addr"`

	// Mode controls whether the worker polls, receives webhooks, or both.
	Mode Mode `json:"mode"`

	// MaxFileBytes caps the file size read into memory by tools (0 = SDK default 256KiB).
	MaxFileBytes int64 `json:"max_file_bytes"`

	// UnrestrictedPaths allows tools to access files outside of Workdir.
	UnrestrictedPaths bool `json:"unrestricted_paths"`
}

// LoadFromEnv loads configuration from standard environment variables.
func LoadFromEnv() *Config {
	workdir := os.Getenv("WORK_DIR")
	if workdir == "" {
		workdir = os.Getenv("ANTHROPIC_WORKDIR")
	}
	if workdir == "" {
		workdir = DefaultWorkdir
	}

	listenAddr := os.Getenv("PORT")
	if listenAddr != "" {
		if !strings.HasPrefix(listenAddr, ":") {
			listenAddr = ":" + listenAddr
		}
	} else {
		listenAddr = os.Getenv("LISTEN_ADDR")
		if listenAddr == "" {
			listenAddr = DefaultListenAddr
		}
	}

	mode := Mode(strings.ToLower(os.Getenv("WORKER_MODE")))
	if mode != ModeWebhook && mode != ModeHybrid {
		mode = ModePoll
	}

	return &Config{
		EnvironmentID:     os.Getenv("ANTHROPIC_ENVIRONMENT_ID"),
		EnvironmentKey:    os.Getenv("ANTHROPIC_ENVIRONMENT_KEY"),
		WebhookSigningKey: os.Getenv("ANTHROPIC_WEBHOOK_SIGNING_KEY"),
		Workdir:           workdir,
		WorkerID:          os.Getenv("ANTHROPIC_WORKER_ID"),
		ListenAddr:        listenAddr,
		Mode:              mode,
		UnrestrictedPaths: os.Getenv("ANTHROPIC_UNRESTRICTED_PATHS") == "true",
	}
}

// Validate checks that required configuration values are present and valid.
func (c *Config) Validate() error {
	if c.EnvironmentID == "" {
		return errors.New("ANTHROPIC_ENVIRONMENT_ID is required")
	}
	if c.EnvironmentKey == "" {
		return errors.New("ANTHROPIC_ENVIRONMENT_KEY is required")
	}
	if (c.Mode == ModeWebhook || c.Mode == ModeHybrid) && c.WebhookSigningKey == "" {
		return errors.New("ANTHROPIC_WEBHOOK_SIGNING_KEY is required when running in webhook mode")
	}
	if c.Workdir == "" {
		c.Workdir = DefaultWorkdir
	}
	// Ensure workdir is cleaned
	c.Workdir = filepath.Clean(c.Workdir)
	return nil
}

// String returns a safe string representation with secrets redacted.
func (c *Config) String() string {
	hasKey := c.EnvironmentKey != ""
	hasWebhook := c.WebhookSigningKey != ""
	return fmt.Sprintf("Config{EnvironmentID: %q, Workdir: %q, ListenAddr: %q, Mode: %s, HasEnvKey: %t, HasWebhookKey: %t}",
		c.EnvironmentID, c.Workdir, c.ListenAddr, c.Mode, hasKey, hasWebhook)
}
