// Package benchtime times one Dispatch statement against another on the seeded measurement
// database that DISPATCH_BENCH_DATABASE_URL names, and binds the rows those statements read.
//
// It is INFRA-325's protocol 1, the committed harness: a named pair of statement texts - a
// before text and an after text - whose difference is SQL text alone, run alternately on one
// connection, each run's time read as planning plus execution from the EXPLAIN output's own two
// lines, the median of runs 2 to 12 reported, both plans printed, and the pair failed when the
// after text's median is not below the before text's (Pair). The details are the measurement: one
// connection for the whole pair, so the cold cache is paid once and by both sides; alternating
// runs, so a drift in the machine moves both sides together; the first run discarded, so a cache
// fill is not counted and eleven readings decide the median; and the plan printed beside the
// number, because a time alone does not say why a statement costs what it costs.
//
// A side of a pair can be a series: the several statements a read sends in turn, each run of
// them explaining each statement in turn in the run's one transaction, the run's time the sum of
// their planning and execution times and its plan every statement's plan, in order. PairSeries
// is Pair with a series for its after side, for a read that sends several statements where the
// before text was one, and it keeps every rule of Pair. PairWithin is a pair of two series that
// fails when the after median is above the before median times 1.10 plus 0.505 ms rather than
// when it is not below it: it is the pair of a statement whose change keys its reads rather than
// making it faster, where a slower after text is a regression and a faster one is not required.
//
// A before text is the text the caller measures against, which each package holds as its
// equality oracle and reads from there rather than copying it again. For every pair of api and
// docs and for refs' two backlink counts, it is the text the release before migration
// 20261101182419 ran, except that api's readings named "over main's document arm" take this
// release's statement with that release's document arm in its place.
//
// Report is that same two-sided reading with no bound, for a statement whose gain is real but
// smaller than the run-to-run spread of the parts it shares a statement with: the gate then
// belongs on the piece whose text differs, and the pair over that piece is what fails.
//
// A statement whose gain needs a table a migration of this release adds has no such pair: no
// one schema holds both sides of it. Before and After measure one side of such a statement
// alone, each line naming the side it read, the after side on this release's schema and the
// before side on a database migrated from the release before it, which holds none of this
// release's migrations, 20261101182419 to 20261101182522; that is protocol 2. A statement whose
// gain needs an index of this release has a pair all the same: its before side's Setup puts the
// release before's index back in the measuring transaction, which explain rolls back, so one
// database holds both sides and the two differ by that setup rather than by their text alone.
// AfterSeries measures a series alone and bounds nothing, as After does, for a read's new work
// that no before text did and so has nothing to be measured against.
//
// It is an ordinary package rather than a _test.go file because all three statement-owning
// packages (api, refs, docs) run it and Go cannot share test-only code across packages, which is
// why store/storetest is one too. CI never runs a measurement: every entry point skips while
// DISPATCH_BENCH_DATABASE_URL is unset, the gate api/search_bench_test.go and
// api/related_recall_test.go already use.
package benchtime

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// DatabaseURLEnv names the seeded measurement database (cmd/dispatch-seed). It is deliberately
// not DISPATCH_TEST_DATABASE_URL: a measurement must never run against the cluster the Go suites
// template their databases from, whose durability is off and whose cache is 128 MB.
const DatabaseURLEnv = "DISPATCH_BENCH_DATABASE_URL"

const (
	// runs is how many times each side of a pair is explained, and discarded how many of those
	// the median leaves out: runs 2 to 12, eleven readings, the first paying whatever of the
	// statement's pages the cache did not hold.
	runs      = 12
	discarded = 1

	// withinSpread is PairWithin's absolute term (withinBound): the spread of one loader
	// statement against itself at the list sizes its pairs bind, the largest below 25 ms read at
	// --scale 1 with jit off, the block loader at 256 ids reading 3.871 against 3.366 ms. The
	// tenth withinBound adds besides covers the larger statements, whose spread grows with them.
	withinSpread = 505 * time.Microsecond
)

