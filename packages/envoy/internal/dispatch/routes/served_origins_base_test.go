package routes

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// This file names only what the router held before DISPATCH_SERVED_ORIGINS and
// DISPATCH_FORMER_ORIGINS existed, so TestAServerWithNoListsAnswersAsTheBaseDoes also builds and
// runs against the base commit's router, beside host_origins_test.go's constants alone.

// signedInWrite is POST /auth/logout to host from a browser signed in afresh on rig, carrying
// origin as its Origin ("" for none) and header: the browser write the cookie origin check judges.
// A write the check lets by signs the person out, 200; one it refuses is 403 invalid request
// origin.
func signedInWrite(t *testing.T, rig *signInRig, host, origin string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	cookie := sessionCookie(rig.signIn(t, googleClaims("google-subject")))
	if cookie == nil {
		t.Fatal("the sign-in issued no session cookie")
	}
	request := httptest.NewRequest(http.MethodPost, "https://"+host+"/auth/logout", nil)
	for name, values := range header {
		request.Header[name] = values
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, request)
	return response
}

// requireWrite requires response, a write of signedInWrite's described by what, to be served
// (admitted) or refused 403 invalid request origin.
func requireWrite(t *testing.T, what string, response *httptest.ResponseRecorder, admitted bool) {
	t.Helper()
	refused := response.Code == http.StatusForbidden && strings.Contains(response.Body.String(), "invalid request origin")
	switch {
	case admitted && response.Code != http.StatusOK:
		t.Errorf("%s: %d %s, want the write served", what, response.Code, strings.TrimSpace(response.Body.String()))
	case !admitted && !refused:
		t.Errorf("%s: %d %s, want 403 invalid request origin", what, response.Code, strings.TrimSpace(response.Body.String()))
	}
}

// signInAt runs the Google sign-in a browser starts at host on rig: GET /auth/start there, the
// issuer's sign-in, and the callback at the redirect_uri the start sent the issuer, where the
// browser follows it. It answers that redirect_uri and the session cookie the callback set.
func signInAt(t *testing.T, rig *signInRig, host string) (string, *http.Cookie) {
	t.Helper()
	rig.issuer.SignInAs(googleClaims("google-subject"))
	location, state := rig.start(t, "https://"+host+"/auth/start")
	redirectURI := location.Query().Get("redirect_uri")
	code, returned := rig.issuer.Authorize(t, location.String())
	callback := httptest.NewRequest(http.MethodGet, redirectURI+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned), nil)
	callback.AddCookie(state)
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, callback)
	if response.Code != http.StatusFound {
		t.Fatalf("the callback at %s answered %d %s, want the sign-in's 302", redirectURI, response.Code, strings.TrimSpace(response.Body.String()))
	}
	cookie := sessionCookie(response)
	if cookie == nil {
		t.Fatalf("the callback at %s set no session cookie", redirectURI)
	}
	return redirectURI, cookie
}

// TestAServerWithNoListsAnswersAsTheBaseDoes (c-l-served 6): a server with neither
// DISPATCH_SERVED_ORIGINS nor DISPATCH_FORMER_ORIGINS answers the requests of c-l-served's (1) to
// (3) as the base commit's server answers them, which knows no address but DISPATCH_SERVER_URL's:
// a browser write at another Host is judged as the canonical origin's, whatever its Host and
// X-Forwarded-Host say, and a sign-in started at another Host comes back to the canonical
// callback, where its session cookie is the canonical one's, host-only. It names nothing the base
// lacks, so it runs against the base commit's router unchanged.
func TestAServerWithNoListsAnswersAsTheBaseDoes(t *testing.T) {
	rig := newSignInRigFor(t, tableCanonical, identity.DomainRule{Domain: rigDomain}, rigDomain)
	for _, write := range []struct {
		host, origin string
		header       http.Header
		admitted     bool
	}{
		{"b.example.test", tableServed, nil, false},
		{"a.example.test", tableCanonical, nil, true},
		{"B.EXAMPLE.TEST", tableServed, nil, false},
		{"b.example.test", tableCanonical, nil, true},
		{"a.example.test", tableServed, http.Header{"X-Forwarded-Host": {"b.example.test"}}, false},
	} {
		requireWrite(t, "a cookie write with Origin "+write.origin+" to Host "+write.host+" "+headerNames(write.header),
			signedInWrite(t, rig, write.host, write.origin, write.header), write.admitted)
	}
	redirectURI, cookie := signInAt(t, rig, "b.example.test")
	if redirectURI != tableCanonical+"/auth/callback" {
		t.Errorf("GET /auth/start at Host b.example.test: redirect_uri %q, want the canonical callback %q", redirectURI, tableCanonical+"/auth/callback")
	}
	if cookie.Name != "dsession" || cookie.Domain != "" {
		t.Errorf("the sign-in's session cookie is %s with Domain %q, want dsession, host-only", cookie.Name, cookie.Domain)
	}
}

// headerNames says which headers beside the cookie a row's write carries.
func headerNames(header http.Header) string {
	if len(header) == 0 {
		return "and no other header"
	}
	var named []string
	for name, values := range header {
		named = append(named, name+": "+strings.Join(values, ", "))
	}
	return "and " + strings.Join(named, "; ")
}
