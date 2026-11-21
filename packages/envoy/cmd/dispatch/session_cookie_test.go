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
)

// The session cookie's two names. A Secure cookie is `__Host-dsession`, which a browser takes only
// from this host itself, Secure, with Path=/ and no Domain; under DISPATCH_INSECURE_COOKIE (plain
// http, over which a browser refuses a `__Host-` cookie) it is `dsession`, the name every session
// issued before the rename carries.
const (
	hostSessionCookie  = "__Host-dsession"
	plainSessionCookie = "dsession"
)

// sessionCookieNameFor is the name a server issues its session cookie under: plainSessionCookie
// under DISPATCH_INSECURE_COOKIE, hostSessionCookie otherwise.
func sessionCookieNameFor(insecure bool) string {
	if insecure {
		return plainSessionCookie
	}
	return hostSessionCookie
}

// cookieWalk is one setting's servers, each writing the session cookie: the Google and passkey
// sign-ins of passkeyRig over a database, and a dev sign-in server.
type cookieWalk struct {
	insecure bool
	rig      *passkeyRig
	dev      http.Handler
}

// sessionCookieWriter is a route that writes the session cookie, and how the walk makes it write
// as a browser holding prior, a session cookie under the name the setting does not issue.
type sessionCookieWriter struct {
	route string
	// signIn is whether the route issues a session (else it ends one).
	signIn bool
	write  func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder
}

// sessionCookieWriters is every route that writes the session cookie, by the router method that
// serves it: the census below (sessionCookieWriterRoots) reads those methods from the code, so a
// writer added without a row here fails the walk.
var sessionCookieWriters = map[string]sessionCookieWriter{
	"authCallback": {route: "GET /auth/callback", signIn: true, write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		return w.googleSignIn(t, "carol@example.com", prior)
	}},
	"authDevSignIn": {route: "GET /auth/_dev/signin", signIn: true, write: func(_ *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, devSignInOrigin+"/auth/_dev/signin?login="+url.QueryEscape(privOwner), nil)
		request.RemoteAddr = "127.0.0.1:40000"
		request.AddCookie(prior)
		response := httptest.NewRecorder()
		w.dev.ServeHTTP(response, request)
		return response
	}},
	"passkeyRegisterFinish": {route: "POST /auth/passkey/register/finish", signIn: true, write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		return w.enroll(t, newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin), prior)
	}},
	"passkeyLoginFinish": {route: "POST /auth/passkey/login/finish", signIn: true, write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		device := newVirtualAuthenticator(t, passkeyRPID, passkeyOrigin)
		if enrolled := w.enroll(t, device); enrolled.Code != http.StatusOK {
			t.Fatalf("the enrollment answered %d %s, want 200", enrolled.Code, enrolled.Body.String())
		}
		begun := call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/begin", map[string]string{})
		if begun.Code != http.StatusOK {
			t.Fatalf("the sign-in's begin answered %d %s, want 200", begun.Code, begun.Body.String())
		}
		return call(w.rig.handler, passkeyOrigin, http.MethodPost, "/auth/passkey/login/finish", device.assert(t, begun.Body.Bytes()), w.ceremonyCookie(begun), prior)
	}},
	"authLogout": {route: "POST /auth/logout", write: func(t *testing.T, w *cookieWalk, prior *http.Cookie) *httptest.ResponseRecorder {
		// Bob's browser holds his session under both names: the one his sign-in issued, and the
		// other with the same value, as one signed in before the rename and again since would.
		session := sessionCookieNamed(t, "bob's Google sign-in", w.googleSignIn(t, "bob@example.com"), sessionCookieNameFor(w.insecure))
		return call(w.rig.handler, canonicalOrigin, http.MethodPost, "/auth/logout", nil, session, &http.Cookie{Name: prior.Name, Value: session.Value})
	}},
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
// writes it, under each setting, from a browser holding a session under the name the setting does
// not issue:
//   - a sign-in (an issuer's callback, the dev sign-in, a passkey enrollment or sign-in) sets one
//     session, named for the setting (`__Host-dsession`, Secure; `dsession` under
//     DISPATCH_INSECURE_COOKIE, not Secure), and clears the other name, so the browser keeps the
//     one it was given;
//   - the sign-out clears both names.
//
// The census reads the writers from the code: the functions of the module (its non-test files)
// that reach a session-cookie writer of package auth, called or passed as a value, and that
// nothing else of their package calls - the router's handlers. They are this walk's rows, so a
// writer added without a row (a sliding session's re-issue, say) fails here, and so does any code
// outside package auth that spells the cookie's name, as a cookie built by hand would; the
// census's own control plants one of each.
//
// Controls: issue `__Host-dsession` without Secure, and every sign-in row of the Secure setting
// fails; leave out the clear of the other name, and every sign-in row fails; clear one name alone
// at the sign-out, and its row fails.
func TestEverySessionCookieTheServerWritesIsHostOnlyAtTheRoot(t *testing.T) {
	root := filepath.Join("..", "..")
	want := []string{}
	for handler := range sessionCookieWriters {
		want = append(want, "internal/dispatch/routes."+handler)
	}
	slices.Sort(want)
	t.Run("census control", func(t *testing.T) {
		planted := map[string]string{
			"internal/dispatch/routes": "package routes\n" +
				"func (r *router) plantedRefresh(w http.ResponseWriter, req *http.Request) { r.issueSession(w, req, \"\", \"\") }\n" +
				"func plantedCookie(w http.ResponseWriter) { http.SetCookie(w, &http.Cookie{Name: \"__Host-dsession\", Value: \"x\"}) }\n",
			"internal/dispatch/api": "package api\nimport \"github.com/sjawhar/envoy/internal/dispatch/auth\"\n" +
				"func plantedReissue(h http.Header) { h.Add(\"Set-Cookie\", auth.IssueSessionCookie(\"x\", 0, auth.SigningKeys{}, true)) }\n" +
				"var plantedHook = auth.ClearSessionCookies\n",
		}
		got := sessionCookieWriterRoots(t, root, planted)
		wantPlanted := slices.Sorted(slices.Values(append(slices.Clone(want),
			"internal/dispatch/api.plantedHook", "internal/dispatch/api.plantedReissue",
			`internal/dispatch/routes.plantedCookie spells "__Host-dsession"`, "internal/dispatch/routes.plantedRefresh")))
		if !slices.Equal(got, wantPlanted) {
			t.Fatalf("the census with four planted writers named %q, want %q", got, wantPlanted)
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
				dev:      routerFor(t, devBoot, routes.AppContextOptions{ServerURL: devSignInOrigin, People: peopleStore(t)}),
			}
			if w.rig.boot.InsecureCookie != insecure {
				t.Fatalf("the passkey rig's InsecureCookie is %t, want %t", w.rig.boot.InsecureCookie, insecure)
			}
			issued, other := sessionCookieNameFor(insecure), sessionCookieNameFor(!insecure)
			for _, handler := range slices.Sorted(maps.Keys(sessionCookieWriters)) {
				writer := sessionCookieWriters[handler]
				t.Run(writer.route, func(t *testing.T) {
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
					if !writer.signIn {
						if response.Code != http.StatusOK || len(valued) != 0 || !slices.Equal(cleared, []string{hostSessionCookie, plainSessionCookie}) {
							t.Errorf("%s answered %d setting %q, want both names cleared and no session", writer.route, response.Code, lines)
						}
						return
					}
					if response.Code != http.StatusOK && response.Code != http.StatusFound {
						t.Fatalf("%s answered %d %s, want a sign-in", writer.route, response.Code, response.Body.String())
					}
					if len(valued) != 1 || valued[0].Name != issued || valued[0].Secure == insecure || valued[0].MaxAge != 30*24*60*60 {
						t.Errorf("%s set %q, want one session, %s, Secure %t, Max-Age=2592000", writer.route, lines, issued, !insecure)
					}
					if !slices.Equal(cleared, []string{other}) {
						t.Errorf("%s set %q, want the %s the browser holds cleared, and nothing else", writer.route, lines, other)
					}
				})
			}
		})
	}
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
// names it calls (funcsOf), the identifiers it uses, and the literals in it that spell the session
// cookie's name (spellsSessionCookie).
type goFuncs map[string]struct{ calls, uses, spells map[string]bool }

