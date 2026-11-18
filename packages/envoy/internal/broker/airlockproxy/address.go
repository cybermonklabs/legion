// packages/envoy/internal/broker/airlockproxy/address.go
// Where the proxy may dial, and what it dials there. A command's CONNECT names the host, so
// without this check the proxy is an open relay into the cluster the broker runs in: the pod's
// own services, the node's metadata server, the broker's own API and its database all answer a
// dial from this process, and a tunnel would carry whatever a command asked for straight to
// them. Every dial this package makes - a tunnel's, a bound host's and a plain-HTTP request's
// alike - resolves the name once here, refuses the whole name when any one of its addresses is
// one the proxy never dials, and then dials those addresses rather than the name. So a name
// that answers one address to the check and another to the dial, which is DNS rebinding,
// reaches the address that was checked or nothing at all.
package airlockproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/netaddr"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// clusterRange is 100.64.0.0/10, the shared address space of RFC 6598. Kubernetes clusters and
// cloud networks hand node and pod addresses out of it, and netip's own predicates do not name
// it, so it is spelled out here.
var clusterRange = netip.MustParsePrefix("100.64.0.0/10")

// ResolveHost is the default of Server.resolve: the addresses a host name has right now, or the
// address a host that is an address literal already is. It is exported because a caller that
// gives Options.Resolve a resolver of its own - cmd/broker, whose BROKER_AIRLOCK_TEST_RESOLVE
// answers for the hosts it names and nothing else - falls back to exactly this for every other
// host, and a second copy of it would be a second rule about where a name is.
func ResolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{netaddr.Canonical(addr)}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i, addr := range addrs {
		addrs[i] = netaddr.Canonical(addr)
	}
	return addrs, nil
}

// checkedAddresses answers the addresses the proxy may dial for host at port, in the order a
// dial must try them, and why it may dial none of them. A name that resolves to several
// addresses is refused when ANY of them is one the proxy never dials: answering the request
// would otherwise depend on which address the dialer happened to pick, and a name under the
// caller's control can answer with one public address beside one private one precisely to make
// that choice.
//
// What it answers is what is dialled (dialAddrs), so the check and the dial are one reading of
// one name: nothing below resolves that name again, and a name that answers another address to
// a later lookup - DNS rebinding - never reaches this process's socket. The host name goes on
// being the TLS server name upstream, since that is the name a service's certificate is read
// against and the name the rules are read for.
//
// An address Options.DialAllow names is dialled whatever it resolves to, which is what lets one
// run reach a stand-in service on loopback while every other loopback address stays refused.
func (s *Server) checkedAddresses(ctx context.Context, host, port string) ([]netip.Addr, error) {
	addrs, err := s.resolve(ctx, host)
	if err != nil {
		refused := fmt.Errorf("cannot resolve %s: %w", host, err)
		s.logDialFailure(ctx, host, port, "", dialUnresolved, refused.Error())
		return nil, refused
	}
	if len(addrs) == 0 {
		refused := fmt.Errorf("%s resolves to no address", host)
		s.logDialFailure(ctx, host, port, "", dialUnresolved, refused.Error())
		return nil, refused
	}
	// Every answer as every check below reads it and as the dial opens it (netaddr.Canonical):
	// a resolver of a caller's own - Options.Resolve - may answer a zoned or mapped address, and
	// a zoned address is inside no range at all. A copy, since the resolver may hold its slice.
	canonical := make([]netip.Addr, len(addrs))
	for i, addr := range addrs {
		canonical[i] = netaddr.Canonical(addr)
	}
	addrs = canonical
	if slices.Contains(s.dialAllow, net.JoinHostPort(host, port)) {
		return addrs, nil
	}
	for _, addr := range addrs {
		if rule := localRule(addr, s.clusterRanges); rule != "" {
			refused := fmt.Errorf("%s resolves to %s, %s, which this proxy never dials", host, addr, rule)
			s.logDialFailure(ctx, host, port, addr.String(), dialRefused, refused.Error())
			return nil, refused
		}
	}
	return addrs, nil
}

