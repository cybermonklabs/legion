// results_token_test.go holds Dispatch to its results list: a results token - one the secrets
// broker minted from a result entry for one CI run, the scope results alone beside the project
// scope results, a person, one project and the run - reaches the routes resultsScopeRoutes names
// and nothing else. Every other row of the table and GET /ws/live answer it 403
// RESULTS_ONLY_TOKEN before the route's handler, before the sign-in check and before its
// Idempotency-Key is read (refuseLimitedToken). The walks are the read-only token's, run with
// this token and this list (limitedToken).
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
)

// resultsTokenProject is the project the walks' results token writes the results of: the
// project routeProbePath fills a {key} with, so the listed row's project check finds it.
const resultsTokenProject = "TEST"

// resultsOnlyToken is the broker's answer for a result entry's token
// (BROKER_EXCHANGE_RESULT_WORKFLOWS): live, bound to jkt, the scope results alone beside the
// project scope results, the person and the project the entry names, the session of its
// workflow, and the run it was minted for.
func resultsOnlyToken(jkt, project string) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml",
		Person:       "ada@example.com",
		ProjectScope: ciphertoken.ProjectScopeResults,
		Projects:     []string{project},
		Scope:        ciphertoken.ScopeResults,
		CI: &ciphertoken.CIRun{
			Repository: "example-org/example-repo", WorkflowRef: "example-org/example-repo/.github/workflows/ci-results.yaml@refs/heads/topic",
			Ref: "refs/heads/topic", SHA: "0123456789abcdef0123456789abcdef01234567", EventName: "push",
			RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
		},
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
		ClientID:   ciphertoken.ExchangeClientID,
		Thumbprint: jkt,
	}
}

// limitedAnswer is what a listed row answers the mounted walk: its handler's own status and code,
// which a request the gate refused could not carry.
type limitedAnswer struct {
	status int
	code   string
}

// limitedToken is a broker token Dispatch serves on a list of its own beside the route table and
// answers 403 on every other row and on GET /ws/live, before the row's handler: a read-only token
// (readScopeRoutes) or a results token (resultsScopeRoutes), and refuseLimitedToken holds each to
// its list. The two walks below take one, so both tokens are held to one walk.
type limitedToken struct {
	// kind names the token in a failure: "a read-only token".
	kind string
	// answer is what the broker answers for the token, bound to the caller's key.
	answer func(jkt string) ciphertoken.Introspection
	// list is the routes the token may call, by "<method> <pattern>"; listed reads the mark
	// routes() stamps from it onto a row.
	list   []string
	listed func(apiRoute) bool
	// refusal is the code every other row answers the token with, under 403 and no challenge.
	refusal string
	// listAnswers is what each listed row answers the mounted walk.
	listAnswers map[string]limitedAnswer
	// seed prepares the rig's store, so a listed row's chain reaches its handler.
	seed func(t *testing.T, rig *brokerTokenRig)
}

// readOnlyLimitedToken is the read-only token as the walks take it.
func readOnlyLimitedToken() limitedToken {
	return limitedToken{
		kind:        "a read-only token",
		answer:      readOnlyToken,
		list:        readScopeRoutes,
		listed:      func(route apiRoute) bool { return route.ReadScope },
		refusal:     contracts.DispatchReadOnlyToken,
		listAnswers: readListAnswers,
	}
}

// resultsLimitedToken is the results token as the walks take it: one of project TEST, the
// project a probe's {key} names, which the seed creates with its issue TEST-1, the issue a
// probe's issue {key} names, so each listed row's project check (withinProjectScope) finds the
// project the token names. Each listed row answers the mounted walk's probe body, which names
// actor, 400 RESULT_INPUT: the handler's own answer.
func resultsLimitedToken() limitedToken {
	return limitedToken{
		kind:    "a results token",
		answer:  func(jkt string) ciphertoken.Introspection { return resultsOnlyToken(jkt, resultsTokenProject) },
		list:    resultsScopeRoutes,
		listed:  func(route apiRoute) bool { return route.ResultsScope },
		refusal: codeResultsOnlyToken,
		listAnswers: map[string]limitedAnswer{
			"POST /api/v1/projects/{key}/results": {http.StatusBadRequest, "RESULT_INPUT"},
			"POST /api/v1/issues/{key}/results":   {http.StatusBadRequest, "RESULT_INPUT"},
		},
		seed: func(t *testing.T, rig *brokerTokenRig) {
			t.Helper()
			if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": resultsTokenProject, "name": "Test"}); created.Code != http.StatusCreated {
				t.Fatalf("create project %s: status=%d body=%s", resultsTokenProject, created.Code, created.Body.String())
			}
			if filed := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": resultsTokenProject, "title": "Probe", "spec": "A spec\n"}); filed.Code != http.StatusCreated {
				t.Fatalf("create issue %s-1: status=%d body=%s", resultsTokenProject, filed.Code, filed.Body.String())
			}
		},
	}
}