// Statement is one statement text with the arguments one call binds to it. The two sides of a
// pair take their own arguments, since a before text and an after text need not have the same
// parameter list: the listing's paged text takes a limit and an offset the text it replaced did
// not.
type Statement struct {
	SQL  string
	Args []any
	// Setup are the statements the caller runs in the measuring transaction before the EXPLAIN,
	// which explain rolls back at the end of the run, so nothing a setup changes outlasts it:
	// `set local` settings, or a change of an index for the one read. The related-item check's
	// meaning query raises hnsw.ef_search to 400 for its own transaction, and a measurement under
	// pgvector's default 40 would be of a statement production never runs; the before side of a
	// read whose gain needs an index of this release puts back the index the release before read
	// through.
	Setup []string
}

// Runner holds the one connection every measurement of a run shares and the subjects it bound.
// One connection is what makes a pair a comparison rather than two numbers: both sides see the
// same session settings, the same backend and the same local buffers, and whichever side runs
// first pays for the pages the other then finds cached.
type Runner struct {
	conn     *pgx.Conn
	subjects Subjects
}

// Open connects to the measurement database, records the server settings every number is read
// beside, and binds the subjects. databaseURL is what DatabaseURLEnv names, read by the caller:
// a non-test file of internal/dispatch may not read the process environment, which
// internal/textcheck's TestNoReaderBypassesTheSettingsTable holds, so each _test.go caller passes
// os.Getenv(benchtime.DatabaseURLEnv) in. The gate itself stays here: an empty URL skips t,
// which is how CI never runs a measurement, and the three callers share that one sentence
// rather than each keeping a copy of it.
func Open(t *testing.T, databaseURL string) *Runner {
	t.Helper()
	if databaseURL == "" {
		t.Skip(DatabaseURLEnv + " must name the seeded measurement database (cmd/dispatch-seed); CI leaves it unset")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("read %s: %v", DatabaseURLEnv, err)
	}
	// Every run re-plans. pgx's default mode names a prepared statement and caches it, and
	// Postgres switches such a statement to a generic plan from its sixth execution, whose
	// planning time is near zero and whose plan is not the one the measured call gets; the
	// median of runs 2 to 12 would then be eleven readings of two different things.
	config.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	// JIT off, as production runs: Cloud SQL does not support PostgreSQL's LLVM JIT, and under
	// the default jit = on a measurement times compilation production never pays, which added 53
	// to 64 ms to each run of main's closure statement at --scale 1. logSettings reads jit from
	// pg_settings, which gives this session's value, so the settings line shows it.
	config.RuntimeParams["jit"] = "off"
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the measurement database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	runner := &Runner{conn: conn}
	runner.logSettings(t)
	runner.subjects = bindSubjects(t, conn)
	t.Logf("measurement subjects: %s", runner.subjects)
	return runner
}

// Subjects are the rows the statements read.
func (r *Runner) Subjects() Subjects { return r.subjects }

// Conn is the measurement connection, for a subject a single package reads and no other does:
// the mark ids of a document, the ask ids of an issue. A caller writes no row with it, and may
// change an index on it only inside a transaction it rolls back: an index change that committed
// would outlast the run and change the plan of every statement measured after it.
func (r *Runner) Conn() *pgx.Conn { return r.conn }

// beforeSide and afterSide name the two sides of a pair in every line this package logs and in
// the error a pair fails with, which points a reader back at those lines by name. They are
// written once here so the two always read the same.
const (
	beforeSide = "the before text"
	afterSide  = "the after text"
)

// Pair measures a statement's before text against its after text under protocol 1 and fails when
// the after text's median is not below the before text's. Each pair is a subtest of its own, so a
// pair that does not improve names itself and leaves the rest of the run to report their own
// numbers.
func (r *Runner) Pair(t *testing.T, name string, before, after Statement) {
	t.Helper()
	r.pair(t, name, []Statement{before}, []Statement{after}, requireBelow)
}

