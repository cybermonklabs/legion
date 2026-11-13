package rescue_test

// The push check against real git and jj repositories: a bare remote standing in for GitHub, and a
// pod's disk holding a jj clone of it with the issue's workspace (rescuetest). Each case puts the
// workspace in one state and reads what the check reports and what the remote then holds.

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	issue = "WIDGETS-42"
	// prefix is the per-pod prefix the daemon names beside the issue's one branch, which an older
	// shim numbers from 1 and which this check reads only for an older daemon's request.
	prefix = "legion/WIDGETS-42-rescue-9d3f0c7a"
	// target is the issue's one rescue branch, which every check of the issue pushes to.
	target = "legion/WIDGETS-42-rescue"
	// zeroSHA is the remote side of an update line for a branch the push creates.
	zeroSHA = "0000000000000000000000000000000000000000"
)

// only is the check's one entry, which every case's disk holds one workspace for.
func only(t *testing.T, d *rescuetest.Disk, got []shimwire.RescueWorkspace) shimwire.RescueWorkspace {
	t.Helper()
	if len(got) != 1 || got[0].Dir != d.Workspace {
		t.Fatalf("the check reported %+v, want one entry for %s", got, d.Workspace)
	}
	return got[0]
}

// parentsOf is commit's parents as the remote reads them.
func parentsOf(d *rescuetest.Disk, commit string) []string {
	return strings.Fields(d.Run(d.Remote, "git", "log", "-1", "--format=%P", commit))
}

// pushed asserts the entry pushed one branch, the issue's one, forward from previous ("" for a
// branch the push created), and that the remote holds the rescue commit it reports there: a
// commit whose tree is its first parent's and whose subject names the workspace. It returns the
// branch and the commit's parents.
func pushed(t *testing.T, d *rescuetest.Disk, w shimwire.RescueWorkspace, previous string) (shimwire.RescueBranch, []string) {
	t.Helper()
	if w.Status != rescue.Rescued || len(w.Branches) != 1 {
		t.Fatalf("the check reported %+v, want one rescued branch", w)
	}
	b := w.Branches[0]
	if b.Name != target || !b.Pushed || b.Previous != previous {
		t.Fatalf("the check reported %+v, want %s pushed forward from %q", b, target, previous)
	}
	if held := d.RemoteBranch(target); held != b.Commit {
		t.Fatalf("the remote holds %s at %q, want %s", target, held, b.Commit)
	}
	if !slices.ContainsFunc(b.Commits, func(c shimwire.RescueCommit) bool { return c.ID == b.Commit }) {
		t.Errorf("the report's commits %+v do not name the commit pushed, %s", b.Commits, b.Commit)
	}
	parents := parentsOf(d, b.Commit)
	if len(parents) == 0 {
		t.Fatalf("the rescue commit %s has no parent", b.Commit)
	}
	if tree, first := d.Run(d.Remote, "git", "rev-parse", b.Commit+"^{tree}"), d.Run(d.Remote, "git", "rev-parse", parents[0]+"^{tree}"); tree != first {
		t.Errorf("the rescue commit's tree is %s, want its first parent's %s", tree, first)
	}
	if subject, want := d.Run(d.Remote, "git", "log", "-1", "--format=%s", b.Commit), "Rescue of "+d.Workspace+" before its pod was replaced"; subject != want {
		t.Errorf("the rescue commit's subject is %q, want %q", subject, want)
	}
	return b, parents
}

// rescuedTo asserts the entry pushed the issue's one branch, new, to a rescue commit whose one
// parent is want, and returns the branch.
func rescuedTo(t *testing.T, d *rescuetest.Disk, w shimwire.RescueWorkspace, want string) shimwire.RescueBranch {
	t.Helper()
	b, parents := pushed(t, d, w, "")
	if !slices.Equal(parents, []string{want}) {
		t.Errorf("the rescue commit's parents are %v, want %s alone", parents, want)
	}
	return b
}

// pushedNothing asserts the entry is clean, that the remote's branches are before, and that no ref
// of the remote reaches any of objects.
func pushedNothing(t *testing.T, d *rescuetest.Disk, w shimwire.RescueWorkspace, before []string, objects ...string) {
	t.Helper()
	if w.Status != rescue.Clean || len(w.Branches) != 0 || w.Error != "" {
		t.Errorf("the check reported %+v, want clean", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
	for _, object := range objects {
		if d.RemoteReaches(object) {
			t.Errorf("the remote holds %s, which no check may push", object)
		}
	}
}

// ids is the commit ids of commits, sorted.
func ids(commits []shimwire.RescueCommit) []string {
	var listed []string
	for _, c := range commits {
		listed = append(listed, c.ID)
	}
	slices.Sort(listed)
	return listed
}

// sorted is commits, sorted.
func sorted(commits ...string) []string {
	return slices.Sorted(slices.Values(commits))
}

// trailers is the values of the trailer key in commit's message, as a put-back reads them.
func trailers(d *rescuetest.Disk, commit, key string) []string {
	var values []string
	for line := range strings.Lines(d.Run(d.Remote, "git", "log", "-1", "--format=%(trailers:key="+key+",valueonly)", commit)) {
		if line = strings.TrimSpace(line); line != "" {
			values = append(values, line)
		}
	}
	return values
}

// updateLine says whether the stand-in check recorded an update line that moves the issue's one
// branch from remote to local.
func updateLine(recorded []string, local, remote string) bool {
	return slices.ContainsFunc(recorded, func(line string) bool {
		f := strings.Fields(line)
		return len(f) == 4 && f[1] == local && f[2] == "refs/heads/"+target && f[3] == remote
	})
}

// remoteOnFailure has a failed test print every branch the remote holds and its commit, so the
// output names whatever a check pushed that it should not have.
func remoteOnFailure(t *testing.T, d *rescuetest.Disk) {
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the remote holds these branches:\n%s",
				d.Run(d.Remote, "git", "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads"))
		}
	})
}

