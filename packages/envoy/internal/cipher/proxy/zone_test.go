// packages/envoy/internal/cipher/proxy/zone_test.go
//
// A zone on an IPv6 address. netip keeps it ("2001:db8:5::10%eth0"), and netip.Prefix.Contains is
// false for any zoned address, while Linux connects to a zoned global address as if it had no
// zone. So an address check that reads the zone answers about an address the socket never goes
// to. Every address check of the proxy reads an address through netaddr.Canonical, and every dial
// goes to the address that was checked; each row here is one site of that census.
package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// TestResolveHostAnswersALiteralWithNoZone: ResolveHost is the proxy's own resolver, and a host
// that is an address literal answers itself, as the address every check below reads.
func TestResolveHostAnswersALiteralWithNoZone(t *testing.T) {
	for host, want := range map[string]netip.Addr{
		"2001:db8:5::10%eth0": netip.MustParseAddr("2001:db8:5::10"),
		"fe80::1%0":           netip.MustParseAddr("fe80::1"),
		"::ffff:34.118.224.1": netip.MustParseAddr("34.118.224.1"),
	} {
		got, err := ResolveHost(t.Context(), host)
		if err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("ResolveHost(%q) = %v, %v; want [%v]", host, got, err, want)
		}
	}
}

// TestTheDialRuleReadsAZonedAddressWithoutItsZone: netaddr.DialRefusal is every rule of where the
// proxy may dial, and a caller that hands it a zoned address gets the rule of the address without
// its zone. A zoned link-local or loopback address keeps the rule it has today.
func TestTheDialRuleReadsAZonedAddressWithoutItsZone(t *testing.T) {
	ranges := []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/48")}
	for addr, want := range map[string]string{
		"2001:db8:5::10%eth0": clusterRangeRule,
		"fe80::1%eth0":        "a link-local IPv6 address",
		"::1%lo":              "a loopback IPv6 address",
	} {
		if got := netaddr.DialRefusal(netip.MustParseAddr(addr), ranges); !strings.Contains(got, want) {
			t.Errorf("DialRefusal(%s) = %q; want it to name %q", addr, got, want)
		}
	}
	if got := netaddr.DialRefusal(netip.MustParseAddr("2001:db8:6::10%eth0"), ranges); got != "" {
		t.Errorf("DialRefusal(a zoned address outside every range) = %q; want none", got)
	}
}

