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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM bundle holding a private key followed by its certificate,
// the layout of a podcertificate credential bundle.
func (ca *testCA) issue(t *testing.T, serial int64, usage x509.ExtKeyUsage, dnsNames []string, uris []*url.URL) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dnsNames,
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newMTLSRouter starts a fake atenet-router that requires a client certificate
// signed by clientCA and records the SPIFFE ID of the caller.
func newMTLSRouter(t *testing.T, serverCA, clientCA *testCA, gotSPIFFE *string) *httptest.Server {
	t.Helper()
	serverBundle := serverCA.issue(t, 2, x509.ExtKeyUsageServerAuth, []string{DefaultRouterServerName}, nil)
	serverCert, err := tls.X509KeyPair(serverBundle, serverBundle)
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	clientPool.AddCert(clientCA.cert)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 && len(r.TLS.PeerCertificates[0].URIs) > 0 {
			*gotSPIFFE = r.TLS.PeerCertificates[0].URIs[0].String()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientPool,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestNotifyOverRouterMTLS(t *testing.T) {
	serverCA, clientCA := newTestCA(t), newTestCA(t)
	var gotSPIFFE string
	srv := newMTLSRouter(t, serverCA, clientCA, &gotSPIFFE)

	dir := t.TempDir()
	spiffe, _ := url.Parse("spiffe://cluster.local/ns/test-ns/sa/router-client")
	certFile := writeFile(t, dir, "credential-bundle.pem", clientCA.issue(t, 3, x509.ExtKeyUsageClientAuth, nil, []*url.URL{spiffe}))
	caFile := writeFile(t, dir, "trust-bundle.pem", serverCA.pem)

	transport, err := RouterTransportFromFiles(certFile, caFile, DefaultRouterServerName)
	if err != nil {
		t.Fatalf("RouterTransportFromFiles: %v", err)
	}
	mgr := NewManager(newFakeControlClient(), "test-template", "test-ns").
		WithRouterAddr(srv.URL).
		WithRouterTransport(transport)

	if err := mgr.Notify(context.Background(), "session-mtls-01", map[string]string{"work_id": "w1"}); err != nil {
		t.Fatalf("Notify over mTLS: %v", err)
	}
	if gotSPIFFE != spiffe.String() {
		t.Errorf("router saw client identity %q, want %q", gotSPIFFE, spiffe)
	}
}

func TestNotifyRejectsUntrustedRouter(t *testing.T) {
	serverCA, clientCA, otherCA := newTestCA(t), newTestCA(t), newTestCA(t)
	var gotSPIFFE string
	srv := newMTLSRouter(t, serverCA, clientCA, &gotSPIFFE)

	dir := t.TempDir()
	certFile := writeFile(t, dir, "credential-bundle.pem", clientCA.issue(t, 3, x509.ExtKeyUsageClientAuth, nil, nil))
	// Trust a CA that did not issue the router's serving certificate.
	caFile := writeFile(t, dir, "trust-bundle.pem", otherCA.pem)

	transport, err := RouterTransportFromFiles(certFile, caFile, DefaultRouterServerName)
	if err != nil {
		t.Fatalf("RouterTransportFromFiles: %v", err)
	}
	mgr := NewManager(newFakeControlClient(), "test-template", "test-ns").
		WithRouterAddr(srv.URL).
		WithRouterTransport(transport)

	if err := mgr.Notify(context.Background(), "session-mtls-02", map[string]string{}); err == nil {
		t.Fatal("Notify succeeded against a router whose certificate is not in the trust bundle")
	}
}

func TestRouterTransportFromFilesDisabled(t *testing.T) {
	transport, err := RouterTransportFromFiles("", "", "")
	if err != nil || transport != nil {
		t.Fatalf("RouterTransportFromFiles(\"\") = %v, %v; want nil, nil", transport, err)
	}
}
