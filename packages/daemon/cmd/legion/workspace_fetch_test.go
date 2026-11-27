package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// fakeGit is the git first on PATH. Its bare clone of github.com/acme/widgets — the fetch's —
// clones the local bare remote instead, whose file transport joins the https the runner allows,
// after appending the one-shot credential it was handed, the token file's path and what it held,
// to WINIT_CREDENTIAL_LOG; its fetch from github.com/acme/widgets into a bare repository (a feed
// that holds the repository, or one unpacked from the repository cache), and that fetch's read of
// GitHub's HEAD (`ls-remote --symref`), reach that remote in the same way. With
// WINIT_CLONE_FAILURES set (failClones), the clone first counts itself in
// that directory's `attempts`, and an attempt whose number names a file there fails as git does:
// it leaves a partial clone at its destination, writes the file to stderr with @TOKEN@ replaced by
// the token it was handed, and exits 128, git's fatal status. The fetch from GitHub first sleeps
// WINIT_FETCH_SLEEP seconds when that is set. With WINIT_FETCH_FAILURE set (failFetches), each
// fetch from GitHub then writes that file to stderr and exits with the status the file's name
// gives. Every other invocation is the real git's.
const fakeGit = `#!/bin/sh
while [ "$1" = "-c" ]; do
	shift 2
done
if [ "$1 $2 $3 $4" = "clone --bare --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	if [ -n "$WINIT_CLONE_FAILURES" ]; then
		attempt=$(( $(cat "$WINIT_CLONE_FAILURES/attempts" 2>/dev/null || echo 0) + 1 ))
		echo "$attempt" > "$WINIT_CLONE_FAILURES/attempts"
		if [ -f "$WINIT_CLONE_FAILURES/$attempt" ]; then
			mkdir -p "$5" && echo partial > "$5/partial"
			sed "s/@TOKEN@/$(cat "$LEGION_PROVISIONING_TOKEN_FILE")/g" "$WINIT_CLONE_FAILURES/$attempt" >&2
			exit 128
		fi
	fi
	shift 4
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" clone --bare --quiet "$WINIT_REMOTE" "$@"
fi
if [ "$2 $3 $4 $5" = "fetch --prune --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	if [ -n "$WINIT_FETCH_SLEEP" ]; then
		sleep "$WINIT_FETCH_SLEEP"
	fi
	if [ -n "$WINIT_FETCH_FAILURE" ]; then
		cat "$WINIT_FETCH_FAILURE" >&2
		exit "${WINIT_FETCH_FAILURE##*/}"
	fi
	first="$1"
	shift 5
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$first" fetch --prune --quiet "$WINIT_REMOTE" "$@"
fi
if [ "$2 $3 $4 $5" = "ls-remote --symref https://github.com/acme/widgets HEAD" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	first="$1"
	shift 5
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$first" ls-remote --symref "$WINIT_REMOTE" HEAD "$@"
fi
exec "$WINIT_REAL_GIT" "$@"
`

// Every refusal `fetch` makes happens before it runs git or writes anything: the feed is not
// created, and no one-shot credential is left in its TMPDIR. A refusal of its input exits 3, a
// cause outside the issue's volume, and a usage error exits 2.
func TestWorkspaceInitFetchRefusesBeforeFetching(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "a --repo that is not owner/name",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "acme", "--feed", v.feed} },
			code: 3,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "../x", "--feed", v.feed} },
			code: 3,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			name: "a --repo holding whitespace",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "acme/wid gets", "--feed", v.feed} },
			code: 3,
			says: func(*issueVolume) string { return `--repo "acme/wid gets" holds whitespace` },
		},
		{
			// --repo is read first: the feed and the token are wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string { return []string{"fetch", "--repo", "acme/..", "--feed", "feed"} },
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_PROVISION_TOKEN_FILE") },
			code: 3,
			says: func(*issueVolume) string { return `--repo "acme/.." has a ".." segment` },
		},
		{
			name: "a relative --feed",
			args: func(*issueVolume) []string { return []string{"fetch", "--repo", winitRepo, "--feed", "feed"} },
			code: 3,
			says: func(*issueVolume) string { return `--feed must be an absolute path (got "feed")` },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_PROVISION_TOKEN_FILE") },
			code: 3,
			says: func(*issueVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env: func(t *testing.T, v *issueVolume) {
				v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token + ".absent"
			},
			code: 3,
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
			name: "no git on PATH",
			env:  func(t *testing.T, v *issueVolume) { v.env["PATH"] = t.TempDir() },
			code: 3,
			says: func(*issueVolume) string { return "git is not on PATH" },
		},
		{
			name: "a --cache-url that is not an http URL",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "ftp://10.0.0.5:8418") },
			code: 3,
			says: func(*issueVolume) string { return "--cache-url must be an http:// URL with a host" },
		},
		{
			name: "a LEGION_REPOSITORY_CACHE_URL that is not an http URL",
			env:  func(t *testing.T, v *issueVolume) { v.env["LEGION_REPOSITORY_CACHE_URL"] = "10.0.0.5:8418" },
			code: 3,
			says: func(*issueVolume) string { return "--cache-url must be an http:// URL with a host" },
		},
		{
			name: "a --cache-url with no LEGION_REPOSITORY_CACHE_TOKEN_FILE",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "http://10.0.0.5:8418") },
			code: 3,
			says: func(*issueVolume) string {
				return "LEGION_REPOSITORY_CACHE_TOKEN_FILE is not set: a repository cache needs its token"
			},
		},
		{
			name: "a cache token file that is blank",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "http://10.0.0.5:8418") },
			env: func(t *testing.T, v *issueVolume) {
				v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, "\n")
			},
			code: 3,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE names " },
		},
		{
			name: "an unknown flag",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--root", v.root) },
			code: 2,
			says: func(*issueVolume) string { return "-root" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t)
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.fetchArgs()
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
			if _, err := os.Stat(v.feed); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the feed was created (%v)", err)
			}
			if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
				t.Fatalf("TMPDIR holds %v (%v), want no credential left behind", entries, err)
			}
			if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("git ran a clone before the refusal (%v)", err)
			}
		})
	}
}

