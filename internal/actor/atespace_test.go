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
	"errors"
	"testing"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// countingAtespaceClient counts the two atespace calls and fails them on demand.
type countingAtespaceClient struct {
	*fakeControlClient
	gets, creates       int
	getErr, atespaceErr error
}

func (c *countingAtespaceClient) GetAtespace(ctx context.Context, in *ateapipb.GetAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	return &ateapipb.Atespace{}, nil
}

func (c *countingAtespaceClient) CreateAtespace(ctx context.Context, in *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	c.creates++
	if c.atespaceErr != nil {
		return nil, c.atespaceErr
	}
	return &ateapipb.Atespace{}, nil
}

func TestEnsureAtespaceLooksUpOnce(t *testing.T) {
	c := &countingAtespaceClient{fakeControlClient: newFakeControlClient()}
	mgr := NewManager(c, "tmpl", "space")
	for range 5 {
		if err := mgr.EnsureAtespace(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.gets != 1 || c.creates != 0 {
		t.Errorf("got %d GetAtespace and %d CreateAtespace calls, want 1 and 0", c.gets, c.creates)
	}
}

func TestEnsureAtespaceRemembersTheOneItCreated(t *testing.T) {
	c := &countingAtespaceClient{fakeControlClient: newFakeControlClient(), getErr: status.Error(codes.NotFound, "no such atespace")}
	mgr := NewManager(c, "tmpl", "space")
	for range 3 {
		if err := mgr.EnsureAtespace(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if c.gets != 1 || c.creates != 1 {
		t.Errorf("got %d GetAtespace and %d CreateAtespace calls, want 1 and 1", c.gets, c.creates)
	}
}

// A failure says nothing about the atespace, so the next call must ask again.
func TestEnsureAtespaceAsksAgainAfterAFailure(t *testing.T) {
	down := status.Error(codes.Unavailable, "ate-api is unavailable")
	c := &countingAtespaceClient{fakeControlClient: newFakeControlClient(), getErr: down, atespaceErr: down}
	mgr := NewManager(c, "tmpl", "space")
	if err := mgr.EnsureAtespace(context.Background()); !errors.Is(err, down) {
		t.Fatalf("got %v, want the failure", err)
	}
	c.getErr, c.atespaceErr = nil, nil
	if err := mgr.EnsureAtespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.gets != 2 {
		t.Errorf("got %d GetAtespace calls, want 2: one that failed and one that succeeded", c.gets)
	}
}

// An atespace deleted under a running controller must not stay missing for it:
// the create that fails for want of it makes the next create look again.
func TestResumeLooksUpTheAtespaceAgainAfterACreateThatLacksIt(t *testing.T) {
	c := &countingAtespaceClient{fakeControlClient: newFakeControlClient()}
	mgr := NewManager(c, "tmpl", "space")
	if err := mgr.EnsureAtespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.createErr = status.Error(codes.FailedPrecondition, "atespace not found")
	if err := mgr.Resume(context.Background(), "sesn_1"); status.Code(errors.Unwrap(err)) != codes.FailedPrecondition {
		t.Fatalf("got %v, want the create's failure", err)
	}
	if c.gets != 1 {
		t.Fatalf("got %d GetAtespace calls before the failed create, want 1", c.gets)
	}
	_ = mgr.Resume(context.Background(), "sesn_1")
	if c.gets != 2 {
		t.Errorf("got %d GetAtespace calls, want 2: the create after the failure must look again", c.gets)
	}
}
