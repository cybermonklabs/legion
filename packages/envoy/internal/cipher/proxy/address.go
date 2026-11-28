// packages/envoy/internal/cipher/proxy/address.go
// Where the proxy may dial, and what it dials there. A command's CONNECT names the host, so
// without this check the proxy is an open relay into the cluster the broker runs in: the pod's
// own services, the node's metadata server, the broker's own API and its database all answer a
// dial from this process, and a tunnel would carry whatever a command asked for straight to
// them. Every dial this package makes - a tunnel's, a bound host's and a plain-HTTP request's
// alike - resolves the name once here, refuses the whole name when any one of its addresses is
// one the proxy never dials, and then dials those addresses rather than the name. So a name
// that answers one address to the check and another to the dial, which is DNS rebinding,
// reaches the address that was checked or nothing at all. Which addresses the proxy never dials
// is netaddr.DialRefusal's rule, beside the private destinations' (netaddr.DestinationRefusal);
// this file resolves, holds each address to the rule and dials. The one dial the other way round
// is a private destination's (upstreamAddresses): a host the broker's own settings name as inside
// its cluster, dialled only at addresses in the ranges the destination's rule names, and only for
// the allowed requests of the one bound host it serves.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/netaddr"
	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// ResolveHost is the default of Server.resolve: the addresses a host name has right now, or the
// address a host that is an address literal already is. It is exported because a caller that
// gives Options.Resolve a resolver of its own - cmd/cipherd, whose BROKER_AIRLOCK_TEST_RESOLVE
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
		s.logDialFailure(ctx, host, port, "", 0, failedClass(ctx, err, dialUnresolved), refused.Error())
		return nil, refused
	}
	if len(addrs) == 0 {
		refused := fmt.Errorf("%s resolves to no address", host)
		s.logDialFailure(ctx, host, port, "", 0, dialUnresolved, refused.Error())
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
		if rule := netaddr.DialRefusal(addr, s.clusterRanges); rule != "" {
			refused := fmt.Errorf("%s resolves to %s, %s, which this proxy never dials", host, addr, rule)
			s.logDialFailure(ctx, host, port, net.JoinHostPort(addr.String(), port), len(addrs), dialRefused, refused.Error())
			return nil, refused
		}
	}
	return addrs, nil
}

// dialChecked is one dial of a host outside the broker, check and all: the addresses the name
// has now, the refusal of the whole name when any of them is one this proxy never dials, and a
// dial of those addresses. It is the DialContext of the transport every bound host's request goes
// out on, where a refusal and a failed dial are one answer to the caller; a plain-HTTP request
// goes out on a transport of its own, whose dial is dialForwarded. The two paths that must refuse
// a request before they take a command's connection over - a CONNECT and a plain-HTTP request -
// read checkedAddresses themselves, so that their refusal is a 403 rather than a 502, and a
// CONNECT hands what it answered to dialAddrs.
func (s *Server) dialChecked(ctx context.Context, network, addr string) (net.Conn, error) {
	return s.dialName(ctx, network, addr, false)
}

// dialForwarded is dialChecked for the transport a plain-HTTP request is forwarded on, and the
// one line each socket it opens writes, a tunnel's (logTunnel): the proxy decides nothing of a
// plain-HTTP request and puts nothing into it, as it decides nothing of a tunnel, so the line
// is the whole of what the broker records about where the request went. That transport keeps no
// idle connection and speaks HTTP/1.1 alone (Server.forward), so each request opens a socket of
// its own and writes its own line, and no socket opened for one caller carries another's request.
func (s *Server) dialForwarded(ctx context.Context, network, addr string) (net.Conn, error) {
	return s.dialName(ctx, network, addr, true)
}

