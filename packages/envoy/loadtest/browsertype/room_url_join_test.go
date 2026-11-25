package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TestAPeersRoomGoesToThePathJoinedOntoTheURL: -url is the Dispatch base URL, and it can carry a
// path, a query or a fragment. Each peer's room is the URL's path with /ws/doc/<artifact> joined
// onto it, the URL's own query kept ahead of the schema version and its fragment dropped:
// http://host/dispatch?key=... opens /dispatch/ws/doc/<artifact>?key=...&schema_version=N (query),
// and so does http://host/dispatch/?key=... (query-trailing-slash), where the room's query was
// written over the URL's and the server was asked without key=...;
// http://host/dispatch#pane opens /dispatch/ws/doc/<artifact> with no fragment (fragment), which
// no request sends. A URL with neither opens where it did (no-query). A stand-in records the path
// and the query the room reaches it with, read over http, which is the request the websocket dial
// sends.
func TestAPeersRoomGoesToThePathJoinedOntoTheURL(t *testing.T) {
	const artifact = "7c0e6b3a-0b2e-4c21-9f7f-62b7f7d0c111"
	schema := strconv.Itoa(pmdoc.SchemaVersion())
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dispatch?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "/dispatch/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"fragment", "/dispatch#pane", ""},
		{"no-query", "/dispatch", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
			}))
			t.Cleanup(srv.Close)
			base, err := url.Parse(srv.URL + c.suffix)
			if err != nil {
				t.Fatalf("url.Parse of the stand-in's URL: %v", err)
			}
			room, err := roomURL(base, artifact)
			if err != nil {
				t.Fatalf("roomURL = %v; want the room of a -url ending %q", err, c.suffix)
			}
			if !strings.HasPrefix(room, "ws://") {
				t.Errorf("roomURL = %q; want a ws:// room", room)
			}
			resp, err := http.Get("http" + strings.TrimPrefix(room, "ws"))
			if err != nil {
				t.Fatalf("the stand-in was not reached at the room %q: %v", room, err)
			}
			_ = resp.Body.Close()
			got := <-asked
			if want := "/dispatch/ws/doc/" + artifact; got.Path != want {
				t.Errorf("a -url ending %q opened path %q; want %q", c.suffix, got.Path, want)
			}
			if got.Query().Get("schema_version") != schema {
				t.Errorf("a -url ending %q opened query %q; want schema_version %s in it", c.suffix, got.RawQuery, schema)
			}
			if c.query != "" && !strings.HasPrefix(got.RawQuery, c.query+"&") {
				t.Errorf("a -url ending %q opened query %q; want it to begin with the URL's own %q", c.suffix, got.RawQuery, c.query)
			}
			if c.query == "" && got.Query().Has("key") {
				t.Errorf("a -url ending %q opened query %q; want the schema version alone", c.suffix, got.RawQuery)
			}
			if strings.Contains(room, "#") {
				t.Errorf("the room %q holds a fragment; no request sends one", room)
			}
		})
	}
}

// TestAnHTTPSURLOpensAWSSRoom: the room's scheme follows the URL's, as it did before the join.
func TestAnHTTPSURLOpensAWSSRoom(t *testing.T) {
	base, err := url.Parse("https://dispatch.example/dispatch?key=k3y-7Qz4k")
	if err != nil {
		t.Fatal(err)
	}
	room, err := roomURL(base, "7c0e6b3a-0b2e-4c21-9f7f-62b7f7d0c111")
	if err != nil {
		t.Fatalf("roomURL = %v", err)
	}
	want := "wss://dispatch.example/dispatch/ws/doc/7c0e6b3a-0b2e-4c21-9f7f-62b7f7d0c111?key=k3y-7Qz4k&schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	if room != want {
		t.Errorf("roomURL = %q; want %q", room, want)
	}
}
