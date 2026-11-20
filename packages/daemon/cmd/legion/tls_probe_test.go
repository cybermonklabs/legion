package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// servedTLSSiteNames are the fixture listener's sites, in the probe's order (tlsServer.sites).
var servedTLSSiteNames = []string{"emSign", "AAA", "control"}

// testTLSVia is the server address testTLSSites name. No server listens there: a test that needs
// one starts the probe's own (newTLSServer).
const testTLSVia = "127.0.0.1:43210"

// testTLSSites are sites shaped as the probe's: the three of its fixture listener, the control's
// trust file under stateDir, and the two vendored chains, all at testTLSVia, then the two public
// sites.
func testTLSSites(stateDir string) []tlsSite {
	trust := filepath.Join(stateDir, "fixture-control-root.pem")
	return []tlsSite{
		{name: "emSign", url: "https://" + tlsEmSignHost + "/", root: filepath.Join(stateDir, "fixture-emsign-root.pem"), via: testTLSVia},
		{name: "AAA", url: "https://" + tlsAAAHost + "/", root: filepath.Join(stateDir, "fixture-aaa-root.pem"), via: testTLSVia},
		{name: "control", url: "https://" + tlsControlHost + "/", root: trust, control: true, via: testTLSVia, trust: trust},
		{name: "vendored emSign", url: publicEmSignTLSSite.url, root: publicEmSignTLSSite.root, vendored: true, via: testTLSVia},
		{name: "vendored AAA", url: publicAAATLSSite.url, root: publicAAATLSSite.root, vendored: true, via: testTLSVia},
		publicEmSignTLSSite, publicAAATLSSite,
	}
}

// testTLSSite is the site of testTLSSites named name.
func testTLSSite(stateDir, name string) tlsSite {
	sites := testTLSSites(stateDir)
	return sites[slices.IndexFunc(sites, func(site tlsSite) bool { return site.name == name })]
}

// controlTLSSite is the control of testTLSSites, for a test that classifies one client's output and
// needs only a site to name.
var controlTLSSite = testTLSSite("", "control")

// tlsSiteHosts is each testTLSSites site's host name, by site name, written out rather than derived
// from its URL as the clients derive it.
var tlsSiteHosts = map[string]string{
	"emSign":          "dv-g3b-emudhra.we-have-a-problem.com",
	"AAA":             "dv-g3b-aaa-emudhra.we-have-a-problem.com",
	"control":         "control.legion-tls-probe.test",
	"vendored emSign": "dv-g3b-emudhra.we-have-a-problem.com",
	"vendored AAA":    "dv-g3b-aaa-emudhra.we-have-a-problem.com",
	"public emSign":   "dv-g3b-emudhra.we-have-a-problem.com",
	"public AAA":      "dv-g3b-aaa-emudhra.we-have-a-problem.com",
}

// unaskedPublicTLSSites stands in for probePublicTLSSites in the command's tests: no public site
// is asked, as where none can be reached.
func unaskedPublicTLSSites(context.Context, []string, string) []tlsSite {
	sites := slices.Clone(publicTLSSites)
	for index := range sites {
		sites[index].unasked = "the test asks no public site"
	}
	return sites
}

// askedVendoredTLSSites stands in for probeVendoredTLSSites: each vendored site of sites, asked, as
// where its positive control passed.
func askedVendoredTLSSites(_ context.Context, _ []string, _ string, sites []tlsSite) []tlsSite {
	var vendored []tlsSite
	for _, site := range sites {
		if site.vendored {
			vendored = append(vendored, site)
		}
	}
	return vendored
}

// fakeTLSClient gives each site the outcome outcomes names for it. At a site the probe's server
// serves, it counts a handshake there first, as a client that reached the server does.
type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite) tlsOutcome {
	if site.seen != nil {
		site.seen.Add(1)
	}
	return c.outcomes[site.name]
}

func passingImageTLSClients(string, []string, string) []tlsClient {
	outcomes := map[string]tlsOutcome{
		"emSign":          {status: tlsCertificateRefused, detail: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"},
		"AAA":             {status: tlsCertificateRefused, detail: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"},
		"control":         {status: tlsConnectionAccepted, detail: "HTTP 200"},
		"vendored emSign": {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		"vendored AAA":    {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		"public emSign":   {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
		"public AAA":      {status: tlsCertificateRefused, detail: "CERT_REVOKED"},
	}
	names := workerTLSClientNames()
	clients := make([]tlsClient, 0, len(names))
	for _, name := range names {
		clients = append(clients, fakeTLSClient{clientName: name, outcomes: outcomes})
	}
	return clients
}

// At a site the probe's server serves, the compromised chains must be refused for a certificate
// error and the control accepted, and anything else fails the probe; at a vendored chain, a client
// that got past the chain to the server's signature accepted it. At a public site only an
// acceptance fails it: a refusal is recorded, and any other outcome is unknown and fails nothing,
// as is a site no client asks.
func TestProbeTLSClientsDecisions(t *testing.T) {
	refused := tlsOutcome{status: tlsCertificateRefused, detail: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY"}
	accepted := tlsOutcome{status: tlsConnectionAccepted, detail: "HTTP 200"}
	unavailable := tlsOutcome{status: tlsNetworkUnavailable, detail: "dial tcp: network is unreachable"}
	revoked := tlsOutcome{status: tlsCertificateRefused, detail: "CERT_REVOKED"}
	pastTheChain := tlsOutcome{status: tlsNetworkUnavailable, detail: "SSL_connect failed: bad signature", signatureFailed: true}
	sites := testTLSSites(t.TempDir())
	passing := func() map[string]tlsOutcome {
		return map[string]tlsOutcome{"emSign": refused, "AAA": refused, "control": accepted, "vendored emSign": revoked, "vendored AAA": revoked, "public emSign": revoked, "public AAA": revoked}
	}
	with := func(site string, outcome tlsOutcome) map[string]tlsOutcome {
		outcomes := passing()
		outcomes[site] = outcome
		return outcomes
	}

	for _, testCase := range []struct {
		name     string
		outcomes map[string]tlsOutcome
		want     string
		unknown  []string
	}{
		{name: "both compromised chains refused and control reachable", outcomes: passing()},
		{name: "compromised chain accepted", outcomes: with("emSign", accepted), want: "node accepts the emSign chain"},
		{name: "compromised chain neither refused nor accepted", outcomes: with("emSign", unavailable), want: "node cannot verify refusal of the emSign chain: dial tcp: network is unreachable"},
		{name: "control unreachable", outcomes: with("control", unavailable), want: "network unavailable: node cannot reach https://control.legion-tls-probe.test/: dial tcp: network is unreachable"},
		{name: "vendored chain accepted past it to the server's signature", outcomes: with("vendored AAA", pastTheChain), want: "node accepts the vendored AAA chain: it trusts a root of that chain's root's name, and got past the chain's names to a signature check, which the probe's chains cannot pass: SSL_connect failed: bad signature"},
		{name: "vendored chain neither refused nor accepted", outcomes: with("vendored emSign", unavailable), want: "node cannot verify refusal of the vendored emSign chain"},
		{name: "public chain accepted", outcomes: with("public AAA", accepted), want: "node accepts the public AAA chain: HTTP 200"},
		{name: "public site unreachable", outcomes: with("public emSign", unavailable), unknown: []string{"public emSign"}},
		{name: "public site past the probe's deadline", outcomes: with("public emSign", tlsOutcome{status: tlsNetworkUnavailable, detail: tlsProbeDeadline}), want: "node cannot verify refusal of the public emSign chain: probe deadline"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			observations, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: testCase.outcomes}}, sites)
			if testCase.want == "" && err != nil {
				t.Fatalf("probeTLSClients() = %v, want success", err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("probeTLSClients() = %v, want error containing %q", err, testCase.want)
			}
			for _, observation := range observations {
				if unknown := slices.Contains(testCase.unknown, observation.site.name); unknown != (observation.status == tlsPublicUnknown) {
					t.Errorf("node at %s = %+v, want unknown %v", observation.site.name, observation.tlsOutcome, unknown)
				}
			}
		})
	}
}

// A public site that gave no verdict is asked by no later client, so an unreachable host costs one
// client's bound; a public site whose positive control failed is asked by none. Neither fails the
// probe, and each later observation says why it was not asked.
func TestProbeTLSClientsStopAskingAPublicSiteThatGaveNoVerdict(t *testing.T) {
	sites := testTLSSites(t.TempDir())
	sites[len(sites)-1].unasked = "its positive control was unavailable: no route"
	asked := map[string]int{}
	client := func(name string, publicEmSign tlsOutcome) tlsClient {
		return countingTLSClient{asked: asked, fakeTLSClient: fakeTLSClient{clientName: name, outcomes: map[string]tlsOutcome{
			"emSign": {status: tlsCertificateRefused}, "AAA": {status: tlsCertificateRefused}, "control": {status: tlsConnectionAccepted},
			"vendored emSign": {status: tlsCertificateRefused}, "vendored AAA": {status: tlsCertificateRefused},
			"public emSign": publicEmSign, "public AAA": {status: tlsConnectionAccepted, detail: "a site no client asks"},
		}}}
	}
	clients := []tlsClient{
		client("node", tlsOutcome{status: tlsCertificateRefused, detail: "CERT_REVOKED"}),
		client("bun", tlsOutcome{status: tlsNetworkUnavailable, detail: "Could not resolve host"}),
		client("curl", tlsOutcome{status: tlsConnectionAccepted, detail: "a site no client asks after bun"}),
	}

	observations, err := probeTLSClients(context.Background(), clients, sites)
	if err != nil {
		t.Fatalf("probeTLSClients() = %v, want success", err)
	}
	if asked["public emSign"] != 2 || asked["public AAA"] != 0 {
		t.Fatalf("public sites asked %v, want public emSign twice (node, bun) and public AAA never", asked)
	}
	for _, observation := range observations {
		switch {
		case observation.client == "curl" && observation.site.name == "public emSign":
			if observation.status != tlsPublicUnknown || observation.detail != "not asked: bun gave no verdict there: Could not resolve host" {
				t.Errorf("curl at public emSign = %+v, want unknown, not asked because of bun", observation.tlsOutcome)
			}
		case observation.site.name == "public AAA":
			if observation.status != tlsPublicUnknown || observation.detail != "not asked: its positive control was unavailable: no route" {
				t.Errorf("%s at public AAA = %+v, want unknown, not asked because of its positive control", observation.client, observation.tlsOutcome)
			}
		}
	}
}

// countingTLSClient counts, in asked, each site its fake client is asked at.
type countingTLSClient struct {
	fakeTLSClient
	asked map[string]int
}

func (c countingTLSClient) probe(ctx context.Context, site tlsSite) tlsOutcome {
	c.asked[site.name]++
	return c.fakeTLSClient.probe(ctx, site)
}

// A verdict at a site the probe's server serves is the client's only where the server began a
// handshake for that site while the client ran: a client that went to the public host of that
// name, or answered without reaching any, fails the probe whatever it said.
func TestProbeTLSClientsCountAVerdictOnlyWhereTheServerSawTheClient(t *testing.T) {
	sites := testTLSSites(t.TempDir())[:len(servedTLSSiteNames)]
	for index := range sites {
		sites[index].seen = new(atomic.Int64)
	}
	outcomes := map[string]tlsOutcome{"emSign": {status: tlsCertificateRefused, detail: "CERT_REVOKED"}, "AAA": {status: tlsCertificateRefused}, "control": {status: tlsConnectionAccepted}}
	bypassing := countingTLSClient{asked: map[string]int{}, fakeTLSClient: fakeTLSClient{clientName: "node", outcomes: outcomes}}

	_, err := probeTLSClients(context.Background(), []tlsClient{bypassingTLSClient{bypassing}}, sites)
	want := "node cannot verify refusal of the emSign chain: never reached the probe's server, so the verdict is not the probe's chain's: CERT_REVOKED"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "network unavailable: node cannot reach https://control.legion-tls-probe.test/") {
		t.Fatalf("probeTLSClients() with a client that never reached the server = %v, want %q and the control unreachable", err, want)
	}
	if _, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: outcomes}}, sites); err != nil {
		t.Fatalf("probeTLSClients() with a client that reached the server = %v, want success", err)
	}
}

// bypassingTLSClient answers without reaching the probe's server.
type bypassingTLSClient struct{ countingTLSClient }

func (c bypassingTLSClient) probe(_ context.Context, site tlsSite) tlsOutcome {
	return c.outcomes[site.name]
}

