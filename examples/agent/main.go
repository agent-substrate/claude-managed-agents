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

	ctx := context.Background()
	// Credentials resolve in order: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, an
	// 'ant auth login' profile, then workload identity federation.
	client := anthropic.NewClient()

	session, resolvedAgentID := createSession(ctx, &client, agentID, model, prompt)

	fmt.Printf("💬 Prompt:  %s\n", prompt)
	fmt.Printf("🤖 Agent:   %s\n", resolvedAgentID)
	fmt.Printf("🚀 Session: %s\n", session.ID)
	fmt.Printf("🔗 Click to view session & send more inputs: https://platform.claude.com/workspaces/default/sessions/%s\n", session.ID)
}

// createSession creates the agent (unless one was supplied) and opens a session
// on the self-hosted environment.
func createSession(ctx context.Context, client *anthropic.Client, agentID, model, prompt string) (*anthropic.BetaManagedAgentsSession, string) {
	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	if envID == "" {
		log.Fatal("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.\nExport it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
	}

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
