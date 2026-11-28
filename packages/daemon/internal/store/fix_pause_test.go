package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// fixWorkers is the workers of an issue's open phase as workflow.OpenRolesOf names them for the
// phases these tests use: the reviewer while the issue reviews, and the implementer beside it
// where a fix runs.
func fixWorkers(p phase.Phase, fixRunning bool) []claim.Role {
	var roles []claim.Role
	if p == phase.Reviewing {
		roles = append(roles, claim.RoleReviewer)
	}
	if fixRunning {
		roles = append(roles, claim.RoleImplementer)
	}
	return roles
}

// cancelPauseOf is what a cancel made while the project is paused is handed: the moment, the
// backstop, and roles as the workflow's rule of the workers of an issue's open phase.
func cancelPauseOf(at time.Time, backstop time.Duration, roles func(phase.Phase, bool) []claim.Role) ReleaseCancelPause {
	return ReleaseCancelPause{At: at, Backstop: backstop, OpenRoles: roles}
}

// The idle view of a cancel made while the project is paused reads, beside the issue's phase,
// whether a fix runs beside its step (migrations.RunningFixSQL), and hands both to the workflow's
// rule of the open phase's workers, so the stop it records of the fix's implementer arms that
// role's backstop as the reviewer's stop arms the reviewer's. The control, kept: a fix that still
// reads its failed job runs at no entry, and the reviewer's backstop is armed alone.
func TestAPauseBackstopArmsForTheFix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		implementer int
	}{
		{name: "a fix runs beside the review", state: "running", implementer: 1},
		{name: "the control: a fix that reads its job runs at no entry", state: "reading", implementer: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			openRunningTree(t, st)
			if _, err := st.Pool().Exec(ctx, `update issues set phase = 'reviewing' where key = $1`, rootIssue); err != nil {
				t.Fatalf("stand %s in reviewing: %v", rootIssue, err)
			}
			if _, err := st.Pool().Exec(ctx, `insert into task_fixes (issue, generation, task, fix, state, check_id, head, place, entry)
				values ($1, 1, 'a', 1, $2, 'k-ci', 'c0de', 1, 3)`, rootIssue, tc.state); err != nil {
				t.Fatalf("seed fix 1 of %s %s: %v", rootIssue, tc.state, err)
			}
			servingReleasedClaim(t, st, claim.RoleImplementer)
			servingReleasedClaim(t, st, claim.RoleReviewer)
			markRelease(t, st, 18, 20, 22, 0, claim.RoleImplementer, claim.RoleReviewer)
			pause, _ := pauseLegion(t, st, pausedAt)
			at := pausedAt.Add(time.Minute)

			cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, cancelPauseOf(at, time.Hour, fixWorkers))
			if err != nil {
				t.Fatalf("cancel the release while paused: %v", err)
			}
			if cancelled.Pause != pause.ID || len(cancelled.Resumed) != 0 ||
				!slices.Equal(cancelled.Stopped, []claim.Role{claim.RoleImplementer, claim.RoleReviewer}) {
				t.Fatalf("the cancel = %+v; want pause %d, no role resumed and both stopped", cancelled, pause.ID)
			}
			if n := pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1 and role = 'reviewer'
				and not fired and due_at = $2`, rootIssue, at.Add(time.Hour)); n != 1 {
				t.Fatalf("the reviewer's backstops due an hour after the cancel = %d, want 1", n)
			}
			if n := pauseCount(t, st, `select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer'
				and not fired and due_at = $2`, rootIssue, at.Add(time.Hour)); n != tc.implementer {
				t.Fatalf("the implementer's backstops due an hour after the cancel = %d, want %d", n, tc.implementer)
			}
		})
	}
}
