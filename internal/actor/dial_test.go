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
	"net"
	"sync/atomic"
	"testing"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

// countingControlServer answers GetAtespace and counts the calls it receives.
type countingControlServer struct {
	ateapipb.UnimplementedControlServer
	calls atomic.Int64
}

func (s *countingControlServer) GetAtespace(context.Context, *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	s.calls.Add(1)
	return &ateapipb.Atespace{}, nil
}

// TestDialATESpreadsCallsOverEveryReplica resolves one name to three servers,
// as ate-api's headless Service does, and expects each server to receive an
// equal share of one connection's calls.
func TestDialATESpreadsCallsOverEveryReplica(t *testing.T) {
	const replicas = 3
	var backends [replicas]*countingControlServer
	var addrs []resolver.Address
	for i := range backends {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		backends[i] = &countingControlServer{}
		srv := grpc.NewServer()
		ateapipb.RegisterControlServer(srv, backends[i])
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		addrs = append(addrs, resolver.Address{Addr: lis.Addr().String()})
	}
	r := manual.NewBuilderWithScheme("dialatetest")
	r.InitialState(resolver.State{Addresses: addrs})
	resolver.Register(r)

	client, conn, err := DialATE("dialatetest:///api", true, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call := func() {
		t.Helper()
		if _, err := client.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{}); err != nil {
			t.Fatalf("GetAtespace: %v", err)
		}
	}

	// Subchannels become ready one at a time, and only ready ones receive calls.
	for allReady := false; !allReady; {
		call()
		allReady = true
		for _, b := range backends {
			allReady = allReady && b.calls.Load() > 0
		}
	}

	var before [replicas]int64
	for i, b := range backends {
		before[i] = b.calls.Load()
	}
	const rounds = 20
	for range rounds * replicas {
		call()
	}
	for i, b := range backends {
		if got := b.calls.Load() - before[i]; got != rounds {
			t.Errorf("replica %d received %d of %d calls, want %d", i, got, rounds*replicas, rounds)
		}
	}
}
