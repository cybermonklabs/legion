package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAServerWithNoDatabaseReadsAsTheClosedViewer: a server built with no database has nothing to
// resolve a viewer from, and its read check fails closed. A row that needs the viewer runs its
// handler with the closed viewer, which is not open and sees no project; a read of the whole
// deployment is refused 403 DEPLOYMENT_READ_REFUSED; and a caller who names nobody is still 401.
func TestAServerWithNoDatabaseReadsAsTheClosedViewer(t *testing.T) {
	mux := http.NewServeMux()
	served := register(mux, t.Context(), Deps{
		Store:    &store.Store{},
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", People: unrecordedPeople{}},
	})

	route := apiRoute{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets"}, Reads: []readclass.Class{readclass.Project}}
	ran := false
	handler := served.withinReadScope(route, func(w http.ResponseWriter, r *http.Request) {
		reader, ok := readerFromContext(r.Context())
		if !ok || reader.Viewer == nil {
			t.Fatalf("a project row of a server with no database ran its handler with no viewer: %+v, %t", reader, ok)
		}
		if reader.Viewer.Open || reader.Viewer.All || len(reader.Viewer.Sees) != 0 || reader.ID != "alice" {
			t.Fatalf("a project row of a server with no database read as %+v with the viewer %+v, want alice and the closed viewer", reader, *reader.Viewer)
		}
		ran = true
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	handler(httptest.NewRecorder(), request)
	if !ran {
		t.Fatal("a project row of a server with no database did not run its handler")
	}

	refused := dispatchRequest(t, mux, http.MethodGet, "/api/v1/priorities", nil, "alice")
	if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), `"code":"DEPLOYMENT_READ_REFUSED"`) {
		t.Fatalf("GET /api/v1/priorities on a server with no database = %d %s, want 403 DEPLOYMENT_READ_REFUSED", refused.Code, refused.Body.String())
	}
	anonymous := httptest.NewRecorder()
	mux.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/priorities", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/priorities naming nobody on a server with no database = %d %s, want 401", anonymous.Code, anonymous.Body.String())
	}
}

// TestADeploymentReadIsRefusedToAViewerThatIsNotOpen walks every GET row the table classes as a
// read of the deployment. bob, a restricted member, is refused each one 403 DEPLOYMENT_READ_REFUSED
// before its handler runs, the refusal naming the row; alice, a full member no list names, and the
// shared token, both open viewers, are not. A wildcard is filled with a value no row holds, since
// the refusal comes before any row is read. The one row a restricted member's browser reads, the
// list of their own machine logins (restrictedOwnLoginRoutes), has its own rows
// (TestARestrictedMemberListsAndRevokesOnlyTheirOwnMachineLogins,
// TestARestrictedMembersTokensListAndRevokeNoMachineLogin), and is the only one the walk leaves out.
func TestADeploymentReadIsRefusedToAViewerThatIsNotOpen(t *testing.T) {
	seed := seedPrivateProject(t)
	var walked, ownLogin []string
	for _, route := range seed.server.routes() {
		if route.Method != http.MethodGet || !slices.Contains(route.Reads, readclass.Deployment) {
			continue
		}
		if restrictedOwnLoginRoutes[route.Method+" "+route.Pattern] {
			ownLogin = append(ownLogin, route.Pattern)
			continue
		}
		walked = append(walked, route.Pattern)
		target := patternWildcard.ReplaceAllString(route.Pattern, "1")
		refused := seed.request(t, callerBob, http.MethodGet, target, nil)
		var refusal struct{ Code, Error string }
		_ = json.Unmarshal(refused.Body.Bytes(), &refusal)
		if refused.Code != http.StatusForbidden || refusal.Code != "DEPLOYMENT_READ_REFUSED" || !strings.Contains(refusal.Error, "GET "+route.Pattern) {
			t.Errorf("bob read GET %s as %d %s, want 403 DEPLOYMENT_READ_REFUSED naming the row", route.Pattern, refused.Code, refused.Body.String())
		}
		for _, open := range []privateCaller{callerAlice, callerSharedToken} {
			if answered := seed.request(t, open, http.MethodGet, target, nil); strings.Contains(answered.Body.String(), "DEPLOYMENT_READ_REFUSED") {
				t.Errorf("%s, an open viewer, read GET %s as %d %s", open, route.Pattern, answered.Code, answered.Body.String())
			}
		}
	}
	if !slices.Contains(walked, "/api/v1/priorities") {
		t.Fatalf("the walk read the deployment's rows %q, without GET /api/v1/priorities", walked)
	}
	if !slices.Equal(ownLogin, []string{"/api/v1/machine-logins"}) {
		t.Fatalf("the walk left out %q, want GET /api/v1/machine-logins alone", ownLogin)
	}
}

// TestTheReadScopePutsTheReaderInTheRequest: a GET row whose classes need the viewer runs its
// handler with the reader withinReadScope resolved in the request's context, the one readerOf
// answers; a row that needs none runs it with none, and resolves nothing. Each row is read as bob,
// a restricted member, except a row of the deployment, which refuses him (and is held to that) and
// is read as alice, an open member who sees PUB.
func TestTheReadScopePutsTheReaderInTheRequest(t *testing.T) {
	seed := seedPrivateProject(t)
	for _, test := range []struct {
		classes []readclass.Class
		wants   bool
	}{
		{[]readclass.Class{readclass.Project}, true},
		{[]readclass.Class{readclass.Graph}, true},
		{[]readclass.Class{readclass.People}, true},
		{[]readclass.Class{readclass.Agents}, true},
		{[]readclass.Class{readclass.Deployment, readclass.Project}, true},
		{[]readclass.Class{readclass.Caller, readclass.Project}, true},
		{[]readclass.Class{readclass.None}, false},
		{[]readclass.Class{readclass.Caller}, false},
		{[]readclass.Class{readclass.TestHook}, false},
	} {
		route := apiRoute{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets"}, Reads: test.classes}
		var held bool
		var seen []string
		handler := seed.server.withinReadScope(route, func(w http.ResponseWriter, r *http.Request) {
			reader, ok := readerFromContext(r.Context())
			held = ok
			if ok && reader.Viewer != nil {
				seen = reader.Viewer.Sees
			}
			w.WriteHeader(http.StatusNoContent)
		})
		caller, sees := callerBob, []string{seedPrivate}
		if slices.Contains(test.classes, readclass.Deployment) {
			refused := httptest.NewRecorder()
			handler(refused, seed.readerRequest(t, callerBob))
			if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), codeDeploymentReadRefused) {
				t.Errorf("a row of %q answered bob, a restricted member, %d %s, want 403 %s", test.classes, refused.Code, refused.Body.String(), codeDeploymentReadRefused)
			}
			caller, sees = callerAlice, []string{seedPublic}
		}
		held, seen = false, nil
		recorder := httptest.NewRecorder()
		handler(recorder, seed.readerRequest(t, caller))
		if recorder.Code != http.StatusNoContent {
			t.Errorf("a row of %q answered %s %d %s, want its handler run", test.classes, caller, recorder.Code, recorder.Body.String())
			continue
		}
		if held != test.wants {
			t.Errorf("a row of %q ran its handler with a reader in the request: %t, want %t", test.classes, held, test.wants)
		}
		if test.wants && !slices.Equal(seen, sees) {
			t.Errorf("a row of %q handed its handler %s's viewer seeing %q, want %q", test.classes, caller, seen, sees)
		}
	}
}
