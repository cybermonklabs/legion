package daemon

import (
	"log/slog"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// planRecordID is the id of the plan every plan record here writes, and retroRecordID the id of
// the retro's own record, which the rating is written on: each a version 5 id of the form the
// workflow names a plan and a record by, and neither the reviewer's record (stepRecordID), which
// is a record of its own.
const (
	planRecordID  = "5d6e7f80-91a2-5b3c-8d4e-5f6a7b8c9d0e"
	retroRecordID = "7a1c3e55-2b6d-5f48-9c0e-1d2b3a4c5e6f"
)

// planRefusedLine is the one line that says a plan record ended unwritten.
const planRefusedLine = "outbox plan record refused by Dispatch and dropped"

// planPut is one plan write as Dispatch received it.
type planPut struct {
	Issue string
	ID    string
	Plan  dispatch.PlanWrite
}

// planRecordOf is the default plan of the run 2 of the issue.
func planRecordOf() record.PlanRecord {
	return record.PlanRecord{ID: planRecordID, Generation: 2, Source: "default",
		Tasks: []record.PlanRecordTask{{ID: "default", Steps: workflow.DefaultSteps()}}}
}

// ratingRecordOf is the write that rates the plan on the record of the issue's retro.
func ratingRecordOf() record.StepRecord {
	return record.StepRecord{ID: retroRecordID, Task: "default", Step: 0, Kind: "retro", Role: "implementer",
		Agent: "legion-legion-legion-1-implementer", Generation: 2,
		PlanRating: &record.PlanRating{Rating: 3, Reason: "The plan put the tests after the review."}}
}

// A plan record is written once, under the row's issue and the plan's own id, with the run, the
// source, the owner's document and its version where the plan has them, and every task with its
// steps in order, and the row is done. The owner's plan carries every field a default plan leaves
// empty, a version that is not its run, and two tasks out of the order of their ids, so a field the
// copy to Dispatch drops, or a sort of the tasks, changes what Dispatch receives. The daemon queues
// no owner's plan yet (the workflow's defaultPlan is its one plan): that case holds the copy to
// Dispatch of each field such a plan carries, which the record's type already has.
func TestAPlanRecordIsWrittenUnderItsIssueAndItsID(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan record.PlanRecord
		want dispatch.PlanWrite
	}{
		{name: "the default plan", plan: planRecordOf(), want: dispatch.PlanWrite{Generation: 2, Source: "default",
			Tasks: []dispatch.PlanTask{{ID: "default", Steps: workflow.DefaultSteps()}}}},
		{
			name: "an owner's plan of two tasks",
			plan: record.PlanRecord{ID: planRecordID, Generation: 3, Source: "owner", PlanDocument: &record.PlanDocument{ArtifactID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 4},
				Tasks: []record.PlanRecordTask{{ID: "ui", Steps: []string{"tester", "implementer:ui"}}, {ID: "api", Steps: []string{"reviewer", "implementer"}}}},
			want: dispatch.PlanWrite{Generation: 3, Source: "owner", PlanDocument: &dispatch.PlanDocument{ArtifactID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 4},
				Tasks: []dispatch.PlanTask{{ID: "ui", Steps: []string{"tester", "implementer:ui"}}, {ID: "api", Steps: []string{"reviewer", "implementer"}}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStepRecordRigOf(t, tc.plan, nil)
			rig.run()
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left after Dispatch took the plan = %d, want none", left)
			}
			plans, steps := rig.client.receivedPlans(), rig.client.received()
			want := planPut{Issue: stepIssue, ID: planRecordID, Plan: tc.want}
			if len(plans) != 1 || len(steps) != 0 || !reflect.DeepEqual(plans[0], want) {
				t.Fatalf("Dispatch received the plans %+v and the step writes %+v, want the one plan %+v", plans, steps, want)
			}
		})
	}
}

// The rating a retro gave the plan reaches Dispatch as a write of the retro's record on the
// record's rating route (dispatch.Client.PutStepRating), carrying the run's identity, the rating
// and its reason.
func TestTheRetrosRatingReachesDispatch(t *testing.T) {
	rig := newStepRecordRigOf(t, ratingRecordOf(), nil)
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("rows left after Dispatch took the rating = %d, want none", left)
	}
	steps := rig.client.received()
	want := stepPut{Issue: stepIssue, ID: retroRecordID, Write: dispatch.StepWrite{Task: "default", Step: 0, Kind: "retro",
		Role: "implementer", Agent: "legion-legion-legion-1-implementer", Generation: 2},
		Rating: &dispatch.PlanRating{Rating: 3, Reason: "The plan put the tests after the review."}}
	if len(steps) != 1 || !reflect.DeepEqual(steps[0], want) {
		t.Fatalf("Dispatch received %+v, want the one rating %+v", steps, want)
	}
}

