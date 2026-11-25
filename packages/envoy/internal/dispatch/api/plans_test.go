package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The plan of an issue's run and the rating of its retro, as the Legion daemon writes them
// (PUT /api/v1/issues/{key}/plans/{id}, PUT /api/v1/issues/{key}/steps/{id}/rating), and the read
// of a project's finished issues (GET /api/v1/projects/{key}/plans), driven over the credentials a
// daemon and a person present.

// at is the moment n minutes after the first step of every run below started.
func at(n int) time.Time { return stepStart.Add(time.Duration(n) * time.Minute) }

// defaultStepLine is the line of steps of an issue whose owner gave no plan.
var defaultStepLine = []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}

// defaultPlanBody is the plan the daemon writes when the work of an issue's run starts and its
// owner gave none: the source default, one task default holding the default steps.
func defaultPlanBody(actor map[string]any, generation int) map[string]any {
	return map[string]any{
		"actor": actor, "generation": generation, "source": "default",
		"tasks": []map[string]any{{"id": "default", "steps": defaultStepLine}},
	}
}

// putPlanRecord writes body as the plan id of issue over the credential the deployment configured
// for its daemon, which proves no session, so the body names the writer.
func putPlanRecord(t *testing.T, handler http.Handler, issue, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/plans/"+id, body)
}

func putPlanRecordOK(t *testing.T, handler http.Handler, issue, id string, body any) model.PlanRecord {
	t.Helper()
	response := putPlanRecord(t, handler, issue, id, body)
	if response.Code != http.StatusOK {
		t.Fatalf("write plan %s of %s: status=%d body=%s, want 200", id, issue, response.Code, response.Body.String())
	}
	return decodeBody[model.PlanRecord](t, response)
}

// retroIdentity is what every write of the retro of an issue's run names.
func retroIdentity(actor map[string]any, generation int) map[string]any {
	return map[string]any{
		"actor": actor, "task": "default", "step": 0, "kind": "retro", "role": "implementer", "generation": generation,
	}
}

// putRating writes body as the rating of the retro record id of issue.
func putRating(t *testing.T, handler http.Handler, issue, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+id+"/rating", body)
}

func putRatingOK(t *testing.T, handler http.Handler, issue, id string, body any) model.StepRecord {
	t.Helper()
	response := putRating(t, handler, issue, id, body)
	if response.Code != http.StatusOK {
		t.Fatalf("rate retro %s of %s: status=%d body=%s, want 200", id, issue, response.Code, response.Body.String())
	}
	return decodeBody[model.StepRecord](t, response)
}

// readPlans reads the finished issues of project as a person reads them, with query after the path.
func readPlans(t *testing.T, handler http.Handler, project, query string) model.ProjectPlans {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/plans"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the plans of %s: status=%d body=%s, want 200", project, response.Code, response.Body.String())
	}
	return decodeBody[model.ProjectPlans](t, response)
}

// run is one run of one step as the daemon records it: its start and its end, either nil where the
// daemon wrote none.
type run struct {
	kind, role     string
	place          int
	started, ended *time.Time
	result         string
}

// recordRuns writes each run of issue's first generation, start and end, as the daemon writes them,
// and answers their ids in order.
func recordRuns(t *testing.T, handler http.Handler, project, issue string, runs ...run) []string {
	t.Helper()
	ids := make([]string, len(runs))
	for index, each := range runs {
		ids[index] = uuid.NewString()
		body := map[string]any{
			"actor": daemonSessionOf(project), "task": "default", "step": each.place, "kind": each.kind,
			"role": each.role, "generation": 1,
		}
		if each.started != nil {
			putStepOK(t, handler, issue, ids[index], withFields(body, map[string]any{"started_at": *each.started}))
		}
		if each.ended != nil {
			end := withFields(body, map[string]any{"result": each.result, "ended_at": *each.ended})
			if each.result == "failed" {
				end["returned_to"] = 2
			}
			putStepOK(t, handler, issue, ids[index], end)
		}
	}
	return ids
}

// rateRetro rates the retro record id of issue's run generation, as the daemon writes the rating
// the retro's completion carried.
func rateRetro(t *testing.T, handler http.Handler, project, issue, id string, generation, rating int, reason string) {
	t.Helper()
	putRatingOK(t, handler, issue, id, withFields(retroIdentity(daemonSessionOf(project), generation),
		map[string]any{"rating": rating, "rating_reason": reason}))
}

