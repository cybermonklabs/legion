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

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testport"
)

// gitFailure is a failed transfer from GitHub as git reports it, with how transferWithRetries reads
// it: transient, whether the transfer is run again (transientCloneFailure), and outside, whether
// the cause is outside the issue's volume (outsideVolumeFailure), which makes `workspace-init
// provision` exit 3 rather than 1.
type gitFailure struct {
	name               string
	result             Result
	transient, outside bool
}

// gitFailures are the failures two tests read. The texts are git's and curl's own. The first three
// are as a production fetch printed them. The statuses and the cut transfer are as git printed
// them against a local git http-backend over TLS. The rest are curl's error strings, the TLS ones
// both GnuTLS's (whose libcurl Debian's git links) and OpenSSL's. Curl 23 has the two texts
// libcurl holds for it. The bad object is what git 2.47.3's fetch printed from a remote that lacks
// an object a ref of the repository it fetched into names.
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
		{"a destination already there", fatal("fatal: destination path '/legion/repos/github.com/acme/widgets.clone-1234' already exists and is not an empty directory.\n"), false, false},
		{"a full disk", fatal("fatal: write error: No space left on device\nfatal: fetch-pack: invalid index-pack output\n"), false, false},
		{"a bad object in the clone", Result{ExitCode: 1, Stderr: "fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111\nerror: file:///tmp/bad-object/remote.git did not send all necessary objects\n"}, false, false},
		{"a transient text under another exit", Result{ExitCode: 1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false, false},
		{"a clone killed by a signal", Result{ExitCode: -1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false, false},
		{"a clone that timed out", Result{ExitCode: -1, TimedOut: true}, false, true},
	}
}()

// Which transfer failures are retried: git's fatal exit with a cause naming a connection that
// failed or broke, or a status a server may not answer again. A curl error is told the same way
// whichever request met it.
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
// not cure them and `workspace-init provision` exits 3, which keeps a kept volume: an attempt that
// ran out of its time, or git's fatal exit with a cause line that names any HTTP status or a
// connection that failed or broke, a pack stream cut with no cause line, a credential GitHub
// refused, or a repository GitHub did not find. Every other cause line (curl 23, a write to the
// volume that failed, and curl 60), a credential git could not read, and a bad object in the clone
// are not, and exit 1, which replaces the volume. The two tests read one set of git's lines
// (gitFailures).
func TestAFailureOutsideTheVolumeIsReadFromGitsOutput(t *testing.T) {
	for _, tc := range gitFailures {
		t.Run(tc.name, func(t *testing.T) {
			if got := outsideVolumeFailure(tc.result); got != tc.outside {
				t.Fatalf("outsideVolumeFailure(%q, exit %d, timed out %t) = %t, want %t", tc.result.Stderr, tc.result.ExitCode, tc.result.TimedOut, got, tc.outside)
			}
		})
	}
}

// fatal is a transfer git ended with its fatal status, 128, printing stderr.
func fatal(stderr string) Result { return Result{ExitCode: 128, Stderr: stderr} }

// failingFetch is the local runner whose every fetch from GitHub — the repository cache's first
// fill's transfer into a volume that holds no cache yet, whose read of GitHub's HEAD before it runs
// as the local runner has it — takes took of clock's time and then fails with result, or, given
// less time than that, runs to its timeout and is killed there.
type failingFetch struct {
	*recordingRunner
	clock  *testClock
	took   time.Duration
	result Result

	attempts []fetchAttempt
}

// fetchAttempt is one fetch failingFetch ran: when it started, by the clock, and its timeout.
type fetchAttempt struct {
	at      time.Time
	timeout time.Duration
}

// newFailingFetch is failingFetch on clock, failing every fetch with result.
func newFailingFetch(t *testing.T, clock *testClock, took time.Duration, result Result) *failingFetch {
	t.Helper()
	return &failingFetch{recordingRunner: newLocalRunner(t), clock: clock, took: took, result: result}
}

func (r *failingFetch) Run(ctx context.Context, command Command) (Result, error) {
	if command.Argv[0] != "git" || !slices.Contains(command.Argv, "fetch") {
		return r.recordingRunner.Run(ctx, command)
	}
	r.attempts = append(r.attempts, fetchAttempt{at: r.clock.Now(), timeout: command.Timeout})
	if command.Timeout <= r.took {
		r.clock.advance(command.Timeout)
		return Result{ExitCode: -1, TimedOut: true}, nil
	}
	r.clock.advance(r.took)
	return r.result, nil
}

// fetchesUnderTheBound fails the test unless each of run's fetches was given what remained of
// FetchTimeout from the first one's start, and returns how long the fetch ran by clock.
func fetchesUnderTheBound(t *testing.T, run *failingFetch, clock *testClock) time.Duration {
	t.Helper()
	start := run.attempts[0].at
	for i, attempt := range run.attempts {
		if want := FetchTimeout - attempt.at.Sub(start); attempt.timeout != want {
			t.Errorf("fetch attempt %d started %s in with timeout %s, want what remained of the bound, %s", i+1, attempt.at.Sub(start), attempt.timeout, want)
		}
	}
	elapsed := clock.Now().Sub(start)
	if elapsed > FetchTimeout {
		t.Errorf("the fetch ran %s, past its %s bound", elapsed, FetchTimeout)
	}
	return elapsed
}

// A fetch that keeps failing quickly, as one does through a GitHub outage, is retried once after
// each backoff wait and no more: the fifth transient failure ends the fetch, 1 min 50 s in, naming
// the failure and the retry limit, rather than holding the cache for the whole FetchTimeout. The
// failure is outside the volume. transferWithRetries holds an issue pod's fill to the same rule.
func TestFetchStopsAfterItsLastRetry(t *testing.T) {
	clock := newTestClock()
	run := newFailingFetch(t, clock, 0, fatal("fatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 503\n"))
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	err := Fetch(context.Background(), run, req)
	want := "fetch attempt 5 failed, and the fetch retries a fetch at most 4 times"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "returned error: 503") {
		t.Fatalf("Fetch = %v, want the 503 and %q", err, want)
	}
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("Fetch = %v, want a failure outside the volume (ErrOutsideVolume)", err)
	}
	if len(run.attempts) != 5 || len(logged) != 4 {
		t.Fatalf("Fetch fetched %d times and logged %d retries, want 5 and 4", len(run.attempts), len(logged))
	}
	if !slices.Equal(clock.waits, fetchBackoff) {
		t.Fatalf("Fetch waited %v, want %v", clock.waits, fetchBackoff)
	}
	if elapsed := fetchesUnderTheBound(t, run, clock); elapsed != time.Minute+50*time.Second {
		t.Errorf("the fetch gave up %s in, want 1m50s", elapsed)
	}
}

// A fetch whose every attempt runs ten minutes before it breaks, as a slow transfer GitHub cuts
// does, is given what remains of FetchTimeout each time, and the third, started 20 min 20 s in
// with less than ten minutes left, is killed at the bound itself: Fetch never runs past
// FetchTimeout, and an attempt that ran out of its time is a cause outside the volume.
func TestFetchKillsTheAttemptThatReachesItsBound(t *testing.T) {
	clock := newTestClock()
	run := newFailingFetch(t, clock, 10*time.Minute, fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\nfatal: early EOF\n"))
	req := fetchRequest(t)
	req.Clock = clock

	err := Fetch(context.Background(), run, req)
	if want := "command timed out: " + strings.Join(slices.Concat(isolatedGit, []string{"-c", "gc.auto=0"}), " "); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Fetch = %v, want the last attempt's timeout", err)
	}
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("Fetch = %v, want an attempt that ran out of its time outside the volume (ErrOutsideVolume)", err)
	}
	if len(run.attempts) != 3 {
		t.Fatalf("Fetch fetched %d times, want 3", len(run.attempts))
	}
	if elapsed := fetchesUnderTheBound(t, run, clock); elapsed != FetchTimeout {
		t.Errorf("the fetch gave up %s in, want at its bound, %s", elapsed, FetchTimeout)
	}
}

// A retry whose wait would end past FetchTimeout is not made: Fetch fails naming the failure and
// the bound.
func TestFetchMakesNoRetryThatWouldStartPastItsBound(t *testing.T) {
	clock := newTestClock()
	run := newFailingFetch(t, clock, FetchTimeout-3*time.Second,
		fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n"))
	req := fetchRequest(t)
	req.Clock = clock

	err := Fetch(context.Background(), run, req)
	want := "fetch attempt 1 failed, and a retry in 5s would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 {
		t.Fatalf("Fetch fetched %d times and waited %v, want one fetch and no wait", len(run.attempts), clock.waits)
	}
}

// A wait that ends past the bound — its timer fired late — starts no further attempt, which would
// have no time at all: Fetch fails naming the last attempt's failure and the bound.
func TestFetchStartsNoAttemptOnceAWaitEndsPastItsBound(t *testing.T) {
	clock := newTestClock()
	clock.overshoot = 2 * time.Millisecond
	run := newFailingFetch(t, clock, FetchTimeout-5*time.Second-time.Millisecond,
		fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n"))
	req := fetchRequest(t)
	req.Clock = clock

	err := Fetch(context.Background(), run, req)
	want := "fetch attempt 1 failed, and its retry after the 5s wait would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch fetched %d times, want 1", len(run.attempts))
	}
}

