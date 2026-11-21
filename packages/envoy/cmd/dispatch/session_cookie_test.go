package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The session cookie's two names: `__Host-dsession`, which a browser takes only from this host
// itself, Secure, with Path=/ and no Domain, and `dsession`.
const (
	hostSessionCookie  = "__Host-dsession"
	plainSessionCookie = "dsession"
)

// issuedSessionCookie is the name a sign-in issues the session cookie under, at the outside origin
// when outside is true and at the canonical origin otherwise. At the outside origin, which no
// released build has served, it follows that origin's scheme: `__Host-dsession` at the walk's https
// outside origin (passkeyOrigin), whatever DISPATCH_INSECURE_COOKIE says. At the canonical origin it
// is `dsession` under every setting, as every earlier build issues it, so the build before this one
// reads it in a roll and after a rollback.
func issuedSessionCookie(outside bool) string {
	if outside {
		return hostSessionCookie
	}
	return plainSessionCookie
}

// issuedSecure is whether the session a sign-in issues is Secure: at the outside origin as its
// https scheme says, and at the canonical origin as DISPATCH_INSECURE_COOKIE says.
func issuedSecure(outside, insecure bool) bool {
	return outside || !insecure
}

// otherSessionCookie is the session cookie's name that name is not.
func otherSessionCookie(name string) string {
	if name == hostSessionCookie {
		return plainSessionCookie
	}
	return hostSessionCookie
}

// isSessionCookie reports whether cookie, of a response, sets the session cookie with a value under
// either of its names: what a browser keeps and sends back. Which name each sign-in sets is the
// walk's to check (TestEverySessionCookieTheServerWritesIsHostOnlyAtTheRoot).
func isSessionCookie(cookie *http.Cookie) bool {
	return (cookie.Name == hostSessionCookie || cookie.Name == plainSessionCookie) && cookie.Value != "" && cookie.MaxAge >= 0
}

// cookieWalk is one setting's servers, each writing the session cookie: the Google sign-in at the
// canonical origin and the passkey sign-ins of passkeyRig over a database, and a dev sign-in
// server.
type cookieWalk struct {
	insecure bool
	rig      *passkeyRig
	dev      http.Handler
}

// sessionCookieWriter is a route that writes the session cookie, at one origin that serves it, and
// how the walk makes it write as a browser holding prior, another person's session under the name
// the route does not issue there.
type sessionCookieWriter struct {
	// handler is the router method that serves the route.
	handler string
	route   string
	// outside is whether the row runs at the outside origin (else the canonical origin).
	outside bool
	// person is whom a sign-in signs in, "" for the sign-out.
	person string
	write  func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder
	// whoami answers GET /auth/whoami at the row's server and origin from a browser holding jar.
	whoami func(w *cookieWalk, jar []*http.Cookie) *httptest.ResponseRecorder
}

