package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
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
	t.Parallel()
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
	if err := pass.load(ctx, r.seeds); err != nil {
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
// on a day with no newer change, and the next pass begins no rebuild and writes no day. The change
// the cursor rests on leaving the log younger than its retention, as a reset log or a delete by
// hand takes it, is a loss no one can name, and the pass after it begins a rebuild of every family,
// whose walk builds the days of each. So is the change it rests on pruned after its retention when
// no pass had committed for longer than the retention: while the refresher stood still the prune
// may have taken changes after the cursor that no pass read.
func TestAPrunedCursorChangeIsNoLossAndAVanishedYoungOneIs(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	cursors, _, err := r.registry.readRefresherCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resting := cursors.changes
	if resting.Seq == 0 || resting.At == nil {
		t.Fatalf("the changes cursor rests at %d recorded %v, want a change", resting.Seq, resting.At)
	}

	r.exec(t, "age the log past its retention", `update delivery_changes set at = now() - interval '25 hours' where seq <= $1`, resting.Seq)
	r.exec(t, "age the cursor's change with it", `update eval_days_cursor set at = now() - interval '25 hours' where name = $1`, changesCursor)
	pruned, err := delivery.PruneChanges(ctx, r.database.Pool)
	if err != nil {
		t.Fatalf("prune the log: %v", err)
	}
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Errorf("the pass after the cursor's change was pruned in its time wrote %v, want nothing: no rebuild begins", report.sorted())
	}

	r.exec(t, "log a change", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'young')`)
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Fatalf("the pass reading a session group wrote %v, want nothing", report.sorted())
	}
	r.exec(t, "delete the young change", `delete from delivery_changes where ref = 'young'`)
	lost, _ := r.pass(t)
	if !slices.Contains(lost.sorted(), line(walkLegion, "EVAL", legionHistoryDay(2))) {
		t.Errorf("the pass after the cursor's young change vanished built %v, want a rebuild of every family begun, EVAL's Legion days among them", lost.sorted())
	}
	t.Logf("pruned %d changes through the cursor's %d: the pass after wrote nothing; a young change vanished: the pass after wrote %d days",
		pruned, resting.Seq, len(lost.days))

	r.settle(t)
	r.exec(t, "log a change for the cursor to rest on", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'stall')`)
	r.pass(t)
	if cursors, _, err = r.registry.readRefresherCursors(ctx); err != nil {
		t.Fatal(err)
	}
	resting = cursors.changes
	r.exec(t, "age the log past its retention", `update delivery_changes set at = now() - interval '25 hours' where seq <= $1`, resting.Seq)
	r.exec(t, "age the cursor's change with it", `update eval_days_cursor set at = now() - interval '25 hours' where name = $1`, changesCursor)
	if _, err := delivery.PruneChanges(ctx, r.database.Pool); err != nil {
		t.Fatalf("prune the log: %v", err)
	}
	r.exec(t, "stamp the last pass 25 hours before the next", `update eval_days_cursor set updated_at = $1`, r.now.Add(-25*time.Hour))
	stalled, _ := r.pass(t)
	if !slices.Contains(stalled.sorted(), line(walkLegion, "EVAL", legionHistoryDay(2))) {
		t.Errorf("the pass after 25 hours with no pass committed built %v, want a rebuild of every family begun, EVAL's Legion days among them", stalled.sorted())
	}
	t.Logf("after a stall past the retention the pass wrote %d days", len(stalled.days))
}

