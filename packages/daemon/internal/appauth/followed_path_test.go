package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestARefusalAfterAFollowedRedirectNamesNoRunOfTheCredential: whatever answers in GitHub's
// place answers an App call with a 302 on GitHub's own origin to the call's path with the
// credential the call carried after it, and that Location with GitHub's rate limit (429). The
// Manager's client follows a redirect on its own origin (redirects.SameOrigin), so the response it
// reads answers the Location, and its own request (response.Request) is the request to it. The
// call's error, and GitHub's answer it carries (AnswerError), whose method and path the daemon's
// start prints once its rate-limit waits end, name the call the Manager sent, and hold no run of
// the credential: the App's JWT on its identity read (app-slug) and on its token exchange
// (exchange), and an installation token a token_command released on token mode's viewer read
// (viewer). Each printed the credential whole in the answer's path. The twin shows the followed
// response's request holds the echo, so the rows can see a leak. Captures
// appauth.followed-path/<case>.
func TestARefusalAfterAFollowedRedirectNamesNoRunOfTheCredential(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		// sent is the call's method and path, which GitHub's answer must name.
		method, sent string
		// tokenMode makes the App a token_command's, whose installation token the viewer read carries.
		tokenMode bool
	}{
		{name: "twin", method: http.MethodGet, sent: "/app"},
		{name: "app-slug", method: http.MethodGet, sent: "/app"},
		{name: "exchange", method: http.MethodPost, sent: "/app/installations/77/access_tokens"},
		{name: "viewer", method: http.MethodPost, sent: "/graphql", tokenMode: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			released := githubtest.InstallationToken(t.Name()) + "-RDX-"
			var mu sync.Mutex
			var carried string
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				echo := carried
				mu.Unlock()
				switch {
				case r.Method == c.method && r.URL.Path == c.sent:
					sent := r.Header.Get("Authorization")
					sent = strings.TrimPrefix(strings.TrimPrefix(sent, "Bearer "), "token ")
					mu.Lock()
					carried = sent
					mu.Unlock()
					w.Header().Set("Location", c.sent+"/"+sent)
					w.WriteHeader(http.StatusFound)
				case echo != "" && r.URL.Path == c.sent+"/"+echo:
					w.Header().Set("Retry-After", "1")
					http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
				case r.URL.Path == "/app/installations":
					fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
				case r.URL.Path == "/app/installations/77":
					fmt.Fprint(w, installationGranting("77", grantedEverythingAsked))
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			app := config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}
			if c.tokenMode {
				release := filepath.Join(t.TempDir(), "release.json")
				if err := os.WriteFile(release, fmt.Appendf(nil, `{"installations":{"acme":{"installation_id":77,"token":%q,"expires_at":%q}}}`,
					released, now.Add(time.Hour).Format(time.RFC3339)), 0o600); err != nil {
					t.Fatal(err)
				}
				app = config.GitHubApp{AppID: "12345", TokenCommand: "cat '" + release + "'"}
			}
			tokens := New(config.GitHubApps{Implement: app},
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &bytes.Buffer{}})
			defer tokens.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if c.name == "twin" {
				// The identity read as the Manager's own client makes it, with the JWT the Manager signs.
				request, err := http.NewRequestWithContext(ctx, c.method, server.URL+c.sent, nil)
				if err != nil {
					t.Fatal(err)
				}
				signed, err := tokens.generateJWT("12345", privatePEM, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				tokens.appHeaders(request, signed)
				response, err := tokens.client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusTooManyRequests || !strings.Contains(response.Request.URL.Path, signed) {
					t.Fatalf("the followed answer = %d to %s; want the 429 to a Location that holds the JWT, so the rows can see a leak",
						response.StatusCode, response.Request.URL.Path)
				}
				return
			}

			var err error
			switch c.name {
			case "app-slug":
				_, err = tokens.AppLogin(ctx, Implement)
			default:
				_, err = tokens.Token(ctx, Implement, "acme")
			}
			if err == nil {
				t.Fatal("the call answered no error")
			}
			mu.Lock()
			credential := carried
			mu.Unlock()
			if c.tokenMode && credential != released || !c.tokenMode && !strings.HasPrefix(credential, "eyJ") {
				t.Fatalf("the call carried no credential the stand-in could echo: %q", credential)
			}
			var answered *AnswerError
			if !errors.As(err, &answered) || answered.Answer == nil {
				t.Fatalf("the call's error %q carries no GitHub answer", err)
			}
			answer := answered.Answer
			printed := err.Error() + "\n" + answer.Method + " " + answer.Path + "\n" + answer.Error()
			oauth2calltest.RequireNoPiece(t, "appauth.followed-path/"+c.name, printed, credential)
			if answer.Method != c.method || answer.Path != c.sent || answer.Status != http.StatusTooManyRequests || !answer.RateLimited {
				t.Errorf("GitHub's answer = %s %s %d (rate limited %v); want %s %s 429, rate limited, the call the Manager sent",
					answer.Method, answer.Path, answer.Status, answer.RateLimited, c.method, c.sent)
			}
		})
	}
}
