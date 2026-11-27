package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestBoundAddressReadsTheClientATrustedProxyNames: BoundAddress reads X-Forwarded-For only from a
// peer in the trusted proxy ranges (DISPATCH_TRUSTED_PROXY_RANGES), and in three ways:
//   - a trusted peer in Google's external load balancer's ranges is read in the format Google
//     documents, [SUPPLIED,]CLIENT_IP,LOAD_BALANCER_IP, where LOAD_BALANCER_IP is the address of the
//     balancer's forwarding rule: the client is the second-to-last entry of the one line;
//   - any other trusted peer is read as the rightmost entry that is not itself in the ranges, the
//     client a proxy such as nginx appends last, past the trusted hops after it;
//   - a peer outside the ranges keys on its own address, whatever X-Forwarded-For it carries.
//
// A trusted peer's request with two X-Forwarded-For lines names no address (false: 400
// FORWARDED_FOR_INVALID), and one whose header names no client keys on the peer.
//
// Controls: read every trusted peer as the rightmost entry not in the ranges, and the Google rows
// answer the forwarding rule's address 34.120.5.6; key every peer on its own address, and the
// other proxy's rows answer the proxy; read X-Forwarded-For from every peer, and the rows of a peer
// outside the ranges answer the address the caller wrote.
func TestBoundAddressReadsTheClientATrustedProxyNames(t *testing.T) {
	configured := []netip.Prefix{
		netip.MustParsePrefix("35.191.0.0/16"),
		netip.MustParsePrefix("130.211.0.0/22"),
		netip.MustParsePrefix("2600:2d00:1:1::/64"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("2001:db8:100::/48"),
	}
	otherProxyOnly := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	for _, row := range []struct {
		name      string
		ranges    []netip.Prefix
		peer      string
		forwarded []string
		want      string
		ok        bool
	}{
		// A trusted peer in Google's load balancer's ranges: the client is second to last.
		{"Google's balancer, its documented line", configured, "35.191.4.5:41234", []string{"198.51.100.7,34.120.5.6"}, "198.51.100.7", true},
		{"Google's balancer, after what the client sent", configured, "35.191.4.5:41234", []string{"10.0.0.9, 203.0.113.66, 198.51.100.7, 34.120.5.6"}, "198.51.100.7", true},
		{"Google's other range", configured, "130.211.3.200:41234", []string{"198.51.100.8, 34.120.5.6"}, "198.51.100.8", true},
		{"Google's IPv6 proxy range", configured, "[2600:2d00:1:1::17]:41234", []string{"2001:db8:7::9, 2600:1901:0:1::5"}, "2001:db8:7::9", true},
		{"Google's balancer as an IPv4 address written as IPv6", configured, "[::ffff:35.191.4.5]:41234", []string{"::ffff:198.51.100.7, 34.120.5.6"}, "198.51.100.7", true},
		{"Google's balancer with two lines", configured, "35.191.4.5:41234", []string{"203.0.113.66, 34.120.5.6", "198.51.100.7, 34.120.5.6"}, "", false},
		{"Google's balancer with one entry", configured, "35.191.4.5:41234", []string{"198.51.100.7"}, "35.191.4.5", true},
		{"Google's balancer with no address second to last", configured, "35.191.4.5:41234", []string{"not-an-address, 34.120.5.6"}, "35.191.4.5", true},
		{"Google's balancer with no header", configured, "35.191.4.5:41234", nil, "35.191.4.5", true},
		// Any other trusted peer: the rightmost entry not in the ranges.
		{"another proxy appending the client", configured, "192.0.2.10:443", []string{"198.51.100.7"}, "198.51.100.7", true},
		{"another proxy after what the client sent", configured, "192.0.2.10:443", []string{"203.0.113.66, 198.51.100.7"}, "198.51.100.7", true},
		{"another proxy behind a trusted hop", configured, "192.0.2.10:443", []string{"198.51.100.7, 192.0.2.20"}, "198.51.100.7", true},
		{"another proxy behind Google's balancer's own address", configured, "192.0.2.10:443", []string{"198.51.100.7, 35.191.4.5"}, "198.51.100.7", true},
		{"another IPv6 proxy", configured, "[2001:db8:100::5]:443", []string{"2001:db8:7::9"}, "2001:db8:7::9", true},
		{"another proxy naming an IPv4 client written as IPv6", configured, "192.0.2.10:443", []string{"::ffff:198.51.100.7"}, "198.51.100.7", true},
		{"another proxy with two lines", configured, "192.0.2.10:443", []string{"203.0.113.66", "198.51.100.7"}, "", false},
		{"another proxy with no header", configured, "192.0.2.10:443", nil, "192.0.2.10", true},
		{"another proxy naming only trusted hops", configured, "192.0.2.10:443", []string{"192.0.2.20, 192.0.2.30"}, "192.0.2.10", true},
		{"another proxy whose rightmost untrusted entry is no address", configured, "192.0.2.10:443", []string{"198.51.100.7, not-an-address"}, "192.0.2.10", true},
		{"another proxy with an empty line", configured, "192.0.2.10:443", []string{""}, "192.0.2.10", true},
		// A peer outside the ranges keys on its own address.
		{"an untrusted peer with a forged line", configured, "203.0.113.9:1234", []string{"198.51.100.7"}, "203.0.113.9", true},
		{"an untrusted peer with two lines", configured, "203.0.113.9:1234", []string{"198.51.100.7", "198.51.100.8"}, "203.0.113.9", true},
		{"an untrusted peer just past 35.191.0.0/16", configured, "35.192.0.1:1234", []string{"198.51.100.7, 34.120.5.6"}, "35.192.0.1", true},
		{"an untrusted peer just past 192.0.2.0/24", configured, "192.0.3.1:1234", []string{"198.51.100.7"}, "192.0.3.1", true},
		{"Google's balancer when the ranges leave Google out", otherProxyOnly, "35.191.4.5:41234", []string{"198.51.100.7, 34.120.5.6"}, "35.191.4.5", true},
		{"Google's balancer with no ranges", nil, "35.191.4.5:41234", []string{"198.51.100.7, 34.120.5.6"}, "35.191.4.5", true},
		{"another proxy with no ranges", nil, "192.0.2.10:443", []string{"198.51.100.7"}, "192.0.2.10", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/auth/passkey/login/begin", nil)
			request.RemoteAddr = row.peer
			for _, line := range row.forwarded {
				request.Header.Add("X-Forwarded-For", line)
			}
			got, ok := BoundAddress(request, row.ranges)
			if got != row.want || ok != row.ok {
				t.Errorf("BoundAddress from %s with X-Forwarded-For %q = %q, %t; want %q, %t", row.peer, row.forwarded, got, ok, row.want, row.ok)
			}
		})
	}
}
