package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/repocache/fakegcs"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cmdCacheToken is the bearer token the cache command's tests hand the cache, and the fetch's
// present.
const cmdCacheToken = "cache-token-of-acme-widgets"

// writeCacheToken is a file, outside the issue volume and TMPDIR, holding body.
func writeCacheToken(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "cache-token")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// cacheArgs are a cache pod's container's: the volume's root, and an address of the test's own.
func (v *issueVolume) cacheArgs() []string {
	return []string{"cache", "--repo", winitRepo, "--root", v.root, "--listen", "127.0.0.1:0"}
}

// Every refusal `cache` makes happens before it runs git, listens or writes anything: its root is
// as it was, and no one-shot credential is left in its TMPDIR.
func TestWorkspaceInitCacheRefusesBeforeServing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "no --repo",
			args: func(v *issueVolume) []string { return []string{"cache", "--root", v.root} },
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string { return []string{"cache", "--repo", "../x", "--root", v.root} },
			code: 1,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			// --repo is read first: the root and the token are wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string { return []string{"cache", "--repo", "acme", "--root", "cache"} },
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a relative --root",
			args: func(*issueVolume) []string { return []string{"cache", "--repo", winitRepo, "--root", "cache"} },
			code: 1,
			says: func(*issueVolume) string { return `--root must be an absolute path (got "cache")` },
		},
		{
			name: "a --refresh that is not positive",
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "--refresh", "0s") },
			code: 1,
			says: func(*issueVolume) string { return "--refresh must be positive (got 0s)" },
		},
		{
			name: "LEGION_REPOSITORY_CACHE_TOKEN_FILE unset",
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_REPOSITORY_CACHE_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE is not set" },
		},
		{
			name: "a bearer token file that is blank",
			env: func(t *testing.T, v *issueVolume) {
				v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, " \n")
			},
			code: 1,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE names " },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, v *issueVolume) { delete(v.env, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env:  func(t *testing.T, v *issueVolume) { v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token + ".absent" },
			code: 1,
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
			code: 1,
			says: func(v *issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ", which is empty"
			},
		},
		{
			name: "no git on PATH",
			env:  func(t *testing.T, v *issueVolume) { v.env["PATH"] = t.TempDir() },
			code: 1,
			says: func(*issueVolume) string { return "git is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "--feed", v.feed) },
			code: 2,
			says: func(*issueVolume) string { return "-feed" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
			v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken)
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.cacheArgs()
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
				t.Fatalf("the root holds %v (%v), want it as it was", entries, err)
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

// `cache` exits 1 for every failure, a failure from GitHub too: the fetch's exit 3 for a cause
// outside an issue's volume (workspace.InitExitOutsideVolume) is an issue pod's alone. Here GitHub
// answers the cache's first clone with a 403, and the cache, with no clone to serve, ends.
func TestWorkspaceInitCacheExitsOneWhenGitHubRefusesItsFirstClone(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failClones(t, "remote: Write access to repository not granted.\nfatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 403\n")
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr lockedBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- v.runContext(ctx, v.cacheArgs(), &stdout, &stderr)
	}()

	select {
	case code := <-exited:
		if says := "The requested URL returned error: 403"; code != 1 || !strings.Contains(stderr.String(), says) {
			t.Fatalf("cache exited %d, stderr %q; want 1 naming %q", code, stderr.String(), says)
		}
	case <-time.After(testwait.Bounded(t, 1<<60, 30*time.Second)):
		t.Fatalf("cache did not end after GitHub refused its first clone, before the test binary's deadline: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if got := cloneAttempts(t, attempts); got != 1 {
		t.Fatalf("git cloned %d times, want 1", got)
	}
}

// `cache` serves the repository's cache until its context ends: it says where it listens, clones
// the repository onto its root with the provisioning token handed to git as a one-shot credential
// under its TMPDIR, answers /healthz ready at the remote's head to a request carrying no token,
// holds no token on the volume, and
// exits 0 once stopped.
func TestWorkspaceInitCacheServesTheRepositoryUntilStopped(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	v.env["LEGION_PROVISION_TOKEN_FILE"] = v.token
	v.env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] = writeCacheToken(t, cmdCacheToken+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr lockedBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- v.runContext(ctx, v.cacheArgs(), &stdout, &stderr)
	}()

	address := ""
	if !testwait.Holds(testwait.Bounded(t, 1<<60, 30*time.Second), 10*time.Millisecond, func() bool {
		for _, line := range strings.Split(stdout.String(), "\n") {
			if found, ok := strings.CutPrefix(line, "workspace-init cache: serving acme/widgets on "); ok {
				address = found
			}
		}
		return address != ""
	}) {
		t.Fatalf("cache never said where it serves: stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	var health repocache.Health
	// Each read of /healthz, and the wait for one that says ready, ends when what is left of the
	// test binary's deadline less 30 s has passed: a /healthz that takes the connection and never
	// answers fails the row by name, where a request with no bound hung it to go test's alarm.
	wait := testwait.Bounded(t, 1<<60, 30*time.Second)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), wait)
	defer readyCancel()
	if !testwait.Holds(wait, 20*time.Millisecond, func() bool {
		request, err := http.NewRequestWithContext(readyCtx, http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /healthz: %v (each read's bound ends %s before the test binary's deadline); stdout %q, stderr %q", err, 30*time.Second, stdout.String(), stderr.String())
		}
		err = json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return health.Ready
	}) {
		t.Fatalf("the cache never became ready: %+v; stdout %q, stderr %q", health, stdout.String(), stderr.String())
	}
	if want := v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "main"); health.Head != want || health.Repo != winitRepo {
		t.Fatalf("/healthz = %+v, want acme/widgets at the remote's main %s", health, want)
	}
	if main := v.git(t, "--git-dir="+filepath.Join(v.root, "acme", "widgets.git"), "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the cache's main is %q, want the remote's", main)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatalf("the clone recorded no credential: %v", err)
	}
	tokenFile, held, _ := strings.Cut(strings.TrimSpace(string(credential)), " ")
	if held != "ghs_test" || !strings.HasPrefix(tokenFile, v.tmp+string(filepath.Separator)) {
		t.Fatalf("the clone was handed %s holding %q, want the provisioning token under TMPDIR %s", tokenFile, held, v.tmp)
	}
	holdsNoToken(t, v.root, "while the cache serves")

	cancel()
	if code := testwait.Receive(t, "the cache to stop with its context", exited); code != 0 {
		t.Fatalf("cache exited %d once stopped, stderr %q", code, stderr.String())
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) once the cache stopped, want its credential gone", entries, err)
	}
}

