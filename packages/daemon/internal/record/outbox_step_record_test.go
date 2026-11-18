// The outbox row of a step record: the lane it runs on, what it carries through Postgres, and the
// records it refuses where they are queued.
package record

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A step record runs on a lane of its own, which runs no other kind: a Dispatch that is slow to
// take a record, or that backs one off, must hold no status write, notice or start of its issue.
func TestAStepRecordRunsAloneOnTheRecordLane(t *testing.T) {
	if lane := LaneOf(OutboxKindStepRecord); lane != LaneRecord {
		t.Fatalf("LaneOf(%s) = %q, want %q", OutboxKindStepRecord, lane, LaneRecord)
	}
	if kinds := LaneKinds(LaneRecord); !reflect.DeepEqual(kinds, []OutboxKind{OutboxKindStepRecord}) {
		t.Fatalf("LaneKinds(%s) = %v, want the step record alone", LaneRecord, kinds)
	}
}

// stepRecordID is a version 5 id of the form the workflow names a record by.
const stepRecordID = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"

// Dispatch writes each field of a record once and keeps every field the write leaves out (the step
// write of Dispatch's route), so an omitted time and a zero one are two different writes: a zero
// started_at would stand on the record for good, and a returned_to of 0 is a return to the retro.
// The row has to carry that difference through Postgres: a start with no session, no model and no
// end decodes with each of them still absent, and a retro's type, which is "", and its place, 0,
// are still written.
func TestAStepRecordKeepsWhatItOmitsApartFromWhatItWritesEmpty(t *testing.T) {
	started := time.Date(2026, 11, 16, 23, 0, 0, 123456789, time.UTC)
	ended := started.Add(time.Hour)
	session, model, result, report := "ses-1", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	returnedTo, retro := 2, 0
	for _, tc := range []struct {
		name    string
		record  StepRecord
		absent  []string
		present map[string]string
	}{
		{
			name: "a start that knows no session and no model",
			record: StepRecord{ID: stepRecordID, Task: "default", Step: 3, Kind: "step", Role: "tester", Type: "testing",
				Agent: "legion-leg-leg-1-tester", Generation: 1, StartedAt: &started},
			absent:  []string{"session", "model", "ended_at", "result", "returned_to", "report"},
			present: map[string]string{"step": "3", "generation": "1"},
		},
		{
			name: "the end of a retro, whose type is empty and whose place is 0",
			record: StepRecord{ID: stepRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
				Agent: "legion-leg-leg-1-implementer", Generation: 2, Session: &session, Model: &model, EndedAt: &ended,
				Result: &result, ReturnedTo: &returnedTo, Report: &report},
			absent:  []string{"started_at"},
			present: map[string]string{"type": `""`, "step": "0", "returned_to": "2"},
		},
		{
			name: "the end of a merge GitHub made, which no agent ran",
			record: StepRecord{ID: stepRecordID, Task: "default", Kind: "merge", Role: "merger", Generation: 1,
				EndedAt: &ended, Result: &result},
			absent:  []string{"session", "model", "started_at", "returned_to", "report"},
			present: map[string]string{"agent": `""`, "type": `""`},
		},
		{
			name: "the end of a merge that moved back to the retro, whose place it returned to is 0",
			record: StepRecord{ID: stepRecordID, Task: "default", Kind: "merge", Role: "merger",
				Agent: "legion-leg-leg-1-merger", Generation: 1, Session: &session, Model: &model, EndedAt: &ended,
				Result: &result, ReturnedTo: &retro},
			absent:  []string{"started_at", "report"},
			present: map[string]string{"step": "0", "returned_to": "0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := NewOutboxRow("LEG-1", tc.record, started)
			if err != nil {
				t.Fatalf("queue the record: %v", err)
			}
			if row.Kind != OutboxKindStepRecord {
				t.Fatalf("row kind = %q, want %q", row.Kind, OutboxKindStepRecord)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(row.Payload, &fields); err != nil {
				t.Fatalf("read the payload back: %v", err)
			}
			for _, name := range tc.absent {
				if raw, ok := fields[name]; ok {
					t.Errorf("the payload writes %s as %s, want it left out so Dispatch keeps what it holds", name, raw)
				}
			}
			for name, want := range tc.present {
				if got := string(fields[name]); got != want {
					t.Errorf("the payload writes %s as %q, want %s", name, got, want)
				}
			}
			st := migratedStore(t)
			ctx := context.Background()
			var stored OutboxRow
			inTx(t, st, func(tx pgx.Tx) {
				must(t, NewStore().Enqueue(ctx, tx, row))
				rows, err := NewStore().DueRows(ctx, tx, "LEG", "LEG-1", nil, started)
				must(t, err)
				if len(rows) != 1 {
					t.Fatalf("due rows = %#v, want the one record", rows)
				}
				stored = rows[0]
			})
			decoded, err := DecodeOutboxPayload(stored)
			if err != nil {
				t.Fatalf("decode the stored record: %v", err)
			}
			got, ok := decoded.(StepRecord)
			if !ok {
				t.Fatalf("decoded %T, want StepRecord", decoded)
			}
			if !reflect.DeepEqual(stepRecordFields(got), stepRecordFields(tc.record)) {
				t.Fatalf("decoded %s, want %s", stepRecordFields(got), stepRecordFields(tc.record))
			}
		})
	}
}

// stepRecordFields is a record with each pointer read through, nil written as <absent>, so two
// records compare by what they say and a failure prints it.
func stepRecordFields(r StepRecord) string {
	text := func(value *string) string {
		if value == nil {
			return "<absent>"
		}
		return fmt.Sprintf("%q", *value)
	}
	at := func(value *time.Time) string {
		if value == nil {
			return "<absent>"
		}
		return value.UTC().Format(time.RFC3339Nano)
	}
	place := "<absent>"
	if r.ReturnedTo != nil {
		place = fmt.Sprint(*r.ReturnedTo)
	}
	return fmt.Sprintf("{id %s task %q step %d kind %q role %q type %q agent %q generation %d session %s model %s started %s ended %s result %s returned_to %s report %s}",
		r.ID, r.Task, r.Step, r.Kind, r.Role, r.Type, r.Agent, r.Generation, text(r.Session), text(r.Model),
		at(r.StartedAt), at(r.EndedAt), text(r.Result), place, text(r.Report))
}

// A record the workflow builds without its id or its identity is a defect of the daemon, refused
// where it is queued: an id that is empty would address the route's parent path, which Dispatch's
// router answers NOT_FOUND, the one answer the row retries for ever, and a record without its
// task, kind or role is one Dispatch cannot make a node of. So is a kind, a place or a result
// outside the words and the rule Dispatch holds (steprecord, from contracts): Dispatch would refuse
// it 400 STEP_INPUT, which ends the row, and the run would have no record.
func TestAStepRecordWithoutItsIDOrIdentityIsRefusedWhereItIsQueued(t *testing.T) {
	whole := StepRecord{ID: stepRecordID, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Generation: 1}
	if _, err := NewOutboxRow("LEG-1", whole, time.Now()); err != nil {
		t.Fatalf("the whole record was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*StepRecord)
		reason string
	}{
		{name: "no id", change: func(r *StepRecord) { r.ID = "" }, reason: "names no uuid"},
		{name: "an id that is no uuid", change: func(r *StepRecord) { r.ID = "LEG-1/1/default/step/2/1" }, reason: "names no uuid"},
		{name: "no task", change: func(r *StepRecord) { r.Task = "" }, reason: "task, kind and role"},
		{name: "no kind", change: func(r *StepRecord) { r.Kind = "" }, reason: "task, kind and role"},
		{name: "no role", change: func(r *StepRecord) { r.Role = "" }, reason: "task, kind and role"},
		{name: "a kind no run has", change: func(r *StepRecord) { r.Kind = "phase" }, reason: "is no place of"},
		{name: "a retro at a place a plan names", change: func(r *StepRecord) { r.Kind, r.Type = "retro", "" }, reason: "is no place of"},
		{name: "a step at the place 0", change: func(r *StepRecord) { r.Step = 0 }, reason: "is no place of"},
		{name: "a result no run has", change: func(r *StepRecord) {
			at := time.Now()
			r.Result, r.EndedAt = new("done"), &at
		}, reason: "is no result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := whole
			tc.change(&record)
			if _, err := NewOutboxRow("LEG-1", record, time.Now()); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow = %v, want a refusal naming %q", err, tc.reason)
			}
		})
	}
}
