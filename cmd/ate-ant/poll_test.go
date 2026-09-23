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
	"bytes"
	"testing"
)

func TestPollCommand(t *testing.T) {
	cmd := newPollCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "poll" {
		t.Errorf("cmd.Use = %q, want poll", cmd.Use)
	}
	if cmd.Flags().Lookup("handoff-ttl-seconds") == nil {
		t.Error("missing flag --handoff-ttl-seconds on poll command")
	}
	if cmd.Flags().Lookup("anthropic-base-url") == nil {
		t.Error("missing flag --anthropic-base-url on poll command")
	}
}

func TestRootCommandSubcommands(t *testing.T) {
	root := newRootCommand()

	expectedSubcommands := map[string]bool{
		"manifest":   false,
		"poll":       false,
		"run":        false,
		"controller": false,
		"version":    false,
	}

	for _, sub := range root.Commands() {
		if _, ok := expectedSubcommands[sub.Name()]; ok {
			expectedSubcommands[sub.Name()] = true
		}
	}

	for name, found := range expectedSubcommands {
		if !found {
			t.Errorf("expected subcommand %q on root command", name)
		}
	}
}
