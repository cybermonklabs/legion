package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// goingRunTablesVersion is the migration that gives a command-bar run its conversation, state and
// lease and the tables of its steps, events, grant, landings and plans. The two migrations after
// it give server_processes its going_run_shape and user_going_settings its mode.
const goingRunTablesVersion = 20261119201453

// goingRunGrantColumns is every column a run's grant holds, in order: who acts and by what, and
// no column that could hold a cookie, an assertion, a token or a hash of one.
var goingRunGrantColumns = []string{"run_id", "kind", "login", "generation", "expires_at", "token_id", "granted_at"}

// goingRunSeed is a waiting run of alice's with a step, an event, a sign-in grant, a landing, a
// plan of two rows, and a queued follow-up that continues it, with a grant and a step of its own:
// :run and :follow_up are the two runs' ids.
const goingRunSeed = `
	insert into going_runs (id, author_login, words, conversation_id, level, mode, state, shape)
	values (':run', 'alice', 'tidy DEMO-1', ':run', 'standard', 'ask', 'waiting', 1);
	insert into going_runs (id, author_login, words, conversation_id, continues, level, mode, state, shape)
	values (':follow_up', 'alice', 'and close it', ':run', ':run', 'fast', 'auto_review', 'queued', 1);
	insert into going_run_steps (run_id, step, kind, body, note)
	values (':run', 0, 'model', '{}', 'asked the model'), (':follow_up', 0, 'model', '{}', 'asked again');
	insert into going_run_events (run_id, sequence, name, data) values (':run', 1, 'text', '{"delta": "hi"}');
	insert into going_run_grants (run_id, kind, login, generation, expires_at)
	values (':run', 'sign_in', 'alice', 3, now() + interval '30 days'), (':follow_up', 'header', 'alice', null, null);
	insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
	values (':run', 'do', 1, 0, 0, 'PATCH', '/api/v1/issues/DEMO-1', 'ab', '{"status": 200}');
	insert into going_plans (run_id, plan, state) values (':run', 0, 'waiting');
	insert into going_plan_rows (run_id, plan, row, change, needs, state)
	values (':run', 0, 0, '{"tool": "dispatch_issue_update"}', '{}', 'waiting'),
	       (':run', 0, 1, '{"tool": "dispatch_issue_update"}', '{0}', 'waiting');`