// TestARefresherCatchUpThatAPruneCrossesReadsItsCursorAsLost: the refresher's half of the Legion
// facts consumer's TestAPruneBetweenTwoPagesOfACatchUpReadsTheCursorAsLost. Its last read of the
// whole change log stands 23 hours before a pass that catches up a backlog a page at a time; the
// first page reads one change of the backlog and carries that read's ReadAt forward, a prune takes
// that change and EVAL-1's moved hours after it, and the page after the prune reads the clock two
// hours on, past the retention since that read. It reads the cursor as lost and begins a rebuild of
// every family, whose flip writes EVAL-1's close day with its new hours, which no page read.
func TestARefresherCatchUpThatAPruneCrossesReadsItsCursorAsLost(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	evalID := EvalID("EVAL", SlugLegionWorkTime)
	if samples := r.day(t, evalID, metrics.SeriesHours, "", history.d1).Samples; slices.Contains(samples, 7) {
		t.Fatalf("%s already holds 7 hours (%v), so this test examined nothing", history.d1.Format(time.DateOnly), samples)
	}
	var first int64
	if err := r.database.Pool.QueryRow(ctx, `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'backlog') returning seq`).Scan(&first); err != nil {
		t.Fatalf("log the backlog's first change: %v", err)
	}
	row := history.rows["EVAL-1"]
	row.WorkHours = 7
	history.rows["EVAL-1"] = row
	r.writeFacts(t, history, "EVAL-1")
	if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
		t.Fatalf("move EVAL-1's change into the log: %v", err)
	}
	last := r.latestSeq(t)
	r.exec(t, "record the backlog a day ago", `update delivery_changes set at = now() - interval '25 hours' where seq between $1 and $2`, first, last)
	r.exec(t, "log a change the prune keeps", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'kept')`)
	r.exec(t, "stamp the last read of the whole log 23 hours ago", `update eval_days_cursor set updated_at = $1 where name = $2`, r.now.Add(-23*time.Hour), changesCursor)

	clock := r.now
	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	pass.clock = func() time.Time { return clock }
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the pass: %v", err)
	}
	safe, err := r.horizon.Await(ctx)
	if err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	more, err := pass.page(ctx, safe, 1)
	if cursors, _, readErr := r.registry.readRefresherCursors(ctx); err != nil || readErr != nil || !more || cursors.changes.Seq != first {
		t.Fatalf("the first page answered more %t, %v, and left the change cursor at %d (%v); want a full page resting on %d", more, err, cursors.changes.Seq, readErr, first)
	}
	if _, err := delivery.PruneChanges(ctx, r.database.Pool); err != nil {
		t.Fatalf("prune the log: %v", err)
	}
	if left := r.count(t, `select count(*) from delivery_changes where seq between $1 and $2`, first, last); left != 0 {
		t.Fatalf("the prune left %d of the backlog's changes %d to %d, so this test examined nothing", left, first, last)
	}
	clock = clock.Add(2 * time.Hour)
	for more {
		if more, err = pass.page(ctx, safe, 1); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	samples := r.day(t, evalID, metrics.SeriesHours, "", history.d1).Samples
	if !slices.Contains(samples, 7) {
		t.Errorf("after the catch-up %s holds the hours %v, want EVAL-1's 7, which the prune took before a page read it", history.d1.Format(time.DateOnly), samples)
	}
	t.Logf("the page after the prune read the clock 25 hours after the last read of the whole log; %s then held %v", history.d1.Format(time.DateOnly), samples)
}

// TestTheUnownedP0RowOfEachDayIsWrittenWithNoDeliverySettings: with no delivery settings, so no
// delivery walk, a project's unowned-p0 row of today is written by the first pass of each UTC day,
// since the count reads issues alone, and by no later pass of that day.
func TestTheUnownedP0RowOfEachDayIsWrittenWithNoDeliverySettings(t *testing.T) {
	t.Parallel()
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

// TestAnEventAboveTheSafeHorizonAsksForAPassSoon: a pass reads the event log up to the safe
// horizon it took (events.Horizon), and an event that committed while a lower id was still open
// stands above it. A write holds a lower event id open while EVAL-50 opens as a P0 and its
// issue.created commits, which moves today's unowned-p0 count: the pass leaves today's row as it
// was and says its horizon held an event back, since what holds the horizon back may end without a
// wake. The loop, run while the write stays open, asks for its next pass horizonRetry after such a
// pass, doubling the wait with each one in a row: its passes end 250 ms, 500 ms and 1 s apart, each
// plus a pass's length, rather than RefresherGap apart. Once the lower write commits, the next pass
// writes today's row with EVAL-50 counted.
func TestAnEventAboveTheSafeHorizonAsksForAPassSoon(t *testing.T) {
	r := newRefresherRig(t)
	ctx := t.Context()
	r.settle(t)
	// A pass whose Safe answers 0 awaits the horizon instead (SafeOrAwait), which the open write
	// below would hold for as long as it stands; the settled passes proved the seed's events.
	if r.horizon.Proven() == 0 {
		t.Fatal("the settled passes proved no event, so the held pass would await the open write rather than read below it")
	}
	evalID := EvalID("EVAL", SlugUnownedP0)
	if count := r.day(t, evalID, SeriesUnownedP0, "", refresherToday); count.LastValue == nil || *count.LastValue != 0 {
		t.Fatalf("the unowned-p0 row of today holds %v before EVAL-50 opens, want 0", showValue(count.LastValue))
	}

	lower, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower write: %v", err)
	}
	t.Cleanup(func() { _ = lower.Rollback(context.Background()) })
	// The write takes its transaction id before it draws its event id, as Broker.Append does: the
	// writer invariant events.Horizon's proof rests on.
	if _, err := lower.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the lower write's transaction id: %v", err)
	}
	var held int64
	if err := lower.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'refresher.test', '{"kind":"user","id":"alice"}', '{}', false)
		returning id`).Scan(&held); err != nil {
		t.Fatalf("draw the lower event id: %v", err)
	}
	r.exec(t, "open a P0 issue", `
		insert into issues (key, project_key, number, title, status, rank, priority, created_by)
		values ('EVAL-50', 'EVAL', 50, 'Broken', 'triage', '00000050', 0, '{"kind":"user","id":"alice"}')`)
	r.writeIssueEvent(t, "EVAL-50", "issue.created", `{"key":"EVAL-50","priority":0,"status":"triage","changed":["title","status","priority"]}`)
	var opened int64
	if err := r.database.Pool.QueryRow(ctx, `select id from events where issue_key = 'EVAL-50' and type = 'issue.created'`).Scan(&opened); err != nil {
		t.Fatalf("read EVAL-50's issue.created: %v", err)
	}
	if opened <= held {
		t.Fatalf("EVAL-50's issue.created drew id %d, not above the open write's %d", opened, held)
	}

	// The pass as the loop runs it: no Await before it, which would wait out the open write.
	report, _, err := r.registry.refresh(ctx, RefresherDeps{Horizon: r.horizon, Build: r.build},
		refresherSchedule{gap: RefresherGap, page: refresherPage, batch: r.batch, now: func() time.Time { return r.now }}, r.seeds, nil)
	if err != nil {
		t.Fatalf("the pass below the open write failed: %v", err)
	}
	if proven := r.horizon.Proven(); proven >= held {
		t.Fatalf("the horizon stands at %d while the write holding %d is open", proven, held)
	}
	today := line(SlugUnownedP0, "EVAL", refresherToday)
	if slices.Contains(report.sorted(), today) {
		t.Fatalf("the pass below the open write wrote %s, so its pages read past its safe horizon", today)
	}
	if !report.heldBack {
		t.Errorf("the pass that left EVAL-50's issue.created (id %d) above its safe horizon says its horizon held none back", opened)
	}

	loop := r.startLoop(t, RefresherGap, make(chan struct{}, 1))
	loop.awaitRan(t, 4, 5*time.Second)
	ends := loop.passEnds()
	var apart []time.Duration
	for i, wait := range []time.Duration{horizonRetry, 2 * horizonRetry, 4 * horizonRetry} {
		gap := ends[i+1].Sub(ends[i])
		apart = append(apart, gap)
		if gap < wait-5*time.Millisecond || gap > wait+400*time.Millisecond {
			t.Errorf("pass %d below the open write ended %s after the one before it, want %s and a pass's length", i+2, gap, wait)
		}
	}

	if err := lower.Commit(ctx); err != nil {
		t.Fatalf("commit the lower write: %v", err)
	}
	awaitAt(t, time.Now().Add(8*horizonRetry+3*time.Second), "today's unowned-p0 row holding EVAL-50", func() bool {
		count := r.day(t, evalID, SeriesUnownedP0, "", refresherToday)
		return count.LastValue != nil && *count.LastValue == 1
	})
	t.Logf("the held pass read below id %d and left id %d unread; the loop's passes below the open write ended %v apart, and once it committed today's row held EVAL-50",
		held, opened, apart)
}

