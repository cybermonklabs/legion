package routes

import (
	"bytes"
	"context"
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

// The hosts of the walk: canonicalHost is its processes' DISPATCH_SERVER_URL's, outsideHost the
// DISPATCH_OUTSIDE_ORIGIN's of the process that answers at two origins, and foreignHost a host
// neither process answers at, which a forwarding header names.
const (
	canonicalHost = "dispatch.example"
	outsideHost   = "guests.example"
	foreignHost   = "attacker.example"
)

// processWalk is two Dispatch processes built as cmd/dispatch builds one (Process over New), the
// routes they serve, and each origin each answers at. Each router signs people in with Google
// through a fake issuer, with no dev sign-in, and holds no database: Store is nil, as
// cmd/dispatch's settings tests build a router, so the walk needs no DISPATCH_TEST_DATABASE_URL
// and cannot skip having walked nothing.
type processWalk struct {
	// routes is every route walked, "<method> <pattern>" or a bare pattern for one that names no
	// method: the route index's rows (api.WriteRouteIndex), and every pattern readclass recorded on
	// either process's router mux and Process mux.
	routes []string
	// at is each process at each origin it answers at: the process with none of the outside
	// settings at its one origin, then the process that also answers at outsideHost
	// (DISPATCH_OUTSIDE_ORIGIN) and admits restricted members from outside the domain
	// (identity.OutsideRule), at its canonical origin and at its outside origin.
	at []walkOrigin
}

// walkOrigin is a process of the walk at one origin it answers at.
type walkOrigin struct {
	// name says which process and which origin, for a failure to name.
	name string
	// host is the origin's host, which a request to it carries as its Host, and otherHost the host
	// of the process's other origin, empty for the process at one.
	host, otherHost string
	handler         http.Handler
}

func newProcessWalk(t *testing.T) processWalk {
	t.Helper()
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), rigClientID, "dispatch-secret")
	domain := identity.DomainRule{Domain: rigDomain}
	outside := identity.OutsideRule{DomainRule: domain, People: identity.ListerFunc(func(context.Context, string) (bool, error) { return false, nil })}
	atOne, onePatterns := walkProcess(t, issuer, domain, "")
	atTwo, twoPatterns := walkProcess(t, issuer, outside, "https://"+outsideHost)
	walk := processWalk{at: []walkOrigin{
		{name: "at the one origin of a process with no outside origin", host: canonicalHost, handler: atOne},
		{name: "at the canonical origin of a process with an outside origin", host: canonicalHost, otherHost: outsideHost, handler: atTwo},
		{name: "at the outside origin", host: outsideHost, otherHost: canonicalHost, handler: atTwo},
	}}

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
	for _, pattern := range slices.Concat(onePatterns, twoPatterns) {
		union[pattern] = true
	}
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
	for _, at := range walk.at {
		// A credential is admitted: the router the walk reads answers a signed-in caller, so a
		// refusal below is the route refusing the caller and not a router that refuses everyone.
		if response := at.send(t, "GET /api/v1", func(r *http.Request) { r.Header.Set("Authorization", "Bearer agent-token") }); response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1 with the shared agent token %s answered %d %s, want 200", at.name, response.Code, firstLine(response.Body.String()))
		}
		// The origin is answered as itself, so the walk at the outside origin is not the canonical
		// origin walked again: a sign-in started at a canonical origin comes back to its own
		// callback, and the outside origin serves no Google sign-in, its start answering 404.
		start := at.send(t, "GET /auth/start", nil)
		location := start.Header().Get("Location")
		if at.host == outsideHost {
			if start.Code != http.StatusNotFound || location != "" {
				t.Fatalf("GET /auth/start %s answered %d to %q, want the 404 of a path no route serves", at.name, start.Code, location)
			}
			continue
		}
		target, err := url.Parse(location)
		if want := "https://" + at.host + "/auth/callback"; err != nil || target.Query().Get("redirect_uri") != want {
			t.Fatalf("GET /auth/start %s answered %d to %q, want a sign-in that comes back to %s", at.name, start.Code, location, want)
		}
	}
	t.Logf("walked %d routes at %d origins: %d rows of the route index and %d patterns mounted beside it", len(walk.routes), len(walk.at), len(table.Routes), len(walk.routes)-len(table.Routes))
	return walk
}

