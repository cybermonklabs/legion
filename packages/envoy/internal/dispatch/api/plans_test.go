package api

import (
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
			"actor": daemonSession(project), "task": "default", "step": each.place, "kind": each.kind,
			"role": each.role, "generation": 1,
		}
		if each.started != nil {
			putStepOK(t, handler, issue, ids[index], with(body, map[string]any{"started_at": *each.started}))
		}
		if each.ended != nil {
			end := with(body, map[string]any{"result": each.result, "ended_at": *each.ended})
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
	putRatingOK(t, handler, issue, id, with(retroIdentity(daemonSession(project), generation),
		map[string]any{"rating": rating, "rating_reason": reason}))
}

// The read of a project lists each of its issues that is done, newest close first, each with the
// default plan, the rating of its retro with the reason, the time from its first start to its last
// end, and its count of failed steps; the issue still in progress is not listed.
func TestTheReadOfAProjectListsEachFinishedIssue(t *testing.T) {
	handler := newTestHandler(t)
	issues := stepIssues(t, handler, "LEG", 3)
	reviewed, fitted, running := issues[0], issues[1], issues[2]
	daemon := daemonSession("LEG")

	putPlanRecordOK(t, handler, reviewed, uuid.NewString(), defaultPlanBody(daemon, 1))
	runs := recordRuns(t, handler, "LEG", reviewed,
		run{"step", "planner", 1, new(at(0)), new(at(5)), "passed"},
		run{"step", "implementer", 2, new(at(5)), new(at(20)), "passed"},
		run{"step", "reviewer", 4, new(at(20)), new(at(25)), "failed"},
		run{"step", "implementer", 2, new(at(25)), new(at(35)), "passed"},
		run{"step", "reviewer", 4, new(at(35)), new(at(40)), "passed"},
		run{"retro", "implementer", 0, new(at(40)), new(at(42)), "passed"},
		run{"merge", "merger", 0, nil, new(at(50)), "passed"},
	)
	rateRetro(t, handler, "LEG", reviewed, runs[5], 1, 4, "The tester found the bug.")
	closeIssue(t, handler, reviewed)

	putPlanRecordOK(t, handler, fitted, uuid.NewString(), defaultPlanBody(daemon, 1))
	runs = recordRuns(t, handler, "LEG", fitted,
		run{"step", "planner", 1, new(at(100)), new(at(110)), "passed"},
		run{"step", "implementer", 2, new(at(110)), new(at(130)), "passed"},
		run{"retro", "implementer", 0, new(at(140)), new(at(141)), "passed"},
		run{"merge", "merger", 0, nil, new(at(160)), "passed"},
	)
	rateRetro(t, handler, "LEG", fitted, runs[2], 1, 5, "The plan fit the work.")
	closeIssue(t, handler, fitted)

	putPlanRecordOK(t, handler, running, uuid.NewString(), defaultPlanBody(daemon, 1))
	recordRuns(t, handler, "LEG", running, run{"step", "reviewer", 4, new(at(200)), new(at(205)), "failed"})

	read := readPlans(t, handler, "LEG", "")
	if read.Project != "LEG" || len(read.Issues) != 2 || read.Issues[0].Issue != fitted || read.Issues[1].Issue != reviewed {
		t.Fatalf("the read lists %+v, want %s then %s, the newest close first", read, fitted, reviewed)
	}
	for _, want := range []struct {
		issue          model.FinishedIssue
		rating         int
		reason         string
		duration       int64
		failed         int
		started, ended time.Time
	}{
		{read.Issues[0], 5, "The plan fit the work.", 3600, 0, at(100), at(160)},
		{read.Issues[1], 4, "The tester found the bug.", 3000, 1, at(0), at(50)},
	} {
		issue := want.issue
		if issue.Plan == nil || issue.Plan.Source != "default" || issue.Plan.ArtifactID != nil || issue.Plan.Version != nil ||
			len(issue.Plan.Tasks) != 1 || issue.Plan.Tasks[0].ID != "default" || !slices.Equal(issue.Plan.Tasks[0].Steps, defaultStepLine) {
			t.Fatalf("%s's plan is %+v, want the default plan of one task default with the default steps", issue.Issue, issue.Plan)
		}
		if len(issue.Ratings) != 1 || issue.Ratings[0].Rating != want.rating || issue.Ratings[0].Reason != want.reason ||
			issue.Ratings[0].StepID == "" || issue.Ratings[0].EndedAt == nil {
			t.Fatalf("%s's ratings are %+v, want one, %d %q, with its record and end", issue.Issue, issue.Ratings, want.rating, want.reason)
		}
		if issue.DurationSeconds == nil || *issue.DurationSeconds != want.duration ||
			issue.StartedAt == nil || !issue.StartedAt.Equal(want.started) || issue.EndedAt == nil || !issue.EndedAt.Equal(want.ended) {
			t.Fatalf("%s's time is %v to %v, %v seconds, want %s to %s, %d seconds", issue.Issue, issue.StartedAt, issue.EndedAt,
				issue.DurationSeconds, want.started, want.ended, want.duration)
		}
		if issue.FailedSteps != want.failed {
			t.Fatalf("%s counts %d failed steps, want %d", issue.Issue, issue.FailedSteps, want.failed)
		}
		if issue.Title == "" || issue.ClosedAt.IsZero() {
			t.Fatalf("%s is listed with title %q closed %s, want its title and its close", issue.Issue, issue.Title, issue.ClosedAt)
		}
	}
}