// The read of a project lists each of its issues that is done, newest close first, each with the
// plan its run wrote, the rating of its retro with the reason, the time from its first start to its
// last end, and its count of failed steps; the issue still in progress is not listed. The three
// finished issues close in an order that is neither their key order nor their title order, either
// way round, so a read sorted by key or by title is seen. The first issue ran the default plan and
// the other two owners' plans, each of its own document and steps, so a plan read onto another
// issue's row is seen; each failed step returns to the implementer's place, before its own. The
// first issue's last end is 30.7 seconds past a whole minute, so its time is whole seconds, cut,
// not minutes.
func TestTheReadOfAProjectListsEachFinishedIssue(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issues := createListedIssues(t, handler, "LEG", 4)
	reviewed, fitted, hurried, running := issues[0], issues[1], issues[2], issues[3]
	daemon := daemonSessionOf("LEG")
	fittedSteps := []string{"planner", "implementer", "acceptance:owner"}
	hurriedSteps := []string{"planner", "implementer", "tester"}
	fittedDocument, fittedVersion := uuid.NewString(), 2
	hurriedDocument, hurriedVersion := uuid.NewString(), 5
	reviewedPlan := model.PlanRecord{ID: uuid.NewString(), Issue: reviewed, Generation: 1, Source: "default",
		Tasks: []model.PlanTask{{ID: "default", Steps: defaultStepLine}}}
	fittedPlan := model.PlanRecord{ID: uuid.NewString(), Issue: fitted, Generation: 1, Source: "owner",
		ArtifactID: &fittedDocument, Version: &fittedVersion, Tasks: []model.PlanTask{{ID: "default", Steps: fittedSteps}}}
	hurriedPlan := model.PlanRecord{ID: uuid.NewString(), Issue: hurried, Generation: 1, Source: "owner",
		ArtifactID: &hurriedDocument, Version: &hurriedVersion, Tasks: []model.PlanTask{{ID: "default", Steps: hurriedSteps}}}
	merged := at(50).Add(30*time.Second + 700*time.Millisecond)
	// The closes, oldest first: fitted, hurried, reviewed. So the read lists reviewed, hurried,
	// fitted: issues 0, 2, 1, which is neither ascending nor descending by key or by title.
	fittedClosed, hurriedClosed, reviewedClosed := at(1000), at(2000), at(3000)
	closeAt := func(key string, closed time.Time) {
		t.Helper()
		closeIssue(t, handler, key)
		if _, err := database.Pool.Exec(context.Background(), "update issues set closed_at = $2 where key = $1", key, closed); err != nil {
			t.Fatalf("close %s at %s: %v", key, closed, err)
		}
	}

	putPlanRecordOK(t, handler, reviewed, reviewedPlan.ID, defaultPlanBody(daemon, 1))
	runs := recordRuns(t, handler, "LEG", reviewed,
		run{"step", "planner", 1, new(at(0)), new(at(5)), "passed"},
		run{"step", "implementer", 2, new(at(5)), new(at(20)), "passed"},
		run{"step", "reviewer", 4, new(at(20)), new(at(25)), "failed"},
		run{"step", "implementer", 2, new(at(25)), new(at(35)), "passed"},
		run{"step", "reviewer", 4, new(at(35)), new(at(40)), "passed"},
		run{"retro", "implementer", 0, new(at(40)), new(at(42)), "passed"},
		run{"merge", "merger", 0, nil, &merged, "passed"},
	)
	reviewedRetro := runs[5]
	rateRetro(t, handler, "LEG", reviewed, reviewedRetro, 1, 4, "The tester found the bug.")

	putPlanRecordOK(t, handler, fitted, fittedPlan.ID, withFields(defaultPlanBody(daemon, 1), map[string]any{
		"source": "owner", "artifact_id": fittedDocument, "version": fittedVersion,
		"tasks": []map[string]any{{"id": "default", "steps": fittedSteps}},
	}))
	runs = recordRuns(t, handler, "LEG", fitted,
		run{"step", "planner", 1, new(at(100)), new(at(110)), "passed"},
		run{"step", "implementer", 2, new(at(110)), new(at(130)), "passed"},
		run{"retro", "implementer", 0, new(at(140)), new(at(141)), "passed"},
		run{"merge", "merger", 0, nil, new(at(160)), "passed"},
	)
	fittedRetro := runs[2]
	rateRetro(t, handler, "LEG", fitted, fittedRetro, 1, 5, "The plan fit the work.")

	putPlanRecordOK(t, handler, hurried, hurriedPlan.ID, withFields(defaultPlanBody(daemon, 1), map[string]any{
		"source": "owner", "artifact_id": hurriedDocument, "version": hurriedVersion,
		"tasks": []map[string]any{{"id": "default", "steps": hurriedSteps}},
	}))
	runs = recordRuns(t, handler, "LEG", hurried,
		run{"step", "planner", 1, new(at(300)), new(at(302)), "passed"},
		run{"step", "implementer", 2, new(at(302)), new(at(305)), "passed"},
		run{"step", "tester", 3, new(at(305)), new(at(310)), "failed"},
		run{"step", "implementer", 2, new(at(310)), new(at(311)), "passed"},
		run{"step", "tester", 3, new(at(311)), new(at(318)), "failed"},
		run{"step", "implementer", 2, new(at(318)), new(at(319)), "passed"},
		run{"step", "tester", 3, new(at(319)), new(at(320)), "passed"},
		run{"retro", "implementer", 0, new(at(320)), new(at(321)), "passed"},
		run{"merge", "merger", 0, nil, new(at(330)), "passed"},
	)
	hurriedRetro := runs[7]
	rateRetro(t, handler, "LEG", hurried, hurriedRetro, 1, 2, "The tests came last and failed twice.")

	closeAt(fitted, fittedClosed)
	closeAt(hurried, hurriedClosed)
	closeAt(reviewed, reviewedClosed)

	putPlanRecordOK(t, handler, running, uuid.NewString(), defaultPlanBody(daemon, 1))
	recordRuns(t, handler, "LEG", running, run{"step", "reviewer", 4, new(at(200)), new(at(205)), "failed"})

	read := readPlans(t, handler, "LEG", "")
	listed := make([]string, len(read.Issues))
	for index, issue := range read.Issues {
		listed[index] = issue.Issue
	}
	if want := []string{reviewed, hurried, fitted}; read.Project != "LEG" || !slices.Equal(listed, want) {
		t.Fatalf("the read of %s lists %v, want %v, the newest close first", read.Project, listed, want)
	}
	for _, want := range []struct {
		issue          model.FinishedIssue
		title          string
		closed         time.Time
		plan           model.PlanRecord
		rating         int
		reason         string
		retro          string
		retroEnded     time.Time
		duration       int64
		failed         int
		started, ended time.Time
	}{
		{read.Issues[0], "LEG listed issue 0", reviewedClosed, reviewedPlan, 4, "The tester found the bug.", reviewedRetro, at(42), 3030, 1, at(0), merged},
		{read.Issues[1], "LEG listed issue 2", hurriedClosed, hurriedPlan, 2, "The tests came last and failed twice.", hurriedRetro, at(321), 1800, 2, at(300), at(330)},
		{read.Issues[2], "LEG listed issue 1", fittedClosed, fittedPlan, 5, "The plan fit the work.", fittedRetro, at(141), 3600, 0, at(100), at(160)},
	} {
		issue := want.issue
		if issue.Plan == nil || !reflect.DeepEqual(*issue.Plan, want.plan) {
			t.Fatalf("%s's plan is %s, want %s, the plan its own run wrote", issue.Issue, asJSON(t, issue.Plan), asJSON(t, want.plan))
		}
		if len(issue.Ratings) != 1 || issue.Ratings[0].Rating != want.rating || issue.Ratings[0].Reason != want.reason {
			t.Fatalf("%s's ratings are %s, want one, %d %q", issue.Issue, asJSON(t, issue.Ratings), want.rating, want.reason)
		}
		// The retro's record and end, and the issue's title and close, each differ from a value of
		// the same type beside it (the issue's key, its last end), so each is checked equal.
		if rated := issue.Ratings[0]; rated.StepID != want.retro || rated.EndedAt == nil || !rated.EndedAt.Equal(want.retroEnded) {
			t.Errorf("%s's rating is of record %s ended %v, want the retro %s ended %s", issue.Issue, rated.StepID, rated.EndedAt,
				want.retro, want.retroEnded)
		}
		if issue.Title != want.title || !issue.ClosedAt.Equal(want.closed) {
			t.Errorf("%s is listed with title %q closed %s, want %q closed %s", issue.Issue, issue.Title, issue.ClosedAt, want.title, want.closed)
		}
		if issue.DurationSeconds == nil || *issue.DurationSeconds != want.duration ||
			issue.StartedAt == nil || !issue.StartedAt.Equal(want.started) || issue.EndedAt == nil || !issue.EndedAt.Equal(want.ended) {
			seconds := "no"
			if issue.DurationSeconds != nil {
				seconds = fmt.Sprint(*issue.DurationSeconds)
			}
			t.Fatalf("%s's time is %v to %v, %s seconds, want %s to %s, %d seconds", issue.Issue, issue.StartedAt, issue.EndedAt,
				seconds, want.started, want.ended, want.duration)
		}
		if issue.FailedSteps != want.failed {
			t.Fatalf("%s counts %d failed steps, want %d", issue.Issue, issue.FailedSteps, want.failed)
		}
	}
}