// cmdTestBucket is the bucket the cache command's bucket test points the cache at: the
// placeholder bucket the double's fixtures name.
const cmdTestBucket = "example-legion-packs"

// cacheGet is GET url, presenting bearer where it is not empty, within ctx: the status, the
// headers and the body read whole.
func cacheGet(t *testing.T, ctx context.Context, url, bearer string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", request.URL.Path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", request.URL.Path, err)
	}
	return response.StatusCode, response.Header, body
}

// `cache` runs the real command path with a Cloud Storage double on a loopback address that
// STORAGE_EMULATOR_HOST names, as the real-binary harness runs it. With
// LEGION_REPOSITORY_CACHE_BUCKET set it uploads each pack and index of its clone to that bucket and
// answers /packs with its token: the bucket, the clone's refs, a planted branch's among them, and
// every pack with its index, each the size and SHA-256 of the clone's own file, and each address
// one the double serves that file's exact bytes for, with no credential. With the variable unset it
// sends the double nothing and /packs is the mux's 404, naming no repository. Either way no line it
// prints holds a signed address, a V4 parameter, its bearer or the planted ref's contents.
func TestWorkspaceInitCacheUsesTheBucketWhenSet(t *testing.T) {
	// Not parallel: it sets STORAGE_EMULATOR_HOST for the whole process (startPacksCache), which the cache's bucket reads there as Google's clients do.
	for _, tc := range []struct {
		name   string
		bucket bool
	}{
		{"with the bucket set it uploads the clone's packs and serves /packs", true},
		{"with the bucket unset it sends the double nothing and /packs is 404", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			cache, planted := v.startPacksCache(t, tc.bucket)
			ctx := testwait.Context(t)
			var addresses []string
			if tc.bucket {
				status, _, body := cacheGet(t, ctx, cache.base+"/packs", cmdCacheToken)
				if status != http.StatusOK {
					t.Fatalf("/packs answered %d, not 200: %s; stdout %q, stderr %q", status, body, cache.stdout.String(), cache.stderr.String())
				}
				objects := requireBucketManifest(t, v, body, planted)
				if got := cache.double.Count(fakegcs.RouteUpload).Requests; got < len(objects) {
					t.Errorf("the double received %d uploads for the %d objects /packs lists", got, len(objects))
				}
				for _, object := range objects {
					addresses = append(addresses, object.URL)
					status, _, got := cacheGet(t, ctx, object.URL, "")
					sum := sha256.Sum256(got)
					if status != http.StatusOK || int64(len(got)) != object.Size || hex.EncodeToString(sum[:]) != object.SHA256 {
						t.Errorf("the signed download of %s answered %d and %d bytes, not 200 and the %d bytes of SHA-256 %s", object.Name, status, len(got), object.Size, object.SHA256)
					}
				}
			} else {
				status, header, body := cacheGet(t, ctx, cache.base+"/packs", cmdCacheToken)
				if status != http.StatusNotFound || header.Get(workspace.CacheRepositoryHeader) != "" {
					t.Errorf("/packs with no bucket answered %d naming %q: %s; want the mux's 404 naming nothing", status, header.Get(workspace.CacheRepositoryHeader), body)
				}
				if requests := cache.double.Requests(); len(requests) != 0 {
					t.Errorf("a cache with no bucket sent the double %d requests: %v", len(requests), requests)
				}
			}

			if code := cache.stop(); code != 0 {
				t.Fatalf("cache exited %d once stopped, stderr %q", code, cache.stderr.String())
			}
			stdout, stderr := cache.stdout.String(), cache.stderr.String()
			for _, secret := range append([]string{"X-Goog-", cmdCacheToken, planted}, addresses...) {
				if strings.Contains(stdout+stderr, secret) {
					t.Errorf("the cache printed %q: stdout %q, stderr %q", secret, stdout, stderr)
				}
			}
			if tc.bucket && !strings.Contains(stdout, "workspace-init cache: listed ") {
				t.Errorf("the cache did not say it listed its packs: stdout %q", stdout)
			}
		})
	}
}

