package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordResultAsSession records one result as a session rather than as a person: the shared
// bearer token proves no session, so the session names itself in actor, and check_results.session
// then holds it. That session is the first of the three callers a retraction admits.
func recordResultAsSession(t *testing.T, handler http.Handler, issue, session string, body map[string]any) model.CheckResult {
	t.Helper()
	body["actor"] = map[string]any{"kind": "session", "id": session}
	response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/results", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("record a result on %s as %s: status=%d body=%s", issue, session, response.Code, response.Body.String())
	}
	return decodeBody[recordedResult](t, response).Result
}

// retractAsSession retracts one result as a session.
func retractAsSession(t *testing.T, handler http.Handler, resultID, session, reason string) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPost, "/api/v1/results/"+resultID+"/retract", map[string]any{
		"actor": map[string]any{"kind": "session", "id": session}, "reason": reason,
	})
}

// retractAsPerson retracts one result as a signed-in person, whose cookie carries them as its
// owner and holds the person scope.
func retractAsPerson(t *testing.T, handler http.Handler, resultID, reason string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/results/"+resultID+"/retract",
		map[string]any{"reason": reason}, "alice")
}

// claimIssueFor writes one session's claim on an issue as the claim route's write leaves it.
// The route itself reads the holder's liveness from Envoy, which these tests run without.
func claimIssueFor(t *testing.T, database *store.Store, key, session string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set claimed_by = jsonb_build_object('kind', 'session', 'id', $2::text),
			claimed_at = now()
		where key = $1
	`, key, session); err != nil {
		t.Fatalf("claim %s for %s: %v", key, session, err)
	}
}

// storedRetraction is the retraction row of one result, and whether one stands against it.
func storedRetraction(t *testing.T, database *store.Store, resultID string) (model.CheckResultRetraction, bool) {
	t.Helper()
	var retraction model.CheckResultRetraction
	var actorJSON []byte
	err := database.Pool.QueryRow(context.Background(), `
		select result_id::text, reason, session, actor, created_at, event_seq
		from check_result_retractions where result_id = $1
	`, resultID).Scan(&retraction.ResultID, &retraction.Reason, &retraction.Session,
		&actorJSON, &retraction.CreatedAt, &retraction.EventSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.CheckResultRetraction{}, false
	}
	if err != nil {
		t.Fatalf("read the retraction of %s: %v", resultID, err)
	}
	if err := json.Unmarshal(actorJSON, &retraction.Actor); err != nil {
		t.Fatalf("read the actor of %s's retraction: %v", resultID, err)
	}
	return retraction, true
}

// retractionEventsOn counts the result.retracted events appended on one issue.
func retractionEventsOn(t *testing.T, database *store.Store, key string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and type = 'result.retracted'
	`, key).Scan(&count); err != nil {
		t.Fatalf("count %s's result.retracted events: %v", key, err)
	}
	return count
}

// Three callers retract a result, and each writes a record of its own: the session that
// recorded it, a session holding the Dispatch claim of the issue the result was recorded on,
// and a person. Every retraction goes on the check's issue, as the record does, and leaves the
// result itself exactly as it was — the results read still lists all three with their verdicts,
// which is what makes both records readable.
func TestResultIsRetractedByItsRecorderByTheClaimantAndByAPerson(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	parent := createInteractionIssue(t, handler, "RETA", "Parent", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", twoChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RETA", "title": "Child", "parent": parent.Key, "force": true}, "alice"))

	// The recording session's own result.
	own := recordResultAsSession(t, handler, parent.Key, "ses-recorder", resultBody("k1", "pass", headCommit, nil))
	byRecorder := retractAsSession(t, handler, own.ID, "ses-recorder", "The run was against the wrong commit.")
	if byRecorder.Code != http.StatusCreated {
		t.Fatalf("the recording session retracts its own result: status=%d body=%s", byRecorder.Code, byRecorder.Body.String())
	}
	answered := decodeBody[model.CheckResultRetraction](t, byRecorder)
	if answered.ResultID != own.ID || answered.Reason != "The run was against the wrong commit." ||
		answered.Session != "ses-recorder" || answered.Actor.ID != "ses-recorder" {
		t.Fatalf("the retraction = %+v, want ses-recorder's against %s with its reason", answered, own.ID)
	}

	// A session holding the claim of the issue the result was recorded on, which is the child
	// here: the result stands against the parent's check and the child's session wrote it.
	fromChild := recordResultAsSession(t, handler, child.Key, "ses-worker", resultBody("k1", "fail", newHeadCommit, map[string]any{"failed": 1}))
	if fromChild.CheckIssueKey != parent.Key {
		t.Fatalf("the child's result stands against %s, want the parent's check on %s", fromChild.CheckIssueKey, parent.Key)
	}
	claimIssueFor(t, database, child.Key, "ses-claimant")
	byClaimant := retractAsSession(t, handler, fromChild.ID, "ses-claimant", "The tester ran the wrong suite.")
	if byClaimant.Code != http.StatusCreated {
		t.Fatalf("the claimant of the recording issue retracts: status=%d body=%s", byClaimant.Code, byClaimant.Body.String())
	}

	// A person, whose own credential carries them as its owner.
	third := recordResultAsSession(t, handler, parent.Key, "ses-worker", resultBody("k1", "pass", mergeCommit, nil))
	byPerson := retractAsPerson(t, handler, third.ID, "The check's words were misread.")
	if byPerson.Code != http.StatusCreated {
		t.Fatalf("a person retracts: status=%d body=%s", byPerson.Code, byPerson.Body.String())
	}
	person := decodeBody[model.CheckResultRetraction](t, byPerson)
	if person.Session != "" || person.Actor.Kind != "user" || person.Actor.ID != "alice" {
		t.Fatalf("a person's retraction = %+v, want alice and no session", person)
	}

	// Every retraction is a row of its own, and all three events are on the check's issue, as
	// the three result.recorded events are.
	for _, id := range []string{own.ID, fromChild.ID, third.ID} {
		if _, stands := storedRetraction(t, database, id); !stands {
			t.Fatalf("result %s carries no retraction row", id)
		}
	}
	if events := retractionEventsOn(t, database, parent.Key); events != 3 {
		t.Fatalf("the check's issue holds %d result.retracted events, want three", events)
	}
	if events := retractionEventsOn(t, database, child.Key); events != 0 {
		t.Fatalf("the recording issue holds %d result.retracted events, want none", events)
	}

	// Nothing about the results themselves moved: all three stand under the check with the
	// verdicts they were recorded with.
	results := checkOf(t, readResults(t, handler, parent.Key), "k1").Results
	verdicts := map[string]string{}
	for _, result := range results {
		verdicts[result.ID] = result.Verdict
	}
	if len(results) != 3 || verdicts[own.ID] != "pass" || verdicts[fromChild.ID] != "fail" || verdicts[third.ID] != "pass" {
		t.Fatalf("k1's results = %+v, want all three unchanged", results)
	}
}

