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
	"context"
	"testing"

	"github.com/agent-substrate/claude-managed-agents/internal/manifest"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

const testCredURI = "ate-secret://k8s.io/default/ate-ant/ate-ant-creds/ANTHROPIC_ENVIRONMENT_KEY"

func TestBuildEgressPolicyDefault(t *testing.T) {
	p, err := BuildEgressPolicy("ate-ant", EgressConfig{CredentialURI: testCredURI})
	if err != nil {
		t.Fatalf("BuildEgressPolicy: %v", err)
	}
	if p.GetMetadata().GetAtespace() != "ate-ant" || p.GetMetadata().GetName() != "default" {
		t.Errorf("metadata = %+v", p.GetMetadata())
	}
	if len(p.GetRules()) != 3 {
		t.Fatalf("rules = %d, want 3", len(p.GetRules()))
	}

	https := p.GetRules()[0].GetHttps()
	if https == nil {
		t.Fatalf("first rule = %+v, want https", p.GetRules()[0])
	}
	if got := https.GetHostnames(); len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Errorf("https hostnames = %v", got)
	}
	if got := https.GetPorts().GetNumbers(); len(got) != 1 || got[0] != 443 {
		t.Errorf("https ports = %v", got)
	}
	hdrs := https.GetEffects().GetReplaceHeaders()
	if len(hdrs) != 1 || hdrs[0].GetHeader() != "Authorization" || hdrs[0].GetPrefix() != "Bearer " || hdrs[0].GetCredentialUri() != testCredURI {
		t.Errorf("replace headers = %+v", hdrs)
	}

	if r := p.GetRules()[1].GetHttp(); r == nil || r.GetHostnames()[0] != "*" || r.GetPorts().GetNumbers()[0] != 80 || r.GetEffects() != nil {
		t.Errorf("second rule = %+v, want http * on port 80 without effects", p.GetRules()[1])
	}
	if r := p.GetRules()[2].GetTlsPassthrough(); r == nil || r.GetHostnames()[0] != "*" || r.GetPorts().GetAll() == nil {
		t.Errorf("third rule = %+v, want tls_passthrough * on all ports", p.GetRules()[2])
	}
}

func TestBuildEgressPolicyCustomBaseURL(t *testing.T) {
	tests := []struct {
		baseURL  string
		wantHTTP bool
		wantHost string
		wantPort int32
	}{
		{"https://Gateway.example.com:8443/v1", false, "gateway.example.com", 8443},
		{"http://managed-agents.example.svc.cluster.local:8400", true, "managed-agents.example.svc.cluster.local", 8400},
		{"http://plain.example.com", true, "plain.example.com", 80},
	}
	for _, tt := range tests {
		p, err := BuildEgressPolicy("ns", EgressConfig{AnthropicBaseURL: tt.baseURL, CredentialURI: testCredURI})
		if err != nil {
			t.Fatalf("%s: BuildEgressPolicy: %v", tt.baseURL, err)
		}
		var hosts []string
		var ports []int32
		var effects *ateapipb.HttpRuleEffects
		if tt.wantHTTP {
			r := p.GetRules()[0].GetHttp()
			if r == nil {
				t.Fatalf("%s: first rule = %+v, want http", tt.baseURL, p.GetRules()[0])
			}
			hosts, ports, effects = r.GetHostnames(), r.GetPorts().GetNumbers(), r.GetEffects()
		} else {
			r := p.GetRules()[0].GetHttps()
			if r == nil {
				t.Fatalf("%s: first rule = %+v, want https", tt.baseURL, p.GetRules()[0])
			}
			hosts, ports, effects = r.GetHostnames(), r.GetPorts().GetNumbers(), r.GetEffects()
		}
		if len(hosts) != 1 || hosts[0] != tt.wantHost || len(ports) != 1 || ports[0] != tt.wantPort {
			t.Errorf("%s: hosts=%v ports=%v, want %s:%d", tt.baseURL, hosts, ports, tt.wantHost, tt.wantPort)
		}
		if len(effects.GetReplaceHeaders()) != 1 {
			t.Errorf("%s: effects = %+v, want one replace header", tt.baseURL, effects)
		}
	}
}

func TestBuildEgressPolicyErrors(t *testing.T) {
	for _, cfg := range []EgressConfig{
		{},
		{CredentialURI: testCredURI, AnthropicBaseURL: "ftp://api.anthropic.com"},
		{CredentialURI: testCredURI, AnthropicBaseURL: "https://"},
		{CredentialURI: testCredURI, AnthropicBaseURL: "https://api.anthropic.com:0"},
	} {
		if _, err := BuildEgressPolicy("ns", cfg); err == nil {
			t.Errorf("BuildEgressPolicy(%+v) = nil error, want error", cfg)
		}
	}
}

func TestManagerResumeCreatesInjectingEgressPolicy(t *testing.T) {
	fakeClient := newFakeControlClient()
	mgr := NewManager(fakeClient, "test-template", "test-ns")

	sessionID := "session-egress-test-01"
	if err := mgr.Resume(context.Background(), sessionID); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	fakeClient.mu.Lock()
	defer fakeClient.mu.Unlock()
	policy, ok := fakeClient.createdEgressPolicies["test-ns/"+ActorName(sessionID)]
	if !ok {
		t.Fatalf("expected an egress policy for the actor")
	}
	hdrs := policy.GetRules()[0].GetHttps().GetEffects().GetReplaceHeaders()
	if len(hdrs) != 1 || hdrs[0].GetCredentialUri() != manifest.EnvironmentKeyCredentialURI("test-ns") {
		t.Errorf("replace headers = %+v, want the test-ns ate-ant-creds environment key", hdrs)
	}
}
