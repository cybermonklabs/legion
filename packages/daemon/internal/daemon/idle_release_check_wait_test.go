package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// releaseCheckFaults is the real release store with two faults a test arms by setting a field.
// declineErr refuses every DeclineIssueRelease while it is set; checkErr refuses the first
// IssueReleaseCheck and no later one. declineTried records that the check asked to decline the
// release, and checkRefused that the check's one refusal was given. Every other call reaches the
// real store.
type releaseCheckFaults struct {
	outboxTrees
	declineErr   error
	checkErr     error
	declineTried bool
	checkRefused bool
}

func (s *releaseCheckFaults) IssueReleaseCheck(ctx context.Context, project, issue string, row int64) (store.ReleaseDecision, error) {
	if s.checkErr != nil && !s.checkRefused {
		s.checkRefused = true
		return store.ReleaseDecision{}, s.checkErr
	}
	return s.outboxTrees.IssueReleaseCheck(ctx, project, issue, row)
}

func (s *releaseCheckFaults) DeclineIssueRelease(ctx context.Context, project, issue string, decline store.IdleReleaseDecline,
	woken store.ReleaseWokenRow, outOfWorkflow func(status string) bool, paused store.ReleaseCancelPause) (store.ReleaseCancelled, error) {
	s.declineTried = true
	if s.declineErr != nil {
		return store.ReleaseCancelled{}, s.declineErr
	}
	return s.outboxTrees.DeclineIssueRelease(ctx, project, issue, decline, woken, outOfWorkflow, paused)
}

// pendingCheck is the release's workspace check as the drain left it after one failed attempt:
// still queued, its attempt counted and its lease released, so the next drain runs it again.
func (r *idleRig) pendingCheck(t *testing.T) {
	t.Helper()
	var attempts int
	var lease string
	if err := r.pool.QueryRow(context.Background(), `select attempts, lease_token from outbox
		where issue = $1 and kind = 'issue_bundle'`, r.issue.Key).Scan(&attempts, &lease); err != nil {
		t.Fatalf("read the pending workspace check: %v", err)
	}
	if attempts != 1 || lease != "" {
		t.Fatalf("the pending workspace check has %d attempts and lease %q, want its first failed attempt settled and its lease cleared",
			attempts, lease)
	}
}

