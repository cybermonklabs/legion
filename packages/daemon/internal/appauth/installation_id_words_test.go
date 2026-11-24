package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestAnInstallationIDThatEchoesTheAppJWTPrintsNoneOfIt: whatever answers in GitHub's place lists
// the App's installation on acme under an id that echoes the discovery's Authorization, the App's
// JWT, as a string. The mint's later calls carry the same JWT (the Manager signs it again at the
// same time), and they named the installation by that id: its installation read and its exchange
// answered 404 (gone) and the mint's error named the installation gone, its installation read
// answered a suspension (suspended), and its installation read answered GitHub's rate limit
// (limited), whose answer the error carries by its path. Each printed the JWT whole. Discovery
// refuses an id that is not a number, as GitHub numbers every installation, so no later call is
// made. The twin lists the installation as 77, and the mint's error names the installation as
// discovery listed it, so the rows can see a leak. Captures appauth.answer-words/installation-id-<case>.
func TestAnInstallationIDThatEchoesTheAppJWTPrintsNoneOfIt(t *testing.T) {
	_, privatePEM := testAppKey(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	gone := func(w http.ResponseWriter) { http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) }
	for _, c := range []struct {
		name string
		// echoes lists the installation under the JWT discovery was sent; otherwise under 77.
		echoes bool
		// read answers the installation's read, GET /app/installations/<id>, and exchange its
		// token request.
		read, exchange func(w http.ResponseWriter)
		// names is what the mint's error must say.
		names string
	}{
		{name: "twin", read: gone, exchange: gone, names: "installation 77 on acme answers"},
		{name: "gone", echoes: true, read: gone, exchange: gone, names: "an installation id that is not a number"},
		{name: "suspended", echoes: true, read: func(w http.ResponseWriter) {
			fmt.Fprint(w, installationSuspended("77", grantedEverythingAsked))
		}, names: "an installation id that is not a number"},
		{name: "limited", echoes: true, read: func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusTooManyRequests)
		}, names: "an installation id that is not a number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var jwt string
			listed := func() string {
				mu.Lock()
				defer mu.Unlock()
				if c.echoes {
					return jwt
				}
				return "77"
			}
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch id := listed(); {
				case r.URL.Path == "/app/installations":
					mu.Lock()
					jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
					mu.Unlock()
					listedAs := `77`
					if c.echoes {
						listedAs = fmt.Sprintf("%q", listed())
					}
					fmt.Fprintf(w, `[{"id":%s,"account":{"login":"acme"}}]`, listedAs)
				case r.URL.Path == "/app/installations/"+id:
					c.read(w)
				case r.URL.Path == "/app/installations/"+id+"/access_tokens" && c.exchange != nil:
					c.exchange(w)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", PrivateKey: privatePEM}},
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &bytes.Buffer{}})
			defer tokens.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err := tokens.Token(ctx, Implement, "acme")
			if err == nil {
				t.Fatal("the mint answered no error")
			}
			mu.Lock()
			credential := jwt
			mu.Unlock()
			if !strings.HasPrefix(credential, "eyJ") {
				t.Fatalf("discovery carried no JWT the stand-in could echo: %q", credential)
			}
			printed := err.Error()
			var answered *AnswerError
			if errors.As(err, &answered) {
				printed += "\n" + answered.Answer.Path + "\n" + answered.Answer.Error()
			}
			if c.name == "twin" {
				if !strings.Contains(printed, c.names) {
					t.Fatalf("the mint's error is %q, want it to name the installation as discovery listed it (%q), so the rows can see a leak", printed, c.names)
				}
				return
			}
			oauth2calltest.RequireNoPiece(t, "appauth.answer-words/installation-id-"+c.name, printed, credential)
			if !strings.Contains(printed, c.names) {
				t.Errorf("the mint's error is %q, want it to say %q", printed, c.names)
			}
		})
	}
}
