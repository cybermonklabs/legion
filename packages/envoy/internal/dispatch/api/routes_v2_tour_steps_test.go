package api

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
)

func init() { addTourSteps("steps", stepsTourSteps) }

// stepsTourSteps are the route tour's steps of an issue's step records, #4400's two routes, the
// rating #4488 added and the experiment #5014 added: the read of every run of an issue, the end of
// a run its start has made, as the issue's daemon writes it, the rating of a retro its start has
// made, and the experiment and the variant of a run its start has made. A run's {id} names a
// stored record only to a write that cannot make one, so the write's missing step is a write that
// names no identity, the end, the rating or the experiment, sent to an id no record has. It holds
// #4488's plans too: the plan of a run, whose write makes the plan whatever id it names, so its
// {id} names no missing object, and the read of a project's finished issues, one closed with its
// plan.
func stepsTourSteps() []tourStep {
	issueMissing := tourMissing{Param: "key", Value: "NOPE-404", Code: apicode.IssueNotFound}
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/steps", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.writeStep(t, issue, uuid.NewString(), tourStepStart())
				return tourRequest{Path: map[string]string{"key": issue.Key}, As: tourPerson("alice")}
			},
			Missing: []tourMissing{issueMissing},
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				run := uuid.NewString()
				tour.writeStep(t, issue, run, tourStepStart())
				return stepWriteOf(issue, run, tourStepEnd())
			},
			Missing: []tourMissing{issueMissing, {Param: "id", Value: uuid.NewString(), Code: apicode.StepNotFound}},
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}/rating", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				run := uuid.NewString()
				tour.writeStep(t, issue, run, tourRetroStart())
				return stepWriteOf(issue, run, tourRating())
			},
			Missing: []tourMissing{issueMissing, {Param: "id", Value: uuid.NewString(), Code: apicode.StepNotFound}},
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}/experiment", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				run := uuid.NewString()
				tour.writeStep(t, issue, run, tourStepStart())
				return stepWriteOf(issue, run, tourExperiment())
			},
			Missing: []tourMissing{issueMissing, {Param: "id", Value: uuid.NewString(), Code: apicode.StepNotFound}},
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/plans/{id}", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				return stepWriteOf(tour.issue(t), uuid.NewString(), tourDefaultPlan())
			},
			Missing: []tourMissing{issueMissing},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/plans", Status: http.StatusOK,
			Setup: func(t *testing.T, tour *routeTour) tourRequest {
				issue := tour.issue(t)
				tour.writePlan(t, issue, uuid.NewString(), tourDefaultPlan())
				closeIssue(t, tour.handler, issue.Key)
				return tourRequest{Path: map[string]string{"key": tour.projectOf(t, issue)}, As: tourPerson("alice")}
			},
			Missing: []tourMissing{{Param: "key", Value: "NOPE", Code: apicode.ProjectNotFound}},
		},
	}
}

// tourStepStart is the start of the implementing step of an issue's first run, as the daemon writes
// it: the run's identity, which makes its record, and when it started.
func tourStepStart() map[string]any {
	return map[string]any{
		"task": "default", "step": 2, "kind": "step", "role": "implementer", "type": "implementation",
		"generation": 1, "started_at": "2026-11-16T10:00:00Z",
	}
}

// tourStepEnd is that run's end as the daemon writes it when its start has landed: how it ended
// and when, and no identity.
func tourStepEnd() map[string]any {
	return map[string]any{"result": "passed", "ended_at": "2026-11-16T10:25:00Z"}
}

// tourRetroStart is the start of the retro of an issue's first run, as the daemon writes it: the
// run's identity, which makes its record, and when it started.
func tourRetroStart() map[string]any {
	return map[string]any{
		"task": "default", "step": 0, "kind": "retro", "role": "implementer", "generation": 1,
		"started_at": "2026-11-16T11:00:00Z",
	}
}

// tourRating is the rating that retro gave the plan, as the daemon writes it when the retro's start
// has landed: the rating and its reason, and no identity.
func tourRating() map[string]any {
	return map[string]any{"rating": 4, "rating_reason": "The plan put the tests before the review."}
}

// tourExperiment is the experiment a draw put an issue in and the variant it drew there, as the
// daemon writes them once the run's start has landed: the pair, and no identity.
func tourExperiment() map[string]any {
	return map[string]any{"experiment": "planning", "variant": "b"}
}

// tourDefaultPlan is the plan of an issue's first run whose owner gave none, in the shape the
// daemon writes it: the source default and the one task default, whose steps are four of the
// daemon's five default steps (workflow.DefaultSteps), all but acceptance:owner.
func tourDefaultPlan() map[string]any {
	return map[string]any{
		"generation": 1, "source": "default",
		"tasks": []map[string]any{{"id": "default", "steps": []string{"planner", "implementer", "tester", "reviewer"}}},
	}
}

