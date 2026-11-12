package launcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// The two rows of this file are the sweep end to end, driven with the real git and jj a role runs:
// a workspace jj refuses to work recovers because the sweep removed the lock, and the same
// workspace whose lock a live process holds stays refused. Every other row of this package plants
// a lock beside a clone's bookkeeping and reads what the pass decided about that file; these two
// read what a role's own tool does before and after the pass, which is the whole of what a wedged
// clone costs an issue — a jj that answers nothing but "Could not acquire lock for index file"
// until someone removes the file, which in a running pod is this sweep and nothing else.
//
// The clone is built the way an issue pod's `workspace-init provision` builds one, out of the
// commands that provisioning itself runs (workspace.feedClone, workspace.createWorkspace and
// Provision's checkout, with fixturegit holding the init argv): a bare feed, `git clone
// --no-checkout`, `jj git init --colocate` with the clone's sparse patterns cleared and its first
// snapshot abandoned, then `jj workspace add --sparse-patterns empty` and the workspace's own `jj
// sparse reset`. A shorter fixture would prove nothing about the file jj takes, because the lock
// these rows plant is the index of this workspace's own git worktree entry, which exists only in a
// colocated clone that has a workspace added to it.
//
// Neither row calls t.Parallel, as no row of this package may (gitlocks_test.go).

// jjWorkspaceName is the name the workspace is added under, which is also the base name of its
// directory: `jj workspace add --name` names the jj workspace, and git names the worktree entry
// after the directory, so the two agree here as they do in a pod, where both are the issue's key.
const jjWorkspaceName = "ws"

// gitIndexLockRefusal is what jj prints when the index lock of the workspace's worktree entry is
// already there: git's own words for a lock it could not take, which jj answers exit 1 on after
// retrying for a second. It is the message LEGION-731 wedged every provisioning of an issue's pods
// with, and the one a role's jj has answered ever since whenever a lock outlives its writer.
const gitIndexLockRefusal = "Could not acquire lock for index file"

// jjClone is a lockClone whose files the real git and jj wrote, with the runner that wrote them.
// The clone, its worktree entry and the workspace sit where a pod has them, so every helper of
// gitlocks_test.go — the plant, the sweep over the row's own /proc, the require of a removal or of
// a keep — reads this clone exactly as it reads a planted one.
type jjClone struct {
	*lockClone
	run fixturegit.Runner
}

// newJJClone builds that clone. A missing git or jj fails the row rather than skipping it: both
// are in this pod's worker image and in CI (.github/ci-tools.yaml pins jj at the version this
// machine runs, and .github/actions/install-jj installs it), so a skip here would report a green
// run for a sweep nothing had driven.
func newJJClone(t *testing.T) *jjClone {
	t.Helper()
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("this row drives the real %s, which the worker image and CI both install: %v", tool, err)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing of the process that runs the tests reaches these commands: no workspace, no
	// repository, no remote, no credential and no configuration but a home of this row's own
	// (fixtureenv.Isolated, docs/testing.md). Each of them names the directory it runs in for the
	// same reason, since jj searches upward from the directory it is started in for the closest
	// .jj and a command with none would open the workspace the test process stands in.
	run := fixturegit.Isolated(t, home)
	feed, seed := filepath.Join(root, "feed"), filepath.Join(root, "seed")
	clone, ws := filepath.Join(root, "clone"), filepath.Join(root, jjWorkspaceName)
	// The feed is the bare repository a pod's workspace-fetch leaves on the issue's volume, with
	// one commit on the default branch, which is what the clone below is made from.
	run.Run(root, fixturegit.InitBare(feed, "main")...)
	run.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run.Run(seed, "git", "add", "base.txt")
	run.Run(seed, "git", "commit", "--quiet", "-m", "base")
	run.Run(seed, "git", "push", "--quiet", feed, "main")
	// The pod's shared clone, in provisioning's own order, which is load-bearing (workspace's
	// feedClone says why): the checkout-less clone, jj's import of every commit, the sparse clear
	// that leaves the clone no working tree, and the abandon of the snapshot `jj git init` took.
	run.Run(root, "git", "clone", "--quiet", "--no-checkout", feed, clone)
	run.Run(clone, fixturegit.JJInit(clone, true)...)
	run.Run(clone, "jj", "sparse", "set", "--clear", "--color=never", "-R", clone)
	run.Run(clone, "jj", "abandon", "@", "--color=never", "-R", clone)
	// The issue's workspace: registered with no file, as provisioning registers it under the
	// clone's store lock, and checked out after, which is what gives it a working copy to
	// snapshot and the worktree entry an index of its own.
	run.Run(clone, "jj", "workspace", "add", ws, "--name", jjWorkspaceName, "--revision", "main", "--sparse-patterns", "empty", "-R", clone)
	run.Run(ws, "jj", "sparse", "reset")
	// git names a worktree entry after the directory's base name, with a number on a collision, so
	// the entry this row plants its lock in is read rather than derived (gitDirWorktreeEntries
	// reads it the same way).
	admin := filepath.Join(clone, ".git", "worktrees")
	entries, err := os.ReadDir(admin)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the clone's git worktree entries after one `jj workspace add` = %v (%v), want exactly the workspace's own", entries, err)
	}
	return &jjClone{
		lockClone: &lockClone{
			t: t, dir: ws, clone: clone,
			gitDir:   filepath.Join(clone, ".git"),
			worktree: filepath.Join(admin, entries[0].Name()),
			repo:     filepath.Join(clone, ".jj", "repo"),
		},
		run: run,
	}
}

