package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shim/shimtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// shimCopyBundler is the runtime's bundler of an issue workspace with the real shim behind it:
// each ask runs the shim's bundler against a real jj workspace (shimtest.Workspace.Bundle) and
// answers the shim's bundle as the runtime does. The worker stream leaves out the shim's own time
// of its reading (stream.Conn.BundleWorkspace), and the Kubernetes runtime names the pod the
// claim's locator records (sandbox's Runtime.BundleWorkspace), which is the rig's.
type shimCopyBundler struct {
	workspace *shimtest.Workspace
	mu        sync.Mutex
	asked     int
}

func (b *shimCopyBundler) BundleWorkspace(ctx context.Context, issue string, _ claim.Role) (runtime.WorkspaceBundle, error) {
	b.mu.Lock()
	b.asked++
	b.mu.Unlock()
	if issue != b.workspace.Issue {
		return runtime.WorkspaceBundle{}, fmt.Errorf("the cadence asked for the workspace of %s, and this one is %s's", issue, b.workspace.Issue)
	}
	saved, err := b.workspace.Bundle(ctx, runtime.MaxWorkspaceBundleBytes)
	if err != nil {
		return runtime.WorkspaceBundle{}, err
	}
	return runtime.WorkspaceBundle{Bundle: saved.Bundle, Head: saved.Head, Tips: saved.Tips, Bookmarks: saved.Bookmarks,
		PodUID: releasedPodUID}, nil
}

// asks is how many times the cadence has asked the shim for a bundle.
func (b *shimCopyBundler) asks() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.asked
}

// storedCopy is the issue's running copy as the store holds it, its bytes included.
type storedCopy struct {
	bundle []byte
	head   string
	at     time.Time
}

// stored is the issue's running copy as the store holds it now.
func (r *copyRig) stored(t *testing.T) storedCopy {
	t.Helper()
	var held storedCopy
	if err := r.st.Pool().QueryRow(context.Background(), `select bundle, head, at from workspace_running_copies where issue = $1`, r.issue.Key).
		Scan(&held.bundle, &held.head, &held.at); err != nil {
		t.Fatalf("read the running copy of %s: %v", r.issue.Key, err)
	}
	return held
}

// awaitStored waits for the cadence to have logged count copies of the issue stored, and answers
// the copy the store holds then. A real bundle's size is not known ahead, which is what awaitCopy
// waits on, so this reads the lines instead, and gives up at the first copy that came to nothing.
func (r *copyRig) awaitStored(t *testing.T, count int) storedCopy {
	t.Helper()
	var copied, unsaved int
	if !testwait.Holds(testwait.Bounded(t, 2*time.Minute, time.Second), testwait.Poll, func() bool {
		copied, unsaved = len(r.lines(t, runningCopyStoredLine)), len(r.lines(t, runningCopyUnsavedLine))
		return copied >= count || unsaved > 0
	}) || copied < count {
		t.Fatalf("the cadence stored %d copies of the workspace, want %d; the daemon logged:\n%s", copied, count, r.logs.String())
	}
	return r.stored(t)
}

// bundledCommits is every commit bundle carries that the remote does not hold, read as the pod
// that replaces this one reads a copy: a bare clone of the remote as it stands, which holds the
// bundle's bases and nothing of the workspace, verifies the bundle (`git bundle verify`), fetches
// its heads, and lists every commit they reach that the remote's branches do not.
//
// Its git runs no automatic maintenance. A fetch starts `git maintenance run --auto --detach`
// (git 2.46 and later), and that process can still be writing in the clone, which is under the
// test's TempDir, when the test ends and its cleanup removes the directory.
func bundledCommits(t *testing.T, w *shimtest.Workspace, bundle []byte) []string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "copy.bundle")
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "remote.git")
	git := func(args ...string) string {
		return w.Run(dir, slices.Concat([]string{"git", "-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args)...)
	}
	git("clone", "--quiet", "--bare", "--no-local", w.Remote, repo)
	git("--git-dir="+repo, "bundle", "verify", "--quiet", path)
	git("--git-dir="+repo, "fetch", "--quiet", path, "+refs/*:refs/bundled/*")
	heads := strings.Fields(git("--git-dir="+repo, "for-each-ref", "--format=%(objectname)", "refs/bundled/"))
	return strings.Fields(git(slices.Concat([]string{"--git-dir=" + repo, "rev-list"}, heads, []string{"--not", "--branches"})...))
}

// The cadence's copies hold what the shim's bundler builds of a real workspace, not bytes a fake
// was told to answer: the bundler here is the real shim run against a real jj workspace
// (shimtest.Workspace). A turn that ends stores a git bundle carrying the commit the turn made;
// between turns a due tick stores nothing, since nobody writes the workspace then; and five
// minutes into the next turn the cadence's tick stores a second copy, carrying the commit made
// since the first. A copy that is a valid bundle of an older reading would cost an eviction every
// commit since, so each copy is read back as the pod that replaces this one reads it, against a
// clone of the remote alone.
func TestTheCadenceStoresTheShimsBundleOfARealWorkspaceAtATurnsEndAndFiveMinutesIntoATurn(t *testing.T) {
	rig := newCopyRig(t)
	workspace := shimtest.New(t, rig.issue.Key)
	bundler := &shimCopyBundler{workspace: workspace}
	rig.w.copyBundler = bundler
	rig.copiesTurnEnds()
	rig.clock.advance(0)
	ctx := context.Background()

	// The turn commits its work and ends, and the copy its end takes holds that commit.
	rig.turnStart(t)
	turned := workspace.Work("the-turns-work")
	head := workspace.WorkingCopy()
	rig.turnEnd(t)
	ended := rig.awaitStored(t, 1)
	if ended.head != head {
		t.Fatalf("the turn's end stored a copy on %s, want the workspace's working copy %s", ended.head, head)
	}
	if carried := bundledCommits(t, workspace, ended.bundle); !slices.Contains(carried, turned) {
		t.Fatalf("the turn's end stored a bundle carrying %v, want the commit %s the turn made", carried, turned)
	}

	// Between turns, with the turn's end stored and so no retry owed, a due tick asks no shim and
	// leaves the stored copy as it was.
	rig.wind(t)
	rig.w.copyWorkingWorkspaces(ctx)
	if testwait.Holds(time.Second, testwait.Poll, func() bool { return bundler.asks() > 1 }) {
		t.Fatalf("the shim was asked for %d bundles, want the turn's end alone: a due tick between turns copied the workspace", bundler.asks())
	}
	if held := rig.stored(t); !held.at.Equal(ended.at) {
		t.Fatalf("the running copy was taken at %s after the tick between turns, want the turn end's copy of %s", held.at, ended.at)
	}

	// The next turn commits more work, and five minutes into it the cadence's tick stores a second
	// copy, which carries that work as well as the turn before's, still unpushed.
	rig.turnStart(t)
	later := workspace.Work("five-minutes-on")
	rig.wind(t)
	rig.w.copyWorkingWorkspaces(ctx)
	inTurn := rig.awaitStored(t, 2)
	carried := bundledCommits(t, workspace, inTurn.bundle)
	for _, want := range []string{later, turned} {
		if !slices.Contains(carried, want) {
			t.Fatalf("the copy five minutes into the turn carries %v, want the commits %s and %s no remote holds", carried, later, turned)
		}
	}
	if asked := bundler.asks(); asked != 2 {
		t.Fatalf("the shim was asked for %d bundles, want the turn's end and the one five minutes into the next turn", asked)
	}
}
