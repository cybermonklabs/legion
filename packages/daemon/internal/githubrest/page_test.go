package githubrest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// GetPage sends one GET and never another. A list of two pages answers its first page with the
// second's URL as Next, which a later GetPage reads. A redirect is the page's Redirect, its target
// not asked. A next page, a redirect or a continuation on another origin is refused before any
// request goes there. GetPages, built on it, still reads every page, a deferred redirect's
// included, and ends a redirect loop where Go's own client ends one.
func TestGetPageReturnsOnlyOnePage(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`["leaked"]`))
	}))
	defer other.Close()
	var mu sync.Mutex
	asked := map[string]int{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/commits/abc/pulls":
			if r.URL.Query().Get("per_page") != "100" {
				http.Error(w, `{"message":"want per_page=100"}`, http.StatusBadRequest)
				return
			}
			w.Header().Set("Link", "<"+server.URL+`/repositories/42/commits/abc/pulls?per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`["first"]`))
		case "/repositories/42/commits/abc/pulls":
			_, _ = w.Write([]byte(`["second"]`))
		case "/repos/acme/widgets/commits/moved/pulls":
			http.Redirect(w, r, "/repositories/42/commits/moved/pulls?"+r.URL.RawQuery, http.StatusMovedPermanently)
		case "/repositories/42/commits/moved/pulls":
			_, _ = w.Write([]byte(`["moved"]`))
		case "/repos/acme/widgets/commits/away/pulls":
			w.Header().Set("Link", "<"+other.URL+`/repositories/42/commits/away/pulls?per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`["near"]`))
		case "/repos/acme/widgets/commits/gone/pulls":
			http.Redirect(w, r, other.URL+"/repositories/42/commits/gone/pulls", http.StatusMovedPermanently)
		case "/repos/acme/widgets/commits/loop/pulls":
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	sent := func() int {
		mu.Lock()
		defer mu.Unlock()
		total := 0
		for _, n := range asked {
			total += n
		}
		return total
	}
	sentTo := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return asked[path]
	}
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	ctx := context.Background()

	first, err := GetPage[string](ctx, client, "/commits/abc/pulls")
	if n := sent(); n != 1 {
		t.Fatalf("GetPage of the first page sent %d requests, want 1: a second request occurred", n)
	}
	next := server.URL + "/repositories/42/commits/abc/pulls?per_page=100&page=2"
	if err != nil || !slices.Equal(first.Items, []string{"first"}) || first.Next != next || first.Redirect != "" {
		t.Fatalf("GetPage of the first page = %+v, %v; want its one item and Next %s", first, err, next)
	}
	second, err := GetPage[string](ctx, client, first.Next)
	if n := sent(); n != 2 || err != nil || !slices.Equal(second.Items, []string{"second"}) || second.Next != "" || second.Redirect != "" {
		t.Fatalf("GetPage of the Next page = %+v, %v after %d requests; want the last page's one item, 2 requests in all", second, err, n)
	}

	moved, err := GetPage[string](ctx, client, "/commits/moved/pulls")
	if n := sentTo("/repositories/42/commits/moved/pulls"); n != 0 {
		t.Fatalf("GetPage of a redirect sent %d requests to its target, want none: it followed the redirect", n)
	}
	target := server.URL + "/repositories/42/commits/moved/pulls?per_page=100"
	if err != nil || moved.Items != nil || moved.Next != "" || moved.Redirect != target {
		t.Fatalf("GetPage of a redirect = %+v, %v; want no items and Redirect %s", moved, err, target)
	}
	followed, err := GetPage[string](ctx, client, moved.Redirect)
	if n := sentTo("/repositories/42/commits/moved/pulls"); n != 1 || err != nil || !slices.Equal(followed.Items, []string{"moved"}) {
		t.Fatalf("GetPage of the Redirect = %+v, %v after %d requests to it; want its one item from one request", followed, err, n)
	}

	for _, tc := range []struct{ name, path string }{
		{"a next page on another host", "/commits/away/pulls"},
		{"a redirect to another host", "/commits/gone/pulls"},
		{"a continuation on another host", other.URL + "/repositories/42/commits/abc/pulls?per_page=100&page=2"},
	} {
		page, err := GetPage[string](ctx, client, tc.path)
		if err == nil || !strings.Contains(err.Error(), other.URL) || page.Items != nil || page.Next != "" || page.Redirect != "" {
			t.Fatalf("GetPage of %s = %+v, %v; want an error naming %s and no page", tc.name, page, err, other.URL)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}

	if got, err := GetPages[string](ctx, client, "/commits/abc/pulls"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetPages = %v, %v; want both pages", got, err)
	}
	if got, err := GetPages[string](ctx, client, "/commits/moved/pulls"); err != nil || !slices.Equal(got, []string{"moved"}) {
		t.Fatalf("GetPages of a redirect = %v, %v; want the redirect's target read", got, err)
	}
	looped := sentTo("/repos/acme/widgets/commits/loop/pulls")
	got, err := GetPages[string](ctx, client, "/commits/loop/pulls")
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") || got != nil {
		t.Fatalf("GetPages of a redirect loop = %v, %v; want an error after 10 redirects and no items", got, err)
	}
	if n := sentTo("/repos/acme/widgets/commits/loop/pulls") - looped; n != 10 {
		t.Fatalf("GetPages of a redirect loop sent %d requests, want 10, as Go's own client sends", n)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}
}
