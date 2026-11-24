package secrets_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAMintCallFollowsNoRedirect: whatever answers at the upstream's address answers a mint's call,
// which carries a credential in its Authorization (the broker's own bearer at IAM's
// generateAccessToken, the App's JWT at GitHub's installation list), with a 302 whose Location
// names a stand-in on the same host name at another port and quotes the credential in its query.
// net/http compares host names without their ports when it copies Authorization onto a redirect's
// request, so a client that follows the 302 sends the stand-in the credential whatever the
// Location says. Under the minters' own client (a nil HTTP, as cmd/cipherd builds every minter) and
// a client a caller gives, the stand-in is sent nothing, and the mint fails on the upstream's 302 by
// its status, as ErrMintUpstream at its one attempt: no endpoint a minter asks answers a redirect
// its client must follow, so the 302 is the answer, and not one the Retry asks again. The twin
// shows a client that follows the redirect sends the stand-in the credential, so the rows can see
// one sent on. Captures secrets.redirect-followed/<case>, what the stand-in was sent.
func TestAMintCallFollowsNoRedirect(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	// echo is the upstream's stand-in, answering every call 302 with a Location to the target that
	// quotes the bearer the call carried, and bearer the last one it was sent.
	echo := func(t *testing.T) (base string, bearer func() string) {
		var (
			mu   sync.Mutex
			seen string
		)
		base = redirectstest.Echo(t, "Authorization", func(credential string) string {
			mu.Lock()
			defer mu.Unlock()
			seen = strings.TrimPrefix(credential, "Bearer ")
			return redirectstest.To(target, seen)
		})
		return base, func() string {
			mu.Lock()
			defer mu.Unlock()
			return seen
		}
	}
	t.Run("twin", func(t *testing.T) {
		base, _ := echo(t)
		request, err := http.NewRequest(http.MethodPost, base+"/v1/projects/-/serviceAccounts/"+serviceAccountEmail+":generateAccessToken", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+brokerOwnToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("a client that follows the redirect failed: %v", err)
		}
		_ = response.Body.Close()
		if text := sent(); !strings.Contains(text, brokerOwnToken) {
			t.Fatalf("a client that follows the redirect sent the stand-in no bearer, so the rows cannot see one sent on: %q", text)
		}
	})
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount := func(client *http.Client) func(*testing.T, string) error {
		return func(t *testing.T, base string) error {
			retry, _, _ := testRetry(atTheBottom)
			minter := &secrets.GoogleServiceAccount{
				BaseURL: base, HTTP: client, Source: brokerSource(),
				Accounts: map[string]string{"GCP_FLEET": serviceAccountEmail}, Retry: retry,
			}
			_, err := minter.Mint(t.Context(), secrets.MintRequest{Name: "GCP_FLEET", Source: "GCP_FLEET", Value: serviceAccountValue})
			return err
		}
	}
	githubApp := func(t *testing.T, base string) error {
		retry, _, _ := testRetry(atTheBottom)
		app := secrets.GitHubApp{Base: base, Retry: retry}
		_, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHub: readScope("ada")})
		return err
	}
	for _, c := range []struct {
		name string
		mint func(*testing.T, string) error
	}{
		{"serviceaccount-default", serviceAccount(nil)},
		{"serviceaccount-given", serviceAccount(&http.Client{})},
		{"githubapp-default", githubApp},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, bearer := echo(t)
			err := c.mint(t, base)
			text, credential := sent(), bearer()
			if credential == "" {
				t.Fatal("the upstream's stand-in was sent no bearer, so the row reads nothing")
			}
			redacttest.RequireNoPiece(t, "secrets.redirect-followed/"+c.name, text, credential)
			if text != "" {
				t.Errorf("the stand-in the Location names was asked: %q", text)
			}
			if err == nil || !strings.Contains(err.Error(), "answered HTTP 302") || secrets.MintErrorClass(err) != "upstream" || secrets.MintAttempts(err) != 1 {
				t.Errorf("Mint = %v (class %s, %d attempts), want the upstream's 302 as ErrMintUpstream at one attempt", err, secrets.MintErrorClass(err), secrets.MintAttempts(err))
			}
		})
	}
}
