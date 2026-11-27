package codesurvival

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// widgets is the repository every test measures, and firstDay the UTC midnight its days count from.
const widgets = "acme/widgets"

var firstDay = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// resultTables are the five tables of the results store, in the order a snapshot reads them.
var resultTables = []string{"code_repos", "code_changes", "code_pairs", "code_days", "code_projects"}

// dayN is the UTC midnight n days after firstDay.
func dayN(n int) time.Time { return firstDay.AddDate(0, 0, n) }

// shaOf is a 40-character lower-case hex SHA made of n.
func shaOf(n int) string { return fmt.Sprintf("%040x", n) }

// facts are a pull request's facts: its number, a head branch made of it, one author and an issue.
func facts(pr int, issue string) survival.ChangeFacts {
	return survival.ChangeFacts{PR: pr, HeadRef: "feature/" + strconv.Itoa(pr), Author: "alice", IssueKey: issue}
}

// changeOn is the change of commit seed, landed seed hours into day n.
func changeOn(n, seed, pr int, issue string, added, alive, removedByNonPR int) survival.Change {
	return survival.Change{SHA: shaOf(seed), Day: dayN(n), CommittedAt: dayN(n).Add(time.Duration(seed) * time.Hour),
		ChangeFacts: facts(pr, issue), Added: added, AliveAtMark: alive, RemovedByNonPR: removedByNonPR}
}

// fingerprint is a day's fingerprint, its digest made of seed.
func fingerprint(n, commits int, seed byte) survival.Fingerprint {
	return survival.Fingerprint{Day: dayN(n), Commits: commits, Digest: sha256.Sum256([]byte{seed})}
}

// measured is the result of a measured day.
func measured(n int) survival.DayResult {
	return survival.DayResult{Day: dayN(n), State: survival.DayMeasured}
}

// firstRun is what a run that measured days 0 and 1 hands ReplaceDays: three changes, a pair of
// each of two of them, and the fingerprints of the two days and of the two days after them.
func firstRun() ([]time.Time, survival.MeasureOutput) {
	c1 := changeOn(0, 1, 11, "ACME-1", 10, 6, 2)
	c2 := changeOn(0, 2, 12, "", 5, 5, 0)
	c3 := changeOn(1, 3, 13, "ACME-2", 8, 4, 0)
	return []time.Time{dayN(0), dayN(1)}, survival.MeasureOutput{
		Changes: []survival.Change{c1, c2, c3},
		Pairs: []survival.Pair{
			{AdderSHA: c1.SHA, RemoverSHA: shaOf(3), Remover: facts(13, "ACME-2"), Lines: 2, DaysBetween: 0.5},
			{AdderSHA: c3.SHA, RemoverSHA: shaOf(9), Remover: facts(19, ""), Lines: 4, DaysBetween: 2.25},
		},
		Fingerprints: []survival.Fingerprint{fingerprint(0, 3, 1), fingerprint(1, 2, 2), fingerprint(2, 1, 3), fingerprint(3, 0, 4)},
		Days:         []survival.DayResult{measured(0), measured(1)},
		PeakLines:    40,
	}
}