// TestTheShippedMembersReadKeepsItsPartialIndexUnderAGenericPlan: the read of the pull requests a
// run shipped (shippedMembers) reads eval_day_members through eval_day_members_shipped, the
// partial index of the members a run shipped, under the generic plan pgx's cached statement
// settles on, which knows no value of the runs it is executed with. 5,000 members, one in ten
// shipped by a run, are analyzed first, and sequential scans are off, so the plan names the index
// the statement can prove and nothing else.
func TestTheShippedMembersReadKeepsItsPartialIndexUnderAGenericPlan(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.database.Pool.Exec(ctx, `
		insert into eval_day_members (project_key, family, deploy_repo, row_key, day, repo, shipped_by)
		select 'EVAL', 'delivery', 'acme/deploy', 'pr:acme/web#' || n, date '2026-11-01' + n % 30, 'acme/web',
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

// TestTheLoggedZeroFillWritesEveryMissingDayAndNoOther: the fill of the logged evals' zero rows
// (fillLoggedZeros) writes each day from a series' first row to today that holds no row of it,
// between two rows as after the newest, each with the listing of the series' newest row, and no
// other day: none before a series' first row, and none after today though a row of a later day
// exists. A second fill writes nothing. The readings write writes the zero rows its readings leave
// owed itself (writeLoggedZeros), so the test takes them away after the writes (leaveGaps), leaving
// the gaps a write of an earlier build, which left them to the refresher, leaves.
func TestTheLoggedZeroFillWritesEveryMissingDayAndNoOther(t *testing.T) {
	t.Parallel()
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
	leaveGaps(t, r)
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
	filled, err := refresh.fillLoggedZeros(ctx, r.project)
	if err != nil {
		t.Fatalf("fill the logged evals: %v", err)
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
	if days, _ := zeroDays(other, "minutes"); !slices.Equal(days, span(11, 20)) {
		t.Errorf("test-minutes holds zero rows on %v, want %v", days, span(11, 20))
	}
	if want := int64(len(wantMinutes) + 5 + 10); filled != want {
		t.Errorf("the fill answers %d rows written, want %d", filled, want)
	}
	if again, err := refresh.fillLoggedZeros(ctx, r.project); err != nil || again != 0 {
		t.Errorf("a second fill wrote %d rows (%v), want none", again, err)
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
	t.Parallel()
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

// sourcesDay is the UTC day the three-source history happens on, the day before refresherToday.
var sourcesDay = refresherToday.AddDate(0, 0, -1)

// The three-source history's repositories (dispatch://INFRA-978, D1): ALPHA's default repository
// acme/alpha-app, which ALPHA's first source names none of and so reads; acme/alpha-ops, no
// project's default, which ALPHA's second source names; OMEGA's default repository acme/omega-app,
// which its one GitHub source reads; and acme/alpha-next, which ALPHA's default repository moves
// to.
const (
	alphaApp  = "acme/alpha-app"
	alphaOps  = "acme/alpha-ops"
	omegaApp  = "acme/omega-app"
	alphaNext = "acme/alpha-next"
)

// The production job each source's rule names.
const (
	alphaJob = "alpha / apply"
	opsJob   = "ops / release"
	omegaJob = "omega / apply"
)

// ruleOfJob is a GitHub source's rule that calls a run a deploy by its job named job.
func ruleOfJob(job string) delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{job},
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}
}

// deployRun stores deploy run id of repo on main, created at created, with one job named job that
// succeeded, which completes 19 minutes after created.
func (r *refresherRig) deployRun(t *testing.T, repo string, id int64, created time.Time, job string) {
	t.Helper()
	ctx, pool := t.Context(), r.database.Pool
	started, completed := created.Add(time.Minute), created.Add(20*time.Minute)
	if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
		Repo: repo, RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", id),
		HeadCommitAt: created.Add(-time.Minute), StartedAt: started, CreatedAt: new(created), CompletedAt: &completed,
		Conclusion: new(delivery.DeliveryRunConclusionSuccess), URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id),
		HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("write deploy run %d of %s: %v", id, repo, err)
	}
	ran := delivery.DeliveryRunJob{Name: job, StartedAt: new(started.Add(time.Minute)), CompletedAt: new(completed.Add(-time.Minute)),
		Conclusion: new(delivery.DeliveryJobConclusionSuccess)}
	if err := delivery.UpsertRunJobs(ctx, pool, repo, id, []delivery.DeliveryRunJob{ran}, true); err != nil {
		t.Fatalf("write the job of deploy run %d of %s: %v", id, repo, err)
	}
}

// seedSources writes the three-source history on sourcesDay. ALPHA holds two GitHub sources, one
// naming no repository (acme/alpha-app, its default, under alphaJob) and one naming acme/alpha-ops
// (under opsJob); OMEGA one naming none (acme/omega-app under omegaJob) and an events source;
// DELTA an events source with delivery off. The runs, each on main and shipping no pull request:
// acme/alpha-app's 1001 by alphaJob; acme/alpha-ops' 2001 by opsJob and 2002 by alphaJob, which
// its own rule calls no deploy; acme/omega-app's 3001 by omegaJob; and acme/alpha-next's 4001 and
// 4002 by alphaJob, which no source reads until ALPHA's default moves there.
func (r *refresherRig) seedSources(t *testing.T) {
	t.Helper()
	pool := r.database.Pool
	r.exec(t, "the projects", `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 1, $1), ('OMEGA', 'Omega', 'alice', 2, $2), ('DELTA', 'Delta', 'alice', 1, null)`, alphaApp, omegaApp)
	r.exec(t, "the issue", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('OMEGA-1', 'OMEGA', 1, 'Omega work', 'in_progress', '00000001', $1, '{"kind":"system","id":"sources-test"}', $2)`,
		omegaApp, sourcesDay.AddDate(0, 0, -1))
	authors := []string{"dev-ana"}
	putSources(t, pool, "ALPHA", authors, deliverytest.SourceOf(ruleOfJob(alphaJob), nil), deliverytest.SourceOf(ruleOfJob(opsJob), new(alphaOps)))
	putSources(t, pool, "OMEGA", authors, deliverytest.SourceOf(ruleOfJob(omegaJob), nil), eventsSource)
	putProjectSettings(t, pool, delivery.ProjectSettingsInput{Project: "DELTA", Delivery: false, Sources: []delivery.SourceInput{eventsSource}})
	at := func(hour int) time.Time { return sourcesDay.Add(time.Duration(hour) * time.Hour) }
	r.deployRun(t, alphaApp, 1001, at(10), alphaJob)
	r.deployRun(t, alphaOps, 2001, at(10), opsJob)
	r.deployRun(t, alphaOps, 2002, at(12), alphaJob)
	r.deployRun(t, omegaApp, 3001, at(10), omegaJob)
	r.deployRun(t, alphaNext, 4001, at(10), alphaJob)
	r.deployRun(t, alphaNext, 4002, at(12), alphaJob)
}