// A rating is a retro's: the rating route refuses a record of kind step or merge, and a record the
// node holds as a step, 400 STEP_INPUT naming the rating; the same rating of a retro lands, and the
// retro's record carries it to a reader. The step write itself takes no rating: a body that names
// one is written without it.
func TestARatingBelongsToARetroRecordAlone(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	rating := map[string]any{"rating": 3, "rating_reason": "The plan was late."}

	step := putRating(t, handler, issue, uuid.NewString(), withFields(stepIdentity(daemon), rating))
	expectRefusal(t, "a rating of a step", step, http.StatusBadRequest, codeStepInput, "rating")
	merge := putRating(t, handler, issue, uuid.NewString(), withFields(retroIdentity(daemon, 1), withFields(rating, map[string]any{"kind": "merge"})))
	expectRefusal(t, "a rating of a merge", merge, http.StatusBadRequest, codeStepInput, "rating")
	held := uuid.NewString()
	putStepOK(t, handler, issue, held, implementerStart(daemon))
	asRetro := putRating(t, handler, issue, held, withFields(retroIdentity(daemon, 1), rating))
	expectRefusal(t, "a rating of a record held as a step", asRetro, http.StatusBadRequest, codeStepInput, "rating")

	unrated := uuid.NewString()
	written := putStepOK(t, handler, issue, unrated, withFields(retroIdentity(daemon, 1), withFields(rating, map[string]any{"started_at": stepStart})))
	if written.Rating != nil || written.RatingReason != nil {
		t.Fatalf("the step write answers rating %v %v, want none: a rating has a route of its own", written.Rating, written.RatingReason)
	}

	retro := uuid.NewString()
	rated := putRatingOK(t, handler, issue, retro, withFields(retroIdentity(daemon, 1), rating))
	if rated.Kind != "retro" || rated.Rating == nil || *rated.Rating != 3 || rated.RatingReason == nil || *rated.RatingReason != "The plan was late." {
		t.Fatalf("the rating answers %s, want the retro rated 3 with its reason", asJSON(t, rated))
	}
	for _, record := range stepsOf(t, handler, issue) {
		switch record.ID {
		case retro:
			if record.Rating == nil || *record.Rating != 3 || record.RatingReason == nil || *record.RatingReason != "The plan was late." {
				t.Fatalf("the read lists the retro as %s, want its rating and reason", asJSON(t, record))
			}
		default:
			if record.Rating != nil || record.RatingReason != nil {
				t.Fatalf("the read lists %s with a rating, want only the rated retro to carry one", asJSON(t, record))
			}
		}
	}
}

// Only the daemon of the issue's project rates a retro, as only it writes the retro's record: a
// person, an agent's session and the daemon of another project, each sending the rating the
// daemon's own write lands with, are refused 403 STEP_WRITER, whether the retro's record exists or
// not, and the retro the daemon recorded stays unrated; the daemon's rating of it lands, which is
// the control.
func TestARatingIsTheDaemonsAlone(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	retro := uuid.NewString()
	putStepOK(t, handler, issue, retro, withFields(retroIdentity(daemonSessionOf("LEG"), 1), map[string]any{"started_at": stepStart}))
	rating := map[string]any{"rating": 4, "rating_reason": "The plan fit the work."}

	for _, id := range []string{retro, uuid.NewString()} {
		path := "/api/v1/issues/" + issue + "/steps/" + id + "/rating"
		person := dispatchRequest(t, handler, http.MethodPut, path, withFields(retroIdentity(nil, 1), rating), "alice")
		expectRefusal(t, "a person's rating of "+id, person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
		for _, session := range []string{model.LegionDaemonSessionPrefix + "OTHER", "ses_agent"} {
			refused := putRating(t, handler, issue, id, withFields(retroIdentity(map[string]any{"kind": "session", "id": session}, 1), rating))
			expectRefusal(t, "a rating of "+id+" as "+session, refused, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
		}
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != retro || steps[0].Rating != nil || steps[0].RatingReason != nil {
		t.Fatalf("the refused ratings left %s, want the one retro %s, unrated", asJSON(t, steps), retro)
	}

	rated := putRatingOK(t, handler, issue, retro, withFields(retroIdentity(daemonSessionOf("LEG"), 1), rating))
	if rated.Rating == nil || *rated.Rating != 4 || rated.RatingReason == nil || *rated.RatingReason != "The plan fit the work." {
		t.Fatalf("the daemon's rating answers %s, want the retro rated 4 with its reason", asJSON(t, rated))
	}
}

// exactLogin is a sign-in that takes the person the X-Dispatch-User header names as written, where
// the package's header identity lowercases it: so a person can carry a session's id, here the
// daemon's, which no sign-in a deployment configures gives.
type exactLogin struct{}

func (exactLogin) Login(r *http.Request) (string, error) {
	if login := r.Header.Get("X-Dispatch-User"); login != "" {
		return login, nil
	}
	return "", identity.ErrNoIdentity
}

// The writer of an issue's records is the daemon's session, kind and id both: a signed-in person
// whose login is legion-daemon:LEG is refused the plan write and the rating write 403 STEP_WRITER
// with nothing written, where the session of that id writes both, which is the control.
func TestTheDaemonsIDIsNoWriterButAsASession(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{identity: exactLogin{}})
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	writer := model.LegionDaemonSessionPrefix + "LEG"
	plan, retro := uuid.NewString(), uuid.NewString()
	rating := withFields(retroIdentity(nil, 1), map[string]any{"rating": 2, "rating_reason": "The plan was thin."})

	planned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/plans/"+plan, defaultPlanBody(nil, 1), writer)
	expectRefusal(t, "a plan of a person named "+writer, planned, http.StatusForbidden, codeStepWriter, writer, issue)
	rated := dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+retro+"/rating", rating, writer)
	expectRefusal(t, "a rating of a person named "+writer, rated, http.StatusForbidden, codeStepWriter, writer, issue)
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}
	if holdsPlan(t, handler, issue) {
		t.Fatalf("the refused writes left a plan of %s, want none", issue)
	}

	putPlanRecordOK(t, handler, issue, plan, defaultPlanBody(daemonSessionOf("LEG"), 1))
	putRatingOK(t, handler, issue, retro, withFields(rating, map[string]any{"actor": daemonSessionOf("LEG")}))
}

