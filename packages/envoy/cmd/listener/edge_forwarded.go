package main

// edge_forwarded.go is how an edge listener reads its caller's address. The edge is reached
// through Google's global external Application Load Balancer, so every request comes from a
// Google Front End proxy: the connection's own address (r.RemoteAddr) names that proxy, which many
// callers share, and one caller's requests come from many proxies. The broker-token credential
// keys its introspection bucket on a caller's address (ciphertoken.DefaultAddressLimit), and a
// bucket keyed on the proxy would let a stranger's junk tokens refuse every caller behind it
// while bounding no one caller.
//
// Google writes the client's address into X-Forwarded-For: the balancer appends
// "<client address>,<load balancer address>" to whatever the client sent, as one line
// (https://cloud.google.com/load-balancing/docs/https#x-forwarded-for_header), and its proxies
// connect from googleFrontEnds (https://cloud.google.com/load-balancing/docs/firewall-rules). A
// request from one of them is keyed on the address netaddr.ForwardedClient reads from that one
// line, an IPv4 address or the /64 an IPv6 address is in, and is refused 400 when it carries no
// such line; a request from any other peer is keyed on its connection, as before, and its
// forwarded headers choose nothing. The proxy's own address reaches the pod only with
// container-native backends (zonal NEGs), which the edge's route uses: behind instance-group
// backends kube-proxy rewrites the source to a node's address, outside those ranges, and every
// request would be keyed on the node.

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
)

// googleFrontEnds are the ranges Google's load balancer proxies connect to a zonal NEG's backends
// from, IPv4 and IPv6, and its IPv4 health checkers from 35.191.0.0/16
// (https://cloud.google.com/load-balancing/docs/firewall-rules, "GFE proxy ranges").
var googleFrontEnds = []netip.Prefix{
	netip.MustParsePrefix("35.191.0.0/16"),
	netip.MustParsePrefix("130.211.0.0/22"),
	netip.MustParsePrefix("2600:2d00:1:1::/64"),
}

// edgeForwardedFor is the header Google's load balancer writes the client's address into.
const edgeForwardedFor = "X-Forwarded-For"

// edgeForwardedRefusal is the 400 an edge listener answers a request from Google's proxies that
// carries no address the balancer forwarded.
const edgeForwardedRefusal = "the edge listener reads its caller's address from the second-to-last entry of one " +
	edgeForwardedFor + " line, the entry Google's load balancer appends, and this request carries no such line"

// edgeCallerAddress is the address an edge listener keys r's caller on, and whether Google's load
// balancer forwarded it. For a connection from googleFrontEnds it is netaddr.ForwardedClient's
// reading of r's X-Forwarded-For, and that reading's error when r carries no address the balancer
// wrote. For any other connection it is "" and false: that peer did not come through the balancer,
// so what it wrote in the header is its own and chooses nothing, and listenerSourceAddress keys it
// on the connection.
func edgeCallerAddress(r *http.Request) (string, bool, error) {
	if !fromGoogleFrontEnd(r.RemoteAddr) {
		return "", false, nil
	}
	address, err := netaddr.ForwardedClient(r.Header.Values(edgeForwardedFor))
	if err != nil {
		return "", false, err
	}
	return address, true, nil
}

// fromGoogleFrontEnd reports whether remote, a connection's address as net/http writes it
// (host:port), is in googleFrontEnds, read as netaddr.Canonical reads it: an IPv4-mapped address
// is the IPv4 address it holds, and a zone is dropped, either of which netip.Prefix.Contains would
// otherwise not find in an IPv4 range.
func fromGoogleFrontEnd(remote string) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	addr := netaddr.Canonical(peer.Addr())
	for _, prefix := range googleFrontEnds {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// callerAddressKey is the context key the address apiAuth read for a request's caller is carried
// under (withCallerAddress).
type callerAddressKey struct{}

// withCallerAddress carries the address an edge listener read for a request's caller
// (edgeCallerAddress) to the introspection bucket (listenerSourceAddress). apiAuth sets it once,
// where it refuses a request it reads no address from, so the address that decides the refusal is
// the one the bucket keys on.
func withCallerAddress(ctx context.Context, address string) context.Context {
	return context.WithValue(ctx, callerAddressKey{}, address)
}
