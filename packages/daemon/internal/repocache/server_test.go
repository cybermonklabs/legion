package repocache

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/volumestat"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const testTimeout = 30 * time.Second

// gitHub is how the tests' git reaches https://github.com/acme/widgets: the URL in a command's argv
// is replaced with the local bare remote, over git's file transport, which the tools' wrappers add
// to the runner's https-only allow-list.
const gitHub = "https://github.com/acme/widgets"

// testRunner runs every command through workspace.NewRunner with the real git, the GitHub URL in
// its argv replaced by the local bare remote. It records, for every clone and fetch, the token its
// one-shot credential held while it ran. A test that arms hold has the next clone or fetch signal
// started and wait for release before it runs.
type testRunner struct {
	remote string
	others map[string]string
	runner workspace.Runner

	mu      sync.Mutex
	argv    [][]string
	tokens  []string
	held    chan struct{}
	release chan struct{}
	// on, when set, runs in place of the next command whose argv holds its word (onWord).
	onWord string
	on     func(ctx context.Context, command workspace.Command) (workspace.Result, error)
}

// instead arms the next command whose argv holds word to run do in place of git; do may run the
// command itself (real).
func (r *testRunner) instead(word string, do func(ctx context.Context, command workspace.Command) (workspace.Result, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onWord, r.on = word, do
}

func newTestRunner(t *testing.T, remote string) *testRunner {
	t.Helper()
	tools := map[string]string{}
	dir := t.TempDir()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the cache's tests drive a real git: %v", err)
	}
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["git"] = wrapper
	return &testRunner{remote: remote, others: map[string]string{}, runner: workspace.NewRunner(testTimeout, tools)}
}

// serves has the runner's git reach repo's GitHub URL at the local bare remote too.
func (r *testRunner) serves(repo, remote string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.others[workspace.GitHubURL(ghrepo.MustParse(repo))] = remote
}

func (r *testRunner) Timeout() time.Duration { return testTimeout }

func (r *testRunner) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	transfer := false
	actual := command
	actual.Argv = append([]string(nil), command.Argv...)
	r.mu.Lock()
	for i, arg := range actual.Argv {
		if arg == gitHub {
			actual.Argv[i] = "file://" + r.remote
			transfer = true
		} else if other, ok := r.others[arg]; ok {
			actual.Argv[i] = "file://" + other
			transfer = true
		}
	}
	r.mu.Unlock()
	r.mu.Lock()
	r.argv = append(r.argv, command.Argv)
	if r.on != nil && slices.Contains(command.Argv, r.onWord) {
		do := r.on
		r.on, r.onWord = nil, ""
		r.mu.Unlock()
		return do(ctx, actual)
	}
	var held, release chan struct{}
	if transfer {
		token := ""
		for _, entry := range command.Env {
			if file, ok := strings.CutPrefix(entry, "LEGION_PROVISIONING_TOKEN_FILE="); ok {
				if body, err := os.ReadFile(file); err == nil {
					token = string(body)
				}
			}
		}
		r.tokens = append(r.tokens, token)
		// A fetch's read of GitHub's HEAD is a transfer too, but hold is for the next clone or fetch.
		if !slices.Contains(command.Argv, "ls-remote") {
			held, release = r.held, r.release
			r.held, r.release = nil, nil
		}
	}
	r.mu.Unlock()
	if held != nil {
		close(held)
		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
	}
	return r.runner.Run(ctx, actual)
}

// hold arms the next clone or fetch to wait: started is closed once it is reached, and it runs once
// the returned release is called.
func (r *testRunner) hold() (started <-chan struct{}, release func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held, r.release = make(chan struct{}), make(chan struct{})
	started, gate := r.held, r.release
	var once sync.Once
	return started, func() { once.Do(func() { close(gate) }) }
}

// transfers is every clone and fetch the runner saw, its argv's subcommand each.
func (r *testRunner) transfers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var seen []string
	for _, argv := range r.argv {
		for _, arg := range argv {
			if arg != gitHub {
				continue
			}
			args := argv[1:]
			for len(args) > 2 && args[0] == "-c" {
				args = args[2:]
			}
			seen = append(seen, args[0]+" "+args[1])
			break
		}
	}
	return seen
}

func (r *testRunner) seenTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

// git runs a fixture git command in dir, under fixtureenv.Isolated, and returns its output, trimmed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return fixturegit.Isolated(t, t.TempDir()).Run(dir, append([]string{"git"}, args...)...)
}

// newRemote is a bare remote whose default branch is main, holding one commit.
func newRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	git(t, root, fixturegit.InitBare(remote, "main")[1:]...)
	advance(t, remote, "seed")
	return remote
}

// advance pushes one more commit, adding a file named for message, to the remote's main and returns
// its id.
func advance(t *testing.T, remote, message string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, message+".txt"), []byte(message+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", message)
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	return git(t, work, "rev-parse", "HEAD")
}

// syncLog collects the cache's log lines, safe for the test to read while Serve writes.
type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) log(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *syncLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// running is one running Serve: its options, its address, its log, and Serve's answer once it ends.
type running struct {
	opts   Options
	run    *testRunner
	url    string
	log    *syncLog
	cancel context.CancelFunc
	done   chan error
}

// testCacheToken is the bearer a test cache of acme/widgets takes.
const testCacheToken = "cache-token-of-acme-widgets"

// testOptions are a cache of acme/widgets's options on a fresh volume: its provisioning token file
// holding ghs_first, and its bearer token file testCacheToken.
func testOptions(t *testing.T, run *testRunner) Options {
	t.Helper()
	return repoOptions(t, run, "acme/widgets", testCacheToken)
}

// repoOptions are the options of a cache of repo on a fresh volume, taking bearer.
func repoOptions(t *testing.T, run *testRunner, repo, bearer string) Options {
	t.Helper()
	dir := t.TempDir()
	tokenFile, bearerFile := filepath.Join(dir, "token"), filepath.Join(dir, "cache-token")
	if err := os.WriteFile(tokenFile, []byte("ghs_first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bearerFile, []byte(bearer+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		Repo: ghrepo.MustParse(repo), Root: t.TempDir(), TokenFile: tokenFile, CacheTokenFile: bearerFile, CredentialDir: t.TempDir(),
		Refresh: time.Hour, Listen: "127.0.0.1:0", Run: run,
	}
}

// get is GET path of the cache, presenting bearer when it is not empty.
func (c *running) get(t *testing.T, path, bearer string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, c.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// bearer is the token the cache takes, as its file holds it.
func (c *running) bearer(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(c.opts.CacheTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

// start runs Serve with opts until the test ends, and returns once it serves.
func start(t *testing.T, opts Options) *running {
	t.Helper()
	log := &syncLog{}
	opts.Log = log.log
	ctx, cancel := context.WithCancel(context.Background())
	c := &running{opts: opts, run: opts.Run.(*testRunner), log: log, cancel: cancel, done: make(chan error, 1)}
	go func() { c.done <- Serve(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-c.done:
			if err != nil {
				t.Errorf("Serve ended with %v", err)
			}
		case <-time.After(testTimeout):
			t.Error("Serve did not end with its context")
		}
	})
	if !testwait.Holds(testTimeout, testwait.Poll, func() bool {
		for _, line := range log.all() {
			if address, ok := strings.CutPrefix(line, "serving "+opts.Repo.String()+" on "); ok {
				c.url = "http://" + address
				return true
			}
		}
		return false
	}) {
		t.Fatalf("Serve never said where it serves: %q", log.all())
	}
	return c
}

// health is GET /healthz, with the cache's bearer: its status and its body.
func (c *running) health(t *testing.T) (int, Health) {
	t.Helper()
	status, health, _ := c.healthFields(t)
	return status, health
}

// ready waits until /healthz answers 200 and returns what it said.
func (c *running) ready(t *testing.T) Health {
	t.Helper()
	return c.waitFor(t, "ready", func(status int, _ Health) bool { return status == http.StatusOK })
}

// waitFor polls /healthz until done holds and returns what it said then.
func (c *running) waitFor(t *testing.T, what string, done func(int, Health) bool) Health {
	t.Helper()
	var status int
	var health Health
	if !testwait.Holds(testTimeout, 20*time.Millisecond, func() bool {
		status, health = c.health(t)
		return done(status, health)
	}) {
		t.Fatalf("the cache never became %s: /healthz answers %d %+v; log %q", what, status, health, c.log.all())
	}
	return health
}

// dir is the cache's bare clone.
func (c *running) dir() string {
	return filepath.Join(c.opts.Root, "acme", "widgets.git")
}

// unpack writes a tar stream into a new directory and returns it with every entry name it held.
func unpack(t *testing.T, stream io.Reader) (string, []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "widgets.git")
	var names []string
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return dir, names
		}
		if err != nil {
			t.Fatalf("read the tar: %v", err)
		}
		names = append(names, header.Name)
		path := filepath.Join(dir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("the tar holds %s, of type %q: neither a regular file nor a directory", header.Name, header.Typeflag)
		}
	}
}

// getTar is GET /tar with the cache's bearer, read whole.
func (c *running) getTar(t *testing.T) (int, []byte) {
	t.Helper()
	response := c.get(t, "/tar", c.bearer(t))
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /tar: %v", err)
	}
	return response.StatusCode, body
}

// /healthz answers 503, not ready, until the first clone finished, and /tar answers 503 too; then
// 200 with the repository, the default branch's head, when that clone finished and how many bytes
// the clone holds.
func TestHealthzIsUnavailableUntilTheFirstCloneFinishes(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	started, release := run.hold()
	defer release()
	before := time.Now()
	c := start(t, testOptions(t, run))
	<-started

	status, health := c.health(t)
	if status != http.StatusServiceUnavailable || health.Ready || health.Repo != "acme/widgets" {
		t.Fatalf("/healthz during the clone answers %d %+v, want 503, not ready, acme/widgets", status, health)
	}
	if status, _ := c.getTar(t); status != http.StatusServiceUnavailable {
		t.Fatalf("/tar during the clone answers %d, want 503", status)
	}
	release()
	health = c.ready(t)
	if want := git(t, remote, "--git-dir="+remote, "rev-parse", "main"); health.Head != want {
		t.Errorf("/healthz head = %s, want the remote's main %s", health.Head, want)
	}
	if health.Bytes <= 0 || health.LastFetchAt.Before(before) || health.LastFetchAt.After(time.Now()) {
		t.Errorf("/healthz = %+v, want the clone's bytes and the time it finished", health)
	}
	if !health.DefaultBranchOnly {
		t.Errorf("/healthz = %+v, want defaultBranchOnly true", health)
	}
	if _, err := os.Stat(filepath.Join(c.opts.Root, defaultBranchOnlyFile)); err != nil {
		t.Errorf("the first clone has no default-branch-only marker: %v", err)
	}
	if got := run.repacks(); got != 0 {
		t.Errorf("the first default-only clone ran %d repacks, want none", got)
	}
	if logged(c.log.all(), "narrowed "+c.dir()+" ") {
		t.Errorf("the first default-only clone logged a narrowing: %q", c.log.all())
	}
	if transfers := run.transfers(); len(transfers) != 1 || transfers[0] != "clone --bare" {
		t.Errorf("the cache ran %q, want one clone", transfers)
	}
}

