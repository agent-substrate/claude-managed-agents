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
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func main() {
	var (
		prompt  string
		model   string
		agentID string
	)
	defaultModel := os.Getenv("ANTHROPIC_MODEL")
	if defaultModel == "" {
		defaultModel = anthropic.BetaManagedAgentsModelClaudeOpus5
	}

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "initial prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.StringVar(&agentID, "agent", "", "existing agent to reuse; a new agent is created when empty")
	flag.Parse()

	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	if envID == "" {
		fmt.Println("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.")
		fmt.Println("Export it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
		os.Exit(1)
	}

	ctx := context.Background()
	// Credentials resolve in order: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, an
	// 'ant auth login' profile, then workload identity federation.
	client := anthropic.NewClient()

	session, resolvedAgentID := createSession(ctx, &client, envID, agentID, model, prompt)
	sessionURL := fmt.Sprintf("https://platform.claude.com/workspaces/default/sessions/%s", session.ID)

	fmt.Printf("Prompt:       %s\n", prompt)
	fmt.Printf("Model:        %s\n", model)
	fmt.Printf("Agent:        %s\n", resolvedAgentID)
	fmt.Printf("Session:      %s\n", session.ID)
	fmt.Printf("Environment:  %s\n", envID)
	fmt.Printf("Session URL:  %s\n", sessionURL)
}

// createSession creates the agent (unless one was supplied) and opens a session
// on the self-hosted environment.
func createSession(ctx context.Context, client *anthropic.Client, envID, agentID, model, prompt string) (*anthropic.BetaManagedAgentsSession, string) {
	if agentID == "" {
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name: "Agent Substrate Example Agent",
			Model: anthropic.BetaManagedAgentsModelConfigParams{
				ID: anthropic.BetaManagedAgentsModel(model),
			},
			Tools: []anthropic.BetaAgentNewParamsToolUnion{
				{
					OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
						Type: "agent_toolset_20260401",
						DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
							// This demo approves every tool call unattended.
							PermissionPolicy: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParamsPermissionPolicyUnion{
								OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{
									Type: "always_allow",
								},
							},
						},
					},
				},
			},
		})
		if err != nil {
			log.Fatalf("failed to create agent: %v", err)
		}
		agentID = agent.ID
	}

	params := anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(agentID),
		},
		EnvironmentID: envID,
	}
	if prompt != "" {
		params.InitialEvents = []anthropic.BetaSessionNewParamsInitialEventUnion{
			{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Type: "user.message",
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{
						{
							OfText: &anthropic.BetaManagedAgentsTextBlockParam{
								Type: "text",
								Text: prompt,
							},
						},
					},
				},
			},
		}
	}

	session, err := client.Beta.Sessions.New(ctx, params)
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	return session, agentID
}