// leaveOtherWork puts on d's disk work of every source that is not the issue's own line, and
// returns the objects of it no check may push: a commit the pod fetched while it ran whose branch
// origin has deleted since, a commit on another local git branch, a commit only a local tag
// names, a stash entry and the file it set aside, and a file in a second worktree of the clone.
// The workspace's files are as they were before it.
func leaveOtherWork(t *testing.T, d *rescuetest.Disk) []string {
	t.Helper()
	d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
	side := filepath.Join(filepath.Dir(d.Remote), "side")
	d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
	d.Write(side, "later.txt", "another tree's work, fetched while the pod ran\n")
	d.Run(side, "git", "add", "later.txt")
	d.Run(side, "git", "commit", "--quiet", "-m", "another tree's work")
	fetched := d.Run(side, "git", "rev-parse", "HEAD")
	d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/later")
	d.JJ("git", "fetch")
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", "later")
	d.JJ("git", "fetch")

	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	topic := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "work on another local branch")
	d.Run(d.Clone, "git", "branch", "topic", topic)
	tagged := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "tagged and never pushed")
	d.Run(d.Clone, "git", "tag", "-a", "-m", "a tag of the role's", "draft", tagged)

	second := filepath.Join(d.StateDir, "scratch", "review")
	d.Run(d.Clone, "git", "worktree", "add", "--quiet", "--detach", second, d.Base)
	d.Write(second, "review.txt", "a review in a second worktree\n")
	reviewed := d.Run(second, "git", "hash-object", "review.txt")

	before, err := os.ReadFile(filepath.Join(d.Workspace, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	d.Write(d.Workspace, "base.txt", "an edit set aside in the stash\n")
	d.Git("stash", "push", "--quiet", "-m", "set aside")
	stash := d.Git("rev-parse", "refs/stash")
	setAside := d.Git("rev-parse", "refs/stash:base.txt")
	if body, err := os.ReadFile(filepath.Join(d.Workspace, "base.txt")); err != nil || string(body) != string(before) {
		t.Fatalf("the workspace's base.txt is %q (%v) after the stash, want it put back", body, err)
	}
	return []string{fetched, topic, tagged, stash, setAside, reviewed}
}

// The first rescue of an issue pushes its own work and nothing else, to one branch: two diverged
// commits of its own line, one under the issue's bookmark and one under the working copy, and an
// edit of a tracked file in no commit, beside work of every other source on the disk. The branch
// is new, so the guard's update line names the all-zero commit as the remote's. Its tip is a
// rescue commit over the heads of that work, whose trailers name the working-copy commit and the
// issue's bookmark, and the report lists exactly the commits the push added.
//
// Checked again, the same disk pushes nothing, under the same role's environment and under another
// role's identity: the edits commit is the same commit both times, already on the branch. The
// report names the branch at its tip, not pushed by this check, with the own commits it holds.
func TestRescuePushesTheIssuesOwnWorkAloneToOneBranch(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.Write(d.Workspace, "two.txt", "own work under the issue's bookmark\n")
	d.JJ("commit", "-m", "own work under the issue's bookmark")
	underBookmark := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.JJ("bookmark", "set", d.Branch, "-r", "@-")
	d.JJ("new", d.Base)
	d.Write(d.Workspace, "one.txt", "own work under the working copy\n")
	d.JJ("commit", "-m", "own work under the working copy")
	underWorkingCopy := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	others := leaveOtherWork(t, d)
	// Last, and with no jj command after it, so no commit holds it.
	d.Write(d.Workspace, "base.txt", "base, edited in no commit\n")
	if head := d.Git("rev-parse", "HEAD"); head != underWorkingCopy {
		t.Fatalf("the workspace's git HEAD is %s, want %s: the case does not stand", head, underWorkingCopy)
	}
	before := d.RemoteBranches()
	updates := d.RecordUpdates()

	b, parents := pushed(t, d, only(t, d, d.Check(context.Background(), prefix, target)), "")
	if after, want := d.RemoteBranches(), sorted(append(slices.Clone(before), target)...); !slices.Equal(after, want) {
		t.Errorf("the remote's branches are %v, want %v: the issue's one branch and nothing else", after, want)
	}
	if len(parents) != 2 || parents[1] != underBookmark {
		t.Fatalf("the rescue commit's parents are %v, want the edits commit and then %s", parents, underBookmark)
	}
	edits := parents[0]
	if parent := parentsOf(d, edits); !slices.Equal(parent, []string{underWorkingCopy}) {
		t.Errorf("the edits commit's parents are %v, want the working copy's commit %s", parent, underWorkingCopy)
	}
	if subject, want := d.Run(d.Remote, "git", "log", "-1", "--format=%s", edits), "Edits left in the workspace "+d.Workspace+" when its pod was replaced"; subject != want {
		t.Errorf("the edits commit's subject is %q, want %q", subject, want)
	}
	for file, want := range map[string]string{"base.txt": "base, edited in no commit", "one.txt": "own work under the working copy"} {
		if got := d.Run(d.Remote, "git", "show", b.Commit+":"+file); got != want {
			t.Errorf("the rescue commit's %s holds %q, want %q", file, got, want)
		}
	}
	for _, object := range others {
		if d.RemoteReaches(object) {
			t.Errorf("the remote holds %s, which is no work of the issue's own line", object)
		}
	}
	if got := trailers(d, b.Commit, rescue.TrailerWorkingCopy); !slices.Equal(got, []string{edits}) {
		t.Errorf("the rescue commit names the working copy %q, want the edits commit %s", got, edits)
	}
	if got, want := trailers(d, b.Commit, rescue.TrailerBookmark), []string{d.Branch + " " + underBookmark}; !slices.Equal(got, want) {
		t.Errorf("the rescue commit names the bookmarks %q, want %q", got, want)
	}
	added := strings.Fields(d.Run(d.Remote, "git", "rev-list", b.Commit, "--not", "--exclude=refs/heads/"+target, "--all"))
	if got, want := ids(b.Commits), sorted(added...); !slices.Equal(got, want) {
		t.Errorf("the report lists the commits %v, want what the push added, %v", got, want)
	}
	if want := sorted(b.Commit, edits, underWorkingCopy, underBookmark); !slices.Equal(sorted(added...), want) {
		t.Errorf("the push added %v, want the rescue commit, the edits commit and the two own commits %v", added, want)
	}
	if recorded := updates(); !updateLine(recorded, b.Commit, zeroSHA) {
		t.Errorf("the stand-in check recorded %q, want an update line moving refs/heads/%s from %s to %s", recorded, target, zeroSHA, b.Commit)
	}

	own := sorted(edits, underWorkingCopy, underBookmark)
	for _, again := range []struct {
		name string
		env  []string
	}{
		{name: "checked again under the same environment", env: d.Env},
		{name: "checked again under another role's identity", env: append(slices.Clone(d.Env),
			"GIT_AUTHOR_NAME=Another role", "GIT_AUTHOR_EMAIL=another-role@example.com",
			"GIT_COMMITTER_NAME=Another role", "GIT_COMMITTER_EMAIL=another-role@example.com")},
	} {
		t.Run(again.name, func(t *testing.T) {
			updates := d.RecordUpdates()
			w := only(t, d, rescue.Run(context.Background(), rescue.Request{
				StateDir: d.StateDir, Branch: prefix, Target: target, Env: again.env,
				WorkspaceTimeout: rescue.WorkspaceTimeout, Deadline: time.Now().Add(rescue.Timeout),
			}))
			if w.Status != rescue.Rescued || len(w.Branches) != 1 {
				t.Fatalf("the check reported %+v, want the branch reported and nothing pushed", w)
			}
			if got := w.Branches[0]; got.Name != target || got.Commit != b.Commit || got.Pushed || got.Previous != "" || !slices.Equal(ids(got.Commits), own) {
				t.Errorf("the check reported %+v, want %s at %s, not pushed, with the own commits %v", got, target, b.Commit, own)
			}
			if held := d.RemoteBranch(target); held != b.Commit {
				t.Errorf("the remote holds %s at %s, want it left at %s", target, held, b.Commit)
			}
			if recorded := updates(); len(recorded) != 0 {
				t.Errorf("the stand-in check recorded %q, want no push to check", recorded)
			}
		})
	}
}

// The next pod's disk, made after the first rescue, holds the branch's tip and one own head that
// lacks it: the branch moves forward, with no force, to a rescue commit whose parents are that head
// and the tip, and the guard's update line names the tip as the remote's commit. The remote
// refuses every update that is not a fast-forward. Checked again, the disk pushes nothing.
func TestRescueOfTheNextPodMovesTheBranchForward(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.Write(d.Workspace, "first.txt", "the first pod's work\n")
	d.JJ("commit", "-m", "the first pod's work")
	first := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	tip := rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), first).Commit

	next := d.NextPod(t)
	d.Run(d.Remote, "git", "config", "receive.denyNonFastForwards", "true")
	next.Write(next.Workspace, "second.txt", "the next pod's work\n")
	next.JJ("commit", "-m", "the next pod's work")
	second := next.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	updates := next.RecordUpdates()

	moved, parents := pushed(t, next, only(t, next, next.Check(context.Background(), prefix, target)), tip)
	if !slices.Equal(parents, []string{second, tip}) {
		t.Errorf("the rescue commit's parents are %v, want the next pod's head %s and the earlier tip %s", parents, second, tip)
	}
	if recorded := updates(); !updateLine(recorded, moved.Commit, tip) {
		t.Errorf("the stand-in check recorded %q, want an update line moving refs/heads/%s from %s to %s", recorded, target, tip, moved.Commit)
	}

	updates = next.RecordUpdates()
	again := only(t, next, next.Check(context.Background(), prefix, target))
	want := []shimwire.RescueBranch{{Name: target, Commit: moved.Commit, Commits: []shimwire.RescueCommit{{ID: second, Subject: "the next pod's work"}}}}
	if again.Status != rescue.Rescued || !slices.EqualFunc(again.Branches, want, func(a, b shimwire.RescueBranch) bool {
		return a.Name == b.Name && a.Commit == b.Commit && a.Pushed == b.Pushed && a.Previous == b.Previous && slices.Equal(a.Commits, b.Commits)
	}) {
		t.Errorf("a second check reported %+v, want %+v and nothing pushed", again, want)
	}
	if held := next.RemoteBranch(target); held != moved.Commit {
		t.Errorf("the remote holds %s at %s, want it left at %s", target, held, moved.Commit)
	}
	if recorded := updates(); len(recorded) != 0 {
		t.Errorf("the stand-in check recorded %q, want no push to check", recorded)
	}
}

