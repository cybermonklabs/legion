package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
)

// forwardedRequest is a request whose connection comes from peer, carrying each of forwarded as
// one X-Forwarded-For line.
func forwardedRequest(peer string, forwarded []string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.RemoteAddr = peer
	for _, line := range forwarded {
		request.Header.Add("X-Forwarded-For", line)
	}
	return request
}

// TestAPeerOutsideEveryRangeKeepsTheRightmostEntry: a peer in no trusted proxy range keeps the
// rule Dispatch has always read, the rightmost X-Forwarded-For entry, with ranges configured or
// not. So a caller that reaches Dispatch directly and forges a line of two entries never makes
// the second from the right, which it chose, its key, and a header such a caller writes is never
// refused: only a configured proxy is held to the line it writes. A request the router never
// resolved reads the same (ForwardedAddress).
//
// Control: make ResolveForwardedAddress read every peer as a configured one (drop the range
// check), and the forged rows answer the entry the caller wrote second from the right, and the
// one-entry, two-line and no-header rows are refused.
func TestAPeerOutsideEveryRangeKeepsTheRightmostEntry(t *testing.T) {
	const peer = "127.0.0.1:40000"
	for _, list := range []struct {
		name       string
		configured []netip.Prefix
	}{
		{"no range", nil},
		{"an empty list", []netip.Prefix{}},
		{"a range the peer is outside", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
	} {
		for _, row := range []struct {
			name      string
			forwarded []string
			want      string
		}{
			{"one entry the caller wrote", []string{"198.51.100.66"}, "198.51.100.66"},
			{"a forged line of two entries", []string{"198.51.100.8, 198.51.100.66"}, "198.51.100.66"},
			{"a forged line of three entries", []string{"198.51.100.8, 203.0.113.7, 198.51.100.66"}, "198.51.100.66"},
			{"two lines", []string{"198.51.100.8", "203.0.113.7, 198.51.100.66"}, "198.51.100.66"},
			{"no header", nil, "127.0.0.1"},
		} {
			t.Run(list.name+", "+row.name, func(t *testing.T) {
				request := forwardedRequest(peer, row.forwarded)
				got, err := ResolveForwardedAddress(request, list.configured)
				if got != row.want || err != nil {
					t.Errorf("ResolveForwardedAddress from %s with X-Forwarded-For %q under %s = %q, %v; want %q, the rightmost entry (the connection's own address with none)", peer, row.forwarded, list.name, got, err, row.want)
				}
				if got := ForwardedAddress(request); got != row.want {
					t.Errorf("ForwardedAddress from %s with X-Forwarded-For %q and no resolved address = %q, want %q", peer, row.forwarded, got, row.want)
				}
			})
		}
	}
}

// TestAConfiguredPeerIsReadByForwardedClient: a peer in a trusted proxy range is read as
// netaddr.ForwardedClient reads the one line such a proxy writes, the client it appended second
// from the right: an IPv4 client as it is, one written as IPv6 as the IPv4 address it holds, an
// IPv6 client as its /64, and never an entry to its left, which the caller wrote. A peer written as
// an IPv4-mapped IPv6 address is in the IPv4 range that holds it. A header such a proxy could not
// have written is ForwardedClient's refusal, and no address.
//
// Control: read a configured peer by the rightmost entry, and the accepted rows answer the
// proxy's forwarding rule, 10.0.0.9, and the refusals answer an address.
func TestAConfiguredPeerIsReadByForwardedClient(t *testing.T) {
	configured := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, row := range []struct {
		name, peer string
		forwarded  []string
		want       string
		refusal    error
	}{
		{"an IPv4 client", "192.0.2.10:443", []string{"203.0.113.7, 10.0.0.9"}, "203.0.113.7", nil},
		{"a client entry a caller wrote to the left", "192.0.2.10:443", []string{"198.51.100.66, 203.0.113.7, 10.0.0.9"}, "203.0.113.7", nil},
		{"an IPv4 client written as IPv6", "192.0.2.10:443", []string{"::ffff:203.0.113.7, 10.0.0.9"}, "203.0.113.7", nil},
		{"an IPv6 client, by its /64", "192.0.2.10:443", []string{"2001:db8:7:1::9, 10.0.0.9"}, "2001:db8:7:1::/64", nil},
		{"a peer written as IPv4-mapped IPv6", "[::ffff:192.0.2.10]:443", []string{"203.0.113.7, 10.0.0.9"}, "203.0.113.7", nil},
		{"one entry", "192.0.2.10:443", []string{"203.0.113.7"}, "", netaddr.ErrForwardedEntries},
		{"two lines", "192.0.2.10:443", []string{"198.51.100.8", "203.0.113.7, 10.0.0.9"}, "", netaddr.ErrForwardedLines},
		{"no header", "192.0.2.10:443", nil, "", netaddr.ErrForwardedLines},
		{"a second-to-last entry that is no address", "192.0.2.10:443", []string{"not-an-address, 10.0.0.9"}, "", netaddr.ErrForwardedAddress},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := ResolveForwardedAddress(forwardedRequest(row.peer, row.forwarded), configured)
			if got != row.want || !errors.Is(err, row.refusal) {
				t.Errorf("ResolveForwardedAddress from %s with X-Forwarded-For %q = %q, %v; want %q, %v", row.peer, row.forwarded, got, err, row.want, row.refusal)
			}
		})
	}
}