// begin opens a transaction that the test's end rolls back where nothing committed it.
func begin(t *testing.T, database *store.Store) pgx.Tx {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// replace stores a run's results in a transaction of their own, which it commits.
func replace(t *testing.T, database *store.Store, days []time.Time, out survival.MeasureOutput) {
	t.Helper()
	replaceWith(t, database, days, out, nil)
}

// replaceWith is replace with reasons, the reason of each blocking commit by its SHA.
func replaceWith(t *testing.T, database *store.Store, days []time.Time, out survival.MeasureOutput, reasons map[string]string) {
	t.Helper()
	tx := begin(t, database)
	if err := ReplaceDays(t.Context(), tx, widgets, days, out, time.Now(), reasons); err != nil {
		t.Fatalf("ReplaceDays: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the replace: %v", err)
	}
}

// snapshot is every row of tables as Postgres writes it as text, in order: two snapshots are equal
// where the tables hold the same rows.
func snapshot(t *testing.T, database *store.Store, tables ...string) []string {
	t.Helper()
	var rows []string
	for _, table := range tables {
		found, err := database.Pool.Query(t.Context(), `select '`+table+`: ' || t::text from `+table+` t order by 1`)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		listed, err := pgx.CollectRows(found, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		rows = append(rows, listed...)
	}
	return rows
}

// requireRefusal fails t unless err is Postgres's refusal with code, naming constraint.
func requireRefusal(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != code || refusal.ConstraintName != constraint {
		t.Fatalf("error = %v, want SQLSTATE %s naming %s", err, code, constraint)
	}
}

// allowed is every column of the five tables of the results store, with its type, and the only
// ones. Each is a count, a day or a time, a digest, or a name of one of the kinds R3 allows to be
// stored: a SHA, a repository, a branch, a login, an issue key, a state or a reason. No column holds
// the text of a patch, a file's contents or a list of a branch's commits.
var allowed = map[string]string{
	// Repositories: names, a branch, days and times, and a state with its reason.
	"code_repos.repo":             "text",
	"code_repos.branch":           "text",
	"code_repos.start_day":        "date",
	"code_repos.state":            "text",
	"code_repos.state_reason":     "text",
	"code_repos.checked_on":       "date",
	"code_repos.last_measured_on": "date",
	"code_repos.retry_at":         "timestamp with time zone",
	// Changes: the commit's SHA, its day and time, its pull request's number, head branch,
	// author's login and issue key, and the counts of its lines.
	"code_changes.repo":              "text",
	"code_changes.sha":               "text",
	"code_changes.day":               "date",
	"code_changes.committed_at":      "timestamp with time zone",
	"code_changes.pr_number":         "integer",
	"code_changes.head_ref":          "text",
	"code_changes.author":            "text",
	"code_changes.issue_key":         "text",
	"code_changes.added":             "integer",
	"code_changes.alive":             "integer",
	"code_changes.removed_by_non_pr": "integer",
	// Pairs: the adder's and the remover's SHAs, the remover's pull request, head branch,
	// author's login and issue key, the lines removed and the days between.
	"code_pairs.repo":              "text",
	"code_pairs.adder_sha":         "text",
	"code_pairs.remover_sha":       "text",
	"code_pairs.remover_pr":        "integer",
	"code_pairs.remover_head_ref":  "text",
	"code_pairs.remover_author":    "text",
	"code_pairs.remover_issue_key": "text",
	"code_pairs.lines":             "integer",
	"code_pairs.days_between":      "double precision",
	// Days: a count and a 32-byte digest, a state, its times, and a blocking commit's SHA with
	// the count of its files and a reason.
	"code_days.repo":           "text",
	"code_days.day":            "date",
	"code_days.commits":        "integer",
	"code_days.digest":         "bytea",
	"code_days.state":          "text",
	"code_days.measured_at":    "timestamp with time zone",
	"code_days.marked_at":      "timestamp with time zone",
	"code_days.blocked_sha":    "text",
	"code_days.blocked_files":  "integer",
	"code_days.blocked_reason": "text",
	"code_days.tried_at":       "timestamp with time zone",
	// Projects: a project's key, the digest of the groups its rows stand built under, and when.
	"code_projects.project_key":   "text",
	"code_projects.groups_digest": "bytea",
	"code_projects.built_at":      "timestamp with time zone",
}

// R3: no patch is kept, in the database or on disk. This is the inventory of the database side
// that no run's data can show: every column of the five tables, its name and its type, against the
// list above, which holds no text column but SHAs, repositories, branches, logins, issue keys,
// states and reasons, and no bytea but two digests. It fails at zero columns, at a column the list
// lacks, at a column the table lacks, and at a sixth table named for code.
func TestTheResultsTablesHoldNoTextOfAPatch(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	rows, err := database.Pool.Query(t.Context(), `
		select table_name || '.' || column_name, data_type
		from information_schema.columns
		where table_schema = current_schema() and table_name = any($1)
		order by table_name, ordinal_position`, resultTables)
	if err != nil {
		t.Fatalf("read information_schema.columns: %v", err)
	}
	type column struct{ Name, DataType string }
	columns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[column])
	if err != nil {
		t.Fatalf("read the columns: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("the five tables hold no column, so none was checked")
	}
	seen := map[string]bool{}
	for _, c := range columns {
		seen[c.Name] = true
		want, listed := allowed[c.Name]
		if !listed {
			t.Errorf("%s (%s) is on no list: a column of the results store is a count, a day, a digest or a SHA, a repository, a branch, a login, an issue key, a state or a reason, never text of a patch", c.Name, c.DataType)
			continue
		}
		if c.DataType != want {
			t.Errorf("%s is %s, want %s", c.Name, c.DataType, want)
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Errorf("%s is on the list and in no table", name)
		}
	}

	tables, err := database.Pool.Query(t.Context(), `
		select table_name from information_schema.tables
		where table_schema = current_schema() and table_name like 'code\_%' order by table_name`)
	if err != nil {
		t.Fatalf("read information_schema.tables: %v", err)
	}
	named, err := pgx.CollectRows(tables, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables: %v", err)
	}
	if want := slices.Sorted(slices.Values(resultTables)); !slices.Equal(named, want) {
		t.Errorf("the tables named code_ are %v, want %v: a table the inventory does not list is a table it does not check", named, want)
	}
}

// R3 and R8: a run's results go in whole. The selected days' rows of code_changes, code_pairs and
// code_days are replaced in the caller's transaction, so a replace that fails midway, on a row of
// any of its statements, leaves every earlier row as it was once the caller rolls back, and one that
// commits leaves exactly the new rows of those days and none of the old: the other days' rows stand
// as they were, a replayed day's fingerprint is written again, and a day that holds a result is not
// overwritten by a fingerprint.
func TestTheSelectedDaysAreReplacedWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	days, out := firstRun()
	replace(t, database, days, out)
	before := snapshot(t, database, resultTables...)
	if len(before) != 3+2+4 {
		t.Fatalf("the first run stored %d rows %v, want 3 changes, 2 pairs and 4 days", len(before), before)
	}

	// A new result for day 0: a good change and a pair, then the row that fails.
	good := changeOn(0, 21, 31, "ACME-9", 6, 6, 0)
	goodPair := survival.Pair{AdderSHA: good.SHA, RemoverSHA: shaOf(23), Remover: facts(33, ""), Lines: 1, DaysBetween: 1}
	fails := map[string]struct {
		days       []time.Time
		build      func(*survival.MeasureOutput)
		constraint string
	}{
		"the last change of the day outruns its added lines": {
			days: []time.Time{dayN(0)},
			build: func(out *survival.MeasureOutput) {
				out.Changes = append(out.Changes, changeOn(0, 22, 32, "", 3, 3, 1))
			},
			constraint: "code_changes_lines_fit_in_added",
		},
		"the last pair has no lines": {
			days: []time.Time{dayN(0)},
			build: func(out *survival.MeasureOutput) {
				out.Pairs = append(out.Pairs, survival.Pair{AdderSHA: good.SHA, RemoverSHA: shaOf(24), Remover: facts(34, ""), DaysBetween: 1})
			},
			constraint: "code_pairs_lines_check",
		},
		// The days are written after every change and pair, so this one fails on the last statement.
		"the last day is blocked by a commit with no file": {
			days: []time.Time{dayN(0), dayN(1)},
			build: func(out *survival.MeasureOutput) {
				out.Days = []survival.DayResult{measured(0), {Day: dayN(1), State: survival.DayBlocked, BlockedSHA: shaOf(25)}}
			},
			constraint: "code_days_blocked_files_check",
		},
	}
	for name, failure := range fails {
		t.Run(name, func(t *testing.T) {
			rerun := survival.MeasureOutput{
				Changes:      []survival.Change{good},
				Pairs:        []survival.Pair{goodPair},
				Fingerprints: []survival.Fingerprint{fingerprint(0, 1, 40), fingerprint(1, 1, 41)},
				Days:         []survival.DayResult{measured(0)},
			}
			failure.build(&rerun)
			tx := begin(t, database)
			err := ReplaceDays(t.Context(), tx, widgets, failure.days, rerun, time.Now(), nil)
			requireRefusal(t, err, "23514", failure.constraint)
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatalf("roll back the failed replace: %v", err)
			}
			if after := snapshot(t, database, resultTables...); !slices.Equal(after, before) {
				t.Fatalf("a replace that failed and rolled back changed the store:\nbefore %v\nafter  %v", before, after)
			}
		})
	}

	// The same result without the failing row commits: day 0 holds exactly the new rows.
	rerun := survival.MeasureOutput{
		Changes: []survival.Change{good},
		Pairs:   []survival.Pair{goodPair},
		Fingerprints: []survival.Fingerprint{
			fingerprint(0, 1, 40),
			fingerprint(1, 7, 41), // a measured day that a run did not select: never overwritten
			fingerprint(2, 7, 42), // a replayed day: written again
			fingerprint(4, 2, 43), // a day with no row yet: written
		},
		Days: []survival.DayResult{measured(0)},
	}
	replace(t, database, []time.Time{dayN(0)}, rerun)

	changes, err := Changes(t.Context(), database.Pool, widgets, []time.Time{dayN(0), dayN(1)})
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if got := shas(changes); !slices.Equal(got, []string{shaOf(21), shaOf(3)}) {
		t.Errorf("the changes of days 0 and 1 are %v, want the new change of day 0 and the old one of day 1, and no change of the old day 0", got)
	}
	pairs, err := Pairs(t.Context(), database.Pool, widgets, []time.Time{dayN(0), dayN(1)})
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	if len(pairs) != 2 || pairs[0].AdderSHA != shaOf(21) || pairs[1].AdderSHA != shaOf(3) {
		t.Errorf("the pairs of days 0 and 1 are %+v, want the new pair of day 0 and the old pair of day 1, and no pair of an old change of day 0", pairs)
	}
	var orphans int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from code_pairs where adder_sha in ($1, $2)`, shaOf(1), shaOf(2)).Scan(&orphans); err != nil || orphans != 0 {
		t.Errorf("pairs of the replaced changes left = %d (%v), want 0", orphans, err)
	}
	prints, err := FingerprintsFrom(t.Context(), database.Pool, widgets, dayN(0))
	if err != nil {
		t.Fatalf("FingerprintsFrom: %v", err)
	}
	want := []survival.Fingerprint{
		fingerprint(0, 1, 40),
		fingerprint(1, 2, 2), // the measured day 1 keeps the fingerprint of its own run
		fingerprint(2, 7, 42),
		fingerprint(3, 0, 4),
		fingerprint(4, 2, 43),
	}
	if !reflect.DeepEqual(prints, want) {
		t.Errorf("the fingerprints from day 0 are %+v, want %+v", prints, want)
	}
	statuses, err := DayStatuses(t.Context(), database.Pool, widgets, dayN(0), dayN(4))
	if err != nil {
		t.Fatalf("DayStatuses: %v", err)
	}
	var states []survival.DayState
	for _, status := range statuses {
		states = append(states, status.State)
	}
	if want := []survival.DayState{survival.DayMeasured, survival.DayMeasured, survival.DayReplayed, survival.DayReplayed, survival.DayReplayed}; !slices.Equal(states, want) {
		t.Errorf("the states of days 0 to 4 are %v, want %v", states, want)
	}
}

// shas is the SHA of each change.
func shas(changes []survival.Change) []string {
	var out []string
	for _, change := range changes {
		out = append(out, change.SHA)
	}
	return out
}

// A replace that is handed what cannot be written whole writes nothing and says why: a selected day
// with no result, a result of a day nobody selected, a result that is no measured or blocked day, a
// change of a day that holds no measurement, a pair of no change of the result, a fingerprint a
// selected day lacks, a day that is no UTC midnight, and no day at all.
func TestAReplaceRefusesAnOutputThatDoesNotMatchItsDays(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	days, out := firstRun()
	replace(t, database, days, out)
	before := snapshot(t, database, resultTables...)

	cases := []struct {
		name   string
		days   []time.Time
		mutate func(*survival.MeasureOutput)
		want   string
	}{
		{"a selected day with no result", []time.Time{dayN(0), dayN(1), dayN(5)}, func(*survival.MeasureOutput) {}, "no result"},
		{"a result of a day nobody selected", []time.Time{dayN(0)}, func(*survival.MeasureOutput) {}, "not selected"},
		{"a result that is neither measured nor blocked", days, func(out *survival.MeasureOutput) {
			out.Days[1].State = survival.DayMarked
		}, "neither measured nor blocked"},
		{"a change of a blocked day", days, func(out *survival.MeasureOutput) {
			out.Days[1] = survival.DayResult{Day: dayN(1), State: survival.DayBlocked, BlockedSHA: shaOf(50), BlockedFiles: 1}
		}, "holds no measurement"},
		{"a pair whose adder is no change of the result", days, func(out *survival.MeasureOutput) {
			out.Pairs[0].AdderSHA = shaOf(51)
		}, "no change of the result"},
		{"a selected day with no fingerprint", days, func(out *survival.MeasureOutput) {
			out.Fingerprints = out.Fingerprints[:1]
		}, "no fingerprint"},
		{"a day that is no UTC midnight", []time.Time{dayN(0).Add(time.Hour), dayN(1)}, func(*survival.MeasureOutput) {}, "UTC midnight"},
		{"no day at all", nil, func(*survival.MeasureOutput) {}, "no day"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rerun := survival.MeasureOutput{
				Changes:      slices.Clone(out.Changes),
				Pairs:        slices.Clone(out.Pairs),
				Fingerprints: slices.Clone(out.Fingerprints),
				Days:         slices.Clone(out.Days),
			}
			tc.mutate(&rerun)
			tx := begin(t, database)
			err := ReplaceDays(t.Context(), tx, widgets, tc.days, rerun, time.Now(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReplaceDays = %v, want an error saying %q", err, tc.want)
			}
			_ = tx.Rollback(t.Context())
			if after := snapshot(t, database, resultTables...); !slices.Equal(after, before) {
				t.Fatalf("a refused replace changed the store:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// R8: a mark is written in a transaction of its own and outlives the run that fails after it. The
// mark is committed first; a run that then replaces the marked days' rows and rolls back leaves the
// store as the mark left it, and the days still read marked. MarkDays marks measured days alone,
// keeps the time of a mark already made, and writes nothing outside code_days.
func TestAMarkOutlivesARunThatFails(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	days, out := firstRun()
	replace(t, database, days, out)
	outsideDays := []string{"code_repos", "code_changes", "code_pairs", "code_projects"}
	beforeMark := snapshot(t, database, outsideDays...)
	markedAt := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

	tx := begin(t, database)
	// Day 2 holds a fingerprint alone and day 40 holds no row: neither is marked.
	marked, err := MarkDays(t.Context(), tx, widgets, []time.Time{dayN(0), dayN(2), dayN(40)}, markedAt)
	if err != nil {
		t.Fatalf("MarkDays: %v", err)
	}
	if !reflect.DeepEqual(marked, []time.Time{dayN(0)}) {
		t.Fatalf("MarkDays marked %v, want day 0 alone: a mark goes on a measured day", marked)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the marks: %v", err)
	}
	if afterMark := snapshot(t, database, outsideDays...); !slices.Equal(afterMark, beforeMark) {
		t.Fatalf("MarkDays wrote outside code_days:\nbefore %v\nafter  %v", beforeMark, afterMark)
	}
	withMark := snapshot(t, database, resultTables...)

	// The run that follows replaces the marked day's rows, then fails.
	failing := begin(t, database)
	if err := ReplaceDays(t.Context(), failing, widgets, days, out, time.Now(), nil); err != nil {
		t.Fatalf("ReplaceDays: %v", err)
	}
	if err := failing.Rollback(t.Context()); err != nil {
		t.Fatalf("roll back the run: %v", err)
	}
	if after := snapshot(t, database, resultTables...); !slices.Equal(after, withMark) {
		t.Fatalf("a run that rolled back changed what the mark left:\nbefore %v\nafter  %v", withMark, after)
	}
	statuses, err := DayStatuses(t.Context(), database.Pool, widgets, dayN(0), dayN(1))
	if err != nil {
		t.Fatalf("DayStatuses: %v", err)
	}
	if len(statuses) != 2 || statuses[0].State != survival.DayMarked || statuses[1].State != survival.DayMeasured {
		t.Fatalf("the states after the failed run are %+v, want day 0 marked and day 1 measured", statuses)
	}
	var stored time.Time
	if err := database.Pool.QueryRow(t.Context(), `select marked_at from code_days where repo = $1 and day = $2`, widgets, dayN(0)).Scan(&stored); err != nil || !stored.Equal(markedAt) {
		t.Fatalf("the mark's time = %v (%v), want %v", stored, err, markedAt)
	}

	// A second mark of the marked day marks nothing and keeps the first time.
	again := begin(t, database)
	marked, err = MarkDays(t.Context(), again, widgets, []time.Time{dayN(0), dayN(1)}, markedAt.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(marked, []time.Time{dayN(1)}) {
		t.Fatalf("MarkDays of a marked and a measured day = %v (%v), want day 1 alone", marked, err)
	}
	if err := again.Commit(t.Context()); err != nil {
		t.Fatalf("commit the second marks: %v", err)
	}
	if err := database.Pool.QueryRow(t.Context(), `select marked_at from code_days where repo = $1 and day = $2`, widgets, dayN(0)).Scan(&stored); err != nil || !stored.Equal(markedAt) {
		t.Fatalf("the first mark's time after a second mark = %v (%v), want %v", stored, err, markedAt)
	}
}

// R8 and R9: the replace that measures a day clears its mark, and the SHA that blocked a day goes
// with the row that measures it: a day holds a blocking commit only while it is blocked, and a
// marked day holds no result of the run before.
func TestAReplaceClearsTheMarkAndTheBlockOfTheDaysItMeasures(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	days, out := firstRun()
	blocked := out
	blocked.Days = []survival.DayResult{measured(0), {Day: dayN(1), State: survival.DayBlocked, BlockedSHA: shaOf(77), BlockedFiles: 3}}
	blocked.Changes, blocked.Pairs = out.Changes[:2], out.Pairs[:1]
	replaceWith(t, database, days, blocked, map[string]string{shaOf(77): "the diff route answered 406; files[] withheld 3 of 12 files"})

	statuses, err := DayStatuses(t.Context(), database.Pool, widgets, dayN(0), dayN(1))
	if err != nil {
		t.Fatalf("DayStatuses: %v", err)
	}
	if len(statuses) != 2 || statuses[0].State != survival.DayMeasured || statuses[1].State != survival.DayBlocked ||
		statuses[0].TriedAt.IsZero() || statuses[1].TriedAt.IsZero() {
		t.Fatalf("the states after a run that blocked day 1 are %+v, want day 0 measured and day 1 blocked, each tried", statuses)
	}
	var (
		sha, reason string
		files       int
	)
	if err := database.Pool.QueryRow(t.Context(), `select blocked_sha, blocked_files, blocked_reason from code_days where repo = $1 and day = $2`, widgets, dayN(1)).
		Scan(&sha, &files, &reason); err != nil || sha != shaOf(77) || files != 3 || reason != "the diff route answered 406; files[] withheld 3 of 12 files" {
		t.Fatalf("the blocked day holds %q, %d files and the reason %q (%v), want the commit, its 3 files and the run's reason", sha, files, reason, err)
	}

	tx := begin(t, database)
	if _, err := MarkDays(t.Context(), tx, widgets, days, time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("MarkDays: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit the marks: %v", err)
	}
	var marked int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from code_days where state = 'marked'`).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("marked days = %d (%v), want day 0 alone: the blocked day is no measured day", marked, err)
	}

	replace(t, database, days, out)
	rows, err := database.Pool.Query(t.Context(), `
		select state, measured_at is not null, marked_at is null, blocked_sha is null, blocked_files is null, blocked_reason is null
		from code_days where repo = $1 and day < $2 order by day`, widgets, dayN(2))
	if err != nil {
		t.Fatalf("read the days: %v", err)
	}
	type held struct {
		State                                             string
		Measured, NoMark, NoBlockedSHA, NoFiles, NoReason bool
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[held])
	if err != nil {
		t.Fatalf("read the days: %v", err)
	}
	if want := (held{string(survival.DayMeasured), true, true, true, true, true}); len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("the days after the run that measured both = %+v, want each measured with no mark and no blocking commit", got)
	}
}

