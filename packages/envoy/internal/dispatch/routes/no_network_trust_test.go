package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// noCredentialNeeded is every route a caller with no credential is answered on as a credential is:
// the two sign-in routes a person has no credential yet to call, the process's two probes, and
// the static web app, whose page is what asks a person to sign in. Every other route the process
// serves needs a credential (c-no-route-trusts-the-network).
var noCredentialNeeded = []string{"GET /auth/start", "GET /auth/callback", "GET /healthz", "GET /livez", "/"}

// processWalk is a Dispatch process built as cmd/dispatch builds one (Process over New) and the
// routes it serves. The router signs people in with Google through a fake issuer, with no dev
// sign-in, and holds no database: Store is nil, as cmd/dispatch's settings tests build a router,
// so the walk needs no DISPATCH_TEST_DATABASE_URL and cannot skip having walked nothing.
type processWalk struct {
	handler http.Handler
	// routes is every route walked, "<method> <pattern>" or a bare pattern for one that names no
	// method: the route index's rows (api.WriteRouteIndex), and every pattern readclass recorded on
	// the router's mux and on Process's.
	routes []string
}

func newProcessWalk(t *testing.T) processWalk {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), rigClientID, "dispatch-secret")
	flow := oidc.NewCodeFlow(issuer.URL(), rigClientID, "dispatch-secret", rigDomain)
	people := newMemoryPeople()
	sessions := &memorySessionStore{generations: map[string]int64{}}
	rule := identity.DomainRule{Domain: rigDomain}
	membership := &identity.Membership{People: people, Sessions: sessions, SignIn: flow, Rule: rule}
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html><title>Dispatch</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:  testSigningKey,
		WebDistDir:  webDist,
		People:      people,
		Sessions:    sessions,
		Identity:    identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions, Membership: membership},
		SignIn:      flow,
		SignInRule:  rule,
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Lifetime:    t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	router, mux, _ := serve(ctx)
	health := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	process := Process(router, health)

	var index bytes.Buffer
	if err := api.WriteRouteIndex(&index); err != nil {
		t.Fatalf("write the route index: %v", err)
	}
	var table struct {
		Routes []struct{ Method, Path string } `json:"routes"`
	}
	if err := json.Unmarshal(index.Bytes(), &table); err != nil || len(table.Routes) == 0 {
		t.Fatalf("the route index reads as %d rows (%v): %s", len(table.Routes), err, index.String())
	}
	union := map[string]bool{}
	for _, row := range table.Routes {
		union[row.Method+" "+row.Path] = true
	}
	for _, pattern := range slices.Concat(readclass.Patterns(mux), readclass.Patterns(process)) {
		union[pattern] = true
	}
	walk := processWalk{handler: process}
	for route := range union {
		walk.routes = append(walk.routes, route)
	}
	slices.Sort(walk.routes)

	// What the walk must hold, so a record that came back empty, lost the sockets or lost the
	// process's own mounts fails here rather than passing having walked less.
	for _, floor := range []string{"GET /ws/live", "GET /ws/doc/{room}", "GET /healthz", "GET /livez", "GET /auth/whoami", "GET /api/v1", "GET /metrics"} {
		if !slices.Contains(walk.routes, floor) {
			t.Fatalf("the walk does not hold %s: it read %q", floor, walk.routes)
		}
	}
	if len(walk.routes) <= len(table.Routes) {
		t.Fatalf("the walk holds %d routes and the route index %d: the patterns mounted beside the table were not read", len(walk.routes), len(table.Routes))
	}
	for _, exempt := range noCredentialNeeded {
		if !slices.Contains(walk.routes, exempt) {
			t.Fatalf("%s needs no credential, and the walk does not hold it: the list names a route the process does not serve", exempt)
		}
	}
	// A credential is admitted: the router the walk reads answers a signed-in caller, so a
	// refusal below is the route refusing the caller and not a router that refuses everyone.
	if response := walk.send(t, "GET /api/v1", func(r *http.Request) { r.Header.Set("Authorization", "Bearer agent-token") }); response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1 with the shared agent token answered %d %s, want 200", response.Code, firstLine(response.Body.String()))
	}
	t.Logf("walked %d routes: %d rows of the route index and %d patterns mounted beside it", len(walk.routes), len(table.Routes), len(walk.routes)-len(table.Routes))
	return walk
}

