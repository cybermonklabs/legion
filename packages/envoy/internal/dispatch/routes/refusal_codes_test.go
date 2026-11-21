package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// codedRefusal is a refusal's body as a client reads it.
type codedRefusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// requireCodedRefusal fails unless response is status with a JSON body naming code and a message.
func requireCodedRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string) codedRefusal {
	t.Helper()
	var body codedRefusal
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != status || body.Code != code || body.Error == "" {
		t.Fatalf("%s: status=%d body=%s, want %d {code: %s, error}", what, response.Code, response.Body.String(), status, code)
	}
	return body
}

// failingSignIns is a people store whose sign-ins cannot be recorded.
type failingSignIns struct{ *memoryPeopleStore }

func (failingSignIns) SignIn(context.Context, string, string) error {
	return errors.New("the people store is down")
}

// failingSessions is a session store that cannot ensure or revoke a generation.
type failingSessions struct{ *memorySessionStore }

func (failingSessions) EnsureSession(context.Context, string) (int64, error) {
	return 0, errors.New("the session store is down")
}

func (failingSessions) RevokeSessions(context.Context, string) error {
	return errors.New("the session store is down")
}

// rebuilt is the rig's router built again with options changed by change: another process
// holding the same keys, so a sign-in the rig started completes on it.
func (r *signInRig) rebuilt(t *testing.T, change func(*AppContextOptions)) http.Handler {
	t.Helper()
	options := r.options
	change(&options)
	ctx, err := BuildAppContext(options)
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ := New(ctx)
	return handler
}