// workflowRows counts the rows of the workflow store's two tables, which every plan write and
// every step write adds to.
func workflowRows(t *testing.T, database *store.Store) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(),
		`select (select count(*) from workflow_nodes) + (select count(*) from workflow_tuples)`).Scan(&rows); err != nil {
		t.Fatalf("count the workflow rows: %v", err)
	}
	return rows
}

// A credential scoped to one project writes no plan and no rating of another project's issue,
// though it proves the session of that issue's daemon and the body names that session, so the
// project check alone stands between them: 403 PROJECT_SCOPE with nothing written. The same writes
// over a credential scoped to the issue's project land, which is the control.
func TestAProjectCredentialWritesNoPlanOrRatingOfAnotherProject(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "OTH", "name": "OTH"}); created.Code != http.StatusCreated {
		t.Fatalf("create project OTH: status=%d body=%s", created.Code, created.Body.String())
	}
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTH", "title": "Plans of OTH"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create an issue in OTH: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	const elsewhere, own = "a-daemon-token-scoped-to-another-project", "a-daemon-token-scoped-to-the-issues-project"
	for token, projects := range map[string][]string{elsewhere: {brokerTestProject}, own: {"OTH"}} {
		answer := liveNamedToken(caller.jkt)
		answer.Session, answer.Projects = model.LegionDaemonSessionPrefix+"OTH", projects
		rig.broker.Says(token, answer)
	}
	daemon := daemonSessionOf("OTH")

	for _, write := range []struct {
		name, path string
		body       map[string]any
	}{
		{"plan", "/api/v1/issues/" + issue + "/plans/" + uuid.NewString(), defaultPlanBody(daemon, 1)},
		{"rating", "/api/v1/issues/" + issue + "/steps/" + uuid.NewString() + "/rating",
			withFields(retroIdentity(daemon, 1), map[string]any{"rating": 3, "rating_reason": "The plan was late."})},
	} {
		before := workflowRows(t, rig.database)
		refused := rig.serve(caller.presenting(t, http.MethodPut, write.path, elsewhere, write.body))
		expectRefusal(t, "a "+write.name+" over a credential of "+brokerTestProject, refused, http.StatusForbidden,
			codeProjectScope, brokerTestProject, "OTH")
		if after := workflowRows(t, rig.database); after != before {
			t.Fatalf("the refused %s took the workflow store from %d rows to %d, want none written", write.name, before, after)
		}
		if answered := rig.serve(caller.presenting(t, http.MethodPut, write.path, own, write.body)); answered.Code != http.StatusOK {
			t.Fatalf("the same %s over a credential of OTH: status=%d body=%s, want 200", write.name, answered.Code, answered.Body.String())
		}
	}
}

// Each rating outside its form is refused 400 STEP_INPUT in the words of the check that refuses it,
// and writes nothing: the rating 1 to 5 and the reason not blank and at most 2,000 code points,
// each the store's refusal. A body naming no rating is the rating route's own refusal, since the
// store takes a step write with no rating; the same body on the step route lands, which is its
// control. The writer is checked before that refusal, as every write route checks its caller
// first: the same body from an agent's session is refused 403 STEP_WRITER. Every row names its
// refusal's words, since the route's own refusal names both rating and rating_reason, so a value
// the route dropped on its way to the store is seen. A rating with no reason reaches the store
// only after the issue's lookup: the same body for an issue Dispatch does not hold is 404
// ISSUE_NOT_FOUND. A reason of exactly 2,000 code points is taken, which is the control of the
// bound.
func TestARatingWriteRefusesItsBadValues(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	retro := retroIdentity(daemonSessionOf("LEG"), 1)
	noReason := withFields(retro, map[string]any{"rating": 3})
	noRating := withFields(retro, map[string]any{"rating_reason": "Why."})
	const routesOwn = "rating is required: the rating route writes a retro's rating, 1 to 5, with its rating_reason"

	for _, row := range []struct {
		name    string
		body    map[string]any
		refusal string
	}{
		{"a rating of 0", withFields(retro, map[string]any{"rating": 0, "rating_reason": "Why."}), "rating 0 is no rating"},
		{"a rating of 6", withFields(retro, map[string]any{"rating": 6, "rating_reason": "Why."}), "rating 6 is no rating"},
		{"no rating", noRating, routesOwn},
		{"neither a rating nor a reason", retro, routesOwn},
		{"no reason", noReason, "rating_reason is left out or blank: a rating carries one sentence of why"},
		{"a blank reason", withFields(retro, map[string]any{"rating": 3, "rating_reason": " \n "}), "rating_reason is left out or blank: a rating carries one sentence of why"},
		{"a reason of 2,001 code points", withFields(retro, map[string]any{"rating": 3, "rating_reason": strings.Repeat("é", 2001)}), "rating_reason holds 2001 characters"},
	} {
		t.Run(row.name, func(t *testing.T) {
			expectRefusal(t, row.name, putRating(t, handler, issue, uuid.NewString(), row.body), http.StatusBadRequest, codeStepInput, row.refusal)
		})
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused ratings left %d records, want none", len(steps))
	}
	fromAgent := putRating(t, handler, issue, uuid.NewString(), withFields(retroIdentity(map[string]any{"kind": "session", "id": "ses_agent"}, 1), map[string]any{"rating_reason": "Why."}))
	expectRefusal(t, "no rating from an agent's session", fromAgent, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	missing := putRating(t, handler, "LEG-999", uuid.NewString(), noReason)
	expectRefusal(t, "a rating with no reason of LEG-999", missing, http.StatusNotFound, "ISSUE_NOT_FOUND", "LEG-999")
	if written := putStepOK(t, handler, issue, uuid.NewString(), retro); written.Kind != "retro" || written.Rating != nil {
		t.Fatalf("the step route answers %s for the retro's identity alone, want the retro with no rating", asJSON(t, written))
	}
	longest := putRatingOK(t, handler, issue, uuid.NewString(), withFields(retro, map[string]any{"rating": 1, "rating_reason": strings.Repeat("é", 2000)}))
	if longest.RatingReason == nil || len([]rune(*longest.RatingReason)) != 2000 {
		t.Fatalf("a reason of 2,000 code points answers %s, want it whole", asJSON(t, longest))
	}
}

// A rating reason holding a NUL character (U+0000), which PostgreSQL's text cannot hold, is refused
// by the body's decoder before the route reads it (decodeJSON, unstorableJSON): 400 NUL_CHARACTER
// naming rating_reason and where in it the NUL stands, with nothing written. The same rating with
// the NUL taken out lands, which is the control.
func TestARatingReasonHoldingANulIsRefused(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	retro := retroIdentity(daemonSessionOf("LEG"), 1)
	id := uuid.NewString()

	refused := putRating(t, handler, issue, id, withFields(retro, map[string]any{"rating": 3, "rating_reason": "The plan\x00 was late."}))
	expectRefusal(t, "a reason holding a NUL", refused, http.StatusBadRequest, "NUL_CHARACTER",
		"rating_reason holds a NUL character (U+0000) at character 9, which Dispatch cannot store")
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused rating left %d records, want none", len(steps))
	}
	if rated := putRatingOK(t, handler, issue, id, withFields(retro, map[string]any{"rating": 3, "rating_reason": "The plan was late."})); rated.RatingReason == nil || *rated.RatingReason != "The plan was late." {
		t.Fatalf("the rating with no NUL answers %s, want its reason", asJSON(t, rated))
	}
}

