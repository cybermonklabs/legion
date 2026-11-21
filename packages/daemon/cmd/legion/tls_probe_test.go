package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
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
		{name: "control", url: "https://" + tlsControlHost + "/", root: trust, kind: tlsControlSite, via: testTLSVia, trust: trust},
		{name: "vendored emSign", url: publicEmSignTLSSite.url, root: publicEmSignTLSSite.root, kind: tlsVendoredSite, via: testTLSVia},
		{name: "vendored AAA", url: publicAAATLSSite.url, root: publicAAATLSSite.root, kind: tlsVendoredSite, via: testTLSVia},
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

// emSignTLSSite and aaaTLSSite are the fixture sites of testTLSSites, the compromised chains the
// probe's server serves, for a test that runs a client at one of them.
var (
	emSignTLSSite = testTLSSite("", "emSign")
	aaaTLSSite    = testTLSSite("", "AAA")
)

// compromisedTLSSites are the fixture sites of testTLSSites: the sites the positive controls reach
// when they pass.
func compromisedTLSSites() []tlsSite { return []tlsSite{emSignTLSSite, aaaTLSSite} }

// servedTLSSites are the fixture sites and the control of testTLSSites, the sites every client is
// held to.
func servedTLSSites() []tlsSite { return []tlsSite{emSignTLSSite, aaaTLSSite, controlTLSSite} }

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
func askedVendoredTLSSites(_ context.Context, _ []string, _ string, sites []tlsSite) ([]tlsSite, error) {
	var vendored []tlsSite
	for _, site := range sites {
		if site.kind == tlsVendoredSite {
			vendored = append(vendored, site)
		}
	}
	return vendored, nil
}

// fakeTLSClient gives each site the outcome outcomes names for it. At a site the probe's server
// serves, it counts a handshake there first, as a client that reached the server does.
type fakeTLSClient struct {
	clientName string
	outcomes   map[string]tlsOutcome
}

func (c fakeTLSClient) name() string { return c.clientName }

func (c fakeTLSClient) probe(_ context.Context, site tlsSite, _ tlsRunTerms) tlsOutcome {
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
// as is a site no client asks. A public site that is down takes none of main's no-verdict exit: only
// a run the probe's deadline ended has no verdict there, as at every site.
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
		name      string
		outcomes  map[string]tlsOutcome
		want      string
		unknown   []string
		noVerdict string
	}{
		{name: "both compromised chains refused and control reachable", outcomes: passing()},
		{name: "compromised chain accepted", outcomes: with("emSign", accepted), want: "node accepts the emSign chain"},
		{name: "compromised chain neither refused nor accepted", outcomes: with("emSign", unavailable), want: "node cannot verify refusal of the emSign chain: dial tcp: network is unreachable"},
		{name: "control unreachable", outcomes: with("control", unavailable), want: "network unavailable: node cannot reach https://control.legion-tls-probe.test/: dial tcp: network is unreachable"},
		{name: "vendored chain accepted past it to the server's signature", outcomes: with("vendored AAA", pastTheChain), want: "node accepts the vendored AAA chain: it trusts a root of that chain's root's name, and got past the chain's names to a signature check, which the probe's chains cannot pass: SSL_connect failed: bad signature"},
		{name: "vendored chain neither refused nor accepted", outcomes: with("vendored emSign", unavailable), want: "node cannot verify refusal of the vendored emSign chain"},
		{name: "public chain accepted", outcomes: with("public AAA", accepted), want: "node accepts the public AAA chain: HTTP 200"},
		{name: "public site unreachable", outcomes: with("public emSign", unavailable), unknown: []string{"public emSign"}},
		{name: "public site whose host went down after its positive control", outcomes: with("public AAA", tlsOutcome{status: tlsNetworkUnavailable, detail: "its 20s limit ended it after 20.0s: connect timed out"}), unknown: []string{"public AAA"}},
		{name: "public site past the probe's deadline", outcomes: with("public emSign", tlsOutcome{status: tlsNoVerdict, detail: "the probe's deadline ended it after 1.0s: signal: terminated"}), noVerdict: "node public emSign: the probe's deadline ended it after 1.0s: signal: terminated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: testCase.outcomes}}, sites, nil)
			if want := []string{}; testCase.noVerdict != "" {
				want = []string{testCase.noVerdict}
				if !slices.Equal(noVerdict, want) {
					t.Fatalf("probeTLSClients() runs with no verdict = %q, want %q", noVerdict, want)
				}
			} else if len(noVerdict) > 0 {
				t.Fatalf("probeTLSClients() runs with no verdict = %q, want none: a public site that gives no verdict is UNKNOWN, which takes no exit 75", noVerdict)
			}
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

	observations, _, err := probeTLSClients(context.Background(), clients, sites, nil)
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

func (c countingTLSClient) probe(ctx context.Context, site tlsSite, terms tlsRunTerms) tlsOutcome {
	c.asked[site.name]++
	return c.fakeTLSClient.probe(ctx, site, terms)
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

	_, _, err := probeTLSClients(context.Background(), []tlsClient{bypassingTLSClient{bypassing}}, sites, nil)
	want := "node cannot verify refusal of the emSign chain: never reached the probe's server, so the verdict is not the probe's chain's: CERT_REVOKED"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "network unavailable: node cannot reach https://control.legion-tls-probe.test/") {
		t.Fatalf("probeTLSClients() with a client that never reached the server = %v, want %q and the control unreachable", err, want)
	}
	if _, _, err := probeTLSClients(context.Background(), []tlsClient{fakeTLSClient{clientName: "node", outcomes: outcomes}}, sites, nil); err != nil {
		t.Fatalf("probeTLSClients() with a client that reached the server = %v, want success", err)
	}
}

// bypassingTLSClient answers without reaching the probe's server.
type bypassingTLSClient struct{ countingTLSClient }

func (c bypassingTLSClient) probe(_ context.Context, site tlsSite, _ tlsRunTerms) tlsOutcome {
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

	outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{})

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

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
				t.Fatalf("%s probe = %+v, want status %q", testCase.client, outcome, testCase.want)
			}
		})
	}
}