// dialName is the dial behind dialChecked and dialForwarded: addr split into the host and port a
// transport asks for, checked (checkedAddresses) and dialled (dialAddrs), with the line of a dial
// that failed, and a tunnel's line for a socket it opened when tunnel is set. A bound host's
// socket writes no line of its own: the requests on it are read, and each writes its call line or
// its refusal line, but for a call whose command goes away while its answer streams, which writes
// neither, since ReverseProxy aborts the handler that was copying it.
func (s *Server) dialName(ctx context.Context, network, addr string, tunnel bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	addrs, err := s.checkedAddresses(ctx, host, port)
	if err != nil {
		return nil, err
	}
	conn, dialed, err := s.dialAddrs(ctx, network, addrs, port)
	if err != nil {
		s.logUnreachable(ctx, host, port, addrs, err)
		return nil, err
	}
	if tunnel {
		s.logTunnel(ctx, host, port, dialed)
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

// addressList is the addresses a dial tried, as one field of its line: as many whole addresses,
// in the order they were dialled, as fit in the bytes answer.ClipFields keeps of a field. A cut
// through an address would leave half of it, which can read as another address, so the list
// stops at the last address that fits whole; a name has that many addresses only when its owner
// gave it them, and the line's resolved field says how many were dialled in all.
func addressList(addrs []netip.Addr, port string) string {
	var list strings.Builder
	for _, addr := range addrs {
		dialed := net.JoinHostPort(addr.String(), port)
		if list.Len() > 0 {
			dialed = " " + dialed
		}
		if list.Len()+len(dialed) > answer.FieldBytes {
			break
		}
		list.WriteString(dialed)
	}
	return list.String()
}

// logUnreachable writes the line of a dial no address of which answered: every address the name
// had was dialled in turn (dialAddrs), and err is what each dial answered.
func (s *Server) logUnreachable(ctx context.Context, host, port string, addrs []netip.Addr, err error) {
	s.logDialFailure(ctx, host, port, addressList(addrs, port), len(addrs), failedClass(ctx, err, dialUnreachable), err.Error())
}

// failedClass is the class of a lookup or a dial that failed with err: dialCancelled when the
// request it was made for had ended - its command closed its connection, which net/http answers by
// cancelling the request's context - and every error err joins is that cancel, so the whole of the
// failure was the lookup or dial given up for it; and class otherwise. A dial of several addresses
// joins one error for each, and an address that refused or timed out before the command went away
// is a failure of the service, which keeps its class. Only context.Canceled is that cancel: the
// dialer's own Timeout answers an error that is context.DeadlineExceeded, and nothing sets a
// deadline on a request's context, so a deadline is always a timeout of the service's. A dial a
// transport makes runs under a context its request's end does not cancel, so only a CONNECT's dial
// and the check of a CONNECT or a plain-HTTP request can be given up this way.
func failedClass(ctx context.Context, err error, class string) string {
	if ctx.Err() == nil {
		return class
	}
	errs := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	}
	for _, each := range errs {
		if !errors.Is(each, context.Canceled) {
			return class
		}
	}
	return dialCancelled
}

// tunnelMessage is the message of the one line each socket the proxy opens for a tunnel or for a
// forwarded plain-HTTP request logs, and dialFailedMessage of the one line each dial it does not
// make logs. Those are the things this port carries without reading them: no request of a tunnel
// is decided, nor is a plain-HTTP request, so no call line is written for either, and without
// these two lines nothing in the broker's log says a session reached an address at all.
const (
	tunnelMessage     = "airlock proxy: a tunnel"
	dialFailedMessage = "airlock proxy: a dial that did not happen"
)

// The classes dialFailedMessage's line names, which is what an operator reads a refusal of the
// broker's own rules apart from a service that was down, and both apart from a command that went
// away, by.
const (
	// dialRefused: an address this proxy never dials (netaddr.DialRefusal), so nothing was dialled.
	dialRefused = "refused"
	// dialUnresolved: a host the resolver answered no address for, or an error, so there was
	// nothing to dial.
	dialUnresolved = "unresolved"
	// dialUnreachable: every address of the host was dialled and none answered.
	dialUnreachable = "unreachable"
	// dialCancelled: the request the lookup or the dial was made for ended first - the command
	// closed its connection - and every attempt was given up for that, so its error says nothing
	// about the service or the resolver. An address that failed on its own first keeps the
	// dial's class (failedClass).
	dialCancelled = "cancelled"
)

// callerKey is the context key the grant a request was proved for travels under, for the two
// lines below: the dials this file makes run under the request's own context, and a transport's
// dial runs under it too, inside the transport. serveProxy puts the grant there for a CONNECT and
// a plain-HTTP request, and serveBound puts there the grant each request on a decrypted
// connection read for itself, before anything is dialled for it, so a dial made for that request
// names the caller its own line names, even after the grant has moved since the CONNECT.
type callerKey struct{}

// withCaller is the hand-over of a request's grant to the dials below it.
func withCaller(ctx context.Context, grant requests.AirlockConnection) context.Context {
	return context.WithValue(ctx, callerKey{}, grant)
}

// callerOfContext is the grant a dial is made for: the one withCaller put there, and the zero
// grant for a dial made under none. It reads no other grant - not the one a decrypted connection
// was opened with (connKey), which a grant that moved since the CONNECT no longer is - so a dial
// path that hands no grant over writes an empty caller rather than a stale one.
func callerOfContext(ctx context.Context) requests.AirlockConnection {
	grant, _ := ctx.Value(callerKey{}).(requests.AirlockConnection)
	return grant
}

// dialFields is who a dial was made for and what it was for: the grant, its enrollment, and the
// session and runtime behind it, then the host and port the caller asked for and the address
// this process asked the kernel for. No field is read from a request's body, or from any of its
// headers but the Host header a CONNECT with a path for its target is read by. The host and the
// port are the command's own text: the host is written lowercased, with no zone, and the port as
// the number it names (portNumber), and the address is the resolver's answer with that port.
// Lowercasing and the cut every field passes (answer.ClipFields, which keeps the line one Cloud
// Logging entry) are no redaction: what keeps a placeholder the command wrote into its host off
// the line is the broker's placeholder mask, which every line it writes passes (logmask.Writer,
// under cmd/cipherd's handler).
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

// logTunnel writes the one line a socket the proxy opened for a tunnel or a forwarded plain-HTTP
// request logs. It is written as soon as the socket is open, before the command is answered, so
// a socket whose command went away before its answer reached it is on the log too; nothing of
// what crosses the socket is read or written down.
func (s *Server) logTunnel(ctx context.Context, host, port, address string) {
	s.log.Info(tunnelMessage, answer.ClipFields(dialFields(callerOfContext(ctx), host, port, address))...)
}

// logDialFailure writes the one line a dial that did not happen logs: the same fields, how many
// addresses the host resolved to, the class of the failure, and the broker's own words for it.
// The address is the one refused for a refusal, none for a host that resolved to nothing, and the
// ones dialled for a dial no address answered (addressList). It is at WARNING, as a refusal is: a
// dial the rules refused and a service that could not be reached are each a call that did not
// happen, and an operator reading the log is reading for exactly those.
func (s *Server) logDialFailure(ctx context.Context, host, port, address string, resolved int, failure, reason string) {
	fields := append(dialFields(callerOfContext(ctx), host, port, address),
		"resolved", resolved, "failure", failure, "reason", reason)
	s.log.Warn(dialFailedMessage, answer.ClipFields(fields)...)
}