// A Dispatch older than the plan route or the rating route answers its router's 404 under the
// code NOT_FOUND, "no route for …", which is no refusal of the write: the row waits rather than
// fails, as every row does whose write meets that answer (the outbox's execute), until the upgraded
// Dispatch takes it, which is what lets the daemon and Dispatch deploy in either order. Each run
// keeps the row and logs `outbox row waits`, at INFO on its first run and DEBUG on the next, and no
// ERROR line, and nothing says the write was dropped. 404 ISSUE_NOT_FOUND, Dispatch holding no such
// issue, ends the row instead (TestAPlanRecordThatDispatchRefusesEndsWithAWarning, and
// TestAStepRecordThatDispatchRefusesEndsWithAWarning for the step record a rating is), and so does
// a 400 STEP_INPUT naming the rating (TestARatingOnARecordOfAnotherKindEndsWithAWarning).
func TestAPlanAndARatingWaitForADispatchThatHasNoRouteForThem(t *testing.T) {
	notFound := noRouteError("no route for PUT /api/v1/issues/LEGION-1/…")
	for _, route := range []struct {
		name    string
		payload record.OutboxPayload
		dropped string
	}{
		{name: "the plan route", payload: planRecordOf(), dropped: planRefusedLine},
		{name: "the rating route", payload: ratingRecordOf(), dropped: stepRefusedLine},
	} {
		t.Run(route.name, func(t *testing.T) {
			rig := newStepRecordRigOf(t, route.payload, answersInTurn(notFound, notFound))
			rig.runner.log = slog.New(slog.NewJSONHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			for attempt := 1; attempt <= 2; attempt++ {
				rig.run()
				if left := rig.left(); left != 1 {
					t.Fatalf("rows left after attempt %d = %d, want the write kept for its next run", attempt, left)
				}
			}
			rig.run()
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left after Dispatch took the write = %d, want none", left)
			}
			plans, steps := rig.client.receivedPlans(), rig.client.received()
			if len(plans)+len(steps) != 3 {
				t.Fatalf("PUTs = %d plans and %d step writes, want three of one: two the old router refused, one stored", len(plans), len(steps))
			}
			rig.waitedNeverFailed(route.dropped, []string{"INFO", "DEBUG"})
		})
	}
}

// A 400 STEP_INPUT naming the rating ends the rating's row after its one run, as every refusal no
// retry changes ends a step record's (TestAStepRecordThatDispatchRefusesEndsWithAWarning): Dispatch
// answers it where the record it holds under the retro's id is a run of another kind
// (workflowstore's ratingOfKind), and that answer would come again at every retry and hold the
// record lane behind it (record.LaneRecord). The one Warn line names the issue, the record's id and
// Dispatch's code, and the row's failure is not logged as a failed attempt besides.
func TestARatingOnARecordOfAnotherKindEndsWithAWarning(t *testing.T) {
	refusal := &dispatch.Error{Status: http.StatusBadRequest, Code: oauth2calltest.Said(steprecord.CodeInput),
		Message: "rating is the rating a retro gives its plan, and step " + retroRecordID + " is a run of kind step"}
	rig := newStepRecordRigOf(t, ratingRecordOf(), answersInTurn(refusal))
	rig.run()
	if left := rig.left(); left != 0 {
		t.Fatalf("rows left after Dispatch refused the rating = %d, want none", left)
	}
	if puts := rig.client.received(); len(puts) != 1 {
		t.Fatalf("PUTs of the rating = %d, want the one Dispatch refused", len(puts))
	}
	rig.endedWithOneWarning(stepRefusedLine, "step", retroRecordID, refusal)
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
		{name: "404 ISSUE_NOT_FOUND", err: &dispatch.Error{Status: http.StatusNotFound, Code: oauth2calltest.Said(dispatch.IssueNotFound), Message: "no issue LEGION-1"}},
		{name: "400 " + steprecord.CodePlanInput, err: &dispatch.Error{Status: http.StatusBadRequest, Code: oauth2calltest.Said(steprecord.CodePlanInput), Message: "source must be one of default owner"}},
		{name: "400 INVALID_JSON", err: &dispatch.Error{Status: http.StatusBadRequest, Code: oauth2calltest.Said("INVALID_JSON"), Message: "invalid JSON body: unexpected EOF"}},
		{name: "403 " + steprecord.CodeWriter, err: &dispatch.Error{Status: http.StatusForbidden, Code: oauth2calltest.Said(steprecord.CodeWriter), Message: "only the session legion-daemon:LEGION writes a plan of LEGION-1"}},
		{name: "409 " + steprecord.CodePlanOtherIssue, err: &dispatch.Error{Status: http.StatusConflict, Code: oauth2calltest.Said(steprecord.CodePlanOtherIssue), Message: "the plan belongs to LEGION-9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newStepRecordRigOf(t, planRecordOf(), answersInTurn(tc.err))
			rig.run()
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left after Dispatch refused the plan = %d, want none", left)
			}
			if plans := rig.client.receivedPlans(); len(plans) != 1 {
				t.Fatalf("PUTs of the plan = %d, want the one Dispatch refused", len(plans))
			}
			rig.endedWithOneWarning(planRefusedLine, "plan", planRecordID, tc.err)
		})
	}
}
