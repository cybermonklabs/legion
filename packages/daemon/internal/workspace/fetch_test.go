package workspace

import (
	"archive/tar"
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
	"errors"
	"io"
	"maps"
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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testport"
)

// gitFailure is a failed transfer from GitHub as git reports it, with how Fetch reads it:
// transient, whether Fetch runs the transfer again (transientCloneFailure), and outside, whether
// the cause is outside the issue's volume (outsideVolumeFailure).
type gitFailure struct {
	name               string
	result             Result
	transient, outside bool
}

// gitFailures are the failures two tests read. The texts are git's and curl's own. The first three
// are as the production fetch printed them. The statuses and the cut transfer are as git printed
// them against a local git http-backend over TLS. The rest are curl's error strings, the TLS ones
// both GnuTLS's (whose libcurl Debian's git links) and OpenSSL's. Curl 23 has the two texts
// libcurl holds for it. The bad object is what git 2.47.3's fetch printed from a remote that lacks
// an object a ref of the feed names.
var gitFailures = func() []gitFailure {
	const access = "fatal: unable to access 'https://github.com/acme/widgets/': "
	return []gitFailure{
		{"an HTTP/2 stream cancelled mid-pack", fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n"), true, true},
		{"a 408 on the pack request", fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n"), true, true},
		{"a connect that timed out", fatal(access + "Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n"), true, true},
		{"a transfer cut short", fatal("error: RPC failed; curl 18 transfer closed with outstanding read data remaining\nerror: 32 bytes of body are still expected\nfatal: expected flush after ref listing\n"), true, true},
		{"a 502 on a request of the transfer", fatal("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: expected flush after ref listing\n"), true, true},
		{"a TLS read that failed mid-transfer", fatal("error: RPC failed; curl 56 GnuTLS recv error (-9): Error decoding the received TLS packet.\nfatal: early EOF\n"), true, true},
		{"a 408 on the first request", fatal("remote: nope\n" + access + "The requested URL returned error: 408\n"), true, true},
		{"a 429 on the first request", fatal(access + "The requested URL returned error: 429\n"), true, true},
		{"a 500 on the first request", fatal(access + "The requested URL returned error: 500\n"), true, true},
		{"a 503 on the first request", fatal(access + "The requested URL returned error: 503\n"), true, true},
		{"a host that did not resolve", fatal(access + "Could not resolve host: github.com\n"), true, true},
		{"a connection refused", fatal(access + "Failed to connect to github.com port 443 after 3 ms: Couldn't connect to server\n"), true, true},
		{"a connection reset", fatal(access + "Recv failure: Connection reset by peer\n"), true, true},
		{"an empty reply", fatal(access + "Empty reply from server\n"), true, true},
		{"a request that timed out", fatal(access + "Operation timed out after 300000 milliseconds with 0 out of 0 bytes received\n"), true, true},
		{"a TLS handshake closed under GnuTLS", fatal(access + "gnutls_handshake() failed: The TLS connection was non-properly terminated.\n"), true, true},
		{"a TLS handshake closed under OpenSSL", fatal(access + "OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443\n"), true, true},
		{"an HTTP/2 stream on the first request", fatal(access + "HTTP/2 stream 1 was not closed cleanly: INTERNAL_ERROR (err 2)\n"), true, true},
		{"a pack stream that ended early", fatal("fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n"), true, true},

		{"the token refused", fatal("remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/acme/widgets/'\n"), false, true},
		{"no credential reached git", fatal("fatal: could not read Username for 'https://github.com': terminal prompts disabled\n"), false, false},
		{"the repository not found", fatal("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"), false, true},
		{"a 403 on the first request", fatal("remote: Write access to repository not granted.\n" + access + "The requested URL returned error: 403\n"), false, true},
		{"a 403 on a request of the transfer", fatal("error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403\nfatal: expected flush after ref listing\n"), false, true},
		{"a certificate GnuTLS did not verify", fatal(access + "server certificate verification failed. CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none\n"), false, false},
		{"a certificate OpenSSL did not verify", fatal(access + "SSL certificate problem: unable to get local issuer certificate\n"), false, false},
		{"a certificate refused on a request of the transfer", fatal("error: RPC failed; curl 60 SSL certificate problem: unable to get local issuer certificate\nfatal: expected flush after ref listing\n"), false, false},
		{"a write that failed on a request of the transfer", fatal("error: RPC failed; curl 23 Failure writing output to destination\nfatal: early EOF\n"), false, false},
		{"a write of received data that failed", fatal("error: RPC failed; curl 23 Failed writing received data to disk/application\n"), false, false},
		{"a curl error nobody has seen on a request of the transfer", fatal("error: RPC failed; curl 999 something nobody has seen\nfatal: early EOF\n"), false, false},
		{"a destination already there", fatal("fatal: destination path '/legion/feed/acme/widgets.git.clone-1234' already exists and is not an empty directory.\n"), false, false},
		{"a full disk", fatal("fatal: write error: No space left on device\nfatal: fetch-pack: invalid index-pack output\n"), false, false},
		{"a bad object in the feed", Result{ExitCode: 1, Stderr: "fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111\nerror: file:///tmp/bad-object/remote.git did not send all necessary objects\n"}, false, false},
		{"a transient text under another exit", Result{ExitCode: 1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false, false},
		{"a clone killed by a signal", Result{ExitCode: -1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false, false},
		{"a clone that timed out", Result{ExitCode: -1, TimedOut: true}, false, true},
	}
}()

// Which clone failures Fetch retries: git's fatal exit with a cause naming a connection that failed
// or broke, or a status a server may not answer again. A curl error is told the same way whichever
// request met it.
func TestATransientCloneFailureIsToldFromAPermanentOne(t *testing.T) {
	for _, tc := range gitFailures {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientCloneFailure(tc.result); got != tc.transient {
				t.Fatalf("transientCloneFailure(%q, exit %d) = %t, want %t", tc.result.Stderr, tc.result.ExitCode, got, tc.transient)
			}
		})
	}
}

// Which failures from GitHub have a cause outside the issue's volume, so that a new volume does
// not cure them: an attempt that ran out of its time, or git's fatal exit with a cause line that
// names any HTTP status or a connection that failed or broke, a pack stream cut with no cause
// line, a credential GitHub refused, or a repository GitHub did not find. Every other cause line
// (curl 23, a write to the volume that failed, and curl 60), a credential git could not read, and
// a bad object in the feed are not. The two tests read one set of git's lines (gitFailures).
func TestAFailureOutsideTheVolumeIsReadFromGitsOutput(t *testing.T) {
	for _, tc := range gitFailures {
		t.Run(tc.name, func(t *testing.T) {
			if got := outsideVolumeFailure(tc.result); got != tc.outside {
				t.Fatalf("outsideVolumeFailure(%q, exit %d, timed out %t) = %t, want %t", tc.result.Stderr, tc.result.ExitCode, tc.result.TimedOut, got, tc.outside)
			}
		})
	}
}

// fatal is a clone git ended with its fatal status, 128, printing stderr.
func fatal(stderr string) Result { return Result{ExitCode: 128, Stderr: stderr} }

// testClock is the fetch's clock in a test: each wait passes at once and moves it on by its length
// and overshoot, as a timer firing late does, and as advance does for the time an attempt takes.
type testClock struct {
	now       time.Time
	overshoot time.Duration
	waits     []time.Duration
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.advance(d + c.overshoot)
	c.waits = append(c.waits, d)
	return ctx.Err()
}

func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// failingClone is a runner whose every clone takes took of clock's time and then fails with
// result — or, given less time than that, runs to its timeout and is killed there.
type failingClone struct {
	clock  *testClock
	took   time.Duration
	result Result

	attempts []cloneAttempt
}

// cloneAttempt is one clone failingClone ran: when it started, by the clock, and its timeout.
type cloneAttempt struct {
	at      time.Time
	timeout time.Duration
}

func (r *failingClone) Timeout() time.Duration { return testTimeout }

func (r *failingClone) Run(_ context.Context, command Command) (Result, error) {
	r.attempts = append(r.attempts, cloneAttempt{at: r.clock.Now(), timeout: command.Timeout})
	if command.Timeout <= r.took {
		r.clock.advance(command.Timeout)
		return Result{ExitCode: -1, TimedOut: true}, nil
	}
	r.clock.advance(r.took)
	return r.result, nil
}

// clonesUnderTheBound fails the test unless each of run's clones was given what remained of
// FetchTimeout from the first one's start, and returns how long the fetch ran by clock.
func clonesUnderTheBound(t *testing.T, run *failingClone, clock *testClock) time.Duration {
	t.Helper()
	start := run.attempts[0].at
	for i, attempt := range run.attempts {
		if want := FetchTimeout - attempt.at.Sub(start); attempt.timeout != want {
			t.Errorf("clone attempt %d started %s in with timeout %s, want what remained of the bound, %s", i+1, attempt.at.Sub(start), attempt.timeout, want)
		}
	}
	elapsed := clock.Now().Sub(start)
	if elapsed > FetchTimeout {
		t.Errorf("the fetch ran %s, past its %s bound", elapsed, FetchTimeout)
	}
	return elapsed
}

// A clone that keeps failing quickly, as one does through a GitHub outage, is retried once after
// each backoff wait and no more: the fifth transient failure ends the fetch, 1 min 50 s in, naming
// the failure and the retry limit, rather than holding the launch for the whole FetchTimeout.
func TestFetchStopsAfterItsLastRetry(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("fatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 503\n")}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 5 failed, and the fetch retries a clone at most 4 times"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "returned error: 503") {
		t.Fatalf("Fetch = %v, want the 503 and %q", err, want)
	}
	if len(run.attempts) != 5 || len(logged) != 4 {
		t.Fatalf("Fetch cloned %d times and logged %d retries, want 5 and 4", len(run.attempts), len(logged))
	}
	if !slices.Equal(clock.waits, fetchBackoff) {
		t.Fatalf("Fetch waited %v, want %v", clock.waits, fetchBackoff)
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != time.Minute+50*time.Second {
		t.Errorf("the fetch gave up %s in, want 1m50s", elapsed)
	}
}

// A clone whose every attempt runs ten minutes before it breaks, as a slow transfer GitHub cuts
// does, is given what remains of FetchTimeout each time, and the third, started 20 min 20 s in
// with less than ten minutes left, is killed at the bound itself: Fetch never runs past
// FetchTimeout, which the daemon's wait for a launching pod's role launcher allows the pod while
// it provisions (sandbox.Runtime.ProvisionBound).
func TestFetchKillsTheAttemptThatReachesItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, took: 10 * time.Minute, result: fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\nfatal: early EOF\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	if want := "command timed out: " + strings.Join(slices.Concat(feedGit, []string{"-c", "gc.auto=0"}), " "); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Fetch = %v, want the last attempt's timeout", err)
	}
	if len(run.attempts) != 3 {
		t.Fatalf("Fetch cloned %d times, want 3", len(run.attempts))
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != FetchTimeout {
		t.Errorf("the fetch gave up %s in, want at its bound, %s", elapsed, FetchTimeout)
	}
}

// A retry whose wait would end past FetchTimeout is not made: Fetch fails naming the failure and
// the bound.
func TestFetchMakesNoRetryThatWouldStartPastItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, took: FetchTimeout - 3*time.Second,
		result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 1 failed, and a retry in 5s would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 {
		t.Fatalf("Fetch cloned %d times and waited %v, want one clone and no wait", len(run.attempts), clock.waits)
	}
}

// A wait that ends past the bound — its timer fired late — starts no further attempt, which would
// have no time at all: Fetch fails naming the last attempt's failure and the bound.
func TestFetchStartsNoAttemptOnceAWaitEndsPastItsBound(t *testing.T) {
	clock := newTestClock()
	clock.overshoot = 2 * time.Millisecond
	run := &failingClone{clock: clock, took: FetchTimeout - 5*time.Second - time.Millisecond,
		result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 1 failed, and its retry after the 5s wait would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch cloned %d times, want 1", len(run.attempts))
	}
}

