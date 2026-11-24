package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

// echoedBotTokens are forgettingTokens whose lease names its App's bot as bot: the bot login an App
// read answered, GitHub's words, here an installation token in GitHub's shape that it echoed.
type echoedBotTokens struct {
	*forgettingTokens
	bot string
}

func (t echoedBotTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.forgettingTokens.Token(ctx, role, owner)
	lease.Identity.Name = t.bot
	return lease, err
}

// TestAPermissionReadGitHubRefusesLogsTheAppsBotOnlyInItsShape: GitHub refuses the review App's
// installation a review author's repository permission (403), and the review App's lease names
// its bot as the installation token an App read echoed. The line that says the review decides
// nothing names the bot as a login GitHub allows (githubrest.ShownLogin), here its fixed phrase,
// and holds no run of the token. Captures daemon.answer-words/permission-bot.
func TestAPermissionReadGitHubRefusesLogsTheAppsBotOnlyInItsShape(t *testing.T) {
	pool := reviewPermissionPool(t)
	bot := githubtest.InstallationToken(t.Name()) + "-RDX-"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration","status":"403"}`)
	}))
	defer server.Close()
	var logs bytes.Buffer
	w := reviewPermissionRuntime(pool, server.URL, slog.New(slog.NewTextHandler(&logs, nil)))
	w.tokens = echoedBotTokens{forgettingTokens: w.tokens.(*forgettingTokens), bot: bot}
	canWrite, err := w.reviewerCanWrite(context.Background(), intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: "a-writer"})
	if err != nil || canWrite {
		t.Fatalf("the review's author can write = %v, %v; want false and no error", canWrite, err)
	}
	oauth2calltest.RequireNoPiece(t, "daemon.answer-words/permission-bot", logs.String(), bot)
	requireHolds(t, logs.String(), `bot="`+notALogin+`"`, "status=403")
}