// PairSeries is Pair with a series for its after side, for a read that sends several statements
// where the before text was one: each run of the after side explains each of them in turn in one
// transaction, and its time is the sum of theirs. It keeps every rule of Pair, and the after
// side's line holds every plan of its median run.
func (r *Runner) PairSeries(t *testing.T, name string, before Statement, after []Statement) {
	t.Helper()
	r.pair(t, name, []Statement{before}, after, requireBelow)
}

// PairWithin measures two series as PairSeries measures its after side, and fails when the after
// median is above the before median times 1.10 plus 0.505 ms (withinBound) rather than when it is
// not below it. It is the pair of a statement whose change keys its reads rather than making it
// faster, where a slower after text is a regression and a faster one is not required.
func (r *Runner) PairWithin(t *testing.T, name string, before, after []Statement) {
	t.Helper()
	r.pair(t, name, before, after, requireWithin)
}

// rule is what a pair holds its after median to, against its before median, failing t naming
// the pair when the after median breaks it.
type rule func(t *testing.T, name string, before, after time.Duration)

// pair measures the two sides as one subtest named name and holds their medians to holds.
func (r *Runner) pair(t *testing.T, name string, before, after []Statement, holds rule) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		beforeMedian, afterMedian := r.bothSides(t, name, before, after)
		holds(t, name, beforeMedian.took, afterMedian.took)
	})
}

// requireBelow is the rule of Pair and PairSeries: the after median below the before median.
func requireBelow(t *testing.T, name string, before, after time.Duration) {
	t.Helper()
	if after >= before {
		t.Errorf("%s: the after text's median is %v, not below the before text's %v; %s",
			name, after, before, plansLogged(name))
	}
}

// requireWithin is the rule of PairWithin: the after median at most withinBound of the before
// median.
func requireWithin(t *testing.T, name string, before, after time.Duration) {
	t.Helper()
	if bound := withinBound(before); after > bound {
		t.Errorf("%s: the after text's median is %v, above %v, the before text's %v times 1.10 plus %v; %s",
			name, after, bound, before, withinSpread, plansLogged(name))
	}
}

// withinBound is the most PairWithin lets an after median read against before, the before
// median: before times 1.10, plus withinSpread.
func withinBound(before time.Duration) time.Duration {
	return before + before/10 + withinSpread
}

// plansLogged points the reader of a failed pair at the two lines its plans were logged in, by
// the name each carries.
func plansLogged(name string) string {
	return fmt.Sprintf("both plans are logged above this line, under %q and %q",
		name+": "+beforeSide, name+": "+afterSide)
}

// Report measures the same two sides as Pair and bounds neither, for a statement whose gain is
// real but smaller than the run-to-run spread of the parts it shares a statement with: the gate
// belongs on the piece that differs, and the two end-to-end numbers are still what a reader of
// that gate asks for next.
func (r *Runner) Report(t *testing.T, name string, before, after Statement) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		r.bothSides(t, name, []Statement{before}, []Statement{after})
	})
}

// bothSides runs the two sides alternately and answers their medians, each logged beside the name
// of the side it was read from.
func (r *Runner) bothSides(t *testing.T, name string, before, after []Statement) (reading, reading) {
	t.Helper()
	requireStatements(t, name, beforeSide, before)
	requireStatements(t, name, afterSide, after)
	beforeRuns := make([]reading, 0, runs)
	afterRuns := make([]reading, 0, runs)
	for range runs {
		beforeRuns = append(beforeRuns, r.explain(t, beforeSide, before))
		afterRuns = append(afterRuns, r.explain(t, afterSide, after))
	}
	beforeMedian := median(beforeRuns[discarded:])
	afterMedian := median(afterRuns[discarded:])
	logSide(t, name, beforeSide, beforeMedian)
	logSide(t, name, afterSide, afterMedian)
	return beforeMedian, afterMedian
}

