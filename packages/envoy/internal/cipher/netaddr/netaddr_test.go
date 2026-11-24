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