// `fetch` clones the repository bare into the feed with the provisioning token, which its git is
// handed as a one-shot credential on the container's own filesystem (its TMPDIR) and which is gone
// once it returns; the feed never holds it.
func TestWorkspaceInitFetchFillsTheFeed(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token

	code, stdout, stderr := v.run(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	if !noCacheLine.MatchString(stdout) {
		t.Fatalf("stdout %q, want the clone from GitHub with no repository cache", stdout)
	}
	if printed, held := printedBytes(t, noCacheLine.FindStringSubmatch(stdout)[1]), objectBytes(t, feed); printed != held || held == 0 {
		t.Fatalf("the clone printed %d bytes from GitHub, want the %d the feed's objects hold", printed, held)
	}
	if main := v.git(t, "--git-dir="+feed, "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the feed's main is %q, want the remote's", main)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatalf("the clone recorded no credential: %v", err)
	}
	tokenFile, held, _ := strings.Cut(strings.TrimSpace(string(credential)), " ")
	if held != "ghs_test" || !strings.HasPrefix(tokenFile, v.tmp+string(filepath.Separator)) {
		t.Fatalf("the clone was handed %s holding %q, want the provisioning token under TMPDIR %s", tokenFile, held, v.tmp)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after fetch returned, want its credential gone", entries, err)
	}
	holdsNoToken(t, v.feed, "after fetch")
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

// failClones has the volume's git fail its first clones of github.com/acme/widgets, one per
// entry of stderr, in order (fakeGit), and returns the file that counts every clone attempt.
func (v *issueVolume) failClones(t *testing.T, stderr ...string) string {
	t.Helper()
	dir := t.TempDir()
	for attempt, text := range stderr {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(attempt+1)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v.env["WINIT_CLONE_FAILURES"] = dir
	return filepath.Join(dir, "attempts")
}

// cloneAttempts is how many clones of github.com/acme/widgets the volume's git saw.
func cloneAttempts(t *testing.T, attempts string) int {
	t.Helper()
	body, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatalf("the fetch ran no clone: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// failFetches has the volume's git fail every fetch from github.com/acme/widgets as git does
// (fakeGit): each writes stderr and exits with status. A clone does not fail.
func (v *issueVolume) failFetches(t *testing.T, stderr string, status int) {
	t.Helper()
	file := filepath.Join(t.TempDir(), strconv.Itoa(status))
	if err := os.WriteFile(file, []byte(stderr), 0o600); err != nil {
		t.Fatal(err)
	}
	v.env["WINIT_FETCH_FAILURE"] = file
}

// refetchArgs fills the volume's feed from its remote with a clone, which no WINIT_FETCH_*
// setting changes, and answers the arguments of a fetch into the feed that now holds the
// repository, with the volume's environment and the provisioning token set for it.
func (v *issueVolume) refetchArgs(t *testing.T) []string {
	t.Helper()
	v.fetch(t)
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	return v.fetchArgs()
}

// instantClock is the fetch's clock in a test: each wait passes at once, recorded, and moves the
// clock on by its length. atFirstWait, when set, runs as the first wait starts: what changes while
// the fetch waits.
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

// instantClock makes an instantClock the clock of the volume's fetches (issueVolume.proc).
func (v *issueVolume) instantClock() *instantClock {
	clock := &instantClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
	v.proc.clock = clock
	return clock
}

// A clone that fails transiently is run again after a backoff wait, each failure logged on stdout
// as one line, until one succeeds and fills the feed. Real git removes its own destination when a
// clone fails; this git leaves a partial clone there anyway, so the test holds the fetch to removing
// whatever a failed attempt left before it clones again.
func TestWorkspaceInitFetchRetriesATransientCloneFailure(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failClones(t, curl92Failure, http408Failure)
	clock := v.instantClock()
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token

	code, stdout, stderr := v.run(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	want := "workspace-init fetch: clone attempt 1 failed; retrying in 5s: error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8) | fatal: early EOF | fatal: fetch-pack: invalid index-pack output\n" +
		"workspace-init fetch: clone attempt 2 failed; retrying in 15s: error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408 | fatal: expected 'packfile'\n"
	rest, retried := strings.CutPrefix(stdout, want)
	if !retried || !noCacheLine.MatchString(rest) {
		t.Fatalf("stdout %q, want %q then the clone from GitHub", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want nothing", stderr)
	}
	if got := cloneAttempts(t, attempts); got != 3 {
		t.Fatalf("git cloned %d times, want 3", got)
	}
	if waits := clock.waits; !slices.Equal(waits, []time.Duration{5 * time.Second, 15 * time.Second}) {
		t.Fatalf("the fetch waited %v, want 5s then 15s", waits)
	}
	if main := v.git(t, "--git-dir="+feed, "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the feed's main is %q, want the remote's", main)
	}
	if _, err := os.Stat(filepath.Join(feed, "partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the feed still holds a failed attempt's partial clone (%v)", err)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after fetch returned, want its credential gone", entries, err)
	}
}

// A clone that keeps failing transiently is retried four times, one per backoff wait, and the
// fifth failure fails the fetch: the four retries on stdout, the failure and the limit on stderr.
// GitHub's failure is outside the issue's volume, so the fetch exits 3.
func TestWorkspaceInitFetchGivesUpAfterItsLastRetry(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failClones(t, http408Failure, http408Failure, http408Failure, http408Failure, http408Failure, http408Failure)
	clock := v.instantClock()
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token

	code, stdout, stderr := v.run(v.fetchArgs())
	if says := "clone attempt 5 failed, and the fetch retries a clone at most 4 times"; code != 3 || !strings.Contains(stderr, says) {
		t.Fatalf("exit %d, stderr %q; want 3 naming %q", code, stderr, says)
	}
	if retries := strings.Count(stdout, "workspace-init fetch: clone attempt "); retries != 4 {
		t.Fatalf("stdout %q logs %d retries, want 4", stdout, retries)
	}
	if got := cloneAttempts(t, attempts); got != 5 {
		t.Fatalf("git cloned %d times, want 5", got)
	}
	if waits := clock.waits; !slices.Equal(waits, []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}) {
		t.Fatalf("the fetch waited %v, want 5s, 15s, 30s, 1m", waits)
	}
}

// A clone GitHub refuses for good fails on its first attempt, with no wait and no retry, naming
// git's own message on stderr and logging nothing on stdout, and exits 3: GitHub's refusal is
// outside the issue's volume. Which failures are permanent is workspace.Fetch's to tell; this is
// what the command does with one.
func TestWorkspaceInitFetchFailsAPermanentCloneFailureAtOnce(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	notFound := "remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"
	attempts := v.failClones(t, notFound, notFound)
	clock := v.instantClock()
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token

	code, stdout, stderr := v.run(v.fetchArgs())
	if says := "fatal: repository 'https://github.com/acme/widgets/' not found"; code != 3 || !strings.Contains(stderr, says) {
		t.Fatalf("exit %d, stderr %q; want 3 naming %q", code, stderr, says)
	}
	if stdout != "" {
		t.Fatalf("stdout %q, want nothing", stdout)
	}
	if got := cloneAttempts(t, attempts); got != 1 {
		t.Fatalf("git cloned %d times, want 1", got)
	}
	if waits := clock.waits; len(waits) != 0 {
		t.Fatalf("the fetch waited %v, want no wait", waits)
	}
}

// Neither a retried attempt's log line nor the final failure carries the provisioning token, even
// where git echoes it: in a URL the authority that carries it is written as the mark of a URL with
// credentials (the URL rule, redact.Writer), and anywhere else the token says *** in its place.
func TestWorkspaceInitFetchLogsNoToken(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.failClones(t,
		"fatal: unable to access 'https://x-access-token:@TOKEN@@github.com/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n",
		"remote: Invalid username or token @TOKEN@.\nfatal: Authentication failed for 'https://x-access-token:@TOKEN@@github.com/acme/widgets/'\n",
	)
	v.instantClock()
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token

	code, stdout, stderr := v.run(v.fetchArgs())
	if code != 3 {
		t.Fatalf("exit %d, stderr %q; want 3", code, stderr)
	}
	if strings.Contains(stdout+stderr, "ghs_test") {
		t.Fatalf("the fetch logged the provisioning token:\nstdout %q\nstderr %q", stdout, stderr)
	}
	if want := "workspace-init fetch: clone attempt 1 failed; retrying in 5s: fatal: unable to access 'https://[a URL with credentials]/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	for _, says := range []string{"remote: Invalid username or token ***.", "fatal: Authentication failed for 'https://[a URL with credentials]/acme/widgets/'"} {
		if !strings.Contains(stderr, says) {
			t.Fatalf("stderr %q, want it to name %q", stderr, says)
		}
	}
}

// noCacheLine is the one line `fetch` prints when no repository cache is named: the clone from
// GitHub, with its bytes.
var noCacheLine = regexp.MustCompile(`^workspace-init fetch: no repository cache named: cloned acme/widgets from GitHub \(([0-9,]+) bytes\)\n$`)

// printedBytes is a byte count as `fetch` prints it, digits grouped by commas.
func printedBytes(t *testing.T, printed string) int64 {
	t.Helper()
	if !regexp.MustCompile(`^[0-9]{1,3}(,[0-9]{3})*$`).MatchString(printed) {
		t.Fatalf("%q is not a count grouped by commas", printed)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(printed, ",", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// objectBytes is how many bytes the regular files under the bare repository's objects hold.
func objectBytes(t *testing.T, gitDir string) int64 {
	t.Helper()
	var size int64
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(_ string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			size += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return size
}

// advanceRemote puts one more commit on the volume's remote's main and returns its id.
func (v *issueVolume) advanceRemote(t *testing.T) string {
	t.Helper()
	scratch := t.TempDir()
	work := filepath.Join(scratch, "work")
	for _, argv := range [][]string{
		{"clone", "--quiet", v.env["WINIT_REMOTE"], work},
		{"-C", work, "commit", "--quiet", "--allow-empty", "-m", "advanced"},
		{"-C", work, "push", "--quiet", "origin", "HEAD:main"},
	} {
		// The clone creates work, so every command in the run names its parent as the directory
		// it runs in rather than inheriting the test process's.
		v.runner(t).Run(scratch, append([]string{v.env["WINIT_REAL_GIT"]}, argv...)...)
	}
	return v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "main")
}

// cacheFault is what a test's repository cache does wrong, nothing when it is zero. notReady
// answers every /healthz 503, as a cache with no clone yet does. damagedPack flips one byte in the
// middle of the tar's pack, so the pack fails its checksum. fullPack puts a symbolic link to
// /dev/full at the path of the tar's pack in the fetch's staging directory, once the /tar request's
// token is checked and before the tar streams. The fetch's open of the pack follows the link, so
// the write of the pack to the volume fails inside its copy.
type cacheFault struct {
	notReady, damagedPack, fullPack bool
}

// cacheTarServer serves, as the repository cache's GET /healthz and GET /tar, a ready cache and the
// history and refs of a bare clone of the volume's remote as it is now, with fault, and returns its
// address and the tar's length. The clone runs over git's transport, so its history is one pack,
// as the cache's is. Any other path, /packs among them, is the mux's plain 404 naming no
// repository, as a cache with no bucket answers it.
func (v *issueVolume) cacheTarServer(t *testing.T, fault cacheFault) (string, int) {
	t.Helper()
	scratch := t.TempDir()
	cache := filepath.Join(scratch, "widgets.git")
	// The clone creates the cache, so it runs in the scratch directory holding it rather than in
	// the directory the test process happens to have.
	clone := []string{v.env["WINIT_REAL_GIT"], "clone", "--bare", "--quiet", "--no-local", v.env["WINIT_REMOTE"], cache}
	if output, err := v.runner(t).Try(scratch, clone...); err != nil {
		t.Fatalf("clone the cache: %v\n%s", err, output)
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
		if !workspace.FeedEntry(name, entry.IsDir()) {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		switch r.URL.Path {
		case "/healthz":
			if fault.notReady {
				http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ready":true,"repo":"acme/widgets","bytes":1}`))
			return
		case "/tar":
			if r.Header.Get("Authorization") != "Bearer "+cmdCacheToken {
				w.Header().Del(workspace.CacheRepositoryHeader)
				http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
				return
			}
			if fault.fullPack {
				if err := v.linkToDevFull(pack); err != nil {
					t.Errorf("put /dev/full at the fetch's pack: %v", err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		default:
			w.Header().Del(workspace.CacheRepositoryHeader)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL, len(body)
}

// linkToDevFull puts a symbolic link to /dev/full at entry, a path inside the one staging
// directory the fetch is filling beside the volume's feed (<feed>.clone-*).
func (v *issueVolume) linkToDevFull(entry string) error {
	staging, err := filepath.Glob(filepath.Join(v.feed, "acme", "widgets.git.clone-*"))
	if err != nil {
		return err
	}
	if len(staging) != 1 {
		return fmt.Errorf("the feed's directory holds %d staging directories %q, want the fetch's one", len(staging), staging)
	}
	link := filepath.Join(staging[0], filepath.FromSlash(entry))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	return os.Symlink("/dev/full", link)
}

// With a ready repository cache — LEGION_REPOSITORY_CACHE_URL and its token's file, which the
// runtime sets on every issue pod's fetch container — `fetch` unpacks the cache's tar into the feed
// and fetches from GitHub, with the provisioning token, only what the remote gained since: one line
// says both, with each source's bytes, after the line that says /tar was the source because this
// cache serves no /packs. A relaunch, whose feed on the issue volume holds the repository already,
// fetches the delta alone and says so, and names no source.
func TestWorkspaceInitFetchStreamsTheRepositoryCache(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	cache, length := v.cacheTarServer(t, cacheFault{})
	head := v.advanceRemote(t)
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	v.env["LEGION_REPOSITORY_CACHE_URL"] = cache
	v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken)

	code, stdout, stderr := v.run(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	line := regexp.MustCompile(`^` + regexp.QuoteMeta(notServedLine) + tarSummaryLine(cache, "1 ref") + `$`)
	match := line.FindStringSubmatch(stdout)
	if match == nil {
		t.Fatalf("stdout %q, want /tar as the source, then the cache's stream and GitHub's delta", stdout)
	}
	if printed := printedBytes(t, match[1]); printed != int64(length) {
		t.Fatalf("the line counts %d bytes from the cache, want the tar's %d", printed, length)
	}
	if delta := printedBytes(t, match[2]); delta <= 0 || delta >= 64<<10 {
		t.Fatalf("the line counts %d bytes from GitHub, want the one commit's few", delta)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	if main := v.git(t, "--git-dir="+feed, "rev-parse", "refs/heads/main"); main != head {
		t.Fatalf("the feed's main is %s, want the remote's %s", main, head)
	}
	credential := logLines(t, v.credentialLog)
	if len(credential) != 3 || !strings.HasSuffix(credential[0], " ghs_test") || !strings.HasSuffix(credential[1], " ghs_test") || !strings.HasSuffix(credential[2], " ghs_test") {
		t.Fatalf("GitHub was reached with %q, want the read of GitHub's HEAD that plans the delta, the delta's fetch and the read that follows HEAD, each with the provisioning token", credential)
	}
	holdsNoToken(t, v.feed, "after fetch")

	code, stdout, stderr = v.run(v.fetchArgs())
	if code != 0 {
		t.Fatalf("relaunch: exit %d, stderr %q", code, stderr)
	}
	if !regexp.MustCompile(`^workspace-init fetch: the feed holds acme/widgets already: ([0-9,]+) bytes from GitHub for the delta \(0 refs\)\n$`).MatchString(stdout) {
		t.Fatalf("relaunch stdout %q, want the delta alone", stdout)
	}
}

// A cache that never becomes ready fails the fetch, exit 3, naming the cache and its last answer,
// after one waiting line per change of that answer: nothing is cloned from GitHub instead.
func TestWorkspaceInitFetchFailsWhenTheCacheIsNeverReady(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	v.instantClock()

	v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken)
	code, stdout, stderr := v.run(append(v.fetchArgs(), "--cache-url", server.URL))
	if code != 3 || !strings.Contains(stderr, "the repository cache "+server.URL+" was not ready within "+workspace.CacheWaitTimeout.String()+": GET "+server.URL+"/healthz answered 503 Service Unavailable") {
		t.Fatalf("exit %d, stderr %q; want 3 naming the cache and its answer", code, stderr)
	}
	if !strings.HasPrefix(stdout, "workspace-init fetch: waiting for the repository cache "+server.URL+" to be ready: GET "+server.URL+"/healthz answered 503 Service Unavailable\n") {
		t.Fatalf("stdout %q, want the wait logged", stdout)
	}
	if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub was reached while the cache was not ready (%v)", err)
	}
}

// `fetch` exits 3 (workspace.InitExitOutsideVolume) for a cause outside the issue's volume, which a
// new volume does not cure, and 1 for every other failure: the runtime keeps a kept volume for the
// first and replaces it for the second. Each failing git prints git's own output, the lines
// internal/workspace reads in gitFailures.
//
// On a feed that holds the repository, a fetch from GitHub exits 3 when it fails with an HTTP
// status, a host that did not resolve, a pack stream cut with no cause line, a credential GitHub
// refused or a repository GitHub did not find. It exits 3 when its attempt runs out of its time,
// and when a read of the provisioning token before a retry fails. It exits 1 for curl 23, a write
// to the volume that failed, and for a bad object in the feed. A refusal before the volume exits 3:
// an unreadable provisioning token, no git on PATH. On a volume with no feed, a repository cache
// that is never ready, or that refuses the /tar request, exits 3. A cache tar whose write to the
// volume fails inside the copy, or whose pack fails its checksum, exits 1. A bad flag exits
// workspace.InitExitUsage, the usage exit of every legion command.
func TestWorkspaceInitFetchExitsThreeForAFailureFromGitHub(t *testing.T) {
	t.Parallel()
	const access = "fatal: unable to access 'https://github.com/acme/widgets/': "
	// failing is a fetch into a feed that holds the repository, whose every fetch from GitHub
	// fails with stderr and status.
	failing := func(stderr string, status int) func(*testing.T, *issueVolume, *instantClock) []string {
		return func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			v.failFetches(t, stderr, status)
			return v.refetchArgs(t)
		}
	}
	// fromCache is a fetch into the volume's empty feed from a repository cache of its remote with
	// fault, which presents token for the tar.
	fromCache := func(fault cacheFault, token string) func(*testing.T, *issueVolume, *instantClock) []string {
		return func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			cache, _ := v.cacheTarServer(t, fault)
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
			v.env["LEGION_REPOSITORY_CACHE_URL"] = cache
			v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, token)
			return v.fetchArgs()
		}
	}
	// timedOut is how the error of a fetch that ran out of its time begins: its git command, whose
	// first flags are the four pack settings (workspace.PackWindowConfig) as `-c` pairs.
	timedOut := "command timed out: git"
	for _, pair := range workspace.PackWindowConfig() {
		timedOut += " -c " + pair[0] + "=" + pair[1]
	}
	timedOut += " -c gc.auto=0 -c maintenance.auto=false --git-dir="
	for _, tc := range []struct {
		name string
		// fetch prepares the volume, its git, its cache, the clock and the environment, and
		// answers the fetch's arguments.
		fetch func(t *testing.T, v *issueVolume, clock *instantClock) []string
		code  int
		says  string
		// then, when set, checks what the fetch left.
		then func(t *testing.T, v *issueVolume)
	}{
		{name: "a 403", fetch: failing("remote: Write access to repository not granted.\n"+access+"The requested URL returned error: 403\n", 128),
			code: 3, says: "The requested URL returned error: 403"},
		{name: "a host that did not resolve", fetch: failing(access+"Could not resolve host: github.com\n", 128),
			code: 3, says: "fetch attempt 5 failed, and the fetch retries a fetch at most 4 times"},
		{name: "a pack stream cut with no cause line", fetch: failing("fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n", 128),
			code: 3, says: "fatal: early EOF"},
		{name: "a credential GitHub refused", fetch: failing("remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/acme/widgets/'\n", 128),
			code: 3, says: "fatal: Authentication failed for 'https://github.com/acme/widgets/'"},
		{name: "a repository GitHub did not find", fetch: failing("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n", 128),
			code: 3, says: "remote: Repository not found."},
		{name: "a write of received data that failed", fetch: failing("error: RPC failed; curl 23 Failed writing received data to disk/application\n", 128),
			code: 1, says: "curl 23 Failed writing received data to disk/application"},
		{name: "a bad object in the feed", fetch: func(t *testing.T, v *issueVolume, clock *instantClock) []string {
			return failing("fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111\nerror: file://"+v.env["WINIT_REMOTE"]+" did not send all necessary objects\n", 1)(t, v, clock)
		}, code: 1, says: "fatal: bad object refs/jj/keep/1111111111111111111111111111111111111111"},
		{name: "an attempt that runs out of its time", fetch: func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			// The fetch from GitHub sleeps past any test binary's deadline, so only the bound's stop
			// ends it: a stop a loaded machine makes late still comes before the sleep ends.
			v.env["WINIT_FETCH_SLEEP"] = "86400"
			args := v.refetchArgs(t)
			v.proc.fetchBound = time.Second
			return args
		}, code: 3, says: timedOut},
		{name: "a read of the provisioning token before a retry that fails", fetch: func(t *testing.T, v *issueVolume, clock *instantClock) []string {
			args := failing(http408Failure, 128)(t, v, clock)
			clock.atFirstWait = func() {
				if err := os.Remove(v.token); err != nil {
					t.Error(err)
				}
			}
			return args
		}, code: 3, says: "fetch attempt 2: read the provisioning token: "},
		{name: "an unreadable provisioning token", fetch: func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token + ".absent"
			return v.fetchArgs()
		}, code: 3, says: "LEGION_PROVISION_TOKEN_FILE names a file that could not be read: it does not exist"},
		{name: "no git on PATH", fetch: func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
			v.env["PATH"] = t.TempDir()
			return v.fetchArgs()
		}, code: 3, says: "git is not on PATH"},
		{name: "a repository cache that is never ready", fetch: fromCache(cacheFault{notReady: true}, cmdCacheToken),
			code: 3, says: "was not ready within " + workspace.CacheWaitTimeout.String()},
		{name: "a repository cache that refuses the tar", fetch: fromCache(cacheFault{}, "another-cache's-token"),
			code: 3, says: "/tar answered 401 Unauthorized"},
		{name: "a cache entry whose write fails inside the copy", fetch: func(t *testing.T, v *issueVolume, clock *instantClock) []string {
			if info, err := os.Stat("/dev/full"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
				t.Skipf("this case writes a cache entry to Linux's /dev/full, which this system does not have (%v)", err)
			}
			return fromCache(cacheFault{fullPack: true}, cmdCacheToken)(t, v, clock)
		}, code: 1, says: "no space left on device", then: func(t *testing.T, v *issueVolume) {
			if info, err := os.Stat("/dev/full"); err != nil || info.Mode()&os.ModeCharDevice == 0 {
				t.Errorf("/dev/full is %v (%v) after the fetch, want the device the link named", info, err)
			}
			if staging, err := filepath.Glob(filepath.Join(v.feed, "acme", "widgets.git.clone-*")); err != nil || len(staging) != 0 {
				t.Errorf("the fetch left %q (%v) beside the feed", staging, err)
			}
		}},
		{name: "a pack that fails its checksum", fetch: fromCache(cacheFault{damagedPack: true}, cmdCacheToken),
			code: 1, says: "which does not verify"},
		{name: "a bad flag", fetch: func(t *testing.T, v *issueVolume, _ *instantClock) []string {
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
			return append(v.fetchArgs(), "--bogus")
		}, code: workspace.InitExitUsage, says: "unknown flag: --bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			clock := v.instantClock()
			args := tc.fetch(t, v, clock)
			code, stdout, stderr := v.answered(t, "workspace-init fetch ("+tc.name+")", args)
			if code != tc.code || !strings.Contains(stderr, tc.says) {
				t.Fatalf("exit %d, stderr %q, stdout %q; want %d naming %q", code, stderr, stdout, tc.code, tc.says)
			}
			if tc.then != nil {
				tc.then(t, v)
			}
		})
	}
}

// notServedLine is the line `fetch` says /tar filled a fresh feed with because the cache serves no
// /packs: a cache with no bucket, or of a release before the bucket.
const notServedLine = "workspace-init fetch: source=/tar reason=/packs is not served\n"

// tarSummaryLine is the pattern of the line `fetch` ends a fresh feed filled from a cache's tar
// with, byte for byte as before the bucket: the tar's bytes from the cache at cache, how long it
// streamed and how long its history took to verify, then the delta's bytes from GitHub and its
// refs, refs as `fetch` counts them. Its two groups are the two byte counts.
func tarSummaryLine(cache, refs string) string {
	return `workspace-init fetch: ([0-9,]+) bytes from the repository cache ` + regexp.QuoteMeta(cache) +
		` in [0-9.]+(?:µs|ms|s), its history verified in [0-9.]+(?:µs|ms|s); ([0-9,]+) bytes from GitHub for the delta \(` + regexp.QuoteMeta(refs) + `\)\n`
}

// bucketSummaryLine is the line `fetch` ends a fresh feed filled from the cache's bucket with: the
// bytes of every object it downloaded and verified, how long the manifest and the downloads took
// and how long the history took to verify, then the delta's bytes from GitHub and its refs.
var bucketSummaryLine = regexp.MustCompile(`^workspace-init fetch: ([0-9,]+) bytes from the repository cache http://[^ ]+ \(bucket\) in [0-9.]+(?:µs|ms|s), its history verified in [0-9.]+(?:µs|ms|s); ([0-9,]+) bytes from GitHub for the delta \(1 ref\)$`)

// packsCacheRun is a `workspace-init cache` a test runs in-process on its volume's root, beside a
// double of Cloud Storage and IAM (fakegcs) on a loopback address of its own that
// STORAGE_EMULATOR_HOST names, as the real-binary harness runs it: the cache's address, the
// double, how far the double's clock runs ahead of the test's, which a signed download's expiry
// reads, what the command printed on its standard output and error, and stop, which ends the
// command with its context and answers its exit; a run the test does not stop is stopped when the
// test ends, and fails the test there unless it exits 0.
type packsCacheRun struct {
	base           string
	double         *fakegcs.Server
	skew           atomic.Int64
	stdout, stderr *lockedBuffer
	stop           func() int
}

// startPacksCache plants a branch on the volume's remote, whose commit it answers, packs the remote
// so the cache's clone holds a pack and its index, and runs the cache on the volume's process, its
// bucket set when bucket is, its token cmdCacheToken, until the test ends. It returns once the
// cache answers a manifest on /packs with the bucket set, or ready on /healthz without. The
// volume's environment stays set for the fetch the test runs next. The double's address is the
// one variable it sets on the test process itself (STORAGE_EMULATOR_HOST): the cache's bucket
// reads it there, as Google's clients do, so a test that calls this runs beside no other.
func (v *issueVolume) startPacksCache(t *testing.T, bucket bool) (*packsCacheRun, string) {
	t.Helper()
	remote := v.env["WINIT_REMOTE"]
	v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/planted", "planted.txt", "planted\n")
	planted := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/planted")
	// The volume's git clones the remote by its path, which copies the remote's objects as they
	// are, so the remote is packed first: the cache's clone then holds a pack and its index, as a
	// clone from GitHub does.
	v.git(t, "--git-dir="+remote, "repack", "-a", "-d", "--quiet")
	cache := &packsCacheRun{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}}
	double, err := fakegcs.New(fakegcs.Options{Root: t.TempDir(), Email: repocache.EmulatorEmail, Now: func() time.Time { return time.Now().Add(time.Duration(cache.skew.Load())) }})
	if err != nil {
		t.Fatal(err)
	}
	// The cmd/legion test binary links no testcontainers (TestThisTestBinaryLinksNoTestcontainers),
	// and internal/testnats does, so the double listens on a loopback port of the kernel's choosing
	// here.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the double: %v", err)
	}
	running := double.Serve(listener)
	t.Cleanup(func() { _ = running.Close() })
	cache.double = double
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken+"\n")
	t.Setenv("STORAGE_EMULATOR_HOST", running.Addr())
	if bucket {
		v.env[repocache.BucketEnv] = cmdTestBucket
	} else {
		delete(v.env, repocache.BucketEnv)
	}
	// The cache's process is read here, before its goroutine starts, so a variable the test sets
	// on the volume for its fetch afterwards is no write beside the cache's read.
	process := v.process()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := cache.stdout, cache.stderr
	exited := make(chan int, 1)
	go func() {
		exited <- runIn(ctx, append([]string{"legion", "workspace-init"}, v.cacheArgs()...), stdout, stderr, process)
	}()
	stopped, code := false, 0
	cache.stop = func() int {
		if !stopped {
			stopped = true
			cancel()
			code = testwait.Receive(t, "the cache to stop with its context", exited)
		}
		return code
	}
	t.Cleanup(func() {
		if stopped {
			return
		}
		if code := cache.stop(); code != 0 {
			t.Errorf("cache exited %d once stopped, stderr %q", code, stderr.String())
		}
	})
	// Each wait, and each request within it, ends when what is left of the test binary's deadline
	// less 30 s has passed, so a cache that never answers fails the row by name.
	wait := testwait.Bounded(t, 1<<60, 30*time.Second)
	bounded, boundedCancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(boundedCancel)
	address := ""
	if !testwait.Holds(wait, 10*time.Millisecond, func() bool {
		for _, line := range strings.Split(stdout.String(), "\n") {
			if found, ok := strings.CutPrefix(line, "workspace-init cache: serving acme/widgets on "); ok {
				address = found
			}
		}
		return address != ""
	}) {
		t.Fatalf("cache never said where it serves: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	cache.base = "http://" + address
	path, bearer := "/healthz", ""
	if bucket {
		path, bearer = "/packs", cmdCacheToken
	}
	var status int
	var body []byte
	if !testwait.Holds(wait, 20*time.Millisecond, func() bool {
		status, _, body = cacheGet(t, bounded, cache.base+path, bearer)
		return status == http.StatusOK && (bucket || bytes.Contains(body, []byte(`"ready":true`)))
	}) {
		t.Fatalf("%s never answered 200: last %d %s; stdout %q, stderr %q", path, status, body, stdout.String(), stderr.String())
	}
	return cache, planted
}

// cachePack is one pack or index of the volume's cache clone: its size and the SHA-256 of its
// bytes, in hex.
type cachePack struct {
	size   int64
	sha256 string
}

// cachePacks is each pack and index of the volume's cache clone that a feed takes
// (workspace.FeedEntry), by its object name under the repository's lower-case prefix.
func (v *issueVolume) cachePacks(t *testing.T) map[string]cachePack {
	t.Helper()
	packs := filepath.Join(v.root, "acme", "widgets.git", "objects", "pack")
	entries, err := os.ReadDir(packs)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]cachePack{}
	for _, entry := range entries {
		if !workspace.FeedEntry("objects/pack/"+entry.Name(), false) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(packs, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		files["acme/widgets/objects/pack/"+entry.Name()] = cachePack{size: int64(len(content)), sha256: hex.EncodeToString(sum[:])}
	}
	return files
}

// cacheObjectLines is the line `fetch` prints for each pack and index of the volume's cache clone
// when it downloaded and verified it from the bucket: its object name under the repository's
// lower-case prefix, its size and the SHA-256 of the clone's own file, sorted.
func (v *issueVolume) cacheObjectLines(t *testing.T) ([]string, int64) {
	t.Helper()
	var lines []string
	var size int64
	for name, file := range v.cachePacks(t) {
		lines = append(lines, fmt.Sprintf("workspace-init fetch: bucket object=%s size=%d sha256=%s", name, file.size, file.sha256))
		size += file.size
	}
	if len(lines) != 2 {
		t.Fatalf("the cache's clone holds %q, not one pack and its index", lines)
	}
	slices.Sort(lines)
	return lines, size
}

// requirePrintsNoSecret fails the test unless stdout and stderr hold none of what a fetch from the
// cache must never print: the cache's bearer, the provisioning token, a V4 parameter, the query
// and the signature of each signed download the double received, and the planted ref's contents.
func requirePrintsNoSecret(t *testing.T, cache *packsCacheRun, planted, stdout, stderr string) {
	t.Helper()
	secrets := []string{cmdCacheToken, "ghs_test", "X-Goog-", planted}
	for _, request := range cache.double.Requests() {
		if request.Route == fakegcs.RouteDownload {
			secrets = append(secrets, request.URL.RawQuery, request.URL.Query().Get("X-Goog-Signature"))
		}
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(stdout+stderr, secret) {
			t.Errorf("the fetch printed %q: stdout %q, stderr %q", secret, stdout, stderr)
		}
	}
}

// `fetch` says where a fresh feed's history came from, before its last line, against the real
// cache command and a double of Cloud Storage. From a bucket whose every object holds it prints
// `source=bucket`, one `bucket object=` line for each pack and index with the object's name, exact
// size and the SHA-256 of the cache's own file, which it verified, and a last line of the bucket's
// bytes and times and the delta's; its downloads carry no Authorization; and a relaunch onto the
// feed it filled prints the kept feed's line alone, as before. A signed download the bucket refuses
// (an address past its expiry) has it print `source=/tar` with the failed download's reason, then
// /tar's line as before; a cache with no bucket has it print that /packs is not served, then /tar's
// line. A fetch with no cache named prints the line it did before the bucket alone. No fetch
// prints the cache's bearer, the provisioning token, a signed address's query or signature, or the
// contents of a ref the cache's manifest carries, a planted branch's.
func TestWorkspaceInitFetchReportsPackSource(t *testing.T) {
	// Not parallel: its cache rows set STORAGE_EMULATOR_HOST for the whole process (startPacksCache), which the cache's bucket reads there as Google's clients do.
	t.Run("a bucket whose every object holds fills the feed", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, planted := v.startPacksCache(t, true)
		head := v.advanceRemote(t)
		v.env["LEGION_REPOSITORY_CACHE_URL"] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init fetch from the bucket", v.fetchArgs())
		t.Logf("the fetch printed %q", stdout)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
		}
		objects, size := v.cacheObjectLines(t)
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		if len(lines) != len(objects)+2 || lines[0] != "workspace-init fetch: source=bucket" {
			t.Fatalf("stdout %q, want the bucket as the source, a line for each of its %d objects, and the summary", stdout, len(objects))
		}
		if got := slices.Sorted(slices.Values(lines[1 : len(lines)-1])); !slices.Equal(got, objects) {
			t.Errorf("the fetch printed the objects %q, want %q", got, objects)
		}
		match := bucketSummaryLine.FindStringSubmatch(lines[len(lines)-1])
		if match == nil {
			t.Fatalf("the last line %q is not the bucket's bytes and times and the delta's", lines[len(lines)-1])
		}
		if printed := printedBytes(t, match[1]); printed != size {
			t.Errorf("the line counts %d bytes from the bucket, want its objects' %d", printed, size)
		}
		if got := cache.double.Count(fakegcs.RouteDownload); got.Requests != len(objects) || got.WithAuthorization != 0 {
			t.Errorf("the double served %d downloads, %d with an Authorization header, want %d with none", got.Requests, got.WithAuthorization, len(objects))
		}
		feed := filepath.Join(v.feed, "acme", "widgets.git")
		if main := v.git(t, "--git-dir="+feed, "rev-parse", "refs/heads/main"); main != head {
			t.Errorf("the feed's main is %s, want the remote's %s", main, head)
		}
		requirePrintsNoSecret(t, cache, planted, stdout, stderr)

		code, stdout, stderr = v.answered(t, "workspace-init fetch onto the kept feed", v.fetchArgs())
		if code != 0 || !regexp.MustCompile(`^workspace-init fetch: the feed holds acme/widgets already: ([0-9,]+) bytes from GitHub for the delta \(0 refs\)\n$`).MatchString(stdout) {
			t.Fatalf("relaunch: exit %d, stdout %q, stderr %q; want the kept feed's delta alone", code, stdout, stderr)
		}
	})

	t.Run("a signed download the bucket refuses falls back to /tar", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, planted := v.startPacksCache(t, true)
		v.advanceRemote(t)
		cache.skew.Store(int64(workspace.BucketFetchTimeout + time.Minute))
		v.env["LEGION_REPOSITORY_CACHE_URL"] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init fetch past the addresses' expiry", v.fetchArgs())
		t.Logf("the fetch printed %q", stdout)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
		}
		reason := regexp.QuoteMeta("workspace-init fetch: source=/tar reason=failed download: acme/widgets/objects/pack/") +
			`pack-[0-9a-f]{40}\.(?:pack|idx)` + regexp.QuoteMeta(": the bucket answered 403 Forbidden (ExpiredToken)\n")
		if !regexp.MustCompile(`^` + reason + tarSummaryLine(cache.base, "1 ref") + `$`).MatchString(stdout) {
			t.Fatalf("stdout %q, want /tar as the source for the refused download, then /tar's line", stdout)
		}
		if got := cache.double.Count(fakegcs.RouteDownload); got.Requests == 0 || got.WithAuthorization != 0 {
			t.Errorf("the double served %d downloads, %d with an Authorization header, want some with none", got.Requests, got.WithAuthorization)
		}
		requirePrintsNoSecret(t, cache, planted, stdout, stderr)
	})

	t.Run("a cache with no bucket serves no /packs", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, planted := v.startPacksCache(t, false)
		v.advanceRemote(t)
		v.env["LEGION_REPOSITORY_CACHE_URL"] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init fetch from a cache with no bucket", v.fetchArgs())
		t.Logf("the fetch printed %q", stdout)
		if code != 0 || !regexp.MustCompile(`^`+regexp.QuoteMeta(notServedLine)+tarSummaryLine(cache.base, "1 ref")+`$`).MatchString(stdout) {
			t.Fatalf("exit %d, stdout %q, stderr %q; want /tar as the source, /packs not served, then /tar's line", code, stdout, stderr)
		}
		if requests := cache.double.Requests(); len(requests) != 0 {
			t.Errorf("a cache and a fetch with no bucket sent the double %d requests", len(requests))
		}
		requirePrintsNoSecret(t, cache, planted, stdout, stderr)
	})

	t.Run("a fetch with no cache named clones from GitHub", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
		code, stdout, stderr := v.answered(t, "workspace-init fetch with no cache", v.fetchArgs())
		if code != 0 || !noCacheLine.MatchString(stdout) {
			t.Fatalf("exit %d, stdout %q, stderr %q; want the clone from GitHub's line alone", code, stdout, stderr)
		}
	})
}
