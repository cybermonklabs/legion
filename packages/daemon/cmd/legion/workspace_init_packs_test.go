package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// notServedLine is the line provision says /tar filled a fresh clone with because the cache serves
// no /packs: a cache with no bucket, or of a release before the bucket.
const notServedLine = "workspace-init: source=/tar reason=/packs is not served\n"

// tarSummaryLine is the pattern of the line provision ends a fresh clone filled from a cache's tar
// with, byte for byte as before the bucket: the tar's bytes from the cache at cache, how long it
// streamed and how long its history took to verify, then the delta's bytes from GitHub and its
// refs. Its two groups are the two byte counts.
func tarSummaryLine(cache string) string {
	return `workspace-init: ([0-9,]+) bytes from the repository cache ` + regexp.QuoteMeta(cache) +
		` in [0-9.]+(?:µs|ms|s), its history verified in [0-9.]+(?:µs|ms|s); ([0-9,]+) bytes from GitHub for the delta \([0-9]+ refs?\)\n`
}

// bucketSummaryLine is the line provision ends a fresh clone filled from the cache's bucket with:
// the bytes of every object it downloaded and verified, how long the manifest and the downloads
// took and how long the history took to verify, then the delta's bytes from GitHub and its refs.
var bucketSummaryLine = regexp.MustCompile(`^workspace-init: ([0-9,]+) bytes from the repository cache http://[^ ]+ \(bucket\) in [0-9.]+(?:µs|ms|s), its history verified in [0-9.]+(?:µs|ms|s); ([0-9,]+) bytes from GitHub for the delta \([0-9]+ refs?\)$`)

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

// startPacksCache moves the volume's remote's main on by a planted commit, whose id it answers,
// packs the remote so the cache's clone holds a pack and its index, and runs the cache on the
// volume's process, its bucket set when bucket is, its token cmdCacheToken, until the test ends. It
// returns once the cache answers a manifest on /packs with the bucket set, or ready on /healthz
// without. The volume's environment stays set for the provision the test runs next. The double's
// address is the one variable it sets on the test process itself (STORAGE_EMULATOR_HOST): the
// cache's bucket reads it there, as Google's clients do, so a test that calls this runs beside no
// other.
func (v *issueVolume) startPacksCache(t *testing.T, bucket bool) (*packsCacheRun, string) {
	t.Helper()
	remote := v.env["WINIT_REMOTE"]
	v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/main", "planted.txt", "planted\n")
	planted := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/main")
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
	v.provisionEnv()
	v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken+"\n")
	t.Setenv("STORAGE_EMULATOR_HOST", running.Addr())
	if bucket {
		v.env[repocache.BucketEnv] = cmdTestBucket
	} else {
		delete(v.env, repocache.BucketEnv)
	}
	// The cache's process is read here, before its goroutine starts, so a variable the test sets
	// on the volume for its provision afterwards is no write beside the cache's read.
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