// deploysOn is project's deploy-frequency on day as the eval routes read it (readTallies, over the
// repositories recorded with the rows): its deploys and how many reached it through a repository.
func (r *refresherRig) deploysOn(t *testing.T, project string, day time.Time) defaultTally {
	t.Helper()
	tallies, _ := r.readTallies(t, project, map[string]string{SlugDeployFrequency: "deploys"}, day)
	return tallies[SlugDeployFrequency]
}

// TestAProjectReadsEachOfItsSourceRepositoriesUnderItsOwnRule: over the three-source history,
// ALPHA's read of deploy-frequency folds the rows of both its source repositories, each built under
// its own rule (acme/alpha-ops' 2002, a deploy by ALPHA's first rule, is none by its own), and a
// deploy of acme/omega-app, OMEGA's, counts nowhere for it; the repositories recorded with the rows
// are each project's sources'. Red under one rule for every repository: 2002 counts, or 2001 does
// not.
func TestAProjectReadsEachOfItsSourceRepositoriesUnderItsOwnRule(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	repos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := repos.Owned("ALPHA"); !slices.Equal(got, []string{alphaApp, alphaOps}) {
		t.Errorf("ALPHA's rows stand recorded under %v, want %s and %s", got, alphaApp, alphaOps)
	}
	if got := repos.Owned("OMEGA"); !slices.Equal(got, []string{omegaApp}) {
		t.Errorf("OMEGA's rows stand recorded under %v, want %s alone", got, omegaApp)
	}
	for project, want := range map[string]defaultTally{"ALPHA": {n: 2, via: 2}, "OMEGA": {n: 1, via: 1}} {
		if got := r.deploysOn(t, project, sourcesDay); got != want {
			t.Errorf("%s's deploy-frequency on %s counts %d deploys, %d through a repository; want %d and %d",
				project, sourcesDay.Format(time.DateOnly), got.n, got.via, want.n, want.via)
		}
	}
	if n := r.count(t, `select count(*) from eval_day_members where project_key = 'ALPHA' and row_key = $1`, RunMember(omegaApp, 3001)); n != 0 {
		t.Errorf("ALPHA counts OMEGA's deploy 3001 %d times, want none", n)
	}
	// acme/alpha-ops' 2002 ran no job its own rule names, so it reached no production: ALPHA's
	// deploy-frequency above counts 2001 alone of acme/alpha-ops', where under alphaJob it would
	// count 2002 too.
}