// A client that accepted the chain and then got an error status names it, and that reads as an
// acceptance, never as unavailable: at a public site unavailable reads as UNKNOWN and fails nothing,
// so an acceptance an error status followed would pass. Every HTTP client the probe holds to a
// public site has a row here (redis-cli and socat speak no HTTP). Each output is the client's, run
// as the probe runs it, at a server of 127.0.0.1 answering the status with an empty body and a leaf
// whose root the client was given, measured on 2026-11-20 in the image of 1c7c825367 with a 401,
// 403, 429 and 503 (a status not written here read as its row's does, with its own number or
// phrase). The refusals beside them, which name no status, still read as refusals.
func TestImageTLSClientsReadAnHTTPErrorAfterTheHandshakeAsAnAcceptance(t *testing.T) {
	fetchSource := `file:///home/legion/[eval1]:1 const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status); ^ `
	bunSource := `1 | const response = await fetch(process.env.LEGION_TLS_URL); if (!response.ok) throw new Error("HTTP " + response.status); ^ `
	pyTrace := `Traceback (most recent call last): File "/usr/lib/python3.13/urllib/request.py", line 613, in http_error_default raise HTTPError(req.full_url, code, msg, hdrs, fp) `
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), workerTLSClientNames()...)
	rows := []struct {
		client, name, stdout, stderr, exit string
		want                               tlsStatus
	}{
		{client: "node", name: "503", exit: "1", stderr: fetchSource + `Error: HTTP 503 at file:///home/legion/[eval1]:1:83`, want: tlsConnectionAccepted},
		{client: "codegraph-node", name: "503", exit: "1", stderr: fetchSource + `Error: HTTP 503 at file:///home/legion/[eval1]:1:83`, want: tlsConnectionAccepted},
		{client: "bun", name: "503", exit: "1", stderr: bunSource + `error: HTTP 503 at /home/legion/[eval]:1:87 Bun v1.4.4 (Linux x64)`, want: tlsConnectionAccepted},
		{client: "omp", name: "503", exit: "1", stderr: bunSource + `error: HTTP 503 at /home/legion/[eval]:1:87 Bun v1.4.5 (Linux x64)`, want: tlsConnectionAccepted},
		{client: "uv", name: "429", exit: "2", stderr: `Using Python 3.13.5 environment at: /usr error: Request failed after 3 retries in 6.2s cause: Failed to fetch: https://127.0.0.1/legion-tls-probe-not-a-package/ cause: HTTP status client error (429 Too Many Requests) for url (https://127.0.0.1/legion-tls-probe-not-a-package/)`, want: tlsConnectionAccepted},
		{client: "uv", name: "503", exit: "2", stderr: `Using Python 3.13.5 environment at: /usr error: Request failed after 3 retries in 7.5s cause: Failed to fetch: https://127.0.0.1/legion-tls-probe-not-a-package/ cause: HTTP status server error (503 Service Unavailable) for url (https://127.0.0.1/legion-tls-probe-not-a-package/)`, want: tlsConnectionAccepted},
		{client: "uv", name: "403", exit: "1", stderr: `Using Python 3.13.5 environment at: /usr error: No solution found when resolving dependencies cause: Because legion-tls-probe-not-a-package was not found in the package registry and you require legion-tls-probe-not-a-package, we can conclude that your requirements are unsatisfiable. hint: An index (https://127.0.0.1/) returned a 403 Forbidden error.`, want: tlsConnectionAccepted},
		{client: "aws", name: "503", exit: "254", stderr: `aws: [ERROR]: An error occurred (503) when calling the GetCallerIdentity operation (reached max retries: 2): Service Unavailable`, want: tlsConnectionAccepted},
		{client: "aws", name: "401", exit: "255", stderr: `aws: [ERROR]: Unable to parse response (no element found: line 1, column 0), invalid XML received. Further retries may succeed: b''`, want: tlsConnectionAccepted},
		{client: "curl", name: "503", exit: "22", stderr: `curl: (22) The requested URL returned error: 503`, want: tlsConnectionAccepted},
		{client: "python3", name: "503", exit: "1", stderr: pyTrace + `urllib.error.HTTPError: HTTP Error 503: Service Unavailable`, want: tlsConnectionAccepted},
		{client: "gh", name: "503", exit: "1", stderr: `gh: HTTP 503`, want: tlsConnectionAccepted},
		{client: "wget", name: "503", exit: "8", stderr: `https://127.0.0.1/: 2026-11-20 16:37:49 ERROR 503: Service Unavailable.`, want: tlsConnectionAccepted},
		{client: "wget", name: "401", exit: "6", stderr: `Username/Password Authentication Failed.`, want: tlsConnectionAccepted},
		{client: "git", name: "503", exit: "128", stderr: `fatal: unable to access 'https://127.0.0.1/': The requested URL returned error: 503`, want: tlsConnectionAccepted},
		{client: "git", name: "401", exit: "128", stderr: `fatal: could not read Username for 'https://127.0.0.1': terminal prompts disabled`, want: tlsConnectionAccepted},
		{client: "go", name: "503", exit: "1", stderr: `go: 127.0.0.1/legion-tls-probe@latest: unrecognized import path "127.0.0.1/legion-tls-probe": reading https://127.0.0.1/legion-tls-probe?go-get=1: 503 Service Unavailable`, want: tlsConnectionAccepted},
		{client: "kubectl", name: "401", exit: "1", stderr: `error: You must be logged in to the server (the server has asked for the client to provide credentials)`, want: tlsConnectionAccepted},
		{client: "kubectl", name: "403", exit: "1", stderr: `Error from server (Forbidden):`, want: tlsConnectionAccepted},
		{client: "kubectl", name: "503", exit: "1", stderr: `Error from server (ServiceUnavailable): the server is currently unable to handle the request`, want: tlsConnectionAccepted},
		{client: "gcloud", name: "503", exit: "1", stderr: `ERROR: (gcloud.compute.regions.list) Some requests did not succeed: -`, want: tlsConnectionAccepted},
		{client: "bq", name: "401", exit: "1", stdout: `Authorization error. If you used service account credentials, the server likely returned an Unauthorized response. Verify that you are using the correct account with the correct permissions to access the service endpoint. google.auth.exceptions.RefreshError: The credentials do not contain the necessary fields need to refresh the access token.`, want: tlsConnectionAccepted},
		{client: "bq", name: "503", exit: "1", stdout: `BigQuery error in ls operation: Got 503 response from discovery url: https://127.0.0.1/$discovery/rest?version=v2`, want: tlsConnectionAccepted},
		{client: "az", name: "401", exit: "1", stderr: `ERROR: Unauthorized Interactive authentication is needed. Please run: az logout az login`, want: tlsConnectionAccepted},
		{client: "az", name: "429", exit: "1", stderr: `ERROR: Too Many Requests`, want: tlsConnectionAccepted},
		{client: "az", name: "503", exit: "1", stderr: `ERROR: Service Unavailable`, want: tlsConnectionAccepted},
		{client: "mise", name: "503", exit: "1", stderr: `DEBUG GET Downloading https://127.0.0.1/ to ~/data/downloads/http-legion-tls-probe/1/ DEBUG GET https://127.0.0.1/ DEBUG GET https://127.0.0.1/ 503 Service Unavailable Error: 0: Failed to install http:legion-tls-probe@1`, want: tlsConnectionAccepted},
		{client: "chromium", name: "401", exit: "0", stderr: `[735:735:1120/163705.4:ERROR:components/headless/command_handler/headless_command_handler.cc:403] Page load failed:net::ERR_INVALID_AUTH_CREDENTIALS`, want: tlsConnectionAccepted},
		{client: "chromium", name: "503", exit: "0", stderr: `[3202:3202:1120/163752.9:ERROR:components/headless/command_handler/headless_command_handler.cc:403] Page load failed:net::ERR_HTTP_RESPONSE_CODE_FAILURE`, want: tlsConnectionAccepted},
		{client: "node", name: "refusal, its source line printed", exit: "1", stderr: fetchSource + tlsRefusalRows[0].public, want: tlsCertificateRefused},
		{client: "bun", name: "refusal, its source line printed", exit: "1", stderr: bunSource + tlsRefusalRows[1].public, want: tlsCertificateRefused},
		{client: "wget", name: "refusal", exit: "5", stderr: wgetEmSignRefusal, want: tlsCertificateRefused},
		{client: "git", name: "refusal", exit: "128", stderr: gitEmSignRefusal, want: tlsCertificateRefused},
		{client: "kubectl", name: "refusal", exit: "1", stderr: kubectlEmSignRefusal, want: tlsCertificateRefused},
		{client: "chromium", name: "refusal", exit: "0", stderr: `Page load failed:net::ERR_CERT_AUTHORITY_INVALID`, want: tlsCertificateRefused},
	}
	answered := map[string]bool{}
	for _, row := range rows {
		answered[row.client] = answered[row.client] || row.want == tlsConnectionAccepted
	}
	for _, name := range workerTLSClientNames() {
		if name != "redis-cli" && name != "socat" && !answered[name] {
			t.Errorf("%s has no measured error-status row: every HTTP client the probe holds to a public site needs one", name)
		}
	}
	for _, row := range rows {
		t.Run(row.client+" "+row.name, func(t *testing.T) {
			command := clients[row.client]
			command.command = func(tlsSite) (string, []string, []string) {
				return "sh", []string{"-c", `printf %s "$1"; printf %s "$2" >&2; exit "$3"`, "sh", row.stdout, row.stderr, row.exit}, nil
			}
			var client tlsClient = command
			if row.client == "chromium" {
				client = tlsChromiumClient{tlsCommandClient: command}
			}
			if outcome := client.probe(context.Background(), publicEmSignTLSSite, tlsRunTerms{}); outcome.status != row.want {
				t.Fatalf("%s at the public emSign site = %+v, want status %q", row.client, outcome, row.want)
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
				if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != tlsCertificateRefused || outcome.signatureFailed {
					t.Fatalf("%s at the %s chain = %+v, want refused, no failed signature", row.client, site, outcome)
				}
			})
		}
		if same := address.ReplaceAllString(row.public, "") == address.ReplaceAllString(row.fixture, ""); same != row.same {
			t.Errorf("%s: the public and fixture refusals agree %v, but the row says %v", row.client, same, row.same)
		}
	}
}