// /tar streams the bare clone's refs and packs alone — HEAD, packed-refs, refs/,
// objects/pack/pack-<id>.pack and the index git reads each pack through — and no reverse index,
// loose object, or anything of the clone's own configuration, hooks or descriptions: unpacked, it
// is a repository whose connectivity git checks and whose default branch is the remote's, holding
// regular files and directories alone and no temporary pack or lock file git was writing.
func TestTarStreamsTheBareRepositorysRefsAndPacks(t *testing.T) {
	remote := newRemote(t)
	c := start(t, testOptions(t, newTestRunner(t, remote)))
	c.ready(t)
	for _, planted := range []string{"objects/pack/tmp_pack_abc", "refs/heads/main.lock", "packed-refs.lock"} {
		if err := os.WriteFile(filepath.Join(c.dir(), planted), []byte("being written\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, own := range []string{"config", "description", "hooks", "info"} {
		if _, err := os.Stat(filepath.Join(c.dir(), own)); err != nil {
			t.Fatalf("the clone holds no %s to leave out: %v", own, err)
		}
	}

	status, body := c.getTar(t)
	if status != http.StatusOK {
		t.Fatalf("/tar answers %d", status)
	}
	unpacked, names := unpack(t, bytes.NewReader(body))
	packs, indexes := 0, 0
	for _, name := range names {
		switch {
		case name == "HEAD" || name == "packed-refs" || strings.HasPrefix(name, "refs/") || name == "objects/" || name == "objects/pack/":
		case regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.pack$`).MatchString(name):
			packs++
		case regexp.MustCompile(`^objects/pack/pack-[0-9a-f]{40}\.idx$`).MatchString(name):
			indexes++
		default:
			t.Errorf("the tar holds %s, which is neither a ref nor a pack", name)
		}
	}
	if packs == 0 || indexes != packs {
		t.Fatalf("the tar holds %d packs and %d indexes: %q; want every pack with the index git reads it through", packs, indexes, names)
	}
	indexed(t, unpacked)
	if got, want := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"), git(t, remote, "--git-dir="+remote, "rev-parse", "main"); got != want {
		t.Errorf("the unpacked main is %s, want the remote's %s", got, want)
	}
}

// indexed checks the unpacked bare repository dir as an issue pod's fetch does: each pack the
// cache streamed came with the index git reads it through, so none is indexed again, and the
// history is whole.
func indexed(t *testing.T, dir string) {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pack := range packs {
		index := strings.TrimSuffix(pack, ".pack") + ".idx"
		if _, err := os.Stat(index); err != nil {
			t.Fatalf("the cache streamed %s without its index: %v", filepath.Base(pack), err)
		}
	}
	git(t, dir, "--git-dir="+dir, "fsck", "--connectivity-only")
}

func TestTheCachesClonesHistoryStaysPacked(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	moved := advance(t, remote, "moved")
	c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
	if loose := git(t, c.dir(), "--git-dir="+c.dir(), "count-objects"); !strings.HasPrefix(loose, "0 objects") {
		t.Errorf("the cache's clone holds %s after a fetch", loose)
	}
}

// A tar asked for while a fetch runs waits for the fetch and streams the state it left, and
// /healthz keeps answering ready meanwhile: the kubelet's readiness probe never waits on a fetch.
func TestATarAskedForDuringAFetchWaitsForIt(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	moved := advance(t, remote, "moved")
	started, release := run.hold()
	defer release()
	<-started

	streamed := make(chan []byte, 1)
	request, err := http.NewRequest(http.MethodGet, c.url+"/tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.bearer(t))
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			streamed <- nil
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		streamed <- body
	}()
	select {
	case <-streamed:
		t.Fatal("the tar streamed while a fetch ran")
	case <-time.After(300 * time.Millisecond):
	}
	if status, health := c.health(t); status != http.StatusOK || !health.Ready {
		t.Fatalf("/healthz during the fetch answers %d %+v, want ready", status, health)
	}
	release()
	var body []byte
	select {
	case body = <-streamed:
	case <-time.After(testTimeout):
		t.Fatal("the tar never streamed once the fetch ended")
	}
	unpacked, _ := unpack(t, bytes.NewReader(body))
	indexed(t, unpacked)
	if got := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"); got != moved {
		t.Errorf("the tar's main is %s, want what the fetch brought, %s", got, moved)
	}
}

// A tar's reader that stops reading partway holds nothing a refresh waits on: the stream takes the
// clone's refs and packs under the read lock and sends them with no lock held, so the next tick's
// fetch goes through while the reader stalls, /healthz moves to its head, and the stalled reader,
// once it reads on, gets the whole tar it began, as it stood when it began. The pack is large
// enough that the cache blocks writing it to a reader that stopped.
func TestAStalledTarReaderDoesNotHoldTheNextFetch(t *testing.T) {
	remote := newRemote(t)
	large := make([]byte, 24<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "large")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	began := git(t, work, "rev-parse", "HEAD")
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.waitFor(t, "at the large commit", func(_ int, h Health) bool { return h.Head == began })

	stalled := c.get(t, "/tar", c.bearer(t))
	defer stalled.Body.Close()
	first := make([]byte, 4096)
	if _, err := io.ReadFull(stalled.Body, first); err != nil {
		t.Fatalf("read the tar's first bytes: %v", err)
	}
	moved := advance(t, remote, "moved")
	c.waitFor(t, "at the moved head while a tar's reader stalls", func(_ int, h Health) bool { return h.Head == moved })

	unpacked, _ := unpack(t, io.MultiReader(bytes.NewReader(first), stalled.Body))
	indexed(t, unpacked)
	if got := git(t, unpacked, "--git-dir="+unpacked, "rev-parse", "refs/heads/main"); got != began {
		t.Errorf("the stalled tar's main is %s, want the head it began at, %s", got, began)
	}
}

// Past maxStreams streams a tar waits for one to end: it gets nothing while the stream ahead of it
// is sent, a waiter whose request ends first gives up having sent nothing and holding no turn, and
// the next waiter gets the whole tar once the stream ahead ends.
func TestATarPastTheStreamBoundWaitsForAStreamToEnd(t *testing.T) {
	defer func(was int) { maxStreams = was }(maxStreams)
	maxStreams = 1
	remote := newRemote(t)
	large := make([]byte, 24<<20)
	if _, err := rand.Read(large); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	if err := os.WriteFile(filepath.Join(work, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "large")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	head := git(t, work, "rev-parse", "HEAD")
	c := start(t, testOptions(t, newTestRunner(t, remote)))
	c.waitFor(t, "at the large commit", func(_ int, h Health) bool { return h.Head == head })

	ahead := c.get(t, "/tar", c.bearer(t))
	defer ahead.Body.Close()
	first := make([]byte, 4096)
	if _, err := io.ReadFull(ahead.Body, first); err != nil {
		t.Fatalf("read the first tar's first bytes: %v", err)
	}

	ask := func(ctx context.Context) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/tar", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+c.bearer(t))
		return http.DefaultClient.Do(request)
	}
	impatient, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if response, err := ask(impatient); err == nil {
		response.Body.Close()
		t.Fatalf("a tar past the bound was answered %s while the stream ahead of it was sent", response.Status)
	}

	answered := make(chan *http.Response, 1)
	go func() {
		response, err := ask(context.Background())
		if err != nil {
			t.Errorf("the waiting tar: %v", err)
			close(answered)
			return
		}
		answered <- response
	}()
	select {
	case response := <-answered:
		if response != nil {
			response.Body.Close()
		}
		t.Fatal("a tar past the bound was answered while the stream ahead of it was sent")
	case <-time.After(300 * time.Millisecond):
	}
	if _, health := c.health(t); health.Streams == nil || *health.Streams != 2 {
		t.Errorf("/healthz counts streams %v, want 2: one sent and one waiting", health.Streams)
	}
	unpacked, _ := unpack(t, io.MultiReader(bytes.NewReader(first), ahead.Body))
	indexed(t, unpacked)
	ahead.Body.Close()
	select {
	case response := <-answered:
		if response == nil {
			t.FailNow()
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("the waiting tar was answered %s, want 200 once the stream ahead ended", response.Status)
		}
		next, _ := unpack(t, response.Body)
		if got := git(t, next, "--git-dir="+next, "rev-parse", "refs/heads/main"); got != head {
			t.Errorf("the waiting tar's main is %s, want %s", got, head)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting tar was not answered within 30s of the stream ahead ending")
	}
}

// The next tick's fetch brings what the remote gained, and /healthz says so; every fetch reads the
// token file again, so a token the Secret's projection rotated is the one the next fetch presents.
func TestEachTickFetchesWithTheTokenFileAsItIsThen(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	first := c.ready(t)
	if err := os.WriteFile(opts.TokenFile, []byte("ghs_rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := advance(t, remote, "moved")

	health := c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
	if !health.LastFetchAt.After(first.LastFetchAt) {
		t.Errorf("lastFetchAt stayed %s after a fetch that moved the head", health.LastFetchAt)
	}
	tokens := run.seenTokens()
	if tokens[0] != "ghs_first" || tokens[len(tokens)-1] != "ghs_rotated" {
		t.Errorf("the transfers presented %q, want ghs_first first and the rotated ghs_rotated last", tokens)
	}
	for i, transfer := range run.transfers()[1:] {
		want := "--git-dir=" + c.dir() + " ls-remote"
		if i%3 == 1 {
			want = "--git-dir=" + c.dir() + " fetch"
		}
		if transfer != want {
			t.Errorf("a later tick's transfer %d is %q, want %q: each tick reads GitHub's HEAD, fetches that branch, then follows GitHub's HEAD", i+1, transfer, want)
		}
	}
}

// A fetch that fails is logged, and the cache stays ready on its last good state: the head it had.
func TestAFailedFetchKeepsTheCacheReady(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	// Each tick's fetch that succeeds publishes its own lastFetchAt, so the last good state is
	// read while the next tick's fetch is held: the refresh before it has published, and none
	// publishes again until the release, which comes once the remote is gone. Read while the ticks
	// ran, that state was overtaken whenever a fetch landed between the read and the removal.
	started, release := run.hold()
	testwait.Receive(t, "the next tick's fetch", started)
	before := c.ready(t)
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	release()

	if !testwait.Holds(testwait.Bounded(t, 1<<60, testTimeout), testwait.Poll, func() bool {
		return logged(c.log.all(), "fetch failed; serving the clone as it was: ")
	}) {
		t.Fatalf("no failed fetch was logged: %q", c.log.all())
	}
	// The next tick reads GitHub's default branch before it fetches. That read fails after the remote
	// is removed, so the fetch does not run. It still proves that Serve scheduled another tick before
	// the cache's last good state is read.
	next := make(chan struct{})
	run.instead("ls-remote", func(context.Context, workspace.Command) (workspace.Result, error) {
		close(next)
		return workspace.Result{ExitCode: 128, Stderr: "fatal: the remote is gone\n"}, nil
	})
	testwait.Receive(t, "the default-branch read of the tick after the failed one", next)
	status, health := c.health(t)
	if status != http.StatusOK || !health.Ready || health.Head != before.Head || health.LastFetchAt != before.LastFetchAt {
		t.Fatalf("/healthz after a failed fetch answers %d %+v, want ready on %+v", status, health, before)
	}
	if status, _ := c.getTar(t); status != http.StatusOK {
		t.Fatalf("/tar after a failed fetch answers %d, want 200", status)
	}
}

func logged(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// A cache restarted on a volume that holds its clone fetches into it rather than cloning again, and
// when that first fetch fails it serves the clone as the volume holds it.
func TestARestartOnAVolumeThatHoldsTheCloneFetchesIntoIt(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil

	moved := advance(t, remote, "moved")
	again := start(t, opts)
	if health := again.ready(t); health.Head != moved {
		t.Errorf("the restarted cache serves %s, want what its fetch brought, %s", health.Head, moved)
	}
	if transfers := run.transfers(); !slices.Equal(transfers, []string{"clone --bare", "--git-dir=" + again.dir() + " ls-remote", "--git-dir=" + again.dir() + " fetch", "--git-dir=" + again.dir() + " ls-remote"}) {
		t.Errorf("the cache ran %q, want a clone, then the restart's read of GitHub's HEAD, fetch and final read", transfers)
	}
	again.cancel()
	if err := <-again.done; err != nil {
		t.Fatalf("the second Serve ended with %v", err)
	}
	again.done <- nil

	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	offline := start(t, opts)
	if health := offline.ready(t); health.Head != moved || !health.LastFetchAt.IsZero() {
		t.Errorf("the cache restarted with GitHub unreachable serves %+v, want the volume's %s and no fetch time", health, moved)
	}
	if !logged(offline.log.all(), "fetch failed; serving the clone as it was: ") {
		t.Errorf("the failed first fetch was not logged: %q", offline.log.all())
	}
}

// A cache killed mid-fetch leaves git's lock files in its clone. Restarted on that volume, it is
// the clone's only writer, so its first fetch removes them and brings what the remote gained.
func TestACacheRestartedAfterAnInterruptedFetchClearsItsLocks(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	for _, lock := range []string{"refs/heads/main.lock", "packed-refs.lock"} {
		if err := os.WriteFile(filepath.Join(first.dir(), lock), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moved := advance(t, remote, "moved")

	again := start(t, opts)
	if health := again.ready(t); health.Head != moved || health.LastFetchAt.IsZero() {
		t.Fatalf("the restarted cache serves %+v, want what its fetch brought, %s", health, moved)
	}
	if !logged(again.log.all(), "removed the stale git lock "+filepath.Join(again.dir(), "refs/heads/main.lock")) {
		t.Errorf("the restart did not log the lock it removed: %q", again.log.all())
	}
}

func TestARestartedCacheReplacesACloneItsFetchDidNotWrite(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	grafts := filepath.Join(first.dir(), "info", "grafts")
	if err := os.WriteFile(grafts, []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := advance(t, remote, "moved")

	again := start(t, opts)
	if health := again.ready(t); health.Head != moved || health.LastFetchAt.IsZero() {
		t.Fatalf("the restarted cache serves %+v, want what its clone afresh brought, %s", health, moved)
	}
	if !slices.ContainsFunc(again.log.all(), func(line string) bool {
		return strings.Contains(line, "is not one this fetch wrote (it holds info/grafts") && strings.Contains(line, "removed")
	}) {
		t.Errorf("the restart did not log the clone it removed: %q", again.log.all())
	}
	if _, err := os.Stat(grafts); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the planted info/grafts after the restart: %v, want it gone with the clone", err)
	}
	if clones := slices.DeleteFunc(run.transfers(), func(transfer string) bool { return transfer != "clone --bare" }); len(clones) != 2 {
		t.Errorf("the cache ran %q, want a clone at the first start and a clone afresh at the restart", run.transfers())
	}
}

// GitHub renaming the repository's default branch moves the cache's HEAD with it at the next fetch.
// The fetch's prune removes the old branch, so a HEAD left naming it would name nothing: every
// publish would fail, and a restart would end Serve, the cache never ready again. Restarted on its
// volume after the rename, the cache is ready at the renamed branch's head, its HEAD names that
// branch, and the old branch is gone.
func TestACacheFollowsARenamedDefaultBranch(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	moved := advance(t, remote, "moved")
	git(t, remote, "branch", "-m", "main", "trunk")

	again := start(t, opts)
	if health := again.ready(t); health.Head != moved {
		t.Fatalf("the restarted cache serves %+v, want the renamed branch's head %s", health, moved)
	}
	head, err := os.ReadFile(filepath.Join(again.dir(), "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "ref: refs/heads/trunk\n" {
		t.Errorf("the cache's HEAD is %q, want the renamed branch", head)
	}
	if refs := git(t, again.dir(), "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/trunk" {
		t.Errorf("the cache's branches are %q, want trunk alone", refs)
	}
}

// A cache whose volume holds no clone and whose first clone fails has nothing to serve: Serve ends
// with the clone's failure, so the container exits and the kubelet restarts it.
func TestServeEndsWhenTheFirstCloneFails(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	opts.Log = func(string) {}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := Serve(ctx, opts); err == nil || !strings.Contains(err.Error(), "-c gc.auto=0 -c maintenance.auto=false clone") {
		t.Fatalf("Serve = %v, want the clone's failure", err)
	}
}

// Once a repack is due, the fetch that finds it so is followed by a repack of the clone into one
// pack with no loose object; until then a fetch leaves what it brought as git stored it.
func TestAFetchRepacksTheCloneOnceARepackIsDue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		every  time.Duration
		repack bool
	}{
		{"not due", 24 * time.Hour, false},
		{"due", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := repackEvery
			repackEvery = tc.every
			t.Cleanup(func() { repackEvery = saved })
			remote := newRemote(t)
			opts := testOptions(t, newTestRunner(t, remote))
			opts.Refresh = 50 * time.Millisecond
			c := start(t, opts)
			c.ready(t)
			moved := advance(t, remote, "moved")
			c.waitFor(t, "at the moved head", func(_ int, h Health) bool { return h.Head == moved })
			packed := func() (bool, string) {
				counts := map[string]string{}
				for _, line := range strings.Split(git(t, c.dir(), "--git-dir="+c.dir(), "count-objects", "-v"), "\n") {
					if key, value, ok := strings.Cut(line, ": "); ok {
						counts[key] = value
					}
				}
				return counts["count"] == "0" && counts["packs"] == "1", "count=" + counts["count"] + " packs=" + counts["packs"]
			}
			// The repack follows the fetch that brought the moved head, so a due one is waited for.
			deadline := time.Now().Add(testTimeout)
			got, held := packed()
			for tc.repack && !got && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
				got, held = packed()
			}
			if got != tc.repack {
				t.Errorf("the clone holds %s, want repacked %t", held, tc.repack)
			}
		})
	}
}

// Serve refuses options it cannot run on before it listens or touches the volume.
func TestServeRefusesIncompleteOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"no repository", func(o *Options) { o.Repo = ghrepo.Repository{} }, "repository cache repository is required"},
		{"a relative root", func(o *Options) { o.Root = "cache" }, `repository cache root must be an absolute path (got "cache")`},
		{"no token file", func(o *Options) { o.TokenFile = "" }, "repository cache token file is required"},
		{"no bearer token file", func(o *Options) { o.CacheTokenFile = "" }, "repository cache bearer token file is required"},
		{"a blank bearer token file", func(o *Options) {
			if err := os.WriteFile(o.CacheTokenFile, []byte(" \n"), 0o600); err != nil {
				panic(err)
			}
		}, "LEGION_REPOSITORY_CACHE_TOKEN_FILE names "},
		{"no credential directory", func(o *Options) { o.CredentialDir = "" }, "repository cache credential directory is required"},
		{"no refresh", func(o *Options) { o.Refresh = 0 }, "repository cache refresh must be positive (got 0s)"},
		{"no listen address", func(o *Options) { o.Listen = "" }, "repository cache listen address is required"},
		{"no log", func(o *Options) { o.Log = nil }, "repository cache log is required"},
		{"no runner", func(o *Options) { o.Run = nil }, "repository cache runner is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newTestRunner(t, newRemote(t))
			opts := testOptions(t, run)
			opts.Log = func(string) {}
			tc.edit(&opts)
			if err := Serve(context.Background(), opts); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Serve = %v, want %q", err, tc.want)
			}
			if transfers := run.transfers(); len(transfers) != 0 {
				t.Errorf("Serve ran %q before refusing", transfers)
			}
			if entries, err := os.ReadDir(opts.Root); tc.name != "a relative root" && (err != nil || len(entries) != 0) {
				t.Errorf("Serve wrote %v (%v) into its root before refusing", entries, err)
			}
		})
	}
}

func TestAnIssuePodsWorkspaceInitStreamsItsCloneFromTheCache(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	c := start(t, testOptions(t, run))
	c.ready(t)
	gitDir := filepath.Join(t.TempDir(), "widgets.git")
	report, err := workspace.StreamCache(context.Background(), run, workspace.CacheStream{
		Repo: ghrepo.MustParse("acme/widgets"), URL: c.url, Token: testCacheToken,
		Log: func(string) {}, Clock: clock.Real(), Bound: workspace.FetchTimeout,
	}, gitDir)
	if err != nil {
		t.Fatalf("StreamCache from the cache: %v", err)
	}
	if report.Bytes <= 0 {
		t.Fatalf("StreamCache reported %+v, want bytes from the cache", report)
	}
	if got, want := git(t, gitDir, "--git-dir="+gitDir, "rev-parse", "refs/heads/main"), git(t, remote, "--git-dir="+remote, "rev-parse", "main"); got != want {
		t.Errorf("the streamed clone's main is %s, want the cache's %s", got, want)
	}
	git(t, gitDir, "--git-dir="+gitDir, "fsck", "--connectivity-only")
	for _, transfer := range run.transfers()[1:] {
		if strings.HasPrefix(transfer, "clone ") || strings.HasSuffix(transfer, " fetch") {
			t.Errorf("workspace-init fetched from GitHub after streaming the cache: %q", transfer)
		}
	}
}

// /tar answers only a request carrying the cache's own token: none, or another, is 401, and says
// nothing of the repository. /healthz, its health, which carries no secret and nothing of the
// repository's content, answers any request, a token or none, the same way: 200 and the health,
// naming the repository in workspace.CacheRepositoryHeader. /readyz, the kubelet's readiness
// probe, answers 503 then 200 with nothing but its status.
func TestOnlyTheTarNeedsTheCachesToken(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	started, release := run.hold()
	defer release()
	c := start(t, testOptions(t, run))
	<-started
	if response := c.get(t, "/readyz", ""); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during the first clone answers %d, want 503", response.StatusCode)
	}
	release()
	c.ready(t)
	for _, bearer := range []string{"", "cache-token-of-acme-gadgets"} {
		response := c.get(t, "/tar", bearer)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get(workspace.CacheRepositoryHeader) != "" || strings.Contains(string(body), "acme/widgets") {
			t.Errorf("/tar with bearer %q answers %d %q naming %q, want 401 naming nothing", bearer, response.StatusCode, body, response.Header.Get(workspace.CacheRepositoryHeader))
		}
	}
	if response := c.get(t, "/tar", testCacheToken); response.StatusCode != http.StatusOK || response.Header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" {
		t.Errorf("/tar with its token answers %d naming %q, want 200 naming acme/widgets", response.StatusCode, response.Header.Get(workspace.CacheRepositoryHeader))
	} else {
		response.Body.Close()
	}
	for _, bearer := range []string{"", "cache-token-of-acme-gadgets", testCacheToken} {
		response := c.get(t, "/healthz", bearer)
		var health Health
		err := json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get(workspace.CacheRepositoryHeader) != "acme/widgets" || err != nil || !health.Ready || health.Repo != "acme/widgets" {
			t.Errorf("/healthz with bearer %q answers %d naming %q: %+v (%v), want 200 and the health naming acme/widgets", bearer, response.StatusCode, response.Header.Get(workspace.CacheRepositoryHeader), health, err)
		}
	}
	response := c.get(t, "/readyz", "")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(body) != 0 || response.Header.Get(workspace.CacheRepositoryHeader) != "" {
		t.Errorf("/readyz once ready answers %d %q naming %q, want 200 and nothing else", response.StatusCode, body, response.Header.Get(workspace.CacheRepositoryHeader))
	}
}

func TestAnIssuePodTakesNothingFromAnotherRepositorysCache(t *testing.T) {
	widgets, gadgets := newRemote(t), newRemote(t)
	tagged := advance(t, gadgets, "gadgets-only")
	git(t, gadgets, "--git-dir="+gadgets, "tag", "gadgets-only", tagged)
	run := newTestRunner(t, widgets)
	run.serves("acme/gadgets", gadgets)
	ownCache := start(t, testOptions(t, run))
	otherCache := start(t, repoOptions(t, run, "acme/gadgets", "cache-token-of-acme-gadgets"))
	ownCache.ready(t)
	otherCache.ready(t)
	stream := func(cache *running, bearer string) (workspace.StreamReport, string, error) {
		gitDir := filepath.Join(t.TempDir(), "widgets.git")
		report, err := workspace.StreamCache(context.Background(), run, workspace.CacheStream{
			Repo: ghrepo.MustParse("acme/widgets"), URL: cache.url, Token: bearer,
			Log: func(string) {}, Clock: clock.Real(), Bound: workspace.FetchTimeout,
		}, gitDir)
		return report, gitDir, err
	}
	for _, tc := range []struct {
		bearer, says string
	}{
		{testCacheToken, "is the repository cache of acme/gadgets, not of acme/widgets"},
		{"cache-token-of-acme-gadgets", "is the repository cache of acme/gadgets, not of acme/widgets"},
	} {
		_, _, err := stream(otherCache, tc.bearer)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("StreamCache of acme/widgets from acme/gadgets's cache with %q = %v, want a refusal naming %q", tc.bearer, err, tc.says)
		}
	}
	report, gitDir, err := stream(ownCache, testCacheToken)
	if err != nil {
		t.Fatalf("StreamCache from its own cache: %v", err)
	}
	if report.Bytes <= 0 {
		t.Fatalf("StreamCache reported %+v, want bytes from its cache", report)
	}
	if tags := git(t, gitDir, "--git-dir="+gitDir, "tag", "--list"); tags != "" {
		t.Errorf("the streamed clone holds tags %q", tags)
	}
	held := exec.Command("git", "--git-dir="+gitDir, "cat-file", "-e", tagged)
	held.Dir = gitDir
	held.Env = fixtureenv.Isolated(t.TempDir())
	if err := held.Run(); err == nil {
		t.Errorf("the streamed clone holds acme/gadgets's commit %s", tagged)
	}
}

// A due repack runs outside the lock a tar takes: a tar asked for while the repack runs is served
// at once, and the repack then completes. It runs within repackTimeout, and each of repackMemory's
// pairs comes after CacheGit's pack-window pair of the same key, where there is one: git takes the
// last value of a key, so the repack reads its own.
func TestATarIsServedWhileARepackRuns(t *testing.T) {
	saved := repackEvery
	repackEvery = 0
	t.Cleanup(func() { repackEvery = saved })
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	started, release := make(chan struct{}), make(chan struct{})
	var bound time.Duration
	var argv []string
	run.instead("repack", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		bound, argv = command.Timeout, command.Argv
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
		return run.runner.Run(ctx, command)
	})
	advance(t, remote, "moved")
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("no repack started")
	}
	request, err := http.NewRequest(http.MethodGet, c.url+"/tar", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.bearer(t))
	served := make(chan int, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			served <- 0
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		served <- response.StatusCode
	}()
	select {
	case status := <-served:
		if status != http.StatusOK {
			t.Errorf("the tar asked for during the repack answered %d, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Error("the tar waited on the repack")
	}
	if bound != repackTimeout {
		t.Errorf("the repack ran within %s, want repackTimeout %s, not the runner's own budget", bound, repackTimeout)
	}
	// last is the index of the last `-c` value of each key, lower-cased as git reads it.
	last := map[string]int{}
	for i := 1; i < len(argv); i++ {
		if argv[i-1] == "-c" {
			key, _, _ := strings.Cut(argv[i], "=")
			last[strings.ToLower(key)] = i
		}
	}
	pack := map[string]string{}
	for _, pair := range workspace.PackWindowConfig() {
		pack[strings.ToLower(pair[0])] = pair[0] + "=" + pair[1]
	}
	for i := 1; i < len(repackMemory); i += 2 {
		setting := repackMemory[i]
		key, _, _ := strings.Cut(setting, "=")
		at, found := last[strings.ToLower(key)]
		if !found || argv[at] != setting {
			t.Errorf("the repack ran %q, want its memory bounded by %s, the last value of %s", argv, setting, key)
			continue
		}
		if pair, ok := pack[strings.ToLower(key)]; ok {
			if packAt := slices.Index(argv, pair); packAt < 0 || packAt >= at {
				t.Errorf("the repack ran %q, want %s after CacheGit's %s", argv, setting, pair)
			}
		}
	}
	close(release)
	deadline := time.Now().Add(testTimeout)
	for !logged(c.log.all(), "repacked ") {
		if time.Now().After(deadline) {
			t.Fatalf("the repack never completed: %q", c.log.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A repack that fails is logged, and the partial packs it left are removed at once, each logged,
// so a repack that keeps failing never fills the cache's volume; the cache stays ready.
func TestAFailedRepackLeavesNoPartialPack(t *testing.T) {
	saved := repackEvery
	repackEvery = 0
	t.Cleanup(func() { repackEvery = saved })
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	pack := filepath.Join(c.dir(), "objects", "pack")
	partial := filepath.Join(pack, "tmp_pack_Ab12Cd")
	others := []string{filepath.Join(pack, "tmp_rev_Ef34Gh"), filepath.Join(pack, ".tmp-123-pack-0a1b.pack")}
	kept, err := filepath.Glob(filepath.Join(pack, "pack-*"))
	if err != nil || len(kept) == 0 {
		t.Fatalf("the clone holds no pack to keep: %v", err)
	}
	keep := strings.TrimSuffix(kept[0], filepath.Ext(kept[0])) + ".keep"
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, keep)
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		for _, path := range append([]string{partial}, others...) {
			if err := os.WriteFile(path, []byte("PACK half a pack\n"), 0o444); err != nil {
				return workspace.Result{}, err
			}
		}
		return workspace.Result{ExitCode: 128, Stderr: "fatal: killed\n"}, nil
	})
	advance(t, remote, "moved")
	partials := append([]string{partial}, others...)
	removal := func(path string) string {
		return "removed the partial pack " + filepath.Base(path) + ", which a killed git left"
	}
	settled := testwait.Holds(testwait.Bounded(t, 1<<60, testTimeout), testwait.Poll, func() bool {
		lines := c.log.all()
		for _, path := range partials {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || !logged(lines, removal(path)) {
				return false
			}
		}
		return true
	})
	lines := c.log.all()
	if settled {
		started, release := run.hold()
		testwait.Receive(t, "the fetch of the tick after the failed repack", started)
		lines = c.log.all()
		release()
	}
	// repack logs its failure before it calls removePartialPacks, so the line is there once any
	// removal's is.
	if !logged(lines, "repack failed; tried again in ") {
		t.Errorf("the failed repack was not logged: %q", lines)
	}
	for _, path := range partials {
		switch _, err := os.Stat(path); {
		case !errors.Is(err, os.ErrNotExist):
			t.Errorf("the partial file %s is still there (%v)", filepath.Base(path), err)
		case !logged(lines, removal(path)):
			t.Errorf("the partial file %s is gone, but the failed repack logged no removal of it: %q", filepath.Base(path), lines)
		case logged(lines, "removed the partial pack "+path+", last written "):
			t.Errorf("the failed repack logged the removal of the partial file %s, but the next fetch found it there and removed it: %q", filepath.Base(path), lines)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the removal took %s, which is no partial file: %v", filepath.Base(path), err)
		}
	}
	c.ready(t)
}

// A cache restarted on a volume where a killed repack left partial packs removes them before its
// first fetch, each logged.
func TestARestartedCacheRemovesThePartialPacksAKilledRepackLeft(t *testing.T) {
	remote := newRemote(t)
	opts := testOptions(t, newTestRunner(t, remote))
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil
	partial := filepath.Join(first.dir(), "objects", "pack", ".tmp-1234-pack-Zz99Yy.pack")
	if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	again := start(t, opts)
	again.ready(t)
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the partial pack outlived the restart (%v)", err)
	}
	if !logged(again.log.all(), "removed the partial pack .tmp-1234-pack-Zz99Yy.pack, which a killed git left") {
		t.Errorf("the removal was not logged: %q", again.log.all())
	}
}

// The cache's own first clone gets the repository-sized bound, workspace.CacheFetchTimeout, not an
// issue pod's FetchTimeout, and a fetch into the clone keeps FetchTimeout.
func TestTheCachesCloneTakesTheRepositorysBound(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	bound := make(chan time.Duration, 1)
	run.instead("clone", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		bound <- command.Timeout
		return run.runner.Run(ctx, command)
	})
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	c.ready(t)
	select {
	case got := <-bound:
		if got <= workspace.FetchTimeout || got > workspace.CacheFetchTimeout {
			t.Errorf("the cache's clone ran within %s, want past FetchTimeout %s and within CacheFetchTimeout %s", got, workspace.FetchTimeout, workspace.CacheFetchTimeout)
		}
	default:
		t.Fatal("the cache cloned with no command the runner saw")
	}
	refreshed := make(chan time.Duration, 1)
	run.instead("fetch", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		refreshed <- command.Timeout
		return run.runner.Run(ctx, command)
	})
	advance(t, remote, "moved")
	select {
	case got := <-refreshed:
		if got > workspace.FetchTimeout {
			t.Errorf("a fetch into the clone ran within %s, want FetchTimeout %s at most: it holds the lock every tar waits on", got, workspace.FetchTimeout)
		}
	case <-time.After(testTimeout):
		t.Fatal("the cache never fetched into its clone")
	}
}

// A snapshot taken while a repack swaps the clone's packs is taken again, and the tar it gives
// holds exactly the packs the clone holds then.
func TestASnapshotTakesThePacksARepackLeft(t *testing.T) {
	remote := newRemote(t)
	clone := filepath.Join(t.TempDir(), "widgets.git")
	git(t, filepath.Dir(clone), "clone", "--bare", "--quiet", remote, clone)
	git(t, clone, "--git-dir="+clone, "config", "fetch.unpackLimit", "1")
	advance(t, remote, "second pack")
	git(t, clone, "--git-dir="+clone, "fetch", "--quiet", remote, "+refs/heads/*:refs/heads/*")
	c := newCache(Options{Log: func(string) {}}, clone)
	c.narrowed = true
	calls := 0
	snapshotWalked = func() {
		calls++
		if calls == 1 {
			git(t, clone, "--git-dir="+clone, "repack", "-a", "-d", "--quiet")
		}
	}
	t.Cleanup(func() { snapshotWalked = nil })
	entries, err := c.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer closeEntries(entries)
	if calls != 2 {
		t.Errorf("the snapshot was taken %d times, want twice", calls)
	}
	var taken []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.header.Name, "objects/pack/pack-") {
			taken = append(taken, filepath.Base(entry.header.Name))
		}
	}
	var held []string
	for _, entry := range mustReadDir(t, filepath.Join(clone, "objects", "pack")) {
		if workspace.CacheEntry("objects/pack/"+entry, false) {
			held = append(held, entry)
		}
	}
	slices.Sort(taken)
	slices.Sort(held)
	if len(taken) == 0 || !slices.Equal(taken, held) {
		t.Errorf("the snapshot took %q, want the clone's %q", taken, held)
	}
}

// mustReadDir is the names in dir.
func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// cacheOn is a cache of opts that Serve has not started, for a test that drives refresh itself.
func cacheOn(t *testing.T, opts Options) *cache {
	t.Helper()
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(opts, dir)
	c.health.Store(&Health{Repo: opts.Repo.String()})
	return c
}

// repacks is how many repacks the runner was asked to run.
func (r *testRunner) repacks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, argv := range r.argv {
		if slices.Contains(argv, "repack") {
			n++
		}
	}
	return n
}

// A repack that fails is not tried at the next fetch, only a repackEvery after it: one that cannot
// fit never holds the cache from fetch to fetch.
func TestAFailedRepackIsTriedAgainOnlyARepackEveryLater(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	ctx := context.Background()
	if err := c.refresh(ctx); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	c.lastRepack = time.Now().Add(-repackEvery - time.Hour)
	old := c.lastRepack.UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(opts.Root, lastRepackFile), []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var held time.Time
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		held = recorded(t, opts.Root)
		return workspace.Result{ExitCode: 128, Stderr: "fatal: killed\n"}, nil
	})
	for i := range 2 {
		advance(t, remote, fmt.Sprintf("moved-%d", i))
		if err := c.refresh(ctx); err != nil {
			t.Fatalf("a refresh: %v", err)
		}
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("the runner was asked for %d repacks, want the one that failed and none at the next fetch", got)
	}
	if held.Before(time.Now().Add(-time.Hour)) {
		t.Errorf("the record held %s while the repack ran, want the attempt's own time written before it: a repack that kills its pod must not run again at the next start", held)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("a new process's refresh: %v", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("a new process after the failed repack asked for %d repacks in all, want none of its own", got)
	}
	if !logged(log.all(), "repack failed; tried again in "+repackEvery.String()+": ") {
		t.Errorf("the failed repack was not logged: %q", log.all())
	}
}

// A fetch that fails has the partial files its git left in objects/pack removed, each logged.
func TestAFailedFetchLeavesNoPartialPack(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	ctx := context.Background()
	if err := c.refresh(ctx); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	partial := filepath.Join(c.dir, "objects", "pack", "tmp_pack_Fe7ch0")
	run.instead("fetch", func(context.Context, workspace.Command) (workspace.Result, error) {
		if err := os.WriteFile(partial, []byte("PACK half a pack\n"), 0o444); err != nil {
			return workspace.Result{}, err
		}
		return workspace.Result{ExitCode: 128, Stderr: "fatal: write error: No space left on device\n"}, nil
	})
	advance(t, remote, "moved")
	if err := c.refresh(ctx); err == nil {
		t.Fatal("a refresh whose fetch failed answered nil")
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed fetch's partial pack is still there (%v)", err)
	}
	if !logged(log.all(), "removed the partial pack tmp_pack_Fe7ch0, which a killed git left") {
		t.Errorf("the removal was not logged: %q", log.all())
	}
}

// The time of the last repack outlives the process: a cache restarted after a repack is not due
// again until a repackEvery after it, and one restarted a repackEvery after it repacks at once.
func TestTheLastRepackOutlivesTheProcess(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	ctx := context.Background()
	before := time.Now().Add(-time.Second)
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the first process's refresh: %v", err)
	}
	if at := recorded(t, opts.Root); at.Before(before.Truncate(time.Second)) || at.After(time.Now()) {
		t.Fatalf("the first process recorded %s, want its first fetch's time, after %s", at, before)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the second process's refresh: %v", err)
	}
	if got := run.repacks(); got != 0 {
		t.Fatalf("a process restarted inside repackEvery repacked %d times", got)
	}
	old := time.Now().Add(-repackEvery - time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(opts.Root, lastRepackFile), []byte(old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cacheOn(t, opts).refresh(ctx); err != nil {
		t.Fatalf("the third process's refresh: %v", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("a process restarted a repackEvery after the last repack repacked %d times, want 1", got)
	}
}

// recorded is the time the cache's last-repack record under root holds.
func recorded(t *testing.T, root string) time.Time {
	t.Helper()
	_, at, there := timeFile(t, root, lastRepackFile, "last-repack record")
	if !there {
		t.Fatalf("read the last-repack record: %s is not under %s", lastRepackFile, root)
	}
	return at
}

// timeFile reads the time file name under root, which the cache writes as one RFC 3339 time and
// a line feed: its text, the time it holds, and whether it is there. what names the file in a
// failure.
func timeFile(t *testing.T, root, name, what string) (string, time.Time, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read the %s: %v", what, err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("the %s holds %q: %v", what, body, err)
	}
	return string(body), at, true
}

// volumeSeam stands in for readVolume with the figures a case sets: a volume of a size with so
// many bytes available, or, once fail is called, a statfs that fails. Every read must be of the
// cache's volume root, Options.Root.
type volumeSeam struct {
	t    *testing.T
	root string

	mu      sync.Mutex
	reading volumestat.Reading
	failing error
}

// seamVolume replaces readVolume, until the test ends, with a seam answering a volume at root of
// size bytes with available bytes left.
func seamVolume(t *testing.T, root string, size, available uint64) *volumeSeam {
	t.Helper()
	seam := &volumeSeam{t: t, root: root}
	seam.set(size, available)
	saved := readVolume
	readVolume = seam.read
	t.Cleanup(func() { readVolume = saved })
	return seam
}

func (s *volumeSeam) read(path string) (volumestat.Reading, error) {
	if path != s.root {
		s.t.Errorf("the cache read the volume at %s, want its root %s", path, s.root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != nil {
		return volumestat.Reading{}, s.failing
	}
	return s.reading, nil
}

// set has the seam answer a volume of size bytes with available bytes left. Its used bytes leave
// a reserve beside them, as a real filesystem's do (f_bfree against f_bavail): the volume less the
// used bytes is the free space, which is more than the available bytes, so a cache that took the
// free space for its room would answer more than every case sets, and the case would fail.
func (s *volumeSeam) set(size, available uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserve := min(seamReserve, size-available)
	s.reading = volumestat.Reading{SizeBytes: size, UsedBytes: size - available - reserve, AvailableBytes: available}
	s.failing = nil
}

// seamReserve is the space the seam's volume holds back from an unprivileged writer: the part of
// its free space that is not available.
const seamReserve = 1 << 30

// fail has the seam's statfs fail with err.
func (s *volumeSeam) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = err
}

// healthFields decodes a /healthz body into its Health and the names of the fields it holds, so a
// case tells a field that is absent from one that is there and zero.
func healthFields(t *testing.T, status int, body []byte) (int, Health, map[string]bool) {
	t.Helper()
	var health Health
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("/healthz answered %d with a body that is not its JSON: %v: %s", status, err, body)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("/healthz answered %d with a body that is not a JSON object: %v: %s", status, err, body)
	}
	fields := map[string]bool{}
	for name := range raw {
		fields[name] = true
	}
	return status, health, fields
}

// healthzOf is GET /healthz of c through its own handler, as Serve serves it.
func healthzOf(t *testing.T, c *cache) (int, Health, map[string]bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c.handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return healthFields(t, recorder.Code, recorder.Body.Bytes())
}

// healthFields is GET /healthz of the running Serve: its status, its body and the fields it holds.
func (c *running) healthFields(t *testing.T) (int, Health, map[string]bool) {
	t.Helper()
	response := c.get(t, "/healthz", c.bearer(t))
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	return healthFields(t, response.StatusCode, body)
}

// The lines git wrote when one deployment's cache ran out of room on 2026-11-19, as its log holds
// them: its fetch's write that found no room, its repack's, and its fetch's ref it could not write,
// which names neither of outOfSpacePhrases. The paths are the test clone's.
const fetchOutOfSpace = "fatal: write error: No space left on device"

func repackOutOfSpace(dir string) string {
	return "fatal: sha1 file '" + filepath.Join(dir, "objects", "pack") + "/tmp_pack_jrqcNe' write error. Out of diskspace"
}

func refNotWritten(dir string) string {
	return "error: cannot lock ref 'refs/heads/main': unable to create directory for " + filepath.Join(dir, "refs", "heads", "main")
}

// spaceFixture is one cache of acme/widgets that a case drives refresh by refresh, with the real
// git but for the step the case fails, its volume's figures the seam's, and /healthz read through
// the cache's own handler, or through the running Serve's once a case has restarted it.
type spaceFixture struct {
	remote  string
	run     *testRunner
	opts    Options
	log     *syncLog
	c       *cache
	volume  *volumeSeam
	serving *running
	// release lets the first fetch of a restarted Serve run, which a case holds (testRunner.hold).
	release func()
	// from and to bound the last refresh, and repacks is how many repacks the runner had been
	// asked for before it.
	from, to time.Time
	repacks  int
}

// newSpaceFixture is a fixture whose cache has cloned onto a volume of 20 GiB with 5 GiB left, so
// no failure counts for lack of space by its room alone.
func newSpaceFixture(t *testing.T) *spaceFixture {
	t.Helper()
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	f := &spaceFixture{remote: remote, run: run, opts: testOptions(t, run), log: &syncLog{}}
	f.opts.Log = f.log.log
	f.volume = seamVolume(t, f.opts.Root, 20<<30, 5<<30)
	f.c = cacheOn(t, f.opts)
	f.refresh(t, false)
	return f
}

// refresh runs one refresh of the cache, wanting it to fail or not, noting when it ran and how many
// repacks the runner had been asked for before it.
func (f *spaceFixture) refresh(t *testing.T, fails bool) {
	t.Helper()
	f.repacks = f.run.repacks()
	f.from = time.Now()
	err := f.c.refresh(context.Background())
	f.to = time.Now()
	if (err != nil) != fails {
		t.Fatalf("a refresh answered %v, want it failed %t; log %q", err, fails, f.log.all())
	}
}

// failFetch has the next refresh's fetch fail as git does, exiting exit having written stderr.
func (f *spaceFixture) failFetch(t *testing.T, exit int, stderr string) {
	t.Helper()
	f.run.instead("fetch", func(context.Context, workspace.Command) (workspace.Result, error) {
		return workspace.Result{ExitCode: exit, Stderr: stderr + "\n"}, nil
	})
	f.refresh(t, true)
}

// repackDue makes a repack due at the next refresh, as a repackEvery since the last one does.
func (f *spaceFixture) repackDue() {
	f.c.lastRepack = time.Now().Add(-repackEvery - time.Hour)
}

// failRepack has the next refresh repack, and its repack fail as git does, having written stderr.
func (f *spaceFixture) failRepack(t *testing.T, stderr string) {
	t.Helper()
	f.repackDue()
	f.run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		return workspace.Result{ExitCode: 128, Stderr: stderr + "\n"}, nil
	})
	f.refresh(t, false)
	if got := f.run.repacks() - f.repacks; got != 1 {
		t.Fatalf("the refresh asked for %d repacks, want the one that fails", got)
	}
}

// repackOK has the next refresh repack, and its repack succeed.
func (f *spaceFixture) repackOK(t *testing.T) {
	t.Helper()
	f.repackDue()
	f.refresh(t, false)
	if got := f.run.repacks() - f.repacks; got != 1 {
		t.Fatalf("the refresh asked for %d repacks, want one", got)
	}
}

// healthz is GET /healthz of the fixture's cache.
func (f *spaceFixture) healthz(t *testing.T) (int, Health, map[string]bool) {
	t.Helper()
	if f.serving != nil {
		return f.serving.healthFields(t)
	}
	return healthzOf(t, f.c)
}

// failedInTheRefresh is whether at lies within the last refresh.
func (f *spaceFixture) failedInTheRefresh(at time.Time) bool {
	return !at.Before(f.from) && !at.After(f.to)
}

// keptRepackFailure is the time repackOutOfSpaceFile under root holds, and whether it is there.
func keptRepackFailure(t *testing.T, root string) (time.Time, bool) {
	t.Helper()
	body, at, there := timeFile(t, root, repackOutOfSpaceFile, "kept repack failure")
	if there && !strings.HasSuffix(body, "Z\n") {
		t.Errorf("the kept repack failure holds %q, want one RFC 3339 UTC time and a line feed", body)
	}
	return at, there
}

// A fetch's and a repack's failure for lack of space reach /healthz, each with its step and its
// time, and each step's next success clears its own. A failure counts when git's output says so
// (`No space left on device` for the fetch, `Out of diskspace` for the repack, the lines a full
// cache logged) or when the volume had less than 1 GiB available as it failed; a failure naming
// neither, with 5 GiB available, counts for nothing. A recorded failure holds back no repack that
// is due. The volume's figures are read at every answer, so they move with the seam's and no fetch
// between, and a statfs that fails leaves them out and names its error. A fetch's failure is kept
// in memory alone: a restarted cache reports none until a fetch fails again.
func TestTheCacheReportsAFailureForLackOfSpace(t *testing.T) {
	none := func(*testing.T, *spaceFixture) {}
	fetchFailed := func(t *testing.T, f *spaceFixture) { f.failFetch(t, 128, fetchOutOfSpace) }
	repackFailed := func(t *testing.T, f *spaceFixture) { f.failRepack(t, repackOutOfSpace(f.c.dir)) }
	for _, tc := range []struct {
		name  string
		state func(*testing.T, *spaceFixture)
		event func(*testing.T, *spaceFixture)
		check func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool)
	}{
		{"a fetch fails No space left on device with 5 GiB available", none, fetchFailed,
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if status != http.StatusOK || !fields["fetchOutOfSpaceAt"] || !f.failedInTheRefresh(health.FetchOutOfSpaceAt) {
					t.Errorf("/healthz answers %d %+v, want ready with the fetch's failure within its refresh, %s to %s", status, health, f.from, f.to)
				}
				if fields["repackOutOfSpaceAt"] {
					t.Errorf("/healthz names a repack failure at %s, though no repack ran", health.RepackOutOfSpaceAt)
				}
			}},
		{"a repack fails Out of diskspace with 5 GiB available", none, repackFailed,
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if status != http.StatusOK || !fields["repackOutOfSpaceAt"] || !f.failedInTheRefresh(health.RepackOutOfSpaceAt) {
					t.Errorf("/healthz answers %d %+v, want ready with the repack's failure within its refresh, %s to %s", status, health, f.from, f.to)
				}
				if fields["fetchOutOfSpaceAt"] {
					t.Errorf("/healthz names a fetch failure at %s, though the fetch succeeded", health.FetchOutOfSpaceAt)
				}
				if kept, ok := keptRepackFailure(t, f.opts.Root); !ok || !kept.Equal(health.RepackOutOfSpaceAt.Truncate(time.Second)) {
					t.Errorf("%s holds %s (there: %t), want the failure's time %s to the second", repackOutOfSpaceFile, kept, ok, health.RepackOutOfSpaceAt)
				}
			}},
		{"a fetch fails naming neither phrase with 5 GiB available", none,
			func(t *testing.T, f *spaceFixture) { f.failFetch(t, 1, refNotWritten(f.c.dir)) },
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if fields["fetchOutOfSpaceAt"] || fields["repackOutOfSpaceAt"] {
					t.Errorf("/healthz answers %d %+v, want no failure for lack of space: git named none and 5 GiB were left", status, health)
				}
			}},
		{"a fetch fails naming neither phrase with 512 MiB available", none,
			func(t *testing.T, f *spaceFixture) {
				f.volume.set(20<<30, 512<<20)
				f.failFetch(t, 1, refNotWritten(f.c.dir))
			},
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if !fields["fetchOutOfSpaceAt"] || !f.failedInTheRefresh(health.FetchOutOfSpaceAt) {
					t.Errorf("/healthz answers %d %+v, want the fetch's failure within its refresh, %s to %s: under 1 GiB was left", status, health, f.from, f.to)
				}
			}},
		{"the next fetch after a failed one succeeds", fetchFailed,
			func(t *testing.T, f *spaceFixture) { f.refresh(t, false) },
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if fields["fetchOutOfSpaceAt"] {
					t.Errorf("/healthz answers %d %+v, want the fetch's failure cleared by the fetch that succeeded", status, health)
				}
			}},
		{"the next refresh after a failed fetch succeeds with a repack due", fetchFailed,
			func(t *testing.T, f *spaceFixture) {
				f.repackDue()
				f.refresh(t, false)
			},
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if got := f.run.repacks() - f.repacks; got != 1 {
					t.Errorf("the refresh asked for %d repacks, want the one that was due: a recorded failure holds none back", got)
				}
			}},
		{"the next repack after a failed one succeeds", repackFailed,
			func(t *testing.T, f *spaceFixture) { f.repackOK(t) },
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if fields["repackOutOfSpaceAt"] {
					t.Errorf("/healthz answers %d %+v, want the repack's failure cleared by the repack that succeeded", status, health)
				}
				if kept, ok := keptRepackFailure(t, f.opts.Root); ok {
					t.Errorf("%s still holds %s after a repack succeeded", repackOutOfSpaceFile, kept)
				}
			}},
		{"the volume's figures change with no fetch between",
			func(t *testing.T, f *spaceFixture) {
				repackFailed(t, f)
				fetchFailed(t, f)
			},
			func(t *testing.T, f *spaceFixture) { f.volume.set(40<<30, 21<<30) },
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if health.VolumeBytes == nil || *health.VolumeBytes != 40<<30 || health.AvailableBytes == nil || *health.AvailableBytes != 21<<30 {
					t.Errorf("/healthz answers %d %+v, want the seam's new figures, a 40 GiB volume with 21 GiB available", status, health)
				}
				if !fields["fetchOutOfSpaceAt"] || !fields["repackOutOfSpaceAt"] {
					t.Errorf("/healthz answers %d %+v, want both failures still named: neither step ran since", status, health)
				}
			}},
		{"the volume's statfs fails", fetchFailed,
			func(t *testing.T, f *spaceFixture) {
				f.volume.fail(errors.New("statfs " + f.opts.Root + ": input/output error"))
			},
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if fields["volumeBytes"] || fields["availableBytes"] {
					t.Errorf("/healthz answers %d %+v, want no figures from a statfs that failed", status, health)
				}
				if want := "statfs " + f.opts.Root + ": input/output error"; health.VolumeError != want {
					t.Errorf("/healthz names the volume's error %q, want %q", health.VolumeError, want)
				}
				if !fields["fetchOutOfSpaceAt"] {
					t.Errorf("/healthz answers %d %+v, want the fetch's failure still named", status, health)
				}
			}},
		{"the process restarts after a failed fetch", fetchFailed,
			func(t *testing.T, f *spaceFixture) {
				f.opts.Refresh = 50 * time.Millisecond
				started, release := f.run.hold()
				t.Cleanup(release)
				f.serving, f.release = start(t, f.opts), release
				<-started
			},
			func(t *testing.T, f *spaceFixture, status int, health Health, fields map[string]bool) {
				if status != http.StatusServiceUnavailable || fields["fetchOutOfSpaceAt"] {
					t.Errorf("the restarted cache's first answer, during its first fetch, is %d %+v, want 503 naming no fetch failure: the last process's was in its memory alone", status, health)
				}
				f.release()
				f.serving.ready(t)
				f.run.instead("fetch", func(context.Context, workspace.Command) (workspace.Result, error) {
					return workspace.Result{ExitCode: 128, Stderr: fetchOutOfSpace + "\n"}, nil
				})
				f.serving.waitFor(t, "naming the fetch that failed again", func(_ int, h Health) bool { return !h.FetchOutOfSpaceAt.IsZero() })
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpaceFixture(t)
			tc.state(t, f)
			tc.event(t, f)
			status, health, fields := f.healthz(t)
			if health.VolumeBytes != nil && health.VolumeError != "" {
				t.Errorf("/healthz answers %d %+v, both the volume's figures and a statfs error", status, health)
			}
			tc.check(t, f, status, health, fields)
		})
	}
}

// A repack's failure for lack of space outlives its process: a cache started on the volume of one
// whose repack failed so names that failure in its first /healthz answer, the 503 one during its
// first fetch, with the volume's figures beside it, and with no repack between. The control is the
// same start after a repack that succeeded since: its first answer names no repack failure, and the
// volume holds no repackOutOfSpaceFile.
func TestTheCacheKeepsARepackFailureAcrossARestart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repaired bool
	}{
		{"after a repack that failed for lack of space", false},
		{"after a repack that succeeded since", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpaceFixture(t)
			f.failRepack(t, repackOutOfSpace(f.c.dir))
			_, first, _ := healthzOf(t, f.c)
			if first.RepackOutOfSpaceAt.IsZero() {
				t.Fatalf("the first process answers %+v, naming no repack failure to keep", first)
			}
			failed := first.RepackOutOfSpaceAt.Truncate(time.Second)
			if tc.repaired {
				f.repackOK(t)
			}
			repacks := f.run.repacks()
			started, release := f.run.hold()
			t.Cleanup(release)
			again := start(t, f.opts)
			<-started

			status, health, fields := again.healthFields(t)
			if status != http.StatusServiceUnavailable || health.VolumeBytes == nil || health.AvailableBytes == nil {
				t.Errorf("the restarted cache's first answer, during its first fetch, is %d %+v, want 503 with the volume's figures", status, health)
			}
			if tc.repaired {
				if fields["repackOutOfSpaceAt"] {
					t.Errorf("the restarted cache's first answer names a repack failure at %s, though a repack succeeded since", health.RepackOutOfSpaceAt)
				}
				if kept, ok := keptRepackFailure(t, f.opts.Root); ok {
					t.Errorf("%s holds %s after a repack succeeded", repackOutOfSpaceFile, kept)
				}
			} else if !health.RepackOutOfSpaceAt.Equal(failed) {
				t.Errorf("the restarted cache's first answer names the repack's failure at %s, want the last process's %s", health.RepackOutOfSpaceAt, failed)
			}
			release()
			health = again.ready(t)
			want := failed
			if tc.repaired {
				want = time.Time{}
			}
			if !health.RepackOutOfSpaceAt.Equal(want) {
				t.Errorf("the restarted cache, ready, names a repack failure at %s, want %s (the zero time is none)", health.RepackOutOfSpaceAt, want)
			}
			if got := f.run.repacks(); got != repacks {
				t.Errorf("the runner was asked for %d repacks across the restart, want none: the failure is the last process's", got-repacks)
			}
		})
	}
}

// statFS is `stat -f -c '%S %b %f %a' dir`: the fundamental block size, which is the unit statfs
// counts its blocks in (f_frsize; %s is f_bsize, which need not be), the volume's blocks, its free
// blocks, and the blocks available to an unprivileged writer.
func statFS(t *testing.T, dir string) (unit, blocks, free, available uint64) {
	t.Helper()
	out, err := exec.Command("stat", "-f", "-c", "%S %b %f %a", dir).Output()
	if err != nil {
		t.Fatalf("stat -f %s: %v", dir, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 4 {
		t.Fatalf("stat -f %s printed %q, want four counts", dir, out)
	}
	counts := make([]uint64, 4)
	for i, field := range fields {
		if counts[i], err = strconv.ParseUint(field, 10, 64); err != nil {
			t.Fatalf("stat -f %s printed %q: %v", dir, out, err)
		}
	}
	return counts[0], counts[1], counts[2], counts[3]
}

// The cache reads its own volume with the real reader (readVolume is volumestat.Read) at Options.Root:
// on a temporary directory, the size /healthz answers is what `stat -f` prints as the fundamental
// block size times the blocks, which does not move, and the available bytes are that block size
// times the available blocks, which do. A shared disk's available bytes move in both directions
// while the other tests of the run write and delete, so a try reads `stat -f`, then /healthz, then
// `stat -f` again, and the answer must lie between the smaller and the larger of the two available
// readings, widened by readMargin on each side.
//
// Two tests elsewhere hold the two halves of taking the available bytes rather than the free ones,
// each without a disk: volumestat's TestTheVolumesBytesAreCountedInFragments holds the reader to
// f_bavail over f_bfree, and this package's seam (seamReserve), which replaces the reader, holds
// the cache to Reading.AvailableBytes over its size less its used bytes. Against the real disk this
// test holds the two together where the disk reserves more than the margin (its free blocks less
// its available ones, from the same `stat -f`): there a try passes only when the free bytes of both
// readings lie above the range, so a reader of the free blocks would fail it. Where the disk
// reserves no more than the margin, free and available cannot be told apart on it, and a try passes
// on the range alone, with a log line saying so.
//
// A try that misses (an answer outside the range, or a range that reaches the free bytes because
// the readings moved past the reserve) is kept as the last miss, and the test tries again: a write
// and a delete larger than the margin, with /healthz read between them, moves one window and not
// the next. A reader of a wrong statfs field or in a wrong unit is wrong by a fixed amount and so
// misses every try. After 20 tries with no try passing, the test fails naming the last miss. A
// cache with no clone yet answers 503, so the figures are in that answer too.
func TestTheCacheReadsItsVolume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the volume is read with statfs on Linux alone; on %s the reader refuses", runtime.GOOS)
	}
	const tries = 20
	root := t.TempDir()
	c := cacheOn(t, Options{Repo: ghrepo.MustParse("acme/widgets"), Root: root, Log: func(string) {}})
	last := ""
	for try := 1; try <= tries; try++ {
		unit, blocks, free, available := statFS(t, root)
		status, health, _ := healthzOf(t, c)
		unitAfter, blocksAfter, freeAfter, availableAfter := statFS(t, root)
		if status != http.StatusServiceUnavailable || health.VolumeError != "" || health.VolumeBytes == nil || health.AvailableBytes == nil {
			t.Fatalf("/healthz of a cache with no clone answers %d %+v, want 503 with the volume's figures", status, health)
		}
		if unitAfter != unit || blocksAfter != blocks {
			t.Fatalf("stat -f read %s as %d blocks of %d bytes and then %d blocks of %d: the volume's size moved", root, blocks, unit, blocksAfter, unitAfter)
		}
		if volume := unit * blocks; *health.VolumeBytes != volume {
			t.Fatalf("/healthz answered a volume of %d bytes, want %d blocks of %d bytes, %d, as stat -f read them before and after it", *health.VolumeBytes, blocks, unit, volume)
		}
		low := min(available, availableAfter) * unit
		high := max(available, availableAfter)*unit + readMargin
		low -= min(low, readMargin)
		got := *health.AvailableBytes
		answer := fmt.Sprintf("on try %d, /healthz answered %d bytes available, and stat -f read %d and then %d available blocks of %d bytes (%d and %d free)",
			try, got, available, availableAfter, unit, free, freeAfter)
		if got < low || got > high {
			last = fmt.Sprintf("%s: the answer is outside %d to %d, the two readings widened by %d", answer, low, high, readMargin)
			continue
		}
		if reserve := (min(free-available, freeAfter-availableAfter)) * unit; reserve <= readMargin {
			t.Logf("the filesystem of %s reserves %d bytes, not more than the margin of %d, so this disk cannot tell a reader of the available bytes from one of the free bytes; volumestat's TestTheVolumesBytesAreCountedInFragments holds the reader to f_bavail, and this package's seam (seamReserve) holds the cache to Reading.AvailableBytes",
				root, reserve, readMargin)
			return
		}
		if min(free, freeAfter)*unit > high {
			return
		}
		last = fmt.Sprintf("%s: the range %d to %d reaches the free bytes, so a reader of the free blocks would have passed this try", answer, low, high)
	}
	t.Fatalf("no try of %d passed; the last miss: %s", tries, last)
}

// readMargin widens the range of available bytes a try of TestTheCacheReadsItsVolume accepts on
// each side: what the other tests of a run can write and delete again between the two readings of
// one try. Where a disk reserves more than it, the test also tells a reader of the available bytes
// from one of the free bytes: an issue pod's volume held 16 MiB back on 2026-11-21 (stat -f: 4,096
// blocks of 4,096 bytes).
const readMargin = 4 << 20

func TestCacheNarrowsAnOldFullVolumeBeforeServing(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("the old cache's packs are %q (%v), want at least one", packs, err)
	}
	keep := strings.TrimSuffix(packs[0], ".pack") + ".keep"
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	c := start(t, opts)
	health := c.ready(t)
	if !health.DefaultBranchOnly {
		t.Errorf("/healthz = %+v, want defaultBranchOnly true", health)
	}
	assertDefaultBranchOnly(t, c.dir())
	if _, err := os.Stat(keep); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old cache's kept pack survived narrowing (%v)", err)
	}
	before, after := narrowedBytes(t, c.log.all(), c.dir())
	if before <= after {
		t.Errorf("narrowed %s from %d to %d bytes, want fewer bytes after the repack", c.dir(), before, after)
	}
	assertTarObjectsAreOnMain(t, c)
}

func TestCacheDeletesSymbolicRefsWithoutDereferencingThem(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "--git-dir="+dir, "symbolic-ref", "refs/heads/main-alias", "refs/heads/main")
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}

	c := start(t, opts)
	c.ready(t)
	assertDefaultBranchOnly(t, c.dir())
}

func TestCacheDoesNotServeAnOldFullVolumeBeforeNarrowing(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	started, release := holdRepack(t, run)
	defer release()

	c := start(t, opts)
	testwait.Receive(t, "the narrowing repack", started)
	for _, path := range []string{"/readyz", "/tar"} {
		response := c.get(t, path, c.bearer(t))
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s while narrowing answers %d, want 503", path, response.StatusCode)
		}
	}
	release()
	if health := c.ready(t); !health.DefaultBranchOnly {
		t.Errorf("/healthz after narrowing = %+v, want defaultBranchOnly true", health)
	}
}

func TestCacheNarrowsAnOldVolumeAfterAFailedRefreshBeforeFallback(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}

	c := start(t, opts)
	health := c.ready(t)
	if !health.DefaultBranchOnly {
		t.Errorf("/healthz after a failed refresh = %+v, want defaultBranchOnly true", health)
	}
	assertDefaultBranchOnly(t, c.dir())
	narrowedBytes(t, c.log.all(), c.dir())
	assertTarObjectsAreOnMain(t, c)
}

func TestCacheKeepsANarrowedVolumeWithoutAnotherRepack(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	first := start(t, opts)
	first.ready(t)
	assertDefaultBranchOnly(t, first.dir())
	packs := packNames(t, first.dir())
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil

	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	beforeRepacks := run.repacks()
	run.instead("fetch", func(context.Context, workspace.Command) (workspace.Result, error) {
		return workspace.Result{}, nil
	})
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("refreshing the narrowed volume: %v", err)
	}
	assertDefaultBranchOnly(t, first.dir())
	if got := packNames(t, first.dir()); !slices.Equal(got, packs) {
		t.Errorf("the narrowed volume's packs after a no-op refresh are %q, want %q", got, packs)
	}
	if got := run.repacks(); got != beforeRepacks {
		t.Errorf("the narrowed volume ran %d more repacks, want none", got-beforeRepacks)
	}
	if logged(log.all(), "narrowed "+first.dir()+" ") {
		t.Errorf("a no-op refresh logged another narrowing: %q", log.all())
	}
	if slices.Contains(run.transfers(), "clone --bare") {
		t.Errorf("the cache cloned after the old volume was laid down, want none: %q", run.transfers())
	}
}

func TestCacheRepacksAnUnmarkedDefaultBranchVolume(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(dir), "clone", "--bare", "--single-branch", "--quiet", remote, dir)
	assertDefaultBranchOnly(t, dir)

	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("refreshing the unmarked default-branch volume: %v", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("the unmarked default-branch volume ran %d repacks, want one", got)
	}
	if _, err := os.Stat(filepath.Join(opts.Root, defaultBranchOnlyFile)); err != nil {
		t.Errorf("the narrowed volume has no marker: %v", err)
	}
	narrowedBytes(t, log.all(), dir)
	status, health, fields := healthzOf(t, c)
	if status != http.StatusOK || !fields["defaultBranchOnly"] || !health.DefaultBranchOnly {
		t.Errorf("/healthz after narrowing the unmarked volume answers %d %+v with fields %v, want ready and defaultBranchOnly", status, health, fields)
	}
}

func TestCacheRefusesToMarkACloneWhoseHeadRefIsMissing(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("making the cache clone: %v", err)
	}
	if err := os.Remove(filepath.Join(opts.Root, defaultBranchOnlyFile)); err != nil {
		t.Fatal(err)
	}
	git(t, c.dir, "--git-dir="+c.dir, "update-ref", "-d", "refs/heads/main")

	err := c.narrow(context.Background(), false, false)
	if !errors.Is(err, errCacheHeadMissing) {
		t.Fatalf("narrowing a clone whose HEAD ref is gone = %v, want the missing-head refusal", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Root, defaultBranchOnlyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the missing-HEAD clone left a marker (%v), want none", err)
	}
}

func TestCacheFetchRepairsACloneWhoseHeadRefIsMissing(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(dir), "clone", "--bare", "--quiet", remote, dir)
	git(t, dir, "--git-dir="+dir, "update-ref", "-d", "refs/heads/main")

	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("fetching into the clone whose HEAD ref is missing: %v", err)
	}
	if status, health, _ := healthzOf(t, c); status != http.StatusOK || !health.DefaultBranchOnly {
		t.Errorf("/healthz after repairing the missing HEAD ref answers %d %+v, want ready and defaultBranchOnly", status, health)
	}
	assertDefaultBranchOnly(t, dir)
}

func TestCachePreservesAUnicodeWhitespaceDefaultBranch(t *testing.T) {
	remote := newRemote(t)
	branch := "release/stable "
	git(t, remote, "--git-dir="+remote, "branch", "-m", "main", branch)
	c := start(t, testOptions(t, newTestRunner(t, remote)))
	c.ready(t)
	if got, want := cacheRefs(t, c.dir()), []string{"refs/heads/" + branch}; !slices.Equal(got, want) {
		t.Errorf("the cache's refs are %q, want %q", got, want)
	}
}

func TestCacheRepacksAfterTheDefaultBranchHistoryIsRewritten(t *testing.T) {
	remote := newRemote(t)
	old := advance(t, remote, "old")
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Refresh = 50 * time.Millisecond
	c := start(t, opts)
	if health := c.ready(t); health.Head != old {
		t.Fatalf("the cache serves %s, want %s before rewriting the default branch", health.Head, old)
	}
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	git(t, work, "reset", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(work, "rewritten.txt"), []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "rewritten")
	git(t, work, "push", "--quiet", "--force", "origin", "HEAD:main")
	rewritten := git(t, work, "rev-parse", "HEAD")

	c.waitFor(t, "at the rewritten default branch", func(_ int, health Health) bool { return health.Head == rewritten })
	narrowedBytes(t, c.log.all(), c.dir())
	assertTarObjectsAreOnMain(t, c)
}

func TestCacheRepacksAnInterruptedDefaultBranchFetch(t *testing.T) {
	remote := newRemote(t)
	advance(t, remote, "old")
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	first := start(t, opts)
	first.ready(t)
	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatalf("the first Serve ended with %v", err)
	}
	first.done <- nil

	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	git(t, work, "reset", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(work, "rewritten.txt"), []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "rewritten")
	git(t, work, "push", "--quiet", "--force", "origin", "HEAD:main")
	git(t, first.dir(), "--git-dir="+first.dir(), "fetch", "--quiet", "--no-tags", "file://"+remote, "+refs/heads/main:refs/heads/main")
	if err := cacheOn(t, opts).mark(defaultBranchFetchFile); err != nil {
		t.Fatal(err)
	}

	again := start(t, opts)
	again.ready(t)
	if _, err := os.Stat(filepath.Join(opts.Root, defaultBranchFetchFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the recovered cache kept the interrupted-fetch marker (%v)", err)
	}
	if got := run.repacks(); got != 1 {
		t.Errorf("the interrupted fetch recovery ran %d repacks, want one", got)
	}
	narrowedBytes(t, again.log.all(), again.dir())
	assertTarObjectsAreOnMain(t, again)
}

// A fetch marker outlives the clone it guarded when a cache process removes that clone. The first
// clone that replaces it must clear the stale marker instead of treating it as an interrupted fetch
// and repacking the already single-branch clone.
func TestCacheClearsAnInterruptedFetchMarkerBeforeAFirstClone(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	first := cacheOn(t, opts)
	if err := first.refresh(context.Background()); err != nil {
		t.Fatalf("making the first cache clone: %v", err)
	}
	if err := first.mark(defaultBranchFetchFile); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first.dir); err != nil {
		t.Fatal(err)
	}

	again := cacheOn(t, opts)
	if err := again.refresh(context.Background()); err != nil {
		t.Fatalf("making the replacement cache clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Root, defaultBranchFetchFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the replacement clone kept the interrupted-fetch marker (%v)", err)
	}
	if got := run.repacks(); got != 0 {
		t.Errorf("the replacement default-branch clone ran %d repacks, want none", got)
	}
	assertDefaultBranchOnly(t, again.dir)
}

func TestCacheDoesNotPublishAMarkerBeforeValidatingTheClone(t *testing.T) {
	remote := newRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	if err := cacheOn(t, opts).refresh(context.Background()); err != nil {
		t.Fatalf("making the cache clone: %v", err)
	}
	run.mu.Lock()
	before := len(run.argv)
	run.mu.Unlock()

	restarted := cacheOn(t, opts)
	if err := restarted.publish(context.Background(), time.Time{}); !errors.Is(err, errDefaultBranchOnly) {
		t.Errorf("publishing before ownership and narrowing checks = %v, want an unavailable cache", err)
	}
	if status, health, _ := healthzOf(t, restarted); status != http.StatusServiceUnavailable || health.Ready {
		t.Errorf("/healthz before validation answers %d %+v, want 503 and not ready", status, health)
	}
	run.mu.Lock()
	after := len(run.argv)
	run.mu.Unlock()
	if after != before {
		t.Errorf("publication ran %d git commands before validating ownership, want none", after-before)
	}
}

func TestCacheInvalidatesItsMarkerWhenRenarrowingFails(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	opts.Log = func(string) {}
	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("making the cache clone: %v", err)
	}
	// A default-branch change can leave both the old and new branch until narrowing finishes.
	git(t, c.dir, "--git-dir="+c.dir, "fetch", "--no-tags", "file://"+remote, "+refs/heads/cold/x:refs/heads/cold/x")
	c.streams = make(chan struct{}, 1)
	c.streams <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/tar", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testCacheToken)
	queued := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		c.handler().ServeHTTP(recorder, request)
		queued <- recorder.Code
	}()
	if !testwait.Holds(testTimeout, testwait.Poll, func() bool { return c.inFlight.Load() == 1 }) {
		t.Fatal("the tar request never queued behind the stream bound")
	}
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		return workspace.Result{ExitCode: 128, Stderr: "fatal: the narrowing repack failed\n"}, nil
	})
	refreshErr := c.refresh(context.Background())
	<-c.streams
	if refreshErr == nil || !strings.Contains(refreshErr.Error(), "repack the narrowed cache") {
		t.Fatalf("renarrowing the cache = %v, want the repack failure", refreshErr)
	}
	select {
	case status := <-queued:
		if status != http.StatusServiceUnavailable {
			t.Errorf("the tar queued before failed narrowing answers %d, want 503", status)
		}
	case <-ctx.Done():
		t.Fatal("the queued tar did not finish after failed narrowing")
	}
	if _, err := os.Lstat(filepath.Join(opts.Root, defaultBranchOnlyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the failed narrowing left its old marker (%v), want none", err)
	}
	if err := c.publish(context.Background(), time.Time{}); !errors.Is(err, errDefaultBranchOnly) {
		t.Errorf("fallback publication after failed narrowing = %v, want an unavailable cache", err)
	}
	if status, health, fields := healthzOf(t, c); status != http.StatusServiceUnavailable || health.Ready || fields["defaultBranchOnly"] {
		t.Errorf("/healthz after failed narrowing answers %d %+v with fields %v, want 503 without defaultBranchOnly", status, health, fields)
	}
	restarted := cacheOn(t, opts)
	if err := restarted.refresh(context.Background()); err != nil {
		t.Fatalf("retrying narrowing after a restart: %v", err)
	}
	if got := run.repacks(); got != 2 {
		t.Errorf("the failed narrowing and its retry ran %d repacks, want two", got)
	}
	if status, health, _ := healthzOf(t, restarted); status != http.StatusOK || !health.DefaultBranchOnly {
		t.Errorf("/healthz after retrying narrowing answers %d %+v, want ready and defaultBranchOnly", status, health)
	}
}

func TestCacheKeepsAnUnnarrowableOldVolumeUnavailable(t *testing.T) {
	remote := fullRemote(t)
	run := newTestRunner(t, remote)
	opts := testOptions(t, run)
	oldCache(t, opts, remote)
	repack := make(chan struct{})
	run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
		close(repack)
		return workspace.Result{ExitCode: 128, Stderr: "fatal: the narrowing repack failed\n"}, nil
	})

	c := start(t, opts)
	testwait.Receive(t, "the failed narrowing repack", repack)
	if !testwait.Holds(testwait.Bounded(t, 1<<60, testTimeout), testwait.Poll, func() bool {
		return logged(c.log.all(), "cache remains unavailable: ")
	}) {
		t.Fatalf("the failed narrowing did not name why the cache stayed unavailable: %q", c.log.all())
	}
	status, health := c.health(t)
	if status != http.StatusServiceUnavailable || health.Ready {
		t.Errorf("/healthz after a failed narrowing answers %d %+v, want 503 and not ready", status, health)
	}
	for _, path := range []string{"/readyz", "/tar"} {
		response := c.get(t, path, c.bearer(t))
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s after a failed narrowing answers %d, want 503", path, response.StatusCode)
		}
	}
}

// An old volume's narrowing repack can fail because its volume has no room: git says so, or less
// than 1 GiB is available when it fails. Removing that clone makes the next refresh clone the
// default branch afresh instead of leaving the cache unready for every retry of the same repack.
// Another narrowing failure, with room left, keeps the clone for the next retry.
func TestCacheRemovesAnOldVolumeWhenNarrowingRunsOutOfSpace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gitNoSpace  bool
		available   uint64
		wantRemoved bool
	}{
		{"git reports no space", true, 5 << 30, true},
		{"the volume has under 1 GiB", false, 512 << 20, true},
		{"another failure with room left", false, 5 << 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := fullRemote(t)
			run := newTestRunner(t, remote)
			opts := testOptions(t, run)
			log := &syncLog{}
			opts.Log = log.log
			seamVolume(t, opts.Root, 20<<30, tc.available)
			oldCache(t, opts, remote)
			c := cacheOn(t, opts)
			stderr := "fatal: the narrowing repack failed"
			if tc.gitNoSpace {
				stderr = repackOutOfSpace(c.dir)
			}
			run.instead("repack", func(context.Context, workspace.Command) (workspace.Result, error) {
				return workspace.Result{ExitCode: 128, Stderr: stderr + "\n"}, nil
			})

			if err := c.refresh(context.Background()); err == nil {
				t.Fatal("narrowing an old volume answered nil, want its repack failure")
			}
			_, statErr := os.Stat(c.dir)
			removedLine := logged(log.all(), "removed the cache clone after its narrowing repack ran out of space; the next fetch clones it afresh")
			if !tc.wantRemoved {
				if statErr != nil || removedLine {
					t.Fatalf("the clone after a narrowing failure with room left is %v, removal logged %t; want it kept: %q", statErr, removedLine, log.all())
				}
				return
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the clone after its out-of-space narrowing repack is %v, want removed", statErr)
			}
			if !removedLine {
				t.Errorf("the out-of-space narrowing did not log the clone removal: %q", log.all())
			}

			if err := c.refresh(context.Background()); err != nil {
				t.Fatalf("refreshing after the out-of-space clone removal: %v", err)
			}
			assertDefaultBranchOnly(t, c.dir)
			if _, err := os.Stat(filepath.Join(opts.Root, defaultBranchOnlyFile)); err != nil {
				t.Errorf("the fresh default-branch clone has no marker: %v", err)
			}
			if got := run.repacks(); got != 1 {
				t.Errorf("the cache ran %d repacks, want only the failed narrowing repack", got)
			}
			if !slices.Contains(run.transfers(), "clone --bare") {
				t.Errorf("the cache did not clone after removing the old volume: %q", run.transfers())
			}
		})
	}
}

func fullRemote(t *testing.T) string {
	t.Helper()
	remote := newRemote(t)
	branchCommit(t, remote, "legion/K", "issue", []byte("the issue branch\n"))
	cold := make([]byte, 2<<20)
	if _, err := rand.Read(cold); err != nil {
		t.Fatal(err)
	}
	coldCommit := branchCommit(t, remote, "cold/x", "cold", cold)
	git(t, remote, "--git-dir="+remote, "tag", "cold-tag", coldCommit)
	return remote
}

func branchCommit(t *testing.T, remote, branch, name string, body []byte) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	git(t, filepath.Dir(work), "clone", "--quiet", remote, work)
	git(t, work, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, name+".bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", name)
	git(t, work, "push", "--quiet", "origin", "HEAD:"+branch)
	return git(t, work, "rev-parse", "HEAD")
}

func oldCache(t *testing.T, opts Options, remote string) {
	t.Helper()
	dir, err := workspace.CacheRepository(opts.Root, opts.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, remote, "--git-dir="+remote, "repack", "-a", "-d", "--quiet")
	git(t, filepath.Dir(dir), "clone", "--bare", "--quiet", remote, dir)
	if got, want := cacheRefs(t, dir), []string{"refs/heads/cold/x", "refs/heads/legion/K", "refs/heads/main", "refs/tags/cold-tag"}; !slices.Equal(got, want) {
		t.Fatalf("the old cache's refs are %q, want %q", got, want)
	}
}

func cacheRefs(t *testing.T, dir string) []string {
	t.Helper()
	refs := strings.TrimSuffix(fixturegit.Isolated(t, t.TempDir()).Raw(dir, "git", "--git-dir="+dir, "for-each-ref", "--format=%(refname)"), "\n")
	if refs == "" {
		return nil
	}
	return strings.Split(refs, "\n")
}

func assertDefaultBranchOnly(t *testing.T, dir string) {
	t.Helper()
	if got, want := cacheRefs(t, dir), []string{"refs/heads/main"}; !slices.Equal(got, want) {
		t.Fatalf("the cache's refs are %q, want %q", got, want)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(dir, "HEAD")))); got != "ref: refs/heads/main" {
		t.Fatalf("the cache's HEAD is %q, want refs/heads/main", got)
	}
}

func narrowedBytes(t *testing.T, lines []string, dir string) (int64, int64) {
	t.Helper()
	prefix := "narrowed " + dir + " "
	var found []string
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the cache logged %d narrowing lines for %s, want one: %q", len(found), dir, lines)
	}
	fields := strings.Fields(found[0])
	if len(fields) != 4 {
		t.Fatalf("the cache logged %q, want narrowed <dir> <bytes before> <bytes after>", found[0])
	}
	before, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatalf("the cache logged %q, whose before size is not a byte count: %v", found[0], err)
	}
	after, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		t.Fatalf("the cache logged %q, whose after size is not a byte count: %v", found[0], err)
	}
	return before, after
}

func assertTarObjectsAreOnMain(t *testing.T, c *running) {
	t.Helper()
	status, body := c.getTar(t)
	if status != http.StatusOK {
		t.Fatalf("/tar answers %d, want 200", status)
	}
	unpacked, _ := unpack(t, bytes.NewReader(body))
	objects := map[string]bool{}
	for _, line := range strings.Split(git(t, unpacked, "--git-dir="+unpacked, "rev-list", "--objects", "main"), "\n") {
		if fields := strings.Fields(line); len(fields) != 0 {
			objects[fields[0]] = true
		}
	}
	indexes, err := filepath.Glob(filepath.Join(unpacked, "objects", "pack", "pack-*.idx"))
	if err != nil || len(indexes) == 0 {
		t.Fatalf("the tar holds indexes %q (%v), want at least one", indexes, err)
	}
	for _, index := range indexes {
		file, err := os.Open(index)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("git", "show-index")
		command.Dir, command.Env, command.Stdin = unpacked, fixtureenv.Isolated(t.TempDir()), file
		out, err := command.Output()
		closeErr := file.Close()
		if err != nil {
			t.Fatalf("git show-index %s: %v", index, err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				t.Fatalf("git show-index %s printed %q, want offset and object id", index, line)
			}
			if !objects[fields[1]] {
				t.Errorf("the streamed index %s holds %s, which main does not reach", filepath.Base(index), fields[1])
			}
		}
	}
}

func packNames(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	return names
}

func holdRepack(t *testing.T, run *testRunner) (<-chan struct{}, func()) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	run.instead("repack", func(ctx context.Context, command workspace.Command) (workspace.Result, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return workspace.Result{}, ctx.Err()
		}
		return run.runner.Run(ctx, command)
	})
	return started, func() { once.Do(func() { close(release) }) }
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
