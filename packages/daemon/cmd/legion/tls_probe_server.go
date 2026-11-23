package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The host names the probe's server answers for. The two compromised chains' leaves name the
// public test sites they stand in for, so each client asks for the name it asked the public site
// for; the control's names a host under .test, which no public resolver answers.
const (
	tlsEmSignHost  = "dv-g3b-emudhra.we-have-a-problem.com"
	tlsAAAHost     = "dv-g3b-aaa-emudhra.we-have-a-problem.com"
	tlsControlHost = "control.legion-tls-probe.test"
)

// tlsServerConnLimit bounds the start of each connection to the probe's server, counted from its
// accept: its first byte, a proxy CONNECT and the TLS handshake (handle); and it bounds the read of
// each request's header, counted from the start of that read (ReadHeaderTimeout). It is the longest
// a client's first run at a site may last (tlsChromiumProbeTimeout). Once the HTTP server takes a
// connection, Go's net/http clears that deadline after the handshake and sets none between
// requests, since the server sets
// no IdleTimeout, so an idle connection stays open until its client closes it or the server
// closes; a client the probe has stopped closes its connections when its process group goes. No
// verdict rests on the bound: a second run, within two times its client's limit (up to 180 s for
// Chromium), may outlast it, and loses only a connection that hung in its handshake or a header.
const tlsServerConnLimit = tlsChromiumProbeTimeout

// tlsServedSite is one host one of the probe's listeners answers for: its site, its chain, the page
// it serves, and how many TLS handshakes the server began for it.
type tlsServedSite struct {
	site tlsSite
	cert tls.Certificate
	page http.HandlerFunc
	seen atomic.Int64
}

// tlsListener is one listener of the probe's server and the hosts it answers for. The fixture
// chains and the vendored public chains name the same two hosts, so each set has a listener, and
// a port, of its own.
type tlsListener struct {
	listener net.Listener
	hosts    map[string]*tlsServedSite
}

// tlsServer is the probe's own TLS server: two listeners on 127.0.0.1, each at a port the kernel
// chooses, so that no public host's name or route decides the probe. One serves the fixture chains
// and the control's; the other serves the two public test sites' own chains, vendored
// (tls-fixtures/vendor.go), which the probe has no key for, with a key of its own: a client
// refuses such a chain for what it finds in it, as at the public site, or accepts it and then
// fails the server's signature (tlsSignatureFailure), which the probe reads as an acceptance.
//
// Every client reaches a listener under the site's own host name, so each verifies the leaf by the
// name it would verify the public site's by, and no client resolves that name: a client that speaks
// HTTP reaches it as its HTTPS proxy, sending CONNECT <host>:443, and redis-cli and socat, which
// have no proxy, connect to the listener itself and name the host in their TLS server name (SNI).
// The server reads which of the two a connection is from its first byte: 0x16 begins a TLS
// handshake record, and anything else is a proxy request. It takes a CONNECT only to one of the
// listener's hosts at port 443 and answers every other request 403, so a client's fetch of any other
// URL through it fails at once (Chromium's AIA fetches among them) and nothing reaches the network.
// After the handshake it serves HTTP/1.1: at the compromised hosts, the answer the public test sites
// give (200 for GET, HEAD and OPTIONS at every path, 405 for any other method), and at the control,
// a page at / and 404 elsewhere, as example.com answered; so the answers the clients' response
// markers read are the ones those sites gave.
//
// The server counts the handshakes it begins for each site (tlsSite.seen), and the probe reads a
// verdict at a site as the client's only where that count moved while the client ran
// (probeTLSClients): a client that went around the server, to the public host of the same name,
// never decides the probe.
type tlsServer struct {
	fixtures, vendored *tlsListener
	http               *http.Server
	conns              chan net.Conn
	closed             chan struct{}
	once               sync.Once
	wait               sync.WaitGroup
}