func TestTLSCertificateErrorRecognizesClientRefusals(t *testing.T) {
	for _, detail := range []string{
		"Error: certificate revoked",
		"invalid peer certificate: Other(OtherError(CertDistrusted))",
		"SSL validation failed: certificate verify failed",
		"curl: (60) SSL certificate problem",
		"x509: certificate signed by unknown authority",
	} {
		if !tlsCertificateError(detail) {
			t.Errorf("tlsCertificateError(%q) = false, want true", detail)
		}
	}
}

// curl names one refusal in several ways, by version and by TLS backend, and an image may carry
// any of them: an image built on another distribution keeps that distribution's curl. git 2.53.0's
// https transport, through libcurl's GnuTLS build, printed the Ubuntu row in the image probe of a
// worker image built on Ubuntu 26.04 on 2026-11-19. Read as no certificate error, a refusal is
// "unavailable" and the probe fails an image that is safe.
func TestTLSCertificateErrorRecognizesEachCurlRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name, detail string
		want         bool
	}{
		{"curl 8.14", "curl: (60) SSL certificate problem: unable to get local issuer certificate", true},
		{"curl 8.18", "curl: (60) SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)", true},
		{"git 2.53.0 on Ubuntu 26.04", `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': SSL certificate verification failed: certificate signer not trusted. (CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none)`, true},
		{"no route", "curl: (7) Failed to connect to example.com port 443 after 3 ms: Could not connect to server", false},
		{"no such host", `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': Could not resolve host: dv-g3b-emudhra.we-have-a-problem.com`, false},
		{"timeout", "curl: (28) SSL connection timeout", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tlsCertificateError(testCase.detail); got != testCase.want {
				t.Fatalf("tlsCertificateError(%q) = %v, want %v", testCase.detail, got, testCase.want)
			}
		})
	}
}

// What the floor clients wget, git, go and kubectl wrote to stderr at the emSign site and at the
// control, with the forms workerTLSClients runs, in an INFRA issue pod on 2026-11-18 (GNU wget
// 1.25.0, git 2.47.3, go 1.27.3, kubectl 1.35.9), each output's lines joined by a space as the
// probe's detail joins them. kubectl wrote the control's page to stdout, which the probe discards.
const (
	wgetEmSignRefusal    = `ERROR: The certificate of 'dv-g3b-emudhra.we-have-a-problem.com' is not trusted. ERROR: The certificate of 'dv-g3b-emudhra.we-have-a-problem.com' doesn't have a known issuer.`
	wgetControlAnswer    = `2026-11-18 06:11:03 URL:https://example.com/ [577] -> "/dev/null" [1]`
	gitEmSignRefusal     = `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': server verification failed: certificate signer not trusted. (CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none)`
	gitControlAnswer     = `fatal: repository 'https://example.com/' not found`
	goEmSignRefusal      = `go: dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe@latest: unrecognized import path "dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe": https fetch: Get "https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe?go-get=1": tls: failed to verify certificate: x509: certificate signed by unknown authority`
	goControlAnswer      = `go: example.com/legion-tls-probe@latest: unrecognized import path "example.com/legion-tls-probe": reading https://example.com/legion-tls-probe?go-get=1: 404 Not Found`
	kubectlEmSignRefusal = `Unable to connect to the server: tls: failed to verify certificate: x509: certificate signed by unknown authority`
	// git and go with their certificate checks turned off (GIT_SSL_NO_VERIFY=true, GOINSECURE=*),
	// at the emSign site, measured in the same pod: what each says once it has accepted a
	// compromised chain.
	gitEmSignAccepted = `fatal: https://dv-g3b-emudhra.we-have-a-problem.com/info/refs not valid: is this a git repository?`
	goEmSignAccepted  = `go: dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe@latest: unrecognized import path "dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe": parse https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe?go-get=1: no go-import meta tags ()`
	// redis-cli 8.0.2's, measured on 2026-11-18 on the pull request's image (sha256:25d2edfc…) as
	// `redis-cli --tls --sni <host> -h <host> -p 443 PING`: the emSign site refuses at the
	// handshake, example.com answers past it with HTTP, and with --insecure the emSign site closes
	// the connection only after the handshake.
	redisEmSignRefusal  = `Could not connect to Redis at dv-g3b-emudhra.we-have-a-problem.com:443: SSL_connect failed: certificate verify failed`
	redisControlAnswer  = `Error: Protocol error, got "H" as reply type byte`
	redisEmSignAccepted = `Error: Server closed the connection`
)

func TestTLSCommandDetailKeepsCertificateErrorTail(t *testing.T) {
	detail := trimTLSDetail(tlsCommandMessage(nil, strings.Repeat("x", tlsDetailLimit)+" invalid peer certificate"))
	if !tlsCertificateError(detail) {
		t.Fatalf("detail = %q, dropped the certificate refusal", detail)
	}
}

// A detail longer than tlsDetailLimit whose head's bound falls inside a URL's user name, before
// its "@", or whose end's window starts inside a URL's query, after its "?", keeps no part of
// either: trimTLSDetail gives the whole detail through the URL rule before it cuts it. A cut made
// first keeps the start of the user name, where the rule reads it as the URL's host, or the end of
// the query with no "?" before it, where the rule finds no query. A detail the rule brings within
// tlsDetailLimit is kept whole, rather than as a head and an end that overlap.
func TestTrimTLSDetailKeepsNoPartOfAURLItsBoundsFallInside(t *testing.T) {
	const user, query = "legion-reader", "token=planted-query-token-8"
	head, window := tlsDetailLimit/3, tlsDetailLimit-tlsDetailLimit/3-1
	// part is the first four bytes in a row of planted that text holds, and "" where it holds none.
	part := func(text, planted string) string {
		for i := 0; i+4 <= len(planted); i++ {
			if strings.Contains(text, planted[i:i+4]) {
				return planted[i : i+4]
			}
		}
		return ""
	}
	headDetail := strings.Repeat("w", 85) + " https://" + user + "@h.example/p " + strings.Repeat("z", 400)
	tailDetail := strings.Repeat("w", 400) + " https://h.example/p?" + query + " " + strings.Repeat("z", 177)
	for _, tc := range []struct {
		name, detail, planted string
		// bound is what the bound alone keeps of the detail: its head, or its end's window; ""
		// where the detail's length is what the case reads.
		bound, want string
	}{
		{
			name:    "a head whose bound falls inside a URL's user name",
			detail:  headDetail,
			planted: user,
			bound:   headDetail[:head],
			want:    strings.Repeat("w", 85) + " https://h.exam" + "…" + strings.Repeat("z", window),
		},
		{
			name:    "an end whose window starts inside a URL's query",
			detail:  tailDetail,
			planted: query,
			bound:   tailDetail[len(tailDetail)-window:],
			want:    strings.Repeat("w", head) + "…" + "w https://h.example/p " + strings.Repeat("z", 177),
		},
		{
			name:    "a detail the rule brings within the bound",
			detail:  strings.Repeat("w", 260) + " https://h.example/p?" + query,
			planted: query,
			want:    strings.Repeat("w", 260) + " https://h.example/p",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.bound != "" && (part(tc.bound, tc.planted) == "" || strings.Contains(tc.bound, tc.planted)) {
				t.Fatalf("the bound alone keeps %q, which does not cut %q: the case tests nothing", tc.bound, tc.planted)
			}
			got := trimTLSDetail(tc.detail)
			for _, planted := range []string{user, query} {
				if held := part(got, planted); held != "" {
					t.Fatalf("trimTLSDetail = %q, which holds %q of %q", got, held, planted)
				}
			}
			if got != tc.want {
				t.Fatalf("trimTLSDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTLSCommandClientClassifiesApplicationErrorBeforeTruncatingDetail(t *testing.T) {
	response := strings.Repeat("x", tlsDetailLimit) + " invalid XML received " + strings.Repeat("y", tlsDetailLimit)
	client := tlsCommandClient{
		clientName:      "aws",
		timeout:         tlsDefaultProbeTimeout,
		applicationSeen: awsResponseSeen,
		command: func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 255`, "sh", response}, nil
		},
	}

	outcome := client.probe(context.Background(), controlTLSSite)

	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("client.probe() = %+v, want an accepted application response", outcome)
	}
}

func TestImageTLSClientsCoverTheWorkerInventory(t *testing.T) {
	clients := imageTLSClients("omp", nil, "")
	want := []struct {
		name     string
		timeout  time.Duration
		optional bool
	}{
		{"node", tlsDefaultProbeTimeout, false},
		{"bun", tlsDefaultProbeTimeout, false},
		{"omp", tlsDefaultProbeTimeout, false},
		{"codegraph-node", tlsDefaultProbeTimeout, false},
		{"uv", tlsDefaultProbeTimeout, false},
		{"aws", tlsDefaultProbeTimeout, false},
		{"curl", tlsDefaultProbeTimeout, false},
		{"python3", tlsDefaultProbeTimeout, false},
		{"gh", tlsDefaultProbeTimeout, false},
		{"wget", tlsDefaultProbeTimeout, false},
		{"git", tlsDefaultProbeTimeout, false},
		{"go", tlsDefaultProbeTimeout, false},
		{"kubectl", tlsDefaultProbeTimeout, false},
		{"redis-cli", tlsDefaultProbeTimeout, false},
		{"gcloud", tlsDefaultProbeTimeout, true},
		{"bq", tlsBigQueryProbeTimeout, true},
		{"az", tlsDefaultProbeTimeout, true},
		{"mise", tlsDefaultProbeTimeout, true},
		{"socat", tlsDefaultProbeTimeout, true},
		{"chromium", tlsChromiumProbeTimeout, false},
	}
	if len(workerTLSClients) != len(want) || len(clients) != len(want) {
		t.Fatalf("inventory has %d specifications and %d clients, want %d", len(workerTLSClients), len(clients), len(want))
	}
	for index, expected := range want {
		spec := workerTLSClients[index]
		if spec.name != expected.name || spec.timeout != expected.timeout || clients[index].name() != expected.name {
			t.Errorf("inventory client %d = (%q, %s, %q), want (%q, %s, %q)", index, spec.name, spec.timeout, clients[index].name(), expected.name, expected.timeout, expected.name)
		}
		var timeout time.Duration
		optional := false
		switch client := clients[index].(type) {
		case tlsCommandClient:
			timeout = client.timeout
		case tlsOptionalClient:
			timeout, optional = client.timeout, true
		case tlsChromiumClient:
			timeout = client.timeout
		default:
			t.Errorf("inventory client %d = %T, want a command client", index, client)
			continue
		}
		if timeout != expected.timeout {
			t.Errorf("image client %s timeout = %s, want inventory timeout %s", expected.name, timeout, expected.timeout)
		}
		// An optional client passes an image that lacks it, so a floor client must never be one.
		if optional != expected.optional {
			t.Errorf("image client %s optional = %v, want %v", expected.name, optional, expected.optional)
		}
	}
	if workerTLSClients[len(workerTLSClients)-1].timeout <= tlsDefaultProbeTimeout {
		t.Errorf("Chromium timeout %s must exceed the default %s", workerTLSClients[len(workerTLSClients)-1].timeout, tlsDefaultProbeTimeout)
	}
}

// optionalTLSClientNames names each optional client of the image.
var optionalTLSClientNames = []string{"gcloud", "bq", "az", "mise", "socat"}

// imageTLSClientsOf returns the image's clients of type T by name, built as the probe builds them
// under env with stateDir as their state directory, failing t unless each of names is one of them.
// Every image client is a tlsCommandClient underneath, so for that T it returns every client, an
// optional or Chromium client as the command client it embeds, whose command line it runs.
func imageTLSClientsOf[T tlsClient](t *testing.T, env []string, stateDir string, names ...string) map[string]T {
	t.Helper()
	clients := map[string]T{}
	add := func(client any) {
		if typed, ok := client.(T); ok {
			clients[typed.name()] = typed
		}
	}
	for _, client := range imageTLSClients("omp", env, stateDir) {
		switch client := client.(type) {
		case T:
			add(client)
		case tlsOptionalClient:
			add(client.tlsCommandClient)
		case tlsChromiumClient:
			add(client.tlsCommandClient)
		}
	}
	for _, name := range names {
		if _, ok := clients[name]; !ok {
			var zero T
			t.Fatalf("imageTLSClients() has no %T named %s", zero, name)
		}
	}
	return clients
}

// The probe's server's sites are reached through it: testTLSSites name its address, testTLSVia.
const testTLSViaHost, testTLSViaPort = "127.0.0.1", "43210"

// wget, git, go, kubectl, redis-cli and uv are each classified from the output and exit status
// their program gave: the control's answer is an acceptance and the emSign site's refusal a
// refusal. git, go, redis-cli and uv fail at the control as well, so each needs the response
// marker its client reads, and the same marker reads the answer each gives at a compromised site
// whose chain it accepted. uv's control and refusal rows were measured on 2026-11-18 with uv
// 0.13.5 at example.com and the emSign site, and its accepting row on the image of 841974e9
// (sha256:37018eaa…, uv 0.13.5) in `legion probe-image`: an image whose /etc/uv/uv.toml allows
// the emSign and AAA hosts made uv accept both chains. At 841974e9, before uvResponseSeen read
// "Unsupported `Content-Type`", the probe failed naming uv as unable to verify a refusal rather
// than as accepting the chain, because the site's text/plain page was not an answer
// uvResponseSeen read.
func TestImageFloorTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), "wget", "git", "go", "kubectl", "redis-cli", "uv")
	for _, testCase := range []struct {
		client, name, stderr, exit string
		want                       tlsStatus
	}{
		{client: "wget", name: "control", exit: "0", stderr: wgetControlAnswer, want: tlsConnectionAccepted},
		{client: "wget", name: "refusal", exit: "5", stderr: wgetEmSignRefusal, want: tlsCertificateRefused},
		{client: "git", name: "control", exit: "128", stderr: gitControlAnswer, want: tlsConnectionAccepted},
		{client: "git", name: "refusal", exit: "128", stderr: gitEmSignRefusal, want: tlsCertificateRefused},
		{client: "git", name: "accepting a compromised chain", exit: "128", stderr: gitEmSignAccepted, want: tlsConnectionAccepted},
		{client: "go", name: "control", exit: "1", stderr: goControlAnswer, want: tlsConnectionAccepted},
		{client: "go", name: "refusal", exit: "1", stderr: goEmSignRefusal, want: tlsCertificateRefused},
		{client: "go", name: "accepting a compromised chain", exit: "1", stderr: goEmSignAccepted, want: tlsConnectionAccepted},
		{client: "kubectl", name: "control", exit: "0", want: tlsConnectionAccepted},
		{client: "kubectl", name: "refusal", exit: "1", stderr: kubectlEmSignRefusal, want: tlsCertificateRefused},
		{client: "redis-cli", name: "control", exit: "1", stderr: redisControlAnswer, want: tlsConnectionAccepted},
		{client: "redis-cli", name: "refusal", exit: "1", stderr: redisEmSignRefusal, want: tlsCertificateRefused},
		{client: "redis-cli", name: "accepting a compromised chain", exit: "1", stderr: redisEmSignAccepted, want: tlsConnectionAccepted},
		{client: "uv", name: "control", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: No solution found when resolving dependencies\n  cause: Because legion-tls-probe-not-a-package was not found in the package registry and you require legion-tls-probe-not-a-package, we can conclude that your requirements are unsatisfiable.", want: tlsConnectionAccepted},
		{client: "uv", name: "refusal", exit: "2", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Failed to fetch: https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/\n  cause: error sending request for url (https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/)\n  cause: client error (Connect)\n  cause: invalid peer certificate: Other(OtherError(CertDistrusted))\n\nhint: Consider enabling use of system TLS certificates with the `--system-certs` command-line flag", want: tlsCertificateRefused},
		{client: "uv", name: "accepting a compromised chain", exit: "1", stderr: "Using Python 3.13.5 environment at: /usr\nerror: Unsupported `Content-Type` \"text/plain\" for `https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/`. Expected JSON or HTML.", want: tlsConnectionAccepted},
	} {
		t.Run(testCase.client+" "+testCase.name, func(t *testing.T) {
			client := clients[testCase.client]
			client.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1" >&2; exit "$2"`, "sh", testCase.stderr, testCase.exit}, nil
			}

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
				t.Fatalf("%s probe = %+v, want status %q", testCase.client, outcome, testCase.want)
			}
		})
	}
}

