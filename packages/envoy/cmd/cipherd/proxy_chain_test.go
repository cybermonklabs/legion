// packages/envoy/cmd/cipherd/proxy_chain_test.go
// The broker binary on each form of the proxy's chain: an intermediate from a pool (here the
// stand-in of Certificate Authority Service the local stack runs), which it is not ready
// without, and a mounted intermediate with its chain; and what it refuses to start on.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

const testProxyPool = "projects/example-project/locations/us-west1/caPools/proxy"

// localProxyEnv is what a broker with a proxy is started with on the local stack, less the
// authority: the database at databaseURL, a port of its own, a fake secret store, which makes it
// a local run, and a profiles file declaring one bound kind.
func localProxyEnv(t *testing.T, databaseURL string) []string {
	t.Helper()
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_PROFILES_FILE=" + profilesFile(t, `{"kinds": [{"name": "deel", "hosts": ["api.deel.com"], "ways": ["swap"]}], "profiles": []}`),
		"BROKER_AIRLOCK_PROXY_URL=http://127.0.0.1:13381",
		"BROKER_AIRLOCK_LISTEN_ADDR=127.0.0.1:0",
		"KUBERNETES_SERVICE_HOST=",
	}
}

// healthz answers GET /healthz at addr: its status and body.
func healthz(t *testing.T, addr string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET http://%s/healthz: %v", addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func caStandService(t *testing.T, options proxyrulestest.CAOptions) (*proxyrulestest.CAStand, *proxyrulestest.Service) {
	t.Helper()
	stand, err := proxyrulestest.NewCAStand(options)
	if err != nil {
		t.Fatal(err)
	}
	service, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.AnswerWith(stand.Answer)
	return stand, service
}