// The reads answer what the survival package's own types hold: the fingerprints from a day on, the
// states of a range of days (with the time a day was tried), and the changes and pairs of a set of
// days, as the run and the regroup will take them.
func TestTheReadsAnswerWhatReplaceWrote(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	days, out := firstRun()
	replace(t, database, days, out)
	db := database.Pool
	ctx := t.Context()

	prints, err := FingerprintsFrom(ctx, db, widgets, dayN(1))
	if err != nil || !reflect.DeepEqual(prints, out.Fingerprints[1:]) {
		t.Errorf("FingerprintsFrom day 1 = %+v (%v), want %+v", prints, err, out.Fingerprints[1:])
	}
	if prints, err := FingerprintsFrom(ctx, db, "acme/other", dayN(0)); err != nil || len(prints) != 0 {
		t.Errorf("FingerprintsFrom of another repository = %+v (%v), want none", prints, err)
	}

	statuses, err := DayStatuses(ctx, db, widgets, dayN(1), dayN(2))
	if err != nil || len(statuses) != 2 {
		t.Fatalf("DayStatuses days 1 to 2 = %+v (%v), want two days", statuses, err)
	}
	if statuses[0].Day != dayN(1) || statuses[0].State != survival.DayMeasured || statuses[0].TriedAt.IsZero() ||
		statuses[1].Day != dayN(2) || statuses[1].State != survival.DayReplayed || !statuses[1].TriedAt.IsZero() {
		t.Errorf("DayStatuses days 1 to 2 = %+v, want day 1 measured and tried, day 2 replayed and never tried", statuses)
	}

	changes, err := Changes(ctx, db, widgets, []time.Time{dayN(1), dayN(0)})
	if err != nil || !reflect.DeepEqual(changes, out.Changes) {
		t.Errorf("Changes of days 1 and 0 = %+v (%v), want %+v in the order they were committed", changes, err, out.Changes)
	}
	one, err := Changes(ctx, db, widgets, []time.Time{dayN(1)})
	if err != nil || !reflect.DeepEqual(one, out.Changes[2:]) {
		t.Errorf("Changes of day 1 = %+v (%v), want %+v", one, err, out.Changes[2:])
	}
	if none, err := Changes(ctx, db, widgets, nil); err != nil || len(none) != 0 {
		t.Errorf("Changes of no day = %+v (%v), want none", none, err)
	}
	pairs, err := Pairs(ctx, db, widgets, []time.Time{dayN(0), dayN(1)})
	if err != nil || !reflect.DeepEqual(pairs, out.Pairs) {
		t.Errorf("Pairs of days 0 and 1 = %+v (%v), want %+v", pairs, err, out.Pairs)
	}
	later, err := Pairs(ctx, db, widgets, []time.Time{dayN(1)})
	if err != nil || !reflect.DeepEqual(later, out.Pairs[1:]) {
		t.Errorf("Pairs of day 1 = %+v (%v), want %+v", later, err, out.Pairs[1:])
	}
}