// newTLSServer builds the fixture chains for this run (newTLSFixture, newTLSControlFixture), writes
// their three roots and the two CA-published roots (writeTLSPublishedRoots) into stateDir, where the
// positive controls and the control's clients read them, and starts the probe's server on
// 127.0.0.1. Close stops it.
func newTLSServer(stateDir string) (*tlsServer, error) {
	server := &tlsServer{conns: make(chan net.Conn), closed: make(chan struct{})}
	fixtures := map[string]*tlsServedSite{}
	for _, fixture := range []struct {
		name, file, host string
		kind             tlsSiteKind
		page             http.HandlerFunc
	}{
		{"emSign", "emsign", tlsEmSignHost, tlsFixtureSite, tlsCompromisedPage},
		{"AAA", "aaa", tlsAAAHost, tlsFixtureSite, tlsCompromisedPage},
		{"control", "control", tlsControlHost, tlsControlSite, tlsControlPage},
	} {
		var built tlsFixture
		var err error
		if fixture.kind == tlsControlSite {
			built, err = newTLSControlFixture()
		} else {
			var vendored []*x509.Certificate
			if vendored, err = vendoredTLSChain(fixture.host); err == nil {
				built, err = newTLSFixture(vendored)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("the TLS probe's %s fixture: %w", fixture.name, err)
		}
		rootPath := filepath.Join(stateDir, "fixture-"+fixture.file+"-root.pem")
		if err := os.WriteFile(rootPath, built.root, 0o644); err != nil {
			return nil, fmt.Errorf("write the TLS probe's %s fixture root: %w", fixture.name, err)
		}
		served := &tlsServedSite{cert: built.cert, page: fixture.page}
		served.site = tlsSite{name: fixture.name, url: "https://" + fixture.host + "/", root: rootPath, kind: fixture.kind, seen: &served.seen}
		if fixture.kind == tlsControlSite {
			served.site.trust = rootPath
		}
		fixtures[fixture.host] = served
	}
	if err := writeTLSPublishedRoots(stateDir); err != nil {
		return nil, fmt.Errorf("the TLS probe's CA-published roots: %w", err)
	}
	vendored := map[string]*tlsServedSite{}
	for _, public := range publicTLSSites {
		served, err := vendoredTLSSite(stateDir, public)
		if err != nil {
			return nil, err
		}
		vendored[tlsSiteURL(public).Hostname()] = served
	}
	var err error
	if server.fixtures, err = listenTLS(fixtures); err != nil {
		return nil, err
	}
	if server.vendored, err = listenTLS(vendored); err != nil {
		_ = server.fixtures.listener.Close()
		return nil, err
	}
	server.http = &http.Server{
		Handler:           http.HandlerFunc(server.serveHTTP),
		ReadHeaderTimeout: tlsServerConnLimit,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	server.wait.Add(3)
	go func() { defer server.wait.Done(); _ = server.http.Serve(tlsConnListener{server}) }()
	go func() { defer server.wait.Done(); server.accept(server.fixtures) }()
	go func() { defer server.wait.Done(); server.accept(server.vendored) }()
	return server, nil
}

// listenTLS opens a listener on 127.0.0.1 for hosts and gives each site its address.
func listenTLS(hosts map[string]*tlsServedSite) (*tlsListener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start the TLS probe's server: %w", err)
	}
	for _, served := range hosts {
		served.site.via = listener.Addr().String()
	}
	return &tlsListener{listener: listener, hosts: hosts}, nil
}

// vendoredTLSSite is public's own chain, vendored, as the probe's server serves it: with a key of
// the leaf's type and size that the server makes, since it has none of the site's. Its root is
// public's CA-published root, in stateDir (writeTLSPublishedRoots), which its positive control
// holds it to (probeVendoredTLSSites): the positive controls' curl, not Go's x509, because Go
// 1.27.3 refused these chains against their own roots ("x509: no valid chains built", measured on
// 2026-11-20), as the image's go client did.
func vendoredTLSSite(stateDir string, public tlsSite) (*tlsServedSite, error) {
	host := tlsSiteURL(public).Hostname()
	chain, err := vendoredTLSChain(host)
	if err != nil {
		return nil, err
	}
	key, err := tlsKeyFor(0, chain[0].PublicKey)
	if err != nil {
		return nil, fmt.Errorf("a stand-in key for the vendored chain of %s: %w", host, err)
	}
	served := &tlsServedSite{cert: tls.Certificate{PrivateKey: key}, page: tlsCompromisedPage}
	for _, cert := range chain {
		served.cert.Certificate = append(served.cert.Certificate, cert.Raw)
	}
	served.site = tlsSite{name: "vendored " + strings.TrimPrefix(public.name, "public "), url: public.url, root: tlsPublishedRootIn(stateDir, public), kind: tlsVendoredSite, seen: &served.seen}
	return served, nil
}

// sites are the five sites the server answers for, in the probe's order: the fixture emSign and AAA
// chains, the control, then the vendored emSign and AAA chains.
func (s *tlsServer) sites() []tlsSite {
	return []tlsSite{
		s.fixtures.hosts[tlsEmSignHost].site, s.fixtures.hosts[tlsAAAHost].site, s.fixtures.hosts[tlsControlHost].site,
		s.vendored.hosts[tlsEmSignHost].site, s.vendored.hosts[tlsAAAHost].site,
	}
}

// Close stops the server and every connection the HTTP server holds; a connection still in its
// handshake ends at its deadline (tlsServerConnLimit) or when its client goes.
func (s *tlsServer) Close() {
	s.once.Do(func() {
		close(s.closed)
		_ = s.fixtures.listener.Close()
		_ = s.vendored.listener.Close()
		_ = s.http.Close()
	})
	s.wait.Wait()
}

func (s *tlsServer) accept(l *tlsListener) {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(l, conn)
	}
}