// A permanent failure is Fetch's answer at once: one fetch, no wait, no retry logged. GitHub not
// finding the repository is outside the volume.
func TestFetchFailsAPermanentFetchFailureAtOnce(t *testing.T) {
	clock := newTestClock()
	run := newFailingFetch(t, clock, 0, fatal("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"))
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	err := Fetch(context.Background(), run, req)
	if err == nil || !strings.Contains(err.Error(), "fatal: repository 'https://github.com/acme/widgets/' not found") {
		t.Fatalf("Fetch = %v, want git's not found", err)
	}
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("Fetch = %v, want a failure outside the volume (ErrOutsideVolume)", err)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 || len(logged) != 0 {
		t.Fatalf("Fetch fetched %d times, waited %v and logged %q; want one fetch, no wait, nothing logged", len(run.attempts), clock.waits, logged)
	}
}

// A fetch whose context ends during a backoff wait — its process stopped — ends with it, running
// no further attempt.
func TestFetchEndsWithItsContextDuringAWait(t *testing.T) {
	clock := newTestClock()
	run := newFailingFetch(t, clock, 0, fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"))
	req := fetchRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	req.Clock = clock
	req.Log = func(string) { cancel() }

	err := Fetch(ctx, run, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch = %v, want it ended with its context", err)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch fetched %d times, want 1", len(run.attempts))
	}
}

// The first fill's retry starts from the empty repository the first attempt found: what a failed
// attempt left in the temporary sibling — a partial pack a transfer cut mid-pack leaves under a
// temporary name, a lock — is removed and logged before the next attempt, as a clone's retry once
// started from an empty directory, and the cache holds none of it.
func TestAFirstFillsRetryStartsWithoutWhatTheFailedAttemptLeft(t *testing.T) {
	clock := newTestClock()
	run := &leavesPartial{recordingRunner: newLocalRunner(t)}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(run.left) != 2 {
		t.Fatalf("the failed attempt left %q, want a partial pack and a lock", run.left)
	}
	for _, path := range run.left {
		if !slices.ContainsFunc(logged, func(line string) bool {
			return strings.HasPrefix(line, "removed the ") && strings.Contains(line, path+", last written ")
		}) {
			t.Errorf("the removal of %s before the retry was not logged: %q", path, logged)
		}
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	for _, name := range []string{filepath.Join("objects", "pack", "tmp_pack_cut"), "packed-refs.lock"} {
		if _, err := os.Lstat(filepath.Join(cache, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the cache holds the failed attempt's %s (%v)", name, err)
		}
	}
	if !slices.Equal(clock.waits, fetchBackoff[:1]) {
		t.Errorf("Fetch waited %v, want one backoff", clock.waits)
	}
}

// leavesPartial fails the first fetch from GitHub as a transfer cut mid-pack does, a 502 after
// writing a partial pack and a lock into the repository it fetches into, and runs every other
// command, the retry included, as the local runner does.
type leavesPartial struct {
	*recordingRunner
	left []string
}

func (r *leavesPartial) Run(ctx context.Context, command Command) (Result, error) {
	if r.left != nil || command.Argv[0] != "git" || !slices.Contains(command.Argv, "fetch") {
		return r.recordingRunner.Run(ctx, command)
	}
	r.left = []string{}
	gitDir := gitDirOf(command.Argv)
	for _, name := range []string{filepath.Join("objects", "pack", "tmp_pack_cut"), "packed-refs.lock"} {
		path := filepath.Join(gitDir, name)
		if err := os.WriteFile(path, []byte("cut\n"), 0o600); err != nil {
			return Result{}, err
		}
		r.left = append(r.left, path)
	}
	return fatal("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: expected flush after ref listing\n"), nil
}

// A fetch request names the log its retries are told to and the clock they wait on; one without
// either is refused before Fetch runs anything or touches the cache's volume.
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
			if err := Fetch(context.Background(), run, req); err == nil || err.Error() != tc.want {
				t.Fatalf("Fetch = %v, want %q", err, tc.want)
			}
			if calls := run.Calls(); len(calls) != 0 {
				t.Errorf("Fetch ran %q before refusing", calls)
			}
			if entries, err := os.ReadDir(req.Root); err != nil || len(entries) != 0 {
				t.Errorf("Fetch left %v (%v) on the cache's volume before refusing", entries, err)
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
// GIT_SSL_CAINFO trusts. So a transfer from GitHub runs as it does in a pod, its one-shot
// credential included, which the stand-in requires over Basic auth as GitHub does, refusing any
// password but token. Each pack request (the protocol v2 `command=fetch` POST) fails the next of
// failures' ways, then the stand-in serves them.
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

// attemptCount is how many transfers reached the stand-in: one authenticated ref advertisement
// each.
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

// gitHubTransfers are the two fills from GitHub that transferWithRetries runs a real git for: the
// repository cache's first fill (Fetch), and an issue pod's fetch into the clone it fills
// (fetchIssuePod), whose failure decides `workspace-init provision`'s exit. Each runs against GitHub
// as the process's environment reaches it, on token, clock and log, and answers the git directory
// it filled. before is how many transfers each makes ahead of the one that fetches the pack: its
// read of GitHub's refs.
var gitHubTransfers = []struct {
	name     string
	before   int
	transfer func(t *testing.T, ctx context.Context, token func() (string, error), clock Clock, log func(string)) (string, error)
}{
	{name: "the repository cache's first fill", before: 1, transfer: func(t *testing.T, ctx context.Context, token func() (string, error), clock Clock, log func(string)) (string, error) {
		req := fetchRequest(t)
		req.Token, req.Clock, req.Log = token, clock, log
		return filepath.Join(req.Root, "acme", "widgets.git"), Fetch(ctx, NewRunner(procgroup.NoReaper, testTimeout, testTools(t)), req)
	}},
	{name: "the issue pod's fill", before: 1, transfer: func(t *testing.T, ctx context.Context, token func() (string, error), clock Clock, log func(string)) (string, error) {
		clone := filepath.Join(t.TempDir(), "widgets")
		runSetup(t, filepath.Dir(clone), "git", "init", "--quiet", clone)
		_, _, _, err := fetchIssuePod(ctx, NewRunner(procgroup.NoReaper, testTimeout, testTools(t)), clone, issuePodRemote(t, token, "", clock, log), true)
		return filepath.Join(clone, ".git"), err
	}},
}

// issuePodRemote is the remote a Kubernetes issue pod's provision opens (ForIssuePod) for
// WIDGETS-42 of acme/widgets, whose recorded default branch is main: token reads its provisioning
// token, cacheURL is the repository cache it streams first, with testCacheToken (none when empty),
// and clock and log are its fill's, whose bound is FetchTimeout's (zero). Its one-shot credential
// is gone when the test ends.
func issuePodRemote(t *testing.T, token func() (string, error), cacheURL string, clock Clock, log func(string)) remote {
	t.Helper()
	cacheToken := ""
	if cacheURL != "" {
		cacheToken = testCacheToken
	}
	source := ForIssuePod(token, t.TempDir(), cacheURL, cacheToken, clock, 0, log)
	if err := source.check(ghrepo.MustParse("acme/widgets")); err != nil {
		t.Fatal(err)
	}
	opened, err := source.open(ghrepo.MustParse("acme/widgets"), "legion/WIDGETS-42", "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.remove() })
	return opened
}

// fillIssueClone fills an issue pod's clone of acme/widgets, as its provision does
// (ensureRepoClone), from source on run, and answers the clone's directory.
func fillIssueClone(t *testing.T, run Runner, clock Clock, source remote) (string, error) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "repos", "github.com", "acme", "widgets")
	return clone, ensureRepoClone(context.Background(), run, clock, clone, GitHubURL(ghrepo.MustParse("acme/widgets")), source)
}

// Against a real git and curl reaching the stand-in as github.com, the failures a production
// transfer met on the pack request — a 408, a transfer cut short, an HTTP/2 stream reset mid-pack —
// are each told transient from what git prints, and the attempt after them fills the repository.
// Every attempt presents the provisioning token. Both the cache's first fill and an issue pod's fill
// hold to it.
func TestFetchRetriesWhatARealGitPrintsForATransientFailure(t *testing.T) {
	for _, how := range gitHubTransfers {
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
			t.Run(how.name+", "+tc.name, func(t *testing.T) {
				newLocalRunner(t)
				github := newGitHubStandIn(t, tc.http2, testInstallationToken, tc.failures...)
				clock := newTestClock()
				var logged []string
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				gitDir, err := how.transfer(t, ctx, staticToken(testInstallationToken), clock, func(line string) { logged = append(logged, line) })
				if err != nil {
					t.Fatalf("the transfer: %v", err)
				}
				if len(logged) != len(tc.logged) {
					t.Fatalf("the transfer logged %q, want %d retries", logged, len(tc.logged))
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
					t.Fatalf("the transfer waited %v, want %v", clock.waits, want)
				}
				if attempts, want := github.attemptCount(), how.before+len(tc.logged)+1; attempts != want {
					t.Fatalf("git made %d attempts, want %d", attempts, want)
				}
				github.tokensPresented(t, testInstallationToken)
				if main, want := strings.TrimSpace(runSetup(t, gitDir, "git", "--git-dir="+gitDir, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, gitDir, "git", "--git-dir="+github.remote, "rev-parse", "main")); main != want {
					t.Errorf("the filled repository's main is %s, not the remote's %s", main, want)
				}
			})
		}
	}
}

// Against the same real git, a 403 on the pack request and a token GitHub refuses each fail the
// transfer on its first attempt: no wait, no retry logged. Each is outside the volume, so an issue
// pod's provision exits 3 for it.
func TestFetchFailsWhatARealGitPrintsForAPermanentFailureAtOnce(t *testing.T) {
	for _, how := range gitHubTransfers {
		for _, tc := range []struct {
			name, accepts string
			failures      []packFailure
			says          string
			// refs is whether the failure meets the fill's read of GitHub's refs, which presents
			// the token before the transfer of the pack does.
			refs bool
		}{
			{name: "a 403 on the pack request", failures: []packFailure{http.StatusForbidden, http.StatusForbidden}, says: "error: RPC failed; HTTP 403 curl 22"},
			{name: "the token refused", accepts: "ghs_another", says: "fatal: Authentication failed for 'https://github.com/acme/widgets/'", refs: true},
		} {
			t.Run(how.name+", "+tc.name, func(t *testing.T) {
				newLocalRunner(t)
				github := newGitHubStandIn(t, false, cmp.Or(tc.accepts, testInstallationToken), tc.failures...)
				clock := newTestClock()
				var logged []string
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()

				_, err := how.transfer(t, ctx, staticToken(testInstallationToken), clock, func(line string) { logged = append(logged, line) })
				if err == nil || !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("the transfer = %v, want git's %q", err, tc.says)
				}
				t.Logf("the transfer failed: %v", err)
				if !errors.Is(err, ErrOutsideVolume) {
					t.Errorf("the transfer = %v, want a failure outside the volume (ErrOutsideVolume)", err)
				}
				want := how.before + 1
				if tc.refs {
					want = 1
				}
				if attempts := github.attemptCount(); attempts != want || len(clock.waits) != 0 || len(logged) != 0 {
					t.Fatalf("git made %d attempts, the transfer waited %v and logged %q; want %d attempts, no wait, nothing logged", attempts, clock.waits, logged, want)
				}
				github.tokensPresented(t, testInstallationToken)
			})
		}
	}
}

