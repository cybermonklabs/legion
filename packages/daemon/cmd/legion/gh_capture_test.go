package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
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

// realGHForCapture is the real gh a test that drives one runs: LEGION_GH_PATH when the test runs
// in a Legion pane or pod, whose `gh` on PATH is the wrapper, else the gh on PATH; a machine with
// neither skips, as the other gh-backed tests do. A gh on PATH that is a script skips too: it is
// a wrapper of the machine's own (a gh shim that hands each call to the next gh on PATH, which is
// the `legion gh` this test puts there), as boot refuses one for the real gh (daemon.RefuseScriptGH).
//
// It also puts this test binary's own `legion gh` first on the test's PATH
// (ghWrapperFirstOnPath), because a test that runs `gh` by name must reach the wrapper it is
// testing: Go resolves a command's name against the PATH of the process making the call and
// never against the one the test sets on the child's environment (os/exec LookPath, read when
// exec.Command is built), so on a machine that has a `gh` of its own — a Legion pod's
// /opt/legion/bin/gh, which runs the image's legion and not this build, among them — that gh
// would answer the call and the test would measure it. Every caller runs `gh` that way.
func realGHForCapture(t *testing.T) string {
	t.Helper()
	path := os.Getenv("LEGION_GH_PATH")
	if path == "" {
		found, err := exec.LookPath("gh")
		if err != nil {
			t.Skip("no real gh on this machine: the capture test drives one")
		}
		if err := daemon.RefuseScriptGH(found); err != nil {
			t.Skip(err)
		}
		path = found
	}
	ghWrapperFirstOnPath(t)
	return path
}

