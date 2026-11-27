// packages/envoy/internal/cipher/proxy/proxy_test.go
//
// Every row here drives the real proxy: a real listener on loopback, a real HTTP client that
// points its proxy at it, real TLS in both directions, a stand-in service that records what it
// was sent, and a fake GrantSource standing in for the two reads of requests.Machine. Nothing is
// mocked at the HTTP layer, because what each row is about - what a command's own client sends,
// what the proxy swaps, what the service receives, what the command reads back - is exactly what
// a mock would have to decide for itself.
//
// The two seams a row drives are the ones the production code has: Options.Dial, which is where
// every upstream connection is made, and Server.resolve, which answers the addresses a name has.
// Together they let a public name reach a stand-in service on loopback without the loopback
// refusal standing in the way, and let a row say which addresses a name has when that refusal is
// what it is about.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// TestACredentialThatOpensNothingIsRefusedAndTunnelsNothing: the proxy's password is a
// placeholder of a live grant, and every other credential - none, one that is not Basic, one
// that is not base64, one no row holds, one whose grant has ended, one whose names are all
// withheld - is 407 with nothing dialled. A proxy that tunnelled first and checked afterwards
// would be an open relay for anyone who can reach the pod.
func TestACredentialThatOpensNothingIsRefusedAndTunnelsNothing(t *testing.T) {
	r := newRig(t, Options{})
	ended := placeholder.Prefix + "endedendedendedendedendedended01"
	noNames := placeholder.Prefix + "nonamesnonamesnonamesnonames0001"
	r.grants.refusals[ended] = requests.ErrGrantNotLive
	r.grants.refusals[noNames] = requests.ErrAirlockNoNames

	for _, row := range []struct{ name, header, want string }{
		{"no credential at all", "", "carries none"},
		{"a credential that is not Basic", "Proxy-Authorization: Bearer " + workerPlaceholder + "\r\n", "not Basic"},
		{"a Bas" + "ic credential that is not base64", "Proxy-Authorization: Bas" + "ic not-base-64-at-all\r\n", "not base64"},
		{"a password no grant holds", basic(placeholder.Prefix + "unknownunknownunknownunknown001"), requests.ErrGrantNotLive.Error()},
		{"an empty password", basic(""), requests.ErrGrantNotLive.Error()},
		{"a grant that has ended", basic(ended), requests.ErrGrantNotLive.Error()},
		{"a grant holding no name this session may have", basic(noNames), requests.ErrAirlockNoNames.Error()},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), row.header)
			body := bodyOf(t, answer)
			if answer.StatusCode != http.StatusProxyAuthRequired {
				t.Fatalf("CONNECT with %s = %d %q; want 407: a connection whose credential opens nothing must open nothing", row.name, answer.StatusCode, body)
			}
			if got := answer.Header.Get("Proxy-Authenticate"); got != `Basic realm="airlock"` {
				t.Errorf("Proxy-Authenticate = %q; want Basic realm=\"airlock\", which is how a client knows to send its placeholder", got)
			}
			if !strings.Contains(body, row.want) {
				t.Errorf("the refusal reads %q; want it to name %q, the broker's own reason", body, row.want)
			}
			if strings.Contains(body, workerPlaceholder) || strings.Contains(body, ended) || strings.Contains(body, noNames) {
				t.Errorf("the refusal names a placeholder: %q", body)
			}
			if after := r.dials.Load(); after != before {
				t.Fatalf("the proxy dialled %d time(s) for a credential it refused; want none tunnelled", after-before)
			}
		})
	}
}

// TestABoundHostGetsTheValueSwappedIntoTheHeaderTheEntryNames is the airlock itself: the command
// holds a placeholder, the service receives the value, and what the command sent in any other
// place is sent on as it was. A placeholder anywhere but the header its entry names is refused
// (TestARequestThatStillCarriesAPlaceholderAfterTheSwapIsRefused); the body is not read, so a
// placeholder there travels as the command wrote it.
func TestABoundHostGetsTheValueSwappedIntoTheHeaderTheEntryNames(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things?token=query-text", "the body holds "+workerPlaceholder,
		"Authorization", "Bearer "+workerPlaceholder,
		"X-Other", "carried: other-text")
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call answered %d %q; want 200 from the service", answer.StatusCode, body)
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want exactly one", len(seen))
	}
	got := seen[0]
	if want := "Bearer " + workerValue; got.Header.Get("Authorization") != want {
		t.Errorf("the service saw Authorization %q; want %q: the entry names that header, so the value goes in it", got.Header.Get("Authorization"), want)
	}
	// Everything the entry does not name travels as the command wrote it: the airlock swaps the
	// headers of the entry and nothing else.
	if want := "carried: other-text"; got.Header.Get("X-Other") != want {
		t.Errorf("the service saw X-Other %q; want %q: a header the entry does not name is not swapped", got.Header.Get("X-Other"), want)
	}
	if want := "token=query-text"; got.Query != want {
		t.Errorf("the service saw the query %q; want %q: the airlock changes no query", got.Query, want)
	}
	if want := "the body holds " + workerPlaceholder; got.Body != want {
		t.Errorf("the service saw the body %q; want %q: the airlock reads no body and changes none", got.Body, want)
	}
	if got.Host != boundHost {
		t.Errorf("the service saw Host %q; want %q, the CONNECT target's host", got.Host, boundHost)
	}
	if got.Method != http.MethodPost || got.Path != "/v1/things" {
		t.Errorf("the service saw %s %s; want POST /v1/things", got.Method, got.Path)
	}
}

// TestARequestWhoseHostIsNotTheConnectTargetIsRefused: the CONNECT says where this connection
// goes and the Host header says which service is being addressed, so a request that disagrees is
// a request trying to carry the value somewhere the rules were never read for.
func TestARequestWhoseHostIsNotTheConnectTargetIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things",
		"Authorization", "Bearer "+workerPlaceholder,
		"Host", "elsewhere.example.com")
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "host header mismatch") {
		t.Fatalf("the call answered %d %q; want 403 naming the host header mismatch", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none: a refused request reaches no service", len(seen))
	}
}

// TestAMethodOrPathTheEntryDoesNotAllowIsRefused: the entry's allow list is a bound on where the
// value may go, and the proxy is where it is enforced.
func TestAMethodOrPathTheEntryDoesNotAllowIsRefused(t *testing.T) {
	for _, row := range []struct{ name, method, path string }{
		{"a method the entry does not allow", http.MethodDelete, "/v1/things"},
		{"a path the entry does not allow", http.MethodGet, "/admin/things"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			client := r.client(workerPlaceholder)
			answer, body := r.do(client, row.method, "https://"+boundHost+row.path, "", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "not allowed by rule") {
				t.Fatalf("%s %s answered %d %q; want 403 naming the rule", row.method, row.path, answer.StatusCode, body)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d requests; want none", len(seen))
			}
		})
	}
}

// TestAPathThatWalksOutOfTheAllowedPrefixIsRefused: the entry's allow list is a path prefix, and
// a path that leaves that prefix through a dot-dot segment is outside it however it is spelled.
// A reader that resolves dot segments first, which is what RFC 3986's remove_dot_segments says a
// URI means, routes "/v1/../admin/things" as "/admin/things", so a proxy that reads the prefix
// off the literal path carries the value to a path the entry forbids. The row asserts the
// property the airlock exists for: no request that leaves the prefix reaches the service with
// the real value in it.
func TestAPathThatWalksOutOfTheAllowedPrefixIsRefused(t *testing.T) {
	for _, row := range []struct{ name, path string }{
		{"a dot-dot segment", "/v1/../admin/things"},
		{"a dot-dot segment after a deeper path", "/v1/things/../../admin/things"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			client := r.client(workerPlaceholder)
			answer, body := r.get(client, "https://"+boundHost+row.path, "Authorization", "Bearer "+workerPlaceholder)
			for _, seen := range r.requests() {
				if strings.Contains(seen.Header.Get("Authorization"), workerValue) {
					t.Fatalf("GET %s reached the service as %s with the real value in Authorization; want the value never to leave on a path outside the entry's allow prefix", row.path, seen.Path)
				}
			}
			if answer.StatusCode != http.StatusForbidden {
				t.Fatalf("GET %s answered %d %q; want 403", row.path, answer.StatusCode, strings.TrimSpace(body))
			}
		})
	}
}

// TestABoundHostOnAnotherPortCarriesNoValue: an entry binds a host, and every name the decision
// compares - the CONNECT target, the TLS name and the Host header - is a name with no port in
// it, so port 443 is the only port the rules speak about. A command that opens its CONNECT on
// another port of a bound host and writes its Host header without the port makes all three
// agree, and the value must still not leave on that connection: another port of one name is
// another service.
func TestABoundHostOnAnotherPortCarriesNoValue(t *testing.T) {
	r := newRig(t, Options{})
	r.dialTo[net.JoinHostPort(boundHost, "8443")] = r.service.Addr()

	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatalf("dial the proxy port: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := fmt.Fprintf(conn, "CONNECT %s:8443 HTTP/1.1\r\nHost: %s:8443\r\n%s\r\n", boundHost, boundHost, basic(workerPlaceholder)); err != nil {
		t.Fatalf("write the CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	if answer.StatusCode == http.StatusOK {
		// The proxy opened something. Whether it decrypted the connection is what the rest of
		// the row reads: a plain tunnel to the stand-in answers a certificate this client does
		// not trust, and a decrypted one answers the proxy's own authority.
		pool := x509.NewCertPool()
		pool.AddCert(r.authority.Certificate())
		decrypted := tls.Client(conn, &tls.Config{ServerName: boundHost, RootCAs: pool})
		if err := decrypted.Handshake(); err == nil {
			if _, err := fmt.Fprintf(decrypted, "GET /v1/things HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", boundHost, workerPlaceholder); err != nil {
				t.Fatalf("write the request: %v", err)
			}
			if err := decrypted.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatalf("set the read deadline: %v", err)
			}
			if _, err := http.ReadResponse(bufio.NewReader(decrypted), nil); err != nil {
				t.Logf("the request on port 8443 was not answered: %v", err)
			}
		}
	}
	for _, seen := range r.requests() {
		if strings.Contains(seen.Header.Get("Authorization"), workerValue) {
			t.Fatalf("a call on port 8443 of %s reached the service with the real value in Authorization (Host %q); want the value to leave on port 443 alone", boundHost, seen.Host)
		}
	}
}