// The provisioning token is read as its file stands before every transfer from GitHub, never kept
// from the start: the daemon keeps every live issue pod's provisioning Secret fresh, whatever that
// pod's fill has already done, and the kubelet rewrites the file projected from it in place. Against
// a real git reaching the stand-in as github.com: a token file rewritten while an issue pod's fill
// waits on the repository cache is the one its fetch presents, the stand-in taking that token alone;
// and one rewritten during a retry's backoff is the one the retry presents. A read that fails, the
// first one or a renewal, fails the fetch for a cause outside the issue's volume.
func TestFetchReadsTheProvisioningTokenAsItsFileStandsBeforeEveryTransfer(t *testing.T) {
	t.Run("rewritten during the fill's wait on the repository cache", func(t *testing.T) {
		local := newLocalRunner(t)
		github := newGitHubStandIn(t, false, "ghs_fresh")
		served := &fakeCache{body: cacheTar(t, cacheOf(t, github.remote)), notReady: 2}
		pushRemoteBranch(t, github.remote, "legion/WIDGETS-42", "newer.txt")
		file := tokenFile(t, "ghs_stale")
		clock := &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		source := issuePodRemote(t, tokenFrom(file), served.serve(t), clock, func(string) {})

		if _, err := fillIssueClone(t, local.runner, clock, source); err != nil {
			t.Fatalf("the fill: %v", err)
		}
		if served.healthAsked != 3 || served.tarAsked != 1 {
			t.Fatalf("the fill asked /healthz %d and /tar %d times, want the cache streamed once after the wait", served.healthAsked, served.tarAsked)
		}
		github.tokensPresented(t, "ghs_fresh")
	})
	t.Run("rewritten during a retry's backoff", func(t *testing.T) {
		newLocalRunner(t)
		github := newGitHubStandIn(t, false, "ghs_stale", http.StatusRequestTimeout)
		github.accept("ghs_fresh")
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.Clock = &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := Fetch(ctx, NewRunner(procgroup.NoReaper, testTimeout, testTools(t)), req); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		presented := github.presented()
		fresh := slices.Index(presented, "ghs_fresh")
		if github.attemptCount() != 3 || fresh <= 0 || slices.Contains(presented[:fresh], "ghs_fresh") || slices.Contains(presented[fresh:], "ghs_stale") {
			t.Fatalf("git presented %q over %d transfers, want the stale token on the read of GitHub's HEAD and the first attempt, then the rewritten one alone", presented, github.attemptCount())
		}
	})
	// A read of the token that fails is a cause outside the issue's volume (ErrOutsideVolume):
	// Fetch's own first read, and a renewal before a retry, of a file removed during the backoff.
	t.Run("unreadable when the fetch starts", func(t *testing.T) {
		run := newFailingFetch(t, newTestClock(), 0, Result{})
		req := fetchRequest(t)
		req.Token = tokenFrom(filepath.Join(t.TempDir(), "LEGION_PROVISION_TOKEN"))
		err := Fetch(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "no such file or directory") || !errors.Is(err, ErrOutsideVolume) {
			t.Fatalf("Fetch = %v, want the failed read, outside the volume", err)
		}
		if len(run.attempts) != 0 {
			t.Fatalf("Fetch ran %d transfers, want none", len(run.attempts))
		}
	})
	t.Run("removed during a retry's backoff", func(t *testing.T) {
		clock := newTestClock()
		run := newFailingFetch(t, clock, 0, fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n"))
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.Clock = &removingClock{testClock: clock, file: file}

		err := Fetch(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "fetch attempt 2: read the provisioning token: ") || !errors.Is(err, ErrOutsideVolume) {
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

// tokenFrom is a provisioning token reader that reads file each time it is called.
func tokenFrom(file string) func() (string, error) {
	return func() (string, error) {
		contents, err := os.ReadFile(file)
		return strings.TrimSpace(string(contents)), err
	}
}

// rewritingClock is the test clock that, at its first wait, rewrites file to hold token: what the
// daemon's refresh and the kubelet do to a pod's token file while a transfer waits.
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

// The repository cache's first fill makes an empty bare repository in a temporary sibling of the
// cache and fetches GitHub's default branch alone into it, with the one-shot credential under
// FetchTimeout and no tag, then points HEAD at that branch. It reads no git configuration but its
// own: not the system's, not the image user's, and not what git's `-c` (GIT_CONFIG_PARAMETERS)
// names in its environment — here each names a template directory, which would plant a file in
// every repository git creates, and the system's and the user's also rewrite every file:// URL,
// the tests' GitHub, to one that names nothing, so a transfer that read them fails. Its credential
// is gone once it returns.
func TestFetchFillsTheCacheBareReadingNoConfigurationButItsOwn(t *testing.T) {
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "release/v7.1", "other.txt")
	template := t.TempDir()
	if err := os.WriteFile(filepath.Join(template, "planted"), []byte("from a configuration Fetch must not read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[init]\n\ttemplateDir = "+template+"\n[url \"file:///nowhere/\"]\n\tinsteadOf = file://\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_SYSTEM", config)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'init.templatedir'='"+template+"'")
	req := fetchRequest(t)
	// The clock stands still, so the fetch is given the whole bound it shares with the read before.
	req.Clock = newTestClock()

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	if _, err := os.Stat(filepath.Join(cache, "planted")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Fetch's git read a configuration other than its own: the template planted a file (%v)", err)
	}
	if bare := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "--is-bare-repository")); bare != "true" {
		t.Errorf("the cache is bare: %s", bare)
	}
	if main := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+run.remote, "rev-parse", "main")) {
		t.Errorf("the cache's main is %s, not the remote's", main)
	}
	if refs := runSetup(t, cache, "git", "--git-dir="+cache, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main\n" {
		t.Errorf("the cache holds %q, want the default branch alone", refs)
	}
	if head := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "symbolic-ref", "HEAD")); head != "refs/heads/main" {
		t.Errorf("the cache's HEAD names %s, want refs/heads/main", head)
	}
	initialized := findCall(t, run.Calls(), "git", "init")
	staging := initialized.Argv[len(initialized.Argv)-1]
	if want := slices.Concat(isolatedGit, []string{"init", "--bare", "--quiet"}); !slices.Equal(initialized.Argv[:len(initialized.Argv)-1], want) || !strings.HasPrefix(staging, cache+cloneSuffix) {
		t.Errorf("Fetch ran %q, want %q into a temporary sibling of %s", initialized.Argv, want, cache)
	}
	fetch := findCall(t, run.Calls(), "git", "--git-dir="+staging, "fetch")
	if want := slices.Concat(isolatedGit, []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "--git-dir=" + staging, "fetch", "--no-tags", "--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/main:refs/heads/main"}); !slices.Equal(fetch.Argv, want) {
		t.Errorf("Fetch ran %q, want %q", fetch.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, fetch, FetchTimeout)
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// The repository cache holds the default branch's history and no tag object, so no pod's /tar
// streams one. A bare clone sends include-tag even with --no-tags, and GitHub's pack then carries
// every annotated tag that points into the default branch with no ref naming it. The cache's first
// fill and each refresh fetch the default branch alone with --no-tags, which sends no include-tag:
// here an annotated tag on main before the first fill, and another on main's next commit before a
// refresh, reach the cache in neither.
func TestFetchBringsNoTagObjectIntoTheCache(t *testing.T) {
	run := newLocalRunner(t)
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "tag", "-a", "v1.0", "-m", "a release", "main")
	req := fetchRequest(t)
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	types := func() []string {
		return strings.Fields(runSetup(t, cache, "git", "--git-dir="+cache, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype)"))
	}
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	if objects := types(); !slices.Contains(objects, "commit") || slices.Contains(objects, "tag") {
		t.Errorf("after the first fill the cache holds objects of the types %q, want the default branch's history and no tag", objects)
	}
	pushRemoteBranch(t, run.remote, "main", "release-two.txt")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "tag", "-a", "v2.0", "-m", "another release", "main")
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the refresh: %v", err)
	}
	if objects := types(); slices.Contains(objects, "tag") {
		t.Errorf("after a refresh the cache holds objects of the types %q, want no tag", objects)
	}
}

