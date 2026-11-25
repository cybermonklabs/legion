package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTheProfileFetchesGoToTheirRoutesJoinedOntoThePprofURL: -pprof can carry a path and a
// query. The step's profile fetches (fetchTo) go to that path with each profile's route joined
// onto it, the URL's query kept ahead of the route's own: http://host/prefix?key=... fetches
// /prefix/debug/pprof/profile?key=...&seconds=<n>, /prefix/debug/pprof/goroutine?key=...&debug=2,
// /prefix/debug/pprof/heap?key=... and /prefix/debug/pprof/heap?key=...&debug=1 (query), and so
// does http://host/prefix/?key=... (query-trailing-slash), where the route appended to the URL's
// text sat inside the query, the profiler was asked /prefix and the step saved no profile. A URL
// with no query fetches where it did (no-query). A stand-in records the path and the query of
// each fetch and answers it, and each profile's file holds its answer.
func TestTheProfileFetchesGoToTheirRoutesJoinedOntoThePprofURL(t *testing.T) {
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			base, asked := joinAskedStand(t, func(w http.ResponseWriter, r *http.Request) {
				if filepath.Dir(r.URL.Path) != "/prefix/debug/pprof" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(r.URL.Path))
			})
			now := time.Now()
			st := &step{
				cfg: runConfig{ep: endpoints{pprof: base + c.suffix}, out: t.TempDir()},
				lg:  log.New(io.Discard, "", 0),
				// A window that ended a second after it started, so the CPU profile asks one second.
				winStart: now.Add(-2 * time.Second), winEnd: now.Add(-time.Second),
			}
			st.profile(context.Background())
			want := []struct{ path, query string }{
				{"/prefix/debug/pprof/profile", joinWantQuery(c.query, "seconds=1")},
				{"/prefix/debug/pprof/goroutine", joinWantQuery(c.query, "debug=2")},
				{"/prefix/debug/pprof/heap", c.query},
				{"/prefix/debug/pprof/heap", joinWantQuery(c.query, "debug=1")},
			}
			got := asked()
			for i, u := range got {
				if i >= len(want) || u.Path != want[i].path || u.RawQuery != want[i].query {
					t.Errorf("fetch %d of a -pprof ending %q went to path %q with query %q; want %+v", i+1, c.suffix, u.Path, u.RawQuery, want)
				}
			}
			if len(got) != len(want) {
				t.Errorf("the profiler was asked %d times; want %d", len(got), len(want))
			}
			for file, body := range map[string]string{
				"cpu.pprof": "/prefix/debug/pprof/profile", "goroutines.txt": "/prefix/debug/pprof/goroutine",
				"heap.pprof": "/prefix/debug/pprof/heap", "heap.txt": "/prefix/debug/pprof/heap",
			} {
				if saved, err := os.ReadFile(filepath.Join(st.cfg.out, file)); err != nil || string(saved) != body {
					t.Errorf("%s holds %q, %v; want the answer to %s", file, saved, err, body)
				}
			}
		})
	}
}