// tlsRefusalRow is what one client wrote at the public emSign site and at the probe's own emSign
// chain, measured on 2026-11-20 in the image of 1c7c825367
// (ghcr.io/cybermonklabs/legion-worker@sha256:36c4d8ea…), each with the command workerTLSClients
// runs, the public site reached directly and the fixture through the probe's server, its lines
// joined by a space as the probe's detail joins them, and a long one shortened, its refusal kept.
// The AAA rows read the same with the AAA host. same says whether the two are one trust decision.
type tlsRefusalRow struct {
	client, exit, public, fixture string
	stdout, same                  bool
}

// tlsRefusalRows: fifteen clients refuse the fixture chain with the words they refuse the public
// site's, each because no store of theirs holds a root of that name. node, CodeGraph's node, bun,
// omp and uv do not: at the public site each finds the chain revoked or distrusted ("CERT_REVOKED",
// "CertDistrusted") by a list keyed to the public chain's own certificates, and at the fixture,
// which no such list names, each refuses an unknown issuer. Only the public sites show those five
// refusing the real chains, which is why the probe asks them still (tlsSite).
var tlsRefusalRows = []tlsRefusalRow{
	{client: "node", exit: "1", same: false,
		public:  `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: Error: certificate revoked at TLSSocket.onConnectSecure (node:internal/tls/wrap:1788:34) { code: 'CERT_REVOKED' } } Node.js v24.22.1`,
		fixture: `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: Error: unable to get local issuer certificate at TLSSocket.onConnectSecure (node:internal/tls/wrap:1788:34) { code: 'UNABLE_TO_GET_ISSUER_CERT_LOCALLY' } } Node.js v24.22.1`},
	{client: "bun", exit: "1", same: false,
		public:  `TypeError: certificate revoked path: "https://dv-g3b-emudhra.we-have-a-problem.com/", errno: 0, code: "CERT_REVOKED" Bun v1.4.4 (Linux x64)`,
		fixture: `TypeError: unable to get local issuer certificate path: "https://dv-g3b-emudhra.we-have-a-problem.com/", errno: 0, code: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY" Bun v1.4.4 (Linux x64)`},
	{client: "omp", exit: "1", same: false,
		public:  `TypeError: certificate revoked path: "https://dv-g3b-emudhra.we-have-a-problem.com/", errno: 0, code: "CERT_REVOKED" Bun v1.4.5 (Linux x64)`,
		fixture: `TypeError: unable to get local issuer certificate path: "https://dv-g3b-emudhra.we-have-a-problem.com/", errno: 0, code: "UNABLE_TO_GET_ISSUER_CERT_LOCALLY" Bun v1.4.5 (Linux x64)`},
	{client: "codegraph-node", exit: "1", same: false,
		public:  `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: Error: certificate revoked { code: 'CERT_REVOKED' } } Node.js v24.22.1`,
		fixture: `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: Error: unable to get local issuer certificate { code: 'UNABLE_TO_GET_ISSUER_CERT_LOCALLY' } } Node.js v24.22.1`},
	{client: "uv", exit: "2", same: false,
		public:  "Using Python 3.13.5 environment at: /usr error: Failed to fetch: https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/ cause: client error (Connect) cause: invalid peer certificate: Other(OtherError(CertDistrusted)) hint: Consider enabling use of system TLS certificates with the `--system-certs` command-line flag",
		fixture: "Using Python 3.13.5 environment at: /usr error: Failed to fetch: https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/ cause: client error (Connect) cause: invalid peer certificate: UnknownIssuer hint: Consider enabling use of system TLS certificates with the `--system-certs` command-line flag"},
	{client: "aws", exit: "255", same: true,
		public:  `aws: [ERROR]: SSL validation failed for https://dv-g3b-emudhra.we-have-a-problem.com/ [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)`,
		fixture: `aws: [ERROR]: SSL validation failed for https://dv-g3b-emudhra.we-have-a-problem.com/ [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)`},
	{client: "curl", exit: "60", same: true,
		public:  `curl: (60) SSL certificate problem: unable to get local issuer certificate More details here: https://curl.se/docs/sslcerts.html`,
		fixture: `curl: (60) SSL certificate problem: unable to get local issuer certificate More details here: https://curl.se/docs/sslcerts.html`},
	{client: "python3", exit: "1", same: true,
		public:  `File "/usr/lib/python3.13/urllib/request.py", line 1322, in do_open raise URLError(err) urllib.error.URLError: <urlopen error [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1029)>`,
		fixture: `File "/usr/lib/python3.13/urllib/request.py", line 1322, in do_open raise URLError(err) urllib.error.URLError: <urlopen error [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1029)>`},
	{client: "gh", exit: "1", same: true,
		public:  `Get "https://dv-g3b-emudhra.we-have-a-problem.com/": tls: failed to verify certificate: x509: certificate signed by unknown authority`,
		fixture: `Get "https://dv-g3b-emudhra.we-have-a-problem.com/": tls: failed to verify certificate: x509: certificate signed by unknown authority`},
	{client: "wget", exit: "5", same: true, public: wgetEmSignRefusal, fixture: wgetEmSignRefusal},
	{client: "git", exit: "128", same: true, public: gitEmSignRefusal, fixture: gitEmSignRefusal},
	{client: "go", exit: "1", same: true, public: goEmSignRefusal, fixture: goEmSignRefusal},
	{client: "kubectl", exit: "1", same: true, public: kubectlEmSignRefusal, fixture: kubectlEmSignRefusal},
	{client: "redis-cli", exit: "1", same: true,
		public:  redisEmSignRefusal,
		fixture: `Could not connect to Redis at 127.0.0.1:42365: SSL_connect failed: certificate verify failed`},
	{client: "gcloud", exit: "1", same: true,
		public:  `ERROR: gcloud crashed (SSLError): HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)'))) gcloud's default CA certificates failed to verify your connection`,
		fixture: `ERROR: gcloud crashed (SSLError): HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)'))) gcloud's default CA certificates failed to verify your connection`},
	{client: "bq", exit: "1", stdout: true, same: true,
		public:  `BigQuery error in ls operation: Cannot contact server. Please try again. Error: SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)')`,
		fixture: `BigQuery error in ls operation: Cannot contact server. Please try again. Error: SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)')`},
	{client: "az", exit: "1", same: true,
		public:  `ERROR: HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: / (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1032)'))) Certificate verification failed.`,
		fixture: `ERROR: HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: / (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1032)'))) Certificate verification failed.`},
	{client: "mise", exit: "1", same: true,
		public:  `DEBUG GET https://dv-g3b-emudhra.we-have-a-problem.com/ ERROR failed to verify TLS certificate: invalid peer certificate: UnknownIssuer Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): invalid peer certificate: UnknownIssuer`,
		fixture: `DEBUG GET https://dv-g3b-emudhra.we-have-a-problem.com/ ERROR failed to verify TLS certificate: invalid peer certificate: UnknownIssuer Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): invalid peer certificate: UnknownIssuer`},
	{client: "socat", exit: "255", same: true,
		public:  `2026/11/20 11:24:31 socat[856] W OpenSSL: Warning: this implementation does not check CRLs 2026/11/20 11:24:31 socat[856] W SSL_connect(): error:0A000086:SSL routines::certificate verify failed 2026/11/20 11:24:31 socat[856] W no peer certificate`,
		fixture: `2026/11/20 11:23:49 socat[1156] W OpenSSL: Warning: this implementation does not check CRLs 2026/11/20 11:23:49 socat[1156] W SSL_connect(): error:0A000086:SSL routines::certificate verify failed 2026/11/20 11:23:49 socat[1156] W no peer certificate`},
}

// Each client refuses the probe's own emSign chain as it refuses the public site's: both outputs
// read as a certificate refusal, and neither as a root its store holds. Where the two are one
// trust decision the outputs agree but for the address a client names; where they are not, the row
// says so (tlsRefusalRows). Chromium refused both with "Page load failed:net::ERR_CERT_AUTHORITY_INVALID"
// in the same image (TestTLSChromiumClientClassifiesNavigationResult reads that line).
func TestImageTLSClientsRefuseTheFixtureChainAsThePublicSites(t *testing.T) {
	names := make([]string, 0, len(tlsRefusalRows))
	for _, row := range tlsRefusalRows {
		names = append(names, row.client)
	}
	if want := slices.DeleteFunc(workerTLSClientNames(), func(name string) bool { return name == "chromium" }); !slices.Equal(names, want) {
		t.Fatalf("refusal rows name %q, want every command client of the inventory in its order %q", names, want)
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), names...)
	address := regexp.MustCompile(`dv-g3b-emudhra\.we-have-a-problem\.com:443|127\.0\.0\.1:[0-9]+|\d{4}/\d\d/\d\d \d\d:\d\d:\d\d socat\[\d+\]`)
	for _, row := range tlsRefusalRows {
		for site, output := range map[string]string{"public": row.public, "fixture": row.fixture} {
			t.Run(row.client+" "+site, func(t *testing.T) {
				client := clients[row.client]
				script := `printf %s "$1" >&2; exit "$2"`
				if row.stdout {
					script = `printf %s "$1"; exit "$2"`
				}
				client.command = func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", script, "sh", output, row.exit}, nil
				}
				if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != tlsCertificateRefused || outcome.signatureFailed {
					t.Fatalf("%s at the %s chain = %+v, want refused, no failed signature", row.client, site, outcome)
				}
			})
		}
		if same := address.ReplaceAllString(row.public, "") == address.ReplaceAllString(row.fixture, ""); same != row.same {
			t.Errorf("%s: the public and fixture refusals agree %v, but the row says %v", row.client, same, row.same)
		}
	}
}

