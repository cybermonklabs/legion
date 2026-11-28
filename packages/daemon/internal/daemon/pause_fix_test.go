package daemon

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pause's stop of the implementer that runs a fix beside the review arms the implementer's
// backstop, as the stop of the reviewer, the worker of the issue's phase, arms the reviewer's:
// both are workers of the issue's open phase while the fix runs (workflow.OpenRoles), and the
// record names the fix (record.Issue.RunningFix). The control, kept: a fix that still reads its
// failed job runs at no entry, so the reviewer's backstop is armed alone.
func TestAPauseStopArmsTheFixsBackstop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		state       string
		implementer int
	}{
		{name: "a fix runs beside the review", state: "running", implementer: 1},
		{name: "the control: a fix that reads its job runs at no entry", state: "reading", implementer: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newPauseRig(t, claim.RoleImplementer, claim.RoleReviewer)
			if _, err := r.pool.Exec(r.ctx, `update issues set phase = 'reviewing' where key = $1`, pauseIssue); err != nil {
				t.Fatalf("stand %s in reviewing: %v", pauseIssue, err)
			}
			if _, err := r.pool.Exec(r.ctx, `insert into task_fixes (issue, generation, task, fix, state, check_id, head, place, entry)
				values ($1, 1, 'a', 1, $2, 'k-ci', 'c0de', 1, 3)`, pauseIssue, tc.state); err != nil {
				t.Fatalf("seed fix 1 of %s %s: %v", pauseIssue, tc.state, err)
			}
			held := r.pauseLegion()

			r.mustStop(r.claim(claim.RoleImplementer, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})
			r.mustStop(r.claim(claim.RoleReviewer, supervise.StateSuspended), supervise.PauseStop{Pause: held.ID})

			if got := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'reviewer' and not fired`, pauseIssue); got != 1 {
				t.Fatalf("the reviewer's backstops = %d, want the one its stop armed", got)
			}
			if got := r.count(`select count(*) from pod_wake_backstops where issue = $1 and role = 'implementer' and not fired`, pauseIssue); got != tc.implementer {
				t.Fatalf("the implementer's backstops = %d, want %d", got, tc.implementer)
			}
		})
	}
}