// At a public site a refusal counts only where it is a refusal of the site's own chain. node,
// CodeGraph's node, bun, omp and uv refuse the public chains by a list keyed to their certificates,
// in words no other chain draws (tlsClientSpec.publicRefusal), so a refusal of theirs there in the
// words any chain whose root they lack draws (their fixture refusal) refused some other chain, as
// one a server the site's name has come to reach serves: that run is no refusal, its detail says
// so, the site reads as unknown, and no later client asks it. Every other client refuses such a
// chain in the words it refuses the site's (tlsRefusalRows' same), so its refusal at a public site
// cannot be told apart, and counts as the site's.
func TestTLSProbeCountsARefusalAtAPublicSiteOnlyWhereItNamesTheSitesChain(t *testing.T) {
	names := make([]string, 0, len(tlsRefusalRows))
	for _, row := range tlsRefusalRows {
		names = append(names, row.client)
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), names...)
	printing := func(client tlsCommandClient, output, exit string, stdout bool) tlsCommandClient {
		script := `printf %s "$1" >&2; exit "$2"`
		if stdout {
			script = `printf %s "$1"; exit "$2"`
		}
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", script, "sh", output, exit}, nil
		}
		return client
	}
	for _, row := range tlsRefusalRows {
		for column, output := range map[string]string{"public": row.public, "fixture": row.fixture} {
			t.Run(row.client+" "+column+" refusal at the public site", func(t *testing.T) {
				outcome := printing(clients[row.client], output, row.exit, row.stdout).probe(context.Background(), publicEmSignTLSSite, tlsRunTerms{})
				if column == "public" || row.same {
					if outcome.status != tlsCertificateRefused {
						t.Errorf("%s's %s refusal at the public emSign site = %+v, want refused", row.client, column, outcome)
					}
				} else if outcome.status != tlsNetworkUnavailable || !strings.HasPrefix(outcome.detail, tlsUntiedRefusal) {
					t.Errorf("%s's %s refusal at the public emSign site = %+v, want unavailable, its detail beginning %q", row.client, column, outcome, tlsUntiedRefusal)
				}
			})
		}
	}

	node := printing(clients["node"], tlsRefusalRows[0].fixture, "1", false)
	wget := printing(clients["wget"], wgetEmSignRefusal, "5", false)
	observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{node, wget}, []tlsSite{publicEmSignTLSSite}, nil)
	if err != nil || len(noVerdict) != 0 || len(observations) != 2 {
		t.Fatalf("probeTLSClients() = %+v, runs with no verdict %q, %v; want two observations and no failure", observations, noVerdict, err)
	}
	if got, want := tlsUnknownLine(observations[0]), "probe-image: UNKNOWN: node against "+tlsEmSignHost+": "+tlsUntiedRefusal+"exit status 1: "; observations[0].status != tlsPublicUnknown || !strings.HasPrefix(got, want) {
		t.Errorf("node's refusal of another chain at the public emSign site = %+v, line %q; want unknown, the line beginning %q", observations[0].tlsOutcome, got, want)
	}
	if want := "not asked: node gave no verdict there: " + tlsUntiedRefusal; observations[1].status != tlsPublicUnknown || !strings.HasPrefix(observations[1].detail, want) {
		t.Errorf("wget at the public emSign site after node = %+v, want unknown, its detail beginning %q", observations[1].tlsOutcome, want)
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
		outcome := client.probe(context.Background(), testTLSSite("", "vendored emSign"), tlsRunTerms{})
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
// CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE and GIT_SSL_CAINFO naming a bundle holding them). Where a
// client's output there was its ordinary refusal (redis-cli, socat at the RSA root), the rule is
// blind to it, and only the vendored chains or the public sites can show it trusting the root. git
// with no root of the name refused the fixture with "certificate signer not trusted"
// (gitEmSignRefusal), so its "no details" refusal is the root's failed signature; wget, given the
// bundle with --ca-certificate as only an image's /etc/wgetrc can give it, printed "is not
// trusted." alone, where with no root of the name it adds "doesn't have a known issuer."
// (wgetEmSignRefusal).
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
		"git":     `fatal: unable to access 'https://dv-g3b-emudhra.we-have-a-problem.com/': server verification failed: certificate error, no details available. (CAfile: /planted/real.pem CRLfile: none)`,
		"wget":    `ERROR: The certificate of 'dv-g3b-emudhra.we-have-a-problem.com' is not trusted.`,
	}
	blind := map[string]string{
		"redis-cli": `Could not connect to Redis at 127.0.0.1:35193: SSL_connect failed: certificate verify failed`,
		"git":       gitEmSignRefusal,
		"wget":      wgetEmSignRefusal,
	}
	sites := testTLSSites(t.TempDir())
	emSign := sites[0]
	for name, output := range held {
		client := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), name)[name]
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 1`, "sh", output}, nil
		}
		if outcome := client.probe(context.Background(), emSign, tlsRunTerms{}); !outcome.signatureFailed {
			t.Errorf("%s at the emSign chain = %+v, want a failed signature read from its output", name, outcome)
		}
		_, _, err := probeTLSClients(context.Background(), []tlsClient{client}, sites[:1], nil)
		if want := name + " holds the emSign root"; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("probeTLSClients() with %s's output = %v, want an error containing %q", name, err, want)
		}
	}
	for name, output := range blind {
		client := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), name)[name]
		client.command = func(tlsSite) (string, []string, []string) {
			return "sh", []string{"-c", `printf %s "$1" >&2; exit 1`, "sh", output}, nil
		}
		if outcome := client.probe(context.Background(), emSign, tlsRunTerms{}); outcome.status != tlsCertificateRefused || outcome.signatureFailed {
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
		if site.kind != tlsPublicSite {
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
			if site.kind != tlsPublicSite {
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
				if site.kind == tlsControlSite && slices.Contains(controlTrustSettings[client.name()], name) {
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
						if site.kind == tlsControlSite {
							kind = "control"
						}
						return "sh", []string{"-c", script, "sh", kind}, set
					}
					clients = append(clients, client)
				}
				return clients
			}

			sites := testTLSSites(t.TempDir())
			_, _, err := probeTLSClients(context.Background(), standIns(append(slices.Clone(pod), testCase.setting)), sites, nil)
			for _, name := range testCase.clients {
				if want := name + " accepts the emSign chain"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("probeTLSClients() under a pod env with %s = %v, want an error containing %q", testCase.setting, err, want)
				}
			}
			if _, _, err := probeTLSClients(context.Background(), standIns(pod), sites, nil); err != nil {
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

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
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
		observations, _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite(program)}, sites, nil)
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

	if _, _, err := probeTLSClients(context.Background(), []tlsClient{acceptingEverySite("sh")}, sites, nil); err == nil || !strings.Contains(err.Error(), "optional accepts the emSign chain") {
		t.Fatalf("probeTLSClients() with the program on PATH accepting every chain = %v, want the emSign acceptance named", err)
	}

	optional := imageTLSClientsOf[tlsOptionalClient](t, nil, t.TempDir(), optionalTLSClientNames...)
	t.Setenv("PATH", bin)
	for name, client := range optional {
		for _, site := range sites {
			if outcome := client.probe(context.Background(), site, tlsRunTerms{}); outcome.status != tlsClientAbsent || outcome.detail != name+" is not on PATH" {
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
		if site.kind != tlsPublicSite {
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
		if site.kind != tlsPublicSite {
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
// failing fails the probe, the probe's own fault, before any client is held to a chain. A run the
// probe's deadline ended has no verdict, which fails nothing and is named apart from the failures.
func TestTLSPositiveControlsRequireTheExpectedChain(t *testing.T) {
	sites := testTLSSites(t.TempDir())[:len(servedTLSSiteNames)]
	cut := tlsOutcome{status: tlsNoVerdict, detail: "the probe's deadline ended it after 0.2s: signal: terminated"}
	for _, testCase := range []struct {
		name, want         string
		outcome, isolation tlsOutcome
		noVerdict          []string
	}{
		{name: "accepts its root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsCertificateRefused}},
		{name: "refuses its root", outcome: tlsOutcome{status: tlsCertificateRefused, detail: "certificate verify failed"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control at https://dv-g3b-emudhra.we-have-a-problem.com/ did not accept its root"},
		{name: "cannot reach site", outcome: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, isolation: tlsOutcome{status: tlsCertificateRefused}, want: "emSign positive control"},
		{name: "the control accepts the compromised root", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsConnectionAccepted}, want: "emSign positive control isolation at https://control.legion-tls-probe.test/ was accepted"},
		{name: "cannot reach the control", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: tlsOutcome{status: tlsNetworkUnavailable, detail: "network is unreachable"}, want: "positive control isolation at"},
		{name: "the isolation cut by the probe's deadline", outcome: tlsOutcome{status: tlsConnectionAccepted}, isolation: cut, noVerdict: []string{
			"emSign positive control isolation at https://" + tlsControlHost + "/: " + cut.detail,
			"AAA positive control isolation at https://" + tlsControlHost + "/: " + cut.detail,
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var isolationRoots []string
			noVerdict, err := verifyTLSPositiveControls(sites, func(site tlsSite) tlsOutcome {
				if site.kind == tlsControlSite {
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
			if !slices.Equal(noVerdict, testCase.noVerdict) {
				t.Errorf("verifyTLSPositiveControls() runs with no verdict = %q, want %q", noVerdict, testCase.noVerdict)
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
	_, _ = probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
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
// with the CA-published root alone is asked, and so is one that verified that chain and answered an
// error status, which curl --fail names "The requested URL returned error: 503": an outage or a rate
// limit at the site leaves the chain the clients must refuse in place. One that refuses the chain,
// or cannot be reached, is not asked, and says why. Each is held to its CA-published root in the
// state directory, where the probe's server writes the roots the probe embeds.
func TestPublicTLSSitesAreAskedOnlyWhereTheirPositiveControlAccepts(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *" + tlsAAAHost + "*) echo 'curl: (6) Could not resolve host: " + tlsAAAHost + "' >&2; exit 6 ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stateDir := t.TempDir()
	sites := probePublicTLSSites(context.Background(), []string{"PATH=" + os.Getenv("PATH")}, stateDir)
	if len(sites) != 2 || sites[0].name != "public emSign" || sites[0].unasked != "" {
		t.Fatalf("public sites = %+v, want public emSign asked", sites)
	}
	for index, site := range sites {
		if want := filepath.Join(stateDir, publicTLSSites[index].root); site.root != want {
			t.Errorf("%s's root = %q, want %q in the state directory", site.name, site.root, want)
		}
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

	answering := "#!/bin/sh\necho 'curl: (22) The requested URL returned error: 503' >&2\nexit 22\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(answering), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, site := range probePublicTLSSites(context.Background(), []string{"PATH=" + os.Getenv("PATH")}, t.TempDir()) {
		if site.unasked != "" {
			t.Errorf("%s whose positive control verified the chain and got a 503: unasked %q, want asked", site.name, site.unasked)
		}
	}
}

// A vendored chain's positive control decides whether clients are held to it: one whose curl,
// given the CA-published root alone, gets past the chain to the server's signature, and refuses the
// control's chain with that root, is asked; one that refuses the chain as expired, as every one
// does once its leaf has expired, is not, and its line says why as UNKNOWN. Every other control run
// that does not pass fails the probe, as a built chain's positive control does, since the copy is
// served on the pod's own loopback from roots the probe embeds: a curl that gets past every chain,
// as `curl -k` does, which only the isolation at the control shows; a curl that cannot read its
// root (exit 77), as when the probe ran with the image's roots and an image lacked them; a refusal
// for another cause; and a run its own limit or a signal ended, at the chain or at its isolation.
// A run the probe's deadline ended has no verdict, which fails nothing and leaves the chain unasked.
func TestVendoredTLSSitesAreAskedOnlyWhereTheirPositiveControlGetsPastTheChain(t *testing.T) {
	bin := t.TempDir()
	install := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const pastTheChain = "echo 'curl: (35) TLS connect error: error:030000EA:digital envelope routines::provider signature failure' >&2; exit 35\n"
	const refusesTheControl = "*" + tlsControlHost + "*) echo 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2; exit 60 ;;\n"
	install("#!/bin/sh\ncase \"$*\" in\n" + refusesTheControl +
		"*aaa-certificate-services*) echo 'curl: (60) SSL certificate problem: certificate has expired' >&2; exit 60 ;;\n" +
		"esac\n" + pastTheChain)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := []string{"PATH=" + os.Getenv("PATH")}
	stateDir := t.TempDir()
	sites, err := probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
	if err != nil || len(sites) != 2 || sites[0].name != "vendored emSign" || sites[0].unasked != "" {
		t.Fatalf("vendored sites = %+v, %v; want vendored emSign asked and no failure", sites, err)
	}
	if want := "its positive control was refused, where it must get past the chain to the server's signature: exit status 60: curl: (60) SSL certificate problem: certificate has expired"; sites[1].name != "vendored AAA" || sites[1].unasked != want {
		t.Errorf("vendored AAA = %+v, want unasked %q", sites[1], want)
	}
	if got, want := tlsSiteLine(sites[1]), "probe-image: UNKNOWN: the vendored AAA chain at "+tlsAAAHost+": "+sites[1].unasked; got != want {
		t.Errorf("tlsSiteLine(vendored AAA) = %q, want %q", got, want)
	}

	for _, row := range []struct{ name, script, want string }{
		{name: "a curl that accepts every chain", script: "#!/bin/sh\ncase \"$*\" in *" + tlsControlHost + "*) exit 0 ;; esac\n" + pastTheChain,
			want: "vendored emSign positive control isolation at https://" + tlsControlHost + "/ was accepted: the command returned no output"},
		{name: "a curl that cannot read its root", script: "#!/bin/sh\necho 'curl: (77) error setting certificate file: emsign-ecc-root-ca-g3.pem' >&2\nexit 77\n",
			want: "vendored emSign positive control at " + publicEmSignTLSSite.url + " was unavailable: exit status 77: curl: (77) error setting certificate file: "},
		{name: "a refusal for another cause", script: "#!/bin/sh\necho 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2\nexit 60\n",
			want: "vendored emSign positive control at " + publicEmSignTLSSite.url + " was refused: exit status 60: curl: (60) SSL certificate problem: unable to get local issuer certificate"},
	} {
		t.Run(row.name, func(t *testing.T) {
			install(row.script)
			sites, err := probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
			if want := "worker TLS probe cannot verify test chains: " + row.want; err == nil || !strings.Contains(err.Error(), want) || sites != nil {
				t.Fatalf("probeVendoredTLSSites() = %+v, %v; want no sites and the failure %q", sites, err, want)
			}
		})
	}

	// stalls is a curl that runs action where its arguments match pattern, refuses the control
	// elsewhere, and gets past every other chain to the server's signature.
	stalls := func(pattern, action string) string {
		return "#!/bin/sh\ncase \"$*\" in\n*" + pattern + "*) " + action + " ;;\n" + refusesTheControl + "esac\n" + pastTheChain
	}
	atTheChain, atTheIsolation := "emsign-ecc-root-ca-g3*"+tlsEmSignHost, "emsign-ecc-root-ca-g3*"+tlsControlHost
	previous := tlsPositiveControlSpec.timeout
	t.Cleanup(func() { tlsPositiveControlSpec.timeout = previous })
	for _, row := range []struct{ name, script, want string }{
		{name: "its limit ended the run at the chain", script: stalls(atTheChain, "exec sleep 30"),
			want: "vendored emSign positive control at " + publicEmSignTLSSite.url + " was unavailable: its 300ms limit ended it after "},
		{name: "a signal ended the run at the chain", script: stalls(atTheChain, "kill -9 $$"),
			want: "vendored emSign positive control at " + publicEmSignTLSSite.url + " was unavailable: signal: killed ended it after "},
		{name: "its limit ended the isolation", script: stalls(atTheIsolation, "exec sleep 30"),
			want: "vendored emSign positive control isolation at https://" + tlsControlHost + "/ was unavailable: its 300ms limit ended it after "},
	} {
		t.Run(row.name, func(t *testing.T) {
			install(row.script)
			tlsPositiveControlSpec.timeout = 300 * time.Millisecond
			sites, err := probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
			if want := "worker TLS probe cannot verify test chains: " + row.want; err == nil || !strings.Contains(err.Error(), want) || sites != nil {
				t.Fatalf("probeVendoredTLSSites() = %+v, %v; want no sites and the failure %q", sites, err, want)
			}
		})
	}

	t.Run("the probe's deadline ended the run at the chain", func(t *testing.T) {
		install(stalls(atTheChain, "exec sleep 30"))
		tlsPositiveControlSpec.timeout = time.Minute
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		sites, err := probeVendoredTLSSites(ctx, env, stateDir, testTLSSites(stateDir))
		var noVerdict tlsNoVerdicts
		if !errors.As(err, &noVerdict) || len(noVerdict) == 0 || !strings.HasPrefix(noVerdict[0], "vendored emSign positive control at "+publicEmSignTLSSite.url+": the probe's deadline ended it after ") {
			t.Fatalf("probeVendoredTLSSites() past the deadline = %+v, %v; want runs with no verdict, the emSign copy's first", sites, err)
		}
		if len(sites) != 2 || sites[0].unasked == "" || sites[1].unasked == "" {
			t.Errorf("vendored sites past the deadline = %+v, want both unasked: nothing verified them", sites)
		}
	})

	// The one exception: a first run a signal ended after its output showed the server's signature
	// failing got past the chain, so the isolation decides the copy, and here it is asked.
	t.Run("a signal ended the run at the chain after the server's signature failed", func(t *testing.T) {
		install(stalls(atTheChain, "echo 'curl: (35) TLS connect error: error:030000EA:digital envelope routines::provider signature failure' >&2; kill -9 $$"))
		tlsPositiveControlSpec.timeout = time.Minute
		sites, err := probeVendoredTLSSites(context.Background(), env, stateDir, testTLSSites(stateDir))
		if err != nil || len(sites) != 2 || sites[0].unasked != "" || sites[1].unasked != "" {
			t.Fatalf("probeVendoredTLSSites() = %+v, %v; want both copies asked and no failure", sites, err)
		}
	})
}

// The probe warns of a served leaf whose end is within 30 days, or past, so the chains are vendored
// again before every client moves to the public sites: a leaf planted 29 days from its end, or past
// it, draws the WARNING line and the GitHub warning command, each naming the site, the leaf, its
// end and the command that vendors the chains; one planted 31 days from its end draws none. The
// leaves the probe embeds are read as served, and draw the warning 29 days from their end and none
// 31 days from it. Each row gives the time it reads, so no verdict depends on the day it runs.
func TestTLSProbeWarnsOfAServedLeafNearItsEnd(t *testing.T) {
	const day = 24 * time.Hour
	end := time.Date(2027, 5, 3, 1, 4, 36, 0, time.UTC)
	planted := []tlsServedLeaf{{site: "vendored emSign", leaf: &x509.Certificate{Subject: pkix.Name{CommonName: tlsEmSignHost}, NotAfter: end}}}
	text := "the served vendored emSign chain's leaf, CN=" + tlsEmSignHost + ", is valid until 2027-05-03T01:04:36Z, within 30 days or past: vendor the chains again (" +
		tlsVendorCommand + ") and commit what it writes, or from then every client is held to the public site in its place"
	for _, now := range []time.Time{end.Add(-29 * day), end.Add(day)} {
		if got, want := tlsServedLeafWarnings(planted, now), []string{"probe-image: WARNING: " + text, "::warning title=TLS probe::" + text}; !slices.Equal(got, want) {
			t.Errorf("tlsServedLeafWarnings() at %s = %q, want %q", now, got, want)
		}
	}
	if got := tlsServedLeafWarnings(planted, end.Add(-31*day)); len(got) != 0 {
		t.Errorf("tlsServedLeafWarnings() 31 days before the leaf's end = %q, want none", got)
	}

	served := tlsServedLeaves(testTLSSites("")[3:5])
	if len(served) != 2 || served[0].site != "vendored emSign" || served[1].site != "vendored AAA" {
		t.Fatalf("tlsServedLeaves() = %+v, want the leaves of the vendored emSign and AAA chains", served)
	}
	for _, leaf := range served {
		if got := tlsServedLeafWarnings([]tlsServedLeaf{leaf}, leaf.leaf.NotAfter.Add(-29*day)); len(got) != 2 || !strings.Contains(got[0], "the served "+leaf.site+" chain's leaf, ") {
			t.Errorf("tlsServedLeafWarnings() 29 days before the embedded %s leaf's end = %q, want its warning", leaf.site, got)
		}
		if got := tlsServedLeafWarnings([]tlsServedLeaf{leaf}, leaf.leaf.NotAfter.Add(-31*day)); len(got) != 0 {
			t.Errorf("tlsServedLeafWarnings() 31 days before the embedded %s leaf's end = %q, want none", leaf.site, got)
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
	newVendoredTLSSites = func(ctx context.Context, env []string, stateDir string, sites []tlsSite) ([]tlsSite, error) {
		vendored, err := askedVendoredTLSSites(ctx, env, stateDir, sites)
		vendored[1].unasked = expired
		return vendored, err
	}
	newPublicTLSSites = unaskedPublicTLSSites
	newImageTLSClients = passingImageTLSClients
	var stdout bytes.Buffer
	noVerdict, err := runTLSProbe(context.Background(), "omp", nil, &stdout)
	if err != nil {
		t.Fatalf("runTLSProbe() with the public hosts unreachable = %v, want success", err)
	}
	if len(noVerdict) > 0 {
		t.Fatalf("runTLSProbe() with the public hosts unreachable has runs with no verdict %q, want none: an UNKNOWN public site takes no exit 75", noVerdict)
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

// A TLS run the probe's deadline (--probe-budget) ends has no verdict, wherever it ran and whatever
// it printed: the probe reads no output of a run it did not let finish. It fails nothing and gets
// no second run, since the deadline has passed.
func TestTLSProbeRunCutByTheProbesDeadlineHasNoVerdict(t *testing.T) {
	sites := testTLSSites(t.TempDir())[:2]
	client := tlsCommandClient{
		clientName: "curl",
		timeout:    tlsDefaultProbeTimeout,
		command: func(site tlsSite) (string, []string, []string) {
			if site.name == "emSign" {
				return "sh", []string{"-c", "echo certificate verify failed >&2; exit 1"}, nil
			}
			return "sh", []string{"-c", "exec sleep 30"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	observations, noVerdict, err := probeTLSClients(ctx, []tlsClient{client}, sites, nil)
	if len(observations) != 2 {
		t.Fatalf("probeTLSClients() returned %d observations, want one run at each site and no second run: %+v", len(observations), observations)
	}
	if got := observations[1]; got.status != tlsNoVerdict || !strings.HasPrefix(got.detail, "the probe's deadline ended it after ") {
		t.Errorf("AAA outcome = %+v, want no verdict, the probe's deadline named", got.tlsOutcome)
	}
	if err != nil {
		t.Errorf("probeTLSClients() = %v, want no failure: a run the deadline ended fails nothing", err)
	}
	if len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "curl AAA: the probe's deadline ended it after ") {
		t.Errorf("probeTLSClients() runs with no verdict = %q, want curl's AAA run alone, the deadline named", noVerdict)
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

			if outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{}); outcome.status != testCase.want {
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

			outcome := client.probe(context.Background(), controlTLSSite, tlsRunTerms{})
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

	outcome := chromium.probe(context.Background(), controlTLSSite, tlsRunTerms{})
	if outcome.status != tlsConnectionAccepted {
		t.Fatalf("Chromium probe after a 21s cold start = %+v, want accepted", outcome)
	}
}

// What node and aws printed at the TLS sites in the probe pods, which the stand-in clients below
// replay: node's refusal of a compromised chain (exit 1, on stderr), as INFRA's probe pod logged it
// on 2026-11-19 with the probe's trim; aws's refusal (exit 255, on stderr), which names the site's
// URL, and its answer at the control (exit 255), the page it could not parse, as BET's probe pod
// logged them on 2026-11-17. node exits 0 with no output at the control.
const (
	nodeTLSRefusal   = `node:internal/modules/run_main:107 triggerUncaughtException( ^ [TypeError: fetch fail…:34) at TLSSocket.emit (node:events:514:28) at TLSSocket._finishInit (node:internal/tls/wrap:1216:8) at ssl.onhandshakedone (node:internal/tls/wrap:997:12) { code: 'CERT_REVOKED' } } Node.js v24.22.1`
	awsTLSRefusal    = `aws: [ERROR]: SSL validation failed for %s [SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: unable to get local issuer certificate (_ssl.c:1082)`
	awsControlAnswer = `aws: [ERROR]: Unable to parse response (syntax error: line 1, column 0), invalid XML received. Further retries may succeed: <!doctype html><html lang="en"><head><title>Example Domain</title>`
)

// nodeAnswer is what node prints at site, and the status it exits with.
func nodeAnswer(site tlsSite) (string, int) {
	if site.kind == tlsControlSite {
		return "", 0
	}
	return nodeTLSRefusal, 1
}

// awsAnswer is what aws prints at site, and the status it exits with.
func awsAnswer(site tlsSite) (string, int) {
	if site.kind == tlsControlSite {
		return awsControlAnswer, 255
	}
	return fmt.Sprintf(awsTLSRefusal, site.url), 255
}

// The scripts the stand-in clients run (shTLSClient). sh runs each with the client's name as $0,
// the site's name as $1, a directory as $2, and what the client prints at the site and the status
// it exits with there as $3 and $4. A script that must be stopped runs exec sleep, so that the stop
// reaches the sleep itself, also where nothing stops a shell's children: the merge base's
// exec.CommandContext killed the shell alone.
const (
	// tlsAnswers prints $3, where there is any, on stderr and exits $4.
	tlsAnswers = `[ -z "$3" ] || printf '%s\n' "$3" >&2; exit "$4"`
	// tlsFirstRunSleeps sleeps past the limit on the client's first run of all, which is at
	// emSign, leaving a mark in $2, and answers on every later run.
	tlsFirstRunSleeps = `if [ ! -e "$2/$0.ran" ]; then : >"$2/$0.ran"; exec sleep 30; fi; ` + tlsAnswers
	// tlsSleepsAtEmSign sleeps past the limit on every run at emSign, and answers at every other
	// site.
	tlsSleepsAtEmSign = `[ "$1" != emSign ] || exec sleep 30; ` + tlsAnswers
)

// shTLSClient is a command client named name with a limit of one second whose run at a site is sh
// running script, as the constants above say, answer giving what it prints there and the status it
// exits with. Each run first appends "<name> <site>" to dir/runs.
func shTLSClient(name, dir, script string, answer func(tlsSite) (string, int)) tlsCommandClient {
	return tlsCommandClient{
		clientName: name,
		timeout:    time.Second,
		command: func(site tlsSite) (string, []string, []string) {
			output, status := answer(site)
			return "sh", []string{"-c", `printf '%s %s\n' "$0" "$1" >>"$2/runs"; ` + script, name, site.name, dir, output, strconv.Itoa(status)}, nil
		},
	}
}

// tlsLines are stdout's TLS lines of client at site, in the order they were printed.
func tlsLines(stdout, client, site string) []string {
	prefix := "probe-image: TLS client " + client + " " + site + ": "
	var lines []string
	for line := range strings.Lines(stdout) {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

// tlsRunsLogged are the runs the stand-in clients sharing dir logged, "<client> <site>" each, in
// the order they started.
func tlsRunsLogged(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// timedTLSClient is a client that sums the time its runs at each site took, by the site's name.
type timedTLSClient struct {
	tlsClient
	spent map[string]time.Duration
}

func (c timedTLSClient) probe(ctx context.Context, site tlsSite, terms tlsRunTerms) tlsOutcome {
	started := time.Now()
	defer func() { c.spent[site.name] += time.Since(started) }()
	return c.tlsClient.probe(ctx, site, terms)
}

// Every client of the image's inventory, of each kind (workerTLSClients: command, optional and
// Chromium clients), is slow on its first run at emSign, a site the positive controls reached: its
// own limit ends that run, which has no verdict, and its one more run refuses the chain in that
// client's own terms, so the probe passes. Each client is built as the probe builds it
// (tlsClientSpec.build), from a copy of its spec whose program is a stand-in named by an absolute
// path, so gh's and codegraph-node's own paths are replaced too, and whose limit is 300 ms. A
// client whose shell waits on a child holding the output pipe returns within its limit, the stop's
// grace and procgroup.WaitDelay, since its limit stops its whole process group. The positive
// controls' curl gets no second run: its own limit fails the probe.
func TestTLSProbeRunsEveryImageClientAgainAfterItsLimitAtAReachedSite(t *testing.T) {
	const limit = 300 * time.Millisecond
	stubs, stateDir := t.TempDir(), t.TempDir()
	// standIn writes an executable stand-in that runs firstRun on its first run of all, then exits
	// 0 at the control and prints refusal on stderr and exits 1 at every other site.
	standIn := func(name, firstRun, refusal string) string {
		path := filepath.Join(stubs, name)
		script := "#!/bin/sh\n" +
			"if [ ! -e '" + path + ".ran' ]; then : >'" + path + ".ran'; " + firstRun + "; fi\n" +
			"[ \"$LEGION_TLS_URL\" != '" + controlTLSSite.url + "' ] || exit 0\n" +
			"echo '" + refusal + "' >&2\nexit 1\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	refusalOf := func(spec tlsClientSpec) string {
		if spec.kind == tlsChromiumKind {
			return "Page load failed: net::ERR_CERT_AUTHORITY_INVALID"
		}
		return "certificate verify failed"
	}
	clients := make([]tlsClient, 0, len(workerTLSClients))
	for _, spec := range workerTLSClients {
		spec.program, spec.timeout = standIn(spec.name, "exec sleep 30", refusalOf(spec)), limit
		clients = append(clients, spec.build("omp", nil, stateDir))
	}

	observations, noVerdict, err := probeTLSClients(context.Background(), clients, servedTLSSites(), compromisedTLSSites())

	if err != nil || len(noVerdict) != 0 {
		t.Fatalf("probeTLSClients() = %v, runs with no verdict %q; want every client to pass on its second run at emSign", err, noVerdict)
	}
	for _, spec := range workerTLSClients {
		var runs []tlsOutcome
		for _, observation := range observations {
			if observation.client == spec.name && observation.site.name == emSignTLSSite.name {
				runs = append(runs, observation.tlsOutcome)
			}
		}
		if len(runs) != 2 || runs[0].status != tlsNoVerdict || !strings.HasPrefix(runs[0].detail, "its 300ms limit ended it after ") || runs[1].status != tlsCertificateRefused {
			t.Errorf("%s at emSign ran %+v, want a first run its 300ms limit ended with no verdict, then a refusal", spec.name, runs)
		}
	}

	t.Run("a shell waiting on a child that holds the output pipe", func(t *testing.T) {
		spec := workerTLSClients[0]
		spec.program, spec.timeout = standIn("held", "sleep 30 & wait", refusalOf(spec)), limit
		started := time.Now()
		outcome := spec.build("omp", nil, stateDir).probe(context.Background(), emSignTLSSite, tlsRunTerms{reached: true})
		if elapsed, bound := time.Since(started), limit+procgroup.StopGrace+procgroup.WaitDelay; outcome.status != tlsNoVerdict || elapsed >= bound {
			t.Errorf("a run whose shell waits on a child holding its output = %+v after %s, want no verdict within %s", outcome, elapsed, bound)
		}
	})

	t.Run("the positive controls get no second run", func(t *testing.T) {
		path := filepath.Join(stubs, "positive-control")
		script := "#!/bin/sh\n" +
			"printf '%s\\n' \"$LEGION_TLS_URL\" >>'" + path + ".runs'\n" +
			"if [ ! -e '" + path + ".ran' ]; then : >'" + path + ".ran'; exec sleep 30; fi\n" +
			"[ \"$LEGION_TLS_URL\" != '" + controlTLSSite.url + "' ] || { echo 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2; exit 60; }\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		previous := tlsPositiveControlSpec
		t.Cleanup(func() { tlsPositiveControlSpec = previous })
		tlsPositiveControlSpec.program, tlsPositiveControlSpec.timeout = path, limit

		err := probeTLSPositiveControls(context.Background(), nil, t.TempDir(), servedTLSSites())

		if want := "emSign positive control at " + emSignTLSSite.url + " did not accept its root: its 300ms limit ended it after "; err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("probeTLSPositiveControls() = %v, want the failure %q", err, want)
		}
		raw, readErr := os.ReadFile(path + ".runs")
		if got := strings.Count(string(raw), emSignTLSSite.url+"\n"); readErr != nil || got != 1 {
			t.Errorf("the emSign positive control ran %d times (%v), want once: %q", got, readErr, raw)
		}
	})
}

// A public host that goes down after its positive control passed reads UNKNOWN, by name with its
// host, and the probe passes with exit 0 and the OK line: an UNKNOWN public site takes none of the
// no-verdict exit (bootprobe.NoVerdictExit), so a public host that is down never fails the image
// build. That holds for a host that refuses at once (its name no longer resolves) and for one that
// hangs until the client's own limit ends the run (a blackhole): a public site is never counted
// reached, so that run is unknown rather than one with no verdict. The control is the path main's
// no-verdict exit is for: a run the probe's deadline cuts at that same public host has no verdict,
// and the probe exits 75 naming it.
func TestProbeImagePassesWhenAPublicHostGoesDownAfterItsPositiveControl(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	askedPublic := func(context.Context, []string, string) []tlsSite { return slices.Clone(publicTLSSites) }
	down := func(site tlsSite) (string, int) {
		if site.kind == tlsPublicSite {
			return "curl: (6) Could not resolve host: " + tlsSiteURL(site).Hostname(), 6
		}
		return nodeAnswer(site)
	}

	for _, tc := range []struct {
		name, script, detail string
	}{
		{name: "a name that no longer resolves", script: tlsAnswers, detail: "exit status 6: curl: (6) Could not resolve host: "},
		{name: "a host that hangs", script: `case "$1" in public*) exec sleep 30 ;; esac; ` + tlsAnswers, detail: "its 1s limit ended it after "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := shTLSClient("node", t.TempDir(), tc.script, down)
			code, stdout, stderr := probeImageAt(theseTLSClients(node), passingTLSPositiveControls, askedPublic, img.flags()...)
			if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
				t.Fatalf("probe-image with the public hosts down = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
			}
			if _, ok := bootprobe.NoVerdict(stderr); ok {
				t.Errorf("stderr %q carries the no-verdict line: a public host that is down must not take exit %d", stderr, bootprobe.NoVerdictExit)
			}
			if !strings.Contains(stdout, "probe-image: TLS public emSign: chain verified\n") {
				t.Errorf("stdout lacks the public emSign positive control's line:\n%s", stdout)
			}
			for _, host := range []string{tlsEmSignHost, tlsAAAHost} {
				if line := "probe-image: UNKNOWN: node against " + host + ": " + tc.detail; !strings.Contains(stdout, line) {
					t.Errorf("stdout lacks %q:\n%s", line, stdout)
				}
			}
		})
	}

	cut := shTLSClient("node", t.TempDir(), `[ "$1" != "public emSign" ] || exec sleep 30; `+tlsAnswers, down)
	cut.timeout = time.Minute
	code, stdout, stderr := probeImageAt(theseTLSClients(cut), passingTLSPositiveControls, askedPublic, img.flags("--probe-budget", shortProbeBudget)...)
	detail, ok := bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.Contains(detail, "node public emSign: the probe's deadline ended it after ") || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image with a run the deadline cut at a public host = %d, stdout %q, stderr %q; want exit %d naming that run", code, stdout, stderr, bootprobe.NoVerdictExit)
	}
}

// c-slow-probe-passes case (1): node, the first client, is slow on its first run at emSign, where
// only the positive control has reached the site. Its own limit ends that run with neither a
// refusal nor an answer in its output, so the run has no verdict, and its one more run, at two
// times the limit, refuses the chain: the probe passes. At the merge base the first run was
// unavailable and failed the probe ("node cannot verify refusal of the emSign chain: signal:
// killed"), as BET's and OSSV's probes failed on 2026-11-17.
func TestTLSProbePassesTheFirstClientWhoseSlowFirstRunAtEmSignRefusesOnItsSecondRun(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), tlsFirstRunSleeps, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	lines := tlsLines(stdout, "node", emSignTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client node emSign: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client node emSign: refused (") {
		t.Errorf("node's emSign lines = %q, want a first run with no verdict naming its 1s limit, then a refusal", lines)
	}
}

// c-slow-probe-passes case (2): the same for aws, a later client, at emSign after node refused the
// chain there: aws's first run, which its own limit ends, has no verdict, and its second run
// refuses the chain, so the probe passes. At the merge base it failed the probe ("aws cannot
// verify refusal of the emSign chain: signal: killed").
func TestTLSProbePassesALaterClientWhoseSlowFirstRunAtEmSignRefusesOnItsSecondRun(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	aws := shTLSClient("aws", dir, tlsFirstRunSleeps, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	code, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	if lines := tlsLines(stdout, "node", emSignTLSSite.name); len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client node emSign: refused (") {
		t.Errorf("node's emSign lines = %q, want one refusal", lines)
	}
	lines := tlsLines(stdout, "aws", emSignTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client aws emSign: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client aws emSign: refused (") {
		t.Errorf("aws's emSign lines = %q, want a first run with no verdict naming its 1s limit, then a refusal", lines)
	}
}

// c-slow-probe-passes case (3): a client whose every run at emSign, a reached site, outlasts its
// limit still has no verdict after its second run. Nothing failed, so `legion probe-image` exits
// bootprobe.NoVerdictExit with the line the daemon reads, naming the client and the site, and
// prints neither the OK line nor the failure's. The TLS run took its limit and two times its limit,
// and less than that plus what the two stops can add (StopGrace and WaitDelay each).
func TestTLSProbeExits75NamingAClientThatNeverAnswersAtAReachedSite(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), tlsSleepsAtEmSign, nodeAnswer)
	timed := timedTLSClient{tlsClient: node, spent: map[string]time.Duration{}}

	code, stdout, stderr := probeImageWith(theseTLSClients(timed), passingTLSPositiveControls, img.flags()...)

	detail, ok := bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.HasPrefix(detail, "node emSign: its 2s limit ended it after ") || strings.Contains(detail, ";") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit %d and the no-verdict line naming node's emSign run alone", code, stdout, stderr, bootprobe.NoVerdictExit)
	}
	if strings.Contains(stderr, "worker TLS probe failed") || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Errorf("probe-image printed the failure's line or the OK line: stdout %q, stderr %q", stdout, stderr)
	}
	least := 3 * node.timeout
	most := least + 2*(procgroup.StopGrace+procgroup.WaitDelay)
	if spent := timed.spent[emSignTLSSite.name]; spent < least || spent >= most {
		t.Errorf("node's emSign TLS run took %s, want at least %s and less than %s", spent, least, most)
	}
}

// c-slow-probe-passes case (4), on the probe's own server: a run its own limit ends at a site no
// earlier run reached fails the probe, since nothing shows the client could reach it. Every site
// the server serves is one the positive controls reach when they pass, the control too, which their
// isolation runs reach; so the site no run reached is one where those controls had no verdict, and
// there the first client's run at the control fails. Where they passed, that run has no verdict and
// runs once more, as at any reached site. A compromised site no run reached is the same, and an
// earlier client's refusal there makes such a run one with no verdict. The run's own limit ended it
// also where the probe's deadline passes while the limit's stop is still under way, since the run
// records which of the two fired first when it fires: such a run is unavailable too, and never one
// with no verdict.
func TestTLSProbeFailsAClientWhoseLimitPassesAtASiteNoRunReached(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), `[ "$1" != control ] || exec sleep 30; `+tlsAnswers, nodeAnswer)
	controlsCut := func(context.Context, []string, string, []tlsSite) error {
		return tlsNoVerdicts{"emSign positive control at " + emSignTLSSite.url + ": the probe's deadline ended it after 0.1s: signal: terminated"}
	}

	code, stdout, stderr := probeImageWith(theseTLSClients(node), controlsCut, img.flags()...)

	want := "network unavailable: node cannot reach " + controlTLSSite.url + ": its 1s limit ended it after "
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, want)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
	if lines := tlsLines(stdout, "node", controlTLSSite.name); len(lines) != 1 {
		t.Errorf("node's control lines = %q, want one run and no second", lines)
	}

	node = shTLSClient("node", t.TempDir(), `[ "$1" != control ] || exec sleep 30; `+tlsAnswers, nodeAnswer)
	code, stdout, stderr = probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)
	detail, ok := bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.HasPrefix(detail, "node control: its 2s limit ended it after ") || strings.Contains(stderr, "worker TLS probe failed") {
		t.Fatalf("probe-image with the controls passed = %d, stdout %q, stderr %q; want exit %d naming node's control run alone, its second run's 2s limit", code, stdout, stderr, bootprobe.NoVerdictExit)
	}
	if lines := tlsLines(stdout, "node", controlTLSSite.name); len(lines) != 2 {
		t.Errorf("node's control lines with the controls passed = %q, want a run and its second run", lines)
	}

	// A served copy of a public chain whose own positive control passed is reached too: aws, which
	// is held to the copies, hangs at the emSign copy, and that run has no verdict, not a failure.
	vendoredHang := shTLSClient("aws", t.TempDir(), `[ "$1" != "vendored emSign" ] || exec sleep 30; `+tlsAnswers, awsAnswer)
	vendoredHang.applicationSeen = awsResponseSeen
	code, stdout, stderr = probeImageWith(theseTLSClients(vendoredHang), passingTLSPositiveControls, img.flags()...)
	detail, ok = bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.HasPrefix(detail, "aws vendored emSign: its 2s limit ended it after ") || strings.Contains(stderr, "worker TLS probe failed") {
		t.Fatalf("probe-image with aws hanging at the emSign copy = %d, stdout %q, stderr %q; want exit %d naming that run alone", code, stdout, stderr, bootprobe.NoVerdictExit)
	}

	dir := t.TempDir()
	aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
	aws.applicationSeen = awsResponseSeen
	if _, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{aws}, servedTLSSites(), nil); err == nil || !strings.Contains(err.Error(), "aws cannot verify refusal of the emSign chain: its 1s limit ended it after ") || len(noVerdict) != 0 {
		t.Errorf("probeTLSClients() with emSign unreached = %v, runs with no verdict %q; want aws's emSign run to fail", err, noVerdict)
	}
	node = shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	if _, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{node, aws}, servedTLSSites(), nil); err != nil || len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "aws emSign: its 2s limit ended it after ") {
		t.Errorf("probeTLSClients() with emSign reached by node's refusal = %v, runs with no verdict %q; want aws's emSign run alone with no verdict", err, noVerdict)
	}

	// A command at the control that answers its limit's SIGTERM, at 1 s, with an exit of its own
	// 0.6 s later, inside procgroup.StopGrace, while the probe's deadline passes at 1.3 s, 0.3 s
	// after the limit and 0.3 s before that exit.
	stopped := shTLSClient("node", t.TempDir(), `trap '/bin/sleep 0.6; exit 1' TERM; /bin/sleep 30 & wait`, nodeAnswer)
	ctx, cancel := context.WithTimeout(context.Background(), 1300*time.Millisecond)
	defer cancel()
	outcome := stopped.probe(ctx, controlTLSSite, tlsRunTerms{})
	if ctx.Err() == nil {
		t.Fatalf("the run returned %+v before the probe's deadline; the row needs the deadline to pass while the limit's stop is under way", outcome)
	}
	if outcome.status != tlsNetworkUnavailable || !strings.HasPrefix(outcome.detail, "its 1s limit ended it after ") || !strings.HasSuffix(outcome.detail, ": exit status 1") {
		t.Errorf("a run its 1s limit stopped, which exited after the probe's deadline = %+v, want unavailable naming its 1s limit", outcome)
	}
}

// c-slow-probe-passes case (5): the positive controls keep their rule, so a control its own limit
// ends fails the probe at once, with no second run: a pod with no egress fails here.
func TestTLSProbeFailsAPositiveControlThatPassesItsLimit(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	runs := filepath.Join(t.TempDir(), "runs")
	img.install(t, "curl", "#!/bin/sh\nprintf '%s\\n' \"$LEGION_TLS_URL\" >>'"+runs+"'\nexec sleep 30\n")
	previous := tlsPositiveControlSpec.timeout
	t.Cleanup(func() { tlsPositiveControlSpec.timeout = previous })
	tlsPositiveControlSpec.timeout = time.Second

	code, stdout, stderr := probeImageWith(passingImageTLSClients, probeTLSPositiveControls, img.flags()...)

	want := "worker TLS probe cannot verify test chains: emSign positive control at " + emSignTLSSite.url + " did not accept its root: its 1s limit ended it after "
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) || strings.Contains(stdout, "probe-image: TLS client ") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q before any client runs", code, stdout, stderr, want)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
	raw, err := os.ReadFile(runs)
	if got := strings.Count(string(raw), emSignTLSSite.url+"\n"); err != nil || got != 1 {
		t.Errorf("the emSign positive control ran %d times (%v), want once: %q", got, err, raw)
	}
}

// The probe holds the served copies' positive controls to their verdict: a failure there fails the
// probe at once, exit 1 naming it, before any client runs, and a run of theirs with no verdict
// takes the no-verdict exit (bootprobe.NoVerdictExit) where nothing fails, naming that run. A
// positive control's curl that cannot read its root, as when an image lacked the roots the probe
// once read from it, fails the probe through the served copies' own controls, exit 1, rather than
// marking both copies unasked and passing.
func TestTLSProbeTakesTheServedCopiesPositiveControlsVerdict(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	const failed = "worker TLS probe cannot verify test chains: vendored emSign positive control at https://" + tlsEmSignHost + "/ was unavailable: its 20s limit ended it after 20.0s: signal: terminated"
	failing := func(context.Context, []string, string, []tlsSite) ([]tlsSite, error) { return nil, errors.New(failed) }
	code, stdout, stderr := probeImageWithSites(passingImageTLSClients, passingTLSPositiveControls, failing, unaskedPublicTLSSites, img.flags()...)
	if code != 1 || !strings.Contains(stderr, failed) || strings.Contains(stdout, bootprobe.OKPrefix) || strings.Contains(stdout, "probe-image: TLS client ") {
		t.Fatalf("probe-image with a served copy's control failed = %d, stdout %q, stderr %q; want exit 1 naming %q before any client runs", code, stdout, stderr, failed)
	}

	const cut = "vendored emSign positive control at https://" + tlsEmSignHost + "/: the probe's deadline ended it after 0.1s: signal: terminated"
	noVerdict := func(ctx context.Context, env []string, stateDir string, sites []tlsSite) ([]tlsSite, error) {
		vendored, _ := askedVendoredTLSSites(ctx, env, stateDir, sites)
		vendored[0].unasked = "its positive control was no verdict"
		return vendored, tlsNoVerdicts{cut}
	}
	code, stdout, stderr = probeImageWithSites(passingImageTLSClients, passingTLSPositiveControls, noVerdict, unaskedPublicTLSSites, img.flags()...)
	detail, ok := bootprobe.NoVerdict(stderr)
	if code != bootprobe.NoVerdictExit || !ok || !strings.Contains(detail, cut) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image with a served copy's control cut by the deadline = %d, stdout %q, stderr %q; want exit %d naming %q", code, stdout, stderr, bootprobe.NoVerdictExit, cut)
	}

	img.install(t, "curl", "#!/bin/sh\necho 'curl: (77) error setting certificate file: '\"$3\" >&2\nexit 77\n")
	code, stdout, stderr = probeImageWithSites(passingImageTLSClients, passingTLSPositiveControls, probeVendoredTLSSites, unaskedPublicTLSSites, img.flags()...)
	if want := "worker TLS probe cannot verify test chains: vendored emSign positive control at https://" + tlsEmSignHost + "/ was unavailable: exit status 77: curl: (77) error setting certificate file: "; code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image with a curl that cannot read its root = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, want)
	}
}

// c-slow-probe-passes case (7): a failure fails the probe beside a TLS run with no verdict, and
// decides it: a client that accepts the emSign chain on its second run, one that exits at emSign
// with no certificate error before its limit, one a signal kills before its limit, one a signal
// kills before its limit after it printed a certificate refusal, which the signal leaves
// unavailable all the same, and one that prints `signal: killed` itself and exits 1, which is read
// from the run's own record as an exit and never as a signal or a limit. Beside each, bun's runs at
// emSign have no verdict, and no run exits 75.
func TestTLSProbeFailsWhenAFailureStandsBesideARunWithNoVerdict(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	nodeAtEmSign := func(output string, status int) func(tlsSite) (string, int) {
		return func(site tlsSite) (string, int) {
			if site.name == emSignTLSSite.name {
				return output, status
			}
			return nodeAnswer(site)
		}
	}
	for _, tc := range []struct {
		name, client, script string
		answer               func(tlsSite) (string, int)
		want                 string
		line                 *regexp.Regexp
	}{
		{name: "accepts emSign", client: "aws", script: tlsFirstRunSleeps,
			answer: func(tlsSite) (string, int) { return awsControlAnswer, 255 },
			want:   "aws accepts the emSign chain: "},
		{name: "exits at emSign with no certificate error", client: "node", script: tlsAnswers,
			answer: nodeAtEmSign("TypeError: fetch failed [cause]: read ECONNRESET", 1),
			want:   "node cannot verify refusal of the emSign chain: exit status 1: TypeError: fetch failed"},
		{name: "killed by a signal", client: "node", script: `[ "$1" != emSign ] || kill -9 $$; ` + tlsAnswers, answer: nodeAnswer,
			want: "node cannot verify refusal of the emSign chain: signal: killed ended it after ",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(signal: killed ended it after [0-9]+\.[0-9]s, before its 1s limit: signal: killed\)$`)},
		{name: "killed by a signal after it printed a certificate refusal", client: "node", script: `if [ "$1" = emSign ]; then printf '%s\n' "$3" >&2; kill -9 $$; fi; ` + tlsAnswers, answer: nodeAnswer,
			want: "node cannot verify refusal of the emSign chain: signal: killed ended it after ",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(signal: killed ended it after [0-9]+\.[0-9]s, before its 1s limit: signal: killed: node:internal/.*CERT_REVOKED.*\)$`)},
		{name: "prints signal: killed itself and exits 1", client: "node", script: tlsAnswers, answer: nodeAtEmSign("signal: killed", 1),
			want: "node cannot verify refusal of the emSign chain: exit status 1: signal: killed",
			line: regexp.MustCompile(`^probe-image: TLS client node emSign: unavailable \(exit status 1: signal: killed\)$`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bun := shTLSClient("bun", dir, tlsSleepsAtEmSign, nodeAnswer)
			failing := shTLSClient(tc.client, dir, tc.script, tc.answer)
			if tc.client == "aws" {
				failing.applicationSeen = awsResponseSeen
			}

			code, stdout, stderr := probeImageWith(theseTLSClients(bun, failing), passingTLSPositiveControls, img.flags()...)

			if code != 1 || !strings.Contains(stderr, "legion probe-image: worker TLS probe failed: ") || !strings.Contains(stderr, tc.want) || strings.Contains(stdout, bootprobe.OKPrefix) {
				t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, tc.want)
			}
			if detail, ok := bootprobe.NoVerdict(stderr); ok {
				t.Errorf("stderr carries the no-verdict line %q beside the failure", detail)
			}
			if lines := tlsLines(stdout, "bun", emSignTLSSite.name); len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client bun emSign: no verdict (") || !strings.HasPrefix(lines[1], "probe-image: TLS client bun emSign: no verdict (") {
				t.Errorf("bun's emSign lines = %q, want two runs with no verdict", lines)
			}
			if tc.line != nil && !slices.ContainsFunc(tlsLines(stdout, tc.client, emSignTLSSite.name), tc.line.MatchString) {
				t.Errorf("%s's emSign lines = %q, want one matching %s", tc.client, tlsLines(stdout, tc.client, emSignTLSSite.name), tc.line)
			}
		})
	}
}

// A run a signal ended before its limit that had already printed an answer of the site's accepted
// the chain, whatever ended it: at a public site, where a run with no verdict is unknown and fails
// nothing, that acceptance fails the probe by name, as the same output with an ordinary exit does,
// and as it does at a fixture chain. At the control the same run is the acceptance the control
// needs. A run a signal ended with no answer in its output is unavailable all the same
// (TestTLSProbeFailsWhenAFailureStandsBesideARunWithNoVerdict).
func TestTLSProbeFailsAnAcceptanceASignalEndedAtAPublicSite(t *testing.T) {
	client := func(script string) tlsCommandClient {
		return tlsCommandClient{clientName: "node", timeout: tlsDefaultProbeTimeout, applicationSeen: tlsFetchAnswer.MatchString,
			command: func(tlsSite) (string, []string, []string) { return "sh", []string{"-c", script}, nil }}
	}
	signalled := client(`printf 'Error: HTTP 503\n' >&2; kill -9 $$`)
	exited := client(`printf 'Error: HTTP 503\n' >&2; exit 1`)
	for _, row := range []struct {
		name   string
		client tlsCommandClient
		site   tlsSite
		want   string
	}{
		{name: "a signal after the answer, at a public site", client: signalled, site: publicEmSignTLSSite, want: "node accepts the public emSign chain: signal: killed ended it after "},
		{name: "an exit after the answer, at a public site", client: exited, site: publicEmSignTLSSite, want: "node accepts the public emSign chain: exit status 1: Error: HTTP 503"},
		{name: "a signal after the answer, at a fixture chain", client: signalled, site: emSignTLSSite, want: "node accepts the emSign chain: signal: killed ended it after "},
		{name: "a signal after the answer, at the control", client: signalled, site: controlTLSSite},
	} {
		t.Run(row.name, func(t *testing.T) {
			observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{row.client}, []tlsSite{row.site}, nil)
			if len(observations) != 1 || observations[0].status != tlsConnectionAccepted || len(noVerdict) != 0 {
				t.Fatalf("probeTLSClients() observed %+v, runs with no verdict %q; want one acceptance", observations, noVerdict)
			}
			if row.want == "" && err != nil {
				t.Fatalf("probeTLSClients() = %v, want success", err)
			}
			if row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)) {
				t.Fatalf("probeTLSClients() = %v, want a failure naming %q", err, row.want)
			}
		})
	}
}

// Chromium's answer is the page it dumps (--dump-dom, on stdout), which it prints only once the
// page has loaded (chromiumDOMDumped): a run that dumped the page and was then ended by a signal
// accepted the chain, so at a public site it fails the probe by name, as at a fixture chain, and at
// the control it is the acceptance the control needs. The stand-in prints the DOM Chromium dumped
// for the compromised sites' page. A run a signal ended after a refused navigation, which dumps
// nothing, is unavailable, so unknown at a public site.
func TestTLSProbeFailsAChromiumAcceptanceASignalEndedAtAPublicSite(t *testing.T) {
	const dom = `<html><head><meta name="color-scheme" content="light dark"></head><body><pre style="word-wrap: break-word; white-space: pre-wrap;">Your client successfully connected to the site. That's bad!` + "\n" + `</pre></body></html>`
	chromium := func(script string) tlsClient {
		return tlsChromiumClient{tlsCommandClient: tlsCommandClient{clientName: "chromium", timeout: tlsChromiumProbeTimeout, applicationSeen: chromiumHTTPAnswer,
			command: func(tlsSite) (string, []string, []string) { return "sh", []string{"-c", script, "sh", dom}, nil }}}
	}
	dumpedThenKilled := chromium(`printf '%s' "$1"; kill -9 $$`)
	refusedThenKilled := chromium(`echo 'Page load failed: net::ERR_CERT_AUTHORITY_INVALID' >&2; kill -9 $$`)
	for _, row := range []struct {
		name   string
		client tlsClient
		site   tlsSite
		status tlsStatus
		want   string
	}{
		{name: "a signal after the dumped page, at a public site", client: dumpedThenKilled, site: publicEmSignTLSSite, status: tlsConnectionAccepted, want: "chromium accepts the public emSign chain: signal: killed ended it after "},
		{name: "a signal after the dumped page, at a fixture chain", client: dumpedThenKilled, site: emSignTLSSite, status: tlsConnectionAccepted, want: "chromium accepts the emSign chain: signal: killed ended it after "},
		{name: "a signal after the dumped page, at the control", client: dumpedThenKilled, site: controlTLSSite, status: tlsConnectionAccepted},
		{name: "a signal after a refused navigation, at a public site", client: refusedThenKilled, site: publicEmSignTLSSite, status: tlsPublicUnknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{row.client}, []tlsSite{row.site}, nil)
			if len(observations) != 1 || observations[0].status != row.status || len(noVerdict) != 0 {
				t.Fatalf("probeTLSClients() observed %+v, runs with no verdict %q; want one run, %s", observations, noVerdict, row.status)
			}
			if row.want == "" && err != nil {
				t.Fatalf("probeTLSClients() = %v, want success", err)
			}
			if row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)) {
				t.Fatalf("probeTLSClients() = %v, want a failure naming %q", err, row.want)
			}
		})
	}
}