// handle reads a connection's first byte, takes a CONNECT where that begins no TLS record, makes
// the TLS handshake with the chain of the host l answers for, and hands the connection to the HTTP
// server.
func (s *tlsServer) handle(l *tlsListener, conn net.Conn) {
	deadline := time.Now().Add(tlsServerConnLimit)
	_ = conn.SetDeadline(deadline)
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		_ = conn.Close()
		return
	}
	host := ""
	if first[0] != 0x16 {
		if host, err = l.connect(conn, reader); err != nil {
			_ = conn.Close()
			return
		}
	}
	tlsConn := tls.Server(tlsBufferedConn{Conn: conn, reader: reader}, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if host != "" {
				name = host
			}
			served := l.hosts[name]
			if served == nil || (host != "" && hello.ServerName != "" && hello.ServerName != host) {
				return nil, fmt.Errorf("the TLS probe's server answers for no host %q", hello.ServerName)
			}
			served.seen.Add(1)
			return &served.cert, nil
		},
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		// A client that refused the chain ends here, as a compromised chain's client should, and so
		// does every client of a vendored chain, which the server cannot sign for.
		_ = conn.Close()
		return
	}
	select {
	case s.conns <- tlsConn:
	case <-s.closed:
		_ = conn.Close()
	}
}

// connect reads one proxy request from reader: a CONNECT to one of l's hosts at port 443 is
// answered 200 and its host returned; any other request is answered 403 and refused.
func (l *tlsListener) connect(conn net.Conn, reader *bufio.Reader) (string, error) {
	request, err := http.ReadRequest(reader)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(request.Host)
	if request.Method != http.MethodConnect || err != nil || port != "443" || l.hosts[host] == nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return "", errors.New("the TLS probe's server proxies only a CONNECT to one of its own hosts at port 443")
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return "", err
	}
	return host, nil
}

// serveHTTP answers a request with its host's fixture page: the host the client named in its TLS
// server name, else in its Host header. A vendored chain's handshake completes only for a client
// that does not check the server's handshake signature against the chain's leaf, since the server
// signs with a key of its own; such a client gets the page of the fixture of the same host, the
// page the compromised site gives, which reads as an acceptance.
func (s *tlsServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.TLS.ServerName
	if name == "" {
		name = r.Host
		if host, _, err := net.SplitHostPort(r.Host); err == nil {
			name = host
		}
	}
	served := s.fixtures.hosts[name]
	if served == nil {
		http.NotFound(w, r)
		return
	}
	served.page(w, r)
}

// tlsConnListener hands the HTTP server the TLS connections handle made.
type tlsConnListener struct{ server *tlsServer }

func (l tlsConnListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.server.conns:
		return conn, nil
	case <-l.server.closed:
		return nil, net.ErrClosed
	}
}

func (l tlsConnListener) Close() error   { return nil }
func (l tlsConnListener) Addr() net.Addr { return l.server.fixtures.listener.Addr() }

// tlsBufferedConn is a connection whose reads begin with what reader has buffered.
type tlsBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c tlsBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// tlsCompromisedPage answers as the public test sites the compromised chains stand in for, read on
// 2026-11-20: 200 with this text/plain body for GET, HEAD and OPTIONS at any path, and 405 for any
// other method.
func tlsCompromisedPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		_, _ = io.WriteString(w, "Your client successfully connected to the site. That's bad!\n")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "only GET, HEAD and OPTIONS\n")
	}
}

// tlsControlPage answers as example.com, the control before the probe served its own: an HTML
// page at /, for any method, and 404 at any other path.
func tlsControlPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.URL.Path != "/" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<!doctype html><html><head><title>Not Found</title></head><body><p>Not Found</p></body></html>\n")
		return
	}
	_, _ = io.WriteString(w, "<!doctype html><html><head><title>Legion TLS probe control</title></head><body><p>Legion TLS probe control.</p></body></html>\n")
}
