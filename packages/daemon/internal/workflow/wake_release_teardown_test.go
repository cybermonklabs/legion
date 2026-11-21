package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// R3 and R4 (dispatch://INFRA-3589): a wake report that comes during a release and is applied
// again after the release records the claim as released can land before the release's teardown.
// The teardown then reads the wake's start as the pod being used again and cancels the release, so
// the pod never goes away. Whatever the daemon does in that window, the role ends up in one true
// state: either its pod was released and the wake brings it back, or its pod never went and the
// role comes back in it once, told nothing about a release that did not happen and with no resume
// recorded for one.
//
// The idle rig's held-stop row (internal/daemon, AWakeDuringAHeldStopBringsThePodBackOnce) lands
// in this window by its own timing: its 40-second held stop ends when its listener report's first
// 30-second retry comes, and on a loaded pod the retry comes after the release records the claim
// and before the teardown. The tester saw that row fail in 2 of 6 runs at cd089776 (the daemon's
// log: "wake: a released pod is coming back", then "outbox: the idle release of an issue's pod did
// not finish; its roles go on in the same pod" with hold overtaken).
func TestAWakeTakenBeforeTheTeardownLeavesTheRoleInOneTrueState(t *testing.T) {
	rig := newWakeRig(t)
	st := releaseStore(t, rig.pool)
	ctx := context.Background()
	project := wakeProjectToken(t)

	// The release marks the pod, stops the implementer and records its claim as released. Its
	// role stop is done, so its row is gone from the outbox, as a delivered row is. Its teardown
	// row is still queued.
	setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
	boundary := markRelease(t, st)
	setClaimState(t, rig.pool, claim.RoleImplementer, "suspended", "released")
	if err := st.ReleaseSuspendedClaim(ctx, project, "LEGION-208", claim.RoleImplementer, boundary); err != nil {
		t.Fatalf("record the claim the release suspended: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, boundary); err != nil {
		t.Fatalf("finish the release's role stop: %v", err)
	}
	var teardown int64
	if err := rig.pool.QueryRow(ctx, `select id from outbox where issue = 'LEGION-208' and kind = 'issue_release'`).Scan(&teardown); err != nil {
		t.Fatalf("read the release's teardown row: %v", err)
	}

	// The wake report's retry comes now, between the record and the teardown. A wake that waits
	// for the teardown answers a retry; a wake that takes the pod answers nothing.
	fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
	if err := applyWake(rig, "wake", fact); err != nil {
		var later *intake.RetryLater
		if !errors.As(err, &later) {
			t.Fatalf("applying the wake between the release's record and its teardown = %v, want it applied or a retry", err)
		}
	}

	// The teardown decides, and a cancel is carried out as the outbox does (cancelRelease).
	decision, err := st.IssueRelease(ctx, project, store.IssueReleased{Issue: "LEGION-208", Tree: "LEGION-208",
		IssueGeneration: wakeIssueGeneration, TreeGeneration: wakeIssueGeneration, Row: teardown}, record.OutOfWorkflow)
	if err != nil {
		t.Fatalf("the release's teardown decision: %v", err)
	}
	if decision.Act == store.ReleaseCancel {
		resume := func(role claim.Role, generation uint64) (store.IdleReleaseRow, error) {
			row, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
				Role: role, Generation: generation}, time.Now())
			if err != nil {
				return store.IdleReleaseRow{}, err
			}
			return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
		}
		if _, err := st.CancelIssueRelease(ctx, project, "LEGION-208", resume, record.OutOfWorkflow, store.ReleaseCancelPause{At: time.Now()}); err != nil {
			t.Fatalf("cancel the release as the outbox does: %v", err)
		}
	}

	starts := wakeStarts(t, rig.pool)
	if len(starts) > 1 {
		t.Errorf("supervise start rows = %+v, want at most one start of the implementer: the wake and the release's cancel each queued one", starts)
	}
	if decision.Act != store.ReleaseCancel {
		return
	}
	// The release was cancelled: the pod never went away.
	for _, start := range starts {
		if strings.Contains(start.Task, "pod had been released") {
			t.Errorf("the implementer's task = %q, want no text that its pod was released: the teardown cancelled the release (cause %s), so its pod never went away",
				start.Task, decision.Cause)
		}
	}
	if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
		t.Errorf("pod resumes = %v, want none: the release was cancelled (cause %s), so no released pod came back", resumes, decision.Cause)
	}
}
