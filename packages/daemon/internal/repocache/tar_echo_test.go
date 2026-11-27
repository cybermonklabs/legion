package repocache

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// echoedProvisioningToken is the token testOptions' token file holds (testProvisioningToken),
// which the cache's fetch from GitHub carries: the credential whatever answers that fetch in
// GitHub's place holds, and can name a branch with.
const echoedProvisioningToken = testProvisioningToken

// TestTheTarStreamNamesNoRefOfTheClone: whatever answers the cache's fetch in GitHub's place names
// a branch with the provisioning token the fetch carried, and the fetch writes it into the clone as
// a ref file the tar streams. The cache's log line of a stream that failed on that ref (stream),
// and of a snapshot that could not read the ref's file (ref-file) or list the ref's directory
// (ref-dir), names no run of the token: an entry by its place in the stream and its kind, and a
// failed file call by its operation and the system's reason. The twin shows the clone's own path
// of the ref, which the stream names its entry by and a file call's error quotes, holds the token.
// Captures repocache.tar-echo/<case>.
func TestTheTarStreamNamesNoRefOfTheClone(t *testing.T) {
	const branch = "echo-" + echoedProvisioningToken
	t.Run("twin", func(t *testing.T) {
		c, _ := cacheWithBranch(t, branch)
		entries, err := c.snapshot()
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer closeEntries(entries)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.header.Name)
		}
		if !strings.Contains(strings.Join(names, "\n"), echoedProvisioningToken) {
			t.Fatalf("the stream's entries are %q, none of which holds the token, so the rows cannot see a leak", names)
		}
		_, err = os.ReadFile(filepath.Join(c.dir, "refs", "heads", branch, "missing"))
		if err == nil || !strings.Contains(err.Error(), echoedProvisioningToken) {
			t.Fatalf("a file call under the ref = %v, which holds no echo of the token, so the rows cannot see a leak", err)
		}
	})
	t.Run("stream", func(t *testing.T) {
		c, log := cacheWithBranch(t, branch)
		w := &failAfter{header: http.Header{}, name: "refs/heads/" + branch}
		if !serveTarAborted(c, w) {
			t.Fatal("the stream whose writer failed on the ref's body ended as if it had been sent")
		}
		requireLoggedWithoutToken(t, "repocache.tar-echo/stream", log, "tar stream to ")
	})
	for _, c := range []struct {
		name   string
		branch string
		refuse func(dir string) string
	}{
		{"ref-file", branch, func(dir string) string { return filepath.Join(dir, "refs", "heads", branch) }},
		{"ref-dir", branch + "/below", func(dir string) string { return filepath.Join(dir, "refs", "heads", branch) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("the test process is root, which reads a mode-000 file or directory: a refused read cannot be staged for it")
			}
			cache, log := cacheWithBranch(t, c.branch)
			refused := c.refuse(cache.dir)
			info, err := os.Stat(refused)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(refused, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(refused, info.Mode().Perm()) })
			w := httptest.NewRecorder()
			if serveTarAborted(cache, w) {
				t.Fatal("the stream began, where a snapshot that cannot read the clone sends nothing")
			}
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("GET /tar answered %d, want 500 for a clone the snapshot cannot read", w.Code)
			}
			requireLoggedWithoutToken(t, "repocache.tar-echo/"+c.name, log, "take the clone's tar for ")
		})
	}
}

// cacheWithBranch is a cache of a remote whose HEAD names branch: the cache keeps its default
// branch alone (narrow), so the one branch the answering party of its fetch names into the clone
// is the one that party's HEAD names (workspace.Fetch). The cache has filled its clone and then
// fetched branch moved on, so the clone holds branch as a loose ref file of its own, as a fetch
// writes a ref it moves; and the log the cache writes into. Its volume is markedVolume, so every
// path of the clone, a ref's or a pack's, holds it.
func cacheWithBranch(t *testing.T, branch string) (*cache, *syncLog) {
	t.Helper()
	remote := newRemote(t)
	git(t, remote, "--git-dir="+remote, "branch", branch, "main")
	git(t, remote, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	opts := testOptions(t, newTestRunner(t, remote))
	opts.Root = filepath.Join(opts.Root, markedVolume)
	if err := os.Mkdir(opts.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	log := &syncLog{}
	opts.Log = log.log
	c := cacheOn(t, opts)
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("the first refresh: %v", err)
	}
	advance(t, remote, "moved")
	git(t, remote, "--git-dir="+remote, "branch", "--force", branch, "main")
	if err := c.refresh(context.Background()); err != nil {
		t.Fatalf("the refresh that moves %s: %v", branch, err)
	}
	if info, err := os.Stat(filepath.Join(c.dir, "refs", "heads", filepath.FromSlash(branch))); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the clone holds no loose ref file of %s (%v), so no entry of the stream is named with it", branch, err)
	}
	return c, log
}

// serveTarAborted runs c's /tar for w, as a reader asked it with a request still live, and answers
// whether the handler aborted the stream it had begun (http.ErrAbortHandler).
func serveTarAborted(c *cache, w http.ResponseWriter) (aborted bool) {
	defer func() {
		if r := recover(); r != nil {
			if !errors.Is(asError(r), http.ErrAbortHandler) {
				panic(r)
			}
			aborted = true
		}
	}()
	c.serveTar(w, httptest.NewRequest(http.MethodGet, "/tar", nil))
	return false
}

// asError is a recovered value as an error, nil for one that is none.
func asError(r any) error {
	err, _ := r.(error)
	return err
}

// markedVolume is the name of the directory a cacheWithBranch cache's volume is. It holds the
// gate's marker -RDX-, so a line that names a path of the clone, which a pack's name alone does not
// make the token's, still fails a row at its capture with a marker the gate reads there.
const markedVolume = "volume-RDX-"

// requireLoggedWithoutToken fails t unless log holds a line that begins with prefix, and reads
// every line through oauth2calltest.RequireNoPiece under capture: no run of the token, and none of
// markedVolume, which every path of the clone holds.
func requireLoggedWithoutToken(t *testing.T, capture string, log *syncLog, prefix string) {
	t.Helper()
	lines := strings.Join(log.all(), "\n")
	if !logged(log.all(), prefix) {
		t.Fatalf("the cache logged %q, want a line beginning %q", lines, prefix)
	}
	oauth2calltest.RequireNoPiece(t, capture, lines, echoedProvisioningToken, markedVolume)
}

// failAfter is a reader's side of a stream that goes away right after the write of the header the
// tar writes for the entry named name, so that entry's own body fails to write. The tar writes a
// header as one block that begins with the entry's name, ended by a NUL where the name is shorter
// than its field; HEAD's body, which names the same ref as HEAD's branch, is no such write.
type failAfter struct {
	header http.Header
	name   string
	armed  bool
}

func (w *failAfter) Header() http.Header { return w.header }

func (w *failAfter) WriteHeader(int) {}

func (w *failAfter) Write(p []byte) (int, error) {
	if w.armed {
		return 0, errors.New("the reader went away")
	}
	w.armed = bytes.HasPrefix(p, append([]byte(w.name), 0))
	return len(p), nil
}
