package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestARetractionOfAResultOnAHiddenCheckAnswersAsAMissingResultAndTakesNoLock (F22): a result's
// check can be an issue of another project than the issue it was recorded on (a check of a parent
// of another project), and POST /api/v1/results/{id}/retract reads the result as its caller reads
// both issues before it takes the owner lock of the check's issue. So a caller who sees the issue
// the result was recorded on but not the check's issue is answered exactly as for a result id no
// row holds, 404 RESULT_NOT_FOUND, writes nothing, and never waits on that issue's owner lock.
//
// carol puts a Verification document on PRIV's issue and a PUB issue under it, and records a result
// of the document's check k1 on the PUB issue. alice, who sees PUB and not PRIV, retracts it: once
// while the test holds PRIV's issue's owner lock, under a ten-second deadline, and once with the
// lock free. Each answer is the answer of a result id nobody holds, and no retraction or
// result.retracted event is written. With no project private alice's retraction is taken, as
// before (R9).
//
// Control: readRetractedResult reading only check_results.issue_key, as HEAD c99245cbc6 does
// (controls/F22.patch reversed): alice's first retraction waits on PRIV's issue's lock until its
// deadline and her second is taken 201, appending result.retracted on PRIV's issue; the test fails
// at both rows "alice's retraction ... answers" and at the rows that nothing was written.
func TestARetractionOfAResultOnAHiddenCheckAnswersAsAMissingResultAndTakesNoLock(t *testing.T) {
	seed := seedPrivateProject(t)
	check := seed.issue[seedPrivate]
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+check+"/artifacts",
		map[string]any{"name": "verification.md", "content": twoChecks}, http.StatusCreated)
	recording := decodeBody[struct {
		Key string `json:"key"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": seedPublic, "title": "Records on PRIV's check", "parent": check, "force": true,
	}, http.StatusCreated)).Key
	result := decodeBody[recordedResult](t, seed.mustRequest(t, callerCarol, http.MethodPost,
		"/api/v1/issues/"+recording+"/results", resultBody("k1", "pass", headCommit, nil), http.StatusCreated)).Result
	if result.IssueKey != recording || result.CheckIssueKey != check {
		t.Fatalf("the result was recorded on %s for a check of %s, want on %s for a check of %s", result.IssueKey, result.CheckIssueKey, recording, check)
	}
	seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+recording, nil, http.StatusOK)
	seed.mustRequest(t, callerAlice, http.MethodGet, "/api/v1/issues/"+check, nil, http.StatusNotFound)

	reason := map[string]any{"reason": "The run was against the wrong commit."}
	missing := seed.request(t, callerAlice, http.MethodPost, "/api/v1/results/"+uuid.NewString()+"/retract", reason)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("alice's retraction of a result id nobody holds = %d %s, want 404", missing.Code, missing.Body.String())
	}
	asMissing := func(what string, ctx context.Context) {
		t.Helper()
		response := seed.requestIn(t, ctx, callerAlice, http.MethodPost, "/api/v1/results/"+result.ID+"/retract", reason)
		if response.Code != missing.Code || response.Body.String() != missing.Body.String() {
			t.Errorf("alice's retraction %s answers %d %s, want %d %s, the answer of a result id nobody holds",
				what, response.Code, response.Body.String(), missing.Code, missing.Body.String())
		}
		if _, held := storedRetraction(t, seed.database, result.ID); held {
			t.Errorf("alice's retraction %s stored a retraction of a result of a check she may not see", what)
		}
		if events := retractionEventsOn(t, seed.database, check); events != 0 {
			t.Errorf("alice's retraction %s appended %d result.retracted events on %s, want none", what, events, check)
		}
	}

	ctx := context.Background()
	holder, err := seed.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds %s's owner lock: %v", check, err)
	}
	if err := store.LockOwner(ctx, holder, store.IssueOwner(check)); err != nil {
		_ = holder.Rollback(ctx)
		t.Fatalf("take %s's owner lock: %v", check, err)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	asMissing("while the test holds "+check+"'s owner lock", deadline)
	cancel()
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release %s's owner lock: %v", check, err)
	}
	asMissing("with the lock free", ctx)

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/results/"+result.ID+"/retract", reason, http.StatusCreated)
	if events := retractionEventsOn(t, seed.database, check); events != 1 {
		t.Errorf("with no project private alice's retraction appended %d result.retracted events on %s, want one (R9)", events, check)
	}
}
