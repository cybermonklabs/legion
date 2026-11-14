package evals

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestAChangeLandingBetweenTwoPagesOfAPassIsWritten: a pass reads its feeds a page at a time, and a
// day one page wrote is written again by a later page of the same pass when a change to it
// committed after the first page read it. Over the hand-written Legion history of EVAL, EVAL-1's
// hours move and a P0 issue opens, and a page of one change and one event writes the day EVAL-1
// closed on and the unowned-p0 row of today; then EVAL-2, which closed on the same day, moves and
// a second P0 issue opens, and the pages after it, of the same pass, rewrite both: the day holds
// both issues' new hours and today's count is 2.
func TestAChangeLandingBetweenTwoPagesOfAPassIsWritten(t *testing.T) {
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	moveHours := func(key string, hours float64) {
		t.Helper()
		row := history.rows[key]
		row.WorkHours = hours
		history.rows[key] = row
		r.writeFacts(t, history, key)
	}
	openP0 := func(number int) {
		t.Helper()
		key := fmt.Sprintf("EVAL-%d", number)
		r.exec(t, "open the P0 issue "+key, `
			insert into issues (key, project_key, number, title, status, rank, priority, created_by)
			values ($1, 'EVAL', $2, 'Broken', 'triage', $3, 0, '{"kind":"user","id":"alice"}')
		`, key, number, fmt.Sprintf("%08d", number))
		r.writeIssueEvent(t, key, "issue.created", fmt.Sprintf(`{"key":%q,"priority":0,"status":"triage","changed":["title","status","priority"]}`, key))
	}
	safe := func() int64 {
		t.Helper()
		safe, err := r.horizon.Await(ctx)
		if err != nil {
			t.Fatalf("await the event log's safe horizon: %v", err)
		}
		return safe
	}

	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	if err := pass.load(ctx); err != nil {
		t.Fatalf("load the pass: %v", err)
	}
	moveHours("EVAL-1", 7)
	openP0(50)
	more, err := pass.page(ctx, safe(), 1)
	if err != nil || !more {
		t.Fatalf("the first page answered more %t, %v; want a full page", more, err)
	}
	first := pass.report.sorted()
	moveHours("EVAL-2", 9)
	openP0(51)
	pages := 1
	for more {
		if more, err = pass.page(ctx, safe(), 1); err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
	}

	hours := r.day(t, EvalID("EVAL", SlugLegionWorkTime), metrics.SeriesHours, "", history.d1)
	unowned := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", refresherToday)
	samples := slices.Sorted(slices.Values(hours.Samples))
	if !slices.Equal(samples, []float64{7, 9}) || unowned.LastValue == nil || *unowned.LastValue != 2 {
		t.Errorf("after %d pages the work time of %s holds %v and today's unowned-p0 count %v; want EVAL-1's 7 and EVAL-2's 9 hours, and 2",
			pages, history.d1.Format("2006-01-02"), samples, showValue(unowned.LastValue))
	}
	t.Logf("the first page wrote %v; %d pages in all; the day holds %v hours, today's count %v", first, pages, samples, showValue(unowned.LastValue))
}