// TestABrokerOnAPoolIsNotReadyUntilItHoldsAnIntermediate: a broker whose settings name a pool
// serves, and answers 503 NO_INTERMEDIATE while the service fails every request, logging each
// failure; once the service answers it logs its intermediate, with the names of its bound host,
// and is ready. The stand-in is sent no credential.
func TestABrokerOnAPoolIsNotReadyUntilItHoldsAnIntermediate(t *testing.T) {
	stand, service := caStandService(t, proxyrulestest.CAOptions{})
	stand.Fail(true)
	broker := startBroker(t, brokerOptions{Env: append(localProxyEnv(t, storetest.URL(t)),
		"BROKER_PROXY_CA_POOL="+testProxyPool,
		"BROKER_PROXY_TEST_CA_URL="+service.URL(),
	)})
	broker.AwaitRecord(t, "proxy intermediate request failed", map[string]any{"pool": testProxyPool})
	if status, body := healthz(t, broker.Addr); status != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"NO_INTERMEDIATE"`) {
		t.Fatalf("GET /healthz while the pool fails = %d %s; want 503 NO_INTERMEDIATE", status, body)
	}
	stand.Fail(false)
	line := broker.AwaitRecord(t, "proxy intermediate", map[string]any{"names": "api.deel.com"})
	issued := stand.Issued()
	if len(issued) != 1 || line["serial"] != issued[0].SerialNumber.Text(16) {
		t.Fatalf("the broker logged %v; want the serial of the one certificate the stand issued", line)
	}
	testwait.Until(t, "GET /healthz to answer 200 once the broker holds its intermediate", func() (string, bool) {
		status, body := healthz(t, broker.Addr)
		return body, status == http.StatusOK
	})
	for _, seen := range service.Requests() {
		if got := seen.Header.Get("Authorization"); got != "" {
			t.Fatalf("the stand-in was sent the credential %q; want none", got[:min(len(got), 12)])
		}
	}
}

// caPolicyNames is the names scripts/lib/ca-policy.jq gives the profiles file at path: the policy
// dev-airlock.sh and proxy-chain-check.sh apply to the ca stand before a broker starts on the file.
func caPolicyNames(t *testing.T, path string) []string {
	t.Helper()
	filter := filepath.Join("..", "..", "scripts", "lib", "ca-policy.jq")
	out, err := exec.Command("jq", "-c", "-f", filter, path).Output()
	if err != nil {
		t.Fatalf("jq -c -f %s %s: %v", filter, path, err)
	}
	var policy struct {
		Names []string `json:"names"`
	}
	if err := json.Unmarshal(out, &policy); err != nil {
		t.Fatalf("%s answered %q, no policy: %v", filter, out, err)
	}
	return policy.Names
}

// TestABrokerWithADpopBlockIsReadyUnderTheLocalCAPolicy: for a profiles file of a kind and a dpop
// block, the ca stand's policy scripts/lib/ca-policy.jq gives names both hosts, which boundHosts
// asks the intermediate to permit, so the broker takes the stand's answer, logs an intermediate of
// both names and is ready. Control: under a policy of the kind's hosts alone, the policy a filter
// that read no dpop block gave, the broker refuses every answer and answers 503 NO_INTERMEDIATE.
func TestABrokerWithADpopBlockIsReadyUnderTheLocalCAPolicy(t *testing.T) {
	profiles := profilesFile(t, withKinds(mintedProfiles(`"dispatch.test"`, []string{"RESULTS_TOKEN"}),
		`[{"name": "deel", "hosts": ["api.deel.com"], "ways": ["swap"]}]`))
	names := caPolicyNames(t, profiles)
	if !slices.Equal(names, []string{"api.deel.com", "dispatch.test"}) {
		t.Fatalf("ca-policy.jq names %v for a kind of api.deel.com and a dpop block of dispatch.test; want both", names)
	}
	start := func(t *testing.T, policy []string) *brokerProcess {
		t.Helper()
		_, service := caStandService(t, proxyrulestest.CAOptions{Names: policy})
		return startBroker(t, brokerOptions{Env: append(localProxyEnv(t, storetest.URL(t)),
			"BROKER_PROFILES_FILE="+profiles,
			mintedDestination,
			"BROKER_PROXY_CA_POOL="+testProxyPool,
			"BROKER_PROXY_TEST_CA_URL="+service.URL(),
		)})
	}
	t.Run("the policy ca-policy.jq gives", func(t *testing.T) {
		broker := start(t, names)
		broker.AwaitRecord(t, "proxy intermediate", map[string]any{"names": "api.deel.com dispatch.test"})
		testwait.Until(t, "GET /healthz to answer 200 once the broker holds its intermediate", func() (string, bool) {
			status, body := healthz(t, broker.Addr)
			return body, status == http.StatusOK
		})
	})
	t.Run("control: the kind's hosts alone", func(t *testing.T) {
		broker := start(t, []string{"api.deel.com"})
		broker.AwaitRecord(t, "proxy intermediate request failed", map[string]any{"pool": testProxyPool})
		if status, body := healthz(t, broker.Addr); status != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"NO_INTERMEDIATE"`) {
			t.Fatalf("GET /healthz under a policy without the dpop block's host = %d %s; want 503 NO_INTERMEDIATE", status, body)
		}
		if strings.Contains(broker.Log(), `"message":"proxy intermediate",`) {
			t.Fatalf("the broker took an intermediate that does not permit the dpop block's host:\n%s", broker.Log())
		}
	})
}

