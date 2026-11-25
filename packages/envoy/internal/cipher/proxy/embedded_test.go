// packages/envoy/internal/cipher/proxy/embedded_test.go
//
// An IPv6 address that carries an IPv4 address. The network sends a packet for such an address to
// the IPv4 address inside it, by a NAT64 translator's rewrite or inside an IPv4 packet from a 6to4
// or Teredo relay or a host that routes the IPv4-compatible form, so where such a translator or
// relay passes it on, a tunnel to 64:ff9b::a9fe:a9fe is a connection to 169.254.169.254, the
// node's metadata server. Each refused IPv4 address is refused in every such form, with the bare
// address's own rule; the same form carrying a public IPv4 address is still dialled, and so is an
// IPv6 address that carries none.
package proxy

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
)

// teredoPublic is a public IPv4 address, the half of a Teredo address a row does not make the
// refused one: the server of a client row and the client of a server row.
var teredoPublic = [4]byte{65, 54, 227, 120}

// teredo is the Teredo address (2001::/32) of client behind server: the server's address follows
// the prefix, the client's is the last 32 bits with every bit inverted, and between them are the
// flags and the client's obscured port, which no rule reads.
func teredo(server, client [4]byte) netip.Addr {
	a := [16]byte{0x20, 0x01, 0x00, 0x00}
	copy(a[4:], server[:])
	a[8], a[10], a[11] = 0x80, 0xfb, 0xc5
	for i, b := range client {
		a[12+i] = ^b
	}
	return netip.AddrFrom16(a)
}

// embeddingForms are the IPv6 forms that carry an IPv4 address: each form's name, the words a
// refusal names the carried address with (none for the IPv4-mapped form, which is read as the
// IPv4 address it holds and refused in that address's own words), and the address that carries
// v4 in that form.
var embeddingForms = []struct {
	name, words string
	carry       func(v4 [4]byte) netip.Addr
}{
	{"NAT64 well-known 64:ff9b::/96", "the NAT64 form (64:ff9b::/96) of", func(v4 [4]byte) netip.Addr {
		a := [16]byte{0x00, 0x64, 0xff, 0x9b}
		copy(a[12:], v4[:])
		return netip.AddrFrom16(a)
	}},
	{"6to4 2002::/16", "the 6to4 form (2002::/16) of", func(v4 [4]byte) netip.Addr {
		a := [16]byte{0x20, 0x02}
		copy(a[2:], v4[:])
		a[15] = 1
		return netip.AddrFrom16(a)
	}},
	{"IPv4-compatible ::/96", "the IPv4-compatible form (::/96) of", func(v4 [4]byte) netip.Addr {
		var a [16]byte
		copy(a[12:], v4[:])
		return netip.AddrFrom16(a)
	}},
	{"IPv4-mapped ::ffff:0:0/96", "", func(v4 [4]byte) netip.Addr {
		return netip.AddrFrom16(netip.AddrFrom4(v4).As16())
	}},
	{"Teredo 2001::/32, its client", "a Teredo address (2001::/32) whose client is", func(v4 [4]byte) netip.Addr {
		return teredo(teredoPublic, v4)
	}},
	{"Teredo 2001::/32, its server", "a Teredo address (2001::/32) whose server is", func(v4 [4]byte) netip.Addr {
		return teredo(v4, teredoPublic)
	}},
}

// refusalOf is the whole answer localRule gives an address that carries v4 in the form whose
// words are words, when bare is v4's own rule: the form's words, v4 and its rule; and bare alone
// for the IPv4-mapped form, which is v4 itself.
func refusalOf(words, v4, bare string) string {
	if words == "" {
		return bare
	}
	return words + " " + v4 + ", " + bare
}

// localUse are three addresses of the local-use translation prefix 64:ff9b:1::/48 that carry v4
// where a translator inside it could read it: in the last 32 bits of 64:ff9b:1::/96, in the last
// 32 bits of 64:ff9b:1:fffe::/96, one of RFC 8215's own example prefixes, and where RFC 6052 puts
// it under a /48 prefix, bits 48 to 63 and 72 to 87 either side of the u-octet. Where a translator
// inside the /48 reads its IPv4 address depends on the prefix its network gave it, which the
// address alone does not say, so every address of the /48 is refused, whatever it seems to carry.
func localUse(v4 [4]byte) []netip.Addr {
	first := [16]byte{0x00, 0x64, 0xff, 0x9b, 0x00, 0x01}
	copy(first[12:], v4[:])
	example := first
	example[6], example[7] = 0xff, 0xfe
	slash48 := [16]byte{0x00, 0x64, 0xff, 0x9b, 0x00, 0x01, v4[0], v4[1], 0x00, v4[2], v4[3]}
	return []netip.Addr{netip.AddrFrom16(first), netip.AddrFrom16(example), netip.AddrFrom16(slash48)}
}

