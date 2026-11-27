package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// refresherCatchUpWait is how long the process is given to catch the day rows up. Its refresher
// runs a pass at start and then one per wake, each as soon as the pass before it ends, and a batch
// of the walks each ten seconds at the closest (evals.RefresherGap), and the history is three
// delivery days and three Legion trees, which one pass walks in well under a second: the wait is
// the boot, the Legion facts consumer's first whole read, and a few passes of each loop.
const refresherCatchUpWait = 3 * time.Minute

// seriesSpan is the first and last day of one series' all-repositories rows of a project, its own
// and those of the owner keys it reads (eval_project_owners), and how many days of each deploy
// repository they hold, zero days where they hold none.
func seriesSpan(t *testing.T, database *store.Store, project, slug, series string) (time.Time, time.Time, int) {
	t.Helper()
	var first, last *time.Time
	var rows int
	if err := database.Pool.QueryRow(t.Context(), `
		select min(d.day), max(d.day), count(distinct (d.deploy_repo, d.day))
		from eval_project_owners o join eval_days d on d.project_key = o.owner
		where o.project_key = $1 and d.eval_id = o.owner || substr($2, length($1) + 1) and d.series = $3 and d.repo = ''
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
	deliveries := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Days: 3, Now: today})
	legion := legiontest.Seed(t, database.Pool, legionseed.Spec{Trees: 3, Now: now})

	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{})
	process.waitHealthy(t)

	tomorrow := today.AddDate(0, 0, 1)
	deadline := time.Now().Add(refresherCatchUpWait)
	var state string
	for {
		var cursors, deliveryDone, legionDone int
		var build string
		if err := database.Pool.QueryRow(t.Context(), `
			select (select count(*) from eval_days_cursor where name in ('delivery_changes', 'events')),
			       (select count(*) from eval_days_walks where family = 'delivery' and next_day = $1),
			       (select count(*) from eval_days_walks where family = 'legion' and project_key = $2 and next_day = $1),
			       coalesce((select built_by from eval_days_walks where family = 'delivery'), '')
		`, tomorrow, legion.Project).Scan(&cursors, &deliveryDone, &legionDone, &build); err != nil {
			t.Fatalf("read the refresher's state: %v", err)
		}
		mergeFirst, mergeLast, mergeRows := seriesSpan(t, database, deliveries.Project, evals.SlugMergeToProduction, measures.SeriesMinutes)
		workFirst, workLast, workRows := seriesSpan(t, database, legion.Project, evals.SlugLegionWorkTime, "hours")
		// The project's own unowned-p0 row of today (repo ''); the count is also stored for each
		// repository that holds a counted issue, whose rows this wait does not count.
		var unowned int
		if err := database.Pool.QueryRow(t.Context(), `select count(*) from eval_days where eval_id = $1 and day = $2 and repo = ''`,
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
	age, found, err := evals.RefresherAgeIn(t.Context(), database.Pool, time.Now())
	if err != nil || !found || age < 0 || age > refresherCatchUpWait {
		t.Errorf("RefresherAgeIn answers %s, found %t, %v; want the age of a pass the process just ran", age, found, err)
	}
	t.Logf("caught up: %s; the last pass ran %s ago", state, age.Round(time.Millisecond))
}

// firstRefresherPassWait is how long a process is given for its eval day-row refresher's first
// pass to commit after it starts: its boot, the refresher lease, which a killed holder leaves to
// lapse rather than releasing it, and a pass that seeds every project and reads its feeds.
const firstRefresherPassWait = 2 * time.Minute

// databaseClock is the database's clock now (clock_timestamp()). A test reads it before it
// starts a process, which stamps the refresher's cursor on its own clock, the same machine's.
func databaseClock(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(t.Context(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// refresherPassedSince reports whether the eval day-row refresher's events cursor was stamped
// after since. Every pass stamps it with the moment the pass ran as of, after its load has seeded
// the projects the pass owes, and a process's first pass seeds every project
// (evals.RefresherAgeIn). So a stamp after since, the database's clock read before a process
// started while no other process ran, says that process's first pass has committed, its seed of
// every project with it.
func refresherPassedSince(t *testing.T, database *store.Store, since time.Time) bool {
	t.Helper()
	var passed bool
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select updated_at > $1 from eval_days_cursor where name = 'events'), false)
	`, since).Scan(&passed); err != nil {
		t.Fatalf("read the eval day-row refresher's events cursor: %v", err)
	}
	return passed
}

// eventRow is an event of the log as the boot tests read it: its id and type, and the id, slug
// and project of the artifact it is an event of, each empty for an event of no artifact.
type eventRow struct {
	id                      int64
	kind                    string
	artifact, slug, project string
}

func (e eventRow) String() string {
	return fmt.Sprintf("event %d %s of artifact %q, slug %q, project %q", e.id, e.kind, e.artifact, e.slug, e.project)
}

// seedOf reports whether the event is the one the eval registry's seed writes as it creates
// project's evals-md document: the document's artifact.created.
func (e eventRow) seedOf(project string) bool {
	return e.kind == "artifact.created" && e.slug == evals.DocumentSlug && e.project == project
}

