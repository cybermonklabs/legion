package githubapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoingGitHub is a GitHub whose installation token is redacttest.Token and whose every other
// read is refused with status and a body that echoes the Authorization it carried, as whatever
// answers in GitHub's place can. It answers a client of it, its own URL, and a reader of the
// credential the refused read carried.
func echoingGitHub(t *testing.T, status int) (client *githubapp.Client, base string, carried func() string) {
	t.Helper()
	var mu sync.Mutex
	var held string
	base = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read","checks":"read"}}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, redacttest.Token, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		default:
			mu.Lock()
			held = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Unlock()
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"the credential %s may not read this"}`, r.Header.Get("Authorization"))
		}
	}})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxy", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, base, func() string {
		mu.Lock()
		defer mu.Unlock()
		return held
	}
}

// TestTheProxysRelayOfARefusalHoldsNoTokenItsReadCarried: a GitHub answer the proxy does not
// decode passes through to the browser as GitHub sent it but for the rule: whatever answers in
// GitHub's place can echo the installation token the read carried in any word of its refusal, and
// the body the proxy writes holds none of it (redact.Answer.Words). The twin shows the echo is a
// real one: the same answer read as it came holds the token. Captures
// githubapi.answer-words/relayed-refusal.
func TestTheProxysRelayOfARefusalHoldsNoTokenItsReadCarried(t *testing.T) {
	t.Run("twin", func(t *testing.T) {
		_, base, carried := echoingGitHub(t, http.StatusUnprocessableEntity)
		request, err := http.NewRequest(http.MethodGet, base+"/repos/acme/web/pulls/7", nil)
		if err != nil {
			t.Fatalf("the twin's request: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+redacttest.Token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("the twin's read: %v", err)
		}
		defer response.Body.Close()
		said, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("the twin's body: %v", err)
		}
		if piece := redacttest.Piece(string(said), carried()); piece == "" {
			t.Errorf("GitHub's place answered %q, which shows no run of the credential the read carried; the row below would see no leak", said)
		}
	})

	t.Run("relayed-refusal", func(t *testing.T) {
		client, _, carried := echoingGitHub(t, http.StatusUnprocessableEntity)
		response := httptest.NewRecorder()
		ProxyREST(response, httptest.NewRequest(http.MethodGet, "/api/github/rest/repos/acme/web/pulls/7", nil), client)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("the proxy answered %d, want GitHub's 422 relayed: %q", response.Code, response.Body.String())
		}
		if carried() == "" {
			t.Fatal("GitHub was never read")
		}
		redacttest.RequireNoPiece(t, "githubapi.answer-words/relayed-refusal", response.Body.String(), carried())
	})
}