// sessionCookieWriters is every route that writes the session cookie, at every origin that serves
// it, each by the router method that serves it: the census below (sessionCookieWriterRoots) reads
// those methods from the code, so a writer added without a row here fails the walk. The passkey
// ceremonies are the outside origin's alone; Google's callback and the dev sign-in are the
// canonical origin's alone (the outside origin serves no Google sign-in, and the dev sign-in has
// its host fence); the sign-out is served at both.
var sessionCookieWriters = []sessionCookieWriter{
	{handler: "authCallback", route: "GET /auth/callback", person: "carol@example.com", write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		return w.googleSignIn(t, "carol@example.com", prior)
	}, whoami: canonicalWhoami},
	{handler: "authDevSignIn", route: "GET /auth/_dev/signin", person: "dave@example.com", write: func(_ *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		return devSignInRequest(w.dev, http.MethodGet, "/auth/_dev/signin?login="+url.QueryEscape("dave@example.com"), "", func(r *http.Request) { r.AddCookie(prior) })
	}, whoami: func(w *cookieWalk, jar []*http.Cookie) *httptest.ResponseRecorder {
		return devSignInRequest(w.dev, http.MethodGet, "/auth/whoami", "", func(r *http.Request) {
			for _, cookie := range jar {
				r.AddCookie(cookie)
			}
		})
	}},
	{handler: "passkeyRegisterFinish", route: "POST /auth/passkey/register/finish", outside: true, person: outsiderEmail, write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		return w.enroll(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), prior)
	}, whoami: outsideWhoami},
	{handler: "passkeyLoginFinish", route: "POST /auth/passkey/login/finish", outside: true, person: outsiderEmail, write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
		if enrolled := w.enroll(t, device); enrolled.Code != http.StatusOK {
			t.Fatalf("the enrollment answered %d %s, want 200", enrolled.Code, enrolled.Body.String())
		}
		begun := call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
		if begun.Code != http.StatusOK {
			t.Fatalf("the sign-in's begin answered %d %s, want 200", begun.Code, begun.Body.String())
		}
		return call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", device.assert(t, begun.Body.Bytes()), w.ceremonyCookie(begun), prior)
	}, whoami: outsideWhoami},
	{handler: "authLogout", route: "POST /auth/logout", write: func(t *testing.T, w *cookieWalk, _ *http.Cookie) *httptest.ResponseRecorder {
		// Bob's browser holds his session under both names: the one his sign-in issued, and the
		// other with the same value, as a browser that signed in at two builds may.
		session := sessionCookieOf(t, "bob's Google sign-in", w.googleSignIn(t, "bob@example.com"))
		return call(w.rig.handler, canonicalOrigin, http.MethodPost, "/auth/logout", nil, session, &http.Cookie{Name: otherSessionCookie(session.Name), Value: session.Value})
	}, whoami: canonicalWhoami},
	{handler: "authLogout", route: "POST /auth/logout", outside: true, write: func(t *testing.T, w *cookieWalk, _ *http.Cookie) *httptest.ResponseRecorder {
		// The outsider's browser holds the session their enrollment issued, and a copy under the
		// other name.
		session := sessionCookieOf(t, "the outsider's enrollment", w.enroll(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)))
		return call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/logout", nil, session, &http.Cookie{Name: otherSessionCookie(session.Name), Value: session.Value})
	}, whoami: outsideWhoami},
}

// canonicalWhoami and outsideWhoami are GET /auth/whoami at the rig's server, at the canonical and
// at the outside origin, from a browser holding jar.
func canonicalWhoami(w *cookieWalk, jar []*http.Cookie) *httptest.ResponseRecorder {
	return call(w.rig.handler, canonicalOrigin, http.MethodGet, "/auth/whoami", nil, jar...)
}

func outsideWhoami(w *cookieWalk, jar []*http.Cookie) *httptest.ResponseRecorder {
	return call(w.rig.handler, passkeyOrigin, http.MethodGet, "/auth/whoami", nil, jar...)
}

// googleSignIn runs a whole Google sign-in through the rig's server as email, a person of the
// domain, from a browser carrying cookies, and answers the callback's answer.
func (w *cookieWalk) googleSignIn(t *testing.T, email string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return codeFlowSignIn(t, w.rig.handler, w.rig.pool, map[string]any{
		"sub": "google-" + email, "email": email, "email_verified": true, "hd": "example.com",
	}, cookies...)
}

// enroll runs a whole enrollment of outsiderEmail's device with a fresh link from alice through the
// rig's server, the finish from a browser carrying cookies, and answers the finish.
func (w *cookieWalk) enroll(t *testing.T, device *virtualAuthenticator, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	begun := w.rig.beginEnrollment(w.rig.handler, w.rig.invite(t, outsiderEmail))
	if begun.Code != http.StatusOK {
		t.Fatalf("the enrollment's begin answered %d %s, want 200", begun.Code, begun.Body.String())
	}
	return call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/register/finish", device.register(t, begun.Body.Bytes()), append([]*http.Cookie{w.ceremonyCookie(begun)}, cookies...)...)
}