// A client that got the public site's own answer, a 200, accepted its chain, and one whose output
// says so before the run ends fails the probe at that site whatever then ends the run: node,
// CodeGraph's node, bun and omp write the status of every answer first (tlsFetchProgram), and wget
// names the page it saved. Each stand-in, built from its client's spec, prints its client's line for
// a 200 and is then killed; the same client killed before it prints anything is unknown there and
// fails nothing. wget's line has the shape it printed at the control (measured on 2026-11-20 in the
// image of 1c7c825367); Chromium's row is TestTLSProbeFailsAChromiumAcceptanceASignalEndedAtAPublicSite.
func TestTLSProbeFailsAnAnswered200ASignalEndedAtAPublicSite(t *testing.T) {
	answered := map[string]string{
		"node":           "legion-tls-probe: answered 200",
		"codegraph-node": "legion-tls-probe: answered 200",
		"bun":            "legion-tls-probe: answered 200",
		"omp":            "legion-tls-probe: answered 200",
		"wget":           `2026-11-20 22:53:32 URL:https://` + tlsEmSignHost + `/ [60/60] -> "/dev/null" [1]`,
	}
	clients := imageTLSClientsOf[tlsCommandClient](t, nil, t.TempDir(), slices.Sorted(maps.Keys(answered))...)
	for _, name := range slices.Sorted(maps.Keys(answered)) {
		for _, row := range []struct {
			label, script string
			status        tlsStatus
		}{
			{label: "answered 200, then killed", script: `printf '%s\n' "$1" >&2; kill -9 $$`, status: tlsConnectionAccepted},
			{label: "killed before any answer", script: `kill -9 $$`, status: tlsPublicUnknown},
		} {
			t.Run(name+" "+row.label, func(t *testing.T) {
				client := clients[name]
				client.command = func(tlsSite) (string, []string, []string) {
					return "sh", []string{"-c", row.script, "sh", answered[name]}, nil
				}
				observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{client}, []tlsSite{publicEmSignTLSSite}, nil)
				if len(observations) != 1 || observations[0].status != row.status || len(noVerdict) != 0 {
					t.Fatalf("probeTLSClients() observed %+v, runs with no verdict %q; want one run, %s", observations, noVerdict, row.status)
				}
				if want := name + " accepts the public emSign chain: signal: killed ended it after "; row.status == tlsConnectionAccepted && (err == nil || !strings.Contains(err.Error(), want)) {
					t.Fatalf("probeTLSClients() = %v, want a failure naming %q", err, want)
				}
				if row.status == tlsPublicUnknown && err != nil {
					t.Fatalf("probeTLSClients() = %v, want no failure", err)
				}
			})
		}
	}
}