// notTheLimitedRefusal says how response differs from a limited token's refusal, and is empty
// when it is that refusal: 403 with the code given and no WWW-Authenticate challenge, since no
// scope makes the route the token's to call.
func notTheLimitedRefusal(response *httptest.ResponseRecorder, code string) string {
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	switch {
	case response.Code != http.StatusForbidden || refusal.Code != code:
		return "answered " + http.StatusText(response.Code) + " " + strings.TrimSpace(response.Body.String())
	case response.Header().Get("WWW-Authenticate") != "":
		return "carries the challenge " + response.Header().Get("WWW-Authenticate")
	}
	return ""
}

// walkEveryChainWith builds every row's own chain as register does (routeHandler), with the row's
// handler replaced by a recorder, and sends each the token with a body and a valid
// Idempotency-Key. Every row the token's list does not name answers 403 with its refusal and
// never runs its handler; each row the list names runs it once, and is a row the walk names an
// answer for (listAnswers), so a list that grows fails the walk until the walk names the row too.
// GET /ws/live is walked the same way through its own chain (liveSocketHandler), with the socket
// replaced. A row is counted once its assertions have run, and the count is held to the rows the
// registered server mounts (its route index, one entry a row), so a walk that skipped a row, or
// walked none, fails.
func walkEveryChainWith(t *testing.T, token limitedToken) {
	t.Helper()
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	if token.seed != nil {
		token.seed(t, rig)
	}
	caller := newBrokerCaller(t)
	const presented = "a-limited-token-walking-every-chain"
	rig.broker.Says(presented, token.answer(caller.jkt))

	replays := rig.served.keyedReplays()
	walked, listed := 0, 0
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		ran := 0
		route.Handler = func(w http.ResponseWriter, _ *http.Request) {
			ran++
			w.WriteHeader(http.StatusNoContent)
		}
		// Through a mux of the row's own pattern, as register mounts it, so the chain reads the
		// path values its project check and its handler read (r.PathValue).
		mux := http.NewServeMux()
		mux.Handle(name, rig.served.routeHandler(route, replays[name]))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, caller.presentingProbe(t, route, presented))
		if token.listed(route) {
			listed++
			if _, known := token.listAnswers[name]; !known {
				t.Errorf("%s is on the list of %s and the walk names no answer for it", name, token.kind)
			}
			if ran != 1 || response.Code != http.StatusNoContent {
				t.Errorf("%s is on the list of %s, and its handler ran %d times (status=%d body=%s), want once",
					name, token.kind, ran, response.Code, response.Body.String())
			}
		} else {
			if ran != 0 {
				t.Errorf("%s ran its handler %d times for %s, want none", name, ran, token.kind)
			}
			if why := notTheLimitedRefusal(response, token.refusal); why != "" {
				t.Errorf("%s %s for %s, want 403 %s", name, why, token.kind, token.refusal)
			}
		}
		walked++
	}

	socketOpened := 0
	live := rig.served.liveSocketHandler(func(http.ResponseWriter, *http.Request) { socketOpened++ })
	response := httptest.NewRecorder()
	live.ServeHTTP(response, caller.presenting(t, http.MethodGet, liveSocketPattern, presented, nil))
	if socketOpened != 0 {
		t.Errorf("GET %s opened the socket %d times for %s, want none", liveSocketPattern, socketOpened, token.kind)
	}
	if why := notTheLimitedRefusal(response, token.refusal); why != "" {
		t.Errorf("GET %s %s for %s, want 403 %s", liveSocketPattern, why, token.kind, token.refusal)
	}

	if mounted := len(rig.served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	if listed != len(token.list) {
		t.Fatalf("%d rows carry the mark of the list of %s, want the list's %d (%v)", listed, token.kind, len(token.list), token.list)
	}
	t.Logf("walked %d rows of the table (test hooks in) and GET %s with %s: %d listed, %d refused, every refusal %s",
		walked, liveSocketPattern, token.kind, listed, walked-listed+1, token.refusal)
}