// eventsAfter is every event of the log after the event id, in order.
func eventsAfter(t *testing.T, database *store.Store, id int64) []eventRow {
	t.Helper()
	rows, err := database.Pool.Query(t.Context(), `
		select e.id, e.type, coalesce(a.id::text, ''), coalesce(a.slug, ''), coalesce(a.project_key, '')
		from events e left join artifacts a on a.id = e.artifact_id
		where e.id > $1 order by e.id
	`, id)
	if err != nil {
		t.Fatalf("read the events after %d: %v", id, err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (eventRow, error) {
		var event eventRow
		err := row.Scan(&event.id, &event.kind, &event.artifact, &event.slug, &event.project)
		return event, err
	})
	if err != nil {
		t.Fatalf("read the events after %d: %v", id, err)
	}
	return events
}

// bootState is what a boot can write that TestABootOverAnUnchangedProjectWritesNoEvent counts:
// every event, and how many versions of evals, of series and of the project's evals-md document
// the database holds.
type bootState struct {
	events                             []eventRow
	versions, series, documentVersions int
}

func readBootState(t *testing.T, database *store.Store, project string) bootState {
	t.Helper()
	state := bootState{events: eventsAfter(t, database, 0)}
	if err := database.Pool.QueryRow(t.Context(), `
		select (select count(*) from eval_versions),
		       (select count(*) from eval_series),
		       (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
		         where a.project_key = $1 and a.issue_key is null and a.slug = $2)
	`, project, evals.DocumentSlug).Scan(&state.versions, &state.series, &state.documentVersions); err != nil {
		t.Fatalf("count the versions of the evals, the series and the %s document of %s: %v", evals.DocumentSlug, project, err)
	}
	return state
}

// bootUntilFirstRefresherPass starts the envoy-dispatch binary over the database at url, with no
// other process running there, and returns once it serves and its eval day-row refresher's first
// pass has committed (refresherPassedSince).
func bootUntilFirstRefresherPass(t *testing.T, database *store.Store, url string) *dispatchProcess {
	t.Helper()
	booted := databaseClock(t, database)
	process := startDispatchProcessWith(t, url, dispatchOptions{})
	process.waitHealthy(t)
	waitUntil(t, firstRefresherPassWait, "the process's first eval refresher pass, which seeds every project, to commit", func() bool {
		select {
		case <-process.Exited:
			t.Fatalf("the process exited before its first eval refresher pass committed:\n%s", process.Output.String())
		default:
		}
		return refresherPassedSince(t, database, booted)
	})
	return process
}

// TestABootOverAnUnchangedProjectWritesNoEvent: a later boot over an unchanged project writes
// nothing, so no reader of the event log pays for a seed at every boot. Every process's first
// refresher pass seeds every project (evals.Store.Ensure). The first boot over a project no seed
// has reached writes one event, the artifact.created of the project's evals-md, and its versions;
// the second, over the same database once the first has stopped, writes no event and no version
// of an eval, a series or the document.
func TestABootOverAnUnchangedProjectWritesNoEvent(t *testing.T) {
	database := storetest.Open(t)
	const project = "BOOT"
	if _, err := database.Pool.Exec(t.Context(), `insert into projects (key, name, default_assignee) values ($1, 'Booted twice', 'alice')`, project); err != nil {
		t.Fatalf("make project %s: %v", project, err)
	}
	url := database.Pool.Config().ConnString()

	first := bootUntilFirstRefresherPass(t, database, url)
	seeded := readBootState(t, database, project)
	if len(seeded.events) != 1 || !seeded.events[0].seedOf(project) {
		t.Fatalf("the first boot wrote the events %v; want one, the artifact.created of %s's %s\n%s", seeded.events, project, evals.DocumentSlug, first.Output.String())
	}
	t.Logf("the first boot wrote %v, %d eval versions, %d series versions and %d versions of %s",
		seeded.events[0], seeded.versions, seeded.series, seeded.documentVersions, evals.DocumentSlug)
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	second := bootUntilFirstRefresherPass(t, database, url)
	rebooted := readBootState(t, database, project)
	written := eventsAfter(t, database, seeded.events[0].id)
	if len(written) > 0 || rebooted.versions != seeded.versions || rebooted.series != seeded.series || rebooted.documentVersions != seeded.documentVersions {
		t.Fatalf("the second boot over %s unchanged wrote the events %v and left %d eval versions, %d series versions and %d versions of %s, where the first left %d, %d and %d; want nothing written\nthe first process's output:\n%s\nthe second process's output:\n%s",
			project, written, rebooted.versions, rebooted.series, rebooted.documentVersions, evals.DocumentSlug,
			seeded.versions, seeded.series, seeded.documentVersions, first.Output.String(), second.Output.String())
	}
	t.Logf("the second boot wrote nothing: %d events, %d eval versions, %d series versions and %d versions of %s",
		len(rebooted.events), rebooted.versions, rebooted.series, rebooted.documentVersions, evals.DocumentSlug)
}