// TestASourceNamingNoRepositoryMovesWithTheDefaultRepository: ALPHA's first source names no
// repository, so it reads ALPHA's default repository; once that moves to acme/alpha-next, the pass
// after it records ALPHA's rows under acme/alpha-next and acme/alpha-ops, builds acme/alpha-next's
// days under its first rule, and ALPHA's deploy-frequency reads its two deploys and acme/alpha-ops'
// one, each through its repository; acme/alpha-app, which no source reads any more, keeps its rows
// as they stood (none written, none deleted) and its rule, recorded with no project, and ALPHA
// reads none of them (dispatch://INFRA-978, plan T19 path 1: which repositories a project reads is
// a selection). Before the pass the read answers the old selection whole.
func TestASourceNamingNoRepositoryMovesWithTheDefaultRepository(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	r.settle(t)
	appRows := `select project_key || ' ' || eval_id || ' ' || series || ' ' || repo || ' ' || day::text, xmin::text
		from eval_days where deploy_repo = '` + alphaApp + `'`
	held := r.xmins(t, appRows)
	if len(held) == 0 {
		t.Fatalf("%s holds no row before the move, so its keeping them shows nothing", alphaApp)
	}
	r.exec(t, "move ALPHA's default repository", `update projects set default_repo = $1 where key = 'ALPHA'`, alphaNext)
	if got := r.deploysOn(t, "ALPHA", sourcesDay); got != (defaultTally{n: 2, via: 2}) {
		t.Errorf("before the pass ALPHA reads %d deploys, %d through a repository; want the old selection's 2 and 2", got.n, got.via)
	}
	r.settle(t)
	repos, err := DeployReposIn(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := repos.Owned("ALPHA"); !slices.Equal(got, []string{alphaNext, alphaOps}) {
		t.Errorf("after the move ALPHA's rows stand recorded under %v, want %s and %s", got, alphaNext, alphaOps)
	}
	if got := r.deploysOn(t, "ALPHA", sourcesDay); got != (defaultTally{n: 3, via: 3}) {
		t.Errorf("after the move ALPHA reads %d deploys, %d through a repository; want acme/alpha-next's 2 and acme/alpha-ops' 1, each through it", got.n, got.via)
	}
	if differ := sameValues(r.xmins(t, appRows), held, func(xmin string) string { return "written by " + xmin }); len(differ) > 0 {
		t.Errorf("%s, which no source reads any more, did not keep its rows as they stood, first ones:\n\t%s", alphaApp, strings.Join(differ, "\n\t"))
	}
	if n := r.count(t, `select count(*) from eval_days_rules where repo = $1 and project_key is null and kept_since is not null`, alphaApp); n != 1 {
		t.Errorf("%s's rule is recorded with no project and its start %d times, want once", alphaApp, n)
	}
}

// post writes a deploy posted to project's route (prodevents.Write, source api) under ref,
// finished at finished, naming the pull requests prs.
func (r *refresherRig) post(t *testing.T, project, ref string, finished time.Time, prs ...string) prodevents.Event {
	t.Helper()
	event, _, err := prodevents.Write(t.Context(), r.database.Pool, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindDeploy, Source: prodevents.SourceAPI, SourceRef: ref,
		FinishedAt: &finished, PullRequests: prs, Actor: alice,
	})
	if err != nil {
		t.Fatalf("post the deploy %s to %s: %v", ref, project, err)
	}
	return event
}

