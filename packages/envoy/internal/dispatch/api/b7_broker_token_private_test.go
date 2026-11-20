package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// requireAnsweredAsMissing fails, naming what, unless hidden, an answer about the object hiddenID
// names, is missing, the answer about the object nobody holds missingID names, with the id put back
// (R2, R4): the same status and the same bytes.
func requireAnsweredAsMissing(t *testing.T, what string, hidden, missing *httptest.ResponseRecorder, hiddenID, missingID string) {
	t.Helper()
	want := strings.ReplaceAll(strings.TrimSpace(missing.Body.String()), missingID, hiddenID)
	if got := strings.TrimSpace(hidden.Body.String()); hidden.Code != missing.Code || got != want {
		t.Errorf("%s = %d %s, want %d %s, as an object nobody holds", what, hidden.Code, got, missing.Code, want)
	}
}

// TestABrokerTokensListHoldsItOnceAProjectIsPrivate (R3): once a project is private, a broker
// token's list holds its reads and writes, whichever list it is. A token an operator's entry mapped
// to BRK is refused the deployment's priorities (403 DEPLOYMENT_READ_REFUSED), reads another
// project's issue, comments on it and names it blocked_by exactly as an issue nobody holds, is
// refused a write of the deployment naming its list, and whoami answers its list and project. A
// token acting as its person whose trade named BRK is held the same: another project's issue
// answers as missing, an issue filed there is refused 403 PROJECT_SCOPE naming its list, and
// whoami answers BRK.
//
// Control: brokerTokenActor leaving a person token's list off its credential (99e11cdd50's rule):
// the person token reads OTHER's issue, whoami names no list, and the filed issue lands.
func TestABrokerTokensListHoldsItOnceAProjectIsPrivate(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const named, person = "a-named-token-of-one-project", "a-person-token-whose-trade-named-one-project"
	rig.broker.Says(named, liveNamedToken(caller.jkt))
	answer := liveNamedToken(caller.jkt)
	answer.ProjectScope, answer.Projects = brokertoken.ProjectScopePerson, []string{brokerTestProject}
	rig.broker.Says(person, answer)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTHER", "Other")
	seedProject(t, rig, "HIDE", "Hidden")
	makePrivate(t, rig.database, "HIDE")
	const nobody = "NOPE-404"
	as := func(token, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(caller.presenting(t, method, path, token, body))
	}

	for _, token := range []string{named, person} {
		requireAnsweredAsMissing(t, token+": GET another project's issue",
			as(token, http.MethodGet, "/api/v1/issues/"+other.issue, nil), as(token, http.MethodGet, "/api/v1/issues/"+nobody, nil), other.issue, nobody)
		requireAnsweredAsMissing(t, token+": a comment on another project's issue",
			as(token, http.MethodPost, "/api/v1/issues/"+other.issue+"/comments", map[string]any{"body": "A remark."}),
			as(token, http.MethodPost, "/api/v1/issues/"+nobody+"/comments", map[string]any{"body": "A remark."}), other.issue, nobody)
	}

	if response := as(named, http.MethodGet, "/api/v1/priorities", nil); response.Code != http.StatusForbidden ||
		decodeBody[refusalAnswer](t, response).Code != "DEPLOYMENT_READ_REFUSED" {
		t.Errorf("the named token's GET /api/v1/priorities = %d %s, want 403 DEPLOYMENT_READ_REFUSED", response.Code, response.Body.String())
	}
	requireAnsweredAsMissing(t, "the named token's blocked_by target of another project",
		as(named, http.MethodPatch, "/api/v1/issues/"+own.issue, map[string]any{"blocked_by": []string{other.issue}}),
		as(named, http.MethodPatch, "/api/v1/issues/"+own.issue, map[string]any{"blocked_by": []string{nobody}}), other.issue, nobody)
	requireB7Answer(t, "the named token's write of the deployment", as(named, http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project BRK and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
	requireB7Answer(t, "the named token's whoami", as(named, http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","projects":["BRK"],"project":"BRK","scopes":[],"service":null,"session":"` + brokerTestSession + `"}`})

	requireB7Answer(t, "the person token's issue filed in another project", as(person, http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Elsewhere", "spec": "# Spec"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write belongs to project OTHER"}`})
	requireB7Answer(t, "the person token's whoami", as(person, http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","projects":["BRK"],"project":"BRK","scopes":["person","approve"],"service":null,"session":"` + brokerTestSession + `"}`})
}
