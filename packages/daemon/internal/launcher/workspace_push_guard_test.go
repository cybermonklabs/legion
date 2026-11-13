package launcher

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The reviewer's loss, end to end against real git and jj, and the daemon's guard against it.
//
// A copy of a workspace is taken while its roles work and push, so a pod that replaces the one the
// copy came off can be sent a copy older than the issue's branch on GitHub. The launcher's restore
// then runs `jj edit` back to the copy's head and `jj bookmark set --allow-backwards` of the
// branch, and the replacement pod's next push removes the pushed commits from GitHub: the branch
// read `c2, c1, …` and then `c3, c1, …`. The daemon sends no copy older than the newest push of the
// issue's branch it recorded (runtime.DecideWorkspaceRestore, the decision the store's restore
// take makes in its transaction), and takes a copy of its own when it records the push, which is
// the copy a replacement pod is then sent.
//
// Each case runs the order the reviewer named — a running copy is taken, newer work is committed
// and pushed, the daemon records the push, the pod is replaced — with the real shim's bundler for
// every copy and the real launcher for the restore, and holds the remote branch, after the
// replacement pod's own push, to every commit the earlier push put there. An earlier release's
// bundle, left unrestored and older than the push, is held beside the running copy in both, and is
// refused by the same rule. The first case is the one the guard alone saves: the pod is replaced
// before the copy the push asked for has landed, so the newest copy the store holds is the one
// taken before the push.
func TestAPushedIssueBranchKeepsEveryCommitWhenItsWorkspaceIsRestoredFromACopy(t *testing.T) {
	testbin.RequireJJFilters(t)
	for _, c := range []struct {
		name string
		// pushCopyLanded is whether the copy the daemon took at the push had been stored by
		// the time the pod was replaced; the issue's one running-copy row holds the newest copy
		// that landed.
		pushCopyLanded bool
	}{
		{name: "the pod is replaced before the copy taken at the push lands", pushCopyLanded: false},
		{name: "the pod is replaced once the copy taken at the push has landed", pushCopyLanded: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newBundleFixture(t)
			branch := "legion/" + bundleIssue
			// c1 on the issue's branch, not pushed yet, and work in progress on top of it.
			f.run(f.ws, "jj", "new", "-R", f.ws, `remote_bookmarks(exact:"`+branch+`")`, "-m", "c1")
			f.write(filepath.Join(f.ws, "c1.txt"), "c1\n")
			f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, branch, "-r", "@")
			f.commits["c1"] = f.at()
			f.run(f.ws, "jj", "new", "-R", f.ws, "-m", "work in progress")
			f.write(filepath.Join(f.ws, "wip.txt"), "wip\n")
			// An earlier release saved the workspace as it stood then, and no pod restored it.
			earlier := f.bundleWorkspaceOne()
			f.write(filepath.Join(f.ws, "wip.txt"), "wip, five minutes on\n")
			// The cadence's copy while the pod runs.
			running := f.bundleWorkspaceOne()
			if running.Bookmarks[branch] != f.commits["c1"] {
				t.Fatalf("the running copy holds %s on %q, want it on the unpushed c1 %s: a restore of it must move the branch back",
					branch, running.Bookmarks[branch], f.commits["c1"])
			}

			// The newer work is committed and pushed to the issue's branch.
			f.run(f.ws, "jj", "describe", "-R", f.ws, "-m", "c2")
			f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, branch, "-r", "@")
			f.commits["c2"] = f.at()
			f.run(f.ws, "jj", "git", "push", "-R", f.ws, "-b", branch)
			if pushed := f.remoteSubjects(branch); !slices.Equal(pushed[:2], []string{"c2", "c1"}) {
				t.Fatalf("the remote branch reads %s after the push, want c2, c1, …", strings.Join(pushed, ","))
			}
			// The daemon records the push when it applies the webhook, which reaches it after the
			// push reached GitHub, and takes a copy of the workspace then.
			push := runtime.WorkspacePush{At: time.Now().UTC(), Head: f.commits["c2"]}
			f.run(f.ws, "jj", "new", "-R", f.ws, "-m", "after the push")
			f.write(filepath.Join(f.ws, "after.txt"), "after the push\n")
			atPush := f.bundleWorkspaceOne()
			if !atPush.At.After(push.At) {
				t.Fatalf("the copy at the push was taken at %s, want it after the push recorded at %s", atPush.At, push.At)
			}

			// The pod is replaced, and the daemon decides what its launcher is sent.
			held := running
			if c.pushCopyLanded {
				held = atPush
			}
			bundleAct, runningAct := runtime.DecideWorkspaceRestore(
				runtime.WorkspaceCopyHeld{Held: true, At: earlier.At},
				runtime.WorkspaceCopyHeld{Held: true, At: held.At}, push)
			workspace := f.secondPod("ws2")
			var sent *shimwire.WorkspaceBundle
			switch {
			case bundleAct == runtime.WorkspaceCopyOffered:
				sent = &earlier
			case runningAct == runtime.WorkspaceCopyOffered:
				sent = &held
			}
			if sent != nil {
				if notice := f.restoreInto(workspace, *sent, 1); !strings.Contains(notice, "is restored") {
					t.Fatalf("the restore's notice is %q", notice)
				}
			}

			// The replacement pod's role builds on the issue's branch as its workspace holds it,
			// and pushes.
			f.run(workspace, "jj", "new", "-R", workspace, `bookmarks(exact:"`+branch+`")`, "-m", "c3")
			f.write(filepath.Join(workspace, "c3.txt"), "c3\n")
			f.run(workspace, "jj", "bookmark", "set", "-R", workspace, branch, "-r", "@")
			f.run(workspace, "jj", "git", "push", "-R", workspace, "-b", branch)

			remote := f.remoteSubjects(branch)
			if len(remote) < 3 || !slices.Equal(remote[:3], []string{"c3", "c2", "c1"}) {
				t.Fatalf("the remote branch reads %s after the replacement pod's push, want c3, c2, c1, …: the push removed commits the earlier push had put there",
					strings.Join(remote, ","))
			}
			// The decision that kept them: the earlier release's bundle and any copy taken before
			// the push are refused, and the copy taken at the push is the one sent.
			if bundleAct != runtime.WorkspaceCopyRefusedPushed {
				t.Fatalf("the earlier release's bundle, taken at %s before the push at %s, was decided %v, want it refused", earlier.At, push.At, bundleAct)
			}
			if !c.pushCopyLanded {
				if runningAct != runtime.WorkspaceCopyRefusedPushed || sent != nil {
					t.Fatalf("the copy taken at %s before the push at %s was decided %v, want it refused and nothing sent", running.At, push.At, runningAct)
				}
				return
			}
			if runningAct != runtime.WorkspaceCopyOffered || sent == nil {
				t.Fatalf("the copy taken at the push was decided %v, want it sent", runningAct)
			}
			log := f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all()", "--no-graph", "-T", `commit_id ++ "\n"`)
			if !strings.Contains(log, atPush.Head) {
				t.Fatalf("the replacement pod's workspace holds no %s, the head of the copy taken at the push:\n%s", atPush.Head, log)
			}
		})
	}
}

// remoteSubjects is the first line of every commit message on branch in the remote, newest first.
func (f *bundleFixture) remoteSubjects(branch string) []string {
	f.t.Helper()
	return strings.Split(f.run(f.remote, "git", "--git-dir="+f.remote, "log", "--format=%s", "refs/heads/"+branch), "\n")
}
