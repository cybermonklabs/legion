// packages/envoy/internal/cipher/proxy/connect.go
// The CONNECT ingress: what the proxy does with one CONNECT once its grant is read. The target is
// read once (portNumber), a minted capability's grant is held to its one bound host (mintedHostOnly),
// and the connection is then either opened and read by the proxy itself (connectBound), for a host
// a name of the grant, or of another live grant of its enrollment, binds (sessionBinds), or carried
// as bytes it never reads (connectTunnel). serveProxy decides that a request is a CONNECT; what a
// decrypted request may carry is bound.go's.
package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"

	"github.com/sjawhar/envoy/internal/cipher/netaddr"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/fold"
)

// serveConnect answers one CONNECT: a host a name of the grant, or of another live grant of its
// enrollment, is bound to is decrypted and read, any other host is a tunnel. Both are refused
// before any dial when the host resolves somewhere this proxy never goes; the private
// destination's bound host, when it is bound for the session, is refused when the destination
// resolves anywhere but where upstreamAddresses allows.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request, placeholder string, grant requests.AirlockConnection) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	// The authority, with its host ASCII lower-cased: it is the URL and the Host header of
	// every request this connection carries, and the one string a kind's hosts and the
	// request's own Host header are compared against. The reason quotes it lowercased too, as
	// every line names a host the command wrote: a command may write anything here, its own
	// placeholder among it.
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		s.refuse(w, r, call{grant: grant, host: requestHost(r)}, http.StatusBadRequest, codeConnectTarget, fold.ASCII(r.Host)+" is no host and port")
		return
	}
	// An address literal written as every address check reads it (netaddr.Host): the bound-host
	// check, the certificate and the dial below all read this one spelling of the host.
	host = netaddr.Host(fold.ASCII(host))
	// And the port as the number it names, in the one spelling of that number, before the check,
	// the bound-host decision, the dial and the lines read it (portNumber). The refusal names no
	// port: the text that is no port is the command's own, which may be anything at all.
	connectPort, ok := portNumber(port)
	if !ok {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusBadRequest, codeConnectTarget, "the CONNECT to "+host+" names no port from 1 to 65535")
		return
	}
	port = strconv.FormatUint(uint64(connectPort), 10)
	target = net.JoinHostPort(host, port)
	if s.mintedHostOnly(w, r, grant, host, connectPort) {
		return
	}
	// Whether this CONNECT may be decrypted and read: whether a (name, slot) pair of the grant,
	// or of another live grant of its enrollment, has a kind that binds this host on the port
	// the CONNECT named. One answer from the one function that says what is bound for a session
	// (sessionBinds, over candidates), so a change to what is bound is made in one place. The
	// command holds the placeholders of every grant its session has - a role's own GitHub grant
	// beside the one an agent-secrets run inside it asked for - and its password names one of
	// them, so a host only another of them binds is read all the same.
	//
	// A false answer is a tunnel rather than a refusal, whether it is a host no name binds or
	// another port of a bound name: the airlock blocks nothing it does not bind (R3), the
	// proxy reads nothing on such a connection and puts nothing into it, and the command
	// reaches that service exactly as it reaches every host the rules say nothing about. A minted
	// capability's grant is the one exception, refused above (mintedHostOnly). The
	// port is in it because every name the decision compares - the CONNECT host, the TLS name
	// and the Host header - carries no port, so 443 is the only port a kind's hosts speak
	// about and another port of one name is another service.
	bound, err := s.sessionBinds(r.Context(), s.grants.AirlockRules(), grant, host, connectPort)
	if err != nil {
		s.refuseRead(w, r, call{grant: grant, host: host}, "enrollment", err)
		return
	}
	// The addresses this name has, read and checked once: the tunnel below dials these and
	// resolves nothing of its own, and a bound host's requests are checked and dialled the
	// same way by the transport they go out on (dialChecked). A private destination's bound
	// host, bound for this session, is checked where its requests go instead, that destination's
	// own addresses (upstreamAddresses), which its transport checks and dials again for each
	// request (dialUpstream); its public name is not read, since nothing is sent there. Not
	// bound for this session, it is a host like any other.
	var addrs []netip.Addr
	if d := s.destinationOf(host); bound && d != nil {
		_, err = s.upstreamAddresses(r.Context(), d)
	} else {
		addrs, err = s.checkedAddresses(r.Context(), host, port)
	}
	if err != nil {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusForbidden, codeAddressRefused, err.Error())
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, codeNotHijackable, "this connection cannot be taken over")
		return
	}
	if bound {
		s.connectBound(w, hijacker, placeholder, grant, target, host, connectPort)
		return
	}
	s.connectTunnel(w, r, hijacker, target, host, addrs, port)
}

