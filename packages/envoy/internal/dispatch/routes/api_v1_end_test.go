package routes

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// apiV1EndRouter is a router with no store and the test hooks mounted, whose /api/v1 ends at end
// (zero for no end) by a clock that reads *now, so a test moves the time without a restart.
func apiV1EndRouter(t *testing.T, end time.Time, now *time.Time) http.Handler {
	t.Helper()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:       testSigningKey,
		People:           people,
		Identity:         identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:         t.Context(),
		TestHooksEnabled: true,
		APIV1End:         end,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	ctx.now = func() time.Time { return *now }
	handler, _ := New(ctx)
	return handler
}

// The end the tests set, as a deployment writes it with an offset, and its two renderings: the
// Sunset header's IMF-fixdate, in GMT, and the 410's own text, as the setting gave it.
var (
	testAPIV1End    = time.Date(2027, time.January, 1, 9, 0, 0, 0, time.FixedZone("", 9*60*60))
	testSunset      = "Fri, 01 Jan 2027 00:00:00 GMT"
	testEndedReason = "/api/v1 ended at 2027-01-01T09:00:00+09:00; GET /api/v2 lists every route"
)

// apiV1Request is one request the tests send under /api/v1, the status it is answered before the
// end, and the Link that answer carries.
type apiV1Request struct {
	what, method, path string
	status             int
	link               string
}

// apiV1Requests are a route's, an unknown path's and a test hook's: the three kinds of path the
// end holds, the hook asked with no credential so it answers without reaching a store.
var apiV1Requests = []apiV1Request{
	{"a route", http.MethodGet, "/api/v1/schema/blocks", http.StatusOK, `</api/v2/block-schema>; rel="successor-version"`},
	{"an unknown path", http.MethodGet, "/api/v1/no-such-route", http.StatusNotFound, `</api/v2>; rel="successor-version"`},
	{"a test hook", http.MethodPost, "/api/v1/live/_test/disconnect", http.StatusUnauthorized, `</api/v2/_test/live/disconnect>; rel="successor-version"`},
}

func serveAt(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, "http://dispatch.test"+path, nil))
	return response
}

// Every /api/v1 answer, an unknown path's and a test hook's included, carries Deprecation dated
// 00:00Z of the day apiV1DeprecatedAt names and a Link to its successor, and Sunset naming the
// end while it is ahead (R3.1, R3.4); with the end unset it carries no Sunset, and an unknown path
// still answers /api/v1's own NOT_FOUND. A /api/v2 answer carries none of the three.
func TestAV1AnswerCarriesItsDeprecationSuccessorAndSunset(t *testing.T) {
	if !apiV1DeprecatedAt.Equal(apiV1DeprecatedAt.UTC().Truncate(24 * time.Hour)) {
		t.Fatalf("apiV1DeprecatedAt = %v, want 00:00Z of a day (decision 10)", apiV1DeprecatedAt)
	}
	deprecation := "@" + strconv.FormatInt(apiV1DeprecatedAt.Unix(), 10)
	now := testAPIV1End.Add(-24 * time.Hour)
	for _, end := range []time.Time{testAPIV1End, {}} {
		var wantSunset []string
		if !end.IsZero() {
			wantSunset = []string{testSunset}
		}
		handler := apiV1EndRouter(t, end, &now)
		for _, request := range apiV1Requests {
			response := serveAt(handler, request.method, request.path)
			if response.Code != request.status || response.Header().Get("Deprecation") != deprecation ||
				!slices.Equal(response.Header().Values("Link"), []string{request.link}) || !slices.Equal(response.Header().Values("Sunset"), wantSunset) {
				t.Errorf("end %v, %s: %s %s answered %d with Deprecation %q, Link %q, Sunset %q; want %d with %q, %q and %q",
					end, request.what, request.method, request.path, response.Code, response.Header().Get("Deprecation"),
					response.Header().Values("Link"), response.Header().Values("Sunset"), request.status, deprecation, request.link, wantSunset)
			}
		}
		unknown := serveAt(handler, http.MethodGet, "/api/v1/no-such-route")
		if body := requireCodedRefusal(t, "an unknown /api/v1 path", unknown, http.StatusNotFound, "NOT_FOUND"); body.Error != "no route for GET /api/v1/no-such-route" || body.Hint != "GET /api/v1 lists every route" {
			t.Errorf("end %v: an unknown /api/v1 path answered %s, want /api/v1's no-route answer", end, unknown.Body.String())
		}
		for _, path := range []string{"/api/v2/block-schema", "/api/v2/no-such-route"} {
			v2 := serveAt(handler, http.MethodGet, path)
			for _, header := range []string{"Deprecation", "Link", "Sunset"} {
				if got := v2.Header().Values(header); got != nil {
					t.Errorf("end %v: GET %s answered %d with %s %q, want none", end, path, v2.Code, header, got)
				}
			}
		}
	}
}