// cloneRefs is every ref of d's clone and its commit, and the clone's FETCH_HEAD.
func cloneRefs(t *testing.T, d *rescuetest.Disk) (map[string]string, string) {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(d.Run(d.Clone, "git", "for-each-ref", "--format=%(refname) %(objectname)")) {
		name, commit, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[name] = commit
	}
	fetchHead, err := os.ReadFile(filepath.Join(d.Clone, ".git", "FETCH_HEAD"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return refs, string(fetchHead)
}

// A pod made before the first rescue, and checked after it, has a clone that lacks the branch's
// tip. Its check fetches the tip, writing no ref, and moves the branch forward over it: for a disk
// with a commit of its own, and for a disk whose one work is an edit of a tracked file, whose
// rescue commit's parents are the edits commit and the tip. The one ref of the clone the check
// changes is the remote-tracking ref of the branch, which its push updates.
func TestRescueOfAPodMadeBeforeTheBranchMovedFetchesTheTip(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	committed := d.NextPod(t)
	edited := d.NextPod(t)
	d.Write(d.Workspace, "first.txt", "the first pod's work\n")
	d.JJ("commit", "-m", "the first pod's work")
	first := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	tip := rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), first).Commit

	committed.Write(committed.Workspace, "second.txt", "work of a pod made before the rescue\n")
	committed.JJ("commit", "-m", "work of a pod made before the rescue")
	second := committed.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	refsBefore, fetchHeadBefore := cloneRefs(t, committed)
	tracking := "refs/remotes/origin/" + target
	if held, ok := refsBefore[tracking]; ok {
		t.Fatalf("the clone's %s is %s: the case does not stand", tracking, held)
	}
	if objects := strings.Fields(committed.Run(committed.Clone, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")); slices.Contains(objects, tip) {
		t.Fatalf("the clone holds the tip %s: the case does not stand", tip)
	}
	updates := committed.RecordUpdates()
	moved, parents := pushed(t, committed, only(t, committed, committed.Check(context.Background(), prefix, target)), tip)
	if !slices.Equal(parents, []string{second, tip}) {
		t.Errorf("the rescue commit's parents are %v, want the pod's head %s and the fetched tip %s", parents, second, tip)
	}
	if recorded := updates(); !updateLine(recorded, moved.Commit, tip) {
		t.Errorf("the stand-in check recorded %q, want an update line moving refs/heads/%s from %s to %s", recorded, target, tip, moved.Commit)
	}
	refsAfter, fetchHeadAfter := cloneRefs(t, committed)
	if refsAfter[tracking] != moved.Commit {
		t.Errorf("the clone's %s is %q, want the pushed %s", tracking, refsAfter[tracking], moved.Commit)
	}
	delete(refsAfter, tracking)
	if !maps.Equal(refsAfter, refsBefore) || fetchHeadAfter != fetchHeadBefore {
		t.Errorf("the clone's refs are %v and FETCH_HEAD %q after the check, want %v and %q: the fetch writes no ref", refsAfter, fetchHeadAfter, refsBefore, fetchHeadBefore)
	}

	edited.Write(edited.Workspace, "base.txt", "an edit of a pod made before the rescue\n")
	head := edited.Git("rev-parse", "HEAD")
	b, parents := pushed(t, edited, only(t, edited, edited.Check(context.Background(), prefix, target)), moved.Commit)
	if len(parents) != 2 || parents[1] != moved.Commit {
		t.Fatalf("the rescue commit's parents are %v, want the edits commit and the tip %s", parents, moved.Commit)
	}
	if parent := parentsOf(edited, parents[0]); !slices.Equal(parent, []string{head}) {
		t.Errorf("the edits commit %s's parents are %v, want the workspace's head %s", parents[0], parent, head)
	}
	if got := edited.Run(edited.Remote, "git", "show", b.Commit+":base.txt"); got != "an edit of a pod made before the rescue" {
		t.Errorf("the rescue commit's base.txt holds %q, want the edit", got)
	}
}

// A disk whose own line holds nothing the remote lacks is clean, and nothing is pushed, whatever
// work of other sources it holds.
func TestRescueOfADiskWithNoOwnWorkPushesNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	others := leaveOtherWork(t, d)
	before := d.RemoteBranches()
	pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, others...)
}

