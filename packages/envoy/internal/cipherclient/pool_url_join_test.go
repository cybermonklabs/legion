package cipherclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// joinAccount is the service account the rows below mint for.
const joinAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// TestGenerateIDTokenGoesToTheResourceJoinedOntoTheIAMURL: IAM Credentials' URL is an origin a dev
// stack or a test names, and it can carry a path, a query or a fragment. generateIdToken is called
// at the URL's path with the resource's path joined onto it, the query kept and the fragment
// dropped: http://host/iam?key=... is called at
// /iam/v1/projects/-/serviceAccounts/<account>:generateIdToken?key=... (query), and so is
// http://host/iam/?key=... (query-trailing-slash), where the path appended to the URL's text sat
// inside the query (/iam?key=.../v1/..., IAM Credentials asked /iam with the query's value
// changed); http://host/iam#... is called at /iam/v1/...:generateIdToken (fragment), where the path
// sat inside the fragment. A URL with neither is called where it was (no-query). A stand-in records
// the path and the query the call reaches it with.
func TestGenerateIDTokenGoesToTheResourceJoinedOntoTheIAMURL(t *testing.T) {
	resource := "/iam/v1/projects/-/serviceAccounts/" + joinAccount + ":generateIdToken"
	for _, c := range []struct{ name, suffix, query string }{
		{"query", "/iam?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"query-trailing-slash", "/iam/?key=k3y-7Qz4k", "key=k3y-7Qz4k"},
		{"fragment", "/iam#pane", ""},
		{"no-query", "/iam", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			asked := make(chan url.URL, 1)
			iam := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
				asked <- *r.URL
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"token":"google.identity.token.minted.here"}`))
			})
			token, err := generateIDToken(context.Background(), nil, iam+c.suffix, joinAccount, joinAccount, "example-client", "federated-access-token")
			select {
			case got := <-asked:
				if got.Path != resource || got.RawQuery != c.query {
					t.Errorf("an IAM URL ending %q was called at path %q, query %q; want path %q, query %q", c.suffix, got.Path, got.RawQuery, resource, c.query)
				}
			default:
				t.Fatalf("no call reached IAM Credentials; generateIDToken answered %v", err)
			}
			if err != nil || token != "google.identity.token.minted.here" {
				t.Errorf("generateIDToken = %q, %v; want the token the stand-in minted", token, err)
			}
		})
	}
}

// TestAnIAMURLWithACredentialPrintsNoneOfIt: IAM Credentials' URL with redacttest's credentials in
// its user information and its query. A mint IAM Credentials refuses (403) names the service as
// redact.URL prints its URL, never the URL the call was made at (refused); the twin shows that call
// carried the token in its query. A URL url.Parse refuses (an invalid escape in its path) is refused
// by its class, never url.Parse's error, which quotes the URL whole (unparsed); the twin shows
// url.Parse's error holds the credentials. Captures cipherclient.iam-url/<case>.
func TestAnIAMURLWithACredentialPrintsNoneOfIt(t *testing.T) {
	param := "access" + "_token"
	userinfo := redacttest.User + ":" + redacttest.Password + "@"
	asked := make(chan string, 1)
	iam := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED"}}`))
	})
	refused := strings.Replace(iam, "http://", "http://"+userinfo, 1) + "/iam?" + param + "=" + redacttest.Token
	unparsed := "http://" + userinfo + "127.0.0.1:1/iam%zz?" + param + "=" + redacttest.Token
	mint := func(t *testing.T, iamURL string) string {
		t.Helper()
		_, err := generateIDToken(context.Background(), nil, iamURL, joinAccount, joinAccount, "example-client", "federated-access-token")
		if err == nil {
			t.Fatal("generateIDToken minted a token; the row needs a refusal")
		}
		return err.Error()
	}
	t.Run("refused-twin", func(t *testing.T) {
		mint(t, refused)
		if query := <-asked; !strings.Contains(query, redacttest.Token) {
			t.Errorf("the call's query is %q; want it to carry the token, so a print of the call's URL would hold it", query)
		}
	})
	t.Run("refused", func(t *testing.T) {
		said := mint(t, refused)
		<-asked
		redacttest.RequireNoPiece(t, "cipherclient.iam-url/refused", said)
		if want := "IAM Credentials' generateIdToken at (not printed"; !strings.Contains(said, want) {
			t.Errorf("generateIDToken = %q; want the service named as redact.URL prints its URL (%q)", said, want)
		}
	})
	t.Run("unparsed-twin", func(t *testing.T) {
		if _, err := url.Parse(unparsed); err == nil || len(redacttest.Held(err.Error())) != len(redacttest.Secrets) {
			t.Errorf("url.Parse(%q) = %v; want an error that quotes the credentials, so the row can see them", unparsed, err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		said := mint(t, unparsed)
		redacttest.RequireNoPiece(t, "cipherclient.iam-url/unparsed", said)
		if !strings.HasSuffix(said, ": not a valid URL") {
			t.Errorf("generateIDToken = %q; want the URL refused by its class", said)
		}
	})
}
