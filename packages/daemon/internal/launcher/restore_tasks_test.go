package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// bundleTasks are the branches of the two tasks whose working copies sit beside the released
// issue's own: the first give of one slug, and a later give of another, whose part after the key
// holds a dot of its own and is read whole.
var bundleTasks = []string{"legion/" + bundleIssue + ".d", "legion/" + bundleIssue + ".b.2"}

// A released pod's save names the working copy of each task beside the issue's own, by the
// task's branch, at the commit that holds the edit its agent left there — an edit no jj command
// had snapshotted — and the bundle carries each of those commits. The issue's own working copy
// is the save's Head, as it always was, and is no task's.
func TestTheSaveNamesTheWorkingCopyOfEachTask(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newTaskBundleFixture(t)
	saved := f.bundleWorkspaceOne()

	if len(saved.WorkingCopies) != len(bundleTasks) {
		t.Fatalf("the save names the working copies %v, want one for each of %v and none for the issue's own", saved.WorkingCopies, bundleTasks)
	}
	heads := f.bundleHeads(saved)
	for _, branch := range bundleTasks {
		dir := f.taskDir(f.ws, branch)
		at := f.run(dir, "jj", "log", "-R", dir, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id")
		if saved.WorkingCopies[branch] != at {
			t.Fatalf("the save names the working copy of %s as %q, want %s", branch, saved.WorkingCopies[branch], at)
		}
		if got := f.run(dir, "git", "--git-dir="+f.gitDir(dir), "show", at+":"+taskFile(branch)); got+"\n" != taskEdit(branch) {
			t.Fatalf("the working copy of %s was saved on %s, which holds %q, not its agent's edit", branch, at, got)
		}
		if !slices.Contains(heads, at) {
			t.Fatalf("the bundle carries the heads %v, not the working copy of %s on %s", heads, branch, at)
		}
	}
}

// The pod that replaces the released one puts each task's working copy back beside the issue's
// own before any role starts: at the directory TaskDir names, a workspace of the pod's clone on
// the commit the save named, holding its agent's edit, its git worktree entry locked by the
// restore as provisioning locks one, and named in the restore's notice. The issue's own working
// copy is restored as it always was.
func TestTheRestorePutsEachTasksWorkingCopyBack(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newTaskBundleFixture(t)
	saved := f.bundleWorkspaceOne()

	ws := f.secondPod("ws2")
	jjLeavesAddsUnlocked(t)
	notice := f.restoreInto(ws, saved, 1)
	f.requireRestored(ws, saved)
	f.requireTasksRestored(ws, saved, notice)
}

// The daemon keeps a release's save in its store until the pod that replaces the released one
// takes it (store.PutWorkspaceBundle, store.TakeWorkspaceBundleForRestore), and the working copy
// of each task crosses the store with the rest: the shim's answer becomes the daemon's bundle and
// the daemon's bundle the launcher's through the one conversion each way the daemon's own path
// takes (runtime.BundleOf, runtime.WorkspaceBundle.Wire), and the restore puts each task's
// working copy back on its commit. A save a shim of the release before made names no working copy
// of a task, and its restore is the issue's working copy alone, as before.
func TestARestoreThroughTheStorePutsBackEachWorkingCopyTheSaveNamed(t *testing.T) {
	testbin.RequireJJFilters(t)
	for _, tc := range []struct {
		name  string
		tasks bool
	}{
		{name: "a save that names the working copies of two tasks", tasks: true},
		{name: "a save of a shim of the release before, which names no working copy of a task", tasks: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := testpg.CloneStore(t, "legion_launcher_restore")
			f := newTaskBundleFixture(t)
			answered := f.bundleWorkspaceOne()
			if !tc.tasks {
				answered.WorkingCopies = nil
			}
			saved := runtime.BundleOf(answered)
			saved.PodUID = "pod-the-release-took-the-work-off"
			if err := st.PutWorkspaceBundle(ctx, bundleIssue, bundleRelease, saved); err != nil {
				t.Fatalf("save the released workspace's bundle: %v", err)
			}
			// A bundle is a project's to take only where the project records its issue.
			if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ($1, $1, 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, bundleIssue); err != nil {
				t.Fatalf("record %s: %v", bundleIssue, err)
			}
			taken, take, err := st.TakeWorkspaceBundleForRestore(ctx, "infra", bundleIssue, "pod-that-replaces-it", time.Now().Add(time.Minute))
			if err != nil || take != runtime.WorkspaceBundleSend {
				t.Fatalf("the replacement pod's take answered %v (%v), want the bundle sent", take, err)
			}

			ws := f.secondPod("ws2")
			jjLeavesAddsUnlocked(t)
			notice := f.restoreInto(ws, taken.Wire(), 1)
			f.requireRestored(ws, answered)
			if tc.tasks {
				f.requireTasksRestored(ws, answered, notice)
				return
			}
			for _, branch := range bundleTasks {
				if dir := f.taskDir(ws, branch); exists(dir) {
					t.Fatalf("the restore of a save that named no working copy of a task made %s", dir)
				}
			}
			if strings.Contains(notice, "tasks") {
				t.Fatalf("the restore of a save that named no working copy of a task wrote %q", notice)
			}
		})
	}
}

