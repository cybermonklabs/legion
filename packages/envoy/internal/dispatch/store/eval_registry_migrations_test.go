package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// evalProject is the project every eval below belongs to, and evalID the eval whose rows the
// checks write: an eval id is `<project_key>/<slug>`.
const (
	evalProject = "EVAL"
	evalID      = evalProject + "/merge-to-production"
)

// evalRecorder is the actor every reading below is recorded by.
const evalRecorder = `{"kind":"session","id":"ses_eval_writer"}`

// migratedEvalStore is a store carrying every migration, with evalProject in it.
func migratedEvalStore(t *testing.T) *Store {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(context.Background(),
		`insert into projects (key, name) values ($1, 'Evals')`, evalProject); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	return store
}

// loggedChange is one delivery_changes row as a check reads it.
type loggedChange struct {
	kind, repo, ref string
	tOld, tNew      *time.Time
}

// String names the change as a test's log prints it, each end of its span as an RFC 3339 moment
// in UTC, or null.
func (c loggedChange) String() string {
	at := func(moment *time.Time) string {
		if moment == nil {
			return "null"
		}
		return moment.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("{kind %s, repo %s, ref %s, t_old %s, t_new %s}", c.kind, c.repo, c.ref, at(c.tOld), at(c.tNew))
}

// changeLogHead is the newest change the log holds, which a check reads before its own write so
// its assertions see only the rows that write logged.
func changeLogHead(t *testing.T, store *Store) int64 {
	t.Helper()
	var seq int64
	if err := store.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from delivery_changes`).Scan(&seq); err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	return seq
}

// changesAfter is every change logged after seq, oldest first.
func changesAfter(t *testing.T, store *Store, seq int64) []loggedChange {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `
		select kind, coalesce(repo, ''), coalesce(ref, ''), t_old, t_new
		from delivery_changes where seq > $1 order by seq
	`, seq)
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	changes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (loggedChange, error) {
		var change loggedChange
		err := row.Scan(&change.kind, &change.repo, &change.ref, &change.tOld, &change.tNew)
		return change, err
	})
	if err != nil {
		t.Fatalf("read the changes after %d: %v", seq, err)
	}
	return changes
}

// changeDays is the UTC days a change's span covers, both ends counted: the days a reader of
// per-day rows rewrites for that change.
func changeDays(change loggedChange) []string {
	if change.tOld == nil || change.tNew == nil {
		return nil
	}
	days := []string{}
	for day := change.tOld.UTC().Truncate(24 * time.Hour); !day.After(change.tNew.UTC()); day = day.AddDate(0, 0, 1) {
		days = append(days, day.Format(time.DateOnly))
	}
	return days
}

// evalAt is an hour of a UTC day of November 2026, the month these checks state their times in.
func evalAt(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC)
}

// refusal is the SQLSTATE and the constraint of the Postgres error err carries, failing the test
// when err is no such error: what a check that wants a write refused reads of the refusal.
func refusal(t *testing.T, err error, what string) (code, constraint string) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		t.Fatalf("%s was stored (err %v); want it refused", what, err)
	}
	return pgError.Code, pgError.ConstraintName
}

// insertReading stores one reading of evalID's `minutes` series at measured, with value (nil for
// none), the reason it has none, and the reading it replaces ("" for none), and answers its id.
func insertReading(ctx context.Context, store *Store, value *float64, reason string, measured time.Time, replaces string) (string, error) {
	var id string
	err := store.Pool.QueryRow(ctx, `
		insert into eval_readings (eval_id, series, row_kind, row_key, value, value_absent_reason,
		                           measured_at, recorded_by, replaces)
		values ($1, 'minutes', 'pull_request', 'acme/widgets#7', $2, $3, $4, $5::jsonb, nullif($6, '')::uuid)
		returning id::text
	`, evalID, value, reason, measured, evalRecorder, replaces).Scan(&id)
	return id, err
}

// TestAReadingCarriesAValueOrItsReasonAndIsCorrectedOnce: the readings table refuses a reading
// with neither a value nor the reason it has none, takes a correction naming a stored reading,
// and refuses a second correction of that same reading, which only the unique index on replaces
// can refuse when two writers correct one reading at once.
func TestAReadingCarriesAValueOrItsReasonAndIsCorrectedOnce(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)

	_, err := insertReading(ctx, store, nil, "", evalAt(2, 9), "")
	code, constraint := refusal(t, err, "a reading with neither a value nor a reason")
	t.Logf("a reading with neither a value nor a reason: refused, SQLSTATE %s, constraint %s", code, constraint)
	if code != "23514" || constraint != "eval_readings_value_or_reason" {
		t.Fatalf("refused with SQLSTATE %s on %s; want 23514 (check_violation) on eval_readings_value_or_reason", code, constraint)
	}
	if _, err := insertReading(ctx, store, nil, "no deploy ran that day", evalAt(2, 9), ""); err != nil {
		t.Fatalf("a reading with no value and the reason it has none was refused: %v", err)
	}

	measured := 41.0
	original, err := insertReading(ctx, store, &measured, "", evalAt(3, 9), "")
	if err != nil {
		t.Fatalf("store the reading to correct: %v", err)
	}
	corrected := 38.5
	if _, err := insertReading(ctx, store, &corrected, "", evalAt(3, 9), original); err != nil {
		t.Fatalf("a correction naming reading %s was refused: %v", original, err)
	}
	t.Logf("a correction naming reading %s: stored", original)

	again := 39.0
	_, err = insertReading(ctx, store, &again, "", evalAt(3, 9), original)
	code, constraint = refusal(t, err, "a second correction of one reading")
	t.Logf("a second correction of reading %s: refused, SQLSTATE %s, constraint %s", original, code, constraint)
	if code != "23505" || constraint != "eval_readings_replaces" {
		t.Fatalf("refused with SQLSTATE %s on %s; want 23505 (unique_violation) on eval_readings_replaces", code, constraint)
	}

	_, err = insertReading(ctx, store, &again, "", evalAt(3, 9), "00000000-0000-4000-8000-000000000000")
	code, constraint = refusal(t, err, "a correction naming no stored reading")
	t.Logf("a correction naming no stored reading: refused, SQLSTATE %s, constraint %s", code, constraint)
	if code != "23503" || constraint != "eval_readings_replaces_fkey" {
		t.Fatalf("refused with SQLSTATE %s on %s; want 23503 (foreign_key_violation) on eval_readings_replaces_fkey", code, constraint)
	}

	var stored int
	if err := store.Pool.QueryRow(ctx, `select count(*) from eval_readings`).Scan(&stored); err != nil {
		t.Fatalf("count the stored readings: %v", err)
	}
	if stored != 3 {
		t.Fatalf("%d readings are stored; want 3: the reading with a reason, the original and its one correction", stored)
	}
}

// TestAWriteOfReadingsLogsOneChangePerEval: one statement writing a batch of three readings of one
// eval logs one eval_readings change, naming the project and the eval and spanning the readings'
// measured_at; a statement writing readings of two evals logs one change for each; and the write
// of superseded_at on a replaced reading logs none.
func TestAWriteOfReadingsLogsOneChangePerEval(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)
	writeBatch := func(evals []string, measured []time.Time) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into eval_readings (eval_id, series, row_kind, row_key, value, measured_at, recorded_by)
			select eval, 'minutes', 'pull_request', 'acme/widgets#' || ordinality, ordinality * 10, measured, $3::jsonb
			from unnest($1::text[], $2::timestamptz[]) with ordinality as given(eval, measured, ordinality)
		`, evals, measured, evalRecorder); err != nil {
			t.Fatalf("write the batch: %v", err)
		}
	}

	head := changeLogHead(t, store)
	writeBatch([]string{evalID, evalID, evalID}, []time.Time{evalAt(4, 9), evalAt(2, 14), evalAt(6, 23)})
	changes := changesAfter(t, store, head)
	t.Logf("a batch of 3 readings of %s logged %d change rows: %+v", evalID, len(changes), changes)
	if len(changes) != 1 {
		t.Fatalf("a batch of three readings logged %d changes; want one", len(changes))
	}
	logged := changes[0]
	if logged.kind != "eval_readings" || logged.repo != evalProject || logged.ref != evalID {
		t.Fatalf("the change = kind %s, repo %s, ref %s; want kind eval_readings, repo %s, ref %s",
			logged.kind, logged.repo, logged.ref, evalProject, evalID)
	}
	if logged.tOld == nil || !logged.tOld.Equal(evalAt(2, 14)) || logged.tNew == nil || !logged.tNew.Equal(evalAt(6, 23)) {
		t.Fatalf("the change spans %v to %v; want the batch's first and last measured_at, %s to %s",
			logged.tOld, logged.tNew, evalAt(2, 14), evalAt(6, 23))
	}

	other := evalProject + "/pr-open-to-merge"
	head = changeLogHead(t, store)
	writeBatch([]string{evalID, other, other}, []time.Time{evalAt(7, 9), evalAt(7, 10), evalAt(8, 10)})
	changes = changesAfter(t, store, head)
	t.Logf("a statement of 3 readings over 2 evals logged %d change rows: %+v", len(changes), changes)
	refs := []string{}
	for _, change := range changes {
		refs = append(refs, change.ref)
	}
	if !slices.Equal(refs, []string{evalID, other}) {
		t.Fatalf("a statement writing readings of two evals logged changes for %v; want one for each, %v", refs, []string{evalID, other})
	}

	head = changeLogHead(t, store)
	superseded, err := store.Pool.Exec(ctx, `update eval_readings set superseded_at = now() where eval_id = $1`, other)
	if err != nil {
		t.Fatalf("mark the readings superseded: %v", err)
	}
	changes = changesAfter(t, store, head)
	t.Logf("marking %d readings superseded logged %d change rows", superseded.RowsAffected(), len(changes))
	if superseded.RowsAffected() != 2 || len(changes) != 0 {
		t.Fatalf("marking %d readings superseded logged %d changes; want two readings marked and no change", superseded.RowsAffected(), len(changes))
	}
}