// A workspace whose directory names another issue than the request's branch fails, naming both
// keys, and pushes nothing; so does a directory whose name is no issue key, even beside a request
// for its neighbour's issue, which is rescued.
func TestRescueFailsAWorkspaceOfAnotherIssueAndPushesNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.Write(d.Workspace, "feature.txt", "the issue's own work\n")
	d.JJ("commit", "-m", "the issue's own work")
	own := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	stray := filepath.Join(filepath.Dir(d.Workspace), "scratch")
	d.Run(d.Clone, "git", "worktree", "add", "--quiet", "--detach", stray, d.Base)
	d.Write(stray, "stray.txt", "work in a directory that names no issue\n")
	d.Run(stray, "git", "add", "stray.txt")
	d.Run(stray, "git", "commit", "--quiet", "-m", "work in a directory that names no issue")
	strayCommit := d.Run(stray, "git", "rev-parse", "HEAD")
	before := d.RemoteBranches()

	got := d.Check(context.Background(), "legion/WIDGETS-7-rescue-9d3f0c7a", "legion/WIDGETS-7-rescue")
	if len(got) != 2 || got[0].Dir != stray || got[1].Dir != d.Workspace {
		t.Fatalf("the check reported %+v, want the stray directory, then the workspace", got)
	}
	for i, want := range [][]string{{stray, "names no issue key", "WIDGETS-7"}, {d.Workspace, "WIDGETS-42", "WIDGETS-7"}} {
		if got[i].Status != rescue.Failed || len(got[i].Branches) != 0 {
			t.Errorf("the entry of %s is %+v, want an error and no branch", got[i].Dir, got[i])
		}
		for _, part := range want {
			if !strings.Contains(got[i].Error, part) {
				t.Errorf("the error of %s is %q, want it to name %q", got[i].Dir, got[i].Error, part)
			}
		}
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
	for _, commit := range []string{own, strayCommit} {
		if d.RemoteReaches(commit) {
			t.Errorf("the remote holds %s, which a request of another issue may not push", commit)
		}
	}

	got = d.Check(context.Background(), prefix, target)
	if len(got) != 2 || got[0].Status != rescue.Failed || !strings.Contains(got[0].Error, "names no issue key") || !strings.Contains(got[0].Error, issue) {
		t.Fatalf("the check of the issue's own branch reported %+v, want the stray directory refused, naming %s", got, issue)
	}
	rescuedTo(t, d, got[1], own)
	if d.RemoteReaches(strayCommit) {
		t.Errorf("the remote holds %s, from a directory that names no issue", strayCommit)
	}
}

// A workspace directory that workspace.Remove renamed aside to delete, `<issue>.removing`, is not
// checked: its removal already let its work go.
func TestRescueLeavesAWorkspaceSetAsideForRemovalUnchecked(t *testing.T) {
	d := rescuetest.New(t, issue)
	aside := d.Workspace + ".removing"
	d.Run(d.Clone, "git", "worktree", "add", "--quiet", "--detach", aside, d.Base)
	d.Write(aside, "removed.txt", "work of a workspace being removed\n")
	d.Run(aside, "git", "add", "removed.txt")
	d.Run(aside, "git", "commit", "--quiet", "-m", "work of a workspace being removed")
	removed := d.Run(aside, "git", "rev-parse", "HEAD")
	before := d.RemoteBranches()
	pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, removed)
}