// portNumber is the port a caller wrote as the number it names, and false for text that names no
// port: anything but decimal digits, 0, or a number past 16 bits. A command writes the port it
// asks for, and the dialer reads more than one spelling of one port - leading zeros, a service
// name such as "https", a sign - so a port the proxy did not read for itself could reach a bound
// host's 443 as a tunnel, or be written on a line as text no query for its number finds. The
// proxy reads it once, here, and writes it back as strconv.FormatUint spells the number before
// anything checks it, dials it or logs it.
func portNumber(port string) (uint16, bool) {
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return 0, false
	}
	return uint16(number), true
}

// mintedHostOnly refuses, and answers true for, a CONNECT or a plain-HTTP request of a minted
// capability's grant (requests.AirlockConnection.Minted) to anything but the one host its grant
// binds, on 443: such a grant's work is requests to that one host, which the proxy decrypts and
// reads, so its placeholder opens no tunnel and no plain forward to any other host or port, and is no
// egress password of the broker's address. Every other grant keeps the airlock's rule that it
// blocks nothing it does not bind. It is answered before anything is resolved or dialled, with
// codeMintedHostOnly and the rule's words.
func (s *Server) mintedHostOnly(w http.ResponseWriter, r *http.Request, grant requests.AirlockConnection, host string, port uint16) bool {
	if !grant.Minted || len(candidates(s.grants.AirlockRules(), grant, host, port)) > 0 {
		return false
	}
	s.refuse(w, r, call{grant: grant, host: host}, http.StatusForbidden, codeMintedHostOnly,
		"a minted capability's grant carries its requests to the host its grant binds and nowhere else: refusing "+net.JoinHostPort(host, strconv.FormatUint(uint64(port), 10)))
	return true
}

// connectBound answers a CONNECT to a host a name of the grant, or of another live grant of its
// enrollment, is bound to, on port 443: the proxy opens the TLS itself, with a certificate of its
// own authority, and hands the decrypted connection to the inner server. What travels with the
// connection is the target and the password it was opened with: each request on it reads the
// grant again with that password and builds its own candidates from what that read answers
// (serveBound), so a grant that ends or a name that is withheld between two requests stops at
// the second one.
func (s *Server) connectBound(w http.ResponseWriter, hijacker http.Hijacker, placeholder string, grant requests.AirlockConnection, target, host string, port uint16) {
	client, err := hijack(hijacker)
	if err != nil {
		return
	}
	if _, err := client.Write(established(w)); err != nil {
		client.Close()
		return
	}
	cut := &cutConn{Conn: newIdleConn(client, s.idle)}
	decrypted := tls.Server(cut, &tls.Config{
		// http/1.1 first and h2 after it, and crypto/tls answers the first protocol of this
		// list the client offers too, so a client that offers both - net/http's own, curl,
		// git - speaks HTTP/1.1 here, and a client that offers h2 alone - every gRPC client -
		// speaks HTTP/2. One HTTP/2 connection carries many requests at once, each a stream
		// with its own :authority, which net/http puts in r.Host; each stream is one request
		// to serveBound, which reads the grant again for it and has proxyrules.Decide check its
		// Host against the CONNECT target, and what it forwards goes to that target whatever
		// the stream named. So the Host check covers every stream, as it covers every
		// request of an HTTP/1.1 connection.
		NextProtos: []string{"http/1.1", "h2"},
		// HTTP/2 over TLS needs 1.2 at least (RFC 9113, section 9.2). It is crypto/tls's own
		// default for a server, written here so the HTTP/2 this connection speaks never rests
		// on a default.
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			// The CONNECT target's host, not the ClientHello's name: a client that sends no
			// SNI still gets the certificate of the host it asked for, and a client whose SNI
			// differs from its CONNECT is refused by the decision rather than served a
			// certificate for the name it smuggled in.
			return s.ca.leaf(host)
		},
	})
	connection := net.Conn(decrypted)
	s.conns.Store(connection, connInfo{target: target, host: host, port: port, password: placeholder, grant: grant, cut: cut})
	s.track(connection)
	if !s.innerLn.hand(connection) {
		// The drain closed the listener between the hijack and here.
		s.conns.Delete(connection)
		s.forget(connection)
		connection.Close()
	}
}

