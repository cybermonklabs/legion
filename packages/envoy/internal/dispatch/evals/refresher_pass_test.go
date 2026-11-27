package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAChangeLandingBetweenTwoPagesOfAPassIsWritten: a pass reads its feeds a page at a time, and a
// day one page wrote is written again by a later page of the same pass when a change to it
// committed after the first page read it. Over the hand-written Legion history of EVAL, EVAL-1's
// hours move and a P0 issue opens, each change moved into the log as the facts consumer's write
// does, and a page of one change and one event writes the day EVAL-1 closed on, with EVAL-1's new
// hours beside EVAL-2's old, and the unowned-p0 row of today; then EVAL-2, which closed on the same
// day, moves and a second P0 issue opens, and the pages after it, of the same pass, rewrite both:
// the day holds both issues' new hours and today's count is 2.
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
		if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
			t.Fatalf("move the facts' change into the log: %v", err)
		}
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
	hoursOf := func() []float64 {
		t.Helper()
		return slices.Sorted(slices.Values(r.day(t, EvalID("EVAL", SlugLegionWorkTime), metrics.SeriesHours, "", history.d1).Samples))
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
	wantFirst := []string{line(walkLegion, "EVAL", history.d1), line(SlugUnownedP0, "EVAL", refresherToday)}
	slices.Sort(wantFirst)
	if !slices.Equal(first, wantFirst) {
		t.Fatalf("the first page wrote %v, want EVAL-1's close day and today's unowned-p0 row, %v", first, wantFirst)
	}
	if after := hoursOf(); !slices.Equal(after, []float64{4, 7}) {
		t.Fatalf("after the first page the day holds the hours %v, want EVAL-1's new 7 beside EVAL-2's old 4", after)
	}
	moveHours("EVAL-2", 9)
	openP0(51)
	pages := 1
	for more {
		if more, err = pass.page(ctx, safe(), 1); err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		pages++
	}

	unowned := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", refresherToday)
	if samples := hoursOf(); !slices.Equal(samples, []float64{7, 9}) || unowned.LastValue == nil || *unowned.LastValue != 2 {
		t.Errorf("after %d pages the work time of %s holds %v and today's unowned-p0 count %v; want EVAL-1's 7 and EVAL-2's 9 hours, and 2",
			pages, history.d1.Format("2006-01-02"), samples, showValue(unowned.LastValue))
	}
	t.Logf("the first page wrote %v; %d pages in all; the day holds %v hours, today's count %v", first, pages, hoursOf(), showValue(unowned.LastValue))
}