// TestABoundNameOnAnotherPortIsTunnelled: the entry binds a name, and every name the decision
// compares carries no port, so 443 is the only port those rules speak about and another port
// of that name is another service. Such a CONNECT is the plain tunnel any unbound host gets -
// the proxy opens no TLS of its own, reads nothing and logs no call - rather than a refusal,
// because the airlock blocks nothing it does not bind.
func TestABoundNameOnAnotherPortIsTunnelled(t *testing.T) {
	r := newRig(t, Options{})
	r.dialTo[net.JoinHostPort(boundHost, "8443")] = r.service.Addr()
	target := net.JoinHostPort(boundHost, "8443")

	// What answers the handshake inside the tunnel is the stand-in service's own certificate:
	// the bytes reached that service untouched.
	answer, conn, _ := r.connectRaw(target, basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s answered %d %q; want 200, the tunnel any host the rules say nothing about gets", target, answer.StatusCode, bodyOf(t, answer))
	}
	service := tls.Client(conn, &tls.Config{ServerName: boundHost, RootCAs: r.service.Authority().Pool()})
	if err := service.Handshake(); err != nil {
		t.Fatalf("the TLS handshake inside the tunnel on port 8443: %v; want the stand-in service's own certificate", err)
	}

	// And on a second tunnel, the proxy's own authority is not what answered: a proxy that had
	// decrypted this connection would have served a leaf of it, as it does on port 443.
	second, conn2, _ := r.connectRaw(target, basic(workerPlaceholder))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("the second CONNECT %s answered %d; want 200", target, second.StatusCode)
	}
	proxyOnly := x509.NewCertPool()
	proxyOnly.AddCert(r.authority.Certificate())
	if err := tls.Client(conn2, &tls.Config{ServerName: boundHost, RootCAs: proxyOnly}).Handshake(); err == nil {
		t.Fatal("the connection on port 8443 was answered by a certificate of the proxy's own authority; want a tunnel the proxy does not read")
	}
	if calls := r.logs.calls(); len(calls) != 0 {
		t.Errorf("the proxy logged %d call(s) to a bound host for a tunnelled port; want none: it read no request", len(calls))
	}
}

// TestTheLogLineOfARefusedDotSegmentRequestNamesTheReason: every call to a bound host logs one
// line, and a refused one's line is its refusal record, whose reason is the decision's own words.
// An operator reading the line has to see that it was the dot segment that refused the call,
// since the path as written is inside the entry's prefix.
func TestTheLogLineOfARefusedDotSegmentRequestNamesTheReason(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/../admin/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "path has a dot segment") {
		t.Fatalf("the call answered %d %q; want 403 naming the dot segment", answer.StatusCode, strings.TrimSpace(body))
	}
	refusals := r.logs.refusals()
	if len(refusals) != 1 {
		t.Fatalf("the proxy wrote %d refusal records; want exactly one", len(refusals))
	}
	if calls := r.logs.calls(); len(calls) != 0 {
		t.Fatalf("the refused call also logged %d call lines; want its refusal record alone: %v", len(calls), calls)
	}
	line := refusals[0]
	if reason, _ := line.fields["reason"].(string); reason != "path has a dot segment" {
		t.Errorf("the refused call logged the reason %q; want the decision's own words", reason)
	}
	if line.level != slog.LevelWarn || line.fields["status"] != int64(403) || line.fields["code"] != codeNotAllowed {
		t.Errorf("the refused call logged %v at %v; want status 403 and code %s at WARN", line.fields, line.level, codeNotAllowed)
	}
	if path, _ := line.fields["path"].(string); path != "/v1/../admin/things" {
		t.Errorf("the refused call logged the path %q; want the path the command wrote", path)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
}

// TestARequestThatAlreadyCarriesTheValueIsRefused: a command that holds the real value learnt it
// somewhere other than this swap, so the airlock refuses the request rather than carrying it.
func TestARequestThatAlreadyCarriesTheValueIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerValue)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "request already carries the value") {
		t.Fatalf("the call answered %d %q; want 403 naming that the request already carries the value", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
}

// TestPlainHTTPToABoundHostIsRefused: the value may travel only inside TLS the proxy itself
// opened, so an http:// request to a bound host is refused rather than forwarded.
func TestPlainHTTPToABoundHostIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "http://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "refusing plain http") {
		t.Fatalf("plain http to a bound host answered %d %q; want 403 naming the rule", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
	if r.dials.Load() != 0 {
		t.Fatalf("the proxy dialled %d time(s) for a refused plain-http request; want none", r.dials.Load())
	}
}

// TestATunnelledHostKeepsItsOwnCertificateAndItsBytes: a host no name of the grant is bound to is
// a tunnel. The proxy opens no TLS on it - the command's own client verifies the service's own
// certificate, which the proxy's authority did not sign - and the bytes arrive as they were sent.
func TestATunnelledHostKeepsItsOwnCertificateAndItsBytes(t *testing.T) {
	r := newRig(t, Options{})
	answer, conn, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to a tunnelled host answered %d; want 200 Connection established", answer.StatusCode)
	}
	// The command's own client verifies the service's certificate, and the check below is that
	// the proxy's authority did not sign it: a proxy that had opened this connection would have
	// answered a leaf of its own.
	client := tls.Client(conn, &tls.Config{ServerName: tunnelHost, RootCAs: r.trust})
	if err := client.Handshake(); err != nil {
		t.Fatalf("the tunnelled handshake: %v", err)
	}
	peer := client.ConnectionState().PeerCertificates[0]
	if err := peer.CheckSignatureFrom(r.authority.Certificate()); err == nil {
		t.Fatal("the tunnelled service's certificate was signed by the proxy's own authority: the proxy opened a tunnel it was supposed to leave alone")
	}
	sent := "the bytes the command sent\n"
	if _, err := client.Write([]byte(sent)); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	read := make([]byte, len(sent))
	if _, err := io.ReadFull(client, read); err != nil {
		t.Fatalf("read back through the tunnel: %v", err)
	}
	if string(read) != sent {
		t.Fatalf("the tunnel answered %q; want %q", read, sent)
	}
	if calls := r.logs.calls(); len(calls) != 0 {
		t.Fatalf("the proxy logged %d call(s) to a bound host for a tunnel; want none: it reads nothing on a tunnel", len(calls))
	}
}

// TestTheProxyRefusesTheAddressesItMustNeverDial: without this the proxy is a way into the
// cluster the broker runs in. Each row names the rule and the address family in its refusal, and
// nothing is dialled.
func TestTheProxyRefusesTheAddressesItMustNeverDial(t *testing.T) {
	r := newRig(t, Options{})
	r.addresses["mixed.example.net"] = []netip.Addr{netip.MustParseAddr("93.184.216.40"), netip.MustParseAddr("10.9.9.9")}
	for _, row := range []struct{ host, want string }{
		{"127.0.0.1", "a loopback IPv4 address"},
		{"::1", "a loopback IPv6 address"},
		{"10.1.2.3", "a private IPv4 address"},
		{"172.16.3.4", "a private IPv4 address"},
		{"192.168.5.6", "a private IPv4 address"},
		{"fc00::1", "a unique-local IPv6 address"},
		{"169.254.169.254", "a link-local IPv4 address"},
		{"fe80::1", "a link-local IPv6 address"},
		{"100.64.1.2", "a cluster IPv4 address"},
		{"0.1.2.3", "an IPv4 address of 0.0.0.0/8"},
		{"224.0.0.1", "a link-local IPv4 address"},
		{"239.1.2.3", "not a global unicast IPv4 address"},
		{"::", "not a global unicast IPv6 address"},
		{"mixed.example.net", "a private IPv4 address"},
	} {
		t.Run(row.host, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(row.host, "443"), basic(workerPlaceholder))
			body := bodyOf(t, answer)
			if answer.StatusCode != http.StatusForbidden {
				t.Fatalf("CONNECT %s = %d %q; want 403", row.host, answer.StatusCode, body)
			}
			if !strings.Contains(body, row.want) {
				t.Errorf("the refusal of %s reads %q; want it to name %q", row.host, body, row.want)
			}
			if after := r.dials.Load(); after != before {
				t.Fatalf("the proxy dialled %s %d time(s); want none: the refusal comes before any dial", row.host, after-before)
			}
		})
	}
}

// TestDialAllowOpensTheAddressesItNamesAndNoOthers: Options.DialAllow is how the broker's local
// stack reaches its stand-in services on loopback while every other loopback address the pod
// holds - the broker's own ports among them - stays refused, which one check needs both halves
// of in one run. An entry is a host and a port, so the same host at another port is refused
// too.
func TestDialAllowOpensTheAddressesItNamesAndNoOthers(t *testing.T) {
	const standIn = "stand-in.example.net"
	allowed := net.JoinHostPort(standIn, "443")
	r := newRig(t, Options{DialAllow: []string{allowed}})
	r.addresses[standIn] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	r.dialTo[allowed] = r.dialTo[net.JoinHostPort(echoHost, "443")]

	before := r.dials.Load()
	answer, _, _ := r.connectRaw(allowed, basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d %q; want 200: the list names it", allowed, answer.StatusCode, bodyOf(t, answer))
	}
	if after := r.dials.Load(); after != before+1 {
		t.Fatalf("the proxy dialled %d time(s) for an allowed address; want one", after-before)
	}
	for _, target := range []string{net.JoinHostPort(standIn, "8443"), net.JoinHostPort("127.0.0.1", "443")} {
		before := r.dials.Load()
		answer, _, _ := r.connectRaw(target, basic(workerPlaceholder))
		body := bodyOf(t, answer)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "a loopback IPv4 address") {
			t.Errorf("CONNECT %s = %d %q; want 403 naming the loopback address, which the list does not name", target, answer.StatusCode, body)
		}
		if after := r.dials.Load(); after != before {
			t.Errorf("the proxy dialled %s %d time(s); want none", target, after-before)
		}
	}
}

// serviceRange is GKE's default Service range, which a cluster's ClusterIPs are drawn from. It is
// global unicast, so none of localRule's fixed rules refuses an address in it: only
// Options.ClusterRanges does.
var serviceRange = netip.MustParsePrefix("34.118.224.0/20")

// clusterRangeRule is the words every refusal of an address inside Options.ClusterRanges carries.
const clusterRangeRule = "inside BROKER_AIRLOCK_CLUSTER_RANGES, the broker cluster's own addresses"

// clusterIPs are three ClusterIPs of Services in a cluster whose Service range is serviceRange.
var clusterIPs = []string{"34.118.237.200", "34.118.227.252", "34.118.234.195"}

