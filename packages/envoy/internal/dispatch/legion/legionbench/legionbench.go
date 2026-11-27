// Package legionbench loads INFRA's history at root R14's size (dispatch://INFRA-979/spec#b-r14)
// into a database a Dispatch server has migrated, and stands in for INFRA's Legion daemon, for the
// two Jobs of dispatch://INFRA-5577 that measure on the build farm: R8's (k14), the recompute time
// of an open `legion` view at ten times the stated run rate against `main`'s tip, and R7's (k27),
// the hold of the change log's lock by each of its writers during a first whole read of INFRA's
// facts (r8.sh and r7.sh say what each Job runs).
//
// Load writes, into a database the server has migrated and nothing has written, from one seed:
//
//   - INFRA, a project of INFRA's size: 13,103 issues over 365 days (35.9 a day), 67.4 percent
//     of them closed and 5 percent P0; 812,386 events, 62 an issue on average, each by a session
//     actor that carries its title, the Legion worker sessions titled `Legion <role> · <KEY>`; the
//     handoffs of each worked issue; and the daemon's phase, slot and claim observations of the
//     issues of the last 120 days (infra.go);
//   - INFRA's legion_settings row, naming Spec.StateURL, the daemon the server's Legion state
//     intake reads whole (StandIn serves it);
//   - INFRA's delivery history: 5,402 pull requests over three repositories, a fifth of them
//     rework, and over the last 200 days 20 deploy runs a day of 8 jobs and 60 pull-request runs
//     a day of 40 jobs, ten times as many runs under TenTimes (delivery.go).
//
// It writes no other project and no eval reading, and reads no eval table, so it loads a
// database `main`'s tip migrated as it loads one this head migrated: of the consumer tables it
// resets (resetConsumers) it touches only those the database holds (ExistingTables).
//
// Scale multiplies every count (issues, events, pull requests, runs a day) and keeps every span
// of days and the shape of each row, so a small load in a pod exercises every table and every
// read the full one does.
//
// The loader, the history's shape and the delivery history are evals/evalbench's of
// cybermonklabs/legion#3976 (Load, infra.go and delivery.go), whose calls of its fixture package
// are this package's own code here: a file of it that is not a test imports no seeder of the test
// rigs (internal/textcheck's TestOnlyTestFilesImportTheDeliveryAndLegionSeeders).
package legionbench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// Project is the project of INFRA's size; its key is the one the measurements read.
const Project = "INFRA"

// The stated rate: root R14's INFRA volumes (dispatch://INFRA-979/spec#b-r14) and its run rate.
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
	// observedDays is how far back Dispatch recorded INFRA's daemon state: the issues opened in
	// the last 120 days carry observations and the older ones the event log alone, so the
	// project's first observation falls inside the history, as it does for a project whose
	// daemon Dispatch began to read partway through.
	observedDays = 120
)

// Repos are INFRA's three repositories, the deploy repository first: the one whose runs Dispatch
// reads and whose pull requests a deploy ships.
var Repos = []string{"acme/infra-platform", "acme/infra-api", "acme/infra-web"}

// actor is the actor every row this package writes that records one names.
var actor = model.Actor{Kind: "system", ID: "legionbench"}

// Spec is one load: the moment the history ends, the seed of every draw, the scale, the run rate,
// the daemon INFRA's setting names, and whether the load must leave the facts for the server's
// first whole read.
type Spec struct {
	// Now is the moment every history ends at. Every time is relative to it, so two loads with
	// one Now and one Seed hold the same history at the same times.
	Now  time.Time
	Seed uint64
	// Scale multiplies every count; 1 is INFRA's stated rate. It is above 0 and at most 1.
	Scale float64
	// TenTimes writes ten times the stated run rate: 200 deploy runs and 600 pull-request runs a
	// day (R8's load).
	TenTimes bool
	// StateURL is the daemon address INFRA's legion_settings row names: the stand-in's
	// (StandIn), which the server's Legion state intake reads whole as it starts, where the
	// server has NATS. Required.
	StateURL string
	// FirstRead holds the load to what R7's Job measures: a database whose issue_facts holds no
	// row and whose issue_facts_computed records no build, so the server that boots on it makes
	// its first whole read of INFRA's facts. Load refuses to finish otherwise, and refuses a
	// database that has no issue_facts.
	FirstRead bool
}

