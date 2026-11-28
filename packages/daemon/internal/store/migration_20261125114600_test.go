package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimSessionAgentVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const claimSessionAgentVersion = 20261125114600

// claimSessionAgentRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const claimSessionAgentRelease = "the release that records the agent of a claim's session"

// 20261125114600 adds claims.session_agent, the agent whose session a claim holds
// (supervise.Claim.SessionAgent). Every claim the release before left whose name no plan row of
// its issue holds, as here on an issue with no plan rows, takes the default, the empty string,
// which records no agent: such a claim takes the agent of the next step's start that reaches it,
// brought back or still running, and keeps the session it holds for that agent. A claim whose name
// a plan row holds records the agent of such a row, with a key or without one (`key:a`,
// `step:2:t`: migration_20261125114600_keys_test.go). A row the release before inserts names no
// such column and takes the default.
//
// This release also names a step's agent on each supervise start (record.SuperviseRequest.Agent),
// a key the release before does not know: it decodes strictly, so a start left queued with it would
// fail at every attempt. The way back, the last paragraph of the migration's header
// (migrations.WayBack), takes that key off each queued supervise row and leaves every other key,
// drops the function claim_agent_from_steps and the column, and deletes the version row, so the
// roll forward runs the migration again.
func TestTheClaimRecordsTheAgentOfItsSessionAndTheWayBackForgetsIt(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimSessionAgentVersion-1)
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The release before's write of a claim names each column it writes, and session_agent is none
	// of them.
	const releaseBeforesClaim = `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses_i', '', 'idle', 0, 0, 0, 0)`
	exec(releaseBeforesClaim, "legion-LEGION-208-implementer")
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the agent of a claim's session: %v", err)
	}
	if got := rowsAsText(t, store, `select concat(token, '=', session_agent) from claims`); !reflect.DeepEqual(got, []string{"legion-LEGION-208-implementer="}) {
		t.Fatalf("the claims after the migration = %v, want the release before's claim recording no agent", got)
	}
	exec(releaseBeforesClaim, "legion-LEGION-209-implementer")
	exec(`update claims set session_agent = 'key:a' where token = 'legion-LEGION-208-implementer'`)

	const start = `{"op":"start","tree":"LEGION-208","role":"implementer","generation":1,"phase":"implementing","entry":2,"task":"Build it.","agent":"key:a"}`
	const notice = `{"kind":"task-stopped","role":"implementer","agent":"key:a"}`
	var startID, noticeID int64
	for _, row := range []struct {
		kind, payload string
		id            *int64
	}{{"supervise", start, &startID}, {"notice", notice, &noticeID}} {
		if err := store.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
			values ($1, 'LEGION-208', $2, 0, now(), '', '') returning id`, row.kind, row.payload).Scan(row.id); err != nil {
			t.Fatalf("queue the %s row: %v", row.kind, err)
		}
	}

	exec(wayBackOf(t, claimSessionAgentVersion, claimSessionAgentRelease))

	columns := rowsAsText(t, store, `select column_name::text from information_schema.columns where table_name = 'claims' and column_name = 'session_agent'`)
	if len(columns) != 0 {
		t.Fatalf("the columns of claims after the way back hold %v, want no session_agent", columns)
	}
	var payload []byte
	if err := store.Pool().QueryRow(ctx, `select payload from outbox where id = $1`, startID).Scan(&payload); err != nil {
		t.Fatalf("read the start after the way back: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(start), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "agent")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the start after the way back = %v, want %v: the agent taken off and every other key kept", after, before)
	}
	decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: startID, Kind: record.OutboxKind("supervise"), Payload: payload})
	if err != nil {
		t.Fatalf("decode the start after the way back: %v", err)
	}
	if got := decoded.(record.SuperviseRequest).Agent; got != "" {
		t.Fatalf("the start after the way back names the agent %q, want none", got)
	}
	if got := rowsAsText(t, store, `select payload::text from outbox where kind = 'notice'`); len(got) != 1 || !jsonHas(t, got[0], "agent") {
		t.Fatalf("the notice after the way back = %v, want it as it was: the way back takes the key off supervise rows alone", got)
	}
	if got := rowsAsText(t, store, `select count(*)::text from schema_version where version = 20261125114600`); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the version rows of the migration after the way back = %v, want none", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if got := rowsAsText(t, store, `select concat(token, '=', session_agent) from claims order by token`); !reflect.DeepEqual(got,
		[]string{"legion-LEGION-208-implementer=", "legion-LEGION-209-implementer="}) {
		t.Fatalf("the claims after the roll forward = %v, want each recording no agent", got)
	}
}

// jsonHas is whether the JSON object text holds key at its top level.
func jsonHas(t *testing.T, text, key string) bool {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	_, ok := object[key]
	return ok
}
