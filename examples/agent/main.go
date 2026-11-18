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
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func main() {
	var (
		prompt     string
		model      string
		agentID    string
		system     string
		systemFile string
		skills     skillFlags
	)
	defaultModel := os.Getenv("ANTHROPIC_MODEL")
	if defaultModel == "" {
		defaultModel = anthropic.BetaManagedAgentsModelClaudeOpus5
	}

	flag.StringVar(&prompt, "prompt", "Run `uname -a` in bash and show the output.", "initial prompt to execute in the sandbox")
	flag.StringVar(&model, "model", defaultModel, "Anthropic model to power the agent")
	flag.StringVar(&agentID, "agent", "", "existing agent to reuse; a new agent is created when empty")
	flag.StringVar(&system, "system", "", "system prompt for the agent (optional)")
	flag.StringVar(&systemFile, "system-file", "", "path to a file containing the system prompt (optional)")
	flag.Var(&skills, "skill", "skill to attach, as [anthropic|custom:]skill_id (optional, repeatable)")
	flag.Parse()

	systemPrompt, err := resolveSystemPrompt(system, systemFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

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

	session, resolvedAgentID := createSession(ctx, &client, envID, agentID, model, prompt, systemPrompt, skills)
	sessionURL := fmt.Sprintf("https://platform.claude.com/workspaces/default/sessions/%s", session.ID)

	fmt.Printf("Prompt:       %s\n", prompt)
	fmt.Printf("Model:        %s\n", model)
	if systemPrompt != "" {
		firstLine := strings.SplitN(systemPrompt, "\n", 2)[0]
		if len(firstLine) > 60 {
			firstLine = firstLine[:60] + "…"
		}
		fmt.Printf("System:       %s (%d bytes)\n", firstLine, len(systemPrompt))
	}
	for _, sk := range skills {
		typ, id := "", ""
		if t := sk.GetType(); t != nil {
			typ = *t
		}
		if i := sk.GetSkillID(); i != nil {
			id = *i
		}
		fmt.Printf("Skill:        %s (%s)\n", id, typ)
	}
	fmt.Printf("Agent:        %s\n", resolvedAgentID)
	fmt.Printf("Session:      %s\n", session.ID)
	fmt.Printf("Environment:  %s\n", envID)
	fmt.Printf("Session URL:  %s\n", sessionURL)
}

// createSession creates the agent (unless one was supplied) and opens a session
// on the self-hosted environment.
func createSession(ctx context.Context, client *anthropic.Client, envID, agentID, model, prompt, systemPrompt string, skills skillFlags) (*anthropic.BetaManagedAgentsSession, string) {
	if agentID == "" {
		newParams := anthropic.BetaAgentNewParams{
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
		}
		if systemPrompt != "" {
			newParams.System = param.NewOpt(systemPrompt)
		}
		if len(skills) > 0 {
			newParams.Skills = []anthropic.BetaManagedAgentsSkillParamsUnion(skills)
		}
		agent, err := client.Beta.Agents.New(ctx, newParams)
		if err != nil {
			log.Fatalf("failed to create agent: %v", err)
		}
		agentID = agent.ID
	} else if systemPrompt != "" || len(skills) > 0 {
		fmt.Println("Note: -system/-skill only apply when creating a new agent; the existing agent's configuration is used.")
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

// skillFlags accumulates repeatable -skill values of the form
// [anthropic|custom:]skill_id; a bare ID defaults to type "anthropic".
type skillFlags []anthropic.BetaManagedAgentsSkillParamsUnion

func (s *skillFlags) String() string {
	ids := make([]string, 0, len(*s))
	for _, u := range *s {
		if id := u.GetSkillID(); id != nil {
			ids = append(ids, *id)
		}
	}
	return strings.Join(ids, ",")
}

func (s *skillFlags) Set(v string) error {
	typ, id := "anthropic", v
	if i := strings.Index(v, ":"); i >= 0 {
		typ, id = v[:i], v[i+1:]
	}
	if id == "" {
		return fmt.Errorf("invalid -skill %q: skill ID is empty", v)
	}
	switch typ {
	case "anthropic":
		*s = append(*s, anthropic.BetaManagedAgentsSkillParamsOfAnthropic(id))
	case "custom":
		*s = append(*s, anthropic.BetaManagedAgentsSkillParamsOfCustom(id))
	default:
		return fmt.Errorf("invalid -skill %q: type must be anthropic or custom", v)
	}
	return nil
}

// resolveSystemPrompt merges the -system and -system-file flags. Both are
// optional; the empty result means no system prompt is configured.
func resolveSystemPrompt(inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", fmt.Errorf("-system and -system-file are mutually exclusive")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return inline, nil
}
