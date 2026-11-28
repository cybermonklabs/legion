package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Who writes an issue's plan and the rating of its retro (PUT /api/v1/issues/{key}/plans/{id},
// PUT /api/v1/issues/{key}/steps/{id}/rating): the session of the daemon of the issue's project,
// or the session of another project's daemon that the request's broker token proves
// (isIssueDaemon; steps_test.go holds that case). The variant's write is held to the same writer
// here where a person carries the daemon's id. plans_test.go has what each write takes and the
// read of a project's finished issues.

// Under a credential that proves no session, only the daemon of the issue's project rates a retro,
// as only it writes the retro's record: a person, an agent's session and the daemon of another
// project, each sending the rating the daemon's own write lands with, are refused 403 STEP_WRITER,
// whether the retro's record exists or not, and the retro the daemon recorded stays unrated; the
// daemon's rating of it lands, which is the control.
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
		expectRefusal(t, "a person's rating of "+id, person, http.StatusForbidden, contracts.StepWriterCode, "legion-daemon:LEG", issue)
		for _, session := range []string{legionDaemonSessionPrefix + "OTHER", "ses_agent"} {
			refused := putRating(t, handler, issue, id, withFields(retroIdentity(map[string]any{"kind": "session", "id": session}, 1), rating))
			expectRefusal(t, "a rating of "+id+" as "+session, refused, http.StatusForbidden, contracts.StepWriterCode, "legion-daemon:LEG", issue)
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
// whose login is legion-daemon:LEG is refused the plan write, the rating write and the experiment
// write 403 STEP_WRITER with nothing written, where the session of that id writes all three, which
// is the control.
func TestTheDaemonsIDIsNoWriterButAsASession(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{identity: exactLogin{}})
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	writer := legionDaemonSessionPrefix + "LEG"
	plan, retro, run := uuid.NewString(), uuid.NewString(), uuid.NewString()
	rating := withFields(retroIdentity(nil, 1), map[string]any{"rating": 2, "rating_reason": "The plan was thin."})
	variant := withFields(stepIdentity(nil), map[string]any{"experiment": "planning", "variant": "b"})

	planned := dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/plans/"+plan, defaultPlanBody(nil, 1), writer)
	expectRefusal(t, "a plan of a person named "+writer, planned, http.StatusForbidden, contracts.StepWriterCode, writer, issue)
	rated := dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+retro+"/rating", rating, writer)
	expectRefusal(t, "a rating of a person named "+writer, rated, http.StatusForbidden, contracts.StepWriterCode, writer, issue)
	drawn := dispatchRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+run+"/experiment", variant, writer)
	expectRefusal(t, "a variant of a person named "+writer, drawn, http.StatusForbidden, contracts.StepWriterCode, writer, issue)
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}
	if holdsPlan(t, handler, issue) {
		t.Fatalf("the refused writes left a plan of %s, want none", issue)
	}

	putPlanRecordOK(t, handler, issue, plan, defaultPlanBody(daemonSessionOf("LEG"), 1))
	putRatingOK(t, handler, issue, retro, withFields(rating, map[string]any{"actor": daemonSessionOf("LEG")}))
	if written := putExperiment(t, handler, issue, run, withFields(variant, map[string]any{"actor": daemonSessionOf("LEG")})); written.Code != http.StatusOK {
		t.Fatalf("the daemon's variant of %s: status=%d body=%s, want 200", run, written.Code, written.Body.String())
	}
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

// Under a credential that proves no session, only the daemon of the issue's project writes its
// plan, as it writes its steps (403 STEP_WRITER); an issue Dispatch does not hold is 404
// ISSUE_NOT_FOUND; a closed issue takes it; the write appends no event and answers the plan as it
// stands.
func TestAPlanWriteIsTheDaemonsAlone(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issues := createListedIssues(t, handler, "LEG", 2)
	issue, closed := issues[0], issues[1]
	path := "/api/v1/issues/" + issue + "/plans/"

	person := dispatchRequest(t, handler, http.MethodPut, path+uuid.NewString(), defaultPlanBody(nil, 1), "alice")
	expectRefusal(t, "a person's plan", person, http.StatusForbidden, contracts.StepWriterCode, "legion-daemon:LEG", issue)
	other := putPlanRecord(t, handler, issue, uuid.NewString(), defaultPlanBody(map[string]any{"kind": "session", "id": legionDaemonSessionPrefix + "OTHER"}, 1))
	expectRefusal(t, "another daemon's plan", other, http.StatusForbidden, contracts.StepWriterCode, "legion-daemon:LEG")
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