// TestTheProxyRefusesTheBrokerClustersOwnRanges: without Options.ClusterRanges the proxy is a way
// into the broker's own cluster through every Service it has, because a Service range such as
// serviceRange is global unicast and passes every fixed rule. Each refused row's address is wired
// to a service, so a proxy that admits it opens the tunnel and answers 200 rather than failing for
// want of a route; each admitted row is the control that the refusal is about the range and not
// about the address's neighbourhood.
func TestTheProxyRefusesTheBrokerClustersOwnRanges(t *testing.T) {
	ranges := []netip.Prefix{serviceRange, netip.MustParsePrefix("2001:db8:5::/48")}
	newRangeRig := func(t *testing.T, ranges []netip.Prefix) *rig {
		r := newRig(t, Options{ClusterRanges: ranges})
		r.addresses["service.example.net"] = []netip.Addr{netip.MustParseAddr(clusterIPs[0])}
		r.addresses["mixed-service.example.net"] = []netip.Addr{netip.MustParseAddr("93.184.216.40"), netip.MustParseAddr(clusterIPs[1])}
		echo := r.dialTo[net.JoinHostPort(echoHost, "443")]
		// Each address a refused row names is wired to the echo service, the zoned forms as the
		// proxy would dial them, so a proxy that let one through opens a tunnel and answers 200.
		for _, addr := range []string{clusterIPs[0], clusterIPs[1], clusterIPs[2], "34.118.224.0", "34.118.239.255", "2001:db8:5::10",
			"2001:db8:5::10%eth0", "2001:db8:5::10%0", "2001:db8:5::10%x", "::%x",
			"34.118.240.0", "34.118.223.255", "2001:db8:6::10", "93.184.216.40"} {
			r.dialTo[net.JoinHostPort(addr, "443")] = echo
		}
		return r
	}
	refusedWith := func(t *testing.T, r *rig, host, want, why string) {
		t.Helper()
		before := r.dials.Load()
		answer, _, _ := r.connectRaw(net.JoinHostPort(host, "443"), basic(workerPlaceholder))
		// The status first: a CONNECT answered 200 is an open tunnel, whose body never ends.
		if answer.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s = %d after %d dial(s); want 403 and none: %s",
				host, answer.StatusCode, r.dials.Load()-before, why)
		}
		body := bodyOf(t, answer)
		if !strings.Contains(body, want) {
			t.Errorf("the refusal of %s reads %q; want it to name %q", host, body, want)
		}
		if after := r.dials.Load(); after != before {
			t.Fatalf("the proxy dialled %s %d time(s); want none: the refusal comes before any dial", host, after-before)
		}
	}
	refused := func(t *testing.T, r *rig, host, family string) {
		t.Helper()
		refusedWith(t, r, host, "an "+family+" address "+clusterRangeRule, "it is inside BROKER_AIRLOCK_CLUSTER_RANGES")
	}
	admitted := func(t *testing.T, r *rig, host string) {
		t.Helper()
		before := r.dials.Load()
		answer, _, _ := r.connectRaw(net.JoinHostPort(host, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT %s = %d %q; want 200: it is inside no range", host, answer.StatusCode, bodyOf(t, answer))
		}
		if after := r.dials.Load(); after != before+1 {
			t.Fatalf("the proxy dialled %s %d time(s); want one", host, after-before)
		}
	}

	r := newRangeRig(t, ranges)
	for _, row := range []struct{ name, host, family string }{
		{"a ClusterIP in the cluster's Service range (" + clusterIPs[0] + ")", clusterIPs[0], "IPv4"},
		{"a ClusterIP in the cluster's Service range (" + clusterIPs[1] + ")", clusterIPs[1], "IPv4"},
		{"a ClusterIP in the cluster's Service range (" + clusterIPs[2] + ")", clusterIPs[2], "IPv4"},
		{"the first address of the range", "34.118.224.0", "IPv4"},
		{"the last address of the range", "34.118.239.255", "IPv4"},
		{"an address in an IPv6 range", "2001:db8:5::10", "IPv6"},
		{"a name that resolves to a ClusterIP", "service.example.net", "IPv4"},
		{"a name whose answer holds a ClusterIP beside a public address", "mixed-service.example.net", "IPv4"},
		// A zone on a global IPv6 address names nothing a socket to it needs, and netip's
		// Contains is false for any zoned address, so each of these is read without its zone.
		{"a zoned IPv6 literal in a range (%25eth0)", "2001:db8:5::10%25eth0", "IPv6"},
		{"a zoned IPv6 literal in a range (%250)", "2001:db8:5::10%250", "IPv6"},
		{"a zoned IPv6 literal in a range (%25x)", "2001:db8:5::10%25x", "IPv6"},
	} {
		t.Run("refuses "+row.name, func(t *testing.T) { refused(t, r, row.host, row.family) })
	}
	// The unspecified address with a zone. netip's IsGlobalUnicast compares an address with ::
	// whole, zone included, so ::%x was global unicast to a check that kept the zone, and Linux
	// connects a socket to the unspecified address on the loopback: a tunnel to [::%x] reached the
	// broker's own pod. Read without its zone it is ::, which the fixed rules refuse.
	t.Run("refuses the unspecified address with a zone (::%25x), which a socket connects on the loopback", func(t *testing.T) {
		refusedWith(t, r, "::%25x", "not a global unicast IPv6 address", "a socket to it connects on the pod's own loopback")
	})
	for _, row := range []struct{ name, host string }{
		{"the address after the range", "34.118.240.0"},
		{"the address before the range", "34.118.223.255"},
		{"an IPv6 address outside its range", "2001:db8:6::10"},
		{"a public address", "93.184.216.40"},
	} {
		t.Run("dials "+row.name, func(t *testing.T) { admitted(t, r, row.host) })
	}
	// And the same ClusterIPs through a proxy given no ranges, which is a broker outside a
	// cluster: dialled, so what refuses them above is the setting.
	open := newRangeRig(t, nil)
	for _, addr := range clusterIPs {
		t.Run("with no ranges dials "+addr, func(t *testing.T) { admitted(t, open, addr) })
	}
}

// TestAClusterRangeAddressIsRefusedOnEveryPathTheProxyDials: a CONNECT is one of three ways a
// command reaches an address through the proxy. A bound host is decrypted and its requests are
// dialled by the proxy's own transport, and a plain-HTTP request is forwarded; both must refuse a
// ClusterIP as a tunnel does, and the controls show each path still carries a public address.
func TestAClusterRangeAddressIsRefusedOnEveryPathTheProxyDials(t *testing.T) {
	clusterIP := netip.MustParseAddr(clusterIPs[0])
	options := Options{ClusterRanges: []netip.Prefix{serviceRange, netip.MustParsePrefix("2001:db8:5::/48")}}

	t.Run("a bound host whose name resolves to a ClusterIP is refused at its CONNECT", func(t *testing.T) {
		r := newRig(t, options)
		r.addresses[boundHost] = []netip.Addr{clusterIP}
		r.dialTo[net.JoinHostPort(clusterIP.String(), "443")] = r.service.Addr()
		answer, _, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s at %s = %d; want 403: the ClusterIP is inside BROKER_AIRLOCK_CLUSTER_RANGES", boundHost, clusterIP, answer.StatusCode)
		}
		if body := bodyOf(t, answer); !strings.Contains(body, clusterRangeRule) {
			t.Fatalf("the refusal of %s at %s reads %q; want it to name %q", boundHost, clusterIP, body, clusterRangeRule)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Fatalf("the service at the ClusterIP was sent %d requests; want none", len(seen))
		}
	})

	t.Run("a bound host's request is refused when its name resolves to a ClusterIP at the dial", func(t *testing.T) {
		r := newRig(t, options)
		// The CONNECT's check reads a public address, and the dial of the request behind it
		// reads the ClusterIP, which is wired to the service: a transport that did not check the
		// range would carry the swapped value to it.
		r.answers[boundHost] = [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}, {clusterIP}}
		r.dialTo[net.JoinHostPort(clusterIP.String(), "443")] = r.service.Addr()
		answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode == http.StatusOK || !strings.Contains(body, clusterRangeRule) {
			t.Fatalf("the request answered %d %q; want it refused naming %q", answer.StatusCode, body, clusterRangeRule)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Fatalf("the service at the ClusterIP was sent %d requests; want none", len(seen))
		}
		for _, addr := range r.dialedAddresses() {
			if strings.HasPrefix(addr, clusterIP.String()+":") {
				t.Fatalf("the proxy dialled the ClusterIP at %s", addr)
			}
		}
	})

	t.Run("a bound host other than the private destination is refused when its name resolves to a ClusterIP at the dial", func(t *testing.T) {
		// The row above with a destination set for another host, whose name resolves to the same
		// ClusterIP: the destination takes its own host's requests and no other host's, so this
		// request goes out on the proxy's own transport and is refused at its dial as before.
		withDestination := options
		withDestination.PrivateUpstreams = []PrivateUpstream{privateDestination()}
		r := newRig(t, withDestination)
		r.addresses[privateName] = []netip.Addr{clusterIP}
		r.answers[boundHost] = [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}, {clusterIP}}
		r.dialTo[net.JoinHostPort(clusterIP.String(), "443")] = r.service.Addr()
		r.dialTo[net.JoinHostPort(clusterIP.String(), privatePort)] = r.service.Addr()
		answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		for _, addr := range r.dialedAddresses() {
			if strings.HasPrefix(addr, clusterIP.String()+":") {
				t.Errorf("the proxy dialled the ClusterIP at %s", addr)
			}
		}
		if answer.StatusCode == http.StatusOK || !strings.Contains(body, clusterRangeRule) {
			t.Errorf("the request answered %d %q; want it refused naming %q", answer.StatusCode, body, clusterRangeRule)
		}
		if seen := r.requests(); len(seen) != 0 {
			t.Errorf("the service at the ClusterIP was sent %d requests; want none", len(seen))
		}
	})

	t.Run("the private destination's bound host is sent its allowed request at a ClusterIP", func(t *testing.T) {
		// BROKER_PROXY_PRIVATE_UPSTREAM's one exception to the ranges: the destination resolves
		// to a ClusterIP inside them, and the allowed write to its bound host reaches the service
		// there, while a CONNECT to that ClusterIP, or to the destination's name, is refused: by a
		// secret grant, which may tunnel to a host its names do not bind, naming the ranges, and by
		// the minted grant, which reaches its bound host alone, by that rule.
		r, standIn := newPrivateRig(t, clusterIP, options.ClusterRanges)
		answer, body, err := r.writeMinted()
		if err != nil || answer.StatusCode != http.StatusCreated || len(standIn.writes()) != 1 {
			t.Fatalf("the allowed write answered %v %v and the service received %+v; want 201 and one write", answer, err, standIn.writes())
		}
		if dialed := r.dialedAddresses(); len(dialed) != 1 || dialed[0] != net.JoinHostPort(clusterIP.String(), privatePort) {
			t.Fatalf("the proxy dialled %v; want the ClusterIP %s:%s alone (%s)", dialed, clusterIP, privatePort, body)
		}
		for _, target := range []string{net.JoinHostPort(clusterIP.String(), privatePort), net.JoinHostPort(privateName, privatePort)} {
			if _, refusal := connectRefused(t, r, target, workerPlaceholder); !strings.Contains(refusal, clusterRangeRule) {
				t.Errorf("the refusal of a secret grant's CONNECT %s reads %q; want it to name %q", target, refusal, clusterRangeRule)
			}
			if code, refusal := connectRefused(t, r, target, mintedPlaceholder); code != codeMintedHostOnly {
				t.Errorf("the minted grant's CONNECT %s was refused %s (%q); want %s", target, code, refusal, codeMintedHostOnly)
			}
		}
	})

	t.Run("control: a bound host at a public address is carried", func(t *testing.T) {
		r := newRig(t, options)
		answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the request answered %d %q; want 200", answer.StatusCode, body)
		}
		if seen := r.requests(); len(seen) != 1 {
			t.Fatalf("the service was sent %d requests; want one", len(seen))
		}
	})

	// The plain-HTTP rows: a service that answers plain HTTP, wired to the ClusterIP and to a
	// public address alike, so a forward of either answers 200 with its words.
	plainRig := func(t *testing.T) (*rig, *atomic.Int64) {
		r := newRig(t, options)
		var served atomic.Int64
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served.Add(1)
			io.WriteString(w, "the plain service answered")
		}))
		t.Cleanup(plain.Close)
		r.addresses["service.example.net"] = []netip.Addr{clusterIP}
		for _, addr := range []string{clusterIP.String(), "93.184.216.40", "2001:db8:5::10%eth0", "2001:db8:5::10%0", "2001:db8:5::10%x", "2001:db8:6::10"} {
			r.dialTo[net.JoinHostPort(addr, "80")] = plain.Listener.Addr().String()
		}
		return r, &served
	}
	for _, host := range []string{clusterIP.String(), "service.example.net"} {
		t.Run("a plain-HTTP request to "+host+" is refused", func(t *testing.T) {
			r, served := plainRig(t)
			answer, body := r.get(r.client(workerPlaceholder), "http://"+host+"/status")
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, clusterRangeRule) {
				t.Fatalf("plain http to %s answered %d %q; want 403 naming %q", host, answer.StatusCode, body, clusterRangeRule)
			}
			if r.dials.Load() != 0 || served.Load() != 0 {
				t.Fatalf("the proxy dialled %d time(s) and the service answered %d request(s); want none", r.dials.Load(), served.Load())
			}
		})
	}
	// The zoned literals are written on a socket of the row's own (plainRaw), as the bytes a
	// command's client may send: net/http's client drops the zone from the URL it writes to a
	// proxy, so through it each row would be the unzoned literal and prove nothing about a zone.
	for _, host := range []string{"[2001:db8:5::10%25eth0]", "[2001:db8:5::10%250]", "[2001:db8:5::10%25x]"} {
		t.Run("a plain-HTTP request to the zoned literal "+host+" is refused", func(t *testing.T) {
			r, served := plainRig(t)
			answer, body := r.plainRaw(host, "/status", basic(workerPlaceholder))
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, clusterRangeRule) {
				t.Fatalf("plain http to %s answered %d %q after the dials %v; want 403 naming %q", host, answer.StatusCode, body, r.dialedAddresses(), clusterRangeRule)
			}
			if r.dials.Load() != 0 || served.Load() != 0 {
				t.Fatalf("the proxy dialled %v and the service answered %d request(s); want none", r.dialedAddresses(), served.Load())
			}
		})
	}
	t.Run("control: a plain-HTTP request to a zoned literal outside the ranges is forwarded to the address it was checked as", func(t *testing.T) {
		r, served := plainRig(t)
		answer, body := r.plainRaw("[2001:db8:6::10%25eth0]", "/status", basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK || body != "the plain service answered" || served.Load() != 1 {
			t.Fatalf("plain http to [2001:db8:6::10%%25eth0] answered %d %q with %d request(s) served after the dials %v; want 200 from the service", answer.StatusCode, body, served.Load(), r.dialedAddresses())
		}
		if dialed := r.dialedAddresses(); !slices.Equal(dialed, []string{"[2001:db8:6::10]:80"}) {
			t.Fatalf("the proxy dialled %v; want [[2001:db8:6::10]:80], the address without its zone", dialed)
		}
	})
	t.Run("control: a plain-HTTP request to a public address is forwarded", func(t *testing.T) {
		r, served := plainRig(t)
		answer, body := r.get(r.client(workerPlaceholder), "http://93.184.216.40/status")
		if answer.StatusCode != http.StatusOK || body != "the plain service answered" || served.Load() != 1 {
			t.Fatalf("plain http to a public address answered %d %q with %d request(s) served; want 200 from the service", answer.StatusCode, body, served.Load())
		}
	})
}