// A body that repeats a member is read for a NUL character (U+0000) in every copy of the member,
// as the route's typed decode keeps each: it merges a repeated object into its first copy, so a
// later actor that leaves the origin out keeps the first copy's, and a null in place of a string
// leaves that string as the first copy set it. Each body below is refused 400 NUL_CHARACTER naming
// actor.origin.cwd, on the plan write and on the rating write, with nothing written; the same body
// with the NUL taken out lands, which is the control.
func TestARepeatedMemberHidesNoNulFromAPlanOrARating(t *testing.T) {
	t.Parallel()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	awaitBackgroundWrites(t, served)
	daemon := `{"kind":"session","id":"legion-daemon:LEG"`
	for _, route := range []struct {
		name, target, fields string
	}{
		{"plan", "/api/v1/issues/" + issue + "/plans/%s",
			`"generation":1,"source":"default","tasks":[{"id":"default","steps":["planner"]}]`},
		{"rating", "/api/v1/issues/" + issue + "/steps/%s/rating",
			`"task":"default","step":0,"kind":"retro","role":"implementer","generation":1,"rating":3,"rating_reason":"The plan fit."`},
	} {
		for _, actor := range []struct {
			name string
			// actor is the body's actor with %s where its cwd stands.
			actor string
		}{
			{"a repeated actor", daemon + `,"origin":{"cwd":"%s"}},"actor":` + daemon + `}`},
			{"a repeated cwd set to null", daemon + `,"origin":{"cwd":"%s","cwd":null}}`},
		} {
			t.Run(route.name+": "+actor.name, func(t *testing.T) {
				body := func(cwd string) json.RawMessage {
					return json.RawMessage(`{"actor":` + fmt.Sprintf(actor.actor, cwd) + `,` + route.fields + `}`)
				}
				target := fmt.Sprintf(route.target, uuid.NewString())
				before := databaseFingerprint(t, database)
				refused := sessionRequest(t, handler, http.MethodPut, target, body(`a\u0000b`))
				assertRefusal(t, "PUT "+target, refused.Code, refused.Body.Bytes(), "NUL_CHARACTER", "actor.origin.cwd")
				assertUnchanged(t, "PUT "+target, before, databaseFingerprint(t, database))
				if clean := sessionRequest(t, handler, http.MethodPut, target, body("ab")); clean.Code != http.StatusOK {
					t.Fatalf("the body with no NUL: status=%d body=%s, want 200", clean.Code, clean.Body.String())
				}
			})
		}
	}
}

// A rating is written once, and the retro's end that lands after it keeps it: the daemon's two rows
// for one retro land in either order.
func TestARatingIsWrittenOnceInEitherOrder(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	id := uuid.NewString()

	rateRetro(t, handler, "LEG", issue, id, 1, 2, "The plan skipped the tests.")
	again := putRatingOK(t, handler, issue, id, withFields(retroIdentity(daemon, 1), map[string]any{"rating": 5, "rating_reason": "Changed my mind."}))
	if again.Rating == nil || *again.Rating != 2 || *again.RatingReason != "The plan skipped the tests." {
		t.Fatalf("a second rating answers %s, want the first, 2", asJSON(t, again))
	}
	ended := putStepOK(t, handler, issue, id, withFields(retroIdentity(daemon, 1), map[string]any{"result": "passed", "ended_at": at(3)}))
	if ended.Result != "passed" || ended.Rating == nil || *ended.Rating != 2 {
		t.Fatalf("the retro's end after its rating answers %s, want the end beside the rating", asJSON(t, ended))
	}
}