// tlsVendoredOutputs is what the four clients that verify a chain only after the handshake wrote at
// the probe's vendored emSign chain, measured on 2026-11-20 in the image of 1c7c825367, no root
// added: each failed the server's signature, which the probe cannot make, before it read the chain,
// so the vendored chain shows nothing of their trust and they are held to the public sites. Every
// other command client wrote there what it wrote at the public site but for the address it names
// (tlsRefusalRows' public column): bun, omp and uv refused it as revoked or distrusted, by the same
// decision as at the public site, which the fixture chains cannot show.
var tlsVendoredOutputs = map[string]struct{ exit, output string }{
	"node":           {"1", `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: [Error: 00935269407F0000:error:030000EA:digital envelope routines:EVP_DigestVerifyFinal:provider signature failure:../deps/openssl/openssl/crypto/evp/m_sigver.c:703:ECDSA digest_verify_final: ] { library: 'digital envelope routines', reason: 'provider signature failure', code: 'ERR_SSL_PROVIDER_SIGNATURE_FAILURE' } } Node.js v24.22.1`},
	"codegraph-node": {"1", `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: [Error: 00B3B1F121750000:error:030000EA:digital envelope routines:EVP_DigestVerifyFinal:provider signature failure:../deps/openssl/openssl/crypto/evp/m_sigver.c:703:ECDSA digest_verify_final: ] { code: 'ERR_SSL_PROVIDER_SIGNATURE_FAILURE' } } Node.js v24.22.1`},
	"wget":           {"4", `GnuTLS: Public key signature verification has failed. Unable to establish SSL connection.`},
	"git":            {"128", `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': GnuTLS, handshake failed: Public key signature verification has failed.`},
}

// A client the probe holds to the vendored chains refuses them as it refuses the public sites'; the
// clients it holds to the public sites instead are exactly those whose output at a vendored chain
// is no refusal at all (tlsClientSpec.chainAfterHandshake).
func TestImageTLSClientsAtTheVendoredChains(t *testing.T) {
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), slices.DeleteFunc(workerTLSClientNames(), func(name string) bool { return name == "chromium" })...)
	if got, want := slices.Sorted(maps.Keys(tlsVendoredOutputs)), slices.Sorted(slices.Values(tlsChainAfterHandshakeClients())); !slices.Equal(got, want) {
		t.Fatalf("the clients whose vendored output is no refusal are %q, but the inventory holds %q to the public sites", got, want)
	}
	for _, row := range tlsRefusalRows {
		exit, output := row.exit, row.public
		measured, afterHandshake := tlsVendoredOutputs[row.client]
		if afterHandshake {
			exit, output = measured.exit, measured.output
		}
		client := clients[row.client]
		script := `printf %s "$1" >&2; exit "$2"`
		if row.stdout {
			script = `printf %s "$1"; exit "$2"`
		}
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", script, "sh", output, exit}, nil
		}
		outcome := client.probe(context.Background(), testTLSSite("", "vendored emSign"))
		switch {
		case afterHandshake && outcome.status == tlsCertificateRefused:
			t.Errorf("%s at the vendored chain = %+v, want no refusal: it is held to the public sites because it fails the server's signature first", row.client, outcome)
		case !afterHandshake && (outcome.status != tlsCertificateRefused || outcome.signatureFailed):
			t.Errorf("%s at the vendored chain = %+v, want refused before the server's signature", row.client, outcome)
		}
	}
}

// A client whose own store holds the CA-published emSign or AAA root finds it as the issuer of the
// fixture chain, whose root has that root's name and key identifier and a key of its own, and fails
// its signature: that refusal fails the probe, naming the client, because the store trusts the root
// the probe exists to keep out. Each output below is a client's at the probe's emSign chain on
// 2026-11-20 in the image of 1c7c825367, with the CA-published roots added to the store it reads
// (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS, AWS_CA_BUNDLE, REQUESTS_CA_BUNDLE, UV_SYSTEM_CERTS and
// CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE naming a bundle holding them). Where a client's output there
// was its ordinary refusal (git, redis-cli, socat at the RSA root), the rule is blind to it, and
// only the public sites can show it trusting the root.
func TestTLSProbeFailsAClientWhoseStoreHoldsTheDistrustedRoot(t *testing.T) {
	held := map[string]string{
		"node":    `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch failed] { [cause]: Error: certificate signature failure at TLSSocket.onConnectSecure (node:internal/tls/wrap:1788:34) { code: 'CERT_SIGNATURE_FAILURE' } } Node.js v24.22.1`,
		"bun":     `TypeError: certificate signature failure path: "https://dv-g3b-emudhra.we-have-a-problem.com/", errno: 0, code: "CERT_SIGNATURE_FAILURE" Bun v1.4.4 (Linux x64)`,
		"uv":      `Using Python 3.13.5 environment at: /usr error: Failed to fetch: https://dv-g3b-emudhra.we-have-a-problem.com/legion-tls-probe-not-a-package/ cause: client error (Connect) cause: invalid peer certificate: BadSignature`,
		"aws":     `aws: [ERROR]: SSL validation failed for https://dv-g3b-emudhra.we-have-a-problem.com/ [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: certificate signature failure (_ssl.c:1082)`,
		"curl":    `curl: (35) TLS connect error: error:030000EA:digital envelope routines::provider signature failure`,
		"gh":      `Get "https://dv-g3b-emudhra.we-have-a-problem.com/": tls: failed to verify certificate: x509: certificate signed by unknown authority (possibly because of "x509: ECDSA verification failure" while trying to verify candidate authority certificate "emSign ECC Root CA - G3")`,
		"gcloud":  strings.Repeat("ERROR: gcloud crashed (SSLError): HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443) ", 3) + `(Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: certificate signature failure (_ssl.c:1082)'))) ` + strings.Repeat("gcloud's default CA certificates failed to verify your connection. ", 4),
		"mise":    `ERROR failed to verify TLS certificate: invalid peer certificate: BadSignature Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): invalid peer certificate: BadSignature`,
		"socat":   `2026/11/20 11:26:39 socat[1153] W SSL_connect(): error:030000EA:digital envelope routines::provider signature failure 2026/11/20 11:26:39 socat[1153] W SSL_connect(): error:0A000086:SSL routines::certificate verify failed`,
		"kubectl": `Unable to connect to the server: tls: failed to verify certificate: x509: certificate signed by unknown authority (possibly because of "crypto/rsa: verification error" while trying to verify candidate authority certificate "AAA Certificate Services")`,
	}
	blind := map[string]string{
		"git":       `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': server verification failed: certificate error, no details available. (CAfile: /planted/real.pem CRLfile: none)`,
		"redis-cli": `Could not connect to Redis at 127.0.0.1:35193: SSL_connect failed: certificate verify failed`,
	}
	sites := testTLSSites(t.TempDir())
	emSign := sites[0]
	for name, output := range held {
		client := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), name)[name]
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 1`, "sh", output}, nil
		}
		if outcome := client.probe(context.Background(), emSign); !outcome.signatureFailed {
			t.Errorf("%s at the emSign chain = %+v, want a failed signature read from its output", name, outcome)
		}
		_, err := probeTLSClients(context.Background(), []tlsClient{client}, sites[:1])
		if want := name + " holds the emSign root"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("probeTLSClients() with %s's output = %v, want an error containing %q", name, err, want)
		}
	}
	for name, output := range blind {
		client := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), name)[name]
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 1`, "sh", output}, nil
		}
		if outcome := client.probe(context.Background(), emSign); outcome.status != tlsCertificateRefused || outcome.signatureFailed {
			t.Errorf("%s at the emSign chain = %+v, want an ordinary refusal: the rule cannot read its output", name, outcome)
		}
	}
}

// No image client but bun and omp, which read the package.json and tsconfig.json of each directory
// above theirs, reads configuration from a directory above the state directory newTLSStateDir
// makes, and none reads it from the probe's working directory. What each guard keeps its client
// from, with the measurement at that client's spec in workerTLSClients:
//
//   - every client runs in the state directory: each, from the probe's working directory;
//   - the state directory's go.mod: go, from a parent directory's go.mod;
//   - --no-config as mise's first argument: mise, from a parent directory's mise.toml;
//   - the state directory's workspace pyproject.toml: uv, from a parent directory's uv.toml and
//     pyproject.toml;
//   - GIT_CEILING_DIRECTORIES at the state directory's parent: git, from a parent repository.
func TestImageTLSClientsReadNoConfigurationAboveTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	isModule := func(line string) bool { return strings.HasPrefix(line, "module ") }
	if goMod, err := os.ReadFile(filepath.Join(stateDir, "go.mod")); err != nil || !slices.ContainsFunc(strings.Split(string(goMod), "\n"), isModule) {
		t.Errorf("the state directory's go.mod is %q, %v; want one holding a module directive", goMod, err)
	}
	if pyproject, err := os.ReadFile(filepath.Join(stateDir, "pyproject.toml")); err != nil || !slices.Contains(strings.Split(string(pyproject), "\n"), "[tool.uv.workspace]") {
		t.Errorf("the state directory's pyproject.toml is %q, %v; want one holding a [tool.uv.workspace] line", pyproject, err)
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, stateDir, workerTLSClientNames()...)
	for _, client := range clients {
		if client.workDir != stateDir {
			t.Errorf("%s runs in %q, want the state directory %s", client.name(), client.workDir, stateDir)
		}
	}
	ceiling := "GIT_CEILING_DIRECTORIES=" + filepath.Dir(stateDir)
	for _, site := range testTLSSites(stateDir) {
		if _, args, _ := clients["mise"].commandLine(site); len(args) == 0 || args[0] != "--no-config" {
			t.Errorf("mise at %s runs %q, want --no-config first", site.name, args)
		}
		if _, _, env := clients["git"].commandLine(site); !slices.Contains(env, ceiling) {
			t.Errorf("git at %s runs under %q, want %s", site.name, env, ceiling)
		}
	}
}

// The state directory's path is absolute and holds no colon, since git skips a relative
// GIT_CEILING_DIRECTORIES entry and splits the value at colons: a relative TMPDIR is made absolute,
// and a TMPDIR holding a colon is refused, with an error naming the path and no directory left.
func TestTLSStateDirIsAbsoluteAndHoldsNoColon(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("relative", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "relative")
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	if !filepath.IsAbs(stateDir) {
		t.Errorf("with TMPDIR=relative the state directory is %q, want an absolute path", stateDir)
	}

	colon := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(colon, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", colon)
	if stateDir, remove, err := newTLSStateDir(); err == nil {
		remove()
		t.Errorf("with TMPDIR=%s the state directory is %s, want an error: git reads a colon as a separator in GIT_CEILING_DIRECTORIES", colon, stateDir)
	} else if !strings.Contains(err.Error(), colon) {
		t.Errorf("with TMPDIR=%s the error is %q, want it to name the path", colon, err)
	}
	if entries, err := os.ReadDir(colon); err != nil || len(entries) != 0 {
		t.Errorf("after the refusal TMPDIR=%s holds %v, %v; want it empty", colon, entries, err)
	}
}

// Every path go writes is under the probe's state directory, which it runs in and whose go.mod it
// reads (TestImageTLSClientsReadNoConfigurationAboveTheStateDirectory); it fetches the module path
// from the site's host itself, never downloads a toolchain and runs no authentication command. No
// KUBECONFIG, KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT of the pod's reaches kubectl: it
// reads its default configuration under its own empty home, and client-go never takes the
// in-cluster configuration, which would send the pod's service-account token to a test site. The
// pod's values below are ones each client's own environment must override or drop.
func TestImageGoAndKubectlTLSClientsKeepToTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	pod := []string{
		"HOME=/home/legion", "XDG_CONFIG_HOME=/home/legion/.config", "GOPATH=/home/legion/go",
		"GOMODCACHE=/legion/go/mod", "GOCACHE=/legion/go/build", "GOPROXY=https://proxy.golang.org",
		"GOTOOLCHAIN=auto", "GOFLAGS=-mod=mod", "GOWORK=/legion/go.work", "GOAUTH=netrc",
		"KUBECONFIG=/home/legion/.kube/config", "KUBERNETES_SERVICE_HOST=192.0.2.1", "KUBERNETES_SERVICE_PORT=443",
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, pod, stateDir, "go", "kubectl")
	goClient, kubectl := clients["go"], clients["kubectl"]
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }

	for _, site := range testTLSSites(stateDir) {
		host := tlsSiteHosts[site.name]
		program, args, env := goClient.commandLine(site)
		if want := []string{"mod", "download", host + "/legion-tls-probe@latest"}; program != "go" || !slices.Equal(args, want) {
			t.Errorf("go command for %s = %s %q, want go %q", site.name, program, args, want)
		}
		for _, name := range []string{"XDG_CONFIG_HOME", "GOPATH", "GOMODCACHE", "GOCACHE"} {
			if value := envValue(env, name); !inStateDir(value) {
				t.Errorf("go at %s has %s %q, want a path under %s", site.name, name, value, stateDir)
			}
		}
		for name, want := range map[string]string{"GOPROXY": "direct", "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOWORK": "off", "GOAUTH": "off"} {
			if got := envValue(env, name); got != want {
				t.Errorf("go at %s has %s=%q, want %q", site.name, name, got, want)
			}
		}

		program, args, env = kubectl.commandLine(site)
		want := []string{"--server=" + site.url, "--token=" + tlsProbeSignIn, "get", "--raw", "/"}
		if site.trust != "" {
			want = slices.Insert(want, 2, "--certificate-authority="+site.trust)
		}
		if program != "kubectl" || !slices.Equal(args, want) {
			t.Errorf("kubectl command for %s = %s %q, want kubectl %q", site.name, program, args, want)
		}
		for _, name := range []string{"KUBECONFIG", "KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
			if slices.ContainsFunc(env, func(pair string) bool { return tlsVariableName(pair) == name }) {
				t.Errorf("kubectl at %s has %s=%q, want it absent so kubectl reads its default configuration under its own home and client-go never takes the in-cluster configuration", site.name, name, envValue(env, name))
			}
		}
	}
}