// A rating is a retro's: the rating route refuses a record of kind step or merge, and a record the
// node holds as a step, 400 STEP_INPUT naming the rating; the same rating of a retro lands, and the
// retro's record carries it to a reader. The step write itself takes no rating: a body that names
// one is written without it.
func TestARatingBelongsToARetroRecordAlone(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
	rating := map[string]any{"rating": 3, "rating_reason": "The plan was late."}

	step := putRating(t, handler, issue, uuid.NewString(), with(stepIdentity(daemon), rating))
	requireStepRefusal(t, "a rating of a step", step, http.StatusBadRequest, codeStepInput, "rating")
	merge := putRating(t, handler, issue, uuid.NewString(), with(retroIdentity(daemon, 1), with(rating, map[string]any{"kind": "merge"})))
	requireStepRefusal(t, "a rating of a merge", merge, http.StatusBadRequest, codeStepInput, "rating")
	held := uuid.NewString()
	putStepOK(t, handler, issue, held, implementerStart(daemon))
	asRetro := putRating(t, handler, issue, held, with(retroIdentity(daemon, 1), rating))
	requireStepRefusal(t, "a rating of a record held as a step", asRetro, http.StatusBadRequest, codeStepInput, "rating")

	unrated := uuid.NewString()
	written := putStepOK(t, handler, issue, unrated, with(retroIdentity(daemon, 1), with(rating, map[string]any{"started_at": stepStart})))
	if written.Rating != nil || written.RatingReason != nil {
		t.Fatalf("the step write answers rating %v %v, want none: a rating has a route of its own", written.Rating, written.RatingReason)
	}

	retro := uuid.NewString()
	rated := putRatingOK(t, handler, issue, retro, with(retroIdentity(daemon, 1), rating))
	if rated.Kind != "retro" || rated.Rating == nil || *rated.Rating != 3 || rated.RatingReason == nil || *rated.RatingReason != "The plan was late." {
		t.Fatalf("the rating answers %s, want the retro rated 3 with its reason", stepJSON(t, rated))
	}
	for _, record := range stepsOf(t, handler, issue) {
		switch record.ID {
		case retro:
			if record.Rating == nil || *record.Rating != 3 || record.RatingReason == nil || *record.RatingReason != "The plan was late." {
				t.Fatalf("the read lists the retro as %s, want its rating and reason", stepJSON(t, record))
			}
		default:
			if record.Rating != nil || record.RatingReason != nil {
				t.Fatalf("the read lists %s with a rating, want only the rated retro to carry one", stepJSON(t, record))
			}
		}
	}
}

