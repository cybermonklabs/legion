package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// apiWalk is the API mounted as a production deployment mounts it (no test hooks, the document
// service of its own) on a mux of its own, with no database, and every route it serves: each row
// of the route table and each pattern mounted beside the table, the two sockets
// (readclass.Patterns). The router above it is walked by routes' tests of the same names; this one
// holds the API alone, so a route of its own that answers with no credential fails here first.
func apiWalk(t *testing.T) (http.Handler, []string) {
	t.Helper()
	deps, err := NewDeps(DepsInput{
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User"},
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("build the API's dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, t.Context(), deps)
	var routes []string
	for _, route := range (&server{}).routes() {
		routes = append(routes, route.Method+" "+route.Pattern)
	}
	table := len(routes)
	beside := readclass.Patterns(mux)
	for _, socket := range []string{"GET /ws/live", "GET /ws/doc/{room}"} {
		if !slices.Contains(beside, socket) {
			t.Fatalf("%s is not among the patterns mounted beside the table: %q", socket, beside)
		}
	}
	routes = append(routes, beside...)
	if !slices.Contains(routes, "GET /metrics") {
		t.Fatalf("the walk holds no GET /metrics: %q", routes)
	}
	t.Logf("walked %d routes: %d rows of the table and %d patterns mounted beside it", len(routes), table, len(beside))
	return mux, routes
}

// unfilledWildcard is a wildcard routeProbePath leaves as written.
var unfilledWildcard = regexp.MustCompile(`\{[a-z_]+\}`)

// walkRequest is route's request with no credential: routeProbePath's path with every wildcard it
// leaves filled too, and routeProbeBody for a write.
func walkRequest(t *testing.T, route string) *http.Request {
	t.Helper()
	method, pattern, _ := strings.Cut(route, " ")
	path := unfilledWildcard.ReplaceAllString(routeProbePath(pattern), "x")
	body := ""
	if method != http.MethodGet {
		body = routeProbeBody(apiRoute{routeRow: routeRow{Method: method, Pattern: pattern}})
	}
	request := httptest.NewRequestWithContext(t.Context(), method, "https://dispatch.example"+path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

// TestEveryRouteNeedsACredentialButTheNamedOnes (c-no-route-trusts-the-network): every route the
// API serves, sent with no cookie and no Authorization, is refused for the credential it does not
// carry (authRefusal). The API names no route that needs none: sign-in, health and the web app
// are the router's and the process's, and routes' test of this name holds them.
func TestEveryRouteNeedsACredentialButTheNamedOnes(t *testing.T) {
	handler, routes := apiWalk(t)
	for _, route := range routes {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, walkRequest(t, route))
		if refused, _ := authRefusal(response); !refused {
			t.Errorf("%s answers a caller with no credential %d %s, want it refused for the credential it does not carry", route, response.Code, firstLine(response.Body.String()))
		}
	}
}

// TestNoRouteGrantsAccessByNetworkAddress (c-no-route-trusts-the-network): every route the API
// serves answers a caller with no credential alike whether it comes bare or from a loopback peer
// carrying every network header set to an internal address.
func TestNoRouteGrantsAccessByNetworkAddress(t *testing.T) {
	handler, routes := apiWalk(t)
	for _, route := range routes {
		bare, inside := httptest.NewRecorder(), httptest.NewRecorder()
		handler.ServeHTTP(bare, walkRequest(t, route))
		request := walkRequest(t, route)
		request.RemoteAddr = "127.0.0.1:40000"
		for name, value := range map[string]string{
			"X-Forwarded-For":        "127.0.0.1, 10.0.0.1, 100.64.0.1, fd7a:115c:a1e0::1",
			"X-Real-IP":              "100.64.0.1",
			"Forwarded":              `for=127.0.0.1;proto=https, for="[fd7a:115c:a1e0::1]"`,
			"X-Forwarded-Host":       "dispatch.example",
			"Via":                    "1.1 10.0.0.1",
			"Tailscale-User-Login":   "alice@example.com",
			"Tailscale-User-Name":    "alice@example.com",
			"Tailscale-Headers-Info": "https://tailscale.com/s/serve-headers",
		} {
			request.Header.Set(name, value)
		}
		handler.ServeHTTP(inside, request)
		if bare.Code != inside.Code || bare.Body.String() != inside.Body.String() {
			t.Errorf("%s answers a caller with no credential %d %s, and the same caller from an address of ours %d %s",
				route, bare.Code, firstLine(bare.Body.String()), inside.Code, firstLine(inside.Body.String()))
		}
	}
}