// TestDialAllowStillOpensAnAddressInsideTheClusterRanges: Options.DialAllow is a test's way to a
// stand-in service, and it opens the address it names whatever rule would refuse it, the cluster
// ranges among them. The same address under any other host:port stays refused.
func TestDialAllowStillOpensAnAddressInsideTheClusterRanges(t *testing.T) {
	const standIn = "stand-in.example.net"
	allowed := net.JoinHostPort(standIn, "443")
	r := newRig(t, Options{ClusterRanges: []netip.Prefix{serviceRange}, DialAllow: []string{allowed}})
	r.addresses[standIn] = []netip.Addr{netip.MustParseAddr(clusterIPs[0])}
	echo := r.dialTo[net.JoinHostPort(echoHost, "443")]
	r.dialTo[allowed] = echo
	r.dialTo[net.JoinHostPort(clusterIPs[0], "443")] = echo
	r.dialTo[net.JoinHostPort(clusterIPs[0], "8443")] = echo

	before := r.dials.Load()
	answer, _, _ := r.connectRaw(allowed, basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d %q; want 200: DialAllow names it", allowed, answer.StatusCode, bodyOf(t, answer))
	}
	if after := r.dials.Load(); after != before+1 {
		t.Fatalf("the proxy dialled %d time(s) for an allowed address; want one", after-before)
	}
	for _, target := range []string{net.JoinHostPort(standIn, "8443"), net.JoinHostPort(clusterIPs[0], "443")} {
		before := r.dials.Load()
		answer, _, _ := r.connectRaw(target, basic(workerPlaceholder))
		if answer.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d after %d dial(s); want 403 and none, since DialAllow does not name it", target, answer.StatusCode, r.dials.Load()-before)
			continue
		}
		if body := bodyOf(t, answer); !strings.Contains(body, clusterRangeRule) {
			t.Errorf("the refusal of %s reads %q; want it to name %q", target, body, clusterRangeRule)
		}
		if after := r.dials.Load(); after != before {
			t.Errorf("the proxy dialled %s %d time(s); want none", target, after-before)
		}
	}
}

// TestTheIdleLimitEndsAQuietConnectionAndKeepsABusyOne: a connection carries a grant's
// placeholders, so one a command walked away from is state the broker holds for nobody. A
// connection with bytes crossing it is not ended, however long it lives.
func TestTheIdleLimitEndsAQuietConnectionAndKeepsABusyOne(t *testing.T) {
	const idle = 400 * time.Millisecond
	t.Run("a connection with no byte is ended", func(t *testing.T) {
		r := newRig(t, Options{Idle: idle})
		// began is taken before the CONNECT, because the idle clock starts when the proxy
		// accepts the connection, not when the 200 reaches this test: measured from after the
		// answer, a tunnel ended on time reads as ended a millisecond early.
		began := time.Now()
		answer, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT answered %d; want 200", answer.StatusCode)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) && err == nil {
			t.Fatalf("the quiet tunnel answered a byte after %v; want it closed", time.Since(began))
		} else if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the quiet tunnel was still open %v after its %v idle limit", time.Since(began), idle)
		}
		if took := time.Since(began); took < idle {
			t.Fatalf("the tunnel ended after %v, before its %v idle limit had run out", took, idle)
		}
	})
	t.Run("a connection with bytes is kept", func(t *testing.T) {
		r := newRig(t, Options{Idle: idle})
		_, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
		// Three times the idle limit, a byte every tenth of it: a connection the limit ends here
		// is one whose clock a read or a write did not push out.
		deadline := time.Now().Add(3 * idle)
		for time.Now().Before(deadline) {
			if _, err := conn.Write([]byte("x")); err != nil {
				t.Fatalf("the busy tunnel was closed while it was carrying bytes: %v", err)
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := reader.ReadByte(); err != nil {
				t.Fatalf("the busy tunnel was closed while it was carrying bytes: %v", err)
			}
			time.Sleep(idle / 10)
		}
	})
}

// TestShutdownClosesAnOpenTunnel: http.Server.Shutdown neither closes a hijacked connection nor
// waits for one, so without the proxy's own registry a drained pod would leave every tunnel open
// until the kubelet killed it.
func TestShutdownClosesAnOpenTunnel(t *testing.T) {
	r := newRig(t, Options{})
	answer, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d; want 200", answer.StatusCode)
	}
	if _, err := conn.Write([]byte("open\n")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("the tunnel was not carrying bytes before the drain: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.proxy.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := reader.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the tunnel was still open after the drain (%v); want it closed", err)
	}
}

// TestTheValuesOfOneGrantAreReadOnceOnThisPod: AirlockValues runs the full release checks - the
// record chain, the policy re-evaluation, the max-grant, the airlock fingerprint - so running it
// per request would put that whole read on every call a command makes.
func TestTheValuesOfOneGrantAreReadOnceOnThisPod(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	for range 2 {
		answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the call answered %d %q; want 200", answer.StatusCode, body)
		}
		// A second connection, not a second request on the first one: the read is once per
		// grant per pod, not once per connection.
		client.CloseIdleConnections()
	}
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times for two connections of one grant; want once per grant per pod", calls)
	}
	if r.grants.connCalls < 2 {
		t.Fatalf("the proxy took %d connections; want two, so that the row is about two connections of one grant", r.grants.connCalls)
	}
}