// scaled is count at the spec's scale, at least floor.
func (s Spec) scaled(count float64, floor int) int {
	return max(floor, int(math.Round(count*s.Scale)))
}

// Counts is what one load wrote, by table, read back from the database once it is done.
type Counts map[string]int64

// countedTables are the tables Counts reads, in the order it prints them; `main`'s tip lacks the
// last three.
var countedTables = []string{
	"projects", "issues", "events", "handoffs", "legion_observations", "legion_settings",
	"delivery_pull_requests", "delivery_runs", "delivery_run_jobs", "session_titles",
	"issue_facts", "issue_facts_computed", "delivery_changes_pending",
}

// consumerTables are the tables of the Legion facts consumer: its rows, its filings, its cursors,
// its record of each project's whole read, and the outbox its triggers record their changes in.
// `main`'s tip has none of them; resetConsumers and the FirstRead check touch only those the
// database holds.
var consumerTables = []string{"issue_facts", "issue_filings", "issue_facts_cursor", "issue_facts_computed", "delivery_changes_pending"}

// Load writes the history spec names into the database, which the server must have migrated and
// nothing else written, and answers the counts it then holds. logf reports each step as it ends.
//
// The server's facts consumer committed its cursors over the empty store when the server that
// migrated it booted (at this head; `main`'s tip has none); Load deletes them last, so the next
// server to boot starts the consumer at the heads of the feeds and reads INFRA whole, as on a
// store a seeder wrote before any pass ran, rather than paging through the history as a stream of
// new changes. It then writes the keyword fill's cursor as caught up with the loaded log
// (keywordFillCaughtUp), on a database that has the fill.
func Load(ctx context.Context, database *store.Store, spec Spec, logf func(format string, args ...any)) (Counts, error) {
	switch {
	case spec.Scale <= 0 || spec.Scale > 1:
		return nil, fmt.Errorf("the scale is above 0 and at most 1, not %g", spec.Scale)
	case spec.Now.IsZero():
		return nil, errors.New("name the moment the history ends")
	case strings.TrimSpace(spec.StateURL) == "":
		return nil, errors.New("name the daemon INFRA's legion_settings row reads (the stand-in's URL)")
	}
	spec.Now = spec.Now.UTC()
	pool := database.Pool
	if spec.FirstRead {
		held, err := ExistingTables(ctx, pool, []string{"issue_facts", "issue_facts_computed"})
		if err != nil {
			return nil, err
		}
		if len(held) != 2 {
			return nil, fmt.Errorf("-first-read loads for the server's first whole read of the Legion facts, and this database holds only %v of issue_facts and issue_facts_computed", held)
		}
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
	if err := SettleNumbers(ctx, pool, []string{Project}); err != nil {
		return nil, err
	}
	if err := resetConsumers(ctx, pool); err != nil {
		return nil, err
	}
	// Autovacuum would analyze the loaded tables in its own time; the measurements read plans,
	// so the statistics a long-running store holds are taken now.
	if _, err := pool.Exec(ctx, `analyze`); err != nil {
		return nil, fmt.Errorf("analyze the loaded tables: %w", err)
	}
	step("analyze")
	counts, err := count(ctx, pool)
	if err != nil {
		return nil, err
	}
	if spec.FirstRead && (counts["issue_facts"] != 0 || counts["issue_facts_computed"] != 0) {
		return nil, fmt.Errorf("-first-read leaves issue_facts empty and no build in issue_facts_computed, and the load ends with %d and %d rows",
			counts["issue_facts"], counts["issue_facts_computed"])
	}
	return counts, nil
}

// checkEmpty refuses a database that holds a project, an issue or a delivery row: the load's
// counts are the history's only where nothing else is there, and its consumer reset is right only
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

// resetConsumers empties what the Legion facts consumer wrote over the empty store (Load), in the
// tables of consumerTables the database holds: `truncate`, which fires no row trigger, so the
// reset logs no change of its own. It then writes the keyword fill's cursor as caught up with the
// loaded log (keywordFillCaughtUp).
func resetConsumers(ctx context.Context, pool *store.Pool) error {
	held, err := ExistingTables(ctx, pool, consumerTables)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		if _, err := pool.Exec(ctx, `truncate `+strings.Join(held, ", ")); err != nil {
			return fmt.Errorf("reset the Legion facts consumer (%s): %w", strings.Join(held, ", "), err)
		}
	}
	return keywordFillCaughtUp(ctx, pool)
}