// TestAPostedDeployCountsInItsRoutesProjectsDeployFrequencyAlone: over the three-source history, a
// deploy posted to OMEGA, whose events source reads its posts, adds one to OMEGA's deploy-frequency
// on the day it finished and to no other project's (k12's posted deploy); one posted to ALPHA,
// which has no events source, and one to DELTA, whose delivery is off, count in no eval. A post
// naming no pull request adds to deploy frequency alone.
func TestAPostedDeployCountsInItsRoutesProjectsDeployFrequencyAlone(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	at := func(hour int) time.Time { return sourcesDay.Add(time.Duration(hour) * time.Hour) }
	r.post(t, "OMEGA", "release-1", at(15))
	r.post(t, "ALPHA", "release-1", at(15))
	r.post(t, "DELTA", "release-1", at(15))
	r.settle(t)
	for project, want := range map[string]defaultTally{"ALPHA": {n: 2, via: 2}, "OMEGA": {n: 2, via: 1}, "DELTA": {}} {
		if got := r.deploysOn(t, project, sourcesDay); got != want {
			t.Errorf("%s's deploy-frequency on %s counts %d deploys, %d through a repository; want %d and %d",
				project, sourcesDay.Format(time.DateOnly), got.n, got.via, want.n, want.via)
		}
	}
	if n := r.count(t, `select count(*) from eval_days where eval_id = $1 and deploy_repo = $2 and series = 'minutes' and n > 0`,
		EvalID("OMEGA", SlugMergeToProduction), EventsRepo("OMEGA")); n != 0 {
		t.Errorf("a post naming no pull request times %d merge-to-production rows, want none", n)
	}
	expectTruth(t, "after the posted deploys", r.storedFigures(t), r.truthFigures(t))
}

