package heldpath

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubpath"
)

// private makes keys the private set until the test ends, known or not.
func private(t *testing.T, known bool, keys ...string) {
	t.Helper()
	set := NewPrivateSet(keys...)
	Use(func() (PrivateSet, bool) { return set, known })
	t.Cleanup(func() { Use(nil) })
}

// served answers what Of says of target, served under route through a real mux, so the request
// carries its pattern and path values as Dispatch's handlers see them. A route that is no pattern
// (Unmatched) is mounted as the mux's catch-all.
func served(t *testing.T, route, method, target string) Path {
	t.Helper()
	mux := http.NewServeMux()
	var got *Path
	pattern := method + " " + route
	if !strings.HasPrefix(route, "/") {
		pattern = "/"
	}
	mux.HandleFunc(pattern, func(_ http.ResponseWriter, r *http.Request) {
		path := Of(route, r)
		got = &path
	})
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, target, nil))
	if got == nil {
		t.Fatalf("%s %s did not reach %s", method, target, pattern)
	}
	return *got
}

// With no private project and the set read, every path is written whole and escaped, as the
// request line wrote it before, whatever its route (R9).
func TestWithNoPrivateProjectEveryPathIsWhole(t *testing.T) {
	for _, test := range []struct{ route, target string }{
		{"/api/v1/projects/{key}/artifacts/{slug}", "/api/v1/projects/PUB/artifacts/r14-plan"},
		{"/api/v1/agents/{session_id}/artifacts/{slug}", "/api/v1/agents/s-1/artifacts/r14-plan"},
		{"/api/v1/issues/{key}", "/api/v1/issues/a%2Fb%20c"},
		{githubpath.Prefix, "/api/github/rest/r14-plan"},
		{Unmatched, "/api/v1/r14-plan"},
	} {
		got := served(t, test.route, http.MethodGet, test.target)
		if got != (Path{Text: test.target}) {
			t.Errorf("%s under %s = %+v, want the path whole", test.target, test.route, got)
		}
	}
}

// A private project's path writes each name as its placeholder and each identifier as the request
// named it, escaped; a public project's path is whole.
func TestAPrivateProjectsPathKeepsOnlyItsIdentifiers(t *testing.T) {
	private(t, true, "PRIV")
	for _, test := range []struct {
		route, target string
		want          Path
	}{
		{"/api/v1/projects/{key}/artifacts/{slug}", "/api/v1/projects/PRIV/artifacts/r14-plan",
			Path{Text: "/api/v1/projects/PRIV/artifacts/{slug}", Held: true, PrivateProject: true}},
		{"/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", "/api/v1/issues/PRIV-7/artifacts/r14-plan/versions/3",
			Path{Text: "/api/v1/issues/PRIV-7/artifacts/{slug}/versions/3", Held: true, PrivateProject: true}},
		{"/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", "/api/v1/projects/PRIV/architecture-sources/acme/r14-repo",
			Path{Text: "/api/v1/projects/PRIV/architecture-sources/acme/r14-repo", Held: true, PrivateProject: true}},
		{"/api/v1/projects/{key}/access/people/{email}", "/api/v1/projects/PRIV/access/people/r14-mark@example.com",
			Path{Text: "/api/v1/projects/PRIV/access/people/{email}", Held: true, PrivateProject: true}},
		{"/api/v1/projects/{key}/access/groups/{name}", "/api/v1/projects/PRIV/access/groups/r14-group",
			Path{Text: "/api/v1/projects/PRIV/access/groups/{name}", Held: true, PrivateProject: true}},
		// A key is a key in any case, and an identifier is written escaped, as the whole path is.
		{"/api/v1/issues/{key}/artifacts/{slug}", "/api/v1/issues/priv-7/artifacts/r14-plan",
			Path{Text: "/api/v1/issues/priv-7/artifacts/{slug}", Held: true, PrivateProject: true}},
		{"/api/v1/issues/{key}/messages/{id}", "/api/v1/issues/PRIV-7/messages/a%2Fb%20c",
			Path{Text: "/api/v1/issues/PRIV-7/messages/a%2Fb%20c", Held: true, PrivateProject: true}},
		// A name that runs to the end of the path is one placeholder, and {$} names no text.
		{"/api/v1/projects/{key}/files/{slug...}", "/api/v1/projects/PRIV/files/r14/plan.md",
			Path{Text: "/api/v1/projects/PRIV/files/{slug...}", Held: true, PrivateProject: true}},
		{"/api/v1/projects/{key}/{$}", "/api/v1/projects/PRIV/",
			Path{Text: "/api/v1/projects/PRIV/", Held: true, PrivateProject: true}},
		// Another project's paths, a key that only starts with the private one's included, are whole.
		{"/api/v1/projects/{key}/artifacts/{slug}", "/api/v1/projects/PUB/artifacts/r14-plan",
			Path{Text: "/api/v1/projects/PUB/artifacts/r14-plan"}},
		{"/api/v1/projects/{key}/artifacts/{slug}", "/api/v1/projects/PRIVX/artifacts/r14-plan",
			Path{Text: "/api/v1/projects/PRIVX/artifacts/r14-plan"}},
		{"/api/v1/issues/{key}/artifacts/{slug}", "/api/v1/issues/PUB-7/artifacts/r14-plan",
			Path{Text: "/api/v1/issues/PUB-7/artifacts/r14-plan"}},
		{"/api/v1/issues/{key}/artifacts/{slug}", "/api/v1/issues/PRIV-x/artifacts/r14-plan",
			Path{Text: "/api/v1/issues/PRIV-x/artifacts/r14-plan"}},
	} {
		if got := served(t, test.route, http.MethodGet, test.target); got != test.want {
			t.Errorf("%s under %s = %+v, want %+v", test.target, test.route, got, test.want)
		}
	}
}