// An issue that ran three times has three plans and three retros. The second run's plan is its
// owner's, with the document, a version that is no generation of the issue, and three tasks in
// the order the owner gave them, which is neither order of their ids; the plan write answers it
// whole. The first and the third run's plans are default plans, of runs whose owner gave none. The
// plans were written in the order of generations 1, 3 and 2, so the newest, generation 3, is
// neither the first nor the last made, and its id is neither the least nor the greatest of the
// three; and the owner's plan is not the newest, so a read that took an owner's plan over a default
// one lists another. The read lists the newest plan, the highest generation, and the three ratings
// whole, in the order the retros ran: each its own score, reason, record and end. The scores (3, 1,
// 4) and the reasons ("The review…", "The plan…", "Two tasks…") are each in neither ascending nor
// descending order as the retros ran, so a sort of the ratings by either, in either direction, is
// seen; with two retros one direction of each always matches the order they ran in.
func TestAnIssueWithThreeRetrosHasThreeRatings(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	document, version := uuid.NewString(), 7
	owners := model.PlanRecord{ID: "10000000-0000-4000-8000-000000000000", Issue: issue, Generation: 2, Source: "owner", ArtifactID: &document, Version: &version,
		Tasks: []model.PlanTask{
			{ID: "web", Steps: []string{"planner", "implementer"}},
			{ID: "api", Steps: []string{"implementer", "reviewer:security", "tester"}},
			{ID: "docs", Steps: []string{"writer"}},
		}}
	newest := model.PlanRecord{ID: "50000000-0000-4000-8000-000000000000", Issue: issue, Generation: 3, Source: "default",
		Tasks: []model.PlanTask{{ID: "default", Steps: defaultStepLine}}}
	putPlanRecordOK(t, handler, issue, "90000000-0000-4000-8000-000000000000", defaultPlanBody(daemon, 1))
	putPlanRecordOK(t, handler, issue, newest.ID, defaultPlanBody(daemon, 3))
	written := putPlanRecordOK(t, handler, issue, owners.ID, map[string]any{
		"actor": daemon, "generation": 2, "source": "owner", "artifact_id": document, "version": version,
		"tasks": []map[string]any{
			{"id": "web", "steps": []string{"planner", "implementer"}},
			{"id": "api", "steps": []string{"implementer", "reviewer:security", "tester"}},
			{"id": "docs", "steps": []string{"writer"}},
		},
	})
	if !reflect.DeepEqual(written, owners) {
		t.Fatalf("the owner's plan write answers %s, want %s", asJSON(t, written), asJSON(t, owners))
	}
	retros := map[int]struct {
		started time.Time
		rating  int
		reason  string
	}{
		1: {at(10), 3, "The review came late."},
		2: {at(100), 1, "The plan was late."},
		3: {at(190), 4, "Two tasks were one."},
	}
	ids := map[int]string{}
	for generation, retro := range retros {
		id := uuid.NewString()
		ids[generation] = id
		putStepOK(t, handler, issue, id, withFields(retroIdentity(daemon, generation), map[string]any{"started_at": retro.started}))
		putStepOK(t, handler, issue, id, withFields(retroIdentity(daemon, generation), map[string]any{"result": "passed", "ended_at": retro.started.Add(time.Minute)}))
		rateRetro(t, handler, "LEG", issue, id, generation, retro.rating, retro.reason)
	}
	closeIssue(t, handler, issue)

	read := readPlans(t, handler, "LEG", "")
	if len(read.Issues) != 1 {
		t.Fatalf("the read lists %d issues, want %s", len(read.Issues), issue)
	}
	if plan := read.Issues[0].Plan; plan == nil || !reflect.DeepEqual(*plan, newest) {
		t.Fatalf("the read lists the plan %s, want the default plan of generation 3, %s", asJSON(t, plan), asJSON(t, newest))
	}
	want := []model.PlanRating{
		{Rating: 3, Reason: "The review came late.", StepID: ids[1], EndedAt: new(at(11))},
		{Rating: 1, Reason: "The plan was late.", StepID: ids[2], EndedAt: new(at(101))},
		{Rating: 4, Reason: "Two tasks were one.", StepID: ids[3], EndedAt: new(at(191))},
	}
	if got := planRatingsText(read.Issues[0].Ratings); !slices.Equal(got, planRatingsText(want)) {
		t.Fatalf("the ratings are\n%s\nwant each retro's own, in the order they ran\n%s",
			strings.Join(got, "\n"), strings.Join(planRatingsText(want), "\n"))
	}
}

// planRatingsText is each rating of a read as one line, its score, reason, record and end, so two
// lists compare whole and a difference prints whole.
func planRatingsText(ratings []model.PlanRating) []string {
	lines := make([]string, len(ratings))
	for index, rating := range ratings {
		ended := "no end"
		if rating.EndedAt != nil {
			ended = rating.EndedAt.UTC().Format(time.RFC3339Nano)
		}
		lines[index] = fmt.Sprintf("%d %q %s %s", rating.Rating, rating.Reason, rating.StepID, ended)
	}
	return lines
}

// A record whose start Dispatch never got (a step that started before the release) counts no time:
// the issue's time starts at the earliest start the records hold.
func TestAnUnknownStartCountsNoTime(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	recordRuns(t, handler, "LEG", issue,
		run{"step", "planner", 1, nil, new(at(5)), "passed"},
		run{"step", "implementer", 2, new(at(10)), new(at(30)), "passed"},
		run{"merge", "merger", 0, nil, new(at(40)), "passed"},
	)
	closeIssue(t, handler, issue)

	finished := readPlans(t, handler, "LEG", "").Issues
	if len(finished) != 1 {
		t.Fatalf("the read lists %d issues, want %s", len(finished), issue)
	}
	read := finished[0]
	if read.StartedAt == nil || !read.StartedAt.Equal(at(10)) || read.EndedAt == nil || !read.EndedAt.Equal(at(40)) ||
		read.DurationSeconds == nil || *read.DurationSeconds != 1800 {
		t.Fatalf("the time is %v to %v, %v seconds, want %s to %s, 1800 seconds", read.StartedAt, read.EndedAt, read.DurationSeconds, at(10), at(40))
	}
	if read.Plan != nil || len(read.Ratings) != 0 {
		t.Fatalf("the issue with no plan node lists plan %s and ratings %s, want none of either", asJSON(t, read.Plan), asJSON(t, read.Ratings))
	}
}

// An issue whose only known start comes after its last end holds no time Dispatch can count: a run
// whose start Dispatch never got (one that started before the release) ended before a later run
// started, and the later run's end has not reached Dispatch. The read answers no duration, where
// the difference would be negative, and the start and the end as the records hold them.
func TestAStartAfterTheLastEndCountsNoTime(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	recordRuns(t, handler, "LEG", issue,
		run{"step", "planner", 1, nil, new(at(5)), "passed"},
		run{"step", "implementer", 2, new(at(10)), nil, ""},
	)
	closeIssue(t, handler, issue)

	finished := readPlans(t, handler, "LEG", "").Issues
	if len(finished) != 1 {
		t.Fatalf("the read lists %d issues, want %s", len(finished), issue)
	}
	read := finished[0]
	if read.DurationSeconds != nil {
		t.Fatalf("the issue's time is %d seconds, want no duration: its last end, %s, is before its first start, %s",
			*read.DurationSeconds, at(5), at(10))
	}
	if read.StartedAt == nil || !read.StartedAt.Equal(at(10)) || read.EndedAt == nil || !read.EndedAt.Equal(at(5)) {
		t.Fatalf("the time is %v to %v, want %s to %s, as the records hold them", read.StartedAt, read.EndedAt, at(10), at(5))
	}
}

// An issue whose last end is the moment of its first start holds a time of 0 seconds, which the
// read answers as 0 and not as no duration: a run whose start Dispatch never got ended at the
// moment a later run started, whose end has not reached Dispatch. The start and the end are the
// moment the records hold.
func TestAStartAtTheLastEndCountsZeroTime(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	recordRuns(t, handler, "LEG", issue,
		run{"step", "planner", 1, nil, new(at(10)), "passed"},
		run{"step", "implementer", 2, new(at(10)), nil, ""},
	)
	closeIssue(t, handler, issue)

	finished := readPlans(t, handler, "LEG", "").Issues
	if len(finished) != 1 {
		t.Fatalf("the read lists %d issues, want %s", len(finished), issue)
	}
	read := finished[0]
	if read.DurationSeconds == nil || *read.DurationSeconds != 0 {
		t.Fatalf("the issue's time is %v seconds, want 0: its last end and its first start are both %s", read.DurationSeconds, at(10))
	}
	if read.StartedAt == nil || !read.StartedAt.Equal(at(10)) || read.EndedAt == nil || !read.EndedAt.Equal(at(10)) {
		t.Fatalf("the time is %v to %v, want %s to %s, as the records hold them", read.StartedAt, read.EndedAt, at(10), at(10))
	}
}

