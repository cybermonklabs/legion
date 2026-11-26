// Package evalbench loads the fixture at INFRA's stated rate that the eval registry's measurement
// Job reads (dispatch://INFRA-2916, checks k2, k8, k13 and k16), and names what the
// measurement reads of it: the project, its repositories, its logged evals and the windows of
// the first paint.
//
// Load writes, into a database the server has migrated and nothing has written, from one seed:
//
//   - INFRA, a project of INFRA's size: 13,103 issues over 365 days (35.9 a day), 67.4 percent
//     of them closed and 5 percent P0; 812,386 events, 62 an issue on average, each by a session
//     actor that carries its title, the Legion worker sessions titled `Legion <role> · <KEY>`; the
//     handoffs of each worked issue; and the daemon's phase, slot and claim observations of the
//     issues of the last 120 days (infra.go);
//   - INFRA's delivery history: 5,402 pull requests over three repositories, a fifth of them
//     rework, and over the last 200 days 20 deploy runs a day of 8 jobs and 60 pull-request runs
//     a day of 40 jobs, ten times as many runs under TenTimes (delivery.go);
//   - nine smaller projects, each the Legion history evalfixture writes (legiontest's);
//   - 2,800,000 readings of INFRA's 20 logged evals, in measured_at order, through the registry's
//     own register write and readings write (readings.go), over the whole UTC days from the later
//     of evals.MeasuredAtFloor (2025-01-01) and the day 730 days before Now's, to the end of the
//     day before Now's: 730 days once Now is 2027-01-01 or later, fewer before, the same total.
//     Every reading is measured before Now, so a server whose clock has reached Now (the bench's
//     default Now is today's UTC midnight) takes each one; the readings write refuses a
//     measured_at more than a day past its clock or before the floor (R18), and Load refuses a
//     Now that would make it refuse one.
//
// Base writes, into a database the base's envoy-dispatch migrated (main at the commit this head
// merges), the history main's Delivery page reads, for k8's and k13's negative controls: the same
// issues, events, observations and delivery history, INFRA's project with its default repository,
// and the same smaller projects, and no reading. What the base lacks of this head's tables it
// reads from the base database's catalog (checkSchema, resetConsumers), never from a list fixed
// here.
//
// AddFailureDay writes, into a database Load filled, a failure day on top of its history: the
// worst day of deploy failures INFRA's own history has had, 36 failed deploy runs with 45 failed
// jobs across them, on one day of the first paint's windows (failureday.go), so the Delivery
// timeline, which reads each deploy run's failed jobs, is measured on the day it reads the most.
//
// Scale multiplies every count (issues, events, pull requests, runs a day, the smaller projects'
// issues, readings) and keeps every span of days and the shape of each row, so a small load in a
// pod exercises every table and every read the full one does.
package evalbench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// Project is the project of INFRA's size; its key is the one the measurement reads.
const Project = "INFRA"

// The stated rate: dispatch://INFRA-2916's plan, after INFRA's measured volumes.
const (
	issues         = 13103
	issueDays      = 365
	closedShare    = 0.674
	p0Share        = 0.05
	eventsPerIssue = 62
	pullRequests   = 5402
	reworkShare    = 0.2
	runDays        = 200
	deployRuns     = 20
	checkRuns      = 60
	checkJobs      = 40
	readings       = 2_800_000
	readingDays    = 730
	// observedDays is how far back the daemon's sampler saw INFRA's issues: the issues opened in
	// the last 120 days carry observations and the older ones the event log alone, so the
	// project's first observation falls inside the history, as it does for a project whose
	// daemon Dispatch began to read partway through.
	observedDays = 120
	// smallIssues is how many finished issues each smaller project's Legion history holds at
	// least (evalfixture.DefaultIssues), and smallIssuesFloor the fewest it holds at any scale:
	// evalfixture writes the older half of the history unobserved and needs whole days on both
	// sides of the first observation, which twenty trees six hours apart give.
	smallIssues      = evalfixture.DefaultIssues
	smallIssuesFloor = 80
)

// Repos are INFRA's three repositories, the deploy repository first: the one whose runs Dispatch
// reads and whose pull requests a deploy ships.
var Repos = []string{"acme/infra-platform", "acme/infra-api", "acme/infra-web"}

