package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
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

// TestABrokerTokensListHoldsItOnceAProjectIsPrivate (R3): once a project is private, a full
// member's broker token's list decides only which private projects it reads (access.CredentialCovers),
// and a token an operator's entry mapped to one project keeps the write scope it had before
// projects could be private. A token an operator's entry mapped to BRK reads another project's
// issue and the deployment's priorities, as the open viewer it is; it is refused a comment on that
// issue and a blocked_by target of it 403 PROJECT_SCOPE naming the project, and a write of the
// deployment 403 PROJECT_SCOPE_DEPLOYMENT naming its list, in the words it was refused before; and
// whoami answers its list and project. A token acting as its person whose trade named BRK reads
// and writes another public project, filing an issue there, and whoami answers BRK.
//
// Control: VisibleProjectsSQL with a list covering only the listed projects (the rule before):
// both tokens read OTHER's issue as missing, and the test fails naming each.
func TestABrokerTokensListHoldsItOnceAProjectIsPrivate(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const named, person = "a-named-token-of-one-project", "a-person-token-whose-trade-named-one-project"
	rig.broker.Says(named, liveNamedToken(caller.jkt))
	answer := liveNamedToken(caller.jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, []string{brokerTestProject}
	rig.broker.Says(person, answer)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTHER", "Other")
	seedProject(t, rig, "HIDE", "Hidden")
	makePrivate(t, rig.database, "HIDE")
	as := func(token, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(caller.presenting(t, method, path, token, body))
	}

	for _, token := range []string{named, person} {
		requireB7Answer(t, token+": GET another project's issue", as(token, http.MethodGet, "/api/v1/issues/"+other.issue, nil), b7Answer{status: http.StatusOK})
	}

	requireB7Answer(t, "the named token's GET /api/v1/priorities", as(named, http.MethodGet, "/api/v1/priorities", nil), b7Answer{status: http.StatusOK})
	requireB7Answer(t, "the named token's comment on another project's issue", as(named, http.MethodPost, "/api/v1/issues/"+other.issue+"/comments", map[string]any{"body": "A remark."}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write belongs to project OTHER"}`})
	requireB7Answer(t, "the named token's blocked_by target of another project", as(named, http.MethodPatch, "/api/v1/issues/"+own.issue, map[string]any{"blocked_by": []string{other.issue}}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; blocked_by issue ` + other.issue + ` belongs to project OTHER"}`})
	requireB7Answer(t, "the named token's write of the deployment", as(named, http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project BRK and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
	requireB7Answer(t, "the named token's whoami", as(named, http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","projects":["BRK"],"project":"BRK","scopes":[],"service":null,"session":"` + brokerTestSession + `"}`})

	requireB7Answer(t, "the person token's issue filed in another project", as(person, http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Elsewhere", "spec": "# Spec"}), b7Answer{status: http.StatusCreated})
	requireB7Answer(t, "the person token's whoami", as(person, http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","projects":["BRK"],"project":"BRK","scopes":["person"],"service":null,"session":"` + brokerTestSession + `"}`})
}
