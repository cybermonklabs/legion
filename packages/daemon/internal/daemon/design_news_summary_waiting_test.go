package daemon

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The root architect's summary says what is true of each member's delivery when it is rendered
// (dispatch://INFRA-175, the INFRA-138 architect's decision on implement handoff v1's third open
// question). A member in accepting owned by a running sub-architect is told by that
// sub-architect's news-only start, and the reconcile queues no such start while the sub-architect's
// claim holds a task its worker has not taken (holdsUntakenTask): its news waits. So the summary
// does not say that a task tells it now. The control is the same tree once a turn has taken that
// task: the reconcile queues the sub-architect's start, and the summary says a task tells it now.
func TestTheRootArchitectsSummarySaysAMemberWaitsWhileItsSubArchitectHoldsAnUntakenTask(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
	}{
		{name: "a task the sub-architect has not taken"},
		{name: "a task a turn of the sub-architect took", confirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Implementing)
			r.member(reconcileChild, reconcileRoot, phase.Implementing)
			r.member(reconcileGrandchild, reconcileChild, phase.Accepting)
			r.told(reconcileRoot, record.AudienceHolder, 5)
			r.told(reconcileRoot, record.AudienceArchitect, 4)
			r.told(reconcileChild, record.AudienceHolder, 5)
			r.told(reconcileGrandchild, record.AudienceHolder, 4)
			r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
			sub := r.holder(reconcileChild, claim.RoleArchitect, supervise.StateWorking, "", 0)
			r.inTx(func(tx pgx.Tx) error {
				_, err := tx.Exec(r.ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
					values ($1, 'outbox:7', 'Accept the child.', now(), 1, '', case when $2 then now() end)`, string(sub), tc.confirmed)
				return err
			})

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			var summary *entryStartRow
			subQueued := false
			for i, start := range starts {
				switch start.row.Issue {
				case reconcileRoot:
					summary = &starts[i]
				case reconcileChild:
					subQueued = true
				}
			}
			if subQueued != tc.confirmed {
				t.Fatalf("the news-only starts = %v, want the sub-architect's queued: %t", startsTo(starts), tc.confirmed)
			}
			if summary == nil {
				t.Fatalf("the news-only starts = %v, want the root architect's summary", startsTo(starts))
			}

			machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
			if err := r.runner.execute(r.ctx, summary.row); err != nil {
				t.Fatalf("run the root architect's news-only start: %v", err)
			}
			task, _ := pendingNews(t, machine)
			const prefix = "- LEGION-602, in accepting: "
			var line string
			for _, candidate := range strings.Split(task, "\n") {
				if strings.HasPrefix(candidate, prefix) {
					line = candidate
				}
			}
			if line == "" {
				t.Fatalf("the root architect's summary = %q, want a line for LEGION-602", task)
			}
			if tells := strings.Contains(line, "a task tells"); tells != tc.confirmed {
				t.Fatalf("the summary's line for LEGION-602 = %q, want it to say a task tells it now: %t", line, tc.confirmed)
			}
			if strings.Contains(line, "You hold it in accepting") {
				t.Fatalf("the summary's line for LEGION-602 = %q, want no answer of the root architect's: a running sub-architect owns it", line)
			}
		})
	}
}