// SmallProjects are the nine smaller projects.
var SmallProjects = []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO", "FOXTROT", "GOLF", "HOTEL", "JULIETT"}

// actor is the actor every row this package writes that records one names.
var actor = model.Actor{Kind: "system", ID: "evalbench"}

// Spec is one load: the moment the history ends, the seed of every draw, the scale, the run rate,
// the schema it writes into and the build its readings' day rows record.
type Spec struct {
	// Now is the moment every history ends at. Every time is relative to it, so two loads with
	// one Now and one Seed hold the same history at the same times.
	Now  time.Time
	Seed uint64
	// Scale multiplies every count; 1 is INFRA's stated rate. It is above 0 and at most 1.
	Scale float64
	// TenTimes writes ten times the stated run rate: 200 deploy runs and 600 pull-request runs a
	// day, the rate R13's 660 ms for the run-jobs read was measured at.
	TenTimes bool
	// Base writes into the base's schema, main's at the commit this head merges, for k8's and
	// k13's negative controls: no reading.
	Base bool
	// BuiltBy is the build the readings write records on the logged evals' day rows
	// (eval_days.built_by): the server's stamped commit, so every family's rows carry the running
	// build once the server's refresher has walked. Required unless Base.
	BuiltBy string
}

// scaled is count at the spec's scale, at least floor.
func (s Spec) scaled(count float64, floor int) int {
	return max(floor, int(math.Round(count*s.Scale)))
}

// Counts is what one load wrote, by table, read back from the database once it is done.
type Counts map[string]int64

// countedTables are the tables Counts reads, in the order it prints them; it reads those the
// database holds, so a load of the base counts none of the registry's.
var countedTables = []string{
	"projects", "issues", "events", "handoffs", "legion_observations", "legion_settings",
	"delivery_pull_requests", "delivery_runs", "delivery_run_jobs",
	"session_titles", "eval_versions", "eval_series", "eval_readings", "eval_days",
}

// Load writes the fixture spec names into the database, which the server must have migrated and
// nothing else written, and answers the counts it then holds. logf reports each step as it ends.
//
// The server's consumers that read the feeds this load writes (the Legion facts consumer and the
// eval day-row refresher, those of them the base runs at the base) committed their cursors over
// the empty store when the server that migrated it booted; Load deletes those rows last, so the
// next server to boot starts each consumer at the heads of the feeds and reads every project
// whole, as on a store a seeder wrote before any pass ran, rather than paging through the history
// as a stream of new changes. resetConsumers then writes the keyword fill's cursor as caught up
// with the loaded log (keywordFillCaughtUpQuery), at the base too, which runs the same fill.
func Load(ctx context.Context, database *store.Store, spec Spec, logf func(format string, args ...any)) (Counts, error) {
	switch {
	case spec.Scale <= 0 || spec.Scale > 1:
		return nil, fmt.Errorf("the scale is above 0 and at most 1, not %g", spec.Scale)
	case spec.Now.IsZero():
		return nil, errors.New("name the moment the history ends")
	case !spec.Base && spec.BuiltBy == "":
		return nil, errors.New("name the build the readings' day rows record (the server's stamped commit)")
	}
	spec.Now = spec.Now.UTC()
	pool := database.Pool
	if !spec.Base {
		if err := checkReadingSpan(ctx, pool, spec.Now); err != nil {
			return nil, err
		}
	}
	lacks, err := checkSchema(ctx, pool, spec.Base)
	if err != nil {
		return nil, err
	}
	if spec.Base {
		logf("the base lacks %d of this head's %d derived tables, by its catalog: %s", len(lacks), len(evalfixture.DerivedTables), strings.Join(lacks, ", "))
	}
	if err := checkEmpty(ctx, pool); err != nil {
		return nil, err
	}
	started := time.Now()
	step := func(what string) {
		logf("%s done after %s", what, time.Since(started).Round(time.Second))
	}
	history, err := writeInfra(ctx, pool, spec)
	if err != nil {
		return nil, err
	}
	step("INFRA's issues, events, handoffs and observations")
	if err := writeDelivery(ctx, pool, spec, history); err != nil {
		return nil, err
	}
	step("INFRA's delivery history")
	for _, project := range SmallProjects {
		write := evalfixture.WriteLegion
		if spec.Base {
			write = evalfixture.WriteBase
		}
		if _, err := write(ctx, pool, project, spec.scaled(smallIssues, smallIssuesFloor), spec.Now); err != nil {
			return nil, fmt.Errorf("write the Legion history of %s: %w", project, err)
		}
	}
	step("the nine smaller projects")
	if !spec.Base {
		if err := writeReadings(ctx, database, spec, history, logf); err != nil {
			return nil, err
		}
		step("INFRA's logged evals and readings")
	}
	if err := evalfixture.SettleNumbers(ctx, pool, append([]string{Project}, SmallProjects...)); err != nil {
		return nil, err
	}
	if err := resetConsumers(ctx, pool, spec.Base); err != nil {
		return nil, err
	}
	// Autovacuum would analyze the loaded tables in its own time; the measurement reads plans,
	// so the statistics a long-running store holds are taken now.
	if _, err := pool.Exec(ctx, `analyze`); err != nil {
		return nil, fmt.Errorf("analyze the loaded tables: %w", err)
	}
	step("analyze")
	return count(ctx, pool)
}

