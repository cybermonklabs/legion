package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The daemon sends a copy again to a pod whose roles container restarted before any role of the
// pod registered (store's heldCopy): the restarted container's launcher is a new process, which
// holds no record of the restore the first one ran, over the same disk. A restore that ran to its
// end deletes its temporary branches, so the second restore of the same bundle runs as the first
// did and leaves the workspace holding the same work: the same commits, the same bookmarks and the
// working copy on the saved head.
func TestAnUnspentCopyRestoredAgainByARestartedContainerLeavesTheWorkAsTheFirstRestoreDid(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newBundleFixture(t)
	f.workOne()
	saved := f.bundleWorkspaceOne()
	workspace := f.secondPod("ws-restarted")

	if notice := f.restoreInto(workspace, saved, 1); !strings.Contains(notice, "is restored") {
		t.Fatalf("the first restore's notice is %q", notice)
	}
	f.requireRestored(workspace, saved)
	first := f.workspaceState(workspace)

	// The restarted container: a launcher process of its own, sent the same bundle.
	if notice := f.restoreInto(workspace, saved, 1); !strings.Contains(notice, "is restored") {
		t.Fatalf("the restarted container's restore of the same bundle answered %q, want it restored", notice)
	}
	f.requireRestored(workspace, saved)
	if again := f.workspaceState(workspace); again != first {
		t.Fatalf("the second restore left the workspace\n%s\nwant it as the first left it\n%s", again, first)
	}
}

// One live launcher sent the same slice sequence twice — a second role start of the pod before any
// role registered, which the daemon sends the unspent copy again — answers the second from what it
// holds and applies the bundle once: the work done in the workspace after the first restore stays,
// and the daemon is sent no second report.
func TestAnUnspentCopySentTwiceToOneLiveLauncherIsAppliedOnce(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newBundleFixture(t)
	f.workOne()
	saved := f.bundleWorkspaceOne()
	workspace := f.secondPod("ws-live")
	frames, err := shimwire.SplitWorkspaceBundle("restore-"+workspace, bundleRelease, saved)
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t)
	sendAndStart := func(generation uint64) {
		t.Helper()
		for _, frame := range frames {
			got, ok := g.answer(frame).(shimwire.LauncherBundleResult)
			if !ok || !got.OK || got.Held != (frame.Index == len(frames)-1) {
				t.Fatalf("slice %d of the sequence sent for generation %d answered %#v", frame.Index, generation, got)
			}
		}
		marker := filepath.Join(t.TempDir(), "marker")
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, append(f.Env, "LEGION_WORKSPACE="+workspace)...)
		if result := g.start(command); !result.OK {
			t.Fatalf("the start of generation %d answered %#v", generation, result)
		}
		published(t, marker)
		g.stop(generation)
	}

	sendAndStart(1)
	if g.restored == nil || !g.restored.OK {
		t.Fatalf("the first restore reported %#v", g.restored)
	}
	f.requireRestored(workspace, saved)
	// Work in the workspace after the restore, which a second apply would `jj edit` back over.
	f.Run(workspace, "jj", "new", "-R", workspace, "-m", "work after the restore")
	f.Write(filepath.Join(workspace, "after.txt"), "after\n")
	// The read snapshots the working copy, so the file written above is in the commit it names.
	after := f.Run(workspace, "jj", "log", "-R", workspace, "-r", "@", "--no-graph", "-T", "commit_id")
	g.restored = nil

	sendAndStart(2)
	if g.restored != nil {
		t.Fatalf("the launcher reported %#v of the sequence sent again, want no second restore to report", g.restored)
	}
	if head := f.Run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id"); head != after {
		t.Fatalf("after the sequence sent again the working copy is on %s, want %s, the work after the first restore", head, after)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "after.txt")); err != nil || string(body) != "after\n" {
		t.Fatalf("the work after the first restore reads %q (%v) after the sequence sent again", body, err)
	}
}

// workspaceState is what a restore leaves in workspace, as one string a test compares: every
// commit jj shows with its description, every local bookmark with its commit, and the working
// copy's commit.
func (f *bundleFixture) workspaceState(workspace string) string {
	f.t.Helper()
	log := f.Run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all()", "--no-graph",
		"-T", `commit_id ++ " " ++ description.first_line() ++ "\n"`)
	bookmarks := f.Run(workspace, "jj", "bookmark", "list", "-R", workspace, "--ignore-working-copy",
		"-T", `if(remote, "", name ++ " " ++ normal_target.commit_id() ++ "\n")`)
	head := f.Run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id")
	return "commits:\n" + log + "bookmarks:\n" + bookmarks + "working copy: " + head
}