// A repository cache that holds the repository — the cache's clone at each refresh, or after a
// restart on its volume — is fetched into, not filled again: the default branch GitHub's HEAD
// names, moved, reaches it through the one-shot credential, bounded as the first fill is, without
// tags, and no other branch does. The credential is gone once Fetch returns.
func TestFetchIntoACacheThatHoldsTheRepositoryFetchesItsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	// The clock stands still, so the fetch is given the whole bound it shares with the read before.
	req.Clock = newTestClock()
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	advanceRemote(t, run.remote)
	pushRemoteBranch(t, run.remote, "release/v7.1", "gained.txt")
	before := len(run.Calls())

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	calls := run.Calls()[before:]
	for _, call := range calls {
		if commandWith(call.Argv, "git", "init") {
			t.Fatalf("the second Fetch filled the cache afresh: %q", call.Argv)
		}
	}
	fetch := findCall(t, calls, "git", "--git-dir="+cache, "fetch")
	if want := slices.Concat(isolatedGit, []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "--git-dir=" + cache, "fetch", "--no-tags", "--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/main:refs/heads/main"}); !slices.Equal(fetch.Argv, want) {
		t.Fatalf("Fetch ran %q, want %q", fetch.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, fetch, FetchTimeout)
	if got, want := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
		t.Errorf("the cache's main is %s, want the remote's %s", got, want)
	}
	if refs := runSetup(t, cache, "git", "--git-dir="+cache, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/main\n" {
		t.Errorf("the cache holds %q, want the default branch alone", refs)
	}
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A fetch into a cache that holds the repository points its HEAD at the branch GitHub's HEAD names
// now, read under the same credential and bound as the fetch: GitHub renaming the default branch
// would otherwise leave the cache's HEAD naming a branch the cache's narrowing removes, and the
// cache could publish no head. The move is logged; a fetch with nothing renamed moves nothing.
func TestFetchPointsTheCachesHeadAtGitHubsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "default branch") }) {
		t.Errorf("a fetch with nothing renamed logged a move: %q", logged)
	}
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	before := len(run.Calls())

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after the rename: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	if head := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "symbolic-ref", "HEAD")); head != "refs/heads/trunk" {
		t.Errorf("the cache's HEAD names %s, want the renamed refs/heads/trunk", head)
	}
	if got, want := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/trunk")), strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+run.remote, "rev-parse", "trunk")); got != want {
		t.Errorf("the cache's trunk is %s, want the remote's %s", got, want)
	}
	var read Command
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "git", "--git-dir="+cache, "ls-remote") {
			read = call
		}
	}
	if want := slices.Concat(isolatedGit, []string{"--git-dir=" + cache, "ls-remote", "--symref", "https://github.com/acme/widgets", "HEAD"}); !slices.Equal(read.Argv, want) {
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
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	head := func() string {
		return strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "symbolic-ref", "HEAD"))
	}
	// A fetch into the cache reads GitHub's HEAD twice: before its fetch, for the branch to fetch,
	// and after it, to follow HEAD. GitHub renames its branch between the two.
	reads := 0
	run.beforeLsRemote = func() {
		if reads++; reads == 2 {
			run.beforeLsRemote = nil
			runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
		}
	}

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch GitHub renamed its branch under: %v", err)
	}
	if got := head(); got != "refs/heads/main" {
		t.Errorf("after a fetch that did not bring trunk, the cache's HEAD names %s, want refs/heads/main still", got)
	}
	if want := "GitHub's default branch of acme/widgets is now trunk, which this fetch did not bring: HEAD stays at refs/heads/main until a fetch brings it"; !slices.Contains(logged, want) {
		t.Errorf("the fetch did not say %q: %q", want, logged)
	}

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if got := head(); got != "refs/heads/trunk" {
		t.Errorf("after the fetch that brought trunk, the cache's HEAD names %s, want refs/heads/trunk", got)
	}
}

// A cache repository Fetch did not write is not taken as its own. It could carry a hook,
// configuration or link the token-holding fetch would run or follow, so Fetch runs no git in it:
// it removes it without following a link, says why, and fills the cache afresh in its place. No
// planted hook runs, what a planted link named is left as it was, and the cache Fetch leaves is one
// it wrote.
func TestFetchReplacesACacheItDidNotWriteRunningNothingInIt(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, cache, marker string) (kept string){
		"a hook": func(t *testing.T, cache, marker string) string {
			writeExecutable(t, filepath.Join(cache, "hooks", "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			return ""
		},
		"core.hooksPath": func(t *testing.T, cache, marker string) string {
			hooks := t.TempDir()
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			runSetup(t, cache, "git", "--git-dir="+cache, "config", "core.hooksPath", hooks)
			return hooks
		},
		"an http.proxy": func(t *testing.T, cache, _ string) string {
			runSetup(t, cache, "git", "--git-dir="+cache, "config", "http.proxy", "http://127.0.0.1:1")
			return ""
		},
		"alternates": func(t *testing.T, cache, _ string) string {
			elsewhere := t.TempDir()
			if err := os.WriteFile(filepath.Join(cache, "objects", "info", "alternates"), []byte(elsewhere+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return elsewhere
		},
		"info/grafts": func(t *testing.T, cache, _ string) string {
			if err := os.WriteFile(filepath.Join(cache, "info", "grafts"), []byte("0000000000000000000000000000000000000000\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"a symlinked refs directory": func(t *testing.T, cache, _ string) string {
			elsewhere := filepath.Join(t.TempDir(), "refs")
			if err := os.Rename(filepath.Join(cache, "refs"), elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(cache, "refs")); err != nil {
				t.Fatal(err)
			}
			return elsewhere
		},
		"a gitfile": func(t *testing.T, cache, _ string) string {
			elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
			if err := os.Rename(cache, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cache, []byte("gitdir: "+elsewhere+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return elsewhere
		},
		"a symlinked owner directory": func(t *testing.T, cache, _ string) string {
			owner := filepath.Dir(cache)
			elsewhere := filepath.Join(t.TempDir(), "acme")
			if err := os.Rename(owner, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, owner); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(elsewhere, filepath.Base(cache))
		},
		// git reads a key written on its section header's line, `[section]key=value`, and ends an
		// unquoted value at `;`, so each of these sets what it names. `git config` never writes
		// this form; whoever wrote the file by hand could.
		"a hooks path on a section header's line": func(t *testing.T, cache, marker string) string {
			hooks := filepath.Join(filepath.Dir(cache), "h")
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			appendConfig(t, cache, "[core]hooksPath="+hooks+";]\n")
			return hooks
		},
		"an include on a section header's line": func(t *testing.T, cache, _ string) string {
			included := filepath.Join(filepath.Dir(cache), "c")
			if err := os.WriteFile(included, []byte("[http]\n\tproxy = http://127.0.0.1:1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			appendConfig(t, cache, "[include]path="+included+";]\n")
			return included
		},
		"an http.proxy on a section header's line": func(t *testing.T, cache, _ string) string {
			appendConfig(t, cache, "[http]proxy=http://127.0.0.1:1;]\n")
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			if err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("the first Fetch: %v", err)
			}
			cache := filepath.Join(req.Root, "acme", "widgets.git")
			marker := filepath.Join(t.TempDir(), "the hook ran")
			kept := plant(t, cache, marker)
			advanceRemote(t, run.remote)
			before := len(run.Calls())

			if err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("the Fetch after the plant: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a planted hook ran in the fetch")
			}
			if !slices.ContainsFunc(logged, func(line string) bool {
				return strings.Contains(line, "is not one this fetch wrote") && strings.HasSuffix(line, ": removed; the cache is filled afresh")
			}) {
				t.Errorf("Fetch logged %q, want the planted cache removed and why", logged)
			}
			if calls := run.Calls()[before:]; len(calls) == 0 || !commandWith(calls[0].Argv, "git", "init", "--bare") {
				t.Errorf("the Fetch after the plant ran %q, want its first git the init of a fresh cache, and none in the planted one", calls)
			}
			if kept != "" {
				if _, err := os.Lstat(kept); err != nil {
					t.Errorf("what the planted cache named, %s, after the fetch: %v, want it left as it was", kept, err)
				}
			}
			if why, err := foreignCache(cache); err != nil || why != "" {
				t.Errorf("foreignCache of the cache Fetch left = %q, %v; want one Fetch wrote", why, err)
			}
			if got, want := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
				t.Errorf("the fresh cache's main is %s, want the remote's %s", got, want)
			}
		})
	}
}

// git's update-server-info, which every `git repack` runs (the cache's due repack among them),
// writes info/refs and objects/info/packs through a temporary sibling named <file>_XXXXXX and
// renames it into place; a repack killed in between leaves that file. git never reads it, so a
// cache holding one is still one a fetch wrote: kept, never removed and filled again whole.
func TestFetchKeepsACacheHoldingAKilledRepacksServerInfoFiles(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	for _, leftover := range []string{"info/refs_XCPf7q", "objects/info/packs_cs3EN0"} {
		path := filepath.Join(cache, leftover)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("0000000000000000000000000000000000000000\trefs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := len(run.Calls())

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
		t.Errorf("Fetch refused a cache holding only git's own leftovers: %q", logged)
	}
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "git", "init") {
			t.Errorf("Fetch filled the cache afresh: %q", call.Argv)
		}
	}
}

// A cache can be garbage-collected between fetches. A completed gc and one killed while its repack
// is running leave only files git owns, so the next fetch keeps the cache instead of replacing it.
// A gc killed earlier, while it packs refs, also leaves packed-refs.lock: a lock makes no cache
// foreign, and Fetch removes it once it takes the cache, so it keeps that cache too.
func TestFetchKeepsACacheAfterGitGC(t *testing.T) {
	for _, tc := range []struct {
		name string
		gc   func(t *testing.T, cache string)
	}{
		{name: "completed", gc: func(t *testing.T, cache string) {
			runSetup(t, cache, "git", "--git-dir="+cache, "gc", "--prune=now")
		}},
		{name: "killed during repack", gc: killGitGC},
		{name: "killed during pack-refs", gc: func(t *testing.T, cache string) {
			// git gc writes gc.pid, then packs refs under packed-refs.lock before it repacks; a
			// process that ends there leaves both behind.
			if err := os.WriteFile(filepath.Join(cache, "gc.pid"), []byte("4242 killed-pod"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache, "packed-refs.lock"), []byte("# pack-refs with: peeled fully-peeled sorted \n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			if err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("first Fetch: %v", err)
			}
			cache := filepath.Join(req.Root, "acme", "widgets.git")
			tc.gc(t, cache)
			if why, err := foreignCache(cache); err != nil || why != "" {
				t.Fatalf("foreignCache after git gc = %q, %v; want the cache trusted", why, err)
			}
			before := len(run.Calls())

			if err := Fetch(context.Background(), run, req); err != nil {
				t.Fatalf("Fetch after git gc: %v", err)
			}
			if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
				t.Errorf("Fetch refused a cache after git gc: %q", logged)
			}
			for _, call := range run.Calls()[before:] {
				if commandWith(call.Argv, "git", "init") {
					t.Errorf("Fetch replaced the cache after git gc: %q", call.Argv)
				}
			}
			if _, err := os.Lstat(filepath.Join(cache, "packed-refs.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Fetch left the killed gc's packed-refs.lock: %v", err)
			}
		})
	}
}

// killGitGC runs a large, real repack and kills its process group once the repack is writing its
// pack, a tmp_pack_* entry under objects/pack. The kill is deliberate: a process can end at this
// point, leaving gc.pid and the partial pack. git gc writes gc.pid before it packs refs and expires
// the reflog, both of which come before the repack, so a kill at gc.pid could land in pack-refs
// instead; that kill is the "killed during pack-refs" case. The gc runs under the fixture's
// environment, so no git configuration of the process running the tests reaches the repack, and
// the helper returns only once the kill, and not a failure of git's own, ended that gc with its
// pack half written.
func killGitGC(t *testing.T, cache string) {
	t.Helper()
	slowGitGC(t, cache)
	command := exec.Command("/usr/bin/git", "--git-dir="+cache, "gc", "--prune=now")
	command.Dir = cache
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
		if writing, _ := writingAPack(t, cache); writing {
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill git gc: %v", err)
			}
			var ended *exec.ExitError
			if err := command.Wait(); !errors.As(err, &ended) {
				t.Fatalf("git gc exited cleanly after its process group was killed: %v", err)
			} else if status, ok := ended.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("git gc ended on its own, not on the kill: %v", err)
			}
			if left, packs := writingAPack(t, cache); !left {
				t.Fatalf("git gc finished its pack before the kill reached it: the cache holds %q, not a repack's partial pack", packs)
			}
			return
		}
		select {
		case <-deadline.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			_ = command.Wait()
			_, packs := writingAPack(t, cache)
			t.Fatalf("git gc did not start writing a pack during its repack: the cache holds %q", packs)
		case <-poll.C:
		}
	}
}