// checkSchema refuses a database of the wrong head and answers which of this head's derived
// tables (evalfixture.DerivedTables) its catalog does not hold. This head's mode needs every one
// of them. The base mode refuses a database that holds this head's day rows
// (evalfixture.HeadTable) and takes whatever else the base's catalog holds: the base is main at
// the commit this head merges, and a merge of main can bring some of this head's tables, so what
// the base lacks is read from the database, never fixed here.
func checkSchema(ctx context.Context, pool *store.Pool, base bool) ([]string, error) {
	held, err := evalfixture.ExistingTables(ctx, pool, evalfixture.DerivedTables)
	if err != nil {
		return nil, err
	}
	var lacks []string
	for _, table := range evalfixture.DerivedTables {
		if !slices.Contains(held, table) {
			lacks = append(lacks, table)
		}
	}
	switch {
	case base && slices.Contains(held, evalfixture.HeadTable):
		return nil, fmt.Errorf("-base writes into the base's schema, main's at the commit this head merges, and this database holds %s, which is this head's", evalfixture.HeadTable)
	case !base && len(lacks) > 0:
		return nil, fmt.Errorf("this head's mode writes into this head's schema, and the database lacks %s (a database of the base takes -base)", strings.Join(lacks, ", "))
	}
	return lacks, nil
}

// checkEmpty refuses a database that holds a project, an issue or a delivery row: the load's
// counts are the fixture's only where nothing else is there, and its consumer reset is right only
// for a store no consumer has read.
func checkEmpty(ctx context.Context, pool *store.Pool) error {
	var projects, issues, pullRequests, runs int64
	if err := pool.QueryRow(ctx, `
		select (select count(*) from projects), (select count(*) from issues),
		       (select count(*) from delivery_pull_requests), (select count(*) from delivery_runs)
	`).Scan(&projects, &issues, &pullRequests, &runs); err != nil {
		return fmt.Errorf("read whether the database is empty: %w", err)
	}
	if projects+issues+pullRequests+runs > 0 {
		return fmt.Errorf("the database holds %d projects, %d issues, %d pull requests and %d runs: load into a database the server migrated and nothing wrote", projects, issues, pullRequests, runs)
	}
	return nil
}