// TestABrokerRenewsAThreeDayIntermediateWithTwoDaysLeft: the broker binary, on a pool that answers
// 3-day intermediates and on the clock BROKER_PROXY_TEST_CLOCK_OFFSET_FILE moves, holds its first
// intermediate a minute before 2 days of it remain, and takes the next once they do: a day after
// the first's start, not halfway through its life.
func TestABrokerRenewsAThreeDayIntermediateWithTwoDaysLeft(t *testing.T) {
	clockFile := filepath.Join(t.TempDir(), "clock-offset")
	setOffset := func(offset time.Duration) {
		t.Helper()
		writeWhole(t, clockFile, offset.String()+"\n")
	}
	setOffset(0)
	clock, err := proxy.NewOffsetFileClock(clockFile)
	if err != nil {
		t.Fatal(err)
	}
	stand, service := caStandService(t, proxyrulestest.CAOptions{Now: clock.Now, MaxLifetime: 3 * 24 * time.Hour})
	broker := startBroker(t, brokerOptions{Env: append(localProxyEnv(t, storetest.URL(t)),
		"BROKER_PROXY_CA_POOL="+testProxyPool,
		"BROKER_PROXY_TEST_CA_URL="+service.URL(),
		"BROKER_PROXY_TEST_CLOCK_OFFSET_FILE="+clockFile,
	)})
	var first *x509.Certificate
	testwait.Until(t, "the broker's first intermediate", func() (string, bool) {
		issued := stand.Issued()
		if len(issued) > 0 {
			first = issued[0]
		}
		return fmt.Sprint(len(issued)), first != nil
	})
	broker.AwaitRecord(t, "proxy intermediate", map[string]any{"serial": first.SerialNumber.Text(16)})
	if life := first.NotAfter.Sub(first.NotBefore); life != 3*24*time.Hour {
		t.Fatalf("the stand answered an intermediate of %v; want 3 days", life)
	}
	due := first.NotAfter.Add(-2 * 24 * time.Hour)
	t.Logf("first intermediate %s, %s to %s; 2 days remain from %s, %v after its start",
		first.SerialNumber.Text(16), first.NotBefore.UTC().Format(time.RFC3339), first.NotAfter.UTC().Format(time.RFC3339),
		due.UTC().Format(time.RFC3339), due.Sub(first.NotBefore))

	// A minute before: the broker reads its clock file at least once a second, so a few seconds
	// on it has read the move and still holds the first.
	setOffset(time.Until(due.Add(-time.Minute)))
	time.Sleep(4 * time.Second)
	if got := len(stand.Issued()); got != 1 {
		t.Fatalf("a minute before 2 days remain the stand has issued %d intermediates; want the first alone", got)
	}
	t.Logf("clock at %s, a minute before: still the first intermediate alone", clock.Now().UTC().Format(time.RFC3339))

	setOffset(time.Until(due.Add(time.Minute)))
	var second *x509.Certificate
	testwait.Until(t, "the broker to take its next intermediate once 2 days remain", func() (string, bool) {
		issued := stand.Issued()
		if len(issued) > 1 {
			second = issued[1]
		}
		return fmt.Sprint(len(issued)), second != nil
	})
	broker.AwaitRecord(t, "proxy intermediate", map[string]any{"serial": second.SerialNumber.Text(16)})
	t.Logf("clock at %s, a minute after: next intermediate %s, %s to %s",
		clock.Now().UTC().Format(time.RFC3339), second.SerialNumber.Text(16),
		second.NotBefore.UTC().Format(time.RFC3339), second.NotAfter.UTC().Format(time.RFC3339))
	requireHealthy(t, broker.Addr)
}

// TestABrokerStartsOnAMountedIntermediateAndItsChain is the local half of k21's control: a
// broker whose mounted pair is an intermediate and its chain to its root starts, ready, and logs
// that intermediate.
func TestABrokerStartsOnAMountedIntermediateAndItsChain(t *testing.T) {
	certFile, keyFile, intermediate := mountedIntermediate(t, []string{"api.deel.com"})
	broker := startBroker(t, brokerOptions{Env: append(localProxyEnv(t, storetest.URL(t)),
		"BROKER_AIRLOCK_CA_CERT_FILE="+certFile,
		"BROKER_AIRLOCK_CA_KEY_FILE="+keyFile,
	)})
	broker.AwaitRecord(t, "proxy intermediate", map[string]any{"serial": intermediate.SerialNumber.Text(16), "self_signed": false})
	requireHealthy(t, broker.Addr)
}

// TestMainRefusesAProxyChainItCannotStartOn: an intermediate whose name constraints leave out
// the bound host, and a clock file that holds no duration, each refuse the start naming what is
// wrong. The database is unreachable, so each refusal is made before the broker reaches it.
func TestMainRefusesAProxyChainItCannotStartOn(t *testing.T) {
	binary := buildBroker(t)
	const unreachable = "postgres://nonexistent-host-this-test-must-never-reach/db"
	elsewhereCert, elsewhereKey, _ := mountedIntermediate(t, []string{"elsewhere.example"})
	boundCert, boundKey, _ := mountedIntermediate(t, []string{"api.deel.com"})
	for _, row := range []struct {
		name string
		env  []string
		want []string
	}{
		{"a mounted chain that signs nothing for the bound host", []string{
			"BROKER_AIRLOCK_CA_CERT_FILE=" + elsewhereCert, "BROKER_AIRLOCK_CA_KEY_FILE=" + elsewhereKey,
		}, []string{"BROKER_AIRLOCK_CA_CERT_FILE", "api.deel.com"}},
		{"a clock file with no duration", []string{
			"BROKER_AIRLOCK_CA_CERT_FILE=" + boundCert, "BROKER_AIRLOCK_CA_KEY_FILE=" + boundKey,
			"BROKER_PROXY_TEST_CLOCK_OFFSET_FILE=" + writeTempFile(t, "clock-offset", []byte("two days\n")),
		}, []string{"BROKER_PROXY_TEST_CLOCK_OFFSET_FILE", "no duration"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			out := runRefusedBroker(t, binary, append(localProxyEnv(t, unreachable), row.env...)...)
			for _, want := range row.want {
				if !strings.Contains(out, want) {
					t.Fatalf("the broker refused, but its output does not name %q: %s", want, out)
				}
			}
		})
	}
}