// writingAPack answers whether the cache's repack is writing its pack, a tmp_pack_* entry under
// objects/pack: the file pack-objects streams a pack into before it renames it into place, which
// stays behind when the repack is killed. It answers what that directory holds either way, so a
// wait that ends in neither state says what it saw instead.
func writingAPack(t *testing.T, cache string) (bool, []string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cache, "objects", "pack"))
	if err != nil {
		t.Fatalf("read the cache's packs during git gc: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, "tmp_pack_") }), names
}

// slowGitGC gives the cache a large incompressible object the repack must write into its pack, so
// that the pack write lasts about a second and the wait above meets it. A ref holds the object:
// `git gc --prune=now` repacks what the cache's refs reach and prunes the rest, so an object no ref
// reaches never enters the pack at all. Without one, the repack wrote the whole pack, 194 bytes, in
// 11 milliseconds: a window a wait polling every millisecond held only by luck, and lost on a
// loaded runner, where the whole repack ran between two polls of a starved test.
func slowGitGC(t *testing.T, cache string) {
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
	object := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "hash-object", "-w", blob.Name()))
	runSetup(t, cache, "git", "--git-dir="+cache, "update-ref", "refs/large-blob", object)
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

// A fetch killed mid-update leaves git's lock files in the cache, which outlives it on the cache's
// volume, and a transfer killed mid-pack leaves the pack it was receiving under a temporary name:
// the next Fetch, the cache's only writer, removes each and logs it with its age before it fetches,
// so the cache is never blocked for good and carries no pack that never arrived, and every object
// it already held is kept.
func TestFetchRemovesTheLocksAnInterruptedFetchLeftInTheCache(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	objects := strings.Fields(runSetup(t, cache, "git", "--git-dir="+cache, "rev-list", "--objects", "--no-object-names", "--all"))
	locks := []string{"refs/heads/main.lock", "packed-refs.lock", "HEAD.lock", "shallow.lock"}
	for _, lock := range locks {
		if err := os.WriteFile(filepath.Join(cache, lock), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	partial := filepath.Join(cache, "objects", "pack", "tmp_pack_Ab12Cd")
	if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := pushRemoteBranch(t, run.remote, "main", "after-the-interruption.txt")
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after an interrupted one: %v", err)
	}
	if got := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/main")); got != moved {
		t.Errorf("the cache's main is %s, want the remote's %s", got, moved)
	}
	for _, lock := range locks {
		if _, err := os.Stat(filepath.Join(cache, lock)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still in the cache (%v)", lock, err)
		}
		if !slices.ContainsFunc(logged, func(line string) bool {
			return strings.HasPrefix(line, "removed the stale git lock "+filepath.Join(cache, lock)+", last written ")
		}) {
			t.Errorf("the removal of %s was not logged: %q", lock, logged)
		}
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partial pack is still in the cache (%v)", err)
	}
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.HasPrefix(line, "removed the partial pack "+partial+", last written ")
	}) {
		t.Errorf("the partial pack's removal was not logged: %q", logged)
	}
	for _, object := range objects {
		// The probe reads the cache the fetch left, under the package's fixture environment, so
		// the home it reads is the one the code under test and every other fixture read.
		held := exec.Command("git", "--git-dir="+cache, "cat-file", "-e", object)
		held.Dir = cache
		held.Env = setupEnvironment()
		if err := held.Run(); err != nil {
			t.Errorf("the cache lost %s, an object it held", object)
		}
	}
}

// A first fill that dies mid-transfer leaves nothing at the cache's repository: the fill runs in a
// temporary sibling renamed into place once git finished, so a volume that outlives the process
// never holds a half-filled repository the next Fetch would take for a whole one and fetch into. A
// sibling a Fetch killed outright left (no defer ran) is removed by the next one, which fills the
// cache whole. An issue pod's fill stages its clone the same way (stageBeside).
func TestFetchLeavesNoHalfFilledRepositoryAtTheCache(t *testing.T) {
	req := fetchRequest(t)
	run := newLocalRunner(t)
	killed := killedFetch{Runner: run}
	if err := Fetch(context.Background(), &killed, req); err == nil || !strings.Contains(err.Error(), "killed mid-transfer") {
		t.Fatalf("Fetch = %v, want the killed fetch's failure", err)
	}
	if killed.gitDir == "" {
		t.Fatal("the stand-in saw no fetch")
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	if killed.gitDir == cache {
		t.Fatalf("Fetch filled %s in place", cache)
	}
	for _, path := range []string{cache, killed.gitDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the killed fetch left %s behind (%v)", path, err)
		}
	}

	stale := cache + cloneSuffix + "12345"
	if err := os.MkdirAll(filepath.Join(stale, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the next Fetch left the killed one's %s (%v)", stale, err)
	}
	if want := []string{"removed the clone an earlier fetch left at " + stale}; !slices.Equal(logged, want) {
		t.Errorf("the next Fetch logged %q, want %q", logged, want)
	}
	if main := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+run.remote, "rev-parse", "main")) {
		t.Errorf("the cache's main is %s, not the remote's", main)
	}
}

