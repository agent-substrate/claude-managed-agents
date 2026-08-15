package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Colors, dropped when stdout is not a terminal or $NO_COLOR is set.
var dim, bold, cyan, reset = func() (string, string, string, string) {
	info, err := os.Stdout.Stat()
	if os.Getenv("NO_COLOR") != "" || err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return "", "", "", ""
	}
	return "\x1b[2m", "\x1b[1m", "\x1b[36m", "\x1b[0m"
}()

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

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.StringVar(&agentID, "agent-id", os.Getenv("ANTHROPIC_AGENT_ID"), "existing agent to reuse (or $ANTHROPIC_AGENT_ID); one is created when empty")
	flag.Parse()

	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	if envID == "" {
		log.Fatal("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.\nExport it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
	}

	ctx := context.Background()
	// Credentials resolve in order: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, an
	// 'ant auth login' profile, then workload identity federation.
	client := anthropic.NewClient()

	// Agents are persistent, versioned objects: create one, then reuse its ID on
	// every run. Creating a fresh agent per run leaves orphaned agents behind and
	// gives up version pinning.
	if agentID == "" {
		fmt.Printf("● Creating agent  %s(model %s)%s\n", dim, model, reset)
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
		fmt.Printf("  %sagent%s    %s\n", dim, reset, agentID)
		fmt.Printf("  %sreuse%s    export ANTHROPIC_AGENT_ID=%s\n", dim, reset, agentID)
	} else {
		fmt.Printf("● Using agent     %s\n", agentID)
	}

	fmt.Printf("● Creating session  %s(environment %s)%s\n", dim, envID, reset)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(agentID),
		},
		EnvironmentID: envID,
	})
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	fmt.Printf("  %ssession%s  %s\n", dim, reset, session.ID)
	fmt.Printf("  %strace%s    https://platform.claude.com/workspaces/default/sessions/%s\n", dim, reset, session.ID)

	fmt.Printf("\n%s▸%s %s\n", bold, reset, prompt)

	// Open the stream before sending: it only delivers events that happen after
	// it opens, so sending first can lose the early ones.
	stream := client.Beta.Sessions.Events.StreamEvents(ctx, session.ID, anthropic.BetaSessionEventStreamParams{})
	defer stream.Close()

	// Send initial user message event to start the work
	_, err = client.Beta.Sessions.Events.Send(ctx, session.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{
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
		},
	})
	if err != nil {
		log.Fatalf("failed to send event: %v", err)
	}

	var sessionErr string

	fmt.Printf("%s%s%s\n", dim, strings.Repeat("─", 60), reset)
events:
	for stream.Next() {
		event := stream.Current()

		switch v := event.AsAny().(type) {
		case anthropic.BetaManagedAgentsAgentThinkingEvent:
			fmt.Printf("  %s⋯ thinking%s\n", dim, reset)

		case anthropic.BetaManagedAgentsAgentToolUseEvent,
			anthropic.BetaManagedAgentsAgentCustomToolUseEvent,
			anthropic.BetaManagedAgentsAgentMCPToolUseEvent:
			printToolUse(event.RawJSON())

		case anthropic.BetaManagedAgentsUserToolResultEvent,
			anthropic.BetaManagedAgentsAgentToolResultEvent,
			anthropic.BetaManagedAgentsUserCustomToolResultEvent,
			anthropic.BetaManagedAgentsAgentMCPToolResultEvent:
			printToolResult(event.RawJSON())

		case anthropic.BetaManagedAgentsAgentMessageEvent:
			fmt.Printf("\n%s● Claude%s\n", bold, reset)
			for _, block := range v.Content {
				if block.Type == "text" {
					fmt.Printf("%s\n", block.Text)
				}
			}

		case anthropic.BetaManagedAgentsSessionStatusIdleEvent:
			// A session also goes idle while it waits on us, for a tool
			// confirmation or a custom tool result. Any stop reason other than
			// requires_action means it has stopped for good.
			if v.StopReason.Type != "requires_action" {
				break events
			}

		case anthropic.BetaManagedAgentsSessionStatusTerminatedEvent:
			break events

		case anthropic.BetaManagedAgentsSessionErrorEvent:
			sessionErr = v.Error.Message
			break events

		default:
			// do nothing
		}
	}

	if err := stream.Err(); err != nil {
		log.Fatalf("stream error: %v", err)
	}
	if sessionErr != "" {
		log.Fatalf("session error: %s", sessionErr)
	}
}

func printToolUse(rawJSON string) {
	fmt.Printf("\n  %s⚒ tool call%s\n%s\n", cyan, reset, rawJSON)
}

func printToolResult(rawJSON string) {
	fmt.Printf("\n  %s↳ tool result%s\n%s\n", cyan, reset, rawJSON)
}
