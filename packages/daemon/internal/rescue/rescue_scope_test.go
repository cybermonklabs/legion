package rescue_test

// What the push check takes as the workspace's own. A pod's shared clone keeps every commit it
// ever fetched visible to jj (workspace-init sets git.abandon-unreachable-commits false), so its
// visible heads hold other trees' handoffs and the pre-rebase copies of branches origin rewrote or
// deleted. The issue's own work is the ancestors of the workspace's working-copy commit and of the
// issue's bookmark, the issue's git branch and the workspace's git HEAD: that is pushed, as the
// one rescue branch of the issue. Nothing else is, wherever it came from and whatever its dates
// say: not a commit the clone fetched, not a side line a role left off its own line, and not
// another local branch.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// visibleHeads is every head jj can see in the clone, as the pod's roles see it.
func visibleHeads(t *testing.T, d *rescuetest.Disk) []string {
	t.Helper()
	return strings.Fields(d.Run(d.Clone, "jj", "log", "--no-graph", "--ignore-working-copy", "-r", "visible_heads()", "-T", `commit_id ++ "\n"`))
}

// foreignBranch pushes another tree's branch to origin, one commit on the base that someone else
// made, and returns the commit. Run in one of rescuetest.New's before, it is a branch the pod's
// clone holds from the start.
func foreignBranch(d *rescuetest.Disk, branch string) string {
	side := filepath.Join(filepath.Dir(d.Remote), "side-"+branch)
	d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
	d.Write(side, branch+".txt", branch+"\n")
	d.Run(side, "git", "add", branch+".txt")
	d.Run(side, "git", "commit", "--quiet", "-m", "another tree's "+branch)
	d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return d.Run(side, "git", "rev-parse", "HEAD")
}

// A local bookmark off the workspace's own line that tracks another tree's branch, which origin
// deleted after the pod last fetched it, still names that branch's commit, and so does the clone's
// remote-tracking ref of it. That commit is not rescued; the workspace's own work beside it is, and
// alone.
func TestATrackedBookmarkWhoseBranchOriginDeletedIsNotRescued(t *testing.T) {
	var foreign string
	d := rescuetest.New(t, issue, func(d *rescuetest.Disk) { foreign = foreignBranch(d, "other") })
	d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
	d.JJ("bookmark", "track", "other", "--remote", "origin")
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", "other")
	if !slices.Contains(visibleHeads(t, d), foreign) {
		t.Fatalf("the fetched commit %s is not a visible head of the clone (%v): the case does not stand", foreign, visibleHeads(t, d))
	}
	if held := d.RemoteBranch("other"); held == foreign {
		t.Fatalf("the remote still holds %s on other: the case does not stand", foreign)
	}

	d.Write(d.Workspace, "feature.txt", "the workspace's own work\n")
	d.JJ("commit", "-m", "the workspace's own work")
	own := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")

	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), own)
	if d.RemoteReaches(foreign) {
		t.Errorf("the remote holds the fetched %s, which is no work of the issue's", foreign)
	}
}

// A side line a role made in the pod and left, its working copy moved back onto the issue's
// bookmark since, is on neither the working copy's line nor the bookmark's: nothing is pushed. The
// worker's own push before it answers a round is what keeps such work.
func TestASideLineOffTheWorkspacesOwnLineIsNotRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("new", "-m", "an experiment off the base", d.Base)
	d.Write(d.Workspace, "experiment.txt", "an experiment\n")
	side := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
	d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
	offBothLines(t, d, side)
	if head := d.Git("rev-parse", "HEAD"); head == side {
		t.Fatalf("the workspace's git HEAD is the side line %s: the case does not stand", side)
	}
	before := d.RemoteBranches()
	pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, side)
}

