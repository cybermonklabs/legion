package launcher

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The reviewer's loss, end to end against real git and jj, and the daemon's two guards against it.
//
// A copy of a workspace is taken while its roles work and push, so a pod that replaces the one the
// copy came off can be sent a copy whose `legion/<KEY>` is behind the issue's branch on GitHub or
// beside it. Were the restore to set that bookmark — the launcher sets every bookmark it is sent
// with `jj bookmark set --allow-backwards` — the replacement pod's role would move the branch on
// from the copy's commit and push, and the push would remove the pushed commits from GitHub: the
// branch read `c2, c1, …` and then `c3, c1, …`. The first guard is that the daemon sends no copy's
// own value of the issue's branch (runtime.RestorableBookmarks, which the sandbox applies to every
// copy it sends), so the branch stays on what the new pod's fetch brought from GitHub, and a
// role's move of it onto work built on a stale copy is refused by jj as backwards or sideways. The
// second is that the decision sends no running copy older than the newest push of the issue's
// branch it recorded (runtime.DecideWorkspaceRestore), and the daemon takes a copy of its own when
// it records the push, which is the copy a replacement pod is then sent.
//
// Every case takes its copies with the real shim's bundler, decides with the daemon's own
// decision, restores with the real launcher what the sandbox would send, and has the replacement
// pod's role follow its own recipe for a push (rolePush). It then holds the remote branch to every
// commit it held before the pod was replaced.
//
// The first two cases run the order the reviewer named — a running copy is taken, newer work is
// committed and pushed, the daemon records the push, the pod is replaced — with an earlier
// release's bundle, left unrestored and older than both, held beside the running copy and closed
// unsent as the older of the two. In the first the pod is replaced before the copy the push asked
// for has landed, so the newest copy the store holds is the one taken before the push, which the
// time rule refuses. The last three are copies the time rule sends, where the withheld branch
// alone keeps the pushed commits on GitHub: a copy taken after a push from outside the pod that the
// workspace never fetched, a copy taken before a push the daemon has not recorded, and a release
// bundle taken before a push from outside the pod, which the time rule never refuses.
func TestAPushedIssueBranchKeepsEveryCommitWhenItsWorkspaceIsRestoredFromACopy(t *testing.T) {
	testbin.RequireJJFilters(t)
	branch := "legion/" + bundleIssue
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
				t.Fatalf("the running copy holds %s on %q, want it on the unpushed c1 %s: a restore that set it would move the branch back",
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
			before := f.remoteCommits(branch)
			workspace, created := f.replacementPod("ws2")
			bundleTake, runningTake, offered := runtime.DecideWorkspaceRestore(copyHeldOff(earlier, earlierReleasedPodUID), copyHeld(held), push, replacementPodUID, created)
			// The decision that keeps them: the earlier release's bundle is the older copy and is
			// closed unsent, a running copy taken before the push is refused, and the copy taken
			// at the push is the one sent. A wrong decision is reported and the case goes on, so it
			// also says what that decision did to the branch.
			if bundleTake != runtime.WorkspaceBundleSuperseded {
				t.Errorf("the earlier release's bundle, taken at %s before the running copy at %s, was decided %v, want it superseded", earlier.At, held.At, bundleTake)
			}
			if !c.pushCopyLanded && (runningTake != runtime.WorkspaceBundleRefusedPushed || offered != runtime.WorkspaceCopyRunning) {
				t.Errorf("the copy taken at %s before the push at %s was decided %v with %q offered, want it offered and refused", running.At, push.At, runningTake, offered)
			}
			if c.pushCopyLanded && (runningTake != runtime.WorkspaceBundleSend || offered != runtime.WorkspaceCopyRunning) {
				t.Errorf("the copy taken at the push was decided %v with %q offered, want it offered and sent", runningTake, offered)
			}
			switch {
			case bundleTake == runtime.WorkspaceBundleSend:
				f.restoreAsSent(workspace, earlier)
			case runningTake == runtime.WorkspaceBundleSend:
				f.restoreAsSent(workspace, held)
			}
			if c.pushCopyLanded && !f.holdsCommit(workspace, atPush.Head) {
				t.Fatalf("the replacement pod's workspace holds no %s, the head of the copy taken at the push", atPush.Head)
			}

			if err := f.rolePush(workspace, "c3"); err != nil {
				t.Logf("the replacement pod's role pushed nothing: %v", err)
			}
			f.keepsEveryCommit(branch, before)
			if remote := f.remoteSubjects(branch); len(remote) < 3 || !slices.Equal(remote[:3], []string{"c3", "c2", "c1"}) {
				t.Fatalf("the remote branch reads %s after the replacement pod's role pushed, want c3, c2, c1, …: the role's work did not land on the branch",
					strings.Join(remote, ","))
			}
		})
	}

	t.Run("a push from outside the pod that the workspace never fetched", func(t *testing.T) {
		f := newBundleFixture(t)
		// Another clone of the issue's branch — a person's, or another pod's — pushes cX onto it.
		other := filepath.Join(f.root, "other")
		f.run(f.root, "git", "clone", "--quiet", "--no-local", "--branch", branch, f.remote, other)
		f.write(filepath.Join(other, "cx.txt"), "cX\n")
		f.run(other, "git", "add", "-A")
		f.run(other, "git", "commit", "--quiet", "-m", "cX")
		f.run(other, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
		f.commits["cX"] = f.run(other, "git", "rev-parse", "HEAD")
		// The daemon records that push as it records any push of the issue's branch.
		push := runtime.WorkspacePush{At: time.Now().UTC(), Head: f.commits["cX"]}
		// The workspace never fetched it: its role commits c1 on the branch as it last saw it,
		// moves its own legion/<KEY> onto c1, and its push is refused, since GitHub's branch is no
		// longer where this clone last saw it.
		if err := f.rolePush(f.ws, "c1"); err == nil {
			t.Fatalf("the push of c1 from the workspace that never fetched cX went through; the remote branch reads %s",
				strings.Join(f.remoteSubjects(branch), ","))
		}
		f.commits["c1"] = f.run(f.ws, "jj", "log", "-R", f.ws, "-r", "@-", "--no-graph", "-T", "commit_id")
		// The cadence copies the workspace after that push, so the time rule lets the copy
		// through, and the copy holds the branch beside cX.
		copied := f.bundleWorkspaceOne()
		if !copied.At.After(push.At) {
			t.Fatalf("the copy was taken at %s, want it after the push recorded at %s", copied.At, push.At)
		}
		if copied.Bookmarks[branch] != f.commits["c1"] {
			t.Fatalf("the copy holds %s on %q, want it on c1 %s, beside cX on GitHub", branch, copied.Bookmarks[branch], f.commits["c1"])
		}

		before := f.remoteCommits(branch)
		workspace := f.restoreSentCopy(copied, push)
		if err := f.rolePush(workspace, "c3"); err != nil {
			t.Logf("the replacement pod's role pushed nothing: %v", err)
		}
		f.keepsEveryCommit(branch, before)
		// The copy's work is in the replacement pod's workspace all the same, for the role to
		// bring forward over cX.
		if !f.holdsCommit(workspace, f.commits["c1"]) {
			t.Fatalf("the replacement pod's workspace holds no c1 %s, the work of the copy it was sent", f.commits["c1"])
		}
	})

	t.Run("a push the daemon has not recorded", func(t *testing.T) {
		f := newBundleFixture(t)
		// c1 committed and the issue's branch moved onto it, as the role's recipe does before its
		// push, and not pushed yet.
		f.write(filepath.Join(f.ws, "c1.txt"), "c1\n")
		f.run(f.ws, "jj", "describe", "-R", f.ws, "-m", "c1")
		f.run(f.ws, "jj", "new", "-R", f.ws)
		f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, branch, "-r", "@-")
		f.commits["c1"] = f.run(f.ws, "jj", "log", "-R", f.ws, "-r", "@-", "--no-graph", "-T", "commit_id")
		// The cadence copies the workspace.
		copied := f.bundleWorkspaceOne()
		if copied.Bookmarks[branch] != f.commits["c1"] {
			t.Fatalf("the copy holds %s on %q, want it on the unpushed c1 %s", branch, copied.Bookmarks[branch], f.commits["c1"])
		}
		// The role commits c2 and pushes it.
		if err := f.rolePush(f.ws, "c2"); err != nil {
			t.Fatalf("the push of c2: %v", err)
		}
		if pushed := f.remoteSubjects(branch); !slices.Equal(pushed[:2], []string{"c2", "c1"}) {
			t.Fatalf("the remote branch reads %s after the push, want c2, c1, …", strings.Join(pushed, ","))
		}
		// The daemon has no record of that push — its webhook is late or was lost — so the
		// decision reads none, and the copy taken before it is the one sent.
		before := f.remoteCommits(branch)
		workspace := f.restoreSentCopy(copied, runtime.WorkspacePush{})
		if !f.holdsCommit(workspace, copied.Head) {
			t.Fatalf("the replacement pod's workspace holds no %s, the head of the copy it was sent", copied.Head)
		}
		if err := f.rolePush(workspace, "c3"); err != nil {
			t.Logf("the replacement pod's role pushed nothing: %v", err)
		}
		f.keepsEveryCommit(branch, before)
	})

	t.Run("a release bundle taken before a push from outside the pod", func(t *testing.T) {
		f := newBundleFixture(t)
		// The role pushes c1 and goes on with work it has not pushed, and the idle release saves
		// the workspace while the role is idle, before it stops it.
		if err := f.rolePush(f.ws, "c1"); err != nil {
			t.Fatalf("the push of c1: %v", err)
		}
		f.write(filepath.Join(f.ws, "wip.txt"), "work the role has not pushed\n")
		saved := f.bundleWorkspaceOne()
		// While the release runs, another clone — GitHub's "Update branch", a person's — pushes cX
		// onto the issue's branch, and the daemon records that push after the save.
		other := filepath.Join(f.root, "other")
		f.run(f.root, "git", "clone", "--quiet", "--no-local", "--branch", branch, f.remote, other)
		f.write(filepath.Join(other, "cx.txt"), "cX\n")
		f.run(other, "git", "add", "-A")
		f.run(other, "git", "commit", "--quiet", "-m", "cX")
		f.run(other, "git", "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
		push := runtime.WorkspacePush{At: time.Now().UTC(), Head: f.run(other, "git", "rev-parse", "HEAD")}
		if !saved.At.Before(push.At) {
			t.Fatalf("the release bundle was taken at %s, want it before the push recorded at %s", saved.At, push.At)
		}

		before := f.remoteCommits(branch)
		workspace, created := f.replacementPod("ws2")
		bundleTake, runningTake, offered := runtime.DecideWorkspaceRestore(copyHeld(saved), runtime.WorkspaceCopyHeld{}, push, replacementPodUID, created)
		if bundleTake != runtime.WorkspaceBundleSend || runningTake != runtime.WorkspaceBundleNone || offered != runtime.WorkspaceCopyReleaseBundle {
			t.Fatalf("the release bundle taken at %s, with the push at %s, was decided (%v, %v, %q), want it offered and sent: the time rule refuses running copies alone",
				saved.At, push.At, bundleTake, runningTake, offered)
		}
		f.restoreAsSent(workspace, saved)
		if !f.holdsCommit(workspace, saved.Head) {
			t.Fatalf("the replacement pod's workspace holds no %s, the head of the release bundle it was sent", saved.Head)
		}
		if err := f.rolePush(workspace, "c3"); err != nil {
			t.Logf("the replacement pod's role pushed nothing: %v", err)
		}
		f.keepsEveryCommit(branch, before)
	})
}