// A permanent failure is Fetch's answer at once: one clone, no wait, no retry logged.
func TestFetchFailsAPermanentCloneFailureAtOnce(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n")}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	_, err := Fetch(context.Background(), run, req)
	if err == nil || !strings.Contains(err.Error(), "fatal: repository 'https://github.com/acme/widgets/' not found") {
		t.Fatalf("Fetch = %v, want git's not found", err)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 || len(logged) != 0 {
		t.Fatalf("Fetch cloned %d times, waited %v and logged %q; want one clone, no wait, nothing logged", len(run.attempts), clock.waits, logged)
	}
}

// A fetch whose context ends during a backoff wait — the init container stopped — ends with it,
// running no further clone.
func TestFetchEndsWithItsContextDuringAWait(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n")}
	req := fetchRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	req.Clock = clock
	req.Log = func(string) { cancel() }

	_, err := Fetch(ctx, run, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch = %v, want it ended with its context", err)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch cloned %d times, want 1", len(run.attempts))
	}
}

// A fetch request names the log its retries are told to and the clock they wait on; one without
// either is refused before Fetch runs anything or touches the feed.
func TestFetchRefusesARequestWithNoLogOrClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*FetchRequest)
		want string
	}{
		{"no log", func(r *FetchRequest) { r.Log = nil }, "workspace fetch log is required"},
		{"no clock", func(r *FetchRequest) { r.Clock = nil }, "workspace fetch clock is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			tc.edit(&req)
			if _, err := Fetch(context.Background(), run, req); err == nil || err.Error() != tc.want {
				t.Fatalf("Fetch = %v, want %q", err, tc.want)
			}
			if calls := run.Calls(); len(calls) != 0 {
				t.Errorf("Fetch ran %q before refusing", calls)
			}
			if _, err := os.Stat(req.Feed); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Fetch touched the feed before refusing: %v", err)
			}
			if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
				t.Errorf("Fetch left %v (%v) in its credential directory", entries, err)
			}
		})
	}
}

// packFailure is how the GitHub stand-in fails a pack request: with an HTTP status, or one of the
// two ways a connection breaks mid-transfer.
type packFailure int

const (
	// cutShort sends half the answer, then closes the connection: HTTP/1.1.
	cutShort packFailure = -1
	// resetStream sends half the answer, then resets the stream: HTTP/2.
	resetStream packFailure = -2
)

// gitHubStandIn is a real git http-backend that git reaches as https://github.com/acme/widgets:
// through a CONNECT proxy https_proxy names, with a certificate for github.com that
// GIT_SSL_CAINFO trusts. So Fetch's clone runs as it does in a pod, its one-shot credential
// included, which the stand-in requires over Basic auth as GitHub does, refusing any password but
// token. Each pack request (the protocol v2 `command=fetch` POST) fails the next of failures'
// ways, then the stand-in serves them.
type gitHubStandIn struct {
	remote string

	mu        sync.Mutex
	tokens    []string
	failures  []packFailure
	attempts  int
	passwords []string
}

func newGitHubStandIn(t *testing.T, http2 bool, token string, failures ...packFailure) *gitHubStandIn {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the stand-in runs a real git http-backend: %v", err)
	}
	root := t.TempDir()
	s := &gitHubStandIn{remote: localBareRemote(t), tokens: []string{token}, failures: failures}
	if err := os.Mkdir(filepath.Join(root, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.remote, filepath.Join(root, "acme", "widgets.git")); err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		s.mu.Lock()
		if ok {
			s.passwords = append(s.passwords, password)
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
				s.attempts++
			}
		}
		accepted := slices.Contains(s.tokens, password)
		s.mu.Unlock()
		if !ok || user != "x-access-token" || !accepted {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "Invalid username or token.", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the request: %v", err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if packRequest(t, r, body) {
				s.mu.Lock()
				failure := packFailure(0)
				if len(s.failures) > 0 {
					failure, s.failures = s.failures[0], s.failures[1:]
				}
				s.mu.Unlock()
				if failure != 0 {
					failPack(t, w, r, backend, failure)
					return
				}
			}
		}
		backend.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = http2
	ca, leaf := gitHubCertificate(t)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	t.Setenv("https_proxy", "http://"+connectProxy(t, server.Listener.Addr().String()))
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	return s
}

// packRequest is whether r, whose body is body, is a protocol v2 pack request.
func packRequest(t *testing.T, r *http.Request, body []byte) bool {
	t.Helper()
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Errorf("read the gzipped request: %v", err)
			return false
		}
		if body, err = io.ReadAll(reader); err != nil {
			t.Errorf("read the gzipped request: %v", err)
			return false
		}
	}
	return bytes.Contains(body, []byte("command=fetch"))
}

// failPack answers r, a pack request, as failure says.
func failPack(t *testing.T, w http.ResponseWriter, r *http.Request, backend http.Handler, failure packFailure) {
	if failure > 0 {
		http.Error(w, "flaky", int(failure))
		return
	}
	answer := httptest.NewRecorder()
	backend.ServeHTTP(answer, r)
	for name, values := range answer.Header() {
		w.Header()[name] = values
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(answer.Code)
	body := answer.Body.Bytes()
	_, _ = w.Write(body[:len(body)/2])
	w.(http.Flusher).Flush()
	if failure == resetStream {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("cut the answer short: %v", err)
		return
	}
	_ = conn.Close()
}

// gitHubCertificate is a CA's certificate, DER, and a certificate for github.com it signed.
func gitHubCertificate(t *testing.T) ([]byte, tls.Certificate) {
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
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stand-in CA"},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	ca, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com"},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leaf, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return ca, tls.Certificate{Certificate: [][]byte{leaf}, PrivateKey: leafKey}
}

// connectProxy answers each CONNECT, whatever host it names, with a tunnel to target, and returns
// its address.
func connectProxy(t *testing.T, target string) string {
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

// attemptCount is how many clone attempts reached the stand-in: one authenticated ref
// advertisement each.
func (s *gitHubStandIn) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// accept has the stand-in take token as well as the one it was made with.
func (s *gitHubStandIn) accept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, token)
}

// presented is every password git presented, in order.
func (s *gitHubStandIn) presented() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.passwords)
}

// tokensPresented fails the test unless git presented the one-shot credential's token on every
// authenticated request, and on at least one per attempt.
func (s *gitHubStandIn) tokensPresented(t *testing.T, token string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == 0 || len(s.passwords) < s.attempts || slices.ContainsFunc(s.passwords, func(password string) bool { return password != token }) {
		t.Fatalf("git presented %q over %d attempts, want the provisioning token on every request", s.passwords, s.attempts)
	}
}

// Against a real git and curl reaching the stand-in as github.com, the failures the production
// fetch met on the pack request — a 408, a transfer cut short, an HTTP/2 stream reset mid-pack —
// are each told transient from what git prints, and the attempt after them fills the feed. Every
// attempt presents the provisioning token.
func TestFetchRetriesWhatARealGitPrintsForATransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		http2    bool
		failures []packFailure
		logged   [][]string
	}{
		{
			name: "a 408, then a transfer cut short", failures: []packFailure{http.StatusRequestTimeout, cutShort},
			logged: [][]string{
				{"error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408", "expected 'packfile'"},
				{"error: RPC failed; curl 18 transfer closed with outstanding read data remaining"},
			},
		},
		{
			name: "an HTTP/2 stream reset mid-pack", http2: true, failures: []packFailure{resetStream},
			logged: [][]string{{"error: RPC failed; curl 92 HTTP/2 stream", "was not closed cleanly"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fetchRequest(t)
			github := newGitHubStandIn(t, tc.http2, testInstallationToken, tc.failures...)
			clock := newTestClock()
			req.Clock = clock
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			report, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			feed := report.Feed
			if len(logged) != len(tc.logged) {
				t.Fatalf("Fetch logged %q, want %d retries", logged, len(tc.logged))
			}
			for i, says := range tc.logged {
				t.Logf("retry %d logged: %s", i+1, logged[i])
				for _, text := range says {
					if !strings.Contains(logged[i], text) {
						t.Fatalf("retry %d logged %q, want it to name %q", i+1, logged[i], text)
					}
				}
			}
			if want := fetchBackoff[:len(tc.logged)]; !slices.Equal(clock.waits, want) {
				t.Fatalf("Fetch waited %v, want %v", clock.waits, want)
			}
			if attempts := github.attemptCount(); attempts != len(tc.logged)+1 {
				t.Fatalf("git made %d attempts, want %d", attempts, len(tc.logged)+1)
			}
			github.tokensPresented(t, testInstallationToken)
			if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+github.remote, "rev-parse", "main")) {
				t.Errorf("the feed's main is %s, not the remote's", main)
			}
		})
	}
}

// Against the same real git, a 403 on the pack request and a token GitHub refuses each fail the
// fetch on its first attempt: no wait, no retry logged.
func TestFetchFailsWhatARealGitPrintsForAPermanentFailureAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name, accepts string
		failures      []packFailure
		says          string
	}{
		{name: "a 403 on the pack request", failures: []packFailure{http.StatusForbidden, http.StatusForbidden}, says: "error: RPC failed; HTTP 403 curl 22"},
		{name: "the token refused", accepts: "ghs_another", says: "fatal: Authentication failed for 'https://github.com/acme/widgets/'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fetchRequest(t)
			accepts := testInstallationToken
			if tc.accepts != "" {
				accepts = tc.accepts
			}
			github := newGitHubStandIn(t, false, accepts, tc.failures...)
			clock := newTestClock()
			req.Clock = clock
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			_, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want git's %q", err, tc.says)
			}
			t.Logf("Fetch failed: %v", err)
			if attempts := github.attemptCount(); attempts != 1 || len(clock.waits) != 0 || len(logged) != 0 {
				t.Fatalf("git made %d attempts, Fetch waited %v and logged %q; want one attempt, no wait, nothing logged", attempts, clock.waits, logged)
			}
			github.tokensPresented(t, testInstallationToken)
		})
	}
}