// refusedIPv4 are IPv4 addresses localRule refuses, one for each of its rules a form could
// otherwise carry a tunnel past: among them 0.1.2.3 and 0.255.255.255, the last address, of
// 0.0.0.0/8, "this network" (RFC 1122), of which Go's IsGlobalUnicast excludes only 0.0.0.0; and
// last a ClusterIP, refused only by the cluster ranges.
var refusedIPv4 = []string{"169.254.169.254", "10.0.0.5", "127.0.0.1", "100.64.1.2", "0.1.2.3", "0.255.255.255", clusterIPs[0]}

// publicIPv4 is the control of every form: a public address, which each form still carries.
const publicIPv4 = "8.8.8.8"

// allowedIPv4 are the public addresses each form still carries: publicIPv4 and 1.0.0.0, the first
// address after 0.0.0.0/8, so the refusal of that block stops at its edge.
var allowedIPv4 = []string{publicIPv4, "1.0.0.0"}

// publicIPv6 are IPv6 addresses that carry no IPv4 address, two public resolvers'; each is
// dialled as it is today.
var publicIPv6 = []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}

// beyondForms are addresses just outside a form's prefix, each holding the bytes of a refused
// address where that form carries one: ::1:a9fe:a9fe outside ::/96, 64:ff9b::1:a9fe:a9fe outside
// 64:ff9b::/96, 64:ff9b:2::a9fe:a9fe outside 64:ff9b:1::/48, and 2001:1:a9fe:a9fe::, inside
// 2001::/23 and outside Teredo's 2001::/32, whose server half is 169.254.169.254 and whose client
// half is 255.255.255.255. None of them is in a form, so no rule refuses one: each form is read up
// to its prefix's edge and no further.
var beyondForms = []string{"::1:a9fe:a9fe", "64:ff9b::1:a9fe:a9fe", "64:ff9b:2::a9fe:a9fe", "2001:1:a9fe:a9fe::"}

// localUseRule is the words of the refusal of the whole local-use translation prefix.
const localUseRule = "64:ff9b:1::/48"

// TestLocalRuleRefusesEveryIPv6FormOfARefusedIPv4Address: localRule refuses an address that
// carries a refused IPv4 address in any form, and its answer names the form, the address carried
// and that address's own rule, so an operator reads which rule refused it and why. An IPv4-mapped
// address is the IPv4 address it holds (netaddr.Canonical), so its answer is the bare address's
// answer. A form that carries a public IPv4 address, an IPv6 address that carries none, and an
// address just outside a form are refused by no rule.
func TestLocalRuleRefusesEveryIPv6FormOfARefusedIPv4Address(t *testing.T) {
	ranges := []netip.Prefix{serviceRange}
	for _, form := range embeddingForms {
		t.Run(form.name, func(t *testing.T) {
			for _, v4 := range refusedIPv4 {
				bare := netip.MustParseAddr(v4)
				rule := localRule(bare, ranges)
				if rule == "" {
					t.Errorf("localRule(%s) = \"\"; want a refusal", v4)
					continue
				}
				v6 := form.carry(bare.As4())
				if got, want := localRule(v6, ranges), refusalOf(form.words, v4, rule); got != want {
					t.Errorf("localRule(%s) = %q; want %q", v6, got, want)
				}
			}
			for _, v4 := range allowedIPv4 {
				v6 := form.carry(netip.MustParseAddr(v4).As4())
				if got := localRule(v6, ranges); got != "" {
					t.Errorf("localRule(%s) = %q; want none: it carries %s, a public address", v6, got, v4)
				}
			}
		})
	}
	t.Run("local-use 64:ff9b:1::/48", func(t *testing.T) {
		v6s := []netip.Addr{netip.MustParseAddr("64:ff9b:1:ffff:ffff:ffff:ffff:ffff")}
		for _, v4 := range slices.Concat(refusedIPv4, allowedIPv4) {
			v6s = append(v6s, localUse(netip.MustParseAddr(v4).As4())...)
		}
		for _, v6 := range v6s {
			if got := localRule(v6, ranges); !strings.Contains(got, localUseRule) {
				t.Errorf("localRule(%s) = %q; want it to name %q", v6, got, localUseRule)
			}
		}
	})
	t.Run("an IPv6 address in no form", func(t *testing.T) {
		for _, v6 := range slices.Concat(publicIPv6, beyondForms) {
			if got := localRule(netip.MustParseAddr(v6), ranges); got != "" {
				t.Errorf("localRule(%s) = %q; want none", v6, got)
			}
		}
	})
}