// stepWriteOf is the write of body as run of issue, as its project's Legion daemon sends it: the
// shared token, naming the daemon's session in actor.
func stepWriteOf(issue tourIssue, run string, body map[string]any) tourRequest {
	body["actor"] = map[string]any{"kind": "session", "id": legionDaemonSessionOf(issue.Key)}
	return tourRequest{
		Path: map[string]string{"key": issue.Key, "id": run},
		Body: body,
		As:   tourBearer("agent-token"),
	}
}

// writeStep writes body as run of issue through the /api/v2 route, answering the record it
// answered.
func (tour *routeTour) writeStep(t *testing.T, issue tourIssue, run string, body map[string]any) model.StepRecord {
	t.Helper()
	written := tour.send(t, tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}"}),
		stepWriteOf(issue, run, body), "")
	if written.Code != http.StatusOK {
		t.Fatalf("write step %s of %s: %d %s", run, issue.Key, written.Code, written.Body.String())
	}
	return decodeBody[model.StepRecord](t, written)
}

// writePlan writes body as plan of issue through the /api/v2 route, answering the plan it
// answered.
func (tour *routeTour) writePlan(t *testing.T, issue tourIssue, plan string, body map[string]any) model.PlanRecord {
	t.Helper()
	written := tour.send(t, tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/plans/{id}"}),
		stepWriteOf(issue, plan, body), "")
	if written.Code != http.StatusOK {
		t.Fatalf("write plan %s of %s: %d %s", plan, issue.Key, written.Code, written.Body.String())
	}
	return decodeBody[model.PlanRecord](t, written)
}

// refusalWithoutExperiment leaves the experiment and the variant out of an experiment write's
// body, which then names neither.
func refusalWithoutExperiment(_ *testing.T, _ *routeTour, request tourRequest) tourRequest {
	body := maps.Clone(request.Body.(map[string]any))
	delete(body, "experiment")
	delete(body, "variant")
	request.Body = body
	return request
}

// refusalStepOfAnotherIssue names in {id} a run the daemon recorded on another issue of the tour's
// project, so the write's issue holds no record of that id and the store holds it as the other's.
func refusalStepOfAnotherIssue(t *testing.T, tour *routeTour, request tourRequest) tourRequest {
	t.Helper()
	run := uuid.NewString()
	tour.writeStep(t, tour.issue(t), run, tourStepStart())
	request.Path = maps.Clone(request.Path)
	request.Path["id"] = run
	return request
}