// wildcard is a path wildcard of a ServeMux pattern.
var wildcard = regexp.MustCompile(`\{([a-z_]+)(\.\.\.)?\}`)

// wildcardValues fills each wildcard with a fixed value of its own kind.
var wildcardValues = map[string]string{
	"id":         "00000000-0000-0000-0000-000000000001",
	"room":       "00000000-0000-0000-0000-000000000001",
	"key":        "PRIV",
	"session_id": "s1",
	"block_id":   "b1",
	"login":      "alice@example.com",
	"email":      "outsider@other.example",
	"name":       "readers",
	"owner":      "owner",
	"repo":       "repo",
	"slug":       "spec",
	"number":     "1",
	"attempt":    "1",
	"phase":      "plan",
	"point":      "after-commit",
}

// walkRequest is route's request with no credential: its method (GET for a pattern that names
// none), its path with every wildcard filled, and, for a write, the body a bearer names its
// session in.
func walkRequest(t *testing.T, route string) *http.Request {
	t.Helper()
	method, path, named := strings.Cut(route, " ")
	if !named {
		method, path = http.MethodGet, route
	}
	path = wildcard.ReplaceAllStringFunc(path, func(match string) string {
		value, known := wildcardValues[wildcard.FindStringSubmatch(match)[1]]
		if !known {
			t.Fatalf("%s holds the wildcard %s, which the walk has no value for", route, match)
		}
		return value
	})
	if strings.HasSuffix(path, "/") && path != "/" {
		// A prefix pattern (the GitHub proxy's) is called below its prefix.
		path += "repos/owner/repo"
	}
	body := ""
	if method != http.MethodGet {
		body = `{"actor":{"kind":"session","id":"s1"}}`
	}
	request := httptest.NewRequestWithContext(t.Context(), method, "https://dispatch.example"+path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func (w processWalk) send(t *testing.T, route string, edit func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := walkRequest(t, route)
	if edit != nil {
		edit(request)
	}
	response := httptest.NewRecorder()
	w.handler.ServeHTTP(response, request)
	return response
}

// TestEveryRouteNeedsACredentialButTheNamedOnes (c-no-route-trusts-the-network): every route the
// process serves, sent with no cookie and no Authorization, is refused for the credential it does
// not carry (refusedForNoCredential), but the ones noCredentialNeeded names.
func TestEveryRouteNeedsACredentialButTheNamedOnes(t *testing.T) {
	walk := newProcessWalk(t)
	for _, route := range walk.routes {
		if slices.Contains(noCredentialNeeded, route) {
			continue
		}
		if response := walk.send(t, route, nil); !refusedForNoCredential(response) {
			t.Errorf("%s answers a caller with no credential %d %s, want it refused for the credential it does not carry", route, response.Code, firstLine(response.Body.String()))
		}
	}
}

// TestNoVariantOfAServerRouteIsTheDashboard: a path the mux does not route but that names one of
// the process's own routes another way - with a trailing slash (`/metrics/`) or in another case
// (`/API/V1`) - is answered by the server as a JSON refusal or a JSON 404, with no credential,
// and never by the dashboard's page, whose 200 a scraper or a client would read as an answer of
// the route it meant. Every walked route but the static web app is tried both ways.
func TestNoVariantOfAServerRouteIsTheDashboard(t *testing.T) {
	walk := newProcessWalk(t)
	tried := 0
	for _, route := range walk.routes {
		if route == "/" {
			continue
		}
		method, pattern, named := strings.Cut(route, " ")
		if !named {
			method, pattern = http.MethodGet, route
		}
		var variants []string
		if !strings.HasSuffix(pattern, "/") {
			variants = append(variants, method+" "+pattern+"/")
		}
		variants = append(variants, method+" "+strings.ToUpper(pattern))
		for _, variant := range variants {
			tried++
			response := walk.send(t, variant, nil)
			if strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") || response.Code == http.StatusOK {
				t.Errorf("%s (a variant of %s) answers %d %s %s, want a JSON refusal or 404, not the dashboard", variant, route, response.Code, response.Header().Get("Content-Type"), firstLine(response.Body.String()))
			}
		}
	}
	if tried < len(walk.routes) {
		t.Fatalf("tried %d variants of %d routes: the walk tried too few", tried, len(walk.routes))
	}
}

// refusedForNoCredential reports whether response is the server turning away a caller for the
// credential it does not carry: 401 NO_IDENTITY or UNAUTHORIZED, the document socket's plain 401
// "unauthorized", or one of the three 403s by which a route that takes an agent's bearer alone
// refuses every other caller, a caller with none included (REPLY_FORBIDDEN, ACCEPT_FORBIDDEN,
// HANDOFF_FORBIDDEN, the codes the api package's authRefusal reads as a refusal of the credential).
func refusedForNoCredential(response *httptest.ResponseRecorder) bool {
	code := codeOf(response)
	switch response.Code {
	case http.StatusUnauthorized:
		return code == "NO_IDENTITY" || code == "UNAUTHORIZED" || strings.TrimSpace(response.Body.String()) == "unauthorized"
	case http.StatusForbidden:
		return code == "REPLY_FORBIDDEN" || code == "ACCEPT_FORBIDDEN" || code == "HANDOFF_FORBIDDEN"
	}
	return false
}

// networkHeaders are the headers a proxy or a tailnet writes to say where a request came from,
// each set to an address of ours or a person of ours.
var networkHeaders = map[string]string{
	"X-Forwarded-For":        "127.0.0.1, 10.0.0.1, 100.64.0.1, fd7a:115c:a1e0::1",
	"X-Real-IP":              "100.64.0.1",
	"Forwarded":              `for=127.0.0.1;proto=https, for="[fd7a:115c:a1e0::1]"`,
	"X-Forwarded-Host":       "dispatch.example",
	"Via":                    "1.1 10.0.0.1",
	"Tailscale-User-Login":   "alice@example.com",
	"Tailscale-User-Name":    "alice@example.com",
	"Tailscale-Headers-Info": "https://tailscale.com/s/serve-headers",
}

// TestNoRouteGrantsAccessByNetworkAddress (c-no-route-trusts-the-network): every route the process
// serves is sent the same request twice, once bare and once from a loopback peer carrying every
// network header set to an internal address, and answers the two alike. A route that needs a
// credential is thereby refused on both, so no address of ours is what a credential gets.
func TestNoRouteGrantsAccessByNetworkAddress(t *testing.T) {
	walk := newProcessWalk(t)
	if slices.Contains(walk.routes, "GET /auth/_dev/signin") {
		t.Fatalf("the production-shaped process mounts GET /auth/_dev/signin, which signs anyone in from a loopback peer")
	}
	internal := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:40000"
		for name, value := range networkHeaders {
			r.Header.Set(name, value)
		}
	}
	for _, route := range walk.routes {
		bare, inside := walk.send(t, route, nil), walk.send(t, route, internal)
		if bare.Code != inside.Code || answerOf(bare) != answerOf(inside) {
			t.Errorf("%s answers a caller with no credential %d %s, and the same caller from an address of ours %d %s",
				route, bare.Code, firstLine(answerOf(bare)), inside.Code, firstLine(answerOf(inside)))
		}
	}
}

// answerOf is what a response says, but the one value each sign-in start draws anew: a redirect
// answers its target with the state and nonce it carries removed, which are random per request.
func answerOf(response *httptest.ResponseRecorder) string {
	if location := response.Header().Get("Location"); location != "" {
		target, err := url.Parse(location)
		if err != nil {
			return "Location: " + location
		}
		query := target.Query()
		query.Del("state")
		query.Del("nonce")
		target.RawQuery = query.Encode()
		return "Location: " + target.String()
	}
	return response.Body.String()
}

// firstLine is text's first line, which is what a failure quotes of an answer.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// codeOf is the code of a JSON error answer, empty for any other answer.
func codeOf(response *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return body.Code
}