// A commit someone else made, which the pod fetched while it ran and origin deleted since, is no
// work of the issue's: nothing is pushed.
func TestACommitThePodFetchedAndOriginDeletedSinceIsNotRescued(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
	side := filepath.Join(filepath.Dir(d.Remote), "side")
	d.Run(filepath.Dir(d.Remote), "git", "clone", "--quiet", d.Remote, side)
	d.Write(side, "later.txt", "fetched while the pod ran\n")
	d.Run(side, "git", "add", "later.txt")
	d.Run(side, "git", "commit", "--quiet", "-m", "fetched while the pod ran")
	later := d.Run(side, "git", "rev-parse", "HEAD")
	d.Run(side, "git", "push", "--quiet", "origin", "HEAD:refs/heads/later")
	d.JJ("git", "fetch")
	d.Run(d.Remote, "git", "branch", "--quiet", "-D", "later")
	d.JJ("git", "fetch")
	if !slices.Contains(visibleHeads(t, d), later) {
		t.Fatalf("the fetched commit %s is not a visible head of the clone: the case does not stand", later)
	}
	before := d.RemoteBranches()
	pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, later)
}

// recreate creates the issue's workspace again in the live pod, in the order provisioning does
// (workspace.Remove, then workspace.createWorkspace): the workspace forgotten and its directory
// gone; then, with setAside, the issue's bookmark deleted; the workspace added again at revision,
// with its creation recorded anew; and, with setAside, the bookmark set on its working copy. The
// record is dated two seconds on, so everything made before the re-creation was made before the
// record at any clock's resolution.
func recreate(t *testing.T, d *rescuetest.Disk, revision string, setAside bool) {
	t.Helper()
	name := strings.ToLower(issue)
	d.Run(d.Clone, "jj", "workspace", "forget", name)
	if err := os.RemoveAll(d.Workspace); err != nil {
		t.Fatal(err)
	}
	if setAside {
		d.Run(d.Clone, "jj", "bookmark", "delete", `exact:"`+d.Branch+`"`)
	}
	d.Run(d.Clone, "jj", "workspace", "add", "--name", name, "--revision", revision, d.Workspace)
	if err := workspace.RecordCreated(d.Workspace, time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if setAside {
		d.JJ("bookmark", "set", d.Branch, "-r", "@")
	}
}

// offBothLines fails the test when commit is on the workspace's working-copy line or the issue's
// bookmark's: the case stands only where the commit is made in the pod and on neither.
func offBothLines(t *testing.T, d *rescuetest.Disk, commit string) {
	t.Helper()
	if on := d.JJ("log", "--no-graph", "-r", commit+` & ::(@ | bookmarks(exact:"`+d.Branch+`"))`, "-T", "commit_id"); on != "" {
		t.Fatalf("%s is on the workspace's own line: the case does not stand", commit)
	}
	if !slices.Contains(visibleHeads(t, d), commit) {
		t.Fatalf("%s is not a visible head of the clone (%v): the case does not stand", commit, visibleHeads(t, d))
	}
}

// Work a role made in the pod and left off the line of a workspace that was created again since is
// on neither the new working copy's line nor the issue's bookmark's: nothing is pushed.
// Plain: an unbookmarked commit, the working copy moved onto the issue's bookmark, then the
// workspace created again there. Set aside: the issue's bookmark moved onto a commit nobody
// described after its last push and GitHub deleted the branch, so the re-creation deletes the
// bookmark and starts at main, and the commit stays visible in the shared clone, recoverable by id.
func TestWorkLeftOffTheLineOfAWorkspaceCreatedAgainIsNotRescued(t *testing.T) {
	t.Run("created again on the issue's bookmark", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		d.JJ("new", "-m", "work before the re-creation", d.Base)
		d.Write(d.Workspace, "early.txt", "work before the re-creation\n")
		early := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
		d.JJ("new", `bookmarks(exact:"`+d.Branch+`")`)
		recreate(t, d, `bookmarks(exact:"`+d.Branch+`")`, false)
		offBothLines(t, d, early)
		before := d.RemoteBranches()
		pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, early)
	})
	t.Run("created again at main, the issue's moved bookmark set aside", func(t *testing.T) {
		d := rescuetest.New(t, issue)
		d.Run(d.Clone, "jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false")
		d.Write(d.Workspace, "pushed.txt", "pushed work\n")
		d.JJ("commit", "-m", "pushed work")
		d.JJ("bookmark", "set", d.Branch, "-r", "@-")
		d.JJ("git", "push", "--bookmark", d.Branch)
		d.Write(d.Workspace, "undescribed.txt", "work nobody described\n")
		d.JJ("bookmark", "set", d.Branch, "-r", "@")
		moved := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id")
		d.JJ("new", "main")
		d.Run(d.Remote, "git", "branch", "--quiet", "-D", d.Branch)
		d.JJ("git", "fetch")
		if listed := d.JJ("bookmark", "list", "--conflicted", "-T", `name ++ "\n"`); !strings.Contains(listed, d.Branch) {
			t.Fatalf("the conflicted bookmarks are %q, want %s with one side a deletion: the case does not stand", listed, d.Branch)
		}
		recreate(t, d, "main", true)
		offBothLines(t, d, moved)
		before := d.RemoteBranches()
		pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, moved)
	})
}

