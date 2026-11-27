package readclass_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// recordingMux is the mux a test owns and hands the server's mounting code: a ServeMux that keeps
// the pattern of each mount made on it, through readclass.Handle or directly, and, as a
// readclass.Recorder, the classes Handle mounted each of its patterns with.
type recordingMux struct {
	http.ServeMux
	patterns []string
	classes  map[string][]readclass.Class
}

var (
	_ api.RouteMux       = (*recordingMux)(nil)
	_ readclass.Recorder = (*recordingMux)(nil)
)

func (m *recordingMux) Handle(pattern string, handler http.Handler) {
	m.ServeMux.Handle(pattern, handler)
	m.patterns = append(m.patterns, pattern)
}

func (m *recordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(handler))
}

func (m *recordingMux) Record(pattern string, classes []readclass.Class) {
	if m.classes == nil {
		m.classes = map[string][]readclass.Class{}
	}
	m.classes[pattern] = classes
}

// unclassed is every pattern mounted on m that is none of table and that Handle recorded no class
// for, in the order it was mounted.
func (m *recordingMux) unclassed(table []string) []string {
	var unclassed []string
	for _, pattern := range m.patterns {
		if _, classed := m.classes[pattern]; !classed && !slices.Contains(table, pattern) {
			unclassed = append(unclassed, pattern)
		}
	}
	return unclassed
}

// The two sockets api.Register mounts beside its route table.
var sockets = []string{"GET /ws/doc/{room}", "GET /ws/live"}

// TestEveryMountedPatternHasAReadClass hands api.Register, which mounts the API's route table and
// both sockets, a recordingMux, and reads back every pattern it mounted, of any method. Each is a
// row of the route table, whose GET rows the api package classes (readSources: register refuses a
// table with a GET row it does not class), or was mounted through readclass.Handle with its
// classes. Any other was mounted on the mux directly, with no class, and fails the test, named.
// Register mounts the document socket only for a document service that serves its websocket, so
// it is given one.
//
// The walk's controls run every time: every row of the route index and both sockets must be among
// the mounts it read, so a recorder that saw nothing fails rather than passes, and a GET pattern
// mounted on the mux directly beside them must be the one pattern it names.
func TestEveryMountedPatternHasAReadClass(t *testing.T) {
	table := routeTableRows(t)
	mux := registered(t)
	for _, pattern := range slices.Concat(table, sockets) {
		if !slices.Contains(mux.patterns, pattern) {
			t.Fatalf("api.Register mounts %s, and the recorder it was handed did not see it: the walk is blind (it read %q)", pattern, mux.patterns)
		}
	}
	for _, pattern := range mux.unclassed(table) {
		t.Errorf("api.Register mounts %s on its mux directly, with no read class: mount it through readclass.Handle", pattern)
	}

	t.Run("a pattern mounted on the mux with no class fails the walk", func(t *testing.T) {
		mux := registered(t)
		const planted = "GET /planted-with-no-read-class"
		mux.HandleFunc(planted, func(http.ResponseWriter, *http.Request) {})
		if named, want := mux.unclassed(table), []string{planted}; !slices.Equal(named, want) {
			t.Fatalf("the walk named %q, want the planted mount alone, %q", named, want)
		}
	})
}

// registered is a recordingMux api.Register mounted the API on, with a document service that
// serves its websocket and nothing else: register reads no Store and dials nothing.
func registered(t *testing.T) *recordingMux {
	t.Helper()
	mux := &recordingMux{}
	t.Cleanup(api.Register(mux, t.Context(), api.Deps{Docs: documentSocket{}}).Wait)
	return mux
}

// documentSocket stands in for the document service, so api.Register mounts GET /ws/doc/{room}.
type documentSocket struct{ docs.API }

func (documentSocket) ServeHTTP(http.ResponseWriter, *http.Request) {}

// routeTableRows is every row of the route table a server without test hooks mounts, "<method>
// <path>" per row, as GET /api/v1 lists them (api.WriteRouteIndex).
func routeTableRows(t *testing.T) []string {
	t.Helper()
	var written bytes.Buffer
	if err := api.WriteRouteIndex(&written); err != nil {
		t.Fatalf("write the route index: %v", err)
	}
	var index struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(written.Bytes(), &index); err != nil || len(index.Routes) == 0 {
		t.Fatalf("the route index %s (%v) holds no row", written.String(), err)
	}
	rows := make([]string, len(index.Routes))
	for i, route := range index.Routes {
		rows[i] = route.Method + " " + route.Path
	}
	return rows
}