// TestTheRunTablesHoldTheirRules: the migrations apply over a database of the schema before them
// that holds a run record, a setting and a registered process, each of which reads null or its
// default in every new column, and the inserts written before them still land; a run's steps,
// events, grant, landings, plans and plan rows, and the run that follows it, are deleted with it;
// a second landing with the same key is refused; a value outside its column's check is refused;
// and a grant holds no column that could keep a secret.
func TestTheRunTablesHoldTheirRules(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, goingRunTablesVersion-1)
	exec := func(t *testing.T, what, statement string, arguments ...any) {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, statement, arguments...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const stored = "00000000-0000-4000-8000-0000000000a1"
	exec(t, "record a run as the schema before stores it", `
		insert into going_runs (id, author_login, words, context) values ($1::uuid, 'alice', 'close DEMO-1', '{"path": "/issues/DEMO-1"}')`, stored)
	exec(t, "store a setting as the schema before stores it", `
		insert into user_going_settings (login, confirm_writes, updated_at) values ('alice', true, now())`)
	exec(t, "register a process as the schema before registers it", `
		insert into server_processes (instance, signing_keys) values ('00000000-0000-4000-8000-0000000000b1', '{fingerprint}')`)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate over the schema before the run tables: %v", err)
	}

	// seed stores goingRunSeed under the two ids.
	seed := func(t *testing.T, run, followUp string) {
		t.Helper()
		exec(t, "seed a run, its follow-up and their rows", strings.NewReplacer(":follow_up", followUp, ":run", run).Replace(goingRunSeed))
	}
	refused := func(t *testing.T, what, code, constraint, statement string) {
		t.Helper()
		_, err := store.Pool.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
			t.Errorf("%s: %v, want %s to refuse it (SQLSTATE %s)", what, err, constraint, code)
		}
	}

	t.Run("what the schema before stored and inserts reads null or its default", func(t *testing.T) {
		const inserted = "00000000-0000-4000-8000-0000000000a2"
		exec(t, "record a run as the bar's pause records it", `
			insert into going_runs (id, author_login, words, context) values ($1::uuid, 'alice', 'what moved?', null)`, inserted)
		exec(t, "store a setting that names no mode", `
			insert into user_going_settings (login, updated_at) values ('bob', now())
			on conflict (login) do update set updated_at = excluded.updated_at`)
		for _, id := range []string{stored, inserted} {
			var blank bool
			var budget string
			var kills int
			if err := store.Pool.QueryRow(ctx, `
				select conversation_id is null and continues is null and level is null and mode is null and state is null
				  and stop_reason is null and shape is null and lease_token is null and lease_owner is null and lease_ends_at is null,
				  budget_used::text, kills
				from going_runs where id = $1::uuid`, id).Scan(&blank, &budget, &kills); err != nil {
				t.Fatalf("read run %s: %v", id, err)
			}
			if !blank || budget != `{"calls": 0, "tokens": 0, "working_ms": 0}` || kills != 0 {
				t.Errorf("run %s: every new column null %v, budget_used %s, kills %d; want null, nothing spent and 0, a row no lease takes", id, blank, budget, kills)
			}
		}
		rows, err := store.Pool.Query(ctx, `select login || '=' || mode from user_going_settings order by login`)
		if err != nil {
			t.Fatalf("read the settings: %v", err)
		}
		if modes, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(modes, []string{"alice=ask", "bob=ask"}) {
			t.Errorf("settings modes = %v (%v), want ask for the stored row and the new one", modes, err)
		}
		var shapeless bool
		if err := store.Pool.QueryRow(ctx, `select going_run_shape is null from server_processes`).Scan(&shapeless); err != nil || !shapeless {
			t.Errorf("the registered process's going_run_shape is null: %v (%v), want true", shapeless, err)
		}
	})

	t.Run("a run's rows and the run that follows it are deleted with it", func(t *testing.T) {
		const run, followUp = "00000000-0000-4000-8000-0000000000c1", "00000000-0000-4000-8000-0000000000c2"
		seed(t, run, followUp)
		exec(t, "delete the run", `delete from going_runs where id = $1::uuid`, run)
		for _, table := range []string{"going_runs", "going_run_steps", "going_run_events", "going_run_grants", "going_landings", "going_plans", "going_plan_rows"} {
			column := "run_id"
			if table == "going_runs" {
				column = "id"
			}
			var left int
			if err := store.Pool.QueryRow(ctx, `select count(*) from `+table+` where `+column+` in ($1::uuid, $2::uuid)`, run, followUp).Scan(&left); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			if left != 0 {
				t.Errorf("%s holds %d rows of the deleted run and its follow-up, want none", table, left)
			}
		}
	})

	t.Run("a second landing with the same key is refused", func(t *testing.T) {
		const run = "00000000-0000-4000-8000-0000000000d1"
		seed(t, run, "00000000-0000-4000-8000-0000000000d2")
		refused(t, "a second landing of run, do, step 1, call 0, request 0", "23505", "going_landings_pkey", `
			insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
			values ('`+run+`', 'do', 1, 0, 0, 'PATCH', '/api/v1/issues/DEMO-1', 'cd', '{"status": 200}')`)
		exec(t, "land the call's next request, and the undo of the same call", `
			insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer)
			values ($1::uuid, 'do', 1, 0, 1, 'POST', '/api/v1/issues/DEMO-1/close', 'ef', '{"status": 200}'),
			       ($1::uuid, 'undo', 1, 0, 0, 'PATCH', '/api/v1/issues/DEMO-1', 'ab', '{"status": 200}')`, run)
	})

	t.Run("a value outside its column's check is refused", func(t *testing.T) {
		const run = "00000000-0000-4000-8000-0000000000e1"
		seed(t, run, "00000000-0000-4000-8000-0000000000e2")
		of := "where run_id = '" + run + "'"
		for _, refusal := range []struct{ what, code, constraint, statement string }{
			{"a run's state paused", "23514", "going_runs_state_check", `update going_runs set state = 'paused' where id = '` + run + `'`},
			{"a run's level max", "23514", "going_runs_level_check", `update going_runs set level = 'max' where id = '` + run + `'`},
			{"a run's mode auto", "23514", "going_runs_mode_check", `update going_runs set mode = 'auto' where id = '` + run + `'`},
			{"a grant of kind cookie", "23514", "going_run_grants_kind_check", `update going_run_grants set kind = 'cookie' ` + of},
			{"a grant of a token no row holds", "23503", "going_run_grants_token_id_fkey", `update going_run_grants set kind = 'token', token_id = gen_random_uuid() ` + of},
			{"a landing of phase redo", "23514", "going_landings_phase_check", `update going_landings set phase = 'redo' ` + of},
			{"an event of sequence 0", "23514", "going_run_events_sequence_check", `update going_run_events set sequence = 0 ` + of},
			{"a plan's state pending", "23514", "going_plans_state_check", `update going_plans set state = 'pending' ` + of},
			{"a plan row's state ticked", "23514", "going_plan_rows_state_check", `update going_plan_rows set state = 'ticked' ` + of},
			{"a plan row of a plan the run does not hold", "23503", "going_plan_rows_run_id_plan_fkey", `insert into going_plan_rows (run_id, plan, row, change, state) values ('` + run + `', 7, 0, '{}', 'waiting')`},
			{"a setting's mode auto", "23514", "user_going_settings_mode_check", `update user_going_settings set mode = 'auto' where login = 'alice'`},
		} {
			refused(t, refusal.what, refusal.code, refusal.constraint, refusal.statement)
		}
	})

	t.Run("a grant holds no column that could keep a secret", func(t *testing.T) {
		rows, err := store.Pool.Query(ctx, `
			select column_name from information_schema.columns
			where table_schema = current_schema() and table_name = 'going_run_grants'
			order by ordinal_position`)
		if err != nil {
			t.Fatalf("read the grant's columns: %v", err)
		}
		columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the grant's columns: %v", err)
		}
		if !slices.Equal(columns, goingRunGrantColumns) {
			t.Errorf("going_run_grants columns = %v, want exactly %v: a grant names who acts and by what, and keeps no cookie, assertion, token or hash", columns, goingRunGrantColumns)
		}
	})
}