// Every other caller is refused, and the claim that admits one is the recording issue's, not
// the check's issue's: a session holding the parent's claim is no more admitted than a stranger
// is. A refusal writes nothing at all. A result a retraction already stands against is refused
// too, so one result carries one retraction.
func TestResultRetractionRefusesEveryOtherCallerAndASecondRetraction(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	parent := createInteractionIssue(t, handler, "RETB", "Parent", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+parent.Key, "verification.md", twoChecks)
	child := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues",
		map[string]any{"project": "RETB", "title": "Child", "parent": parent.Key, "force": true}, "alice"))
	result := recordResultAsSession(t, handler, child.Key, "ses-recorder", resultBody("k1", "pass", headCommit, nil))

	// The claim of the check's issue admits nobody: the result was recorded on the child.
	claimIssueFor(t, database, parent.Key, "ses-parent-claimant")
	refusals := map[string]*httptest.ResponseRecorder{
		"the claimant of the check's issue": retractAsSession(t, handler, result.ID, "ses-parent-claimant", "Not mine to judge."),
		"a session that recorded nothing":   retractAsSession(t, handler, result.ID, "ses-stranger", "Not mine to judge."),
	}
	for who, refused := range refusals {
		code, text := refusalOf(t, refused)
		if refused.Code != http.StatusForbidden || code != "RETRACTION_FORBIDDEN" {
			t.Fatalf("%s: status=%d body=%s, want 403 RETRACTION_FORBIDDEN", who, refused.Code, refused.Body.String())
		}
		if !strings.Contains(text, child.Key) || !strings.Contains(text, "personal token") {
			t.Fatalf("%s is told %q, want the claim of %s and the personal token named", who, text, child.Key)
		}
	}
	if _, stands := storedRetraction(t, database, result.ID); stands {
		t.Fatalf("a refused retraction wrote a row for %s", result.ID)
	}
	if events := retractionEventsOn(t, database, parent.Key); events != 0 {
		t.Fatalf("a refused retraction appended %d events", events)
	}

	// A blank reason, and an id no result has, are refused before anything is judged.
	blank := retractAsSession(t, handler, result.ID, "ses-recorder", "   ")
	if code, _ := refusalOf(t, blank); blank.Code != http.StatusBadRequest || code != "RETRACTION_INPUT" {
		t.Fatalf("a blank reason: status=%d body=%s", blank.Code, blank.Body.String())
	}
	missing := retractAsPerson(t, handler, uuid.NewString(), "Nothing to retract.")
	if code, _ := refusalOf(t, missing); missing.Code != http.StatusNotFound || code != "RESULT_NOT_FOUND" {
		t.Fatalf("an id no result has: status=%d body=%s", missing.Code, missing.Body.String())
	}

	// The recording session retracts it once, and the second retraction is refused whoever
	// makes it.
	if first := retractAsSession(t, handler, result.ID, "ses-recorder", "The suite was stale."); first.Code != http.StatusCreated {
		t.Fatalf("the recording session retracts: status=%d body=%s", first.Code, first.Body.String())
	}
	second := retractAsPerson(t, handler, result.ID, "Retracting it again.")
	if code, _ := refusalOf(t, second); second.Code != http.StatusConflict || code != "RESULT_RETRACTED" {
		t.Fatalf("a second retraction: status=%d body=%s", second.Code, second.Body.String())
	}
	stored, stands := storedRetraction(t, database, result.ID)
	if !stands || stored.Reason != "The suite was stale." {
		t.Fatalf("the retraction that stands = %+v, want the first one", stored)
	}
	if events := retractionEventsOn(t, database, parent.Key); events != 1 {
		t.Fatalf("the check's issue holds %d result.retracted events, want one", events)
	}
}