// Every refusal of the /auth routes, of the cookie-origin guard and of an unknown API path is a
// {code, error} body a client can tell apart by its code (R7, R3.2), the /auth refusals and the
// origin guard among them.
func TestAuthAndOriginRefusalsCarryACode(t *testing.T) {
	t.Run("a callback with an expired state", func(t *testing.T) {
		rig := newSignInRig(t, "")
		rig.issuer.SignInAs(personClaims(rigGroup))
		authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
		code, state := rig.issuer.Authorize(t, authURL.String())
		signed, ok := verifyOAuthState(testSigningKey, stateCookie.Value, time.Now())
		if !ok {
			t.Fatalf("the start's state cookie %q does not verify", stateCookie.Value)
		}
		signed.Exp = time.Now().Add(-time.Second).Unix()
		expired, err := signOAuthState(testSigningKey, signed)
		if err != nil {
			t.Fatalf("sign the expired state: %v", err)
		}
		stateCookie.Value = expired
		requireCodedRefusal(t, "expired state", callbackOn(rig.handler, code, state, stateCookie), http.StatusBadRequest, codeAuthStateInvalid)
	})

	t.Run("a callback with no state cookie", func(t *testing.T) {
		rig := newSignInRig(t, "")
		requireCodedRefusal(t, "no state cookie", callbackOn(rig.handler, "a-code", "a-state", nil), http.StatusBadRequest, codeAuthStateInvalid)
	})

	t.Run("a callback missing its code and state", func(t *testing.T) {
		rig := newSignInRig(t, "")
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/callback", nil))
		requireCodedRefusal(t, "no code or state", response, http.StatusBadRequest, codeAuthParamsInvalid)
	})

	t.Run("a code the issuer refuses", func(t *testing.T) {
		rig := newSignInRig(t, "")
		rig.issuer.SignInAs(personClaims(rigGroup))
		authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
		callback := callbackOn(rig.handler, "a-code-the-issuer-never-issued", authURL.Query().Get("state"), stateCookie)
		requireCodedRefusal(t, "refused exchange", callback, http.StatusBadGateway, codeSigninFailed)
	})

	t.Run("a sign-in the people store cannot record", func(t *testing.T) {
		rig := newSignInRig(t, "")
		handler := rig.rebuilt(t, func(options *AppContextOptions) { options.People = failingSignIns{rig.people} })
		rig.issuer.SignInAs(personClaims(rigGroup))
		authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
		code, state := rig.issuer.Authorize(t, authURL.String())
		requireCodedRefusal(t, "unrecorded sign-in", callbackOn(handler, code, state, stateCookie), http.StatusInternalServerError, codeSigninRecordFailed)
	})

	t.Run("a sign-in whose session cannot be ensured", func(t *testing.T) {
		rig := newSignInRig(t, "")
		handler := rig.rebuilt(t, func(options *AppContextOptions) { options.Sessions = failingSessions{rig.sessions} })
		rig.issuer.SignInAs(personClaims(rigGroup))
		authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
		code, state := rig.issuer.Authorize(t, authURL.String())
		requireCodedRefusal(t, "no session generation", callbackOn(handler, code, state, stateCookie), http.StatusInternalServerError, codeSessionFailed)
	})

	t.Run("a logout whose sessions cannot be revoked", func(t *testing.T) {
		people := newMemoryPeople()
		if err := people.SignIn(context.Background(), "sami@d.example", "refresh-token"); err != nil {
			t.Fatalf("record sign-in: %v", err)
		}
		sessions := failingSessions{&memorySessionStore{generations: map[string]int64{"sami@d.example": 0}}}
		ctx, err := BuildAppContext(AppContextOptions{
			SigningKey: testSigningKey,
			People:     people,
			Sessions:   sessions,
			Identity:   identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
			Lifetime:   t.Context(),
		})
		if err != nil {
			t.Fatalf("build context: %v", err)
		}
		standInEventLog(ctx, 0)
		handler, _ := New(ctx)
		cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("sami@d.example", 0, testSigningKey, true))
		if err != nil {
			t.Fatalf("parse session cookie: %v", err)
		}
		logout := httptest.NewRequest(http.MethodPost, "http://dispatch.test/auth/logout", nil)
		logout.Header.Set("Origin", "http://dispatch.test")
		logout.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, logout)
		requireCodedRefusal(t, "unrevoked logout", response, http.StatusInternalServerError, codeSessionFailed)
	})

	t.Run("an anonymous whoami and logout", func(t *testing.T) {
		handler, _, _ := cookieRouter(t, "")
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path := map[string]string{http.MethodGet: "/auth/whoami", http.MethodPost: "/auth/logout"}[method]
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, "http://dispatch.test"+path, nil))
			requireCodedRefusal(t, method+" "+path, response, http.StatusUnauthorized, "NO_IDENTITY")
		}
	})

	t.Run("a sign-in on a server with none configured", func(t *testing.T) {
		handler, _ := newTestRouter(t)
		for _, target := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test"+target, nil))
			requireCodedRefusal(t, target, response, http.StatusServiceUnavailable, "SIGNIN_UNCONFIGURED")
		}
	})

	t.Run("a cross-origin cookie write", func(t *testing.T) {
		handler, cookie, _ := cookieRouter(t, "https://dispatch.example")
		for _, path := range []string{"/auth/logout", "/api/v1/issues", "/api/v2/issues"} {
			request := httptest.NewRequest(http.MethodPost, "https://dispatch.example"+path, strings.NewReader(`{}`))
			request.AddCookie(cookie)
			request.Header.Set("Origin", "https://other.dispatch.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			requireCodedRefusal(t, "foreign-origin POST "+path, response, http.StatusForbidden, "ORIGIN_REFUSED")
		}
	})

	t.Run("an unknown path under each API version", func(t *testing.T) {
		handler, _ := newTestRouter(t)
		for _, test := range []struct{ path, code, hint string }{
			{"/api/v2/does-not-exist", "NO_ROUTE", "GET /api/v2 lists every route"},
			{"/api/v1/does-not-exist", "NOT_FOUND", "GET /api/v1 lists every route"},
			{"/v1/issues", "NOT_FOUND", "GET /api/v1 lists every route"},
		} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test"+test.path, nil))
			body := requireCodedRefusal(t, "GET "+test.path, response, http.StatusNotFound, test.code)
			if body.Error != "no route for GET "+test.path || body.Hint != test.hint {
				t.Fatalf("GET %s: body=%s, want error %q and hint %q", test.path, response.Body.String(), "no route for GET "+test.path, test.hint)
			}
		}
	})
}
