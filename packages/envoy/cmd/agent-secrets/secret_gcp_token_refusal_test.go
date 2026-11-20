// packages/envoy/cmd/agent-secrets/secret_gcp_token_refusal_test.go
//
// A token source's refusal, printed through the Secret Manager client's own error: this command's
// real client, built from its real Application Default Credentials, reaches a stand-in Secret
// Manager behind TLS through a CONNECT proxy, and asks the credentials for a token on its first
// call; the credentials' token endpoint refuses the refresh with words that quote a URL carrying a
// credential.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASecretManagerCallsTokenRefusalPrintsNoneOfItsWords: `agent-secrets secret list`, built from
// this package and run as its own process under Application Default Credentials of a person's
// sign-in whose token endpoint refuses the refresh invalid_grant with words that quote a URL
// carrying a credential, fails, and nothing it prints holds the credential: the Secret Manager
// client asks the credentials' token source for a token as it makes its first call, and the
// refusal reaches the command inside that call's error, as the auth library prints a refused
// token ("cannot fetch token: 400", then the answer's body). The token endpoint is asked, and the
// refusal is named, so the row cannot pass on a command that failed before it reached the client.
// The process is the command's own so the SDK's proxy and root certificate caches, each read once
// per process, read this row's environment.
func TestASecretManagerCallsTokenRefusalPrintsNoneOfItsWords(t *testing.T) {
	binary := buildAgentSecrets(t)

	// Secret Manager: the fake, behind TLS with a certificate for its real name.
	authority, err := airlocktest.NewAuthority("secret manager stand-in authority")
	if err != nil {
		t.Fatal(err)
	}
	certificate, _, err := authority.Leaf([]string{gcpAirlockHost})
	if err != nil {
		t.Fatal(err)
	}
	certFile, _, err := authority.WriteFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeSecretManager()
	serviceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(certificate)), grpc.UnaryInterceptor(fake.recordAuthorization))
	secretmanagerpb.RegisterSecretManagerServiceServer(service, fake)
	go func() { _ = service.Serve(serviceListener) }()
	t.Cleanup(service.Stop)
	proxy := connectProxyTo(t, serviceListener.Addr().String())

	// The credentials' token endpoint, refusing every refresh with the words.
	var asked atomic.Int32
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": redacttest.Words})
	}))
	t.Cleanup(tokenEndpoint.Close)
	adc := filepath.Join(t.TempDir(), "application_default_credentials.json")
	data, err := json.Marshal(map[string]string{
		"type": "authorized_user", "client_id": "example-client.apps.example.com",
		"client_secret": "example-client-not-a-secret", "refresh_token": "example-refresh-not-a-secret",
		"token_uri": tokenEndpoint.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adc, data, 0o600); err != nil {
		t.Fatal(err)
	}

	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "secret", "list")
	command.Env = append(os.Environ(),
		"PATH="+t.TempDir(), // no gcloud
		// The credentials file GOOGLE_APPLICATION_CREDENTIALS names outranks every source but a
		// lent token, so the command signs in with the file above.
		"GOOGLE_APPLICATION_CREDENTIALS="+adc,
		"CLOUDSDK_AUTH_ACCESS_TOKEN=", "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=", "CLOUDSDK_CORE_ACCOUNT=", "CLOUDSDK_ACTIVE_CONFIG_NAME=",
		"HTTPS_PROXY="+proxy, "https_proxy="+proxy,
		// The broker and the token endpoint this row started are on loopback, and reached direct.
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
		"SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+t.TempDir(),
		"GOOGLE_API_USE_MTLS_ENDPOINT=never", "GOOGLE_API_USE_CLIENT_CERTIFICATE=false",
	)
	out, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("secret list under refused credentials succeeded:\n%s", out)
	}
	if asked.Load() == 0 || !strings.Contains(string(out), "cannot fetch token: 400") {
		t.Fatalf("secret list did not fail on the token endpoint's refusal (asked %d times, deadline: %v):\n%s", asked.Load(), ctx.Err(), out)
	}
	redacttest.RequireNone(t, "agent-secrets.secret-manager-token", string(out))
}

// connectProxyTo is an HTTP CONNECT proxy on loopback, for the rest of t, that carries every
// tunnel it is asked for to upstream whatever host the tunnel names, and its URL.
func connectProxyTo(t *testing.T, upstream string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				reader := bufio.NewReader(client)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				server, err := net.Dial("tcp", upstream)
				if err != nil {
					_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer server.Close()
				if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(server, reader); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, server); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}