// TestEachCallToABoundHostLogsOneLine: an operator reading the broker's log must be able to say
// which session carried which secret where, and on what issue's authority - and must never read
// a placeholder or a value there. A carried call logs its call line, and a refused one its
// refusal record in that line's place, so each call logs exactly one.
func TestEachCallToABoundHostLogsOneLine(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	carried, _ := r.get(client, "https://"+boundHost+"/v1/things?token=secret-query", "Authorization", "Bearer "+workerPlaceholder)
	refusedAnswer, _ := r.do(client, http.MethodDelete, "https://"+boundHost+"/v1/things", "", "Authorization", "Bearer "+workerPlaceholder)

	calls := r.logs.callsAtLeast(t, 1)
	refusals := r.logs.refusals()
	if len(calls) != 1 || len(refusals) != 1 {
		t.Fatalf("the proxy logged %d call lines and %d refusal records for one carried and one refused call; want one each", len(calls), len(refusals))
	}
	allowed, refused := calls[0], refusals[0]
	for _, line := range []logLine{allowed, refused} {
		if strings.Contains(fmt.Sprint(line.fields), workerValue) {
			t.Errorf("the line names the value: %v", line.fields)
		}
		if strings.Contains(fmt.Sprint(line.fields), workerPlaceholder) {
			t.Errorf("the line names the placeholder: %v", line.fields)
		}
		if strings.Contains(fmt.Sprint(line.fields["path"]), "token=") {
			t.Errorf("the line's path carries the query: %v", line.fields["path"])
		}
		if got := line.fields["host"]; got != boundHost {
			t.Errorf("the line's host = %v; want %s", got, boundHost)
		}
		if got := line.fields["path"]; got != "/v1/things" {
			t.Errorf("the line's path = %v; want /v1/things", got)
		}
		if got := line.fields["issue"]; got != testIssue {
			t.Errorf("the line's issue = %v; want %s", got, testIssue)
		}
	}
	if allowed.level != slog.LevelInfo {
		t.Errorf("the call line is at %v; want info", allowed.level)
	}
	for field, want := range map[string]any{
		"enrollment": testEnrollID, "grant": testGrantID, "secret": workerName, "method": http.MethodGet,
		"allowed": true, "status": int64(200), "answer_id": carried.Header.Get(answer.Header),
	} {
		if got := allowed.fields[field]; got != want {
			t.Errorf("the allowed call's %s = %v; want %v", field, got, want)
		}
	}
	if _, named := allowed.fields["reason"]; named {
		t.Errorf("the allowed call logged a reason: %v", allowed.fields)
	}
	if refused.level != slog.LevelWarn {
		t.Errorf("the refusal record is at %v; want warn", refused.level)
	}
	for field, want := range map[string]any{
		"caller": testGrantID + "@" + testEnrollID, "caller_class": answer.CallerAirlock, "method": http.MethodDelete,
		"status": int64(403), "code": codeNotAllowed, "reason": "not allowed by rule", "answer_id": refusedAnswer.Header.Get(answer.Header),
	} {
		if got := refused.fields[field]; got != want {
			t.Errorf("the refused call's %s = %v; want %v", field, got, want)
		}
	}
	if got := fmt.Sprint(refused.fields["secrets"]); got != "["+workerName+"]" {
		t.Errorf("the refused call's secrets = %s; want [%s]", got, workerName)
	}
}

// TestOneRequestIsHandledForOneNameOfTheGrant: two secrets of one grant can be bound to one host,
// and the decision is over one secret, so the request is handled for the secret it carries a
// placeholder of - and for the first candidate when it carries none, which is still decided and
// still forwarded with nothing swapped.
func TestOneRequestIsHandledForOneNameOfTheGrant(t *testing.T) {
	const (
		alphaName        = "ALPHA_TOKEN"
		betaName         = "BETA_TOKEN"
		alphaValue       = "the-real-alpha"
		betaValue        = "the-real-beta"
		alphaPlaceholder = placeholder.Prefix + "alphaalphaalphaalphaalphaalpha01"
		betaPlaceholder  = placeholder.Prefix + "betabetabetabetabetabetabetabe01"
	)
	r := newRuledRig(t, twoNameRules(alphaName, betaName), Options{})
	r.grants.connections[alphaPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			alphaName: {Slots: map[string]string{requests.SlotValue: alphaPlaceholder}},
			betaName:  {Slots: map[string]string{requests.SlotValue: betaPlaceholder}},
		},
	}
	r.grants.values[testGrantID] = map[string]requests.AirlockSecret{
		alphaName: {Slots: map[string]string{requests.SlotValue: alphaValue}},
		betaName:  {Slots: map[string]string{requests.SlotValue: betaValue}},
	}
	client := r.client(alphaPlaceholder)

	// The request carries beta's placeholder and no other, so it is handled for beta: beta's
	// value goes into the header beta's entry names.
	r.get(client, "https://"+boundHost+"/v1/things", "X-Beta", "Token "+betaPlaceholder)
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want one", len(seen))
	}
	if want := "Token " + betaValue; seen[0].Header.Get("X-Beta") != want {
		t.Fatalf("the service saw X-Beta %q; want %q: the request carried beta's placeholder, so it is handled for beta", seen[0].Header.Get("X-Beta"), want)
	}
	if secret := r.logs.callsAtLeast(t, 1)[0].fields["secret"]; secret != betaName {
		t.Fatalf("the call logged the secret %v; want %s", secret, betaName)
	}

	// A request that carries no placeholder at all is still decided - for the first candidate in
	// name order - and forwarded with nothing swapped.
	r.get(client, "https://"+boundHost+"/v1/other", "X-Plain", "nothing of this grant")
	seen = r.requests()
	if len(seen) != 2 {
		t.Fatalf("the service saw %d requests; want two", len(seen))
	}
	if got := seen[1].Header.Get("X-Plain"); got != "nothing of this grant" {
		t.Errorf("the service saw X-Plain %q; want it unchanged", got)
	}
	if secret := r.logs.callsAtLeast(t, 2)[1].fields["secret"]; secret != alphaName {
		t.Fatalf("the call logged the secret %v; want %s, the first candidate in name order", secret, alphaName)
	}

	// A request that carries both is handled for the first candidate, and for that one alone:
	// the decision is over one secret, so the other's placeholder would leave the proxy as the
	// command wrote it, and the request is refused rather than sent.
	answer, body := r.get(client, "https://"+boundHost+"/v1/both", "Authorization", "Bearer "+alphaPlaceholder, "X-Beta", "Token "+betaPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "placeholder") {
		t.Errorf("the request carrying both answered %d %q; want 403: one request is handled for one name, and beta's placeholder would leave the proxy", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 2 {
		t.Errorf("the service saw %d requests; want the two above alone", len(seen))
	}
}

// TestAGrantTheReleaseChecksRefuseIsRefused: the full checks run once per grant per pod, and
// their refusal is this request's. A grant that is no longer live is 407, as the connection
// check's own refusals are; every other refusal those checks make is 403 in the broker's own
// words, since no retry clears one of them; a read of those checks that did not happen at all
// is 503 in the broker's own fixed words, and the store's own error reaches the log and no
// answer.
func TestAGrantTheReleaseChecksRefuseIsRefused(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want int
		says string
	}{
		{"a grant that is no longer live", fmt.Errorf("%w: AUTO_TOKEN's airlock entry changed since this grant was decided", requests.ErrGrantNotLive), http.StatusProxyAuthRequired, requests.ErrGrantNotLive.Error()},
		{"a read of those checks the broker could not make", databaseDown, http.StatusServiceUnavailable, grantUnreadable},
		// Not every refusal of the release checks is about the grant's life: a chain that no
		// longer verifies, a secret the store no longer holds, a mint its own minter refused
		// and a value the airlock cannot take apart are the broker's own answer about this
		// grant or its secret, in words that name nothing of the store. Each is 403: a command
		// told to make the call again would make it for ever.
		{"a grant whose approval chain no longer verifies", fmt.Errorf("%w: record 9f2c: stored body does not reproduce its id", requests.ErrGrantChainInvalid), http.StatusForbidden, requests.ErrGrantChainInvalid.Error()},
		{"a secret the store no longer holds", fmt.Errorf("%w: AUTO_TOKEN", requests.ErrSecretNotInStore), http.StatusForbidden, requests.ErrSecretNotInStore.Error()},
		{"a mint the secret's own minter refused", fmt.Errorf("%w: the minter said no", requests.ErrMintFailed), http.StatusForbidden, requests.ErrMintFailed.Error()},
		// And the one mint failure that is no answer about this grant at all: the minter could
		// not even ask its upstream, which did not answer or answered a 5xx or a rate limit.
		// It reaches here wrapped in ErrMintFailed like any other, so the sentinel the secrets
		// package put inside it is what tells them apart, and a re-mint of a github-app,
		// google-login or azure-login name behind the airlock fails this way whenever its
		// upstream has a bad minute. 503, and nothing of the upstream's own words.
		{"a mint whose upstream could not be asked", fmt.Errorf("%w: %w: github app 42: installation 7: 502 Bad Gateway", requests.ErrMintFailed, secrets.ErrMintUpstream), http.StatusServiceUnavailable, grantUnreadable},
		// The shape error as the release loop hands it over: the name it refused for, and the
		// sentinel that says the airlock could not take the value apart (requests.readRelease).
		{"a value the airlock cannot take apart", fmt.Errorf("release %s behind the airlock: %w: its value is not a JSON object, which the airlock must take apart to stand placeholders in for its fields", workerName, requests.ErrAirlockValueShape), http.StatusForbidden, requests.ErrAirlockValueShape.Error()},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			r.grants.valuesErr = row.err
			client := r.client(workerPlaceholder)
			answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != row.want || !strings.Contains(body, row.says) {
				t.Fatalf("the call answered %d %q; want %d naming %q", answer.StatusCode, body, row.want, row.says)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d requests; want none", len(seen))
			}
		})
	}
}

// TestAProfilesRefusalOfTheGrantIsARefusalAtTheProxy: the two refusals a profile makes of a
// grant's release — a target the live profile does not hold (requests.ErrTargetRefused), and a
// scope its minter refuses (secrets.ErrMintNarrowingRefused: a cut that leaves no permission, a
// repository GitHub refuses) — are the broker's answer about that grant, which no retry clears.
// So the proxy answers each 403 in the broker's own words, under the code the API port answers
// the same refusal of a release with, and never the 503 of a read that did not happen, which
// tells a command to make the call again.
func TestAProfilesRefusalOfTheGrantIsARefusalAtTheProxy(t *testing.T) {
	for _, row := range []struct {
		name       string
		err        error
		code, says string
	}{
		{"a target outside the live profile", fmt.Errorf("%w: PROOFS_READ: the target ada/site is outside the profile, which names the account ada and the repositories ada/lean", requests.ErrTargetRefused), "TARGET_REFUSED", "ada/site"},
		{"a scope the minter refuses", fmt.Errorf("%w: github app 42: GitHub answered HTTP 422 to the profile's token for ada: likely a repository it names (missing) that the installation does not reach", secrets.ErrMintNarrowingRefused), "PROFILE_MINT_REFUSED", "(missing)"},
		// A stored secret under a dpop block: the API port answers its release MINTED_ONLY, and
		// so does this port, under the same code.
		{"a stored secret under a dpop block", fmt.Errorf("release %s behind the airlock: %w", workerName, requests.ErrMintedOnly), "MINTED_ONLY", "never a stored secret's release"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			r.grants.valuesErr = row.err
			client := r.client(workerPlaceholder)
			answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, row.says) {
				t.Fatalf("the call answered %d %q; want 403 naming %q", answer.StatusCode, body, row.says)
			}
			if records := r.logs.refusals(); len(records) != 1 || records[0].fields["code"] != row.code {
				t.Fatalf("the refusal recorded %v; want one record coded %s", records, row.code)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d requests; want none", len(seen))
			}
		})
	}
}