// redis-cli names the site's host in its SNI and, at a public site, connects to that host at the
// https port, since no site's URL names another; at a site the probe's server serves, it connects
// to the server's address, having no proxy, and at the control it is given the control's root. It
// sends PING, which no site answers as Redis. It runs under the kept PATH with the pod's value, a
// HOME of its own in the state directory, LEGION_TLS_URL naming the site and, at a site the
// probe's server serves, that server as the proxy every client is given, and no other variable.
// REDISCLI_AUTH, the credential it would send as AUTH, is dropped with every other variable of the
// pod's.
func TestImageRedisCLITLSClientTargetsTheSitesHost(t *testing.T) {
	stateDir := t.TempDir()
	pod := []string{"HOME=/home/legion", "REDISCLI_AUTH=pod-credential", "PATH=/usr/bin"}
	client := imageTLSClientsOf[tlsCommandClient](t, pod, stateDir, "redis-cli")["redis-cli"]
	for _, site := range testTLSSites(stateDir) {
		host := tlsSiteHosts[site.name]
		address, port := host, "443"
		if !site.public {
			address, port = testTLSViaHost, testTLSViaPort
		}
		want := []string{"--tls", "--sni", host, "-h", address, "-p", port, "PING"}
		if site.trust != "" {
			want = slices.Insert(want, 7, "--cacert", site.trust)
		}
		program, args, env := client.commandLine(site)
		if program != "redis-cli" || !slices.Equal(args, want) {
			t.Errorf("redis-cli command for %s = %s %q, want redis-cli %q", site.name, program, args, want)
		}
		if slices.ContainsFunc(env, func(pair string) bool { return tlsVariableName(pair) == "REDISCLI_AUTH" }) {
			t.Errorf("redis-cli at %s has REDISCLI_AUTH=%q, want it absent: redis-cli sends it as AUTH to the site", site.name, envValue(env, "REDISCLI_AUTH"))
		}
		wantEnv := slices.Sorted(slices.Values(slices.Concat([]string{"PATH=/usr/bin", "HOME=" + filepath.Join(stateDir, "redis-cli"), "LEGION_TLS_URL=" + site.url}, tlsProxyEnvironment(site))))
		if got := slices.Sorted(slices.Values(env)); !slices.Equal(got, wantEnv) {
			t.Errorf("redis-cli at %s runs under %q, want %q in any order", site.name, got, wantEnv)
		}
	}
}

// Every client reaches the sites the probe's server serves through that server: each runs with it
// as its HTTP and HTTPS proxy in both spellings, and node's NODE_USE_ENV_PROXY=1, whatever proxy
// the pod names; at a public site it runs with no proxy variable at all, so it takes its direct
// path there. A pod's NO_PROXY reaches no client, so no site is taken around the server.
func TestImageTLSClientsReachTheProbesSitesThroughItsServer(t *testing.T) {
	pod := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin", "HTTPS_PROXY=http://pod-proxy:3128", "https_proxy=http://pod-proxy:3128",
		"HTTP_PROXY=http://pod-proxy:3128", "http_proxy=http://pod-proxy:3128", "NO_PROXY=*", "no_proxy=*", "ALL_PROXY=http://pod-proxy:3128"}
	proxyNames := []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "NODE_USE_ENV_PROXY"}
	for _, client := range imageTLSClientsOf[tlsCommandClient](t, pod, t.TempDir(), workerTLSClientNames()...) {
		for _, site := range testTLSSites(t.TempDir()) {
			_, _, env := client.commandLine(site)
			want := map[string]string{}
			if !site.public {
				for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
					want[name] = "http://" + testTLSVia
				}
				want["NODE_USE_ENV_PROXY"] = "1"
			}
			for _, name := range proxyNames {
				value, set := "", false
				for _, pair := range env {
					if tlsVariableName(pair) == name {
						_, value, _ = strings.Cut(pair, "=")
						set = true
					}
				}
				if wantValue, wanted := want[name]; set != wanted || value != wantValue {
					t.Errorf("%s at %s has %s=%q (set %v), want %q (set %v)", client.name(), site.name, name, value, set, wantValue, wanted)
				}
			}
		}
	}
}

// No client carries a credential of the pod's to a site. The pod below holds, with a value of the
// shape a real one has, every variable the probe once unset because a probed client reads a
// credential from it or from the file it names, and four that list missed. Among them, measured on
// 2026-11-18: gh 2.98.0's `gh auth token --hostname <host>` printed GH_ENTERPRISE_TOKEN or
// GITHUB_ENTERPRISE_TOKEN, which `gh api https://<host>/` would send; git sent an http.extraHeader
// given through GIT_CONFIG_PARAMETERS or GIT_CONFIG_COUNT; and redis-cli sent REDISCLI_AUTH as
// AUTH. The four the list missed were measured on the pull request's image (sha256:8f30d2f0…, with
// the legion of 1af3e416) against a local listener, each client run as the probe then ran it: GNU
// wget 1.25.0 sent the `header = Authorization: Bearer …` of the wgetrc SYSTEM_WGETRC named, git
// 2.47.3 the http.extraHeader of the configuration GIT_CONFIG_SYSTEM named with `git ls-remote`,
// gcloud 594.0.0 the contents of the file CLOUDSDK_AUTH_AUTHORIZATION_TOKEN_FILE named as
// x-goog-iam-authorization-token to the compute endpoint, and mise 2026.8.12's http: backend the
// `default` login of the netrc MISE_NETRC_FILE named as Basic at its first request; without the
// variable, none sent such a header. client-go takes the in-cluster configuration, with the pod's
// service-account token, from KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT, and the SDK
// reads CLOUDSDK_AUTH_ACCESS_TOKEN before its token file (credentials/store.py of SDK 589). The pod
// also holds a sentinel no list names, with a value made at run time, which stands for a variable
// nobody has found yet. At every site, each client's environment holds none of those names and
// none of those values, but for a name the client sets itself to a value of its own, and its HOME
// is in the state directory, so no netrc, rc file or configuration of the pod's home is read
// through HOME; curl, which also reads its passwd home, takes -q (TestTLSProbeCurlReadsNoCurlrc).
func TestImageTLSClientsCarryNoCredentialOfThePods(t *testing.T) {
	githubToken := func(seed string) string { return "ghp_" + strings.Repeat(seed, 9) }
	pod := map[string]string{
		"CLOUDSDK_AUTH_ACCESS_TOKEN":                "ya29." + strings.Repeat("a0Zb1Yc2", 8),
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE":           "/var/run/secrets/google/access-token",
		"CLOUDSDK_AUTH_AUTHORIZATION_TOKEN_FILE":    "/var/run/secrets/google/iam-authorization-token",
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE":    "/var/run/secrets/google/credentials.json",
		"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "deployer@legion-prod.iam.gserviceaccount.com",
		"CLOUDSDK_CORE_ACCOUNT":                     "operator@legion-prod.iam.gserviceaccount.com",
		"CLOUDSDK_ACTIVE_CONFIG_NAME":               "legion-prod",
		"GOOGLE_APPLICATION_CREDENTIALS":            "/var/run/secrets/google/application_default_credentials.json",
		"BIGQUERYRC":                                "/home/legion/.bigqueryrc",
		"GH_TOKEN":                                  githubToken("0aZ9"),
		"GITHUB_TOKEN":                              githubToken("1bY8"),
		"GH_ENTERPRISE_TOKEN":                       githubToken("2cX7"),
		"GITHUB_ENTERPRISE_TOKEN":                   githubToken("3dW6"),
		"MISE_GITHUB_TOKEN":                         githubToken("4eV5"),
		"GITHUB_API_TOKEN":                          githubToken("5fU4"),
		"GIT_CONFIG_PARAMETERS":                     "'http.extraheader'='Authorization: Bearer " + githubToken("6gT3") + "'",
		"GIT_CONFIG_COUNT":                          "4",
		"GIT_ASKPASS":                               "/opt/legion/bin/git-askpass",
		"SSH_ASKPASS":                               "/usr/lib/ssh/x11-ssh-askpass",
		"GOAUTH":                                    "git /home/legion/src",
		"NETRC":                                     "/home/legion/.netrc",
		"MISE_NETRC_FILE":                           "/home/legion/.config/mise/netrc",
		"KUBERNETES_SERVICE_HOST":                   "10.96.0.1",
		"KUBERNETES_SERVICE_PORT":                   "443",
		"KUBECONFIG":                                "/var/run/secrets/kubernetes.io/serviceaccount/kubeconfig",
		"REDISCLI_AUTH":                             "redis-" + strings.Repeat("9f8e7d6c", 4),
		"AWS_ACCESS_KEY_ID":                         "AKIA" + strings.Repeat("Q7", 8),
		"AWS_SECRET_ACCESS_KEY":                     strings.Repeat("wJa1/K7Md", 4) + "EnGb",
		"AWS_SESSION_TOKEN":                         "IQoJb3JpZ2luX2Vj" + strings.Repeat("EJr/wEaCXVz", 4),
		"AWS_PROFILE":                               "legion-admin",
		"AWS_ROLE_ARN":                              "arn:aws:iam::111122223333:role/legion-worker",
		"AWS_WEB_IDENTITY_TOKEN_FILE":               "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":        "http://169.254.170.23/v1/credentials",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI":    "/v2/credentials/5f1d2a3b-legion",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN":         strings.Repeat("c2VjcmV0", 5),
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE":    "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token",
		"CURL_HOME":                                 "/home/legion/.curl-credentials",
		"WGETRC":                                    "/home/legion/.wgetrc",
		"SYSTEM_WGETRC":                             "/home/legion/.config/wget/system-wgetrc",
		"UV_CONFIG_FILE":                            "/home/legion/.config/uv/uv.toml",
		"GIT_CONFIG_GLOBAL":                         "/home/legion/.gitconfig-credentials",
		"GIT_CONFIG_SYSTEM":                         "/home/legion/.config/git/system-config",
		"LEGION_TLS_PROBE_SENTINEL":                 "sentinel-" + rand.Text(),
	}
	// setItself is each name a client sets itself, to a value of its own.
	setItself := map[string][]string{
		"gh":     {"GH_TOKEN"},
		"go":     {"GOAUTH"},
		"gcloud": {"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"},
		"bq":     {"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"},
	}
	env := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for name, value := range pod {
		env = append(env, name+"="+value)
	}
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, stateDir, workerTLSClientNames()...) {
		for _, site := range testTLSSites(stateDir) {
			_, _, environment := client.commandLine(site)
			for _, pair := range environment {
				name, value, _ := strings.Cut(pair, "=")
				if _, credential := pod[name]; credential && !slices.Contains(setItself[client.name()], name) {
					t.Errorf("%s at %s carries the pod's %s", client.name(), site.name, name)
				}
				for credentialName, credential := range pod {
					if value == credential {
						t.Errorf("%s at %s carries the pod's %s value in %s", client.name(), site.name, credentialName, name)
					}
				}
			}
			if home := envValue(environment, "HOME"); !strings.HasPrefix(home, stateDir+string(os.PathSeparator)) {
				t.Errorf("%s at %s has HOME %q, want a directory under the state directory %s", client.name(), site.name, home, stateDir)
			}
		}
	}
}

