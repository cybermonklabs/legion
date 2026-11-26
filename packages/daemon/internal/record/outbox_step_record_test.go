// The outbox rows of a step record and of a plan record: the lane they run on, what they carry
// through Postgres, and the records they refuse where they are queued.
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

// The records of an issue's work — each step's and the plan's — run on a lane of their own, which
// runs no other kind: a Dispatch that is slow to take a record, or that backs one off until it has
// the route, must hold no status write, notice or start of its issue.
func TestTheRecordLaneRunsTheStepAndThePlanRecordsAlone(t *testing.T) {
	for _, kind := range []OutboxKind{OutboxKindStepRecord, OutboxKindPlanRecord} {
		if lane := LaneOf(kind); lane != LaneRecord {
			t.Fatalf("LaneOf(%s) = %q, want %q", kind, lane, LaneRecord)
		}
	}
	if kinds := LaneKinds(LaneRecord); !reflect.DeepEqual(kinds, []OutboxKind{OutboxKindStepRecord, OutboxKindPlanRecord}) {
		t.Fatalf("LaneKinds(%s) = %v, want the step record and the plan record alone", LaneRecord, kinds)
	}
}

// stepRecordID is a version 5 id of the form the workflow names a record by.
const stepRecordID = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"

// Dispatch writes each field of a record once and keeps every field the write leaves out (the step
// write of Dispatch's route), so an omitted time and a zero one are two different writes: a zero
// started_at would stand on the record for good, and a returned_to of 0 is a return to the retro.
// The row has to carry that difference through Postgres: a start with no session, no model and no
// end decodes with each of them still absent, and a retro's type, which is "", and its place, 0,
// are still written. Only the write that rates a retro carries a rating and its reason, which every
// other write leaves out.
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
			absent:  []string{"session", "model", "ended_at", "result", "returned_to", "report", "rating", "rating_reason"},
			present: map[string]string{"step": "3", "generation": "1"},
		},
		{
			name: "the end of a retro, whose type is empty and whose place is 0",
			record: StepRecord{ID: stepRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
				Agent: "legion-leg-leg-1-implementer", Generation: 2, Session: &session, Model: &model, EndedAt: &ended,
				Result: &result, ReturnedTo: &returnedTo, Report: &report},
			absent:  []string{"started_at", "rating", "rating_reason"},
			present: map[string]string{"type": `""`, "step": "0", "returned_to": "2"},
		},
		{
			name: "the end of a merge GitHub made, which no agent ran",
			record: StepRecord{ID: stepRecordID, Task: "default", Kind: "merge", Role: "merger", Generation: 1,
				EndedAt: &ended, Result: &result},
			absent:  []string{"session", "model", "started_at", "returned_to", "report", "rating", "rating_reason"},
			present: map[string]string{"agent": `""`, "type": `""`},
		},
		{
			name: "the rating of a retro, which knows no moment and no result",
			record: StepRecord{ID: stepRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
				Agent: "legion-leg-leg-1-implementer", Generation: 2, PlanRating: &PlanRating{Rating: 4, Reason: "The plan fit the work."}},
			absent:  []string{"session", "model", "started_at", "ended_at", "result", "returned_to", "report"},
			present: map[string]string{"rating": "4", "rating_reason": `"The plan fit the work."`, "type": `""`, "step": "0"},
		},
		{
			name: "the end of a merge that moved back to the retro, whose place it returned to is 0",
			record: StepRecord{ID: stepRecordID, Task: "default", Kind: "merge", Role: "merger",
				Agent: "legion-leg-leg-1-merger", Generation: 1, Session: &session, Model: &model, EndedAt: &ended,
				Result: &result, ReturnedTo: &retro},
			absent:  []string{"started_at", "report", "rating", "rating_reason"},
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
	number := func(value *int) string {
		if value == nil {
			return "<absent>"
		}
		return fmt.Sprint(*value)
	}
	rating := "<absent>"
	if r.PlanRating != nil {
		rating = fmt.Sprintf("%d %q", r.PlanRating.Rating, r.PlanRating.Reason)
	}
	variant := "<absent>"
	if r.StepExperiment != nil {
		variant = fmt.Sprintf("%q %q", r.StepExperiment.Experiment, r.StepExperiment.Variant)
	}
	return fmt.Sprintf("{id %s task %q step %d kind %q role %q type %q agent %q generation %d session %s model %s started %s ended %s result %s returned_to %s report %s rating %s variant %s}",
		r.ID, r.Task, r.Step, r.Kind, r.Role, r.Type, r.Agent, r.Generation, text(r.Session), text(r.Model),
		at(r.StartedAt), at(r.EndedAt), text(r.Result), number(r.ReturnedTo), text(r.Report), rating, variant)
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

// The rating of the plan is written by the retro (workflow's rateRetro), onto the retro's record:
// its kind retro, its place 0, its role the role its agent runs as (the implementer here, as on a
// task whose last implementation step is the implementer's and at an owner's retro) and its type
// empty. A rating without its
// reason, or a reason without its rating, is refused where the record is queued and where a stored
// row is read (TestDecodeOutboxPayloadRefusesInvalidRows); validateOutboxPayload says why the
// daemon refuses the two apart rather than leaving them to Dispatch. A retro's write with no rating
// is taken, and so is one with both; each is queued as the payload the stored rows already hold,
// and that payload, read back, is the record.
func TestARetrosRatingIsTakenWithItsReasonOrNotAtAll(t *testing.T) {
	retro := StepRecord{ID: stepRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
		Agent: "legion-leg-leg-1-implementer", Generation: 1}
	identity := `{"id":"` + stepRecordID + `","task":"default","step":0,"kind":"retro","role":"implementer","type":"",` +
		`"agent":"legion-leg-leg-1-implementer","generation":1`
	rated := retro
	rated.PlanRating = &PlanRating{Rating: 3, Reason: "The plan put the tests after the review."}
	for _, tc := range []struct {
		name   string
		record StepRecord
		stored string
	}{
		{name: "a write of the retro that carries no rating", record: retro, stored: identity + `}`},
		{name: "the retro's rating with its reason", record: rated,
			stored: identity + `,"rating":3,"rating_reason":"The plan put the tests after the review."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := NewOutboxRow("LEG-1", tc.record, time.Now())
			if err != nil {
				t.Fatalf("the record was refused: %v", err)
			}
			if string(row.Payload) != tc.stored {
				t.Fatalf("the record is queued as %s, want %s", row.Payload, tc.stored)
			}
			decoded, err := DecodeOutboxPayload(OutboxRow{ID: 70, Kind: OutboxKindStepRecord, Payload: json.RawMessage(tc.stored)})
			if err != nil {
				t.Fatalf("the stored payload was refused: %v", err)
			}
			got, ok := decoded.(StepRecord)
			if !ok || stepRecordFields(got) != stepRecordFields(tc.record) {
				t.Fatalf("the stored payload reads as %T %+v, want %s", decoded, decoded, stepRecordFields(tc.record))
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*StepRecord)
	}{
		{name: "a reason without its rating", change: func(r *StepRecord) { r.PlanRating = &PlanRating{Reason: "The plan put the tests after the review."} }},
		{name: "a rating without its reason", change: func(r *StepRecord) { r.PlanRating = &PlanRating{Rating: 3} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := retro
			tc.change(&record)
			if _, err := NewOutboxRow("LEG-1", record, time.Now()); err == nil || !strings.Contains(err.Error(), "its reason or neither") {
				t.Fatalf("NewOutboxRow = %v, want a refusal naming %q", err, "its reason or neither")
			}
		})
	}
}

// The experiment a draw put a record's issue in and the variant it drew are one value, as a
// rating and its reason are: a write names both or neither. A record naming one alone is a defect
// of the daemon, refused where it is queued and where a stored row is read, rather than found by
// Dispatch's refusal after the work it records went on. A record naming both is queued with the two
// members beside its other fields, and that payload, read back, is the record; a record naming
// neither is queued as the payload the stored rows already hold.
func TestAStepRecordsExperimentIsTakenWithItsVariantOrNotAtAll(t *testing.T) {
	start := StepRecord{ID: stepRecordID, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Generation: 1}
	identity := `{"id":"` + stepRecordID + `","task":"default","step":2,"kind":"step","role":"implementer",` +
		`"type":"implementation","agent":"legion-leg-leg-1-implementer","generation":1`
	named := start
	named.StepExperiment = &StepExperiment{Experiment: "planning", Variant: "b"}
	for _, tc := range []struct {
		name   string
		record StepRecord
		stored string
	}{
		{name: "a start that names no experiment", record: start, stored: identity + `}`},
		{name: "a start that names the experiment with its variant", record: named,
			stored: identity + `,"experiment":"planning","variant":"b"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := NewOutboxRow("LEG-1", tc.record, time.Now())
			if err != nil {
				t.Fatalf("the record was refused: %v", err)
			}
			if string(row.Payload) != tc.stored {
				t.Fatalf("the record is queued as %s, want %s", row.Payload, tc.stored)
			}
			decoded, err := DecodeOutboxPayload(OutboxRow{ID: 71, Kind: OutboxKindStepRecord, Payload: json.RawMessage(tc.stored)})
			if err != nil {
				t.Fatalf("the stored payload was refused: %v", err)
			}
			got, ok := decoded.(StepRecord)
			if !ok || stepRecordFields(got) != stepRecordFields(tc.record) {
				t.Fatalf("the stored payload reads as %T %+v, want %s", decoded, decoded, stepRecordFields(tc.record))
			}
		})
	}
	const reason = "its variant or neither"
	for _, tc := range []struct {
		name   string
		pair   StepExperiment
		stored string
	}{
		{name: "an experiment without its variant", pair: StepExperiment{Experiment: "planning"}, stored: identity + `,"experiment":"planning"}`},
		{name: "a variant without its experiment", pair: StepExperiment{Variant: "b"}, stored: identity + `,"variant":"b"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := start
			record.StepExperiment = &tc.pair
			if _, err := NewOutboxRow("LEG-1", record, time.Now()); err == nil || !strings.Contains(err.Error(), reason) {
				t.Errorf("NewOutboxRow = %v, want a refusal naming %q", err, reason)
			}
			_, err := DecodeOutboxPayload(OutboxRow{ID: 72, Kind: OutboxKindStepRecord, Payload: json.RawMessage(tc.stored)})
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Errorf("DecodeOutboxPayload(%s) = %v, want a refusal naming %q", tc.stored, err, reason)
			}
		})
	}
}

// planRecordID is a version 5 id of the form the workflow names a plan by.
const planRecordID = "5d6e7f80-91a2-5b3c-8d4e-5f6a7b8c9d0e"

// A plan record is the plan one run of an issue carries out, written once when its work starts. A
// plan of the source default names no document and no version, which the row leaves out, since
// Dispatch refuses either on such a plan; its tasks and their steps survive Postgres in order. A
// plan of the source owner carries both; the daemon queues none yet (the workflow's defaultPlan is
// its one plan), so that case holds the row's copy of each field such a plan carries. The owner's
// tasks stand out of the order of their ids, ui before api, and each one's steps out of sorted
// order, so a sort of either list on the way in or out changes what is compared; and each case
// builds its plan afresh for the comparison, so a sort of the queued value in place does not change
// the value it is held to.
func TestAPlanRecordKeepsItsTasksInOrderThroughTheOutbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    func() PlanRecord
		absent  []string
		present map[string]string
	}{
		{
			name: "the default plan",
			plan: func() PlanRecord {
				return PlanRecord{ID: planRecordID, Generation: 2, Source: "default",
					Tasks: []PlanRecordTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}
			},
			absent:  []string{"artifact_id", "version"},
			present: map[string]string{"generation": "2", "source": `"default"`, "tasks": `[{"id":"default","steps":["planner","implementer","tester","reviewer","acceptance:owner"]}]`},
		},
		{
			name: "an owner's plan of two tasks",
			plan: func() PlanRecord {
				return PlanRecord{ID: planRecordID, Generation: 1, Source: "owner", PlanDocument: &PlanDocument{ArtifactID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 3},
					Tasks: []PlanRecordTask{{ID: "ui", Steps: []string{"tester", "implementer:ui"}}, {ID: "api", Steps: []string{"reviewer", "implementer"}}}}
			},
			present: map[string]string{"artifact_id": `"6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"`, "version": "3",
				"tasks": `[{"id":"ui","steps":["tester","implementer:ui"]},{"id":"api","steps":["reviewer","implementer"]}]`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
			row, err := NewOutboxRow("LEG-1", tc.plan(), now)
			if err != nil {
				t.Fatalf("queue the plan: %v", err)
			}
			if row.Kind != OutboxKindPlanRecord {
				t.Fatalf("row kind = %q, want %q", row.Kind, OutboxKindPlanRecord)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(row.Payload, &fields); err != nil {
				t.Fatalf("read the payload back: %v", err)
			}
			for _, name := range tc.absent {
				if raw, ok := fields[name]; ok {
					t.Errorf("the payload writes %s as %s, want it left out", name, raw)
				}
			}
			for name, want := range tc.present {
				if got := string(fields[name]); got != want {
					t.Errorf("the payload writes %s as %s, want %s", name, got, want)
				}
			}
			st := migratedStore(t)
			ctx := context.Background()
			var stored OutboxRow
			inTx(t, st, func(tx pgx.Tx) {
				must(t, NewStore().Enqueue(ctx, tx, row))
				rows, err := NewStore().DueRows(ctx, tx, "LEG", "LEG-1", nil, now)
				must(t, err)
				if len(rows) != 1 {
					t.Fatalf("due rows = %#v, want the one plan", rows)
				}
				stored = rows[0]
			})
			decoded, err := DecodeOutboxPayload(stored)
			if err != nil {
				t.Fatalf("decode the stored plan: %v", err)
			}
			if want := tc.plan(); !reflect.DeepEqual(decoded, want) {
				t.Fatalf("decoded %#v, want %#v", decoded, want)
			}
		})
	}
}

// A plan record the workflow builds without its id is a defect of the daemon, refused where it is
// queued: an empty id addresses the route's parent path, which Dispatch's router answers
// NOT_FOUND, the one answer the row retries for ever.
func TestAPlanRecordWithoutItsIDIsRefusedWhereItIsQueued(t *testing.T) {
	whole := PlanRecord{ID: planRecordID, Generation: 1, Source: "default", Tasks: []PlanRecordTask{{ID: "default", Steps: []string{"planner"}}}}
	if _, err := NewOutboxRow("LEG-1", whole, time.Now()); err != nil {
		t.Fatalf("the whole plan was refused: %v", err)
	}
	for name, id := range map[string]string{"no id": "", "an id that is no uuid": "LEG-1/1"} {
		t.Run(name, func(t *testing.T) {
			plan := whole
			plan.ID = id
			if _, err := NewOutboxRow("LEG-1", plan, time.Now()); err == nil || !strings.Contains(err.Error(), "names no uuid") {
				t.Fatalf("NewOutboxRow = %v, want a refusal naming no uuid", err)
			}
		})
	}
}