// ghWrapperFirstOnPath puts a `gh` that runs this test binary's `legion gh` first on the test's
// PATH for the rest of the test, as a pane's and a pod's PATH carries the wrapper the runtimes
// install. The wrapper runs the binary under test, so a call a test makes by the name `gh`
// reaches the code of this package; t.Setenv puts the PATH back when the test ends, and the
// directory goes with the test's own temporary one.
func ghWrapperFirstOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	wrapper := "#!/bin/sh\n" + testMainEnv + "=1 exec '" + os.Args[0] + "' gh \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Through the real gh and the real git, against a capture proxy standing in for GitHub: every
// request `legion gh` and `legion git-credential` let through goes to GitHub alone and carries the
// token of the owner of the repository it targets — -R before the command, a selector after `--`,
// an option's value read as that option's from the leaf command's own help, GH_REPO under an
// explicit endpoint, -R under GH_REPO, --help=false, a name in --repos under --org, -R after an
// optional-value shorthand, every argument of a plural slot, a placeholder gh fills from GH_REPO,
// the working directory's own repository as gh reads its remotes whatever their scheme — the issue
// owner's token only for the issue owner's repository and for a command naming none run
// where only that owner's remotes are, each other owner's own token for that owner's, and the
// git gh runs itself asking the wrapper for the owner in its path. A command the wrapper cannot
// read for certain (a gh alias or an extension, run, as `gh help`'s target or after --version, a
// command gh's own help does not list, `gh extension exec`, a search qualifier it cannot read,
// GH_HOST naming another host, an argument a later gh's usage names otherwise), one naming two
// owners — an option or another repository naming one owner while gh takes the command's own from
// the working directory or GH_REPO, or a workspace whose remotes name two — and one naming an owner
// the bundle does not hold are refused with exit 2 and send nothing; no alias or extension runs,
// none is installed, and gh's own help and version answer.
func TestRealGhAndGitSendTheTargetOwnersToken(t *testing.T) {
	gh := realGHForCapture(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	proxy := startCaptureProxy(t)
	root := filepath.Join(t.TempDir(), "gh")
	expires := time.Now().Add(time.Hour)
	tokens := map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_write"}
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{
		"acme":    ghconfig.Render(tokens["acme"], "implement", expires),
		"sjawhar": ghconfig.Render(tokens["sjawhar"], "implement", expires),
	}}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "legion"), []byte("#!/bin/sh\n"+testMainEnv+"=1 exec '"+os.Args[0]+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The pane's `gh` wrapper, first on PATH, as the runtimes install it.
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexec '"+filepath.Join(bin, "legion")+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=" + bin + ":" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".state"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".data"), "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"HTTPS_PROXY=" + proxy.url, "https_proxy=" + proxy.url, "NO_PROXY=", "no_proxy=",
		"SSL_CERT_FILE=" + proxy.caFile, "GIT_SSL_CAINFO=" + proxy.caFile,
		"GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1", "GH_SPINNER_DISABLED=1", "NO_COLOR=1",
		"GH_TELEMETRY=false", "DO_NOT_TRACK=1",
		"GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=",
		"GH_CONFIG_DIR=" + ghconfig.Dir(root, "acme"), "LEGION_GH_OWNERS_DIR=" + root, "LEGION_GH_PATH=" + gh,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		// `legion gh` passes every call through the leak guard, which refuses one it can run no
		// shared check for; what this test reads is which token each call carries, so its check
		// clears every call and the guard's own visibility read is answered by the proxy.
		"PUBLIC_LEAK_CHECK=" + leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
		// The capture proxy's CA, for the gh `legion gh` runs: its environment keeps the caller's
		// proxy, the capture proxy above, but no trust store (ghChildEnviron), so only the test
		// binary hands it this CA, which stands in for GitHub's own. SSL_CERT_DIR names an empty
		// directory, so gh trusts no system CA either, and a request that bypassed the proxy would
		// fail its handshake with GitHub rather than reach it.
		testGhChildEnv + "=" + strings.Join([]string{"SSL_CERT_FILE=" + proxy.caFile, "SSL_CERT_DIR=" + t.TempDir(), "GIT_SSL_CAINFO=" + proxy.caFile}, "\n"),
	}
	runIn := func(dir string, env []string, name string, args ...string) (int, string) {
		cmd := exec.Command(name, args...)
		cmd.Env, cmd.Dir = append(append([]string{}, base...), env...), dir
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatalf("%s %q: %v", name, args, err)
		}
		return 0, string(out)
	}
	run := func(env []string, name string, args ...string) (int, string) { return runIn(home, env, name, args...) }
	wantSent := func(what, owner string, git bool) {
		t.Helper()
		requests := proxy.take()
		if len(requests) == 0 {
			t.Errorf("%s sent GitHub nothing", what)
			return
		}
		want := "token " + tokens[owner]
		if git {
			want = "Basic " + base64.StdEncoding.EncodeToString([]byte(ghconfig.GitUser+":"+tokens[owner]))
		}
		for _, r := range requests {
			if r.host != "api.github.com" && r.host != "github.com" {
				t.Errorf("%s sent %s%s with %q, to a host that is not GitHub's", what, r.host, r.path, r.auth)
				continue
			}
			if git && r.auth == "" {
				continue
			}
			if r.auth != want || (r.owner != "" && r.owner != owner) {
				t.Errorf("%s sent %s%s (target owner %q) with %q, want %s's %q", what, r.host, r.path, r.owner, r.auth, owner, want)
			}
		}
	}

	// The control: the gh under test, run directly with acme's directory, sends acme's token. A gh
	// on PATH that does not — a wrapper of the machine's own that routes credentials elsewhere —
	// is no real gh to prove the wrapper against, and LEGION_GH_PATH names the real one.
	run(nil, gh, "api", "repos/acme/widgets")
	if control := proxy.take(); len(control) == 0 || control[0].auth != "token "+tokens["acme"] {
		t.Skipf("%s sent %+v for acme's directory: it does not read GH_CONFIG_DIR; set LEGION_GH_PATH to the real gh", gh, control)
	}

	// acme's directory, the issue owner's, holds gh aliases of its own beside gh's default co
	// (which an aliases key of the file's own replaces, so it is listed again): one naming
	// sjawhar's repository, one nested under pr, and a shell alias; and gh has an extension.
	// Neither alias nor extension may run: each would touch its marker. A gh printing pr view's
	// usage with <URL> for <url>, as no gh this was read from does, stands for a later gh's help.
	ran := filepath.Join(home, "ran")
	config := filepath.Join(ghconfig.Dir(root, "acme"), ghconfig.ConfigFile)
	aliases := ghconfig.Config + "aliases:\n    co: pr checkout\n    pv: pr view -R sjawhar/legion\n    pr pv2: pr view -R sjawhar/legion\n    shx: '!touch " + ran + "-shx'\n"
	if err := os.WriteFile(config, []byte(aliases), 0o600); err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(home, ".data", "gh", "extensions", "gh-hello")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "gh-hello"), []byte("#!/bin/sh\ntouch '"+ran+"-hello'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	drift := filepath.Join(home, "gh-drift")
	if err := os.WriteFile(drift, []byte("#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = --help ]; then\n  '"+gh+"' \"$@\" | sed 's/<url>/<URL>/g'\n  exit\nfi\nexec '"+gh+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Every command of the three reviews' reproductions, and controls: each either sends GitHub
	// the token of the owner it targets, and only that token, or is refused with exit 2 and sends
	// nothing. Each asks for a field only the server answers (title, nameWithOwner), so no gh
	// version answers it locally without a request. deploy-key add reads its key file from the
	// working directory.
	if err := os.WriteFile(filepath.Join(home, "key.pub"), []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl legion-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ghEnv := []string{testMainEnv + "=1"}
	// check runs `legion gh args…` in dir: refused with exit 2 and refuse's text, sending nothing,
	// or sending GitHub alone owner's token on every request. A refusal of an option a gh version
	// lacks names both texts, joined by "||", either of which passes.
	check := func(dir string, env, args []string, owner, refuse string) {
		t.Helper()
		what := "legion gh " + strings.Join(args, " ")
		code, out := runIn(dir, append(append([]string{}, ghEnv...), env...), os.Args[0], append([]string{"gh"}, args...)...)
		if refuse != "" {
			if code != 2 || !slices.ContainsFunc(strings.Split(refuse, "||"), func(text string) bool { return strings.Contains(out, text) }) {
				t.Errorf("%s = exit %d, %q; want exit 2 containing %q", what, code, out, refuse)
			}
			if sent := proxy.take(); len(sent) != 0 {
				t.Errorf("%s sent %+v, want nothing", what, sent)
			}
			return
		}
		if code == 0 {
			t.Errorf("%s exit 0 against the capture proxy, which answers nothing: %s", what, out)
		}
		wantSent(what, owner, false)
	}
	for _, tc := range []struct {
		env    []string
		args   []string
		owner  string // the owner whose token every request carries; "" when refused
		refuse string // the refusal's text, exit 2 and no request
	}{
		// The first re-review's reproductions.
		{nil, []string{"-R", "sjawhar/legion", "pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"--repo=sjawhar/legion", "pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"api", "--", "repos/sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "--json", "title", "--", "https://github.com/sjawhar/legion/pull/1"}, "sjawhar", ""},
		{nil, []string{"repo", "deploy-key", "add", "key.pub", "-R", "sjawhar/legion", "--title", "-Racme/widgets"}, "sjawhar", ""},
		{nil, []string{"pr", "--json", "title", "view", "https://github.com/sjawhar/legion/pull/1"}, "", `"--json" comes before the command's name`},
		// The first review's reproductions.
		{[]string{"GH_REPO=acme/widgets"}, []string{"api", "repos/sjawhar/legion"}, "sjawhar", ""},
		{[]string{"GH_REPO=acme/widgets"}, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets", "--json", "title"}, "", "more than one owner"},
		{nil, []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, "sjawhar", ""},
		// The second re-review's reproductions: --help=false, before the command's name or
		// after; a name in --repos, which gh reads under --org; -c taking its optional value.
		{nil, []string{"api", "repos/sjawhar/legion", "--help=false"}, "sjawhar", ""},
		{nil, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--json", "title"}, "sjawhar", ""},
		{nil, []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion", "--body", "dummy", "-R", "sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"browse", "-c", "-R", "sjawhar/legion", "--no-browser"}, "sjawhar", ""},
		// The gaps it found that fail open: a gh alias at any depth, gh's default co among them;
		// a shell alias; an extension; the Copilot CLI; a search qualifier gh and GitHub read
		// otherwise; GH_HOST naming another host; and a usage spelling an argument otherwise.
		{nil, []string{"pv", "1", "--json", "title"}, "", `"pv" is a gh alias`},
		{nil, []string{"pr", "pv2", "1", "--json", "title"}, "", `"pv2" is a gh alias`},
		{nil, []string{"co", "https://github.com/sjawhar/legion/pull/1"}, "", `"co" is a gh alias`},
		{nil, []string{"shx"}, "", `"shx" is a gh alias`},
		{nil, []string{"hello", "-R", "sjawhar/legion"}, "", `"hello" is a gh extension`},
		// gh 2.65.0 has no copilot; both refuse it before reading its help.
		{nil, []string{"copilot"}, "", "`gh copilot` runs another program"},
		{nil, []string{"search", "issues", "(repo:sjawhar/legion)"}, "", "is a search qualifier this cannot read for certain"},
		{nil, []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, "sjawhar", ""},
		{[]string{"GH_HOST=ghe.example.com"}, []string{"api", "repos/sjawhar/legion"}, "", `GH_HOST is "ghe.example.com"`},
		{[]string{"LEGION_GH_PATH=" + drift}, []string{"pr", "view", "--json", "title", "https://github.com/sjawhar/legion/pull/1"}, "", "<URL>"},
		// The third re-review's: gh's help of an extension runs the extension, so a help target is
		// read as a command, a gh alias or an extension refused; and a plural slot takes one or
		// more arguments, every one a selector.
		{nil, []string{"help", "hello"}, "", `"hello" is a gh extension`},
		{nil, []string{"help", "pr", "pv2"}, "", `"pv2" is a gh alias`},
		{nil, []string{"help", "co"}, "", `"co" is a gh alias`},
		{nil, []string{"issue", "edit", "1", "2", "--title", "capture", "-R", "sjawhar/legion"}, "sjawhar", ""},
		{nil, []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/sjawhar/legion/issues/2", "--title", "capture"}, "sjawhar", ""},
		{nil, []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/acme/widgets/issues/2", "--title", "capture"}, "", "more than one owner"},
		// The final gates' at 96e1672cd: --version before a word gh runs as an alias or an
		// extension; `gh extension exec`; a first word gh's help does not list, an official
		// extension's stub gh installs under CI; an option naming a repository; gh api's colon
		// placeholders, filled from GH_REPO.
		{nil, []string{"--version", "hello"}, "", `"--version" comes before the command's name`},
		{nil, []string{"--version", "shx"}, "", `"--version" comes before the command's name`},
		{nil, []string{"-R", "sjawhar/legion", "--version", "hello"}, "", `"--version" comes before the command's name`},
		{nil, []string{"extension", "exec", "hello"}, "", "`gh extension exec` runs another program"},
		{[]string{"CI=1"}, []string{"stack"}, "", `"stack" is no command gh's own help lists`},
		{nil, []string{"repo", "sync", "acme/widgets", "--source", "sjawhar/legion"}, "", "more than one owner"},
		{[]string{"GH_REPO=sjawhar/legion"}, []string{"api", "graphql", "-F", "o=:owner", "-F", "r=:repo", "-f", "query=query($o:String!,$r:String!){repository(owner:$o,name:$r){id}}"}, "sjawhar", ""},
		// Controls: the issue owner's repository, a command naming none, an owner outside the
		// bundle, and an option the leaf's help does not list.
		{nil, []string{"pr", "view", "--json", "title", "https://github.com/sjawhar/legion/pull/2"}, "sjawhar", ""},
		{nil, []string{"api", "repos/acme/widgets"}, "acme", ""},
		{nil, []string{"api", "graphql", "-f", "query={viewer{login}}"}, "acme", ""},
		{nil, []string{"api", "repos/other/thing"}, "", `the role's GitHub App is not installed on "other"`},
		{nil, []string{"pr", "view", "1", "-R", "sjawhar/legion", "--bogus"}, "", `"--bogus" is no option`},
		// gh 2.104.0's --attach uploads a file the leak guard does not read, so it is refused by
		// name; a gh with no --attach refuses it as an option its help does not list.
		{nil, []string{"issue", "comment", "1", "-R", "sjawhar/legion", "--body", "capture", "--attach", "notes.svg"}, "", `"--attach"`},
		// gh 2.104.0 sends github.com's API requests, with the directory's token, to the host
		// api_host names; gh writes that key, and any key it does not know, with `config set`.
		{nil, []string{"config", "set", "api_host", "api.example.invalid", "--host", "github.com"}, "", "`gh config set` writes the gh directory's configuration"},
	} {
		check(home, tc.env, tc.args, tc.owner, tc.refuse)
	}

	// The caller's proxy is kept, since a deployment that reaches GitHub only through a proxy names
	// it there, and its trust store is not. Through a forwarding proxy, the shape of such a
	// deployment's, the call reaches GitHub, here the capture proxy, with acme's token, and the
	// proxy is sent a CONNECT to api.github.com carrying no credential and, after it, TLS bytes
	// that never hold the token. A second proxy that reads TLS with a CA of its own, given gh as
	// SSL_CERT_FILE or in SSL_CERT_DIR, is sent the same CONNECT and reads no credential inside it:
	// gh does not trust its CA, so it sends no request through it, and nothing reaches GitHub.
	tunnel := startTunnelProxy(t, proxy.url)
	check(home, []string{"HTTPS_PROXY=" + tunnel.url, "https_proxy=" + tunnel.url}, []string{"api", "repos/acme/widgets"}, "acme", "")
	connects, relayed := tunnel.take()
	if !slices.ContainsFunc(connects, func(c capturedConnect) bool { return c.host == "api.github.com:443" }) {
		t.Errorf("legion gh api repos/acme/widgets sent the caller's proxy %+v, want a CONNECT to api.github.com:443", connects)
	}
	for _, c := range connects {
		if c.auth != "" {
			t.Errorf("the CONNECT to %s through the caller's proxy carried %q, want no credential", c.host, c.auth)
		}
	}
	if bytes.Contains(relayed, []byte(tokens["acme"])) {
		t.Errorf("the caller's proxy relayed acme's token in the clear")
	}
	induced := startCaptureProxy(t)
	inducedDir := t.TempDir()
	if ca, err := os.ReadFile(induced.caFile); err != nil || os.WriteFile(filepath.Join(inducedDir, "induced.pem"), ca, 0o600) != nil {
		t.Fatalf("copy the induced proxy's CA: %v", err)
	}
	for _, env := range [][]string{
		{"HTTPS_PROXY=" + induced.url, "https_proxy=" + induced.url, "SSL_CERT_FILE=" + induced.caFile},
		{"HTTPS_PROXY=" + induced.url, "https_proxy=" + induced.url, "SSL_CERT_FILE=", "SSL_CERT_DIR=" + inducedDir},
	} {
		what := fmt.Sprintf("legion gh api repos/acme/widgets under %q", env)
		if code, out := runIn(home, append(append([]string{}, ghEnv...), env...), os.Args[0], "gh", "api", "repos/acme/widgets"); code == 0 {
			t.Errorf("%s exit 0 through a proxy whose CA gh does not trust: %s", what, out)
		}
		connects := induced.takeConnects()
		if !slices.ContainsFunc(connects, func(c capturedConnect) bool { return c.host == "api.github.com:443" }) {
			t.Errorf("%s sent the proxy it names %+v, want a CONNECT to api.github.com:443", what, connects)
		}
		for _, c := range connects {
			if c.auth != "" {
				t.Errorf("%s: the CONNECT to %s carried %q, want no credential", what, c.host, c.auth)
			}
		}
		for _, r := range induced.take() {
			if r.auth != "" {
				t.Errorf("%s: the proxy it names read %s%s with %q inside its tunnel, want no credential", what, r.host, r.path, r.auth)
			}
		}
		if sent := proxy.take(); len(sent) != 0 {
			t.Errorf("%s reached GitHub with %+v, want nothing", what, sent)
		}
	}

	// An editor gh runs after the leak guard has read the call posts what it wrote, which the
	// guard never read; the Copilot API host GitHub names is handed the token by `gh agent-task`.
	// Each is refused before gh runs, whatever names the editor, and the editor never runs.
	editor := filepath.Join(home, "editor.sh")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\ntouch '"+ran+"-editor'\nprintf 'from the editor\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		env    []string
		args   []string
		refuse string
	}{
		{[]string{"GH_EDITOR=" + editor}, []string{"issue", "comment", "1", "-R", "acme/widgets", "-e"}, `"-e" runs an editor program`},
		{[]string{"EDITOR=" + editor}, []string{"pr", "comment", "1", "-R", "acme/widgets", "--editor"}, `"--editor" runs an editor program`},
		{[]string{"VISUAL=" + editor}, []string{"issue", "create", "-R", "acme/widgets", "--title", "t", "-e"}, `"-e" runs an editor program`},
		{[]string{"GH_EDITOR=" + editor}, []string{"gist", "edit", "0123456789abcdef"}, "`gh gist edit` opens an editor program"},
		{nil, []string{"agent-task", "list"}, "`gh agent-task` sends the token to the Copilot API host"},
	} {
		check(home, tc.env, tc.args, "", tc.refuse)
	}

	// An editor gh opens beyond the routes refused by name runs no program of the caller's: gh is
	// handed an editor that fails (ghNoEditor) whatever GH_EDITOR, EDITOR or VISUAL name, and the
	// nano gh falls back to is never reached. `gh discussion create` given an empty body, after it
	// reads the repository's categories, and `gh preview prompter markdown-editor` each ask to
	// launch an editor whatever prompts allow, and launch it on the `e` they read here; each
	// editor fails, so nothing it wrote is sent. Measured with the editor left to gh's own choice:
	// nano ran in both, and discussion create posted what it wrote.
	nano := t.TempDir()
	if err := os.WriteFile(filepath.Join(nano, "nano"), []byte("#!/bin/sh\ntouch '"+ran+"-nano'\nprintf 'from nano\\n' > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	editors := []string{"GH_EDITOR=" + editor, "EDITOR=" + editor, "VISUAL=" + editor, "PATH=" + nano + ":" + bin + ":" + filepath.Dir(git) + ":/usr/bin:/bin"}
	for _, tc := range []struct {
		args    []string
		graphql int // the GraphQL requests gh may send: discussion create's category list
	}{
		{[]string{"discussion", "create", "-R", "acme/widgets", "-t", "T", "-c", "General", "-b", ""}, 1},
		{[]string{"preview", "prompter", "markdown-editor"}, 0},
	} {
		what := "legion gh " + strings.Join(tc.args, " ")
		cmd := exec.Command(os.Args[0], append([]string{"gh"}, tc.args...)...)
		cmd.Env = append(append(append([]string{}, base...), ghEnv...), editors...)
		cmd.Dir, cmd.Stdin = home, strings.NewReader("e\ne\ne\n")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "to launch") {
			t.Errorf("%s = %v, %q; want gh's prompt to launch an editor, and a failure", what, err, out)
		}
		graphql := 0
		for _, r := range proxy.take() {
			if r.path == "/graphql" {
				graphql++
			}
			if r.auth != "token "+tokens["acme"] {
				t.Errorf("%s sent %s%s with %q, want acme's token", what, r.host, r.path, r.auth)
			}
		}
		if graphql != tc.graphql {
			t.Errorf("%s sent %d GraphQL requests, want %d: no request after its editor", what, graphql, tc.graphql)
		}
	}
	for _, marker := range []string{"-editor", "-nano"} {
		if _, err := os.Stat(ran + marker); err == nil {
			t.Errorf("an editor program (%s) ran under `legion gh`", strings.TrimPrefix(marker, "-"))
		}
	}

	// A gh directory whose config.yml holds a hosts block, which carries settings gh reads per
	// host (an editor, a pager, a browser, a socket gh sends its requests through), or an alias
	// named `help`, which gh would run on `legion gh help`, is refused before gh runs; one whose
	// config.yml is no regular file is refused without being opened, so a named pipe there stalls
	// nothing. The pane's own directory is judged before gh reads any help, so each is refused
	// promptly; each run has a bound of its own, which a gh stalled on the pipe would meet.
	acmeConfig := filepath.Join(ghconfig.Dir(root, "acme"), ghconfig.ConfigFile)
	keptConfig, err := os.ReadFile(acmeConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		plant  func() error
		refuse string
	}{
		{"a hosts block", func() error {
			return os.WriteFile(acmeConfig, append(slices.Clone(keptConfig), "hosts:\n    github.com:\n        editor: vi\n"...), 0o600)
		}, "holds a hosts block"},
		{"an alias named help", func() error {
			return os.WriteFile(acmeConfig, []byte(ghconfig.Config+"aliases:\n    help: api repos/sjawhar/legion\n"), 0o600)
		}, "names an alias `help`"},
		{"a named pipe", func() error {
			if err := os.Remove(acmeConfig); err != nil {
				return err
			}
			return syscall.Mkfifo(acmeConfig, 0o600)
		}, "is no regular file"},
	} {
		if err := tc.plant(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "gh", "api", "repos/acme/widgets")
		cmd.Env, cmd.Dir = append(append([]string{}, base...), ghEnv...), home
		out, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), tc.refuse) {
			t.Errorf("legion gh api repos/acme/widgets with %s in config.yml = %v, %q; want exit 2 containing %q", tc.name, err, out, tc.refuse)
		}
		if sent := proxy.take(); len(sent) != 0 {
			t.Errorf("legion gh api repos/acme/widgets with %s in config.yml sent %+v, want nothing", tc.name, sent)
		}
		if err := os.RemoveAll(acmeConfig); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(acmeConfig, keptConfig, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory whose files send gh's requests elsewhere, as a pane can write its own between
	// two of the daemon's refreshes, is refused before anything is sent: api_host in hosts.yml's
	// github.com block, http_unix_socket at the top of config.yml or in a host's block of it.
	acmeDir := ghconfig.Dir(root, "acme")
	for _, edit := range []struct{ file, text, key string }{
		{ghconfig.HostsFile, ghconfig.Hosts(tokens["acme"]) + "    api_host: api.example.invalid\n", "api_host"},
		{ghconfig.ConfigFile, ghconfig.Config + "http_unix_socket: /tmp/example.sock\n", "http_unix_socket"},
		{ghconfig.ConfigFile, ghconfig.Config + "hosts:\n    github.com:\n        http_unix_socket: /tmp/example.sock\n", "http_unix_socket"},
	} {
		path := filepath.Join(acmeDir, edit.file)
		kept, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(edit.text), 0o600); err != nil {
			t.Fatal(err)
		}
		check(home, nil, []string{"api", "repos/acme/widgets"}, "", path+" sets "+edit.key)
		if err := os.WriteFile(path, kept, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check(home, nil, []string{"api", "repos/acme/widgets"}, "acme", "")

	// A command naming no repository, or naming another while gh takes the command's own from the
	// working directory, acts on the repository gh reads from the working directory's git remotes,
	// as gh reads them whatever their scheme: in the issue's own workspace it carries acme's
	// token; in a clone of sjawhar's, sjawhar's; in a clone outside the project, or a workspace
	// whose remotes name two owners, or an option naming another owner than the one gh acts on, it
	// is refused; -R names the repository there.
	workspace := func(remotes [][]string) string {
		dir := t.TempDir()
		for _, args := range append([][]string{fixturegit.InitWorkTree("", "")[1:]}, remotes...) {
			if code, out := runIn(dir, nil, git, args...); code != 0 {
				t.Fatalf("git %q: %s", args, out)
			}
		}
		return dir
	}
	origin := []string{"remote", "add", "origin", "https://github.com/acme/widgets"}
	issueWorkspace := workspace([][]string{origin})
	otherClone := workspace([][]string{{"remote", "add", "origin", "https://github.com/sjawhar/legion"}})
	outsideClone := workspace([][]string{{"remote", "add", "origin", "https://github.com/octocat/hello"}})
	upstream := workspace([][]string{origin, {"remote", "add", "upstream", "git@github.com:sjawhar/legion.git"}})
	setDefault := workspace([][]string{origin, {"config", "remote.origin.gh-resolved", "sjawhar/legion"}})
	gitPlusHTTPS := workspace([][]string{{"remote", "add", "origin", "git+https://github.com/sjawhar/legion"}})
	type row struct {
		dir    string
		env    []string
		args   []string
		owner  string
		refuse string
	}
	rows := []row{
		{issueWorkspace, nil, []string{"pr", "view", "1", "--json", "title"}, "acme", ""},
		{issueWorkspace, nil, []string{"api", "repos/{owner}/{repo}"}, "acme", ""},
		{otherClone, nil, []string{"pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		{otherClone, nil, []string{"repo", "view", "--json", "nameWithOwner"}, "sjawhar", ""},
		{otherClone, nil, []string{"api", "repos/{owner}/{repo}"}, "sjawhar", ""},
		{otherClone, nil, []string{"-R", "acme/widgets", "pr", "view", "1", "--json", "title"}, "acme", ""},
		{outsideClone, nil, []string{"pr", "list", "--json", "title"}, "", `the role's GitHub App is not installed on "octocat"`},
		{upstream, nil, []string{"pr", "list", "--json", "title"}, "", `remote "upstream" (git@github.com:sjawhar/legion.git)`},
		{setDefault, nil, []string{"pr", "list", "--json", "title"}, "", "remote.origin.gh-resolved sjawhar/legion"},
		// Round 4's: remote spellings gh reads as github.com's (deep 2, quality A, final B1).
		{gitPlusHTTPS, nil, []string{"pr", "view", "1", "--json", "title"}, "sjawhar", ""},
		// Round 4's: an option or another repository naming one owner while gh acts on the
		// working directory's (deep 1, quality B, final B2), and the options of deep 3.
		{issueWorkspace, nil, []string{"pr", "comment", "5", "--body", "https://github.com/sjawhar/legion/pull/2"}, "acme", ""},
		{issueWorkspace, nil, []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/9"}, "", "more than one owner||is no option"},
		{issueWorkspace, nil, []string{"repo", "fork", "--org", "sjawhar", "--clone=false", "--remote=false"}, "", "more than one owner"},
		{issueWorkspace, nil, []string{"issue", "create", "--title", "t", "--body", "b", "--blocked-by", "1,https://github.com/sjawhar/legion/issues/5"}, "", "more than one owner||is no option"},
		{otherClone, nil, []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9"}, "sjawhar", ""},
		{otherClone, nil, []string{"issue", "close", "3", "--duplicate-of", "https://github.com/acme/widgets/issues/1"}, "", "more than one owner||is no option"},
		{otherClone, nil, []string{"repo", "sync", "--source", "acme/widgets"}, "", "more than one owner"},
		{otherClone, nil, []string{"issue", "develop", "3", "--branch-repo", "acme/widgets", "--name", "x"}, "", "more than one owner"},
		{outsideClone, nil, []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2"}, "", `the role's GitHub App is not installed on "octocat"`},
		{home, []string{"GH_REPO=acme/widgets"}, []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, "", "more than one owner||is no option"},
		{issueWorkspace, []string{"GH_REPO=sjawhar/legion"}, []string{"project", "link", "1", "--owner", "acme"}, "", "more than one owner"},
		// Round 5's: a passthrough leaf's repository after `--` (deep 1); repo set-default's
		// remotes whatever its argument names (deep 2, quality 1); a pr command's OWNER/REPO#N or
		// non-pull-request URL, a branch of its own repository (final F1).
		{issueWorkspace, nil, []string{"repo", "fork", "--", "sjawhar/legion"}, "sjawhar", ""},
		{issueWorkspace, nil, []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, "", "more than one owner"},
		{issueWorkspace, nil, []string{"repo", "clone", "--", "sjawhar/legion"}, "sjawhar", ""},
		{issueWorkspace, nil, []string{"repo", "set-default", "sjawhar/legion"}, "", "more than one owner"},
		{outsideClone, nil, []string{"pr", "comment", "sjawhar/legion#2", "--body", "hi"}, "", `the role's GitHub App is not installed on "octocat"`},
		{issueWorkspace, nil, []string{"pr", "view", "sjawhar/legion#2", "--json", "title"}, "acme", ""},
		{issueWorkspace, nil, []string{"pr", "view", "https://github.com/sjawhar/legion/issues/5", "--json", "title"}, "", "more than one owner"},
		{otherClone, nil, []string{"pr", "view", "https://github.com/acme/widgets/tree/x", "--json", "title"}, "", "more than one owner"},
		// `gh agent-task` sends the token to the Copilot API host GitHub names, so it is refused
		// whatever it names; a gh with no agent-task refuses it as no command its help lists.
		{issueWorkspace, nil, []string{"agent-task", "view", "https://github.com/sjawhar/legion/issues/1"}, "", "`gh agent-task` sends the token||no command gh's own help lists"},
	}
	// The git gh runs reads the working directory's repository configuration before its command
	// line, and a URL-scoped http.<url>.sslVerify=false there beat the forced http.sslVerify=true
	// (measured with git 2.43: a man-in-the-middle proxy read the credential helper's token). So a
	// repository whose own configuration, or a file it includes, sets an http ssl or proxy key, or a
	// credential helper other than `legion git-credential`, is refused before gh runs, naming the
	// key and never its value. A Legion clone's own helpers and actions/checkout's extraheader pass.
	included := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(included, []byte("[http]\n\tsslCAInfo = /tmp/example.pem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows = append(rows,
		row{workspace([][]string{origin, {"config", "http.https://github.com/.sslVerify", "false"}}), nil, []string{"pr", "view", "1", "--json", "title"}, "", "sets http.https://github.com/.sslverify"},
		row{workspace([][]string{origin, {"config", "http.proxy", "http://127.0.0.1:1"}}), nil, []string{"pr", "view", "1", "--json", "title"}, "", "sets http.proxy"},
		row{workspace([][]string{origin, {"config", "include.path", included}}), nil, []string{"pr", "view", "1", "--json", "title"}, "", "sets include.path, an include"},
		row{workspace([][]string{origin, {"config", "credential.helper", "!/tmp/example-helper"}}), nil, []string{"pr", "view", "1", "--json", "title"}, "", "sets credential.helper to a helper other than"},
		row{workspace([][]string{origin,
			{"config", "credential.https://github.com.helper", ""},
			{"config", "--add", "credential.https://github.com.helper", "!legion git-credential"},
			{"config", "credential.useHttpPath", "true"},
			{"config", "http.https://github.com/.extraheader", "AUTHORIZATION: basic example"},
		}), nil, []string{"pr", "view", "1", "--json", "title"}, "acme", ""},
		// The git `legion gh` routes a call by runs with gh's own environment (gitLines), so a
		// caller's GIT_DIR, which gh's git never sees, cannot route the call to another owner's
		// directory than the repository gh then acts on.
		row{issueWorkspace, []string{"GIT_DIR=" + filepath.Join(otherClone, ".git")}, []string{"pr", "view", "1", "--json", "title"}, "acme", ""},
		// An explicit submodule recursion, which no configuration turns off, is refused by name:
		// `pr checkout --recurse-submodules` runs `git submodule update --init --recursive`. A
		// clone's git flags are judged against an allowlist (ghCloneGitFlags): a flag that sets the
		// clone's configuration, in full or abbreviated, its hooks, its submodules or where its
		// objects come from is refused and sends nothing; the flags a fixture clone of
		// scripts/e2e passes reach GitHub with the owner's token.
		row{issueWorkspace, nil, []string{"pr", "checkout", "1", "--recurse-submodules"}, "", `"--recurse-submodules" runs`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--config", "http.https://github.com/.sslVerify=false"}, "", `hands "--config" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--conf", "http.https://github.com/.sslVerify=false"}, "", `hands "--conf" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--recursive"}, "", `hands "--recursive" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "widgets-dir", "--", "--recurse-submodules=lib"}, "", `hands "--recurse-submodules=lib" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--template", "/tmp/example-template"}, "", `hands "--template" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--reference", "/tmp/example-reference"}, "", `hands "--reference" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", "--", "--separate-git-dir", "/tmp/example-git-dir"}, "", `hands "--separate-git-dir" to git clone`},
		row{home, nil, []string{"repo", "fork", "acme/widgets", "--clone", "--", "--recurse-submodules"}, "", `hands "--recurse-submodules" to git clone`},
		row{home, nil, []string{"gist", "clone", "0123456789abcdef", "--", "--config", "core.hooksPath=/tmp/example-hooks"}, "", `hands "--config" to git clone`},
		row{home, nil, []string{"repo", "clone", "acme/widgets", filepath.Join(t.TempDir(), "widgets"), "--", "--quiet", "--depth", "1", "--branch", "main"}, "acme", ""},
	)
	for _, scheme := range []string{"git+https://github.com/sjawhar/legion.git", "ftp://github.com/sjawhar/legion", "ftps://github.com/sjawhar/legion", "file://github.com/sjawhar/legion", "github.com://sjawhar/legion", "git@github.com/sjawhar:legion"} {
		rows = append(rows, row{workspace([][]string{origin, {"remote", "add", "upstream", scheme}}), nil, []string{"pr", "list", "--json", "title"}, "", "more than one owner"})
	}
	for _, tc := range rows {
		check(tc.dir, tc.env, tc.args, tc.owner, tc.refuse)
	}

	// Every git gh runs gets ghForcedGitConfig, submodule recursion off among it, whatever the
	// command: gh's git client sets no environment of its own (cli/cli v2.104.0 git/client.go), so
	// each git it starts takes gh's, which runGh builds. A git that records its arguments and its
	// GIT_CONFIG_* environment, then runs the real git, is first on gh's PATH, and the proxy
	// answers what each command asks GitHub before its git runs. Each row reads the git that
	// command ran: gist clone, repo clone and repo fork --clone a clone, pr checkout and repo sync a
	// fetch. pr create runs no push: with prompts off, which `legion gh` forces, gh 2.104.0 stops
	// when the branch is on no remote (pkg/cmd/pr/create/create.go:1005-1009), so its row reads that
	// it ran no push.
	gitRecord := filepath.Join(t.TempDir(), "git-record")
	recorder := t.TempDir()
	recordingGit := "#!/bin/sh\n{ printf '%s\\t' \"$*\"; env | grep '^GIT_CONFIG_' | sort | tr '\\n' ' '; echo; } >>'" + gitRecord + "'\nexec '" + git + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(recorder, "git"), []byte(recordingGit), 0o700); err != nil {
		t.Fatal(err)
	}
	repositoryInfo := `{"data":{"repository":{"id":"R_1","databaseId":1,"name":"widgets","owner":{"login":"acme"},"sshUrl":"git@github.com:acme/widgets.git","hasIssuesEnabled":true,"description":"","hasWikiEnabled":false,"viewerPermission":"WRITE","defaultBranchRef":{"name":"main"},"parent":null,"mergeCommitAllowed":true,"rebaseMergeAllowed":true,"squashMergeAllowed":true}}}`
	forkAnswer := `{"node_id":"R_2","name":"widgets-fork","created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","owner":{"login":"acme"}}`
	unpushed := workspace([][]string{origin, {"checkout", "-q", "-b", "feature"}, {"-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "-q", "--allow-empty", "-m", "unpushed"}})
	for _, tc := range []struct {
		name    string
		dir     string
		args    []string
		answers map[string]string
		ran     string // the git subcommand the command must have run; "" for none
		said    string // what gh must have printed; "" for anything
	}{
		{"gh gist clone", t.TempDir(), []string{"gist", "clone", "0123456789abcdef"}, nil, "clone", ""},
		{"gh repo clone", t.TempDir(), []string{"repo", "clone", "acme/widgets"}, map[string]string{"graphql RepositoryInfo": repositoryInfo}, "clone", ""},
		{"gh repo fork --clone", t.TempDir(), []string{"repo", "fork", "acme/widgets", "--clone"}, map[string]string{"POST /repos/acme/widgets/forks": forkAnswer}, "clone", ""},
		{"gh pr checkout", issueWorkspace, []string{"pr", "checkout", "1"}, map[string]string{
			"graphql PullRequestByNumber": `{"data":{"repository":{"pullRequest":{"number":1,"headRefName":"feature","headRepository":{"id":"R_1","name":"widgets"},"headRepositoryOwner":{"id":"U_1","login":"acme"},"isCrossRepository":false,"maintainerCanModify":false}}}}`,
		}, "fetch", ""},
		{"gh repo sync", issueWorkspace, []string{"repo", "sync", "--source", "acme/widgets", "--branch", "main"}, nil, "fetch", ""},
		{"gh pr create", unpushed, []string{"pr", "create", "--title", "a title", "--body", "a body"}, map[string]string{"graphql RepositoryInfo": repositoryInfo}, "", "you must first push the current branch"},
	} {
		if err := os.WriteFile(gitRecord, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for key, body := range tc.answers {
			proxy.answer(key, body)
		}
		recordedPath := "PATH=" + bin + ":" + recorder + ":" + filepath.Dir(git) + ":/usr/bin:/bin"
		code, out := runIn(tc.dir, append(append([]string{}, ghEnv...), recordedPath), os.Args[0], append([]string{"gh"}, tc.args...)...)
		proxy.clearAnswers()
		for _, r := range proxy.take() {
			if r.auth != "" && r.auth != "token "+tokens["acme"] && r.auth != "Basic "+base64.StdEncoding.EncodeToString([]byte(ghconfig.GitUser+":"+tokens["acme"])) {
				t.Errorf("%s sent %s%s with %q, want acme's token or none", tc.name, r.host, r.path, r.auth)
			}
		}
		if tc.said != "" && !strings.Contains(out, tc.said) {
			t.Errorf("%s = exit %d, %q; want it to say %q", tc.name, code, out, tc.said)
		}
		recorded, err := os.ReadFile(gitRecord)
		if err != nil {
			t.Fatal(err)
		}
		ran := false
		for _, line := range strings.Split(strings.TrimSpace(string(recorded)), "\n") {
			argv, environ, _ := strings.Cut(line, "\t")
			if tc.ran != "" && strings.Contains(" "+argv+" ", " "+tc.ran+" ") {
				ran = true
			}
			if strings.Contains(" "+argv+" ", " push ") {
				t.Errorf("%s ran git %q, want no push", tc.name, argv)
			}
			pairs := map[string]string{}
			values := map[string]string{}
			for _, pair := range strings.Fields(environ) {
				name, value, _ := strings.Cut(pair, "=")
				if index, ok := strings.CutPrefix(name, "GIT_CONFIG_KEY_"); ok {
					pairs[index] = value
				} else if index, ok := strings.CutPrefix(name, "GIT_CONFIG_VALUE_"); ok {
					values[index] = value
				}
			}
			got := map[string]string{}
			for index, key := range pairs {
				got[key] = values[index]
			}
			// Named here rather than read from ghForcedGitConfig, so a change that drops one
			// fails this row instead of changing what it checks.
			for _, forced := range [][2]string{{"http.sslVerify", "true"}, {"submodule.recurse", "false"}, {"fetch.recurseSubmodules", "false"}, {"push.recurseSubmodules", "no"}} {
				if got[forced[0]] != forced[1] {
					t.Errorf("%s ran git %q with %s=%q, want %q", tc.name, argv, forced[0], got[forced[0]], forced[1])
				}
			}
		}
		if tc.ran != "" && !ran {
			t.Errorf("%s = exit %d, %q, and ran no git %s; the git it ran: %q", tc.name, code, out, tc.ran, recorded)
		}
	}

	// A clone's git flags are its own configuration: `-c http.<url>.sslVerify=false` and `-c
	// http.<url>.proxy=<proxy>` after `--` send the clone, and the token gh's credential helper
	// answers, to that proxy, whatever runGh forces, since a URL-scoped key beats a generic one
	// (the second review's reproduction, with real gh 2.98.0 and git 2.47.3). Each clone leaf's
	// call is refused before gh runs, so the proxy, one whose CA nothing trusts, is sent nothing,
	// not even a CONNECT. The proxy answers what each leaf asks GitHub first, so a call this did
	// not refuse would reach its git clone.
	cloneFlags := func(host string) []string {
		return []string{"-c", "http.https://" + host + "/.sslVerify=false", "-c", "http.https://" + host + "/.proxy=" + induced.url}
	}
	for _, tc := range []struct {
		name    string
		args    []string
		answers map[string]string
	}{
		{"gh repo clone", append([]string{"repo", "clone", "acme/widgets", "w2", "--"}, cloneFlags("github.com")...), map[string]string{"graphql RepositoryInfo": repositoryInfo}},
		{"gh repo fork --clone", append([]string{"repo", "fork", "acme/widgets", "--clone", "--"}, cloneFlags("github.com")...), map[string]string{"POST /repos/acme/widgets/forks": forkAnswer}},
		{"gh gist clone", append([]string{"gist", "clone", "0123456789abcdef", "--"}, cloneFlags("gist.github.com")...), nil},
	} {
		for key, body := range tc.answers {
			proxy.answer(key, body)
		}
		induced.take()
		induced.takeConnects()
		code, out := runIn(t.TempDir(), ghEnv, os.Args[0], append([]string{"gh"}, tc.args...)...)
		proxy.clearAnswers()
		if code != 2 || !strings.Contains(out, `hands "-c" to git clone`) {
			t.Errorf("%s with -c flags after -- = exit %d, %q; want exit 2 naming -c", tc.name, code, out)
		}
		if connects, requests := induced.takeConnects(), induced.take(); len(connects) != 0 || len(requests) != 0 {
			t.Errorf("%s sent the proxy its -c flags name %+v and %+v, want nothing", tc.name, connects, requests)
		}
		if sent := proxy.take(); len(sent) != 0 {
			t.Errorf("%s sent %+v, want nothing", tc.name, sent)
		}
	}
	// gh's own help, its topics and its version still answer, exit 0, and send nothing.
	for _, args := range [][]string{{"help"}, {"help", "pr", "view"}, {"help", "environment"}, {"pr", "view", "--help"}, {"--version"}} {
		code, out := run(ghEnv, os.Args[0], append([]string{"gh"}, args...)...)
		if code != 0 || out == "" {
			t.Errorf("legion gh %s = exit %d, %q; want gh's help, exit 0", strings.Join(args, " "), code, out)
		}
		if sent := proxy.take(); len(sent) != 0 {
			t.Errorf("legion gh %s sent %+v, want nothing", strings.Join(args, " "), sent)
		}
	}
	for _, marker := range []string{"-shx", "-hello"} {
		if _, err := os.Stat(ran + marker); err == nil {
			t.Errorf("%s ran: `legion gh` ran a gh alias or extension", strings.TrimPrefix(marker, "-"))
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".data", "gh", "extensions", "gh-stack")); err == nil {
		t.Errorf("legion gh stack installed gh's stack extension")
	}

	// The git gh runs itself (a clone, a fork's parent, `pr checkout`) authenticates through the
	// credential helper gh names after its own executable, GH_PATH (quality C). `legion gh` sets
	// GH_PATH to the pane's wrapper (TestLegionGhExecsTheRealGhWithTheOwnersDirectory), so the
	// helper the real gh writes under that GH_PATH is the wrapper's `auth git-credential`, and git,
	// told useHttpPath as `legion gh` tells gh's git, gets the token of the owner in each request's
	// path from it, never GH_CONFIG_DIR's acme token for sjawhar's repository. `legion gh auth
	// setup-git` itself is refused: it writes git's global configuration, which the git `legion gh`
	// runs never reads (GIT_CONFIG_GLOBAL=/dev/null), so git could not write it either.
	check(home, nil, []string{"auth", "setup-git", "--hostname", "github.com"}, "", "`gh auth setup-git` writes git's global configuration")
	gitConfig := filepath.Join(home, "gh-gitconfig")
	if code, out := run([]string{"GIT_CONFIG_GLOBAL=" + gitConfig, "GH_PATH=" + filepath.Join(bin, "gh")}, gh, "auth", "setup-git", "--hostname", "github.com"); code != 0 {
		t.Fatalf("gh auth setup-git under the wrapper's GH_PATH = exit %d: %s", code, out)
	}
	if sent := proxy.take(); len(sent) != 0 {
		t.Errorf("gh auth setup-git sent %+v, want nothing", sent)
	}
	written, err := os.ReadFile(gitConfig)
	if wrapperHelper := "helper = !" + filepath.Join(bin, "gh") + " auth git-credential"; err != nil || !strings.Contains(string(written), wrapperHelper) {
		t.Fatalf("the helper gh wrote = %q, %v; want %q", written, err, wrapperHelper)
	}
	ghGit := []string{"GIT_CONFIG_GLOBAL=" + gitConfig, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_0=true"}
	for repo, owner := range map[string]string{"sjawhar/legion": "sjawhar", "Acme/widgets": "acme"} {
		run(ghGit, git, "-c", "credential.interactive=false", "ls-remote", "https://github.com/"+repo)
		wantSent("git ls-remote "+repo+" through the helper gh wrote", owner, true)
	}

	helper := []string{"-c", "credential.helper=", "-c", "credential.helper=!legion git-credential", "-c", "credential.useHttpPath=true", "-c", "credential.interactive=false"}
	for repo, owner := range map[string]string{"sjawhar/legion": "sjawhar", "Acme/widgets": "acme"} {
		run(nil, git, append(helper, "ls-remote", "https://github.com/"+repo)...)
		wantSent("git ls-remote "+repo, owner, true)
	}
	code, out := run(nil, git, append(helper, "ls-remote", "https://github.com/other/thing")...)
	if code == 0 {
		t.Errorf("git ls-remote of an owner the bundle does not hold exit 0: %s", out)
	}
	for _, r := range proxy.take() {
		if r.auth != "" {
			t.Errorf("git ls-remote of other/thing sent %s%s with %q, want no credential", r.host, r.path, r.auth)
		}
	}

	// `gh auth login`, `logout`, `refresh` and `switch` write or swap the token the gh directory
	// holds, which the daemon alone writes: each is refused before gh runs, sends nothing, and
	// leaves acme's hosts.yml as the daemon wrote it. They run last, since a gh that ran them
	// would change the directory every row above reads.
	hostsBefore, err := os.ReadFile(filepath.Join(ghconfig.Dir(root, "acme"), ghconfig.HostsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"auth", "login", "--with-token"}, {"auth", "logout"}, {"auth", "switch"}, {"auth", "refresh"},
	} {
		check(home, nil, args, "", "`gh "+strings.Join(args[:2], " ")+"` ")
	}
	if hostsAfter, err := os.ReadFile(filepath.Join(ghconfig.Dir(root, "acme"), ghconfig.HostsFile)); err != nil || !bytes.Equal(hostsAfter, hostsBefore) {
		t.Errorf("acme's hosts.yml changed under the refused auth commands: %q, %v", hostsAfter, err)
	}
}
