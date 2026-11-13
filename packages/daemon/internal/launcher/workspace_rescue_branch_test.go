package launcher

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// No restore sets the issue's rescue branch `legion/<KEY>-rescue`, end to end against real git and
// jj. The rescue branch only ever moves forward on GitHub, and a copy of a workspace can hold a
// local bookmark of it on a commit no remote branch holds yet, which the shim's bundler keeps with
// every other such local bookmark. The pod then pushes a rescue past that commit, and the pod that
// replaces it fetches the rescue branch where GitHub holds it now. Were the restore to set the
// copy's own value — the launcher sets every bookmark it is sent with `jj bookmark set
// --allow-backwards` — the replacement pod's rescue bookmark would move back behind what GitHub
// holds. The bookmark must stay where the replacement pod's fetch put it.
func TestARestoreLeavesTheRescueBranchWhereThePodFetchedItAfterARescuePush(t *testing.T) {
	testbin.RequireJJFilters(t)
	rescue := "legion/" + bundleIssue + "-rescue"
	f := newBundleFixture(t)

	// Work no remote holds, with the rescue bookmark set on it locally.
	f.write(filepath.Join(f.ws, "r1.txt"), "r1\n")
	f.run(f.ws, "jj", "describe", "-R", f.ws, "-m", "r1")
	f.run(f.ws, "jj", "new", "-R", f.ws)
	f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, rescue, "-r", "@-")
	r1 := f.run(f.ws, "jj", "log", "-R", f.ws, "-r", "@-", "--no-graph", "-T", "commit_id")

	// The cadence copies the workspace, and the copy holds the local rescue bookmark on r1.
	copied := f.bundleWorkspaceOne()
	if copied.Bookmarks[rescue] != r1 {
		t.Fatalf("the copy holds %s on %q, want it on the unpushed r1 %s; its bookmarks are %v",
			rescue, copied.Bookmarks[rescue], r1, copied.Bookmarks)
	}

	// The pod goes on and pushes a rescue past r1, so GitHub's rescue branch is ahead of the copy.
	f.write(filepath.Join(f.ws, "r2.txt"), "r2\n")
	f.run(f.ws, "jj", "describe", "-R", f.ws, "-m", "r2")
	f.run(f.ws, "jj", "new", "-R", f.ws)
	f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, rescue, "-r", "@-")
	f.run(f.ws, "jj", "git", "push", "-R", f.ws, "--bookmark", rescue)
	r2 := strings.TrimSpace(f.run(f.remote, "git", "--git-dir="+f.remote, "rev-parse", "refs/heads/"+rescue))
	if r2 == r1 {
		t.Fatalf("the rescue push left GitHub's %s on r1 %s, want it on r2", rescue, r1)
	}

	// The pod is replaced. Its fetch brings the rescue branch where GitHub holds it, and it tracks
	// it, so its own rescue bookmark sits on r2.
	workspace, created := f.replacementPod("ws2")
	f.run(workspace, "jj", "bookmark", "track", "-R", workspace, rescue+"@origin")
	if got := rescueTarget(f, workspace, rescue); got != r2 {
		t.Fatalf("the replacement pod's %s is on %q after its fetch, want it on r2 %s", rescue, got, r2)
	}

	// The daemon sends the copy, and the launcher restores it as the sandbox sends it.
	bundleTake, runningTake, offered := runtime.DecideWorkspaceRestore(runtime.WorkspaceCopyHeld{}, copyHeld(copied), runtime.WorkspacePush{}, replacementPodUID, created)
	if bundleTake != runtime.WorkspaceBundleNone || runningTake != runtime.WorkspaceBundleSend || offered != runtime.WorkspaceCopyRunning {
		t.Fatalf("the copy was decided (%v, %v, %q), want it offered and sent", bundleTake, runningTake, offered)
	}
	f.restoreAsSent(workspace, copied)
	if !f.holdsCommit(workspace, r1) {
		t.Fatalf("the replacement pod's workspace holds no r1 %s, the work of the copy it was sent", r1)
	}

	if got := rescueTarget(f, workspace, rescue); got != r2 {
		t.Fatalf("after the restore the replacement pod's %s is on %q, want it still on r2 %s where its fetch put it, not on the copy's r1 %s: the restore set the rescue bookmark",
			rescue, got, r2, r1)
	}
}

// rescueTarget is the commit workspace's local bookmark branch is on, and "" when it has none.
func rescueTarget(f *bundleFixture, workspace, branch string) string {
	f.t.Helper()
	return strings.TrimSpace(f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy",
		"-r", `bookmarks(exact:"`+branch+`")`, "--no-graph", "-T", `commit_id`))
}