// killedFetch runs every command through Runner but a fetch from GitHub, which it fails as a fetch
// the kernel killed mid-transfer would leave it: a partial pack in the repository it fetched into,
// status 137.
type killedFetch struct {
	Runner
	gitDir string
}

func (r *killedFetch) Run(ctx context.Context, command Command) (Result, error) {
	if command.Argv[0] != "git" || !slices.Contains(command.Argv, "fetch") {
		return r.Runner.Run(ctx, command)
	}
	r.gitDir = gitDirOf(command.Argv)
	if err := os.WriteFile(filepath.Join(r.gitDir, "objects", "pack", "tmp_pack_killed"), []byte("PACK half a pack\n"), 0o600); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: 137, Stderr: "killed mid-transfer"}, nil
}

// gitDirOf is the repository a git command line names with --git-dir.
func gitDirOf(argv []string) string {
	for _, arg := range argv {
		if gitDir, ok := strings.CutPrefix(arg, "--git-dir="); ok {
			return gitDir
		}
	}
	return ""
}

// A transfer a signal ended after git printed the provisioning token, as a `fatal: unable to
// access` line naming the URL with its credential does, returns that line with the token scrubbed:
// what a killed run printed goes under its error only in checkedResult, after the transfer has
// scrubbed the Result's stderr, never inside the runner, whose error transferWithRetries does not
// scrub. The stand-in git prints the token it was handed and kills itself on a transfer from
// GitHub (the one command that names the credential's file), and runs the real git for every other
// command.
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
	run := NewRunner(procgroup.NoReaper, time.Minute, map[string]string{"git": fakeGit})

	err = Fetch(context.Background(), run, fetchRequest(t))

	if err == nil {
		t.Fatal("Fetch succeeded through a transfer a signal ended")
	}
	if strings.Contains(err.Error(), testInstallationToken) {
		t.Errorf("Fetch's error carries the provisioning token: %v", err)
	}
	if !strings.Contains(err.Error(), ": signal: killed\nfatal: unable to access 'https://x-access-token:***@github.com/acme/widgets/'") {
		t.Errorf("Fetch = %v, want the transfer's signal and its line with the token scrubbed", err)
	}
}

// The cache's configuration keeps automatic maintenance from competing with its fetch and repack,
// and keeps each fetched object in a pack for the cache to stream: a fetch into it leaves no loose
// object.
func TestFetchTurnsTheCachesGarbageCollectionOffAndKeepsPacks(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cache := filepath.Join(req.Root, "acme", "widgets.git")
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false", "fetch.unpackLimit": "1"} {
		if got := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "config", "--get", key)); got != want {
			t.Errorf("the cache's %s is %q, want %q", key, got, want)
		}
	}
	runSetup(t, cache, "git", "--git-dir="+cache, "repack", "-a", "-d", "--quiet")
	pushRemoteBranch(t, run.remote, "main", "one-small-change.txt")
	if err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if loose := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "count-objects")); !strings.HasPrefix(loose, "0 objects") {
		t.Errorf("the fetch into the cache left %s", loose)
	}
}

// What of a bare repository the cache streams and an issue pod's clone takes: its refs, its packs
// and the index git reads each pack through, nothing the clone rebuilds itself from them (a
// reverse index, a bitmap, a commit-graph), no loose object, nothing of its own, nothing that would
// point it at objects elsewhere, and nothing git is still writing.
func TestCacheEntryIsTheRepositorysRefsAndPacksAlone(t *testing.T) {
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
		if got := CacheEntry(tc.name, tc.dir); got != tc.want {
			t.Errorf("CacheEntry(%q, dir %t) = %t, want %t", tc.name, tc.dir, got, tc.want)
		}
	}
}

// cacheOf is the repository cache's clone of remote as the cache keeps it: bare, holding the
// default branch alone in one pack, as a clone over git's transport keeps it.
func cacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "clone", "--bare", "--single-branch", "--no-tags", "--quiet", "--no-local", remote, cache)
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
	writeIndex(t, pack, body)
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

// writeIndex replaces the pack file or pack index at path, which git wrote read-only.
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
// and refs (CacheEntry), each a directory or a regular file.
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
		if !CacheEntry(name, entry.IsDir()) {
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

// testCacheToken is the bearer the fake cache takes and a test's cache stream presents.
const testCacheToken = "cache-token-of-acme-widgets"

// fakeCache stands in for the repository cache's server: GET /tar must carry the bearer
// testCacheToken, or it is answered 401; GET /healthz answers any request: 503 for its first
// notReady polls, then 200 ready without defaultBranchOnly for wide more, as a cache of an earlier
// release that serves every branch does, and then 200 ready with defaultBranchOnly, recording the
// Authorization each carried. An answered request names the cache's repository, acme/widgets
// unless repo or tarRepo says otherwise, in CacheRepositoryHeader. It counts each.
type fakeCache struct {
	body           []byte
	notReady, wide int
	repo, tarRepo  string

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
		switch {
		case c.healthAsked <= c.notReady:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false,"repo":"` + repo + `","bytes":0}`))
		case c.healthAsked-c.notReady <= c.wide:
			_, _ = w.Write([]byte(`{"ready":true,"repo":"` + repo + `","bytes":1}`))
		default:
			_, _ = w.Write([]byte(`{"ready":true,"defaultBranchOnly":true,"repo":"` + repo + `","bytes":1}`))
		}
	case r.Method == http.MethodGet && r.URL.Path == "/tar":
		c.tarAsked++
		if r.Header.Get("Authorization") != "Bearer "+testCacheToken {
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

// cacheStream is an issue pod's stream of acme/widgets from the repository cache at url, with
// testCacheToken, on clock and log.
func cacheStream(url string, clock Clock, log func(string)) CacheStream {
	return CacheStream{Repo: ghrepo.MustParse("acme/widgets"), URL: url, Token: testCacheToken, Log: log, Clock: clock}
}

// assertNoGitHubTransfer fails when run reached GitHub: a clone, a fetch or a read of GitHub's refs.
func assertNoGitHubTransfer(t *testing.T, run *recordingRunner) {
	t.Helper()
	for _, call := range run.Calls() {
		if slices.Contains(call.Argv, GitHubURL(ghrepo.MustParse("acme/widgets"))) {
			t.Errorf("the stream reached GitHub: %q", call.Argv)
		}
	}
}

// While the repository cache is not ready, or answers ready without saying it serves the default
// branch alone (an earlier release's cache, which serves every branch), an issue pod's stream waits
// on it, asking its /healthz every CachePoll and logging what it answered whenever that changes,
// then streams it once it is ready and narrowed. Nothing reaches GitHub meanwhile. The health polls
// carry no credential: the cache's token goes with the tar alone. The index the cache streamed with
// its pack is the one the clone reads it through: no pack is indexed again.
func TestStreamCacheWaitsForTheRepositoryCacheToBeReady(t *testing.T) {
	run := newLocalRunner(t)
	body := cacheTar(t, cacheOf(t, run.remote))
	served := &fakeCache{body: body, notReady: 2, wide: 1}
	url := served.serve(t)
	clock := newTestClock()
	var logged []string
	gitDir := filepath.Join(t.TempDir(), "widgets.git")

	report, err := StreamCache(context.Background(), run, cacheStream(url, clock, func(line string) { logged = append(logged, line) }), gitDir)
	if err != nil {
		t.Fatalf("StreamCache: %v", err)
	}
	if report.Bytes != int64(len(body)) || served.tarAsked != 1 || served.healthAsked != 4 {
		t.Fatalf("StreamCache reported %+v after %d health polls and %d tars, want the tar's %d bytes streamed once after 4 polls", report, served.healthAsked, served.tarAsked, len(body))
	}
	if want := []time.Duration{CachePoll, CachePoll, CachePoll}; !slices.Equal(clock.waits, want) {
		t.Errorf("StreamCache waited %v between polls, want %v", clock.waits, want)
	}
	if len(served.healthCredentials) != 0 {
		t.Errorf("StreamCache's health polls carried %q, want no credential", served.healthCredentials)
	}
	waiting := "waiting for the repository cache " + url + " to be ready: GET " + url + "/healthz answered "
	if want := []string{waiting + "503 Service Unavailable", waiting + "ready without defaultBranchOnly"}; !slices.Equal(logged, want) {
		t.Errorf("StreamCache logged %q, want each answer that kept it waiting once: %q", logged, want)
	}
	if got, want := strings.TrimSpace(runSetup(t, gitDir, "git", "--git-dir="+gitDir, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, gitDir, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
		t.Errorf("the streamed main is %s, want the remote's %s", got, want)
	}
	assertNoGitHubTransfer(t, run)
	for _, call := range run.Calls() {
		if slices.Contains(call.Argv, "index-pack") {
			t.Errorf("StreamCache indexed again a pack the cache streamed its index with: %q", call.Argv)
		}
	}
}

// A cache that never becomes ready within CacheWaitTimeout — answering not ready, answering ready
// but never narrowed to the default branch, or not answering at all — fails the stream, naming the
// cache and what it last answered: there is no full fetch from GitHub instead, so a cache that is
// down never costs each new issue a full download. The wait's failure is outside the issue's
// volume, and nothing is written there.
func TestStreamCacheFailsWhenTheRepositoryCacheIsNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(t *testing.T) string
		says string
	}{
		{"a cache answering not ready", func(t *testing.T) string { return (&fakeCache{notReady: 1 << 30}).serve(t) }, "503 Service Unavailable"},
		{"a cache that is never narrowed", func(t *testing.T) string { return (&fakeCache{wide: 1 << 30}).serve(t) }, "answered ready without defaultBranchOnly"},
		{"a cache that does not answer", func(t *testing.T) string { return "http://" + testport.Refused(t) }, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			url := tc.url(t)
			gitDir := filepath.Join(t.TempDir(), "widgets.git")
			_, err := StreamCache(context.Background(), run, cacheStream(url, newTestClock(), func(string) {}), gitDir)
			if err == nil || !strings.Contains(err.Error(), "the repository cache "+url+" was not ready within "+CacheWaitTimeout.String()) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("StreamCache = %v, want the wait's failure naming %q", err, tc.says)
			}
			if !errors.Is(err, ErrOutsideVolume) {
				t.Errorf("StreamCache = %v, want a failure outside the volume (ErrOutsideVolume)", err)
			}
			if calls := run.Calls(); len(calls) != 0 {
				t.Errorf("StreamCache ran %q, want nothing run", calls)
			}
			if _, err := os.Lstat(gitDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("StreamCache made %s (%v), want nothing written", gitDir, err)
			}
		})
	}
}

// A cache whose tar holds an entry a clone does not take — one whose name leaves the clone's git
// directory, one that is not a regular file or directory, the cache's own configuration, an
// alternates file, a loose object — or whose history does not verify — a pack with a damaged
// object, a pack index damaged or belonging to another pack, a history missing an object it
// reaches — fails an issue pod's fill, naming the cache and the entry or the check: nothing is
// fetched from GitHub, and nothing is left where the clone or its temporary sibling would be. None
// of these failures is a failed write: each is an entry unpackEntry refuses by its name or its
// type, or a failure of verifyCache, so none reads as a failure outside the issue's volume, and the
// provision exits 1.
func TestStreamCacheRefusesARepositoryCacheItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(t *testing.T, remote string) []byte
		says string
	}{
		{"an entry leaving the git directory", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"HEAD", "ref: refs/heads/main\n"}, [2]string{"../x", "escaped\n"})
		}, `the repository cache's tar holds "../x", which is not a path inside the clone's git directory`},
		{"a symbolic link", func(t *testing.T, _ string) []byte { return tarOf(t, [2]string{"refs/heads/main@"}) },
			`the repository cache's tar holds "refs/heads/main", which is neither a regular file nor a directory`},
		{"the cache's configuration", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"config", "[core]\n\tfsmonitor = true\n"})
		}, `the repository cache's tar holds "config", which is not one of a repository's refs or packs`},
		{"an alternates file", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"objects/info/alternates", "/elsewhere\n"})
		}, `the repository cache's tar holds "objects/info/alternates", which is not one of a repository's refs or packs`},
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
			url := (&fakeCache{body: tc.body(t, run.remote)}).serve(t)
			clock := newTestClock()

			clone, err := fillIssueClone(t, run, clock, issuePodRemote(t, staticToken(testInstallationToken), url, clock, func(string) {}))
			if err == nil || !strings.Contains(err.Error(), "fill the clone from the repository cache "+url+": ") || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the fill = %v, want the cache refused naming %q", err, tc.says)
			}
			if errors.Is(err, ErrOutsideVolume) {
				t.Errorf("the fill = %v, want a failure of what the cache streamed, not one outside the volume (ErrOutsideVolume)", err)
			}
			assertNoGitHubTransfer(t, run)
			if entries, err := os.ReadDir(filepath.Dir(clone)); err != nil || len(entries) != 0 {
				t.Errorf("the fill left %v (%v) where the clone goes, want nothing", entries, err)
			}
		})
	}
}