// requireBucketManifest fails the test unless body is a /packs manifest of the volume's cache
// clone: the test bucket; refs that hold the planted branch at its commit; and every pack and
// index of the clone, each pack with its index, each under the repository's lowercase prefix with
// the size and SHA-256 of the clone's own file. It answers the manifest's objects.
func requireBucketManifest(t *testing.T, v *issueVolume, body []byte, planted string) []workspace.PackObject {
	t.Helper()
	var manifest workspace.PackManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("/packs answered no manifest: %v", err)
	}
	if manifest.Bucket != cmdTestBucket {
		t.Errorf("the manifest names bucket %q, not %s", manifest.Bucket, cmdTestBucket)
	}
	plantedListed := false
	for _, ref := range manifest.Refs {
		plantedListed = plantedListed || bytes.Contains(ref.Bytes, []byte(planted+" refs/heads/planted")) || (ref.Path == "refs/heads/planted" && string(ref.Bytes) == planted+"\n")
	}
	if !plantedListed {
		t.Errorf("the manifest's refs hold no refs/heads/planted at %s", planted)
	}
	want := v.cachePacks(t)
	got := map[string]string{}
	for _, object := range manifest.Objects {
		if local, held := want[object.Name]; !held || local.size != object.Size {
			t.Errorf("the manifest lists %s of %d bytes, which the clone holds as %+v (held: %t)", object.Name, object.Size, local, held)
		}
		got[object.Name] = object.SHA256
	}
	if len(want) < 2 || len(got) != len(want) {
		t.Errorf("the manifest lists %v, not the clone's packs and indexes %v", got, want)
	}
	for name, local := range want {
		if got[name] != local.sha256 {
			t.Errorf("the manifest lists %s with SHA-256 %q, not the clone's file's %s", name, got[name], local.sha256)
		}
		if stem, pack := strings.CutSuffix(name, ".pack"); pack {
			if _, indexed := got[stem+".idx"]; !indexed {
				t.Errorf("the manifest lists %s without its index", name)
			}
		}
	}
	return manifest.Objects
}