// TestTheProxyOpensNoTunnelToAnIPv6FormOfARefusedIPv4Address: the rule holds where it matters,
// at a CONNECT of an address that carries the metadata server's address, a private address or a
// ClusterIP, which is answered 403 naming the form and the address's rule before anything is
// dialled. Every address a row names is wired to the echo service, so a proxy that let one
// through opens the tunnel and answers 200 rather than failing for want of a route; the controls,
// the same forms carrying a public address and IPv6 addresses carrying none, open.
func TestTheProxyOpensNoTunnelToAnIPv6FormOfARefusedIPv4Address(t *testing.T) {
	ranges := []netip.Prefix{serviceRange}
	r := newRig(t, Options{ClusterRanges: ranges})
	echo := r.dialTo[net.JoinHostPort(echoHost, "443")]
	type target struct {
		name, want string
		addr       netip.Addr
	}
	var refused, admitted []target
	tunnelled := []string{"169.254.169.254", "10.0.0.5", clusterIPs[0]}
	public := netip.MustParseAddr(publicIPv4).As4()
	for _, form := range embeddingForms {
		for _, v4 := range tunnelled {
			bare := netip.MustParseAddr(v4)
			refused = append(refused, target{name: form.name + " of " + v4, want: refusalOf(form.words, v4, localRule(bare, ranges)), addr: form.carry(bare.As4())})
		}
		admitted = append(admitted, target{name: form.name + " of " + publicIPv4, addr: form.carry(public)})
	}
	for _, v4 := range append(slices.Clone(tunnelled), publicIPv4) {
		for _, addr := range localUse(netip.MustParseAddr(v4).As4()) {
			refused = append(refused, target{name: "local-use " + addr.String() + " of " + v4, want: localUseRule, addr: addr})
		}
	}
	for _, v6 := range publicIPv6 {
		admitted = append(admitted, target{name: v6, addr: netip.MustParseAddr(v6)})
	}
	// Each address is wired as the proxy dials it, which for an IPv4-mapped address is the IPv4
	// address it holds (netaddr.Canonical).
	for _, row := range slices.Concat(refused, admitted) {
		r.dialTo[net.JoinHostPort(netaddr.Canonical(row.addr).String(), "443")] = echo
	}

	for _, row := range refused {
		t.Run(row.name, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(row.addr.String(), "443"), basic(workerPlaceholder))
			// The status first: a CONNECT answered 200 is an open tunnel, whose body never ends.
			if answer.StatusCode != http.StatusForbidden {
				t.Fatalf("CONNECT %s = %d after %d dial(s); want 403 and none", row.addr, answer.StatusCode, r.dials.Load()-before)
			}
			if body := bodyOf(t, answer); !strings.Contains(body, row.want) {
				t.Errorf("the refusal of %s reads %q; want it to name %q", row.addr, body, row.want)
			}
			if after := r.dials.Load(); after != before {
				t.Fatalf("the proxy dialled %s %d time(s); want none: the refusal comes before any dial", row.addr, after-before)
			}
		})
	}
	for _, row := range admitted {
		t.Run(row.name, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(row.addr.String(), "443"), basic(workerPlaceholder))
			if answer.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT %s = %d %q; want 200: it carries no refused address", row.addr, answer.StatusCode, bodyOf(t, answer))
			}
			if after := r.dials.Load(); after != before+1 {
				t.Fatalf("the proxy dialled %s %d time(s); want one", row.addr, after-before)
			}
		})
	}
}
