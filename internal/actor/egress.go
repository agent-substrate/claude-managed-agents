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
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

const (
	// DefaultAnthropicBaseURL is the Anthropic API the worker calls when no
	// custom base URL is configured.
	DefaultAnthropicBaseURL = "https://api.anthropic.com"

	egressPolicyName = "default"
)

// EgressConfig configures the egress policy attached to each actor.
type EgressConfig struct {
	// AnthropicBaseURL is the Anthropic API the worker calls. Requests to its
	// host get the environment key injected. Defaults to DefaultAnthropicBaseURL.
	AnthropicBaseURL string

	// CredentialURI is the ate-secret:// URI of the Anthropic environment key,
	// resolved by the egress gateway's credential provider.
	CredentialURI string
}

// BuildEgressPolicy returns the egress policy for an actor in atespace.
//
// Requests to the Anthropic API are intercepted by the Substrate egress
// gateway, which replaces the worker's placeholder Authorization header with
// "Bearer <environment key>" resolved from cfg.CredentialURI. The actor never
// holds the key.
//
// All other traffic stays open so that tools (bash, package managers, skills)
// keep working: cleartext HTTP on port 80, and TLS on every port forwarded
// without interception. The Anthropic rule names an exact host, so it takes
// precedence over the "*" rules.
func BuildEgressPolicy(atespace string, cfg EgressConfig) (*ateapipb.EgressPolicy, error) {
	if cfg.CredentialURI == "" {
		return nil, errors.New("credential URI for the Anthropic environment key is required")
	}
	baseURL := cfg.AnthropicBaseURL
	if baseURL == "" {
		baseURL = DefaultAnthropicBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing Anthropic base URL %q: %w", baseURL, err)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("Anthropic base URL %q has no host", baseURL)
	}

	effects := &ateapipb.HttpRuleEffects{
		ReplaceHeaders: []*ateapipb.CredentialHeader{{
			Header:        "Authorization",
			Prefix:        "Bearer ",
			CredentialUri: cfg.CredentialURI,
		}},
	}

	var anthropicRule *ateapipb.EgressRule
	switch u.Scheme {
	case "https":
		port, err := portOf(u, 443)
		if err != nil {
			return nil, err
		}
		anthropicRule = &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{
			Hostnames: []string{host},
			Ports:     &ateapipb.Ports{Numbers: []int32{port}},
			Effects:   effects,
		}}
	case "http":
		port, err := portOf(u, 80)
		if err != nil {
			return nil, err
		}
		anthropicRule = &ateapipb.EgressRule{Http: &ateapipb.HttpRule{
			Hostnames: []string{host},
			Ports:     &ateapipb.Ports{Numbers: []int32{port}},
			Effects:   effects,
		}}
	default:
		return nil, fmt.Errorf("Anthropic base URL %q: unsupported scheme %q", baseURL, u.Scheme)
	}

	return &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: atespace,
			Name:     egressPolicyName,
		},
		Rules: []*ateapipb.EgressRule{
			anthropicRule,
			{Http: &ateapipb.HttpRule{
				Hostnames: []string{"*"},
				Ports:     &ateapipb.Ports{Numbers: []int32{80}},
			}},
			{TlsPassthrough: &ateapipb.TlsPassthroughRule{
				Hostnames: []string{"*"},
				Ports:     &ateapipb.Ports{All: &ateapipb.AllPorts{}},
			}},
		},
	}, nil
}

func portOf(u *url.URL, def int32) (int32, error) {
	p := u.Port()
	if p == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("Anthropic base URL %q: invalid port %q", u.String(), p)
	}
	return int32(n), nil
}