// ceremonyCookie is the passkey ceremony cookie a begin set, under the name of the walk's setting.
func (w *cookieWalk) ceremonyCookie(response *httptest.ResponseRecorder) *http.Cookie {
	if !w.insecure {
		return ceremonyCookie(response)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "dispatch_passkey_ceremony" && cookie.Value != "" {
			return cookie
		}
	}
	return nil
}

// Every Set-Cookie of the session cookie the server writes, under either name, carries Path=/ and
// no Domain, HttpOnly and SameSite=Strict, and `__Host-dsession` carries Secure (a browser refuses
// it without, and a clear of it so refused leaves it standing). The walk drives each route that
// writes it, at every origin that serves it, under each setting, from a browser holding another
// person's session under the name the route does not issue there:
//   - a sign-in sets one session, under the name its origin issues (issuedSessionCookie):
//     `dsession` at the canonical origin (Google's callback, the dev sign-in), Secure as the
//     setting says; `__Host-dsession`, Secure, at the https outside origin (a passkey enrollment or
//     sign-in), whatever the setting says. It clears the other name, and the browser, keeping what
//     those lines leave, is signed in as the person who signed in, not as the one whose cookie it
//     held;
//   - the sign-out clears both names, at either origin.
//
// Google's sign-in is the canonical origin's alone, and the outside origin's host at another port
// is no canonical origin: a browser sends that host's cookies there, and the router reads and
// issues them there by the outside origin's one name. So at the outside host on another port
// Google's start and callback answer 404 and set no session cookie, a callback carrying a code the
// issuer gave for a sign-in begun at the canonical origin included.
//
// The census reads the writers from the code: the functions of the module (its non-test files)
// that reach a session-cookie writer of package auth, called or passed as a value, and that
// nothing else of their package calls - the router's handlers. They are this walk's handlers, so a
// writer added without a row (a sliding session's re-issue, say) fails here, and so does any code
// outside package auth that spells the cookie's name, as a cookie built by hand would. A writer in
// package auth is one of its package functions, which the census follows; a method of package auth
// that reaches the cookie's names fails the census instead, since a call of a method through a
// value names no package it could follow. The census's own control plants one of each. Which
// origins serve a handler is the walk's rows to say: a handler a later change serves at another
// origin needs a row there too.
//
// Controls: issue `__Host-dsession` without Secure, and every outside sign-in row fails; issue
// `__Host-dsession` at the canonical origin, and its Secure sign-in rows fail; issue `dsession` at
// the outside origin, and its sign-in rows fail; let the outside origin's sign-ins skip the outside
// mark, and their rows fail; leave out the clear of the other name, and every sign-in row fails;
// clear one name alone at the sign-out, and its rows fail; refuse Google's sign-in at the outside
// origin by host and port (atOutside), and the row of the outside host's other port fails, its
// callback signing the person in with `__Host-dsession`.
func TestEverySessionCookieTheServerWritesIsHostOnlyAtTheRoot(t *testing.T) {
	root := filepath.Join("..", "..")
	var want []string
	for _, writer := range sessionCookieWriters {
		want = append(want, "internal/dispatch/routes."+writer.handler)
	}
	slices.Sort(want)
	want = slices.Compact(want)
	t.Run("census control", func(t *testing.T) {
		planted := map[string]string{
			"internal/dispatch/routes": "package routes\nimport \"github.com/sjawhar/envoy/internal/dispatch/auth\"\n" +
				"func (r *router) plantedRefresh(w http.ResponseWriter, req *http.Request) { r.issueSession(w, req, \"\", \"\") }\n" +
				"func plantedCookie(w http.ResponseWriter) { http.SetCookie(w, &http.Cookie{Name: \"__Host-dsession\", Value: \"x\"}) }\n" +
				"func (r *router) plantedLiteralWriter(w http.ResponseWriter) { auth.RefreshSessionCookie(w.Header(), \"x\") }\n" +
				"func (r *router) plantedRenew(w http.ResponseWriter, c auth.Cookies) { c.Renew(w.Header(), \"x\") }\n",
			"internal/dispatch/api": "package api\nimport \"github.com/sjawhar/envoy/internal/dispatch/auth\"\n" +
				"func plantedReissue(h http.Header) { h.Add(\"Set-Cookie\", auth.IssueSessionCookie(\"x\", 0, auth.SigningKeys{}, true)) }\n" +
				"var plantedHook = auth.ClearSessionCookies\n",
			// A writer of package auth that spells the name in a literal, and a method of a new type
			// of package auth that wraps a writer, each called from a routes handler above.
			"internal/dispatch/auth": "package auth\n" +
				"func RefreshSessionCookie(h http.Header, v string) { h.Add(\"Set-Cookie\", \"__Host-dsession=\"+v+\"; Path=/; Secure\") }\n" +
				"type Cookies struct{ Keys SigningKeys; Secure bool }\n" +
				"func (c Cookies) Renew(h http.Header, email string) { ClearSessionCookies(context.Background(), h, c.Secure) }\n",
		}
		got := sessionCookieWriterRoots(t, root, planted)
		wantPlanted := slices.Sorted(slices.Values(append(slices.Clone(want),
			"internal/dispatch/api.plantedHook", "internal/dispatch/api.plantedReissue",
			"internal/dispatch/auth.Renew is a method",
			`internal/dispatch/routes.plantedCookie spells "__Host-dsession"`,
			"internal/dispatch/routes.plantedLiteralWriter", "internal/dispatch/routes.plantedRefresh")))
		if !slices.Equal(got, wantPlanted) {
			t.Fatalf("the census with six planted writers named %q, want %q", got, wantPlanted)
		}
	})
	if got := sessionCookieWriterRoots(t, root, nil); !slices.Equal(got, want) {
		t.Fatalf("these functions write the session cookie: %q; want exactly the walk's rows %q", got, want)
	}

	for _, insecure := range []bool{false, true} {
		setting := map[bool]string{false: "Secure", true: "DISPATCH_INSECURE_COOKIE"}[insecure]
		t.Run(setting, func(t *testing.T) {
			overrides := map[string]string{}
			if insecure {
				overrides["DISPATCH_INSECURE_COOKIE"] = "1"
			}
			devBoot, err := resolveBootConfig(devSignInEnvironment(overrides))
			if err != nil || !devBoot.DevSignIn || devBoot.InsecureCookie != insecure {
				t.Fatalf("the dev sign-in boot: DevSignIn %t, InsecureCookie %t, %v", devBoot.DevSignIn, devBoot.InsecureCookie, err)
			}
			w := &cookieWalk{
				insecure: insecure,
				rig:      newPasskeyRig(t, overrides),
				dev:      routerFor(t, devBoot, routes.AppContextOptions{ServerURL: devSignInOrigin, People: peopleStore(t), Store: storetest.Open(t)}),
			}
			if w.rig.boot.InsecureCookie != insecure {
				t.Fatalf("the passkey rig's InsecureCookie is %t, want %t", w.rig.boot.InsecureCookie, insecure)
			}
			for _, writer := range sessionCookieWriters {
				at := "canonical origin"
				if writer.outside {
					at = "outside origin"
				}
				t.Run(writer.route+" at the "+at, func(t *testing.T) {
					issued := issuedSessionCookie(writer.outside)
					other := otherSessionCookie(issued)
					prior := &http.Cookie{Name: other, Value: w.rig.alice.Value}
					response := writer.write(t, w, prior)
					lines := sessionSetCookieLines(response)
					for _, line := range lines {
						cookie, err := http.ParseSetCookie(line)
						if err != nil {
							t.Fatalf("parse %q: %v", line, err)
						}
						if cookie.Path != "/" || cookie.Domain != "" || strings.Contains(strings.ToLower(line), "domain=") || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
							t.Errorf("%s set %q, want Path=/, no Domain, HttpOnly, SameSite=Strict", writer.route, line)
						}
						if strings.HasPrefix(cookie.Name, "__Host-") && !cookie.Secure {
							t.Errorf("%s set %q, a __Host- cookie without Secure, which a browser refuses", writer.route, line)
						}
					}
					valued, cleared := sessionCookiesIn(t, lines)
					if writer.person == "" {
						if response.Code != http.StatusOK || len(valued) != 0 || !slices.Equal(cleared, []string{hostSessionCookie, plainSessionCookie}) {
							t.Errorf("%s answered %d setting %q, want both names cleared and no session", writer.route, response.Code, lines)
						}
						return
					}
					if response.Code != http.StatusOK && response.Code != http.StatusFound {
						t.Fatalf("%s answered %d %s, want a sign-in", writer.route, response.Code, response.Body.String())
					}
					if secure := issuedSecure(writer.outside, insecure); len(valued) != 1 || valued[0].Name != issued || valued[0].Secure != secure || valued[0].MaxAge != 30*24*60*60 {
						t.Errorf("%s set %q, want one session, %s, Secure %t, Max-Age=2592000", writer.route, lines, issued, secure)
					}
					if !slices.Equal(cleared, []string{other}) {
						t.Errorf("%s set %q, want the %s the browser holds cleared, and nothing else", writer.route, lines, other)
					}
					jar := browserJar(t, []*http.Cookie{prior}, lines)
					if whoami := writer.whoami(w, jar); whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"login":"`+writer.person+`"`) {
						t.Errorf("after %s, the browser holding %v answered whoami %d %s, want %s signed in", writer.route, jar, whoami.Code, whoami.Body.String(), writer.person)
					}
				})
			}
			t.Run("Google's sign-in at the outside host on another port", func(t *testing.T) {
				const otherPort = passkeyOrigin + ":8443"
				if start := call(w.rig.handler, otherPort, http.MethodGet, "/auth/start", nil); start.Code != http.StatusNotFound || len(sessionSetCookieLines(start)) != 0 {
					t.Errorf("GET %s/auth/start answered %d %s, setting %q; want 404 and no session cookie", otherPort, start.Code, start.Body.String(), sessionSetCookieLines(start))
				}
				// A code and state the issuer gave for a sign-in begun at the canonical origin,
				// brought back at the outside host's other port.
				w.rig.pool.SignInAs(map[string]any{"sub": "google-erin@example.com", "email": "erin@example.com", "email_verified": true, "hd": "example.com"})
				start := call(w.rig.handler, canonicalOrigin, http.MethodGet, "/auth/start", nil)
				if start.Code != http.StatusFound {
					t.Fatalf("GET /auth/start at the canonical origin answered %d %s, want a redirect to Google", start.Code, start.Body.String())
				}
				code, state := w.rig.pool.Authorize(t, start.Header().Get("Location"))
				callback := call(w.rig.handler, otherPort, http.MethodGet, "/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil, start.Result().Cookies()...)
				if callback.Code != http.StatusNotFound || len(sessionSetCookieLines(callback)) != 0 {
					t.Errorf("GET %s/auth/callback answered %d %s, setting %q; want 404 and no session cookie", otherPort, callback.Code, callback.Body.String(), sessionSetCookieLines(callback))
				}
			})
		})
	}
}

// browserJar is the session cookies a browser holding held keeps after the Set-Cookie lines: each
// line with a value sets its name, and each clear (an empty value, Max-Age=0) removes it.
func browserJar(t *testing.T, held []*http.Cookie, lines []string) []*http.Cookie {
	t.Helper()
	jar := map[string]string{}
	for _, cookie := range held {
		jar[cookie.Name] = cookie.Value
	}
	for _, line := range lines {
		cookie, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if cookie.Value == "" || cookie.MaxAge < 0 {
			delete(jar, cookie.Name)
		} else {
			jar[cookie.Name] = cookie.Value
		}
	}
	var kept []*http.Cookie
	for _, name := range slices.Sorted(maps.Keys(jar)) {
		kept = append(kept, &http.Cookie{Name: name, Value: jar[name]})
	}
	return kept
}

// sessionSetCookieLines is every Set-Cookie line of response naming the session cookie, under
// either name.
func sessionSetCookieLines(response *httptest.ResponseRecorder) []string {
	var lines []string
	for _, line := range response.Header().Values("Set-Cookie") {
		name, _, _ := strings.Cut(line, "=")
		if name := strings.TrimSpace(name); name == hostSessionCookie || name == plainSessionCookie {
			lines = append(lines, line)
		}
	}
	return lines
}

// sessionCookiesIn splits lines into the cookies they set with a value and the names they clear
// (an empty value with Max-Age=0), sorted.
func sessionCookiesIn(t *testing.T, lines []string) (valued []*http.Cookie, cleared []string) {
	t.Helper()
	for _, line := range lines {
		cookie, err := http.ParseSetCookie(line)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		switch {
		case cookie.Value == "" && cookie.MaxAge < 0:
			cleared = append(cleared, cookie.Name)
		case cookie.Value != "":
			valued = append(valued, cookie)
		default:
			t.Errorf("%q neither sets a session nor clears one", line)
		}
	}
	slices.Sort(cleared)
	return valued, cleared
}

// authPackage is the import path of the package whose functions build the session cookie.
const authPackage = "github.com/sjawhar/envoy/internal/dispatch/auth"

// sessionCookieReaders are package auth's functions that read the session cookie; every other
// function of it that reaches the cookie's names writes it.
var sessionCookieReaders = []string{"HasSessionCookie", "SessionFromRequest"}

// goFuncs is, for each function of one package (by name, a method's without its receiver), the
// names it calls (funcsOf), the identifiers it uses, the literals in it that spell the session
// cookie's name (spellsSessionCookie), and whether a declaration of that name is a method.
type goFuncs map[string]struct {
	calls, uses, spells map[string]bool
	method              bool
}

// sessionCookieWriterRoots is the census of the code that writes the session cookie, read from the
// non-test Go files under root (the module) with planted, each a file's source added to a
// directory, for the control. Package auth's writers are its exported functions, but the readers,
// that spell the cookie's name or reach a package-level name holding "dsession"; the census
// answers, as "<directory>.<name>", each function elsewhere that reaches one within its package and
// that nothing else of it calls, as "<directory>.<name> spells <literal>" each function elsewhere
// that spells the cookie's name, and as "<auth directory>.<name> is a method" each method of package
// auth that reaches the cookie's names, which no call through a value lets it follow. It fails the
// test when it reads no name or no writer.
func sessionCookieWriterRoots(t *testing.T, root string, planted map[string]string) []string {
	t.Helper()
	packages := map[string][]*ast.File{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		packages[filepath.ToSlash(dir)] = append(packages[filepath.ToSlash(dir)], file)
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's code under %s: %v", root, err)
	}
	for dir, source := range planted {
		file, err := parser.ParseFile(fset, dir+"/planted.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse the planted file of %s: %v", dir, err)
		}
		packages[dir] = append(packages[dir], file)
	}

	authDir := strings.TrimPrefix(authPackage, "github.com/sjawhar/envoy/")
	names := cookieNames(packages[authDir])
	if len(names) == 0 {
		t.Fatalf("the census read no package-level name holding \"dsession\" in %s (%d files)", authDir, len(packages[authDir]))
	}
	authFuncs := funcsOf(packages[authDir])
	reaching := reachers(authFuncs, func(name string) bool {
		return len(authFuncs[name].spells) > 0 || slices.ContainsFunc(slices.Collect(maps.Keys(authFuncs[name].uses)), func(used string) bool { return names[used] })
	})
	writers := map[string]bool{}
	var roots []string
	for name := range reaching {
		if slices.Contains(sessionCookieReaders, name) {
			continue
		}
		if authFuncs[name].method {
			roots = append(roots, authDir+"."+name+" is a method")
		}
		if ast.IsExported(name) {
			writers[authPackage+"."+name] = true
		}
	}
	if len(writers) == 0 {
		t.Fatalf("the census read no writer of the session cookie in %s, which reaches %q", authDir, slices.Sorted(maps.Keys(names)))
	}

	for dir, files := range packages {
		if dir == authDir {
			continue
		}
		funcs := funcsOf(files)
		reaching := reachers(funcs, func(name string) bool {
			return slices.ContainsFunc(slices.Collect(maps.Keys(funcs[name].calls)), func(called string) bool { return writers[called] })
		})
		for name := range reaching {
			calledByAnother := false
			for other := range reaching {
				calledByAnother = calledByAnother || other != name && funcs[other].calls[name]
			}
			if !calledByAnother {
				roots = append(roots, dir+"."+name)
			}
		}
		for name, function := range funcs {
			for spelled := range function.spells {
				roots = append(roots, dir+"."+name+" spells "+spelled)
			}
		}
	}
	slices.Sort(roots)
	return roots
}

// cookieNames is the package-level names of files whose value holds "dsession": a string literal
// holding it, or another such name.
func cookieNames(files []*ast.File) map[string]bool {
	names := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, file := range files {
			for _, decl := range file.Decls {
				general, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if names[name.Name] || i >= len(value.Values) {
							continue
						}
						ast.Inspect(value.Values[i], func(node ast.Node) bool {
							switch node := node.(type) {
							case *ast.BasicLit:
								if node.Kind == token.STRING && strings.Contains(node.Value, "dsession") {
									names[name.Name], grew = true, true
								}
							case *ast.Ident:
								if names[node.Name] {
									names[name.Name], grew = true, true
								}
							}
							return true
						})
					}
				}
			}
		}
	}
	return names
}

// funcsOf is the functions of a package's files (goFuncs), and each package-level variable with a
// value, as a function of its name. A name of another package, called or not (a writer passed as a
// value), is named by that package's import path, so it is told apart from a function of this one
// of the same name; a function of this package counts only where it is called, so the function that
// mounts a handler is no caller of it.
func funcsOf(files []*ast.File) goFuncs {
	funcs := goFuncs{}
	for _, file := range files {
		imports := map[string]string{}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			local := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				local = spec.Name.Name
			}
			imports[local] = path
		}
		read := func(name string, method bool, body ast.Node) {
			entry, ok := funcs[name]
			if !ok {
				entry.calls, entry.uses, entry.spells = map[string]bool{}, map[string]bool{}, map[string]bool{}
			}
			entry.method = entry.method || method
			funcs[name] = entry
			ast.Inspect(body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CallExpr:
					switch callee := node.Fun.(type) {
					case *ast.Ident:
						entry.calls[callee.Name] = true
					case *ast.SelectorExpr:
						if pkg, ok := callee.X.(*ast.Ident); !ok || imports[pkg.Name] == "" {
							entry.calls[callee.Sel.Name] = true
						}
					}
				case *ast.SelectorExpr:
					if pkg, ok := node.X.(*ast.Ident); ok && imports[pkg.Name] != "" {
						entry.calls[imports[pkg.Name]+"."+node.Sel.Name] = true
					}
				case *ast.BasicLit:
					if value, err := strconv.Unquote(node.Value); err == nil && node.Kind == token.STRING && spellsSessionCookie(value) {
						entry.spells[node.Value] = true
					}
				case *ast.Ident:
					entry.uses[node.Name] = true
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					read(decl.Name.Name, decl.Recv != nil, decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for i, name := range value.Names {
							if i < len(value.Values) {
								read(name.Name, false, value.Values[i])
							}
						}
					}
				}
			}
		}
	}
	return funcs
}

// spellsSessionCookie reports whether value is the session cookie's name under either spelling, or
// a header of it: the name, then "=".
func spellsSessionCookie(value string) bool {
	for _, name := range []string{hostSessionCookie, plainSessionCookie} {
		if value == name || strings.HasPrefix(value, name+"=") {
			return true
		}
	}
	return false
}

// reachers is the functions of funcs that seed holds for, and every one that calls one of them.
func reachers(funcs goFuncs, seed func(name string) bool) map[string]bool {
	reaching := map[string]bool{}
	for name := range funcs {
		if seed(name) {
			reaching[name] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for name, function := range funcs {
			if reaching[name] {
				continue
			}
			for called := range function.calls {
				if reaching[called] {
					reaching[name], grew = true, true
					break
				}
			}
		}
	}
	return reaching
}
