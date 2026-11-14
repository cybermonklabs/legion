package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A copy that read the workspace before a push of the issue's branch is not restored to the next
// pod, however its write and the push's record interleave. The copy in flight is marked overtaken
// only once the push's transaction has committed (workflow.Config.PushRecorded runs on commit),
// so a copy whose overtaken check runs just before that mark, and whose write lands just after the
// push was recorded, is stored. Here the push's commit lets the copy in flight go and waits for it
// to be stored before the daemon is told of the push, which is one order the two goroutines can
// run in. The pod then ends before the copy owed behind the push is taken. The stored copy holds
// the workspace as it was before the pushed commits were made, so restoring it would put the next
// pod's workspace back behind the push, which is what the push rule refuses
// (runtime.DecideWorkspaceRestore).
func TestACopyReadBeforeAPushAndWrittenAfterItsRecordIsNotRestored(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	release := make(chan struct{})
	recorded := func(issue string) {
		close(release)
		rig.awaitLine(t, runningCopyStoredLine)
		rig.w.copyWorkspaceAtPush(issue)
	}
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Pushes: rig.st, PushRecorded: recorded}, quietLogger())
	beforePush := copyRigBundle
	beforePush.Bundle = []byte("PACK the workspace as it was read before the push")
	rig.bundler.holds(func(ctx context.Context) (runtime.WorkspaceBundle, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return runtime.WorkspaceBundle{}, ctx.Err()
		}
		// The pod ends before the copy owed behind the push can read its workspace.
		rig.bundler.holds(func(context.Context) (runtime.WorkspaceBundle, error) {
			return runtime.WorkspaceBundle{}, errors.New("the pod is gone")
		})
		return beforePush, nil
	})
	rig.turnStart(t)
	rig.w.copyWorkingWorkspaces(ctx)
	rig.awaitAsks(t, 1)

	push := intake.Push{Repo: rig.issue.GitHubRepo, Branch: "legion/" + rig.issue.Key, Before: "c1", After: "c2"}
	if _, err := intake.ApplyFact(ctx, rig.pool, "github", "push-of-the-issue-branch", push, engine); err != nil {
		t.Fatalf("apply the push: %v", err)
	}
	rig.awaitAsks(t, 2)
	rig.awaitQuiet(t)

	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it")
	if err != nil {
		t.Fatal(err)
	}
	if sent := restore.Running; sent.Take == runtime.WorkspaceBundleSend {
		t.Fatalf("the pod that replaces this one was sent %q, a copy whose bundling began before the push it is stamped after (%s): the restore puts its workspace back behind the pushed commits",
			sent.Bundle.Bundle, sent.Bundle.At)
	}
}