// A workspace's repository is read from its path as workspace.Location lays it out,
// <state>/workspaces/<owner>/<repo>/<issue>, under any state directory; a directory laid out
// otherwise, the repository's own directory among them, stands for itself.
func TestRescueRepositoryOfIsTheOwnerAndRepoOfAWorkspacesPath(t *testing.T) {
	located, err := workspace.Location("/legion", ghrepo.MustParse(rescuetest.Owner+"/"+rescuetest.Repo), issue)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ dir, want string }{
		{located.Dir, rescuetest.Owner + "/" + rescuetest.Repo},
		{"/var/state/workspaces/octo/tools/tools-7", "octo/tools"},
		{"/legion/workspaces/acme/widgets", "/legion/workspaces/acme/widgets"},
		{"/legion/scratch/acme/widgets/widgets-42", "/legion/scratch/acme/widgets/widgets-42"},
	} {
		if got := rescue.RepositoryOf(tc.dir); got != tc.want {
			t.Errorf("RepositoryOf(%q) = %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// A request of an older daemon names no Target: the check pushes the issue's own work to the
// request's per-pod Branch itself, with no number, by the same rule.
func TestRescueForAnOlderDaemonPushesToItsPerPodBranchWithNoNumber(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.Write(d.Workspace, "feature.txt", "work an older daemon's request finds\n")
	d.JJ("commit", "-m", "work an older daemon's request finds")
	own := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	w := only(t, d, d.Check(context.Background(), prefix, ""))
	if w.Status != rescue.Rescued || len(w.Branches) != 1 || w.Branches[0].Name != prefix || !w.Branches[0].Pushed || len(w.Branches[0].Commits) == 0 {
		t.Fatalf("the check reported %+v, want %s pushed", w, prefix)
	}
	commit := w.Branches[0].Commit
	if held := d.RemoteBranch(prefix); held != commit {
		t.Errorf("the remote holds %s at %q, want %s", prefix, held, commit)
	}
	if parents := parentsOf(d, commit); !slices.Equal(parents, []string{own}) {
		t.Errorf("the rescue commit's parents are %v, want %s alone", parents, own)
	}
	for _, name := range []string{target, prefix + "-1"} {
		if held := d.RemoteBranch(name); held != "" {
			t.Errorf("the remote holds %s at %s, want no such branch", name, held)
		}
	}
}

// The issue's bookmark on jj's empty working-copy commit with no description, where provisioning
// sets it, is still there for a read that takes no snapshot while the role's edit of a tracked
// file is in no commit. The rescue commit names the bookmark at the commit under it, which the
// remote holds, and never at the empty commit, which no push carries: a put-back must find every
// commit a trailer names.
func TestRescueNamesTheIssuesBookmarkOnAnEmptyWorkingCopyAtTheCommitUnderIt(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.JJ("bookmark", "set", d.Branch, "-r", "@")
	empty := d.JJ("log", "--no-graph", "-r", `@ & empty() & description(exact:"")`, "-T", "commit_id")
	if branch := d.Run(d.Clone, "git", "rev-parse", "refs/heads/"+d.Branch); empty == "" || branch != empty {
		t.Fatalf("the issue's git branch is %s, want jj's empty working-copy commit %q: the case does not stand", branch, empty)
	}
	// Last, and with no jj command after it, so jj still reads the bookmark on the empty commit.
	d.Write(d.Workspace, "base.txt", "base, edited in no commit\n")

	b, _ := pushed(t, d, only(t, d, d.Check(context.Background(), prefix, target)), "")
	got := trailers(d, b.Commit, rescue.TrailerBookmark)
	if len(got) != 1 {
		t.Fatalf("the rescue commit names the bookmarks %q, want %s alone", got, d.Branch)
	}
	name, commit, _ := strings.Cut(got[0], " ")
	if name != d.Branch || commit == empty || commit != d.Base {
		t.Errorf("the rescue commit names the bookmark %q, want %s at the commit under the empty working copy, %s", got[0], d.Branch, d.Base)
	}
	if !d.RemoteReaches(commit) {
		t.Errorf("the remote holds no %s, which the rescue commit names for %s", commit, d.Branch)
	}
}

// A rescue commit names a bookmark other than the issue's own only when it is ahead of what the
// clone holds of the remote's branch of its name or the remote has no branch of its name: a
// put-back that set one at or behind that branch would move the new pod's copy back. Local `main`
// behind the remote's, which moved after the pod last fetched it so that the clone lacks its tip,
// is not named: the remote-tracking ref holds it. A bookmark at the remote's tip of its branch,
// which the clone holds while its remote-tracking ref of that branch is absent or older, is not
// named either: the advertised tip holds it. `main` moved ahead onto an own commit is named, and
// stays named where only the issue's own branch on the remote holds that commit, since a bookmark
// is compared with its own branch alone; and so is a bookmark the remote has no branch of, on an
// own commit or on a commit the remote holds.
func TestRescueNamesNoBookmarkTheRemoteHasThatCarriesNoWorkOfItsOwn(t *testing.T) {
	// ownWork commits work the remote lacks on the workspace's own line, over main, and is it.
	ownWork := func(d *rescuetest.Disk) string {
		d.Write(d.Workspace, "feature.txt", "the issue's own work\n")
		d.JJ("commit", "-m", "the issue's own work")
		return d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	}
	// named is the bookmarks the rescue commit d's check pushes names.
	named := func(t *testing.T, d *rescuetest.Disk) []string {
		t.Helper()
		b, _ := pushed(t, d, only(t, d, d.Check(context.Background(), prefix, target)), "")
		return trailers(d, b.Commit, rescue.TrailerBookmark)
	}

	t.Run("main behind the remote's, which moved after the pod fetched it", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		remoteOnFailure(t, d)
		moved := d.Run(d.Remote, "git", "commit-tree", d.Base+"^{tree}", "-p", d.Base, "-m", "main moves on after the pod fetched it")
		d.Run(d.Remote, "git", "update-ref", "refs/heads/main", moved, d.Base)
		ownWork(d)
		if main := d.JJ("log", "--no-graph", "-r", `bookmarks(exact:"main")`, "-T", "commit_id"); main != d.Base {
			t.Fatalf("the workspace's main is %q, want %s, behind the remote's %s: the case does not stand", main, d.Base, moved)
		}
		if objects := strings.Fields(d.Run(d.Clone, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")); slices.Contains(objects, moved) {
			t.Fatalf("the clone holds the remote's main %s: the case does not stand", moved)
		}
		if got, want := named(t, d), []string{d.Branch + " " + d.Base}; !slices.Equal(got, want) {
			t.Errorf("the rescue commit names the bookmarks %q, want %q alone: main carries nothing the remote's main at %s lacks", got, want, moved)
		}
	})

	t.Run("main moved ahead onto an own commit", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		remoteOnFailure(t, d)
		work := ownWork(d)
		d.JJ("bookmark", "set", "main", "-r", work)
		if got, want := named(t, d), []string{d.Branch + " " + d.Base, "main " + work}; !slices.Equal(got, want) {
			t.Errorf("the rescue commit names the bookmarks %q, want %q: main carries work the remote's main lacks", got, want)
		}
	})

	t.Run("main moved ahead onto an own commit only the issue's branch on the remote holds", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		remoteOnFailure(t, d)
		work := ownWork(d)
		d.JJ("bookmark", "set", d.Branch, "-r", work)
		d.JJ("git", "push", "--bookmark", d.Branch)
		d.JJ("bookmark", "set", "main", "-r", work)
		if branch, main := d.RemoteBranch(d.Branch), d.RemoteBranch("main"); branch != work || main != d.Base {
			t.Fatalf("the remote holds %s at %s and main at %s, want %s and %s: the case does not stand", d.Branch, branch, main, work, d.Base)
		}
		// More work, so that the check has something to push.
		d.Write(d.Workspace, "more.txt", "more of the issue's own work\n")
		d.JJ("commit", "-m", "more of the issue's own work")
		if got, want := named(t, d), []string{d.Branch + " " + work, "main " + work}; !slices.Equal(got, want) {
			t.Errorf("the rescue commit names the bookmarks %q, want %q: main carries work the remote's main lacks, whatever another branch holds", got, want)
		}
	})

	t.Run("a bookmark at the remote's tip, which the clone holds and its remote-tracking ref does not reach", func(t *testing.T) {
		for name, fetched := range map[string]bool{"no remote-tracking ref": false, "a remote-tracking ref at the base": true} {
			t.Run(name, func(t *testing.T) {
				d := rescuetest.New(t, issue)
				remoteOnFailure(t, d)
				work := ownWork(d)
				tracking := "" // the clone's refs/remotes/origin/feature
				if fetched {
					// A push to origin by its name sets the clone's remote-tracking ref.
					d.Run(d.Clone, "git", "push", "--quiet", "origin", d.Base+":refs/heads/feature")
					tracking = d.Base
				}
				// A push to the remote's path sets no remote-tracking ref of the clone.
				d.Run(d.Clone, "git", "push", "--quiet", d.Remote, work+":refs/heads/feature")
				d.JJ("bookmark", "set", "feature", "-r", work)
				// More work, so that the check has something to push.
				d.Write(d.Workspace, "more.txt", "more of the issue's own work\n")
				d.JJ("commit", "-m", "more of the issue's own work")
				if held := d.Run(d.Clone, "git", "for-each-ref", "--format=%(objectname)", "refs/remotes/origin/feature"); held != tracking {
					t.Fatalf("the clone's refs/remotes/origin/feature is %q, want %q: the case does not stand", held, tracking)
				}
				if remote, local := d.RemoteBranch("feature"), d.JJ("log", "--no-graph", "-r", `bookmarks(exact:"feature")`, "-T", "commit_id"); remote != work || local != work {
					t.Fatalf("the remote holds feature at %q and the workspace at %q, want both at %s: the case does not stand", remote, local, work)
				}
				if got, want := named(t, d), []string{d.Branch + " " + d.Base}; !slices.Equal(got, want) {
					t.Errorf("the rescue commit names the bookmarks %q, want %q alone: feature is at the remote's feature, %s, which the clone holds", got, want, work)
				}
			})
		}
	})

	t.Run("bookmarks the remote has no branch of", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		remoteOnFailure(t, d)
		work := ownWork(d)
		d.JJ("bookmark", "create", "side-note", "-r", work)
		d.JJ("bookmark", "create", "starting-point", "-r", d.Base)
		if got, want := named(t, d), []string{d.Branch + " " + d.Base, "side-note " + work, "starting-point " + d.Base}; !slices.Equal(got, want) {
			t.Errorf("the rescue commit names the bookmarks %q, want %q: the remote has neither name, so both are the issue's", got, want)
		}
	})
}