// The provisioning token is read as its file stands before every transfer from GitHub, never kept
// from the fetch's start: the daemon keeps every live issue pod's provisioning Secret fresh,
// whatever that pod's fetch has already done, and the kubelet rewrites the file projected from it
// in place. Against a real git reaching the stand-in as github.com: a token file rewritten while
// the fetch waits on the repository cache is the one the delta presents, the stand-in taking that
// token alone; and one rewritten during a retry's backoff is the one the retry presents. A read
// that fails, the first one or a renewal, fails the fetch for a cause outside the issue's volume.
func TestFetchReadsTheProvisioningTokenAsItsFileStandsBeforeEveryTransfer(t *testing.T) {
	t.Run("rewritten during the wait on the repository cache", func(t *testing.T) {
		github := newGitHubStandIn(t, false, "ghs_fresh")
		served := &fakeCache{body: cacheTar(t, cacheOf(t, github.remote)), notReady: 2}
		pushRemoteBranch(t, github.remote, "release/v7.1", "newer.txt")
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
		req.Clock = &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		report, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if report.Cloned || report.CacheBytes == 0 || served.healthAsked != 3 {
			t.Fatalf("Fetch reported %+v after %d health polls, want the cache streamed after the wait", report, served.healthAsked)
		}
		github.tokensPresented(t, "ghs_fresh")
	})
	t.Run("rewritten during a retry's backoff", func(t *testing.T) {
		github := newGitHubStandIn(t, false, "ghs_stale", http.StatusRequestTimeout)
		github.accept("ghs_fresh")
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.Clock = &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if _, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		presented := github.presented()
		fresh := slices.Index(presented, "ghs_fresh")
		if github.attemptCount() != 2 || fresh <= 0 || slices.Contains(presented[:fresh], "ghs_fresh") || slices.Contains(presented[fresh:], "ghs_stale") {
			t.Fatalf("git presented %q over %d attempts, want the first attempt's token, then the rewritten one alone", presented, github.attemptCount())
		}
	})
	// A read of the token that fails is a cause outside the issue's volume (ErrOutsideVolume):
	// Fetch's own first read, and a renewal before a retry, of a file removed during the backoff.
	t.Run("unreadable when the fetch starts", func(t *testing.T) {
		run := &failingClone{clock: newTestClock()}
		req := fetchRequest(t)
		req.Token = tokenFrom(filepath.Join(t.TempDir(), "LEGION_PROVISION_TOKEN"))
		_, err := Fetch(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "no such file or directory") || !errors.Is(err, ErrOutsideVolume) {
			t.Fatalf("Fetch = %v, want the failed read, outside the volume", err)
		}
		if len(run.attempts) != 0 {
			t.Fatalf("Fetch ran %d transfers, want none", len(run.attempts))
		}
	})
	t.Run("removed during a retry's backoff", func(t *testing.T) {
		clock := newTestClock()
		run := &failingClone{clock: clock, result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.Clock = &removingClock{testClock: clock, file: file}

		_, err := Fetch(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "clone attempt 2: read the provisioning token: ") || !errors.Is(err, ErrOutsideVolume) {
			t.Fatalf("Fetch = %v, want the retry's failed read, outside the volume", err)
		}
		if len(run.attempts) != 1 {
			t.Fatalf("Fetch ran %d transfers, want the first alone", len(run.attempts))
		}
	})
}

// tokenFile is a provisioning token file holding token, as a pod mounts one.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "LEGION_PROVISION_TOKEN")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// tokenFrom is a FetchRequest.Token that reads file each time it is called.
func tokenFrom(file string) func() (string, error) {
	return func() (string, error) {
		contents, err := os.ReadFile(file)
		return strings.TrimSpace(string(contents)), err
	}
}

// rewritingClock is the test clock that, at its first wait, rewrites file to hold token: what the
// daemon's refresh and the kubelet do to a pod's token file while the fetch waits.
type rewritingClock struct {
	*testClock
	file, token string
	rewritten   bool
}

func (c *rewritingClock) Sleep(ctx context.Context, d time.Duration) error {
	if !c.rewritten {
		c.rewritten = true
		if err := os.WriteFile(c.file, []byte(c.token), 0o600); err != nil {
			return err
		}
	}
	return c.testClock.Sleep(ctx, d)
}

// removingClock is the test clock that removes file at its first wait: a token file that is gone
// while the fetch waits to retry.
type removingClock struct {
	*testClock
	file    string
	removed bool
}

func (c *removingClock) Sleep(ctx context.Context, d time.Duration) error {
	if !c.removed {
		c.removed = true
		if err := os.Remove(c.file); err != nil {
			return err
		}
	}
	return c.testClock.Sleep(ctx, d)
}

// A fetch killed mid-update leaves git's lock files in the feed, which outlives it on the pod's
// disk for the kubelet's retry (or on the cache's volume), and a transfer killed mid-pack leaves the
// pack it was receiving under a temporary name: the next Fetch, the feed's only writer, removes
// each and logs it with its age before it fetches, so the feed is never blocked for good and
// carries no pack that never arrived, and every object it already held is kept.
func TestFetchRemovesTheLocksAnInterruptedFetchLeftInTheFeed(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := first.Feed
	objects := strings.Fields(runSetup(t, feed, "git", "--git-dir="+feed, "rev-list", "--objects", "--no-object-names", "--all"))
	locks := []string{"refs/heads/main.lock", "packed-refs.lock", "HEAD.lock", "shallow.lock"}
	for _, lock := range locks {
		if err := os.WriteFile(filepath.Join(feed, lock), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	partial := filepath.Join(feed, "objects", "pack", "tmp_pack_Ab12Cd")
	if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := pushRemoteBranch(t, run.remote, "main", "after-the-interruption.txt")
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after an interrupted one: %v", err)
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); got != moved {
		t.Errorf("the feed's main is %s, want the remote's %s", got, moved)
	}
	for _, lock := range locks {
		if _, err := os.Stat(filepath.Join(feed, lock)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still in the feed (%v)", lock, err)
		}
		if !slices.ContainsFunc(logged, func(line string) bool {
			return strings.HasPrefix(line, "removed the stale git lock "+filepath.Join(feed, lock)+", last written ")
		}) {
			t.Errorf("the removal of %s was not logged: %q", lock, logged)
		}
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partial pack is still in the feed (%v)", err)
	}
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.HasPrefix(line, "removed the partial pack "+partial+", last written ")
	}) {
		t.Errorf("the partial pack's removal was not logged: %q", logged)
	}
	for _, object := range objects {
		// The probe reads the feed the fetch left, under the package's fixture environment, so
		// the home it reads is the one the code under test and every other fixture read.
		held := exec.Command("git", "--git-dir="+feed, "cat-file", "-e", object)
		held.Dir = feed
		held.Env = setupEnvironment()
		if err := held.Run(); err != nil {
			t.Errorf("the feed lost %s, an object the clone may borrow", object)
		}
	}
}

// A feed that already holds the repository — the repository cache's clone at each refresh, a feed
// the kubelet's retry of the fetch container finds on the pod's disk — is fetched into, not cloned
// again: every branch the remote has
// moved, gained or lost reaches it, through the one-shot credential, bounded as the clone is, and
// the credential is gone once Fetch returns.
func TestFetchIntoAFeedThatHoldsTheRepositoryFetchesEveryBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	// The clock stands still, so the fetch is given the whole bound it shares with the reads before.
	req.Clock = newTestClock()
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "doomed", "main")
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := first.Feed
	advanceRemote(t, run.remote)
	gained := pushRemoteBranch(t, run.remote, "release/v7.1", "gained.txt")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-D", "doomed")
	before := len(run.Calls())

	again, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if again.Feed != feed {
		t.Fatalf("the second Fetch filled %s, want the first's %s", again.Feed, feed)
	}
	calls := run.Calls()[before:]
	for _, call := range calls {
		if commandWith(call.Argv, "git", "clone") {
			t.Fatalf("the second Fetch cloned again: %q", call.Argv)
		}
	}
	var fetch Command
	for _, call := range calls {
		if commandWith(call.Argv, "git", "--git-dir="+feed, "fetch") {
			fetch = call
			break
		}
	}
	if want := slices.Concat(feedGit, []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "--git-dir=" + feed, "fetch", "--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/*:refs/heads/*"}); !slices.Equal(fetch.Argv, want) {
		t.Fatalf("Fetch ran %q, want %q", fetch.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, fetch, FetchTimeout)
	for _, branch := range []string{"main", "release/v7.1"} {
		if got, want := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/"+branch)), strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", branch)); got != want {
			t.Errorf("the feed's %s is %s, want the remote's %s", branch, got, want)
		}
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/release/v7.1")); got != gained {
		t.Errorf("the feed's release/v7.1 is %s, want the pushed %s", got, gained)
	}
	if refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)", "refs/heads/doomed"); refs != "" {
		t.Errorf("the branch the remote deleted is still in the feed: %q", refs)
	}
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A fetch into a feed that holds the repository points its HEAD at the branch GitHub's HEAD names
// now, read under the same credential and bound as the fetch: GitHub renaming the default branch
// would otherwise leave HEAD naming the branch the fetch's prune removed, and an issue's clone of
// the feed would check nothing out. The move is logged; a fetch with nothing renamed moves nothing.
func TestFetchPointsTheFeedsHeadAtGitHubsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "default branch") }) {
		t.Errorf("a fetch with nothing renamed logged a move: %q", logged)
	}
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	before := len(run.Calls())

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after the rename: %v", err)
	}
	if head := strings.TrimSpace(runSetup(t, first.Feed, "git", "--git-dir="+first.Feed, "symbolic-ref", "HEAD")); head != "refs/heads/trunk" {
		t.Errorf("the feed's HEAD names %s, want the renamed refs/heads/trunk", head)
	}
	read := findCall(t, run.Calls()[before:], "git", "--git-dir="+first.Feed, "ls-remote")
	if want := slices.Concat(feedGit, []string{"--git-dir=" + first.Feed, "ls-remote", "--symref", "https://github.com/acme/widgets", "HEAD"}); !slices.Equal(read.Argv, want) {
		t.Errorf("Fetch read GitHub's HEAD with %q, want %q", read.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, read, read.Timeout)
	if want := "the default branch of acme/widgets is now trunk (HEAD named refs/heads/main)"; !slices.Contains(logged, want) {
		t.Errorf("the move was not logged as %q: %q", want, logged)
	}
}

// GitHub renaming its default branch between a fetch and that fetch's read of GitHub's HEAD names a
// branch the fetch did not bring. HEAD stays where it was, and the fetch says so, rather than naming
// nothing: a cache whose HEAD named nothing could publish no head, and a restart while GitHub is
// unreachable would end Serve. The next fetch brings the branch and moves HEAD to it.
func TestFetchLeavesHeadUntilAFetchBringsGitHubsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	head := func() string {
		return strings.TrimSpace(runSetup(t, first.Feed, "git", "--git-dir="+first.Feed, "symbolic-ref", "HEAD"))
	}
	run.beforeLsRemote = func() {
		run.beforeLsRemote = nil
		runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	}

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch GitHub renamed its branch under: %v", err)
	}
	if got := head(); got != "refs/heads/main" {
		t.Errorf("after a fetch that did not bring trunk, the feed's HEAD names %s, want refs/heads/main still", got)
	}
	if want := "GitHub's default branch of acme/widgets is now trunk, which this fetch did not bring: HEAD stays at refs/heads/main until a fetch brings it"; !slices.Contains(logged, want) {
		t.Errorf("the fetch did not say %q: %q", want, logged)
	}

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if got := head(); got != "refs/heads/trunk" {
		t.Errorf("after the fetch that brought trunk, the feed's HEAD names %s, want refs/heads/trunk", got)
	}
}