// A stream asks only its own repository's cache, which it proves twice: it takes nothing from a
// cache whose answer names another repository — its /healthz, or the /tar itself, read before any
// of the tar's content — and it presents that cache's token for the tar, which another
// repository's cache refuses with 401. Each is refused at once, without waiting, and nothing is
// unpacked. A cache named without its token is refused before anything is asked. Each refusal is
// outside the issue's volume, and names the cache.
func TestStreamCacheTakesNothingFromAnotherRepositorysCache(t *testing.T) {
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
			url := tc.cache.serve(t)
			clock := newTestClock()
			stream := cacheStream(url, clock, func(string) {})
			stream.Token = tc.token
			gitDir := filepath.Join(t.TempDir(), "widgets.git")

			_, err := StreamCache(context.Background(), run, stream, gitDir)
			if err == nil || !strings.Contains(err.Error(), url) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("StreamCache = %v, want a refusal naming %q", err, tc.says)
			}
			if !errors.Is(err, ErrOutsideVolume) {
				t.Errorf("StreamCache = %v, want a refusal outside the volume (ErrOutsideVolume)", err)
			}
			if tc.cache.healthAsked != tc.health || tc.cache.tarAsked != tc.tars || len(clock.waits) != 0 {
				t.Errorf("StreamCache asked /healthz %d and /tar %d times and waited %v, want %d, %d and no wait", tc.cache.healthAsked, tc.cache.tarAsked, clock.waits, tc.health, tc.tars)
			}
			if packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*")); err != nil || len(packs) != 0 {
				t.Errorf("StreamCache unpacked %q (%v), want nothing", packs, err)
			}
			assertNoGitHubTransfer(t, run)
		})
	}

	served := &fakeCache{}
	stream := cacheStream(served.serve(t), newTestClock(), func(string) {})
	stream.Token = ""
	_, err := StreamCache(context.Background(), newLocalRunner(t), stream, filepath.Join(t.TempDir(), "widgets.git"))
	if err == nil || err.Error() != "workspace repository cache token is required" {
		t.Fatalf("StreamCache with a cache and no token = %v", err)
	}
	if !errors.Is(err, ErrOutsideVolume) {
		t.Errorf("StreamCache with a cache and no token = %v, want a refusal outside the volume (ErrOutsideVolume)", err)
	}
	if served.healthAsked+served.tarAsked != 0 {
		t.Errorf("StreamCache with no token asked the cache %d times, want none", served.healthAsked+served.tarAsked)
	}
}

// A pack the cache streamed without its index — a cache on a release that never streamed one — is
// indexed in the pod, as every pack was before, and the history is whole.
func TestAPackWithoutItsIndexIsIndexedInThePod(t *testing.T) {
	run := newLocalRunner(t)
	cache := cacheOf(t, run.remote)
	if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(t.TempDir(), "widgets.git")
	if _, err := StreamCache(context.Background(), run, cacheStream((&fakeCache{body: cacheTar(t, cache)}).serve(t), newTestClock(), func(string) {}), gitDir); err != nil {
		t.Fatalf("StreamCache: %v", err)
	}
	indexed := 0
	for _, call := range run.Calls() {
		if slices.Contains(call.Argv, "index-pack") {
			indexed++
		}
	}
	if indexed != 1 {
		t.Errorf("StreamCache indexed %d packs, want the one the cache streamed without its index", indexed)
	}
	runSetup(t, gitDir, "git", "--git-dir="+gitDir, "fsck", "--connectivity-only")
}