// TestV2StepWriteOfWhatTheRecordHoldsWritesNothing: a write of a run every field of which the
// record already holds as sent is the no-op PUT of decision 8. It answers the record as it stands
// and makes no write: it answers while another transaction holds the run's row, which a write's
// lock would wait on. An end the record does not hold yet is a change, and is stored; and the list
// reads the run as the write answered it.
func TestV2StepWriteOfWhatTheRecordHoldsWritesNothing(t *testing.T) {
	tour := newRouteTour(t)
	route := tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}"})
	issue := tour.issue(t)
	run := uuid.NewString()
	started := tour.writeStep(t, issue, run, tourStepStart())

	holding, err := tour.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that holds the run: %v", err)
	}
	defer holding.Rollback(t.Context())
	if _, err := holding.Exec(t.Context(), `select id from workflow_nodes where kind = $1 and id = $2 for update`,
		workflowstore.KindStep, run); err != nil {
		t.Fatalf("lock the run's row: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	again := tour.request(t, route, stepWriteOf(issue, run, tourStepStart()), "")
	go func() { answered <- tour.serve(again) }()
	select {
	case response := <-answered:
		if response.Code != http.StatusOK {
			t.Fatalf("the start again: %d %s, want 200 with the record", response.Code, response.Body.String())
		}
		if answer := decodeBody[model.StepRecord](t, response); answer.ID != started.ID || answer.Result != "" ||
			answer.StartedAt == nil || !answer.StartedAt.Equal(*started.StartedAt) {
			t.Fatalf("the start again answered %+v, want the record as it was started, %+v", answer, started)
		}
	case <-time.After(10 * time.Second):
		_ = holding.Rollback(t.Context())
		<-answered
		t.Fatal("the start again waited on the lock of the run's row: it wrote, where an equal write writes nothing")
	}
	if err := holding.Rollback(t.Context()); err != nil {
		t.Fatalf("release the run's row: %v", err)
	}

	ended := tour.writeStep(t, issue, run, tourStepEnd())
	if ended.Result != "passed" || ended.EndedAt == nil || ended.Role != "implementer" {
		t.Fatalf("the end answered %+v, want the started record ended passed", ended)
	}
	listed := tour.send(t, tour.stepRoute(t, tourStep{Method: http.MethodGet, Pattern: "/issues/{key}/steps"}),
		tourRequest{Path: map[string]string{"key": issue.Key}, As: tourPerson("alice")}, "")
	if read := decodeBody[page[model.StepRecord]](t, listed); listed.Code != http.StatusOK || read.Total != 1 ||
		len(read.Items) != 1 || read.Items[0].ID != run || read.Items[0].Result != "passed" {
		t.Fatalf("the list answered %d %s, want the one run, ended passed", listed.Code, listed.Body.String())
	}
}

// TestV2PlanAndRatingWritesOfWhatTheyHoldWriteNothing: a write of a plan, or of a retro's rating,
// every member of which the record already holds as sent is the no-op PUT of decision 8, as a
// step's write is (TestV2StepWriteOfWhatTheRecordHoldsWritesNothing): it answers the record as it
// stands while another transaction holds the record's row, which a write's lock would wait on. A
// rating the record does not hold yet is a change, and is stored; and the read of the project's
// finished issues lists the closed issue, one page, with that plan and that rating.
func TestV2PlanAndRatingWritesOfWhatTheyHoldWriteNothing(t *testing.T) {
	tour := newRouteTour(t)
	rating := tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/steps/{id}/rating"})
	issue := tour.issue(t)
	plan, retro := uuid.NewString(), uuid.NewString()
	written := tour.writePlan(t, issue, plan, tourDefaultPlan())
	tour.writeStep(t, issue, retro, tourRetroStart())
	rated := tour.send(t, rating, stepWriteOf(issue, retro, tourRating()), "")
	if record := decodeBody[model.StepRecord](t, rated); rated.Code != http.StatusOK || record.Rating == nil || *record.Rating != 4 {
		t.Fatalf("the rating answered %d %s, want 200 with the record rated 4", rated.Code, rated.Body.String())
	}

	for _, again := range []struct {
		name, kind, id string
		route          apiRoute
		body           map[string]any
		// stands answers whether the write's answer is the record as it stands.
		stands func(t *testing.T, answer *httptest.ResponseRecorder) bool
	}{
		{"the plan again", workflowstore.KindPlan, plan, tour.stepRoute(t, tourStep{Method: http.MethodPut, Pattern: "/issues/{key}/plans/{id}"}), tourDefaultPlan(),
			func(t *testing.T, answer *httptest.ResponseRecorder) bool {
				return decodeBody[model.PlanRecord](t, answer).ID == written.ID
			}},
		{"the rating again", workflowstore.KindStep, retro, rating, tourRating(),
			func(t *testing.T, answer *httptest.ResponseRecorder) bool {
				record := decodeBody[model.StepRecord](t, answer)
				return record.ID == retro && record.Rating != nil && *record.Rating == 4 &&
					record.RatingReason != nil && *record.RatingReason == tourRating()["rating_reason"]
			}},
	} {
		t.Run(again.name, func(t *testing.T) {
			release := holdLock(t, tour.database, `select id from workflow_nodes where kind = $1 and id = $2 for update`,
				again.kind, again.id)
			answered := make(chan *httptest.ResponseRecorder, 1)
			sending := tour.request(t, again.route, stepWriteOf(issue, again.id, again.body), "")
			go func() { answered <- tour.serve(sending) }()
			select {
			case response := <-answered:
				if response.Code != http.StatusOK || !again.stands(t, response) {
					t.Fatalf("%s: %d %s, want 200 with the record as it stands", again.name, response.Code, response.Body.String())
				}
			case <-time.After(10 * time.Second):
				release()
				<-answered
				t.Fatalf("%s waited on the lock of the record's row: it wrote, where an equal write writes nothing", again.name)
			}
		})
	}

	closeIssue(t, tour.handler, issue.Key)
	listed := tour.send(t, tour.stepRoute(t, tourStep{Method: http.MethodGet, Pattern: "/projects/{key}/plans"}),
		tourRequest{Path: map[string]string{"key": tour.projectOf(t, issue)}, As: tourPerson("alice")}, "")
	read := decodeBody[page[model.FinishedIssue]](t, listed)
	if listed.Code != http.StatusOK || read.Total != 1 || len(read.Items) != 1 {
		t.Fatalf("the list answered %d %s, want the one finished issue", listed.Code, listed.Body.String())
	}
	if finished := read.Items[0]; finished.Issue != issue.Key || finished.Plan == nil || finished.Plan.ID != written.ID ||
		len(finished.Ratings) != 1 || finished.Ratings[0].Rating != 4 || finished.Ratings[0].StepID != retro {
		t.Fatalf("the list answered %s, want %s with plan %s and the retro's one rating, 4", listed.Body.String(), issue.Key, written.ID)
	}
}