// c-slow-probe-passes case (8): a run its own limit ends whose output already shows a certificate
// error is a refusal, decided by that output: its line says refused and names the limit, and it
// gets no second run.
func TestTLSProbeReadsAKilledRunThatPrintedACertificateErrorAsARefusal(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	node := shTLSClient("node", t.TempDir(), `if [ "$1" != control ]; then printf '%s\n' "$3" >&2; exec sleep 30; fi; `+tlsAnswers, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	for _, site := range compromisedTLSSites() {
		lines := tlsLines(stdout, "node", site.name)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client node "+site.name+": refused (its 1s limit ended it after ") || !strings.Contains(lines[0], "CERT_REVOKED") {
			t.Errorf("node's %s lines = %q, want one refusal naming its 1s limit and the certificate error", site.name, lines)
		}
	}
}

// The state table's row of a run its own limit ends whose output already shows an answer: node,
// first, reaches the control, and aws, whose control run prints the page it could not parse and is
// then slow, is accepted there, decided by that output: its line names the limit, and it gets no
// second run. node cannot carry this row: its answer at the control is an exit 0 with no output,
// and its client reads no answer marker, so a killed node run shows no answer.
func TestTLSProbeReadsAKilledRunThatPrintedAnAnswerAsAccepted(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	aws := shTLSClient("aws", dir, `if [ "$1" = control ]; then printf '%s\n' "$3" >&2; exec sleep 30; fi; `+tlsAnswers, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	code, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	if lines := tlsLines(stdout, "aws", controlTLSSite.name); len(lines) != 1 || !strings.HasPrefix(lines[0], "probe-image: TLS client aws control: accepted (its 1s limit ended it after ") || !strings.Contains(lines[0], "invalid XML received") {
		t.Errorf("aws's control lines = %q, want one acceptance naming its 1s limit and the page", lines)
	}
}

// c-slow-probe-passes case (9): the TLS line of a run its own limit ended says so, with the limit,
// the second run's two times the first, and how long the run took; the line of a run a signal
// ended before its limit names the signal; a run that exited on its own keeps the detail it always
// had. Each detail then carries what the run printed and its error.
func TestTLSProbeLineNamesWhatEndedARunAndAfterHowLong(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsSleepsAtEmSign, nodeAnswer)
	aws := shTLSClient("aws", dir, `[ "$1" != emSign ] || kill -9 $$; `+tlsAnswers, awsAnswer)
	aws.applicationSeen = awsResponseSeen

	_, stdout, stderr := probeImageWith(theseTLSClients(node, aws), passingTLSPositiveControls, img.flags()...)

	seconds := func(line string, form *regexp.Regexp) (float64, bool) {
		match := form.FindStringSubmatch(line)
		if match == nil {
			return 0, false
		}
		elapsed, err := strconv.ParseFloat(match[1], 64)
		return elapsed, err == nil
	}
	bound := (procgroup.StopGrace + procgroup.WaitDelay).Seconds()
	emSign := tlsLines(stdout, "node", emSignTLSSite.name)
	if len(emSign) != 2 {
		t.Fatalf("node's emSign lines = %q, want two; stdout %q, stderr %q", emSign, stdout, stderr)
	}
	for i, limit := range []float64{1, 2} {
		form := regexp.MustCompile(`^probe-image: TLS client node emSign: no verdict \(its ` + strconv.FormatFloat(limit, 'f', -1, 64) + `s limit ended it after ([0-9]+\.[0-9])s: signal: terminated\)$`)
		if elapsed, ok := seconds(emSign[i], form); !ok || elapsed < limit || elapsed >= limit+bound {
			t.Errorf("node's emSign line %d = %q, want it to name its %gs limit and a time from %gs to less than %gs", i+1, emSign[i], limit, limit, limit+bound)
		}
	}
	signal := regexp.MustCompile(`^probe-image: TLS client aws emSign: unavailable \(signal: killed ended it after ([0-9]+\.[0-9])s, before its 1s limit: signal: killed\)$`)
	if lines := tlsLines(stdout, "aws", emSignTLSSite.name); len(lines) != 1 {
		t.Errorf("aws's emSign lines = %q, want one", lines)
	} else if elapsed, ok := seconds(lines[0], signal); !ok || elapsed >= 1 {
		t.Errorf("aws's emSign line = %q, want it to name the signal and a time under its 1s limit", lines[0])
	}
	if lines := tlsLines(stdout, "node", aaaTLSSite.name); len(lines) != 1 || lines[0] != "probe-image: TLS client node AAA: refused ("+trimTLSDetail("exit status 1: "+nodeTLSRefusal)+")" {
		t.Errorf("node's AAA lines = %q, want the refusal's detail as a run that exited has always had it", lines)
	}
}

// One more run comes after every client's first run at every site, in the same order, with two
// times the client's limit: the runs' log shows aws's second run at emSign after the last client's
// control run, and its line names a 2s limit where the first named 1s. A run the probe's deadline
// ended gets no second run, nor does any run after it, which never starts.
func TestTLSProbeRunsARunWithNoVerdictAgainAfterEveryOtherRunAtTwiceItsLimit(t *testing.T) {
	dir := t.TempDir()
	aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
	aws.applicationSeen = awsResponseSeen
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)

	observations, noVerdict, err := probeTLSClients(context.Background(), []tlsClient{aws, node}, servedTLSSites(), compromisedTLSSites())

	want := []string{"aws emSign", "aws AAA", "aws control", "node emSign", "node AAA", "node control", "aws emSign"}
	if got := tlsRunsLogged(t, dir); !slices.Equal(got, want) {
		t.Errorf("the clients ran %q, want %q", got, want)
	}
	if len(observations) != len(want) {
		t.Fatalf("probeTLSClients() observed %d runs, want %d: %+v", len(observations), len(want), observations)
	}
	first, second := observations[0], observations[len(observations)-1]
	if first.client != "aws" || first.site.name != emSignTLSSite.name || first.status != tlsNoVerdict || !strings.HasPrefix(first.detail, "its 1s limit ended it after ") {
		t.Errorf("first observation = %+v, want aws's emSign run with no verdict, its 1s limit named", first)
	}
	if second.client != "aws" || second.site.name != emSignTLSSite.name || second.status != tlsNoVerdict || !strings.HasPrefix(second.detail, "its 2s limit ended it after ") {
		t.Errorf("last observation = %+v, want aws's second emSign run with no verdict, its 2s limit named", second)
	}
	if err != nil || len(noVerdict) != 1 || !strings.HasPrefix(noVerdict[0], "aws emSign: its 2s limit ended it after ") {
		t.Errorf("probeTLSClients() = %v, runs with no verdict %q; want no failure and aws's emSign run, as its second run left it", err, noVerdict)
	}

	t.Run("a run the probe's deadline ended", func(t *testing.T) {
		dir := t.TempDir()
		aws := shTLSClient("aws", dir, tlsSleepsAtEmSign, awsAnswer)
		node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		observations, noVerdict, err := probeTLSClients(ctx, []tlsClient{aws, node}, servedTLSSites(), compromisedTLSSites())

		if got := tlsRunsLogged(t, dir); !slices.Equal(got, []string{"aws emSign"}) {
			t.Errorf("the clients ran %q, want aws's emSign run alone", got)
		}
		if len(observations) != 2*len(servedTLSSites()) || len(noVerdict) != len(observations) || err != nil {
			t.Fatalf("probeTLSClients() = %v, %d observations, runs with no verdict %q; want one run of each client at each site, each with no verdict", err, len(observations), noVerdict)
		}
		for _, observation := range observations {
			if observation.status != tlsNoVerdict || !strings.HasPrefix(observation.detail, "the probe's deadline ended it after ") {
				t.Errorf("%s at %s = %+v, want no verdict, the probe's deadline named", observation.client, observation.site.name, observation.tlsOutcome)
			}
		}
	})
}

