// Package fakegithub is GitHub as a test meets it: git's smart HTTP, served by a real git
// http-backend (upload-pack and receive-pack), and the REST and GraphQL calls the daemon, its
// pods and the leak guard make (api.go), over TLS with a certificate for github.com and
// api.github.com that an authority of the server's own signed. A test reaches it as GitHub
// itself: a process the test starts through Route's CONNECT proxy and trust file, an in-process
// client through Client, and a kind cluster's pods through a DNS rule that sends both names to
// the server's listener (internal/kindtest).
//
// What it holds is the test's: the repositories it serves (Serve, NewRepository), the tokens it
// takes (Grant) and the Apps that mint them (AddApp), the pull requests, reviews, collaborator
// permissions and branch activity it answers (AddPullRequest, AddReview, SetPermission,
// AddActivity), and the webhook its push and pull request events go to, signed as GitHub signs
// them (Webhooks, webhook.go). What it refuses is what GitHub refuses: a git request with no credential, or one
// GitHub does not take, answers 401 with GitHub's Basic challenge, but a read of a public
// repository with none; a push with a token that may not write answers 403; a REST read of a
// private repository the caller's token does not reach answers 404, and a REST write such a token
// asks for answers 403.
package fakegithub

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// The names GitHub serves under, both of which the server's certificate carries: git's smart HTTP
// on github.com, and the REST and GraphQL APIs on api.github.com.
const (
	Host    = "github.com"
	APIHost = "api.github.com"
	// API is GitHub's REST root as the daemon names it (githubrest.RepositoryAPI, appauth.New's
	// default base).
	API = "https://" + APIHost
	// GitUser is the user name git presents a token under, as GitHub asks of an installation
	// token; the git server refuses any other.
	GitUser = "x-access-token"
	// GrantedLogin is the account a token of Grant's acts as, which GraphQL's viewer names.
	GrantedLogin = "legion-test-user"
)

// Options is how Start serves.
type Options struct {
	// Listen is the address the server listens on; empty is 127.0.0.1:0. A kind rig's is its
	// network's gateway, which the cluster's pods reach (internal/kindtest).
	Listen string
	// HTTP2 serves HTTP/2 as well as HTTP/1.1.
	HTTP2 bool
}

// PackFailure is how the server fails a pack request (git's protocol v2 `command=fetch` POST):
// with an HTTP status, or one of the two ways a connection breaks mid-transfer.
type PackFailure int

const (
	// CutShort sends half the answer, then closes the connection: HTTP/1.1.
	CutShort PackFailure = -1
	// ResetStream sends half the answer, then resets the stream: HTTP/2.
	ResetStream PackFailure = -2
)

// Server is one fake GitHub, closed when the test that started it ends.
type Server struct {
	git     string
	root    string
	addr    string
	http2   bool
	ca      *x509.Certificate
	caPEM   []byte
	caFile  string
	backend *cgi.Handler
	// home is the home every git the server runs itself reads (fixtureenv.Isolated): empty, and
	// the server's own.
	home string

	mu        sync.Mutex
	ids       int64
	repos     map[string]*repository
	tokens    map[string]*grant
	apps      map[int64]*app
	failures  []PackFailure
	attempts  int
	passwords []string
	requests  []string
	// hook is where the server delivers its events (Webhooks), nil for none, and delivered what
	// came of each delivery made.
	hook      *webhook
	delivered []string
	// hooks is every hook goroutine Webhooks started, hooksContext what cancels their deliveries
	// and stopHooks its cancel, which the test's end calls (stopWebhooks).
	hooks        sync.WaitGroup
	hooksContext context.Context
	stopHooks    context.CancelFunc
	// tokenLifetime is how long an installation token lives (SetTokenLifetime); zero is an hour.
	tokenLifetime time.Duration
}

// Repository is one repository the server serves: its owner and name as GitHub spells them, the
// bare repository git serves it from, and whether GitHub shows it to anyone (public) or only to a
// token that reaches it.
type Repository struct {
	Owner, Name string
	Bare        string
	Public      bool
}

// Slug is the repository's owner/name.
func (r Repository) Slug() string { return r.Owner + "/" + r.Name }