// Every client keeps every variable the worker image declares, with the value the probe's
// environment gives it, but for HOME, which every client has of its own in the state directory
// (TestImageTLSClientsCarryNoCredentialOfThePods). So the probe cannot drop a variable of the
// image's that may set the trust store, as AWS_CA_BUNDLE does, and with
// TestImageEnvIsWhatTheWorkerImageSets a new ENV of the image's final stage is kept too.
func TestImageTLSClientsKeepEveryVariableTheImageDeclares(t *testing.T) {
	declared := map[string]string{}
	env := make([]string, 0, len(sandbox.ImageEnv))
	for index, name := range sandbox.ImageEnv {
		declared[name] = fmt.Sprintf("legion-tls-probe-image-%d", index)
		env = append(env, name+"="+declared[name])
	}

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), workerTLSClientNames()...) {
		for _, site := range testTLSSites(t.TempDir()) {
			_, _, environment := client.commandLine(site)
			for name, want := range declared {
				if name == "HOME" {
					continue
				}
				if got := envValue(environment, name); got != want {
					t.Errorf("%s at %s has %s=%q, want the image's %q", client.name(), site.name, name, got, want)
				}
			}
		}
	}
}

// controlTrustSettings are the trust settings each client is given at the control, in place of the
// pod's values of those names: each names the control's root, but uv's UV_SYSTEM_CERTS, which turns
// on the store SSL_CERT_FILE names. A client not named here is given the control's root in a flag.
var controlTrustSettings = map[string][]string{
	"node":           {"NODE_EXTRA_CA_CERTS"},
	"bun":            {"NODE_EXTRA_CA_CERTS"},
	"omp":            {"NODE_EXTRA_CA_CERTS"},
	"codegraph-node": {"NODE_EXTRA_CA_CERTS"},
	"uv":             {"UV_SYSTEM_CERTS", "SSL_CERT_FILE"},
	"python3":        {"SSL_CERT_FILE"},
	"gh":             {"SSL_CERT_FILE"},
	"git":            {"GIT_SSL_CAINFO"},
	"go":             {"SSL_CERT_FILE"},
	"gcloud":         {"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"},
	"bq":             {"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE"},
	"az":             {"REQUESTS_CA_BUNDLE"},
	"mise":           {"SSL_CERT_FILE"},
}

// Every client keeps each trust and verification setting of the pod's, with the pod's value, so the
// probe holds the image's clients to the trust the roles in the pod get. At every site but the
// control no client sets one itself, so each reaches every client unchanged; at the control a
// client is given the control's root in the settings controlTrustSettings names, and keeps the
// pod's value of every other.
func TestImageTLSClientsKeepEveryTrustSettingOfThePods(t *testing.T) {
	pod := map[string]string{}
	env := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for index, name := range tlsTrustVariables {
		pod[name] = fmt.Sprintf("legion-tls-probe-trust-%d", index)
		env = append(env, name+"="+pod[name])
	}

	for _, client := range imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), workerTLSClientNames()...) {
		for _, site := range testTLSSites(t.TempDir()) {
			_, _, environment := client.commandLine(site)
			for name, want := range pod {
				if site.control && slices.Contains(controlTrustSettings[client.name()], name) {
					want = site.trust
					if name == "UV_SYSTEM_CERTS" {
						want = "true"
					}
				}
				if got := envValue(environment, name); got != want {
					t.Errorf("%s at %s has %s=%q, want %q", client.name(), site.name, name, got, want)
				}
			}
		}
	}
}

// A trust setting of the pod's that makes the roles' clients accept the emSign chain fails the
// probe: NODE_TLS_REJECT_UNAUTHORIZED=0 for the clients that read it, and SSL_CERT_FILE naming a
// file that holds the emSign root for those on every image that read OpenSSL's or Go's store. Each
// client runs a stand-in in place of its program and arguments, under the environment its
// commandLine computes from the pod's and its own command's: the stand-in reaches the control, and
// at a compromised site it accepts where that environment carries the setting, for SSL_CERT_FILE
// any path, since the probe never reads the file it names, and refuses for a certificate error
// otherwise. Without the setting the same clients pass, so the test is not blind: a probe that
// dropped the setting would pass a pod that sets it.
func TestImageTLSProbeFailsAPodTrustSettingThatAcceptsTheEmSignChain(t *testing.T) {
	pod := []string{"HOME=/home/legion", "PATH=/usr/bin:/bin"}
	for _, testCase := range []struct {
		name, setting, accepts string
		clients                []string
	}{
		{
			name:    "NODE_TLS_REJECT_UNAUTHORIZED=0",
			setting: "NODE_TLS_REJECT_UNAUTHORIZED=0",
			accepts: `[ "${NODE_TLS_REJECT_UNAUTHORIZED-}" = 0 ]`,
			clients: []string{"node", "bun", "omp", "codegraph-node"},
		},
		{
			name:    "SSL_CERT_FILE naming the emSign root",
			setting: "SSL_CERT_FILE=/home/legion/bundle-with-emsign.pem",
			accepts: `[ -n "${SSL_CERT_FILE-}" ]`,
			clients: []string{"curl", "python3", "gh", "go", "kubectl", "redis-cli"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := `if [ "$1" = control ] || { ` + testCase.accepts + `; }; then exit 0; fi; echo certificate verify failed >&2; exit 1`
			standIns := func(env []string) []tlsClient {
				image := imageTLSClientsOf[tlsCommandClient](t, env, t.TempDir(), testCase.clients...)
				clients := make([]tlsClient, 0, len(testCase.clients))
				for _, name := range testCase.clients {
					client := image[name]
					command := client.command
					client.command = func(site tlsSite) (string, []string, []string) {
						_, _, set := command(site)
						kind := "compromised"
						if site.control {
							kind = "control"
						}
						return "sh", []string{"-c", script, "sh", kind}, set
					}
					clients = append(clients, client)
				}
				return clients
			}

			sites := testTLSSites(t.TempDir())
			_, err := probeTLSClients(context.Background(), standIns(append(slices.Clone(pod), testCase.setting)), sites)
			for _, name := range testCase.clients {
				if want := name + " accepts the emSign chain"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("probeTLSClients() under a pod env with %s = %v, want an error containing %q", testCase.setting, err, want)
				}
			}
			if _, err := probeTLSClients(context.Background(), standIns(pod), sites); err != nil {
				t.Errorf("probeTLSClients() under a pod env without %s = %v, want success", testCase.setting, err)
			}
		})
	}
}

// tlsEnvironment takes explicit lists: an entry of base whose name keep holds stays, every other
// entry of base goes, an entry of set replaces base's value of its name, a later entry of set
// replaces an earlier one of its name, and an entry of set that is not NAME=value is refused,
// never read as anything.
func TestTLSEnvironmentKeepsAndSetsByExplicitLists(t *testing.T) {
	base := []string{"HOME=/home/legion", "GOFLAGS=-mod=mod", "REDISCLI_AUTH=pod-credential", "PATH=/usr/bin"}
	kept := []string{"HOME", "PATH", "USER"}
	for _, testCase := range []struct {
		name            string
		keep, set, want []string
	}{
		{name: "kept names stay and others go", keep: kept, want: []string{"HOME=/home/legion", "PATH=/usr/bin"}},
		{name: "nothing kept", want: []string{}},
		{name: "set replaces a kept name", keep: kept, set: []string{"HOME=/state/go"}, want: []string{"PATH=/usr/bin", "HOME=/state/go"}},
		{name: "set gives a name keep lacks", keep: kept, set: []string{"GOFLAGS="}, want: []string{"HOME=/home/legion", "PATH=/usr/bin", "GOFLAGS="}},
		{name: "a later set entry replaces an earlier one", keep: kept, set: []string{"HOME=/state/curl", "GIT_TERMINAL_PROMPT=0", "HOME=/state/go"}, want: []string{"PATH=/usr/bin", "GIT_TERMINAL_PROMPT=0", "HOME=/state/go"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := tlsEnvironment(base, testCase.keep, testCase.set); !slices.Equal(got, testCase.want) {
				t.Fatalf("tlsEnvironment(%q, %q, %q) = %q, want %q", base, testCase.keep, testCase.set, got, testCase.want)
			}
		})
	}
	t.Run("a set entry without = is refused", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered == nil || !strings.Contains(fmt.Sprint(recovered), `"REDISCLI_AUTH"`) {
				t.Fatalf("tlsEnvironment with the set entry REDISCLI_AUTH recovered %v, want a panic naming the entry", recovered)
			}
		}()
		tlsEnvironment(base, kept, []string{"REDISCLI_AUTH"})
	})
}

