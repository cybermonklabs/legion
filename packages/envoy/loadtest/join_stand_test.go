package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinAskedStand serves, until t ends, a stand-in that records the URL of every request it is
// asked and answers it with answer. It answers its base URL and a reading of the URLs asked so
// far, in the order they came.
func joinAskedStand(t *testing.T, answer http.HandlerFunc) (string, func() []url.URL) {
	t.Helper()
	var mu sync.Mutex
	var asked []url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, *r.URL)
		mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []url.URL {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// joinURLCases are the configured URLs a URL-join row gives a client, each as the suffix of its
// stand-in's base URL, with the query the client must keep: a path and a query (query), the same
// with the path's trailing slash (query-trailing-slash), and a path alone (no-query), which reads
// where it always did.
var joinURLCases = []struct{ name, suffix, query string }{
	{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
	{"query-trailing-slash", "/prefix/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
	{"no-query", "/prefix", ""},
}

// joinWantQuery is the query a request joined onto a configured URL reaches the server with: the
// configured query first, then the request's own.
func joinWantQuery(configured, own string) string {
	switch {
	case configured == "":
		return own
	case own == "":
		return configured
	}
	return configured + "&" + own
}

// joinRefusedURLs are two endpoint URLs with path, each with redacttest's credentials in its user
// information and its query: unparsed, which url.Parse refuses (an invalid escape in its path), and
// reparsed, which url.Parse takes but whose joined URL, as url.URL.String writes it, url.Parse
// refuses (an IPv6 zone holding a byte that is not ASCII, which String escapes as %FD).
func joinRefusedURLs(path string) (unparsed, reparsed string) {
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	query := "?access" + "_token=" + redacttest.Token
	return "http://" + userinfo + "127.0.0.1:1" + path + "%zz" + query, "http://" + userinfo + "[::%25\xfd]" + path + query
}

// joinRequireTwins runs a refusal row's twins, each of which shows that http.NewRequest's error
// quotes every one of redacttest's credentials, so a row whose printed text holds none of them has
// kept them out: for unparsed with route appended (twin) and for reparsed, which url.Parse takes,
// with route joined onto it (reparsed-twin).
func joinRequireTwins(t *testing.T, unparsed, reparsed, route string) {
	t.Helper()
	requireQuoted := func(t *testing.T, raw string) {
		t.Helper()
		if _, err := http.NewRequest(http.MethodGet, raw, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	}
	t.Run("twin", func(t *testing.T) { requireQuoted(t, unparsed+route) })
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the URL (%v); the row needs one it takes", err)
		}
		requireQuoted(t, parsed.JoinPath(route).String())
	})
}
