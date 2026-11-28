package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testport"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const winitRepo = "acme/widgets"

// winitHelper is the credential helper every provisioned clone gets; a test function's local
// `workspace` path shadows the package where the clone is checked.
const winitHelper = workspace.GitHubCredentialHelper

// fakeJJ is the jj first on the issue volume's PATH. It records every invocation as one line,
// "<WINIT_TAG> <argv>" without the runner's leading --config pin, and runs the real jj with that
// pin — with WINIT_HOLD set, the `jj git init --colocate` that completes the directly filled clone
// opens the directory's `release` fifo, writes its pid to its `held` fifo, waits on `release` until
// the test closes its end there, and exits without initializing. A fresh provisioning's first jj
// command is that init, so a process held there is mid-provisioning. WINIT_HOLD is empty for every
// other case, which therefore holds nowhere.
const fakeJJ = `#!/bin/sh
pin=
case "$1" in --config=*) pin=$1; shift ;; esac
printf '%s %s\n' "$WINIT_TAG" "$*" >> "$WINIT_JJ_LOG"
if [ -n "$WINIT_HOLD" ] && [ "$1 $2 $3" = "git init --colocate" ]; then
	exec 3< "$WINIT_HOLD/release"
	printf '%s' "$$" > "$WINIT_HOLD/held"
	read _ <&3
	exit 1
fi
PATH="${PATH#*:}" exec "$WINIT_REAL_JJ" ${pin:+"$pin"} "$@"
`

// fakeGit maps GitHub to the fixture remote while recording the one-shot provisioning credential:
// a fetch from github.com/acme/widgets into a repository — an issue pod's fill's, or the repository
// cache's first fill's and refresh's — and a read of its HEAD (`ls-remote --symref`) each reach the
// local bare remote instead, whose file transport joins the https the runner allows, after
// appending the token file's path and what it held to WINIT_CREDENTIAL_LOG. A fetch first sleeps
// WINIT_FETCH_SLEEP seconds when that is set. With WINIT_FETCH_FAILURES set (failFetches), a fetch
// then counts itself in that directory's `attempts`, and an attempt whose number names a file there
// fails as git does: it writes the file to stderr with @TOKEN@ replaced by the token it was handed,
// and exits with the status the directory's `status` file holds. Every other invocation is the real
// git's.
const fakeGit = `#!/bin/sh
while [ "$1" = "-c" ]; do
	shift 2
done
if [ "$2" = fetch ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	if [ -n "$WINIT_FETCH_SLEEP" ]; then
		sleep "$WINIT_FETCH_SLEEP"
	fi
	if [ -n "$WINIT_FETCH_FAILURES" ]; then
		attempt=$(( $(cat "$WINIT_FETCH_FAILURES/attempts" 2>/dev/null || echo 0) + 1 ))
		echo "$attempt" > "$WINIT_FETCH_FAILURES/attempts"
		if [ -f "$WINIT_FETCH_FAILURES/$attempt" ]; then
			sed "s/@TOKEN@/$(cat "$LEGION_PROVISIONING_TOKEN_FILE")/g" "$WINIT_FETCH_FAILURES/$attempt" >&2
			exit "$(cat "$WINIT_FETCH_FAILURES/status")"
		fi
	fi
	first="$1"
	shift 2
	while [ "$1" = "--no-tags" ] || [ "$1" = "--prune" ] || [ "$1" = "--quiet" ]; do
		shift
	done
	shift
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$first" fetch --no-tags --prune --quiet "$WINIT_REMOTE" "$@"
fi
if [ "$2 $3 $4 $5" = "ls-remote --symref https://github.com/acme/widgets HEAD" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	first="$1"
	shift 5
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$first" ls-remote --symref "$WINIT_REMOTE" HEAD "$@"
fi
exec "$WINIT_REAL_GIT" "$@"
`

// fakeCodegraph is a recording `codegraph` stub first on the issue volume's PATH: it never does
// anything but log its own argv, so workspace-init calling it at all is the regression this
// guards, caught at once rather than by a timeout. Every real codegraph invocation runs with
// codegraphEnvironment()'s minimal environment (PATH/HOME/TMPDIR/DO_NOT_TRACK only), so the log
// path can't come from the test's own environment the way the jj/git stubs' do: it is derived
// from the stub's own invocation path ($0 is "<dir>/bin/codegraph"; stripping "/bin/codegraph"
// leaves "<dir>", matching v.codegraphLog).
const fakeCodegraph = `#!/bin/sh
printf '%s\n' "$*" >> "${0%/bin/codegraph}/codegraph.log"
`

// issueVolume is one issue pod's volume and environment: a PATH whose git maps fixture GitHub to
// a local bare remote, its mounted provisioning token, a private TMPDIR for one-shot credentials,
// and an empty Jujutsu config home. env is the environment each run of the command reads over
// proc's (process); proc is the rest of what a run inside this test process reads instead of the
// process's own (workspaceProcess: the clock, the fill's bound, the runner), which a test sets on
// it. A run takes them from the volume, never from the process, so the tests that run the command
// run beside each other.
type issueVolume struct {
	root, token, jjLog, credentialLog, codegraphLog, realJJ, tmp, bin string
	defaultBranch                                                     string
	env                                                               map[string]string
	proc                                                              workspaceProcess
}