// A /api/v1 write that a layer answers before reading its body, whose body runs past the route's
// limit, is answered 413 in place of that layer's answer (the drain, api/body_drain.go), and that
// 413 is a /api/v1 answer like any other: it carries Deprecation, the Link to its successor and,
// while the end is ahead, Sunset (R3.1, R3.4). The query's NUL is refused before any credential is
// read, so the write needs no store.
func TestAV1AnswerTheDrainReplacesCarriesItsDeprecationSuccessorAndSunset(t *testing.T) {
	now := testAPIV1End.Add(-24 * time.Hour)
	handler := apiV1EndRouter(t, testAPIV1End, &now)
	request := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/issues/CORE-1/comments?q=%00", bytes.NewReader(make([]byte, 2<<20)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requireCodedRefusal(t, "a /api/v1 write past its body limit", response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
	deprecation := "@" + strconv.FormatInt(apiV1DeprecatedAt.Unix(), 10)
	link := []string{`</api/v2/issues/CORE-1/comments>; rel="successor-version"`}
	if response.Header().Get("Deprecation") != deprecation || !slices.Equal(response.Header().Values("Link"), link) ||
		!slices.Equal(response.Header().Values("Sunset"), []string{testSunset}) {
		t.Errorf("the 413 carries Deprecation %q, Link %q, Sunset %q; want %q, %q and %q",
			response.Header().Get("Deprecation"), response.Header().Values("Link"), response.Header().Values("Sunset"), deprecation, link, testSunset)
	}
}

// Once the instant the end names has passed, every path under /api/v1 - a route's, an unknown
// one, a test hook's - answers 410 API_V1_ENDED pointing to GET /api/v2, and no route runs (R3.4).
// The router reads its clock on each request, so the router that served /api/v1 a moment before
// the instant answers 410 at it, with no restart, while /api/v2 still serves.
func TestAPIV1EndsAtTheInstantItsSettingNames(t *testing.T) {
	now := testAPIV1End.Add(-time.Nanosecond)
	handler := apiV1EndRouter(t, testAPIV1End, &now)
	if before := serveAt(handler, http.MethodGet, "/api/v1/schema/blocks"); before.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/schema/blocks just before the end answered %d %s, want 200", before.Code, before.Body.String())
	}
	for _, at := range []time.Time{testAPIV1End, testAPIV1End.Add(24 * time.Hour)} {
		now = at
		for _, request := range apiV1Requests {
			response := serveAt(handler, request.method, request.path)
			body := requireCodedRefusal(t, request.what+" at "+at.String(), response, http.StatusGone, "API_V1_ENDED")
			if body.Error != testEndedReason || body.Hint != "GET /api/v2" {
				t.Errorf("at %v, %s: %s %s answered %s, want error %q and hint GET /api/v2", at, request.what, request.method, request.path, response.Body.String(), testEndedReason)
			}
			if link := response.Header().Values("Link"); !slices.Equal(link, []string{`</api/v2>; rel="successor-version"`}) || response.Header().Get("Sunset") != "" {
				t.Errorf("at %v, %s: the 410 carries Link %q and Sunset %q, want the Link to /api/v2 and no Sunset", at, request.what, link, response.Header().Get("Sunset"))
			}
		}
		if v2 := serveAt(handler, http.MethodGet, "/api/v2/block-schema"); v2.Code != http.StatusOK || v2.Header().Get("Link") != "" {
			t.Errorf("at %v: GET /api/v2/block-schema answered %d with Link %q, want 200 and no Link", at, v2.Code, v2.Header().Get("Link"))
		}
	}
}

// After the end, the 410 is where an operator sees who still calls /api/v1, so it writes the
// access-log line every answer writes: its method, path, status and User-Agent, under the route
// that would have served it, or unmatched for a path no route answers and for a method its route
// does not serve, as the no-route answer's line names them.
func TestTheEndOfAPIV1WritesAnAccessLogLine(t *testing.T) {
	now := testAPIV1End.Add(24 * time.Hour)
	handler := apiV1EndRouter(t, testAPIV1End, &now)
	for _, request := range []struct{ method, path, route string }{
		{http.MethodGet, "/api/v1/schema/blocks", "/api/v1/schema/blocks"},
		{http.MethodPost, "/api/v1/live/_test/disconnect", "/api/v1/live/_test/disconnect"},
		{http.MethodGet, "/api/v1/live/_test/disconnect", unmatchedRoute},
		{http.MethodGet, "/api/v1/no-such-route", unmatchedRoute},
	} {
		logged := captureAccessLog(t)
		sent := httptest.NewRequest(request.method, "http://dispatch.test"+request.path, nil)
		sent.Header.Set("User-Agent", "legacy-client/1.0")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, sent)
		if response.Code != http.StatusGone {
			t.Fatalf("%s %s after the end answered %d %s, want 410", request.method, request.path, response.Code, response.Body.String())
		}
		lines := routeLines(logged, request.route)
		if len(lines) != 1 {
			t.Fatalf("%s %s after the end wrote %d access-log lines under route %s, want one; the log holds %q", request.method, request.path, len(lines), request.route, logged.String())
		}
		for _, want := range []string{"method=" + request.method, "path=" + request.path, "status=410", "user_agent=legacy-client/1.0"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("%s %s after the end: access-log line %q does not carry %q", request.method, request.path, lines[0], want)
			}
		}
	}
}