// jj's working-copy commit can be an empty merge with no description (`jj new A B`): holdingWork
// keeps a merge, and ownHeads drops the empty commit, so the merge is no tip and is in no push.
// The rescue commit stands for it, since its parents are the merge's heads, and names its own
// first parent as the working copy: a put-back must find every commit a trailer names.
func TestRescueNamesTheFirstParentForAWorkingCopyThatIsAnEmptyMerge(t *testing.T) {
	d := rescuetest.New(t, issue)
	remoteOnFailure(t, d)
	d.JJ("new", "-m", "the change, made one way", d.Base)
	d.Write(d.Workspace, "shared.txt", "the same change, made twice\n")
	one := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", "-m", "the change, made the other way", d.Base)
	d.Write(d.Workspace, "shared.txt", "the same change, made twice\n")
	other := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", one, other)
	merge := d.JJ("log", "--no-graph", "-r", `@ & empty() & description(exact:"")`, "-T", "commit_id")
	if merge == "" {
		t.Fatal("jj's working-copy commit is not an empty merge with no description: the case does not stand")
	}
	if status := d.Git("status", "--porcelain"); status != "" {
		t.Fatalf("the workspace's files are %q, want them clean, so no edits commit stands for the working copy: the case does not stand", status)
	}

	b, parents := pushed(t, d, only(t, d, d.Check(context.Background(), prefix, target)), "")
	if want := sorted(one, other); !slices.Equal(parents, want) {
		t.Fatalf("the rescue commit's parents are %v, want the merge's heads %v", parents, want)
	}
	if got := trailers(d, b.Commit, rescue.TrailerWorkingCopy); !slices.Equal(got, []string{parents[0]}) || slices.Contains(got, merge) {
		t.Errorf("the rescue commit names the working copy %q, want its first parent %s and not the merge %s", got, parents[0], merge)
	}
	if d.RemoteReaches(merge) {
		t.Errorf("the remote holds the empty merge %s, which no push carries", merge)
	}
}

// A workspace that holds nothing its remote lacks — jj's empty, undescribed working copy on the
// pushed commit — is clean, and nothing is pushed.
func TestACleanWorkspaceIsCleanAndNothingIsPushed(t *testing.T) {
	d := rescuetest.New(t, issue)
	before := d.RemoteBranches()
	w := only(t, d, d.Check(context.Background(), prefix, target))
	if w.Status != rescue.Clean || len(w.Branches) != 0 || w.Error != "" {
		t.Fatalf("the check reported %+v, want clean", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// A commit made with plain git in the workspace, with its files left clean, moves the workspace's
// git HEAD before jj reads it; no remote ref holds it, so it is rescued as it stands.
func TestAnUnpushedGitCommitOnTheWorkspaceHeadIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "fix.txt", "a fix made with plain git\n")
	d.Git("add", "fix.txt")
	d.Git("commit", "--quiet", "-m", "a fix made with plain git")
	commit := d.Git("rev-parse", "HEAD")
	if status := d.Git("status", "--porcelain"); status != "" {
		t.Fatalf("the workspace's files are %q, want them clean: the case does not stand", status)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
}

// A commit on the issue's own branch that no remote ref holds is rescued, though the workspace's
// HEAD does not name it: the branch is the issue's, whichever worktree last worked on it.
func TestAnUnpushedCommitOnTheIssuesOwnBranchIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "work on the issue's branch")
	d.Run(d.Clone, "git", "branch", "--force", d.Branch, commit)
	if head := d.Git("rev-parse", "HEAD"); head != d.Base {
		t.Fatalf("the workspace's git HEAD is %s, want %s: only the issue's branch may name the commit", head, d.Base)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
}

// Files written in the workspace and in no commit become one commit on top of the workspace's
// head, built in an index of its own: the workspace's files, its index, its HEAD and jj's
// operation log are what they were. The rescue commit over it names it as the working copy.
func TestADirtyTreeIsRescuedAsOneCommitWithoutChangingTheWorkspace(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "notes/draft.txt", "an edit in no commit\n")
	d.Write(d.Workspace, "base.txt", "base, edited\n")
	head := d.Git("rev-parse", "HEAD")
	index := d.Git("rev-parse", "--path-format=absolute", "--git-path", "index")
	indexBefore, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	ops := opCount(t, d)

	b, parents := pushed(t, d, only(t, d, d.Check(context.Background(), prefix, target)), "")
	if len(parents) != 1 {
		t.Fatalf("the rescue commit's parents are %v, want the edits commit alone", parents)
	}
	edits := parents[0]
	if parent := d.Run(d.Remote, "git", "rev-parse", edits+"^"); parent != head {
		t.Errorf("the edits commit's parent is %s, want the workspace's head %s", parent, head)
	}
	for file, want := range map[string]string{"notes/draft.txt": "an edit in no commit", "base.txt": "base, edited"} {
		if got := d.Run(d.Remote, "git", "show", edits+":"+file); got != want {
			t.Errorf("the edits commit's %s holds %q, want %q", file, got, want)
		}
	}
	if got := trailers(d, b.Commit, rescue.TrailerWorkingCopy); !slices.Equal(got, []string{edits}) {
		t.Errorf("the rescue commit names the working copy %q, want the edits commit %s", got, edits)
	}
	if got := d.Git("rev-parse", "HEAD"); got != head {
		t.Errorf("the workspace's HEAD is %s after the check, want %s", got, head)
	}
	if indexAfter, err := os.ReadFile(index); err != nil || string(indexAfter) != string(indexBefore) {
		t.Errorf("the workspace's index changed under the check (%v)", err)
	}
	if body, err := os.ReadFile(filepath.Join(d.Workspace, "notes", "draft.txt")); err != nil || string(body) != "an edit in no commit\n" {
		t.Errorf("the workspace's file is %q (%v) after the check, want it as written", body, err)
	}
	if got := opCount(t, d); got != ops {
		t.Errorf("jj's operation log holds %d operations after the check, want %d", got, ops)
	}
}