// A repository is one row, written whole: a readable one with its branch, its start day and the
// days it was checked and measured, then an unreadable one with its reason and the time it is tried
// again. Unreadable without a reason is refused, and a repository nobody wrote is not found.
func TestARepositoryIsOneRowWithItsStateAndReason(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	db := database.Pool
	ctx := t.Context()
	if _, found, err := GetRepo(ctx, db, widgets); err != nil || found {
		t.Fatalf("GetRepo of an unwritten repository = found %t (%v), want none", found, err)
	}

	readable := Repo{Repo: widgets, Branch: "main", StartDay: dayN(0), State: RepoReadable, CheckedOn: dayN(9), LastMeasuredOn: dayN(8)}
	if err := PutRepo(ctx, db, readable); err != nil {
		t.Fatalf("PutRepo: %v", err)
	}
	if got, found, err := GetRepo(ctx, db, widgets); err != nil || !found || !reflect.DeepEqual(got, readable) {
		t.Fatalf("GetRepo = %+v (found %t, %v), want %+v", got, found, err, readable)
	}

	unreadable := Repo{Repo: widgets, State: RepoUnreadable, Reason: "the App cannot read acme/widgets", CheckedOn: dayN(10),
		RetryAt: time.Date(2026, 9, 11, 4, 5, 6, 0, time.UTC)}
	if err := PutRepo(ctx, db, unreadable); err != nil {
		t.Fatalf("PutRepo of the unreadable repository: %v", err)
	}
	if got, found, err := GetRepo(ctx, db, widgets); err != nil || !found || !reflect.DeepEqual(got, unreadable) {
		t.Fatalf("GetRepo = %+v (found %t, %v), want %+v, which replaces the row whole", got, found, err, unreadable)
	}
	var rows int
	if err := db.QueryRow(ctx, `select count(*) from code_repos`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("code_repos holds %d rows (%v), want one for the repository", rows, err)
	}

	err := PutRepo(ctx, db, Repo{Repo: widgets, State: RepoUnreadable})
	requireRefusal(t, err, "23514", "code_repos_reason_when_unreadable")
}

