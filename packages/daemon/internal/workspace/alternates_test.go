package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// objectFiles is every object file a repository's object store holds, by its path under the store:
// each pack and its index, and each loose object, the files a clone of it would copy or hard-link
// under the same names.
func objectFiles(t *testing.T, objects string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(objects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(objects, path)
		if err != nil {
			return err
		}
		if top, _, _ := strings.Cut(filepath.ToSlash(relative), "/"); top != "info" {
			files = append(files, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A pod's clone of its feed reads the feed's objects in place, through objects/info/alternates on
// the feed's own path, which in a pod is the read-only /legion/feed: the clone is made with
// `git clone --shared` from the read-only feed, its alternates name the feed's objects and nothing
// else, and its own object store holds none of the feed's packs or loose objects, so the pod's disk
// holds the repository once. Every object of the clone's history is still reachable — git's
// connectivity check passes in the clone — and the issue's workspace reads its default branch
// through the same alternates. A plain local clone, as before, held a copy of each feed pack, or a
// hard link to it, under the same name; across the pod's two mounts of one disk it was a copy.
func TestAClonedWorkspaceReadsTheFeedThroughAlternates(t *testing.T) {
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "main", "history.txt")
	fetch := fetchRequest(t)
	fetched, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	runSetup(t, fetched.Feed, "git", "--git-dir="+fetched.Feed, "repack", "-a", "-d", "-q")
	feedObjects := filepath.Join(fetched.Feed, "objects")
	fed := objectFiles(t, feedObjects)
	if !slices.ContainsFunc(fed, func(name string) bool { return strings.HasSuffix(name, ".pack") }) {
		t.Fatalf("the feed holds %q and no pack: the test could not see a pack copied", fed)
	}
	readOnly(t, fetch.Feed)
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the read-only feed: %v", err)
	}
	cloneObjects := filepath.Join(working.Clone, ".git", "objects")
	alternates, err := os.ReadFile(filepath.Join(cloneObjects, "info", "alternates"))
	if err != nil {
		t.Fatalf("the clone's alternates: %v; want them to name the feed's objects", err)
	}
	if got := strings.Fields(string(alternates)); !slices.Equal(got, []string{feedObjects}) {
		t.Errorf("the clone's alternates name %q, want the feed's objects %s alone", got, feedObjects)
	}
	held := objectFiles(t, cloneObjects)
	for _, name := range fed {
		if slices.Contains(held, name) {
			t.Errorf("the clone holds the feed's %s itself, want it read through the alternates", name)
		}
	}
	runSetup(t, working.Clone, "git", "--git-dir="+filepath.Join(working.Clone, ".git"), "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
	main := strings.TrimSpace(runSetup(t, working.Clone, "git", "--git-dir="+run.remote, "rev-parse", "main"))
	if parent := commitOf(t, working.Dir, "@-"); parent != main {
		t.Errorf("the workspace's @- is %s, want the default branch's %s read through the feed", parent, main)
	}
	if _, err := os.Stat(filepath.Join(working.Dir, "history.txt")); err != nil {
		t.Errorf("the workspace's file from the feed's history: %v", err)
	}
}

// A feed Fetch finds present but foreign is never removed or replaced: a clone may read its
// objects through its alternates, and a feed replaced under it would leave that clone's history
// gone, which is what moved clones off alternates once. So the fetch fails naming why the feed is
// not one it wrote, every object file of the feed stays in place, and the clone that borrows from
// it still passes git's connectivity check. That the fetch runs no git, removes nothing and keeps
// each planted shape is TestFetchTakesNoFeedItDidNotWrite's, over every shape it refuses. Before,
// the fetch removed the feed and cloned a fresh one.
func TestAForeignFeedFailsTheFetchAndIsKept(t *testing.T) {
	run := newLocalRunner(t)
	lost := pushRemoteBranch(t, run.remote, "history", "history.txt")
	fetch := fetchRequest(t)
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	request := provisionRequest(t)
	request.Source = FromFeed(fetch.Feed)
	working, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision from the feed: %v", err)
	}
	runSetup(t, fetch.Feed, "git", "--git-dir="+run.remote, "update-ref", "-d", "refs/heads/history")
	if err := os.WriteFile(filepath.Join(first.Feed, "info", "grafts"), []byte(lost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(first.Feed, "objects")
	stood := objectFiles(t, objects)
	if len(stood) == 0 {
		t.Fatal("the feed holds no object file, so the test could not see one removed")
	}

	if _, err := Fetch(context.Background(), run, fetch); err == nil || !strings.Contains(err.Error(), "is not one this fetch wrote") {
		t.Fatalf("Fetch of a foreign feed = %v, want it to fail naming why the feed is foreign", err)
	}
	if left := objectFiles(t, objects); !slices.Equal(left, stood) {
		t.Errorf("the foreign feed holds %q after the fetch, want %q", left, stood)
	}
	if _, err := os.Stat(filepath.Join(working.Clone, ".git", "objects", "info", "alternates")); errors.Is(err, os.ErrNotExist) {
		t.Errorf("the clone has no alternates, want it borrowing the feed's objects")
	}
	runSetup(t, working.Clone, "git", "-C", working.Clone, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
}

// The repository cache's own fetch shares Fetch, and no clone borrows objects from the cache's
// clone: an issue pod takes a tar of it and keeps what it unpacks. So a clone of the cache's that
// Fetch did not write is removed, logged naming why, and filled afresh from GitHub, as it always
// was, where an issue pod's feed is kept and fails the fetch (TestAForeignFeedFailsTheFetchAndIsKept).
// The request is the cache's own (the repository cache's fetch sets ReplaceForeign). Under the
// never-replace rule applied to the cache's fetch as well as the pod's feed, this fetch fails and
// the planted file stays.
func TestTheCachesFetchStillReplacesAForeignClone(t *testing.T) {
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "main", "history.txt")
	fetch := fetchRequest(t)
	fetch.ReplaceForeign = true
	var logged []string
	fetch.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("the cache's first Fetch: %v", err)
	}
	grafts := filepath.Join(first.Feed, "info", "grafts")
	if err := os.WriteFile(grafts, []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	advanceRemote(t, run.remote)

	again, err := Fetch(context.Background(), run, fetch)
	if err != nil {
		t.Fatalf("the cache's Fetch of a clone it did not write = %v, want the clone removed and filled afresh", err)
	}
	if !again.Cloned {
		t.Error("the cache's Fetch kept a clone with grafts, want it cloned afresh")
	}
	if !slices.ContainsFunc(logged, func(line string) bool {
		return strings.Contains(line, "is not one this fetch wrote") && strings.Contains(line, "removed")
	}) {
		t.Errorf("the cache's Fetch logged %q, want it to say it removed a clone it did not write", logged)
	}
	if _, err := os.Stat(grafts); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the planted info/grafts after the cache's Fetch: %v, want it gone with the clone", err)
	}
	if why, err := foreignFeed(again.Feed); err != nil || why != "" {
		t.Errorf("the refilled clone reads as foreign: %q (%v)", why, err)
	}
	if got, want := strings.TrimSpace(runSetup(t, again.Feed, "git", "--git-dir="+again.Feed, "rev-parse", "refs/heads/main")),
		strings.TrimSpace(runSetup(t, again.Feed, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
		t.Errorf("the refilled clone's main is %s, want the remote's %s", got, want)
	}
}