// A finished issue Legion never ran is listed with no plan, no rating, no time and no failed step,
// each as null or empty on the wire, which is what Dispatch holds of it.
func TestTheReadOfAnIssueLegionNeverRanHoldsNoTime(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	closeIssue(t, handler, issue)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LEG/plans", nil, "alice")
	raw := response.Body.String()
	for _, want := range []string{`"plan":null`, `"ratings":[]`, `"started_at":null`, `"ended_at":null`, `"duration_seconds":null`, `"failed_steps":0`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("the read %s does not carry %s", raw, want)
		}
	}
}

// The plans read refuses a project Dispatch does not hold 404 PROJECT_NOT_FOUND, and a since or
// until that is no RFC 3339 time, or a since not before its until, an equal one too, since
// [since, until) then holds no moment, 400 INVALID_QUERY; a project with no finished issue answers
// an empty list.
func TestTheReadOfPlansRefusesAnUnknownProjectAndABadWindow(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestIssue(t, handler, "LEG", "A run of steps")

	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/NOPE/plans", nil, "alice")
	expectRefusal(t, "the plans of NOPE", missing, http.StatusNotFound, "PROJECT_NOT_FOUND")
	for _, query := range []string{
		"?since=yesterday", "?until=2026-13-01", "?since=2026-11-17T00:00:00Z&until=2026-11-16T00:00:00Z",
		"?since=2026-11-17T00:00:00Z&until=2026-11-17T00:00:00Z",
	} {
		refused := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LEG/plans"+query, nil, "alice")
		expectRefusal(t, "the plans of LEG"+query, refused, http.StatusBadRequest, "INVALID_QUERY")
	}
	if read := readPlans(t, handler, "LEG", "?since=2026-11-01T00:00:00Z"); read.Issues == nil || len(read.Issues) != 0 {
		t.Fatalf("the plans of a project with no finished issue are %s, want an empty list", asJSON(t, read))
	}
}

// readPlanKeys is the keys of the finished issues the plans read of project lists with query, in
// the read's order.
func readPlanKeys(t *testing.T, handler http.Handler, project, query string) []string {
	t.Helper()
	keys := []string{}
	for _, finished := range readPlans(t, handler, project, query).Issues {
		keys = append(keys, finished.Issue)
	}
	return keys
}

// A window bound finer than the microsecond PostgreSQL stores keeps its meaning: an issue closed at
// …00.123456 is before …00.123456001, so a since at that bound leaves it out and an until at it keeps
// it. pgx encodes a timestamptz by truncating it to the microsecond, which would move each
// comparison onto the close itself.
func TestTheReadOfPlansKeepsASubMicrosecondBoundsMeaning(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	closeIssue(t, handler, issue)
	closed := time.Date(2026, time.November, 3, 0, 0, 0, 123456000, time.UTC)
	if _, err := database.Pool.Exec(context.Background(), "update issues set closed_at = $2 where key = $1", issue, closed); err != nil {
		t.Fatalf("close %s at %s: %v", issue, closed, err)
	}
	const (
		stored = "2026-11-03T00:00:00.123456Z"
		after  = "2026-11-03T00:00:00.123456001Z"
		later  = "2026-11-03T00:00:00.123456999Z"
		before = "2026-11-03T00:00:00.123455999Z"
	)
	for _, window := range []struct {
		query string
		want  []string
	}{
		{"since=" + stored, []string{issue}},
		{"since=" + after, []string{}},
		{"until=" + stored, []string{}},
		{"until=" + after, []string{issue}},
		{"since=" + before + "&until=" + after, []string{issue}},
		{"since=" + after + "&until=" + later, []string{}},
	} {
		query := "?" + strings.ReplaceAll(window.query, ":", "%3A")
		if got := readPlanKeys(t, handler, "LEG", query); !slices.Equal(got, window.want) {
			t.Errorf("%s lists %v, want %v", query, got, window.want)
		}
	}
}

