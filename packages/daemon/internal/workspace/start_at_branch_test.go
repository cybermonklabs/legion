package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// nextBranch is the issue branch of every StartAtBranch test.
const nextBranch = "legion/ACME-1"

// branchFixture is a remote and a role's workspace laid out as provisioning lays them out: a
// colocated clone that tracks the remote's main and the issue's branch and keeps unreachable
// commits, and the issue's workspace added to it, whose working copy stands on the issue's pushed
// commit O and holds a change of its own. The seed is the repository that writes the remote, as
// GitHub's merge and the daemon's move of the branch write it. Every command is the real git or jj
// under the fixture's own home.
type branchFixture struct {
	t                               *testing.T
	jj                              string
	run                             fixturegit.Runner
	root, home, remote, seed, clone string
	ws                              string
}

func newBranchFixture(t *testing.T) *branchFixture {
	t.Helper()
	root := t.TempDir()
	f := &branchFixture{
		t: t, jj: fixturegit.CompiledJJ(t), root: root, home: filepath.Join(root, "home"),
		remote: filepath.Join(root, "remote.git"), seed: filepath.Join(root, "seed"),
		clone: filepath.Join(root, "clone"), ws: filepath.Join(root, "ws"),
	}
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}
	f.run = fixturegit.Isolated(t, f.home)
	f.run.Run(root, fixturegit.InitBare(f.remote, "main")...)
	f.run.Run(root, fixturegit.InitWorkTree(f.seed, "main")...)
	f.writeIn(f.seed, "base.txt")
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", "base")
	f.git("remote", "add", "origin", f.remote)
	f.git("push", "--quiet", "-u", "origin", "main")
	f.git("switch", "--quiet", "-c", nextBranch)
	f.writeIn(f.seed, "feature.txt")
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", "feat: the change")
	f.git("push", "--quiet", "origin", nextBranch)
	f.git("switch", "--quiet", "main")

	f.run.Run(root, "git", "clone", "--quiet", "--no-local", f.remote, f.clone)
	f.run.Run(f.clone, append([]string{f.jj}, fixturegit.JJInit(f.clone, true)[1:]...)...)
	f.run.Run(f.clone, f.jj, "config", "set", "-R", f.clone, "--repo", "git.abandon-unreachable-commits", "false")
	f.run.Run(f.clone, f.jj, "bookmark", "track", "-R", f.clone, "main@origin", nextBranch+"@origin")
	f.run.Run(f.clone, f.jj, "workspace", "add", "-R", f.clone, "--name", filepath.Base(f.ws), f.ws)
	f.inWorkspace("new", nextBranch)
	f.writeIn(f.ws, "notes.txt")
	return f
}

