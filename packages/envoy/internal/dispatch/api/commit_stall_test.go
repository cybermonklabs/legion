package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A write whose COMMIT reached the database and was never answered may have landed: the server
// sent it and the database may have applied it before the socket timed out. It is answered 500
// COMMIT_OUTCOME_UNKNOWN with no Retry-After, as a commit that ran out its own bound is
// (TestAWriteWhoseCommitRunsOutItsBoundIsNotAnsweredDatabaseBusy), and never the retryable 503
// DATABASE_BUSY, which tells a client that nothing of the call committed and to send it again.
func TestAWriteWhoseCommitTheDatabaseStopsAnsweringIsAnOutcomeNobodyKnows(t *testing.T) {
	socket := storetest.NewStalledSocket(store.RequestsPool)
	logs := captureDebugLog(t)
	handler, _, _ := newTestServer(t, testServerOptions{dialRequestPool: socket.Dial, settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "The database stops answering a COMMIT", "A spec")

	socket.StallAfterSent(storetest.SimpleQuery("commit"))
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": "A comment whose commit was never answered"}, "alice")

	if !socket.Fired() {
		t.Fatal("the socket never timed out, so the write's COMMIT was answered as any other")
	}
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"COMMIT_OUTCOME_UNKNOWN"`) {
		t.Fatalf("status=%d body=%s, want 500 COMMIT_OUTCOME_UNKNOWN", response.Code, response.Body.String())
	}
	if retry := response.Result().Header.Get("Retry-After"); retry != "" {
		t.Fatalf("Retry-After = %q, want none: the commit may have landed", retry)
	}
	if busy := logs.Lines("request found the database busy"); busy != "" || strings.Contains(response.Body.String(), admission.CodeDatabaseBusy) {
		t.Fatalf("the commit was offered as retryable:\n%s", busy)
	}
}
