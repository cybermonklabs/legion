package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEntraExchangeGoesToTheTokenPathJoinedOntoTheAuthority: the Entra authority is a setting
// (EntraConfig.Authority), and it can carry a path and a query. The exchange posts to the
// authority's path with the tenant's token path joined onto it and the authority's query kept:
// http://host:port/entra?sig=... posts to /entra/<tenant>/oauth2/v2.0/token?sig=... (query),
// where the path appended to the authority's text sat inside the query
// (/entra?sig=.../<tenant>/oauth2/v2.0/token, the authority asked /entra with the query's value
// changed). An authority with neither posts where it did (no-query). A stand-in authority records
// the path and the query the exchange reaches it with.
func TestAnEntraExchangeGoesToTheTokenPathJoinedOntoTheAuthority(t *testing.T) {
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/entra?sig=s1g-7Qz4k", "sig=s1g-7Qz4k"},
		{"no-query", "/entra", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []url.URL
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, *r.URL)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3599,"access_token":"token-1"}`))
			}))
			t.Cleanup(server.Close)
			source, err := NewEntraTokenSource(EntraConfig{TenantID: "test-tenant", ClientID: "test-client", TokenFile: assertionFile(t),
				Scope: DefaultEntraScope, Authority: server.URL + c.suffix, HTTPClient: server.Client()})
			if err != nil {
				t.Fatalf("NewEntraTokenSource: %v", err)
			}
			if _, err := source.Token(context.Background()); err != nil {
				t.Errorf("Token: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := "/entra/test-tenant/oauth2/v2.0/token"; len(asked) != 1 || asked[0].Path != want || asked[0].RawQuery != c.query {
				t.Errorf("an authority ending %q was asked %v; want one exchange at path %q, query %q", c.suffix, asked, want, c.query)
			}
		})
	}
}

// TestAnEntraAuthorityThatDoesNotParsePrintsNoneOfIt: an authority url.Parse refuses (an invalid
// escape in its path), with redacttest's credentials in its user information and its query, is
// refused by NewEntraTokenSource in fixed words, with none of the authority (unparsed); url.Parse's
// error, which it printed, quotes the authority whole (twin). An authority that parses, whose
// token URL, as url.URL.String writes it, url.Parse refuses (an IPv6 zone holding a byte that is
// not ASCII, which String escapes as %FD), fails the exchange as "not a valid URL" (reparsed);
// its twin shows http.NewRequest's error for that token URL holds the credentials. Captures
// embed.entra-authority/<case>.
func TestAnEntraAuthorityThatDoesNotParsePrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	unparsed := "http://" + userinfo + "127.0.0.1:1/entra%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/entra?" + param + "=" + redacttest.Token
	config := func(t *testing.T, authority string) EntraConfig {
		return EntraConfig{TenantID: "test-tenant", ClientID: "test-client", TokenFile: assertionFile(t), Scope: DefaultEntraScope, Authority: authority}
	}
	t.Run("twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		_, err := NewEntraTokenSource(config(t, unparsed))
		if err == nil {
			t.Fatal("NewEntraTokenSource took an authority that does not parse")
		}
		redacttest.RequireNoPiece(t, "embed.entra-authority/unparsed", err.Error())
		if want := "embed: entra authority must be an absolute URL with a host (it is not printed)"; err.Error() != want {
			t.Errorf("NewEntraTokenSource = %q; want %q", err, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the authority (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath("test-tenant/oauth2/v2.0/token").String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the token URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		source, err := NewEntraTokenSource(config(t, reparsed))
		if err != nil {
			t.Fatalf("NewEntraTokenSource: %v; the row needs an authority that parses", err)
		}
		_, err = source.Token(context.Background())
		if err == nil {
			t.Fatal("Token exchanged at a token URL that does not parse")
		}
		redacttest.RequireNoPiece(t, "embed.entra-authority/reparsed", err.Error())
		if want := "embed: entra build request: not a valid URL"; err.Error() != want {
			t.Errorf("Token = %q; want %q", err, want)
		}
	})
}

// assertionFile is a token file holding a federated assertion, for an exchange the test makes.
func assertionFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("federated-assertion"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}