// TestAGrantReadThatFailedIsNotTheGrantsOwnRefusal: a read of a grant that did not happen - the
// store did not answer - is the broker's own 503 and not the grant's 407, on the CONNECT and on
// a request of a connection already open alike. Two things hang on it. A 407 is what tells a
// command that its grant has ended and that it needs a new one, so a store down for a minute
// would otherwise send every agent in the middle of a call to ask for a grant, which on the
// human tier is a person approving each one; and the store's own words name the database's
// user, its name and its address, which no command may read. A grant that really has ended is
// 407 in the broker's own words, as it always was.
func TestAGrantReadThatFailedIsNotTheGrantsOwnRefusal(t *testing.T) {
	// The words of the store's own error that no answer of this port may carry.
	store := []string{"user=broker", "10.20.0.7", "db.internal"}
	for _, row := range []struct {
		name      string
		err       error
		want      int
		says      string
		challenge string
	}{
		{"a grant that has ended", requests.ErrGrantNotLive, http.StatusProxyAuthRequired, requests.ErrGrantNotLive.Error(), `Basic realm="airlock"`},
		{"a read the store did not answer", databaseDown, http.StatusServiceUnavailable, grantUnreadable, ""},
	} {
		t.Run(row.name+", on the CONNECT", func(t *testing.T) {
			r := newRig(t, Options{})
			r.grants.refuseConnection(workerPlaceholder, row.err)
			answer, _, _ := r.connectRaw(net.JoinHostPort(boundHost, "443"), basic(workerPlaceholder))
			body := bodyOf(t, answer)
			if answer.StatusCode != row.want || !strings.Contains(body, row.says) {
				t.Fatalf("the CONNECT answered %d %q; want %d naming %q", answer.StatusCode, body, row.want, row.says)
			}
			if got := answer.Header.Get("Proxy-Authenticate"); got != row.challenge {
				t.Errorf("Proxy-Authenticate = %q; want %q: a credential is asked for again only where the grant itself refused", got, row.challenge)
			}
			for _, word := range store {
				if strings.Contains(body, word) {
					t.Errorf("the answer carries %q of the store's own error; want nothing of it", word)
				}
			}
			if dialed := r.dialedAddresses(); len(dialed) != 0 {
				t.Fatalf("the proxy dialled %v; want nothing: a CONNECT this port could not decide opens nothing", dialed)
			}
		})
		t.Run(row.name+", on an open connection", func(t *testing.T) {
			r := newRig(t, Options{})
			client := r.client(workerPlaceholder)
			if answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder); answer.StatusCode != http.StatusOK {
				t.Fatalf("the first call answered %d %q; want 200", answer.StatusCode, body)
			}
			r.grants.refuseConnection(workerPlaceholder, row.err)
			// The same client, so the connection the first call left open: no CONNECT is made
			// for this one and the per-request read is the whole of what answers it.
			answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != row.want || !strings.Contains(body, row.says) {
				t.Fatalf("the second call answered %d %q; want %d naming %q", answer.StatusCode, body, row.want, row.says)
			}
			for _, word := range store {
				if strings.Contains(body, word) {
					t.Errorf("the answer carries %q of the store's own error; want nothing of it", word)
				}
			}
			if seen := r.requests(); len(seen) != 1 {
				t.Fatalf("the service was sent %d requests; want the one from before the read failed", len(seen))
			}
		})
	}
}

// TestARequestThatIsNoProxyRequestIsRefused: this port serves nothing of its own.
func TestARequestThatIsNoProxyRequestIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + r.addr + "/healthz")
	if err != nil {
		t.Fatalf("GET the proxy port: %v", err)
	}
	body := bodyOf(t, answer)
	if answer.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not a proxy request") {
		t.Fatalf("an ordinary request to the proxy port answered %d %q; want 400", answer.StatusCode, body)
	}
}

// testClock is the clock a row drives Options.Now with: read on the proxy's goroutines and set
// on the test's, so it is taken under a mutex.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// cachedNames is the names a grant's held values still carry, which is what DropName changes.
// Read through the same mutex the proxy's own paths take.
func cachedNames(s *Server, grantID string) []string {
	s.valuesMu.Lock()
	held := s.values[grantID]
	s.valuesMu.Unlock()
	if held == nil {
		return nil
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	return slices.Sorted(maps.Keys(held.values))
}

// TestAMintedValueIsMintedAgainBeforeItExpires: a minted token the proxy holds until it lapses
// would be handed to a call that then fails on it, so a value within mintAgainBefore of its
// expiry is read - and so minted - again. A value further off than that is not, since the read
// costs the whole of the release checks.
func TestAMintedValueIsMintedAgainBeforeItExpires(t *testing.T) {
	const (
		firstToken  = "ya2" + "9.the-first-access-token"
		secondToken = "ya2" + "9.the-second-access-token"
	)
	clock := &testClock{now: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	expires := clock.read().Add(time.Hour)
	r := newRig(t, Options{Now: clock.read})
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Slots: map[string]string{requests.SlotValue: firstToken}, Expires: expires, Type: "google-login"},
	})
	client := r.client(workerPlaceholder)
	call := func(path string) string {
		t.Helper()
		answer, body := r.get(client, "https://"+boundHost+path, "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the call to %s answered %d %q; want 200", path, answer.StatusCode, body)
		}
		seen := r.requests()
		return seen[len(seen)-1].Header.Get("Authorization")
	}

	if got, want := call("/v1/first"), "Bearer "+firstToken; got != want {
		t.Fatalf("the service saw Authorization %q; want %q", got, want)
	}
	// Six minutes off its expiry: further than mintAgainBefore, so the held value stands and
	// the release checks are not run again.
	clock.set(expires.Add(-6 * time.Minute))
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Slots: map[string]string{requests.SlotValue: secondToken}, Expires: expires.Add(time.Hour), Type: "google-login"},
	})
	if got, want := call("/v1/second"), "Bearer "+firstToken; got != want {
		t.Fatalf("the service saw Authorization %q six minutes before the expiry; want the held %q", got, want)
	}
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times while the token had six minutes left; want once", calls)
	}

	// Four minutes off: inside mintAgainBefore, so the grant is read again and the call goes
	// out on the token that read minted.
	clock.set(expires.Add(-4 * time.Minute))
	if got, want := call("/v1/third"), "Bearer "+secondToken; got != want {
		t.Fatalf("the service saw Authorization %q four minutes before the expiry; want the freshly minted %q", got, want)
	}
	if calls := r.grants.valueReads(testGrantID); calls != 2 {
		t.Fatalf("AirlockValues ran %d times; want twice: once for the first call and once inside the five minutes", calls)
	}

	// And the second answer is held in its turn: its own expiry is an hour further off.
	if got, want := call("/v1/fourth"), "Bearer "+secondToken; got != want {
		t.Fatalf("the service saw Authorization %q; want the held %q", got, want)
	}
	if calls := r.grants.valueReads(testGrantID); calls != 2 {
		t.Fatalf("AirlockValues ran %d times; want twice: the second answer is held as the first was", calls)
	}
}

// TestAnEndedStoredGrantsValuesAreDropped: a grant released from a stored secret ends at the end
// its connection carries, as a minted capability's does, and nothing extends it, so the read that
// adds a new grant's entry drops the entry of a stored grant whose end the pod's clock has reached,
// keeps every live grant's entry, the older ones' too, and a grant's with no end at all, and reads
// nothing to do it (TestAnEndedMintedGrantsValuesAreDropped is the minted half, with the same
// script).
func TestAnEndedStoredGrantsValuesAreDropped(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	r := newRig(t, Options{Now: clock.read})
	// Four stored grants: the first ends a quarter of an hour in, the next two an hour after it,
	// and the last has no end (its connection carries no Expires: it lasts until its session ends).
	type storedGrant struct {
		id, placeholder string
		ends            time.Time
	}
	grants := []storedGrant{
		{"grant-00000011", placeholder.Prefix + "QqRrSsTtUuVvWwXxYyZz0123456789cd", clock.read().Add(15 * time.Minute)},
		{"grant-00000012", placeholder.Prefix + "QqRrSsTtUuVvWwXxYyZz0123456789ef", clock.read().Add(time.Hour)},
		{"grant-00000013", placeholder.Prefix + "QqRrSsTtUuVvWwXxYyZz0123456789gh", clock.read().Add(time.Hour)},
		{"grant-00000014", placeholder.Prefix + "QqRrSsTtUuVvWwXxYyZz0123456789ij", time.Time{}},
	}
	for _, g := range grants {
		connection := testConnection()
		connection.GrantID, connection.Expires = g.id, nil
		if !g.ends.IsZero() {
			connection.Expires = &g.ends
		}
		connection.Names = map[string]requests.AirlockGrantName{workerName: {Slots: map[string]string{requests.SlotValue: g.placeholder}}}
		r.grants.setConnection(g.placeholder, connection)
		r.grants.setValues(g.id, storedValues(workerName, workerValue))
	}
	call := func(g storedGrant) {
		t.Helper()
		answer, body := r.get(r.client(g.placeholder), "https://"+boundHost+"/v1/call", "Authorization", "Bearer "+g.placeholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the call of grant %s = %d %s, want 200", g.id, answer.StatusCode, body)
		}
	}
	holds := func(g storedGrant) bool {
		r.proxy.valuesMu.Lock()
		defer r.proxy.valuesMu.Unlock()
		_, held := r.proxy.values[g.id]
		return held
	}
	first, second, third, endless := grants[0], grants[1], grants[2], grants[3]

	call(endless)
	call(first)
	if !holds(first) || !holds(endless) {
		t.Fatalf("held: first %v, endless %v after their calls; the row cannot see a drop", holds(first), holds(endless))
	}
	// Before the first grant's end, a new grant's read leaves the first grant's values held.
	clock.set(first.ends.Add(-time.Minute))
	call(second)
	if !holds(first) || !holds(second) {
		t.Fatalf("held: first %v, second %v; want both, since neither grant has ended", holds(first), holds(second))
	}
	// At its end the broker answers the first grant's placeholder as no live grant, and the next
	// new grant's read drops the first grant's values and keeps the live ones, the older second
	// grant's among them.
	clock.set(first.ends)
	r.grants.refuseConnection(first.placeholder, requests.ErrGrantNotLive)
	call(third)
	if holds(first) {
		t.Errorf("the pod still holds the values of stored grant %s, which ended at %s, with the clock at %s", first.id, first.ends, clock.read())
	}
	if !holds(second) || !holds(third) {
		t.Errorf("held: second %v, third %v; want both, since neither grant has ended", holds(second), holds(third))
	}
	if !holds(endless) {
		t.Errorf("the pod holds no values of grant %s, which has no end and so keeps its entry until the pod rolls", endless.id)
	}
	for _, g := range []storedGrant{first, endless} {
		if reads := r.grants.valueReads(g.id); reads != 1 {
			t.Errorf("the values of grant %s were read %d times; want once, for its call", g.id, reads)
		}
	}
}