// An issue pod's fill gives the verification of what the cache streamed one FetchTimeout, counted
// from the stream's end, the delta from GitHub one of its own, and the Jujutsu steps after it one
// CloneTimeout of their own, so its waits and its clocked stages before the Jujutsu steps end
// within FetchBudget however long it waited; the stream between them holds the low-speed bound and
// no clock at all (CacheStreamLowSpeedBytes). A cache ready 35 minutes in, whose history takes 25
// minutes to check, gives the delta its whole bound, and the clone is filled. So does a cache ready
// on the wait's last ask, 129 min 50 s in, whose history takes that whole bound: before the delta
// had a bound of its own, a fill died there and its replacement streamed the cache again
// (sjawhar/legion#2235). A delta that takes its whole bound leaves the Jujutsu import its own. A pack
// whose indexing takes the whole verification bound fails the fill before GitHub is reached, and a
// Jujutsu import that outlives CloneTimeout is killed there. Neither leaves a clone. The cache's
// bucket has a bound of its own too: a cache ready on the wait's last ask whose bucket's history
// takes past the bucket attempt's whole BucketFetchTimeout to check, which ends it there, leaves
// /tar's verification its whole FetchTimeout, which it takes, and the delta its whole own bound;
// the clone comes from /tar, the fill saying the bucket's check failed. Each other row's cache
// serves no /packs, which costs no clock time, and the fill says so.
func TestTheFillGivesTheCachesVerificationAndTheDeltaTheirOwnBounds(t *testing.T) {
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
		// reason is how the reason of the fill's source line begins, for a fill that succeeds.
		reason string
	}{
		{
			name: "a cache ready 35 minutes in", notReady: polls(35 * time.Minute),
			took:     map[string]time.Duration{"fsck": 25 * time.Minute},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}, "jj git init": {CloneTimeout}},
			elapsed:  35*time.Minute + 25*time.Minute,
			reason:   reasonNotServed,
		},
		{
			name: "a history that takes the whole first bound to check", notReady: polls(CacheWaitTimeout) - 1,
			took:     map[string]time.Duration{"fsck": FetchTimeout},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}, "jj git init": {CloneTimeout}},
			elapsed:  CacheWaitTimeout - CachePoll + FetchTimeout,
			reason:   reasonNotServed,
		},
		{
			name: "a bucket attempt that runs its whole bound, then a tar history that takes its whole own", notReady: polls(CacheWaitTimeout) - 1, bucket: true,
			took:     map[string]time.Duration{"bucket fsck": BucketFetchTimeout + time.Second, "fsck": FetchTimeout},
			timeouts: map[string][]time.Duration{"bucket fsck": {BucketFetchTimeout}, "fsck": {FetchTimeout}, "fetch": {FetchTimeout}, "jj git init": {CloneTimeout}},
			elapsed:  CacheWaitTimeout - CachePoll + BucketFetchTimeout + FetchTimeout,
			reason:   reasonGit,
		},
		{
			name: "a pack whose indexing takes the whole first bound", notReady: 0,
			took:     map[string]time.Duration{"index-pack": FetchTimeout},
			timeouts: map[string][]time.Duration{"index-pack": {FetchTimeout}},
			fails:    "the cache stream's " + FetchTimeout.String() + " bound ran out before checking the repository cache's history is whole",
			elapsed:  FetchTimeout,
		},
		{
			name: "a delta that takes its whole own bound", notReady: 0,
			took:     map[string]time.Duration{"fetch": FetchTimeout},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}, "jj git init": {CloneTimeout}},
			elapsed:  FetchTimeout,
			reason:   reasonNotServed,
		},
		{
			name: "a Jujutsu import that outlives its bound", notReady: 0,
			took:     map[string]time.Duration{"jj git init": CloneTimeout + time.Minute},
			timeouts: map[string][]time.Duration{"fsck": {FetchTimeout}, "fetch": {FetchTimeout}, "jj git init": {CloneTimeout}},
			fails:    "command timed out: jj git init",
			elapsed:  CloneTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "issue.txt")
			clock := newTestClock()
			start := clock.Now()
			cache := cacheOf(t, run.remote)
			if _, indexes := tc.took["index-pack"]; indexes {
				if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
					t.Fatal(err)
				}
			}
			step, url := fillStep, ""
			if tc.bucket {
				step, url = bucketFirst(fillStep), newPacksCache(t, cache, tc.notReady).serve(t, "")
			} else {
				url = (&fakeCache{body: cacheTar(t, cache), notReady: tc.notReady}).serve(t)
			}
			slow := &slowSteps{recordingRunner: run, clock: clock, step: step, took: tc.took, timeouts: map[string][]time.Duration{}}
			var logged []string

			clone, err := fillIssueClone(t, slow, clock, issuePodRemote(t, staticToken(testInstallationToken), url, clock, func(line string) { logged = append(logged, line) }))
			if tc.fails == "" && err != nil {
				t.Fatalf("the fill: %v", err)
			}
			if tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Fatalf("the fill = %v; want it failed: %q", err, tc.fails)
			}
			if tc.fails == "" && !slices.ContainsFunc(logged, func(line string) bool { return strings.HasPrefix(line, "source=/tar reason="+tc.reason) }) {
				t.Errorf("the fill logged %q, want /tar as its source because %q", logged, tc.reason)
			}
			if !maps.EqualFunc(slow.timeouts, tc.timeouts, slices.Equal[[]time.Duration]) {
				t.Errorf("the fill's commands were given %v, want %v: each stage's own bound, from when it began", slow.timeouts, tc.timeouts)
			}
			if elapsed := clock.Now().Sub(start); elapsed != tc.elapsed || elapsed > FetchBudget+CloneTimeout {
				t.Errorf("the fill ran %s, want %s, within FetchBudget %s and the Jujutsu steps' CloneTimeout", elapsed, tc.elapsed, FetchBudget)
			}
			if tc.fails != "" {
				if entries, err := os.ReadDir(filepath.Dir(clone)); err != nil || len(entries) != 0 {
					t.Errorf("the failed fill left %v (%v) where the clone goes, want nothing", entries, err)
				}
			} else if _, err := os.Stat(filepath.Join(clone, ".jj")); err != nil {
				t.Errorf("the fill left no clone: %v", err)
			}
		})
	}
}

// slowSteps is the local runner with the clock time each step a test names takes: step names the
// command's step, "" for one that takes no clock time; each named step records the timeout it was
// given, then takes took of the clock's time — or, given less than that, runs to its timeout and is
// killed there — before it runs.
type slowSteps struct {
	*recordingRunner
	clock    *testClock
	step     func(argv []string) string
	took     map[string]time.Duration
	timeouts map[string][]time.Duration
}

func (r *slowSteps) Run(ctx context.Context, command Command) (Result, error) {
	step := r.step(command.Argv)
	if step == "" {
		return r.recordingRunner.Run(ctx, command)
	}
	r.timeouts[step] = append(r.timeouts[step], command.Timeout)
	took := r.took[step]
	if command.Timeout < took {
		r.clock.advance(command.Timeout)
		return Result{ExitCode: -1, TimedOut: true}, nil
	}
	r.clock.advance(took)
	return r.recordingRunner.Run(ctx, command)
}

// bucketFirst is step with the first `git fsck` it names called "bucket fsck": a fill from a cache
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

// fillStep names the steps of an issue pod's fill whose duration follows the repository's size:
// each pack's indexing, the check of the history the cache streamed, the delta from GitHub, and
// jj's import of every commit.
func fillStep(argv []string) string {
	switch {
	case slices.Contains(argv, "index-pack"):
		return "index-pack"
	case slices.Contains(argv, "fsck"):
		return "fsck"
	case argv[0] == "git" && slices.Contains(argv, "fetch"):
		return "fetch"
	case commandWith(argv, "jj", "git", "init"):
		return "jj git init"
	}
	return ""
}

// Every git an issue pod's fill starts on its clone's objects, every git the repository cache's
// Fetch starts, and every git the repository cache runs on its clone through CacheGit and
// CacheGitWithin, maps packs in small windows from its own command line: it reads no system file,
// its runner removes the role environment's pairs, and the cache's configuration may hold none of
// the four settings. A recording git first on PATH, the git the runner resolves as boot does, reads
// what each of those commands sees of the four with `git config --show-scope --get-regexp` under
// that command's `-c` pairs, its repository and its environment, then runs the real git. Each reads
// core.packedGitWindowSize 32m, core.packedGitLimit 256m, pack.windowMemory 64m and
// core.deltaBaseCacheLimit 64m, every line of them from the command scope: a fill from a repository
// cache whose pack came without its index, so it indexes, checks, reads GitHub's refs, fetches the
// delta and sets the clone's refs; the cache's first Fetch, which fills it; and the cache's read of
// its head and its repack. The fill's `git init` and its `git config` writes of origin open no
// pack, and are not held to it. The repack here is given `-c` pairs of a caller's own, which come
// after the four and win for its command: 64m windows, a 512m limit and 256m of window memory.
// Every git the runner started is recorded.
func TestTheCachesAndTheFillsGitMapPacksInSmallWindows(t *testing.T) {
	windows := map[string]string{}
	for _, setting := range packWindows {
		windows[strings.ToLower(setting[0])] = setting[1]
	}
	// callerPairs are a caller's own `-c` pairs, as a caller hands them to CacheGitWithin, and
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
		{"the fill from the repository cache", func(t *testing.T, run *recordingRunner) func() error {
			cache := cacheOf(t, run.remote)
			if err := os.Remove(strings.TrimSuffix(packOf(t, cache), ".pack") + ".idx"); err != nil {
				t.Fatal(err)
			}
			url := (&fakeCache{body: cacheTar(t, cache)}).serve(t)
			pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "newer.txt")
			clock := newTestClock()
			source := issuePodRemote(t, staticToken(testInstallationToken), url, clock, func(string) {})
			return func() error { _, err := fillIssueClone(t, run, clock, source); return err }
		}},
		{"the cache's Fetch from GitHub", func(t *testing.T, run *recordingRunner) func() error {
			req := fetchRequest(t)
			return func() error { return Fetch(context.Background(), run, req) }
		}},
		{"the repository cache's own git and its repack", func(t *testing.T, run *recordingRunner) func() error {
			cache := cacheOf(t, run.remote)
			return func() error {
				if _, err := CacheGit(context.Background(), run, cache, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
					return err
				}
				_, err := CacheGitWithin(context.Background(), run, cache, testTimeout, append(slices.Clone(callerPairs), "repack", "-a", "-d", "--quiet")...)
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
			held := 0
			for _, line := range recorded {
				argv, reads, _ := strings.Cut(line, "\t")
				if !strings.HasPrefix(argv, strings.Join(isolatedGit[1:], " ")) && slices.Contains([]string{"init", "config"}, gitSubcommand(strings.Fields(argv))) {
					continue
				}
				held++
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
			if started == 0 || held == 0 || len(recorded) != started {
				t.Errorf("the recording git ran %d times, %d of them held to the windows, for the %d gits the runner started", len(recorded), held, started)
			}
		})
	}
}

// gitSubcommand is the subcommand of the git arguments args: the first that is neither a `-c`
// pair nor a --git-dir.
func gitSubcommand(args []string) string {
	pair := false
	for _, arg := range args {
		switch {
		case pair:
			pair = false
		case arg == "-c":
			pair = true
		case strings.HasPrefix(arg, "--git-dir="):
		default:
			return arg
		}
	}
	return ""
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
	run.runner = NewRunner(procgroup.NoReaper, testTimeout, testTools(t))
	t.Setenv("PATH", path)
	return sink
}
