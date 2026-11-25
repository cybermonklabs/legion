package store

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// launchTotalsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const launchTotalsVersion = 20261125171320

// launchTotalsRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const launchTotalsRelease = "the release that counts each step's models"

// stepRecordKeysBefore is every key of a step_record payload the release before decodes. That
// release decodes a stored row strictly (record's DecodeOutboxPayload, DisallowUnknownFields), so
// a row holding any other key fails its decode at every attempt and its write never lands. The
// list is a copy because that release's code is not in the tree.
var stepRecordKeysBefore = []string{"id", "task", "step", "kind", "role", "type", "agent", "generation", "session", "model",
	"started_at", "ended_at", "result", "returned_to", "report", "rating", "rating_reason"}

// stepRecordKeys is each key of the payload of the one step_record row of store, sorted.
func stepRecordKeys(t *testing.T, store *Store) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stepRecordPayload(t, store)), &fields); err != nil {
		t.Fatalf("read the step record's payload: %v", err)
	}
	return slices.Sorted(maps.Keys(fields))
}

// stepRecordPayload is the payload of the one step_record row of store, as text.
func stepRecordPayload(t *testing.T, store *Store) string {
	t.Helper()
	payloads := rowsAsText(t, store, `select payload::text from outbox where kind = 'step_record'`)
	if len(payloads) != 1 {
		t.Fatalf("the outbox holds %d step_record rows, want the one the test queued", len(payloads))
	}
	return payloads[0]
}

// storedStepRecord is the one step_record row of store as the record decodes it.
func storedStepRecord(t *testing.T, store *Store) record.StepRecord {
	t.Helper()
	decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: 1, Kind: record.OutboxKindStepRecord, Issue: "LEGION-208",
		Payload: json.RawMessage(stepRecordPayload(t, store))})
	if err != nil {
		t.Fatalf("decode the stored step record: %v", err)
	}
	got, ok := decoded.(record.StepRecord)
	if !ok {
		t.Fatalf("the stored step record decodes as %T, want record.StepRecord", decoded)
	}
	return got
}

// storedStepStart is the step_starts row of LEGION-208's place as the record reads it.
func storedStepStart(t *testing.T, store *Store, place record.StepPlace) record.StepStart {
	t.Helper()
	var start record.StepStart
	if err := Transact(context.Background(), store.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		start, err = record.NewStore().StepStarts(ctx, tx, "LEGION-208", place)
		return err
	}); err != nil {
		t.Fatalf("read the starts of LEGION-208: %v", err)
	}
	return start
}

// 20261125171320 adds the table claim_launches and the column step_starts.start_totals. Every row
// the release before wrote still reads after it: a step's start as the record reads it, with no
// start totals, and a step_record row as the outbox decodes it, with no set model and no answers.
// The rows are written with SQL that names the columns and keys of the release before.
func TestTheLaunchTotalsMigrationLeavesEveryRowReadable(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, launchTotalsVersion-1)
	started := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	testing3 := record.StepPlace{Task: "default", Kind: "step", Place: 3}
	const tester = "legion-legion-legion-208-tester"
	if _, err := store.Pool().Exec(ctx, `insert into step_starts (issue, task, kind, place, starts, agent, started_at, run, record)
		values ('LEGION-208', 'default', 'step', 3, 2, $1, $2, 'open', '{"id": "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"}')`, tester, started); err != nil {
		t.Fatalf("record the start as the release before writes it: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('step_record', 'LEGION-208', $1, 0, now(), '')`, `{"id":"0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b","task":"default","step":3,`+
		`"kind":"step","role":"tester","type":"testing","agent":"`+tester+`","generation":1,"session":"ses-tester",`+
		`"model":"anthropic/claude-opus-5","ended_at":"2026-11-24T02:00:00Z","result":"passed"}`); err != nil {
		t.Fatalf("queue the end as the release before writes it: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the launch totals: %v", err)
	}

	if applied := rowsAsText(t, store, `select version::text from schema_version where version = 20261125171320`); len(applied) != 1 {
		t.Fatalf("schema_version records %v of 20261125171320, want the migration applied", applied)
	}
	if got := rowsAsText(t, store, `select row(issue, starts, start_totals is null)::text from step_starts`); !slices.Equal(got, []string{"(LEGION-208,2,t)"}) {
		t.Fatalf("the starts after the migration (issue, starts, no start totals) = %v, want the start of the release before with none", got)
	}
	if got, want := storedStepStart(t, store, testing3), (record.StepStart{Starts: 2, Agent: tester, StartedAt: started, Run: record.RunOpen}); got != want {
		t.Fatalf("the start of the release before reads as %+v, want %+v", got, want)
	}
	session, model, result := "ses-tester", "anthropic/claude-opus-5", "passed"
	want := record.StepRecord{ID: "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b", Task: "default", Step: 3, Kind: "step", Role: "tester",
		Type: "testing", Agent: tester, Generation: 1, Session: &session, Model: &model, EndedAt: &ended, Result: &result}
	if got := storedStepRecord(t, store); !reflect.DeepEqual(got, want) {
		t.Fatalf("the end of the release before decodes as %+v, want %+v", got, want)
	}
	if got := rowsAsText(t, store, `select claim from claim_launches`); len(got) != 0 {
		t.Fatalf("the migration records the launches %v, want none: a launch is recorded by the claim write of this release", got)
	}
}

