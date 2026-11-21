package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth/forwardedtest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// The hosts of the API walk: apiHost is its DISPATCH_SERVER_URL's, and foreignHost a host it does
// not answer at, which a forwarding header names.
const (
	apiHost     = "dispatch.example"
	foreignHost = "attacker.example"
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
		ServerURL:   "https://" + apiHost,
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
	request := httptest.NewRequestWithContext(t.Context(), method, "https://"+apiHost+path, strings.NewReader(body))
	if method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

// TestEveryRouteNeedsACredentialButTheNamedOnes (c-no-route-trusts-the-network): every route the
// API serves, sent with no cookie and no Authorization, is refused for the credential it does not
// carry (authRefusal); sent as HEAD and as OPTIONS, it is refused for the credential too, or
// answered 404 or 405, and never served. The API names no route that needs none: sign-in, health
// and the web app are the router's and the process's, and routes' test of this name holds them.
func TestEveryRouteNeedsACredentialButTheNamedOnes(t *testing.T) {
	handler, routes := apiWalk(t)
	for _, route := range routes {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, walkRequest(t, route))
		if refused, _ := authRefusal(response); !refused {
			t.Errorf("%s answers a caller with no credential %d %s, want it refused for the credential it does not carry", route, response.Code, firstLine(response.Body.String()))
		}
		for _, method := range []string{http.MethodHead, http.MethodOptions} {
			// HEAD is served by a GET pattern, and OPTIONS by none of the table's, so each is the
			// route's own answer or the mux's.
			request := walkRequest(t, route)
			request.Method = method
			request.Body, request.ContentLength = http.NoBody, 0
			request.Header.Del("Content-Type")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if refused, _ := authRefusal(response); !refused && response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s sent as %s answers a caller with no credential %d %s, want it refused for the credential, 404 or 405", route, method, response.Code, firstLine(response.Body.String()))
			}
		}
	}
}

// TestNoRouteGrantsAccessByNetworkAddress (c-no-route-trusts-the-network): every route the API
// serves answers a caller with no credential alike whether it comes bare or as from an address of
// ours, in each of forwardedtest's shapes: X-Forwarded-For holding every internal address in every
// position a rule might read (leftmost, rightmost, second-to-last, alone, either of two lines), on a
// loopback peer and on a peer of Google's load balancer, with every other network header set to an
// internal address. Each shape carries X-Forwarded-Host naming the API's own host, so a route that
// trusts what a proxy of ours says it forwarded is caught; and the first carries it naming a host the
// API does not answer at too, so a route that takes the origin a request came to from that header
// rather than from Host is caught.
func TestNoRouteGrantsAccessByNetworkAddress(t *testing.T) {
	handler, routes := apiWalk(t)
	variants := forwardedtest.Variants()
	for _, forwardedHost := range []string{apiHost, foreignHost} {
		shapes := variants
		if forwardedHost != apiHost {
			shapes = variants[:1]
		}
		for _, shape := range shapes {
			for _, route := range routes {
				bare, inside := httptest.NewRecorder(), httptest.NewRecorder()
				handler.ServeHTTP(bare, walkRequest(t, route))
				request := walkRequest(t, route)
				shape.Apply(request)
				request.Header.Set("X-Forwarded-Host", forwardedHost)
				handler.ServeHTTP(inside, request)
				if bare.Code != inside.Code || bare.Body.String() != inside.Body.String() {
					t.Errorf("%s answers a caller with no credential %d %s, and the same caller with %s, forwarded for %s, %d %s",
						route, bare.Code, firstLine(bare.Body.String()), shape.Name, forwardedHost, inside.Code, firstLine(inside.Body.String()))
				}
			}
		}
	}
}