// blocked fails the row unless jj refuses the workspace on the index lock, and answers what it
// printed: exit 1 carrying git's own words. The whole output is answered rather than matched,
// because it is what an operator reads in a pod and what this change exists to end, and each row
// logs it (`go test -v`).
func (c *jjClone) blocked(when string) string {
	c.t.Helper()
	out, err := c.run.Try(c.dir, "jj", "status", "--color=never")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(out, gitIndexLockRefusal) {
		c.t.Fatalf("jj status %s: %v\n%s\nwant exit 1 holding %q", when, err, out, gitIndexLockRefusal)
	}
	return out
}

// worked is the other half: jj runs the workspace and answers 0, which is what a role gets back
// once the lock is gone.
func (c *jjClone) worked(when string) string {
	c.t.Helper()
	out, err := c.run.Try(c.dir, "jj", "status", "--color=never")
	if err != nil {
		c.t.Fatalf("jj status %s: %v\n%s\nwant it to work", when, err, out)
	}
	return out
}

// holdsOpen starts a live child process holding lock open — the writer a sweep must never take a
// lock from — and answers its pid, which is what the row's /proc root carries. The child is a
// `sleep` handed the open descriptor as its one extra file, so what it holds is that file by
// device and inode, which is how the holder read compares it, and not a path it could be made to
// reopen; the row's own descriptor is closed at once, so the one holder in the container's /proc
// is that child. A child whose comm is neither git nor jj is deliberate: it keeps the lock through
// the condition this row is about, a process of the container holding it open, and not through
// the blind read a git the sweep cannot read would take (TestAClockPassKeepsALockWhoseGitHolderItCannotRead).
func holdsOpen(t *testing.T, lock string) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("this row holds the lock open in a live process: %v", err)
	}
	file, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Longer than any pass of this row and killed when the row ends, so nothing of it outlives the
	// test binary.
	holder := exec.Command(sleep, "600")
	holder.ExtraFiles = []*os.File{file}
	if err := holder.Start(); err != nil {
		t.Fatalf("start the process that holds %s open: %v", lock, err)
	}
	t.Cleanup(func() {
		if err := holder.Process.Kill(); err != nil {
			t.Error(err)
		}
		_ = holder.Wait()
	})
	return holder.Process.Pid
}

// A stale index.lock in the issue workspace's own git worktree entry is what wedges a pod's
// clone: jj takes that lock to write the workspace's git index whenever it has something to
// snapshot, and a lock left behind by a killed writer makes every later jj of that workspace fail
// after a second of retries. Nothing but this sweep removes such a lock while the pod runs — the
// pod's `workspace-init provision` ran before any container started — so a clock pass is the whole
// of the issue's recovery, and this row reads it the way a role does: jj refuses the workspace,
// one pass runs, jj works again.
//
// jj is read before the lock's own removal is, which is this row's point: a pass that removed
// nothing fails here on jj's real refusal rather than on a missing file.
func TestAStaleIndexLockBlocksJJUntilTheSweepRemovesIt(t *testing.T) {
	c := newJJClone(t)
	// jj takes the index lock only when it has something to snapshot, so the workspace is given a
	// changed file first: measured against the jj this machine runs (0.45.1-sami), with this lock
	// in the worktree entry and a clean working copy `jj status` exits 0, and with one changed
	// file it exits 1.
	c.write(filepath.Join(c.dir, "work.txt"), "a role's unsnapshotted work\n")
	// Zero bytes, as git's lockfile API leaves one it never got to write, and last written an hour
	// ago, which is past the sweep's 15-minute guard.
	lock := c.worktreeLock("index.lock", "", time.Hour)
	t.Logf("jj status with the stale lock:\n%s", c.blocked("with the stale lock in the workspace's worktree entry"))

	// One clock pass, the pass a pod runs every minute of its life, over a /proc root holding the
	// one process this row has: the test binary, which holds no descriptor of the lock. The sweep
	// its clone answers carries a millisecond's recheck, so the second read of the lock costs the
	// row nothing (lockClone.sweep).
	log := &lockedBuffer{}
	c.sweep(log, procRoot(t, os.Getpid())).pass(context.Background(), false)

	t.Logf("jj status after the sweep's clock pass:\n%s", c.worked("after the sweep's clock pass"))
	requireRemoved(t, log, lock)
}

// The control of the row above: the same workspace, the same stale lock, and a live process
// holding that lock open. One clock pass removes nothing, the file is where it was, and jj stays
// refused — which is what makes the removal above the sweep's own decision rather than this test
// deleting a file. A lock some process still holds must never be removed, whatever its age: two
// writers would then replace one file, and the loser's write would be the one the clone kept.
func TestAHeldIndexLockStaysAndJJStaysBlocked(t *testing.T) {
	c := newJJClone(t)
	c.write(filepath.Join(c.dir, "work.txt"), "a role's unsnapshotted work\n")
	lock := c.worktreeLock("index.lock", "", time.Hour)
	holder := holdsOpen(t, lock)
	t.Logf("jj status with the held lock:\n%s", c.blocked("with the held lock in the workspace's worktree entry"))

	log := &lockedBuffer{}
	c.sweep(log, procRoot(t, holder)).pass(context.Background(), false)

	requireKept(t, log, lock, "a process of the container holds it open")
	t.Logf("jj status after the sweep's clock pass:\n%s", c.blocked("after the sweep's clock pass"))
}
