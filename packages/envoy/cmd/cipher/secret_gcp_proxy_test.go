// packages/envoy/cmd/cipher/secret_gcp_proxy_test.go
//
// The secret forms under an airlock grant of a Google login, end to end: this command's own
// Secret Manager client - gRPC, whose TLS handshake offers h2 and nothing else - reaches Secret
// Manager through the broker's real airlock proxy, holding the grant's placeholder as its access
// token, as `agent-secrets GCLOUD_DEMO -- sh -c 'CLOUDSDK_AUTH_ACCESS_TOKEN="$GCLOUD_DEMO"
// agent-secrets secret …'` runs it. Only the hosts are stand-ins: Secret Manager is the fake
// secret manager of secret_gcp_test.go behind TLS, with a certificate for its real name, and the
// proxy dials it for that name.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/proxy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// The one grant of this file: a Google login whose access token the proxy holds and whose
// placeholder the command holds, served behind a kind of its own bound to Secret Manager's host.
const (
	gcpAirlockHost        = "secretmanager.googleapis.com"
	gcpAirlockKind        = "google"
	gcpAirlockName        = "GCLOUD_TEST"
	gcpAirlockGrantID     = "grant-gcloud-test"
	gcpAirlockToken       = "the-real-access-token-of-the-grant"
	gcpAirlockPlaceholder = placeholder.Prefix + "GgCcLlOoUuDdTtEeSsTtPpLlAaCcEeHh"
	gcpAirlockPolicy      = "policy-v1"
)

// gcpAirlockGrant is the proxy's GrantSource: the one grant above, live, for its placeholder.
type gcpAirlockGrant struct{}

func (gcpAirlockGrant) AirlockConnection(_ context.Context, placeholder string) (requests.AirlockConnection, error) {
	if placeholder != gcpAirlockPlaceholder {
		return requests.AirlockConnection{}, requests.ErrGrantNotLive
	}
	return requests.AirlockConnection{
		GrantID: gcpAirlockGrantID,
		Names: map[string]requests.AirlockGrantName{
			gcpAirlockName: {Type: "google-login", Slots: map[string]string{requests.SlotValue: gcpAirlockPlaceholder}},
		},
	}, nil
}

func (gcpAirlockGrant) AirlockValues(_ context.Context, grantID string) (map[string]requests.AirlockSecret, string, error) {
	if grantID != gcpAirlockGrantID {
		return nil, "", requests.ErrGrantNotLive
	}
	return map[string]requests.AirlockSecret{
		gcpAirlockName: {Type: "google-login", Slots: map[string]string{requests.SlotValue: gcpAirlockToken}, Expires: time.Now().Add(time.Hour)},
	}, gcpAirlockPolicy, nil
}

func (gcpAirlockGrant) PolicyVersion() string { return gcpAirlockPolicy }

func (gcpAirlockGrant) AirlockAccessPlaceholder(context.Context, string, string, string) (string, error) {
	return "", fmt.Errorf("this grant has no Azure exchange")
}

// AirlockRules is the airlock's rules for the one grant: a kind on Secret Manager's host whose
// credential travels in the default Authorization header on every method and path, and the
// grant's one name bound to it as a Google login's token, which the broker mints and the proxy
// swaps in, with no allow of a profile's own.
func (gcpAirlockGrant) AirlockRules() proxyrules.Rules {
	return proxyrules.Rules{
		Kinds: []proxyrules.Kind{{Name: gcpAirlockKind, Hosts: []string{gcpAirlockHost}, Ways: []string{proxyrules.WayMint}}},
		Names: []proxyrules.Binding{{Name: gcpAirlockName, Kind: gcpAirlockKind, Way: proxyrules.WayMint}},
	}
}