// A release's teardown waits until the release's own workspace check has left the outbox. A
// decline that fails and a failed check authorization each leave that row queued while the role
// stop and the teardown of the same pass go on, so each route must leave the pod and its workspace
// alone: a teardown that ran ahead of the check would delete a disk whose workspace nobody read.
// Once the check row leaves on a later pass, the teardown it held back finishes without a pod to
// take (the check declined the release) or tears the pod down (the check found nothing).
func TestATeardownWaitsForTheReleasesWorkspaceCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// One drain over what the release queued: the check fails first, and the stop and teardown
	// listed beside it run in the same pass.
	runOnce := func(t *testing.T, rig *idleRig) {
		t.Helper()
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the release once: %v", err)
		}
	}
	// The loss is a teardown, so that is read first. The pod stays because the teardown waits,
	// not because the release was cancelled: the mark stands and the teardown row is queued
	// again with the check as its reason.
	pendingCheckKeepsPod := func(t *testing.T, rig *idleRig, wantReads int) {
		t.Helper()
		read, teardowns := rig.rt.reads()
		if teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none while the workspace check is pending", teardowns)
		}
		if len(read) != wantReads {
			t.Fatalf("workspace reads = %d, want %d", len(read), wantReads)
		}
		rig.pendingCheck(t)
		if rig.declined(t) {
			t.Fatal("the failed check recorded a decline")
		}
		if !rig.marked(t) {
			t.Fatal("the release's mark is gone: the teardown cancelled the release instead of waiting for its check")
		}
		var attempts int
		var lease, lastErr string
		if err := rig.pool.QueryRow(ctx, `select attempts, lease_token, last_error from outbox
			where issue = $1 and kind = 'issue_release'`, rig.issue.Key).Scan(&attempts, &lease, &lastErr); err != nil {
			t.Fatalf("read the waiting teardown: %v", err)
		}
		if attempts != 1 || lease != "" || !strings.Contains(lastErr, "waits for its workspace check") {
			t.Fatalf("the teardown has %d attempts, lease %q and last error %q, want one settled wait for the workspace check",
				attempts, lease, lastErr)
		}
	}
	// The next pass, with the rows the first one retried made due in SQL, so no clock decides it:
	// the check runs first, by id, and the teardown it held back runs after it in the same pass.
	nextPass := func(t *testing.T, rig *idleRig) {
		t.Helper()
		if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() - interval '1 minute' where issue = $1`,
			rig.issue.Key); err != nil {
			t.Fatalf("make the release's rows due: %v", err)
		}
		runOnce(t, rig)
	}
	// releaseRowsLeft is how many of the release's check and teardown are still in the outbox.
	releaseRowsLeft := func(t *testing.T, rig *idleRig) int {
		t.Helper()
		var left int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
			and kind in ('issue_bundle', 'issue_release')`, rig.issue.Key).Scan(&left); err != nil {
			t.Fatalf("count the release's check and teardown: %v", err)
		}
		return left
	}
	unpushed := runtime.WorkspaceWork{Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Count: 1,
		Commits: []string{"9b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c"}}

	t.Run("the check's decline fails", func(t *testing.T) {
		rig := newIdleRig(t)
		faults := &releaseCheckFaults{outboxTrees: rig.st, declineErr: errors.New("the decline transaction is unavailable")}
		rig.runner.trees = faults
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, unpushed)

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if !faults.declineTried {
			t.Fatal("the check that found work asked to decline nothing")
		}
		pendingCheckKeepsPod(t, rig, 1)

		// The decline answers again. The check's retry reads the same work and now declines the
		// release, so the teardown it held back reads the release as over and finishes without a
		// teardown, and the implementer's resume is queued.
		faults.declineErr = nil
		nextPass(t, rig)
		if left := releaseRowsLeft(t, rig); left != 0 {
			t.Fatalf("%d of the check and the teardown are left, want both finished once the check's retry declined the release", left)
		}
		if rig.marked(t) {
			t.Fatal("the release's mark outlived its decline")
		}
		if !rig.declined(t) {
			t.Fatal("the check's retry recorded no decline")
		}
		if _, teardowns := rig.rt.reads(); teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none: the declined release keeps the pod", teardowns)
		}
		var resumes int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'implementer'`, rig.issue.Key).Scan(&resumes); err != nil {
			t.Fatal(err)
		}
		if resumes != 1 {
			t.Fatalf("queued resumes of the implementer = %d, want the one the decline queues", resumes)
		}
	})

	t.Run("the check's authorization fails once", func(t *testing.T) {
		rig := newIdleRig(t)
		faults := &releaseCheckFaults{outboxTrees: rig.st, checkErr: errors.New("the check authorization transaction is unavailable")}
		rig.runner.trees = faults

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if !faults.checkRefused {
			t.Fatal("the check authorization fault was not exercised")
		}
		pendingCheckKeepsPod(t, rig, 0)

		// The check's authorization answers again: the check reads a workspace with nothing origin
		// lacks and leaves the outbox, and the teardown it held back tears the pod down once in the
		// same pass.
		nextPass(t, rig)
		if left := releaseRowsLeft(t, rig); left != 0 {
			t.Fatalf("%d of the check and the teardown are left, want both finished once the check found nothing", left)
		}
		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want one once the check has left the outbox", teardowns)
		}
		if rig.marked(t) {
			t.Fatal("the finished release left its mark behind")
		}
	})

	t.Run("control: a clean workspace's check still tears down", func(t *testing.T) {
		rig := newIdleRig(t)

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the clean release to tear down", teardowns)
		}
	})

	t.Run("control: a check whose decline succeeds keeps the pod", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.rt.ScriptWorkspaceWork(rig.issue.Key, unpushed)

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if _, teardowns := rig.rt.reads(); teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none after the release was declined", teardowns)
		}
		if rig.marked(t) {
			t.Fatal("the decline left the release mark behind")
		}
	})

	t.Run("control: an unowned pod's teardown follows its launcher's check", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)

		if got := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(ctx) }); !slices.Equal(got, []string{"issue_bundle", "issue_release"}) {
			t.Fatalf("queued rows = %v, want the launcher's workspace check and the teardown: an unowned release reads the disk it deletes (R6)", got)
		}
		runOnce(t, rig)

		read, teardowns := rig.rt.reads()
		if teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the unowned release to tear down", teardowns)
		}
		if len(read) != 0 {
			t.Fatalf("workspace reads = %d, want none for an unowned pod", len(read))
		}
	})
}