// TestAPostedDeployTimesThePullRequestsItShipsFirst: OMEGA's post at 11:00 names acme/omega-app#7,
// merged at 09:00, which OMEGA's GitHub deploy 3001 shipped at 10:19 first, and #8, merged at 10:30,
// which no GitHub deploy ships, each in another case than GitHub's. merge-to-production times each
// pull request once, from its merge to the first deploy that ships it: #7 in acme/omega-app's rows
// (79 minutes) and #8 in OMEGA's events repository's (30 minutes), whose rows hold #7 nowhere.
func TestAPostedDeployTimesThePullRequestsItShipsFirst(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	at := func(hour, minute int) time.Time {
		return sourcesDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	for _, pr := range []struct {
		number int
		merged time.Time
	}{{7, at(9, 0)}, {8, at(10, 30)}} {
		if err := delivery.UpsertPullRequest(t.Context(), r.database.Pool, delivery.DeliveryPullRequest{
			Repo: omegaApp, Number: pr.number, Title: fmt.Sprintf("OMEGA-1: change %d", pr.number),
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", omegaApp, pr.number), Author: "dev-ana",
			CreatedAt: new(pr.merged.Add(-time.Hour)), MergedAt: new(pr.merged), IssueKey: new("OMEGA-1"), Sessions: []string{},
		}); err != nil {
			t.Fatalf("write %s#%d: %v", omegaApp, pr.number, err)
		}
	}
	r.post(t, "OMEGA", "release-2", at(11, 0), "Acme/Omega-App#7", "ACME/omega-app#8")
	r.settle(t)
	samples := func(deployRepo string) []float64 {
		t.Helper()
		var held []float64
		if err := r.database.Pool.QueryRow(t.Context(), `
			select coalesce(samples, '{}') from eval_days
			where eval_id = $1 and deploy_repo = $2 and series = 'minutes' and repo = '' and day = $3
		`, EvalID("OMEGA", SlugMergeToProduction), deployRepo, sourcesDay).Scan(&held); err != nil {
			t.Fatalf("read OMEGA's merge-to-production of %s: %v", deployRepo, err)
		}
		slices.Sort(held)
		return held
	}
	if got := samples(omegaApp); !slices.Equal(got, []float64{79}) {
		t.Errorf("acme/omega-app's merge-to-production of OMEGA times %v, want #7's 79 minutes alone", got)
	}
	if got := samples(EventsRepo("OMEGA")); !slices.Equal(got, []float64{30}) {
		t.Errorf("OMEGA's posted merge-to-production times %v, want #8's 30 minutes alone", got)
	}
	if n := r.count(t, `select count(*) from eval_day_members where deploy_repo = $1 and row_key = $2`,
		EventsRepo("OMEGA"), PullRequestMember(omegaApp, 7)); n != 0 {
		t.Errorf("OMEGA's events repository counts #7, which a GitHub deploy shipped first, %d times, want none", n)
	}
	expectTruth(t, "after the posted deploy timing pull requests", r.storedFigures(t), r.truthFigures(t))
}

// TestAPostedDeployWrittenAfterItsDayWasBuiltRewritesThatDay: once the history has settled with
// one posted deploy of OMEGA's, a deploy posted later that finished today, a day already built,
// rewrites that day of OMEGA through its production_events change (pageWork.addChange), and today's
// deploy-frequency of OMEGA counts it.
func TestAPostedDeployWrittenAfterItsDayWasBuiltRewritesThatDay(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	r.putFailures(t, "OMEGA", omegaJob, []string{delivery.FailureSourceEvents}, sourcesDay, eventsSource)
	r.post(t, "OMEGA", "release-1", sourcesDay.Add(15*time.Hour))
	r.settle(t)
	if got := r.deploysOn(t, "OMEGA", refresherToday); got.n != 0 {
		t.Fatalf("OMEGA counts %d deploys today before the post, want none", got.n)
	}
	r.post(t, "OMEGA", "release-3", refresherToday.Add(time.Hour))
	report, _ := r.pass(t)
	if !report.days[builtDay{family: walkDelivery, project: "OMEGA", day: refresherToday}] {
		t.Errorf("the pass after the post built %v, want OMEGA's day %s among them", report.sorted(), refresherToday.Format(time.DateOnly))
	}
	if got := r.deploysOn(t, "OMEGA", refresherToday); got.n != 1 {
		t.Errorf("OMEGA counts %d deploys today after the post, want 1", got.n)
	}
	window := Range{From: refresherToday, To: refresherToday.AddDate(0, 0, 1)}
	wantFigures(t, "after the late posted deploy", r.failureValues(t, "OMEGA", window), map[string]*float64{
		"change-fail-rate.total": new(1.0), "deployment-rework-rate.total": new(1.0),
	})
	expectTruth(t, "after the late posted deploy", r.storedFigures(t), r.truthFigures(t))
}
