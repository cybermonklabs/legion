package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// b7Answer is one answer a row holds to the bytes it was answered before projects could be
// private: its status, and its body when the row names one ("" holds the status alone).
type b7Answer struct {
	status int
	body   string
}

// requireB7Answer fails, naming what, unless response is want.
func requireB7Answer(t *testing.T, what string, response *httptest.ResponseRecorder, want b7Answer) {
	t.Helper()
	got := strings.TrimSpace(response.Body.String())
	if response.Code != want.status || (want.body != "" && got != want.body) {
		t.Errorf("%s = %d %s, want %d %s", what, response.Code, got, want.status, want.body)
	}
}

// TestABrokerTokenScopedToOneProjectAnswersAsBeforeWhileNoProjectIsPrivate (R9): a broker token
// an operator's entry mapped to one project, BRK, in a deployment that holds no private project,
// reads what it read before projects could be private and is refused each write it was refused,
// in the same bytes. It reads the deployment's priorities and another project's issue, and whoami
// answers project BRK and no list; a write of the deployment, a write to another project's
// object, a write to an object nobody holds, an issue filed in another project and a blocked_by
// target of another project are each 403 in the words they were answered then; its write to its
// own project's issue lands.
func TestABrokerTokenScopedToOneProjectAnswersAsBeforeWhileNoProjectIsPrivate(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-named-token-of-one-project"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTHER", "Other")
	as := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(caller.presenting(t, method, path, token, body))
	}

	requireB7Answer(t, "GET /api/v1/priorities", as(http.MethodGet, "/api/v1/priorities", nil), b7Answer{status: http.StatusOK})
	requireB7Answer(t, "GET another project's issue", as(http.MethodGet, "/api/v1/issues/"+other.issue, nil), b7Answer{status: http.StatusOK})
	requireB7Answer(t, "GET /api/v1/whoami", as(http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","project":"` + brokerTestProject + `","scopes":[],"service":null,"session":"` + brokerTestSession + `"}`})

	requireB7Answer(t, "a write of the deployment", as(http.MethodPost, "/api/v1/priorities", map[string]any{"title": "A priority"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE_DEPLOYMENT","error":"this request's credential may write project BRK and nothing else; POST /api/v1/priorities belongs to the deployment rather than to a project"}`})
	requireB7Answer(t, "a comment on another project's issue", as(http.MethodPost, "/api/v1/issues/"+other.issue+"/comments", map[string]any{"body": "A remark."}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write belongs to project OTHER"}`})
	requireB7Answer(t, "a comment on an issue nobody holds", as(http.MethodPost, "/api/v1/issues/NOPE-404/comments", map[string]any{"body": "A remark."}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write resolves to no project of that credential's"}`})
	requireB7Answer(t, "an issue filed in another project", as(http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Elsewhere", "spec": "# Spec"}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; this write belongs to project OTHER"}`})
	requireB7Answer(t, "a blocked_by target of another project", as(http.MethodPatch, "/api/v1/issues/"+own.issue, map[string]any{"blocked_by": []string{other.issue}}), b7Answer{http.StatusForbidden,
		`{"code":"PROJECT_SCOPE","error":"this request's credential may write project BRK; blocked_by issue ` + other.issue + ` belongs to project OTHER"}`})
	requireB7Answer(t, "a comment on its own project's issue", as(http.MethodPost, "/api/v1/issues/"+own.issue+"/comments", map[string]any{"body": "A remark."}), b7Answer{status: http.StatusCreated})
}

// TestABrokerTokenActingAsItsPersonAnswersAsBeforeWhileNoProjectIsPrivate (R9): a broker token
// acting as its person whose trade named a list, BRK, in a deployment that holds no private
// project, reads and writes as it did before projects could be private, when the list limited
// nothing: it reads the deployment's priorities and another project's issue, whoami answers no
// project and no list, and it comments on another project's issue, files an issue there and
// names a blocked_by target there.
func TestABrokerTokenActingAsItsPersonAnswersAsBeforeWhileNoProjectIsPrivate(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-person-token-whose-trade-named-one-project"
	answer := liveNamedToken(caller.jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, []string{brokerTestProject}
	rig.broker.Says(token, answer)
	own := seedProject(t, rig, brokerTestProject, "Broker")
	other := seedProject(t, rig, "OTHER", "Other")
	as := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return rig.serve(caller.presenting(t, method, path, token, body))
	}

	requireB7Answer(t, "GET /api/v1/priorities", as(http.MethodGet, "/api/v1/priorities", nil), b7Answer{status: http.StatusOK})
	requireB7Answer(t, "GET another project's issue", as(http.MethodGet, "/api/v1/issues/"+other.issue, nil), b7Answer{status: http.StatusOK})
	requireB7Answer(t, "GET /api/v1/whoami", as(http.MethodGet, "/api/v1/whoami", nil), b7Answer{http.StatusOK,
		`{"kind":"agent","owner":"` + brokerTestPerson + `","project":null,"scopes":["person"],"service":null,"session":"` + brokerTestSession + `"}`})
	requireB7Answer(t, "a comment on another project's issue", as(http.MethodPost, "/api/v1/issues/"+other.issue+"/comments", map[string]any{"body": "A remark."}), b7Answer{status: http.StatusCreated})
	requireB7Answer(t, "an issue filed in another project", as(http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Elsewhere", "spec": "# Spec"}), b7Answer{status: http.StatusCreated})
	requireB7Answer(t, "a blocked_by target of another project", as(http.MethodPatch, "/api/v1/issues/"+own.issue, map[string]any{"blocked_by": []string{other.issue}}), b7Answer{status: http.StatusOK})
}