// A project's groups digest is read, locked and stored: a project that was never built reads as
// not built; the lock holds the row, so a second builder waits for it (and here gives up at its
// lock_timeout) until the first commits; and what the first stored is what the second then reads
// under its own lock.
func TestAProjectsGroupsDigestIsReadLockedAndStored(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	db := database.Pool
	ctx := t.Context()
	if _, err := db.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ALPHA', 'Alpha', 'alice')`); err != nil {
		t.Fatalf("insert the project: %v", err)
	}
	if project, err := ProjectDigest(ctx, db, "ALPHA"); err != nil || project.Built() || project.Key != "ALPHA" {
		t.Fatalf("ProjectDigest of a project never built = %+v (%v), want not built", project, err)
	}

	first := begin(t, database)
	if project, err := LockProject(ctx, first, "ALPHA"); err != nil || project.Built() {
		t.Fatalf("LockProject of a project never built = %+v (%v), want not built", project, err)
	}
	second := begin(t, database)
	if _, err := second.Exec(ctx, `set local lock_timeout = '200ms'`); err != nil {
		t.Fatalf("set the second builder's lock_timeout: %v", err)
	}
	if _, err := LockProject(ctx, second, "ALPHA"); err == nil {
		t.Fatal("a second builder locked the project while the first held it")
	} else {
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "55P03" {
			t.Fatalf("the second builder's error = %v, want SQLSTATE 55P03 (lock_not_available)", err)
		}
	}
	_ = second.Rollback(ctx)

	digest := sha256.Sum256([]byte("groups"))
	builtAt := time.Date(2026, 11, 26, 5, 6, 7, 0, time.UTC)
	if err := StoreProject(ctx, first, "ALPHA", digest, builtAt); err != nil {
		t.Fatalf("StoreProject: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit the first builder: %v", err)
	}

	third := begin(t, database)
	got, err := LockProject(ctx, third, "ALPHA")
	if err != nil || !got.Built() || got.GroupsDigest != digest || !got.BuiltAt.Equal(builtAt) {
		t.Fatalf("LockProject after the first builder = %+v (%v), want the digest %x built at %s", got, err, digest, builtAt)
	}
	_ = third.Rollback(ctx)
	if read, err := ProjectDigest(ctx, db, "ALPHA"); err != nil || read != got {
		t.Fatalf("ProjectDigest = %+v (%v), want %+v", read, err, got)
	}

	if _, err := LockProject(ctx, begin(t, database), "NOSUCH"); err == nil {
		t.Fatal("LockProject of a project that does not exist answered no error")
	}
}