// While the private projects cannot be read, every keyed path is a private project's.
func TestAnUnknownSetHoldsEveryKeyedPath(t *testing.T) {
	private(t, false)
	want := Path{Text: "/api/v1/projects/PUB/artifacts/{slug}", Held: true, PrivateProject: true}
	if got := served(t, "/api/v1/projects/{key}/artifacts/{slug}", http.MethodGet, "/api/v1/projects/PUB/artifacts/r14-plan"); got != want {
		t.Fatalf("a public project's path with the set unknown = %+v, want %+v", got, want)
	}
}

// A route with a name and no key writes its names as placeholders while any project is private or
// the set is unknown, and names no private project; a route with only identifiers is whole.
func TestARouteWithNoKeyHoldsItsNamesWhileHolding(t *testing.T) {
	for _, known := range []bool{true, false} {
		private(t, known, "PRIV")
		want := Path{Text: "/api/v1/agents/s-1/artifacts/{slug}", Held: true}
		if got := served(t, "/api/v1/agents/{session_id}/artifacts/{slug}", http.MethodGet, "/api/v1/agents/s-1/artifacts/r14-plan"); got != want {
			t.Errorf("known %t: the agents' artifact = %+v, want %+v", known, got, want)
		}
		want = Path{Text: "/api/v1/access-groups/{name}", Held: true}
		if got := served(t, "/api/v1/access-groups/{name}", http.MethodGet, "/api/v1/access-groups/r14-group"); got != want {
			t.Errorf("known %t: an access group = %+v, want %+v", known, got, want)
		}
		if got := served(t, "/api/v1/asks/{id}", http.MethodGet, "/api/v1/asks/r14-plan"); got != (Path{Text: "/api/v1/asks/r14-plan"}) {
			t.Errorf("known %t: an ask's path = %+v, want it whole", known, got)
		}
	}
}

// While holding, the unmatched route and a request served under no route write no path, and the
// proxy writes whole only a path one of its classes reads.
func TestTheUnmatchedAndProxyRoutesHoldWhatNoRouteNames(t *testing.T) {
	for _, known := range []bool{true, false} {
		private(t, known, "PRIV")
		for _, test := range []struct {
			route, target string
			want          Path
		}{
			{Unmatched, "/api/v1/r14-plan", Path{Text: "{unmatched}", Held: true}},
			{githubpath.Prefix, "/api/github/rest/r14-plan", Path{Text: "/api/github/rest/{path}", Held: true}},
			{githubpath.Prefix, "/api/github/rest/repos/acme/r14-repo/contents/r14-plan", Path{Text: "/api/github/rest/{path}", Held: true}},
			{githubpath.Prefix, "/api/github/rest/repos/acme/r14-repo/pulls/7", Path{Text: "/api/github/rest/repos/acme/r14-repo/pulls/7"}},
			{githubpath.Prefix, "/api/github/rest/repos/acme/r14-repo/actions/runs", Path{Text: "/api/github/rest/repos/acme/r14-repo/actions/runs"}},
		} {
			if got := served(t, test.route, http.MethodGet, test.target); got != test.want {
				t.Errorf("known %t: %s under %s = %+v, want %+v", known, test.target, test.route, got, test.want)
			}
		}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/PRIV/artifacts/r14-plan", nil)
		if got := Of(RouteOf(request), request); got != (Path{Text: "{unmatched}", Held: true}) {
			t.Errorf("known %t: a request served under no route = %+v, want no path", known, got)
		}
	}
}

// RouteOf is the pattern the mux served a request under, without its method and host.
func TestRouteOfDropsTheMethodAndHost(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /api/v1/issues/{key}":        "/api/v1/issues/{key}",
		"/api/github/rest/":               "/api/github/rest/",
		"GET example.com/api/v1/whoami":   "/api/v1/whoami",
		"example.com/api/v1/issues/{key}": "/api/v1/issues/{key}",
		"":                                "",
	} {
		if got := RouteOf(&http.Request{Pattern: pattern}); got != want {
			t.Errorf("RouteOf(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// Holding is true while a project is private or the set is unknown, and false with none and the
// set read, as with no Use at all.
func TestHoldingFollowsTheSet(t *testing.T) {
	if Holding() {
		t.Fatal("with no Use, Holding() = true")
	}
	private(t, true)
	if Holding() {
		t.Error("with an empty known set, Holding() = true")
	}
	private(t, false)
	if !Holding() {
		t.Error("with an unknown set, Holding() = false")
	}
	private(t, true, "PRIV")
	if !Holding() {
		t.Error("with PRIV private, Holding() = false")
	}
}