// TestDropNameDropsOneNameAndKeepsTheGrantsOthers: a reread says one secret has a new version,
// and says nothing about the rest of a grant, so the drop is of that name alone - and the next
// request reads the grant's values again, which is what puts the new version in front of it.
func TestDropNameDropsOneNameAndKeepsTheGrantsOthers(t *testing.T) {
	const (
		alphaName        = "ALPHA_TOKEN"
		betaName         = "BETA_TOKEN"
		alphaPlaceholder = placeholder.Prefix + "alphaalphaalphaalphaalphaalpha01"
		betaPlaceholder  = placeholder.Prefix + "betabetabetabetabetabetabetabe01"
	)
	r := newRuledRig(t, twoNameRules(alphaName, betaName), Options{})
	r.grants.connections[alphaPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			alphaName: {Slots: map[string]string{requests.SlotValue: alphaPlaceholder}},
			betaName:  {Slots: map[string]string{requests.SlotValue: betaPlaceholder}},
		},
	}
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		alphaName: {Slots: map[string]string{requests.SlotValue: "alpha-v1"}},
		betaName:  {Slots: map[string]string{requests.SlotValue: "beta-v1"}},
	})
	client := r.client(alphaPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+alphaPlaceholder)
	if want := []string{alphaName, betaName}; !slices.Equal(cachedNames(r.proxy, testGrantID), want) {
		t.Fatalf("the grant holds %v; want %v after its first request", cachedNames(r.proxy, testGrantID), want)
	}

	// A name no grant on this pod holds changes nothing, and a held one leaves the rest.
	r.proxy.DropName("A_NAME_NO_GRANT_HOLDS")
	if want := []string{alphaName, betaName}; !slices.Equal(cachedNames(r.proxy, testGrantID), want) {
		t.Fatalf("the grant holds %v after a reread of another secret; want %v", cachedNames(r.proxy, testGrantID), want)
	}
	r.proxy.DropName(betaName)
	if want := []string{alphaName}; !slices.Equal(cachedNames(r.proxy, testGrantID), want) {
		t.Fatalf("the grant holds %v after beta was reread; want %v: the drop is of that name alone", cachedNames(r.proxy, testGrantID), want)
	}
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times; want once: a drop reads nothing by itself", calls)
	}

	// The next request reads the grant's values again, and carries what that read answered.
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		alphaName: {Slots: map[string]string{requests.SlotValue: "alpha-v1"}},
		betaName:  {Slots: map[string]string{requests.SlotValue: "beta-v2"}},
	})
	r.get(client, "https://"+boundHost+"/v1/other", "X-Beta", "Token "+betaPlaceholder)
	if calls := r.grants.valueReads(testGrantID); calls != 2 {
		t.Fatalf("AirlockValues ran %d times; want twice: the request after a drop reads the grant again", calls)
	}
	seen := r.requests()
	if got, want := seen[len(seen)-1].Header.Get("X-Beta"), "Token beta-v2"; got != want {
		t.Fatalf("the service saw X-Beta %q; want %q, the version the reread brought", got, want)
	}
	// And the read that followed the drop put every name back.
	if want := []string{alphaName, betaName}; !slices.Equal(cachedNames(r.proxy, testGrantID), want) {
		t.Fatalf("the grant holds %v; want %v", cachedNames(r.proxy, testGrantID), want)
	}
}

// TestAPolicyChangeTheReloadTookReachesTheValuesThisPodHolds: the one thing that dropped a held
// value was the reread of one name (DropName), which `agent-secrets secret set` drives. A change
// made any other way - a console or infra-applier retag that takes a session off requesters, a
// raised tier, a disabled version, or a peer's reread this pod missed while its LISTEN
// connection was down - reaches the broker by its periodic reload, which moves the policy
// version and nothing else, so a pod that had already read a grant's values went on swapping
// them into every new connection until the grant ended. The held values are read again once
// that version moves, and are not read again while it stands; the read runs the full release
// checks, which is what ends a grant the policy now refuses.
func TestAPolicyChangeTheReloadTookReachesTheValuesThisPodHolds(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times for the first request; want once", calls)
	}

	// A new value behind the same policy version is not read: nothing says the pod's answer is
	// stale, and reading the grant per request is what AirlockValues' own contract forbids.
	r.grants.setValues(testGrantID, storedValues(workerName, "value-v2"))
	r.get(client, "https://"+boundHost+"/v1/second", "Authorization", "Bearer "+workerPlaceholder)
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times while the policy version stood; want once: nothing had changed", calls)
	}
	seen := r.requests()
	if got, want := seen[len(seen)-1].Header.Get("Authorization"), "Bearer "+workerValue; got != want {
		t.Fatalf("the service saw Authorization %q; want the held %q", got, want)
	}

	// The reload took a policy change: the version moves, and the next request reads the grant
	// again and carries what that read answered.
	r.grants.setPolicyVersion("policy-v2")
	r.get(client, "https://"+boundHost+"/v1/third", "Authorization", "Bearer "+workerPlaceholder)
	if calls := r.grants.valueReads(testGrantID); calls != 2 {
		t.Fatalf("AirlockValues ran %d times; want twice: the request after the policy moved reads the grant again", calls)
	}
	seen = r.requests()
	if got, want := seen[len(seen)-1].Header.Get("Authorization"), "Bearer value-v2"; got != want {
		t.Fatalf("the service saw Authorization %q after the policy moved; want %q, what the new read answered", got, want)
	}

	// And that answer is held in its turn, under the version it was read at.
	r.get(client, "https://"+boundHost+"/v1/fourth", "Authorization", "Bearer "+workerPlaceholder)
	if calls := r.grants.valueReads(testGrantID); calls != 2 {
		t.Fatalf("AirlockValues ran %d times; want twice: the second answer is held as the first was", calls)
	}
}

// TestKeepValuesHoldsAValueAcrossAReread is the negative control of the check that a new version
// of a stored secret reaches a running command: with Options.KeepValues the proxy must go on
// carrying the value it already holds, so a check that turns the switch on and sees the old value
// has proved the switch is what the behaviour hangs on. Dropping the name and then holding what
// is left would read the grant again and find the new value, which is the defect this row pins.
func TestKeepValuesHoldsAValueAcrossAReread(t *testing.T) {
	const placeholder = placeholder.Prefix + "keepkeepkeepkeepkeepkeepkeepke01"
	r := newRig(t, Options{KeepValues: true})
	r.grants.connections[placeholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			workerName: {Slots: map[string]string{requests.SlotValue: placeholder}},
		},
	}
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Slots: map[string]string{requests.SlotValue: "value-v1"}},
	})
	client := r.client(placeholder)
	r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+placeholder)

	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		workerName: {Slots: map[string]string{requests.SlotValue: "value-v2"}},
	})
	r.proxy.DropName(workerName)
	r.get(client, "https://"+boundHost+"/v1/other", "Authorization", "Bearer "+placeholder)

	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times; want once: with KeepValues a reread reads nothing again", calls)
	}
	seen := r.requests()
	if got, want := seen[len(seen)-1].Header.Get("Authorization"), "Bearer value-v1"; got != want {
		t.Fatalf("the service saw Authorization %q after the reread; want %q, the value the proxy already held", got, want)
	}
}

// TestAGitHubAppInstallationsTokenIsSwappedForItsOwnSlotsPlaceholder: a github-app name holds
// one token per installation and no value slot, so what a request is handled for is the (name,
// slot) pair whose placeholder it carries - the owner the command asked for - and the value
// swapped in is that installation's own token.
func TestAGitHubAppInstallationsTokenIsSwappedForItsOwnSlotsPlaceholder(t *testing.T) {
	const (
		appName    = "GITHUB_APP_KEY"
		acmeSlot   = requests.SlotGitHubPrefix + "acme"
		zedSlot    = requests.SlotGitHubPrefix + "zed"
		acmeHolder = placeholder.Prefix + "acmeacmeacmeacmeacmeacmeacmeac01"
		zedHolder  = placeholder.Prefix + "zedzedzedzedzedzedzedzedzedzed01"
		acmeToken  = "ghs_the-real-acme-installation-token"
		zedToken   = "ghs_the-real-zed-installation-token"
	)
	// A github-app name is minted and swapped in (way mint), here on GET under /v1/ alone.
	rules := proxyrules.Rules{
		Kinds: []proxyrules.Kind{{
			Name: "github", Hosts: []string{boundHost}, Headers: []string{"authorization"}, Ways: []string{proxyrules.WayMint},
			Allow: []proxyrules.Allow{{Method: http.MethodGet, PathPrefix: "/v1/"}},
		}},
		Names: []proxyrules.Binding{{Name: appName, Kind: "github", Way: proxyrules.WayMint}},
	}
	r := newRuledRig(t, rules, Options{})
	r.grants.connections[acmeHolder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			appName: {Type: "github-app", Slots: map[string]string{acmeSlot: acmeHolder, zedSlot: zedHolder}},
		},
	}
	r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
		appName: {Type: "github-app", Slots: map[string]string{acmeSlot: acmeToken, zedSlot: zedToken}},
	})
	client := r.client(acmeHolder)

	// zed's placeholder: zed's token, and the line names the slot so an operator reads which
	// installation the call used.
	r.get(client, "https://"+boundHost+"/v1/repos", "Authorization", "Bearer "+zedHolder)
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want one", len(seen))
	}
	if got, want := seen[0].Header.Get("Authorization"), "Bearer "+zedToken; got != want {
		t.Fatalf("the service saw Authorization %q; want %q: the request carried zed's placeholder", got, want)
	}
	line := r.logs.callsAtLeast(t, 1)[0]
	if line.fields["secret"] != appName || line.fields["slot"] != zedSlot {
		t.Fatalf("the call logged %v; want secret %s and slot %s", line.fields, appName, zedSlot)
	}
	for _, never := range []string{acmeToken, zedToken, acmeHolder, zedHolder} {
		if strings.Contains(fmt.Sprint(line.fields), never) {
			t.Errorf("the call line names a value or a placeholder: %v", line.fields)
		}
	}

	// acme's placeholder on the same connection: acme's token, for the same reason.
	r.get(client, "https://"+boundHost+"/v1/issues", "Authorization", "Bearer "+acmeHolder)
	seen = r.requests()
	if got, want := seen[1].Header.Get("Authorization"), "Bearer "+acmeToken; got != want {
		t.Fatalf("the service saw Authorization %q; want %q: the request carried acme's placeholder", got, want)
	}
	if slot := r.logs.callsAtLeast(t, 2)[1].fields["slot"]; slot != acmeSlot {
		t.Fatalf("the call logged the slot %v; want %s", slot, acmeSlot)
	}

	// A request that carries no placeholder of the grant is still decided, for the first
	// candidate pair in (name, slot) order, and forwarded with nothing swapped.
	r.get(client, "https://"+boundHost+"/v1/plain", "X-Plain", "nothing of this grant")
	seen = r.requests()
	if got := seen[2].Header.Get("X-Plain"); got != "nothing of this grant" {
		t.Errorf("the service saw X-Plain %q; want it unchanged", got)
	}
	if slot := r.logs.callsAtLeast(t, 3)[2].fields["slot"]; slot != acmeSlot {
		t.Fatalf("the call logged the slot %v; want %s, the first candidate in slot order", slot, acmeSlot)
	}
}