// A feed repository Fetch did not write is not taken as its own, and it is not replaced either.
// Before this release an issue's agents could write the issue volume's feed directory, so a volume
// may hold a repository there carrying a hook, configuration or link that the token-holding fetch
// would run or follow; and a pod's clone reads the feed's objects through its alternates, so a feed
// replaced under it would take the clone's history with it. Fetch takes an existing feed only in
// the shape it writes, and otherwise fails, naming why, runs no git, and leaves the feed where it
// stands: no planted hook runs, and nothing is removed or cloned in its place — not even the stale
// lock and the partial pack a killed fetch leaves, which Fetch's sweep removes only from a feed it
// takes as its own (RemoveStaleFeedLocks). Before, that sweep ran first and removed both from a
// feed Fetch then refused.
func TestFetchTakesNoFeedItDidNotWrite(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, feed, marker string){
		"a hook": func(t *testing.T, feed, marker string) {
			writeExecutable(t, filepath.Join(feed, "hooks", "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
		},
		"core.hooksPath": func(t *testing.T, feed, marker string) {
			hooks := t.TempDir()
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			runSetup(t, feed, "git", "--git-dir="+feed, "config", "core.hooksPath", hooks)
		},
		"an http.proxy": func(t *testing.T, feed, _ string) {
			runSetup(t, feed, "git", "--git-dir="+feed, "config", "http.proxy", "http://127.0.0.1:1")
		},
		"alternates": func(t *testing.T, feed, _ string) {
			if err := os.WriteFile(filepath.Join(feed, "objects", "info", "alternates"), []byte(t.TempDir()+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"info/grafts": func(t *testing.T, feed, _ string) {
			if err := os.WriteFile(filepath.Join(feed, "info", "grafts"), []byte("0000000000000000000000000000000000000000\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked refs directory": func(t *testing.T, feed, _ string) {
			elsewhere := filepath.Join(t.TempDir(), "refs")
			if err := os.Rename(filepath.Join(feed, "refs"), elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(feed, "refs")); err != nil {
				t.Fatal(err)
			}
		},
		"a gitfile": func(t *testing.T, feed, _ string) {
			elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
			if err := os.Rename(feed, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(feed, []byte("gitdir: "+elsewhere+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked owner directory": func(t *testing.T, feed, _ string) {
			owner := filepath.Dir(feed)
			elsewhere := filepath.Join(t.TempDir(), "acme")
			if err := os.Rename(owner, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, owner); err != nil {
				t.Fatal(err)
			}
		},
		// git reads a key written on its section header's line, `[section]key=value`, and ends an
		// unquoted value at `;`, so each of these sets what it names. `git config` never writes
		// this form; an agent writing the file by hand could.
		"a hooks path on a section header's line": func(t *testing.T, feed, marker string) {
			hooks := filepath.Join(filepath.Dir(feed), "h")
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			appendConfig(t, feed, "[core]hooksPath="+hooks+";]\n")
		},
		"an include on a section header's line": func(t *testing.T, feed, _ string) {
			included := filepath.Join(filepath.Dir(feed), "c")
			if err := os.WriteFile(included, []byte("[http]\n\tproxy = http://127.0.0.1:1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			appendConfig(t, feed, "[include]path="+included+";]\n")
		},
		"an http.proxy on a section header's line": func(t *testing.T, feed, _ string) {
			appendConfig(t, feed, "[http]proxy=http://127.0.0.1:1;]\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			first, err := Fetch(context.Background(), run, req)
			if err != nil {
				t.Fatalf("the first Fetch: %v", err)
			}
			marker := filepath.Join(t.TempDir(), "the hook ran")
			plant(t, first.Feed, marker)
			var stale []string
			if info, err := os.Lstat(first.Feed); err == nil && info.IsDir() {
				for _, name := range []string{"packed-refs.lock", filepath.Join("objects", "pack", "tmp_pack_Stale1")} {
					path := filepath.Join(first.Feed, name)
					if err := os.WriteFile(path, []byte("interrupted\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					stale = append(stale, path)
				}
			}
			advanceRemote(t, run.remote)
			before := len(run.Calls())
			owner, err := os.Lstat(filepath.Dir(first.Feed))
			if err != nil {
				t.Fatal(err)
			}
			planted, err := os.Lstat(first.Feed)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), "is not one this fetch wrote") {
				t.Fatalf("the Fetch after the plant = %v, want it to fail saying the feed is not one it wrote", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a planted hook ran in the fetch")
			}
			if calls := run.Calls()[before:]; len(calls) != 0 {
				t.Errorf("the Fetch after the plant ran %v, want no git at all", calls)
			}
			if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "removed") }) {
				t.Errorf("Fetch logged %q, want nothing removed", logged)
			}
			if info, err := os.Lstat(filepath.Dir(first.Feed)); err != nil || info.Mode().Type() != owner.Mode().Type() {
				t.Errorf("the feed's owner directory is %v (%v) after the fetch, want it as it stood, %v", info, err, owner.Mode().Type())
			}
			if info, err := os.Lstat(first.Feed); err != nil || info.Mode().Type() != planted.Mode().Type() {
				t.Errorf("the feed is %v (%v) after the fetch, want it as it stood, %v", info, err, planted.Mode().Type())
			}
			for _, path := range stale {
				if _, err := os.Lstat(path); err != nil {
					t.Errorf("the foreign feed's %s after the fetch: %v, want it left as it stood", path, err)
				}
			}
		})
	}
}

// git's update-server-info, which every `git repack` runs (the cache's due repack among them), writes
// info/refs and objects/info/packs through a temporary sibling named <file>_XXXXXX and renames it
// into place; a repack killed in between leaves that file. git never reads it, so a feed holding
// one is still one a fetch wrote: kept, never removed and cloned again whole.
func TestFetchKeepsAFeedHoldingAKilledRepacksServerInfoFiles(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	for _, leftover := range []string{"info/refs_XCPf7q", "objects/info/packs_cs3EN0"} {
		path := filepath.Join(first.Feed, leftover)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("0000000000000000000000000000000000000000\trefs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := len(run.Calls())

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
		t.Errorf("Fetch refused a feed holding only git's own leftovers: %q", logged)
	}
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned the feed again: %q", call.Argv)
		}
	}
}

// A feed can be garbage-collected between pod starts. A completed gc and one killed while its
// repack is running leave only files Git owns, so the next fetch keeps the feed instead of replacing
// it. A gc killed earlier, while it packs refs, also leaves packed-refs.lock: a lock makes no feed
// foreign, and Fetch removes it once it takes the feed, so it keeps that feed too.
func TestFetchKeepsAFeedAfterGitGC(t *testing.T) {
	for _, tc := range []struct {
		name string
		gc   func(t *testing.T, feed string)
	}{
		{name: "completed", gc: func(t *testing.T, feed string) {
			runSetup(t, feed, "git", "--git-dir="+feed, "gc", "--prune=now")
		}},
		{name: "killed during repack", gc: killGitGC},
		{name: "killed during pack-refs", gc: func(t *testing.T, feed string) {
			// git gc writes gc.pid, then packs refs under packed-refs.lock before it repacks; a pod
			// that ends there leaves both behind.
			if err := os.WriteFile(filepath.Join(feed, "gc.pid"), []byte("4242 killed-pod"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(feed, "packed-refs.lock"), []byte("# pack-refs with: peeled fully-peeled sorted \n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			first, err := Fetch(context.Background(), run, req)
			if err != nil {
				t.Fatalf("first Fetch: %v", err)
			}
			tc.gc(t, first.Feed)
			if why, err := foreignFeed(first.Feed); err != nil || why != "" {
				t.Fatalf("foreignFeed after git gc = %q, %v; want the feed trusted", why, err)
			}
			before := len(run.Calls())

			if _, err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("Fetch after git gc: %v", err)
			}
			if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
				t.Errorf("Fetch refused a feed after git gc: %q", logged)
			}
			for _, call := range run.Calls()[before:] {
				if commandWith(call.Argv, "git", "clone") {
					t.Errorf("Fetch replaced the feed after git gc: %q", call.Argv)
				}
			}
			if _, err := os.Lstat(filepath.Join(first.Feed, "packed-refs.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Fetch left the killed gc's packed-refs.lock: %v", err)
			}
		})
	}
}

// killGitGC runs a large, real repack and kills its process group once the repack is writing its
// pack, a tmp_pack_* entry under objects/pack. The kill is deliberate: a pod can end at this point,
// leaving gc.pid and the partial pack. git gc writes gc.pid before it packs refs and expires the
// reflog, both of which come before the repack, so a kill at gc.pid could land in pack-refs instead;
// that kill is the "killed during pack-refs" case. The gc runs under the fixture's environment, so
// no git configuration of the process running the tests reaches the repack, and the helper returns
// only once the kill, and not a failure of git's own, ended that gc with its pack half written.
func killGitGC(t *testing.T, feed string) {
	t.Helper()
	slowGitGC(t, feed)
	command := exec.Command("/usr/bin/git", "--git-dir="+feed, "gc", "--prune=now")
	command.Dir = feed
	environment := without(setupEnvironment(), "PATH")
	for _, key := range []string{"OMP_SESSION_ID", "PI_SESSION_FILE", "PI_TOOL_BRIDGE_URL", "AGENTBOX_SESSION"} {
		environment = without(environment, key)
	}
	command.Env = append(environment, "PATH=/usr/bin:/bin")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start git gc: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	})
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		if writing, _ := writingAPack(t, feed); writing {
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill git gc: %v", err)
			}
			var ended *exec.ExitError
			if err := command.Wait(); !errors.As(err, &ended) {
				t.Fatalf("git gc exited cleanly after its process group was killed: %v", err)
			} else if status, ok := ended.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("git gc ended on its own, not on the kill: %v", err)
			}
			if left, packs := writingAPack(t, feed); !left {
				t.Fatalf("git gc finished its pack before the kill reached it: the feed holds %q, not a repack's partial pack", packs)
			}
			return
		}
		select {
		case <-deadline.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			_, packs := writingAPack(t, feed)
			t.Fatalf("git gc did not start writing a pack during its repack: the feed holds %q", packs)
		case <-poll.C:
		}
	}
}

// writingAPack answers whether the feed's repack is writing its pack, a tmp_pack_* entry under
// objects/pack: the file pack-objects streams a pack into before it renames it into place, which
// stays behind when the repack is killed. It answers what that directory holds either way, so a
// wait that ends in neither state says what it saw instead.
func writingAPack(t *testing.T, feed string) (bool, []string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(feed, "objects", "pack"))
	if err != nil {
		t.Fatalf("read the feed's packs during git gc: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, "tmp_pack_") }), names
}

// slowGitGC gives the feed a large incompressible object the repack must write into its pack, so
// that the pack write lasts about a second and the wait above meets it. A ref holds the object:
// `git gc --prune=now` repacks what the feed's refs reach and prunes the rest, so an object no ref
// reaches never enters the pack at all. Without one, the repack wrote the feed's whole pack, 194
// bytes, in 11 milliseconds: a window a wait polling every millisecond held only by luck, and lost
// on a loaded runner, where the whole repack ran between two polls of a starved test.
func slowGitGC(t *testing.T, feed string) {
	t.Helper()
	blob, err := os.Create(filepath.Join(t.TempDir(), "large-blob"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(blob, rand.Reader, 64<<20); err != nil {
		_ = blob.Close()
		t.Fatal(err)
	}
	if err := blob.Close(); err != nil {
		t.Fatal(err)
	}
	object := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "hash-object", "-w", blob.Name()))
	runSetup(t, feed, "git", "--git-dir="+feed, "update-ref", "refs/large-blob", object)
}

// writeExecutable writes an executable script at path, making its directory.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// appendConfig appends text to the bare repository gitDir's configuration file as it stands.
func appendConfig(t *testing.T, gitDir, text string) {
	t.Helper()
	config, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	if _, err := config.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A clone that dies mid-transfer leaves nothing at the feed's repository: the clone runs in a
// temporary sibling renamed into place once git finished, so a feed on a volume that outlives the
// process (the issue's own, the cache's) never holds a half-cloned repository the next Fetch would
// take for a whole one and fetch into. A sibling a Fetch killed outright left (no defer ran) is
// removed by the next one, which clones whole.
func TestFetchLeavesNoHalfClonedRepositoryAtTheFeed(t *testing.T) {
	req := fetchRequest(t)
	run := newLocalRunner(t)
	killed := killedClone{Runner: run}
	if _, err := Fetch(context.Background(), &killed, req); err == nil || !strings.Contains(err.Error(), "killed mid-transfer") {
		t.Fatalf("Fetch = %v, want the killed clone's failure", err)
	}
	if killed.destination == "" {
		t.Fatal("the stand-in saw no clone")
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	if killed.destination == feed {
		t.Fatalf("Fetch cloned straight into %s", feed)
	}
	for _, path := range []string{feed, killed.destination} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the killed clone left %s behind (%v)", path, err)
		}
	}

	stale := feed + ".clone-12345"
	if err := os.MkdirAll(filepath.Join(stale, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the next Fetch left the killed one's %s (%v)", stale, err)
	}
	if want := []string{"removed the clone an earlier fetch left at " + stale}; !slices.Equal(logged, want) {
		t.Errorf("the next Fetch logged %q, want %q", logged, want)
	}
	if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", "main")) {
		t.Errorf("the feed's main is %s, not the remote's", main)
	}
}

// killedClone runs every command through Runner but a `git clone`, which it fails as a clone the
// kernel killed mid-transfer would leave it: a partial repository at its destination, status 137.
type killedClone struct {
	Runner
	destination string
}

func (r *killedClone) Run(ctx context.Context, command Command) (Result, error) {
	if !commandWith(command.Argv, "git", "clone") {
		return r.Runner.Run(ctx, command)
	}
	r.destination = command.Argv[len(command.Argv)-1]
	if err := os.MkdirAll(filepath.Join(r.destination, "objects"), 0o700); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: 137, Stderr: "killed mid-transfer"}, nil
}

// A transfer a signal ended after git printed the provisioning token, as a `fatal: unable to
// access` line naming the URL with its credential does, returns that line with the token scrubbed:
// what a killed run printed goes under its error only in checkedResult, after the transfer has
// scrubbed the Result's stderr, never inside the runner, whose error Fetch does not scrub. The
// URL's authority, which held the token, prints as the mark of a URL with credentials. The
// stand-in git prints the token it was handed and kills itself on a transfer from GitHub (the one
// command that names the credential's file), and runs the real git for every other command.
func TestFetchScrubsTheTokenFromATransferASignalEnded(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	fakeGit := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nif [ -n \"${" + provisioningTokenFileEnv + ":-}\" ]; then\n" +
		"  echo \"fatal: unable to access 'https://x-access-token:$(cat \"$" + provisioningTokenFileEnv + "\")@github.com/acme/widgets/': The requested URL returned error: 502\" >&2\n" +
		"  kill -KILL $$\nfi\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the stand-in git: %v", err)
	}
	run := NewRunner(time.Minute, map[string]string{"git": fakeGit})

	_, err = Fetch(context.Background(), run, fetchRequest(t))

	if err == nil {
		t.Fatal("Fetch succeeded through a transfer a signal ended")
	}
	if strings.Contains(err.Error(), testInstallationToken) {
		t.Errorf("Fetch's error carries the provisioning token: %v", err)
	}
	if !strings.Contains(err.Error(), ": signal: killed\nfatal: unable to access 'https://[a URL with credentials]/acme/widgets/'") {
		t.Errorf("Fetch = %v, want the transfer's signal and its line with the token scrubbed", err)
	}
}

// Feed configuration prevents automatic maintenance from competing with the token-holding fetch,
// and keeps each fetched object in a pack for the repository cache to stream. The clone owns its
// history, so a feed refresh cannot remove objects from it.
func TestFetchTurnsTheFeedsGarbageCollectionOffAndKeepsPacks(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	feed := report.Feed
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false", "fetch.unpackLimit": "1"} {
		if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "config", "--get", key)); got != want {
			t.Errorf("the feed's %s is %q, want %q", key, got, want)
		}
	}
	runSetup(t, feed, "git", "--git-dir="+feed, "repack", "-a", "-d", "--quiet")
	pushRemoteBranch(t, run.remote, "main", "one-small-change.txt")
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if loose := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "count-objects")); !strings.HasPrefix(loose, "0 objects") {
		t.Errorf("the fetch into the feed left %s", loose)
	}
}

