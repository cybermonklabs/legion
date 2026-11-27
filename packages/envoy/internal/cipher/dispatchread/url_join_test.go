package dispatchread_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// accountFile writes token to a file of its own and answers the account that reads it.
func accountFile(t *testing.T, token string) ciphertoken.Account {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return ciphertoken.NewAccountToken("BROKER_PROJECT_ACCESS_TOKEN_FILE", file)
}

// asked is what one read reached the stand-in with.
type asked struct{ path, query string }

// standIn answers 200 with an empty JSON object and records the path and the query of each read.
func standIn(t *testing.T) (base string, got *[]asked) {
	t.Helper()
	reads := new([]asked)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reads = append(*reads, asked{path: r.URL.Path, query: r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server.URL, reads
}

// TestADispatchReadGoesToThePathJoinedOntoTheDispatchURL: BROKER_PROJECT_ACCESS_URL is the
// deployment's, and it can carry a path, a query or a fragment. A read asks that URL's path with
// the route joined onto it, the configured query kept ahead of the read's own parameters and the
// fragment dropped: http://host/dispatch?sig=... is asked /dispatch/api/v1/access/projects/PRIV
// with sig=... first and the read's project=PRIV after (query), and so is http://host/dispatch/?sig=...
// (query-trailing-slash), where the route appended to the URL's text sat inside the query
// (/dispatch?sig=.../api/v1/..., Dispatch asked /dispatch with the query's value changed);
// http://host/dispatch#pane is asked /dispatch/api/v1/... with no fragment, which no request sends
// (fragment). A URL with neither is asked where it was (no-query). The stand-in records the path
// and the query each read reaches it with.
func TestADispatchReadGoesToThePathJoinedOntoTheDispatchURL(t *testing.T) {
	t.Parallel()
	const route = "/api/v1/access/projects/PRIV"
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/dispatch?sig=s1g-7Qz4k", "sig=s1g-7Qz4k&project=PRIV"},
		{"query-trailing-slash", "/dispatch/?sig=s1g-7Qz4k", "sig=s1g-7Qz4k&project=PRIV"},
		{"fragment", "/dispatch#pane", "project=PRIV"},
		{"no-query", "/dispatch", "project=PRIV"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base, reads := standIn(t)
			reader := dispatchread.New(base+c.suffix, accountFile(t, "pj4rn-not-a-secret-RDX-7c13"), nil, 10*time.Second, 1<<20)
			answer, err := reader.Get(t.Context(), route, url.Values{"project": {"PRIV"}})
			if err != nil || answer.Status != http.StatusOK {
				t.Fatalf("Get = %d, %v; want Dispatch's 200", answer.Status, err)
			}
			if len(*reads) != 1 {
				t.Fatalf("%d reads reached Dispatch, want 1", len(*reads))
			}
			if want := "/dispatch" + route; (*reads)[0].path != want || (*reads)[0].query != c.query {
				t.Errorf("a URL ending %q was asked path %q, query %q; want path %q, query %q",
					c.suffix, (*reads)[0].path, (*reads)[0].query, want, c.query)
			}
		})
	}
}

// TestAProjectAccessURLThatDoesNotParsePrintsNoneOfIt: a BROKER_PROJECT_ACCESS_URL with
// redacttest's credentials in its user information and its query that url.Parse refuses (an invalid
// escape in its path) fails the read in this package's own words, with none of the URL (unparsed);
// one url.Parse takes whose joined URL url.URL.String writes as text url.Parse refuses again (an
// IPv6 zone escape) fails it in its own words too (reparsed). The twins show net/http's own error
// for each quotes the credentials, so the rows can see one printed. Nothing reaches the wire.
// Captures dispatchread.dispatch-url/<case>.
func TestAProjectAccessURLThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	t.Parallel()
	const route = "/api/v1/access/projects/PRIV"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	param := "access" + "_token"
	unparsed := "http://" + userinfo + "127.0.0.1:1/dispatch%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/dispatch?" + param + "=" + redacttest.Token
	read := func(t *testing.T, base string) string {
		t.Helper()
		reader := dispatchread.New(base, accountFile(t, "pj4rn-not-a-secret-RDX-7c13"), nil, 10*time.Second, 1<<20)
		answer, err := reader.Get(t.Context(), route, nil)
		if err == nil {
			t.Fatalf("Get = %d, nil; want a read that was never made", answer.Status)
		}
		return err.Error()
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodGet, unparsed+route, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := read(t, unparsed)
		redacttest.RequireNoPiece(t, "dispatchread.dispatch-url/unparsed", said)
		if want := "dispatchread: BROKER_PROJECT_ACCESS_URL is not an absolute URL with a host"; said != want {
			t.Errorf("Get = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the project-access URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodGet, parsed.JoinPath(route).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := read(t, reparsed)
		redacttest.RequireNoPiece(t, "dispatchread.dispatch-url/reparsed", said)
		if want := "dispatchread: the URL joined from BROKER_PROJECT_ACCESS_URL does not parse"; said != want {
			t.Errorf("Get = %q; want %q", said, want)
		}
	})
}