// keywordFillCaughtUp writes the keyword fill's one cursor row as production's stands once its one
// backfill, at the deploy, is done: at the newest event of the loaded log, with that event's
// created_at, which the fill checks the log against, and no reach, which the fill computes on its
// next read. The row names no holder, as a fill with no hold writes it, so the first batch of a
// serving fill takes it over. A row the migrating server's fill wrote keeps the rules version it
// wrote, the running build's; where it wrote none, the row takes this build's
// (textversions.RulesVersion). The keyword rows themselves are not written, since no read the
// measurements make reads them: a backfill of the loaded log by the server would take hours at
// the stated rate, beside every recompute and every hold the Jobs measure. A database without the
// fill is left alone.
func keywordFillCaughtUp(ctx context.Context, pool *store.Pool) error {
	held, err := ExistingTables(ctx, pool, []string{"search_text_versions_fill"})
	if err != nil || len(held) == 0 {
		return err
	}
	if _, err := pool.Exec(ctx, `
		with newest as (select id, created_at from events order by id desc limit 1)
		insert into search_text_versions_fill (id, event_id, event_at, reach, holder, rules)
		select 1, coalesce((select id from newest), 0), (select created_at from newest), null, null, $1::smallint
		on conflict (id) do update
		   set event_id = excluded.event_id, event_at = excluded.event_at, reach = null,
		       holder = null, updated_at = now()`, textversions.RulesVersion); err != nil {
		return fmt.Errorf("write the keyword fill's cursor as caught up: %w", err)
	}
	return nil
}

// SettleNumbers moves each project's next issue number past every issue it holds, so the next
// issue the server creates in it takes a number no loaded issue holds.
func SettleNumbers(ctx context.Context, pool *store.Pool, projects []string) error {
	if _, err := pool.Exec(ctx, `
		update projects p
		set next_number = greatest(p.next_number, (select coalesce(max(i.number), 0) + 1 from issues i where i.project_key = p.key))
		where p.key = any($1)
	`, projects); err != nil {
		return fmt.Errorf("settle the next issue numbers of %s: %w", strings.Join(projects, ", "), err)
	}
	return nil
}

// ExistingTables is those of tables the database holds in its search path, in the order tables
// names them: what the load reads a database's head by.
func ExistingTables(ctx context.Context, pool *store.Pool, tables []string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select name from unnest($1::text[]) with ordinality as given(name, position)
		where to_regclass(name) is not null order by position`, tables)
	if err != nil {
		return nil, fmt.Errorf("read which tables the database holds: %w", err)
	}
	defer rows.Close()
	var held []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read which tables the database holds: %w", err)
		}
		held = append(held, name)
	}
	return held, rows.Err()
}

// count reads how many rows each of countedTables holds, those the database has.
func count(ctx context.Context, pool *store.Pool) (Counts, error) {
	held, err := ExistingTables(ctx, pool, countedTables)
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