// What of a bare repository the cache streams and a feed takes: its refs, its packs and the index
// git reads each pack through, nothing the feed rebuilds itself from them (a reverse index, a
// bitmap, a commit-graph), no loose object, nothing of its own, nothing that would point it at
// objects elsewhere, and nothing git is still writing.
func TestFeedEntryIsTheRepositorysRefsAndPacksAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  bool
		want bool
	}{
		{"HEAD", false, true},
		{"packed-refs", false, true},
		{"refs", true, true},
		{"refs/heads", true, true},
		{"refs/heads/release/v7.1", false, true},
		{"objects", true, true},
		{"objects/pack", true, true},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.pack", false, true},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.idx", false, true},

		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.rev", false, false},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.bitmap", false, false},
		{"objects/pack/multi-pack-index", false, false},
		{"objects/pack/pack-x.pack", false, false},
		{"objects/pack/pack-x.idx", false, false},
		{"objects/ab", true, false},
		{"objects/ab/cdef", false, false},
		{"objects/info", true, false},
		{"objects/info/packs", false, false},
		{"objects/info/commit-graph", false, false},
		{"objects/info/alternates", false, false},
		{"objects/info/http-alternates", false, false},
		{"config", false, false},
		{"description", false, false},
		{"hooks", true, false},
		{"hooks/pre-push.sample", false, false},
		{"info", true, false},
		{"info/exclude", false, false},
		{"FETCH_HEAD", false, false},
		{"HEAD", true, false},
		{"refs", false, false},
		{"objects/pack/tmp_pack_x", false, false},
		{"objects/incoming-x", true, false},
		{"refs/heads/main.lock", false, false},
		{"packed-refs.lock", false, false},
	} {
		if got := FeedEntry(tc.name, tc.dir); got != tc.want {
			t.Errorf("FeedEntry(%q, dir %t) = %t, want %t", tc.name, tc.dir, got, tc.want)
		}
	}
}

// cacheOf is the repository cache's clone of remote as the cache keeps it: bare, holding every
// branch remote has now in one pack, as a clone over git's transport keeps it.
func cacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "clone", "--bare", "--quiet", "--no-local", remote, cache)
	return cache
}

// packOf is the one pack of the bare repository dir.
func packOf(t *testing.T, dir string) string {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("%s holds packs %v (%v), want one", dir, packs, err)
	}
	return packs[0]
}

// corruptCacheOf is cacheOf with one byte of its pack flipped, inside an object's compressed data:
// a disk or a transfer that damaged the cache's history.
func corruptCacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	pack := packOf(t, cache)
	body, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0xff
	if err := os.Chmod(pack, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return cache
}

// damagedIndexOf is cacheOf with one byte of its pack's index flipped: an index a disk or a
// transfer damaged, which git would read the pack's objects through.
func damagedIndexOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	index := strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"
	body, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0xff
	writeIndex(t, index, body)
	return cache
}

// foreignIndexOf is cacheOf whose pack carries another repository's index, whole in itself and of
// another pack: what a cache that mixed two repositories' files would stream.
func foreignIndexOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	other := cacheOf(t, seedRemote(t, t.TempDir(), "main"))
	body, err := os.ReadFile(strings.TrimSuffix(packOf(t, other), ".pack") + ".idx")
	if err != nil {
		t.Fatal(err)
	}
	writeIndex(t, strings.TrimSuffix(packOf(t, cache), ".pack")+".idx", body)
	return cache
}

// writeIndex replaces the pack index at path, which git wrote read-only.
func writeIndex(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// incompleteCacheOf is cacheOf whose pack lacks one blob its history reaches: a cache that lost an
// object.
func incompleteCacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	var kept []string
	dropped := ""
	for _, line := range strings.Split(strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-list", "--objects", "--all")), "\n") {
		id, _, _ := strings.Cut(line, " ")
		kind := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "cat-file", "-t", id))
		if kind == "blob" && dropped == "" {
			dropped = id
			continue
		}
		kept = append(kept, id)
	}
	if dropped == "" {
		t.Fatal("the remote's history holds no blob to drop")
	}
	old := packOf(t, cache)
	// The repack runs in the cache it rewrites, under the package's fixture environment, as every
	// other git of these fixtures does.
	command := exec.Command("git", "--git-dir="+cache, "pack-objects", "--quiet", filepath.Join(cache, "objects", "pack", "pack"))
	command.Dir = cache
	command.Env = setupEnvironment()
	command.Stdin = strings.NewReader(strings.Join(kept, "\n") + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pack the cache without %s: %v\n%s", dropped, err, output)
	}
	for _, suffix := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(strings.TrimSuffix(old, ".pack") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return cache
}

