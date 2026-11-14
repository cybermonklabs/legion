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
//   - 2,800,000 readings over 730 days of INFRA's 20 logged evals, in measured_at order, through
//     the registry's own register write and readings write (readings.go).
//
// Base writes only the tables the base head 02a7b3c6 has, for k8's negative control: the same
// issues, events, observations and delivery history and the same smaller projects, and no reading
// (evalfixture.BaseLacks names what the base lacks).
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
	// Base writes only the base head's tables, for k8's negative control: no reading.
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

// countedTables are the tables Counts reads, in the order it prints them; the base head lacks the
// last five.
var countedTables = []string{
	"projects", "issues", "events", "handoffs", "legion_observations", "legion_settings",
	"repo_projects", "delivery_pull_requests", "delivery_runs", "delivery_run_jobs",
	"session_titles", "eval_versions", "eval_series", "eval_readings", "eval_days",
}

// Load writes the fixture spec names into the database, which the server must have migrated and
// nothing else written, and answers the counts it then holds. logf reports each step as it ends.
//
// The server's consumers that read the feeds this load writes (the Legion facts consumer and the
// eval day-row refresher) committed their cursors over the empty store when the server that
// migrated it booted; Load deletes those rows last, so the next server to boot starts each
// consumer at the heads of the feeds and reads every project whole, as on a store a seeder wrote
// before any pass ran, rather than paging through the history as a stream of new changes.
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
	if err := checkSchema(ctx, pool, spec.Base); err != nil {
		return nil, err
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

// checkSchema refuses a database of the wrong head: this head's mode needs the tables the base
// lacks, and the base mode refuses a database that holds them.
func checkSchema(ctx context.Context, pool *store.Pool, base bool) error {
	held, err := evalfixture.ExistingTables(ctx, pool, evalfixture.BaseLacks)
	if err != nil {
		return err
	}
	if base && len(held) > 0 {
		return fmt.Errorf("-base writes into the base head's schema, and this database holds %s, which are this head's", strings.Join(held, ", "))
	}
	if !base && len(held) < len(evalfixture.BaseLacks) {
		var missing []string
		for _, table := range evalfixture.BaseLacks {
			if !slices.Contains(held, table) {
				missing = append(missing, table)
			}
		}
		return fmt.Errorf("this head's mode writes into this head's schema, and the database lacks %s (a database of the base head takes -base)", strings.Join(missing, ", "))
	}
	return nil
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
// (Load): the Legion facts consumer's cursors and project marks, and this head's day-row
// refresher's cursors and walks.
func resetConsumers(ctx context.Context, pool *store.Pool, base bool) error {
	tables := []string{"issue_facts_cursor", "issue_facts_computed"}
	if !base {
		tables = append(tables, "eval_days_cursor", "eval_days_walks")
	}
	for _, table := range tables {
		if _, err := pool.Exec(ctx, `delete from `+table); err != nil {
			return fmt.Errorf("reset %s: %w", table, err)
		}
	}
	return nil
}

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
