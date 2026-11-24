package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestReaderOfAnswersEachCallersViewer: readerOf answers each caller of verification v16 with the
// viewer access.Resolve gives it, the credential dropped; a reader a layer already put in the
// request's context is answered as it is, with no second resolution; and a credential that does
// not resolve is refused as every gate refuses one.
func TestReaderOfAnswersEachCallersViewer(t *testing.T) {
	seed := seedPrivateProject(t)
	carol := seedCarol
	for _, want := range []struct {
		caller privateCaller
		viewer model.Viewer
	}{
		{callerAlice, model.Viewer{Person: seedAlice, Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerBob, model.Viewer{Person: seedBob, Restricted: true, Sees: []string{seedPrivate}, AnyPrivate: true}},
		{callerCarol, model.Viewer{Person: seedCarol, Open: true, Sees: []string{seedPrivate, seedPublic}, AnyPrivate: true}},
		{callerCarolPrivToken, model.Viewer{Person: seedCarol, Open: true, Lists: []string{seedPrivate}, Sees: []string{seedPrivate, seedPublic}, AnyPrivate: true}},
		{callerAlicePrivToken, model.Viewer{Person: seedAlice, Open: true, Lists: []string{seedPrivate}, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerBobPrivToken, model.Viewer{Person: seedBob, Restricted: true, Lists: []string{seedPrivate}, Sees: []string{seedPrivate}, AnyPrivate: true}},
		{callerCarolToken, model.Viewer{Person: seedCarol, Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
		{callerSharedToken, model.Viewer{Open: true, Sees: []string{seedPublic}, AnyPrivate: true}},
	} {
		request := seed.readerRequest(t, want.caller)
		recorder := httptest.NewRecorder()
		reader, ok := seed.server.readerOf(recorder, request)
		if !ok {
			t.Fatalf("readerOf refused %s: %d %s", want.caller, recorder.Code, recorder.Body.String())
		}
		if reader.Credential != nil {
			t.Errorf("readerOf answered %s with its credential %+v: a reader carries none", want.caller, reader.Credential)
		}
		if reader.Viewer == nil {
			t.Fatalf("readerOf answered %s with no viewer", want.caller)
		}
		if !reflect.DeepEqual(*reader.Viewer, want.viewer) {
			t.Errorf("readerOf answered %s the viewer %+v, want %+v", want.caller, *reader.Viewer, want.viewer)
		}
		if want.caller == callerCarolToken && (reader.Owner == nil || *reader.Owner != carol) {
			t.Errorf("readerOf answered %s as %+v, want the token's person", want.caller, reader)
		}
	}

	kept := model.Actor{Kind: "user", ID: "kept", Viewer: &model.Viewer{Person: "kept", Sees: []string{}}}
	request := seed.readerRequest(t, callerAlice)
	request = request.WithContext(withReader(request.Context(), kept))
	reader, ok := seed.server.readerOf(httptest.NewRecorder(), request)
	if !ok || reader.ID != "kept" || reader.Viewer != kept.Viewer {
		t.Errorf("readerOf over a request whose context holds a reader answered %+v, %t; want that reader as it is", reader, ok)
	}

	// A token nobody minted: a minted one with its last character changed, so it has a minted
	// token's shape and resolves to no row.
	minted := seed.tokens[callerCarolToken]
	unminted := minted[:len(minted)-1] + map[bool]string{true: "B", false: "A"}[strings.HasSuffix(minted, "A")]
	refused := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	refused.Header.Set("Authorization", "Bearer "+unminted)
	recorder := httptest.NewRecorder()
	if _, ok := seed.server.readerOf(recorder, refused); ok || recorder.Code != http.StatusUnauthorized {
		t.Errorf("readerOf over a token nobody minted answered %t with %d %s; want 401", ok, recorder.Code, recorder.Body.String())
	}
}

// TestAServerWithNoStoreResolvesTheClosedViewer: a server built with no store - a router test's,
// or a deployment whose database is not yet configured - resolves a reader for a person at the
// browser and for the shared bearer without a panic, and each reader holds the closed viewer: a
// viewer that sees no project and no party, is neither open nor all, and reads as though a project
// were private. So every read that binds it answers nothing, and no write reaches an object its
// caller may not see, while the caller itself is still the one its credential names.
func TestAServerWithNoStoreResolvesTheClosedViewer(t *testing.T) {
	served := directServer(Deps{Identity: &endableIdentity{}, AgentTokens: sharedAgentTokens(t, "agent-token")})
	cookie := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	bearer := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	bearer.Header.Set("Authorization", "Bearer agent-token")
	for _, test := range []struct {
		name    string
		request *http.Request
		id      string
	}{
		{"a person at the browser", cookie, "alice@d.example"},
		{"the shared bearer", bearer, ""},
	} {
		recorder := httptest.NewRecorder()
		reader, ok := served.readerOf(recorder, test.request)
		if !ok {
			t.Fatalf("%s: readerOf refused: %d %s", test.name, recorder.Code, recorder.Body.String())
		}
		if reader.ID != test.id || reader.Credential != nil {
			t.Errorf("%s: readerOf answered %+v, want the caller %q with no credential", test.name, reader, test.id)
		}
		if reader.Viewer == nil {
			t.Fatalf("%s: readerOf answered no viewer, which a read binds as the closed viewer only by accident", test.name)
		}
		if want := (model.Viewer{Sees: []string{}, AnyPrivate: true}); !reflect.DeepEqual(*reader.Viewer, want) {
			t.Errorf("%s: readerOf answered the viewer %+v, want the closed viewer %+v", test.name, *reader.Viewer, want)
		}
	}
}