// Each rating outside its form is refused 400 STEP_INPUT naming its field, and writes nothing: the
// rating 1 to 5 and the reason not blank and at most 2,000 code points, both named. A reason of
// exactly 2,000 code points is taken, which is the control of the bound.
func TestARatingWriteRefusesItsBadValues(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	retro := retroIdentity(daemonSession("LEG"), 1)
	noReason := with(retro, map[string]any{"rating": 3})
	noRating := with(retro, map[string]any{"rating_reason": "Why."})

	for _, row := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"a rating of 0", with(retro, map[string]any{"rating": 0, "rating_reason": "Why."}), "rating"},
		{"a rating of 6", with(retro, map[string]any{"rating": 6, "rating_reason": "Why."}), "rating"},
		{"no rating", noRating, "rating"},
		{"no reason", noReason, "rating_reason"},
		{"a blank reason", with(retro, map[string]any{"rating": 3, "rating_reason": " \n "}), "rating_reason"},
		{"a reason of 2,001 code points", with(retro, map[string]any{"rating": 3, "rating_reason": strings.Repeat("é", 2001)}), "rating_reason"},
	} {
		t.Run(row.name, func(t *testing.T) {
			requireStepRefusal(t, row.name, putRating(t, handler, issue, uuid.NewString(), row.body), http.StatusBadRequest, codeStepInput, row.field)
		})
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused ratings left %d records, want none", len(steps))
	}
	longest := putRatingOK(t, handler, issue, uuid.NewString(), with(retro, map[string]any{"rating": 1, "rating_reason": strings.Repeat("é", 2000)}))
	if longest.RatingReason == nil || len([]rune(*longest.RatingReason)) != 2000 {
		t.Fatalf("a reason of 2,000 code points answers %s, want it whole", stepJSON(t, longest))
	}
}

// A rating reason holding a NUL character (U+0000), which PostgreSQL's text cannot hold, is refused
// by the body's decoder before the route reads it (decodeJSON, unstorableJSON): 400 NUL_CHARACTER
// naming rating_reason and where in it the NUL stands, with nothing written. The same rating with
// the NUL taken out lands, which is the control.
func TestARatingReasonHoldingANulIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	retro := retroIdentity(daemonSession("LEG"), 1)
	id := uuid.NewString()

	refused := putRating(t, handler, issue, id, with(retro, map[string]any{"rating": 3, "rating_reason": "The plan\x00 was late."}))
	requireStepRefusal(t, "a reason holding a NUL", refused, http.StatusBadRequest, "NUL_CHARACTER",
		"rating_reason holds a NUL character (U+0000) at character 9, which Dispatch cannot store")
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused rating left %d records, want none", len(steps))
	}
	if rated := putRatingOK(t, handler, issue, id, with(retro, map[string]any{"rating": 3, "rating_reason": "The plan was late."})); rated.RatingReason == nil || *rated.RatingReason != "The plan was late." {
		t.Fatalf("the rating with no NUL answers %s, want its reason", stepJSON(t, rated))
	}
}

// A rating is written once, and the retro's end that lands after it keeps it: the daemon's two rows
// for one retro land in either order.
func TestARatingIsWrittenOnceInEitherOrder(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
	id := uuid.NewString()

	rateRetro(t, handler, "LEG", issue, id, 1, 2, "The plan skipped the tests.")
	again := putRatingOK(t, handler, issue, id, with(retroIdentity(daemon, 1), map[string]any{"rating": 5, "rating_reason": "Changed my mind."}))
	if again.Rating == nil || *again.Rating != 2 || *again.RatingReason != "The plan skipped the tests." {
		t.Fatalf("a second rating answers %s, want the first, 2", stepJSON(t, again))
	}
	ended := putStepOK(t, handler, issue, id, with(retroIdentity(daemon, 1), map[string]any{"result": "passed", "ended_at": at(3)}))
	if ended.Result != "passed" || ended.Rating == nil || *ended.Rating != 2 {
		t.Fatalf("the retro's end after its rating answers %s, want the end beside the rating", stepJSON(t, ended))
	}
}

