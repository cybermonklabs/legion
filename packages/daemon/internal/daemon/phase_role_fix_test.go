package daemon

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The role the pod start gate puts first (supervise's Deps.PhaseRole) is the first of the issue's
// open roles (workflow.OpenRoles): the worker of its phase where the phase has one, as before, and
// the implementer of a running fix where the phase has none, as while a fix runs beside the
// owner's acceptance. An issue with no running fix is answered as before: the worker of its phase,
// or none. The control, kept: a fix that still reads its failed job runs at no entry, so an issue
// in accepting with such a fix still has no role first.
func TestThePodStartGatePutsTheFixsImplementerFirstWhereThePhaseHasNoWorker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		phase phase.Phase
		fix   string
		want  claim.Role
	}{
		{name: "reviewing with no fix: the reviewer", phase: phase.Reviewing, want: claim.RoleReviewer},
		{name: "reviewing with a running fix: the reviewer still", phase: phase.Reviewing, fix: "running", want: claim.RoleReviewer},
		{name: "accepting with no fix: none", phase: phase.Accepting, want: ""},
		{name: "accepting with a running fix: the fix's implementer", phase: phase.Accepting, fix: "running", want: claim.RoleImplementer},
		{name: "the control: accepting with a fix that reads its job: none", phase: phase.Accepting, fix: "reading", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := podIssueRecord()
			issue.Phase = tc.phase
			putOutboxIssue(t, pool, records, issue)
			if tc.fix != "" {
				if _, err := pool.Exec(ctx, `insert into task_fixes (issue, generation, task, fix, state, check_id, head, place, entry)
					values ($1, 1, 'a', 1, $2, 'k-ci', 'c0de', 1, 3)`, pauseIssue, tc.fix); err != nil {
					t.Fatalf("seed fix 1 of %s %s: %v", pauseIssue, tc.fix, err)
				}
			}
			w := &workflowRuntime{pool: pool, records: records}
			got, err := w.phaseRole(ctx, pauseIssue)
			if err != nil {
				t.Fatalf("phaseRole(%s): %v", pauseIssue, err)
			}
			if got != tc.want {
				t.Errorf("phaseRole(%s) in %s with fix %q = %q, want %q", pauseIssue, tc.phase, tc.fix, got, tc.want)
			}
		})
	}
}
