package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// planRecordID is the id of the plan every plan record here writes, a version 5 id of the form the
// workflow names a plan by.
const planRecordID = "5d6e7f80-91a2-5b3c-8d4e-5f6a7b8c9d0e"

// planRefusedLine is the one line that says a plan record ended unwritten.
const planRefusedLine = "outbox plan record refused by Dispatch and dropped"

// planPut is one plan write as Dispatch received it.
type planPut struct {
	Issue string
	ID    string
	Plan  dispatch.PlanWrite
}

// recordDispatch is Dispatch as the record lane meets it: what each PUT of a plan or a step record
// gets back, and each PUT received. It is read while a lane runs, which outboxDispatch, whose
// other methods it keeps, is not made for.
type recordDispatch struct {
	*outboxDispatch
	mu sync.Mutex
	// answer is what the PUT numbered n, counting from zero over both routes, gets back; nil is
	// the write Dispatch stored.
	answer func(n int) error
	plans  []planPut
	steps  []stepPut
}

func (d *recordDispatch) PutPlan(_ context.Context, key, id string, plan dispatch.PlanWrite) error {
	d.mu.Lock()
	d.plans = append(d.plans, planPut{Issue: key, ID: id, Plan: plan})
	n, answer := len(d.plans)+len(d.steps)-1, d.answer
	d.mu.Unlock()
	if answer == nil {
		return nil
	}
	return answer(n)
}

func (d *recordDispatch) PutStep(_ context.Context, key, id string, write dispatch.StepWrite) error {
	d.mu.Lock()
	d.steps = append(d.steps, stepPut{Issue: key, ID: id, Write: write})
	n, answer := len(d.plans)+len(d.steps)-1, d.answer
	d.mu.Unlock()
	if answer == nil {
		return nil
	}
	return answer(n)
}

func (d *recordDispatch) received() ([]planPut, []stepPut) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.plans), slices.Clone(d.steps)
}

// planRecordOf is the default plan of the run 2 of the issue.
func planRecordOf() record.PlanRecord {
	return record.PlanRecord{ID: planRecordID, Generation: 2, Source: "default",
		Tasks: []record.PlanRecordTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}
}

// ratingRecordOf is the write that rates the plan on the record of the issue's retro.
func ratingRecordOf() record.StepRecord {
	rating, reason := 2, "The plan put the tests after the review."
	return record.StepRecord{ID: stepRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer",
		Agent: "legion-legion-legion-1-implementer", Generation: 2, Rating: &rating, RatingReason: &reason}
}

// recordRig is one issue's row of the record lane, drained by hand (RunOnce) on a clock the test
// holds, with the runner's log captured.
type recordRig struct {
	t      *testing.T
	pool   *store.Pool
	runner *outbox
	client *recordDispatch
	clock  *heldClock
	logs   *syncBuffer
}

func newRecordRig(t *testing.T, payload record.OutboxPayload, answer func(int) error) *recordRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	logs := &syncBuffer{}
	client := &recordDispatch{outboxDispatch: &outboxDispatch{}, answer: answer}
	rig := &recordRig{t: t, pool: pool, client: client, clock: microClock(), logs: logs}
	rig.runner = &outbox{pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: client,
		log: slog.New(slog.NewJSONHandler(logs, nil)), now: rig.clock.Now}
	enqueueOutbox(t, pool, record.NewStore(), mustOutboxRow(t, stepIssue, payload, rig.clock.Now()))
	return rig
}

// run drains every row due now, then moves the clock past the longest backoff, so a row the run
// retried is due at the next run.
func (r *recordRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
	r.clock.advance(2 * retryBackoff(6))
}

// A plan record is written once, under the row's issue and the plan's own id, with the run, the
// source and every task with its steps in order, and the row is done.
func TestAPlanRecordIsWrittenUnderItsIssueAndItsID(t *testing.T) {
	rig := newRecordRig(t, planRecordOf(), nil)
	rig.run()
	if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
		t.Fatalf("rows left after Dispatch took the plan = %d, want none", left)
	}
	plans, steps := rig.client.received()
	want := planPut{Issue: stepIssue, ID: planRecordID, Plan: dispatch.PlanWrite{Generation: 2, Source: "default",
		Tasks: []dispatch.PlanTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}}}
	if len(plans) != 1 || len(steps) != 0 || !reflect.DeepEqual(plans[0], want) {
		t.Fatalf("Dispatch received the plans %+v and the step writes %+v, want the one plan %+v", plans, steps, want)
	}
}

// The rating a retro gave the plan reaches Dispatch as a write of the retro's record carrying the
// rating, its reason and the run's identity, which the client sends to the record's rating route
// (dispatch.HTTPClient.PutStep).
func TestTheRetrosRatingReachesDispatch(t *testing.T) {
	rig := newRecordRig(t, ratingRecordOf(), nil)
	rig.run()
	if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
		t.Fatalf("rows left after Dispatch took the rating = %d, want none", left)
	}
	_, steps := rig.client.received()
	rating, reason := 2, "The plan put the tests after the review."
	want := stepPut{Issue: stepIssue, ID: stepRecordID, Write: dispatch.StepWrite{Task: "default", Step: 0, Kind: "retro",
		Role: "implementer", Agent: "legion-legion-legion-1-implementer", Generation: 2, Rating: &rating, RatingReason: &reason}}
	if len(steps) != 1 || !reflect.DeepEqual(steps[0], want) {
		t.Fatalf("Dispatch received %+v, want the one rating %+v", steps, want)
	}
}