// takenOffPodUID is the pod every copy of these cases was taken off, and replacementPodUID the pod
// that replaces it. earlierReleasedPodUID is the pod an earlier release took away: the bundle that
// release saved came off it, and a pod that came back after it, without restoring that bundle, is
// the one the later copies come off.
const (
	takenOffPodUID        = "pod-the-copies-came-off"
	replacementPodUID     = "pod-that-replaces-it"
	earlierReleasedPodUID = "pod-an-earlier-release-took-away"
)

// copyHeld is taken as the restore's decision reads its row: taken when the shim says it was, its
// one attempt unspent, off the pod being replaced.
func copyHeld(taken shimwire.WorkspaceBundle) runtime.WorkspaceCopyHeld {
	return copyHeldOff(taken, takenOffPodUID)
}

// copyHeldOff is taken as the restore's decision reads its row, off pod.
func copyHeldOff(taken shimwire.WorkspaceBundle, pod string) runtime.WorkspaceCopyHeld {
	return runtime.WorkspaceCopyHeld{At: taken.At, Source: pod}
}

// replacementPod is the pod that replaces the one the copies came off, created now: its own clone
// of the remote as it stands, and its workspace where provisioning puts one, a new working copy on
// the issue's branch as GitHub holds it (internal/workspace's createWorkspace adds the workspace
// at that commit). It answers the workspace and when the pod was created.
func (f *bundleFixture) replacementPod(name string) (string, time.Time) {
	f.t.Helper()
	created := time.Now().UTC()
	workspace := f.secondPod(name)
	f.run(workspace, "jj", "new", "-R", workspace, `remote_bookmarks(exact:"legion/`+bundleIssue+`")`)
	return workspace, created
}