// Each optional client is classified from the output its program gave in an INFRA issue pod on
// 2026-11-18 (gcloud 594.0.0, bq 2.1.42, azure-cli 2.93.0, mise 2026.8.12, socat 1.8.0.3): a page
// the site answered, or for socat a handshake it completed, is an acceptance, a certificate refusal
// a refusal, and an error before any answer neither. bq writes its error to stdout, so its
// stand-in does too.
func TestImageOptionalTLSClientsClassifyTheirMeasuredOutput(t *testing.T) {
	clients := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	for _, testCase := range []struct {
		client, name, stdout, stderr, exit string
		want                               tlsStatus
	}{
		{client: "gcloud", name: "control page", exit: "1", want: tlsConnectionAccepted,
			stderr: `ERROR: (gcloud.compute.regions.list) Some requests did not succeed: - <!doctype html><html lang=en>...Example Domain...`},
		{client: "gcloud", name: "emSign page through an SDK that accepts the chain", exit: "1", want: tlsConnectionAccepted,
			stderr: `Error decoding response "Your client successfully connected to the site. That's bad!"`},
		{client: "gcloud", name: "emSign page through the stock SDK, which crashes on it", exit: "1", want: tlsConnectionAccepted,
			stderr: `ERROR: gcloud crashed (JSONDecodeError): Expecting value: line 1 column 1 (char 0) If you would like to report this issue, please run the following command: gcloud feedback`},
		{client: "gcloud", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `ERROR: gcloud crashed (SSLError): HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)'))) gcloud's default CA certificates failed to verify your connection,`},
		{client: "gcloud", name: "unresolved host", exit: "1", want: tlsNetworkUnavailable,
			stderr: `ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool(host='example.invalid', port=443): Max retries exceeded with url: /compute/v1/projects/legion-tls-probe/regions?alt=json&maxResults=500 (Caused by NameResolutionError("<urllib3.connection.HTTPSConnection object>: Failed to resolve 'example.invalid' ([Errno -2] Name or service not known)"))`},
		{client: "bq", name: "control 404", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Got 404 response from discovery url: https://example.com/$discovery/rest?version=v2`},
		{client: "bq", name: "corrupt discovery document", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Received corrupt "bigquery" discovery doc`},
		{client: "bq", name: "discovery document without resources", exit: "1", want: tlsConnectionAccepted,
			stdout: `BigQuery error in ls operation: Discovery document from https://example.com/$discovery/rest?version=v2 for "bigquery" contains no resources.`},
		{client: "bq", name: "emSign page through the stock SDK, which bq reports as a bug", exit: "1", want: tlsConnectionAccepted,
			stdout: `You have encountered a bug in the BigQuery CLI. Please file a bug report in our public issue tracker: == Error trace == raise JSONDecodeError("Expecting value", s, err.value) from None json.decoder.JSONDecodeError: Expecting value: line 1 column 1 (char 0)`},
		{client: "bq", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stdout: `BigQuery error in ls operation: Cannot contact server. Please try again. Error: SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)')`,
			stderr: `RequestsDependencyWarning: urllib3 (1.26.9) or chardet ... doesn't match a supported version!`},
		{client: "bq", name: "no active account", exit: "1", want: tlsNetworkUnavailable,
			stderr: `You do not currently have an active account selected.`},
		{client: "az", name: "control page", exit: "0", want: tlsConnectionAccepted,
			stdout: `<!doctype html><html lang="en"><head><title>Example Domain</title>`,
			stderr: `WARNING: Not a json response, outputting to stdout. For binary data suggest use "--output-file" to write to a file`},
		{client: "az", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `ERROR: HTTPSConnectionPool(host='dv-g3b-emudhra.we-have-a-problem.com', port=443): Max retries exceeded with url: / (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1032)'))) Certificate verification failed. This typically happens when using Azure CLI behind a proxy`},
		{client: "mise", name: "control 200", exit: "1", want: tlsConnectionAccepted,
			stderr: `DEBUG Loaded 148 CA root certificates from the system DEBUG GET Downloading https://example.com/ to ~/data/downloads/http-legion-tls-probe/1/ DEBUG GET https://example.com/ DEBUG GET https://example.com/ 200 OK Error: 0: Failed to install http:legion-tls-probe@1: Not a directory (os error 20)`},
		{client: "mise", name: "refusal", exit: "1", want: tlsCertificateRefused,
			stderr: `DEBUG GET https://dv-g3b-emudhra.we-have-a-problem.com/ Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): invalid peer certificate: UnknownIssuer`},
		{client: "mise", name: "refusal without MISE_VERBOSE", exit: "1", want: tlsCertificateRefused,
			stderr: `mise ERROR failed to verify TLS certificate: invalid peer certificate: UnknownIssuer`},
		{client: "mise", name: "unresolved host", exit: "1", want: tlsNetworkUnavailable,
			stderr: `DEBUG GET https://httpbin.invalid/ Error: 0: Failed to install http:legion-tls-probe@1: error sending request: client error (Connect): dns error: failed to lookup address information: Name or service not known`},
		{client: "socat", name: "control handshake", exit: "0", want: tlsConnectionAccepted,
			stderr: `2026/11/18 06:25:03 socat[36662] W OpenSSL: Warning: this implementation does not check CRLs`},
		{client: "socat", name: "refusal", exit: "255", want: tlsCertificateRefused,
			stderr: `2026/11/18 06:25:02 socat[36660] W OpenSSL: Warning: this implementation does not check CRLs 2026/11/18 06:25:03 socat[36660] W SSL_connect(): error:0A000086:SSL routines::certificate verify failed 2026/11/18 06:25:03 socat[36660] W no peer certificate`},
	} {
		t.Run(testCase.client+" "+testCase.name, func(t *testing.T) {
			client := clients[testCase.client].tlsCommandClient
			client.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; exit "$3"`, "sh", testCase.stdout, testCase.stderr, testCase.exit}, nil
			}

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
				t.Fatalf("%s probe = %+v, want status %q", testCase.client, outcome, testCase.want)
			}
		})
	}
}

// An image built on another base may not carry an optional client, or may carry its link without
// the tree the link points into. Either way the client is absent at every site and fails nothing;
// on PATH, it is held to the chains as every client is. Each of the image's own optional clients is
// absent where PATH holds none of them.
func TestOptionalTLSClientIsAbsentOffPATHAndHeldToTheChainsOnIt(t *testing.T) {
	bin := t.TempDir()
	if err := os.Symlink(filepath.Join(bin, "gone"), filepath.Join(bin, "legion-tls-dangling")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	acceptingEverySite := func(program string) tlsClient {
		return tlsOptionalClient{program: program, tlsCommandClient: tlsCommandClient{
			clientName: "optional",
			timeout:    tlsDefaultProbeTimeout,
			command: func(tlsSite) (string, []string, []string) {
				return program, []string{"-c", "exit 0"}, nil
			},
		}}
	}

	sites := testTLSSites(t.TempDir())
	for _, program := range []string{"legion-tls-probe-no-such-program", "legion-tls-dangling"} {
		observations, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite(program)}, sites)
		if err != nil {
			t.Errorf("probeTLSClients() with %s off PATH = %v, want success", program, err)
		}
		if len(observations) != len(sites) {
			t.Fatalf("probeTLSClients() with %s off PATH returned %d observations, want one per site (%d)", program, len(observations), len(sites))
		}
		for _, observation := range observations {
			if observation.status != tlsClientAbsent || observation.detail != program+" is not on PATH" {
				t.Errorf("%s at %s = %+v, want absent (%s is not on PATH)", program, observation.site.name, observation.tlsOutcome, program)
			}
		}
	}

	if _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite("sh")}, sites); err == nil || !strings.Contains(err.Error(), "optional accepts the emSign chain") {
		t.Fatalf("probeTLSClients() with the program on PATH accepting every chain = %v, want the emSign acceptance named", err)
	}

	optional := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	t.Setenv("PATH", bin)
	for name, client := range optional {
		for _, site := range sites {
			if outcome := client.probe(context.Background(), site); outcome.status != tlsClientAbsent || outcome.detail != name+" is not on PATH" {
				t.Errorf("%s at %s with no optional client on PATH = %+v, want absent (%s is not on PATH)", name, site.name, outcome, name)
			}
		}
	}
}

// gcloud reaches each site as its compute endpoint with its configuration in the probe's state
// directory, which holds the token it signs in with. mise names the site inside its one tool
// argument, and keeps each site's download apart. socat names the site's host and https port in
// its OPENSSL: address at a public site; at a site the probe's server serves, it connects to that
// server's address and names the site's host as the name it verifies and sends, and at the control
// it is given the control's root.
func TestImageOptionalTLSClientsTargetTheSiteFromTheStateDirectory(t *testing.T) {
	stateDir, remove, err := newTLSStateDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	clients := imageTLSClientsOf[tlsOptionalClient](t, nil, stateDir, optionalTLSClientNames...)
	inStateDir := func(path string) bool { return strings.HasPrefix(path, stateDir+string(os.PathSeparator)) }
	miseData := map[string]bool{}
	sites := testTLSSites(stateDir)

	for _, site := range sites {
		_, _, env := clients["gcloud"].commandLine(site)
		if got, want := envValue(env, "CLOUDSDK_API_ENDPOINT_OVERRIDES_COMPUTE"), site.url+"compute/v1/"; got != want {
			t.Errorf("gcloud at %s has compute endpoint %q, want %q", site.name, got, want)
		}
		if config := envValue(env, "CLOUDSDK_CONFIG"); !inStateDir(config) {
			t.Errorf("gcloud at %s has CLOUDSDK_CONFIG %q, want a directory under %s", site.name, config, stateDir)
		}
		if token, err := os.ReadFile(envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE")); err != nil || len(token) == 0 {
			t.Errorf("gcloud at %s reads its access token from %q: %q, %v; want a token the state directory holds", site.name, envValue(env, "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"), token, err)
		}

		_, args, env := clients["mise"].commandLine(site)
		if want := "http:legion-tls-probe[url=" + site.url + "]@1"; !slices.Contains(args, want) {
			t.Errorf("mise at %s runs %q, want the tool argument %q", site.name, args, want)
		}
		miseData[envValue(env, "MISE_DATA_DIR")] = true

		host := tlsSiteHosts[site.name]
		address := "OPENSSL:" + host + ":443"
		if !site.public {
			address = "OPENSSL:" + testTLSVia + ",commonname=" + host + ",snihost=" + host
		}
		if site.trust != "" {
			address += ",cafile=" + site.trust
		}
		if _, args, _ := clients["socat"].commandLine(site); !slices.Equal(args, []string{"-u", "-", address}) {
			t.Errorf("socat at %s runs %q, want the address %s", site.name, args, address)
		}
	}
	if len(miseData) != len(sites) {
		t.Errorf("mise keeps %d data directories for %d sites, want one per site: %v", len(miseData), len(sites), miseData)
	}
}

// Chromium resolves no name but the public site it loads, so its certificate verifier's AIA fetches
// fail at once instead of waiting on the CA's repository host. At a site the probe's server serves,
// it reaches that server by address as its proxy and resolves no name at all, and at the control
// it is given the control's root as the one key whose certificate errors it ignores, which it reads
// only beside a profile of its own.
func TestImageChromiumResolvesOnlyTheProbedSite(t *testing.T) {
	stateDir := t.TempDir()
	server, err := newTLSServer(stateDir) // writes the control's root where testTLSSites names it
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	if tlsSPKIHash(testTLSSite(stateDir, "control").trust) == "" {
		t.Fatal("the control's root gives no key to ignore errors for")
	}
	chromium := imageTLSClientsOf[tlsChromiumClient](t, nil, stateDir, "chromium")["chromium"]
	for _, site := range testTLSSites(stateDir) {
		want := []string{"--headless", "--no-sandbox", "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE " + tlsSiteHosts[site.name]}
		if !site.public {
			want = []string{"--headless", "--no-sandbox", "--proxy-server=http://" + testTLSVia, "--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE " + testTLSViaHost}
		}
		if site.trust != "" {
			want = append(want, "--user-data-dir="+filepath.Join(stateDir, "chromium-control"), "--ignore-certificate-errors-spki-list="+tlsSPKIHash(site.trust))
		}
		want = append(want, "--dump-dom", site.url)
		program, args, _ := chromium.command(site)
		if program != "chromium" || !slices.Equal(args, want) {
			t.Errorf("chromium command for %s = %s %q, want chromium %q", site.name, program, args, want)
		}
	}
}

// The control's Chromium is given the SHA-256 of the control root's public key, base64, which is
// what --ignore-certificate-errors-spki-list takes; a file that holds no certificate gives no key,
// so Chromium refuses the control and the probe fails.
func TestTLSSPKIHashIsTheControlRootsKey(t *testing.T) {
	stateDir := t.TempDir()
	server, err := newTLSServer(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	control := server.sites()[2]
	pemBytes, err := os.ReadFile(control.trust)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(root.RawSubjectPublicKeyInfo)
	if got, want := tlsSPKIHash(control.trust), base64.StdEncoding.EncodeToString(sum[:]); got != want || got == "" {
		t.Errorf("tlsSPKIHash(%s) = %q, want %q", control.trust, got, want)
	}
	empty := filepath.Join(stateDir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tlsSPKIHash(empty); got != "" {
		t.Errorf("tlsSPKIHash of a file with no certificate = %q, want none", got)
	}
}

// The positive controls hold the probe's own server to its chains: each compromised site's chain
// must reach its fixture root, and the control's chain must be refused with that root alone. Either
// failing fails the probe, the probe's own fault, before any client is held to a chain.
func TestTLSPositiveControlsRequireTheExpectedChain(t *testing.T) {
	sites := testTLSSites(t.TempDir())[:len(servedTLSSiteNames)]
	for _, testCase := range []struct {
		name, want         string
		outcome, isolation tlsOutcome
	}{
		{name: "accepts its root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsCertificateRefused}},
		{name: "refuses its root", outcome: tlsOutcome{status: tlsCertificateRefused, detail: "certificate verify failed"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control at https://dv-g3b-emudhra.we-have-a-problem.com/ did not accept its root"},
		{name: "cannot reach site", outcome: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "the control accepts the compromised root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsConnectionAccepted}, want: "emSign positive control isolation at https://control.legion-tls-probe.test/ was accepted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var isolationRoots []string
			err := verifyTLSPositiveControls(sites, func(site tlsSite) tlsOutcome {
				if site.control {
					if site.trust != "" {
						t.Errorf("the isolation control at %s is given the trust file %s; want none, so only the compromised root is trusted", site.url, site.trust)
					}
					isolationRoots = append(isolationRoots, site.root)
					return testCase.isolation
				}
				return testCase.outcome
			})
			if testCase.want == "" && err != nil {
				t.Fatalf("verifyTLSPositiveControls() = %v, want success", err)
			} else if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("verifyTLSPositiveControls() = %v, want error containing %q", err, testCase.want)
			}
			if want := []string{sites[0].root, sites[1].root}; !slices.Equal(isolationRoots, want) {
				t.Errorf("the isolation control ran with the roots %q, want each compromised site's (%q)", isolationRoots, want)
			}
		})
	}
}

// Every curl the probe runs reads no curlrc: the curl client and the positive controls each reach a
// test site, and curl sends a curlrc's headers to every host it reaches. A HOME of its own does not
// keep curl from the pod's: where the curlrc is in none of CURL_HOME, XDG_CONFIG_HOME and HOME, curl
// 8.14.1 reads it from the home the passwd database names for its user (findfile, curl's
// tool_findfile.c). Measured on 2026-11-18 on the head's image (sha256:40cd7424…, the legion of
// d56fa735) against a local listener, with /home/legion/.curlrc holding an Authorization header: the
// curl client, run with HOME in the state directory, and the positive controls' curl, run with the
// pod's HOME, each sent that header. curl reads no curlrc only where -q (--disable) is its first
// argument (measured: `curl -q -s …` sent no header from a CURL_HOME curlrc, `curl -s -q …` sent it).
func TestTLSProbeCurlReadsNoCurlrc(t *testing.T) {
	stateDir := t.TempDir()
	curl := imageTLSClientsOf[tlsCommandClient](t, nil, stateDir, "curl")["curl"]
	for _, site := range testTLSSites(stateDir) {
		if _, args, _ := curl.commandLine(site); len(args) == 0 || (args[0] != "-q" && args[0] != "--disable") {
			t.Errorf("the curl client at %s runs curl %q, whose first argument is not -q", site.name, args)
		}
	}

	bin := t.TempDir()
	record := filepath.Join(bin, "first-arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + record + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := []string{"PATH=" + os.Getenv("PATH")}
	// The stand-in curl accepts every site, the isolation control included, so the controls fail;
	// only the arguments they ran curl with matter here.
	_ = probeTLSPositiveControls(context.Background(), env, stateDir, testTLSSites(stateDir)[:len(servedTLSSiteNames)])
	_ = probePublicTLSSites(context.Background(), env, stateDir)
	// The stand-in accepts each vendored chain, which gets it no further than the chain, so each
	// vendored site runs curl once, with no isolation after it.
	_ = probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the positive controls never ran curl: %v", err)
	}
	firsts := strings.Fields(string(data))
	if want := 2*2 + len(publicTLSSites) + 2; len(firsts) != want {
		t.Fatalf("the positive controls ran curl %d times, want %d: each fixture chain and its isolation, each public site and each vendored chain", len(firsts), want)
	}
	for _, first := range firsts {
		if first != "-q" && first != "--disable" {
			t.Errorf("a positive control ran curl with first argument %q, not -q", first)
		}
	}
}

// A public site's positive control decides whether the clients ask it: one that accepts its chain
// with the CA-published root alone is asked, and one that refuses it, or cannot reach it, is not,
// and says why.
func TestPublicTLSSitesAreAskedOnlyWhereTheirPositiveControlAccepts(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *" + tlsAAAHost + "*) echo 'curl: (6) Could not resolve host: " + tlsAAAHost + "' >&2; exit 6 ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sites := probePublicTLSSites(context.Background(), []string{"PATH=" + os.Getenv("PATH")}, t.TempDir())
	if len(sites) != 2 || sites[0].name != "public emSign" || sites[0].unasked != "" {
		t.Fatalf("public sites = %+v, want public emSign asked", sites)
	}
	if want := "its positive control was unavailable: exit status 6: curl: (6) Could not resolve host: " + tlsAAAHost; sites[1].unasked != want {
		t.Errorf("public AAA unasked = %q, want %q", sites[1].unasked, want)
	}
	if got, want := tlsSiteLine(sites[1]), "probe-image: UNKNOWN: the public AAA chain at "+tlsAAAHost+": "+sites[1].unasked; got != want {
		t.Errorf("tlsSiteLine(public AAA) = %q, want %q", got, want)
	}
	if got, want := tlsSiteLine(sites[0]), "probe-image: TLS public emSign: chain verified"; got != want {
		t.Errorf("tlsSiteLine(public emSign) = %q, want %q", got, want)
	}
}

// A vendored chain's positive control decides whether clients are held to it: one whose curl,
// given the CA-published root alone, gets past the chain to the server's signature, and refuses the
// control's chain with that root, is asked; one that gets no further, as once its leaf has expired,
// is not, and its line says why as UNKNOWN. A curl that gets past every chain, as `curl -k` does,
// also gets to the server's signature, so only the isolation at the control shows it trusts nothing
// but the root: where the control is accepted with that root, neither vendored chain is asked.
func TestVendoredTLSSitesAreAskedOnlyWhereTheirPositiveControlGetsPastTheChain(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"*" + tlsControlHost + "*) echo 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2; exit 60 ;;\n" +
		"*aaa-certificate-services*) echo 'curl: (60) SSL certificate problem: certificate has expired' >&2; exit 60 ;;\n" +
		"esac\necho 'curl: (35) TLS connect error: error:030000EA:digital envelope routines::provider signature failure' >&2; exit 35\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stateDir := t.TempDir()
	sites := probeVendoredTLSSites(context.Background(), []string{"PATH=" + os.Getenv("PATH")}, stateDir, testTLSSites(stateDir))
	if len(sites) != 2 || sites[0].name != "vendored emSign" || sites[0].unasked != "" {
		t.Fatalf("vendored sites = %+v, want vendored emSign asked", sites)
	}
	if want := "its positive control was refused, where it must get past the chain to the server's signature: exit status 60: curl: (60) SSL certificate problem: certificate has expired"; sites[1].name != "vendored AAA" || sites[1].unasked != want {
		t.Errorf("vendored AAA = %+v, want unasked %q", sites[1], want)
	}
	if got, want := tlsSiteLine(sites[1]), "probe-image: UNKNOWN: the vendored AAA chain at "+tlsAAAHost+": "+sites[1].unasked; got != want {
		t.Errorf("tlsSiteLine(vendored AAA) = %q, want %q", got, want)
	}

	acceptsAll := "#!/bin/sh\ncase \"$*\" in *" + tlsControlHost + "*) exit 0 ;; esac\n" +
		"echo 'curl: (35) TLS connect error: error:030000EA:digital envelope routines::provider signature failure' >&2; exit 35\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(acceptsAll), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, site := range probeVendoredTLSSites(context.Background(), []string{"PATH=" + os.Getenv("PATH")}, stateDir, testTLSSites(stateDir)) {
		if want := "its positive control's isolation at https://" + tlsControlHost + "/ was accepted: the command returned no output"; site.unasked != want {
			t.Errorf("%s with a curl that accepts every chain: unasked %q, want %q", site.name, site.unasked, want)
		}
	}
}

// The probe holds every client to the fixture chains and the control, each client that verifies a
// chain during the handshake to the vendored chains, and the others to the public sites; a vendored
// chain whose positive control failed sends every client to its public site instead. With the
// public hosts unreachable the probe passes, and an UNKNOWN line names each client held to them,
// with the host, as does the line of each site no client is held to: nothing it did not verify
// reads as clean.
func TestTLSProbeHoldsEachClientToItsSitesAndNamesEveryUnknown(t *testing.T) {
	previousClients, previousControls, previousVendored, previousPublic := newImageTLSClients, newTLSPositiveControls, newVendoredTLSSites, newPublicTLSSites
	t.Cleanup(func() {
		newImageTLSClients, newTLSPositiveControls, newVendoredTLSSites, newPublicTLSSites = previousClients, previousControls, previousVendored, previousPublic
	})
	expired := "its positive control was refused, where it must get past the chain to the server's signature: certificate has expired"
	newTLSPositiveControls = func(context.Context, []string, string, []tlsSite) error { return nil }
	newVendoredTLSSites = func(ctx context.Context, env []string, stateDir string, sites []tlsSite) []tlsSite {
		vendored := askedVendoredTLSSites(ctx, env, stateDir, sites)
		vendored[1].unasked = expired
		return vendored
	}
	newPublicTLSSites = unaskedPublicTLSSites
	newImageTLSClients = passingImageTLSClients
	var stdout bytes.Buffer
	if err := runTLSProbe(context.Background(), "omp", nil, &stdout); err != nil {
		t.Fatalf("runTLSProbe() with the public hosts unreachable = %v, want success", err)
	}
	out := stdout.String()
	afterHandshake := tlsChainAfterHandshakeClients()
	public := unaskedPublicTLSSites(context.Background(), nil, "")
	for _, client := range workerTLSClientNames() {
		late := slices.Contains(afterHandshake, client)
		for site, held := range map[string]bool{"emSign": true, "AAA": true, "control": true, "vendored emSign": !late, "vendored AAA": false, "public emSign": late, "public AAA": true} {
			if got := strings.Contains(out, "probe-image: TLS client "+client+" "+site+": "); got != held {
				t.Errorf("%s held to %s: %v, want %v", client, site, got, held)
			}
		}
		for index, host := range []string{tlsEmSignHost, tlsAAAHost} {
			line := "probe-image: UNKNOWN: " + client + " against " + host + ": not asked: " + public[index].unasked
			if want := late || host == tlsAAAHost; strings.Contains(out, line+"\n") != want {
				t.Errorf("stdout names %s UNKNOWN against %s: %v, want %v:\n%s", client, host, !want, want, out)
			}
		}
	}
	for _, line := range []string{
		"probe-image: TLS vendored emSign: chain verified",
		"probe-image: UNKNOWN: the vendored AAA chain at " + tlsAAAHost + ": " + expired,
		"probe-image: UNKNOWN: the public emSign chain at " + tlsEmSignHost + ": " + public[0].unasked,
		"probe-image: UNKNOWN: the public AAA chain at " + tlsAAAHost + ": " + public[1].unasked,
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("stdout lacks %q:\n%s", line, out)
		}
	}
}

func TestTLSCommandPastTheProbeDeadlineIsUnavailableAndFailsTheProbe(t *testing.T) {
	sites := testTLSSites(t.TempDir())[:2]
	client := tlsCommandClient{
		clientName: "curl",
		timeout:    tlsDefaultProbeTimeout,
		command: func(site tlsSite) (string, []string, []string) {
			if site.name == "emSign" {
				return "sh", []string{"-c", "echo certificate verify failed >&2; exit 1"}, nil
			}
			return "sh", []string{"-c", "exec sleep 1"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	observations, err := probeTLSClients(ctx, []tlsClient{client}, sites)
	if len(observations) != 2 {
		t.Fatalf("probeTLSClients() returned %d observations, want the client to reach both sites", len(observations))
	}
	if got := observations[1]; got.status != tlsNetworkUnavailable || got.detail != "probe deadline" {
		t.Fatalf("AAA outcome = %+v, want unavailable (probe deadline)", got.tlsOutcome)
	}
	if err == nil || !strings.Contains(err.Error(), "curl cannot verify refusal of the AAA chain: probe deadline") {
		t.Fatalf("probeTLSClients() = %v, want the deadline to name curl and AAA", err)
	}
}
func TestTLSPositiveControlsLineNamesEverySite(t *testing.T) {
	sites := []tlsSite{{name: "first"}, {name: "second"}, {name: "third"}}
	const want = "probe-image: TLS positive controls: first, second and third chains verified"

	if got := tlsPositiveControlsLine(sites); got != want {
		t.Fatalf("tlsPositiveControlsLine() = %q, want %q", got, want)
	}
}

func TestTLSCommandClientClassifiesOutputStreams(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr, exit string
		want                       tlsStatus
	}{
		{name: "accepts a loaded body with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate failure", stderr: "net::ERR_CERT_AUTHORITY_INVALID", exit: "failure", want: tlsCertificateRefused},
		{name: "reports another failure as unavailable", stderr: "network is unreachable", exit: "failure", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsCommandClient{
				clientName: "generic",
				timeout:    tlsDefaultProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; test "$3" != failure`, "sh", testCase.stdout, testCase.stderr, testCase.exit}, nil
				},
			}

			if outcome := client.probe(context.Background(), controlTLSSite); outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
		})
	}
}

