package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// Every answer the server writes carries X-Content-Type-Options: nosniff, whichever mount wrote
// it. The rows are one answer from each writer the server holds: the two probes dispatchHandler
// mounts above the router, the JSON 404 of a path under /api that no route answers, a route of
// /auth, the refusal the router answers before any route (enforceCookieOrigin), and the GitHub
// proxy. The API table's own rows carry the header through their compression layer
// (api.compressAnswers); nothing else did, and a browser that reads one of these bodies as a type
// it guessed can run the text in it as a page of Dispatch's own origin.
//
// The handler is the one main serves: the router routes.New builds, under dispatchHandler's
// probes.
func TestEveryAnswerOfTheServerRefusesContentSniffing(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	router := routerFor(t, resolveWith(t, nil), routes.AppContextOptions{Store: database})
	handler := dispatchHandler(router, metrics.New(), database, nil, nil, nil, "", nil, issuers{}, nil)
	const person = "alice@example.com"
	for _, row := range []struct {
		name    string
		request func(*testing.T) *http.Request
		status  int
	}{
		{
			name: "GET /healthz",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://dispatch.test/healthz", nil)
			},
			status: http.StatusOK,
		},
		{
			name: "GET /metrics",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://dispatch.test/metrics", nil)
			},
			status: http.StatusOK,
		},
		{
			name: "GET an unknown route under /api/v1",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/v1/no-such-route", nil)
			},
			status: http.StatusNotFound,
		},
		{
			name: "GET /auth/whoami signed in",
			request: func(t *testing.T) *http.Request {
				return signedIn(t, httptest.NewRequest(http.MethodGet, "http://dispatch.test/auth/whoami", nil), person)
			},
			status: http.StatusOK,
		},
		{
			name: "POST from another origin with a session cookie",
			request: func(t *testing.T) *http.Request {
				request := signedIn(t, httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/issues", nil), person)
				request.Header.Set("Origin", "https://elsewhere.example")
				return request
			},
			status: http.StatusForbidden,
		},
		{
			name: "GET through the GitHub proxy",
			request: func(t *testing.T) *http.Request {
				return signedIn(t, httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/github/rest/repos/acme/web/pulls/1", nil), person)
			},
			status: http.StatusServiceUnavailable,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, row.request(t))
			if response.Code != row.status {
				t.Fatalf("%s: status %d %s, want %d", row.name, response.Code, response.Body.String(), row.status)
			}
			if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", row.name, got)
			}
		})
	}
}
