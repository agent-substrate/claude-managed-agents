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
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"testing"
	"time"

	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// startATEServer runs a TLS gRPC server with no services whose certificate,
// issued by ca, names ateapiServerName. A call that completes the TLS
// handshake therefore fails with Unimplemented; one that fails verification
// fails with Unavailable.
func startATEServer(t *testing.T, ca *testCA) string {
	t.Helper()
	bundle := ca.issue(t, 10, x509.ExtKeyUsageServerAuth, []string{ateapiServerName}, nil)
	cert, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func callATE(t *testing.T, addr, caFile string) codes.Code {
	t.Helper()
	client, conn, err := DialATE(addr, false, caFile)
	if err != nil {
		t.Fatalf("DialATE: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = client.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{Atespace: &ateapipb.ObjectRef{Name: "x"}})
	return status.Code(err)
}

func TestDialATEVerifiesServer(t *testing.T) {
	serverCA, otherCA := newTestCA(t), newTestCA(t)
	addr := startATEServer(t, serverCA)
	dir := t.TempDir()

	// Dialing 127.0.0.1 must still verify the certificate for
	// ateapiServerName, as it does for api.ate-system.svc.cluster.local.
	if got := callATE(t, addr, writeFile(t, dir, "trusted.pem", serverCA.pem)); got != codes.Unimplemented {
		t.Fatalf("call to trusted ate-api = %v, want Unimplemented (TLS handshake succeeded)", got)
	}
	if got := callATE(t, addr, writeFile(t, dir, "untrusted.pem", otherCA.pem)); got != codes.Unavailable {
		t.Fatalf("call to ate-api outside the trust bundle = %v, want Unavailable (TLS verification failed)", got)
	}
}

func TestDialATERequiresTrustBundle(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.pem")
	if _, _, err := DialATE("127.0.0.1:1", false, missing); err == nil {
		t.Fatal("DialATE without a trust bundle succeeded; it must not fall back to an unverified connection")
	}
}