// TestAPrunedCursorChangeIsNoLossAndAVanishedYoungOneIs: once every walk is done, the change the
// cursors rest on (the day rows' and the stored figures', which a pass judges apart) passes its 24
// hours and is pruned, as the refresher's own prune takes it on a day with no newer change, and the
// next pass begins no rebuild and writes no day. The change the cursors rest on leaving the log
// younger than its retention, as a reset log or a delete by hand takes it, is a loss no one can
// name, and the pass after it begins a rebuild of every family, whose walk builds the days of each.
// So is the change they rest on pruned after its retention when no pass had committed for longer
// than the retention: while the refresher stood still the prune may have taken changes after the
// cursors that no pass read, and its control is the same state with the last pass recent, which
// begins no rebuild. Each case is judged by the rebuilds the pass began (eval_rebuilds) as well as
// by the days it wrote, since a rebuild of the stored figures alone writes no day.
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
	rebuilds := func() int { return r.count(t, `select count(*) from eval_rebuilds`) }
	// agedAndPruned makes every change the log holds, and the one each of the two cursors rests
	// on, a day and an hour old, and prunes the log of them.
	agedAndPruned := func() int64 {
		t.Helper()
		r.exec(t, "age the log past its retention", `update delivery_changes set at = now() - interval '25 hours'`)
		r.exec(t, "age the cursors' changes with it", `update eval_days_cursor set at = now() - interval '25 hours' where name = any($1)`,
			[]string{changesCursor, figuresCursor})
		pruned, err := delivery.PruneChanges(ctx, r.database.Pool)
		if err != nil {
			t.Fatalf("prune the log: %v", err)
		}
		return pruned
	}

	began := rebuilds()
	pruned := agedAndPruned()
	if report, _ := r.pass(t); len(report.days) != 0 || rebuilds() != began {
		t.Errorf("the pass after the cursors' change was pruned in its time wrote %v and began %d rebuilds, want nothing: no rebuild begins", report.sorted(), rebuilds()-began)
	}

	r.exec(t, "log a change", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'young')`)
	if report, _ := r.pass(t); len(report.days) != 0 || rebuilds() != began {
		t.Fatalf("the pass reading a session group wrote %v and began %d rebuilds, want nothing", report.sorted(), rebuilds()-began)
	}
	r.exec(t, "delete the young change", `delete from delivery_changes where ref = 'young'`)
	lost, _ := r.pass(t)
	if !slices.Contains(lost.sorted(), line(walkLegion, "EVAL", legionHistoryDay(2))) || rebuilds() == began {
		t.Errorf("the pass after the cursors' young change vanished built %v and began %d rebuilds, want a rebuild of every family begun, EVAL's Legion days among them",
			lost.sorted(), rebuilds()-began)
	}
	t.Logf("pruned %d changes through the cursor's %d: the pass after wrote nothing; a young change vanished: the pass after wrote %d days",
		pruned, resting.Seq, len(lost.days))

	for _, stalled := range []bool{false, true} {
		r.settle(t)
		r.exec(t, "log a change for the cursors to rest on", `insert into delivery_changes (kind, repo, ref) values ('session_group', null, 'stall')`)
		r.pass(t)
		agedAndPruned()
		if stalled {
			r.exec(t, "stamp the last pass 25 hours before the next", `update eval_days_cursor set updated_at = $1`, r.now.Add(-25*time.Hour))
		}
		began := rebuilds()
		report, _ := r.pass(t)
		wrote := slices.Contains(report.sorted(), line(walkLegion, "EVAL", legionHistoryDay(2)))
		if rebuilt := rebuilds() > began; rebuilt != stalled || wrote != stalled {
			t.Errorf("the pass after the cursors' change was pruned, with the last pass 25 hours old: %t, began %d rebuilds and wrote %v, want a rebuild begun, EVAL's Legion days among them, only where it was",
				stalled, rebuilds()-began, report.sorted())
		}
		t.Logf("with the last pass 25 hours old: %t, the pass began %d rebuilds and wrote %d days", stalled, rebuilds()-began, len(report.days))
	}
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
// wake. The loop, run while the write stays open, at a gap of loopGap, asks for its next pass
// horizonRetry after such a pass, doubling the wait with each one in a row: it runs four passes,
// which a loop that waited for its gap would not, and the three after the first are each at least
// 250 ms, 500 ms and 1 s after the one before. Once the lower write commits, the next pass writes
// today's row with EVAL-50 counted.
func TestAnEventAboveTheSafeHorizonAsksForAPassSoon(t *testing.T) {
	// Not parallel: its loop logs through the process's default logger (startLoop), which the
	// test reads.
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

	loop := r.startLoop(t, make(chan struct{}, 1))
	loop.awaitRan(t, 4)
	ends := loop.passEnds()
	var apart []time.Duration
	for i, wait := range []time.Duration{horizonRetry, 2 * horizonRetry, 4 * horizonRetry} {
		gap := ends[i+1].Sub(ends[i])
		apart = append(apart, gap)
		// A timer never fires early, so a pass sooner than the wait asked for is a retry the loop
		// did not ask for; how much later one comes is the machine's, which no bound here judges.
		if gap < wait-5*time.Millisecond {
			t.Errorf("pass %d below the open write ended %s after the one before it, want at least %s", i+2, gap, wait)
		}
	}

	if err := lower.Commit(ctx); err != nil {
		t.Fatalf("commit the lower write: %v", err)
	}
	testwait.Eventually(t, "today's unowned-p0 row holding EVAL-50", func() bool {
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
// its first input day whatever else the store holds. A store with a deploy run that started at
// 08:00 and no project yet (a fresh deployment, or the browser suite's store between a reset and its
// next seed) passes with no error. Delivery settings are each project's (dispatch://INFRA-978, R4),
// so a store with no project holds none, and the store's first project is then made with delivery
// on and one source reading the run's repository, and with no issue, pull request or day row of its
// own; the run ships no pull request, so its rows are its repository's source key's (R12), and the
// walk's first input day is the run's alone, as with no project. The pass after it walks from that
// run's day and ends with no error and the walk standing at the day after today, every day it had
// to build built. The tester saw such a pass refused in the browser suite's run of the Legion rows:
// "read the pull requests of 2026-09-27: apply rows read from 2026-09-27T08:00:00Z hold no run of a
// window from 2026-09-27T00:00:00Z".
func TestAPassOverDeliveryInputWithNoProjectSucceeds(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	ctx := t.Context()
	r.exec(t, "remove the rig's one project", `delete from projects`)
	started := refresherToday.AddDate(0, 0, -2).Add(8 * time.Hour)
	r.exec(t, "store a deploy run that started at 08:00", `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, completed_at,
			conclusion, url, head_branch, event, jobs_complete)
		values ('acme/deploy', 1, $1, 'abc123', $2::timestamptz, $2::timestamptz, $2::timestamptz,
			$2::timestamptz + interval '10 minutes', 'success', 'https://github.com/acme/deploy/actions/runs/1', 'main', 'push', true)
	`, string(delivery.DeliveryRunKindDeploy), started)
	r.pass(t)
	r.exec(t, "make the store's first project", `insert into projects (key, name, default_assignee) values ('DEPLOY', 'Deploys', 'alice')`)
	apply := "Apply"
	putSources(t, r.database.Pool, "DEPLOY", []string{}, deliverytest.SourceOf(delivery.DeliverySettings{
		DeployWorkflowPath: ".github/workflows/deploy.yml", ProductionJobNames: []string{"production / apply"},
		ProductionApplyStepName: &apply, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
	}, new("acme/deploy")))
	report, _ := r.pass(t)
	walks, err := r.registry.readWalks(ctx)
	if err != nil {
		t.Fatalf("read the walks' records: %v", err)
	}
	if next := walks[walkKey{family: walkDelivery}].next; next == nil || !next.Equal(refresherToday.AddDate(0, 0, 1)) || report.walksLeft {
		t.Errorf("after the pass the delivery walk stands at %v (days left %t), want the day after today, %s, built from the run's day",
			next, report.walksLeft, refresherToday.AddDate(0, 0, 1).Format(time.DateOnly))
	}
}

// holdingTracer counts the statements started on a context whose caller holds a connection of the
// pool: those of a transaction opened under store.WithTransactionTracking.
type holdingTracer struct {
	held, total int
	mu          sync.Mutex
}

func (h *holdingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	if store.HoldsPooledConnection(ctx) {
		h.held++
	}
	return ctx
}

func (h *holdingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestRefreshThroughRunsItsTransactionsUnderTransactionTracking: RefreshThrough runs the pass the
// loop runs, whose transactions hold their connection on a tracked context (RunRefresher marks it,
// store.WithTransactionTracking), so a read the pass takes from the pool while one of its
// transactions is open is refused (store.ErrNestedAcquire) rather than left to wait for a
// connection the transaction holds. Over a Legion history, RefreshThrough is given an untracked
// context, and the statements its pass sends in its transactions are sent on a context that holds
// a connection.
func TestRefreshThroughRunsItsTransactionsUnderTransactionTracking(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	r.seedLegionEval(t)
	tracer := &holdingTracer{}
	traced := r.tracedStore(t, tracer)
	if err := traced.RefreshThrough(t.Context(), RefresherDeps{Horizon: r.horizon, Build: r.build}, r.now); err != nil {
		t.Fatalf("refresh through %s: %v", r.now.Format(time.RFC3339), err)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	if tracer.held == 0 {
		t.Errorf("none of the %d statements of RefreshThrough's passes was sent in a transaction on a tracked context", tracer.total)
	}
	t.Logf("%d of the %d statements were sent while the pass's transaction held its connection", tracer.held, tracer.total)
}

// TestAProjectMadeAfterAPassLoadedItsProjectsGetsTheDaysOfTheInputThatCountsForIt: a pass reads
// the projects once (load) and then its pages, and the cursors pass the changes of a page when it
// commits, so a project made between the two, whose issue a pull request is then moved to, has the
// days that change names written for it or never, since no later change names them. Over the
// delivery history, a page of a pass that loaded its projects before project NEWP was made, and
// before a pull request of the main project was moved to an issue of it, writes the pull request's
// merge day in NEWP.
func TestAProjectMadeAfterAPassLoadedItsProjectsGetsTheDaysOfTheInputThatCountsForIt(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	main := deliveries.Project
	issueProject := map[string]string{}
	for project, keys := range deliveries.Issues {
		for _, key := range keys {
			issueProject[key] = project
		}
	}
	var moved *delivery.DeliveryPullRequest
	for i, pr := range deliveries.PullRequests {
		if pr.IssueKey != nil && issueProject[*pr.IssueKey] == main && pr.MergedAt != nil && pr.CreatedAt != nil && !pr.Partial &&
			slices.Contains(deliveries.Settings.PopulationAuthors, pr.Author) {
			moved = &deliveries.PullRequests[i]
			break
		}
	}
	if moved == nil {
		t.Fatalf("the history holds no merged pull request of %s", main)
	}
	day := measures.Midnight(*moved.MergedAt)

	pass := r.registry.newRefresh(r.build, r.now, r.batch)
	if err := pass.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the pass: %v", err)
	}
	r.exec(t, "make project NEWP", `insert into projects (key, name, default_assignee) values ('NEWP', 'Made mid-pass', 'alice')`)
	r.exec(t, "make an issue of NEWP", `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ('NEWP-1', 'NEWP', 1, 'Work moved here', 'in_progress', '00000001', '{"kind":"user","id":"alice"}')`)
	r.exec(t, "move "+PullRequestMember(moved.Repo, moved.Number)+" to NEWP", `update delivery_pull_requests set issue_key = 'NEWP-1' where repo = $1 and number = $2`,
		moved.Repo, moved.Number)
	if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
		t.Fatalf("move the changes into the log: %v", err)
	}
	safe, err := r.horizon.Await(ctx)
	if err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	for more := true; more; {
		if more, err = pass.page(ctx, safe, refresherPage); err != nil {
			t.Fatalf("the pass's page: %v", err)
		}
	}
	if !slices.Contains(pass.report.sorted(), line(walkDelivery, "NEWP", day)) {
		t.Errorf("the pass wrote %v, want %s's merge day %s written for NEWP, which counts it now", pass.report.sorted(), PullRequestMember(moved.Repo, moved.Number), day.Format(time.DateOnly))
	}
	if rows := r.count(t, `select count(*) from eval_days where project_key = 'NEWP' and day = $1 and n > 0`, day); rows == 0 {
		t.Errorf("NEWP holds no day row with input on %s, where it counts %s", day.Format(time.DateOnly), PullRequestMember(moved.Repo, moved.Number))
	}
}

// TestAProjectMadeAfterAPassLoadedItsProjectsHasItsFiguresFoldedByThatPass: the stored figures'
// read of the change log (figureChanges) judges each change by the projects the pass loaded, and
// the cursor passes the change once the read commits, so a project made since, whose figures
// another process's pass has built, would have the figures of a write of no figures never folded.
// Over a Legion history of EVAL, a pass loads its projects; project NEWP is then made and the
// passes of another process (settle) seed it and build its figures; a build that writes no figures
// writes a day row of NEWP's legion-work-time (upsertDays); and the first pass's read of the log
// folds it: the stored figures are the fold of the day rows that stand.
func TestAProjectMadeAfterAPassLoadedItsProjectsHasItsFiguresFoldedByThatPass(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	ctx := t.Context()
	stale := r.registry.newRefresh(r.build, r.now, r.batch)
	if err := stale.load(ctx, r.seeds); err != nil {
		t.Fatalf("load the pass: %v", err)
	}
	r.exec(t, "make project NEWP", `insert into projects (key, name, default_assignee) values ('NEWP', 'Made mid-pass', 'alice')`)
	r.settle(t)
	moved := r.day(t, EvalID("EVAL", SlugLegionWorkTime), metrics.SeriesHours, "", history.d1)
	moved.ProjectKey, moved.EvalID = "NEWP", EvalID("NEWP", SlugLegionWorkTime)
	tx, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write of NEWP's day row: %v", err)
	}
	if err := upsertDays(ctx, tx, []DayRow{moved}, "a-build-that-writes-no-figures"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("write NEWP's day row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit NEWP's day row: %v", err)
	}
	if _, err := delivery.DrainPending(ctx, r.database.Pool); err != nil {
		t.Fatalf("move the change into the log: %v", err)
	}
	if err := stale.figureChanges(ctx, refresherPage); err != nil {
		t.Fatalf("the first pass's read of the figures' changes: %v", err)
	}
	if r.count(t, `select count(*) from eval_day_figures where project_key = 'NEWP'`)+r.count(t, `select count(*) from eval_range_figures where project_key = 'NEWP'`) == 0 {
		t.Error("the stored figures hold none of NEWP, whose day row was written with none folded")
	}
	expectTruth(t, "after the pass that loaded its projects before NEWP was made read NEWP's change", r.storedFigures(t), r.truthFigures(t))
}

// shippedMembersReads records the statements the registry sends that read the pull requests runs
// shipped (shippedMembersQuery).
type shippedMembersReads struct {
	mu   sync.Mutex
	sent []pgx.TraceQueryStartData
}

func (s *shippedMembersReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == shippedMembersQuery {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sent = append(s.sent, data)
	}
	return ctx
}

func (s *shippedMembersReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestTheShippedMembersReadGoesInStatementsWithinTheBound: the read of the pull requests the runs of
// a page shipped (shippedMembers) cuts its two lists, the projects and the runs, into statements of
// at most statementBytes of parameters, as the read of where items were counted does (heldMembers).
// Over 14,000 runs whose keys pass the bound twice over, the first and the last of which shipped a
// pull request, it sends at least two statements, each within the bound, and answers both pull
// requests.
func TestTheShippedMembersReadGoesInStatementsWithinTheBound(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := t.Context()
	runs := make([]string, 14000)
	for i := range runs {
		runs[i] = fmt.Sprintf("run:acme/deploy/%d-%s", i, "xxxxxxxxxxxxxxxx")
	}
	if _, err := r.database.Pool.Exec(ctx, `
		insert into eval_day_members (project_key, family, deploy_repo, row_key, day, repo, shipped_by)
		values ('EVAL', $1, 'acme/deploy', 'pull_request:acme/web#1', date '2026-11-01', 'acme/web', $2),
		       ('EVAL', $1, 'acme/deploy', 'pull_request:acme/web#2', date '2026-11-02', 'acme/web', $3)
	`, KindDelivery, runs[0], runs[len(runs)-1]); err != nil {
		t.Fatalf("seed the members: %v", err)
	}
	sent := &shippedMembersReads{}
	traced := r.tracedStore(t, sent)
	held, err := traced.shippedMembers(ctx, []string{"EVAL"}, runs)
	if err != nil {
		t.Fatalf("read the pull requests the runs shipped: %v", err)
	}
	sent.mu.Lock()
	defer sent.mu.Unlock()
	if len(sent.sent) < 2 {
		t.Errorf("the read went in %d statements, want at least two for runs whose keys pass the bound twice over", len(sent.sent))
	}
	for index, statement := range sent.sent {
		if estimated := paramEstimate(t, statement.Args); estimated > statementBytes {
			t.Errorf("statement %d takes an estimated %d bytes of parameters, past %d", index, estimated, statementBytes)
		}
	}
	members := map[string]bool{}
	for _, one := range held {
		members[one.member] = true
	}
	if len(held) != 2 || !members["pull_request:acme/web#1"] || !members["pull_request:acme/web#2"] {
		t.Errorf("the read answered %v, want the two pull requests the first and the last run shipped", held)
	}
}

// TestACorrectionThatEmptiesASeriesFirstDaysLeavesNoRowBeforeItsFirstReading: a logged eval's
// series begins on its first day with a value, as a write of the readings that stand leaves it, so
// a window that starts before that day reads no data. A correction that moves the reading of the
// series' first day to a later day empties that day's row, and the zero rows after it up to the
// series' next reading, which the readings write deletes with it: the series then holds the rows a
// write of the standing readings leaves, its first day (FirstDaysIn, the first day it states data
// since) is the first reading's, and a window over a day the correction emptied reads no data
// where a zero row left in place would read 0. A series a correction leaves with no value holds no
// row of its all-repositories days.
func TestACorrectionThatEmptiesASeriesFirstDaysLeavesNoRowBeforeItsFirstReading(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	on := func(day, hour int) time.Time { return time.Date(2026, time.October, day, hour, 0, 0, 0, time.UTC) }
	reading := func(series, key string, value float64, measured time.Time, extra map[string]any) json.RawMessage {
		fields := map[string]any{"series": series, "row_kind": "run", "row_key": key, "value": value, "measured_at": measured}
		for name, one := range extra {
			fields[name] = one
		}
		return row(t, fields)
	}
	r := newRig(t)
	web := map[string]any{"attributes": map[string]any{"repo": "acme/web"}}
	written := r.write(t, "build-minutes", alice, reading("minutes", "a", 1, on(3, 9), nil), reading("minutes", "b", 2, on(8, 9), nil),
		reading("minutes", "c", 3, on(12, 9), nil), reading("cache", "e", 4, on(15, 9), web), reading("minutes", "g", 5, on(10, 9), web))
	evalID := written.Definition.EvalID
	// The first reading of minutes moves from the 3rd to the 9th, the reading of acme/web in the
	// middle of it from the 10th to the 11th, and the only one of cache, of acme/web too, is
	// corrected to a reading with no value.
	r.write(t, "build-minutes", alice, reading("minutes", "a", 1, on(9, 9), map[string]any{"replaces": written.Readings[0].ID}),
		row(t, map[string]any{"series": "cache", "row_kind": "run", "row_key": "e", "value_absent_reason": "not measured", "measured_at": on(15, 9),
			"replaces": written.Readings[3].ID}),
		reading("minutes", "g", 5, on(11, 9), map[string]any{"replaces": written.Readings[4].ID, "attributes": map[string]any{"repo": "acme/web"}}))

	// stored is the day, repository and n of each row of a series, the project's own rows first.
	stored := func(of rig, series string) []string {
		t.Helper()
		rows, err := of.database.Pool.Query(ctx, `select to_char(day, 'YYYY-MM-DD') || ' ' || repo || ' ' || n from eval_days where eval_id = $1 and series = $2 order by repo collate "C", day`, evalID, series)
		if err != nil {
			t.Fatalf("read the rows of %s: %v", series, err)
		}
		days, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the rows of %s: %v", series, err)
		}
		return days
	}
	// The rows of a second database, whose readings were written as they now stand.
	standing := newRig(t)
	standing.write(t, "build-minutes", alice, reading("minutes", "b", 2, on(8, 9), nil), reading("minutes", "a", 1, on(9, 9), nil),
		reading("minutes", "c", 3, on(12, 9), nil), reading("minutes", "g", 5, on(11, 9), web))
	minutes := stored(r, "minutes")
	if want := stored(standing, "minutes"); !slices.Equal(minutes, want) || len(minutes) == 0 || minutes[0] != "2026-10-08  1" {
		t.Errorf("after the correction minutes holds the rows %v, want those of the readings as they stand, %v, which begin on the 8th and hold acme/web's reading of the 11th alone",
			minutes, want)
	}
	if repoRows := slices.DeleteFunc(slices.Clone(minutes), func(row string) bool { return !strings.Contains(row, " acme/web ") }); !slices.Equal(repoRows, []string{"2026-10-11 acme/web 1"}) {
		t.Errorf("after the correction minutes holds the rows %v of acme/web, want the one of the 11th: no zero row stands for a repository", repoRows)
	}
	if cache := stored(r, "cache"); len(cache) != 0 {
		t.Errorf("after its one reading lost its value cache holds the rows %v, want none", cache)
	}

	page, err := r.registry.Current(ctx, []string{r.project}, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", r.project, err)
	}
	index := slices.IndexFunc(page.Items, func(entry Entry) bool { return entry.Definition.Slug == "build-minutes" })
	if index < 0 {
		t.Fatalf("%s has no build-minutes eval", r.project)
	}
	entry := page.Items[index]
	firsts, err := FirstDaysIn(ctx, r.database.Pool, []Entry{entry}, nil)
	if err != nil {
		t.Fatalf("read the first days of %s: %v", evalID, err)
	}
	if first := firsts[evalID]["minutes"]; !first.Equal(on(8, 0)) {
		t.Errorf("minutes' first day is %v, want the 8th, its first reading's", first)
	}
	read := func(day time.Time) SeriesValues {
		t.Helper()
		window := Range{From: day, To: day.AddDate(0, 0, 1)}
		rows, err := DayWindowIn(ctx, r.database.Pool, []string{r.project}, []Entry{entry}, nil, nil, window, nil)
		if err != nil {
			t.Fatalf("read %s over %s: %v", evalID, day.Format(time.DateOnly), err)
		}
		for _, values := range SeriesValuesOf(entry, rows, firsts[evalID], window, nil) {
			if values.Series.Series == "minutes" {
				return values
			}
		}
		t.Fatalf("%s states no series minutes", evalID)
		return SeriesValues{}
	}
	statesData := func(values SeriesValues) bool {
		for _, value := range values.Window {
			if value != nil {
				return true
			}
		}
		return false
	}
	if before := read(on(5, 0)); statesData(before) || before.DataSince == nil || *before.DataSince != "2026-10-08" {
		t.Errorf("a window over the 5th, which the correction emptied, reads %v with data since %v, want no data, since the 8th", before.Window, before.DataSince)
	}
	if after := read(on(8, 0)); !statesData(after) {
		t.Errorf("a window over the 8th, the series' first day, reads %v, want its first reading's values", after.Window)
	}
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

// TestAPostedDeployNamingAnotherProjectsPullRequestCountsInItsRoutesProjectAlone (dispatch://INFRA-978,
// R10, R12 and A8): a deploy posted to OMEGA's route that names acme/alpha-app#9, a pull request
// of ALPHA's issue that no GitHub deploy ships, counts in OMEGA's deploy-frequency and in no other
// project's, because the post's token writes OMEGA alone; and the pull request's merge-to-production
// minutes, from its merge at 16:00 to the post's finish at 17:00, count in OMEGA's rows of its
// events repository and in none of ALPHA's, whose rows hold no minutes of it.
func TestAPostedDeployNamingAnotherProjectsPullRequestCountsInItsRoutesProjectAlone(t *testing.T) {
	r := newRefresherRig(t)
	r.seedSources(t)
	at := func(hour int) time.Time { return sourcesDay.Add(time.Duration(hour) * time.Hour) }
	r.exec(t, "ALPHA's issue", `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', $1, '{"kind":"system","id":"sources-test"}', $2)`,
		alphaApp, sourcesDay.AddDate(0, 0, -1))
	if err := delivery.UpsertPullRequest(t.Context(), r.database.Pool, delivery.DeliveryPullRequest{
		Repo: alphaApp, Number: 9, Title: "ALPHA-1: change 9", URL: fmt.Sprintf("https://github.com/%s/pull/9", alphaApp), Author: "dev-ana",
		CreatedAt: new(at(15)), MergedAt: new(at(16)), IssueKey: new("ALPHA-1"), Sessions: []string{},
	}); err != nil {
		t.Fatalf("write %s#9: %v", alphaApp, err)
	}
	posted := r.post(t, "OMEGA", "release-9", at(17), "acme/alpha-app#9")
	r.settle(t)

	for project, want := range map[string]defaultTally{"ALPHA": {n: 2, via: 2}, "OMEGA": {n: 2, via: 1}} {
		if got := r.deploysOn(t, project, sourcesDay); got != want {
			t.Errorf("%s's deploy-frequency on %s counts %d deploys, %d through a repository; want %d and %d",
				project, sourcesDay.Format(time.DateOnly), got.n, got.via, want.n, want.via)
		}
	}
	if n := r.count(t, `select count(*) from eval_day_members where row_key = $1 and project_key <> 'OMEGA'`, PostedMember(posted.ID)); n != 0 {
		t.Errorf("the posted deploy is counted in %d projects or keys besides OMEGA, want none", n)
	}
	for _, member := range []string{PostedMember(posted.ID), PullRequestMember(alphaApp, 9)} {
		if n := r.count(t, `select count(*) from eval_day_members where deploy_repo = $1 and row_key = $2 and project_key = 'OMEGA'`,
			EventsRepo("OMEGA"), member); n != 1 {
			t.Errorf("OMEGA's events repository counts %s %d times, want once", member, n)
		}
		if n := r.count(t, `select count(*) from eval_day_members where deploy_repo = $1 and row_key = $2 and project_key <> 'OMEGA'`,
			EventsRepo("OMEGA"), member); n != 0 {
			t.Errorf("OMEGA's events repository counts %s for %d projects or keys besides OMEGA, want none", member, n)
		}
	}
	var minutes []float64
	if err := r.database.Pool.QueryRow(t.Context(), `
		select coalesce(samples, '{}') from eval_days
		where eval_id = $1 and deploy_repo = $2 and series = 'minutes' and repo = '' and day = $3
	`, EvalID("OMEGA", SlugMergeToProduction), EventsRepo("OMEGA"), sourcesDay).Scan(&minutes); err != nil {
		t.Fatalf("read OMEGA's merge-to-production of its events repository: %v", err)
	}
	if !slices.Equal(minutes, []float64{60}) {
		t.Errorf("OMEGA's posted merge-to-production times %v, want acme/alpha-app#9's 60 minutes alone", minutes)
	}
	if n := r.count(t, `select count(*) from eval_days where project_key = 'ALPHA' and eval_id = $1 and series = 'minutes' and n > 0`,
		EvalID("ALPHA", SlugMergeToProduction)); n != 0 {
		t.Errorf("ALPHA holds %d merge-to-production rows with minutes, want none: the post counts for OMEGA's route alone", n)
	}
	expectTruth(t, "after a deploy posted naming another project's pull request", r.storedFigures(t), r.truthFigures(t))
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