// The tables refuse each row that breaks a rule of the store, naming the rule: a repository in
// upper case or unreadable without a reason; a SHA that is not 40 lower-case hex; a change whose day
// is not its commit's UTC day or whose lines outrun what it added; a pair with no lines or of no
// stored change; a digest of the wrong length; a day whose columns do not fit its state; a project
// that does not exist.
func TestTheResultsTablesRefuseARowThatBreaksTheirRules(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	const (
		project = `insert into projects (key, name, default_assignee) values ('ALPHA', 'Alpha', 'alice')`
		change  = `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('a', 40), '2026-09-01', '2026-09-01T10:00:00Z', 1, 'b', 'alice', 10, 5, 2)`
		day = `insert into code_days (repo, day, commits, digest, state, %s) values ('acme/w', '2026-09-01', 1, decode(repeat('ab', 32), 'hex'), %s)`
	)
	cases := []struct {
		name       string
		setup      string
		statement  string
		code       string
		constraint string
	}{
		{"a repository in upper case", "", `insert into code_repos (repo, state) values ('Acme/Widgets', 'readable')`, "23514", "code_repos_repo_check"},
		{"a state that is neither", "", `insert into code_repos (repo, state) values ('acme/w', 'gone')`, "23514", "code_repos_state_check"},
		{"an unreadable repository with no reason", "", `insert into code_repos (repo, state) values ('acme/w', 'unreadable')`, "23514", "code_repos_reason_when_unreadable"},
		{"a readable repository with a reason", "", `insert into code_repos (repo, state, state_reason) values ('acme/w', 'readable', 'why')`, "23514", "code_repos_reason_when_unreadable"},
		{"a SHA of 39 characters", "", `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('a', 39), '2026-09-01', '2026-09-01T10:00:00Z', 1, 'b', 'alice', 10, 5, 2)`, "23514", "code_changes_sha_check"},
		{"a SHA in upper case", "", `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('A', 40), '2026-09-01', '2026-09-01T10:00:00Z', 1, 'b', 'alice', 10, 5, 2)`, "23514", "code_changes_sha_check"},
		{"a day that is not its commit's UTC day", "", `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('a', 40), '2026-09-02', '2026-09-01T23:00:00Z', 1, 'b', 'alice', 10, 5, 2)`, "23514", "code_changes_day_is_the_commit_day"},
		{"lines that outrun the lines added", "", `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('a', 40), '2026-09-01', '2026-09-01T10:00:00Z', 1, 'b', 'alice', 10, 8, 3)`, "23514", "code_changes_lines_fit_in_added"},
		{"a negative count of added lines", "", `insert into code_changes (repo, sha, day, committed_at, pr_number, head_ref, author, added, alive, removed_by_non_pr)
			values ('acme/w', repeat('a', 40), '2026-09-01', '2026-09-01T10:00:00Z', 1, 'b', 'alice', -1, 0, 0)`, "23514", "code_changes_added_check"},
		{"a pair with no lines", change, `insert into code_pairs (repo, adder_sha, remover_sha, remover_pr, remover_head_ref, remover_author, lines, days_between)
			values ('acme/w', repeat('a', 40), repeat('b', 40), 2, 'c', 'bob', 0, 1)`, "23514", "code_pairs_lines_check"},
		{"a pair of no stored change", "", `insert into code_pairs (repo, adder_sha, remover_sha, remover_pr, remover_head_ref, remover_author, lines, days_between)
			values ('acme/w', repeat('a', 40), repeat('b', 40), 2, 'c', 'bob', 1, 1)`, "23503", "code_pairs_repo_adder_sha_fkey"},
		{"a digest of 31 bytes", "", `insert into code_days (repo, day, commits, digest, state) values ('acme/w', '2026-09-01', 1, decode(repeat('ab', 31), 'hex'), 'replayed')`, "23514", "code_days_digest_check"},
		{"a state of a day that is none of the four", "", `insert into code_days (repo, day, commits, digest, state) values ('acme/w', '2026-09-01', 1, decode(repeat('ab', 32), 'hex'), 'waiting')`, "23514", "code_days_state_check"},
		{"a replayed day with a measured time", "", fmt.Sprintf(day, "measured_at", `'replayed', now()`), "23514", "code_days_replayed_holds_a_fingerprint_only"},
		{"a measured day with no measured time", "", fmt.Sprintf(day, "tried_at", `'measured', now()`), "23514", "code_days_measured_columns"},
		{"a measured day that names a blocking commit", "", fmt.Sprintf(day, "measured_at, tried_at, blocked_sha", `'measured', now(), now(), repeat('a', 40)`), "23514", "code_days_measured_columns"},
		{"a marked day with no mark", "", fmt.Sprintf(day, "measured_at, tried_at", `'marked', now(), now()`), "23514", "code_days_marked_columns"},
		{"a blocked day with no file count", "", fmt.Sprintf(day, "blocked_sha, tried_at", `'blocked', repeat('a', 40), now()`), "23514", "code_days_blocked_columns"},
		{"a blocked day with a measured time", "", fmt.Sprintf(day, "blocked_sha, blocked_files, tried_at, measured_at", `'blocked', repeat('a', 40), 2, now(), now()`), "23514", "code_days_blocked_columns"},
		{"a blocked day with no commit", "", fmt.Sprintf(day, "blocked_files, tried_at", `'blocked', 2, now()`), "23514", "code_days_blocked_columns"},
		{"a project digest of 16 bytes", project, `insert into code_projects (project_key, groups_digest, built_at) values ('ALPHA', decode(repeat('ab', 16), 'hex'), now())`, "23514", "code_projects_groups_digest_check"},
		{"a project digest with no time", project, `insert into code_projects (project_key, groups_digest) values ('ALPHA', decode(repeat('ab', 32), 'hex'))`, "23514", "code_projects_digest_and_time_together"},
		{"a row of no project", "", `insert into code_projects (project_key) values ('NOSUCH')`, "23503", "code_projects_project_key_fkey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := begin(t, database)
			if tc.setup != "" {
				if _, err := tx.Exec(t.Context(), tc.setup); err != nil {
					t.Fatalf("set up: %v", err)
				}
			}
			_, err := tx.Exec(t.Context(), tc.statement)
			requireRefusal(t, err, tc.code, tc.constraint)
		})
	}

	// The valid forms of the same rows are accepted, so each refusal above is of its one rule.
	tx := begin(t, database)
	for _, statement := range []string{
		project,
		change,
		`insert into code_repos (repo, state, state_reason) values ('acme/w', 'unreadable', 'why')`,
		`insert into code_pairs (repo, adder_sha, remover_sha, remover_pr, remover_head_ref, remover_author, lines, days_between)
			values ('acme/w', repeat('a', 40), repeat('b', 40), 2, 'c', 'bob', 1, -0.5)`,
		fmt.Sprintf(day, "measured_at, tried_at", `'measured', now(), now()`),
		`insert into code_projects (project_key) values ('ALPHA')`,
	} {
		if _, err := tx.Exec(t.Context(), statement); err != nil {
			t.Fatalf("a valid row %q was refused: %v", statement, err)
		}
	}
}