func newIssueVolume(t *testing.T) *issueVolume {
	t.Helper()
	realJJ := fixturegit.CompiledJJ(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("workspace-init's tests drive a real git: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	tmp := filepath.Join(dir, "tmp")
	for _, made := range []string{bin, tmp} {
		if err := os.Mkdir(made, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{"jj": fakeJJ, "git": fakeGit, "codegraph": fakeCodegraph} {
		if err := execfile.Write(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(dir, "provision-token")
	if err := os.WriteFile(token, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "legion")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	v := &issueVolume{
		root: root, token: token, jjLog: filepath.Join(dir, "jj.log"),
		credentialLog: filepath.Join(dir, "credential.log"), codegraphLog: filepath.Join(dir, "codegraph.log"),
		realJJ: realJJ, tmp: tmp, bin: bin, defaultBranch: "main", proc: processWorkspace(),
	}
	// The standard logger stays the process's own: the tests beside this one write it too.
	v.proc.standardLog = false
	v.env = map[string]string{
		"PATH":                 bin + string(filepath.ListSeparator) + os.Getenv("PATH"),
		"TMPDIR":               tmp,
		"WINIT_REAL_JJ":        realJJ,
		"WINIT_REAL_GIT":       realGit,
		"WINIT_REMOTE":         filepath.Join(dir, "no-remote.git"),
		"WINIT_JJ_LOG":         v.jjLog,
		"WINIT_CREDENTIAL_LOG": v.credentialLog,
		"WINIT_FETCH_FAILURES": "",
		"WINIT_FETCH_SLEEP":    "",
		"WINIT_TAG":            "",
		"WINIT_HOLD":           "",
		"JJ_USER":              "Legion test",
		"JJ_EMAIL":             "legion-test@example.invalid",
		"XDG_CONFIG_HOME":      filepath.Join(dir, "config"),
		"JJ_CONFIG":            filepath.Join(dir, "no-user-config.toml"),
	}
	return v
}

// failFetches has the volume's git fail its first fetches from github.com/acme/widgets, one per
// entry of stderr, in order, each exiting status (fakeGit), and returns the file that counts every
// fetch attempt.
func (v *issueVolume) failFetches(t *testing.T, status int, stderr ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(strconv.Itoa(status)), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt, text := range stderr {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(attempt+1)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v.env["WINIT_FETCH_FAILURES"] = dir
	return filepath.Join(dir, "attempts")
}

// fetchAttempts is how many fetches from github.com/acme/widgets the volume's git counted in
// attempts, the file failFetches returned.
func fetchAttempts(t *testing.T, attempts string) int {
	t.Helper()
	body, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatalf("git ran no fetch from GitHub: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func printedBytes(t *testing.T, value string) int64 {
	t.Helper()
	if !regexp.MustCompile(`^[0-9]{1,3}(,[0-9]{3})*$`).MatchString(value) {
		t.Fatalf("%q is not a grouped byte count", value)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(value, ",", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// narrowedCacheTarServer serves the default-branch-only tar a current repository cache publishes.
func (v *issueVolume) narrowedCacheTarServer(t *testing.T) (string, int) {
	return v.cacheTarServer(t, cacheFault{})
}

// cacheFault is what a test's repository cache does wrong, nothing when it is zero.
// afterFirstHealth runs as the first /healthz is asked, which answers ready without
// defaultBranchOnly, as an earlier release's cache does. notReady answers every /healthz 503, as a
// cache with no clone yet does. damagedPack flips one byte in the middle of the tar's pack, so the
// pack fails its checksum. fullPack puts a symbolic link to /dev/full at the path of the tar's pack
// in the git directory of the clone provision is filling, once the /tar request's token is checked
// and before the tar streams: the open of the pack follows the link, so the write of the pack to
// the volume fails inside its copy.
type cacheFault struct {
	afterFirstHealth                func()
	notReady, damagedPack, fullPack bool
}

// cacheTarServer serves, as the repository cache's GET /healthz and GET /tar, a ready cache of the
// volume's remote's default branch, with fault, and returns its address and the tar's length. Any
// other path, /packs among them, is the mux's plain 404 naming no repository, as a cache with no
// bucket answers it.
func (v *issueVolume) cacheTarServer(t *testing.T, fault cacheFault) (string, int) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "widgets.git")
	if output, err := v.runner(t).Try(filepath.Dir(cache), v.env["WINIT_REAL_GIT"], "clone", "--bare", "--single-branch", "--no-tags", "--quiet", "--no-local", v.env["WINIT_REMOTE"], cache); err != nil {
		t.Fatalf("clone cache: %v\n%s", err, output)
	}
	pack := ""
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == cache {
			return err
		}
		relative, err := filepath.Rel(cache, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !workspace.CacheEntry(name, entry.IsDir()) {
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
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, ".pack") {
			pack = name
			if fault.damagedPack {
				body[len(body)/2] ^= 0xff
			}
		}
		if err := archive.WriteHeader(header); err != nil {
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
	if pack == "" {
		t.Fatalf("the cache %s holds no pack", cache)
	}
	body := stream.Bytes()
	waited := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, winitRepo)
		switch r.URL.Path {
		case "/healthz":
			if fault.notReady {
				http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
				return
			}
			if !waited && fault.afterFirstHealth != nil {
				waited = true
				fault.afterFirstHealth()
				_, _ = w.Write([]byte(`{"ready":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"ready":true,"defaultBranchOnly":true}`))
		case "/tar":
			if r.Header.Get("Authorization") != "Bearer "+cmdCacheToken {
				w.Header().Del(workspace.CacheRepositoryHeader)
				http.Error(w, "the repository cache token is required", http.StatusUnauthorized)
				return
			}
			if fault.fullPack {
				if err := v.linkToDevFull(pack); err != nil {
					t.Errorf("put /dev/full at the clone's pack: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			_, _ = w.Write(body)
		default:
			w.Header().Del(workspace.CacheRepositoryHeader)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, len(body)
}

// linkToDevFull puts a symbolic link to /dev/full at entry, a path inside the git directory of the
// one temporary sibling of the volume's clone provision is filling (<clone>.clone-*).
func (v *issueVolume) linkToDevFull(entry string) error {
	staging, err := filepath.Glob(v.clone() + ".clone-*")
	if err != nil {
		return err
	}
	if len(staging) != 1 {
		return fmt.Errorf("the clone's directory holds %d temporary siblings %q, want provision's one", len(staging), staging)
	}
	link := filepath.Join(staging[0], ".git", filepath.FromSlash(entry))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	return os.Symlink("/dev/full", link)
}

// runner runs one of the volume's own fixture commands — a git or jj that builds its remote or
// reads one of its repositories, never the command under test. Every such command wants the same
// environment, which is this one: fixtureenv.Isolated of a home of its own, so it reaches no
// workspace, repository, remote, credential or configuration of whoever runs the tests.
func (v *issueVolume) runner(t *testing.T) fixturegit.Runner {
	t.Helper()
	return fixturegit.Isolated(t, t.TempDir())
}

// withRemote gives the volume's git a real repository to clone: a bare remote whose default branch,
// defaultBranch, holds one commit, as github.com/acme/widgets would.
func (v *issueVolume) withRemote(t *testing.T, defaultBranch string) *issueVolume {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	for _, argv := range [][]string{
		fixturegit.InitBare(remote, defaultBranch),
		append([]string{v.realJJ}, fixturegit.JJInit(seed, true)[1:]...),
		{v.realJJ, "describe", "-m", "seed", "-R", seed},
		{v.realJJ, "bookmark", "set", defaultBranch, "-r", "@", "-R", seed},
		{v.realJJ, "git", "remote", "add", "origin", remote, "-R", seed},
		{v.realJJ, "git", "push", "--remote", "origin", "--bookmark", defaultBranch, "-R", seed},
	} {
		v.runner(t).Run(dir, argv...)
	}
	v.env["WINIT_REMOTE"] = remote
	v.defaultBranch = defaultBranch
	return v
}

func (v *issueVolume) clone() string {
	return filepath.Join(v.root, "repos", "github.com", "acme", "widgets")
}

func (v *issueVolume) workspace(issue string) string {
	return filepath.Join(v.root, "workspaces", "acme", "widgets", strings.ToLower(issue))
}

func (v *issueVolume) args(issue string) []string {
	return []string{"provision", "--issue", issue, "--repo", winitRepo, "--root", v.root, "--credential-helper", winitHelper, "--default-branch", v.defaultBranch}
}

// runtimeOptionalEnv are the variables a runtime sets on an init container, or on a cache's
// container, only for some launches; no run in these tests inherits them from whoever runs the
// tests.
var runtimeOptionalEnv = []string{"LEGION_PROVISION_TOKEN_FILE", "LEGION_REPOSITORY_CACHE_URL", "LEGION_REPOSITORY_CACHE_TOKEN_FILE", "LEGION_REPOSITORY_CACHE_BUCKET"}

// process is what a run of the command inside this test process reads instead of the process's
// own (runIn): proc, with its environment less runtimeOptionalEnv and with the volume's env over it
// (a test sets a variable by writing env, and unsets one of runtimeOptionalEnv by deleting it).
func (v *issueVolume) process() workspaceProcess {
	p := v.proc
	p.environ = make([]string, 0, len(v.proc.environ)+len(v.env))
	for _, entry := range v.proc.environ {
		name, _, _ := strings.Cut(entry, "=")
		if _, set := v.env[name]; !set && !slices.Contains(runtimeOptionalEnv, name) {
			p.environ = append(p.environ, entry)
		}
	}
	for name, value := range v.env {
		p.environ = append(p.environ, name+"="+value)
	}
	return p
}

// provisionEnv points the one init container at the mounted provisioning token.
func (v *issueVolume) provisionEnv() {
	v.env[provisionTokenFileEnv] = v.token
}

// run runs `legion workspace-init args…` inside this test process on the volume's process.
func (v *issueVolume) run(args []string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = v.runContext(context.Background(), args, &out, &errs)
	return code, out.String(), errs.String()
}

// runContext is run under ctx, writing to stdout and stderr as the command goes: a cache test
// reads the cache's lines while it serves.
func (v *issueVolume) runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runIn(ctx, append([]string{"legion", "workspace-init"}, args...), stdout, stderr, v.process())
}

// answered is run waited for with the test binary's deadline as its only bound (testwait.Receive):
// a run whose stand-in never answers fails the case by name, where the call alone would hang the
// package to its -timeout.
func (v *issueVolume) answered(t *testing.T, what string, args []string) (code int, stdout, stderr string) {
	t.Helper()
	type answer struct {
		code           int
		stdout, stderr string
	}
	answered := make(chan answer, 1)
	go func() {
		var a answer
		a.code, a.stdout, a.stderr = v.run(args)
		answered <- a
	}()
	a := testwait.Receive(t, what, answered)
	return a.code, a.stdout, a.stderr
}

// instantClock is provision's clock in a test: each wait passes at once, recorded, and moves the
// clock on by its length. atFirstWait, when set, runs as the first wait starts: what changes while
// provision waits.
type instantClock struct {
	now         time.Time
	waits       []time.Duration
	atFirstWait func()
}

func (c *instantClock) Now() time.Time { return c.now }

func (c *instantClock) Sleep(ctx context.Context, d time.Duration) error {
	if len(c.waits) == 0 && c.atFirstWait != nil {
		c.atFirstWait()
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

// instantClock makes an instantClock the clock of the volume's provisions (issueVolume.proc).
func (v *issueVolume) instantClock() *instantClock {
	clock := &instantClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
	v.proc.clock = clock
	return clock
}

// logLines is every invocation one of the volume's PATH stubs recorded to path, one call per
// line; nil when the stub was never run.
func logLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// jjCalls is every jj invocation the volume's PATH saw, "<tag> <argv>" each.
func (v *issueVolume) jjCalls(t *testing.T) []string { return logLines(t, v.jjLog) }

// codegraphCalls is every codegraph invocation the volume's PATH saw, one argv per line; nil
// when the stub was never run.
func (v *issueVolume) codegraphCalls(t *testing.T) []string { return logLines(t, v.codegraphLog) }

func (v *issueVolume) jj(t *testing.T, args ...string) string {
	t.Helper()
	// Every call names the repository it reads with -R, so the helper runs in the volume's root,
	// which holds them all: jj searches upward from the directory it runs in for the closest .jj,
	// so a command with no Dir would open the workspace the test process stands in, which in a
	// Legion issue pod is a real one.
	return strings.TrimSpace(v.runner(t).Out(v.root, append([]string{v.realJJ}, args...)...))
}

func (v *issueVolume) git(t *testing.T, args ...string) string {
	t.Helper()
	return v.gitInput(t, "", args...)
}

// gitInput is v.git with input on the command's standard input, which the plumbing that writes a
// tree reads its entries from (git mktree).
func (v *issueVolume) gitInput(t *testing.T, input string, args ...string) string {
	t.Helper()
	command := exec.Command(v.env["WINIT_REAL_GIT"], args...)
	// Every call names the repository it works on with --git-dir, which is not a working
	// directory: the helper runs in the volume's root rather than wherever the test process
	// stands.
	command.Dir = v.root
	// The command carries its own standard input, which a fixturegit.Runner does not take, so it
	// is built here and takes that runner's environment alone (v.runner).
	command.Env = v.runner(t).Env
	command.Stdin = strings.NewReader(input)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// commitAddingAFile commits name, holding content, onto parent's tree in the repository whose git
// directory is gitDir, and points ref at that commit. It runs git plumbing alone and writes no
// working copy: an issue pod's clone has its sparse patterns cleared (workspace.issuePodClone), so
// it has no working tree to write a file into and a jj snapshot of it would record none.
func (v *issueVolume) commitAddingAFile(t *testing.T, gitDir, parent, ref, name, content string) {
	t.Helper()
	dir := "--git-dir=" + gitDir
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := v.git(t, dir, "ls-tree", parent)
	if entries != "" {
		entries += "\n"
	}
	blob := v.git(t, dir, "hash-object", "-w", file)
	tree := v.gitInput(t, entries+"100644 blob "+blob+"\t"+name+"\n", dir, "mktree")
	v.git(t, dir, "update-ref", ref, v.git(t, dir, "commit-tree", tree, "-p", parent, "-m", "adds "+name))
}

// Every refusal `provision` makes happens before anything touches the volume or runs a tool: no
// one-shot credential is left in its TMPDIR and no git reached GitHub. A refusal of its input exits
// 3, a cause outside the issue's volume, and a usage error exits 2.
func TestWorkspaceInitRefusesBeforeTouchingTheVolume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "no subcommand",
			args: func(*issueVolume) []string { return nil },
			code: 2,
			says: func(*issueVolume) string { return "Usage:\n  legion workspace-init [flags]\n" },
		},
		{
			name: "an unknown subcommand",
			args: func(v *issueVolume) []string { return append([]string{"init"}, v.args("LEGION-42")[1:]...) },
			code: 2,
			says: func(*issueVolume) string { return `unknown command "init" for "legion workspace-init"` },
		},
		{
			name: "the one-container invocation, with no subcommand",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[1:] },
			code: 2,
			says: func(*issueVolume) string { return `unknown flag: --issue` },
		},
		{
			name: "no --issue",
			args: func(v *issueVolume) []string { return append([]string{"provision"}, v.args("LEGION-42")[3:]...) },
			code: 3,
			says: func(*issueVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "")` },
		},
		{
			name: "an --issue that is not an issue key",
			args: func(v *issueVolume) []string { return v.args("nope") },
			code: 3,
			says: func(*issueVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "nope")` },
		},
		{
			name: "a --repo that is not owner/name",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "acme", "--root", v.root, "--credential-helper", "x"}
			},
			code: 3,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "../x", "--root", v.root, "--credential-helper", "x"}
			},
			code: 3,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			name: "a --repo holding whitespace",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "acme/wid gets", "--root", v.root, "--credential-helper", "x"}
			},
			code: 3,
			says: func(*issueVolume) string { return `--repo "acme/wid gets" holds whitespace` },
		},
		{
			// --repo is read first: every other input here is wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string {
				return []string{"provision", "--issue", "nope", "--repo", "../..", "--root", "legion-root"}
			},
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_PROVISION_TOKEN_FILE") },
			code: 3,
			says: func(*issueVolume) string { return `--repo "../.." has a ".." segment` },
		},
		{
			name: "no --credential-helper",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[:7] },
			code: 3,
			says: func(*issueVolume) string { return "--credential-helper is required" },
		},
		{
			name: "a relative --root",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", winitRepo, "--root", "legion-root", "--credential-helper", "x"}
			},
			code: 3,
			says: func(*issueVolume) string { return `--root must be an absolute path (got "legion-root")` },
		},
		{
			name: "no --default-branch",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[:9] },
			code: 3,
			says: func(*issueVolume) string { return "--default-branch is required" },
		},
		{
			name: "a provisioning token file that is absent",
			env:  func(t *testing.T, v *issueVolume) { v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token + ".absent" },
			code: 3,
			// The refusal names no path: a path can hold a credential, so the rule of
			// 2026-11-21 (daemon refusals print no credential) leaves it out.
			says: func(*issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names a file that could not be read: it does not exist"
			},
		},
		{
			name: "a provisioning token file that is blank",
			env: func(t *testing.T, v *issueVolume) {
				if err := os.WriteFile(v.token, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 3,
			says: func(v *issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ", which is empty"
			},
		},
		{
			name: "a LEGION_REPOSITORY_CACHE_URL that is not an http URL",
			env:  func(t *testing.T, v *issueVolume) { v.env[cacheURLEnv] = "10.0.0.5:8418" },
			code: 3,
			says: func(*issueVolume) string {
				return `LEGION_REPOSITORY_CACHE_URL must be an http:// URL (got "10.0.0.5:8418")`
			},
		},
		{
			name: "a LEGION_REPOSITORY_CACHE_URL with no LEGION_REPOSITORY_CACHE_TOKEN_FILE",
			env:  func(t *testing.T, v *issueVolume) { v.env[cacheURLEnv] = "http://10.0.0.5:8418" },
			code: 3,
			says: func(*issueVolume) string {
				return "LEGION_REPOSITORY_CACHE_TOKEN_FILE is not set: a repository cache needs its token"
			},
		},
		{
			name: "a cache token file that is blank",
			env: func(t *testing.T, v *issueVolume) {
				v.env[cacheURLEnv] = "http://10.0.0.5:8418"
				v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, "\n")
			},
			code: 3,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE names " },
		},
		{
			name: "no git on PATH",
			env:  func(t *testing.T, v *issueVolume) { v.env["PATH"] = t.TempDir() },
			code: 3,
			says: func(*issueVolume) string { return "git is not on PATH" },
		},
		{
			name: "no jj on PATH",
			env: func(t *testing.T, v *issueVolume) {
				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
					t.Fatal(err)
				}
				v.env["PATH"] = bin
			},
			code: 3,
			says: func(*issueVolume) string { return "jj is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *issueVolume) []string { return append(v.args("LEGION-42"), "--bogus") },
			code: 2,
			says: func(*issueVolume) string { return "-bogus" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.args("LEGION-42"), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newIssueVolume(t)
			v.provisionEnv()
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.args("LEGION-42")
			if tc.args != nil {
				args = tc.args(v)
			}
			code, stdout, stderr := v.run(args)
			if code != tc.code || !strings.Contains(stderr, tc.says(v)) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, tc.code, tc.says(v))
			}
			if stdout != "" {
				t.Fatalf("stdout %q, want nothing", stdout)
			}
			if entries, err := os.ReadDir(v.root); err != nil || len(entries) != 0 {
				t.Fatalf("the issue volume holds %v (%v), want it untouched", entries, err)
			}
			if _, err := os.Stat("legion-root"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a relative --root was created: %v", err)
			}
			if calls := v.jjCalls(t); calls != nil {
				t.Fatalf("jj ran %q before the refusal", calls)
			}
			if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
				t.Fatalf("TMPDIR holds %v (%v), want no credential left behind", entries, err)
			}
			if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("git reached GitHub before the refusal (%v)", err)
			}
		})
	}
}

