package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// releaseSaveFaults is the real release store with one save route refused: the row still reaches
// every other real authorization and outbox effect.
type releaseSaveFaults struct {
	outboxTrees
	cancelErr   error
	saveErr     error
	cancelTried bool
	saveRefused bool
}

func (s *releaseSaveFaults) IssueReleaseSave(ctx context.Context, project, issue string, row int64) (store.ReleaseDecision, error) {
	if s.saveErr != nil && !s.saveRefused {
		s.saveRefused = true
		return store.ReleaseDecision{}, s.saveErr
	}
	return s.outboxTrees.IssueReleaseSave(ctx, project, issue, row)
}

func (s *releaseSaveFaults) CancelIssueRelease(ctx context.Context, project, issue string, resume store.ReleaseResumeRow,
	outOfWorkflow func(status string) bool, paused store.ReleaseCancelPause) (store.ReleaseCancelled, error) {
	s.cancelTried = true
	if s.cancelErr != nil {
		return store.ReleaseCancelled{}, s.cancelErr
	}
	return s.outboxTrees.CancelIssueRelease(ctx, project, issue, resume, outOfWorkflow, paused)
}

// pendingSave is the release's workspace save as the drain left it after one failed attempt:
// still queued, its attempt counted and its lease released, so the next drain runs it again.
func (r *idleRig) pendingSave(t *testing.T) {
	t.Helper()
	var attempts int
	var lease string
	if err := r.pool.QueryRow(context.Background(), `select attempts, lease_token from outbox
		where issue = $1 and kind = 'issue_bundle'`, r.issue.Key).Scan(&attempts, &lease); err != nil {
		t.Fatalf("read the pending workspace save: %v", err)
	}
	if attempts != 1 || lease != "" {
		t.Fatalf("the pending workspace save has %d attempts and lease %q, want its first failed attempt settled and its lease cleared",
			attempts, lease)
	}
}

// A workspace save that did not finish is a prerequisite of its own release's teardown. A failed
// cancellation and a failed save authorization leave that row queued, but the stops and teardown
// from the batch continue, so each route must leave the pod and its workspace alone.
func TestAFailedSaveWhoseCancelFailsCannotDeleteThePod(t *testing.T) {
	ctx := context.Background()
	// One drain over what the release queued: the save fails first, and the stop and teardown
	// listed beside it run in the same pass.
	runOnce := func(t *testing.T, rig *idleRig) {
		t.Helper()
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the release once: %v", err)
		}
	}
	// The loss is a teardown, so that is read first. The pod stays because the teardown waits,
	// not because the release was cancelled: the mark stands and the teardown row is queued
	// again with the save as its reason.
	pendingSaveKeepsPod := func(t *testing.T, rig *idleRig, wantBundled int) {
		t.Helper()
		bundled, teardowns := rig.rt.reads()
		if teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none while the workspace save is pending", teardowns)
		}
		if len(bundled) != wantBundled {
			t.Fatalf("workspace bundle attempts = %d, want %d", len(bundled), wantBundled)
		}
		rig.pendingSave(t)
		if rig.bundled(t) {
			t.Fatal("the failed save left a bundle behind")
		}
		if !rig.marked(t) {
			t.Fatal("the release's mark is gone: the teardown cancelled the release instead of waiting for its save")
		}
		var attempts int
		var lease, lastErr string
		if err := rig.pool.QueryRow(ctx, `select attempts, lease_token, last_error from outbox
			where issue = $1 and kind = 'issue_release'`, rig.issue.Key).Scan(&attempts, &lease, &lastErr); err != nil {
			t.Fatalf("read the waiting teardown: %v", err)
		}
		if attempts != 1 || lease != "" || !strings.Contains(lastErr, "waits for its workspace save") {
			t.Fatalf("the teardown has %d attempts, lease %q and last error %q, want one settled wait for the workspace save",
				attempts, lease, lastErr)
		}
	}

	t.Run("the save's cancel fails", func(t *testing.T) {
		rig := newIdleRig(t)
		faults := &releaseSaveFaults{outboxTrees: rig.st, cancelErr: errors.New("the cancel transaction is unavailable")}
		rig.runner.trees = faults
		rig.rt.bundleErr = errors.New("the workspace bundle cannot be built")

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if !faults.cancelTried {
			t.Fatal("the failed save did not attempt its cancellation")
		}
		pendingSaveKeepsPod(t, rig, 1)
	})

	t.Run("the save's authorization fails once", func(t *testing.T) {
		rig := newIdleRig(t)
		faults := &releaseSaveFaults{outboxTrees: rig.st, saveErr: errors.New("the save authorization transaction is unavailable")}
		rig.runner.trees = faults

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if !faults.saveRefused {
			t.Fatal("the save authorization fault was not exercised")
		}
		pendingSaveKeepsPod(t, rig, 0)
	})

	t.Run("control: a healthy save still tears down", func(t *testing.T) {
		rig := newIdleRig(t)

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the healthy release to tear down", teardowns)
		}
	})

	t.Run("control: a failed save whose cancel succeeds keeps the pod", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.rt.bundleErr = errors.New("the workspace bundle cannot be built")

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if _, teardowns := rig.rt.reads(); teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none after the release was cancelled", teardowns)
		}
		if rig.marked(t) {
			t.Fatal("the successful cancellation left the release mark behind")
		}
	})

	t.Run("control: an empty workspace's save still tears down", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.rt.bundle = runtime.WorkspaceBundle{}

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the empty workspace release to tear down", teardowns)
		}
		if rig.bundled(t) {
			t.Fatal("the empty workspace release stored a bundle")
		}
	})

	t.Run("control: an unowned pod with no save row still tears down", func(t *testing.T) {
		rig := newIdleRig(t)
		rig.stoppedImplementer(t, false)

		if got := rig.queuedBy(t, func() { rig.w.releaseUnownedPods(ctx) }); len(got) != 1 || got[0] != "issue_release" {
			t.Fatalf("queued rows = %v, want the teardown alone: an unowned release saves no bundle", got)
		}
		runOnce(t, rig)

		bundled, teardowns := rig.rt.reads()
		if teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want the unowned release to tear down", teardowns)
		}
		if len(bundled) != 0 {
			t.Fatalf("workspace bundle attempts = %d, want none for an unowned pod", len(bundled))
		}
		if rig.bundled(t) {
			t.Fatal("the unowned release stored a bundle")
		}
	})
}
