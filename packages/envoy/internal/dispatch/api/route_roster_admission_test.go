package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A read that waits on the shared listener fetch of the route roster (readRouteRoster: an agents
// list, an issue list filtered by a judged route_status, an issue's route) waits on another
// server, as a key-set read or an issuer's discovery does, and holds no admission unit across
// that wait (admission.GiveBack). At c-scale's ten times on the production-sized copy (run
// lt-20261123t222834-a9ae, step head-10x-3 at 7db90fa8, 2026-11-24T00:40:04-06Z) the listener
// took up to routeRosterTimeout to answer one fetch; every issue list and detail read waited on
// it holding its read unit, the read class filled, and 1,117 other reads were refused 429
// OVERLOADED (queue_full, wait_exceeded) while main 6e4e55be served the same waits late.
//
// The row holds one roster fetch open with more readers waiting on it than the read class has
// units, then requires: no read unit held and none queued while they wait; a read that does not
// ask the roster (the project list) admitted meanwhile, answered 200 while the fetch is held, where
// one that queued behind the waiters for a unit would be refused at its queue wait; and every waiter
// answered 200 once the listener answers. The fetch's bound and the listener client's window are
// past any run, so the fetch ends only when the row lets the listener answer.
func TestAReadWaitingOnTheListenersRosterHoldsNoReadUnit(t *testing.T) {
	t.Parallel()
	listenerURL, calls, release := heldRosterListener(t)
	limiter := networkWaitLimiter()
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listenerURL, envoyTimeout: pastTheRun, admission: limiter})
	served.rosters.ttl, served.rosters.timeout = pastTheRun, pastTheRun
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test project"}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}

	requests := make([]*http.Request, 0, networkWaiters)
	for index := range networkWaiters {
		target := "/api/v1/agents"
		if index%2 == 1 {
			target = "/api/v1/issues?project=TEST&route_status=no_holder"
		}
		requests = append(requests, testRequestAs(t, http.MethodGet, target, "alice"))
	}
	waiters := startWaiters(handler, requests)
	awaitCalls(t, calls, 1)

	testwait.Until(t, fmt.Sprintf("the read class to hold no unit and queue no request with %d reads waiting on the listener's roster (its units in flight and queued)", networkWaiters), func() (string, bool) {
		stats := limiter.Stats()[admission.Read]
		return fmt.Sprintf("%d and %d", stats.InFlight, stats.Queued), stats.InFlight == 0 && stats.Queued == 0
	})
	for attempt := range networkWaiters {
		answer := serveTimed(handler, testRequestAs(t, http.MethodGet, "/api/v1/projects", "alice"))
		if answer.status != http.StatusOK {
			t.Errorf("project list %d while %d reads wait on the listener's roster: status=%d after %v body=%s, want 200",
				attempt+1, networkWaiters, answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}

	release()
	for _, answer := range waiters.collect(t, networkWaiters) {
		if answer.status != http.StatusOK {
			t.Errorf("a read waiting on the listener's roster: status=%d after %v body=%s, want 200 once the listener answered",
				answer.status, answer.elapsed, strings.TrimSpace(answer.body))
		}
	}
	if stats := limiter.Stats()[admission.Read]; stats.InFlight != 0 {
		t.Errorf("once every waiter was answered the read class holds %d units, want none", stats.InFlight)
	}
}

// testRequestAs is a request as dispatchRequest sends it for login, to serve on a goroutine of its
// own (serveTimed) rather than through dispatchRequest's recorder.
func testRequestAs(t *testing.T, method, target, login string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	return request
}