// cacheTar is what the repository cache's /tar streams of the bare repository at dir: its history
// and refs (FeedEntry), each a directory or a regular file.
func cacheTar(t *testing.T, dir string) []byte {
	t.Helper()
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !FeedEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if entry.IsDir() {
			header.Name += "/"
			return archive.WriteHeader(header)
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = archive.Write(body)
		return err
	})
	if err == nil {
		err = archive.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// tarOf is a tar stream of entries, each a name and, for a regular file, its body; a name ending
// in / is a directory, and one ending in @ a symbolic link to HEAD.
func tarOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	for _, entry := range entries {
		header := &tar.Header{Name: entry[0], Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(entry[1]))}
		switch {
		case strings.HasSuffix(entry[0], "/"):
			header.Typeflag, header.Mode, header.Size = tar.TypeDir, 0o755, 0
		case strings.HasSuffix(entry[0], "@"):
			header.Name, header.Typeflag, header.Linkname, header.Size = strings.TrimSuffix(entry[0], "@"), tar.TypeSymlink, "HEAD", 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := archive.Write([]byte(entry[1])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// testCacheToken is the bearer the fake cache takes and fetchRequest's cache requests present.
const testCacheToken = "cache-token-of-acme-widgets"

// fakeCache stands in for the repository cache's server: GET /tar must carry the bearer token,
// testCacheToken unless token says otherwise, or it is answered 401; GET /healthz answers any
// request, 503 for its first notReady polls and then 200 ready, recording the Authorization each
// carried. An answered request names the cache's repository, acme/widgets unless repo or tarRepo
// says otherwise, in CacheRepositoryHeader. It counts each.
type fakeCache struct {
	body     []byte
	notReady int
	repo     string
	tarRepo  string
	token    string

	mu                sync.Mutex
	healthAsked       int
	healthCredentials []string
	tarAsked          int
}

func (c *fakeCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	repo := cmp.Or(c.repo, "acme/widgets")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		c.healthAsked++
		if credential := r.Header.Get("Authorization"); credential != "" {
			c.healthCredentials = append(c.healthCredentials, credential)
		}
		w.Header().Set(CacheRepositoryHeader, repo)
		if c.healthAsked <= c.notReady {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false,"repo":"` + repo + `","bytes":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true,"repo":"` + repo + `","bytes":1}`))
	case r.Method == http.MethodGet && r.URL.Path == "/tar":
		c.tarAsked++
		if r.Header.Get("Authorization") != "Bearer "+cmp.Or(c.token, testCacheToken) {
			http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
			return
		}
		w.Header().Set(CacheRepositoryHeader, cmp.Or(c.tarRepo, repo))
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(c.body)
	default:
		http.NotFound(w, r)
	}
}

// serve starts c and returns its address, closed when the test ends.
func (c *fakeCache) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	return server.URL
}

// With a repository cache, a feed that does not hold the repository yet is unpacked from the
// cache's tar and fetched from GitHub for what the issue's clone is made from and the cache can be
// behind on: the repository's default branch and the Legion issue branches. A branch outside those
// pushed since the cache's tar stays out of the feed, whatever its size. The report counts the
// tar's bytes and the few GitHub brought, nothing was cloned from GitHub, and the index the cache
// streamed with each pack is the one the feed reads it through: no pack is indexed again. The
// feed's configuration is its own, gc off, whatever the cache's was.
func TestFetchStreamsTheRepositoryCacheThenFetchesTheDeltaFromGitHub(t *testing.T) {
	run := newLocalRunner(t)
	cache := cacheOf(t, run.remote)
	body := cacheTar(t, cache)
	served := &fakeCache{body: body}
	moved := pushRemoteBranch(t, run.remote, "legion/ACME-7", "newer.txt")
	unread := pushRemoteBranch(t, run.remote, "release/v7.1", "unread.txt")
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken

	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	if report.Feed != feed || report.Cloned {
		t.Fatalf("Fetch reported %+v, want the feed %s filled from the cache", report, feed)
	}
	if report.CacheBytes != int64(len(body)) || report.CacheDuration <= 0 || report.VerifyDuration <= 0 {
		t.Errorf("the report counts %d bytes in %s from the cache, verified in %s, want the tar's %d and both times", report.CacheBytes, report.CacheDuration, report.VerifyDuration, len(body))
	}
	if report.GitHubBytes <= 0 || report.GitHubBytes >= 64<<10 || report.Refs != 1 {
		t.Errorf("the report counts %d bytes and %d refs from GitHub, want the one issue branch's few", report.GitHubBytes, report.Refs)
	}
	if got, want := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
		t.Errorf("the feed's main is %s, want the remote's %s", got, want)
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/legion/ACME-7")); got != moved {
		t.Errorf("the feed's legion/ACME-7 is %s, want the remote's newer %s", got, moved)
	}
	if refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "refs/heads/release/v7.1") {
		t.Errorf("the feed took release/v7.1 (%s) from GitHub, a branch no issue reads; its refs are %q", unread, refs)
	}
	runSetup(t, feed, "git", "--git-dir="+feed, "fsck", "--connectivity-only")
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub with a cache to stream: %q", call.Argv)
		}
		if slices.Contains(call.Argv, "index-pack") {
			t.Errorf("Fetch indexed again a pack the cache streamed its index with: %q", call.Argv)
		}
	}
	fetch := deltaCommand(run.Calls(), feed)
	if fetch.Argv == nil {
		t.Fatalf("Fetch fetched no delta from GitHub into the feed: %q", run.Calls())
	}
	if want := []string{"https://github.com/acme/widgets", "+refs/heads/main:refs/heads/main", "+refs/heads/legion/*:refs/heads/legion/*"}; !slices.Equal(fetch.Argv[len(fetch.Argv)-3:], want) || !slices.Contains(fetch.Argv, "--prune") {
		t.Errorf("the delta ran %q, want it pruning and ending in %q", fetch.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, fetch, fetch.Timeout)
	if fetch.Timeout > FetchTimeout || fetch.Timeout < FetchTimeout-time.Minute {
		t.Errorf("the delta was given %s, want its own %s, not what the verification of the cache's stream left", fetch.Timeout, FetchTimeout)
	}
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "config", "--get", key)); got != want {
			t.Errorf("the feed's %s is %q, want %q", key, got, want)
		}
	}
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A fresh feed comes from the repository cache alone: while the cache is not ready, Fetch waits on
// it, asking its /healthz every CachePoll and logging what it answered, then streams it once it is
// ready. Nothing is cloned from GitHub meanwhile. The health polls carry no credential: the cache's
// token goes with the tar alone.
func TestFetchWaitsForTheRepositoryCacheToBeReady(t *testing.T) {
	run := newLocalRunner(t)
	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote)), notReady: 3}
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
	clock := newTestClock()
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if report.Cloned || report.CacheBytes == 0 || served.tarAsked != 1 || served.healthAsked != 4 {
		t.Fatalf("Fetch reported %+v after %d health polls and %d tars, want the cache streamed once after 4 polls", report, served.healthAsked, served.tarAsked)
	}
	if want := []time.Duration{CachePoll, CachePoll, CachePoll}; !slices.Equal(clock.waits, want) {
		t.Errorf("Fetch waited %v between polls, want %v", clock.waits, want)
	}
	if len(served.healthCredentials) != 0 {
		t.Errorf("Fetch's health polls carried %q, want no credential", served.healthCredentials)
	}
	want := "waiting for the repository cache " + req.CacheURL + " to be ready: GET " + req.CacheURL + "/healthz answered 503 Service Unavailable"
	if len(logged) == 0 || !strings.HasPrefix(logged[0], want) {
		t.Errorf("Fetch logged %q, want the wait first: %q", logged, want)
	}
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub while it waited on the cache: %q", call.Argv)
		}
	}
}

// A cache that never becomes ready within CacheWaitTimeout fails the fetch, naming the cache and
// what it last answered: there is no full clone from GitHub instead, so a cache that is down never
// costs each new issue a full download. The wait's failure is outside the issue's volume.
func TestFetchFailsWhenTheRepositoryCacheIsNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(t *testing.T) string
		says string
	}{
		{"a cache answering not ready", func(t *testing.T) string { return (&fakeCache{notReady: 1 << 30}).serve(t) }, "503 Service Unavailable"},
		{"a cache that does not answer", func(t *testing.T) string {
			// A port the test holds and nothing listens on: every dial to it is refused.
			return "http://" + testport.Refused(t)
		}, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = tc.url(t), testCacheToken
			req.Clock = newTestClock()
			req.Log = func(string) {}
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), "the repository cache "+req.CacheURL+" was not ready within "+CacheWaitTimeout.String()) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want the wait's failure naming %q", err, tc.says)
			}
			if !errors.Is(err, ErrOutsideVolume) {
				t.Errorf("Fetch = %v, want a failure outside the volume (ErrOutsideVolume)", err)
			}
			assertNothingFilledTheFeed(t, run, req)
		})
	}
}

// The verification of what the cache streamed gets one FetchTimeout, counted from the stream's
// end, and the delta from GitHub one of its own, so the fetch's waits and its clocked stages end
// within FetchBudget however long it waited; the stream between them holds the low-speed bound
// and no clock at all (CacheStreamLowSpeedBytes). A cache ready 35 minutes in, whose history
// takes 25 minutes to check, gives the delta its whole bound, and the fresh feed is filled. So
// does a cache ready on the wait's last ask, 129 min 50 s in, whose history takes that whole
// bound: before the delta had a bound of its own, the fetch died there and its replacement
// streamed the cache again (sjawhar/legion#2235). The fetch still ends inside FetchBudget. The
// cache's bucket has a bound of its own too: a cache ready on that last ask whose bucket's history
// takes past the bucket attempt's whole BucketFetchTimeout to check, which ends it there, leaves
// /tar's verification its whole FetchTimeout, which it takes, and the delta its whole own bound;
// the feed comes from /tar, saying the bucket's check failed. (Each other row's cache serves no
// /packs, and costs no clock time.)
func TestFetchGivesTheCachesVerificationAndTheDeltaTheirOwnBounds(t *testing.T) {
	polls := func(d time.Duration) int { return int(d / CachePoll) }
	for _, tc := range []struct {
		name     string
		notReady int
		// bucket has the cache serve its packs from a bucket (packsCache), whose attempt's history
		// check is the step "bucket fsck" (bucketFirst).
		bucket   bool
		took     map[string]time.Duration
		timeouts map[string][]time.Duration
		fails    string
		elapsed  time.Duration
		// reason is how the report's TarReason of a fetch that succeeds begins.
		reason string
	}{
		{
			name: "a cache ready 35 minutes in", notReady: polls(35 * time.Minute),
			took:     map[string]time.Duration{"fsck": 25 * time.Minute},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}},
			elapsed:  35*time.Minute + 25*time.Minute,
			reason:   reasonNotServed,
		},
		{
			name: "a history that takes the whole first bound to check", notReady: polls(CacheWaitTimeout) - 1,
			took:     map[string]time.Duration{"fsck": FetchTimeout},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}},
			elapsed:  CacheWaitTimeout - CachePoll + FetchTimeout,
			reason:   reasonNotServed,
		},
		{
			name: "a bucket attempt that runs its whole bound, then a tar history that takes its whole own", notReady: polls(CacheWaitTimeout) - 1, bucket: true,
			took:     map[string]time.Duration{"bucket fsck": BucketFetchTimeout + time.Second, "fsck": FetchTimeout},
			timeouts: map[string][]time.Duration{"bucket fsck": {BucketFetchTimeout}, "fsck": {FetchTimeout}, "fetch": {FetchTimeout}},
			elapsed:  CacheWaitTimeout - CachePoll + BucketFetchTimeout + FetchTimeout,
			reason:   reasonGit,
		},
		{
			name: "a pack whose indexing takes the whole first bound", notReady: 0,
			took:     map[string]time.Duration{"index-pack": FetchTimeout},
			timeouts: map[string][]time.Duration{"index-pack": {FetchTimeout}},
			fails:    "the fetch's " + FetchTimeout.String() + " bound ran out before checking the repository cache's history is whole",
			elapsed:  FetchTimeout,
		},
		{
			name: "a delta that takes its whole own bound", notReady: 0,
			took:     map[string]time.Duration{"fetch": FetchTimeout},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}},
			fails:    "the fetch's " + FetchTimeout.String() + " bound ran out before the ls-remote from GitHub",
			elapsed:  FetchTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			clock := newTestClock()
			start := clock.Now()
			cache := cacheOf(t, run.remote)
			if _, indexes := tc.took["index-pack"]; indexes {
				if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
					t.Fatal(err)
				}
			}
			req := fetchRequest(t)
			req.Clock, req.Log = clock, func(string) {}
			step := cacheFetchStep
			if tc.bucket {
				newPacksCache(t, cache, tc.notReady).serve(t, &req)
				step = bucketFirst(cacheFetchStep)
			} else {
				req.CacheURL, req.CacheToken = (&fakeCache{body: cacheTar(t, cache), notReady: tc.notReady}).serve(t), testCacheToken
			}
			slow := &slowSteps{recordingRunner: run, clock: clock, step: step, took: tc.took, timeouts: map[string][]time.Duration{}}

			report, err := Fetch(context.Background(), slow, req)
			if tc.fails == "" && err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Fatalf("Fetch = %+v, %v; want it failed: %q", report, err, tc.fails)
			}
			if tc.fails == "" && (report.Source != SourceTar || !strings.HasPrefix(report.TarReason, tc.reason)) {
				t.Errorf("Fetch filled the feed from %q because %q, want /tar because %q", report.Source, report.TarReason, tc.reason)
			}
			if !maps.EqualFunc(slow.timeouts, tc.timeouts, slices.Equal[[]time.Duration]) {
				t.Errorf("the fetch's commands were given %v, want %v: each stage's own bound, from when it began", slow.timeouts, tc.timeouts)
			}
			if elapsed := clock.Now().Sub(start); elapsed != tc.elapsed || elapsed > FetchBudget {
				t.Errorf("the fetch ran %s, want %s, within FetchBudget %s", elapsed, tc.elapsed, FetchBudget)
			}
			if tc.fails != "" {
				assertNothingFilledTheFeed(t, run, req)
			}
		})
	}
}

// bucketFirst is step with the first `git fsck` it names called "bucket fsck": a fetch from a cache
// whose packs are in a bucket checks the bucket's history first, and /tar's only after that
// attempt failed.
func bucketFirst(step func(argv []string) string) func(argv []string) string {
	checked := false
	return func(argv []string) string {
		named := step(argv)
		if named == "fsck" && !checked {
			checked = true
			return "bucket fsck"
		}
		return named
	}
}

// cacheFetchStep names the steps of a fetch from the cache whose duration follows the
// repository's size: each pack's indexing, the history's check, and the delta from GitHub.
func cacheFetchStep(argv []string) string {
	switch {
	case slices.Contains(argv, "index-pack"):
		return "index-pack"
	case slices.Contains(argv, "fsck"):
		return "fsck"
	case argv[0] == "git" && slices.Contains(argv, "fetch"):
		return "fetch"
	}
	return ""
}

// assertNothingFilledTheFeed fails when Fetch cloned from GitHub or left anything at, or beside,
// the feed's repository.
func assertNothingFilledTheFeed(t *testing.T, run *recordingRunner, req FetchRequest) {
	t.Helper()
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub: %q", call.Argv)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(req.Feed, "acme")); err == nil && len(entries) != 0 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("Fetch left %v in the feed", names)
	}
}

// A cache whose tar holds an entry a feed does not take — one whose name leaves the feed, one that
// is not a regular file or directory, the cache's own configuration, an alternates file, a loose
// object — or whose history does not verify — a pack with a damaged object, a pack index damaged
// or belonging to another pack, a history missing an object it reaches — fails the fetch, naming
// the entry or the check: nothing of it reaches the feed, and nothing is cloned from GitHub. None
// of these failures is a failed write: each is an entry that unpackEntry refuses by its name or
// its type, or a failure of verifyCache. Fetch does not wrap either kind in ErrOutsideVolume, by
// its own rule (Fetch), so none of them reads as a failure outside the issue's volume.
func TestFetchRefusesARepositoryCacheItCannotUse(t *testing.T) {
	const pack = "objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	for _, tc := range []struct {
		name string
		body func(t *testing.T, remote string) []byte
		says string
	}{
		{"an entry leaving the feed", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"HEAD", "ref: refs/heads/main\n"}, [2]string{"../x", "escaped\n"})
		}, `the repository cache's tar holds "../x", which is not a path inside the feed`},
		{"a symbolic link", func(t *testing.T, _ string) []byte { return tarOf(t, [2]string{"refs/heads/main@"}) },
			`the repository cache's tar holds "refs/heads/main", which is neither a regular file nor a directory`},
		{"the cache's configuration", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"config", "[core]\n\tfsmonitor = true\n"})
		},
			`the repository cache's tar holds "config", which is not one of a repository's refs or packs`},
		{"an alternates file", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"objects/info/alternates", "/elsewhere\n"})
		},
			`the repository cache's tar holds "objects/info/alternates", which is not one of a repository's refs or packs`},
		{"a loose object", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"objects/ab/cdef0123456789abcdef0123456789abcdef01", "x"})
		}, `the repository cache's tar holds "objects/ab/cdef0123456789abcdef0123456789abcdef01", which is not one of a repository's refs or packs`},
		{"a damaged pack index", func(t *testing.T, remote string) []byte { return cacheTar(t, damagedIndexOf(t, remote)) },
			"which does not verify"},
		{"an index of another pack", func(t *testing.T, remote string) []byte { return cacheTar(t, foreignIndexOf(t, remote)) },
			"not this one"},
		{"a pack with a damaged object", func(t *testing.T, remote string) []byte { return cacheTar(t, corruptCacheOf(t, remote)) },
			"which does not verify"},
		{"a history missing an object", func(t *testing.T, remote string) []byte { return cacheTar(t, incompleteCacheOf(t, remote)) },
			"the repository cache's history is not whole"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = (&fakeCache{body: tc.body(t, run.remote)}).serve(t), testCacheToken
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), "fill the feed from the repository cache "+req.CacheURL) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want the cache refused naming %q", err, tc.says)
			}
			if errors.Is(err, ErrOutsideVolume) {
				t.Errorf("Fetch = %v, want a failure of what the cache streamed, not one outside the volume (ErrOutsideVolume)", err)
			}
			assertNothingFilledTheFeed(t, run, req)
			if _, err := os.Stat(filepath.Join(req.Feed, "acme", "x")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the tar wrote outside the feed (%v)", err)
			}
		})
	}
}

