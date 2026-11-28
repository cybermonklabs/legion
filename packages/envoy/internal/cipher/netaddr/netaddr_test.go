package netaddr

import (
	"errors"
	"net/netip"
	"testing"
)

// TestCanonicalDropsAZoneAndUnmaps: the one reading every address check makes. A zoned address is
// the address without its zone, an IPv4-mapped address is the IPv4 address it holds, and a zoned
// link-local or loopback address is still link-local or loopback.
func TestCanonicalDropsAZoneAndUnmaps(t *testing.T) {
	for in, want := range map[string]string{
		"2001:db8:5::10%eth0":      "2001:db8:5::10",
		"2001:db8:5::10%0":         "2001:db8:5::10",
		"::ffff:34.118.224.1":      "34.118.224.1",
		"::ffff:34.118.224.1%eth0": "34.118.224.1",
		"fe80::1%eth0":             "fe80::1",
		"34.118.224.1":             "34.118.224.1",
		"2001:db8:5::10":           "2001:db8:5::10",
	} {
		if got := Canonical(netip.MustParseAddr(in)); got != netip.MustParseAddr(want) {
			t.Errorf("Canonical(%s) = %s; want %s", in, got, want)
		}
	}
	if !Canonical(netip.MustParseAddr("fe80::1%eth0")).IsLinkLocalUnicast() {
		t.Error("a zoned link-local address is not link-local after Canonical")
	}
	if !Canonical(netip.MustParseAddr("::1%lo")).IsLoopback() {
		t.Error("a zoned loopback address is not loopback after Canonical")
	}
	if !netip.MustParsePrefix("2001:db8:5::/48").Contains(Canonical(netip.MustParseAddr("2001:db8:5::10%eth0"))) {
		t.Error("a zoned address inside a prefix is not inside it after Canonical")
	}
}

// TestHostWritesAnAddressLiteralAsCanonicalDoes: a host string is what the bound-host check
// compares and what the proxy's certificate names, so an address literal is written as Canonical
// writes it, and any other host is left as it is.
func TestHostWritesAnAddressLiteralAsCanonicalDoes(t *testing.T) {
	for in, want := range map[string]string{
		"2001:db8:7::1%eth0":   "2001:db8:7::1",
		"::ffff:93.184.216.34": "93.184.216.34",
		"93.184.216.34":        "93.184.216.34",
		"example.com":          "example.com",
		"":                     "",
		"2001:db8:7::1%":       "2001:db8:7::1%",
	} {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestDestinationRefusalReadsTheIPv4AnAddressCarries: an IPv6 address that carries an IPv4
// address - in the NAT64, 6to4, Teredo (server or client), IPv4-compatible or ISATAP form - is no
// private destination at all, whatever it carries: a refused address, a public one or a private
// one, the ISATAP form inside a unique-local prefix among them, since the network sends a packet
// for it to the address it carries (CarriedIPv4); and nor is a local-use translation address,
// whose carried address the address alone does not say. The refusal names the rule, never the
// address (DestinationNever), since the proxy's answer to the command ends with it. The control:
// a private IPv4 address and a unique-local IPv6 address an operator names as the destination
// still pass, with no cluster ranges.
func TestDestinationRefusalReadsTheIPv4AnAddressCarries(t *testing.T) {
	const (
		carries  = "an IPv6 address that carries an IPv4 address"
		localUse = "a local-use translation IPv6 address (64:ff9b:1::/48)"
	)
	for in, want := range map[string]string{
		"64:ff9b::a9fe:a9fe":          carries,
		"2002:a9fe:a9fe::1":           carries,
		"::7f00:1":                    carries,
		"2001:0:7f00:1::":             carries,
		"2001:0:a00:5:0:0:80ff:fffe":  carries,
		"64:ff9b::e000:1":             carries,
		"64:ff9b:1::a9fe:a9fe":        localUse,
		"2001:0:5db8:d822::f5ff:fffa": carries,
		"64:ff9b::a00:5":              carries,
		"2002:a00:5::1":               carries,
		"fd00::5efe:a00:5":            carries,
		"fd00::200:5efe:a00:5":        carries,
		"10.0.0.5":                    "",
		"fd12:3456::5":                "",
	} {
		got := DestinationRefusal(netip.MustParseAddr(in), nil)
		if want == "" {
			if got != "" {
				t.Errorf("DestinationRefusal(%s) = %q; want none", in, got)
			}
			continue
		}
		if len(got) < len(want) || got[:len(want)] != want {
			t.Errorf("DestinationRefusal(%s) = %q; want it to open with %q", in, got, want)
		}
	}
}

// TestForwardedClientReadsTheEntryTheBalancerAppends: the second-to-last entry of the one line, as
// a bucket's key, and an error for every header no such balancer wrote.
func TestForwardedClientReadsTheEntryTheBalancerAppends(t *testing.T) {
	for _, tc := range []struct {
		lines []string
		want  string
		err   error
	}{
		{lines: []string{"203.0.113.9, 34.120.0.1"}, want: "203.0.113.9"},
		{lines: []string{"198.51.100.7, 203.0.113.9,34.120.0.1"}, want: "203.0.113.9"},
		{lines: []string{"::ffff:203.0.113.9, 34.120.0.1"}, want: "203.0.113.9"},
		{lines: []string{"2001:db8:1:2::ff, 34.120.0.1"}, want: "2001:db8:1:2::/64"},
		{lines: nil, err: ErrForwardedLines},
		{lines: []string{"198.51.100.7", "203.0.113.9, 34.120.0.1"}, err: ErrForwardedLines},
		{lines: []string{"203.0.113.9"}, err: ErrForwardedEntries},
		{lines: []string{" , 34.120.0.1"}, err: ErrForwardedAddress},
		{lines: []string{"unknown, 34.120.0.1"}, err: ErrForwardedAddress},
	} {
		got, err := ForwardedClient(tc.lines)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("ForwardedClient(%q) = %q, %v; want %q, %v", tc.lines, got, err, tc.want, tc.err)
		}
	}
}
