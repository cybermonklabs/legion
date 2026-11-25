package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
)

// An issue's references read can run its whole bound on the database: on the production-sized copy
// of the load test (c-scale, INFRA-592 tester round 60) the one-hop read still ran 3.5 to 4 s and
// ended at its statement timeout on almost every call, at main as at the head. While the route takes
// ordinary read units, a burst of such reads holds the whole read class for that long, and every
// other read is refused or waits behind them: there the head answered 13.1% of requests 2xx where
// main answered 93.0%. So a burst of references reads that hold the database must leave the rest
// of the read class to the other routes. Here a lock on refs, which the closure reads, holds every
// references read inside its first refs statement, and a read of another route sent behind the
// burst must be answered while the lock still holds the burst, not when the references reads give
// their units back: the lock goes only after its answer, so a read that queued behind the burst for
// a unit would not be answered before it. The read class's wait, which the burst's reads queued in
// the lane wait in for as long as the row holds the lock, is not what the row proves, and is past
// any run.
func TestABurstOfSlowIssueReferencesLeavesTheReadClassToOtherReads(t *testing.T) {
	t.Parallel()
	const readCap = 4
	limiter := admission.New(admission.Limits{Read: readCap, Write: 4, HeavyReadLane: 1, ReadWait: pastTheRun})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	createProjectDocument(t, handler, "CORE", "Design notes", "# Design\n")
	createReferenceAPIComment(t, handler, issue.Key, "dispatch://CORE/artifact/design-notes")

	release := lockTables(t, database, "refs, block_refs")
	defer release()
	burst := make(chan *httptest.ResponseRecorder, readCap)
	for range readCap {
		go func() { burst <- issueReferencesRequest(t, handler, issue.Key, "") }()
	}
	waitUntil(t, "a references read waiting on the held refs lock", func() bool {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%refs.Closure%'`).Scan(&waiting); err != nil {
			t.Fatalf("read the references reads waiting on the lock: %v", err)
		}
		return waiting > 0
	})
	// Every request of the burst reaches its admission: the admitted ones go on to the lock, and
	// the rest wait in the references lane's queue.
	waitUntil(t, "every references read of the burst admitted or queued in its lane", func() bool {
		return limiter.Stats()[admission.Read].InFlight+referencesLane(t, limiter).Queued == readCap
	})
	stats := limiter.Stats()[admission.Read]

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice") }()
	other := awaitResponse(t, answered)
	release()
	for range readCap {
		awaitResponse(t, burst)
	}

	if other.Code != http.StatusOK {
		t.Fatalf("a read of another route sent behind %d references reads that held the database (read class %d in flight of %d) "+
			"answered status %d with the burst still held, want 200: a burst of one slow route takes the whole read class, "+
			"and every other read waits for it or is refused",
			readCap, stats.InFlight, readCap, other.Code)
	}
}
