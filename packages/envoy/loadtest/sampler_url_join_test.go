package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTheSamplerReadsGoToTheirRoutesJoinedOntoTheEndpointURLs: the sampler reads Dispatch's
// goroutine and heap profiles at -pprof and NATS's /varz and /jsz at -nats-mon, each an endpoint
// URL that can carry a path and a query. Each read goes to that path with its route joined onto
// it, the URL's query kept ahead of the route's own: http://host/prefix?key=... reads
// /prefix/debug/pprof/goroutine?key=...&debug=1, /prefix/debug/pprof/heap?key=...,
// /prefix/varz?key=... and /prefix/jsz?key=... (query), and so does http://host/prefix/?key=...
// (query-trailing-slash), where the route appended to the URL's text sat inside the query, the
// endpoint was asked /prefix and the sample held no reading. A URL with no query reads where it
// did (no-query). A stand-in for both endpoints records the path and the query each read reaches
// it with and answers as each would.
func TestTheSamplerReadsGoToTheirRoutesJoinedOntoTheEndpointURLs(t *testing.T) {
	heap := heapProfileBody(t)
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/debug/pprof/goroutine"):
					_, _ = w.Write([]byte("goroutine profile: total 7\n"))
				case strings.HasSuffix(r.URL.Path, "/debug/pprof/heap"):
					_, _ = w.Write(heap)
				case strings.HasSuffix(r.URL.Path, "/varz"):
					_, _ = w.Write([]byte(`{"connections":3}`))
				case strings.HasSuffix(r.URL.Path, "/jsz"):
					_, _ = w.Write([]byte(`{"streams":2}`))
				default:
					http.NotFound(w, r)
				}
			})
			endpoint := base + c.suffix
			if total, _, err := goroutineCounts(endpoint); err != nil || total != 7 {
				t.Errorf("goroutineCounts = %d, %v; want the stand-in's 7", total, err)
			}
			if b, _, err := heapLive(context.Background(), endpoint); err != nil || b != 4*mib1 {
				t.Errorf("heapLive = %v bytes, %v; want the stand-in's 4 MiB", b, err)
			}
			if v, err := natsVarz(endpoint); err != nil || v["connections"] != 3 {
				t.Errorf("natsVarz = %v, %v; want the stand-in's 3 connections", v, err)
			}
			if v := natsJsz(endpoint); v["js_streams"] != 2 {
				t.Errorf("natsJsz = %v; want the stand-in's 2 streams", v)
			}
			want := []struct{ path, query string }{
				{"/prefix/debug/pprof/goroutine", joinWantQuery(c.query, "debug=1")},
				{"/prefix/debug/pprof/heap", c.query},
				{"/prefix/varz", c.query},
				{"/prefix/jsz", c.query},
			}
			got := asked()
			for i, u := range got {
				if i >= len(want) || u.Path != want[i].path || u.RawQuery != want[i].query {
					t.Errorf("read %d of an endpoint URL ending %q went to path %q with query %q; want %+v", i+1, c.suffix, u.Path, u.RawQuery, want)
				}
			}
			if len(got) != len(want) {
				t.Errorf("the endpoints were asked %d times; want %d", len(got), len(want))
			}
		})
	}
}

// TestASamplerEndpointURLThatDoesNotJoinPrintsNoneOfIt: a -pprof or -nats-mon URL url.Parse
// refuses (an invalid escape in its path), with redacttest's credentials in its user information
// and its query, fails the sampler's goroutine, heap and /varz reads in fixed words that name the
// route and none of the URL (<read>-unparsed), where each read failed with http.NewRequest's error,
// which quotes the URL whole (twin). An endpoint URL that parses, whose joined URL, as
// url.URL.String writes it, url.Parse refuses (an IPv6 zone escape), fails them in fixed words too
// (<read>-reparsed); its twin shows http.NewRequest's error for the joined URL holds the
// credentials. Captures loadtest.sampler-url/<read>-<case>.
func TestASamplerEndpointURLThatDoesNotJoinPrintsNoneOfIt(t *testing.T) {
	unparsed, reparsed := joinRefusedURLs("/endpoint")
	joinRequireTwins(t, unparsed, reparsed, "/varz")
	reads := []struct {
		name, route string
		read        func(endpoint string) error
	}{
		{"pprof", "/debug/pprof/goroutine?debug=1", func(endpoint string) error {
			_, _, err := goroutineCounts(endpoint)
			return err
		}},
		{"heap", "/debug/pprof/heap", func(endpoint string) error {
			_, _, err := heapLive(context.Background(), endpoint)
			return err
		}},
		{"nats", "/varz", func(endpoint string) error {
			_, err := natsVarz(endpoint)
			return err
		}},
	}
	for _, read := range reads {
		for _, c := range []struct{ name, url, want string }{
			{"unparsed", unparsed, "GET " + read.route + ": the URL it is joined onto is not an absolute URL with a host (it is not printed)"},
			{"reparsed", reparsed, "the endpoint's URL joined with the route does not parse (it is not printed)"},
		} {
			t.Run(read.name+"-"+c.name, func(t *testing.T) {
				err := read.read(c.url)
				if err == nil {
					t.Fatal("the read took an endpoint URL that does not join")
				}
				redacttest.RequireNoPiece(t, "loadtest.sampler-url/"+read.name+"-"+c.name, err.Error())
				if err.Error() != c.want {
					t.Errorf("the read's error = %q; want %q", err, c.want)
				}
			})
		}
	}
}
