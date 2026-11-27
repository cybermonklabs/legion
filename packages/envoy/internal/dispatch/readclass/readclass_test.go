package readclass_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// TestHandleRecordsEveryPattern: two patterns mounted through Handle on a Recorder are each
// recorded on it with the classes they were mounted with, and served by it; a pattern mounted on
// it directly is served with no class recorded; and a mux that is no Recorder serves what Handle
// mounts on it. Each pattern is named, so a Handle that stopped recording fails here naming the
// first one it dropped.
func TestHandleRecordsEveryPattern(t *testing.T) {
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux := &recordingMux{}
	zeta := []readclass.Class{readclass.Deployment}
	readclass.Handle(mux, zeta, "GET /zeta", nothing)
	readclass.Handle(mux, []readclass.Class{readclass.Project, readclass.People}, "GET /alpha/{key}", nothing)
	mux.HandleFunc("GET /direct", nothing)
	// The Recorder keeps a copy: the caller's slice is its own to change after the mount.
	zeta[0] = readclass.None

	if want := []string{"GET /zeta", "GET /alpha/{key}", "GET /direct"}; !slices.Equal(mux.patterns, want) {
		t.Fatalf("the recorder holds the mounts %q, want %q, in the order they were made", mux.patterns, want)
	}
	for pattern, want := range map[string][]readclass.Class{
		"GET /zeta":        {readclass.Deployment},
		"GET /alpha/{key}": {readclass.Project, readclass.People},
	} {
		if got := mux.classes[pattern]; !slices.Equal(got, want) {
			t.Fatalf("Handle recorded %q for %s, want the classes it was mounted with, %q", got, pattern, want)
		}
	}
	if got, recorded := mux.classes["GET /direct"]; recorded {
		t.Fatalf("a pattern mounted on the recorder directly was recorded with %q, want no class", got)
	}
	// The recorder serves what Handle mounted: the record is beside the mount, not instead of it.
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/alpha/KEY", nil)); pattern != "GET /alpha/{key}" {
		t.Fatalf("the recorder routes GET /alpha/KEY to %q, want the mounted pattern", pattern)
	}
	plain := http.NewServeMux()
	readclass.Handle(plain, []readclass.Class{readclass.None}, "GET /plain", nothing)
	if _, pattern := plain.Handler(httptest.NewRequest(http.MethodGet, "/plain", nil)); pattern != "GET /plain" {
		t.Fatalf("a ServeMux routes GET /plain, mounted through Handle, to %q, want the mounted pattern", pattern)
	}
}

// A mount names at least one class, and only a class this package defines: a mount with none,
// or with a value spelled outside the set, is refused before it serves anything.
func TestHandleRefusesAMountWithNoKnownClass(t *testing.T) {
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for name, classes := range map[string][]readclass.Class{"no class": nil, "an unknown class": {"everyone"}} {
		t.Run(name, func(t *testing.T) {
			mux := &recordingMux{}
			defer func() {
				if recover() == nil {
					t.Fatalf("Handle mounted GET /refused with %s", name)
				}
				if len(mux.patterns) != 0 || len(mux.classes) != 0 {
					t.Fatalf("a refused mount was mounted (%q) or recorded (%q)", mux.patterns, mux.classes)
				}
			}()
			readclass.Handle(mux, classes, "GET /refused", nothing)
		})
	}
}