// A workspace provisioning started at the default branch has the issue's bookmark on its working
// copy, an empty commit with no description, which jj exports to the issue's git branch. That
// commit holds no work, on git's side as on jj's, and the workspace is clean.
func TestTheIssuesBranchOnAnEmptyWorkingCopyIsClean(t *testing.T) {
	d := rescuetest.New(t, issue)
	d.JJ("new", "main")
	d.JJ("bookmark", "set", d.Branch, "-r", "@")
	if at, branch := d.JJ("log", "--no-graph", "-r", "@", "-T", "commit_id"), d.Run(d.Clone, "git", "rev-parse", "refs/heads/"+d.Branch); at != branch {
		t.Fatalf("the issue's git branch is %s, want the working copy %s: the case does not stand", branch, at)
	}
	if w := only(t, d, d.Check(context.Background(), prefix, target)); w.Status != rescue.Clean || len(w.Branches) != 0 {
		t.Fatalf("the check reported %+v, want clean: an empty working copy holds no work", w)
	}
}

// A commit git made in the workspace itself, whose git HEAD is detached so no branch names it, is
// a visible head and nothing more once jj moves the working copy onto the issue's bookmark or onto
// main: it is on neither the working copy's line nor the bookmark's, and nothing is pushed.
func TestACommitMadeInTheWorkspaceIsNotRescuedOnceTheWorkingCopyMovesOffIt(t *testing.T) {
	for _, moved := range []struct{ name, onto string }{
		{"git commit, then jj new on the issue's bookmark", `bookmarks(exact:"` + "legion/" + issue + `")`},
		{"git commit, then jj new on main", "main"},
	} {
		t.Run(moved.name, func(t *testing.T) {
			d := rescuetest.New(t, issue)
			d.Write(d.Workspace, "committed.txt", "work git committed\n")
			d.Git("add", "committed.txt")
			d.Git("commit", "--quiet", "-m", "work git committed")
			commit := d.Git("rev-parse", "HEAD")
			d.JJ("new", moved.onto)
			if branches := d.Run(d.Clone, "git", "for-each-ref", "--contains", commit, "--format=%(refname)", "refs/heads"); branches != "" {
				t.Fatalf("%s is on the local branches %q: the case does not stand", commit, branches)
			}
			offBothLines(t, d, commit)
			before := d.RemoteBranches()
			pushedNothing(t, d, only(t, d, d.Check(context.Background(), prefix, target)), before, commit)
		})
	}
}

// A graft (`git replace --graft F W`) makes a commit F that is not the pod's look as though the
// workspace's own work W were its parent: git, reading the replacement, takes W as held wherever F
// is, and F is on origin. The check reads no replacement object, and W is rescued.
func TestWorkAGraftMakesLookHeldIsRescued(t *testing.T) {
	var foreign string
	d := rescuetest.New(t, issue, func(d *rescuetest.Disk) { foreign = foreignBranch(d, "other") })
	d.Write(d.Workspace, "grafted.txt", "work a graft hides\n")
	d.JJ("commit", "-m", "work a graft hides")
	work := d.JJ("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.Run(d.Clone, "git", "replace", "--graft", foreign, work)
	if parent := d.Run(d.Clone, "git", "rev-parse", foreign+"^"); parent != work {
		t.Fatalf("git reads %s's parent as %s, want the grafted %s: the case does not stand", foreign, parent, work)
	}
	if held := d.RemoteBranch("other"); held != foreign {
		t.Fatalf("the remote holds other at %q, want %s: the case does not stand", held, foreign)
	}
	rescuedTo(t, d, only(t, d, d.Check(context.Background(), prefix, target)), work)
}