// TestANewVersionLogsOneEvalVersionsChange: one version of a definition logs one eval_versions
// change, naming the project and the eval, with no span.
func TestANewVersionLogsOneEvalVersionsChange(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)

	head := changeLogHead(t, store)
	if _, err := store.Pool.Exec(ctx, `
		insert into eval_versions (eval_id, version, project_key, slug, name, kind, source, written_via, written_by)
		values ($1, 1, $2, 'merge-to-production', 'Merge to production', 'delivery',
		        'computed:delivery/measures.Compute', 'seed', $3::jsonb)
	`, evalID, evalProject, evalRecorder); err != nil {
		t.Fatalf("write the version: %v", err)
	}
	changes := changesAfter(t, store, head)
	t.Logf("one version of %s logged %d change rows: %+v", evalID, len(changes), changes)
	if len(changes) != 1 {
		t.Fatalf("one version logged %d changes; want one", len(changes))
	}
	logged := changes[0]
	if logged.kind != "eval_versions" || logged.repo != evalProject || logged.ref != evalID || logged.tOld != nil || logged.tNew != nil {
		t.Fatalf("the change = %+v; want kind eval_versions, repo %s, ref %s and no span", logged, evalProject, evalID)
	}
}

// TestAnUpsertOfDayRowsLogsOneChangeSpanningItsDays: an upsert of four new days of one eval's day
// rows logs one eval_days change spanning those four days. An upsert that rewrites two stored days
// and adds two more, which Postgres runs as an update half and an insert half each with its own
// statement trigger, still logs one change, spanning all four. A rebuild by another build that
// leaves every value as it was logs none, and a write that moves one day's last_value and last_at
// alone, as the `last` fold's newest value moves, logs one change naming that day.
func TestAnUpsertOfDayRowsLogsOneChangeSpanningItsDays(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)
	upsert := func(build string, days []string, values []float64) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `
			insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
			select $1, $2, 'minutes', '', day::date, true, 1, value, $3
			from unnest($4::text[], $5::double precision[]) as given(day, value)
			on conflict (project_key, eval_id, series, repo, day) do update
			set listed = excluded.listed, n = excluded.n, sum = excluded.sum,
			    built_by = excluded.built_by, updated_at = now()
			where (eval_days.listed, eval_days.n, eval_days.sum, eval_days.built_by)
			      is distinct from (excluded.listed, excluded.n, excluded.sum, excluded.built_by)
		`, evalProject, evalID, build, days, values); err != nil {
			t.Fatalf("upsert the day rows of %v: %v", days, err)
		}
	}
	oneChangeOver := func(what string, changes []loggedChange, want []string) {
		t.Helper()
		t.Logf("%s logged %d change rows: %+v, covering days %v", what, len(changes), changes, func() [][]string {
			covered := [][]string{}
			for _, change := range changes {
				covered = append(covered, changeDays(change))
			}
			return covered
		}())
		if len(changes) != 1 {
			t.Fatalf("%s logged %d changes; want one", what, len(changes))
		}
		logged := changes[0]
		if logged.kind != "eval_days" || logged.repo != evalProject || logged.ref != evalID {
			t.Fatalf("the change = kind %s, repo %s, ref %s; want kind eval_days, repo %s, ref %s",
				logged.kind, logged.repo, logged.ref, evalProject, evalID)
		}
		if days := changeDays(logged); !slices.Equal(days, want) {
			t.Fatalf("the change's span covers days %v; want %v", days, want)
		}
	}

	head := changeLogHead(t, store)
	upsert("build-a", []string{"2026-11-02", "2026-11-03", "2026-11-04", "2026-11-05"}, []float64{10, 20, 30, 40})
	oneChangeOver("an upsert of four new days", changesAfter(t, store, head),
		[]string{"2026-11-02", "2026-11-03", "2026-11-04", "2026-11-05"})

	head = changeLogHead(t, store)
	upsert("build-a", []string{"2026-11-04", "2026-11-05", "2026-11-06", "2026-11-07"}, []float64{31, 41, 50, 60})
	oneChangeOver("an upsert rewriting two stored days and adding two", changesAfter(t, store, head),
		[]string{"2026-11-04", "2026-11-05", "2026-11-06", "2026-11-07"})

	head = changeLogHead(t, store)
	upsert("build-b", []string{"2026-11-02", "2026-11-03", "2026-11-04", "2026-11-05", "2026-11-06", "2026-11-07"},
		[]float64{10, 20, 31, 41, 50, 60})
	var rebuilt int
	if err := store.Pool.QueryRow(ctx, `select count(*) from eval_days where built_by = 'build-b'`).Scan(&rebuilt); err != nil {
		t.Fatalf("count the rebuilt rows: %v", err)
	}
	changes := changesAfter(t, store, head)
	t.Logf("a rebuild of %d rows to the values they held logged %d change rows", rebuilt, len(changes))
	if rebuilt != 6 || len(changes) != 0 {
		t.Fatalf("a rebuild of %d rows to the values they held logged %d changes; want six rows rebuilt and no change", rebuilt, len(changes))
	}

	head = changeLogHead(t, store)
	moved, err := store.Pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, last_value, last_at, built_by)
		values ($1, $2, 'minutes', '', '2026-11-06', true, 1, 50, 7, $3, 'build-b')
		on conflict (project_key, eval_id, series, repo, day) do update
		set last_value = excluded.last_value, last_at = excluded.last_at, updated_at = now()
		where (eval_days.last_value, eval_days.last_at) is distinct from (excluded.last_value, excluded.last_at)
	`, evalProject, evalID, evalAt(6, 18))
	if err != nil {
		t.Fatalf("move the newest value of 2026-11-06: %v", err)
	}
	if moved.RowsAffected() != 1 {
		t.Fatalf("the write of a new last_value rewrote %d rows; want the one stored day", moved.RowsAffected())
	}
	oneChangeOver("a write moving one day's last_value and last_at alone", changesAfter(t, store, head),
		[]string{"2026-11-06"})
}

// explainedPlan is statement's plan with args, as `EXPLAIN (FORMAT JSON)` answers it, and as
// text for the test's log.
func explainedPlan(t *testing.T, store *Store, statement string, args ...any) (plan []byte, text string) {
	t.Helper()
	ctx := context.Background()
	if err := store.Pool.QueryRow(ctx, "explain (format json) "+statement, args...).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	rows, err := store.Pool.Query(ctx, "explain "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	return plan, strings.Join(lines, "\n")
}

// requireIndexRead holds plan to reading relation through index and never by a sequential scan.
func requireIndexRead(t *testing.T, plan []byte, text, relation, index string) {
	t.Helper()
	tree := explaintest.OnePlan(t, plan)
	if explaintest.SeqScansRelation(t, tree, relation) {
		t.Fatalf("the plan scans %s sequentially; want a read through %s:\n%s", relation, index, text)
	}
	read := []string{}
	for _, scan := range explaintest.IndexScans(t, tree) {
		read = append(read, scan.Index)
	}
	if !slices.Contains(read, index) {
		t.Fatalf("the plan reads through %v; want %s:\n%s", read, index, text)
	}
}

// TestTheUnownedP0CountAndTheClosedFactsReadTheirIndexes: on an analyzed store holding enough rows
// that the planner prices a scan of the whole table above an index read (6,000 issues, 20,000
// stored facts), the count of a project's P0 issues with no owner and the live hub's read of every
// open P0 issue's key (delivery.OpenP0Keys) read issues_open_p0, the read of a project's issues
// closed in a window reads issue_facts_closed, and none scans its table.
func TestTheUnownedP0CountAndTheClosedFactsReadTheirIndexes(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)

	// Every fiftieth issue is P0 with no owner; the three after it are P0 and done, claimed or
	// routed, which the rule leaves out; every other issue is P2.
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name) values ('PZERO', 'Zero'), ('PONE', 'One')`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, priority, status,
		                    claimed_by, claimed_at, route)
		select project || '-' || n, project, n, 'Issue ' || n, '{"kind":"user","id":"alice"}',
		       lpad(n::text, 8, '0'),
		       case when n % 50 < 4 then 0 else 2 end,
		       case when n % 50 = 1 then 'done' else 'todo' end,
		       case when n % 50 = 2 then '{"kind":"session","id":"ses_claimer"}'::jsonb end,
		       case when n % 50 = 2 then now() end,
		       case when n % 50 = 3 then 'session:ses_router' end
		from unnest(array['PZERO', 'PONE']) as project, generate_series(1, 3000) as n
	`); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	const p0Count = `select count(*) from issues where project_key = $1 and priority = 0 and status <> 'done' and claimed_by is null and route is null`
	var unowned int
	if err := store.Pool.QueryRow(ctx, p0Count, "PZERO").Scan(&unowned); err != nil {
		t.Fatalf("count the unowned P0 issues: %v", err)
	}
	if unowned != 60 {
		t.Fatalf("the count answers %d unowned P0 issues of PZERO; want 60, one in fifty of its 3,000", unowned)
	}

	// Five projects of 4,000 stored facts each, nine in ten closed, one a day back over a year.
	now := evalAt(13, 0)
	if _, err := store.Pool.Exec(ctx, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at)
		select project, project || '-' || n, $1::timestamptz - make_interval(days => n % 365 + 10),
		       case when n % 10 <> 0 then $1::timestamptz - make_interval(days => n % 365) end
		from unnest(array['FA', 'FB', 'FC', 'FD', 'FE']) as project, generate_series(1, 4000) as n
		order by n % 365 desc, project
	`, now); err != nil {
		t.Fatalf("seed the facts: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `analyze issues, issue_facts`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan, text := explainedPlan(t, store, p0Count, "PZERO")
	t.Logf("the unowned P0 count of one project plans as:\n%s", text)
	requireIndexRead(t, plan, text, "issues", "issues_open_p0")
	const openP0Keys = `select key from issues where priority = 0 and status <> 'done'`
	plan, text = explainedPlan(t, store, openP0Keys)
	t.Logf("every open P0 issue's key plans as:\n%s", text)
	requireIndexRead(t, plan, text, "issues", "issues_open_p0")

	const closedWindow = `select issue_key, closed_at, work_hours from issue_facts where project_key = $1 and closed_at >= $2 and closed_at < $3`
	from, to := now.AddDate(0, 0, -28), now
	var closed int
	if err := store.Pool.QueryRow(ctx, `select count(*) from (`+closedWindow+`) window_read`, "FC", from, to).Scan(&closed); err != nil {
		t.Fatalf("read the closed window: %v", err)
	}
	plan, text = explainedPlan(t, store, closedWindow, "FC", from, to)
	t.Logf("the read of one project's %d issues closed in 28 days plans as:\n%s", closed, text)
	requireIndexRead(t, plan, text, "issue_facts", "issue_facts_closed")
}