// exists is whether anything is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// newTaskBundleFixture is the bundle fixture whose released pod is laid out as provisioning lays
// one out (workspace.Location), which is where a task's working copy sits beside its issue's
// (workspace.TaskDir): the issue's own unpushed work (workOne), and a working copy of the pod's
// clone for each of bundleTasks, each holding an edit no jj command has snapshotted.
func newTaskBundleFixture(t *testing.T) *bundleFixture {
	t.Helper()
	f := newBundleFixture(t)
	f.ws = f.secondPod("released")
	f.workOne()
	for _, branch := range bundleTasks {
		dir := f.taskDir(f.ws, branch)
		f.run(f.ws, "jj", "workspace", "add", "-R", f.ws, "--name", filepath.Base(dir), "--revision", "trunk()", dir)
		f.write(filepath.Join(dir, taskFile(branch)), taskEdit(branch))
	}
	return f
}

// requireTasksRestored fails the test unless each task's working copy of saved is back beside
// the issue's working copy ws, on its saved commit, with its edit, its git worktree entry locked,
// and its branch named in the restore's notice. The restore it reads ran behind
// jjLeavesAddsUnlocked, so the lock is the restore's own.
func (f *bundleFixture) requireTasksRestored(ws string, saved shimwire.WorkspaceBundle, notice string) {
	f.t.Helper()
	worktrees := f.run(ws, "git", "--git-dir="+f.gitDir(ws), "worktree", "list", "--porcelain")
	for _, branch := range bundleTasks {
		dir := f.taskDir(ws, branch)
		if _, err := os.Stat(dir); err != nil {
			f.t.Fatalf("the restore put no working copy of %s back at %s (%v); its notice: %s", branch, dir, err, notice)
		}
		if head := f.run(dir, "jj", "log", "-R", dir, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id"); head != saved.WorkingCopies[branch] {
			f.t.Fatalf("the restored working copy of %s is on %s, want the saved %s", branch, head, saved.WorkingCopies[branch])
		}
		if body, err := os.ReadFile(filepath.Join(dir, taskFile(branch))); err != nil || string(body) != taskEdit(branch) {
			f.t.Fatalf("the restored working copy of %s reads %q (%v), want its agent's edit", branch, body, err)
		}
		if !lockedWorktree(worktrees, dir) {
			f.t.Fatalf("the git worktree entry of the restored working copy %s is not locked:\n%s", dir, worktrees)
		}
		if !strings.Contains(notice, branch) {
			f.t.Fatalf("the restore's notice names no working copy of %s: %s", branch, notice)
		}
	}
}

// lockedWorktree is whether `git worktree list --porcelain` lists dir's entry as locked.
func lockedWorktree(porcelain, dir string) bool {
	for _, block := range strings.Split(porcelain, "\n\n") {
		lines := strings.Split(block, "\n")
		if lines[0] == "worktree "+dir {
			return slices.ContainsFunc(lines, func(line string) bool { return line == "locked" || strings.HasPrefix(line, "locked ") })
		}
	}
	return false
}

// jjLeavesAddsUnlocked puts a jj first on the process's PATH, where the restore looks its jj up
// (workspace.RunCommand), that runs the jj after it and then unlocks the git worktree entry of the
// workspace each `jj workspace add` made. A jj build or wrapper can lock that entry on add, and the
// restore's own lock (workspace.LockGitWorktree) keeps a lock it finds, so behind such a jj a
// working copy reads as locked whether the restore locked it or not. Behind this one, a lock the
// row reads after the restore is the restore's own. A failed unlock fails the add, and with it the
// put-back of that working copy, which the row reads too.
func jjLeavesAddsUnlocked(t *testing.T) {
	t.Helper()
	path := os.Getenv("PATH")
	if strings.Contains(path, "'") {
		t.Fatalf("PATH=%s holds a quote the stand-in jj cannot carry", path)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"PATH='" + path + "' jj \"$@\" || exit\n" +
		"case \" $* \" in *\" workspace add \"*) ;; *) exit 0 ;; esac\n" +
		"for added; do :; done\n" +
		"[ -f \"$added/.git\" ] || exit 0\n" +
		"out=$(PATH='" + path + "' git -C \"$added\" worktree unlock \"$added\" 2>&1) && exit 0\n" +
		"case \"$out\" in *\"is not locked\"*) exit 0 ;; esac\n" +
		"echo \"$out\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "jj"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+path)
}

// taskDir is the directory of the working copy of the task whose branch is branch, beside the
// issue's working copy ws.
func (f *bundleFixture) taskDir(ws, branch string) string {
	f.t.Helper()
	dir, err := workspace.TaskDir(ws, branch)
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// bundleHeads is the commit of each head saved's bundle carries, as git reads them.
func (f *bundleFixture) bundleHeads(saved shimwire.WorkspaceBundle) []string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "saved.bundle")
	f.write(path, string(saved.Bundle))
	var heads []string
	for _, line := range strings.Split(f.run(f.ws, "git", "--git-dir="+f.gitDir(f.ws), "bundle", "list-heads", path), "\n") {
		commit, _, _ := strings.Cut(line, " ")
		heads = append(heads, commit)
	}
	return heads
}

// taskFile is the file a task's agent edits in its working copy, and taskEdit what it writes.
func taskFile(branch string) string { return strings.TrimPrefix(branch, "legion/") + ".txt" }

func taskEdit(branch string) string { return "the work of " + branch + "\n" }