// sessionCookieWriterRoots is the census of the code that writes the session cookie, read from the
// non-test Go files under root (the module) with planted, each a file's source added to a
// directory, for the control. Package auth's writers are its exported functions, but the readers,
// that reach a package-level name holding "dsession"; the census answers, as "<directory>.<name>",
// each function elsewhere that reaches one within its package and that nothing else of it calls,
// and as "<directory>.<name> spells <literal>" each function elsewhere that spells the cookie's
// name. It fails the test when it reads no name or no writer.
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
		return slices.ContainsFunc(slices.Collect(maps.Keys(authFuncs[name].uses)), func(used string) bool { return names[used] })
	})
	writers := map[string]bool{}
	for name := range reaching {
		if ast.IsExported(name) && !slices.Contains(sessionCookieReaders, name) {
			writers[authPackage+"."+name] = true
		}
	}
	if len(writers) == 0 {
		t.Fatalf("the census read no writer of the session cookie in %s, which reaches %q", authDir, slices.Sorted(maps.Keys(names)))
	}

	var roots []string
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
		read := func(name string, body ast.Node) {
			entry, ok := funcs[name]
			if !ok {
				entry.calls, entry.uses, entry.spells = map[string]bool{}, map[string]bool{}, map[string]bool{}
				funcs[name] = entry
			}
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
					read(decl.Name.Name, decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for i, name := range value.Names {
							if i < len(value.Values) {
								read(name.Name, value.Values[i])
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