// restoreSentCopy replaces the pod with one whose issue holds taken as its running copy and no
// release bundle, and whose newest push the daemon recorded is push; it holds the decision to
// sending that copy, restores it as the sandbox sends it, and answers the replacement pod's
// workspace.
func (f *bundleFixture) restoreSentCopy(taken shimwire.WorkspaceBundle, push runtime.WorkspacePush) string {
	f.t.Helper()
	workspace, created := f.replacementPod("ws2")
	bundleTake, runningTake, offered := runtime.DecideWorkspaceRestore(runtime.WorkspaceCopyHeld{}, copyHeld(taken), push, replacementPodUID, created)
	if bundleTake != runtime.WorkspaceBundleNone || runningTake != runtime.WorkspaceBundleSend || offered != runtime.WorkspaceCopyRunning {
		f.t.Fatalf("the copy taken at %s, with the push at %s, was decided (%v, %v, %q), want it offered and sent: this is a copy the time rule lets through",
			taken.At, push.At, bundleTake, runningTake, offered)
	}
	f.restoreAsSent(workspace, taken)
	return workspace
}

// restoreAsSent restores sent into workspace as the sandbox sends a copy: without the copy's own
// values of the issue's branch and its rescue branch (runtime.RestorableBookmarks, which
// internal/runtime/sandbox's bundleFrames applies to every copy it sends).
func (f *bundleFixture) restoreAsSent(workspace string, sent shimwire.WorkspaceBundle) {
	f.t.Helper()
	sent.Bookmarks, _ = runtime.RestorableBookmarks(bundleIssue, sent.Bookmarks)
	if notice := f.restoreInto(workspace, sent, 1); !strings.Contains(notice, "is restored") {
		f.t.Fatalf("the restore's notice is %q", notice)
	}
}

