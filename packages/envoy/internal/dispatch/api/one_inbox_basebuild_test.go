//go:build basebuild

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The base build every base-build check of dispatch://INFRA-1152 boots beside this one: the
// commit BASELINE_SHA names, checked out as a worktree at the path the check names, built with the
// stamp the image build gives a server, and booted over the database the check names, which that
// build migrates itself. What it serves is read back over loopback as a person and as the agent
// bearer, as lib-baseline.sh's baseline_boot serves it to the shell drivers
// (packages/dispatch/e2e/acceptance/lib-baseline.sh), so a Go check and a driver boot the same
// server the same way.
//
// It carries the basebuild tag, as cmd/dispatch's store_census_test.go does: it reads the
// repository's history and builds a second server, which no CI suite pays for.

// baselineSHAEnv names the commit the base build is built from: the release this branch lands
// after. It has no default, as lib-baseline.sh's has none: main's merge base is older than any
// sibling branch this one is stacked on, so a check built on it could pass without being able to
// fail, and only the caller knows which release the branch lands after.
const baselineSHAEnv = "BASELINE_SHA"

// fullCommit is the only spelling of a commit the harness builds: a revision that resolves to
// anything else is refused rather than built, so the stamp and the worktree always name one object.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// baselineBoot is how long the base build gets to say it serves and answer /healthz: its first
// boot migrates a whole schema from empty, which is the longest this wait covers.
const baselineBoot = 3 * time.Minute

// baselineCommit is the commit the base build is built from: BASELINE_SHA, resolved with `git
// rev-parse --verify <rev>^{commit}` to a full sha. An unset or empty one fails the test, naming it.
func baselineCommit(t *testing.T) string {
	t.Helper()
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	revision := strings.TrimSpace(os.Getenv(baselineSHAEnv))
	if revision == "" {
		t.Fatalf("%s is unset: set it to the commit of the release this branch lands after; it has no default", baselineSHAEnv)
	}
	commit := gitOutput(t, root, "rev-parse", "--verify", revision+"^{commit}")
	if !fullCommit.MatchString(commit) {
		t.Fatalf("%s=%q resolves to %q, which is not a full commit sha", baselineSHAEnv, revision, commit)
	}
	return commit
}

// buildBaseline checks the base commit out as a worktree at worktree and builds its
// ./cmd/dispatch stamped with that commit (-ldflags "-X main.buildCommit=<commit>", the flag
// packages/envoy/docker/Dockerfile uses), answering the binary and the commit. The worktree is
// removed when the test ends, so the next run can add it again; a path that already exists is
// refused rather than deleted, since it may be a worktree another check still reads. The binary is
// cmdtest's, in a directory of the test's own under TMPDIR, removed when the test ends.
func buildBaseline(t *testing.T, worktree string) (binary, commit string) {
	t.Helper()
	commit = baselineCommit(t)
	switch _, err := os.Lstat(worktree); {
	case err == nil:
		t.Fatalf("%s already exists, so the base build cannot be checked out there: clear it with\n\tgit worktree remove --force %s", worktree, worktree)
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("read %s: %v", worktree, err)
	}
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", worktree, commit).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add --detach %s %s: %v\n%s", worktree, commit, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("git", "-C", root, "worktree", "remove", "--force", worktree).CombinedOutput(); err != nil {
			t.Errorf("git worktree remove --force %s: %v\n%s", worktree, err, out)
		}
	})
	binary = cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   filepath.Base(worktree) + "-dispatch",
		Dir:    filepath.Join(worktree, "packages", "envoy"),
		Target: "./cmd/dispatch",
		Flags:  []string{"-ldflags", "-X main.buildCommit=" + commit},
	})
	t.Logf("the base build is %s, built from %s in %s", binary, commit, worktree)
	return binary, commit
}

