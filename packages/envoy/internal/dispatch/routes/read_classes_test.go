package routes

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// TestEveryGETRouteHasAReadClass (c-read-class) reads every pattern the router's own mux serves,
// out of the mux itself and from no list in the test, and fails for each GET pattern with no read
// class. A pattern has one when it is a row of the API route table, which the router's own GET
// /api/v1 lists and whose GET rows the api package classes (its TestEveryGETRouteHasAReadClass,
// and register's refusal), or when it was mounted through readclass.Handle, which takes the class
// as an argument. What is left is a GET pattern mounted on the mux directly, named here.
//
// The router is built with every optional route mounted: the dev sign-in route and the document
// socket. The walk's own controls are below it: every row of the route index and every pattern
// readclass recorded must be among the patterns it read, so a reader of the mux that saw nothing
// fails rather than passes, and the GitHub proxy must be among the classed patterns.
func TestEveryGETRouteHasAReadClass(t *testing.T) {
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
	mux := http.NewServeMux()
	mount(ctx, mux)

	served := servedPatterns(t, mux)
	classed := readclass.Patterns(mux)
	tableRows := routeIndexRows(t, mux)
	for _, pattern := range slices.Concat(tableRows, classed) {
		if !slices.Contains(served, pattern) {
			t.Fatalf("the router's mux serves %q, and the walk of its patterns did not read it: the walk is blind (it read %q)", pattern, served)
		}
	}

	reads := 0
	for _, pattern := range served {
		if !servesGET(pattern) {
			continue
		}
		reads++
		if !slices.Contains(tableRows, pattern) && !slices.Contains(classed, pattern) {
			t.Errorf("%s is mounted on the router's mux with no read class: mount it through readclass.Handle", pattern)
		}
	}
	if reads == 0 {
		t.Fatal("the router's mux serves no GET pattern, so no read class was checked")
	}
	for _, floor := range []string{githubapi.Prefix, "GET /ws/live", "GET /ws/doc/{room}"} {
		if !slices.Contains(classed, floor) {
			t.Errorf("%s is not among the patterns mounted through readclass.Handle on the router's mux: %q", floor, classed)
		}
	}
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
func routeIndexRows(t *testing.T, mux *http.ServeMux) []string {
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

// servedPatterns reads every pattern mux serves out of net/http's own routing index
// (net/http.routingIndex), which no exported API lists: each pattern is in its multis or, once
// for each of its segments, in its segments. A ServeMux that no longer holds that index fails the
// test, naming what it could not read, rather than reading as a mux that serves nothing.
func servedPatterns(t *testing.T, mux *http.ServeMux) []string {
	t.Helper()
	index := reflect.ValueOf(mux).Elem().FieldByName("index")
	if !index.IsValid() {
		t.Fatal("net/http.ServeMux holds no index field: the walk cannot read what the mux serves")
	}
	segments, multis := index.FieldByName("segments"), index.FieldByName("multis")
	if segments.Kind() != reflect.Map || multis.Kind() != reflect.Slice {
		t.Fatalf("net/http's routing index holds segments %s and multis %s, not the map and slice the walk reads", segments.Kind(), multis.Kind())
	}
	seen := map[string]bool{}
	read := func(patterns reflect.Value) {
		for i := range patterns.Len() {
			text := patterns.Index(i).Elem().FieldByName("str")
			if text.Kind() != reflect.String {
				t.Fatalf("net/http's pattern holds no str string: the walk cannot read what the mux serves")
			}
			seen[text.String()] = true
		}
	}
	read(multis)
	for entry := segments.MapRange(); entry.Next(); {
		read(entry.Value())
	}
	return slices.Sorted(maps.Keys(seen))
}
