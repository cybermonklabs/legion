package claudebridge

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// TestATurnReportThatGotNoAnswerNamesTheDaemonsURLByItsSchemeHostAndPath: LEGION_DAEMON_URL is
// what the pane is told, and it can carry user information, a query or a fragment. A report the
// daemon never answers (its address is one nothing listens at) is logged with the failure named
// through oauth2call.Unanswered, by the URL the report asked: its scheme, host and path alone,
// none of the credential. The route is appended to the URL as it is written, so a query or a
// fragment takes the route into it. Each twin shows net/http's own error of the same post holds
// the credential. Captures claudebridge.unanswered-name/<case>.
func TestATurnReportThatGotNoAnswerNamesTheDaemonsURLByItsSchemeHostAndPath(t *testing.T) {
	const (
		user  = "not-a-secret-user"
		token = "w3kd8-not-a-secret-token-5b0f"
	)
	dead := redirectstest.Dead(t)
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte("boot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, daemon, secret, asked string
	}{
		{"clean", "http://" + dead, "", "http://" + dead + "/legion/v1/claims/turn-outcome"},
		{"user", "http://" + user + "@" + dead, user, "http://" + dead + "/legion/v1/claims/turn-outcome"},
		{"query", "http://" + dead + "/daemon?sig=" + token, token, "http://" + dead + "/daemon"},
		{"fragment", "http://" + dead + "#" + token, token, "http://" + dead},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			reports := startTurnReports(Config{DaemonURL: c.daemon, BootTokenFile: tokenFile, Generation: 4}, log.New(&logged, "", 0))
			if reports == nil {
				t.Fatalf("the bridge started no reporter: %q", logged.String())
			}
			if c.secret != "" {
				response, err := http.Post(reports.url, "application/json", strings.NewReader("{}"))
				if err == nil {
					_ = response.Body.Close()
					t.Fatalf("%s answered %d: the row needs a daemon that never answers", c.daemon, response.StatusCode)
				}
				if !strings.Contains(err.Error(), c.secret) {
					t.Fatalf("net/http's own error holds no %q, so the row cannot see a leak: %q", c.secret, err)
				}
			}
			reports.post(claim.TurnOutcomeRequest{BootToken: reports.bootToken, Generation: reports.generation, Sequence: 1, Outcome: claim.TurnClean})
			var secrets []string
			if c.secret != "" {
				secrets = []string{c.secret}
			}
			oauth2calltest.RequireNoPiece(t, "claudebridge.unanswered-name/"+c.name, logged.String(), secrets...)
			if want := `could not report the outcome of turn 1 (clean): Post "` + c.asked + `": dial tcp ` + dead + ": "; !strings.HasPrefix(logged.String(), want) {
				t.Errorf("the pane's line is %q, want it to begin %q", logged.String(), want)
			}
		})
	}
}
