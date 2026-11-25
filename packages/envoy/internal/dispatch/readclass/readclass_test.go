package readclass

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"testing"
	"time"
)

// TestHandleRecordsEveryPattern: two patterns mounted through Handle on one mux both appear in
// Patterns of that mux, sorted, each with the classes it was mounted with, and nothing appears for
// a second mux. Each pattern is named, so a Handle that stopped recording fails here naming the
// first one it dropped.
func TestHandleRecordsEveryPattern(t *testing.T) {
	served, other := http.NewServeMux(), http.NewServeMux()
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	Handle(served, []Class{Deployment}, "GET /zeta", nothing)
	Handle(served, []Class{Project, People}, "GET /alpha/{key}", nothing)

	got := Patterns(served)
	for _, pattern := range []string{"GET /alpha/{key}", "GET /zeta"} {
		if !slices.Contains(got, pattern) {
			t.Fatalf("Patterns of the mux Handle mounted %q on answers %q, without it", pattern, got)
		}
	}
	if want := []string{"GET /alpha/{key}", "GET /zeta"}; !slices.Equal(got, want) {
		t.Fatalf("Patterns = %q, want %q, sorted", got, want)
	}
	if mounted := Patterns(other); len(mounted) != 0 {
		t.Fatalf("Patterns of a mux nothing was mounted on = %q, want none", mounted)
	}
	for pattern, want := range map[string][]Class{"GET /zeta": {Deployment}, "GET /alpha/{key}": {Project, People}} {
		if got := ClassesOf(served, pattern); !slices.Equal(got, want) {
			t.Fatalf("ClassesOf %q = %q, want the classes it was mounted with, %q", pattern, got, want)
		}
		if got := ClassesOf(other, pattern); got != nil {
			t.Fatalf("ClassesOf %q on a mux nothing was mounted on = %q, want none", pattern, got)
		}
	}
	// The mux serves what Handle mounted: the record is beside the mount, not instead of it.
	if _, pattern := served.Handler(httptest.NewRequest(http.MethodGet, "/alpha/KEY", nil)); pattern != "GET /alpha/{key}" {
		t.Fatalf("the mux routes GET /alpha/KEY to %q, want the mounted pattern", pattern)
	}
}

// A mount names at least one class, and only a class this package defines: a mount with none,
// or with a value spelled outside the set, is refused before it serves anything.
func TestHandleRefusesAMountWithNoKnownClass(t *testing.T) {
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for name, classes := range map[string][]Class{"no class": nil, "an unknown class": {"everyone"}} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			defer func() {
				if recover() == nil {
					t.Fatalf("Handle mounted GET /refused with %s", name)
				}
				if mounted := Patterns(mux); len(mounted) != 0 {
					t.Fatalf("a refused mount was recorded: %q", mounted)
				}
			}()
			Handle(mux, classes, "GET /refused", nothing)
		})
	}
}

// The record of a mux goes when the mux does, so the many muxes a test run builds leave nothing
// behind in the process.
func TestARecordLeavesWithItsMux(t *testing.T) {
	before := recordedMuxes()
	func() {
		mux := http.NewServeMux()
		Handle(mux, []Class{None}, "GET /short-lived", http.NotFoundHandler())
	}()
	deadline := time.Now().Add(10 * time.Second)
	for recordedMuxes() > before {
		if time.Now().After(deadline) {
			t.Fatalf("the record of a collected mux is still held: %d muxes recorded, %d before it", recordedMuxes(), before)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