// A fresh issue volume is provisioned by filling the issue's clone directly from GitHub or the
// repository cache: its origin is still GitHub's and marked as gh's default repository
// (`remote.origin.gh-resolved base`), and its jj workspace is on its bookmark. The clone has the
// named credential helper, no gh shim and no tmux pane's `legion` launcher on the volume (a pod's
// PATH names the image's gh and legion, and its credential is the gh files its role Secret
// projects), the fill's log line, one log line naming the workspace, and no sessions directory: no
// container of the pod mounts one, every session being a row in the database the deployment names
// (INFRA-80). The permanent guard on the pod path: a recording `codegraph` stub is first on PATH
// throughout, and this container must never call it (#1647) — only a synchronous warm-up regressing
// back onto the init container's launch path would. The provisioning runs as a process of its own
// (start), as an init container does, so the stub is first on that process's PATH, where the
// warm-up looks for codegraph (exec.LookPath); a run inside this test process would hand the stub's
// PATH to the command's children alone.
func TestWorkspaceInitProvisionsTheIssueWorkspace(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")

	p := v.start(t, "", "LEGION-42")
	code, lines := p.wait(t)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, p.stderr.String())
	}
	workspace := v.workspace("LEGION-42")
	const fill = "workspace-init: no repository cache named: fetched the default branch of acme/widgets from GitHub ("
	if want := "workspace-init: " + workspace + " on legion/LEGION-42 from main"; len(lines) != 2 || !strings.HasPrefix(lines[0], fill) || lines[1] != want {
		t.Fatalf("stdout %q, want the fill line then %q", lines, want)
	}
	if listed := v.jj(t, "workspace", "list", "-R", v.clone()); !strings.Contains(listed, "legion-42:") {
		t.Fatalf("the clone's workspaces are %q, want legion-42 among them", listed)
	}
	if parent := v.jj(t, "log", "-r", "@-", "--no-graph", "-T", "description.first_line()", "--ignore-working-copy", "-R", workspace); parent != "seed" {
		t.Fatalf("the workspace sits on %q, want the remote's main", parent)
	}
	if origin := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "remote", "get-url", "origin"); origin != "https://github.com/acme/widgets" {
		t.Fatalf("the clone's origin is %q, want GitHub's", origin)
	}
	if markers := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "config", "--get-regexp", `^remote\..*\.gh-resolved$`); markers != "remote.origin.gh-resolved base" {
		t.Fatalf("the clone's gh-resolved lines are %q, want the one marking origin as gh's default repository, remote.origin.gh-resolved base", markers)
	}
	// -z, because v.git trims its output and the first of the clone's two values is the empty
	// reset: with newline separators that value is a leading newline, which the trim takes away.
	// NUL separators keep it, so the pair reads whole, the reset first and the named helper last.
	if helpers := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "config", "-z", "--get-all", "credential.helper"); helpers != "\x00"+winitHelper+"\x00" {
		t.Fatalf("the clone's credential helpers are %q, want the empty reset and then %q", helpers, winitHelper)
	}

	for _, dir := range []string{"worker-bin", "bin", "gh"} {
		if _, err := os.Stat(filepath.Join(v.root, dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the issue volume holds %s (%v), want no shim, launcher or gh directory of the daemon's", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(v.root, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the issue volume holds a sessions directory (%v), want none: nothing mounts or reads one", err)
	}
	if calls := v.codegraphCalls(t); calls != nil {
		t.Fatalf("workspace-init called codegraph %q, want it to build no index on the pod's registration path", calls)
	}
	holdsNoToken(t, v.root, "after provisioning")
}

// A repository whose default branch is not main, and holds a slash, provisions at that branch: the
// init log names it, the workspace's parent is the remote's head of it, and the issue's bookmark
// is the fresh working copy's.
func TestWorkspaceInitProvisionsAtADefaultBranchWithASlash(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "release/v7.1")
	v.provisionEnv()

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	if want := "workspace-init: " + workspace + " on legion/LEGION-42 from release/v7.1\n"; !strings.HasSuffix(stdout, want) || !strings.HasPrefix(stdout, "workspace-init: no repository cache named: fetched the default branch of acme/widgets from GitHub (") {
		t.Fatalf("stdout %q, want the fill line then %q", stdout, want)
	}
	head := v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "refs/heads/release/v7.1")
	if parent := v.jj(t, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspace); parent != head {
		t.Fatalf("the workspace's @- is %s, want release/v7.1's %s", parent, head)
	}
	if bookmark, at := v.jj(t, "log", "-r", "legion/LEGION-42", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", v.clone()),
		v.jj(t, "log", "-r", "@", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspace); bookmark != at {
		t.Fatalf("legion/LEGION-42 is on %s, want the workspace's working copy %s", bookmark, at)
	}
}