// A failure seen before the probe's deadline stands, also inside a step that returns after it: the
// emSign positive control is refused at once, and the AAA control, slow, is cut by the deadline,
// which leaves that run with no verdict and the emSign failure deciding the probe.
func TestTLSProbeKeepsAFailureSeenBeforeTheDeadline(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	img.install(t, "curl", "#!/bin/sh\n[ \"$LEGION_TLS_URL\" != '"+aaaTLSSite.url+"' ] || exec sleep 30\necho 'curl: (60) SSL certificate problem: unable to get local issuer certificate' >&2\nexit 60\n")
	previous := tlsPositiveControlSpec.timeout
	t.Cleanup(func() { tlsPositiveControlSpec.timeout = previous })
	tlsPositiveControlSpec.timeout = time.Minute

	code, stdout, stderr := probeImageWith(passingImageTLSClients, probeTLSPositiveControls, img.flags("--probe-budget", shortProbeBudget)...)

	want := "emSign positive control at " + emSignTLSSite.url + " did not accept its root: exit status 60: curl: (60) SSL certificate problem"
	if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, bootprobe.OKPrefix) {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 1 naming %q", code, stdout, stderr, want)
	}
	if strings.Contains(stderr, "AAA positive control at "+aaaTLSSite.url+" did not accept") {
		t.Errorf("stderr %q names the AAA control the deadline cut as a failure", stderr)
	}
	if _, ok := bootprobe.NoVerdict(stderr); ok {
		t.Errorf("stderr %q carries the no-verdict line beside the failure", stderr)
	}
}

