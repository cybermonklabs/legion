package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// registeredPatterns is every pattern NewServer registers on its mux, as it registers them.
// `net/http` can list no mux's patterns, so this list is kept by hand, which is the thing this
// change forbids for the shape of an answer and is unavoidable here.
//
// It is not what makes the header ride on every answer, and a case below says so by reading the
// server's own Handler: the mux is wrapped in statingContract, which sets the header before any
// route of the mux runs, so a route that misses it cannot exist — one added tomorrow and left
// out of this list carries the header all the same. What the list buys is that each pattern
// named here is a live route of the server and is answered with the header in fact, the
// catch-all's refusal and the 204 answers among them.
//
// It is read by one test more, which holds every route of the answer table to it
// (TestEveryRouteOfTheAnswerTableIsARegisteredRoute). The two read one list, so neither can
// drift from the routes the other holds.
var registeredPatterns = []string{
	"GET /legion/v1/state",
	"GET /legion/v1/issues/{key}/record",
	"GET /healthz",
	"POST /legion/v1/claims/register",
	"POST /legion/v1/claims/ready",
	"POST /legion/v1/claims/ready-for-update",
	"POST /legion/v1/claims/exit",
	"POST /legion/v1/claims/activity",
	"POST /legion/v1/claims/release-records",
	"POST /legion/v1/grants",
	"POST /legion/v1/controller/secret",
	"POST /legion/v1/controller/github-credential",
	"POST /legion/v1/handoff/complete",
	"POST /legion/v1/issues/status",
	"POST /legion/v1/claims/fresh-session",
	"POST /legion/v1/claims/compact",
	"POST /legion/v1/gates/register",
	"POST /legion/v1/waves/release",
	"POST /legion/v1/phase/backward",
	"POST /legion/v1/phase/retry",
	"POST /legion/v1/signoff",
	"POST /legion/v1/roots/close",
	"POST /legion/v1/children/park",
	"POST /legion/v1/children/rerun",
	"POST /legion/v1/operator/claims",
	"GET /legion/v1/operator/claims",
	"POST /legion/v1/operator/claims/{token}/deliver",
	"POST /legion/v1/operator/claims/{token}/suspend",
	"POST /legion/v1/operator/claims/{token}/resume",
	"POST /legion/v1/operator/claims/{token}/stop",
	"POST /legion/v1/operator/claims/{token}/retry",
	"POST /legion/v1/operator/claims/{token}/compact",
	"POST /legion/v1/operator/claims/{token}/forget-session",
	"POST /legion/v1/operator/claims/{token}/close",
	"/",
}

// Every route an answer of the table names is a route this server registers. An answer's Routes
// say which doors write that answer, and a door shapes its body by the answer id it writes
// (shapeAnswer), so a door writing an id whose Routes do not name it shapes that body to a
// contract that door never declared. The operator's compaction door
// (`POST /legion/v1/operator/claims/{token}/compact`) wrote AnswerCompact, whose one route is
// the controller's door. That answer shapes to itself at every contract today, so no case
// failed for it, and this one reads the pair rather than the shape.
//
// The patterns come from registeredPatterns, and the case above holds that list to the live
// routes of the server, so a route held to it is held to a route this daemon answers.
func TestEveryRouteOfTheAnswerTableIsARegisteredRoute(t *testing.T) {
	registered := make(map[string]bool, len(registeredPatterns))
	for _, pattern := range registeredPatterns {
		registered[pattern] = true
	}

	for _, answer := range Answers {
		for _, route := range answer.Routes {
			if !registered[route] {
				t.Errorf("the %s answer names the route %q, which NewServer registers no pattern for", answer.ID, route)
			}
		}
	}
}

// askedPattern is the method and the target one pattern is asked with: a wildcard stands for a
// value a route of that shape is asked with, and the catch-all pattern for a path no route of
// this daemon has.
func askedPattern(pattern string) (method, target string) {
	method, target, found := strings.Cut(pattern, " ")
	if !found {
		return http.MethodGet, "/legion/v1/no-such-route"
	}
	target = strings.ReplaceAll(target, "{key}", "LEGION-208")
	target = strings.ReplaceAll(target, "{token}", "legion-legion-legion-208-implementer")
	return method, target
}

// Every answer of this daemon states the contract it was written at, whatever the route and
// whatever the answer: the wrapper sets the header before the mux chooses a route, so a success,
// a refusal, the catch-all's 404 and a known path asked with the wrong method all carry it.
func TestEveryAnswerStatesTheDaemonsContract(t *testing.T) {
	want := strconv.Itoa(DaemonAPIVersion)
	server := NewServer("127.0.0.1", 8437, Options{State: fakeSource{}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}}})

	if _, wrapped := server.Handler.(statingContract); !wrapped {
		t.Fatalf("the server's handler is %T, want the mux wrapped in statingContract: the wrapper, and not the list below, is what makes a route that misses the header impossible", server.Handler)
	}

	for _, pattern := range registeredPatterns {
		t.Run(pattern, func(t *testing.T) {
			method, target := askedPattern(pattern)
			recorder := httptest.NewRecorder()

			server.Handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))

			if got := recorder.Header().Get(daemonContractHeader); got != want {
				t.Errorf("%s answered %d with %s %q, want %q", pattern, recorder.Code, daemonContractHeader, got, want)
			}
			if pattern == "/" {
				return
			}
			// A pattern of the list that is no route of the server any more is answered by the
			// catch-all, which says so in its body: the list names live routes.
			if strings.Contains(recorder.Body.String(), `"no route"`) {
				t.Errorf("%s is answered by the catch-all (%d %s): the list names a pattern NewServer no longer registers", pattern, recorder.Code, recorder.Body)
			}
		})
	}

	t.Run("a known path asked with the wrong method", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/legion/v1/state", nil))

		if got := recorder.Header().Get(daemonContractHeader); got != want {
			t.Errorf("a wrong-method request answered %d with %s %q, want %q", recorder.Code, daemonContractHeader, got, want)
		}
	})
}

// The four routes that answer 204 write no body at all, so no writer of a body could put the
// header on them; the wrapper does. This drives one of the four to a real 204 over the whole
// server, rather than holding the shape of the handler.
func TestAnAnswerWithNoBodyStatesTheDaemonsContract(t *testing.T) {
	h := newHarness(t)
	_, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	registered := h.registerContract(boot, session, DaemonAPIVersion, true)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: registered.ClaimToken, SessionID: session, Secret: registered.Secret, Generation: registered.Generation,
	}, nil)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("ready = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("ready answered the body %s, want none: this case is about an answer with no body", recorder.Body)
	}
	if got, want := recorder.Header().Get(daemonContractHeader), strconv.Itoa(DaemonAPIVersion); got != want {
		t.Errorf("the 204 carries %s %q, want %q", daemonContractHeader, got, want)
	}
}