// The issue's checkout (`jj sparse reset`) writes every file of the repository's tree, so it runs
// under workspace.CheckoutTimeout rather than the runner's slow-command budget (workspace.Provision,
// Request.CheckoutBound; workspace.CommandTimeout names which commands carry a bound of their
// own): a pod whose checkout outlasts that budget provisions all the same, rather than exiting 1
// on a killed checkout and spending one of the claim's launches (INFRA-1265). The case reads the
// bound each command of the provisioning was given (issueVolume.proc's runner): the checkout's is
// CheckoutTimeout, and every other command's is the runner's budget or, for a step of the shared
// clone's own clone, what remains of the shared clone's bound (workspace.CloneBound, read through
// programbound). The budget is what is left of the test binary's deadline less 30 s, so no command
// a loaded machine makes slow is stopped by it.
func TestWorkspaceInitGivesTheCheckoutABoundOfItsOwn(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	// The checkout is what puts the tree in the workspace, so the remote's default branch carries
	// a file for it to write there.
	v.commitAddingAFile(t, v.env["WINIT_REMOTE"], "refs/heads/main", "refs/heads/main", "tree.txt", "tree\n")
	v.provisionEnv()
	budget := testwait.Bounded(t, 1<<60, 30*time.Second)
	if budget == workspace.CheckoutTimeout {
		// A budget equal to the checkout's bound could not tell the two apart.
		budget--
	}
	// `provision` runs in this process on the volume's runner, production's wrapped to record each
	// command and the bound it was given.
	var commands []workspace.Command
	next := v.proc.newRunner
	v.proc.newRunner = func(environ []string, _ time.Duration, tools map[string]string) workspace.Runner {
		return commandLog{Runner: next(environ, budget, tools), commands: &commands}
	}

	// The shared clone's steps share one bound, read through programbound when the clone starts:
	// in this binary what is left of its deadline, so it is read here first, no less than the
	// clone's own.
	cloneBound := programbound.Of(&workspace.CloneBound)
	started := time.Now()
	code, stdout, stderr := v.answered(t, "the provisioning of LEGION-42", v.args("LEGION-42"))
	elapsed := time.Since(started)
	if code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	provisionedWorkspace := v.workspace("LEGION-42")
	if want := "workspace-init: " + provisionedWorkspace + " on legion/LEGION-42 from main\n"; !strings.HasSuffix(stdout, want) || !strings.HasPrefix(stdout, "workspace-init: no repository cache named: fetched the default branch of acme/widgets from GitHub (") {
		t.Fatalf("stdout %q, want the fill line then %q", stdout, want)
	}
	if _, err := os.Stat(filepath.Join(provisionedWorkspace, "tree.txt")); err != nil {
		t.Fatalf("the workspace after provisioning: %v, want the tree the checkout writes", err)
	}
	checkouts, budgeted := 0, 0
	for _, command := range commands {
		argv := strings.Join(command.Argv, " ")
		switch {
		case strings.HasPrefix(argv, "jj sparse reset"):
			checkouts++
			if command.Timeout != workspace.CheckoutTimeout {
				t.Errorf("the checkout %q ran under %s, want workspace.CheckoutTimeout's %s", argv, command.Timeout, workspace.CheckoutTimeout)
			}
		case command.Timeout == budget:
			budgeted++
		case command.Timeout <= workspace.FetchTimeout && command.Timeout >= workspace.FetchTimeout-elapsed:
			// The fill's GitHub ref read and delta each have their own network bound.
		case command.Timeout > cloneBound || command.Timeout < cloneBound-elapsed:
			t.Errorf("%q ran under %s, want the runner's budget %s, FetchTimeout, or what remains of the shared clone's %s", argv, command.Timeout, budget, cloneBound)
		}
	}
	if checkouts != 1 || budgeted == 0 {
		t.Fatalf("the provisioning ran %d checkouts and %d commands under the runner's budget of its %d, want 1 and at least 1", checkouts, budgeted, len(commands))
	}
}

// commandLog is a provisioning runner that records each command it is handed, with the bound the
// command was given, before it runs it.
type commandLog struct {
	workspace.Runner
	commands *[]workspace.Command
}

func (r commandLog) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	*r.commands = append(*r.commands, command)
	return r.Runner.Run(ctx, command)
}

