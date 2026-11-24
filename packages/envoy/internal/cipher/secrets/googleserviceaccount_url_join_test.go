package secrets_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinStand is IAM Service Account Credentials wherever under its address a mint comes: it mints
// a token for every request and records the URL each one reached it with.
type joinStand struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  []url.URL
}

func newJoinStand(t *testing.T) *joinStand {
	t.Helper()
	s := &joinStand{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, *r.URL)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "imp" + "ersonated-access-token", "expireTime": "2026-11-06T13:00:00Z"})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *joinStand) got() []url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.URL(nil), s.asked...)
}

// mintAt mints the bound account's token with the broker's own credential, over base.
func mintAt(t *testing.T, base string, client *http.Client) (secrets.Minted, error) {
	t.Helper()
	minter := &secrets.GoogleServiceAccount{
		BaseURL: base, HTTP: client, Source: brokerSource(),
		Accounts: map[string]string{"GCP_FLEET": serviceAccountEmail},
	}
	return minter.Mint(t.Context(), secrets.MintRequest{Name: "GCP_FLEET", Source: "GCP_FLEET", Value: serviceAccountValue})
}

// TestAServiceAccountMintGoesToTheResourceJoinedOntoTheBaseURL: the IAM Credentials URL is a
// setting a dev stack or a test names (BaseURL), and it can carry a path, a query or a fragment.
// The mint goes to that URL's path with the resource's path joined onto it, the query kept and
// the fragment dropped: http://host/iam?key=... mints at
// /iam/v1/projects/-/serviceAccounts/<account>:generateAccessToken?key=... (query), and so does
// http://host/iam/?key=... (query-trailing-slash), where the path appended to the URL's text sat
// inside the query (/iam?key=.../v1/..., IAM asked /iam with the query's value changed);
// http://host/iam#... mints at /iam/v1/...:generateAccessToken (fragment), where the path sat
// inside the fragment. A URL with neither mints where it did (no-query). A stand-in records the
// path and the query the mint reaches it with.
func TestAServiceAccountMintGoesToTheResourceJoinedOntoTheBaseURL(t *testing.T) {
	resource := "/iam/v1/projects/-/serviceAccounts/" + serviceAccountEmail + ":generateAccessToken"
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/iam?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "/iam/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"fragment", "/iam#pane", ""},
		{"no-query", "/iam", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stand := newJoinStand(t)
			minted, err := mintAt(t, stand.server.URL+c.suffix, stand.server.Client())
			got := stand.got()
			if len(got) != 1 {
				t.Fatalf("a base URL ending %q was asked %v; want the one mint, Mint = %q, %v", c.suffix, got, minted.Value, err)
			}
			if got[0].Path != resource || got[0].RawQuery != c.query {
				t.Errorf("a base URL ending %q minted at path %q, query %q; want path %q, query %q", c.suffix, got[0].Path, got[0].RawQuery, resource, c.query)
			}
			if err != nil || minted.Value != "impersonated-access-token" {
				t.Errorf("Mint = %q, %v; want the token the stand-in minted", minted.Value, err)
			}
		})
	}
}

// TestABaseURLWithACredentialPrintsNoneOfIt: the IAM Credentials URL with redacttest's
// credentials in its user information and its query. One url.Parse refuses (an invalid escape in
// its path) fails the mint in the minter's own fixed words, with none of the URL (unparsed);
// http.NewRequest's error, which the request's construction printed, quotes it whole (twin). One
// url.Parse takes, whose joined URL, as url.URL.String writes it, url.Parse refuses (an IPv6 zone
// holding a byte that is not ASCII, which String escapes as %FD), is refused the same way
// (reparsed); its twin shows http.NewRequest's error for the joined URL holds the credentials.
// Captures secrets.google-base-url/<case>.
func TestABaseURLWithACredentialPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	resource := "/v1/projects/-/serviceAccounts/" + serviceAccountEmail + ":generateAccessToken"
	unparsed := "http://" + userinfo + "127.0.0.1:1/iam%zz?" + param + "=" + redacttest.Token
	reparsed := "http://" + userinfo + "[::%25\xfd]/iam?" + param + "=" + redacttest.Token
	mint := func(t *testing.T, base string) string {
		t.Helper()
		minted, err := mintAt(t, base, nil)
		if err == nil {
			t.Fatalf("Mint answered %q at a base URL that does not parse", minted.Value)
		}
		return err.Error()
	}
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := http.NewRequest(http.MethodPost, unparsed+resource, nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := mint(t, unparsed)
		redacttest.RequireNoPiece(t, "secrets.google-base-url/unparsed", said)
		if want := "the upstream could not be asked: create Google service account token request: the IAM Credentials URL does not parse"; said != want {
			t.Errorf("Mint = %q; want %q", said, want)
		}
	})
	t.Run("reparsed-twin", func(t *testing.T) {
		parsed, err := url.Parse(reparsed)
		if err != nil {
			t.Fatalf("url.Parse refused the base URL (%v); the row needs one it takes", err)
		}
		if _, err := http.NewRequest(http.MethodPost, parsed.JoinPath(resource).String(), nil); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("http.NewRequest of the joined URL = %v; want an error that quotes the credentials, so the row can see them", err)
		}
	})
	t.Run("reparsed", func(t *testing.T) {
		said := mint(t, reparsed)
		redacttest.RequireNoPiece(t, "secrets.google-base-url/reparsed", said)
		if want := "the upstream could not be asked: create Google service account token request: the URL it joined does not parse"; said != want {
			t.Errorf("Mint = %q; want %q", said, want)
		}
	})
}