// An issue that ran twice has two retros, and the read lists both ratings, in the order they ran.
func TestAnIssueWithTwoRetrosHasTwoRatings(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
	putPlanRecordOK(t, handler, issue, uuid.NewString(), defaultPlanBody(daemon, 1))
	putPlanRecordOK(t, handler, issue, uuid.NewString(), defaultPlanBody(daemon, 2))
	for generation, retro := range map[int]struct {
		started time.Time
		rating  int
		reason  string
	}{1: {at(10), 3, "The plan was late."}, 2: {at(100), 4, "The second plan fit."}} {
		id := uuid.NewString()
		putStepOK(t, handler, issue, id, with(retroIdentity(daemon, generation), map[string]any{"started_at": retro.started}))
		putStepOK(t, handler, issue, id, with(retroIdentity(daemon, generation), map[string]any{"result": "passed", "ended_at": retro.started.Add(time.Minute)}))
		rateRetro(t, handler, "LEG", issue, id, generation, retro.rating, retro.reason)
	}
	closeIssue(t, handler, issue)

	read := readPlans(t, handler, "LEG", "")
	if len(read.Issues) != 1 {
		t.Fatalf("the read lists %d issues, want %s", len(read.Issues), issue)
	}
	var got []string
	for _, rating := range read.Issues[0].Ratings {
		got = append(got, rating.Reason)
	}
	if want := []string{"The plan was late.", "The second plan fit."}; !slices.Equal(got, want) {
		t.Fatalf("the ratings are %v, want %v", got, want)
	}
}

// A record whose start Dispatch never got (a step that started before the release) counts no time:
// the issue's time starts at the earliest start the records hold.
func TestAnUnknownStartCountsNoTime(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
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
		t.Fatalf("the issue with no plan node lists plan %+v and ratings %+v, want none of either", read.Plan, read.Ratings)
	}
}