// Only the daemon of the issue's project writes its plan, as it writes its steps (403 STEP_WRITER);
// an issue Dispatch does not hold is 404 ISSUE_NOT_FOUND; a closed issue takes it; the write
// appends no event and answers the plan as it stands.
func TestAPlanWriteIsTheDaemonsAlone(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issues := createListedIssues(t, handler, "LEG", 2)
	issue, closed := issues[0], issues[1]
	path := "/api/v1/issues/" + issue + "/plans/"

	person := dispatchRequest(t, handler, http.MethodPut, path+uuid.NewString(), defaultPlanBody(nil, 1), "alice")
	expectRefusal(t, "a person's plan", person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	other := putPlanRecord(t, handler, issue, uuid.NewString(), defaultPlanBody(map[string]any{"kind": "session", "id": model.LegionDaemonSessionPrefix + "OTHER"}, 1))
	expectRefusal(t, "another daemon's plan", other, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG")
	missing := putPlanRecord(t, handler, "LEG-999", uuid.NewString(), defaultPlanBody(daemonSessionOf("LEG"), 1))
	expectRefusal(t, "a plan of LEG-999", missing, http.StatusNotFound, "ISSUE_NOT_FOUND", "LEG-999")

	before := issueEventTypes(t, handler, issue)
	id := uuid.NewString()
	written := putPlanRecordOK(t, handler, issue, id, defaultPlanBody(daemonSessionOf("LEG"), 1))
	if written.ID != id || written.Issue != issue || written.Generation != 1 || written.Source != "default" ||
		written.ArtifactID != nil || written.Version != nil || len(written.Tasks) != 1 || !slices.Equal(written.Tasks[0].Steps, defaultStepLine) {
		t.Fatalf("the plan write answers %s, want the default plan of %s", asJSON(t, written), issue)
	}
	if after := issueEventTypes(t, handler, issue); !slices.Equal(before, after) {
		t.Fatalf("the issue's events went from %v to %v, want no event for a plan write", before, after)
	}

	closeIssue(t, handler, closed)
	putPlanRecordOK(t, handler, closed, uuid.NewString(), defaultPlanBody(daemonSessionOf("LEG"), 1))
}

// A later version of an owner's plan replaces the plan of its run: written at version 1 and then
// at version 2 under the run's one id, the write answers version 2, and the read of the project
// lists the issue with version 2 and version 2's tasks. Version 1 arriving again after it, as a
// write that arrives late, is answered 200 with the plan as it stands and changes nothing.
func TestALaterVersionOfAnOwnersPlanReplacesThePlanOfItsRun(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	id, document := uuid.NewString(), uuid.NewString()
	owner := func(version int, tasks []model.PlanTask) map[string]any {
		return map[string]any{
			"actor": daemonSessionOf("LEG"), "generation": 1, "source": "owner",
			"artifact_id": document, "version": version, "tasks": tasks,
		}
	}
	first := []model.PlanTask{{ID: "api", Steps: []string{"implementer", "tester", "reviewer"}}}
	second := []model.PlanTask{{ID: "api", Steps: []string{"implementer", "reviewer"}}, {ID: "docs", Steps: []string{"implementer"}}}
	want := planRecordLine(&model.PlanRecord{
		ID: id, Issue: issue, Generation: 1, Source: "owner", ArtifactID: &document, Version: new(2), Tasks: second,
	})

	putPlanRecordOK(t, handler, issue, id, owner(1, first))
	if written := putPlanRecordOK(t, handler, issue, id, owner(2, second)); planRecordLine(&written) != want {
		t.Fatalf("version 2 of the plan answers\n%s\nwant\n%s", planRecordLine(&written), want)
	}
	if late := putPlanRecordOK(t, handler, issue, id, owner(1, first)); planRecordLine(&late) != want {
		t.Fatalf("version 1 written after version 2 answers\n%s\nwant the plan as it stands\n%s", planRecordLine(&late), want)
	}

	closeIssue(t, handler, issue)
	read := readPlans(t, handler, "LEG", "")
	if len(read.Issues) != 1 || read.Issues[0].Issue != issue {
		t.Fatalf("the read lists %d issues, want %s alone", len(read.Issues), issue)
	}
	if listed := planRecordLine(read.Issues[0].Plan); listed != want {
		t.Fatalf("the read lists %s with the plan\n%s\nwant\n%s", issue, listed, want)
	}
}

// planRecordLine is a plan as a reader receives it, on one line, so two compare whole and a
// difference prints whole.
func planRecordLine(plan *model.PlanRecord) string {
	if plan == nil {
		return "no plan"
	}
	artifact, version := "null", "null"
	if plan.ArtifactID != nil {
		artifact = *plan.ArtifactID
	}
	if plan.Version != nil {
		version = strconv.Itoa(*plan.Version)
	}
	return fmt.Sprintf("%s %s gen %d %s artifact %s version %s tasks %v",
		plan.ID, plan.Issue, plan.Generation, plan.Source, artifact, version, plan.Tasks)
}

// Each plan outside its form is refused 400 PLAN_INPUT naming the field, and a step whose role is
// longer than a step record's role, alone or before its key, naming the step and its role; a tasks
// that is no list at all the package's own 400 INVALID_JSON; none writes a plan. A lone half of a
// document reaches the store, which names it or the half left out: a default plan naming either
// half, and an owner's plan naming its document and no version. An id another issue's plan holds is
// 409 PLAN_OTHER_ISSUE.
func TestAPlanWriteRefusesItsBadValues(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issues := createListedIssues(t, handler, "LEG", 2)
	daemon := daemonSessionOf("LEG")
	plan := defaultPlanBody(daemon, 1)
	noSource := maps.Clone(plan)
	delete(noSource, "source")

	for _, row := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"a source that is no source", withFields(plan, map[string]any{"source": "mine"}), "source"},
		{"no source", noSource, "source"},
		{"no task", withFields(plan, map[string]any{"tasks": []any{}}), "tasks"},
		{"a task with no steps", withFields(plan, map[string]any{"tasks": []map[string]any{{"id": "default"}}}), "steps"},
		{"a task id that is no task id", withFields(plan, map[string]any{"tasks": []map[string]any{{"id": "Default", "steps": []string{"planner"}}}}), "task"},
		{"a step that is no step", withFields(plan, map[string]any{"tasks": []map[string]any{{"id": "default", "steps": []string{"Planner"}}}}), "steps"},
		{"a generation of 0", withFields(plan, map[string]any{"generation": 0}), "generation"},
		{"an owner's plan with no document", withFields(plan, map[string]any{"source": "owner", "version": 2}), "artifact_id"},
		{"an owner's plan with a document and no version", withFields(plan, map[string]any{"source": "owner", "artifact_id": uuid.NewString()}), "version is required"},
		{"a default plan naming a version alone", withFields(plan, map[string]any{"version": 2}), "version names"},
		{"a default plan naming a document alone", withFields(plan, map[string]any{"artifact_id": uuid.NewString()}), "artifact_id names"},
	} {
		t.Run(row.name, func(t *testing.T) {
			response := putPlanRecord(t, handler, issues[0], uuid.NewString(), row.body)
			expectRefusal(t, row.name, response, http.StatusBadRequest, codePlanInput, row.field)
		})
	}
	over := strings.Repeat("r", 33)
	for _, step := range []string{over, over + ":owner"} {
		response := putPlanRecord(t, handler, issues[0], uuid.NewString(),
			withFields(plan, map[string]any{"tasks": []map[string]any{{"id": "default", "steps": []string{"planner", step}}}}))
		expectRefusal(t, "a step of "+step, response, http.StatusBadRequest, codePlanInput, strconv.Quote(step), strconv.Quote(over))
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/issues/"+issues[0]+"/plans/"+uuid.NewString(),
		bytes.NewReader([]byte(`{"actor":{"kind":"session","id":"legion-daemon:LEG"},"generation":1,"source":"default","tasks":"planner"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	expectRefusal(t, "a tasks that is no list", response, http.StatusBadRequest, "INVALID_JSON")
	if holdsPlan(t, handler, issues[0]) {
		t.Fatalf("the refused plans left a plan of %s, want none", issues[0])
	}

	id := uuid.NewString()
	putPlanRecordOK(t, handler, issues[0], id, plan)
	other := putPlanRecord(t, handler, issues[1], id, plan)
	expectRefusal(t, "the plan id of "+issues[0]+" on "+issues[1], other, http.StatusConflict, codePlanOtherIssue, issues[0], issues[1])
}

// holdsPlan closes issue, which the read of a project lists only once it is finished, and answers
// whether that read lists a plan for it: the plans of an issue a test wrote are seen only so. A read
// that does not list the finished issue at all fails the test, since an answer of false would then
// say nothing about its plan.
func holdsPlan(t *testing.T, handler http.Handler, issue string) bool {
	t.Helper()
	closeIssue(t, handler, issue)
	project, _, _ := strings.Cut(issue, "-")
	read := readPlans(t, handler, project, "")
	for _, finished := range read.Issues {
		if finished.Issue == issue {
			return finished.Plan != nil
		}
	}
	t.Fatalf("the read of %s's plans does not list %s, which was just closed: %s", project, issue, asJSON(t, read.Issues))
	return false
}
