package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// testGhChildEnv names, to this package's test binary run as `legion` (TestMain), environment
// pairs to hand gh on top of the allowlist runGh builds its environment from (ghChildEnviron), one
// NAME=value a line: the capture proxy and the CA it signs with. The allowlist keeps a caller's
// proxy but no trust store, so the CA reaches gh only this way (testGhChildEnviron).
const testGhChildEnv = "LEGION_TEST_GH_CHILD_ENV"

// capturedRequest is one request a real gh or git sent to GitHub through the capture proxy: the
// owner of the repository it targets, read from its path or its GraphQL variables ("" for a
// request naming none), and the credential it carried.
type capturedRequest struct {
	host, path, owner, auth string
}

// capturedConnect is one CONNECT a proxy was sent: the host and port the tunnel is to, and any
// credential the request itself carried (Authorization or Proxy-Authorization).
type capturedConnect struct {
	host, auth string
}

// captureProxy is an HTTPS proxy that answers CONNECT for any host with a certificate of its own
// CA, so a real gh or git told HTTPS_PROXY and that CA sends it every request it means for GitHub,
// which it records and refuses (401 to an unauthenticated git request, so git asks its credential
// helper, 403 or 404 otherwise). Nothing reaches GitHub.
//
// It answers one request of each command rather than refusing it: the leak guard `legion gh`
// runs before gh reads the destination repository's visibility first, through a gh of its own,
// and a visibility it cannot read refuses the call. That read is the first repository read a
// command makes, so the first `GET /repos/<owner>/<name>` after each take is answered with a
// visibility and every later one is refused as before — the command's own read of the same path
// included, which is what keeps each case's exit status what it was.
type captureProxy struct {
	url, caFile string
	mu          sync.Mutex
	requests    []capturedRequest
	connects    []capturedConnect
	// visibility is whether the next repository read is the guard's, to be answered.
	visibility bool
	// answers are the bodies a row has the proxy answer with, by `<METHOD> <path>` for a REST
	// request and `graphql <operation>` for a GraphQL one (answer), so a command reaches the git
	// it runs after them.
	answers map[string]string
}

var capturedRESTOwner = regexp.MustCompile(`^/repos/([^/]+)/`)
var capturedGitOwner = regexp.MustCompile(`^/([^/]+)/[^/]+/info/refs$`)
var capturedRepository = regexp.MustCompile(`^/repos/[^/]+/[^/]+$`)

// capturedOperation finds a GraphQL request's operation in its JSON body, where the query's text
// is escaped, so `\tquery Name(` has no word boundary before `query`.
var capturedOperation = regexp.MustCompile(`(?:query|mutation)\s+([A-Za-z_]\w*)\s*[({]`)

// answer has the proxy answer the request key names (captureProxy's answers) with body, until
// clearAnswers.
func (p *captureProxy) answer(key, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.answers == nil {
		p.answers = map[string]string{}
	}
	p.answers[key] = body
}

// clearAnswers drops every answer a row set.
func (p *captureProxy) clearAnswers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answers = nil
}

func startCaptureProxy(t *testing.T) *captureProxy {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "legion capture CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	p := &captureProxy{caFile: filepath.Join(t.TempDir(), "ca.pem")}
	if err := os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	var serial int64 = 1
	var certMu sync.Mutex
	leaf := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		certMu.Lock()
		serial++
		n := serial
		certMu.Unlock()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: hello.ServerName}, DNSNames: []string{hello.ServerName},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		}, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
	}

	inner := &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(inner) }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = server.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				reader := bufio.NewReader(conn)
				req, err := http.ReadRequest(reader)
				if err != nil || req.Method != http.MethodConnect {
					_ = conn.Close()
					return
				}
				p.mu.Lock()
				p.connects = append(p.connects, capturedConnect{host: req.Host, auth: req.Header.Get("Authorization") + req.Header.Get("Proxy-Authorization")})
				p.mu.Unlock()
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					_ = conn.Close()
					return
				}
				tlsConn := tls.Server(&bufferedConn{Conn: conn, reader: reader}, &tls.Config{GetCertificate: leaf, NextProtos: []string{"http/1.1"}})
				select {
				case inner.conns <- tlsConn:
				case <-inner.done:
					_ = conn.Close()
				}
			}()
		}
	}()
	p.url = "http://" + ln.Addr().String()
	p.visibility = true
	return p
}

