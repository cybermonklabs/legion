package repocache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestASnapshotThatCannotReachAnEntryNamesNoPathOfTheClone: the walk reads the clone's directories
// and then stats each entry it takes, and the snapshot opens each pack of objects/pack/. A
// directory the walk can list but not search fails that stat, and a pack whose mode refuses the
// read fails that open; each failure is an *fs.PathError whose path is the clone's own.
//
// Under refs/ that path holds a branch name whatever answered the cache's fetch in GitHub's place
// wrote, which can be the provisioning token the fetch carried (ref): the log line of the /tar the
// snapshot failed names the operation and the system's reason alone (unpathed) and holds no run of
// the token. The twin shows the stat the walk makes, made here, quotes the path with the token in
// it. A pack's name is git's own (pack), so nothing of the answering party's is in it; the line
// still names no path, which is what keeps the two file calls of one walk reading alike.
// Captures repocache.entry-info/<case>.
func TestASnapshotThatCannotReachAnEntryNamesNoPathOfTheClone(t *testing.T) {
	const branch = "echo-" + echoedProvisioningToken
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which searches a directory with no x and reads a mode-000 file: a refused call cannot be staged for it")
	}
	refuse := func(t *testing.T, path string, mode os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) })
	}

	t.Run("ref", func(t *testing.T) {
		c, log := cacheWithBranch(t, branch+"/below")
		// The ref's directory keeps its read, so the walk lists it and reaches the entry below it,
		// and loses its search, so the stat of that entry fails.
		refuse(t, filepath.Join(c.dir, "refs", "heads", branch), 0o444)

		if _, err := os.Lstat(filepath.Join(c.dir, "refs", "heads", branch, "below")); err == nil || !strings.Contains(err.Error(), echoedProvisioningToken) {
			t.Fatalf("the stat of the entry under the ref = %v, which holds no echo of the token, so the row cannot see a leak", err)
		}

		w := httptest.NewRecorder()
		if serveTarAborted(c, w) {
			t.Fatal("the stream began, where a snapshot that cannot stat an entry sends nothing")
		}
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("GET /tar answered %d, want 500 for a clone the snapshot cannot walk", w.Code)
		}
		requireLoggedWithoutToken(t, "repocache.entry-info/ref", log, "take the clone's tar for ")
	})

	t.Run("pack", func(t *testing.T) {
		c, log := cacheWithBranch(t, branch)
		// The clone packs what it holds, so the snapshot opens a pack rather than reading loose
		// objects, which no feed takes.
		git(t, c.dir, "--git-dir="+c.dir, "repack", "-a", "-d", "-q")
		packs, err := filepath.Glob(filepath.Join(c.dir, "objects", "pack", "pack-*.pack"))
		if err != nil || len(packs) == 0 {
			t.Fatalf("the clone holds no pack after a repack (%v), so the snapshot opens none", err)
		}
		refuse(t, packs[0], 0)

		w := httptest.NewRecorder()
		if serveTarAborted(c, w) {
			t.Fatal("the stream began, where a snapshot that cannot open a pack sends nothing")
		}
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("GET /tar answered %d, want 500 for a pack the snapshot cannot open", w.Code)
		}
		requireLoggedWithoutToken(t, "repocache.entry-info/pack", log, "take the clone's tar for ")
		lines := strings.Join(log.all(), "\n")
		if strings.Contains(lines, filepath.Base(packs[0])) || strings.Contains(lines, c.dir) {
			t.Errorf("the cache logged %q, want the failed open named by its operation and the system's reason alone", lines)
		}
	})
}