// TestTheProxyDialsTheAddressItChecked: a name whose answer is a zoned public address is checked
// without its zone, and the socket is opened to that same address: the check and the dial read
// one value, with no zone the check never saw.
func TestTheProxyDialsTheAddressItChecked(t *testing.T) {
	const host = "zoned.example.net"
	r := newRig(t, Options{ClusterRanges: []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/48")}})
	r.answers[host] = [][]netip.Addr{{netip.MustParseAddr("2001:db8:6::10%eth0")}}
	echo := r.dialTo[net.JoinHostPort(echoHost, "443")]
	r.dialTo["[2001:db8:6::10]:443"] = echo
	r.dialTo["[2001:db8:6::10%eth0]:443"] = echo
	answer, _, _ := r.connectRaw(net.JoinHostPort(host, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s = %d; want 200", host, answer.StatusCode)
	}
	if dialed := r.dialedAddresses(); len(dialed) != 1 || dialed[0] != "[2001:db8:6::10]:443" {
		t.Fatalf("the proxy dialled %v; want [2001:db8:6::10]:443, the address it checked", dialed)
	}
}

// TestTheTransportRefusesANameThatRebindsToAZonedClusterAddress: the transport every bound host's
// request goes out on checks the name again when it dials (dialChecked). A name that answers a
// public address to the CONNECT's check and a zoned address inside a range to that dial is refused
// there, and the service behind the range address is sent nothing.
func TestTheTransportRefusesANameThatRebindsToAZonedClusterAddress(t *testing.T) {
	r := newRig(t, Options{ClusterRanges: []netip.Prefix{netip.MustParsePrefix("2001:db8:5::/48")}})
	r.answers[boundHost] = [][]netip.Addr{{netip.MustParseAddr("93.184.216.34")}, {netip.MustParseAddr("2001:db8:5::10%eth0")}}
	r.dialTo["[2001:db8:5::10%eth0]:443"] = r.service.Addr()
	r.dialTo["[2001:db8:5::10]:443"] = r.service.Addr()
	answer, body := r.get(r.client(workerPlaceholder), "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode == http.StatusOK || !strings.Contains(body, clusterRangeRule) {
		t.Fatalf("the request answered %d %q; want it refused naming %q", answer.StatusCode, body, clusterRangeRule)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service at the range address was sent %d requests; want none", len(seen))
	}
}

// TestAZonedLiteralOfABoundAddressIsThatBoundHost: the bound-host check compares the CONNECT host
// with the hosts an entry binds, as text. A zoned literal of a bound IPv6 address is that address,
// so it is read without its zone before the comparison, and the proxy opens the connection with
// its own certificate for the address, as it does for the unzoned literal, rather than tunnelling
// it unread to whatever answers there.
func TestAZonedLiteralOfABoundAddressIsThatBoundHost(t *testing.T) {
	const bound = "2001:db8:7::1"
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{{Name: workerName, Hosts: []string{bound}, Headers: []string{"authorization"}}}}
	for _, target := range []string{"[" + bound + "]:443", "[" + bound + "%25eth0]:443"} {
		t.Run(target, func(t *testing.T) {
			r := newRig(t, Options{Rules: rules})
			tunnel := r.dialTo[net.JoinHostPort(tunnelHost, "443")]
			r.dialTo["["+bound+"]:443"] = tunnel
			r.dialTo["["+bound+"%eth0]:443"] = tunnel
			answer, conn, _ := r.connectRaw(target, basic(workerPlaceholder))
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT %s = %d; want 200", target, answer.StatusCode)
			}
			client := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
			if err := client.Handshake(); err != nil {
				t.Fatalf("the handshake on CONNECT %s: %v", target, err)
			}
			leaf := client.ConnectionState().PeerCertificates[0]
			if err := leaf.CheckSignatureFrom(r.authority.Certificate()); err != nil {
				t.Fatalf("CONNECT %s presented a certificate the proxy's authority did not sign: it was tunnelled, not read as the bound host %s", target, bound)
			}
			// The proxy's certificate names the bound host as a DNS name alone (ca.go), so the
			// zoned literal's carries the address without its zone.
			if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != bound || len(leaf.IPAddresses) != 0 {
				t.Fatalf("the proxy's certificate names DNS %v and IPs %v; want the bound host %s alone", leaf.DNSNames, leaf.IPAddresses, bound)
			}
		})
	}
}

// TestAPlainHTTPRequestToAZonedLiteralOfABoundAddressIsRefusedAsPlainHTTP: servePlain refuses
// plain HTTP to a bound host, since a value would cross in the clear, and it compares the host
// with the hosts an entry binds as text. A zoned literal of a bound IPv6 address is that address,
// so it is refused in the same words as the unzoned literal, and nothing is dialled for it. The
// request is written on a socket of the row's own (plainRaw), because net/http's client drops the
// zone before the proxy could read it.
func TestAPlainHTTPRequestToAZonedLiteralOfABoundAddressIsRefusedAsPlainHTTP(t *testing.T) {
	const bound = "2001:db8:7::1"
	rules := proxyrules.Rules{Secrets: []proxyrules.SecretRule{{Name: workerName, Hosts: []string{bound}, Headers: []string{"authorization"}}}}
	for _, authority := range []string{"[" + bound + "]", "[" + bound + "%25eth0]"} {
		t.Run(authority, func(t *testing.T) {
			r := newRig(t, Options{Rules: rules})
			// A plain service wired to the address in either spelling, so a proxy that forwarded
			// the request answers 200 in its words.
			var served atomic.Int64
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served.Add(1)
				io.WriteString(w, "the plain service answered")
			}))
			t.Cleanup(plain.Close)
			r.dialTo["["+bound+"]:80"] = plain.Listener.Addr().String()
			r.dialTo["["+bound+"%eth0]:80"] = plain.Listener.Addr().String()
			answer, body := r.plainRaw(authority, "/v1/things", basic(workerPlaceholder))
			if want := "refusing plain http to " + bound + ", a host behind the airlock"; answer.StatusCode != http.StatusForbidden || !strings.Contains(body, want) {
				t.Fatalf("plain http to %s answered %d %q after the dials %v; want 403 naming %q", authority, answer.StatusCode, body, r.dialedAddresses(), want)
			}
			if r.dials.Load() != 0 || served.Load() != 0 {
				t.Fatalf("the proxy dialled %v and the service answered %d request(s); want none", r.dialedAddresses(), served.Load())
			}
		})
	}
}