// URL is the repository's clone URL, as workspace.GitHubURL writes one.
func (r Repository) URL() string { return "https://" + Host + "/" + r.Slug() }

// Start serves a fake GitHub over TLS on options.Listen until the test ends.
func Start(t testing.TB, options Options) *Server {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the fake GitHub serves git through a real git http-backend: %v", err)
	}
	listener, err := net.Listen("tcp", cmp.Or(options.Listen, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("the fake GitHub cannot listen on %s: %v", cmp.Or(options.Listen, "127.0.0.1:0"), err)
	}
	s := &Server{
		git: git, root: t.TempDir(), home: t.TempDir(), addr: listener.Addr().String(), http2: options.HTTP2,
		repos: map[string]*repository{}, tokens: map[string]*grant{}, apps: map[int64]*app{},
	}
	s.hooksContext, s.stopHooks = context.WithCancel(context.Background())
	t.Cleanup(s.stopWebhooks)
	// http.receivepack serves a push, which git http-backend otherwise serves only to a request
	// whose REMOTE_USER is set, and Go's CGI host sets none: the server decides who may push
	// before the backend runs. No configuration of the machine's is read.
	s.backend = &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{
		"GIT_PROJECT_ROOT=" + s.root, "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
	}}
	ca, leaf := certificates(t)
	s.ca = ca
	s.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	s.caFile = filepath.Join(t.TempDir(), "fake-github-ca.pem")
	if err := os.WriteFile(s.caFile, s.caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = server.Listener.Close()
	server.Listener = listener
	server.EnableHTTP2 = options.HTTP2
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	// Registered after stopWebhooks, so it runs first: the server stops taking requests, and so
	// queueing events, before the hooks are stopped.
	t.Cleanup(server.Close)
	return s
}

// Addr is the listener's host:port. Its certificate names github.com and api.github.com and
// never this address, so a client reaches it under those names (Client, Route).
func (s *Server) Addr() string { return s.addr }

// CAPEM is the server's authority, PEM: what a trust store adds to take its certificate.
func (s *Server) CAPEM() []byte { return slices.Clone(s.caPEM) }

// CAFile is a file holding CAPEM.
func (s *Server) CAFile() string { return s.caFile }

// Client is an HTTP client of this test process that reaches the server under any name and
// trusts its authority alone, so https://github.com and https://api.github.com are the server.
func (s *Server) Client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(s.ca)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: s.http2,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, s.addr)
		},
	}}
}

// Route has every git this test starts reach the server as GitHub: https_proxy names a CONNECT
// proxy that tunnels any host to it, GIT_SSL_CAINFO trusts its authority, and no_proxy and
// NO_PROXY are emptied so no host goes around the proxy. It sets the test process's own
// environment (t.Setenv), which a child process inherits.
func (s *Server) Route(t testing.TB) {
	t.Helper()
	t.Setenv("GIT_SSL_CAINFO", s.caFile)
	t.Setenv("https_proxy", "http://"+connectProxy(t, s.addr))
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
}

// Serve has the server serve repo, from its bare repository, as GitHub serves repo.Slug().
func (s *Server) Serve(t testing.TB, repo Repository) {
	t.Helper()
	owner := filepath.Join(s.root, repo.Owner)
	if err := os.MkdirAll(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(repo.Bare, filepath.Join(owner, repo.Name+".git")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[key(repo.Owner, repo.Name)] = &repository{
		Repository: repo, id: s.nextID(),
		reviews: map[int][]Review{}, permissions: map[string]string{}, activity: map[string][]Activity{},
	}
}

// NewRepository is a repository of its own, served: a bare repository whose default branch, main,
// holds one commit with a README.md, made with git alone and reading no configuration of the
// machine's.
func (s *Server) NewRepository(t testing.TB, owner, name string, public bool) Repository {
	t.Helper()
	repo := Repository{Owner: owner, Name: name, Bare: filepath.Join(t.TempDir(), name+".git"), Public: public}
	run := func(stdin string, args ...string) string {
		t.Helper()
		out, err := s.gitOutput(repo.Bare, stdin, args...)
		if err != nil {
			t.Fatalf("seed %s: %v", repo.Slug(), err)
		}
		return out
	}
	if _, err := s.gitOutput("", "", "init", "--quiet", "--bare", "--initial-branch=main", repo.Bare); err != nil {
		t.Fatalf("seed %s: %v", repo.Slug(), err)
	}
	blob := run("# "+name+"\n", "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tREADME.md\n", "mktree")
	commit := run("", "commit-tree", tree, "-m", "The repository's first commit")
	run("", "update-ref", "refs/heads/main", commit)
	s.Serve(t, repo)
	return repo
}

// Grant has the server take token, for every repository, as an account of its own
// (GrantedLogin): one that may push and write refs when write is true, and one that only reads
// otherwise.
func (s *Server) Grant(token string, write bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = &grant{write: write, login: GrantedLogin}
}

// FailPacks has the server fail the next pack requests, one failure each, in order, and serve
// every pack request after them.
func (s *Server) FailPacks(failures ...PackFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failures...)
}

// Attempts is how many git transfers presented a credential: one ref advertisement each.
func (s *Server) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// Presented is every password git presented, in order.
func (s *Server) Presented() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.passwords)
}