// baselineDatabaseURL is the database a base-build check boots the base build over, read from
// variable. Unset, the check fails naming the Verification preamble's lines that make it; a
// database that already holds tables fails too, since the base build migrates the database it is
// given and a check's corpus seeded over an earlier run's is two corpora. The URL itself is never
// printed: it carries the test server's credential.
func baselineDatabaseURL(t *testing.T, variable, database string) string {
	t.Helper()
	recreate := fmt.Sprintf("psql \"$LEGION_TEST_PG_DSN\" -c 'drop database if exists %s' -c 'create database %s'", database, database)
	databaseURL := strings.TrimSpace(os.Getenv(variable))
	if databaseURL == "" {
		t.Fatalf("%s is unset: this check boots the base build over a database of its own, which the Verification preamble makes with\n"+
			"\texport %s=\"$(printf %%s \"$LEGION_TEST_PG_DSN\" | sed -E 's#/[^/?]*(\\?|$)#/%s\\1#')\"\n\t%s",
			variable, variable, database, recreate)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the database %s names: %v", variable, err)
	}
	defer conn.Close(ctx)
	var tables int
	if err := conn.QueryRow(ctx, `select count(*) from pg_tables where schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatalf("read the tables of the database %s names: %v", variable, err)
	}
	if tables > 0 {
		t.Fatalf("the database %s names already holds %d tables, and the base build must migrate an empty one: make it again with\n\t%s",
			variable, tables, recreate)
	}
	return databaseURL
}

// apiAnswer is one answer of a server a check drives: its status, its body, and the time from the
// request's send to its body's last byte. The body is encoded before that clock starts.
type apiAnswer struct {
	status int
	body   []byte
	took   time.Duration
}

// baselineServer is a server a check drives over loopback: a build's process (startBuild) - the base
// build's, or this build's where a measurement times it as the base build is timed - or this build's
// handler served by httptest (serveOnLoopback), whose process is nil. name is what the check calls
// it. It holds the origin every request names (dev sign-in answers only that Host, and a cookie
// write must carry it as its Origin), the agent bearer it trusts, and how it is told who a person
// is: by the header identityHeader names, which it trusts, or, where that is empty, by the session
// cookie of one signed-in client per person it has been asked to act as. signInKey is its
// DISPATCH_DEV_SIGNIN_SHARED_KEY, which a second process booted over the same database must carry
// too, since a process holding a key of its own is refused beside it. trees has every person's
// request ask for display trees (Dispatch-Trees: 1), as the dashboard's client asks on every read
// and write it sends (web/src/api/client.ts); an agent's call asks for none, as no other client
// does.
type baselineServer struct {
	name           string
	process        *cmdtest.Process
	origin         string
	token          string
	signInKey      string
	identityHeader string
	trees          bool
	transport      *http.Transport
	agent          *http.Client
	people         map[string]*http.Client
}

// baselineIdentityHeader is the header a server trusting a header identity reads a person from:
// the one the in-process test server reads (headerIdentity), so both builds of a measurement are
// told who a person is by one request.
const baselineIdentityHeader = "X-Dispatch-User"

// baselineRuntime is the Go runtime's own settings a check runs under (the pod's rule: one Go
// process at a time, each held to these), which the base build is booted with too where the check
// set them, so it runs on the processors, the collector and the memory limit this process runs on.
var baselineRuntime = []string{"GOMAXPROCS", "GOGC", "GOMEMLIMIT"}

// bootBaseline starts binary over databaseURL on a loopback port it holds from the moment the
// kernel chose it (cmdtest.HoldPort, inherited as DISPATCH_LISTEN_FD), with the environment
// lib-baseline.sh's baseline_boot gives a server and nothing of the caller's but the Go runtime's
// settings (baselineRuntime): dev sign-in over a cookie identity, NATS off, a home and a TMPDIR of
// its own, and a stand-in Envoy listener (standInListener) in place of baseline_boot's port 1,
// because the write paths a check times deliver to sessions and a delivery refused at once is not
// the write a session's own listener answers. It returns once the process has said it serves and
// /healthz answers 200, and stops it when the test ends.
func bootBaseline(t *testing.T, binary, databaseURL string) *baselineServer {
	t.Helper()
	return startBuild(t, "the base build", binary, databaseURL, "")
}

// bootBaselineTrustingHeader is bootBaseline with the person read from baselineIdentityHeader, as
// DISPATCH_IDENTITY=header:<name> reads one, in place of dev sign-in's cookie: a measurement that
// times one request on the base build against the same request on a server of this build, which
// reads the same header, then pays one identity's cost on both sides.
func bootBaselineTrustingHeader(t *testing.T, binary, databaseURL string) *baselineServer {
	t.Helper()
	return startBuild(t, "the base build", binary, databaseURL, baselineIdentityHeader)
}

// startBuild is bootBaseline for the build binary is, which the check calls name, resolving a
// person by the header identityHeader names where it is set and by dev sign-in's cookie where it is
// empty.
func startBuild(t *testing.T, name, binary, databaseURL, identityHeader string) *baselineServer {
	t.Helper()
	home := t.TempDir()
	webDist := filepath.Join(home, "web-dist")
	if err := os.Mkdir(webDist, 0o755); err != nil {
		t.Fatalf("make %s's web directory: %v", name, err)
	}
	held := cmdtest.HoldPort(t)
	transport := loopbackTransport(t)
	server := &baselineServer{
		name:           name,
		origin:         "http://" + held.Addr,
		token:          randomBaselineSecret(t),
		identityHeader: identityHeader,
		transport:      transport,
		agent:          &http.Client{Transport: transport},
		people:         map[string]*http.Client{},
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + home,
		"XDG_CACHE_HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=" + server.token,
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(held.Port),
		"DISPATCH_SERVER_URL=" + server.origin,
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_WEB_DIST=" + webDist,
		"ENVOY_URL=" + standInListener(t),
	}
	if identityHeader == "" {
		server.signInKey = randomBaselineSecret(t)
		cmd.Env = append(cmd.Env,
			"DISPATCH_DEV_SIGNIN=1",
			"DISPATCH_DEV_SIGNIN_SHARED_KEY="+server.signInKey,
			"DISPATCH_IDENTITY=cookie",
			"DISPATCH_INSECURE_COOKIE=1",
		)
	} else {
		cmd.Env = append(cmd.Env, "DISPATCH_IDENTITY=header:"+identityHeader, "DISPATCH_IDENTITY_HEADER_TRUSTED=1")
	}
	for _, name := range baselineRuntime {
		if value, set := os.LookupEnv(name); set {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	server.process = cmdtest.Start(t, name, cmd)
	server.waitHealthy(t, `msg="dispatch: listening" addr=`+held.Addr)
	return server
}

// serveOnLoopback serves handler, a server of this build in the process, on a loopback port of
// httptest's, driven as the base build is driven: a person by baselineIdentityHeader, which the
// handler's headerIdentity reads, and an agent by token, the bearer it was built to trust. It is
// closed when the test ends, before the cleanups the handler registered earlier.
func serveOnLoopback(t *testing.T, handler http.Handler, token string) *baselineServer {
	t.Helper()
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)
	transport := loopbackTransport(t)
	return &baselineServer{
		name:           "this build's handler",
		origin:         served.URL,
		token:          token,
		identityHeader: baselineIdentityHeader,
		transport:      transport,
		agent:          &http.Client{Transport: transport},
		people:         map[string]*http.Client{},
	}
}

// loopbackTransport is the transport every server a check drives is called through, one per
// server, so two servers called in turn are called the same way.
func loopbackTransport(t *testing.T) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

// waitHealthy waits for the listening line, then for /healthz to answer 200 while the process
// still runs. The inherited socket is not readiness: the kernel queues connections on it before
// the process serves them.
func (s *baselineServer) waitHealthy(t *testing.T, listening string) {
	t.Helper()
	deadline := time.Now().Add(baselineBoot)
	for time.Now().Before(deadline) {
		select {
		case <-s.process.Exited:
			t.Fatalf("%s exited before it served:\n%s", s.name, s.process.Output.String())
		default:
		}
		if strings.Contains(s.process.Output.String(), listening) {
			response, err := s.agent.Get(s.origin + "/healthz")
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not answer /healthz within %s:\n%s", s.name, baselineBoot, s.process.Output.String())
}

// call sends body to path as login, or on the agent bearer when login is empty, and answers what
// the server answered. A person is named by the server's identity header where it trusts one, and
// is otherwise signed in through dev sign-in the first time a call names them, off the clock; the
// clock covers the request and the reading of its whole answer.
func (s *baselineServer) call(t *testing.T, login, method, path string, body any) apiAnswer {
	t.Helper()
	return s.send(t, login, method, path, body, true)
}

// read is call for a GET whose answer is timed and not read back: an answer of 200 is read to its
// last byte, on the clock, and dropped, so a reading of a large answer leaves none of it to collect
// in this process, which also runs this build's server; any other answer is kept, for the failure
// that names it.
func (s *baselineServer) read(t *testing.T, login, path string) apiAnswer {
	t.Helper()
	return s.send(t, login, http.MethodGet, path, nil, false)
}

// send is call, keeping the body of an answer of 200 only where keep is set.
func (s *baselineServer) send(t *testing.T, login, method, path string, body any, keep bool) apiAnswer {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		payload = encoded
	}
	request, err := http.NewRequest(method, s.origin+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := s.agent
	switch {
	case login == "":
		request.Header.Set("Authorization", "Bearer "+s.token)
	case s.identityHeader != "":
		request.Header.Set(s.identityHeader, login)
	default:
		client = s.person(t, login)
		// A cookie write is held to the dashboard origin (the router's enforceCookieOrigin), as
		// the page's own same-origin request carries it.
		request.Header.Set("Origin", s.origin)
	}
	if login != "" && s.trees {
		request.Header.Set(displayTreesHeader, "1")
	}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s as %q: %v", method, path, login, err)
	}
	var answer []byte
	if keep || response.StatusCode != http.StatusOK {
		answer, err = io.ReadAll(response.Body)
	} else {
		_, err = io.Copy(io.Discard, response.Body)
	}
	took := time.Since(started)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	return apiAnswer{status: response.StatusCode, body: answer, took: took}
}

// person is login's signed-in client: GET /auth/_dev/signin?login=<login> records the person and
// sets their session cookie in the client's jar, which every later call of theirs carries.
func (s *baselineServer) person(t *testing.T, login string) *http.Client {
	t.Helper()
	if client, signed := s.people[login]; signed {
		return client
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("make %s's cookie jar: %v", login, err)
	}
	client := &http.Client{
		Transport: s.transport,
		Jar:       jar,
		// The sign-in answers 302 to the page with the cookie on it; the cookie is all a check needs.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get(s.origin + "/auth/_dev/signin?login=" + url.QueryEscape(login))
	if err != nil {
		t.Fatalf("sign %s in to the base build: %v", login, err)
	}
	answer, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	origin, _ := url.Parse(s.origin)
	if response.StatusCode != http.StatusFound || len(jar.Cookies(origin)) == 0 {
		t.Fatalf("sign %s in to the base build: %d %s, want 302 with a session cookie", login, response.StatusCode, answer)
	}
	s.people[login] = client
	return client
}

// standInListener is a stand-in Envoy listener: every session it is asked about is live and takes
// every delivery mode, and every send is handed over at once, as cmd/dispatch's census test stands
// one in for the base build. It answers the registry's one row through writeSessionRecord, which
// fills in what a live, heartbeating session's row carries.
func standInListener(t *testing.T) string {
	t.Helper()
	var sends atomic.Int64
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch session, lookup := strings.CutPrefix(r.URL.Path, "/v1/registry/"); {
		case r.Method == http.MethodGet && lookup:
			row, err := json.Marshal([]map[string]any{{
				"session_id": session, "title": session, "capabilities": []string{"btw", "aside", "steer"},
			}})
			if err != nil {
				t.Errorf("encode the stand-in listener's row for %s: %v", session, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeSessionRecord(t, w, string(row), session)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var send struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&send); err != nil {
				t.Errorf("decode a send to the stand-in listener: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"event_id": "stand-in-" + strconv.FormatInt(sends.Add(1), 10), "recipient": send.TargetSession,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// randomBaselineSecret is a value for one run alone: the agent bearer and the dev sign-in key.
func randomBaselineSecret(t *testing.T) string {
	t.Helper()
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("make a secret for the base build: %v", err)
	}
	return hex.EncodeToString(secret)
}