// walkProcess is a process of the walk, signing people in through issuer under rule, answering at
// https://canonicalHost (DISPATCH_SERVER_URL) and at outside too (DISPATCH_OUTSIDE_ORIGIN; empty
// for none), and the patterns readclass recorded on its router's mux and on Process's.
func walkProcess(t *testing.T, issuer *oidctest.Issuer, rule identity.SignInRule, outside string) (http.Handler, []string) {
	t.Helper()
	flow := oidc.NewCodeFlow(issuer.URL(), rigClientID, "dispatch-secret", rigDomain)
	people := newMemoryPeople()
	sessions := &memorySessionStore{generations: map[string]int64{}}
	membership := &identity.Membership{People: people, SignIn: flow, Rule: rule}
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html><title>Dispatch</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    testSigningKey,
		WebDistDir:    webDist,
		People:        people,
		Sessions:      sessions,
		Identity:      identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions, Membership: membership},
		SignIn:        flow,
		SignInRule:    rule,
		AgentTokens:   sharedAgentTokens(t, "agent-token"),
		ServerURL:     "https://" + canonicalHost,
		OutsideOrigin: outside,
		Lifetime:      t.Context(),
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
	return process, slices.Concat(readclass.Patterns(mux), readclass.Patterns(process))
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

// walkRequest is route's request to host with no credential: its method (GET for a pattern that
// names none), its path with every wildcard filled, and, for a write, the body a bearer names its
// session in.
func walkRequest(t *testing.T, host, route string) *http.Request {
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
	request := httptest.NewRequestWithContext(t.Context(), method, "https://"+host+path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func (at walkOrigin) send(t *testing.T, route string, edit func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := walkRequest(t, at.host, route)
	if edit != nil {
		edit(request)
	}
	response := httptest.NewRecorder()
	at.handler.ServeHTTP(response, request)
	return response
}

// asMethod sends a route's request as method, with no body: HEAD or OPTIONS, which a pattern
// serves as its own method only in part (a GET pattern serves HEAD, a pattern naming no method
// serves both, and any other is the mux's).
func asMethod(method string) func(*http.Request) {
	return func(r *http.Request) {
		r.Method = method
		r.Body, r.ContentLength = http.NoBody, 0
		r.Header.Del("Content-Type")
	}
}

// TestEveryRouteNeedsACredentialButTheNamedOnes (c-no-route-trusts-the-network): every route the
// processes serve, but the ones noCredentialNeeded names, sent with no cookie and no Authorization
// at each origin of the walk, is refused for the credential it does not carry
// (refusedForNoCredential), with the same refusal at every origin. Sent as HEAD and as OPTIONS, it
// is refused for the credential too, or answered 404 or 405, and never served.
func TestEveryRouteNeedsACredentialButTheNamedOnes(t *testing.T) {
	walk := newProcessWalk(t)
	first := walk.at[0]
	for _, route := range walk.routes {
		if slices.Contains(noCredentialNeeded, route) {
			continue
		}
		want := first.send(t, route, nil)
		for _, at := range walk.at {
			response := at.send(t, route, nil)
			if !refusedForNoCredential(response) {
				t.Errorf("%s %s answers a caller with no credential %d %s, want it refused for the credential it does not carry", route, at.name, response.Code, firstLine(response.Body.String()))
			} else if response.Code != want.Code || answerOf(response) != answerOf(want) {
				t.Errorf("%s %s answers a caller with no credential %d %s, and %s %d %s: want one refusal at every origin",
					route, at.name, response.Code, firstLine(answerOf(response)), first.name, want.Code, firstLine(answerOf(want)))
			}
			for _, method := range []string{http.MethodHead, http.MethodOptions} {
				response := at.send(t, route, asMethod(method))
				if !refusedForNoCredential(response) && response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s sent as %s %s answers a caller with no credential %d %s, want it refused for the credential, 404 or 405", route, method, at.name, response.Code, firstLine(response.Body.String()))
				}
			}
		}
	}
}

// TestNoVariantOfAServerRouteIsTheDashboard: a path the mux does not route but that names one of
// the process's own routes another way - with a trailing slash (`/metrics/`) or in another case
// (`/API/V1`) - is answered by the server as a JSON refusal or a JSON 404, with no credential,
// and never by the dashboard's page, whose 200 a scraper or a client would read as an answer of
// the route it meant. Every walked route but the static web app is tried both ways, at each origin
// of the walk.
func TestNoVariantOfAServerRouteIsTheDashboard(t *testing.T) {
	walk := newProcessWalk(t)
	tried := 0
	for _, at := range walk.at {
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
				response := at.send(t, variant, nil)
				if strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") || response.Code == http.StatusOK {
					t.Errorf("%s (a variant of %s) %s answers %d %s %s, want a JSON refusal or 404, not the dashboard", variant, route, at.name, response.Code, response.Header().Get("Content-Type"), firstLine(response.Body.String()))
				}
			}
		}
	}
	if tried < len(walk.routes)*len(walk.at) {
		t.Fatalf("tried %d variants of %d routes at %d origins: the walk tried too few", tried, len(walk.routes), len(walk.at))
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
// each set to an address of ours or a person of ours. X-Forwarded-Host, the host a proxy says it
// was asked for, is set apart, to each of forwardedHosts.
var networkHeaders = map[string]string{
	"X-Forwarded-For":        "127.0.0.1, 10.0.0.1, 100.64.0.1, fd7a:115c:a1e0::1",
	"X-Real-IP":              "100.64.0.1",
	"Forwarded":              `for=127.0.0.1;proto=https, for="[fd7a:115c:a1e0::1]"`,
	"Via":                    "1.1 10.0.0.1",
	"Tailscale-User-Login":   "alice@example.com",
	"Tailscale-User-Name":    "alice@example.com",
	"Tailscale-Headers-Info": "https://tailscale.com/s/serve-headers",
}

// forwardedHosts are the hosts X-Forwarded-Host names on a request to at: at's own, so a route
// that trusts what a proxy of ours says it forwarded is caught; and the process's other origin's
// and foreignHost, which no route may read as the origin a request came to in place of its Host,
// so a route that takes its origin from the header is caught at each origin.
func (at walkOrigin) forwardedHosts() []string {
	hosts := []string{at.host, foreignHost}
	if at.otherHost != "" {
		hosts = append(hosts, at.otherHost)
	}
	return hosts
}

// TestNoRouteGrantsAccessByNetworkAddress (c-no-route-trusts-the-network): every route the
// processes serve is sent the same request at each origin of the walk, once bare and once from a
// loopback peer carrying every network header set to an internal address and X-Forwarded-Host set
// to each of forwardedHosts, and answers them all alike. A route that needs a credential is thereby
// refused on each, so no address of ours is what a credential gets, and no route takes the origin
// a request came to from a header.
func TestNoRouteGrantsAccessByNetworkAddress(t *testing.T) {
	walk := newProcessWalk(t)
	if slices.Contains(walk.routes, "GET /auth/_dev/signin") {
		t.Fatalf("the production-shaped process mounts GET /auth/_dev/signin, which signs anyone in from a loopback peer")
	}
	for _, at := range walk.at {
		for _, forwardedHost := range at.forwardedHosts() {
			internal := func(r *http.Request) {
				r.RemoteAddr = "127.0.0.1:40000"
				for name, value := range networkHeaders {
					r.Header.Set(name, value)
				}
				r.Header.Set("X-Forwarded-Host", forwardedHost)
			}
			for _, route := range walk.routes {
				bare, inside := at.send(t, route, nil), at.send(t, route, internal)
				if bare.Code != inside.Code || answerOf(bare) != answerOf(inside) {
					t.Errorf("%s %s answers a caller with no credential %d %s, and the same caller from an address of ours, forwarded for %s, %d %s",
						route, at.name, bare.Code, firstLine(answerOf(bare)), forwardedHost, inside.Code, firstLine(answerOf(inside)))
				}
			}
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
