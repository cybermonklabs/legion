package store

import (
	"context"
	"reflect"
	"testing"
)

// modelRequestsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const modelRequestsVersion = 20261126042544

// modelRequestsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const modelRequestsRelease = "the release that keeps each model request its project's hosts report for the provider's signals"

// 20261126042544 adds model_requests, signal_token_limits and claim_turns, which the provider's
// two signals read, and copies into claim_turns the turn each claim is in as it runs: a claim in
// working has its turn's start and no end, and a claim out of a turn has none, since the turns
// that ended before the table carry no row a read of the provider's record could tell from a
// turn that never ran. Each table refuses an empty key, a count below 0 and a refusal of no class.
// Its way back drops the three tables and the provider's row of signal_inputs, leaves every other
// input's row and deletes this version row, so a roll-forward runs the file again.
func TestTheModelRequestsArriveWith20261126042544AndTheirWayBackLetsThemComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, modelRequestsVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name in ('model_requests', 'signal_token_limits', 'claim_turns')`
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the provider's tables before the migration = %v, want none", got)
	}
	for _, claim := range []struct{ token, state, start string }{
		{"legion-legion-legion-601-implementer", "working", "'2026-11-26T09:58:00Z'"},
		{"legion-legion-legion-601-tester", "idle", "'2026-11-26T09:40:00Z'"},
		{"legion-legion-legion-601-reviewer", "working", "null"},
	} {
		if err := exec(`insert into claims (token, project, tree, issue, role, generation, session, session_file, state, failure,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak, plugin_contract, last_turn_start)
			values ($1, 'legion', 'LEGION-601', 'LEGION-601', 'implementer', 3, 'ses', '/s.jsonl', $2, '', 0, 0, 0, 0, 275, `+claim.start+`)`,
			claim.token, claim.state); err != nil {
			t.Fatalf("record %s: %v", claim.token, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the model requests: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the provider's tables after the migration = %v, want the three", got)
	}
	const turns = `select claim || ' ' || generation || ' ' || plugin_contract || ' ' || to_char(started_at at time zone 'UTC', 'HH24:MI') || ' ' || coalesce(ended_at::text, 'running')
		from claim_turns order by claim`
	if got, want := rowsAsText(t, store, turns), []string{"legion-legion-legion-601-implementer 3 275 09:58 running"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claim_turns after the migration = %v, want the one claim in a turn with a start, %v", got, want)
	}
	for _, valid := range []string{
		`insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, 'req-1', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:01Z', 1200, true, null)`,
		`insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, 'req-2', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:02Z', null, true, 'unknown')`,
		`insert into signal_token_limits (project, since, token_limit) values ('LEGION', '2026-11-26T10:00:00Z', 6000)`,
		`insert into signal_token_limits (project, since, token_limit) values ('LEGION', '2026-11-26T11:00:00Z', null)`,
	} {
		if err := exec(valid); err != nil {
			t.Fatalf("insert a valid row (%s): %v", valid, err)
		}
	}
	for name, statement := range map[string]string{
		"a request kept twice": `insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, 'req-1', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:03Z', 1, true, null)`,
		"a request of no id": `insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, '', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:03Z', 1, true, null)`,
		"a request of fewer than no tokens": `insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, 'req-3', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:03Z', -1, true, null)`,
		"a refusal of no class": `insert into model_requests (project, claim, generation, id, session, model, ended_at, uncached, counted, refusal)
			values ('LEGION', 'legion-legion-legion-601-implementer', 3, 'req-4', 'ses', 'claude-opus-5-5', '2026-11-26T10:00:03Z', 1, true, 'quota')`,
		"a limit of no tokens": `insert into signal_token_limits (project, since, token_limit) values ('LEGION', '2026-11-26T12:00:00Z', 0)`,
		"a turn of no claim":   `insert into claim_turns (project, claim, generation, plugin_contract, started_at) values ('legion', '', 1, 275, '2026-11-26T10:00:00Z')`,
	} {
		if err := exec(statement); err == nil {
			t.Errorf("%s was accepted, want it refused", name)
		}
	}
	if err := exec(`insert into signal_inputs (project, input, kept_since) values ('LEGION', 'provider', '2026-11-26T09:00:00Z'), ('LEGION', 'kills', '2026-11-26T09:00:00Z')`); err != nil {
		t.Fatalf("insert the signal inputs: %v", err)
	}

	if err := exec(wayBackOf(t, modelRequestsVersion, modelRequestsRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126042544`
	if got := append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...); !reflect.DeepEqual(got, []string{"0", "0"}) {
		t.Fatalf("after the way back the provider's tables and the version row = %v, want neither", got)
	}
	if got, want := rowsAsText(t, store, `select input from signal_inputs where project = 'LEGION' order by input`), []string{"kills"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the way back the signal inputs = %v, want %v: the provider's row goes with its tables and no other input's does", got, want)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("the provider's tables after the roll-forward = %v, want the three again", got)
	}
}