// A finished issue Legion never ran is listed with no plan, no rating, no time and no failed step,
// each as null or empty on the wire, which is what Dispatch holds of it.
func TestTheReadOfAnIssueLegionNeverRanHoldsNoTime(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
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
// until that is no RFC 3339 time, or a since not before its until, 400 INVALID_QUERY; a project
// with no finished issue answers an empty list.
func TestTheReadOfPlansRefusesAnUnknownProjectAndABadWindow(t *testing.T) {
	handler := newTestHandler(t)
	stepIssues(t, handler, "LEG", 1)

	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/NOPE/plans", nil, "alice")
	requireStepRefusal(t, "the plans of NOPE", missing, http.StatusNotFound, "PROJECT_NOT_FOUND")
	for _, query := range []string{"?since=yesterday", "?until=2026-13-01", "?since=2026-11-17T00:00:00Z&until=2026-11-16T00:00:00Z"} {
		refused := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LEG/plans"+query, nil, "alice")
		requireStepRefusal(t, "the plans of LEG"+query, refused, http.StatusBadRequest, "INVALID_QUERY")
	}
	if read := readPlans(t, handler, "LEG", "?since=2026-11-01T00:00:00Z"); read.Issues == nil || len(read.Issues) != 0 {
		t.Fatalf("the plans of a project with no finished issue are %+v, want an empty list", read)
	}
}

// Only the daemon of the issue's project writes its plan, as it writes its steps (403 STEP_WRITER);
// an issue Dispatch does not hold is 404 ISSUE_NOT_FOUND; a closed issue takes it; the write
// appends no event and answers the plan as it stands.
func TestAPlanWriteIsTheDaemonsAlone(t *testing.T) {
	handler := newTestHandler(t)
	issues := stepIssues(t, handler, "LEG", 2)
	issue, closed := issues[0], issues[1]
	path := "/api/v1/issues/" + issue + "/plans/"

	person := dispatchRequest(t, handler, http.MethodPut, path+uuid.NewString(), defaultPlanBody(nil, 1), "alice")
	requireStepRefusal(t, "a person's plan", person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	other := putPlanRecord(t, handler, issue, uuid.NewString(), defaultPlanBody(map[string]any{"kind": "session", "id": legionDaemonSessionPrefix + "OTHER"}, 1))
	requireStepRefusal(t, "another daemon's plan", other, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG")
	missing := putPlanRecord(t, handler, "LEG-999", uuid.NewString(), defaultPlanBody(daemonSession("LEG"), 1))
	requireStepRefusal(t, "a plan of LEG-999", missing, http.StatusNotFound, "ISSUE_NOT_FOUND", "LEG-999")

	before := issueEventTypes(t, handler, issue)
	id := uuid.NewString()
	written := putPlanRecordOK(t, handler, issue, id, defaultPlanBody(daemonSession("LEG"), 1))
	if written.ID != id || written.Issue != issue || written.Generation != 1 || written.Source != "default" ||
		written.ArtifactID != nil || written.Version != nil || len(written.Tasks) != 1 || !slices.Equal(written.Tasks[0].Steps, defaultStepLine) {
		t.Fatalf("the plan write answers %+v, want the default plan of %s", written, issue)
	}
	if after := issueEventTypes(t, handler, issue); !slices.Equal(before, after) {
		t.Fatalf("the issue's events went from %v to %v, want no event for a plan write", before, after)
	}

	closeIssue(t, handler, closed)
	putPlanRecordOK(t, handler, closed, uuid.NewString(), defaultPlanBody(daemonSession("LEG"), 1))
}

// Each plan outside its form is refused 400 PLAN_INPUT naming the field, and a tasks that is no
// list at all the package's own 400 INVALID_JSON; none writes a plan. An id another issue's plan
// holds is 409 PLAN_OTHER_ISSUE.
func TestAPlanWriteRefusesItsBadValues(t *testing.T) {
	handler := newTestHandler(t)
	issues := stepIssues(t, handler, "LEG", 2)
	daemon := daemonSession("LEG")
	plan := defaultPlanBody(daemon, 1)
	noSource := maps.Clone(plan)
	delete(noSource, "source")

	for _, row := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"a source that is no source", with(plan, map[string]any{"source": "mine"}), "source"},
		{"no source", noSource, "source"},
		{"no task", with(plan, map[string]any{"tasks": []any{}}), "tasks"},
		{"a task with no steps", with(plan, map[string]any{"tasks": []map[string]any{{"id": "default"}}}), "steps"},
		{"a task id that is no task id", with(plan, map[string]any{"tasks": []map[string]any{{"id": "Default", "steps": []string{"planner"}}}}), "task"},
		{"a step that is no step", with(plan, map[string]any{"tasks": []map[string]any{{"id": "default", "steps": []string{"Planner"}}}}), "steps"},
		{"a generation of 0", with(plan, map[string]any{"generation": 0}), "generation"},
		{"an owner's plan with no document", with(plan, map[string]any{"source": "owner", "version": 2}), "artifact_id"},
	} {
		t.Run(row.name, func(t *testing.T) {
			response := putPlanRecord(t, handler, issues[0], uuid.NewString(), row.body)
			requireStepRefusal(t, row.name, response, http.StatusBadRequest, codePlanInput, row.field)
		})
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/issues/"+issues[0]+"/plans/"+uuid.NewString(),
		bytes.NewReader([]byte(`{"actor":{"kind":"session","id":"legion-daemon:LEG"},"generation":1,"source":"default","tasks":"planner"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requireStepRefusal(t, "a tasks that is no list", response, http.StatusBadRequest, "INVALID_JSON")
	if read := readPlansOf(t, handler, issues[0]); read != 0 {
		t.Fatalf("the refused plans left %d plan nodes, want none", read)
	}

	id := uuid.NewString()
	putPlanRecordOK(t, handler, issues[0], id, plan)
	other := putPlanRecord(t, handler, issues[1], id, plan)
	requireStepRefusal(t, "the plan id of "+issues[0]+" on "+issues[1], other, http.StatusConflict, codePlanOtherIssue, issues[0], issues[1])
}

// readPlansOf counts the plan nodes of issue, read from the read of its finished project: the issue
// is closed for the count and its plan read back.
func readPlansOf(t *testing.T, handler http.Handler, issue string) int {
	t.Helper()
	closeIssue(t, handler, issue)
	project, _, _ := strings.Cut(issue, "-")
	for _, finished := range readPlans(t, handler, project, "").Issues {
		if finished.Issue == issue && finished.Plan != nil {
			return 1
		}
	}
	return 0
}
