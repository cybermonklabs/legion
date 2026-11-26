package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
		`insert into projects (key, name, default_assignee) values ($1, 'Evals', 'alice')`, evalProject); err != nil {
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

// drainPending moves the change rows the eval and Legion tables' triggers recorded in the change
// log's outbox into delivery_changes (delivery_changes_drain, migration 20261117100001), a batch
// of 250 at a time until a batch comes back short, as delivery.DrainPending does, and answers how
// many it moved.
func drainPending(t *testing.T, store *Store) int64 {
	t.Helper()
	const batch = 250
	var total int64
	for {
		var moved int64
		if err := store.Pool.QueryRow(context.Background(), `select delivery_changes_drain($1)`, batch).Scan(&moved); err != nil {
			t.Fatalf("drain the pending changes: %v", err)
		}
		total += moved
		if moved < batch {
			return total
		}
	}
}

// changeLogHead is the newest change the log holds once every pending change is drained, which a
// check reads before its own write so its assertions see only the rows that write logged.
func changeLogHead(t *testing.T, store *Store) int64 {
	t.Helper()
	drainPending(t, store)
	var seq int64
	if err := store.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from delivery_changes`).Scan(&seq); err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	return seq
}

// changesAfter is every change logged after seq, oldest first, once every pending change is
// drained.
func changesAfter(t *testing.T, store *Store, seq int64) []loggedChange {
	t.Helper()
	drainPending(t, store)
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
// measured_at; a statement writing readings of two evals logs one change for each; a transaction
// of two such statements, as the readings write sends a batch past the statement bound, logs one
// change of each eval still, spanning every reading of it the two wrote; and the write of
// superseded_at on a replaced reading logs none.
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

	head = changeLogHead(t, store)
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction of two statements: %v", err)
	}
	for _, batch := range []struct {
		evals    []string
		measured []time.Time
	}{{[]string{evalID, other}, []time.Time{evalAt(9, 9), evalAt(9, 10)}}, {[]string{evalID, evalID}, []time.Time{evalAt(10, 9), evalAt(8, 1)}}} {
		if _, err := tx.Exec(ctx, `
			insert into eval_readings (eval_id, series, row_kind, row_key, value, measured_at, recorded_by)
			select eval, 'minutes', 'pull_request', 'acme/widgets#tx' || ordinality || '-' || extract(epoch from measured), ordinality * 10, measured, $3::jsonb
			from unnest($1::text[], $2::timestamptz[]) with ordinality as given(eval, measured, ordinality)
		`, batch.evals, batch.measured, evalRecorder); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("write a statement of the transaction: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the transaction of two statements: %v", err)
	}
	changes = changesAfter(t, store, head)
	t.Logf("a transaction of two statements over 2 evals logged %d change rows: %+v", len(changes), changes)
	spans := map[string][2]time.Time{evalID: {evalAt(8, 1), evalAt(10, 9)}, other: {evalAt(9, 10), evalAt(9, 10)}}
	if len(changes) != 2 {
		t.Fatalf("a transaction of two statements writing readings of two evals logged %d changes; want one of each", len(changes))
	}
	for _, change := range changes {
		want, held := spans[change.ref]
		if !held || change.tOld == nil || !change.tOld.Equal(want[0]) || change.tNew == nil || !change.tNew.Equal(want[1]) {
			t.Fatalf("the change of %s spans %v to %v; want every reading of it the two statements wrote, %s to %s", change.ref, change.tOld, change.tNew, want[0], want[1])
		}
	}
}

// TestTheEvalTriggersRecordTheirChangesPendingUntilADrainMovesThem: a transaction writing readings,
// a version and a day row of an eval and an issue's stored Legion facts holds no change log lock
// (6900) when it commits, and its four changes wait in the outbox, delivery_changes untouched,
// until the drain moves them, each keeping the moment it was recorded; a drain with nothing
// pending moves nothing.
func TestTheEvalTriggersRecordTheirChangesPendingUntilADrainMovesThem(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)
	head := changeLogHead(t, store)
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`insert into eval_readings (eval_id, series, row_kind, row_key, value, measured_at, recorded_by)
		 values ('` + evalID + `', 'minutes', 'pull_request', 'acme/widgets#7', 3, '2026-11-04T09:00:00Z', '` + evalRecorder + `'::jsonb)`,
		`insert into eval_series (eval_id, series, version, unit, role, is_primary, listed, agg)
		 values ('` + evalID + `', 'minutes', 1, 'minutes', 'headline', true, true, 'quantile')`,
		`insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		 values ('` + evalProject + `', '` + evalID + `', 'minutes', '', '2026-11-04', true, 1, 3, 'build-a')`,
		`insert into issues (key, project_key, number, title, status, rank, created_by)
		 values ('EVAL-1', 'EVAL', 1, 'Facts', 'done', 'U', '{"kind":"user","id":"alice"}')`,
		`insert into issue_facts (project_key, issue_key) values ('EVAL', 'EVAL-1')`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var holds bool
	var pendingInTx int
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from pg_locks where locktype = 'advisory' and classid = 6900 and objid = 0 and objsubid = 2
		               and pid = pg_backend_pid()),
		       (select count(*) from delivery_changes_pending)
	`).Scan(&holds, &pendingInTx); err != nil {
		t.Fatal(err)
	}
	if holds {
		t.Error("the transaction holds the change log's lock after its eval and facts writes, want none")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var pending []loggedChange
	var recordedAt []time.Time
	rows, err := store.Pool.Query(ctx, `select kind, coalesce(repo, ''), coalesce(ref, ''), t_old, t_new, at from delivery_changes_pending order by id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var change loggedChange
		var at time.Time
		if err := rows.Scan(&change.kind, &change.repo, &change.ref, &change.tOld, &change.tNew, &at); err != nil {
			t.Fatal(err)
		}
		pending, recordedAt = append(pending, change), append(recordedAt, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, change := range pending {
		kinds = append(kinds, change.kind)
	}
	if !slices.Equal(kinds, []string{"eval_readings", "eval_versions", "eval_days", "issue_facts"}) || pendingInTx != 4 {
		t.Fatalf("the transaction recorded %v pending (%d seen inside it), want one eval_readings, eval_versions, eval_days and issue_facts change", kinds, pendingInTx)
	}
	var logged int64
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes where seq > $1`, head).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != 0 {
		t.Errorf("the transaction wrote %d rows into delivery_changes itself, want none before a drain", logged)
	}
	if moved := drainPending(t, store); moved != 4 {
		t.Fatalf("the drain moved %d changes, want the 4 pending", moved)
	}
	movedRows, err := store.Pool.Query(ctx, `select kind, at from delivery_changes where seq > $1 order by seq`, head)
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	for movedRows.Next() {
		var kind string
		var at time.Time
		if err := movedRows.Scan(&kind, &at); err != nil {
			t.Fatal(err)
		}
		if index >= len(pending) || kind != pending[index].kind || !at.Equal(recordedAt[index]) {
			t.Errorf("moved change %d is %s at %s, want %v in id order, each at the moment it was recorded", index, kind, at, pending)
		}
		index++
	}
	if err := movedRows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != len(pending) {
		t.Errorf("the log holds %d moved changes, want %d", index, len(pending))
	}
	if again := drainPending(t, store); again != 0 {
		t.Errorf("a drain with nothing pending moved %d", again)
	}
	t.Logf("pending %v moved in id order, each keeping its recorded moment", pending)
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

// TestAWriteOfRepoProjectsLogsNoChange: repo_projects is a read-only legacy table (no route writes
// it, and dispatch://INFRA-4411 drops it), and no rule of the eval day rows reads it, so no trigger
// logs a change of it: a row made, saved again by another person, made a default and deleted
// leaves the change log and its outbox as they stood.
func TestAWriteOfRepoProjectsLogsNoChange(t *testing.T) {
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
	}{
		{"a row made by alice", save, []any{evalProject, `{"kind":"user","id":"alice"}`, false}},
		{"the same row saved by bob", save, []any{evalProject, `{"kind":"user","id":"bob"}`, false}},
		{"the row made the project's default", save, []any{evalProject, `{"kind":"user","id":"bob"}`, true}},
		{"the row deleted", `delete from repo_projects where repo = 'acme/web'`, nil},
	} {
		head := changeLogHead(t, store)
		if _, err := store.Pool.Exec(ctx, write.statement, write.args...); err != nil {
			t.Fatalf("%s: %v", write.what, err)
		}
		var pending int
		if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes_pending`).Scan(&pending); err != nil {
			t.Fatalf("count the outbox's rows: %v", err)
		}
		if changes := changesAfter(t, store, head); len(changes) != 0 || pending != 0 {
			t.Errorf("%s logged %v and left %d rows in the outbox, want no change", write.what, changes, pending)
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
			on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do update
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
		on conflict (project_key, eval_id, writer, deploy_repo, series, repo, day) do update
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

// TestAnUpdateThatMovesADayRowsDayLogsBothDays: eval_days_updated matches the old and the new rows
// of an update by the primary key, so an update that moves a row's day matches it on neither side,
// and the one change it logs spans the row's old day and its new one.
func TestAnUpdateThatMovesADayRowsDayLogsBothDays(t *testing.T) {
	ctx := context.Background()
	store := migratedEvalStore(t)
	if _, err := store.Pool.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, built_by)
		values ($1, $2, 'minutes', '', '2026-11-03', true, 1, 10, 'build-a')
	`, evalProject, evalID); err != nil {
		t.Fatalf("store the day row of 2026-11-03: %v", err)
	}
	head := changeLogHead(t, store)
	moved, err := store.Pool.Exec(ctx, `update eval_days set day = '2026-11-06' where project_key = $1 and eval_id = $2 and day = '2026-11-03'`,
		evalProject, evalID)
	if err != nil {
		t.Fatalf("move the day row to 2026-11-06: %v", err)
	}
	if moved.RowsAffected() != 1 {
		t.Fatalf("the move rewrote %d rows; want the one stored row", moved.RowsAffected())
	}
	changes := changesAfter(t, store, head)
	t.Logf("a move of the row's day logged %d change rows: %v", len(changes), changes)
	if len(changes) != 1 || changes[0].kind != "eval_days" || changes[0].repo != evalProject || changes[0].ref != evalID {
		t.Fatalf("a move of the row's day logged %v; want one eval_days change of %s", changes, evalID)
	}
	if days := changeDays(changes[0]); len(days) == 0 || days[0] != "2026-11-03" || days[len(days)-1] != "2026-11-06" {
		t.Fatalf("the change's span covers days %v; want 2026-11-03, the old day, through 2026-11-06, the new one", days)
	}
}

// TestTheTablesARewriteUpdatesKeepTheirSize: eval_days, eval_range_figures and eval_day_figures
// are created with fillfactor 80 (migrations 20261117100006 and 20261120072042). A rewrite after
// a change of the delivery settings updates each row it sends whose values moved (the upserts'
// `is distinct from` leaves the rest) and moves no column an index holds, so the fifth of each
// page left free keeps each update on its page (HOT), writing no index entry; at the default of
// 100 almost none is, and each table grows with every pass until a vacuum. eval_days is also
// vacuumed after every 1,000 inserts, with its index pass, since a rewrite deletes and inserts
// rows of it too and an insert reuses a deleted row's space only after a vacuum records it: at
// autovacuum's defaults the table grew with every pass and was never vacuumed.
func TestTheTablesARewriteUpdatesKeepTheirSize(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	want := map[string][]string{
		"eval_days":          {"fillfactor=80", "autovacuum_vacuum_insert_threshold=1000", "autovacuum_vacuum_insert_scale_factor=0", "vacuum_index_cleanup=on"},
		"eval_range_figures": {"fillfactor=80"},
		"eval_day_figures":   {"fillfactor=80"},
	}
	for table, options := range want {
		var held []string
		if err := store.Pool.QueryRow(ctx,
			`select coalesce(reloptions, '{}') from pg_class where oid = $1::regclass`, table).Scan(&held); err != nil {
			t.Fatalf("read %s's options: %v", table, err)
		}
		for _, option := range options {
			if !slices.Contains(held, option) {
				t.Errorf("%s's options are %v; want %s among them", table, held, option)
			}
		}
	}
}

// TestEveryColumnOfADayRowButItsBookkeepingIsComparedByItsUpdateTrigger: eval_days_updated logs a
// change of every row whose content an update moved by comparing the old row with the new one
// column by column (migration 20261117100014), so a column eval_days gains and the comparison
// leaves out would move with no change logged, and an evals view would never wake to it. Every
// column the catalog lists for eval_days but updated_at and built_by, which a pass rewrites
// whatever it finds, is compared, the old row's against the new row's at the same place, and
// neither of those two is.
func TestEveryColumnOfADayRowButItsBookkeepingIsComparedByItsUpdateTrigger(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	rows, err := store.Pool.Query(ctx, `
		select column_name::text from information_schema.columns
		where table_schema = current_schema() and table_name = 'eval_days'
		order by ordinal_position
	`)
	if err != nil {
		t.Fatalf("read the columns of eval_days: %v", err)
	}
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the columns of eval_days: %v", err)
	}
	ignored := []string{"updated_at", "built_by"}
	want := slices.DeleteFunc(slices.Clone(columns), func(column string) bool { return slices.Contains(ignored, column) })
	if len(want) != len(columns)-len(ignored) {
		t.Fatalf("eval_days has columns %v; want updated_at and built_by among them", columns)
	}
	var definition string
	if err := store.Pool.QueryRow(ctx, `select pg_get_functiondef('eval_days_updated'::regproc)`).Scan(&definition); err != nil {
		t.Fatalf("read eval_days_updated: %v", err)
	}
	comparisons := rowComparison.FindAllStringSubmatch(definition, -1)
	if len(comparisons) != 1 {
		t.Fatalf("eval_days_updated holds %d comparisons of a list of the old row's columns with one of the new row's, "+
			"`(old_row.c, …) is distinct from (new_row.c, …)`; want one:\n%s", len(comparisons), definition)
	}
	oldColumns, oldOthers := comparedColumns(comparisons[0][1], "old_row")
	newColumns, newOthers := comparedColumns(comparisons[0][2], "new_row")
	t.Logf("eval_days has %d columns; eval_days_updated compares the old row's %v with the new row's %v", len(columns), oldColumns, newColumns)
	if len(oldOthers) > 0 || len(newOthers) > 0 {
		t.Errorf("eval_days_updated compares %v beside the old row's columns and %v beside the new row's; want each side the row's columns alone",
			oldOthers, newOthers)
	}
	if !slices.Equal(oldColumns, newColumns) {
		t.Errorf("eval_days_updated compares the old row's %v with the new row's %v; want the same column at each place", oldColumns, newColumns)
	}
	missing := slices.DeleteFunc(slices.Clone(want), func(column string) bool { return slices.Contains(oldColumns, column) })
	extra := slices.DeleteFunc(slices.Clone(oldColumns), func(column string) bool { return slices.Contains(want, column) })
	if len(missing) > 0 || len(extra) > 0 || len(oldColumns) != len(want) {
		t.Errorf("eval_days_updated compares the old row's %v; want every column of eval_days but %v once, %v, which leaves out %v and holds %v besides",
			oldColumns, ignored, want, missing, extra)
	}
}

// rowComparison matches one comparison of two lists of columns, `(…) is distinct from (…)`, each
// list held with no parentheses inside it, its two lists the submatches.
var rowComparison = regexp.MustCompile(`(?i)\(([^()]*)\)\s*is\s+distinct\s+from\s*\(([^()]*)\)`)

// columnName matches a column's name alone.
var columnName = regexp.MustCompile(`^\w+$`)

// comparedColumns is the columns of row, a transition table's name, that list (one side of
// rowComparison) names, in its order, and every item of it that names none of them.
func comparedColumns(list, row string) (columns, others []string) {
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if column, found := strings.CutPrefix(item, row+"."); found && columnName.MatchString(column) {
			columns = append(columns, column)
		} else {
			others = append(others, item)
		}
	}
	return columns, others
}