// A Dispatch older than the plan route or the rating route answers its router's 404 under the
// code NOT_FOUND, which is no refusal of the write: the row retries on its backoff until the
// upgraded Dispatch takes it, which is what lets the daemon and Dispatch deploy in either order.
// 404 ISSUE_NOT_FOUND is Dispatch holding no such issue, which no retry changes: the code tells the
// two apart, never the status. And nothing retries a 400 INVALID_JSON for the rating: a Dispatch
// ignores a body member it does not declare, so no Dispatch refuses the rating by name, and such a
// refusal is a body no Dispatch will take.
func TestAnOlderDispatchsRefusalOfTheRatingRetries(t *testing.T) {
	notFound := &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route for PUT /api/v1/issues/LEGION-1/…"}
	for _, route := range []struct {
		name    string
		payload record.OutboxPayload
	}{
		{name: "the plan route", payload: planRecordOf()},
		{name: "the rating route", payload: ratingRecordOf()},
	} {
		t.Run(route.name, func(t *testing.T) {
			row := mustOutboxRow(t, stepIssue, route.payload, time.Now())
			for _, tc := range []struct {
				name      string
				err       *dispatch.Error
				permanent bool
			}{
				{name: "404 NOT_FOUND", err: notFound, permanent: false},
				{name: "404 ISSUE_NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no issue LEGION-1"}, permanent: true},
				{name: "400 INVALID_JSON naming the rating", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON",
					Message: `invalid JSON body: json: unknown field "rating"`}, permanent: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					wrapped := fmt.Errorf("write the record of %s: %w", stepIssue, tc.err)
					if got := permanentRefusal(row, wrapped); got != tc.permanent {
						t.Fatalf("permanentRefusal(%v) = %v, want %v", tc.err, got, tc.permanent)
					}
				})
			}

			rig := newRecordRig(t, route.payload, answersInTurn(notFound, notFound))
			for attempt := 1; attempt <= 2; attempt++ {
				rig.run()
				if left := outboxRowsOf(t, rig.pool, stepIssue); left != 1 {
					t.Fatalf("rows left after attempt %d = %d, want the write retried", attempt, left)
				}
			}
			rig.run()
			if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
				t.Fatalf("rows left after Dispatch took the write = %d, want none", left)
			}
			plans, steps := rig.client.received()
			if len(plans)+len(steps) != 3 {
				t.Fatalf("PUTs = %d plans and %d step writes, want three of one: two the old router refused, one stored", len(plans), len(steps))
			}
			if warned := loggedLines(t, rig.logs.String(), `"level":"WARN"`); len(warned) != 0 {
				t.Fatalf("logged %d warnings, want none for a write that landed: %s", len(warned), rig.logs.String())
			}
		})
	}
}

// What no retry changes ends a plan record after its one run, and the one Warn line says the plan
// was dropped, naming the issue, the plan's id and Dispatch's code, since nothing else would: an
// issue Dispatch does not have, a plan Dispatch will never take, a writer the route refuses, and an
// id of another issue's plan. The row's failure is not logged as a failed attempt besides.
func TestAPlanRecordThatDispatchRefusesEndsWithAWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *dispatch.Error
	}{
		{name: "404 ISSUE_NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no issue LEGION-1"}},
		{name: "400 PLAN_INPUT", err: &dispatch.Error{Status: http.StatusBadRequest, Code: dispatch.PlanInput, Message: "source must be one of default owner"}},
		{name: "400 INVALID_JSON", err: &dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_JSON", Message: "invalid JSON body: unexpected EOF"}},
		{name: "403 STEP_WRITER", err: &dispatch.Error{Status: http.StatusForbidden, Code: dispatch.StepWriter, Message: "only the session legion-daemon:LEGION writes a plan of LEGION-1"}},
		{name: "409 PLAN_OTHER_ISSUE", err: &dispatch.Error{Status: http.StatusConflict, Code: dispatch.PlanOtherIssue, Message: "the plan belongs to LEGION-9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRecordRig(t, planRecordOf(), answersInTurn(tc.err))
			rig.run()
			if left := outboxRowsOf(t, rig.pool, stepIssue); left != 0 {
				t.Fatalf("rows left after Dispatch refused the plan = %d, want none", left)
			}
			if plans, _ := rig.client.received(); len(plans) != 1 {
				t.Fatalf("PUTs of the plan = %d, want the one Dispatch refused", len(plans))
			}
			logs := rig.logs.String()
			warned := loggedLines(t, logs, `"level":"WARN"`)
			if len(warned) != 1 || warned[0]["msg"] != planRefusedLine {
				t.Fatalf("warnings = %v, want the one %q: %s", warned, planRefusedLine, logs)
			}
			for key, want := range map[string]any{"issue": stepIssue, "plan": planRecordID, "code": tc.err.Code,
				"status": float64(tc.err.Status)} {
				if warned[0][key] != want {
					t.Errorf("%q names %s %v, want %v", planRefusedLine, key, warned[0][key], want)
				}
			}
			if failed := loggedLines(t, logs, "outbox row failed"); len(failed) != 0 {
				t.Fatalf("logged %d failed attempts, want none for a refusal no retry changes: %s", len(failed), logs)
			}
		})
	}
}
