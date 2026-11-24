package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestABoundProbeGoesToThePathJoinedOntoItsAddress: AIRLOCK_S_URL can carry a path and a query.
// probe-bound's GET goes to the URL's path with /go joined onto it and the URL's query kept:
// http://host/prefix?key=... asks /prefix/go?key=... (query), where /go appended to the URL's text
// sat inside the query and the bound host was asked /prefix with the query's value changed. A URL
// with a path and no query asks where it did (no-query). A stand-in records the path and the query
// the GET reaches it with.
func TestABoundProbeGoesToThePathJoinedOntoItsAddress(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/prefix?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"no-query", "/prefix", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			bound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(bound.Close)
			t.Setenv("AIRLOCK_S_URL", bound.URL+c.suffix)
			t.Setenv("AIRLOCK_DEMO_TOKEN", "unused")
			var stdout, stderr strings.Builder
			if code := run(context.Background(), []string{"probe-bound"}, &stdout, &stderr); code != 0 {
				t.Fatalf("dev-airlock-stands probe-bound exited %d; stderr: %s", code, stderr.String())
			}
			select {
			case got := <-asked:
				if got.Path != "/prefix/go" || got.RawQuery != c.query {
					t.Errorf("AIRLOCK_S_URL ending %q: the GET reached path %q with query %q; want path /prefix/go with query %q", c.suffix, got.Path, got.RawQuery, c.query)
				}
			default:
				t.Fatalf("AIRLOCK_S_URL ending %q: no GET reached the bound host; probe-bound printed %q", c.suffix, stdout.String())
			}
			if stdout.String() != "204\n" {
				t.Errorf("probe-bound printed %q; want the GET's status, 204", stdout.String())
			}
		})
	}
}

// TestABoundProbeAddressThatDoesNotReparsePrintsItsClass: an AIRLOCK_S_URL url.Parse takes, with
// redacttest's credentials in its user information and its query, whose URL with /go joined onto
// it, as url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is not
// ASCII, which String escapes as %FD). http.NewRequest's error for that URL quotes it whole
// (reparsed-twin); probe-bound prints the failure by its class alone (reparsed). Captures
// dev-proxy-stands.probe-address/reparsed.
func TestABoundProbeAddressThatDoesNotReparsePrintsItsClass(t *testing.T) {
	address := "https://" + redacttest.User + ":" + redacttest.Password + "@[::%25\xfd]/bound?" + "access" + "_token=" + redacttest.Token
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(address)
		if err != nil {
			t.Fatalf("url.Parse refused AIRLOCK_S_URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath("go").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		t.Setenv("AIRLOCK_S_URL", address)
		t.Setenv("AIRLOCK_DEMO_TOKEN", "unused")
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"probe-bound"}, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands probe-bound exited %d; stderr: %s", code, stderr.String())
		}
		redacttest.RequireNoPiece(t, "dev-proxy-stands.probe-address/reparsed", stdout.String())
		if want := "error " + redact.ClassNotAURL + "\n"; stdout.String() != want {
			t.Errorf("probe-bound printed %q, want %q", stdout.String(), want)
		}
	})
}
