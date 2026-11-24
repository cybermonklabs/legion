// packages/envoy/internal/cipher/proxy/destination.go
// The private destination (BROKER_AIRLOCK_PRIVATE_UPSTREAM): the one bound host whose allowed
// requests the proxy sends to an address inside the broker's own cluster in place of the host's
// own address - in production Dispatch's public host and Dispatch's in-cluster URL. What is here
// is all of it: the setting as the proxy holds it, its own transport, the host it serves
// (toDestination), the addresses every dial to it may open and why none may (upstreamAddresses,
// with the rule of one address, PrivateUpstreamRefusal, which the broker's start reads too),
// the dial itself (dialUpstream), and what the command is told when the destination is refused or
// does not answer, which names the rule and never the destination's in-cluster name or address.
// serveConnect and serveBound decide when a request is the destination's; this file decides
// nothing else.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/netaddr"
)

// PrivateUpstream is one bound host and the URL its requests are sent to in place of the host's own
// address: in production Dispatch's public host and its address inside the cluster
// (BROKER_AIRLOCK_PRIVATE_UPSTREAM). The broker's start holds it to the one host the proxy block of
// the profile DISPATCH_RESULTS_TOKEN binds, exactly and alone, of kind dpop with an allow list
// (config.AirlockResultRules), and the URL to a scheme and a host with no user, path or query.
type PrivateUpstream struct {
	// Host is the bound host, in lower case with no port, as a CONNECT's host is compared.
	Host string
	// URL is where that host's requests go: its scheme, which says whether they go in TLS, and its
	// host and port, which upstreamAddresses resolves and checks.
	URL *url.URL
}

// setPrivateUpstream gives the proxy the private destination's own transport, for a proxy given
// one: every request to its bound host goes out on this and on nothing else (serveBound), and
// nothing else goes out on it. Its dial resolves and checks the destination's host itself
// (dialUpstream, upstreamAddresses) and dials those addresses. Each request it carries is sent to
// the URL's scheme and host, so it speaks TLS exactly when the URL is https, then verified against
// the roots upstream trusts and the URL's host name; it keeps idle connections as upstream does.
// New calls it once the upstream transport is built.
func (s *Server) setPrivateUpstream(upstream PrivateUpstream) {
	if upstream.Host == "" || upstream.URL == nil {
		return
	}
	s.privateUpstream = upstream
	s.private = &http.Transport{
		DialContext:         s.dialUpstream,
		TLSClientConfig:     s.upstream.TLSClientConfig.Clone(),
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: upstreamIdleConns,
		IdleConnTimeout:     upstreamIdleTimeout,
	}
}

// toDestination reports whether host is the private destination's bound host
// (Options.PrivateUpstream): the one host whose CONNECT is checked against the destination's
// addresses when the grant's names bind it (serveConnect), and whose requests go out on the
// destination's own transport to the URL's scheme and host (serveBound). A proxy given no
// destination has no such host.
func (s *Server) toDestination(host string) bool {
	return s.private != nil && host == s.privateUpstream.Host
}

// destination is the private destination's host and port as a dial reads them: the URL's host
// without brackets, and its port, or its scheme's own for a URL that names none.
func (s *Server) destination() (host, port string) {
	host, port = s.privateUpstream.URL.Hostname(), s.privateUpstream.URL.Port()
	if port == "" {
		port = "80"
		if s.privateUpstream.URL.Scheme == "https" {
			port = "443"
		}
	}
	return host, port
}