// dialChecked is one dial of a host outside the broker, check and all: the addresses the name
// has now, the refusal of the whole name when any of them is one this proxy never dials, and a
// dial of those addresses. It is the DialContext of the transport every bound host's request and
// every plain-HTTP request goes out on, where a refusal and a failed dial are one answer to the
// caller. The two paths that must refuse a request before they take a command's connection over
// - a CONNECT and a plain-HTTP request - read checkedAddresses themselves, so that their refusal
// is a 403 rather than a 502, and hand what it answered to dialAddrs.
func (s *Server) dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := s.checkedAddresses(ctx, host, port)
	if err != nil {
		return nil, err
	}
	conn, _, err := s.dialAddrs(ctx, network, addrs, port)
	if err != nil {
		s.logDialFailure(ctx, host, port, addressList(addrs, port), dialUnreachable, err.Error())
		return nil, err
	}
	return conn, nil
}

// dialAddrs dials the addresses in order and answers the first connection one of them gave and
// the address it was opened to, so that a name whose first address is down still reaches the
// service through its second, as a dialer resolving the name for itself would. It resolves
// nothing: these are the addresses checkedAddresses read and checked, and they are the addresses
// the socket is opened to. The address it answers is the one a tunnel's line names: the address
// this process asked for, which is what the check read, rather than the connection's own remote
// address, which a dialer of a test's own may have sent elsewhere.
func (s *Server) dialAddrs(ctx context.Context, network string, addrs []netip.Addr, port string) (net.Conn, string, error) {
	if len(addrs) == 0 {
		return nil, "", errors.New("no address to dial")
	}
	var errs []error
	for _, addr := range addrs {
		dialed := net.JoinHostPort(addr.String(), port)
		conn, err := s.dial(ctx, network, dialed)
		if err == nil {
			return conn, dialed, nil
		}
		errs = append(errs, err)
	}
	return nil, "", errors.Join(errs...)
}

// addressList is every address a dial tried, as one field of its line: one address for almost
// every name, and all of them for a name whose addresses were tried in turn.
func addressList(addrs []netip.Addr, port string) string {
	dialed := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		dialed = append(dialed, net.JoinHostPort(addr.String(), port))
	}
	return strings.Join(dialed, " ")
}

// tunnelMessage is the message of the one line each tunnel the proxy opens logs, and
// dialFailedMessage of the one line each dial it does not make logs. A tunnel is the one thing
// this port carries that it never reads: no request of it is decided, so no call line is written
// for it, and without these two lines nothing in the broker's log says a session reached an
// address at all.
const (
	tunnelMessage     = "airlock proxy: a tunnel"
	dialFailedMessage = "airlock proxy: a dial that did not happen"
)

// The classes dialFailedMessage's line names, which is what an operator reads a refusal of the
// broker's own rules apart from a service that was down by.
const (
	// dialRefused: an address this proxy never dials (localRule), so nothing was dialled.
	dialRefused = "refused"
	// dialUnresolved: a host that resolves to no address at all, so there was nothing to dial.
	dialUnresolved = "unresolved"
	// dialUnreachable: every address of the host was dialled and none answered.
	dialUnreachable = "unreachable"
)

// callerKey is the context key the grant a request was proved for travels under, for the two
// lines below: the dials this file makes run under the request's own context, and a bound host's
// run under it two layers down, inside the transport. A request on a decrypted connection
// carries its connection's grant instead (connKey), which serveBound reads for its own line.
type callerKey struct{}

// withCaller is serveProxy's one hand-over of the grant to the dials below it.
func withCaller(ctx context.Context, grant requests.AirlockConnection) context.Context {
	return context.WithValue(ctx, callerKey{}, grant)
}

// callerOfContext is the grant a dial is made for: the one serveProxy put there for a CONNECT
// and a plain-HTTP request, else the one the decrypted connection a bound host's request arrived
// on was opened with, and the zero grant for a dial made before either was known.
func callerOfContext(ctx context.Context) requests.AirlockConnection {
	if grant, ok := ctx.Value(callerKey{}).(requests.AirlockConnection); ok {
		return grant
	}
	if info, ok := ctx.Value(connKey{}).(connInfo); ok {
		return info.grant
	}
	return requests.AirlockConnection{}
}