// The third way a site is reached: node, first, is accepted at the control, so bun's first control
// run, which its own limit ends with no answer in its output, has no verdict where it would fail
// with no earlier run there, and bun's second run, which answers, is accepted.
func TestTLSProbeRunsAgainALaterClientCutByItsLimitAtAControlAnEarlierClientReached(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	dir := t.TempDir()
	node := shTLSClient("node", dir, tlsAnswers, nodeAnswer)
	bun := shTLSClient("bun", dir, `if [ "$1" = control ] && [ ! -e "$2/$0.ran" ]; then : >"$2/$0.ran"; exec sleep 30; fi; `+tlsAnswers, nodeAnswer)

	code, stdout, stderr := probeImageWith(theseTLSClients(node, bun), passingTLSPositiveControls, img.flags()...)

	if code != 0 || !strings.Contains(stdout, "\n"+bootprobe.OKPrefix+" (") {
		t.Fatalf("probe-image = %d, stdout %q, stderr %q; want exit 0 and the OK line", code, stdout, stderr)
	}
	lines := tlsLines(stdout, "bun", controlTLSSite.name)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "probe-image: TLS client bun control: no verdict (its 1s limit ended it after ") || !strings.HasPrefix(lines[1], "probe-image: TLS client bun control: accepted (") {
		t.Errorf("bun's control lines = %q, want a first run with no verdict naming its 1s limit, then an acceptance", lines)
	}
}
