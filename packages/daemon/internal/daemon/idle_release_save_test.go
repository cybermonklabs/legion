package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseSaveFaults is the real release store with two faults a test arms by setting a field.
// cancelErr refuses every CancelIssueRelease while it is set, whichever row asks; saveErr refuses
// the first IssueReleaseSave and no later one. cancelTried records that some row asked to cancel
// the release, and saveRefused that the save's one refusal was given. Every other call reaches the
// real store.
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

// A release's teardown waits until the release's own workspace save has left the outbox. A failed
// cancel and a failed save authorization each leave that row queued while the role stop and the
// teardown of the same pass go on, so each route must leave the pod and its workspace alone; once
// the save row leaves on a later pass, the teardown it held back finishes or tears the pod down.
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
	// The next pass, with the rows the first one retried made due in SQL, so no clock decides it:
	// the save runs first, by id, and the teardown it held back runs after it in the same pass.
	nextPass := func(t *testing.T, rig *idleRig) {
		t.Helper()
		if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() - interval '1 minute' where issue = $1`,
			rig.issue.Key); err != nil {
			t.Fatalf("make the release's rows due: %v", err)
		}
		runOnce(t, rig)
	}
	// releaseRowsLeft is how many of the release's save and teardown are still in the outbox.
	releaseRowsLeft := func(t *testing.T, rig *idleRig) int {
		t.Helper()
		var left int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
			and kind in ('issue_bundle', 'issue_release')`, rig.issue.Key).Scan(&left); err != nil {
			t.Fatalf("count the release's save and teardown: %v", err)
		}
		return left
	}

	t.Run("the save's cancel fails", func(t *testing.T) {
		rig := newIdleRig(t)
		faults := &releaseSaveFaults{outboxTrees: rig.st, cancelErr: errors.New("the cancel transaction is unavailable")}
		rig.runner.trees = faults
		rig.rt.bundleErr = errors.New("the workspace bundle cannot be built")

		rig.w.releaseIdlePods(ctx)
		runOnce(t, rig)

		if !faults.cancelTried {
			t.Fatal("no row asked to cancel the release after its save failed")
		}
		pendingSaveKeepsPod(t, rig, 1)

		// The cancel answers again. The save's retry still cannot build the bundle and now
		// cancels the release, so the teardown it held back reads the release as over and
		// finishes without a teardown, and the implementer's resume is queued.
		faults.cancelErr = nil
		nextPass(t, rig)
		if left := releaseRowsLeft(t, rig); left != 0 {
			t.Fatalf("%d of the save and the teardown are left, want both finished once the save's retry cancelled the release", left)
		}
		if rig.marked(t) {
			t.Fatal("the release's mark outlived its cancel")
		}
		if _, teardowns := rig.rt.reads(); teardowns != 0 {
			t.Fatalf("Sandbox teardowns = %d, want none: the cancelled release keeps the pod", teardowns)
		}
		var resumes int
		if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'implementer'`, rig.issue.Key).Scan(&resumes); err != nil {
			t.Fatal(err)
		}
		if resumes != 1 {
			t.Fatalf("queued resumes of the implementer = %d, want the one the cancel queues", resumes)
		}
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

		// The save's authorization answers again: the save stores the bundle and leaves the
		// outbox, and the teardown it held back tears the pod down once in the same pass.
		nextPass(t, rig)
		if left := releaseRowsLeft(t, rig); left != 0 {
			t.Fatalf("%d of the save and the teardown are left, want both finished once the save stored its bundle", left)
		}
		if _, teardowns := rig.rt.reads(); teardowns != 1 {
			t.Fatalf("Sandbox teardowns = %d, want one once the save has left the outbox", teardowns)
		}
		if !rig.bundled(t) {
			t.Fatal("the save's retry stored no bundle")
		}
		if rig.marked(t) {
			t.Fatal("the finished release left its mark behind")
		}
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

// A bundle the release cannot save stops it: the pod stays, every claim the release suspended is
// resumed into it, and no Sandbox is torn down. The work a role has not pushed is kept at the
// cost of a pod, never dropped to save one. The pod's hold names which it was: a bundle that could
// not be built, or one too large to carry, which the shim refuses itself with the size it came to
// and which the release's line names.
func TestABundleTheReleaseCannotSaveStopsItAndResumesTheClaims(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail makes the save's bundle fail that way.
		fail func(rig *idleRig)
		hold workflow.IdleHold
		// says is what the release's line says of the bundle, where the case pins it.
		says string
	}{
		{name: "the bundle could not be built", hold: workflow.IdleHoldBundleSave,
			fail: func(rig *idleRig) { rig.rt.bundleErr = errors.New("the launcher answered nothing") }},
		{name: "the shim refuses the bundle as over the bound", hold: workflow.IdleHoldBundleTooLarge,
			says: strconv.Itoa(runtime.MaxWorkspaceBundleBytes + 1),
			fail: func(rig *idleRig) {
				shim := fake.NewConn()
				shim.SetWorkspaceBundle(runtime.WorkspaceBundle{Bundle: make([]byte, runtime.MaxWorkspaceBundleBytes+1),
					Head: "abc", Tips: []string{"abc"}})
				rig.rt.shim = shim
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIdleRig(t)
			ctx := context.Background()
			logs := &syncBuffer{}
			rig.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			tc.fail(rig)

			rig.w.releaseIdlePods(ctx)
			rig.drain(t)

			got := rig.machine.Claim()
			if got.State == supervise.StateSuspended {
				t.Fatalf("the claim = %s by %s, want it resumed into the pod the release kept", got.State, got.SuspendedBy)
			}
			if _, suspended := rig.rt.reads(); suspended != 0 {
				t.Fatalf("Sandbox teardowns = %d, want none", suspended)
			}
			if rig.bundled(t) {
				t.Fatal("a release that could not save its bundle left one behind")
			}
			if rig.marked(t) {
				t.Fatal("the stopped release left its mark behind")
			}
			if holds := rig.records(t).Holds; len(holds) != 1 || holds[0].Reason != tc.hold.String() {
				t.Fatalf("holds = %+v, want the one %s", holds, tc.hold)
			}
			if line := logs.String(); !strings.Contains(line, "hold="+tc.hold.String()) || !strings.Contains(line, tc.says) {
				t.Fatalf("the release logged:\n%s\nwant its cancel under %s naming %q", line, tc.hold, tc.says)
			}
		})
	}
}
