package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// R3 and R4 (dispatch://INFRA-3589): a wake report that comes during a release and is applied
// again after the release records the claim as released can land before the release's teardown.
// A teardown that read the wake's start as the pod being used again would cancel the release, so
// the pod would never go away. Whatever the daemon does in that window, the role ends up in one
// true state: either its pod was released and the wake brings it back, or its pod never went and
// the role comes back in it once, told nothing about a release that did not happen and with no
// resume recorded for one.
//
// The wake is of a role the release recorded, so the teardown passes over the wake's own start
// and acts (store.Store.IssueRelease): the role's one start is the wake's, in the release's
// words, and the wake's resume stands. A cancel can still follow such a wake for another cause.
// In the second row, another role of the pod, the architect, works in it when the teardown reads
// the claims, so the teardown cancels the release (store's releaseOvertaken), and the test carries
// the cancel out as the outbox does (cancelRelease). The cancel deletes the wake's start and its
// resume, and its own resume is the role's one start (store.Store.CancelIssueRelease). The first
// row is the control of the second: there the release's words and the resume are present.
//
// The idle rig's held-stop row (internal/daemon, AWakeDuringAHeldStopBringsThePodBackOnce) lands
// in this window by its own timing: its 40-second held stop ends when its listener report's first
// 30-second retry comes, and on a loaded pod the retry comes after the release records the claim
// and before the teardown.
func TestAWakeTakenBeforeTheTeardownLeavesTheRoleInOneTrueState(t *testing.T) {
	t.Parallel()
	// wakeBeforeTheTeardown marks the release, stops the implementer and records its claim as
	// released. The role stop is done, so its row is gone from the outbox, as a delivered row is.
	// The teardown row is still queued, and the answer is its id. Then the wake report's retry
	// comes, between the record and the teardown, and the wake takes the released claim.
	wakeBeforeTheTeardown := func(t *testing.T, rig wakeRig, st *store.Store) int64 {
		t.Helper()
		ctx := context.Background()
		setClaimState(t, rig.pool, claim.RoleImplementer, "idle", "")
		boundary := markRelease(t, st)
		setClaimState(t, rig.pool, claim.RoleImplementer, "suspended", "released")
		if err := st.ReleaseSuspendedClaim(ctx, wakeProjectToken(t), "LEGION-208", claim.RoleImplementer, boundary); err != nil {
			t.Fatalf("record the claim the release suspended: %v", err)
		}
		if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, boundary); err != nil {
			t.Fatalf("finish the release's role stop: %v", err)
		}
		var teardown int64
		if err := rig.pool.QueryRow(ctx, `select id from outbox where issue = 'LEGION-208' and kind = 'issue_release'`).Scan(&teardown); err != nil {
			t.Fatalf("read the release's teardown row: %v", err)
		}
		fact := intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-agent"}
		if err := applyWake(rig, "wake", fact); err != nil {
			t.Fatalf("applying the wake between the release's record and its teardown = %v, want it applied: the release recorded the claim", err)
		}
		return teardown
	}
	teardownDecision := func(t *testing.T, st *store.Store, teardown int64) store.ReleaseDecision {
		t.Helper()
		decision, err := st.IssueRelease(context.Background(), wakeProjectToken(t), store.IssueReleased{Issue: "LEGION-208",
			Generation: wakeIssueGeneration, Row: teardown}, record.OutOfWorkflow)
		if err != nil {
			t.Fatalf("the release's teardown decision: %v", err)
		}
		return decision
	}

	t.Run("the teardown acts, and the wake brings the role back once in the release's words", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		teardown := wakeBeforeTheTeardown(t, rig, st)

		if decision := teardownDecision(t, st, teardown); decision.Act != store.ReleaseActs {
			t.Fatalf("the teardown after the wake = %s for %s, want it to act: the wake is of a role the release recorded", decision.Act, decision.Cause)
		}
		if starts := wakeStarts(t, rig.pool); len(starts) != 1 || starts[0].Role != claim.RoleImplementer || !strings.HasPrefix(starts[0].Task, wakePreamble) {
			t.Errorf("supervise start rows = %+v, want the wake's one start of the implementer, its task beginning %q: the pod goes away and the wake brings it back",
				starts, wakePreamble)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleImplementer), pods.WakeAgentMessage} {
			t.Errorf("pod resumes = %v, want the wake's one resume of the implementer, of kind %s", resumes, pods.WakeAgentMessage)
		}
	})

	t.Run("another role works in the pod, so the teardown cancels the release, and the role starts once with no wake", func(t *testing.T) {
		rig := newWakeRig(t)
		st := releaseStore(t, rig.pool)
		teardown := wakeBeforeTheTeardown(t, rig, st)
		setClaimState(t, rig.pool, claim.RoleArchitect, "working", "")

		decision := teardownDecision(t, st, teardown)
		if decision.Act != store.ReleaseCancel || decision.Cause != store.ReleaseCauseOvertaken {
			t.Fatalf("the teardown with the architect working = %s for %s, want it cancelled for %s: a role works in the pod",
				decision.Act, decision.Cause, store.ReleaseCauseOvertaken)
		}
		resume := func(role claim.Role, generation uint64, _ store.DroppedWake) (store.IdleReleaseRow, error) {
			row, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
				Role: role, Generation: generation}, time.Now())
			if err != nil {
				return store.IdleReleaseRow{}, err
			}
			return store.IdleReleaseRow{Kind: string(row.Kind), Payload: row.Payload}, nil
		}
		if _, err := st.CancelIssueRelease(context.Background(), wakeProjectToken(t), "LEGION-208", resume, record.OutOfWorkflow,
			store.ReleaseCancelPause{At: time.Now()}); err != nil {
			t.Fatalf("cancel the release as the outbox does: %v", err)
		}

		// The cancel's resume carries no task here, as the builder above writes it.
		if starts := wakeStarts(t, rig.pool); len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != "" {
			t.Errorf("supervise start rows = %+v, want the cancel's one start of the implementer, with no task and none that begins %q: the release was cancelled (cause %s), so its pod never went away",
				starts, wakePreamble, decision.Cause)
		}
		if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
			t.Errorf("pod resumes = %v, want none: the release was cancelled (cause %s), so no released pod came back", resumes, decision.Cause)
		}
	})
}
