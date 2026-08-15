package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func main() {
	var (
		prompt string
		model  string
	)
	defaultModel := os.Getenv("ANTHROPIC_MODEL")
	if defaultModel == "" {
		defaultModel = anthropic.BetaManagedAgentsModelClaudeOpus5
	}

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.Parse()

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")

	if apiKey == "" {
		log.Fatal("Error: ANTHROPIC_API_KEY environment variable is required.\nExport it with: export ANTHROPIC_API_KEY=\"sk-ant-api...\"")
	}
	if envID == "" {
		log.Fatal("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.\nExport it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
	}

	ctx := context.Background()
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	fmt.Printf("1. Creating agent with model '%s'...\n", model)
	agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: "Substrate Agent",
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModel(model),
		},
		Tools: []anthropic.BetaAgentNewParamsToolUnion{
			{
				OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
					Type: "agent_toolset_20260401",
					DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
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
	fmt.Printf("   Agent created: %s\n", agent.ID)

	fmt.Printf("2. Creating session on self-hosted environment '%s'...\n", envID)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(agent.ID),
		},
		EnvironmentID: envID,
	})
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	fmt.Printf("   Session created: %s\n", session.ID)

	fmt.Printf("3. Sending prompt to sandbox:\n   Prompt: %s\n\n", prompt)

	// Stream live events from the session
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

	fmt.Println("Streaming Agent Output...")
	for stream.Next() {
		event := stream.Current()

		switch v := event.AsAny().(type) {
		case anthropic.BetaManagedAgentsAgentThinkingEvent:
			fmt.Printf("Claude is thinking...\n")

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
			fmt.Printf("\n[Claude]\n")
			for _, block := range v.Content {
				if block.Text != "" {
					fmt.Printf("%s\n", block.Text)
				} else if block.AsText().Text != "" {
					fmt.Printf("%s\n", block.AsText().Text)
				}
			}

		case anthropic.BetaManagedAgentsSessionStatusIdleEvent:
			if _, ok := v.StopReason.AsAny().(anthropic.BetaManagedAgentsSessionEndTurn); ok {
				return
			}

		case anthropic.BetaManagedAgentsSessionThreadStatusIdleEvent:
			if _, ok := v.StopReason.AsAny().(anthropic.BetaManagedAgentsSessionEndTurn); ok {
				return
			}

		case anthropic.BetaManagedAgentsSessionStatusTerminatedEvent,
			anthropic.BetaManagedAgentsSessionThreadStatusTerminatedEvent:
			return

		case anthropic.BetaManagedAgentsSessionErrorEvent:
			log.Fatalf("\n[Session Error] %s\n", v.Error.Message)

		default:
			// do nothing
		}
	}

	if err := stream.Err(); err != nil {
		log.Fatalf("stream error: %v", err)
	}
}

func printToolUse(rawJSON string) {
	fmt.Printf("\n[🛠️ Tool Call]\n%s\n", rawJSON)
}

func printToolResult(rawJSON string) {
	fmt.Printf("\n[🛠️ Tool Result]\n%s\n", rawJSON)
}
