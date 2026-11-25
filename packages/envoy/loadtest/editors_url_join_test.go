package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TestAnEditorDialsItsDocumentSocketJoinedOntoTheDispatchURL: the Dispatch URL can carry a path
// and a query. An editor dials its document's socket at that path with /ws/doc/<artifact> joined
// onto it, the URL's query kept ahead of the socket's schema_version: http://host/prefix?key=...
// dials ws://host/prefix/ws/doc/<artifact>?key=...&schema_version=<n> (query), and so does
// http://host/prefix/?key=... (query-trailing-slash), where the socket's path appended to the
// URL's text sat inside the query and the room was asked /prefix. A URL with no query dials where
// it did (no-query). A stand-in records the path and the query of the first upgrade it is asked,
// and refuses it.
func TestAnEditorDialsItsDocumentSocketJoinedOntoTheDispatchURL(t *testing.T) {
	const artifact = "0b6a2c8e-7f4d-4c1a-9e7b-3d2f1a0c9b8e"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	for _, c := range joinURLCases {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case asked <- *r.URL:
				default:
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Now()
			st := &step{
				cfg: runConfig{
					ep:      endpoints{dispatch: srv.URL + c.suffix, pg: "postgres://join@127.0.0.1:1/dispatch?connect_timeout=1"},
					editors: editorConfig{editors: 1, rate: 1, docs: 1},
				},
				lg:    log.New(io.Discard, "", 0),
				sf:    seedFile{Issues: []seedIssue{{Key: "join-doc", ArtifactID: artifact}}},
				start: now, winStart: now, winEnd: now.Add(time.Minute),
			}
			w := st.startEditors(ctx)
			if w == nil {
				t.Fatal("startEditors started no editor")
			}
			var got url.URL
			select {
			case got = <-asked:
			case <-time.After(30 * time.Second):
				t.Fatal("no editor dialled the stand-in")
			}
			cancel()
			w.wait()
			if want := "/prefix/ws/doc/" + artifact; got.Path != want {
				t.Errorf("a Dispatch URL ending %q dialled the socket at path %q (query %q); want %q", c.suffix, got.Path, got.RawQuery, want)
			}
			if want := joinWantQuery(c.query, "schema_version="+schema); got.RawQuery != want {
				t.Errorf("a Dispatch URL ending %q dialled the socket with query %q; want %q", c.suffix, got.RawQuery, want)
			}
		})
	}
}
