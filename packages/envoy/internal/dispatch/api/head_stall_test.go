package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A HEAD on a read route is a read: net/http's mux serves it with the route's GET handler, and it
// commits nothing. A database that stops answering it is refused 503 DATABASE_BUSY with
// Retry-After, as the same GET is (TestAReadWhoseDatabaseStopsAnsweringIsRefusedDatabaseBusy), and
// never 500 COMMIT_OUTCOME_UNKNOWN.
func TestAHeadWhoseDatabaseStopsAnsweringIsRefusedDatabaseBusy(t *testing.T) {
	socket := storetest.NewStalledSocket(store.RequestsPool)
	handler, _, _ := newTestServer(t, testServerOptions{dialRequestPool: socket.Dial, settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "The database stops answering a head", "A spec")

	socket.StallAfter(issue.Key)
	response := dispatchRequest(t, handler, http.MethodHead, "/api/v1/issues/"+issue.Key, nil, "alice")

	if !socket.Fired() {
		t.Fatal("the socket never timed out, so the read was served as any other")
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
}