// dialFields is who a dial was made for and what it was for: the grant, its enrollment, and the
// session and runtime of the pod behind it, then the host and port the caller asked for and the
// address this process asked the kernel for. Never a placeholder and never a value, as the call
// line (callFields): what a command sent is its headers and its body, and no field here is read
// from either. The host is the caller's, so every field passes answer.ClipFields as a call
// line's do, and the line stays one Cloud Logging entry whatever the caller asked for.
func dialFields(grant requests.AirlockConnection, host, port, address string) []any {
	return []any{
		"enrollment", grant.EnrollmentID,
		"grant", grant.GrantID,
		"session", grant.SessionID,
		"runtime", grant.RuntimeID,
		"issue", grant.Issue,
		"host", host,
		"port", port,
		"address", address,
	}
}

// logTunnel writes the one line a tunnel the proxy opened logs. It is written once the tunnel is
// open, so a line means bytes could cross, and nothing of what crosses is read or written down.
func (s *Server) logTunnel(ctx context.Context, host, port, address string) {
	s.log.Info(tunnelMessage, answer.ClipFields(dialFields(callerOfContext(ctx), host, port, address))...)
}

// logDialFailure writes the one line a dial that did not happen logs: the same fields, the class
// of the failure, and the broker's own words for it. It is at WARNING, as a refusal is: a dial
// the rules refused and a service that could not be reached are each a call that did not happen,
// and an operator reading the log is reading for exactly those.
func (s *Server) logDialFailure(ctx context.Context, host, port, address, failure, reason string) {
	fields := append(dialFields(callerOfContext(ctx), host, port, address), "failure", failure, "reason", reason)
	s.log.Warn(dialFailedMessage, answer.ClipFields(fields)...)
}

// localRule names the rule that refuses addr, and "" for an address the proxy may dial. Each
// answer names the rule and the address family, because an operator reading a refusal has to
// know which of a name's addresses it was about: a name with one public IPv4 address and one
// link-local IPv6 address is refused, and the line says which half did it.
//
// clusterRanges are the broker cluster's own ranges (Options.ClusterRanges), read after the
// fixed rules: a Service range such as 34.118.224.0/20 is global unicast, so it is the one rule
// that stands between a tunnel and a ClusterIP of the cluster the broker runs in, and an address
// a fixed rule already refuses keeps that rule's more particular name.
func localRule(addr netip.Addr, clusterRanges []netip.Prefix) string {
	// A 4-in-6 address is the IPv4 address it holds, for every predicate below and for the
	// family each answer names.
	addr = netaddr.Canonical(addr)
	family := "IPv6"
	if addr.Is4() {
		family = "IPv4"
	}
	switch {
	case addr.IsLoopback():
		// 127.0.0.0/8 and ::1: the broker's own pod.
		return "a loopback " + family + " address"
	case clusterRange.Contains(addr):
		// 100.64.0.0/10: the cluster's own nodes and pods.
		return "a cluster " + family + " address"
	case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10: the node's metadata server among them.
		return "a link-local " + family + " address"
	case addr.IsPrivate():
		// 10/8, 172.16/12 and 192.168/16; fc00::/7, which is unique-local rather than private.
		if family == "IPv6" {
			return "a unique-local IPv6 address"
		}
		return "a private IPv4 address"
	case !addr.IsGlobalUnicast():
		// Everything left that is not a host on the public internet: the unspecified address,
		// multicast, the IPv4 broadcast address, and the interface-local scopes.
		return "not a global unicast " + family + " address"
	}
	for _, own := range clusterRanges {
		if own.Contains(addr) {
			return "an " + family + " address inside BROKER_AIRLOCK_CLUSTER_RANGES, the broker cluster's own addresses"
		}
	}
	return ""
}
