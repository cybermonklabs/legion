package appauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// A proxy in GitHub's place can answer with a page of its own that echoes the request back, the
// App's JWT included. That body is not GitHub's error JSON, so the permissions read's refusal and
// the token exchange's name it in one line with the JWT and any Authorization or Bearer value
// redacted, where GitHub's own error JSON still reads whole.
func TestARefusalNamesAProxysPageWithoutTheAppsJWT(t *testing.T) {
	_, privatePEM := testAppKey(t)
	installed := func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`) }
	granted := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
	}
	for _, tc := range []struct {
		name            string
		read, exchange  bool // which of the two the proxy answers
		permissionsWant string
	}{
		{name: "the permissions read", read: true, permissionsWant: "GitHub App installation permissions read failed (502): "},
		{name: "the token exchange", exchange: true, permissionsWant: "GitHub App token exchange failed (502): "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var jwts []string
			proxy := func(w http.ResponseWriter, r *http.Request) {
				authorization := r.Header.Get("Authorization")
				_, jwt, _ := strings.Cut(authorization, " ")
				mu.Lock()
				jwts = append(jwts, jwt)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprintf(w, "<html><body><h1>502 Bad Gateway</h1><pre>Authorization: %s\nseen=%s</pre></body></html>", authorization, jwt)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/app/installations":
					installed(w, r)
				case r.URL.Path == "/app/installations/77" && tc.read, r.URL.Path == "/app/installations/77/access_tokens" && tc.exchange:
					proxy(w, r)
				case r.URL.Path == "/app/installations/77":
					granted(w, r)
				default:
					t.Errorf("unexpected GitHub path %s", r.URL.Path)
				}
			}))
			defer server.Close()
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client()})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_, err := tokens.Token(ctx, Implement, "acme")
			want := tc.permissionsWant + `a body that is not GitHub's error JSON ("502 Bad Gateway Authorization: Bearer [redacted] seen=[redacted]")`
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Token = %v, want a refusal naming %q", err, want)
			}
			if len(jwts) == 0 {
				t.Fatal("the proxy saw no JWT, so the row can see nothing")
			}
			for _, jwt := range jwts {
				if strings.Contains(err.Error(), jwt) {
					t.Fatalf("the refusal prints the App's JWT: %v", err)
				}
			}
		})
	}
}