func TestTLSChromiumClientClassifiesNavigationResult(t *testing.T) {
	for _, testCase := range []struct {
		name, stdout, stderr string
		want                 tlsStatus
	}{
		{name: "accepts a loaded DOM with certificate text", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", want: tlsConnectionAccepted},
		{name: "refuses a certificate navigation failure", stdout: "page body NET::ERR_CERT_AUTHORITY_INVALID", stderr: "Page load failed: net::ERR_CERT_AUTHORITY_INVALID", want: tlsCertificateRefused},
		{name: "refuses after failed AIA fetches", stderr: "[5:18:1028/173830.670117:ERROR:net/cert/internal/cert_issuer_source_aia.cc:138] AiaRequest::OnFetchCompleted got error -105\n[5:5:1028/173830.690777:ERROR:components/headless/command_handler/headless_command_handler.cc:403] Page load failed: net::ERR_CERT_AUTHORITY_INVALID\n", want: tlsCertificateRefused},
		{name: "reports another navigation failure as unavailable", stderr: "Page load failed: net::ERR_NAME_NOT_RESOLVED", want: tlsNetworkUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := tlsChromiumClient{tlsCommandClient: tlsCommandClient{
				clientName: "chromium",
				timeout:    tlsChromiumProbeTimeout,
				command: func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2`, "sh", testCase.stdout, testCase.stderr}, nil
				},
			}}

			outcome := client.probe(context.Background(), controlTLSSite)
			if outcome.status != testCase.want {
				t.Fatalf("client.probe() = %+v, want status %q", outcome, testCase.want)
			}
			if strings.Contains(outcome.detail, "page body") {
				t.Fatalf("client.probe() detail = %q, must not read the DOM", outcome.detail)
			}
		})
	}
}

// Chromium's first process in a probe pod needs more time than the other clients,
// while each ordinary client keeps the short refusal bound.
func TestImageTLSChromiumClientHasColdStartHeadroom(t *testing.T) {
	chromium := imageTLSClientsOf[tlsChromiumClient](t, nil, "", "chromium")["chromium"]
	chromium.command = func(tlsSite) (string, []string, []string) {
		return "sh", []string{"-c", "exec sleep 21"}, nil
	}

	outcome := chromium.probe(context.Background(), controlTLSSite)
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}