// TestTheLogLineNamesNoSlotForAValueSlot: every name whose value is one string fills the value
// slot, which says nothing, so the line carries the slot only where the name has more than one.
func TestTheLogLineNamesNoSlotForAValueSlot(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	line := r.logs.callsAtLeast(t, 1)[0]
	if _, named := line.fields["slot"]; named {
		t.Fatalf("the call line names a slot for a stored secret: %v", line.fields)
	}
	if line.fields["secret"] != workerName {
		t.Fatalf("the call line's secret = %v; want %s", line.fields["secret"], workerName)
	}
}

// TestANameIsDialledWhereItWasChecked: the refusal of a private address is worth nothing if the
// name is looked up again to dial it. A name under the caller's control answers a public address
// to the check and the node's metadata server to the lookup after it - DNS rebinding - and what
// must reach the socket is the address that was checked, or nothing. The second half is the
// other shape of the same trick: one answer holding a public address beside a private one, which
// is refused whole, since which of them a dialer picked would otherwise decide what the proxy
// reached.
func TestANameIsDialledWhereItWasChecked(t *testing.T) {
	const rebindHost = "rebind.example.net"
	const checked, metadata = "93.184.216.40", "169.254.169.254"

	t.Run("a name that answers another address after the check is dialled where it was checked", func(t *testing.T) {
		r := newRig(t, Options{})
		// One answer for the check and the metadata server for every lookup after it. Only
		// the checked address is wired to a service, so a dial of the second answer is a
		// dial this rig refuses and the tunnel would not open.
		r.answers[rebindHost] = [][]netip.Addr{
			{netip.MustParseAddr(checked)},
			{netip.MustParseAddr(metadata)},
		}
		r.dialTo[net.JoinHostPort(checked, "443")] = r.dialTo[net.JoinHostPort(echoHost, "443")]

		answer, conn, reader := r.connectRaw(net.JoinHostPort(rebindHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT %s = %d %q; want 200 through the address the check read", rebindHost, answer.StatusCode, bodyOf(t, answer))
		}
		// The bytes prove which service is on the other end: the echo the checked address
		// is wired to.
		if _, err := conn.Write([]byte("through the checked address\n")); err != nil {
			t.Fatalf("write through the tunnel: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if line, err := reader.ReadString('\n'); err != nil || line != "through the checked address\n" {
			t.Fatalf("the tunnel answered %q, %v; want the echo of what was written", line, err)
		}
		dialed := r.dialedAddresses()
		if len(dialed) != 1 || dialed[0] != net.JoinHostPort(checked, "443") {
			t.Fatalf("the proxy dialled %v; want the one address it checked, %s", dialed, net.JoinHostPort(checked, "443"))
		}
		for _, addr := range dialed {
			if strings.HasPrefix(addr, metadata) {
				t.Fatalf("the proxy dialled the metadata server at %s: the name was resolved again for the dial", addr)
			}
		}
	})

	t.Run("a name whose answer holds a private address beside a public one is refused and nothing is dialled", func(t *testing.T) {
		r := newRig(t, Options{})
		r.answers[rebindHost] = [][]netip.Addr{{netip.MustParseAddr(checked), netip.MustParseAddr(metadata)}}
		r.dialTo[net.JoinHostPort(checked, "443")] = r.dialTo[net.JoinHostPort(echoHost, "443")]

		answer, _, _ := r.connectRaw(net.JoinHostPort(rebindHost, "443"), basic(workerPlaceholder))
		body := bodyOf(t, answer)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "a link-local IPv4 address") {
			t.Fatalf("CONNECT %s = %d %q; want 403 naming the link-local address among the name's answers", rebindHost, answer.StatusCode, body)
		}
		if dialed := r.dialedAddresses(); len(dialed) != 0 {
			t.Fatalf("the proxy dialled %v for a refused name; want nothing: a name is refused whole", dialed)
		}
	})
}

// TestTheGrantIsReadAgainForEveryRequestOfABoundConnection: the grant is what a connection may
// carry, and a connection outlives the grant - a command sending one request every few minutes
// holds one open for as long as it likes, and the idle limit never fires. So a grant that has
// ended, been revoked or had a name withheld must stop at the next request on that connection,
// and the service must see nothing of it.
func TestTheGrantIsReadAgainForEveryRequestOfABoundConnection(t *testing.T) {
	t.Run("a grant that has ended between two requests of one connection is 407", func(t *testing.T) {
		r := newRig(t, Options{})
		client := r.client(workerPlaceholder)
		answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the first call answered %d %q; want 200", answer.StatusCode, body)
		}
		// The grant ends: every later read of this password is the broker's own refusal.
		r.grants.refuseConnection(workerPlaceholder, requests.ErrGrantNotLive)

		// The same client, so the same connection the first call left open: no CONNECT is
		// made for this one, and nothing but a per-request read can refuse it.
		answer, body = r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("the call after the grant ended answered %d %q; want 407", answer.StatusCode, body)
		}
		if !strings.Contains(body, requests.ErrGrantNotLive.Error()) {
			t.Errorf("the refusal reads %q; want the broker's own words for an ended grant", body)
		}
		if seen := r.requests(); len(seen) != 1 {
			t.Fatalf("the service was sent %d requests; want the one from before the grant ended", len(seen))
		}
		if reads := r.grants.connectionReads(); reads < 3 {
			t.Fatalf("the password was read as a grant %d times; want one per request beside the CONNECT's own", reads)
		}
	})

	t.Run("a name withheld between two requests stops while the grant's other name goes on", func(t *testing.T) {
		const (
			alphaName        = "ALPHA_TOKEN"
			betaName         = "BETA_TOKEN"
			alphaValue       = "the-real-alpha"
			betaValue        = "the-real-beta"
			alphaPlaceholder = placeholder.Prefix + "alphaalphaalphaalphaalphaalpha01"
			betaPlaceholder  = placeholder.Prefix + "betabetabetabetabetabetabetabe01"
		)
		both := requests.AirlockConnection{
			GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
			Names: map[string]requests.AirlockGrantName{
				alphaName: {Slots: map[string]string{requests.SlotValue: alphaPlaceholder}},
				betaName:  {Slots: map[string]string{requests.SlotValue: betaPlaceholder}},
			},
		}
		r := newRuledRig(t, twoNameRules(alphaName, betaName), Options{})
		r.grants.setConnection(alphaPlaceholder, both)
		r.grants.setValues(testGrantID, map[string]requests.AirlockSecret{
			alphaName: {Slots: map[string]string{requests.SlotValue: alphaValue}},
			betaName:  {Slots: map[string]string{requests.SlotValue: betaValue}},
		})
		client := r.client(alphaPlaceholder)
		if answer, body := r.get(client, "https://"+boundHost+"/v1/things", "X-Beta", "Token "+betaPlaceholder); answer.StatusCode != http.StatusOK {
			t.Fatalf("the first call answered %d %q; want 200", answer.StatusCode, body)
		}

		// The operator withholds beta. The connection stays open and alpha is still the
		// grant's, so what must change is beta's placeholder and nothing else.
		withheld := both
		withheld.Names = map[string]requests.AirlockGrantName{
			alphaName: {Slots: map[string]string{requests.SlotValue: alphaPlaceholder}},
		}
		r.grants.setConnection(alphaPlaceholder, withheld)

		answer, body := r.get(client, "https://"+boundHost+"/v1/things", "X-Beta", "Token "+betaPlaceholder)
		if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "placeholder") {
			t.Fatalf("the call carrying the withheld name's placeholder answered %d %q; want 403: beta is withheld, so no name swaps its placeholder and it does not leave the proxy", answer.StatusCode, body)
		}
		if seen := r.requests(); len(seen) != 1 {
			t.Fatalf("the service saw %d requests; want the first alone", len(seen))
		}

		// And alpha's own placeholder on that same connection still gets alpha's value.
		r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+alphaPlaceholder)
		seen := r.requests()
		if got, want := seen[1].Header.Get("Authorization"), "Bearer "+alphaValue; got != want {
			t.Fatalf("the service saw Authorization %q; want %q: the grant's other name goes on", got, want)
		}
	})
}

// TestATunnelToAServiceThatNeverClosesIsGoneWithinTheIdleLimit: the command's side alone cannot
// bound a tunnel. A command that half-closes its side and a service that reads nothing, answers
// nothing and closes nothing would leave the upstream socket, both copy goroutines and the
// registry's entry for as long as the pod lives, which is a command opening tunnels to its own
// service until the broker runs out of file descriptors - the process that also serves the
// broker's API.
func TestATunnelToAServiceThatNeverClosesIsGoneWithinTheIdleLimit(t *testing.T) {
	const idle = 400 * time.Millisecond
	const sinkHost = "sink.example.net"
	r := newRig(t, Options{Idle: idle})
	sink, accepted := startSink(t)
	r.addresses[sinkHost] = []netip.Addr{netip.MustParseAddr("93.184.216.41")}
	r.dialTo[net.JoinHostPort(sinkHost, "443")] = sink

	answer, conn, _ := r.connectRaw(net.JoinHostPort(sinkHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", sinkHost, answer.StatusCode)
	}
	if _, err := conn.Write([]byte("everything this command has to say\n")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	upstream := <-accepted
	// The command says everything it has to say and closes its half, as git's upload-pack and
	// a shell pipeline do. The service answers nothing, so nothing crosses the tunnel again.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close the command's side: %v", err)
	}

	// Within a few idle windows: no entry in the registry the drain reads, no goroutine of the
	// tunnel left, and the service's own connection closed.
	deadline := time.Now().Add(20 * idle)
	for {
		if openTunnels(r.proxy) == 0 && !goroutinesIn(t, "connectTunnel") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v after a %v idle limit the proxy still holds %d connection(s) and %v tunnel goroutines",
				20*idle, idle, openTunnels(r.proxy), goroutinesIn(t, "connectTunnel"))
		}
		time.Sleep(idle / 10)
	}
	upstream.SetReadDeadline(time.Now().Add(5 * time.Second))
	// What the command wrote is still in this socket's buffer, unread by a service that reads
	// nothing; what the row is about is what comes after it, which must be the close - an end
	// of file, or the reset of a connection closed with bytes still unread.
	if _, err := io.Copy(io.Discard, upstream); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the service's own connection was still open after the tunnel ended; want it closed with both ends")
	}
}

// openTunnels is how many hijacked connections the proxy's registry holds, read through the same
// mutex the proxy's own paths take.
func openTunnels(s *Server) int {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	return len(s.open)
}

// goroutinesIn reports whether any goroutine of this process is inside the named function, which
// is how a row reads that a tunnel left none behind.
func goroutinesIn(t *testing.T, name string) bool {
	t.Helper()
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), name)
}

// startSink is a TCP service that accepts and then does nothing at all: it reads nothing, writes
// nothing and closes nothing, which is the service a tunnel must not outlive. It answers its
// address and the connections it accepted, so a row can read that its end was closed for it.
func startSink(t *testing.T) (addr string, accepted chan *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the sink service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted = make(chan *net.TCPConn, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
			accepted <- conn.(*net.TCPConn)
		}
	}()
	return listener.Addr().String(), accepted
}
