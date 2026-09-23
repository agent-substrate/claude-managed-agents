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
	"errors"
	"testing"
)

func TestControllerCommandFlags(t *testing.T) {
	cmd := newControllerCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if cmd.Use != "controller" {
		t.Errorf("cmd.Use = %q, want controller", cmd.Use)
	}

	flags := []string{
		"namespace",
		"template",
		"ateapi-addr",
		"atenet-router-addr",
		"insecure",
		"gc-interval",
		"gc-idle-threshold",
	}

	for _, name := range flags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s on controller command", name)
		}
	}
}

func TestRetry(t *testing.T) {
	ctx := t.Context()

	// 1. Success on first try
	var calls int
	err := retry(ctx, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}

	// 2. Success on 3rd attempt
	calls = 0
	err = retry(ctx, func() error {
		calls++
		if calls < 3 {
			return errors.New("temporary error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error on 3rd attempt, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}

	// 3. Exhausted attempts
	calls = 0
	err = retry(ctx, func() error {
		calls++
		return errors.New("permanent failure")
	})
	if err == nil || err.Error() != "permanent failure" {
		t.Fatalf("expected 'permanent failure', got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls before failing, got %d", calls)
	}
}