// rolePush is a role's own recipe for a push (internal/prompts' worker-common.md), run in
// workspace: its work, a file named for subject, committed on the working copy as subject, a new
// working copy started, the issue's branch moved onto the commit below it and, only when that move
// went through, the branch pushed. jj refuses the move when the branch does not reach that commit
// — a move backwards or sideways — and refuses the push when GitHub's branch is not where this
// clone last saw it. Either refusal is the recipe's own answer, which rolePush returns; nil is a
// push that went out.
func (f *bundleFixture) rolePush(workspace, subject string) error {
	f.t.Helper()
	f.write(filepath.Join(workspace, subject+".txt"), subject+"\n")
	f.run(workspace, "jj", "describe", "-R", workspace, "-m", subject)
	f.run(workspace, "jj", "new", "-R", workspace)
	run := fixturegit.Runner{TB: f.t, Env: f.env}
	branch := "legion/" + bundleIssue
	if out, err := run.Try(workspace, "jj", "bookmark", "set", "-R", workspace, branch, "-r", "@-"); err != nil {
		return fmt.Errorf("jj bookmark set %s: %w: %s", branch, err, strings.TrimSpace(out))
	}
	if out, err := run.Try(workspace, "jj", "git", "push", "-R", workspace, "--bookmark", branch); err != nil {
		return fmt.Errorf("jj git push --bookmark %s: %w: %s", branch, err, strings.TrimSpace(out))
	}
	return nil
}

// remoteSubjects is the first line of every commit message on branch in the remote, newest first.
func (f *bundleFixture) remoteSubjects(branch string) []string {
	f.t.Helper()
	return strings.Split(f.run(f.remote, "git", "--git-dir="+f.remote, "log", "--format=%s", "refs/heads/"+branch), "\n")
}

// remoteCommits is the id of every commit on branch in the remote, newest first.
func (f *bundleFixture) remoteCommits(branch string) []string {
	f.t.Helper()
	return strings.Split(f.run(f.remote, "git", "--git-dir="+f.remote, "log", "--format=%H", "refs/heads/"+branch), "\n")
}

// keepsEveryCommit fails the test unless branch in the remote still holds each of before, the
// commits it held before the pod was replaced: a branch that lost one is a push that removed
// pushed commits from GitHub.
func (f *bundleFixture) keepsEveryCommit(branch string, before []string) {
	f.t.Helper()
	after := f.remoteCommits(branch)
	for _, commit := range before {
		if !slices.Contains(after, commit) {
			lost := f.run(f.remote, "git", "--git-dir="+f.remote, "log", "-1", "--format=%s", commit)
			f.t.Fatalf("the remote branch reads %s after the replacement pod's role pushed, and lost %s (%s), which it held before: the push removed pushed commits from GitHub",
				strings.Join(f.remoteSubjects(branch), ","), lost, commit)
		}
	}
}

// holdsCommit is whether workspace's repository holds commit as a visible one.
func (f *bundleFixture) holdsCommit(workspace, commit string) bool {
	f.t.Helper()
	log := f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all()", "--no-graph", "-T", `commit_id ++ "\n"`)
	return slices.Contains(strings.Split(log, "\n"), commit)
}