// Requests is every request the server answered, one line each: its method, host, path and query,
// and the status it got. No credential is in it.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// grant is what a token the server takes may do: write, the account it acts as, and for an
// installation's token the owner whose repositories it reaches and when it expires.
type grant struct {
	write bool
	login string
	// owner is the account of the installation that minted the token; empty for a token of
	// Grant's, which reaches every repository.
	owner        string
	installation int64
	expires      time.Time
}

// reaches says whether the token reaches repo.
func (g *grant) reaches(repo *repository) bool {
	return g.owner == "" || strings.EqualFold(g.owner, repo.Owner)
}

// live says whether GitHub still takes the token.
func (g *grant) live() bool { return g.expires.IsZero() || time.Now().Before(g.expires) }

// repository is a served repository and what the REST API answers about it.
type repository struct {
	Repository
	id          int64
	pulls       []*PullRequest
	reviews     map[int][]Review
	permissions map[string]string
	activity    map[string][]Activity
}

func key(owner, name string) string { return strings.ToLower(owner + "/" + name) }

// nextID is a fresh id for a repository, an installation, a review or an App; s.mu is held.
func (s *Server) nextID() int64 {
	s.ids++
	return 1000 + s.ids
}

// gitCommand is the server's git, against the bare repository bare when it is not empty, under an
// isolated environment of the server's own home (fixtureenv.Isolated), so it reads no
// configuration of the machine's, and with a fixed date, so a seed is the same commit each run.
func (s *Server) gitCommand(bare string, args ...string) *exec.Cmd {
	if bare != "" {
		args = append([]string{"--git-dir=" + bare}, args...)
	}
	command := exec.Command(s.git, args...)
	command.Env = append(fixtureenv.Isolated(s.home), "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	return command
}

// gitOutput runs gitCommand with stdin and returns its standard output, trimmed; an error carries
// what git said.
func (s *Server) gitOutput(bare, stdin string, args ...string) (string, error) {
	command := s.gitCommand(bare, args...)
	command.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(command.Args[1:], " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// statusWriter keeps the status a handler answered with, for the request log, gives every answer
// a type (plainUnlessTyped), and hands the writer's Flush and Hijack through, which a cut-short
// pack needs. Every answer of the server goes out through it (serve).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.plainUnlessTyped()
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.plainUnlessTyped()
	return w.ResponseWriter.Write(b)
}

// plainUnlessTyped gives the answer a Content-Type before its header goes out, where its handler
// named none: text/plain, so no answer is sniffed as HTML, whatever text of the request it
// carries back (a path's owner and name in a refusal, a GraphQL variable in an error). Every
// answer the server writes names its own type before this runs (answer's JSON, the archive's
// gzip, http.Error's plain text, git http-backend's own), so it changes none of them. It is set
// on the wrapped writer, the one the body is written to; once the header is out, setting it
// changes nothing.
func (w *statusWriter) plainUnlessTyped() {
	if header := w.ResponseWriter.Header(); header.Get("Content-Type") == "" {
		header.Set("Content-Type", "text/plain; charset=utf-8")
	}
}

func (w *statusWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// gitPath is a path of git's smart HTTP: /<owner>/<name>[.git]/ and one of its three endpoints.
var gitPath = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	recorded := &statusWriter{ResponseWriter: w}
	defer func() {
		s.mu.Lock()
		s.requests = append(s.requests, fmt.Sprintf("%s %s%s -> %d", r.Method, r.Host, r.URL.RequestURI(), recorded.status))
		s.mu.Unlock()
	}()
	if match := gitPath.FindStringSubmatch(r.URL.Path); match != nil {
		s.serveGit(recorded, r, match[1], match[2], match[3])
		return
	}
	s.serveAPI(recorded, r)
}

// serveGit answers one request of git's smart HTTP for owner/name's endpoint, deciding who may
// read and push before git http-backend serves it.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request, owner, name, endpoint string) {
	push := endpoint == "git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack"
	user, password, presented := r.BasicAuth()
	s.mu.Lock()
	if presented {
		s.passwords = append(s.passwords, password)
		if r.Method == http.MethodGet && endpoint == "info/refs" {
			s.attempts++
		}
	}
	repo := s.repos[key(owner, name)]
	held := s.tokens[password]
	s.mu.Unlock()
	accepted := presented && user == GitUser && held != nil && held.live()
	switch {
	case !presented && repo != nil && repo.Public && !push:
		// GitHub serves a public repository's reads to a request with no credential.
	case !accepted:
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "Invalid username or token. Password authentication is not supported for Git operations.", http.StatusUnauthorized)
		return
	case repo == nil || !held.reaches(repo):
		http.Error(w, "Repository not found.", http.StatusNotFound)
		return
	case push && !held.write:
		http.Error(w, fmt.Sprintf("Permission to %s/%s.git denied to %s.", owner, name, held.login), http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && endpoint == "git-upload-pack" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read the request: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if packRequest(r, body) {
			s.mu.Lock()
			failure := PackFailure(0)
			if len(s.failures) > 0 {
				failure, s.failures = s.failures[0], s.failures[1:]
			}
			s.mu.Unlock()
			if failure != 0 {
				s.failPack(w, r, failure)
				return
			}
		}
	}
	if push && r.Method == http.MethodPost && s.hookSet() {
		// A push the backend takes moves refs, and each ref it moved is one push event.
		before := s.refs(repo.Bare)
		s.backend.ServeHTTP(w, r)
		s.pushed(repo, held.login, before, s.refs(repo.Bare))
		return
	}
	s.backend.ServeHTTP(w, r)
}

// hookSet is whether the server delivers its events anywhere.
func (s *Server) hookSet() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hook != nil
}