// cachePacks is each pack and index of the volume's cache clone that a clone takes
// (workspace.CacheEntry), by its object name under the repository's lower-case prefix.
func (v *issueVolume) cachePacks(t *testing.T) map[string]cachePack {
	t.Helper()
	packs := filepath.Join(v.root, "acme", "widgets.git", "objects", "pack")
	entries, err := os.ReadDir(packs)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]cachePack{}
	for _, entry := range entries {
		if !workspace.CacheEntry("objects/pack/"+entry.Name(), false) {
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

// cacheObjectLines is the line provision prints for each pack and index of the volume's cache clone
// when it downloaded and verified it from the bucket: its object name under the repository's
// lower-case prefix, its size and the SHA-256 of the clone's own file, sorted.
func (v *issueVolume) cacheObjectLines(t *testing.T) ([]string, int64) {
	t.Helper()
	var lines []string
	var size int64
	for name, file := range v.cachePacks(t) {
		lines = append(lines, fmt.Sprintf("workspace-init: bucket object=%s size=%d sha256=%s", name, file.size, file.sha256))
		size += file.size
	}
	if len(lines) != 2 {
		t.Fatalf("the cache's clone holds %q, not one pack and its index", lines)
	}
	slices.Sort(lines)
	return lines, size
}

// requirePrintsNoSecret fails the test unless stdout and stderr hold none of what a fill from the
// cache must never print: the cache's bearer, the provisioning token, a V4 parameter, and the query
// and the signature of each signed download the double received.
func requirePrintsNoSecret(t *testing.T, cache *packsCacheRun, stdout, stderr string) {
	t.Helper()
	secrets := []string{cmdCacheToken, "ghs_test", "X-Goog-"}
	for _, request := range cache.double.Requests() {
		if request.Route == fakegcs.RouteDownload {
			secrets = append(secrets, request.URL.RawQuery, request.URL.Query().Get("X-Goog-Signature"))
		}
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(stdout+stderr, secret) {
			t.Errorf("the provision printed %q: stdout %q, stderr %q", secret, stdout, stderr)
		}
	}
}

// `provision` says where a fresh clone's history came from, before its summary line, against the
// real cache command and a double of Cloud Storage. From a bucket whose every object holds it
// prints `source=bucket`, one `bucket object=` line for each pack and index with the object's
// name, exact size and the SHA-256 of the cache's own file, which it verified, and then the
// summary of the bucket's bytes and times, the cache named as `<url> (bucket)`, and the delta's;
// its downloads carry no Authorization; the clone's main is the remote's; and a provision onto the
// clone it filled prints the kept clone's line, and no source. A signed download the bucket refuses
// (an address past its expiry) has it print `source=/tar` with the failed download's reason, then
// /tar's summary as before; a cache with no bucket has it print that /packs is not served, then
// /tar's summary, and sends the double nothing. A provision with no cache named prints the line it
// did before the bucket and no source. No provision prints the cache's bearer, the provisioning
// token, or a signed address's query or signature.
func TestWorkspaceInitProvisionReportsPackSource(t *testing.T) {
	// Not parallel: its cache rows set STORAGE_EMULATOR_HOST for the whole process (startPacksCache), which the cache's bucket reads there as Google's clients do.
	t.Run("a bucket whose every object holds fills the clone", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, _ := v.startPacksCache(t, true)
		remote := v.env["WINIT_REMOTE"]
		v.commitAddingAFile(t, remote, "refs/heads/main", "refs/heads/main", "advanced.txt", "advanced\n")
		head := v.git(t, "--git-dir="+remote, "rev-parse", "refs/heads/main")
		v.env[cacheURLEnv] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init provision from the bucket", v.args("LEGION-42"))
		t.Logf("the provision printed %q", stdout)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
		}
		objects, size := v.cacheObjectLines(t)
		lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
		first := slices.Index(lines, "workspace-init: source=bucket")
		if first < 0 || len(lines) < first+len(objects)+2 {
			t.Fatalf("stdout %q, want the bucket as the source, a line for each of its %d objects, and the summary", stdout, len(objects))
		}
		if got := slices.Sorted(slices.Values(lines[first+1 : first+1+len(objects)])); !slices.Equal(got, objects) {
			t.Errorf("the provision printed the objects %q, want %q", got, objects)
		}
		summary := lines[first+1+len(objects)]
		match := bucketSummaryLine.FindStringSubmatch(summary)
		if match == nil || !strings.Contains(summary, cache.base+" (bucket)") {
			t.Fatalf("the line after the objects %q is not the bucket's bytes and times and the delta's", summary)
		}
		if printed := printedBytes(t, match[1]); printed != size {
			t.Errorf("the line counts %d bytes from the bucket, want its objects' %d", printed, size)
		}
		if strings.Contains(stdout, "source=/tar") {
			t.Errorf("stdout %q names /tar as a source too", stdout)
		}
		if got := cache.double.Count(fakegcs.RouteDownload); got.Requests != len(objects) || got.WithAuthorization != 0 {
			t.Errorf("the double served %d downloads, %d with an Authorization header, want %d with none", got.Requests, got.WithAuthorization, len(objects))
		}
		if main := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "rev-parse", "refs/remotes/origin/main"); main != head {
			t.Errorf("the clone's origin/main is %s, want the remote's %s, which GitHub's delta brought past the cache's", main, head)
		}
		requirePrintsNoSecret(t, cache, stdout, stderr)

		code, stdout, stderr = v.answered(t, "workspace-init provision onto the kept clone", v.args("LEGION-42"))
		if code != 0 || !regexp.MustCompile(`(?m)^workspace-init: the clone holds acme/widgets already: ([0-9,]+) bytes from GitHub for the delta \([0-9]+ refs?\)$`).MatchString(stdout) || strings.Contains(stdout, "source=") {
			t.Fatalf("relaunch: exit %d, stdout %q, stderr %q; want the kept clone's delta alone and no source", code, stdout, stderr)
		}
	})

	t.Run("a signed download the bucket refuses falls back to /tar", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, _ := v.startPacksCache(t, true)
		cache.skew.Store(int64(workspace.BucketFetchTimeout + time.Minute))
		v.env[cacheURLEnv] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init provision past the addresses' expiry", v.args("LEGION-42"))
		t.Logf("the provision printed %q", stdout)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
		}
		reason := regexp.QuoteMeta("workspace-init: source=/tar reason=failed download: acme/widgets/objects/pack/") +
			`pack-[0-9a-f]{40}\.(?:pack|idx)` + regexp.QuoteMeta(": the bucket answered 403 Forbidden (ExpiredToken)\n")
		if !regexp.MustCompile(`(?m)^` + reason + tarSummaryLine(cache.base)).MatchString(stdout) {
			t.Fatalf("stdout %q, want /tar as the source for the refused download, then /tar's line", stdout)
		}
		if got := cache.double.Count(fakegcs.RouteDownload); got.Requests == 0 || got.WithAuthorization != 0 {
			t.Errorf("the double served %d downloads, %d with an Authorization header, want some with none", got.Requests, got.WithAuthorization)
		}
		requirePrintsNoSecret(t, cache, stdout, stderr)
	})

	t.Run("a cache with no bucket serves no /packs", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		cache, _ := v.startPacksCache(t, false)
		v.env[cacheURLEnv] = cache.base
		code, stdout, stderr := v.answered(t, "workspace-init provision from a cache with no bucket", v.args("LEGION-42"))
		t.Logf("the provision printed %q", stdout)
		if code != 0 || !regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(notServedLine)+tarSummaryLine(cache.base)).MatchString(stdout) {
			t.Fatalf("exit %d, stdout %q, stderr %q; want /tar as the source, /packs not served, then /tar's line", code, stdout, stderr)
		}
		if requests := cache.double.Requests(); len(requests) != 0 {
			t.Errorf("a cache and a provision with no bucket sent the double %d requests", len(requests))
		}
		requirePrintsNoSecret(t, cache, stdout, stderr)
	})

	t.Run("a provision with no cache named fetches from GitHub", func(t *testing.T) {
		v := newIssueVolume(t).withRemote(t, "main")
		v.provisionEnv()
		code, stdout, stderr := v.answered(t, "workspace-init provision with no cache", v.args("LEGION-42"))
		if code != 0 || !regexp.MustCompile(`(?m)^workspace-init: no repository cache named: fetched the default branch of acme/widgets from GitHub \(([0-9,]+) bytes\)$`).MatchString(stdout) || strings.Contains(stdout, "source=") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want the fetch from GitHub's line and no source", code, stdout, stderr)
		}
	})
}
