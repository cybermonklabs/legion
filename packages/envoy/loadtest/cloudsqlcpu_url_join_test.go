package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACloudSQLReadGoesToTheRouteJoinedOntoTheAPIURL: -api is Cloud Monitoring's base URL, and it
// can carry a path, a query or a fragment. Each read goes to the URL's path with the timeSeries
// route joined onto it, the URL's query kept ahead of the read's own parameters and its fragment
// dropped: http://host/monitoring?key=... reads
// /monitoring/v3/projects/<project>/timeSeries?key=...&filter=... (query), and so does
// http://host/monitoring/?key=... (query-trailing-slash), where the route appended to the URL's
// text sat inside the query (/monitoring?key=.../v3/..., Cloud Monitoring asked /monitoring with
// the query's value changed); http://host/monitoring#... reads /monitoring/v3/.../timeSeries
// (fragment), where the route sat inside the fragment. A URL with neither reads where it did
// (no-query). A stand-in records the path and the query the read reaches it with.
func TestACloudSQLReadGoesToTheRouteJoinedOntoTheAPIURL(t *testing.T) {
	route := "/monitoring/v3/projects/" + cloudsqlTestProject + "/timeSeries"
	databaseID := cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance)
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/monitoring?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "/monitoring/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"fragment", "/monitoring#pane", ""},
		{"no-query", "/monitoring", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				_, _ = w.Write([]byte(cloudsqlAnswer("")))
			}))
			t.Cleanup(srv.Close)
			_, err := readCloudSQLSeries(context.Background(), srv.Client(), srv.URL+c.suffix, cloudsqlTestToken, cloudsqlTestProject, cloudsqlCPUMetric, databaseID, cgWinStart, cgWinEnd)
			select {
			case got := <-asked:
				query := got.Query()
				if got.Path != route || query.Get("filter") != cloudsqlFilter(cloudsqlCPUMetric, databaseID) {
					t.Errorf("an -api URL ending %q was read at path %q with filter %q; want path %q with the read's filter", c.suffix, got.Path, query.Get("filter"), route)
				}
				if c.query != "" && !strings.HasPrefix(got.RawQuery, c.query+"&") {
					t.Errorf("an -api URL ending %q was read with query %q; want it to begin with the URL's own %q", c.suffix, got.RawQuery, c.query)
				}
				if c.query == "" && query.Has("key") {
					t.Errorf("an -api URL ending %q was read with query %q; want the read's parameters alone", c.suffix, got.RawQuery)
				}
			default:
				t.Fatalf("no read reached Cloud Monitoring; readCloudSQLSeries answered %v", err)
			}
			if err != nil {
				t.Errorf("readCloudSQLSeries = %v; want the answer read", err)
			}
		})
	}
}

// TestAnAPIURLThatDoesNotParsePrintsNoneOfIt: an -api URL url.Parse refuses (an invalid escape in
// its path), with redacttest's credentials in its user information and its query, fails the read
// in fixed words, with none of the URL (unparsed); http.NewRequest's error, which the read
// returned, quotes it whole (twin). An -api URL that parses, whose joined URL, as url.URL.String
// writes it, url.Parse refuses (an IPv6 zone holding a byte that is not ASCII, which String
// escapes as %FD), is refused the same way (reparsed); its twin shows http.NewRequest's error for
// the joined URL holds the credentials. Captures loadtest.cloudsql-api-url/<case>.
func TestAnAPIURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/monitoring%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/monitoring?" + param + "=" + redacttest.Token
	databaseID := cloudsqlDatabaseID(cloudsqlTestProject, cloudsqlTestInstance)
	read := func(t *testing.T, api string) string {
		t.Helper()
		_, err := readCloudSQLSeries(context.Background(), http.DefaultClient, api, cloudsqlTestToken, cloudsqlTestProject, cloudsqlCPUMetric, databaseID, cgWinStart, cgWinEnd)
		if err == nil {
			t.Fatal("readCloudSQLSeries read an -api URL that does not parse")
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+"/v3/projects/"+cloudsqlTestProject+"/timeSeries", nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := read(t, unparsed)
		redacttest.RequireNoPiece(t, "loadtest.cloudsql-api-url/unparsed", said)
		if want := "the -api URL does not parse (it is not printed)"; said != want {
			t.Errorf("readCloudSQLSeries = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the -api URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("v3/projects", cloudsqlTestProject, "timeSeries").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := read(t, reparsed)
		redacttest.RequireNoPiece(t, "loadtest.cloudsql-api-url/reparsed", said)
		if want := "the -api URL joined with the timeSeries route does not parse (it is not printed)"; said != want {
			t.Errorf("readCloudSQLSeries = %q; want %q", said, want)
		}
	})
}