// Before and After measure one side of a statement whose two sides no one schema holds, because
// its gain needs a table a migration of this release adds: the after text runs only on this
// release's schema and the before text only on a database migrated from the release before it.
// A gain that needs an index alone is a pair, whose before side's Setup puts the release
// before's index back (Statement.Setup). Neither sets a bound, since the two readings come from
// two runs and putting them together is the caller's; each names the side it measured, so
// neither can be read as the other's.
func (r *Runner) Before(t *testing.T, name string, before Statement) {
	t.Helper()
	r.oneSide(t, name, beforeSide, []Statement{before})
}

func (r *Runner) After(t *testing.T, name string, after Statement) {
	t.Helper()
	r.oneSide(t, name, afterSide, []Statement{after})
}

// AfterSeries measures a series alone as After measures one statement, and bounds nothing: it is
// for a read's new work that no before text did, so nothing is there to measure it against. Its
// line holds every plan of its median run.
func (r *Runner) AfterSeries(t *testing.T, name string, after []Statement) {
	t.Helper()
	r.oneSide(t, name, afterSide, after)
}

func (r *Runner) oneSide(t *testing.T, name, side string, series []Statement) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		requireStatements(t, name, side, series)
		readings := make([]reading, 0, runs)
		for range runs {
			readings = append(readings, r.explain(t, side, series))
		}
		logSide(t, name, side, median(readings[discarded:]))
	})
}

// requireStatements fails the measurement name when its side is a series of no statement: a
// run of it would explain nothing and take no time, the fastest reading there is.
func requireStatements(t *testing.T, name, side string, series []Statement) {
	t.Helper()
	if len(series) == 0 {
		t.Fatalf("%s: %s is a series of no statement, so there is nothing to measure", name, side)
	}
}

// logSide is the one line every measurement is reported in, so that a reading can never reach a
// log without the side it was read from.
func logSide(t *testing.T, name, side string, taken reading) {
	t.Helper()
	t.Logf("%s: %s, planning plus execution %v (median of runs %d to %d), its median run's plan:\n%s",
		name, side, taken.took, discarded+1, runs, taken.plan)
}

// reading is one run of a side under EXPLAIN (ANALYZE, BUFFERS): planning plus execution as that
// run's own output reports them, summed over its statements, and the plan text it reported them
// in.
type reading struct {
	took time.Duration
	plan string
}