// A fetch asks only its own repository's cache, which it proves twice: it takes nothing from a
// cache whose answer names another repository — its /healthz, or the /tar itself, read before any
// of the tar's content — and it presents that cache's token for the tar, which another
// repository's cache refuses with 401. Each is refused at once, without waiting, and nothing
// reaches the feed. A cache named without its token is refused before anything is asked. Each
// refusal is outside the issue's volume, and its text is the refusal's own.
func TestFetchTakesNothingFromAnotherRepositorysCache(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cache  *fakeCache
		token  string
		says   string
		health int
		tars   int
	}{
		{"a cache that refuses the token", &fakeCache{}, "another-cache's-token", "/tar answered 401 Unauthorized", 1, 1},
		{"a cache whose health names another repository", &fakeCache{repo: "acme/gadgets"}, testCacheToken,
			"is the repository cache of acme/gadgets, not of acme/widgets", 1, 0},
		{"a cache whose tar names another repository", &fakeCache{tarRepo: "acme/gadgets"}, testCacheToken,
			"is the repository cache of acme/gadgets, not of acme/widgets", 1, 1},
		{"a cache that names no repository", &fakeCache{tarRepo: " "}, testCacheToken,
			`is the repository cache of "", not of acme/widgets`, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			tc.cache.body = cacheTar(t, cacheOf(t, run.remote))
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = tc.cache.serve(t), tc.token
			clock := newTestClock()
			req.Clock = clock
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), req.CacheURL) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want a refusal naming %q", err, tc.says)
			}
			if !errors.Is(err, ErrOutsideVolume) {
				t.Errorf("Fetch = %v, want a refusal outside the volume (ErrOutsideVolume)", err)
			}
			if tc.cache.healthAsked != tc.health || tc.cache.tarAsked != tc.tars || len(clock.waits) != 0 {
				t.Errorf("Fetch asked /healthz %d and /tar %d times and waited %v, want %d, %d and no wait", tc.cache.healthAsked, tc.cache.tarAsked, clock.waits, tc.health, tc.tars)
			}
			assertNothingFilledTheFeed(t, run, req)
		})
	}

	req := fetchRequest(t)
	req.CacheURL = "http://127.0.0.1:1"
	_, err := Fetch(context.Background(), newLocalRunner(t), req)
	if err == nil || err.Error() != "workspace fetch: a repository cache needs its token" {
		t.Fatalf("Fetch with a cache and no token = %v", err)
	}
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("Fetch with a cache and no token = %v, want a refusal outside the volume (ErrOutsideVolume)", err)
	}
}

// Without a cache, Fetch clones from GitHub and the report says how many bytes that brought: what
// the repository cache's own process does. A feed that holds the repository already is fetched
// into and asks no cache, whatever cache it is given.
func TestFetchReportsWhereTheFeedCameFrom(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !report.Cloned || report.CacheBytes != 0 || report.GitHubBytes <= 0 {
		t.Fatalf("Fetch without a cache reported %+v, want a clone from GitHub with its bytes", report)
	}

	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
	advanceRemote(t, run.remote)
	again, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if again.Cloned || again.CacheBytes != 0 || again.Refs != 1 || again.GitHubBytes <= 0 || served.healthAsked+served.tarAsked != 0 {
		t.Fatalf("Fetch into a feed holding the repository reported %+v and asked the cache %d times, want a delta of one ref from GitHub and no cache", again, served.healthAsked+served.tarAsked)
	}
}

// A feed a retried fetch finds on the pod's disk is fetched into with the narrowed refspecs, and
// holds GitHub's branches as GitHub has them: an issue branch GitHub deleted leaves the feed, as
// provisioning relies on (a bookmark with no row starts at the default branch), and so does a
// branch outside the two that the cache's stream brought and GitHub has since deleted, while one
// GitHub still has at the same tip stays, and a branch outside the two pushed since is not taken.
func TestARetriedFetchTakesTheNarrowedDeltaAndDropsADeletedIssueBranch(t *testing.T) {
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "legion/ACME-1", "doomed.txt")
	pushRemoteBranch(t, run.remote, "release/v7.1", "streamed.txt")
	pushRemoteBranch(t, run.remote, "gone/soon", "gone.txt")
	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
	req := fetchRequest(t)
	var logged []string
	req.CacheURL, req.CacheToken, req.Log = served.serve(t), testCacheToken, func(line string) { logged = append(logged, line) }
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-D", "legion/ACME-1")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-D", "gone/soon")
	moved := pushRemoteBranch(t, run.remote, "legion/ACME-2", "kept.txt")
	pushRemoteBranch(t, run.remote, "unread/big", "unread.txt")
	before := len(run.Calls())

	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if report.CacheBytes != 0 || served.tarAsked != 1 {
		t.Fatalf("the second Fetch reported %+v after %d tars, want the delta alone into the feed the first left", report, served.tarAsked)
	}
	fetch := deltaCommand(run.Calls()[before:], feed)
	if want := []string{"--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/main:refs/heads/main", "+refs/heads/legion/*:refs/heads/legion/*"}; fetch.Argv == nil || !slices.Equal(fetch.Argv[len(fetch.Argv)-5:], want) {
		t.Fatalf("the feed the first fetch left was fetched with %q, want it ending in %q", fetch.Argv, want)
	}
	refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)")
	for ref, want := range map[string]bool{"refs/heads/legion/ACME-1": false, "refs/heads/legion/ACME-2": true, "refs/heads/release/v7.1": true, "refs/heads/unread/big": false, "refs/heads/gone/soon": false} {
		if got := strings.Contains(refs, ref+"\n"); got != want {
			t.Errorf("the feed holds %s: %t, want %t; its refs are %q", ref, got, want, refs)
		}
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/legion/ACME-2")); got != moved {
		t.Errorf("the feed's legion/ACME-2 is %s, want the remote's %s", got, moved)
	}
	if !slices.Contains(logged, "removed gone/soon from the feed: GitHub no longer has it") {
		t.Errorf("the removal of gone/soon was not logged: %q", logged)
	}
}

// When GitHub's default is switched from main to trunk and main then moves, a feed a retried
// fetch finds keeps no stale tip of main. For an issue whose recorded default is main, the delta brings
// main current beside trunk, so provisioning starts there as it did when every branch was fetched.
// With no recorded default (a daemon from before the runtime told it), main is outside the delta
// and is deleted, so provisioning finds it absent and refuses rather than starting from the tip
// main had when the feed was first streamed. A branch outside the delta that GitHub has not moved
// stays either way.
func TestARetriedFetchKeepsNoStaleBranchAfterTheDefaultIsSwitched(t *testing.T) {
	for _, recorded := range []string{"main", ""} {
		t.Run("recorded default "+cmp.Or(recorded, "none"), func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "trunk", "trunk.txt")
			pushRemoteBranch(t, run.remote, "release/v7.1", "unmoved.txt")
			served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
			req := fetchRequest(t)
			var logged []string
			req.CacheURL, req.CacheToken, req.RecordedDefault = served.serve(t), testCacheToken, recorded
			req.Log = func(line string) { logged = append(logged, line) }
			if _, err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("the first Fetch: %v", err)
			}
			feed := filepath.Join(req.Feed, "acme", "widgets.git")
			runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			mainTip := pushRemoteBranch(t, run.remote, "main", "moved.txt")
			trunk := pushRemoteBranch(t, run.remote, "trunk", "newer.txt")

			if _, err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("the second Fetch: %v", err)
			}
			refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)")
			for ref, want := range map[string]bool{"refs/heads/main": recorded == "main", "refs/heads/trunk": true, "refs/heads/release/v7.1": true} {
				if got := strings.Contains(refs, ref+"\n"); got != want {
					t.Errorf("the feed holds %s: %t, want %t; its refs are %q", ref, got, want, refs)
				}
			}
			if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/trunk")); got != trunk {
				t.Errorf("the feed's trunk is %s, want the remote's %s", got, trunk)
			}
			removed := "removed main from the feed: GitHub has moved it, and this fetch brings only the default branch and the issue branches"
			if recorded == "main" {
				if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); got != mainTip {
					t.Errorf("the feed's main is %s, want the remote's %s: the issue's recorded default kept current", got, mainTip)
				}
				if slices.Contains(logged, removed) {
					t.Errorf("Fetch removed the issue's recorded default: %q", logged)
				}
			} else if !slices.Contains(logged, removed) {
				t.Errorf("Fetch logged %q, want %q", logged, removed)
			}
		})
	}
}

// A fresh feed — every pod's first fetch since an issue's disk dies with its pod — streamed from a
// cache taken before GitHub's default moved off the issue's recorded default. When GitHub still
// has the recorded default and it has moved since the cache's tar, the delta brings it current
// beside the new default. When GitHub no longer has it (renamed away), the fetch still succeeds:
// the delta does not name it, the feed's old copy is deleted, and that is logged, so provisioning
// refuses an issue with no branch rather than starting it from the cache's old tip.
func TestAFreshFeedHoldsTheRecordedDefaultAsGitHubHasIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		move    func(t *testing.T, remote string) string
		present bool
	}{
		{"moved and still on GitHub", func(t *testing.T, remote string) string {
			pushRemoteBranch(t, remote, "trunk", "trunk.txt")
			runSetup(t, t.TempDir(), "git", "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			return pushRemoteBranch(t, remote, "main", "moved.txt")
		}, true},
		{"renamed away on GitHub", func(t *testing.T, remote string) string {
			runSetup(t, t.TempDir(), "git", "--git-dir="+remote, "branch", "-m", "main", "trunk")
			return ""
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
			want := tc.move(t, run.remote)
			req := fetchRequest(t)
			var logged []string
			req.CacheURL, req.CacheToken, req.RecordedDefault = served.serve(t), testCacheToken, "main"
			req.Log = func(line string) { logged = append(logged, line) }
			report, err := Fetch(context.Background(), run, req)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			fetch := deltaCommand(run.Calls(), report.Feed)
			if named := slices.Contains(fetch.Argv, "+refs/heads/main:refs/heads/main"); named != tc.present {
				t.Errorf("the delta ran %q, naming main %t, want %t", fetch.Argv, named, tc.present)
			}
			refs := runSetup(t, report.Feed, "git", "--git-dir="+report.Feed, "for-each-ref", "--format=%(refname)")
			if held := strings.Contains(refs, "refs/heads/main\n"); held != tc.present || !strings.Contains(refs, "refs/heads/trunk\n") {
				t.Fatalf("the feed's refs are %q, want trunk, and main %t", refs, tc.present)
			}
			if tc.present {
				if got := strings.TrimSpace(runSetup(t, report.Feed, "git", "--git-dir="+report.Feed, "rev-parse", "refs/heads/main")); got != want {
					t.Errorf("the feed's main is %s, want GitHub's %s", got, want)
				}
			} else if !slices.Contains(logged, "removed main from the feed: GitHub no longer has it") {
				t.Errorf("the removal of main was not logged: %q", logged)
			}
		})
	}
}

