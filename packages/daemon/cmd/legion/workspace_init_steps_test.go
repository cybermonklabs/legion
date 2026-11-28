package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// stepLine is one line workspace-init's step clock (workspace.Steps) writes to its log: a step
// that ended and how long it took, the step a failure ended and how long it ran, or the run of
// every step, each in seconds with one decimal.
var stepLine = regexp.MustCompile(`^workspace-init: (?:step ([a-z]+(?:-[a-z]+)*) (took|failed after) ([0-9]+\.[0-9])s|steps took ([0-9]+\.[0-9])s)$`)

// withoutStepLines is stdout, workspace-init's log, without the step clock's lines (stepLine): the
// log of every other line, for a reader that reads which lines come first, how many there are, or
// which come next to each other.
func withoutStepLines(stdout string) string {
	var kept strings.Builder
	for _, line := range strings.SplitAfter(stdout, "\n") {
		if !stepLine.MatchString(strings.TrimSuffix(line, "\n")) {
			kept.WriteString(line)
		}
	}
	return kept.String()
}

// stepTenths is a step line's duration in tenths of a second.
func stepTenths(t *testing.T, seconds string) int {
	t.Helper()
	tenths, err := strconv.Atoi(strings.Replace(seconds, ".", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	return tenths
}

// provisionLog runs `provision` for issue on the volume inside this test process, its standard
// output and its standard error written to one log in the order they come, as an init container's
// log takes them, and answers its exit, that log, and the wall time from before the run started to
// after it ended. The test binary's deadline is the wait's only bound (testwait.Receive).
func (v *issueVolume) provisionLog(t *testing.T, issue string) (code int, log string, wall time.Duration) {
	t.Helper()
	var output testlog.Buffer
	ran := make(chan int, 1)
	started := time.Now()
	go func() { ran <- v.runContext(context.Background(), v.args(issue), &output, &output) }()
	code = testwait.Receive(t, "the provisioning of "+issue, ran)
	return code, output.String(), time.Since(started)
}

// requireSteps fails the test unless log, the whole log of one provision, holds one line for each
// of the steps names in that order, each of stepLine's form: on success the last of them took its
// time, `steps took <seconds>s` follows, and then last, the provision's own last line; on a failure
// (failed) the last of them failed after its time, and the provision's error follows and ends the
// log. The steps' durations add up to the total within 0.1 s for each step, each being rounded
// to a tenth, and neither the total nor, on a failure, their sum is more than wall, the time the
// test measured around the run, but for that rounding.
func requireSteps(t *testing.T, log string, wall time.Duration, names []string, last string, failed bool) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	var got []string
	sum, total, ended, failedAt := 0, -1, -1, -1
	for i, line := range lines {
		if !strings.HasPrefix(line, "workspace-init: step") {
			continue
		}
		match := stepLine.FindStringSubmatch(line)
		switch {
		case match == nil:
			t.Errorf("the log's line %q is not a step line's form, `workspace-init: step <name> took <seconds>s`, `… failed after <seconds>s` or `workspace-init: steps took <seconds>s`", line)
			continue
		case ended >= 0 || failedAt >= 0:
			t.Errorf("the log's line %q comes after the steps ended", line)
		}
		if match[1] == "" {
			total, ended = stepTenths(t, match[4]), i
			continue
		}
		got = append(got, match[1])
		sum += stepTenths(t, match[3])
		if match[2] == "failed after" {
			failedAt = i
		}
	}
	if !slices.Equal(got, names) {
		t.Fatalf("the log names the steps %q, want %q; the log:\n%s", got, names, log)
	}
	// Each duration is printed to the nearest tenth, so each strays from its step's by 0.05 s at
	// most, and so does the total.
	rounding := time.Duration(len(names)+1) * 50 * time.Millisecond
	if failed {
		switch {
		case failedAt < 0 || ended >= 0:
			t.Fatalf("the log's steps end with no failed step, or with a total; the log:\n%s", log)
		case !strings.HasPrefix(strings.Join(lines[failedAt+1:], "\n"), "legion workspace-init provision: "):
			t.Fatalf("the failed step's line is followed by %q, want the provision's error; the log:\n%s", lines[failedAt+1:], log)
		case slices.ContainsFunc(lines[failedAt+1:], func(line string) bool { return strings.HasPrefix(line, "workspace-init: ") }):
			t.Fatalf("the log goes on after the provision's error; the log:\n%s", log)
		case time.Duration(sum)*100*time.Millisecond > wall+rounding:
			t.Fatalf("the steps took %d tenths of a second together, more than the run's %s; the log:\n%s", sum, wall, log)
		}
		return
	}
	switch {
	case failedAt >= 0 || ended < 0:
		t.Fatalf("the log's steps end with a failed step, or with no total; the log:\n%s", log)
	case ended != len(lines)-2 || lines[len(lines)-1] != last:
		t.Fatalf("the log ends %q, want the total and then %q; the log:\n%s", lines[ended:], last, log)
	case sum-total > len(names) || total-sum > len(names):
		t.Fatalf("the steps took %d tenths of a second together and the run %d, more than 0.1 s apart for each of the %d steps; the log:\n%s", sum, total, len(names), log)
	case time.Duration(total)*100*time.Millisecond > wall+50*time.Millisecond:
		t.Fatalf("the steps took %d tenths of a second, more than the run's %s; the log:\n%s", total, wall, log)
	}
}

// wallClockWithNoWaits is a provision's clock that reads the wall clock and whose waits pass at
// once: a fetch's backoff between its retries costs the run nothing, and each step's time is the
// test's own wall time.
type wallClockWithNoWaits struct{}

func (wallClockWithNoWaits) Now() time.Time { return time.Now() }

func (wallClockWithNoWaits) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// bucketFault is what the bucket of bucketCacheServer's repository cache does wrong.
type bucketFault int

const (
	// bucketWhole serves every object as the manifest states it.
	bucketWhole bucketFault = iota
	// bucketRefusesDownloads answers every download 403, as Cloud Storage answers an address it
	// does not take.
	bucketRefusesDownloads
	// bucketDamagedPack serves the pack with one byte flipped, and the manifest states the flipped
	// bytes' size and SHA-256: it downloads whole, and fails the checksum git wrote into it.
	bucketDamagedPack
)

// bucketCacheServer is a repository cache of the volume's remote's default branch that keeps its
// packs in a bucket, with fault, and answers its address. Its /packs answers the cache's token with
// a manifest of a bare clone of the remote: its refs, and its pack and index, each at an address of
// this server's own under cmdTestBucket, where it serves them. Its /healthz and /tar are
// narrowedCacheTarServer's, which it passes them to.
func (v *issueVolume) bucketCacheServer(t *testing.T, fault bucketFault) string {
	t.Helper()
	tarCache, _ := v.narrowedCacheTarServer(t)
	upstream, err := url.Parse(tarCache)
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(t.TempDir(), "widgets.git")
	if output, err := v.runner(t).Try(filepath.Dir(clone), v.env["WINIT_REAL_GIT"], "clone", "--bare", "--single-branch", "--no-tags", "--quiet", "--no-local", v.env["WINIT_REMOTE"], clone); err != nil {
		t.Fatalf("clone the bucket's cache: %v\n%s", err, output)
	}
	manifest := workspace.PackManifest{Bucket: cmdTestBucket}
	objects := map[string][]byte{}
	err = filepath.WalkDir(clone, func(file string, entry os.DirEntry, err error) error {
		if err != nil || file == clone {
			return err
		}
		relative, err := filepath.Rel(clone, file)
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
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if path.Dir(name) != "objects/pack" {
			manifest.Refs = append(manifest.Refs, workspace.PackRef{Path: name, Bytes: body})
			return nil
		}
		if fault == bucketDamagedPack && strings.HasSuffix(name, ".pack") {
			body[len(body)/2] ^= 0xff
		}
		object := workspace.PackPrefix(ghrepo.MustParse(winitRepo)) + path.Base(name)
		sum := sha256.Sum256(body)
		objects["/"+cmdTestBucket+"/"+object] = body
		manifest.Objects = append(manifest.Objects, workspace.PackObject{Name: object, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Objects) != 2 {
		t.Fatalf("the bucket's cache %s holds the objects %v, want one pack and its index", clone, manifest.Objects)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		object, isObject := objects[r.URL.Path]
		switch {
		case r.URL.Path == "/packs":
			if r.Header.Get("Authorization") != "Bearer "+cmdCacheToken {
				http.Error(w, "the repository cache token is required", http.StatusUnauthorized)
				return
			}
			w.Header().Set(workspace.CacheRepositoryHeader, winitRepo)
			if err := json.NewEncoder(w).Encode(manifest); err != nil {
				t.Errorf("answer /packs: %v", err)
			}
		case isObject && fault == bucketRefusesDownloads:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
		case isObject:
			_, _ = w.Write(object)
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	base := "http://" + server.Listener.Addr().String()
	for i, object := range manifest.Objects {
		manifest.Objects[i].URL = base + "/" + cmdTestBucket + "/" + object.Name + "?X-Goog-Signature=steps"
	}
	server.Start()
	t.Cleanup(server.Close)
	return server.URL
}

// The steps of a fresh clone's provision after its history is in the clone: the delta from
// GitHub, the clone's jj, the workspace and its checkout.
var freshCloneSteps = []string{
	"github-delta", "ref-pruning", "jj-init", "jj-clone-working-copy", "jj-track", "jj-configuration",
	"workspace-registration", "configuration", "checkout",
}

// The steps of a provision onto a kept clone from its jj configuration to its tracking of the
// default branches, before its workspace's checkout-check and registration.
var keptCloneSteps = []string{"jj-configuration", "github-delta", "ref-pruning", "jj-import", "jj-track"}

// `provision` logs one line for each step it runs, `workspace-init: step <name> took <seconds>s`,
// as the step ends, and then `workspace-init: steps took <seconds>s` before its last line; a
// provision that fails logs `workspace-init: step <name> failed after <seconds>s` for the step it
// failed in, and its error follows as the last line of its log. Each row is one path through the
// provision and names its steps in the order it runs them: a new clone from GitHub alone (a), from
// a repository cache's /tar where it serves no /packs (b), from its bucket (c), from /tar after the
// bucket's download failed (d) or after the bucket's pack failed its check (d2); a new clone on a
// volume where a stopped removal left the issue's workspace directory renamed aside (a2), which is
// deleted before anything else of the provision (workspace-delete); a kept clone with no workspace
// of the issue's (e0, another issue's provision on the volume), with the issue's workspace checked
// out (e), with that workspace's sparse patterns cleared and no marker (e1), and with a checkout
// Legion started and that was cut short (f), whose workspace is taken away and added again
// (workspace-reset), its old directory deleted before the checkout (workspace-delete); and a fetch
// from GitHub that fails after its retries (g). The steps' durations add up to the total within
// 0.1 s for each, and the total is no more than the run's wall time. A refusal before the volume (h)
// prints nothing on stdout.
func TestWorkspaceInitLogsEachStepItRuns(t *testing.T) {
	t.Parallel()
	// fromCache names the repository cache at cache to the volume's provision, with its token.
	fromCache := func(t *testing.T, v *issueVolume, cache string) {
		v.env[cacheURLEnv] = cache
		v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken)
	}
	fromTar := func(t *testing.T, v *issueVolume) {
		cache, _ := v.narrowedCacheTarServer(t)
		fromCache(t, v, cache)
	}
	fromBucket := func(fault bucketFault) func(*testing.T, *issueVolume) {
		return func(t *testing.T, v *issueVolume) { fromCache(t, v, v.bucketCacheServer(t, fault)) }
	}
	for _, tc := range []struct {
		path string
		// prepare readies the volume the row's provision of LEGION-42 runs on.
		prepare func(t *testing.T, v *issueVolume)
		steps   []string
		failed  bool
	}{
		{
			path:  "a. a new clone with no repository cache",
			steps: slices.Concat([]string{"inputs", "stale-locks", "clone-stage"}, freshCloneSteps),
		},
		{
			path: "a2. a new clone where a stopped removal left the workspace's directory aside",
			prepare: func(t *testing.T, v *issueVolume) {
				aside := v.workspace("LEGION-42") + ".removing"
				if err := os.MkdirAll(filepath.Join(aside, ".jj"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(aside, "half.txt"), []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			steps: slices.Concat([]string{"inputs", "stale-locks", "workspace-delete", "clone-stage"}, freshCloneSteps),
		},
		{
			path:    "b. a new clone from a repository cache's /tar, which serves no /packs",
			prepare: fromTar,
			steps:   slices.Concat([]string{"inputs", "stale-locks", "clone-stage", "cache-wait", "bucket-attempt", "cache-transfer", "pack-check", "history-check"}, freshCloneSteps),
		},
		{
			path:    "c. a new clone from a repository cache's bucket",
			prepare: fromBucket(bucketWhole),
			steps:   slices.Concat([]string{"inputs", "stale-locks", "clone-stage", "cache-wait", "bucket-attempt", "pack-check", "history-check"}, freshCloneSteps),
		},
		{
			path:    "d. a new clone from /tar after the bucket's download failed",
			prepare: fromBucket(bucketRefusesDownloads),
			steps:   slices.Concat([]string{"inputs", "stale-locks", "clone-stage", "cache-wait", "bucket-attempt", "cache-transfer", "pack-check", "history-check"}, freshCloneSteps),
		},
		{
			path:    "d2. a new clone from /tar after the bucket's pack failed its check",
			prepare: fromBucket(bucketDamagedPack),
			steps:   slices.Concat([]string{"inputs", "stale-locks", "clone-stage", "cache-wait", "bucket-attempt", "pack-check", "cache-transfer", "pack-check", "history-check"}, freshCloneSteps),
		},
		{
			path:    "e0. a kept clone with no workspace of the issue's",
			prepare: func(t *testing.T, v *issueVolume) { v.provisioned(t, "LEGION-41") },
			steps:   slices.Concat([]string{"inputs", "stale-locks"}, keptCloneSteps, []string{"workspace-registration", "configuration", "checkout"}),
		},
		{
			path:    "e. a kept clone with the issue's workspace checked out",
			prepare: func(t *testing.T, v *issueVolume) { v.provisioned(t, "LEGION-42") },
			steps:   slices.Concat([]string{"inputs", "stale-locks", "update-stale"}, keptCloneSteps, []string{"checkout-check", "workspace-registration", "configuration"}),
		},
		{
			path: "e1. a kept clone with the issue's workspace not checked out",
			prepare: func(t *testing.T, v *issueVolume) {
				v.provisioned(t, "LEGION-42")
				// An agent cleared the workspace's sparse patterns, leaving no marker.
				v.jj(t, "sparse", "set", "--clear", "-R", v.workspace("LEGION-42"))
			},
			steps: slices.Concat([]string{"inputs", "stale-locks", "update-stale"}, keptCloneSteps, []string{"checkout-check", "workspace-registration", "configuration", "checkout"}),
		},
		{
			path: "f. a kept clone with a checkout cut short",
			prepare: func(t *testing.T, v *issueVolume) {
				script := filepath.Join(t.TempDir(), "cut.sh")
				if err := execfile.Write(script, []byte("#!/bin/sh\nprintf partial > half.txt\nexit 137\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				v.env["WINIT_CUT_CHECKOUT"] = script
				code, stdout, stderr := v.answered(t, "cut-short first provision", v.args("LEGION-42"))
				if code == 0 {
					t.Fatalf("first provision succeeded despite cut-short checkout stand-in:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
				}
				v.env["WINIT_CUT_CHECKOUT"] = ""
			},
			steps: slices.Concat([]string{"inputs", "stale-locks", "update-stale"}, keptCloneSteps, []string{"checkout-check", "workspace-reset", "workspace-registration", "configuration", "workspace-delete", "checkout"}),
		},
		{
			path: "g. a fetch from GitHub that fails after its retries",
			prepare: func(t *testing.T, v *issueVolume) {
				fromTar(t, v)
				v.failFetches(t, 128, slices.Repeat([]string{curl92Failure}, 5)...)
				v.proc.clock = wallClockWithNoWaits{}
			},
			steps:  []string{"inputs", "stale-locks", "clone-stage", "cache-wait", "bucket-attempt", "cache-transfer", "pack-check", "history-check", "github-delta"},
			failed: true,
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			v := newIssueVolume(t).withRemote(t, "main")
			v.provisionEnv()
			if tc.prepare != nil {
				tc.prepare(t, v)
			}
			code, log, wall := v.provisionLog(t, "LEGION-42")
			t.Logf("the provision exited %d in %s and logged:\n%s", code, wall, log)
			if tc.failed != (code != 0) {
				t.Fatalf("the provision exited %d; the log:\n%s", code, log)
			}
			last := "workspace-init: " + v.workspace("LEGION-42") + " on legion/LEGION-42 from main"
			requireSteps(t, log, wall, tc.steps, last, tc.failed)
		})
	}

	t.Run("h. a refusal before the volume", func(t *testing.T) {
		t.Parallel()
		v := newIssueVolume(t).withRemote(t, "main")
		code, stdout, stderr := v.run(v.args("LEGION-42"))
		if code != workspace.InitExitOutsideVolume || stdout != "" || !strings.Contains(stderr, provisionTokenFileEnv+" is not set") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d, nothing on stdout, and the refusal", code, stdout, stderr, workspace.InitExitOutsideVolume)
		}
	})
}
