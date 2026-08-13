package config

import (
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid poll config",
			cfg: Config{
				EnvironmentID:  "env_test123",
				EnvironmentKey: "anthropic_env_key_test",
				Mode:           ModePoll,
				Workdir:        "/workspace",
			},
			wantErr: false,
		},
		{
			name: "missing environment id",
			cfg: Config{
				EnvironmentKey: "anthropic_env_key_test",
				Mode:           ModePoll,
			},
			wantErr: true,
		},
		{
			name: "missing environment key",
			cfg: Config{
				EnvironmentID: "env_test123",
				Mode:          ModePoll,
			},
			wantErr: true,
		},
		{
			name: "webhook mode without signing key",
			cfg: Config{
				EnvironmentID:  "env_test123",
				EnvironmentKey: "anthropic_env_key_test",
				Mode:           ModeWebhook,
			},
			wantErr: true,
		},
		{
			name: "webhook mode with signing key",
			cfg: Config{
				EnvironmentID:     "env_test123",
				EnvironmentKey:    "anthropic_env_key_test",
				WebhookSigningKey: "whsec_test123",
				Mode:              ModeWebhook,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestStringRedaction(t *testing.T) {
	cfg := &Config{
		EnvironmentID:     "env_12345",
		EnvironmentKey:    "super_secret_key",
		WebhookSigningKey: "super_secret_webhook",
		Workdir:           "/workspace",
		ListenAddr:        ":8080",
		Mode:              ModePoll,
	}
	s := cfg.String()
	if contains(s, "super_secret_key") || contains(s, "super_secret_webhook") {
		t.Errorf("Config.String() leaked secret: %s", s)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
