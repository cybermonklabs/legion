package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAProxysStatsThatDoNotDecodeNameNoneOfTheirWords: the stack's proxy, or whatever answers in
// its place, serves its wire counts with a time that quotes a credential's URL, and the rooms'
// window reading noted the decoder's error, which quoted the time; the summary prints that note.
// The note names the answer as not the JSON it reads and none of it. The control: it still says
// the proxy serves no wire counts. Captures loadtest.answer-members/proxy-stats.
func TestAProxysStatsThatDoNotDecodeNameNoneOfTheirWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stats" {
			writeJSONAnswer(w, http.StatusOK, map[string]any{"at": redacttest.Words})
			return
		}
		writeJSONAnswer(w, http.StatusOK, map[string]any{})
	}))
	t.Cleanup(srv.Close)
	r := readWindow(context.Background(), endpoints{proxyStats: srv.URL, pprof: srv.URL, pg: "postgres://127.0.0.1:1/none"})
	redacttest.RequireNone(t, "loadtest.answer-members/proxy-stats", r.proxyErr)
	if r.proxy != nil || !strings.HasPrefix(r.proxyErr, "the proxy serves no wire counts: ") {
		t.Errorf("the window reading's proxy note is %q, want it to say the proxy serves no wire counts", r.proxyErr)
	}
}

// TestAHeapProfileItCannotReadNamesNoneOfItsWords: Dispatch's profiler, or whatever answers in
// its place, serves a heap profile in a legacy text form whose attribute line quotes a
// credential's URL (parse), or a profile whose only sample type does (sample-types). The heap
// read's error printed the parser's error, which quoted the line, or the profile's sample types;
// it names neither. The control: it still names the read. Captures
// loadtest.answer-members/heap-<case>.
func TestAHeapProfileItCannotReadNamesNoneOfItsWords(t *testing.T) {
	var typed bytes.Buffer
	if err := (&profile.Profile{SampleType: []*profile.ValueType{{Type: redacttest.Words, Unit: "count"}}}).Write(&typed); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"parse", []byte("--- contentionz 1 ---\nsampling period = x " + redacttest.Words + "\n")},
		{"sample-types", typed.Bytes()},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(c.body) }))
			t.Cleanup(srv.Close)
			_, _, err := heapLive(context.Background(), srv.URL)
			redacttest.RequireNone(t, "loadtest.answer-members/heap-"+c.name, errorText(err))
			if !strings.Contains(errorText(err), "/debug/pprof/heap: ") {
				t.Errorf("heapLive = %v, want an error naming the heap read", err)
			}
		})
	}
}
