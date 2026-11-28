package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// lookIDVersion is the version of the migration this file tests, so the test can migrate through
// everything before it whatever lands beside it.
const lookIDVersion = 20261127171352

// lookIDRelease names the release whose way back the migration's header carries, as a failure to
// read that block names it.
const lookIDRelease = "the release whose look report gives its look id"

// 20261127171352 adds the look id of a check's current look with the look and the check instance
// it was made for, the generation a check opened in, and the look id of a pending delivery, all
// null on every row it finds, which the stores read as no look id, look 0, the zero time and
// generation 0. The release before's upsert of a later look leaves the look id's columns as this
// release wrote, and the record reads such an id as no id of the current look. Its way back,
// the last paragraph of the migration's header (migrations.WayBack), run for LEGION (LEGION in
// place of rollingBackProject), takes lookId and lookReissue off each of LEGION's supervise rows
// and deletes each of its retire_look rows, which the release before refuses
// (DisallowUnknownFields, an unknown op), and leaves every other key and every row of another kind
// as it stands. It clears opened_generation on LEGION's checks, which the release before's upsert
// of a new check would leave in place, and keeps the look id of every check, here a done root's
// open check, whose look the roll forward's boot gives a new look id where it needs one. It drops
// no column and deletes no version row, so the roll forward applies nothing. The negative control
// is the store before the migration, whose tables hold none of the five columns.
func TestTheLookIDColumnsArriveEmptyAndTheWayBackStripsWhatTheReleaseBeforeRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, lookIDVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	opened := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'done', 'done', 3, 'done', 'A', 1)`)
	// The release before's upsert names these columns and no other (record's PutProductionCheck).
	const releaseBeforesUpsert = `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at,
		role, looks, look_open, look_wake_at, last_wake_kind, last_wake_detail, last_wake_at, wake_at, last, closed_at, outcome, failed_looks)
		values ('LEGION-208', 'LEGION', 'LEGION-208', 'acme/widgets', 42, 'merge', $1::timestamptz, $1::timestamptz + interval '14 days',
		'tester', $2, true, null, 'merge', '', null, null, false, null, null, 0)
		on conflict (issue) do update set looks = excluded.looks, look_open = excluded.look_open`
	exec(releaseBeforesUpsert, opened, 2)
	exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-legion-legion-208-tester', 'LEGION', 'LEGION-208', 'LEGION-208', 'tester', 3,
		'session', '/session.jsonl', 'idle', 0, 0, 0, 0)`)
	// The release before's delivery names no look id (the claim store's putDelivery).
	exec(`insert into pending_task_deliveries (claim_token, delivery_id, task, phase, generation, check_look, check_opened_at, queued_at)
		values ('legion-legion-legion-208-tester', 'outbox:7', 'Production check of LEGION-208: look 2.', '', 3, 2, $1, now())`, opened)
	const columns = `select table_name || '.' || column_name from information_schema.columns
		where (table_name = 'production_checks' and column_name in ('look_id', 'look_id_look', 'look_id_opened_at', 'opened_generation'))
		or (table_name = 'pending_task_deliveries' and column_name = 'look_id') order by 1`
	if got := rowsAsText(t, store, columns); len(got) != 0 {
		t.Fatalf("the columns before the migration = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the look id: %v", err)
	}
	all := []string{"pending_task_deliveries.look_id", "production_checks.look_id", "production_checks.look_id_look",
		"production_checks.look_id_opened_at", "production_checks.opened_generation"}
	if got := rowsAsText(t, store, columns); !reflect.DeepEqual(got, all) {
		t.Fatalf("the columns after the migration = %v, want %v", got, all)
	}
	empty := append(rowsAsText(t, store, `select row(look_id is null, look_id_look is null, look_id_opened_at is null, opened_generation is null)::text
		from production_checks`), rowsAsText(t, store, `select row(look_id is null)::text from pending_task_deliveries`)...)
	if want := []string{"(t,t,t,t)", "(t)"}; !reflect.DeepEqual(empty, want) {
		t.Fatalf("the rows the migration found (the check's four columns null, the delivery's look id null) = %v, want %v", empty, want)
	}
	check := func() record.ProductionCheck {
		t.Helper()
		var got *record.ProductionCheck
		if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			got, err = record.NewStore().ProductionCheck(ctx, tx, "LEGION-208")
			return err
		}); err != nil || got == nil {
			t.Fatalf("read LEGION-208's check: %+v, %v", got, err)
		}
		return *got
	}
	if got := check(); got.LookID != "" || got.LookIDLook != 0 || !got.LookIDOpenedAt.IsZero() || got.OpenedGeneration != 0 || got.CurrentLookID() != "" {
		t.Fatalf("the check the migration found reads %+v, want no look id, look 0, no opening time and generation 0", got)
	}
	if got := onlyClaim(t, store).Pending; got == nil || got.ID != "outbox:7" || got.CheckLook.Look != 2 || got.LookID != "" {
		t.Fatalf("the delivery the migration found reads %+v, want outbox:7 of look 2 with no look id", got)
	}

	// This release gives the open look 2 its id and the check its generation; the release before's
	// upsert of look 3 then leaves both, and the id names no look of the check any more.
	current := check()
	current.LookID, current.LookIDLook, current.LookIDOpenedAt, current.OpenedGeneration = "LOOKIDOFLOOKTWOOFTHECHECK", 2, opened, 3
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(ctx, tx, current)
	}); err != nil {
		t.Fatalf("give look 2 its look id: %v", err)
	}
	if got := check().CurrentLookID(); got != "LOOKIDOFLOOKTWOOFTHECHECK" {
		t.Fatalf("the current look id after this release's write = %q, want look 2's", got)
	}
	exec(releaseBeforesUpsert, opened, 3)
	if got := check(); got.LookID != "LOOKIDOFLOOKTWOOFTHECHECK" || got.OpenedGeneration != 3 || got.CurrentLookID() != "" {
		t.Fatalf("the check after the release before's upsert of look 3 = %+v, want look 2's id and the generation kept, and no current look id", got)
	}
	exec(`update pending_task_deliveries set look_id = 'LOOKIDOFLOOKTWOOFTHECHECK'`)

	queue := func(kind, payload string) {
		t.Helper()
		exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', $2::jsonb, 0, now(), '')`, kind, payload)
	}
	queue("supervise", `{"op":"start","tree":"LEGION-208","role":"tester","generation":3,"task":"look 2","checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","lookId":"LOOKIDOFLOOKTWOOFTHECHECK","lookReissue":true}`)
	queue("supervise", `{"op":"start","tree":"LEGION-208","role":"tester","generation":3,"task":"look 3","checkLook":3,"checkOpenedAt":"2026-11-27T09:00:00Z","lookId":"LOOKIDOFLOOKTHREEOFTHECHECK"}`)
	queue("supervise", `{"op":"start","tree":"LEGION-208","role":"tester","generation":3,"task":"look 1","checkLook":1}`)
	queue("supervise", `{"op":"retire_look","tree":"LEGION-208","role":"tester","generation":3,"checkLook":2,"checkOpenedAt":"2026-11-27T09:00:00Z","reported":true}`)
	queue("supervise", `{"op":"retire_task","tree":"LEGION-208","role":"tester","generation":3,"phase":"testing"}`)
	queue("notice", `{"kind":"phase-finished","lookId":"kept"}`)

	exec(strings.ReplaceAll(wayBackOf(t, lookIDVersion, lookIDRelease), rollingBackProject, "LEGION"))
	const queued = `select kind || ' ' || payload::text from outbox order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`supervise {"op": "start", "role": "tester", "task": "look 2", "tree": "LEGION-208", "checkLook": 2, "generation": 3, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`supervise {"op": "start", "role": "tester", "task": "look 3", "tree": "LEGION-208", "checkLook": 3, "generation": 3, "checkOpenedAt": "2026-11-27T09:00:00Z"}`,
		`supervise {"op": "start", "role": "tester", "task": "look 1", "tree": "LEGION-208", "checkLook": 1, "generation": 3}`,
		`supervise {"op": "retire_task", "role": "tester", "tree": "LEGION-208", "phase": "testing", "generation": 3}`,
		`notice {"kind": "phase-finished", "lookId": "kept"}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the queued rows after the way back = %q, want %q: lookId and lookReissue off each supervise row, every retire_look row gone, every other key and row kept", got, want)
	}
	kept := append(rowsAsText(t, store, `select row(look_id, look_id_look, look_id_opened_at = '2026-11-27T09:00:00Z', opened_generation)::text
		from production_checks`), rowsAsText(t, store, `select row(look_id)::text from pending_task_deliveries`)...)
	kept = append(kept, rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261127171352`)...)
	if want := []string{"(LOOKIDOFLOOKTWOOFTHECHECK,2,t,)", "(LOOKIDOFLOOKTWOOFTHECHECK)", "1"}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("after the way back the check, the delivery and the version row = %v, want %v: the open check's look id kept with its look and its check instance, its generation cleared, the delivery's look id and the version row kept", kept, want)
	}

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if applied != 0 {
		t.Fatalf("the roll forward after the way back applied %d migrations, want none: the five columns stand", applied)
	}
	if got := rowsAsText(t, store, columns); !reflect.DeepEqual(got, all) {
		t.Fatalf("the columns after the roll forward = %v, want %v", got, all)
	}
}