// TestGCPSecretListReachesSecretManagerThroughTheAirlockOverGRPC: `agent-secrets secret list`,
// built from this package and run as its own process, with the grant's placeholder in
// CLOUDSDK_AUTH_ACCESS_TOKEN and HTTPS_PROXY naming the proxy with that placeholder as its
// password, lists the secret Secret Manager holds; every Secret Manager call it made reached the
// service carrying the grant's real token and never the placeholder; and nothing it printed
// holds either. Its gRPC client offers h2 alone, so a proxy that answers HTTP/1.1 alone fails
// it with "tls: no application protocol", as every secret form failed under a GCLOUD_DEMO grant
// before the proxy carried HTTP/2. The process is the command's own so the SDK's proxy and root
// certificate caches, each read once per process, read this row's environment.
func TestGCPSecretListReachesSecretManagerThroughTheAirlockOverGRPC(t *testing.T) {
	binary := buildAgentSecrets(t)

	// Secret Manager: the fake, behind TLS with a certificate for its real name.
	fake := newFakeSecretManager()
	held, err := fake.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/" + testGCPProject, SecretId: "agent-secrets-demo-deploy-token",
		Secret: &secretmanagerpb.Secret{Annotations: map[string]string{policy.TagOwner: "shared", policy.TagTier: "agent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: held.GetName()}); err != nil {
		t.Fatal(err)
	}
	serviceAuthority, err := proxyrulestest.NewAuthority("secret manager stand-in authority")
	if err != nil {
		t.Fatal(err)
	}
	serviceCertificate, _, err := serviceAuthority.Leaf([]string{gcpAirlockHost})
	if err != nil {
		t.Fatal(err)
	}
	serviceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(serviceCertificate)), grpc.UnaryInterceptor(fake.recordAuthorization))
	secretmanagerpb.RegisterSecretManagerServiceServer(service, fake)
	go func() { _ = service.Serve(serviceListener) }()
	t.Cleanup(service.Stop)

	// The broker's own airlock proxy, with an authority of its own whose certificate is the
	// only root the command trusts, dialing Secret Manager's stand-in for its name.
	proxyAuthority, err := proxyrulestest.NewAuthority("airlock proxy test authority")
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, err := proxyAuthority.WriteFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := proxy.NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	proxy := proxy.New(proxy.Options{
		CA: ca, Grants: gcpAirlockGrant{},
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != gcpAirlockHost {
				return nil, fmt.Errorf("this test resolves %s alone, and the command asked for %s", gcpAirlockHost, host)
			}
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, serviceListener.Addr().String())
		},
		UpstreamRoots: serviceAuthority.Pool(),
	})
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := proxy.Serve(proxyListener); err != nil && err != http.ErrServerClosed {
			t.Errorf("serve the proxy: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(ctx)
	})
	proxyURL := (&url.URL{Scheme: "http", Host: proxyListener.Addr().String(), User: url.UserPassword("agent-secrets", gcpAirlockPlaceholder)}).String()

	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	ctx, cancel := context.WithTimeout(context.Background(), untilGiveUp(t))
	defer cancel()
	command := exec.CommandContext(ctx, binary, "secret", "list")
	command.Env = append(os.Environ(),
		"CLOUDSDK_AUTH_ACCESS_TOKEN="+gcpAirlockPlaceholder,
		"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL,
		// The broker this row started is on loopback, and reached direct.
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
		"SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+t.TempDir(),
		"GOOGLE_API_USE_MTLS_ENDPOINT=never", "GOOGLE_API_USE_CLIENT_CERTIFICATE=false",
	)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("secret list through the airlock: %v (deadline: %v)\n%s", err, ctx.Err(), out)
	}
	if !strings.Contains(string(out), "DEMO_DEPLOY_TOKEN") {
		t.Fatalf("secret list did not list the secret Secret Manager holds:\n%s", out)
	}
	for _, credential := range []string{gcpAirlockPlaceholder, gcpAirlockToken} {
		if strings.Contains(string(out), credential) {
			t.Fatalf("secret list printed a credential:\n%s", out)
		}
	}
	authorization, calls := fake.calledUnder()
	if calls == 0 || authorization != "Bearer "+gcpAirlockToken {
		t.Fatalf("Secret Manager was called %d times under %q; want every call under the grant's real token, which the proxy swapped in", calls, authorization)
	}
	t.Logf("secret list made %d Secret Manager calls (%v) through the airlock, each under the grant's real token", calls, fake.called())
}