// serve records every request, whatever its host, and refuses it. The test runs gh with its
// telemetry off, so a request for any host but GitHub's API and git host is one the wrapper let
// through to a host it never means. A discussion's category list is answered with one category,
// General, so `gh discussion create` reaches its editor (its body's prompt) as it would at GitHub.
func (p *captureProxy) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := capturedRequest{host: r.Host, path: r.URL.Path, auth: r.Header.Get("Authorization")}
	switch {
	case capturedRESTOwner.MatchString(r.URL.Path):
		request.owner = strings.ToLower(capturedRESTOwner.FindStringSubmatch(r.URL.Path)[1])
	case capturedGitOwner.MatchString(r.URL.Path):
		request.owner = strings.ToLower(capturedGitOwner.FindStringSubmatch(r.URL.Path)[1])
	case r.URL.Path == "/graphql":
		var query struct {
			Variables map[string]any `json:"variables"`
		}
		if json.Unmarshal(body, &query) == nil {
			if owner, ok := query.Variables["owner"].(string); ok {
				request.owner = strings.ToLower(owner)
			}
		}
	}
	p.mu.Lock()
	p.requests = append(p.requests, request)
	// The guard's visibility read, answered once per command (captureProxy's doc).
	guardsRead := p.visibility && r.Method == http.MethodGet && r.Host == "api.github.com" && capturedRepository.MatchString(r.URL.Path)
	if guardsRead {
		p.visibility = false
	}
	key := r.Method + " " + r.URL.Path
	if match := capturedOperation.FindStringSubmatch(string(body)); r.URL.Path == "/graphql" && match != nil {
		key = "graphql " + match[1]
	}
	answer, answered := p.answers[key]
	p.mu.Unlock()
	if guardsRead {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"visibility":"private"}`)
		return
	}
	if answered {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
		return
	}
	if r.URL.Path == "/graphql" && strings.Contains(string(body), "DiscussionCategoryList") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"hasDiscussionsEnabled":true,"discussionCategories":{"nodes":[{"id":"C_1","name":"General","slug":"general","emoji":"","isAnswerable":false}]}}}}`)
		return
	}
	switch {
	case r.Host == "github.com" && request.auth == "":
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
	case r.Host == "github.com":
		w.WriteHeader(http.StatusForbidden)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// take is every request recorded since the last take. It arms the answer to the next command's
// visibility read, since a take is where one command's requests end and the next command's
// begin.
func (p *captureProxy) take() []capturedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := p.requests
	p.requests = nil
	p.visibility = true
	return requests
}

// takeConnects is every CONNECT the proxy was sent since the last takeConnects.
func (p *captureProxy) takeConnects() []capturedConnect {
	p.mu.Lock()
	defer p.mu.Unlock()
	connects := p.connects
	p.connects = nil
	return connects
}

// tunnelProxy is a plain forwarding proxy, the shape of one a deployment reaches GitHub through:
// it records each CONNECT and passes it, and every byte after it, to upstream, here the capture
// proxy standing in for GitHub, recording the bytes the client sent through the tunnel.
type tunnelProxy struct {
	url      string
	mu       sync.Mutex
	connects []capturedConnect
	relayed  bytes.Buffer
}

func startTunnelProxy(t *testing.T, upstream string) *tunnelProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &tunnelProxy{url: "http://" + ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.relay(conn, strings.TrimPrefix(upstream, "http://"))
		}
	}()
	return p
}

// relay reads conn's CONNECT, records it, sends it on to upstream and then copies both ways,
// recording what conn sent.
func (p *tunnelProxy) relay(conn net.Conn, upstream string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.connects = append(p.connects, capturedConnect{host: req.Host, auth: req.Header.Get("Authorization") + req.Header.Get("Proxy-Authorization")})
	p.mu.Unlock()
	next, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer next.Close()
	if err := req.Write(next); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, next)
		close(done)
	}()
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			p.mu.Lock()
			p.relayed.Write(buffer[:n])
			p.mu.Unlock()
			if _, err := next.Write(buffer[:n]); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = next.(*net.TCPConn).CloseWrite()
	<-done
}

// take is every CONNECT the proxy was sent and every byte relayed through it since the last take.
func (p *tunnelProxy) take() ([]capturedConnect, []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	connects, relayed := p.connects, bytes.Clone(p.relayed.Bytes())
	p.connects = nil
	p.relayed.Reset()
	return connects, relayed
}

// chanListener is a net.Listener over the TLS connections the proxy hands it, so an http.Server
// serves each decrypted tunnel.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// bufferedConn is a connection whose first bytes the proxy's CONNECT reader already buffered.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