// The way back of the release that counts each step's models leaves a store the release before
// runs on, and the roll forward adds the table and the column again, empty. The way back takes the
// two new keys off each queued step_record row, which the release before refuses to decode. It
// drops the start totals, which a start of the release before would leave in place for the next
// run, and the table of launches, and it deletes the migration's version row. The control: before
// the way back, the queued end holds keys the release before refuses.
func TestTheLaunchTotalsWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	started := time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	testing3 := record.StepPlace{Task: "default", Kind: "step", Place: 3}
	agent := tmuxClaim("legion-LEGION-208-tester")
	agent.Issue, agent.Role = "LEGION-208", claim.RoleTester
	if err := store.PutClaim(ctx, agent); err != nil {
		t.Fatalf("record the tester's launch: %v", err)
	}
	read := record.LaunchRead{Totals: shimwire.Totals{Read: 3, SetModel: "anthropic/claude-opus-5",
		Counts: []shimwire.ModelCount{{Model: "anthropic/claude-opus-5", Calls: 4}}}, At: started}
	session, model, result := "ses-tester", "anthropic/claude-opus-5", "passed"
	end := record.StepRecord{ID: "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b", Task: "default", Step: 3, Kind: "step", Role: "tester",
		Type: "testing", Agent: string(agent.Token), Generation: 1, Session: &session, Model: &model, EndedAt: &ended, Result: &result,
		SetModel: &model, Answers: &[]record.ModelCalls{{Model: "anthropic/claude-opus-5", Calls: 7}}}
	if err := Transact(ctx, store.pool, func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if _, err := records.RaiseStepStarts(ctx, tx, "LEGION-208", testing3, func(int64) record.StepRecord {
			return record.StepRecord{ID: end.ID, Task: "default", Step: 3, Kind: "step", Role: "tester", Type: "testing",
				Agent: end.Agent, Generation: 1, StartedAt: &started}
		}); err != nil {
			return err
		}
		if err := records.KeepStartTotals(ctx, tx, "LEGION-208", testing3,
			record.StartTotals{Generation: agent.Generation, Live: true, Read: &read, FromZero: agent.Generation + 1}); err != nil {
			return err
		}
		if err := records.KeepRead(ctx, tx, record.LaunchKey{Claim: agent.Token, Generation: agent.Generation}, read); err != nil {
			return err
		}
		row, err := record.NewOutboxRow("LEGION-208", end, ended)
		if err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, row)
	}); err != nil {
		t.Fatalf("write the step, its start totals, its launch's read and its end as this release does: %v", err)
	}
	if keys := stepRecordKeys(t, store); !slices.Contains(keys, "set_model") || !slices.Contains(keys, "answers") {
		t.Fatalf("control: the queued end holds the keys %v, want set_model and answers among them, which the release before refuses", keys)
	}

	if _, err := store.Pool().Exec(ctx, wayBackOf(t, launchTotalsVersion, launchTotalsRelease), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back of %s: %v", launchTotalsRelease, err)
	}

	for _, key := range stepRecordKeys(t, store) {
		if !slices.Contains(stepRecordKeysBefore, key) {
			t.Errorf("after the way back the queued end holds %q, which the release before refuses to decode", key)
		}
	}
	withoutCounts := end
	withoutCounts.SetModel, withoutCounts.Answers = nil, nil
	if got := storedStepRecord(t, store); !reflect.DeepEqual(got, withoutCounts) {
		t.Errorf("after the way back the queued end reads as %+v, want the same end with no set model and no answers: %+v", got, withoutCounts)
	}
	var launches, startTotals, version bool
	if err := store.Pool().QueryRow(ctx, `select to_regclass('claim_launches') is not null,
		exists (select 1 from information_schema.columns where table_name = 'step_starts' and column_name = 'start_totals'),
		exists (select 1 from schema_version where version = 20261125171320)`).Scan(&launches, &startTotals, &version); err != nil {
		t.Fatalf("read the schema after the way back: %v", err)
	}
	if launches || startTotals || version {
		t.Fatalf("after the way back claim_launches stands (%t), step_starts.start_totals does (%t) or the version row does (%t), want none",
			launches, startTotals, version)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward: %v", err)
	}
	if got, want := storedStepStart(t, store, testing3), (record.StepStart{Starts: 1, Agent: end.Agent, StartedAt: started, Run: record.RunOpen}); got != want {
		t.Fatalf("after the roll forward the start reads as %+v, want %+v with no start totals", got, want)
	}
	if got := rowsAsText(t, store, `select claim from claim_launches`); len(got) != 0 {
		t.Fatalf("after the roll forward claim_launches holds %v, want it empty", got)
	}
	if got := storedStepRecord(t, store); !reflect.DeepEqual(got, withoutCounts) {
		t.Fatalf("after the roll forward the queued end reads as %+v, want %+v", got, withoutCounts)
	}
}
