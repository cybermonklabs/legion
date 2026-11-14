package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// TestTheLoggedZeroFillWritesEveryMissingDayAndNoOther: the fill of a logged eval's zero rows
// (fillLoggedZeros) writes each day from a series' first row to today that holds no row of it,
// between two rows as after the newest, each with the listing of the series' newest row, and no
// other day: none before a series' first row, none after today though a row of a later day
// exists, and none of an eval the fill's ids leave out. A second fill writes nothing, and a fill
// of every logged eval fills the one left out.
func TestTheLoggedZeroFillWritesEveryMissingDayAndNoOther(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	// The days are October 2026's, every one in the past, so the readings write takes the reading
	// of a day after the fill's today as it takes the others (R18 refuses a measured_at more than
	// a day past the server's time), and the fill runs as of the 20th.
	on := func(day, hour int) time.Time { return time.Date(2026, time.October, day, hour, 0, 0, 0, time.UTC) }
	reading := func(series, key string, measured time.Time) json.RawMessage {
		return row(t, map[string]any{"series": series, "row_kind": "run", "row_key": key, "value": 1, "measured_at": measured})
	}
	r.write(t, "build-minutes", alice, reading("minutes", "a", on(3, 9)), reading("minutes", "b", on(8, 9)),
		reading("minutes", "c", on(12, 9)), reading("minutes", "d", on(25, 9)), reading("cache", "e", on(15, 9)))
	r.write(t, "test-minutes", alice, reading("minutes", "f", on(10, 9)))
	built, other := EvalID(r.project, "build-minutes"), EvalID(r.project, "test-minutes")
	// The newest row of minutes is unlisted, which each zero row of minutes takes.
	if _, err := r.database.Pool.Exec(ctx, `update eval_days set listed = false where eval_id = $1 and series = 'minutes' and day = $2`, built, on(25, 0)); err != nil {
		t.Fatalf("unlist the newest row: %v", err)
	}
	zeroDays := func(evalID, series string) ([]int, []bool) {
		t.Helper()
		rows, err := r.database.Pool.Query(ctx, `
			select extract(day from day)::int, listed from eval_days
			where eval_id = $1 and series = $2 and repo = '' and n = 0 order by day
		`, evalID, series)
		if err != nil {
			t.Fatalf("read the zero rows of %s %s: %v", evalID, series, err)
		}
		var days []int
		var listed []bool
		var day int
		var list bool
		if _, err := pgx.ForEachRow(rows, []any{&day, &list}, func() error {
			days, listed = append(days, day), append(listed, list)
			return nil
		}); err != nil {
			t.Fatalf("read the zero rows of %s %s: %v", evalID, series, err)
		}
		return days, listed
	}
	span := func(from, to int) []int {
		var days []int
		for day := from; day <= to; day++ {
			days = append(days, day)
		}
		return days
	}

	refresh := r.registry.newRefresh(testBuild, on(20, 12), walkBatch)
	filled, err := refresh.fillLoggedZeros(ctx, r.project, []string{built})
	if err != nil {
		t.Fatalf("fill build-minutes: %v", err)
	}
	minutes, minutesListed := zeroDays(built, "minutes")
	cache, cacheListed := zeroDays(built, "cache")
	wantMinutes := slices.Concat(span(4, 7), span(9, 11), span(13, 20))
	if !slices.Equal(minutes, wantMinutes) || slices.Contains(minutesListed, true) {
		t.Errorf("minutes holds zero rows on %v listed %v, want %v, each unlisted as its newest row is", minutes, minutesListed, wantMinutes)
	}
	if want := span(16, 20); !slices.Equal(cache, want) || slices.Contains(cacheListed, false) {
		t.Errorf("cache holds zero rows on %v listed %v, want %v, each listed", cache, cacheListed, want)
	}
	if days, _ := zeroDays(other, "minutes"); len(days) != 0 {
		t.Errorf("test-minutes, which the fill's ids leave out, holds zero rows on %v", days)
	}
	if want := int64(len(wantMinutes) + 5); filled != want {
		t.Errorf("the fill answers %d rows written, want %d", filled, want)
	}
	if again, err := refresh.fillLoggedZeros(ctx, r.project, []string{built}); err != nil || again != 0 {
		t.Errorf("a second fill wrote %d rows (%v), want none", again, err)
	}
	if every, err := refresh.fillLoggedZeros(ctx, r.project, nil); err != nil || every != 10 {
		t.Errorf("a fill of every logged eval wrote %d rows (%v), want test-minutes' 10 days from the 11th to the 20th", every, err)
	}
	t.Logf("minutes' zero days %v, cache's %v; %d rows written", minutes, cache, filled)
}

// TestAPassOverDeliveryInputWithNoProjectSucceeds: the delivery walk begins at the UTC midnight of
// its first input day whatever else the store holds. A store with delivery settings and a deploy
// run that started at 08:00, and no project yet (a fresh deployment, or the browser suite's store
// between a reset and its next seed), is walked from that run's day, and the pass ends with no
// error. The tester saw such a pass refused in the browser suite's run of the Legion rows: "read
// the pull requests of 2026-09-27: apply rows read from 2026-09-27T08:00:00Z hold no run of a
// window from 2026-09-27T00:00:00Z".
func TestAPassOverDeliveryInputWithNoProjectSucceeds(t *testing.T) {
	r := newRefresherRig(t)
	ctx := t.Context()
	r.exec(t, "remove the rig's one project", `delete from projects`)
	apply := "Apply"
	if _, err := delivery.PutSettings(ctx, r.database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/deploy", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"production / apply"}, ProductionApplyStepName: &apply,
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}, alice); err != nil {
		t.Fatalf("store the delivery settings: %v", err)
	}
	started := refresherToday.AddDate(0, 0, -2).Add(8 * time.Hour)
	r.exec(t, "store a deploy run that started at 08:00", `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at,
			conclusion, url, head_branch, event, jobs_complete)
		values ('acme/deploy', 1, $1, 'abc123', $2::timestamptz, $2::timestamptz, $2::timestamptz,
			$2::timestamptz + interval '10 minutes', 'success', 'https://github.com/acme/deploy/actions/runs/1', 'main', 'push', true)
	`, string(delivery.DeliveryRunKindDeploy), started)
	r.pass(t)
}
