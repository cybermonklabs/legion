package main

import (
	"slices"
	"testing"
)

// TestAnEndpointFlagKeepsTheSlashThatEndsItsQuery: an endpoint flag's URL whose query ends in a
// slash (http://host/p?next=/, as a signed URL's base64 can) is taken as it came, its query whole
// (<flag>-query), where the trim of the value's text cut the query's slash and every request asked
// with next empty. A URL whose path ends in a slash still loses it (<flag>-trailing-slash), so a
// value that differs from the stack's by that slash alone is the stack's own. The flags are
// -url, -listener, -nats-mon and -pprof (resolveEndpoints, under -allow-foreign) and each
// -loop-listeners endpoint (parseLoopListeners).
func TestAnEndpointFlagKeepsTheSlashThatEndsItsQuery(t *testing.T) {
	const query, slashed, trimmed = "http://join.example:8080/p?next=/", "http://join.example:8080/p/", "http://join.example:8080/p"
	flags := []struct {
		name string
		set  func(f *endpointFlags, v string)
		got  func(e endpoints) string
	}{
		{"url", func(f *endpointFlags, v string) { f.url = v }, func(e endpoints) string { return e.dispatch }},
		{"listener", func(f *endpointFlags, v string) { f.listener = v }, func(e endpoints) string { return e.listener }},
		{"nats-mon", func(f *endpointFlags, v string) { f.natsMon = v }, func(e endpoints) string { return e.natsMon }},
		{"pprof", func(f *endpointFlags, v string) { f.pprof = v }, func(e endpoints) string { return e.pprof }},
	}
	for _, flag := range flags {
		for _, c := range []struct{ name, given, want string }{
			{"query", query, query},
			{"trailing-slash", slashed, trimmed},
		} {
			t.Run(flag.name+"-"+c.name, func(t *testing.T) {
				f := foreignFlags()
				flag.set(&f, c.given)
				e, err := resolveEndpoints(f, testStack())
				if err != nil {
					t.Fatalf("-%s %q was refused: %v", flag.name, c.given, err)
				}
				if got := flag.got(e); got != c.want {
					t.Errorf("-%s %q resolved to %q; want %q", flag.name, c.given, got, c.want)
				}
			})
		}
	}
	for _, c := range []struct{ name, given, want string }{
		{"query", query, query},
		{"trailing-slash", slashed, trimmed},
	} {
		t.Run("loop-listeners-"+c.name, func(t *testing.T) {
			eps, err := parseLoopListeners(c.given + ",http://join.example:8081")
			if err != nil {
				t.Fatalf("-loop-listeners %q was refused: %v", c.given, err)
			}
			if want := []string{c.want, "http://join.example:8081"}; !slices.Equal(eps, want) {
				t.Errorf("-loop-listeners %q read as %q; want %q", c.given, eps, want)
			}
		})
	}
}