// opCount is how many operations jj's log holds, which a check that wrote nothing leaves alone.
func opCount(t *testing.T, d *rescuetest.Disk) int {
	t.Helper()
	return len(strings.Fields(d.Run(d.Clone, "jj", "op", "log", "--no-graph", "--ignore-working-copy", "--at-op=@", "-T", `id.short() ++ "\n"`)))
}

// A commit only jj names — the working copy described with the work in it, so the workspace's git
// HEAD is still the pushed commit under it — is rescued as jj holds it, on the workspace's own line.
func TestAJJOnlyCommitIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work jj alone holds\n")
	d.JJ("describe", "-m", "work jj alone holds")
	commit := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	if head := d.Git("rev-parse", "HEAD"); head != d.Base {
		t.Fatalf("the workspace's git HEAD is %s, want %s: only jj may name the commit", head, d.Base)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
}

// A merge whose bookmark was never moved onto it: the worker's push of the bookmark says "Nothing
// changed", because the bookmark still names the commit the remote has, and the merge is on the
// disk alone. It is empty — both of its parents are pushed and it resolves nothing — but it is
// described. With the working copy still on top of it, it is on the working copy's line and is
// rescued. With the working copy moved back onto the bookmark since, it is on neither line, and
// nothing is pushed.
func TestAMergeWhoseBookmarkWasNotMovedOntoItIsRescuedOnlyOnTheWorkingCopysLine(t *testing.T) {
	for name, moveOn := range map[string]bool{"the working copy on top of it": false, "the working copy moved back onto the bookmark": true} {
		t.Run(name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			side := filepath.Join(filepath.Dir(d.Remote), "side")
			d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
			d.Write(side, "side.txt", "side\n")
			d.Run(side, "git", "add", "side.txt")
			d.Run(side, "git", "commit", "--quiet", "-m", "side")
			d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/side")
			d.JJ("git", "fetch")
			d.JJ("new", "-m", "merge side into the issue", `bookmarks(exact:"`+d.Branch+`")`, `remote_bookmarks(exact:"side")`)
			merge := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
			d.JJ("new")
			if said := d.JJ("git", "push", "--bookmark", d.Branch); !strings.Contains(said, "Nothing changed") {
				t.Fatalf("the worker's push said %q, want the \"Nothing changed\" a bookmark left behind its merge gives", said)
			}
			if held := d.RemoteBranch(d.Branch); held != d.Base {
				t.Fatalf("the remote's %s is %s, want it still at %s", d.Branch, held, d.Base)
			}
			if !moveOn {
				rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), merge)
				return
			}
			d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
			if head := d.Git("rev-parse", "HEAD"); head != d.Base {
				t.Fatalf("the workspace's git HEAD is %s, want %s: only jj may name the merge", head, d.Base)
			}
			before := d.RemoteBranches()
			pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, merge)
		})
	}
}

// A commit the remote already holds, under a ref the clone never fetched (GitHub keeps a pull
// request's head after its branch is deleted), is no lost work: the check reads the remote itself.
func TestACommitTheRemoteHoldsUnderARefTheCloneNeverFetchedIsClean(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "merged.txt", "squash-merged work\n")
	d.JJ("commit", "-m", "squash-merged work")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.Run(d.Clone, "git", "push", "--quiet", "origin", commit+":refs/pull/7/head")
	if w := only(t, d, d.Check(context.Background(), prefix, target)); w.Status != rescue.Clean {
		t.Fatalf("the check reported %+v, want clean: the remote holds %s under refs/pull/7/head", w, commit)
	}
}

// A commit of the issue's own branch that only a stale remote-tracking ref names — the branch
// deleted on the remote since the pod last fetched — is a commit the remote may no longer hold,
// and it is rescued.
func TestAnOwnCommitOnlyAStaleRemoteTrackingRefNamesIsRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	tree := d.Run(d.Clone, "git", "rev-parse", d.Base+"^{tree}")
	commit := d.Run(d.Clone, "git", "commit-tree", tree, "-p", d.Base, "-m", "pushed once, then deleted on the remote")
	d.Run(d.Clone, "git", "branch", "--force", d.Branch, commit)
	d.Run(d.Clone, "git", "push", "--quiet", "origin", d.Branch)
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", d.Branch)
	if tracked := d.Run(d.Clone, "git", "rev-parse", "refs/remotes/origin/"+d.Branch); tracked != commit {
		t.Fatalf("the clone's origin/%s is %s, want the stale %s", d.Branch, tracked, commit)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
}

