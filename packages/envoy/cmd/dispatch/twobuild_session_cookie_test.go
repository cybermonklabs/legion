//go:build twobuild

// The session-cookie half of the two-build suite: a person's browser session where the cookie is
// Secure, while the previous build and this tree serve one database behind one origin, as a rolling
// update and a rollback run them (twobuild_harness_test.go holds the harness). Every other row of
// the suite and of the browser suite starts its builds under DISPATCH_INSECURE_COOKIE, under which
// the session cookie's name never changes, so this is the one row that reads a Secure cookie
// across a version boundary.

package main

import (
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// A person who signs in at either build, with the cookie Secure, is signed in at both, and a
// sign-out at either signs them out at both: each build reads every session cookie the other
// issues, and clears what the other may have left. That is cmd/dispatch/README.md's rule that every
// release keeps the build before it working, for the browser's session; a build that issues a
// session cookie name the previous build does not read breaks it (a person who signs in on a new
// pod is signed out on the old ones, and a rollback signs out everyone who signed in since). The
// browser is modelled by hand from each answer's Set-Cookie lines (sessionBrowser), since a Secure
// cookie over the test's plain http is one a cookie jar would not send.
//
// Control: run this tree with a build before it that reads no session under the name this tree
// issues (DISPATCH_PREVIOUS_BUILD of a commit before that name is read), and the row of the sign-in
// at this tree fails at the previous build.
func TestASessionEitherBuildIssuesIsReadByBothWhereTheCookieIsSecure(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	// The sign-in of issuer's code flow under the group rule, as the multiprocess sign-in rows run
	// it, with no DISPATCH_INSECURE_COOKIE: the cookie is Secure.
	env := map[string]string{
		"DISPATCH_IDENTITY":             "cookie",
		"DISPATCH_SIGNIN_ISSUER":        issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"DISPATCH_SIGNIN_GROUP":         cookieGroup,
	}
	b := newTwoBuilds(t)
	previous, current := b.startPrevious(t, env), b.startCurrent(t, env)
	both := []*dispatchProcess{previous, current}

	for _, step := range []struct {
		name string
		// holding, when set, is the build the browser signed in at first, so it holds that build's
		// session when it signs in at signIn.
		holding, signIn *dispatchProcess
		// signOut, when set, signs out at that build after the sign-in.
		signOut *dispatchProcess
	}{
		{name: "a sign-in at the previous build", signIn: previous},
		{name: "a sign-in at this build", signIn: current},
		{name: "a sign-in at this build over the previous build's session", holding: previous, signIn: current},
		{name: "a sign-in at the previous build over this build's session", holding: current, signIn: previous},
		{name: "a sign-in at this build, then a sign-out at the previous build", signIn: current, signOut: previous},
		{name: "a sign-in at the previous build, then a sign-out at this build", signIn: previous, signOut: current},
	} {
		t.Run(step.name, func(t *testing.T) {
			browser := sessionBrowser{}
			if step.holding != nil {
				browser.signIn(t, step.holding, issuer)
			}
			browser.signIn(t, step.signIn, issuer)
			for _, process := range both {
				if answer, status := browser.whoami(t, process); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
					t.Errorf("the session %s signed in (the browser holds %v) at the %s: whoami %d %s, want 200 naming %s",
						b.buildName(step.signIn), browser.names(), b.buildName(process), status, answer, cookiePerson)
				}
			}
			if step.signOut == nil {
				return
			}
			if status := browser.signOut(t, step.signOut); status != http.StatusOK {
				t.Fatalf("the sign-out at the %s answered %d, want 200", b.buildName(step.signOut), status)
			}
			for _, process := range both {
				if answer, status := browser.whoami(t, process); status != http.StatusUnauthorized {
					t.Errorf("after the sign-out at the %s (the browser holds %v), the %s answered whoami %d %s, want 401",
						b.buildName(step.signOut), browser.names(), b.buildName(process), status, answer)
				}
			}
		})
	}
}

// sessionBrowser is the session cookies one browser holds for the pair's one origin, by name: what
// each answer's Set-Cookie lines leave, a line with a value setting its name and a clear (an empty
// value or Max-Age=0) removing it, as a browser keeps a host-only cookie of either build.
type sessionBrowser map[string]string

// take keeps what the response's Set-Cookie lines of the session cookie leave.
func (b sessionBrowser) take(response *http.Response) {
	for _, cookie := range response.Cookies() {
		if cookie.Name != hostSessionCookie && cookie.Name != plainSessionCookie {
			continue
		}
		if cookie.Value == "" || cookie.MaxAge < 0 {
			delete(b, cookie.Name)
		} else {
			b[cookie.Name] = cookie.Value
		}
	}
}

// names is the names of the cookies the browser holds, sorted.
func (b sessionBrowser) names() []string {
	return slices.Sorted(maps.Keys(b))
}

// request is method target from the browser, carrying every cookie it holds.
func (b sessionBrowser) request(t *testing.T, method, target string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	for _, name := range b.names() {
		request.AddCookie(&http.Cookie{Name: name, Value: b[name]})
	}
	if method != http.MethodGet {
		parsed, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parse %s: %v", target, err)
		}
		request.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return response
}

// signIn signs cookiePerson in at p through the issuer's code flow, as signInThrough does, from
// this browser, and keeps the session cookies the callback leaves.
func (b sessionBrowser) signIn(t *testing.T, p *dispatchProcess, issuer *oidctest.Issuer) {
	t.Helper()
	issuer.SignInAs(map[string]any{
		"sub":              "alice-subject",
		"cognito:username": "ExampleIdP_" + cookiePerson,
		"cognito:groups":   []string{cookieGroup},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})
	started := browserGet(t, p.url("/auth/start"), nil)
	_ = started.Body.Close()
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" && cookie.Value != "" {
			state = cookie
		}
	}
	if started.StatusCode != http.StatusFound || state == nil {
		t.Fatalf("/auth/start answered %d with cookies %v, want a redirect with the state cookie", started.StatusCode, started.Cookies())
	}
	code, returned := issuer.Authorize(t, started.Header.Get("Location"))
	// The callback reads the state cookie, which the browser holds beside its session.
	callback := b.withState(t, p, state, code, returned)
	defer callback.Body.Close()
	if callback.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(callback.Body)
		t.Fatalf("/auth/callback answered %d, want 302: %s", callback.StatusCode, body)
	}
	b.take(callback)
	if len(b) == 0 {
		t.Fatalf("/auth/callback left the browser no session cookie: %v", callback.Cookies())
	}
}

// withState is the callback of code and returned at p from this browser, carrying the state cookie
// beside every session cookie the browser holds.
func (b sessionBrowser) withState(t *testing.T, p *dispatchProcess, state *http.Cookie, code, returned string) *http.Response {
	t.Helper()
	held := sessionBrowser{state.Name: state.Value}
	maps.Copy(held, b)
	return held.request(t, http.MethodGet, p.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)))
}

// whoami asks p who the browser is, answering the body and the status.
func (b sessionBrowser) whoami(t *testing.T, p *dispatchProcess) (string, int) {
	t.Helper()
	response := b.request(t, http.MethodGet, p.url("/auth/whoami"))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body), response.StatusCode
}

// signOut signs the browser out at p and keeps what the answer leaves, answering its status.
func (b sessionBrowser) signOut(t *testing.T, p *dispatchProcess) int {
	t.Helper()
	response := b.request(t, http.MethodPost, p.url("/auth/logout"))
	defer response.Body.Close()
	b.take(response)
	return response.StatusCode
}