// packRequest is whether r, whose body is body, is a protocol v2 pack request.
func packRequest(r *http.Request, body []byte) bool {
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return false
		}
		if body, err = io.ReadAll(reader); err != nil {
			return false
		}
	}
	return bytes.Contains(body, []byte("command=fetch"))
}

// failPack answers r, a pack request, as failure says.
func (s *Server) failPack(w http.ResponseWriter, r *http.Request, failure PackFailure) {
	if failure > 0 {
		http.Error(w, "flaky", int(failure))
		return
	}
	answer := httptest.NewRecorder()
	s.backend.ServeHTTP(answer, r)
	for name, values := range answer.Header() {
		w.Header()[name] = values
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(answer.Code)
	body := answer.Body.Bytes()
	_, _ = w.Write(body[:len(body)/2])
	w.(http.Flusher).Flush()
	if failure == ResetStream {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// certificates is a fresh authority and a certificate for github.com and api.github.com it
// signed, valid from an hour before now for a day.
func certificates(t testing.TB) (*x509.Certificate, tls.Certificate) {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	caKey, leafKey := key(), key()
	valid := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake GitHub test authority"},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: Host}, DNSNames: []string{Host, APIHost},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leaf, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return ca, tls.Certificate{Certificate: [][]byte{leaf}, PrivateKey: leafKey}
}

// connectProxy answers each CONNECT, whatever host it names, with a tunnel to target, until the
// test ends, and returns its address.
func connectProxy(t testing.TB, target string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				go func() { _, _ = io.Copy(upstream, reader) }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	return listener.Addr().String()
}

// atoi is s as a positive int, or 0.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
