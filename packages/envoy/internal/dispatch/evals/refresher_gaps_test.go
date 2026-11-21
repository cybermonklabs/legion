package evals

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// failOnce makes the first insert into eval_days of a row when holds fail, and every later one
// pass: a sequence is no part of the transaction that reads it, so the failure is spent once
// whatever rolls back.
func (r *refresherRig) failOnce(t *testing.T, when string) {
	t.Helper()
	r.exec(t, "count the failures", `create sequence evals_test_failures`)
	r.exec(t, "fail one write", `create function evals_test_fail_once() returns trigger language plpgsql as $$
		begin
			if nextval('evals_test_failures') = 1 then
				raise exception 'the test fails this write once';
			end if;
			return new;
		end $$`)
	r.exec(t, "fail one write", `create trigger evals_test_fail_once before insert or update on eval_days
		for each row when (`+when+`) execute function evals_test_fail_once()`)
}

// failingPass makes one refresher pass as the rig's pass does, and fails the test unless the pass
// fails.
func (r *refresherRig) failingPass(t *testing.T) {
	t.Helper()
	if _, err := r.horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	if _, _, err := r.registry.refresh(t.Context(), RefresherDeps{Horizon: r.horizon, Build: r.build},
		refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds); err == nil {
		t.Fatal("the pass the test fails answered no error")
	}
}

// TestAFirstDayMovedEarlierLeavesNoDayWithoutARowThoughThePassFails: a pull request of the main
// project merged again six days before pr-open-to-merge's first day moves that eval's first input
// day earlier, and the days between the two need zero rows, which the write of the new first day
// writes in its own transaction. The test fails the first write of one of those zero rows: the
// pass fails, and the pass after it, which reads the same change again and the new first day from
// the store, leaves every day from the new first day to today with a row. Written in a later
// transaction of the pass, the zero rows were lost with the failure: the retry found the eval's
// first day already moved, and no gap behind it.
func TestAFirstDayMovedEarlierLeavesNoDayWithoutARowThoughThePassFails(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	main := deliveries.Project
	days, _ := seriesDays(r.dayRows(t), main, SlugPROpenToMerge, measures.SeriesMinutes)
	if len(days) == 0 {
		t.Fatalf("%s holds no pr-open-to-merge row", main)
	}
	first := days[0]
	moved := first.AddDate(0, 0, -6)
	var member string
	for _, day := range r.deliveryDays(t) {
		for _, pr := range day.prs {
			if pr.project == main && member == "" {
				member = pr.member
			}
		}
	}
	if member == "" {
		t.Fatalf("the history holds no pull request of %s", main)
	}
	repo, number := memberPR(t, member)
	r.failOnce(t, "new.eval_id = '"+EvalID(main, SlugPROpenToMerge)+"' and new.day > '"+moved.Format(time.DateOnly)+
		"'::date and new.day < '"+first.Format(time.DateOnly)+"'::date")
	r.exec(t, "merge "+member+" again six days before the first day", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour')
		where repo = $1 and number = $2
	`, repo, number, moved.Add(12*time.Hour))
	r.failingPass(t)
	r.pass(t)
	after, n := seriesDays(r.dayRows(t), main, SlugPROpenToMerge, measures.SeriesMinutes)
	if missing, gap := contiguous(after, refresherToday); len(after) == 0 || !after[0].Equal(moved) || gap {
		t.Errorf("pr-open-to-merge of %s holds rows on %v, missing %s; want every day from %s to today", main, after, missing.Format(time.DateOnly), moved.Format(time.DateOnly))
	}
	t.Logf("the first day moved from %s to %s; the gap's day after it holds n %d", first.Format(time.DateOnly), moved.Format(time.DateOnly), n[moved.AddDate(0, 0, 1)])
}

// TestARunAPullRequestLeftIsRewrittenThoughThePassFailsBeforeIt: a pull request of the deploy
// repository merged a day earlier ships with another deploy run, and the run that shipped it
// before counts it in its own day's rows until that day is written again. The test fails the first
// write of that run's day: the pass fails after the write that moved the pull request's member to
// the new run, and no stored pull request names the old run any more. The pass after it, reading
// the same change again, still rewrites the old run's day, which the moving write recorded owed;
// it leaves no day owed.
func TestARunAPullRequestLeftIsRewrittenThoughThePassFailsBeforeIt(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	main := deliveries.Project
	var picked delivery.DeliveryPullRequest
	var oldDay, merged time.Time
	for _, pr := range deliveries.PullRequests {
		if pr.Repo != deliveries.DeployRepo || pr.MergedAt == nil || picked.Number != 0 {
			continue
		}
		old := delivery.FirstShippingApply(*pr.MergedAt, deliveries.Applies)
		earlier := pr.MergedAt.AddDate(0, 0, -1)
		moved := delivery.FirstShippingApply(earlier, deliveries.Applies)
		if old == nil || moved == nil || moved.RunID == old.RunID {
			continue
		}
		day := runStart(t, deliveries.Runs, old.RunID)
		if day.Equal(measures.Midnight(*pr.MergedAt)) || day.Equal(measures.Midnight(earlier)) || day.Equal(runStart(t, deliveries.Runs, moved.RunID)) {
			continue
		}
		picked, oldDay, merged = pr, day, earlier
	}
	if picked.Number == 0 {
		t.Fatal("the history holds no pull request of the deploy repository that a day's earlier merge moves to another run starting on another day")
	}
	r.failOnce(t, "new.project_key = '"+main+"' and new.day = '"+oldDay.Format(time.DateOnly)+"'::date")
	r.exec(t, "merge the pull request a day earlier", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour') where repo = $1 and number = $2
	`, picked.Repo, picked.Number, merged)
	r.failingPass(t)
	report, _ := r.pass(t)
	if !slices.Contains(report.sorted(), line(walkDelivery, main, oldDay)) {
		t.Errorf("the pass after the failure wrote %v, want the day of the run %s left, %s", report.sorted(), PullRequestMember(picked.Repo, picked.Number), oldDay.Format(time.DateOnly))
	}
	if owed := r.count(t, `select count(*) from eval_days_owed`); owed != 0 {
		t.Errorf("%d days are still owed a rewrite after the pass, want none", owed)
	}
}

