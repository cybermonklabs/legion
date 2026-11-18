package routes

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// browserWrites is every route the server mounts beside the API's table that serves a write, and
// why it takes no Idempotency-Key: each is the browser's, and no agent client calls it.
var browserWrites = map[string]string{
	"POST /auth/logout": "the sign-out resolves its caller and refuses one it cannot, and a second run revokes nothing more",
	githubapi.Prefix:    "the GitHub proxy answers every method but GET 405 and writes nothing",
	"/":                 "the dashboard's files are read and never written",
}

// recordingMux is the ServeMux the census hands the real mounting code (mount), keeping the
// pattern of each route mounted on it.
type recordingMux struct {
	http.ServeMux
	patterns []string
}

var _ api.RouteMux = (*recordingMux)(nil)

func (m *recordingMux) Handle(pattern string, handler http.Handler) {
	m.ServeMux.Handle(pattern, handler)
	m.patterns = append(m.patterns, pattern)
}

func (m *recordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

// mounted is a recordingMux the server's own mounting code (mount) filled.
func mounted(t *testing.T) *recordingMux {
	t.Helper()
	mux := &recordingMux{}
	mount(newKeyedCensusContext(t), mux)
	return mux
}

// Every route the server serves is mounted by one function (mount), the API's table among them,
// so the census reads the routes from that mounting rather than from a list kept beside it. Each
// that serves a write - a POST or PATCH pattern, or one that names no method and so serves every
// method - is a row of the API's table, which the API wraps in its keyed layer and walks row by row
// (the api package's TestAKeyedWriteNeverRunsUnkeyed), or one of browserWrites. A write mounted
// later beside them fails here, as the control's does.
func TestAKeyedWriteNeverRunsUnkeyed(t *testing.T) {
	t.Run("control", func(t *testing.T) {
		mux := mounted(t)
		const planted = "POST /api/v1/planted-outside-the-table"
		mux.HandleFunc(planted, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		want := append(slices.Collect(maps.Keys(browserWrites)), planted)
		slices.Sort(want)
		if outside := writesBesideTheTable(t, mux); !slices.Equal(outside, want) {
			t.Fatalf("the census named %q, want the browser's writes and the planted one, %q", outside, want)
		}
	})

	mux := mounted(t)
	if outside, want := writesBesideTheTable(t, mux), slices.Sorted(maps.Keys(browserWrites)); !slices.Equal(outside, want) {
		t.Fatalf("the server mounts these writes beside the API's table: %q, want exactly the browser's %q", outside, want)
	}
	response := httptest.NewRecorder()
	post := httptest.NewRequest(http.MethodPost, "http://dispatch.test"+githubapi.Prefix+"repos/o/r", strings.NewReader("{}"))
	post.Header.Set("X-Dispatch-User", "alice@d.example")
	mux.ServeHTTP(response, post)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a signed-in POST to the GitHub proxy answered %d %s, want 405", response.Code, response.Body.String())
	}
}

// newKeyedCensusContext is a router's context with header identity, no sign-in and the test hooks
// on, as newTestRouter builds one: the hooks mount nine more write rows, so the census covers
// every row any server mounts.
func newKeyedCensusContext(t *testing.T) *AppContext {
	t.Helper()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:       testSigningKey,
		People:           people,
		Identity:         identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:         t.Context(),
		TestHooksEnabled: true,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	return ctx
}

// writesBesideTheTable is every pattern of mux that serves a write and is no row of the API's
// table, which GET /api/v1 lists, sorted.
func writesBesideTheTable(t *testing.T, mux *recordingMux) []string {
	t.Helper()
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/v1", nil))
	var index struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil || len(index.Routes) == 0 {
		t.Fatalf("GET /api/v1 answered %d %s (%v), want the table's index", response.Code, response.Body.String(), err)
	}
	table := map[string]bool{}
	for _, route := range index.Routes {
		table[route.Method+" "+route.Path] = true
	}
	var outside []string
	for _, pattern := range mux.patterns {
		method, _, named := strings.Cut(pattern, " ")
		if (!named || method == http.MethodPost || method == http.MethodPatch) && !table[pattern] {
			outside = append(outside, pattern)
		}
	}
	slices.Sort(outside)
	return outside
}