// resetConsumers deletes what the consumers of the loaded feeds committed over the empty store
// (Load): the Legion facts consumer's cursors and project marks, and the day-row refresher's
// cursors, walks, serving records, rebuilds with their staging, and record of the delivery
// settings its rows stand built under, and the stored figures it folded, with the marks of their
// stale cuts, whose mark of a project's built figures would otherwise survive its walks. It then
// writes the keyword fill's cursor as caught up with the loaded log (keywordFillCaughtUpQuery).
// Each statement names its table, so the item-row census (api's
// TestEveryWriterOfAnItemRowIsOnTheOwnerLockCensus) reads it as a write of no item row. This head
// runs every one of these consumers. The base, main at the commit this head merges, runs those
// whose tables its catalog holds, so at the base the reset deletes from the tables of these the
// catalog holds and writes the fill's cursor where it holds the fill's table; a list of what main
// runs would go stale at the next merge of main.
func resetConsumers(ctx context.Context, pool *store.Pool, base bool) error {
	statements := []string{`delete from issue_facts_cursor`, `delete from issue_facts_computed`,
		`delete from eval_days_cursor`, `delete from eval_days_walks`, `delete from eval_days_serving`,
		`delete from eval_days_settings`, `delete from eval_rebuilds`, `delete from eval_rebuild_walks`, `delete from eval_rebuild_firsts`,
		`delete from eval_rebuild_days`, `delete from eval_rebuild_members`, `delete from eval_rebuild_range_figures`,
		`delete from eval_rebuild_day_figures`, `delete from eval_rebuild_reverify`,
		`delete from eval_figures_built`, `delete from eval_range_figures`, `delete from eval_day_figures`, `delete from eval_figure_cuts_stale`}
	tableOf := func(statement string) string { return strings.TrimPrefix(statement, "delete from ") }
	fill := true
	if base {
		tables := []string{keywordFillTable}
		for _, statement := range statements {
			tables = append(tables, tableOf(statement))
		}
		held, err := evalfixture.ExistingTables(ctx, pool, tables)
		if err != nil {
			return err
		}
		statements = slices.DeleteFunc(statements, func(statement string) bool { return !slices.Contains(held, tableOf(statement)) })
		fill = slices.Contains(held, keywordFillTable)
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("reset consumers (%s): %w", statement, err)
		}
	}
	if !fill {
		return nil
	}
	if _, err := pool.Exec(ctx, keywordFillCaughtUpQuery, textversions.RulesVersion); err != nil {
		return fmt.Errorf("write the keyword fill's cursor as caught up: %w", err)
	}
	return nil
}

// keywordFillTable is the keyword fill's cursor table, which keywordFillCaughtUpQuery writes.
const keywordFillTable = "search_text_versions_fill"

// keywordFillCaughtUpQuery writes the keyword fill's one cursor row as production's stands once
// its one backfill, at the deploy, is done: at the newest event of the loaded log, with that
// event's created_at, which the fill checks the log against (events.EventLogWasReset), and no
// reach, which the fill computes on its next read. The row names no holder, as a fill with no hold
// writes it (textversions' holderArg), so the first batch of a serving fill takes it over
// (holdCheckQuery); the rules version is $1 (textversions.RulesVersion), the running build's,
// which at the base is the same, since this head changes no rule of the fill main runs. It
// overwrites the row the migrating server's fill wrote at no event, or writes one where it wrote
// none. The keyword rows themselves are not written, since no read the bench measures reads them.
// A backfill of the loaded log by the server would take hours at the stated rate, which is
// neither production's steady state nor a Job's length, so `measure wait` waits only for the fill
// to follow the events the server writes after the load.
const keywordFillCaughtUpQuery = `
	with newest as (select id, created_at from events order by id desc limit 1)
	insert into search_text_versions_fill (id, event_id, event_at, reach, holder, rules)
	select 1, coalesce((select id from newest), 0), (select created_at from newest), null, null, $1::smallint
	on conflict (id) do update
	   set event_id = excluded.event_id, event_at = excluded.event_at, reach = null,
	       holder = excluded.holder, rules = excluded.rules, updated_at = now()`

// count reads how many rows each of countedTables holds, those the database has.
func count(ctx context.Context, pool *store.Pool) (Counts, error) {
	held, err := evalfixture.ExistingTables(ctx, pool, countedTables)
	if err != nil {
		return nil, err
	}
	counts := Counts{}
	for _, table := range held {
		var rows int64
		if err := pool.QueryRow(ctx, `select count(*) from `+table).Scan(&rows); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = rows
	}
	return counts, nil
}

// Lines is the counts as one line a table, in countedTables' order.
func (c Counts) Lines() []string {
	var lines []string
	for _, table := range countedTables {
		if rows, held := c[table]; held {
			lines = append(lines, fmt.Sprintf("%s=%d", table, rows))
		}
	}
	return lines
}