// connectTunnel answers a CONNECT to every other host: bytes in both directions and nothing
// read. The dial comes before the hijack, so a service that cannot be reached is a 502 the
// command's own HTTP client reports, rather than a tunnel that opens and then dies. It dials
// the addresses serveConnect checked, in order, and resolves nothing of its own.
//
// The socket that opened writes one line (logTunnel) as soon as it is open, and a dial that
// failed writes one (logUnreachable): no request of a tunnel is read, so these are the whole of
// what the broker knows about where a command went through it. The line comes before the hijack
// and the 200, so a command that went away between its dial and its answer, whose tunnel never
// carries a byte, still leaves the line of the socket the proxy opened for it.
func (s *Server) connectTunnel(w http.ResponseWriter, r *http.Request, hijacker http.Hijacker, target, host string, addrs []netip.Addr, port string) {
	upstream, dialed, err := s.dialAddrs(r.Context(), "tcp", addrs, port)
	if err != nil {
		s.logUnreachable(r.Context(), host, port, addrs, err)
		fail(w, http.StatusBadGateway, codeUpstream, "dial "+target+": "+err.Error())
		return
	}
	s.logTunnel(r.Context(), host, port, dialed)
	client, err := hijack(hijacker)
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write(established(w)); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	// The idle limit on both sides, so that neither end of the tunnel outlives it. The
	// command's side alone would not: once that side has half-closed, the copy below sits in
	// a read of the upstream connection, which a service that answers nothing and closes
	// nothing never ends, and closing the command's side would not wake it. Both wrappers see
	// the same bytes at the same moment - every byte read from one end is written to the
	// other - so the window is the one window, counted twice.
	bounded := newIdleConn(client, s.idle)
	boundedUpstream := newIdleConn(upstream, s.idle)
	ends := bothEnds{client: bounded, upstream: boundedUpstream}
	s.track(ends)
	// The one end of this tunnel, whichever copy reaches it first: both connections closed and
	// the registry's entry dropped, so nothing of a tunnel outlives the copy that ended it.
	var closed sync.Once
	end := func() {
		closed.Do(func() {
			ends.Close()
			s.forget(ends)
		})
	}
	go func() {
		if _, err := io.Copy(boundedUpstream, bounded); err != nil {
			// The command's side is gone, or the idle limit closed it: there is nothing left
			// to carry in either direction.
			end()
			return
		}
		// A command that has said everything it has to say - git's upload-pack, a shell
		// pipeline - closes its half and waits for the answer, so the half-close is passed on
		// rather than turned into a close of the whole tunnel. What bounds the half-closed
		// state is the upstream's own idle limit above: the service has that long to send
		// something, and each byte it sends pushes the window out again.
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
			return
		}
		end()
	}()
	go func() {
		_, _ = io.Copy(bounded, boundedUpstream)
		end()
	}()
}