// walkTheMountedServerWith walks the server register mounted, test hooks in, with the token:
// every row the list does not name and GET /ws/live answer 403 with its refusal; GET
// /ws/doc/{room}, which reads no broker token at all, answers 401; and each listed row answers its
// handler's own answer (listAnswers). A row is counted once its assertions have run, and the
// count is held to the rows the registered server mounts, as the chain walk's is.
func walkTheMountedServerWith(t *testing.T, token limitedToken) {
	t.Helper()
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	if token.seed != nil {
		token.seed(t, rig)
	}
	caller := newBrokerCaller(t)
	const presented = "a-limited-token-walking-the-mounted-server"
	rig.broker.Says(presented, token.answer(caller.jkt))

	walked := 0
	for _, route := range readOnlyTable() {
		name := route.Method + " " + route.Pattern
		response := rig.serve(caller.presentingProbe(t, route, presented))
		if !token.listed(route) {
			if why := notTheLimitedRefusal(response, token.refusal); why != "" {
				t.Errorf("%s %s for %s, want 403 %s", name, why, token.kind, token.refusal)
			}
		} else if want, known := token.listAnswers[name]; !known {
			t.Errorf("%s is on the list of %s and the walk says nothing of what it answers", name, token.kind)
		} else if refusal := decodeBody[refusalAnswer](t, response); response.Code != want.status || refusal.Code != want.code {
			t.Errorf("%s answered %s %d %s, want its handler's %d %s", name, token.kind, response.Code, response.Body.String(), want.status, want.code)
		}
		walked++
	}

	live := rig.serve(caller.presenting(t, http.MethodGet, liveSocketPattern, presented, nil))
	if why := notTheLimitedRefusal(live, token.refusal); why != "" {
		t.Errorf("GET %s %s for %s, want 403 %s", liveSocketPattern, why, token.kind, token.refusal)
	}
	room := "/ws/doc/00000000-0000-0000-0000-000000000001"
	if document := rig.serve(caller.presenting(t, http.MethodGet, room, presented, nil)); document.Code != http.StatusUnauthorized {
		t.Errorf("GET %s answered %s %d %s, want 401: the document socket takes a cookie alone", room, token.kind, document.Code, document.Body.String())
	}

	if mounted := len(rig.served.routeIndex); walked == 0 || walked != mounted {
		t.Fatalf("walked %d rows, want every one of the %d the server mounts", walked, mounted)
	}
	t.Logf("walked %d rows of the mounted table (test hooks in), GET %s and GET /ws/doc/{room} with %s", walked, liveSocketPattern, token.kind)
}

// TestEveryRowTheResultsListDoesNotNameRefusesAResultsTokenBeforeItsHandler: a results token
// reaches the handlers of the rows the results list names and of no other row, and no socket.
func TestEveryRowTheResultsListDoesNotNameRefusesAResultsTokenBeforeItsHandler(t *testing.T) {
	t.Parallel()
	walkEveryChainWith(t, resultsLimitedToken())
}

// TestTheMountedServerServesAResultsTokenOnTheResultsListAlone: the mounted server answers a
// results token 403 RESULTS_ONLY_TOKEN on every row but the listed ones, whose handlers answer it.
func TestTheMountedServerServesAResultsTokenOnTheResultsListAlone(t *testing.T) {
	t.Parallel()
	walkTheMountedServerWith(t, resultsLimitedToken())
}

// TestRegisterRefusesAResultsListOfNoMountedRow: a results list naming a route nothing serves
// reads as though it opened a route that is not there, so Register refuses it and names the
// route. It replaces the package's list for its length, so it is not parallel.
func TestRegisterRefusesAResultsListOfNoMountedRow(t *testing.T) {
	// Not parallel: it swaps the package variable resultsScopeRoutes, which every server a test of
	// the package builds registers.
	kept := resultsScopeRoutes
	t.Cleanup(func() { resultsScopeRoutes = kept })
	if refusal := registerRefusal(t); refusal != "" {
		t.Fatalf("api.Register refused the results list as it stands: %s", refusal)
	}
	const name = "POST /api/v1/projects/{key}/results/no-such-route"
	resultsScopeRoutes = []string{name}
	if refusal := registerRefusal(t); !strings.Contains(refusal, "results list") || !strings.Contains(refusal, name) {
		t.Fatalf("api.Register with %s on the results list refused %q, want a refusal of the results list naming it", name, refusal)
	}
}
