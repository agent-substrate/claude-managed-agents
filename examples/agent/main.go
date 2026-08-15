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
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

func main() {
	var (
		prompt    string
		model     string
		agentID   string
		sessionID string
	)
	defaultModel := os.Getenv("ANTHROPIC_MODEL")
	if defaultModel == "" {
		defaultModel = anthropic.BetaManagedAgentsModelClaudeOpus5
	}

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.StringVar(&agentID, "agent", "", "existing agent to reuse; a new agent is created when empty")
	flag.StringVar(&sessionID, "session", "", "send the prompt to an existing session instead of starting one (--agent and --model are ignored)")
	flag.Parse()

	ctx := context.Background()
	// Credentials resolve in order: ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, an
	// 'ant auth login' profile, then workload identity federation.
	client := anthropic.NewClient()

	// Sessions are stateful: pass --session to continue an earlier one, and the
	// agent keeps every turn of context it already had.
	var session *anthropic.BetaManagedAgentsSession
	var err error
	if sessionID != "" {
		session, err = client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
		if err != nil {
			log.Fatalf("failed to load session %s: %v", sessionID, err)
		}
		if session.Status == anthropic.BetaManagedAgentsSessionStatusTerminated {
			log.Fatalf("session %s is terminated and cannot accept new messages", session.ID)
		}
		fmt.Printf("● Continuing session  (%s)\n", session.Status)
		fmt.Printf("  agent    %s\n", session.Agent.ID)
		fmt.Printf("  model    %s\n", session.Agent.Model.ID)
	} else {
		session = startSession(ctx, &client, agentID, model)
	}

	fmt.Printf("  session  %s\n", session.ID)
	fmt.Printf("  resume   --session %s\n", session.ID)
	fmt.Printf("  trace    https://platform.claude.com/workspaces/default/sessions/%s\n", session.ID)

	fmt.Printf("\n▸ %s\n", prompt)
	fmt.Println(strings.Repeat("─", 60))

	// Open the stream before sending: it only delivers events that happen after
	// it opens, so sending first can lose the early ones.
	stream := client.Beta.Sessions.Events.StreamEvents(ctx, session.ID, anthropic.BetaSessionEventStreamParams{})
	defer stream.Close()

	sendPrompt(ctx, &client, session.ID, prompt)

	var sessionErr string
	streamEvents(stream, &sessionErr)

	if err := stream.Err(); err != nil {
		log.Fatalf("stream error: %v", err)
	}
	if sessionErr != "" {
		log.Fatalf("session error: %s", sessionErr)
	}
}

// startSession creates the agent (unless one was supplied) and opens a session
// on the self-hosted environment.
func startSession(ctx context.Context, client *anthropic.Client, agentID, model string) *anthropic.BetaManagedAgentsSession {
	envID := os.Getenv("ANTHROPIC_ENVIRONMENT_ID")
	if envID == "" {
		log.Fatal("Error: ANTHROPIC_ENVIRONMENT_ID environment variable is required.\nExport it with: export ANTHROPIC_ENVIRONMENT_ID=\"env_...\"")
	}

	// Agents are persistent, versioned objects: create one, then reuse its ID on
	// every run. Creating a fresh agent per run leaves orphaned agents behind and
	// gives up version pinning.
	if agentID == "" {
		fmt.Printf("● Creating agent  (model %s)\n", model)
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
		fmt.Printf("  agent    %s\n", agentID)
		fmt.Printf("  reuse    --agent %s\n", agentID)
	} else {
		fmt.Printf("● Using agent     %s\n", agentID)
	}

	fmt.Printf("● Creating session  (environment %s)\n", envID)
	session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(agentID),
		},
		EnvironmentID: envID,
	})
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	return session
}

// sendPrompt delivers the prompt as a user message, which starts a new session
// working or wakes an idle one.
func sendPrompt(ctx context.Context, client *anthropic.Client, sessionID, prompt string) {
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
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
}

// streamEvents renders the session's events until it stops for good, recording
// any session error it reports.
func streamEvents(stream *ssestream.Stream[anthropic.BetaManagedAgentsStreamSessionEventsUnion], sessionErr *string) {
	for stream.Next() {
		event := stream.Current()

		switch v := event.AsAny().(type) {
		case anthropic.BetaManagedAgentsAgentThinkingEvent:
			fmt.Println("  ⋯ thinking")

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
			fmt.Printf("\n● Claude\n")
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
				return
			}

		case anthropic.BetaManagedAgentsSessionStatusTerminatedEvent:
			return

		case anthropic.BetaManagedAgentsSessionErrorEvent:
			*sessionErr = v.Error.Message
			return

		default:
			// do nothing
		}
	}
}

func printToolUse(rawJSON string) {
	fmt.Printf("\n  ⚒ tool call\n%s\n", rawJSON)
}

func printToolResult(rawJSON string) {
	fmt.Printf("\n  ↳ tool result\n%s\n", rawJSON)
}