// squashMerge is GitHub's squash merge of the branch into main, then the daemon's move of the
// branch to main's new head, which does not descend from the merged head. It answers main's head.
func (f *branchFixture) squashMerge() string {
	f.t.Helper()
	f.writeIn(f.seed, "feature.txt")
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", "feat: the change (#1)")
	f.git("push", "--quiet", "origin", "main")
	f.git("push", "--quiet", "--force", "origin", "main:refs/heads/"+nextBranch)
	return f.git("rev-parse", "HEAD")
}

// advanceMain is a later merge of another issue into main, which leaves the branch where it is.
func (f *branchFixture) advanceMain() string {
	f.t.Helper()
	f.writeIn(f.seed, "other.txt")
	f.git("add", "-A")
	f.git("commit", "--quiet", "-m", "feat: another issue's change (#2)")
	f.git("push", "--quiet", "origin", "main")
	return f.git("rev-parse", "HEAD")
}

// start is StartAtBranch in the fixture's workspace, as the shim runs it: RunCommand in the
// workspace, under the fixture's own environment.
func (f *branchFixture) start() (StartedAt, error) {
	f.t.Helper()
	env := fixtureenv.Isolated(f.home)
	return StartAtBranch(func(args ...string) (string, error) {
		return RunCommand(context.Background(), procgroup.NoReaper, time.Minute, time.Now(), env, f.ws, nil, append([]string{f.jj}, args...))
	}, f.ws, nextBranch, "main")
}

// read is the commit ids revset names in the workspace, read without a snapshot, so it is the
// state StartAtBranch left.
func (f *branchFixture) read(revset string) string {
	f.t.Helper()
	return f.inWorkspace("log", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy")
}

// workingCopy is the working copy's commit, read with a snapshot, as a role's own jj reads it.
func (f *branchFixture) workingCopy() string {
	f.t.Helper()
	return f.inWorkspace("log", "-r", "@", "--no-graph", "-T", "commit_id")
}

func (f *branchFixture) inWorkspace(args ...string) string {
	f.t.Helper()
	return f.run.Run(f.ws, append(append([]string{f.jj}, args...), "-R", f.ws)...)
}

func (f *branchFixture) git(args ...string) string {
	f.t.Helper()
	return f.run.Run(f.seed, append([]string{"git"}, args...)...)
}

func (f *branchFixture) writeIn(dir, name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// After a squash merge, the working copy stands on the merged head O, and origin's branch moved to
// main's head M. StartAtBranch fetches the branch, which moves the tracked local bookmark to M, and
// starts a new working copy on M. The old working copy holds the role's change, so it stays a
// visible commit, and the answer names it. A working copy that holds nothing moves the same way.
func TestStartAtBranchMovesTheWorkingCopyToTheMovedBranch(t *testing.T) {
	t.Parallel()
	f := newBranchFixture(t)
	old := f.workingCopy()
	merged := f.squashMerge()

	started, err := f.start()
	if err != nil {
		t.Fatalf("StartAtBranch: %v", err)
	}
	if want := (StartedAt{Moved: true, From: old, At: merged}); started != want {
		t.Fatalf("StartAtBranch = %+v, want %+v", started, want)
	}
	if got := f.read("parents(@)"); got != merged {
		t.Fatalf("the working copy's parents are %q, want main's head %s", got, merged)
	}
	if got := f.read(nextBranch); got != merged {
		t.Fatalf("the local bookmark %s is at %q, want origin's %s: the fetch moves a tracked bookmark that did not move", nextBranch, got, merged)
	}
	if got := f.read("visible_heads() & " + old); got != old {
		t.Fatalf("the old working copy %s is not a visible head (visible_heads() & it is %q): its change is lost", old, got)
	}

	t.Run("an empty working copy", func(t *testing.T) {
		t.Parallel()
		f := newBranchFixture(t)
		f.inWorkspace("commit", "-m", "feat: the role's own commit")
		old := f.workingCopy()
		merged := f.squashMerge()
		started, err := f.start()
		if err != nil {
			t.Fatalf("StartAtBranch: %v", err)
		}
		if !started.Moved || started.From != old || started.At != merged || f.read("parents(@)") != merged {
			t.Fatalf("StartAtBranch = %+v with the working copy's parents %q, want a move from %s to %s", started, f.read("parents(@)"), old, merged)
		}
	})
}

// A workspace whose working copy already stands on the branch's commit is left as it stands: the
// second call of one next change, and a fresh workspace, which provisioning starts at the bookmark.
// No commit is made, and the working copy keeps its commit.
func TestStartAtBranchChangesNothingWhereTheWorkspaceIsAtTheBranch(t *testing.T) {
	t.Parallel()
	f := newBranchFixture(t)
	merged := f.squashMerge()
	if _, err := f.start(); err != nil {
		t.Fatalf("the first StartAtBranch: %v", err)
	}
	at := f.workingCopy()
	heads := f.read("visible_heads()")

	started, err := f.start()
	if err != nil {
		t.Fatalf("the second StartAtBranch: %v", err)
	}
	if want := (StartedAt{From: at, At: merged}); started != want {
		t.Fatalf("the second StartAtBranch = %+v, want %+v", started, want)
	}
	if got := f.workingCopy(); got != at {
		t.Fatalf("the working copy is %s after the second call, want %s unchanged", got, at)
	}
	if got := f.read("visible_heads()"); got != heads {
		t.Fatalf("the visible heads are %q after the second call, want %q: it made a commit", got, heads)
	}
}

// A base that moved on after the branch moved, by a later merge of another issue, holds commits the
// branch does not. The answer says so, so the role's task tells it to merge the base forward first,
// and the working copy still stands where the branch is.
func TestStartAtBranchSaysWhereTheBaseMovedOnSince(t *testing.T) {
	t.Parallel()
	f := newBranchFixture(t)
	merged := f.squashMerge()
	if started, err := f.start(); err != nil || started.BehindBase {
		t.Fatalf("StartAtBranch = %+v, %v; want a move with the branch at main's head", started, err)
	}
	f.advanceMain()
	started, err := f.start()
	if err != nil {
		t.Fatalf("StartAtBranch: %v", err)
	}
	if started.Moved || started.At != merged || !started.BehindBase {
		t.Fatalf("StartAtBranch = %+v, want the working copy left at %s and the base named ahead of it", started, merged)
	}
}

// A local bookmark that moved after its last push, while origin's moved too, is in conflict after
// the fetch. StartAtBranch refuses with the conflict's sides and ways out, and the working copy has
// not moved: the local commit is a role's work no push ever reached.
func TestStartAtBranchRefusesALocalBookmarkThatNeverPushed(t *testing.T) {
	t.Parallel()
	f := newBranchFixture(t)
	f.inWorkspace("commit", "-m", "feat: a commit that was never pushed")
	f.inWorkspace("bookmark", "set", nextBranch, "-r", "@-")
	unpushed := f.read("@-")
	before := f.workingCopy()
	f.squashMerge()

	_, err := f.start()
	if err == nil {
		t.Fatal("StartAtBranch moved a working copy whose bookmark is in conflict")
	}
	for _, want := range []string{"Bookmark " + nextBranch + " is conflicted.", unpushed, "did not move", "--allow-backwards"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if got := f.workingCopy(); got != before {
		t.Fatalf("the working copy is %s after the refusal, want %s", got, before)
	}
	if got := f.read("parents(@)"); got != unpushed {
		t.Fatalf("the working copy's parents are %q after the refusal, want the unpushed commit %s", got, unpushed)
	}
}
