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
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// TestATokensIdentityReadNamesTheViewerOnlyInItsShape: a token_command releases beta's
// installation token, a token in GitHub's shape with the gate's marker -RDX- after it, and
// whatever answers in GitHub's place echoes it back as GraphQL's viewer login: alone, which is no
// App's bot (not-a-bot), or with "[bot]" after it. The App read of that slug is answered 404
// (app-refused) or gets no answer (app-unanswered), or answers the token as the App's id
// (another-app); the bot user's read is answered 404 (user-refused) or gets no answer
// (user-unanswered); or the App acted as its own bot on acme's token before (identity). The mint's
// error, and the path of GitHub's answer it carries, which the start prints on a rate limit, name
// the login only as a login GitHub allows and hold none of the token. Each printed the token: the
// login as it came, the slug in the App read's name, its path and net/http's URL, the id, and the
// bot user read's path and URL. Captures appauth.answer-words/viewer-<case>.
func TestATokensIdentityReadNamesTheViewerOnlyInItsShape(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	hangUp := func(w http.ResponseWriter, _ string) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
	notFound := func(w http.ResponseWriter, _ string) { http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) }
	ofTheApp := func(w http.ResponseWriter, _ string) { fmt.Fprint(w, `{"id":12345}`) }
	for _, c := range []struct {
		name string
		// bot says whether the viewer's login is the echoed token with "[bot]" after it, or the
		// token alone.
		bot bool
		// app and user answer GET /apps/<the token> and GET /users/<the token>[bot], given the token.
		app, user func(w http.ResponseWriter, echoed string)
		// first mints acme's token first, whose viewer is the App's own bot, so the App's identity
		// is held when beta's is read.
		first bool
		// names is what the error must say of why the token was refused.
		names string
	}{
		{name: "not-a-bot", names: "not an App's bot"},
		{name: "app-refused", bot: true, app: notFound, names: "failed (404)"},
		{name: "app-unanswered", bot: true, app: hangUp, names: "EOF"},
		{name: "another-app", bot: true, app: func(w http.ResponseWriter, echoed string) { fmt.Fprintf(w, `{"id":%q}`, echoed) },
			names: "released a token of App (an id that is not a number)"},
		{name: "user-refused", bot: true, app: ofTheApp, user: notFound, names: "failed (404)"},
		{name: "user-unanswered", bot: true, app: ofTheApp, user: hangUp, names: "EOF"},
		{name: "identity", bot: true, app: ofTheApp, first: true, names: "where the App acted as legion-implementer[bot]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			echoed := githubtest.InstallationToken(t.Name()) + "-RDX-"
			own := githubtest.InstallationToken("acme")
			release := filepath.Join(t.TempDir(), "release.json")
			expires := now.Add(time.Hour).Format(time.RFC3339)
			if err := os.WriteFile(release, fmt.Appendf(nil, `{"installations":{"acme":{"installation_id":7,"token":%q,"expires_at":%q},"beta":{"installation_id":8,"token":%q,"expires_at":%q}}}`,
				own, expires, echoed, expires), 0o600); err != nil {
				t.Fatal(err)
			}
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch token := strings.TrimPrefix(r.Header.Get("Authorization"), "token "); {
				case r.Method == http.MethodPost && r.URL.Path == "/graphql" && token == own:
					fmt.Fprint(w, `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`)
				case r.Method == http.MethodPost && r.URL.Path == "/graphql" && c.bot:
					fmt.Fprintf(w, `{"data":{"viewer":{"login":%q}}}`, token+"[bot]")
				case r.Method == http.MethodPost && r.URL.Path == "/graphql":
					fmt.Fprintf(w, `{"data":{"viewer":{"login":%q}}}`, token)
				case r.URL.Path == "/apps/legion-implementer":
					fmt.Fprint(w, `{"id":12345}`)
				case r.URL.Path == "/users/legion-implementer[bot]":
					fmt.Fprint(w, `{"id":271566630}`)
				case r.URL.Path == "/apps/"+echoed && c.app != nil:
					c.app(w, echoed)
				case r.URL.Path == "/users/"+echoed+"[bot]" && c.user != nil:
					c.user(w, echoed)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "12345", TokenCommand: "cat '" + release + "'"}},
				Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, Stderr: &bytes.Buffer{}})
			defer tokens.Close()
			if c.first {
				if _, err := tokens.Token(context.Background(), Implement, "acme"); err != nil {
					t.Fatalf("acme's token, whose viewer is the App's own bot: %v", err)
				}
			}

			_, err := tokens.Token(context.Background(), Implement, "beta")
			if err == nil {
				t.Fatal("beta's token, whose viewer echoes it, was taken; want it refused")
			}
			printed := err.Error()
			var answered *AnswerError
			if errors.As(err, &answered) {
				printed += "\n" + answered.Answer.Path + "\n" + answered.Answer.Body
			}
			for _, want := range []string{githubrest.NotALogin, c.names} {
				if !strings.Contains(printed, want) {
					t.Errorf("the refusal prints %q, want %q in it", printed, want)
				}
			}
			oauth2calltest.RequireNoPiece(t, "appauth.answer-words/viewer-"+c.name, printed, echoed)
		})
	}
}