// A retraction is taken on a closed issue and for a check of any `when`. A closed issue's
// results are the production record, and a false one there is the one that must be correctable,
// so neither the ISSUE_CLOSED refusal the record route makes nor any test of `when` stands in
// this route.
func TestResultRetractionIsTakenOnAClosedIssueAndForAnAfterMergeCheck(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "RETC", "Closed", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	uploadDocument(t, handler, owner, "verification.md", twoChecks)
	beforeMerge := recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("k1", "pass", headCommit, nil))

	closed := dispatchRequest(t, handler, http.MethodPatch, owner, map[string]any{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	afterMerge := recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("k2", "pass", mergeCommit, nil))

	// The pre-merge check's result, on an issue that is now closed: the record route refuses
	// such a result there, and the retraction takes it.
	first := retractAsPerson(t, handler, beforeMerge.ID, "The run never touched the server.")
	if first.Code != http.StatusCreated {
		t.Fatalf("retract a before_merge result on a closed issue: status=%d body=%s", first.Code, first.Body.String())
	}
	// The after-merge check's own result, which only a closed issue can carry.
	second := retractAsPerson(t, handler, afterMerge.ID, "Production answered at another commit.")
	if second.Code != http.StatusCreated {
		t.Fatalf("retract an after_merge result on a closed issue: status=%d body=%s", second.Code, second.Body.String())
	}
	if events := retractionEventsOn(t, database, issue.Key); events != 2 {
		t.Fatalf("the closed issue holds %d result.retracted events, want two", events)
	}
	for _, id := range []string{beforeMerge.ID, afterMerge.ID} {
		if _, stands := storedRetraction(t, database, id); !stands {
			t.Fatalf("result %s carries no retraction row", id)
		}
	}
}

// Two retractions of one result sent at once are taken one after the other: the route takes the
// check issue's owner lock before it reads whether a retraction stands (retractCheckResult), so
// the second reads the first's row and answers 409 RESULT_RETRACTED. Without that lock both read
// no retraction while another writer holds the owner, both meet the lock only in the batch that
// appends their event, and the second's insert then hits check_result_retractions' primary key,
// which its caller reads as a 500.
//
// TestXidWindowResultRetracted cannot tell the two apart: the append's batch takes the same lock
// as its first statement, so the window holds the same three statements either way. Holding the
// lock until both retractions wait on it does.
func TestTwoRetractionsOfOneResultSentAtOnceAnswerOneRetractionAndOneConflict(t *testing.T) {
	// An hour's settle keeps the upload's settlement, which takes the same owner lock, out of the
	// queue this test counts.
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "RETR", "Raced", "A spec\n")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	result := recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("k1", "pass", headCommit, nil))

	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder transaction: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner(result.CheckIssueKey)); err != nil {
		t.Fatalf("hold the check issue's owner lock: %v", err)
	}

	answers := make(chan *httptest.ResponseRecorder, 2)
	go func() { answers <- retractAsSession(t, handler, result.ID, "ses-recorder", "The suite was stale.") }()
	go func() { answers <- retractAsPerson(t, handler, result.ID, "The commit was wrong.") }()
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, 10*time.Second); waiting < 2 {
		t.Fatalf("%d retractions queued behind the check issue's owner lock, want both", waiting)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the check issue's owner lock: %v", err)
	}

	codes := map[string]int{}
	for range 2 {
		var answer *httptest.ResponseRecorder
		select {
		case answer = <-answers:
		case <-time.After(30 * time.Second):
			t.Fatal("a retraction never answered once the lock was released")
		}
		switch code, _ := refusalOf(t, answer); {
		case answer.Code == http.StatusCreated:
			codes["201"]++
		case answer.Code == http.StatusConflict && code == "RESULT_RETRACTED":
			codes["409 RESULT_RETRACTED"]++
		default:
			t.Fatalf("a raced retraction: status=%d body=%s, want 201 or 409 RESULT_RETRACTED: the route read "+
				"whether a retraction stands before it held the check issue's owner lock (store.LockOwner)",
				answer.Code, answer.Body.String())
		}
	}
	if codes["201"] != 1 || codes["409 RESULT_RETRACTED"] != 1 {
		t.Fatalf("the raced retractions answered %v, want one 201 and one 409 RESULT_RETRACTED", codes)
	}
	if _, stands := storedRetraction(t, database, result.ID); !stands {
		t.Fatalf("result %s carries no retraction row", result.ID)
	}
	if events := retractionEventsOn(t, database, result.CheckIssueKey); events != 1 {
		t.Fatalf("the check's issue holds %d result.retracted events, want one", events)
	}
}
