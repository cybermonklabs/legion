package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ForwardedAddress keys a connection from Google's external load balancer on the client address
// the balancer writes second to last in the one X-Forwarded-For line, as the secrets broker's edge
// does, and every other connection on the rightmost entry, as it always has. Behind the balancer,
// a header the balancer did not write as one line of at least two entries with an IP address
// second to last is ignored, and the request keys on the balancer's own address, where the caller
// picks nothing.
//
// Controls: ForwardedAddress reading the rightmost entry for every connection (the rule before),
// under which the balancer's rows answer the balancer's own address for every caller; and the IPv6
// range 2600:2d00:1:1::/64 left out of googleLoadBalancerRanges, under which the IPv6 balancer's
// row answers the balancer's own address.
func TestForwardedAddressReadsTheClientGooglesBalancerNames(t *testing.T) {
	for _, row := range []struct {
		name, peer string
		forwarded  []string
		want       string
	}{
		{"the balancer's client, lb line", "35.191.4.5:41234", []string{"198.51.100.7, 35.191.4.5"}, "198.51.100.7"},
		{"the balancer's line after what the client sent", "35.191.4.5:41234", []string{"10.0.0.9, 192.0.2.250, 198.51.100.7, 35.191.4.5"}, "198.51.100.7"},
		{"the other balancer range", "130.211.3.200:41234", []string{"198.51.100.8,130.211.3.200"}, "198.51.100.8"},
		{"an IPv6 client behind the balancer", "35.191.4.5:41234", []string{"2001:db8::7, 35.191.4.5"}, "2001:db8::7"},
		{"an IPv4 client written as IPv6", "35.191.4.5:41234", []string{"::ffff:198.51.100.7, 35.191.4.5"}, "198.51.100.7"},
		{"the balancer as an IPv4 address written as IPv6", "[::ffff:35.191.4.5]:41234", []string{"198.51.100.7, 35.191.4.5"}, "198.51.100.7"},
		{"two lines behind the balancer", "35.191.4.5:41234", []string{"203.0.113.66, 35.191.4.5", "198.51.100.7, 35.191.4.5"}, "35.191.4.5"},
		{"one entry behind the balancer", "35.191.4.5:41234", []string{"198.51.100.7"}, "35.191.4.5"},
		{"no address second to last behind the balancer", "35.191.4.5:41234", []string{"not-an-address, 35.191.4.5"}, "35.191.4.5"},
		{"no header behind the balancer", "35.191.4.5:41234", nil, "35.191.4.5"},
		{"a direct peer with a forged line", "192.0.2.4:1234", []string{"198.51.100.7, 203.0.113.9"}, "203.0.113.9"},
		{"a direct peer with two lines", "192.0.2.4:1234", []string{"198.51.100.7", "203.0.113.9, 203.0.113.10"}, "203.0.113.10"},
		{"a direct peer with no header", "192.0.2.4:1234", nil, "192.0.2.4"},
		{"a peer just past 35.191.0.0/16", "35.192.0.1:1234", []string{"198.51.100.7, 203.0.113.9"}, "203.0.113.9"},
		{"a peer just past 130.211.0.0/22", "130.211.4.1:1234", []string{"198.51.100.7, 203.0.113.9"}, "203.0.113.9"},
		{"the balancer's IPv6 proxy range", "[2600:2d00:1:1::17]:41234", []string{"198.51.100.9, 2600:2d00:1:1::17"}, "198.51.100.9"},
		{"a peer just past 2600:2d00:1:1::/64", "[2600:2d00:1:2::17]:1234", []string{"198.51.100.9, 2600:2d00:1:2::17"}, "2600:2d00:1:2::17"},
		{"Google's IPv6 health-check range, no header", "[2600:2d00:1:b029::5]:1234", nil, "2600:2d00:1:b029::5"},
	} {
		t.Run(row.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
			request.RemoteAddr = row.peer
			for _, line := range row.forwarded {
				request.Header.Add("X-Forwarded-For", line)
			}
			if got := ForwardedAddress(request); got != row.want {
				t.Errorf("ForwardedAddress from %s with X-Forwarded-For %q = %q, want %q", row.peer, row.forwarded, got, row.want)
			}
		})
	}
}
