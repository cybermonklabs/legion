// packages/envoy/internal/cipher/proxy/destination.go
// The private destinations (BROKER_PROXY_PRIVATE_UPSTREAM): the bound hosts whose allowed requests
// the proxy sends to an address inside the broker's own cluster in place of the host's own
// address - in production Dispatch's public host and Dispatch's in-cluster URL. What is here is
// all of it: the setting as the proxy holds it, each destination's own transport, the host each
// serves (destinationOf), the addresses every dial to one may open and why none may
// (upstreamAddresses), the dial itself (dialUpstream), and what the command is told when a
// destination is refused or does not answer, which names the rule and never the destination's
// in-cluster name or address. serveConnect and serveBound decide when a request is a
// destination's; this file decides nothing else.
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
// (BROKER_PROXY_PRIVATE_UPSTREAM). The broker's start holds every destination's host to be the
// host of a dpop block's inline binding, exactly, with an allow list, and no kind's host, and every
// dpop block's host to be a destination's (config.ProxyMintedRules), and the URL to a
// scheme and a host with no user, path or query.
type PrivateUpstream struct {
	// Host is the bound host, in lower case with no port, as a CONNECT's host is compared.
	Host string
	// URL is where that host's requests go: its scheme, which says whether they go in TLS, and its
	// host and port, which upstreamAddresses resolves and checks.
	URL *url.URL
}

// destination is one private destination as the proxy holds it: the setting's entry, and the
// transport every request to its bound host goes out on and nothing else does.
type destination struct {
	upstream  PrivateUpstream
	transport *http.Transport
}

// setPrivateUpstreams gives the proxy each private destination's own transport, keyed by the bound
// host: every request to that host goes out on its destination's transport and on nothing else
// (serveBound), and nothing else goes out on it. Each transport's dial resolves and checks its own
// destination's host (dialUpstream, upstreamAddresses) and dials those addresses, so one host's
// requests never reach another host's destination. Each request it carries is sent to the URL's
// scheme and host, so it speaks TLS exactly when the URL is https, then verified against the roots
// upstream trusts and the URL's host name; it keeps idle connections as upstream does. An entry
// with no host or no URL is no destination. New calls it once the upstream transport is built.
func (s *Server) setPrivateUpstreams(upstreams []PrivateUpstream) {
	for _, upstream := range upstreams {
		if upstream.Host == "" || upstream.URL == nil {
			continue
		}
		d := &destination{upstream: upstream}
		d.transport = &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return s.dialUpstream(ctx, d, network)
			},
			TLSClientConfig:     s.upstream.TLSClientConfig.Clone(),
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: upstreamIdleConns,
			IdleConnTimeout:     upstreamIdleTimeout,
		}
		if s.destinations == nil {
			s.destinations = map[string]*destination{}
		}
		s.destinations[upstream.Host] = d
	}
}

// destinationOf is the private destination whose bound host host is (Options.PrivateUpstreams),
// or nil: the host whose CONNECT is checked against that destination's addresses when the grant's
// names bind it (serveConnect), and whose requests go out on that destination's own transport to
// its URL's scheme and host (serveBound). A proxy given no destination has no such host.
func (s *Server) destinationOf(host string) *destination {
	return s.destinations[host]
}

// hostPort is the destination's host and port as a dial reads them: the URL's host without
// brackets, and its port, or its scheme's own for a URL that names none.
func (d *destination) hostPort() (host, port string) {
	host, port = d.upstream.URL.Hostname(), d.upstream.URL.Port()
	if port == "" {
		port = "80"
		if d.upstream.URL.Scheme == "https" {
			port = "443"
		}
	}
	return host, port
}

// upstreamAddresses answers the addresses destination d's host has, the ones every dial to it
// opens, and why the proxy may dial none of them. It is checkedAddresses turned the other way, for a
// host the broker's settings name as inside its own cluster: each address, read as
// checkedAddresses reads it (netaddr.Canonical), must be one localRule refuses - private,
// 100.64.0.0/10 or inside the cluster ranges - so the exception reaches no address the proxy would
// tunnel to for a command; and none may be one netaddr.DestinationRefusal names - loopback,
// link-local, unspecified, multicast, a cloud metadata server's IPv6 address, or an IPv6 form that
// carries one - where the broker's own pod and the node's metadata server answer and no
// destination of the broker's does. A name refused for one of its addresses is refused whole, as
// checkedAddresses refuses one.
//
// The refusal is written twice. The broker's line of a dial that did not happen (logDialFailure)
// names the destination's host and the address; the error answered, which reaches the command
// that asked (a CONNECT's 403, a request's 502) and so a CI step's public log, names the rule and
// neither the destination's in-cluster name nor its address (destinationRefused).
//
// An address Options.DialAllow names with the destination's port is dialled as it is, which is
// how a local stack reaches a stand-in of Dispatch on loopback.
func (s *Server) upstreamAddresses(ctx context.Context, d *destination) ([]netip.Addr, error) {
	host, port := d.hostPort()
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
		why := netaddr.DestinationRefusal(addr)
		if why == "" && localRule(addr, s.clusterRanges) == "" {
			why = "an address which this proxy would also tunnel to: list its range in BROKER_AIRLOCK_CLUSTER_RANGES"
		}
		if why != "" {
			s.logDialFailure(ctx, host, port, net.JoinHostPort(addr.String(), port), len(addrs), dialRefused,
				fmt.Sprintf("the private upstream %s resolves to %s, %s", host, addr, why))
			return nil, destinationRefused("the private destination resolves to " + why)
		}
	}
	return addrs, nil
}

// destinationRefused is the error of a private destination the proxy will not dial, as the
// command is told it (upstreamAddresses): its text names the rule that refused, and never the
// destination's in-cluster name or address, which the broker's own line holds. It says where the
// detail is, so an operator who reads a CI step's log knows to read the broker's.
func destinationRefused(rule string) error {
	return errors.New(rule + " (BROKER_PROXY_PRIVATE_UPSTREAM); the broker's log names the destination and its address")
}

// dialUpstream is the DialContext of destination d's transport (setPrivateUpstreams): d's
// addresses, checked again at this dial (upstreamAddresses), so a name rebound since its CONNECT
// was checked is refused, and dialled in order (dialAddrs), with the line of a dial no address
// answered, as dialName writes one for every other host. net/http hands it the URL's host and
// port, which every request on that transport is sent to (serveBound), so it reads the
// destination from d rather than from the address and resolves no other name.
func (s *Server) dialUpstream(ctx context.Context, d *destination, network string) (net.Conn, error) {
	addrs, err := s.upstreamAddresses(ctx, d)
	if err != nil {
		return nil, err
	}
	host, port := d.hostPort()
	conn, _, err := s.dialAddrs(ctx, network, addrs, port)
	if err != nil {
		s.logUnreachable(ctx, host, port, addrs, err)
		return nil, err
	}
	return conn, nil
}

// destinationUnreachable is badGateway for a call to a private destination's bound host
// (serveBound): the same line, with the transport's error whole, and the same 502, whose words
// say the destination could not be reached and leave the error out. A transport error names the
// address it dialled or read from, which is the destination's in-cluster address; the command's
// answer reaches a CI step's public log, and the broker's line keeps the detail.
func (s *Server) destinationUnreachable(host, path string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		s.log.Warn(upstreamMessage, answer.ClipFields([]any{"host", host, "path", path, "error", upstreamReason(err)})...)
		fail(w, http.StatusBadGateway, codeUpstream, host+": the private destination did not answer (BROKER_PROXY_PRIVATE_UPSTREAM); the broker's log names the destination and the error")
	}
}