// TestADayAnIssueLeftIsRewrittenThoughThePassFailsBeforeIt: over the hand-written Legion history
// of EVAL, EVAL-3, closed on the history's third day, is closed again a day earlier. The pass
// writes the earlier day first, which moves EVAL-3's member to it, then the day it left; the test
// fails the first write of the day it left, so the pass fails with no stored member naming that
// day. The pass after it, reading the same change again, still rewrites the day EVAL-3 left, which
// the moving write recorded owed, and that day no longer counts EVAL-3.
func TestADayAnIssueLeftIsRewrittenThoughThePassFailsBeforeIt(t *testing.T) {
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	r.failOnce(t, "new.project_key = 'EVAL' and new.day = '"+history.d2.Format(time.DateOnly)+"'::date")
	row := history.rows["EVAL-3"]
	row.ClosedAt = new(history.d1.Add(14 * time.Hour))
	history.rows["EVAL-3"] = row
	r.writeFacts(t, history, "EVAL-3")
	r.failingPass(t)
	report, _ := r.pass(t)
	if !slices.Contains(report.sorted(), line(walkLegion, "EVAL", history.d2)) {
		t.Errorf("the pass after the failure wrote %v, want the day EVAL-3 left, %s", report.sorted(), history.d2.Format(time.DateOnly))
	}
	if _, n := seriesDays(r.dayRows(t), "EVAL", SlugLegionWorkTime, "hours"); n[history.d2] != 0 || n[history.d1] != 3 {
		t.Errorf("work time counts %d issues on %s and %d on %s, want EVAL-3 moved: 0 and 3",
			n[history.d2], history.d2.Format(time.DateOnly), n[history.d1], history.d1.Format(time.DateOnly))
	}
	if owed := r.count(t, `select count(*) from eval_days_owed`); owed != 0 {
		t.Errorf("%d days are still owed a rewrite after the pass, want none", owed)
	}
}