// The check reads only the workspace, never the clone's own directory. An issue pod's clone holds
// no working tree at all (workspace.feedClone), while its git HEAD and its index still name every
// file of the default branch, so a check that took the clone's directory would read the whole
// tree as deleted and push that deletion as a rescue branch. The clone is no agent's workspace,
// and nothing of it is pushed.
func TestTheClonesOwnDirectoryIsNotChecked(t *testing.T) {
	d := rescuetest.NewWithoutCloneTree(t, issue)
	if files := d.Run(d.Clone, "git", "ls-files"); files != "base.txt" {
		t.Fatalf("the clone's index names %q, want base.txt: the case stands on a clone whose HEAD and index name a file it holds no copy of", files)
	}
	before := d.RemoteBranches()
	w := only(t, d, d.Check(context.Background(), prefix, target))
	if w.Status != rescue.Clean || len(w.Branches) != 0 || w.Error != "" {
		for _, b := range w.Branches {
			t.Errorf("the check pushed %s at %s, whose tree is %s, against the clone's HEAD tree %s",
				b.Name, b.Commit, d.Run(d.Remote, "git", "rev-parse", b.Commit+"^{tree}"),
				d.Run(d.Clone, "git", "rev-parse", "HEAD^{tree}"))
		}
		t.Fatalf("the check reported %+v, want clean: the clone's absent working tree is no agent's work", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// Leaving the clone out takes no work with it: a workspace on such a disk that holds a commit the
// remote lacks is rescued, as it is on a disk whose clone has a working tree.
func TestAWorkspaceOfACloneWithNoWorkingTreeIsStillRescued(t *testing.T) {
	d := rescuetest.NewWithoutCloneTree(t, issue)
	d.Write(d.Workspace, "fix.txt", "a fix the remote lacks\n")
	d.JJ("commit", "-m", "a fix the remote lacks")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
	if body := d.Run(d.Remote, "git", "show", commit+":fix.txt"); body != "a fix the remote lacks" {
		t.Errorf("the rescued commit's fix.txt holds %q, want the workspace's file", body)
	}
}

// A directory under workspaces/ that is no git worktree is an error, never a pass: whatever it
// holds, nothing can push it.
func TestADirectoryThatIsNoGitWorktreeIsAnError(t *testing.T) {
	d := rescuetest.New(t, issue)
	stray := filepath.Join(filepath.Dir(d.Workspace), "stray")
	d.Write(stray, "file.txt", "a file in no repository\n")
	got := d.Check(context.Background(), prefix, target)
	if len(got) != 2 || got[0].Dir != stray || got[1].Dir != d.Workspace {
		t.Fatalf("the check reported %+v, want the stray directory, then the workspace", got)
	}
	if got[0].Status != rescue.Failed || !strings.Contains(got[0].Error, "is not a git worktree") {
		t.Errorf("the stray directory's entry is %+v, want an error saying it is no git worktree", got[0])
	}
	if got[1].Status != rescue.Clean {
		t.Errorf("the workspace's entry is %+v, want clean", got[1])
	}
}

// A push the remote refuses is an error, never a pass, and leaves no branch behind.
func TestAPushTheRemoteRefusesIsAnError(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work the remote will not take\n")
	d.JJ("commit", "-m", "work the remote will not take")
	d.RefuseWrites()
	w := only(t, d, d.Check(context.Background(), prefix, target))
	if w.Status != rescue.Failed || !strings.Contains(w.Error, "the remote refuses this push") || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want an error naming the remote's refusal", w)
	}
	if held := d.RemoteBranch(target); held != "" {
		t.Errorf("the remote holds %s at %s, want no such branch", target, held)
	}
}

// The tests' environment lets git use no transport but local files, whatever the repository's own
// configuration allows: a clone whose origin is a loopback HTTP server, and whose configuration
// allows http itself, is an error before the server sees one request.
func TestTheTestsEnvironmentReachesNoNetworkRemote(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work that must not leave the test\n")
	d.JJ("commit", "-m", "work that must not leave the test")
	d.Run(d.Clone, "git", "remote", "set-url", "origin", server.URL+"/owner/repo.git")
	d.Run(d.Clone, "git", "config", "protocol.http.allow", "always")
	w := only(t, d, d.Check(context.Background(), prefix, target))
	if w.Status != rescue.Failed || !strings.Contains(w.Error, "transport 'http' not allowed") {
		t.Errorf("the check reported %+v, want git's refusal of the http transport", w)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the loopback server received %d requests, want none", n)
	}
}

// Every push passes the leak guard before it is made: a push the shared public leak check
// refuses fails the workspace, leaving the remote without the branch it would have created,
// and the same push passes and is made once the check clears it. The check the guard runs is
// the disk's stand-in (rescuetest.Passes, rescuetest.Refuses), the real one being a script no
// clone of this repository holds.
func TestAPushTheLeakGuardRefusesIsNotMade(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.LeakCheck(rescuetest.Refuses)
	d.Write(d.Workspace, "feature.txt", "work the guard reads before it is pushed\n")
	d.JJ("commit", "-m", "work the guard reads before it is pushed")
	commit := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	before := d.RemoteBranches()

	w := only(t, d, d.Check(context.Background(), prefix, target))
	if w.Status != rescue.Failed || len(w.Branches) != 0 || !strings.Contains(w.Error, "LEAK CHECK: refusing") {
		t.Fatalf("the check reported %+v, want the guard's refusal and no branch", w)
	}
	if held := d.RemoteBranch(target); held != "" {
		t.Errorf("the remote holds %s at %s, want no such branch", target, held)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}

	d.LeakCheck(rescuetest.Passes)
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit)
}

// The guard's call keeps a reserve of the workspace's bound for the push it clears, and that
// reserve now holds two things: the push itself, and the second run of the shared check that a
// pod's pre-push hook makes inside it (GuardReserve, 150 s, and the gitPins comment for why the
// hook runs at all). A workspace bound that cannot hold the reserve is refused before the guard
// runs at all, with nothing pushed — the fail-closed answer, and the reason a workspace's own
// bound is six minutes (WorkspaceTimeout, derived from the reserve). The minute a bound was
// here before is now such a bound, which is what this reads.
func TestAWorkspaceBoundUnderTheGuardsReserveIsRefusedAndPushesNothing(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Write(d.Workspace, "feature.txt", "work a bound too small never carries\n")
	d.JJ("commit", "-m", "work a bound too small never carries")
	before := d.RemoteBranches()

	checked := rescue.Run(context.Background(), rescue.Request{
		StateDir: d.StateDir, Branch: prefix, Target: target, Env: d.Env,
		WorkspaceTimeout: time.Minute, Deadline: time.Now().Add(2 * time.Minute),
	})
	w := only(t, d, checked)
	if w.Status != rescue.Failed || len(w.Branches) != 0 || !strings.Contains(w.Error, "has no time left to check this push in") {
		t.Fatalf("the check reported %+v, want a refusal naming the bound and no branch", w)
	}
	if after := d.RemoteBranches(); !slices.Equal(after, before) {
		t.Errorf("the remote's branches are %v, want %v unchanged", after, before)
	}
}

// The fixture's next pod (rescuetest.Disk.NextPod) is a disk over the first disk's remote,
// provisioned from it as it stands, so its clone holds the branch the first disk's check pushed;
// and a stand-in check that records its updates (rescuetest.Disk.RecordUpdates) records the
// update line of the push it cleared. The lines are read, not counted: in a pod the push's own
// pre-push hook runs the same stand-in again.
func TestRescueFixtureNextPodSharesTheRemoteAndRecordsUpdates(t *testing.T) {
	d := rescuetest.New(t, issue)
	updates := d.RecordUpdates()
	d.Write(d.Workspace, "fix.txt", "a fix made with plain git\n")
	d.Git("add", "fix.txt")
	d.Git("commit", "--quiet", "-m", "a fix made with plain git")
	commit := d.Git("rev-parse", "HEAD")
	tip := rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), commit).Commit
	if recorded := updates(); !updateLine(recorded, tip, zeroSHA) {
		t.Errorf("the stand-in check recorded %q, want an update line moving refs/heads/%s to %s", recorded, target, tip)
	}

	next := d.NextPod(t)
	if next.Remote != d.Remote || next.StateDir == d.StateDir || next.Clone == d.Clone || next.Workspace == d.Workspace {
		t.Fatalf("the next pod's disk is %s over %s, want a disk other than %s over the same remote %s", next.StateDir, next.Remote, d.StateDir, d.Remote)
	}
	if held := next.Run(next.Clone, "git", "rev-parse", "refs/remotes/origin/"+target); held != tip {
		t.Errorf("the next pod's clone holds origin/%s at %s, want %s, which the first disk's check pushed", target, held, tip)
	}
	if head := next.Git("rev-parse", "HEAD"); head != d.Base {
		t.Errorf("the next pod's workspace stands on %s, want the remote's main %s", head, d.Base)
	}
}
