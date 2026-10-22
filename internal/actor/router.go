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
	"fmt"
	"net"
	"net/http"
	"os"
)

// Paths at which manifest.BuildControllerDeployment projects the router client
// credentials into the controller pod.
const (
	// DefaultRouterClientCertFile is the podidentity credential bundle (private
	// key followed by the certificate chain) the controller presents to
	// atenet-router. Its SPIFFE ID must be in the router's
	// --ingress-allowed-spiffe-ids.
	DefaultRouterClientCertFile = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	// DefaultServiceDNSCAFile is the servicedns trust bundle that issued the
	// serving certificates of Substrate's in-cluster services (ate-api and
	// atenet-router).
	DefaultServiceDNSCAFile = "/run/servicedns-ca/trust-bundle.pem"
	// DefaultRouterServerName is the DNS name in atenet-router's serving
	// certificate.
	DefaultRouterServerName = "atenet-router.ate-system.svc"
)

// routerTLSConfig returns a TLS config for atenet-router's mTLS listener. It
// presents the client credential bundle at certFile and verifies the router's
// serving certificate for serverName against the trust bundle at caFile, using
// Go's standard certificate verification.
//
// The files are read when the config is built, so callers that must follow
// rotation (the kubelet rewrites projected pod certificates and trust bundles
// in place) should build a new config per connection, as
// RouterTransportFromFiles does.
func routerTLSConfig(certFile, caFile, serverName string) (*tls.Config, error) {
	cert, err := loadClientCertificate(certFile)
	if err != nil {
		return nil, err
	}
	roots, err := loadCertPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ServerName:   serverName,
		RootCAs:      roots,
		Certificates: []tls.Certificate{*cert},
	}, nil
}

// RouterTransportFromFiles returns an mTLS transport for atenet-router built
// from the given files, or nil (meaning http.DefaultTransport) when certFile
// is empty.
//
// Every new connection re-reads the credential bundle and trust bundle, so the
// transport keeps working across certificate and CA rotation.
func RouterTransportFromFiles(certFile, caFile, serverName string) (http.RoundTripper, error) {
	if certFile == "" {
		return nil, nil
	}
	// Fail fast on a misconfigured pod rather than on the first request.
	if _, err := routerTLSConfig(certFile, caFile, serverName); err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		cfg, err := routerTLSConfig(certFile, caFile, serverName)
		if err != nil {
			return nil, err
		}
		d := &tls.Dialer{Config: cfg}
		return d.DialContext(ctx, network, addr)
	}
	return t, nil
}

func loadClientCertificate(certFile string) (*tls.Certificate, error) {
	bundle, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("reading router client credential bundle: %w", err)
	}
	// The podcertificate credential bundle holds the private key and the
	// certificate chain in one PEM file.
	cert, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		return nil, fmt.Errorf("parsing router client credential bundle %s: %w", certFile, err)
	}
	return &cert, nil
}

func loadCertPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading trust bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("trust bundle %s holds no certificates", caFile)
	}
	return pool, nil
}