// explain runs one run of a side: each statement of series in turn under EXPLAIN (ANALYZE,
// BUFFERS), after its own Setup, inside one transaction it rolls back, as a read that sends
// several statements sends them in one transaction. The rollback is what lets the anchor reads
// be measured at all: two of them end in `for share`, and the row locks an analysed run takes
// are held until the transaction ends.
func (r *Runner) explain(t *testing.T, side string, series []Statement) reading {
	t.Helper()
	ctx := context.Background()
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a measurement of %s: %v", side, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plans := make([]string, 0, len(series))
	for index, statement := range series {
		measured := side
		if len(series) > 1 {
			measured += ", " + statementPlace(index, len(series))
		}
		for _, setting := range statement.Setup {
			if _, err := tx.Exec(ctx, setting); err != nil {
				t.Fatalf("%s before a measurement of %s: %v", setting, measured, err)
			}
		}
		rows, err := tx.Query(ctx, "explain (analyze, buffers) "+statement.SQL, statement.Args...)
		if err != nil {
			t.Fatalf("explain %s: %v", measured, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the plan of %s: %v", measured, err)
		}
		plans = append(plans, strings.Join(lines, "\n"))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back a measurement of %s: %v", side, err)
	}
	taken, err := runReading(plans)
	if err != nil {
		t.Fatalf("%s: %v", side, err)
	}
	return taken
}

// runReading is the reading of one run from the plans of its statements, in the order it sent
// them: its time the sum of each plan's planning plus execution, and its plan text every plan
// in turn. A run of several statements writes each plan under a line naming the statement's
// place in the run and its own time, so the median run's line says which statement cost what; a
// run of one writes its plan alone.
func runReading(plans []string) (reading, error) {
	var taken reading
	texts := make([]string, len(plans))
	for index, plan := range plans {
		took, err := planTime(plan)
		if err != nil {
			if len(plans) > 1 {
				err = fmt.Errorf("%s: %w", statementPlace(index, len(plans)), err)
			}
			return reading{}, fmt.Errorf("%w\n%s", err, plan)
		}
		taken.took += took
		texts[index] = plan
		if len(plans) > 1 {
			texts[index] = fmt.Sprintf("%s, planning plus execution %v:\n%s", statementPlace(index, len(plans)), took, plan)
		}
	}
	taken.plan = strings.Join(texts, "\n")
	return taken, nil
}

// statementPlace names the statement at index of a run of count statements.
func statementPlace(index, count int) string {
	return fmt.Sprintf("statement %d of %d", index+1, count)
}

// planningLine and executionLine read the two times EXPLAIN (ANALYZE) reports in its own output.
// The time of record is their sum: a statement whose plan costs more than it saves is no faster,
// and the issue list's paged text is planned on every call.
var (
	planningLine  = regexp.MustCompile(`(?m)^\s*Planning Time: ([0-9.]+) ms\s*$`)
	executionLine = regexp.MustCompile(`(?m)^\s*Execution Time: ([0-9.]+) ms\s*$`)
)

// planTime is a plan's planning plus execution time. A plan missing either line is an error
// rather than a zero: a missing time would read as the fastest run there is and would win every
// median it is in.
func planTime(plan string) (time.Duration, error) {
	var total time.Duration
	for _, line := range []struct {
		what    string
		pattern *regexp.Regexp
	}{{"Planning Time", planningLine}, {"Execution Time", executionLine}} {
		match := line.pattern.FindStringSubmatch(plan)
		if match == nil {
			return 0, fmt.Errorf("the plan carries no %s line", line.what)
		}
		milliseconds, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, fmt.Errorf("read the %s line %q: %w", line.what, match[0], err)
		}
		total += time.Duration(milliseconds * float64(time.Millisecond))
	}
	return total, nil
}

// median is the middle reading of an odd number of them, the whole reading and not its time
// alone, so the plan printed beside a number is the run that gave that number.
func median(readings []reading) reading {
	sorted := slices.Clone(readings)
	slices.SortFunc(sorted, func(a, b reading) int { return cmp.Compare(a.took, b.took) })
	return sorted[len(sorted)/2]
}

// settingsRecorded are the server settings every measurement is recorded beside: the plan asks
// for the measurement cluster's durability and memory with each number, and a plan read under
// another shared_buffers or another effective_cache_size is a different measurement.
var settingsRecorded = []string{
	"server_version", "shared_buffers", "work_mem", "maintenance_work_mem", "effective_cache_size",
	"fsync", "synchronous_commit", "full_page_writes", "random_page_cost", "jit",
	"max_parallel_workers_per_gather",
}

func (r *Runner) logSettings(t *testing.T) {
	t.Helper()
	rows, err := r.conn.Query(context.Background(),
		`select name, setting, coalesce(unit, '') from pg_settings where name = any($1) order by name`,
		settingsRecorded)
	if err != nil {
		t.Fatalf("read the server settings: %v", err)
	}
	settings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var name, setting, unit string
		if err := row.Scan(&name, &setting, &unit); err != nil {
			return "", err
		}
		return name + "=" + setting + unit, nil
	})
	if err != nil {
		t.Fatalf("read the server settings: %v", err)
	}
	t.Logf("measurement cluster: %s", strings.Join(settings, " "))
}
