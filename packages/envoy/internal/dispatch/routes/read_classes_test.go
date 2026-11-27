package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// TestEveryGETRouteHasAReadClass (c-read-class) hands the router's own mounting code (mount) a
// classedMux, reads back every pattern mounted on it, from the mounting itself and from no list
// in the test, and fails for each GET pattern with no read class. A pattern has one when it is a
// row of the API route table, which the router's own GET /api/v1 lists and whose GET rows the api
// package classes (its TestEveryGETRouteHasAReadClass, and register's refusal), or when it was
// mounted through readclass.Handle, which takes the class as an argument and records it on the
// classedMux. What is left is a GET pattern mounted on the mux directly, named here.
//
// The router is built with every optional route mounted: the dev sign-in route and the document
// socket. The walk's own controls run every time: every row of the route index and every pattern
// Handle recorded must be among the mounts it read, so a recorder that saw nothing fails rather
// than passes; the GitHub proxy and both sockets must be among the classed patterns; and a GET
// pattern mounted on the mux directly beside them must be the one pattern it names.
func TestEveryGETRouteHasAReadClass(t *testing.T) {
	mux := classedRouter(t)
	tableRows := routeIndexRows(t, mux)
	for _, pattern := range tableRows {
		if !slices.Contains(mux.patterns, pattern) {
			t.Fatalf("the router's mux serves the table row %q, and the recorder it was handed did not see it mounted: the walk is blind (it read %q)", pattern, mux.patterns)
		}
	}
	for pattern := range mux.classes {
		if !slices.Contains(mux.patterns, pattern) {
			t.Fatalf("readclass.Handle recorded %q, and the recorder did not see it mounted: the walk is blind (it read %q)", pattern, mux.patterns)
		}
	}

	reads, unclassed := mux.unclassedReads(tableRows)
	if reads == 0 {
		t.Fatal("the router's mux serves no GET pattern, so no read class was checked")
	}
	for _, pattern := range unclassed {
		t.Errorf("%s is mounted on the router's mux with no read class: mount it through readclass.Handle", pattern)
	}
	for _, floor := range []string{githubapi.Prefix, "GET /ws/live", "GET /ws/doc/{room}"} {
		if _, classed := mux.classes[floor]; !classed {
			t.Errorf("%s is not among the patterns mounted through readclass.Handle on the router's mux: %q", floor, mux.classes)
		}
	}

	t.Run("a GET pattern mounted on the mux with no class fails the walk", func(t *testing.T) {
		mux := classedRouter(t)
		const planted = "GET /planted-with-no-read-class"
		mux.HandleFunc(planted, func(http.ResponseWriter, *http.Request) {})
		if _, named := mux.unclassedReads(tableRows); !slices.Equal(named, []string{planted}) {
			t.Fatalf("the walk named %q, want the planted mount alone, %q", named, []string{planted})
		}
	})
}

// classedMux is the recordingMux the read-class walk hands the router's mounting code: beside the
// pattern of each mount, it keeps, as a readclass.Recorder, the classes readclass.Handle mounted
// each of its patterns with.
type classedMux struct {
	recordingMux
	classes map[string][]readclass.Class
}

var _ readclass.Recorder = (*classedMux)(nil)

func (m *classedMux) Record(pattern string, classes []readclass.Class) {
	if m.classes == nil {
		m.classes = map[string][]readclass.Class{}
	}
	m.classes[pattern] = classes
}

// unclassedReads answers how many patterns mounted on m serve a GET, and those of them that are no
// row of table and that readclass.Handle recorded no class for, in the order they were mounted.
func (m *classedMux) unclassedReads(table []string) (reads int, unclassed []string) {
	for _, pattern := range m.patterns {
		if !servesGET(pattern) {
			continue
		}
		reads++
		if _, classed := m.classes[pattern]; !classed && !slices.Contains(table, pattern) {
			unclassed = append(unclassed, pattern)
		}
	}
	return reads, unclassed
}

// classedRouter is a classedMux the router's mounting code filled, with every optional route
// mounted: the dev sign-in route and the document socket.
func classedRouter(t *testing.T) *classedMux {
	t.Helper()
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Sessions:   &memorySessionStore{generations: map[string]int64{}},
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		ServerURL:  devSignInOrigin,
		DevSignIn:  true,
		Docs:       documentSocket{},
		Lifetime:   t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	mux := &classedMux{}
	_, background := mount(ctx, mux)
	t.Cleanup(background.Wait)
	return mux
}

// documentSocket stands in for the document service, so the router mounts GET /ws/doc/{room}:
// api.Register mounts it only for a document service that serves its websocket.
type documentSocket struct{ docs.API }

func (documentSocket) ServeHTTP(http.ResponseWriter, *http.Request) {}

// servesGET reports whether a ServeMux pattern answers a GET: one whose method is GET, and one
// that names no method.
func servesGET(pattern string) bool {
	method, _, named := strings.Cut(pattern, " ")
	return !named || method == http.MethodGet
}

// routeIndexRows reads the route table the router serves, "<method> <path>" per row, from its own
// GET /api/v1.
func routeIndexRows(t *testing.T, mux http.Handler) []string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/v1", nil)
	request.Header.Set("X-Dispatch-User", "alice@d.example")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	var index struct {
		Routes []struct{ Method, Path string } `json:"routes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil || response.Code != http.StatusOK || len(index.Routes) == 0 {
		t.Fatalf("GET /api/v1 = %d %s (%v), want the route index", response.Code, response.Body.String(), err)
	}
	rows := make([]string, len(index.Routes))
	for i, route := range index.Routes {
		rows[i] = route.Method + " " + route.Path
	}
	return rows
}