// TestAPrunedCursorChangeIsNoLossAndAVanishedYoungOneIs: once every walk is done, the change the
// changes cursor rests on passes its 24 hours and is pruned, as the refresher's own prune takes it
// on a day with no newer change, and the next pass begins no walk again and writes no day. The
// change the cursor rests on leaving the log younger than its retention, as a reset log or a
// delete by hand takes it, is a loss no one can name, and the pass after it begins every walk
// again, which writes the days of each.
func TestAPrunedCursorChangeIsNoLossAndAVanishedYoungOneIs(t *testing.T) {
	r := newRefresherRig(t)
	r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	cursors, err := r.registry.readRefresherCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resting := cursors[changesCursor]
	if resting.position == 0 || resting.at == nil {
		t.Fatalf("the changes cursor rests at %d recorded %v, want a change", resting.position, resting.at)
	}

	r.exec(t, "age the log past its retention", `update delivery_changes set at = now() - interval '25 hours' where seq <= $1`, resting.position)
	r.exec(t, "age the cursor's change with it", `update eval_days_cursor set at = now() - interval '25 hours' where name = $1`, changesCursor)
	pruned, err := delivery.PruneChanges(ctx, r.database.Pool)
	if err != nil {
		t.Fatalf("prune the log: %v", err)
	}
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Errorf("the pass after the cursor's change was pruned in its time wrote %v, want nothing: no walk begins again", report.sorted())
	}

	r.exec(t, "log a change", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'young')`)
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Fatalf("the pass reading a session group wrote %v, want nothing", report.sorted())
	}
	r.exec(t, "delete the young change", `delete from delivery_changes where ref = 'young'`)
	lost, _ := r.pass(t)
	if !slices.Contains(lost.sorted(), line(walkLegion, "EVAL", legionHistoryDay(2))) {
		t.Errorf("the pass after the cursor's young change vanished wrote %v, want every walk begun again, EVAL's Legion days among them", lost.sorted())
	}
	t.Logf("pruned %d changes through the cursor's %d: the pass after wrote nothing; a young change vanished: the pass after wrote %d days",
		pruned, resting.position, len(lost.days))
}

// TestTheUnownedP0RowOfEachDayIsWrittenWithNoDeliverySettings: with no delivery settings, so no
// delivery walk, a project's unowned-p0 row of today is written by the first pass of each UTC day,
// since the count reads issues alone, and by no later pass of that day.
func TestTheUnownedP0RowOfEachDayIsWrittenWithNoDeliverySettings(t *testing.T) {
	r := newRefresherRig(t)
	if _, err := delivery.GetSettings(t.Context(), r.database.Pool); !errors.Is(err, delivery.ErrNoSettings) {
		t.Fatalf("the store holds delivery settings (%v), want none", err)
	}
	r.exec(t, "open a P0 issue", `
		insert into issues (key, project_key, number, title, status, rank, priority, created_by)
		values ('EVAL-50', 'EVAL', 50, 'Broken', 'triage', '00000050', 0, '{"kind":"user","id":"alice"}')`)
	r.settle(t)
	evalID := EvalID("EVAL", SlugUnownedP0)
	for day := 1; day <= 3; day++ {
		today := refresherToday.AddDate(0, 0, day)
		r.now = refresherNow.AddDate(0, 0, day)
		first, _ := r.pass(t)
		again, _ := r.pass(t)
		count := r.day(t, evalID, SeriesUnownedP0, "", today)
		if want := line(SlugUnownedP0, "EVAL", today); !slices.Contains(first.sorted(), want) || slices.Contains(again.sorted(), want) {
			t.Errorf("on %s the first pass wrote %v and the second %v, want %s by the first alone", today.Format("2006-01-02"), first.sorted(), again.sorted(), want)
		}
		if count.LastValue == nil || *count.LastValue != 1 {
			t.Errorf("the unowned-p0 row of %s holds %v, want 1", today.Format("2006-01-02"), showValue(count.LastValue))
		}
	}
}

// TestTheShippedMembersReadKeepsItsPartialIndexUnderAGenericPlan: the read of the pull requests a
// run shipped (shippedMembers) reads eval_day_members through eval_day_members_shipped, the
// partial index of the members a run shipped, under the generic plan pgx's cached statement
// settles on, which knows no value of the runs it is executed with. 5,000 members, one in ten
// shipped by a run, are analyzed first, and sequential scans are off, so the plan names the index
// the statement can prove and nothing else.
func TestTheShippedMembersReadKeepsItsPartialIndexUnderAGenericPlan(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.database.Pool.Exec(ctx, `
		insert into eval_day_members (project_key, family, row_key, day, repo, shipped_by)
		select 'EVAL', 'delivery', 'pr:acme/web#' || n, date '2026-11-01' + n % 30, 'acme/web',
		       case when n % 10 = 0 then 'run:acme/deploy/' || n / 10 else '' end
		from generate_series(1, 5000) as n
	`); err != nil {
		t.Fatalf("seed the members: %v", err)
	}
	if _, err := r.database.Pool.Exec(ctx, `analyze eval_day_members`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	connection, err := r.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plan: %v", err)
	}
	defer connection.Release()
	plan := explaintest.GenericPlan(t, ctx, connection, "shipped_members", shippedMembersQuery,
		[]string{"text[]", "text[]", "text"}, []string{"array['EVAL']", "array['run:acme/deploy/7']", "'" + KindDelivery + "'"},
		"set local enable_seqscan = off")
	scans := explaintest.IndexScans(t, plan)
	if !slices.ContainsFunc(scans, func(scan explaintest.IndexScan) bool { return scan.Index == "eval_day_members_shipped" }) {
		t.Fatalf("the generic plan of the shipped members read reads %v, want eval_day_members_shipped:\n%s", scans, plan)
	}
	t.Logf("the generic plan reads %v", scans)
}