// A default branch GitHub renamed leaves a feed a retried fetch finds: the next fetch takes the new
// name, deletes the old one, and points HEAD at the new one.
func TestARetriedFetchFollowsARenamedDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken, req.Log = served.serve(t), testCacheToken, func(string) {}
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "refs/heads/main\n") || !strings.Contains(refs, "refs/heads/trunk\n") {
		t.Errorf("the feed's refs are %q, want trunk and no main", refs)
	}
	if head := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "symbolic-ref", "HEAD")); head != "refs/heads/trunk" {
		t.Errorf("the feed's HEAD names %s, want refs/heads/trunk", head)
	}
}

// A GitHub that names no default branch (its HEAD unborn: nothing pushed to it) leaves the delta
// taking every branch, pruned, as a fetch with no cache does, and nothing is deleted from the feed
// on the strength of a listing that names no default.
func TestADeltaWithNoDefaultBranchTakesEveryBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken, req.Log = (&fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}).serve(t), testCacheToken, func(string) {}
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "symbolic-ref", "HEAD", "refs/heads/unborn")
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	fetch := deltaCommand(run.Calls(), report.Feed)
	if want := []string{"--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/*:refs/heads/*"}; fetch.Argv == nil || !slices.Equal(fetch.Argv[len(fetch.Argv)-4:], want) {
		t.Errorf("the delta ran %q, want it ending in %q", fetch.Argv, want)
	}
	if got := strings.TrimSpace(runSetup(t, report.Feed, "git", "--git-dir="+report.Feed, "rev-parse", "--verify", "refs/heads/main")); got == "" {
		t.Errorf("the feed lost main")
	}
}

// The narrowed delta takes the default branch GitHub's HEAD names, which need not be main and may
// hold a slash.
func TestTheDeltaTakesTheDefaultBranchGitHubNames(t *testing.T) {
	run := newLocalRunnerOnBranch(t, "release/v7.1")
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken, req.Log = (&fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}).serve(t), testCacheToken, func(string) {}
	moved := pushRemoteBranch(t, run.remote, "release/v7.1", "newer.txt")
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	fetch := deltaCommand(run.Calls(), report.Feed)
	if want := "+refs/heads/release/v7.1:refs/heads/release/v7.1"; !slices.Contains(fetch.Argv, want) || slices.Contains(fetch.Argv, "+refs/heads/main:refs/heads/main") {
		t.Errorf("the delta ran %q, want it naming %q and no main", fetch.Argv, want)
	}
	if got := strings.TrimSpace(runSetup(t, report.Feed, "git", "--git-dir="+report.Feed, "rev-parse", "refs/heads/release/v7.1")); got != moved {
		t.Errorf("the feed's release/v7.1 is %s, want the remote's %s", got, moved)
	}
}

// A pack the cache streamed without its index — a cache on a release that never streamed one —
// is indexed in the pod, as every pack was before, and the feed is whole.
func TestAPackWithoutItsIndexIsIndexedInThePod(t *testing.T) {
	run := newLocalRunner(t)
	cache := cacheOf(t, run.remote)
	if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
		t.Fatal(err)
	}
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken, req.Log = (&fakeCache{body: cacheTar(t, cache)}).serve(t), testCacheToken, func(string) {}
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	indexed := 0
	for _, call := range run.Calls() {
		if slices.Contains(call.Argv, "index-pack") {
			indexed++
		}
	}
	if indexed != 1 {
		t.Errorf("Fetch indexed %d packs, want the one the cache streamed without its index", indexed)
	}
	runSetup(t, report.Feed, "git", "--git-dir="+report.Feed, "fsck", "--connectivity-only")
}

// Every git one Fetch starts, and every git the repository cache runs on its clone through FeedGit
// and FeedGitWithin, maps packs in small windows from its own command line: it reads no system
// file, its runner removes the role environment's pairs, and the feed's configuration may hold none
// of the four settings. A recording git first on PATH, the git the runner resolves as boot does,
// reads what each of those commands sees of the four with `git config --show-scope --get-regexp`
// under that command's `-c` pairs, its repository and its environment, then runs the real git. Each
// reads core.packedGitWindowSize 32m, core.packedGitLimit 256m, pack.windowMemory 64m and
// core.deltaBaseCacheLimit 64m, every line of them from the command scope: a Fetch from a
// repository cache whose pack came without its index, and whose branch GitHub has since deleted, so
// it initializes, indexes, checks, reads GitHub's refs, prunes, fetches and configures the feed; a
// Fetch from GitHub alone, which clones it; and the cache's read of its head and its repack. The
// repack here is given `-c` pairs of a caller's own, which come after the four and win for its
// command: 64m windows, a 512m limit and 256m of window memory. Every git the runner started is
// recorded.
func TestTheFeedsFetchMapsPacksInSmallWindows(t *testing.T) {
	windows := map[string]string{}
	for _, setting := range packWindows {
		windows[strings.ToLower(setting[0])] = setting[1]
	}
	// callerPairs are a caller's own `-c` pairs, as a caller hands them to FeedGitWithin, and
	// callerWins what its command reads of the four with them.
	callerPairs := []string{
		"-c", "pack.threads=1", "-c", "pack.windowMemory=256m", "-c", "pack.deltaCacheSize=64m",
		"-c", "core.deltaBaseCacheLimit=64m", "-c", "core.packedGitLimit=512m", "-c", "core.packedGitWindowSize=64m",
		"-c", "repack.writeBitmaps=false",
	}
	callerWins := map[string]string{"core.packedgitwindowsize": "64m", "core.packedgitlimit": "512m", "pack.windowmemory": "256m", "core.deltabasecachelimit": "64m"}
	for _, tc := range []struct {
		name string
		// prepare makes what the subtest's gits run on, and answers what runs them.
		prepare func(t *testing.T, run *recordingRunner) func() error
	}{
		{"from the repository cache", func(t *testing.T, run *recordingRunner) func() error {
			req := fetchRequest(t)
			pushRemoteBranch(t, run.remote, "release/gone", "gone.txt")
			cache := cacheOf(t, run.remote)
			runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "update-ref", "-d", "refs/heads/release/gone")
			if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
				t.Fatal(err)
			}
			req.CacheURL, req.CacheToken = (&fakeCache{body: cacheTar(t, cache)}).serve(t), testCacheToken
			pushRemoteBranch(t, run.remote, "legion/ACME-7", "newer.txt")
			return func() error { _, err := Fetch(context.Background(), run, req); return err }
		}},
		{"from GitHub alone", func(t *testing.T, run *recordingRunner) func() error {
			req := fetchRequest(t)
			return func() error { _, err := Fetch(context.Background(), run, req); return err }
		}},
		{"the repository cache's own git and its repack", func(t *testing.T, run *recordingRunner) func() error {
			cache := cacheOf(t, run.remote)
			return func() error {
				if _, err := FeedGit(context.Background(), run, cache, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
					return err
				}
				_, err := FeedGitWithin(context.Background(), run, cache, testTimeout, append(slices.Clone(callerPairs), "repack", "-a", "-d", "--quiet")...)
				return err
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			start := tc.prepare(t, run)
			sink := recordPackWindows(t, run)
			if err := start(); err != nil {
				t.Fatalf("the gits under test: %v", err)
			}
			body, err := os.ReadFile(sink)
			if err != nil {
				t.Fatalf("the recording git recorded nothing: %v", err)
			}
			recorded := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
			for _, line := range recorded {
				argv, reads, _ := strings.Cut(line, "\t")
				want := windows
				if slices.Contains(strings.Fields(argv), "repack") {
					want = callerWins
				}
				// git takes a key's last line, which got keeps.
				got := map[string]string{}
				for read := range strings.SplitSeq(reads, ";") {
					if read == "" {
						continue
					}
					scope, setting, _ := strings.Cut(read, " ")
					if scope != "command" {
						t.Errorf("git %s read %s from the %s scope, want the command scope alone", argv, setting, scope)
					}
					key, value, _ := strings.Cut(setting, " ")
					got[key] = value
				}
				if !maps.Equal(got, want) {
					t.Errorf("git %s reads the pack settings as %v, want %v", argv, got, want)
				}
			}
			started := 0
			for _, call := range run.Calls() {
				if call.Argv[0] == "git" {
					started++
				}
			}
			if started == 0 || len(recorded) != started {
				t.Errorf("the recording git ran %d times for the %d gits the runner started", len(recorded), started)
			}
		})
	}
}

// recordPackWindows has run's runner start a recording git, resolved from PATH as boot resolves
// it (testTools), and answers the file it records in: one line each time it runs, its arguments,
// a tab, then each line `git config --show-scope --get-regexp` prints for packWindows' four keys
// under the `-c` pairs and the --git-dir its command line names before its subcommand, and its
// environment, a space for a tab and each ended by `;`. It then runs the real git with its
// arguments. PATH is the test's own again once the runner has resolved it, so no fixture command
// runs the recording git.
func recordPackWindows(t *testing.T, run *recordingRunner) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, sink := t.TempDir(), filepath.Join(t.TempDir(), "record")
	keys := make([]string, 0, len(packWindows))
	for _, setting := range packWindows {
		keys = append(keys, regexp.QuoteMeta(strings.ToLower(setting[0])))
	}
	script := "#!/bin/sh\n" +
		"scoped() {\n" +
		"  n=$# ahead=1\n" +
		"  while [ \"$n\" -gt 0 ]; do\n" +
		"    arg=$1; shift; n=$((n - 1))\n" +
		"    if [ -n \"$ahead\" ] && [ \"$arg\" = -c ] && [ \"$n\" -gt 0 ]; then\n" +
		"      set -- \"$@\" -c \"$1\"; shift; n=$((n - 1))\n" +
		"    elif [ -n \"$ahead\" ] && [ \"${arg#--git-dir=}\" != \"$arg\" ]; then\n" +
		"      set -- \"$@\" \"$arg\"\n" +
		"    else\n" +
		"      ahead=\n" +
		"    fi\n" +
		"  done\n" +
		"  '" + realGit + "' \"$@\" config --show-scope --get-regexp '^(" + strings.Join(keys, "|") + ")$'\n" +
		"}\n" +
		"reads=$(scoped \"$@\" | tr '\\t\\n' ' ;')\n" +
		"printf '%s\\t%s\\n' \"$*\" \"$reads\" >> '" + sink + "'\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	run.runner = NewRunner(testTimeout, testTools(t))
	t.Setenv("PATH", path)
	return sink
}

// deltaCommand is the last `git fetch` in calls into the feed repository feed or the staging
// sibling a fresh feed is filled in before it is renamed into place.
func deltaCommand(calls []Command, feed string) Command {
	var found Command
	for _, call := range calls {
		if slices.Contains(call.Argv, "fetch") && slices.ContainsFunc(call.Argv, func(arg string) bool {
			return arg == "--git-dir="+feed || strings.HasPrefix(arg, "--git-dir="+feed+cloneSuffix)
		}) {
			found = call
		}
	}
	return found
}
