package evals

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// changesCursor is where the refresher's change cursor stands, 0 where no pass has committed.
func (r *refresherRig) changesCursor(t *testing.T) int {
	t.Helper()
	return r.count(t, `select coalesce((select position from eval_days_cursor where name = $1), 0)`, changesCursor)
}

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
		refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds, &gapBatch{}); err == nil {
		t.Fatal("the pass the test fails answered no error")
	}
}

// TestAFirstDayMovedEarlierLeavesNoDayWithoutARowThoughThePassFails: a pull request of the main
// project merged again six days before pr-open-to-merge's first day moves that eval's first input
// day earlier, and the days between the two need zero rows. The page writes the new first day,
// the day the pull request left and those zero rows in its one commit, with its cursors
// (refresh.writeNamed). The test fails the first write of one of the zero rows: the commit fails
// whole, so the pass leaves every day row, the delivery walk's record and the change cursor as
// they stood, and the pass after it, which reads the same change again, leaves every day from the
// new first day to today with a row. Before, the new first day was written in a transaction of
// its own, which moved the walk back, and the walk wrote the zero rows.
func TestAFirstDayMovedEarlierLeavesNoDayWithoutARowThoughThePassFails(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
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
	before, record, cursor := r.dayRows(t), r.walkRecord(t), r.changesCursor(t)
	r.exec(t, "merge "+member+" again six days before the first day", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour')
		where repo = $1 and number = $2
	`, repo, number, moved.Add(12*time.Hour))
	r.failingPass(t)
	if lines, described := rewritten(before, r.dayRows(t)); len(lines) > 0 {
		t.Errorf("the pass that failed wrote the days %v, want none: its days go in one commit, which failed\n\t%s", lines, strings.Join(described, "\n\t"))
	}
	if got := r.walkRecord(t); got != record {
		t.Errorf("the delivery walk's record is %q after the pass that failed, want it as it stood, %q", got, record)
	}
	if got := r.changesCursor(t); got != cursor {
		t.Errorf("the change cursor stands at %d after the pass that failed, want it where it stood, %d", got, cursor)
	}
	r.pass(t)
	if got := r.walkRecord(t); got != record {
		t.Errorf("the delivery walk's record is %q after the pass that wrote the days, want it as it stood, %q", got, record)
	}
	after, n := seriesDays(r.dayRows(t), main, SlugPROpenToMerge, measures.SeriesMinutes)
	if missing, gap := contiguous(after, refresherToday); len(after) == 0 || !after[0].Equal(moved) || gap {
		t.Errorf("pr-open-to-merge of %s holds rows on %v, missing %s; want every day from %s to today", main, after, missing.Format(time.DateOnly), moved.Format(time.DateOnly))
	}
	t.Logf("the first day moved from %s to %s; the gap's day after it holds n %d", first.Format(time.DateOnly), moved.Format(time.DateOnly), n[moved.AddDate(0, 0, 1)])
}

// TestARunAPullRequestLeftIsRewrittenThoughThePassFailsBeforeIt: a pull request of the deploy
// repository, merged a day earlier, ships with another deploy run, and the run that shipped it
// before counts it in its own day's rows until that day is written again. The pull request names an
// issue of the main project, which it counts for through that issue (one that names none counts for
// no project): where the history's names none, it is given one before the history settles. The test
// fails the first write of that run's day: the pass fails after the write that moved the pull
// request's member to the new run, and no stored pull request names the old run any more. The pass
// after it, reading the same change again, still rewrites the old run's day, which the moving write
// recorded owed; it leaves no day owed.
func TestARunAPullRequestLeftIsRewrittenThoughThePassFailsBeforeIt(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
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
	if !namesIssueOf(deliveries, picked) {
		r.exec(t, "name an issue of "+main+" on "+PullRequestMember(picked.Repo, picked.Number), `
			update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2
		`, picked.Repo, picked.Number, deliveries.Issues[main][0])
	}
	r.settle(t)
	r.pass(t)
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
	t.Parallel()
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

// pageAlone makes a pass's pages and no walk, as a boot that commits the refresher's cursors and
// stops before its walk does: it moves the pending changes into the log, loads what a pass loads,
// and reads the feeds page after page, writing the days they name, until one comes back short. It
// answers what the pages wrote.
func (r *refresherRig) pageAlone(t *testing.T) refreshed {
	t.Helper()
	ctx := t.Context()
	if _, err := r.horizon.Await(ctx); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the refresher's pass: %v", err)
	}
	safe, err := r.horizon.SafeOrAwait(ctx)
	if err != nil {
		t.Fatalf("read the event log's safe horizon: %v", err)
	}
	for {
		more, err := pass.page(ctx, safe, refresherPage)
		if err != nil {
			t.Fatalf("the refresher's page failed: %v", err)
		}
		if !more {
			return pass.report
		}
	}
}

// TestAChangeBeforeTheFirstWalkWritesNoZeroRowOnADayWithInput: over the delivery and Legion
// histories, a boot commits the refresher's cursors and stops before its walk (pageAlone), as the
// migrating boot of dispatch://INFRA-2916's k12 drive did, so every walk is to begin and no day row
// exists. A change then opens the main project's first merged pull request an hour earlier, and the
// page that reads it writes that pull request's merge day and the days it links to from their own
// input: no other day holds a row, so each later day with input reads "no data" until the walk
// computes it, and no row the page wrote is a zero row of a day the walk then writes with input.
// A page that wrote the zero rows of every day after the merge day through today, days with input
// among them, would leave those days reading 0 until the walk reached them. Once the walk has run,
// the database holds the delivery and CI rows, the members and the stored figures
// of a second database that made the same change before its first walk (expectSameAsWalk).
func TestAChangeBeforeTheFirstWalkWritesNoZeroRowOnADayWithInput(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	if report := r.pageAlone(t); len(report.days) != 0 {
		t.Fatalf("the boot's pages wrote %v, and the check needs none: a page with no cursor writes no day", report.sorted())
	}
	if rows := r.count(t, `select count(*) from eval_days`); rows != 0 {
		t.Fatalf("the boot left %d day rows, and the check needs none", rows)
	}
	main := deliveries.Project
	projects := map[string]string{}
	for project, keys := range deliveries.Issues {
		for _, key := range keys {
			projects[key] = project
		}
	}
	var opened *delivery.DeliveryPullRequest
	for i, pr := range deliveries.PullRequests {
		counts := pr.IssueKey != nil && projects[*pr.IssueKey] == main
		if counts && pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial && slices.Contains(deliveries.Settings.PopulationAuthors, pr.Author) &&
			(opened == nil || pr.MergedAt.Before(*opened.MergedAt)) {
			opened = &deliveries.PullRequests[i]
		}
	}
	if opened == nil {
		t.Fatalf("the history holds no merged pull request of %s", main)
	}
	day := measures.Midnight(*opened.MergedAt)
	open := func(rig *refresherRig) {
		t.Helper()
		rig.exec(t, "open "+PullRequestMember(opened.Repo, opened.Number)+" an hour earlier", `
			update delivery_pull_requests set created_at = created_at - interval '1 hour' where repo = $1 and number = $2
		`, opened.Repo, opened.Number)
	}
	open(r)
	report := r.pageAlone(t)
	written := map[string]bool{}
	for _, one := range report.sorted() {
		written[one] = true
	}
	if !written[line(walkDelivery, main, day)] {
		t.Fatalf("the page wrote %v, and not %s's merge day %s", report.sorted(), main, day.Format(time.DateOnly))
	}
	paged := r.dayRows(t)
	for key, row := range paged {
		if !written[dayLine(row)] {
			t.Errorf("%s holds n %d before the walk has computed its day, which no change named", key, row.N)
		}
	}

	r.settle(t)
	walked := r.dayRows(t)
	later := 0
	for key, row := range paged {
		if computed, held := walked[key]; row.N == 0 && held && computed.N > 0 {
			t.Errorf("%s read 0 before the walk computed its day, which holds input: the walk wrote n %d", key, computed.N)
		}
	}
	for key, row := range walked {
		if _, held := paged[key]; !held && row.Repo == "" && row.N > 0 && row.Day.After(day) && familyOf(row) == walkDelivery {
			later++
		}
	}
	if later == 0 {
		t.Errorf("the walk wrote no row with input of a day after %s the page had not written, and the check needs some", day.Format(time.DateOnly))
	}
	walk := newRefresherRig(t)
	walk.seedHistories(t)
	walk.leaveInputsUnread(t)
	open(walk)
	walk.settle(t)
	r.expectSameAsWalk(t, "the change read before the first walk", walk)
	t.Logf("the page wrote %v: %d rows; the walk then wrote %d rows with input of later days the page had left with none",
		report.sorted(), len(paged), later)
}
