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
	_, background := mount(newKeyedCensusContext(t), mux)
	t.Cleanup(background.Wait)
	return mux
}

// Every route the server serves is mounted by one function (mount), the API's table among them,
// so the census reads the routes from that mounting rather than from a list kept beside it. Each
// that serves a write - a POST or PATCH pattern, a PUT or DELETE one under /api/v2, which keys
// those too, or one that names no method and so serves every method - is a row of the API's
// table, which the API wraps in its keyed layer and walks row by row (the api package's
// TestAKeyedWriteNeverRunsUnkeyed), or one of browserWrites. A write mounted later beside them
// fails here, as the control's does.
func TestAKeyedWriteNeverRunsUnkeyed(t *testing.T) {
	t.Parallel()
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
	ctx, err := BuildAppContext(withDocuments(t, AppContextOptions{
		SigningKey:       testSigningKey,
		People:           people,
		Identity:         identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Lifetime:         t.Context(),
		TestHooksEnabled: true,
	}))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	return ctx
}

// writesBesideTheTable is every pattern of mux that serves a write and is no row of the API's
// table, which GET /api/v1 and GET /api/v2 list between them, sorted.
func writesBesideTheTable(t *testing.T, mux *recordingMux) []string {
	t.Helper()
	type indexEntry struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	table := map[string]bool{}
	for _, read := range []struct {
		path string
		list func([]byte) ([]indexEntry, error)
	}{
		{"/api/v1", func(body []byte) ([]indexEntry, error) {
			var index struct {
				Routes []indexEntry `json:"routes"`
			}
			err := json.Unmarshal(body, &index)
			return index.Routes, err
		}},
		{"/api/v2", func(body []byte) ([]indexEntry, error) {
			var index struct {
				Items []indexEntry `json:"items"`
			}
			err := json.Unmarshal(body, &index)
			return index.Items, err
		}},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://dispatch.test"+read.path, nil))
		routes, err := read.list(response.Body.Bytes())
		if err != nil || len(routes) == 0 {
			t.Fatalf("GET %s answered %d %s (%v), want the table's index", read.path, response.Code, response.Body.String(), err)
		}
		for _, route := range routes {
			table[route.Method+" "+route.Path] = true
		}
	}
	var outside []string
	for _, pattern := range mux.patterns {
		method, path, named := strings.Cut(pattern, " ")
		write := method == http.MethodPost || method == http.MethodPatch ||
			((method == http.MethodPut || method == http.MethodDelete) && strings.HasPrefix(path, "/api/v2/"))
		if (!named || write) && !table[pattern] {
			outside = append(outside, pattern)
		}
	}
	slices.Sort(outside)
	return outside
}
