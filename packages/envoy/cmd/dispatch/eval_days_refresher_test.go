package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// refresherCatchUpWait is how long the process is given to catch the day rows up. Its refresher
// runs a pass at start and then one per wake, ten seconds apart at the closest
// (evals.RefresherGap), and the history is three delivery days and three Legion trees, which one
// pass walks in well under a second: the wait is the boot, the Legion facts consumer's first whole
// read, and a few passes of each loop.
const refresherCatchUpWait = 3 * time.Minute

// seriesSpan is the first and last day of one series' all-repositories rows of a project and how
// many rows it holds, zero days where it holds none.
func seriesSpan(t *testing.T, database *store.Store, project, slug, series string) (time.Time, time.Time, int) {
	t.Helper()
	var first, last *time.Time
	var rows int
	if err := database.Pool.QueryRow(t.Context(), `
		select min(day), max(day), count(*) from eval_days where project_key = $1 and eval_id = $2 and series = $3 and repo = ''
	`, project, evals.EvalID(project, slug), series).Scan(&first, &last, &rows); err != nil {
		t.Fatalf("read the days of %s %s: %v", project, slug, err)
	}
	if first == nil {
		return time.Time{}, time.Time{}, 0
	}
	return *first, *last, rows
}

// TestTheEvalDayRefresherCatchesUpInTheDispatchProcess is the refresher in the real envoy-dispatch
// binary, with no NATS, over a store a seeder wrote and no refresher has read: no cursor, no walk,
// no day row. The process's refresher starts at the feeds' heads, walks every delivery day from
// the first merge to today, writes today's unowned-p0 row, and writes the Legion days of the
// issues its own Legion facts consumer stores as that consumer's first whole read lands, each
// series' rows running without a gap from its first input day to today.
func TestTheEvalDayRefresherCatchesUpInTheDispatchProcess(t *testing.T) {
	database := storetest.Open(t)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	if now.Add(refresherCatchUpWait).Truncate(24 * time.Hour).After(today) {
		t.Skipf("the wait would cross UTC midnight at %s, which moves today under the process", now.Format(time.RFC3339))
	}
	deliveries := deliverytest.Seed(t, database.Pool, deliverytest.Spec{Days: 3, Now: today})
	legion := legiontest.Seed(t, database.Pool, legiontest.Spec{Trees: 3, Now: now})

	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{})
	process.waitHealthy(t)

	tomorrow := today.AddDate(0, 0, 1)
	deadline := time.Now().Add(refresherCatchUpWait)
	var state string
	for {
		var cursors, deliveryDone, legionDone int
		var build string
		if err := database.Pool.QueryRow(t.Context(), `
			select (select count(*) from eval_days_cursor),
			       (select count(*) from eval_days_walks where family = 'delivery' and next_day = $1),
			       (select count(*) from eval_days_walks where family = 'legion' and project_key = $2 and next_day = $1),
			       coalesce((select built_by from eval_days_walks where family = 'delivery'), '')
		`, tomorrow, legion.Project).Scan(&cursors, &deliveryDone, &legionDone, &build); err != nil {
			t.Fatalf("read the refresher's state: %v", err)
		}
		mergeFirst, mergeLast, mergeRows := seriesSpan(t, database, deliveries.Project, evals.SlugMergeToProduction, measures.SeriesMinutes)
		workFirst, workLast, workRows := seriesSpan(t, database, legion.Project, evals.SlugLegionWorkTime, "hours")
		var unowned int
		if err := database.Pool.QueryRow(t.Context(), `select count(*) from eval_days where eval_id = $1 and day = $2`,
			evals.EvalID(deliveries.Project, evals.SlugUnownedP0), today).Scan(&unowned); err != nil {
			t.Fatalf("read the unowned-p0 row: %v", err)
		}
		gapless := func(first, last time.Time, rows int) bool {
			return rows > 0 && last.Equal(today) && rows == int(last.Sub(first).Hours()/24)+1
		}
		state = fmt.Sprintf("%d cursors, delivery walk done %d (built by %q), %s's Legion walk done %d; %s merge-to-production %d rows %s..%s; %s work time %d rows %s..%s; unowned-p0 of today %d",
			cursors, deliveryDone, build, legion.Project, legionDone, deliveries.Project, mergeRows, mergeFirst.Format(time.DateOnly), mergeLast.Format(time.DateOnly),
			legion.Project, workRows, workFirst.Format(time.DateOnly), workLast.Format(time.DateOnly), unowned)
		if cursors == 2 && deliveryDone == 1 && legionDone == 1 && build != "" && unowned == 1 &&
			gapless(mergeFirst, mergeLast, mergeRows) && gapless(workFirst, workLast, workRows) &&
			mergeFirst.Equal(deliveries.From.Truncate(24*time.Hour)) {
			break
		}
		select {
		case <-process.Exited:
			t.Fatalf("the process exited before its refresher caught up (%s):\n%s", state, process.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s after the process served, the refresher had not caught up: %s\n%s", refresherCatchUpWait, state, process.Output.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
	age, found, err := evals.New(database, nil, nil).RefresherAge(t.Context(), time.Now())
	if err != nil || !found || age < 0 || age > refresherCatchUpWait {
		t.Errorf("RefresherAge answers %s, found %t, %v; want the age of a pass the process just ran", age, found, err)
	}
	t.Logf("caught up: %s; the last pass ran %s ago", state, age.Round(time.Millisecond))
}
