package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A push the daemon records while an idle release runs, after its save and before its role
// stops, leaves no release bundle holding the issue's pod once the next pod has restored the
// issue's work. The release's rows run in that order (idleReleaseRows): the save, then a stop per
// role, then the teardown, so between the save and the last stop the pod's roles are still live.
// A push recorded in that window — GitHub's "Update branch", a person's push, a webhook the
// per-issue read rebuilt late — asks those roles for the copy each recorded push takes
// (copyWorkspaceAtPush), and that copy is taken off the pod the bundle came off, after the bundle.
// It is the newer of the two, so the next pod is offered it and the bundle is answered superseded,
// and a release bundle row of any outcome holds the issue's pod (workflow.IdleHoldBundleUnrestored)
// until the issue's work is over. The issue would then never be released again, whatever its pod
// does: R2 holds that an issue holding a running copy is still released when it is idle, and that
// a running copy never replaces a release bundle whose restore has not happened.
func TestAPushRecordedWhileAReleaseRunsLeavesNoReleaseBundleHoldingTheNextPod(t *testing.T) {
	rig := newCopyRig(t)
	ctx := context.Background()
	// Whole seconds, which the store keeps exactly.
	saved := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
	rig.rt.bundle.At = saved
	copied := copyRigBundle
	copied.At = saved.Add(2 * time.Minute)
	rig.bundler.answers(copied, nil)

	// The release is queued, and its save runs alone: the role stops behind it have not.
	rig.w.releaseIdlePods(ctx)
	due, err := rig.runner.listDue(ctx, "", nil, rig.runner.now().UTC())
	if err != nil {
		t.Fatalf("list the release's rows: %v", err)
	}
	if len(due) == 0 || due[0].Kind != record.OutboxKindIssueBundle {
		t.Fatalf("the release's first due row = %+v, want its save", due)
	}
	if err := rig.runner.runRow(ctx, due[0]); err != nil {
		t.Fatalf("run the release's save: %v", err)
	}
	if !rig.bundled(t) {
		t.Fatal("the release's save stored no bundle")
	}
	if rows := rig.rows(t); len(rows) < 2 || rows[0] != "supervise:release" || rows[len(rows)-1] != "issue_release" {
		t.Fatalf("queued rows after the save = %v, want the role stops still ahead", rows)
	}

	// A push of the issue's branch is recorded now, with the pod's roles still live, through the
	// workflow's push handler as the daemon wires it.
	before := len(copyOutcomes(t, rig))
	pushed := saved.Add(time.Minute)
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Clock: func() time.Time { return pushed },
		Pushes: rig.st, PushRecorded: rig.w.copyWorkspaceAtPush}, quietLogger())
	push := intake.Push{Repo: rig.issue.GitHubRepo, Branch: "legion/" + rig.issue.Key, Before: "c0", After: "c2"}
	if _, err := intake.ApplyFact(ctx, rig.pool, "github", "push-webhook:c0..c2", push, engine); err != nil {
		t.Fatalf("apply the push of c0..c2: %v", err)
	}
	// The push's copy comes to an end of its own before the release goes on, whatever end that
	// is: stored, not stored for some reason, dropped or overtaken. Each is one line of the copy's
	// own naming the issue, and the release writes none of them, so the drain below never runs
	// beside a copy that is still being built.
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		return len(copyOutcomes(t, rig)) > before
	}) {
		t.Fatalf("the push's copy wrote no line of its end; the daemon logged:\n%s", rig.logs.String())
	}
	rig.awaitQuiet(t)
	rig.drain(t)

	// The pod that replaces the released one restores the issue's work, and its launcher's
	// report of the restore confirms the copy it was sent.
	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it", saved.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var sent []runtime.WorkspaceCopyTaken
	for _, taken := range []runtime.WorkspaceCopyTaken{restore.ReleaseBundle, restore.Running} {
		if taken.Take == runtime.WorkspaceBundleSend {
			sent = append(sent, taken)
		}
	}
	if len(sent) != 1 {
		t.Fatalf("the next pod was sent %d copies (release bundle %v, running copy %v), want the issue's work once",
			len(sent), restore.ReleaseBundle.Take, restore.Running.Take)
	}
	if err := rig.st.KeepRestoredWorkspaceCopy(ctx, rig.issue.Key, "pod-that-replaces-it", sent[0].Bundle.Release); err != nil {
		t.Fatalf("confirm the restore: %v", err)
	}

	if rig.bundled(t) {
		t.Fatalf("after the next pod restored the issue's work, a release bundle (answered %v) still holds the issue's pod (%s), so no idle release of the issue ever runs again",
			restore.ReleaseBundle.Take, workflow.IdleHoldBundleUnrestored)
	}
}

// copyOutcomes is every line of the rig's issue that a running copy writes as it ends: stored, not
// stored for a reason it names, dropped because the workspace holds nothing unpushed, or not
// stored because a push overtook it. A copy writes exactly one of them however it ends, and no
// other path writes any of them.
func copyOutcomes(t *testing.T, rig *copyRig) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, msg := range []string{runningCopyStoredLine, runningCopyUnsavedLine, runningCopyDroppedLine, runningCopyOvertakenLine} {
		for _, line := range rig.lines(t, msg) {
			if line["issue"] == rig.issue.Key {
				found = append(found, line)
			}
		}
	}
	return found
}