// staleLock leaves a git lock file at path, last written age ago, as a git or jj process killed or
// interrupted mid-write leaves one beside the file it was writing.
func staleLock(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// removedLine matches provision's log line for the stale lock it removed at path, last written
// about age ago (whole minutes; the seconds are the test's own run time).
func removedLine(path string, age time.Duration) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^workspace-init: removed the stale git lock ` + regexp.QuoteMeta(path) +
		`, last written ` + regexp.QuoteMeta(strings.TrimSuffix(age.String(), "0s")) + `\d+s ago`)
}

// provisioned runs provision for issue on the volume and fails the test unless it succeeds.
func (v *issueVolume) provisioned(t *testing.T, issue string) string {
	t.Helper()
	code, stdout, stderr := v.run(v.args(issue))
	if code != 0 {
		t.Fatalf("provision %s: exit %d, stdout %q, stderr %q", issue, code, stdout, stderr)
	}
	return stdout
}

// A git or jj process interrupted mid-write leaves its lock beside the file it was writing, and
// every later git or jj write to that file fails on it until someone deletes it (LEGION-731: a
// worktree's index.lock failed every provisioning of an issue's pods with "Could not acquire lock
// for index file"). provision runs before the issue pod's launcher container starts, on a
// volume no other pod mounts, so nothing can hold a lock there: it removes the workspace's stale
// index.lock, logging its path and age, and the update-stale that failed on it succeeds.
func TestWorkspaceInitRemovesTheWorkspacesStaleIndexLock(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()
	v.provisioned(t, "LEGION-42")
	// The working copy is rebased from outside the workspace onto a commit carrying a file, so the
	// next provisioning's update-stale checks out a new tree, which rewrites the worktree's index.
	v.commitAddingAFile(t, filepath.Join(v.clone(), ".git"), "HEAD", "refs/heads/added", "added.txt", "added\n")
	// jj reaches that commit only once it has imported the ref git wrote: a rebase onto `added`
	// before the import fails with "Revision `added` doesn't exist".
	v.jj(t, "git", "import", "-R", v.clone())
	v.jj(t, "rebase", "-r", "legion-42@", "-d", "added", "--ignore-working-copy", "-R", v.clone())
	lock := filepath.Join(v.clone(), ".git", "worktrees", "legion-42", "index.lock")
	staleLock(t, lock, 31*time.Minute)

	stdout := v.provisioned(t, "LEGION-42")
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after provisioning: %v, want it removed", lock, err)
	}
	if want := removedLine(lock, 31*time.Minute); !want.MatchString(stdout) {
		t.Fatalf("stdout %q, want a line matching %q", stdout, want)
	}
	if _, err := os.Stat(filepath.Join(v.workspace("LEGION-42"), "added.txt")); err != nil {
		t.Fatalf("the workspace after provisioning: %v, want the rebased tree update-stale checks out", err)
	}
}

// provision removes the issue's own locks and no other: another workspace's git worktree entry in
// the clone — an issue's that shared it, or a worktree an agent added — keeps its lock as it was,
// however old.
func TestWorkspaceInitLeavesAnotherWorkspacesLockAlone(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()
	v.provisioned(t, "LEGION-42")
	v.provisioned(t, "LEGION-43")
	other := filepath.Join(v.clone(), ".git", "worktrees", "legion-43", "index.lock")
	staleLock(t, other, 31*time.Minute)
	before, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}

	stdout := v.provisioned(t, "LEGION-42")
	after, err := os.Lstat(other)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("LEGION-43's %s after LEGION-42's provisioning: %v (%v), want it left as it was", other, after, err)
	}
	if strings.Contains(stdout, other) {
		t.Fatalf("stdout %q names LEGION-43's lock", stdout)
	}
}

// The clone's own git directory holds locks beside what git and jj write there — its default
// workspace's index, packed-refs, a ref — and the clone is as much the issue's alone as its
// workspace: one per issue volume, which no other pod mounts. provision removes each stale one,
// logging it, and provisions.
func TestWorkspaceInitRemovesTheClonesStaleLocks(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()
	v.provisioned(t, "LEGION-42")
	git := filepath.Join(v.clone(), ".git")
	locks := []string{
		filepath.Join(git, "index.lock"),
		filepath.Join(git, "packed-refs.lock"),
		filepath.Join(git, "refs", "heads", "legion", "LEGION-42.lock"),
	}
	for _, lock := range locks {
		staleLock(t, lock, 2*time.Hour)
	}

	stdout := v.provisioned(t, "LEGION-42")
	for _, lock := range locks {
		if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after provisioning: %v, want it removed", lock, err)
		}
		if want := removedLine(lock, 2*time.Hour); !want.MatchString(stdout) {
			t.Errorf("stdout %q, want a line matching %q", stdout, want)
		}
	}
}

// A stale lock provision cannot remove fails it, naming the file, before any git or jj runs on
// the clone: it would fail the provisioning anyway, on a message that names the lock alone.
func TestWorkspaceInitFailsOnALockItCannotRemove(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a directory it cannot write")
	}
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()
	v.provisioned(t, "LEGION-42")
	entry := filepath.Join(v.clone(), ".git", "worktrees", "legion-42")
	lock := filepath.Join(entry, "index.lock")
	staleLock(t, lock, 31*time.Minute)
	if err := os.Chmod(entry, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(entry, 0o755) })
	calls := len(v.jjCalls(t))

	code, _, stderr := v.run(v.args("LEGION-42"))
	if code != 1 || !strings.Contains(stderr, "remove the stale git lock "+lock) {
		t.Fatalf("exit %d, stderr %q, want exit 1 naming %s", code, stderr, lock)
	}
	if ran := v.jjCalls(t)[calls:]; len(ran) != 0 {
		t.Fatalf("jj ran %q after the lock could not be removed, want nothing run", ran)
	}
}

// The clone's .git and its worktrees directory are real directories, as git creates them, and both
// are on the volume every agent of the issue writes. Either one replaced with a symlink would take
// the sweep, and the worktree writes after it, to whatever directory it names: there an entry
// whose gitdir names the issue's workspace counts as the workspace's own. provision refuses it,
// naming the symlink, before it removes anything or runs jj.
func TestWorkspaceInitRefusesASymlinkedGitDirectory(t *testing.T) {
	t.Parallel()
	for _, linked := range []string{".git", filepath.Join(".git", "worktrees")} {
		t.Run(linked, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			v.provisionEnv()
			v.provisioned(t, "LEGION-42")
			path := filepath.Join(v.clone(), linked)
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(path, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
			evil := filepath.Join(v.clone(), ".git", "worktrees", "evil")
			if err := os.MkdirAll(evil, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(evil, "gitdir"), []byte(filepath.Join(v.workspace("LEGION-42"), ".git")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(evil, "victim.lock")
			staleLock(t, victim, 31*time.Minute)
			calls := len(v.jjCalls(t))

			code, stdout, stderr := v.run(v.args("LEGION-42"))
			if want := "refusing the git worktrees of " + filepath.Join(v.clone(), ".git") + ": " + path + " is a symlink"; code != 1 || !strings.Contains(stderr, want) {
				t.Fatalf("exit %d, stdout %q, stderr %q, want exit 1 with %q", code, stdout, stderr, want)
			}
			if _, err := os.Lstat(victim); err != nil {
				t.Fatalf("%s after the refusal: %v, want it left where the symlink points", victim, err)
			}
			if ran := v.jjCalls(t)[calls:]; len(ran) != 0 {
				t.Fatalf("jj ran %q after the refusal, want nothing run", ran)
			}
		})
	}
}

// A provisioning whose mounted token is unavailable exits 3: replacing the issue volume cannot
// repair a credential outside it.
func TestWorkspaceInitReportsAnUnavailableProvisioningToken(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t)

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != workspace.InitExitOutsideVolume || !strings.Contains(stderr, provisionTokenFileEnv+" is not set") || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d naming the missing token", code, stdout, stderr, workspace.InitExitOutsideVolume)
	}
}

// initProcess is one workspace-init running as its own process, as a pod's init container does.
type initProcess struct {
	cmd    *exec.Cmd
	lines  chan string
	stderr lockedBuffer
	waited bool
}

// lockedBuffer is a process's stderr, readable while the process still writes it.
type lockedBuffer = testlog.Buffer

// start runs workspace-init for issue as a separate process whose jj invocations carry tag, in
// its own process group so a test can kill it with whatever it spawned.
func (v *issueVolume) start(t *testing.T, tag, issue string, env ...string) *initProcess {
	t.Helper()
	p := &initProcess{lines: make(chan string, 16)}
	p.cmd = exec.Command(legionCopy(t), append([]string{"workspace-init"}, v.args(issue)...)...)
	overrides := maps.Clone(v.env)
	overrides[testMainEnv] = "1"
	overrides["WINIT_TAG"] = tag
	overrides[provisionTokenFileEnv] = v.token
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		overrides[name] = value
	}
	// The process under test runs the real git and jj, so it starts from fixtureenv.Isolated, as
	// the test's own fixtures do, and the volume's overrides come after and win.
	p.cmd.Env = fixtureenv.Isolated(t.TempDir())
	for name, value := range overrides {
		p.cmd.Env = append(p.cmd.Env, name+"="+value)
	}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start workspace-init %s: %v", issue, err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if !p.waited {
			// The kill closes the process's stdout. The drain waits for that with the test binary's
			// deadline as its only bound, so a stdout that never closes fails the test by name
			// rather than hanging it to its -timeout.
			drained := make(chan struct{})
			go func() {
				for range p.lines {
				}
				close(drained)
			}()
			testwait.Receive(t, "workspace-init's stdout to close once it was killed", drained)
			_ = p.cmd.Wait()
		}
	})
	return p
}

// wait is the process's exit status and the rest of its stdout, once it exits, waited for with the
// test binary's deadline as its only bound (testwait.Receive).
func (p *initProcess) wait(t *testing.T) (code int, rest []string) {
	t.Helper()
	closed := make(chan []string, 1)
	go func() {
		var lines []string
		for line := range p.lines {
			lines = append(lines, line)
		}
		closed <- lines
	}()
	rest = testwait.Receive(t, "workspace-init to exit", closed)
	p.waited = true
	err := p.cmd.Wait()
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		t.Fatalf("wait for workspace-init: %v", err)
	}
	return p.cmd.ProcessState.ExitCode(), rest
}

// holder starts `provision` for LEGION-42 and returns once it is held inside its first jj command,
// the shared clone's init (fakeJJ's WINIT_HOLD), where it stays until the test ends. That jj leads a
// process group of its own (procgroup.Configure), which no kill of workspace-init reaches, so the
// test holds the one writing end of the jj's `release` fifo (O_RDWR opens a fifo without waiting for
// a reader) and the hold ends when that end closes: in holder's cleanup, which runs once start's has
// ended workspace-init and waits, where /proc shows it, until the jj has exited; or as the kernel
// closes it, when the test binary dies first.
func (v *issueVolume) holder(t *testing.T) *initProcess {
	t.Helper()
	hold := t.TempDir()
	for _, fifo := range []string{"held", "release"} {
		if err := syscall.Mkfifo(filepath.Join(hold, fifo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release, err := os.OpenFile(filepath.Join(hold, "release"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	jj := 0
	t.Cleanup(func() {
		_ = release.Close()
		if jj == 0 {
			return
		}
		script := filepath.Join(v.bin, "jj")
		testwait.WithinPolling(t, testwait.Bounded(t, 1<<60, 30*time.Second), testwait.Poll, "the held jj to exit", func() bool {
			cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(jj) + "/cmdline")
			return err != nil || !strings.Contains(string(cmdline), script)
		})
	})
	p := v.start(t, "first", "LEGION-42", "WINIT_HOLD="+hold)
	held := fifo(t, filepath.Join(hold, "held"), os.O_RDONLY, p)
	if jj, err = strconv.Atoi(held); err != nil {
		t.Fatalf("the held jj wrote %q to its held fifo, want its pid", held)
	}
	return p
}

// fifo opens one end of a fifo — which blocks until the holder opens the other — reads it to EOF
// when reading, and closes it, bounded by the test binary's deadline less 30 s and by the holder's
// staying alive and silent.
func fifo(t *testing.T, path string, flag int, holder *initProcess) string {
	t.Helper()
	type opened struct {
		read []byte
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		var result opened
		file, err := os.OpenFile(path, flag, 0)
		if err == nil {
			if flag == os.O_RDONLY {
				result.read, err = io.ReadAll(file)
			}
			_ = file.Close()
		}
		result.err = err
		done <- result
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("%s: %v", path, result.err)
		}
		return string(result.read)
	case line, ok := <-holder.lines:
		t.Fatalf("the holder printed %q (open: %v) before reaching %s; stderr %q", line, ok, path, holder.stderr.String())
	case <-time.After(testwait.Bounded(t, 1<<60, 30*time.Second)):
		t.Fatalf("nothing opened the other end of %s before the test binary's deadline", path)
	}
	return ""
}

// holdsNoToken fails when any file under dir contains the provisioning token.
func holdsNoToken(t *testing.T, dir, when string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte("ghs_test")) {
			t.Errorf("%s, %s holds the provisioning token", when, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// A held provisioning's jj runs in a process group of its own (procgroup.Configure), which neither
// a test's kill of workspace-init nor start's kill of workspace-init's group reaches: the hold ends
// with the test that took it, or the fake jj, its fifos gone with the test's TempDir, waits for a
// release forever. Each scenario runs as a subtest, and once it has ended no process runs that
// volume's fake jj.
func TestWorkspaceInitHoldEndsWithItsTest(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skipf("no /proc to find a held jj in: %v", err)
	}
	for _, tc := range []struct {
		name string
		kill bool
	}{{"the test kills workspace-init", true}, {"start's cleanup kills workspace-init", false}} {
		var jj string
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			jj = filepath.Join(v.bin, "jj")
			first := v.holder(t)
			if tc.kill {
				if err := first.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				first.wait(t)
			}
		})
		if jj == "" {
			continue
		}
		if left := killProcessesRunning(t, jj); len(left) != 0 {
			t.Errorf("%s: after its subtest ended, its fake jj still runs: %s", tc.name, strings.Join(left, "; "))
		}
	}
}

// killProcessesRunning is every process whose command line names path, as "pid <n>: <argv>", each
// killed once found so a failing run leaves none behind.
func killProcessesRunning(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
		if strings.Contains(argv, path) {
			found = append(found, "pid "+entry.Name()+": "+argv)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return found
}

// The issue pod's only init container provisions its clone directly: it receives the provisioning
// token, makes the one-shot credential under its TMPDIR, and removes it once GitHub has answered.
// Keeping the earlier two-container path makes this fail because provision refuses that token.
func TestWorkspaceInitProvisionUsesTheProvisioningToken(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()

	code, _, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(v.clone()); err != nil {
		t.Fatalf("provision did not make the clone: %v", err)
	}
	if credential, err := os.ReadFile(v.credentialLog); err != nil || !strings.Contains(string(credential), " ghs_test") {
		t.Fatalf("GitHub credential log %q (%v), want the provisioning token", credential, err)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v), want the one-shot credential removed", entries, err)
	}
}

// The clone stores the automatic-maintenance settings itself, so git and Jujutsu a role starts
// after provisioning cannot begin maintenance when the one-shot provisioning environment is gone.
func TestWorkspaceInitWritesCloneMaintenanceConfiguration(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.provisionEnv()

	code, _, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	gitDir := "--git-dir=" + filepath.Join(v.clone(), ".git")
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		if got := v.git(t, gitDir, "config", "--get", key); got != want {
			t.Errorf("clone %s = %q, want %q", key, got, want)
		}
	}
}

// The issue clone takes the default branch and this issue's branches, then persists only the
// refspecs a role may fetch without naming another branch.
func TestWorkspaceInitProvisionKeepsOnlyTheIssueRefs(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	remote := v.env["WINIT_REMOTE"]
	main := "refs/heads/main"
	for _, branch := range []string{
		"legion/LEGION-42", "legion/LEGION-42.task", "legion/LEGION-420",
		"legion/LEGION-421", "legion/OTHER-1", "cold/x",
	} {
		v.commitAddingAFile(t, remote, main, "refs/heads/"+branch, strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n")
	}
	v.git(t, "--git-dir="+remote, "tag", "-a", "v1", "-m", "version", "refs/heads/cold/x")
	cold := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/cold/x")
	tag := v.git(t, "--git-dir="+remote, "rev-parse", "v1^{tag}")
	v.provisionEnv()

	code, _, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	gitDir := "--git-dir=" + filepath.Join(v.clone(), ".git")
	var refs []string
	for _, ref := range strings.Split(v.git(t, gitDir, "for-each-ref", "--format=%(refname)"), "\n") {
		if !strings.HasPrefix(ref, "refs/jj/") {
			refs = append(refs, ref)
		}
	}
	if want := []string{
		"refs/heads/legion/LEGION-42", "refs/heads/main",
		"refs/remotes/origin/HEAD",
		"refs/remotes/origin/legion/LEGION-42", "refs/remotes/origin/legion/LEGION-42.task",
		"refs/remotes/origin/main",
	}; !slices.Equal(refs, want) {
		t.Fatalf("clone refs %q, want %q", refs, want)
	}
	wantFetch := []string{
		"+refs/heads/main:refs/remotes/origin/main",
		"+refs/heads/legion/LEGION-42*:refs/remotes/origin/legion/LEGION-42*",
		"^refs/heads/legion/LEGION-420*", "^refs/heads/legion/LEGION-421*",
		"^refs/heads/legion/LEGION-422*", "^refs/heads/legion/LEGION-423*",
		"^refs/heads/legion/LEGION-424*", "^refs/heads/legion/LEGION-425*",
		"^refs/heads/legion/LEGION-426*", "^refs/heads/legion/LEGION-427*",
		"^refs/heads/legion/LEGION-428*", "^refs/heads/legion/LEGION-429*",
	}
	if got, want := strings.Split(v.git(t, gitDir, "config", "--get-all", "remote.origin.fetch"), "\n"), wantFetch; !slices.Equal(got, want) {
		t.Fatalf("remote.origin.fetch = %q, want %q", got, want)
	}
	if got := v.git(t, gitDir, "config", "--get", "remote.origin.tagopt"); got != "--no-tags" {
		t.Fatalf("remote.origin.tagopt = %q, want --no-tags", got)
	}
	command := exec.Command(v.realJJ, "config", "get", "remotes.origin.fetch-tags", "-R", v.clone())
	command.Dir = v.root
	command.Env = fixtureenv.Isolated(t.TempDir())
	for name, value := range v.env {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("jj config get remotes.origin.fetch-tags: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != "~*" {
		t.Fatalf("remotes.origin.fetch-tags = %q, want ~*", got)
	}
	for _, object := range []string{cold, tag} {
		cmd := exec.Command(v.env["WINIT_REAL_GIT"], "--git-dir="+filepath.Join(v.clone(), ".git"), "cat-file", "-e", object)
		cmd.Env = v.runner(t).Env
		if err := cmd.Run(); err == nil {
			t.Errorf("clone retains object %s outside the default and issue refs", object)
		}
	}
}

// The configured wildcard refspecs match an absent issue branch and a branch GitHub deleted after
// provisioning, so a role's plain fetch never turns either normal lifecycle state into a failure.
func TestWorkspaceInitPlainFetchesTolerateAbsentIssueBranches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		branch bool
		delete bool
	}{
		{name: "not pushed"},
		{name: "deleted after provision", branch: true, delete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newIssueVolume(t).withRemote(t, "main")
			if tc.branch {
				v.commitAddingAFile(t, v.env["WINIT_REMOTE"], "refs/heads/main", "refs/heads/legion/LEGION-42", "issue.txt", "issue\n")
			}
			v.provisionEnv()
			code, _, stderr := v.run(v.args("LEGION-42"))
			if code != 0 {
				t.Fatalf("provision exit %d, stderr %q", code, stderr)
			}
			if tc.delete {
				v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "update-ref", "-d", "refs/heads/legion/LEGION-42")
			}
			// The fixture remote replaces GitHub only for these role commands. Their persisted
			// refspecs stay exactly as provisioning wrote them.
			v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "config", "remote.origin.url", v.env["WINIT_REMOTE"])
			environment := fixtureenv.Isolated(t.TempDir())
			for name, value := range v.env {
				environment = append(environment, name+"="+value)
			}
			git := exec.Command(v.env["WINIT_REAL_GIT"], "--git-dir="+filepath.Join(v.clone(), ".git"), "fetch")
			git.Dir, git.Env = v.root, environment
			if output, err := git.CombinedOutput(); err != nil {
				t.Fatalf("plain git fetch: %v\n%s", err, output)
			}
			jj := exec.Command(v.realJJ, "--config=git.executable-path="+v.env["WINIT_REAL_GIT"], "git", "fetch", "-R", v.clone())
			jj.Dir, jj.Env = v.root, environment
			if output, err := jj.CombinedOutput(); err != nil {
				t.Fatalf("plain jj git fetch: %v\n%s", err, output)
			}
			remoteIssue := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "for-each-ref", "--format=%(refname)", "refs/remotes/origin/legion/LEGION-42")
			if remoteIssue != "" {
				t.Fatalf("plain fetch added issue remote ref %q", remoteIssue)
			}
		})
	}
}

// A current cache streams its default branch into the clone's .git directory; GitHub then supplies
// only this issue's branch. No object unique to an unrelated branch or tag reaches the pod. The
// summary line follows the line that says /tar was the source because this cache serves no /packs.
func TestWorkspaceInitProvisionStreamsANarrowedRepositoryCache(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	cache, length := v.narrowedCacheTarServer(t)
	remote := v.env["WINIT_REMOTE"]
	v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/legion/LEGION-42", "issue.txt", "issue\n")
	v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/cold/x", "cold.txt", "cold\n")
	v.git(t, "--git-dir="+remote, "tag", "-a", "v1", "-m", "version", "refs/heads/cold/x")
	cold := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/cold/x")
	tag := v.git(t, "--git-dir="+remote, "rev-parse", "v1^{tag}")
	v.provisionEnv()
	v.env[cacheURLEnv] = cache
	v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken)

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(notServedLine) + `workspace-init: ([0-9,]+) bytes from the repository cache ` + regexp.QuoteMeta(cache) + ` in .+, its history verified in .+; [0-9,]+ bytes from GitHub for the delta \([0-9]+ refs\)$`)
	match := line.FindStringSubmatch(stdout)
	if match == nil {
		t.Fatalf("stdout %q, want /tar as the source, then the cache stream and GitHub delta", stdout)
	}
	if got := printedBytes(t, match[1]); got != int64(length) {
		t.Fatalf("cache bytes %d, want tar length %d", got, length)
	}
	gitDir := "--git-dir=" + filepath.Join(v.clone(), ".git")
	if got := v.git(t, gitDir, "rev-parse", "refs/remotes/origin/legion/LEGION-42"); got == "" {
		t.Fatal("the clone has no remote issue branch fetched after the cache stream")
	}
	for _, object := range []string{cold, tag} {
		command := exec.Command(v.env["WINIT_REAL_GIT"], gitDir, "cat-file", "-e", object)
		command.Env = v.runner(t).Env
		if err := command.Run(); err == nil {
			t.Errorf("clone retains object %s outside the cache default and issue delta", object)
		}
	}
}

// A cache wait must not leave the GitHub transfer holding the token that provision read before the
// wait. The first health answer is intentionally from an earlier cache release, without its narrow
// capability; it rewrites the mounted token before the next ready answer.
func TestWorkspaceInitProvisionRenewsTheTokenAfterWaitingForTheCache(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	const (
		oldToken = "ghs_before_cache_wait"
		newToken = "ghs_after_cache_wait"
	)
	if err := os.WriteFile(v.token, []byte(oldToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, _ := v.cacheTarServer(t, cacheFault{afterFirstHealth: func() {
		if err := os.WriteFile(v.token, []byte(newToken+"\n"), 0o600); err != nil {
			panic(err)
		}
	}})
	v.provisionEnv()
	v.env[cacheURLEnv] = cache
	v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken)

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(credential), oldToken) || !strings.Contains(string(credential), newToken) {
		t.Fatalf("GitHub credentials %q, want only the token minted after the cache wait", credential)
	}
	if strings.Contains(stdout+stderr, oldToken) {
		t.Fatalf("output exposed the token read before the cache wait: stdout %q stderr %q", stdout, stderr)
	}
}

// Admission may record a default branch after GitHub changed its HEAD. When both branches remain,
// the issue starts at its recorded default and Jujutsu tracks that remote bookmark as well as the
// current default.
func TestWorkspaceInitProvisionTracksTheRecordedDefaultAfterGitHubChangesHead(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	remote := v.env["WINIT_REMOTE"]
	v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/release", "release.txt", "release\n")
	release := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/release")
	v.provisionEnv()

	args := v.args("LEGION-42")
	for index, value := range args {
		if value == v.defaultBranch {
			args[index] = "release"
		}
	}
	code, _, stderr := v.run(args)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspaceDir := v.workspace("LEGION-42")
	if parent := v.jj(t, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspaceDir); parent != release {
		t.Fatalf("workspace parent %s, want recorded release default %s", parent, release)
	}
	if tracked := v.jj(t, "bookmark", "list", "--tracked", "exact:release", "-T", `name ++ "@" ++ remote ++ "\n"`, "--ignore-working-copy", "-R", v.clone()); !strings.Contains(tracked, "release@origin") {
		t.Fatalf("tracked release bookmarks %q, want release@origin", tracked)
	}
}

// A clone the fill makes leaves origin/HEAD naming GitHub's default branch, as a clone from GitHub
// does, so jj's trunk() resolves to that branch's tip when it is named neither main nor master nor
// trunk. Without origin/HEAD, `jj git init --colocate` sets no trunk() alias and trunk() is the
// root commit.
func TestWorkspaceInitProvisionSetsTrunkToTheDefaultBranch(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "develop")
	remote := v.env["WINIT_REMOTE"]
	develop := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/develop")
	v.provisionEnv()

	code, _, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if head := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "symbolic-ref", "refs/remotes/origin/HEAD"); head != "refs/remotes/origin/develop" {
		t.Fatalf("origin/HEAD names %q, want refs/remotes/origin/develop", head)
	}
	// jj keeps the repository's own configuration, the trunk() alias its init writes among it, in
	// the config home the provision ran with, so the read takes the provision's environment.
	command := exec.Command(v.realJJ, "log", "-r", "trunk()", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", v.clone())
	command.Dir = v.root
	command.Env = fixtureenv.Isolated(t.TempDir())
	for name, value := range v.env {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.Output()
	if err != nil {
		t.Fatalf("jj log -r trunk(): %v", err)
	}
	if trunk := strings.TrimSpace(string(output)); trunk != develop {
		t.Fatalf("trunk() is %s, want develop's tip %s", trunk, develop)
	}
}

// workspace-init's lines and its failures pass the URL rule. A repository cache that cannot be
// reached is logged on stdout while `provision` waits for it, and named on stderr in the failure the
// wait ends in, each time with the error of the ask (workspace's askCache), which quotes
// LEGION_REPOSITORY_CACHE_URL with its user name and its query. The product writes each line
// through redact.Writer; the expectation is redact.URLCredentials of the same text, read from the
// rule and never written out here, and neither the user name nor the query reaches the init log.
// A cache that is never ready is a cause outside the issue's volume, so `provision` exits
// workspace.InitExitOutsideVolume. The wait reads and sleeps on the clock of the volume's process
// (issueVolume.proc), whose waits pass at once: on the process's own clock it would last
// workspace.CacheWaitTimeout.
func TestWorkspaceInitProvisionWritesNoURLCredentials(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t)
	// An address nothing listens on, so every ask of the cache fails at its dial.
	cache := testport.Refused(t)
	v.provisionEnv()
	v.env[cacheURLEnv] = "http://legion-reader@" + cache + "/widgets?token=planted-query-token-7"
	v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken)
	clock := v.instantClock()

	code, stdout, stderr := v.answered(t, "the provisioning of LEGION-42", v.args("LEGION-42"))
	cacheURL := v.env[cacheURLEnv]
	shown := redact.URLCredentials(cacheURL)
	// The ask is GET <cache>/healthz (internal/workspace's cacheNotReady), and Go's error quotes
	// that URL, so the rule is given the line as the product writes it, the route included.
	if want := redact.URLCredentials("workspace-init: waiting for the repository cache " + cacheURL + " to be ready: Get \"" + cacheURL + "/healthz\": "); !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout %q, want the wait logged as %q", stdout, want)
	}
	if want := "the repository cache " + shown + " was not ready within " + workspace.CacheWaitTimeout.String(); code != workspace.InitExitOutsideVolume || !strings.Contains(stderr, want) {
		t.Errorf("exit %d, stderr %q; want %d naming %q", code, stderr, workspace.InitExitOutsideVolume, want)
	}
	for _, credential := range []string{"legion-reader", "planted-query-token-7"} {
		if strings.Contains(stdout+stderr, credential) {
			t.Errorf("workspace-init provision wrote stdout %q and stderr %q, which hold %q", stdout, stderr, credential)
		}
	}
	if len(clock.waits) == 0 {
		t.Errorf("the wait for the cache slept on no wait of the volume's clock")
	}
}

// The two transient failures a production fetch met from GitHub, as git prints them: an HTTP/2
// stream GitHub cancelled mid-pack, and a 408 on the pack request.
const (
	curl92Failure = "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n" +
		"fatal: early EOF\n" +
		"fatal: fetch-pack: invalid index-pack output\n"
	http408Failure = "error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\n" +
		"fatal: expected 'packfile'\n"
)

// A fetch from GitHub that fails transiently is run again after a backoff wait on the clock of the
// volume's process (issueVolume.proc), each failure logged on stdout as one line, until one
// succeeds and fills the clone.
func TestWorkspaceInitProvisionRetriesATransientFetchFailure(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failFetches(t, 128, curl92Failure, http408Failure)
	clock := v.instantClock()
	v.provisionEnv()

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	want := "workspace-init: fetch attempt 1 failed; retrying in 5s: error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8) | fatal: early EOF | fatal: fetch-pack: invalid index-pack output\n" +
		"workspace-init: fetch attempt 2 failed; retrying in 15s: error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408 | fatal: expected 'packfile'\n"
	rest, retried := strings.CutPrefix(stdout, want)
	if !retried || !strings.HasPrefix(rest, "workspace-init: no repository cache named: fetched the default branch of acme/widgets from GitHub (") {
		t.Fatalf("stdout %q, want %q then the fill from GitHub", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want nothing", stderr)
	}
	if got := fetchAttempts(t, attempts); got != 3 {
		t.Fatalf("git fetched %d times, want 3", got)
	}
	if waits := clock.waits; !slices.Equal(waits, []time.Duration{5 * time.Second, 15 * time.Second}) {
		t.Fatalf("provision waited %v, want 5s then 15s", waits)
	}
	if main := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the clone's main is %q, want the remote's", main)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after provision returned, want its credential gone", entries, err)
	}
}

// Neither a retried attempt's log line nor the final failure carries the provisioning token, even
// where git echoes it: internal/workspace writes the token as *** wherever git echoed it, and the
// product then writes the line through redact.Writer, which removes a URL's userinfo whole. The
// expectation is redact.URLCredentials of the scrubbed line, read from the rule and never written
// out here.
func TestWorkspaceInitProvisionLogsNoToken(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	const (
		unreachable = "fatal: unable to access 'https://x-access-token:@TOKEN@@github.com/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n"
		invalid     = "remote: Invalid username or token @TOKEN@."
		refused     = "fatal: Authentication failed for 'https://x-access-token:@TOKEN@@github.com/acme/widgets/'"
	)
	v.failFetches(t, 128, unreachable, invalid+"\n"+refused+"\n")
	// scrubbed is a line the fake git writes (the token it was handed in place of @TOKEN@) as
	// internal/workspace hands it on: the token as ***, its scrub (remote.scrub,
	// fetchCredential.scrub), which the package exports nothing to read from.
	scrubbed := func(line string) string { return strings.ReplaceAll(line, "@TOKEN@", "***") }
	v.instantClock()
	v.provisionEnv()

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != workspace.InitExitOutsideVolume {
		t.Fatalf("exit %d, stderr %q; want %d", code, stderr, workspace.InitExitOutsideVolume)
	}
	if strings.Contains(stdout+stderr, "ghs_test") {
		t.Fatalf("provision logged the provisioning token:\nstdout %q\nstderr %q", stdout, stderr)
	}
	// The user name git's URL carries, which the rule removes with the token.
	if strings.Contains(stdout+stderr, "x-access-token") {
		t.Fatalf("provision logged the URL's user name:\nstdout %q\nstderr %q", stdout, stderr)
	}
	if want := redact.URLCredentials("workspace-init: fetch attempt 1 failed; retrying in 5s: " + scrubbed(unreachable)); stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	for _, says := range []string{scrubbed(invalid), redact.URLCredentials(scrubbed(refused))} {
		if !strings.Contains(stderr, says) {
			t.Fatalf("stderr %q, want it to name %q", stderr, says)
		}
	}
}

// `provision` exits 3 (workspace.InitExitOutsideVolume) for a cause outside the issue's volume,
// which a new volume does not cure, and 1 for every other failure: the runtime keeps a kept volume
// for the first and replaces it for the second. Each failing git prints git's own output, the
// lines internal/workspace reads in gitFailures.
//
// The fill's fetch from GitHub exits 3 when it fails with an HTTP status, a host that did not
// resolve, a pack stream cut with no cause line, a credential GitHub refused or a repository GitHub
// did not find, when a read of the provisioning token before a retry fails, and when it runs past
// the bound of its stage, the bound of the volume's process (issueVolume.proc's fetchBound;
// workspace.FetchTimeout in production). It exits 1 for curl 23, a write to the volume that
// failed, and for a bad object in the clone. A repository cache that is never ready, or that
// refuses the /tar request, exits 3. A cache tar whose write to the volume fails inside the copy,
// or whose pack fails its checksum, exits 1. The refusals before the volume, a bad flag among them,
// are TestWorkspaceInitRefusesBeforeTouchingTheVolume's.
func TestWorkspaceInitProvisionExitsThreeForAFailureOutsideTheVolume(t *testing.T) {
	t.Parallel()
	const access = "fatal: unable to access 'https://github.com/acme/widgets/': "
	// failing has every fetch from GitHub fail with stderr and status.
	failing := func(status int, stderr string) func(*testing.T, *issueVolume, *instantClock) {
		return func(t *testing.T, v *issueVolume, _ *instantClock) {
			v.failFetches(t, status, slices.Repeat([]string{stderr}, 6)...)
		}
	}
	// fromCache is a fill from a repository cache of the volume's remote with fault, to which the
	// fill presents token for the tar.
	fromCache := func(fault cacheFault, token string) func(*testing.T, *issueVolume, *instantClock) {
		return func(t *testing.T, v *issueVolume, _ *instantClock) {
			cache, _ := v.cacheTarServer(t, fault)
			v.env[cacheURLEnv] = cache
			v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, token)
		}
	}
	// timedOut is how the error of a transfer that ran out of its time begins: its git command,
	// whose first flags are the four pack settings (workspace.PackWindowConfig) as `-c` pairs.
	timedOut := "command timed out: git"
	for _, pair := range workspace.PackWindowConfig() {
		timedOut += " -c " + pair[0] + "=" + pair[1]
	}
	timedOut += " --git-dir="
	for _, tc := range []struct {
		name string
		// prepare sets up the volume's git, its cache, the clock and the bound.
		prepare func(t *testing.T, v *issueVolume, clock *instantClock)
		code    int
		says    string
		// then, when set, checks what provision left.
		then func(t *testing.T, v *issueVolume)
	}{
		{name: "a 403", prepare: failing(128, "remote: Write access to repository not granted.\n"+access+"The requested URL returned error: 403\n"),
			code: 3, says: "The requested URL returned error: 403"},
		{name: "a host that did not resolve", prepare: failing(128, access+"Could not resolve host: github.com\n"),
			code: 3, says: "fetch attempt 5 failed, and the fetch retries a fetch at most 4 times"},
		{name: "a pack stream cut with no cause line", prepare: failing(128, "fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n"),
			code: 3, says: "fatal: early EOF"},
		{name: "a credential GitHub refused", prepare: failing(128, "remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/acme/widgets/'\n"),
			code: 3, says: "fatal: Authentication failed for 'https://github.com/acme/widgets/'"},
		{name: "a repository GitHub did not find", prepare: failing(128, "remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"),
			code: 3, says: "remote: Repository not found."},
		{name: "a write of received data that failed", prepare: failing(128, "error: RPC failed; curl 23 Failed writing received data to disk/application\n"),
			code: 1, says: "curl 23 Failed writing received data to disk/application"},
		{name: "a bad object in the clone", prepare: func(t *testing.T, v *issueVolume, clock *instantClock) {
			failing(1, "fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111\nerror: file://"+v.env["WINIT_REMOTE"]+" did not send all necessary objects\n")(t, v, clock)
		}, code: 1, says: "fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111"},
		{name: "an attempt that runs out of its time", prepare: func(t *testing.T, v *issueVolume, _ *instantClock) {
			// The fetch from GitHub sleeps past any test binary's deadline, so only the bound's
			// stop ends it: a stop a loaded machine makes late still comes before the sleep ends.
			v.env["WINIT_FETCH_SLEEP"] = "86400"
			v.proc.fetchBound = time.Second
		}, code: 3, says: timedOut},
		{name: "a read of the provisioning token before a retry that fails", prepare: func(t *testing.T, v *issueVolume, clock *instantClock) {
			failing(128, http408Failure)(t, v, clock)
			clock.atFirstWait = func() {
				if err := os.Remove(v.token); err != nil {
					t.Error(err)
				}
			}
		}, code: 3, says: "fetch attempt 2: read the provisioning token: "},
		{name: "a repository cache that is never ready", prepare: fromCache(cacheFault{notReady: true}, cmdCacheToken),
			code: 3, says: "was not ready within " + workspace.CacheWaitTimeout.String()},
		{name: "a repository cache that refuses the tar", prepare: fromCache(cacheFault{}, "another-cache's-token"),
			code: 3, says: "/tar answered 401 Unauthorized"},
		{name: "a cache entry whose write fails inside the copy", prepare: func(t *testing.T, v *issueVolume, clock *instantClock) {
			if info, err := os.Stat("/dev/full"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
				t.Skipf("this case writes a cache entry to Linux's /dev/full, which this system does not have (%v)", err)
			}
			fromCache(cacheFault{fullPack: true}, cmdCacheToken)(t, v, clock)
		}, code: 1, says: "no space left on device", then: func(t *testing.T, v *issueVolume) {
			if info, err := os.Stat("/dev/full"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
				t.Errorf("/dev/full is %v (%v) after provision, want the device the link named", info, err)
			}
			if staging, err := filepath.Glob(v.clone() + ".clone-*"); err != nil || len(staging) != 0 {
				t.Errorf("provision left %q (%v) beside the clone", staging, err)
			}
		}},
		{name: "a pack that fails its checksum", prepare: fromCache(cacheFault{damagedPack: true}, cmdCacheToken),
			code: 1, says: "which does not verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newIssueVolume(t).withRemote(t, "main")
			clock := v.instantClock()
			tc.prepare(t, v, clock)
			v.provisionEnv()
			code, stdout, stderr := v.answered(t, "workspace-init provision ("+tc.name+")", v.args("LEGION-42"))
			if code != tc.code || !strings.Contains(stderr, tc.says) {
				t.Fatalf("exit %d, stderr %q, stdout %q; want %d naming %q", code, stderr, stdout, tc.code, tc.says)
			}
			if _, err := os.Stat(filepath.Join(v.clone(), ".jj")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the failed provision left a clone in place (%v)", err)
			}
			if tc.then != nil {
				tc.then(t, v)
			}
		})
	}
}
