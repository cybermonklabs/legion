package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A read whose read-only transaction's COMMIT the database never answered wrote nothing: a
// read-only transaction cannot. It is refused 503 DATABASE_BUSY with Retry-After, as any other
// stall of a read is (TestAReadWhoseDatabaseStopsAnsweringIsRefusedDatabaseBusy), with no ERROR
// line, and never 500 COMMIT_OUTCOME_UNKNOWN, which only a transaction that wrote can earn.
// GET /api/v1/me/replies reads in a repeatable-read, read-only transaction (api/replies.go).
func TestAReadWhoseReadOnlyCommitTheDatabaseStopsAnsweringIsRefusedDatabaseBusy(t *testing.T) {
	socket := storetest.NewStalledSocket(store.RequestsPool)
	logs := captureDebugLog(t)
	handler, _, _ := newTestServer(t, testServerOptions{dialRequestPool: socket.Dial, settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "A read whose commit the database never answers", "A spec")

	socket.StallAfterSent(storetest.SimpleQuery("commit"))
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies", nil, "alice")

	if !socket.Fired() {
		t.Fatal("the socket never timed out, so the read's COMMIT was answered as any other")
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
	if unknown := logs.Lines("its outcome is unknown"); unknown != "" {
		t.Fatalf("a read-only transaction's commit was reported as an outcome nobody knows:\n%s", unknown)
	}
}