// TestEveryProxyBlocksHostIsBound: the hosts every intermediate must permit exactly are those of
// every kind and inline dpop binding, each once, in the profiles file's order.
func TestEveryProxyBlocksHostIsBound(t *testing.T) {
	rules := proxyrules.Rules{
		Kinds: []proxyrules.Kind{
			{Name: "deel", Hosts: []string{"api.deel.com", "*.deel.com"}},
			{Name: "other", Hosts: []string{"api.deel.com"}},
		},
		Names: []proxyrules.Binding{{Name: "WORKER_TOKEN", Inline: &proxyrules.Kind{Name: "dpop", Hosts: []string{"dispatch.internal.example"}}}},
	}
	want := []string{"api.deel.com", "*.deel.com", "dispatch.internal.example"}
	if got := boundHosts(rules); !slices.Equal(got, want) {
		t.Errorf("boundHosts = %v; want %v", got, want)
	}
}

// TestTheReleaseRootsFileHoldsRootsAlone: a file of two authorities is read in order; one that
// cannot be read, holds nothing, holds a key - which would otherwise ride in every release - or a
// certificate that is no authority is refused, naming the setting.
func TestTheReleaseRootsFileHoldsRootsAlone(t *testing.T) {
	first, second := testAuthority(t, "today's authority"), testAuthority(t, "the new root")
	roots, err := readProxyReleaseRoots(writeTempFile(t, "roots.pem", slices.Concat(first.CertificatePEM(), second.CertificatePEM())))
	if err != nil || len(roots) != 2 || roots[0].Subject.CommonName != "today's authority" || roots[1].Subject.CommonName != "the new root" {
		t.Fatalf("readProxyReleaseRoots of two roots = %d roots, %v; want both, in order", len(roots), err)
	}
	if roots, err := readProxyReleaseRoots(""); roots != nil || err != nil {
		t.Fatalf("readProxyReleaseRoots of no path = %v, %v; want none", roots, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	_, leafPEM, err := first.Leaf([]string{"leaf.example"})
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"a path that cannot be read":         filepath.Join(t.TempDir(), "absent.pem"),
		"a file with no certificate":         writeTempFile(t, "empty.pem", []byte("no PEM here\n")),
		"a key beside a root":                writeTempFile(t, "key.pem", slices.Concat(first.CertificatePEM(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))),
		"a certificate that is no authority": writeTempFile(t, "leaf.pem", leafPEM),
	} {
		if _, err := readProxyReleaseRoots(path); err == nil || !strings.Contains(err.Error(), "BROKER_PROXY_RELEASE_ROOTS_FILE") {
			t.Errorf("%s: readProxyReleaseRoots = %v; want a refusal naming the setting", name, err)
		}
	}
}

// testAuthority is a self-signed test authority whose common name is name.
func testAuthority(t *testing.T, name string) *proxyrulestest.Authority {
	t.Helper()
	authority, err := proxyrulestest.NewAuthority(name)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// mountedIntermediate writes an intermediate of path length 0 for names, signed by a root of
// its own, as the pair a deployment mounts: the certificate file holds the intermediate and then
// the root, the key file the intermediate's key.
func mountedIntermediate(t *testing.T, names []string) (certFile, keyFile string, intermediate *x509.Certificate) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mounted test root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(4242), Subject: pkix.Name{CommonName: "mounted test intermediate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(7*24*time.Hour - time.Hour),
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
		ExtKeyUsage:         []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		PermittedDNSDomains: names, PermittedDNSDomainsCritical: true,
		ExcludedIPRanges: []*net.IPNet{{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}, {IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	if intermediate, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})...)
	return writeTempFile(t, "chain.crt", chain), writeTempFile(t, "chain.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), intermediate
}
