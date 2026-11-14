package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	store := migratedEmptyStore(t)
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
	if code, constraint = sqlState(err); code == "" {
		t.Fatalf("%s was stored (err %v); want it refused", what, err)
	}
	return code, constraint
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
// change, naming the project and the eval, with no span, and so does one version of one of its
// series, which changes how every window of the eval reads as much.
func TestANewVersionLogsOneEvalVersionsChange(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)

	for _, write := range []struct {
		what, statement string
		args            []any
	}{
		{"a version of the definition", `
			insert into eval_versions (eval_id, version, project_key, slug, name, kind, source, written_via, written_by)
			values ($1, 1, $2, 'merge-to-production', 'Merge to production', 'delivery',
			        'computed:delivery/measures.Compute', 'seed', $3::jsonb)`, []any{evalID, evalProject, evalRecorder}},
		{"a version of a series", `
			insert into eval_series (eval_id, series, version, unit, role, is_primary, listed, agg)
			values ($1, 'minutes', 1, 'minutes', 'headline', true, true, 'quantile')`, []any{evalID}},
	} {
		head := changeLogHead(t, store)
		if _, err := store.Pool.Exec(ctx, write.statement, write.args...); err != nil {
			t.Fatalf("write %s: %v", write.what, err)
		}
		changes := changesAfter(t, store, head)
		t.Logf("%s of %s logged %d change rows: %+v", write.what, evalID, len(changes), changes)
		if len(changes) != 1 {
			t.Fatalf("%s logged %d changes; want one", write.what, len(changes))
		}
		logged := changes[0]
		if logged.kind != "eval_versions" || logged.repo != evalProject || logged.ref != evalID || logged.tOld != nil || logged.tNew != nil {
			t.Fatalf("the change of %s = %+v; want kind eval_versions, repo %s, ref %s and no span", write.what, logged, evalProject, evalID)
		}
	}
}

// TestAMappingSavedAgainByAnotherPersonLogsNoChange: a repository's mapping in repo_projects logs a
// settings change naming the repository when it is made, when a column the mapping decides moves
// and when it is deleted, and none when the settings route's upsert saves it again unchanged as
// another person, which rewrites created_by alone.
func TestAMappingSavedAgainByAnotherPersonLogsNoChange(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)
	const save = `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ('acme/web', $1, $2::jsonb, $3, null)
		on conflict (repo) do update set project = excluded.project, created_by = excluded.created_by,
			is_default = excluded.is_default, renamed_to = excluded.renamed_to`
	for _, write := range []struct {
		what      string
		statement string
		args      []any
		changes   int
	}{
		{"the mapping made by alice", save, []any{evalProject, `{"kind":"user","id":"alice"}`, false}, 1},
		{"the same mapping saved by bob", save, []any{evalProject, `{"kind":"user","id":"bob"}`, false}, 0},
		{"the mapping made the project's default", save, []any{evalProject, `{"kind":"user","id":"bob"}`, true}, 1},
		{"the mapping deleted", `delete from repo_projects where repo = 'acme/web'`, nil, 1},
	} {
		head := changeLogHead(t, store)
		if _, err := store.Pool.Exec(ctx, write.statement, write.args...); err != nil {
			t.Fatalf("%s: %v", write.what, err)
		}
		changes := changesAfter(t, store, head)
		t.Logf("%s logged %v", write.what, changes)
		if len(changes) != write.changes {
			t.Errorf("%s logged %d changes, want %d", write.what, len(changes), write.changes)
		}
		for _, change := range changes {
			if change.kind != "settings" || change.repo != "acme/web" {
				t.Errorf("%s logged %v, want a settings change naming acme/web", write.what, change)
			}
		}
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