// upstreamAddresses answers the addresses the private destination's host has, the ones every dial
// to it opens, and why the proxy may dial none of them. It is checkedAddresses turned the other
// way, for the one host the broker's settings name as inside its own cluster: each address, read as
// checkedAddresses reads it (netaddr.Canonical), is held to PrivateUpstreamRefusal. A name refused
// for one of its addresses is refused whole, as checkedAddresses refuses one.
//
// The refusal is written twice. The broker's line of a dial that did not happen (logDialFailure)
// names the destination's host and the address; the error answered, which reaches the command
// that asked (a CONNECT's 403, a request's 502) and so a CI step's public log, names the rule and
// neither the destination's in-cluster name nor its address (destinationRefused).
//
// An address Options.DialAllow names with the destination's port is dialled as it is, which is
// how a local stack reaches a stand-in of Dispatch on loopback.
func (s *Server) upstreamAddresses(ctx context.Context) ([]netip.Addr, error) {
	host, port := s.destination()
	addrs, err := s.resolve(ctx, host)
	if err != nil {
		s.logDialFailure(ctx, host, port, "", 0, failedClass(ctx, err, dialUnresolved), fmt.Sprintf("cannot resolve the private upstream %s: %v", host, err))
		return nil, destinationRefused("the airlock cannot resolve the private destination")
	}
	if len(addrs) == 0 {
		s.logDialFailure(ctx, host, port, "", 0, dialUnresolved, fmt.Sprintf("the private upstream %s resolves to no address", host))
		return nil, destinationRefused("the private destination resolves to no address")
	}
	canonical := make([]netip.Addr, len(addrs))
	for i, addr := range addrs {
		canonical[i] = netaddr.Canonical(addr)
	}
	addrs = canonical
	if slices.Contains(s.dialAllow, net.JoinHostPort(host, port)) {
		return addrs, nil
	}
	for _, addr := range addrs {
		if why := PrivateUpstreamRefusal(addr, s.clusterRanges); why != "" {
			s.logDialFailure(ctx, host, port, net.JoinHostPort(addr.String(), port), len(addrs), dialRefused,
				fmt.Sprintf("the private upstream %s resolves to %s, %s", host, addr, why))
			return nil, destinationRefused("the private destination resolves to " + why)
		}
	}
	return addrs, nil
}

// PrivateUpstreamRefusal answers why addr can be no address of the private destination
// (BROKER_AIRLOCK_PRIVATE_UPSTREAM) on a broker whose cluster ranges are clusterRanges
// (Options.ClusterRanges, BROKER_AIRLOCK_CLUSTER_RANGES), or "" for an address every dial to the
// destination may open. The answer completes "... resolves to". addr must be one localRule refuses
// - private, 100.64.0.0/10 or inside the cluster ranges - so the exception reaches no address the
// proxy would tunnel to for a command; and it may be none netaddr.DestinationRefusal names -
// loopback, link-local, unspecified, multicast, the IPv4 broadcast address or a cloud metadata
// server's IPv6 address - where the broker's own pod and the node's metadata server answer, or no
// one service does, and no destination of the broker's does.
// It reads addr as netaddr.Canonical reads it. The dial (upstreamAddresses) reads it for each
// address the destination's host resolves to, and config's start check reads it for a URL whose
// host is an address, so the start takes no address literal the dial refuses.
func PrivateUpstreamRefusal(addr netip.Addr, clusterRanges []netip.Prefix) string {
	if why := netaddr.DestinationRefusal(addr); why != "" {
		return why
	}
	if localRule(addr, clusterRanges) == "" {
		return "an address which this proxy would also tunnel to: list its range in BROKER_AIRLOCK_CLUSTER_RANGES"
	}
	return ""
}

// destinationRefused is the error of a private destination the proxy will not dial, as the
// command is told it (upstreamAddresses): its text names the rule that refused, and never the
// destination's in-cluster name or address, which the broker's own line holds. It says where the
// detail is, so an operator who reads a CI step's log knows to read the broker's.
func destinationRefused(rule string) error {
	return errors.New(rule + " (BROKER_AIRLOCK_PRIVATE_UPSTREAM); the broker's log names the destination and its address")
}

// dialUpstream is the DialContext of the private destination's transport (New): the destination's
// addresses, checked (upstreamAddresses) and dialled in order (dialAddrs), with the line of a dial
// no address answered, as dialName writes one for every other host. net/http hands it the URL's
// host and port, which every request on that transport is sent to (serveBound), so it reads the
// destination from the Server rather than from addr and resolves no other name.
func (s *Server) dialUpstream(ctx context.Context, network, _ string) (net.Conn, error) {
	addrs, err := s.upstreamAddresses(ctx)
	if err != nil {
		return nil, err
	}
	host, port := s.destination()
	conn, _, err := s.dialAddrs(ctx, network, addrs, port)
	if err != nil {
		s.logUnreachable(ctx, host, port, addrs, err)
		return nil, err
	}
	return conn, nil
}

// destinationUnreachable is badGateway for a call to the private destination's bound host
// (serveBound): the same line, with the transport's error whole, and the same 502, whose words
// say the destination could not be reached and leave the error out. A transport error names the
// address it dialled or read from, which is the destination's in-cluster address; the command's
// answer reaches a CI step's public log, and the broker's line keeps the detail.
func (s *Server) destinationUnreachable(host, path string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		s.log.Warn(upstreamMessage, answer.ClipFields([]any{"host", host, "path", path, "error", upstreamReason(err)})...)
		fail(w, http.StatusBadGateway, codeUpstream, host+": the private destination did not answer (BROKER_AIRLOCK_PRIVATE_UPSTREAM); the broker's log names the destination and the error")
	}
}
