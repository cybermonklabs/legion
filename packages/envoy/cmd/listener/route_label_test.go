package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The path label of envoy_listener_requests_total holds a registered pattern or the gate's own
// /v1/, whatever path a caller sends. The gate here is opened on a mux that also registers a
// test-only wildcard route, /v1/probe/{id}/, with no method: a CONNECT matches only a CONNECT
// pattern or one with no method (Go 1.27, net/http/routing_tree.go), and only the CONNECT branch of
// (*http.ServeMux).Handler answers a trailing-slash redirect with the redirect's own path, the
// caller's text, as its pattern (net/http/server.go, findHandler), so a GET route could not show
// the leak. The three requests go through the outer mux as main mounts it; each reaches the label
// either through apiAuth's count or, for the one the outer mux redirects before apiAuth, through the
// gate's route read directly, as apiAuth would read it.
//
// Control: route returning (*http.ServeMux).Handler's pattern unconditionally; the CONNECT's label
// reads /v1/probe/secret-7f3a/, and the test fails naming it.
func TestTheRouteLabelNeverCarriesACallersPath(t *testing.T) {
	const secret = "secret-7f3a"
	registered := []string{"/v1/registry/", "/v1/probe/{id}/"}
	met := metrics.New()
	logger := logging.NewWithWriter("test-machine", io.Discard)
	gate := newStartingGate("v1", logger)
	routes := http.NewServeMux()
	for _, pattern := range registered {
		routes.Handle(pattern, served)
	}
	gate.open(routes)
	handler := apiAuth(listenerAPIAuth{logger: logger, requests: newListenerRequests(met), route: gate.route}, gate)
	outer := http.NewServeMux()
	outer.Handle("/v1", handler)
	outer.Handle("/v1/", handler)

	allowed := func(label string) bool {
		return label == "/v1/" || slices.Contains(registered, label)
	}
	admitted := 0
	for _, tc := range []struct {
		method, path string
		// counted is whether the request passes the outer mux to apiAuth, which counts it; the
		// outer mux redirects a path with a dot segment before apiAuth sees it.
		counted bool
	}{
		{method: http.MethodConnect, path: "/v1/probe/" + secret, counted: true},
		{method: http.MethodGet, path: "/v1/probe/" + secret, counted: true},
		{method: http.MethodGet, path: "/v1/../v1/probe/" + secret},
	} {
		request := func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Method, request.Host = tc.method, "localhost"
			return request
		}
		before := requestSeries(t, met)
		outer.ServeHTTP(httptest.NewRecorder(), request())
		after := requestSeries(t, met)
		moved := 0
		for key, count := range after {
			if count == before[key] {
				continue
			}
			moved++
			if label := key[1]; !allowed(label) || strings.Contains(label, secret) {
				t.Fatalf("%s %s was counted under the path label %q, want a registered pattern %q or /v1/, and nothing of the caller's path",
					tc.method, tc.path, label, registered)
			}
		}
		if counted := moved == 1; counted != tc.counted {
			t.Fatalf("%s %s moved %d series, want it counted: %t; series: %v", tc.method, tc.path, moved, tc.counted, after)
		}
		if tc.counted {
			admitted++
		}

		// The gate's route for the same request as the outer mux's catch-all hands it over.
		direct := request()
		direct.Pattern = "/v1/"
		if label := gate.route(direct); !allowed(label) || strings.Contains(label, secret) {
			t.Fatalf("the gate names %s %s's route %q, want a registered pattern %q or /v1/, and nothing of the caller's path",
				tc.method, tc.path, label, registered)
		}
	}
	if admitted != 2 {
		t.Fatalf("%d requests reached apiAuth's count, want the CONNECT and the GET", admitted)
	}
}

// The pattern set route reads (v1RegisteredPatterns) is the set registerV1Routes registers: each
// pattern in it resolves, on the mux registerV1Routes fills, to itself, so none names a route the
// listener does not have; and it holds as many patterns as the read-only token's walk of every
// route finds (v1RoutePatterns, TestAPIAuthRefusesAReadOnlyTokenOnEveryV1Route), so a route added
// to registerV1Routes, which that walk must then count, fails here until its pattern is in the set,
// rather than being counted under /v1/.
//
// Control: one pattern left out of the set, or one added that registerV1Routes does not register;
// the test fails naming the count or the pattern.
func TestTheRouteLabelSetIsEveryV1Route(t *testing.T) {
	routes := newBrokerRig(t).routes
	for pattern := range v1RegisteredPatterns {
		if _, resolved := routes.Handler(httptest.NewRequest(http.MethodGet, pattern, nil)); resolved != pattern {
			t.Errorf("the pattern %q resolves on registerV1Routes's mux to %q, so it is no route registerV1Routes registers", pattern, resolved)
		}
	}
	if len(v1RegisteredPatterns) != v1RoutePatterns {
		t.Fatalf("the route label's pattern set holds %d patterns, and registerV1Routes registers %d", len(v1RegisteredPatterns), v1RoutePatterns)
	}
}
