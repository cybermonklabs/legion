package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestTheBaseRulesLineNamesGitHubsContextsAsTheRuleReadsThem: GitHub, or whatever answers in its
// place, names among a ruleset's required contexts one that echoes the installation token the
// read carried and quotes the probe (echoedWords), and a classic protection's context that does
// too. The line the read logs names each context as the rule reads it: none of the token or the
// probe's credentials, and the rest of the context. The rules keep each context as it came, which
// R0 decides on (classify.ConflictNeedsMerge). The line printed the contexts as they came.
// Captures daemon.answer-words/base-rules.
func TestTheBaseRulesLineNamesGitHubsContextsAsTheRuleReadsThem(t *testing.T) {
	classic := "build " + echoedWords
	server, _ := rulesStandIn(t, map[string]rulesAnswer{
		"/repos/acme/widgets/rules/branches/main": {body: `[{"type":"pull_request","parameters":{"required_approving_review_count":1}},` +
			`{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":` + fmt.Sprintf("%q", echoedWords) + `},{"context":"lint"}],"strict_required_status_checks_policy":true}}]`},
		"/repos/acme/widgets/branches/main": {body: `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":` +
			`{"enforcement_level":"non_admins","contexts":[` + fmt.Sprintf("%q", classic) + `],"checks":[{"context":"lint","app_id":null}]}}}`},
	})
	var logs bytes.Buffer
	w := &workflowRuntime{log: slog.New(slog.NewJSONHandler(&logs, nil))}
	github := githubrest.Client{Token: echoedToken, API: githubrest.RepositoryAPI(server.URL, ghrepo.MustParse("acme/widgets"))}

	rules := w.readBaseRules(context.Background(), github, record.PullRequest{Repo: "acme/widgets", Number: 86}, newGitHubText("main", oauth2calltest.Said("main")))

	// Control: the rules keep each context as it came, the echoed ones among them, so the line's
	// contexts are the ones R0 decides on.
	if want := []string{classic, "lint", echoedWords}; !slices.Equal(rules.Required, want) || !rules.Strict || rules.Unread != "" {
		t.Fatalf("the read came to %+v, want required %q as they came, strict, and read", rules, want)
	}
	lines := loggedLines(t, logs.String(), rulesReadLine)
	if len(lines) != 1 {
		t.Fatalf("the read logged %v, want one line of the rules it read", lines)
	}
	line := fmt.Sprint(lines[0]["required"])
	requireHolds(t, line, "build", "refused;", "lint")
	oauth2calltest.RequireNoPiece(t, "daemon.answer-words/base-rules", line, echoedToken)
}

// TestTheBaseRulesReadNamesGitHubsBaseAsTheRuleReadsIt: GitHub, or whatever answers in its place,
// names the pull request's base branch as words that echo the installation token the read carried
// and quote the probe (echoedWords), and the read of that base's rules fails: GitHub answers it 404
// (answered), or the connection closes with no answer (unanswered). The line the rules read logs,
// and the Unread R0 acts on, name the base as the rule reads it: none of the token or the probe's
// credentials, and the rest of the base and the read's route, in GitHub's answer's path or in
// net/http's error, which name the read as baseRules names it. The read asks for the base as it
// came. The line and Unread printed the base as it came, in the line's base, in GitHub's answer's
// path and in net/http's error. Captures daemon.answer-words/base-<case>.
func TestTheBaseRulesReadNamesGitHubsBaseAsTheRuleReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(w http.ResponseWriter)
		// names is what Unread must say: the route, the base as the rule reads it, and why.
		names []string
	}{
		{"answered", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, []string{"GitHub answered GET /rules/branches/token", "with 404"}},
		{"unanswered", func(w http.ResponseWriter) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}, []string{`Get "/rules/branches/token`, "EOF"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Value
			server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/acme/widgets/pulls/86" {
					fmt.Fprintf(w, `{"number":86,"state":"open","base":{"ref":%q},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":false}`, echoedWords)
					return
				}
				asked.CompareAndSwap(nil, r.URL.Path)
				tc.refuse(w)
			}})
			var logs bytes.Buffer
			w := &workflowRuntime{log: slog.New(slog.NewJSONHandler(&logs, nil))}
			github := githubrest.Client{Token: echoedToken, API: githubrest.RepositoryAPI(server.URL, ghrepo.MustParse("acme/widgets"))}
			var pull githubPullRequest
			if err := github.Get(context.Background(), "/pulls/86", "", &pull); err != nil {
				t.Fatalf("read the pull request: %v", err)
			}

			rules := w.readBaseRules(context.Background(), github, record.PullRequest{Repo: "acme/widgets", Number: 86}, pull.Base.Ref)

			// Control: the read asked for the base as it came, and failed, so R0 acts at once. The
			// route is joined onto the API's path (urljoin.Join), which cleans an empty path
			// element, so the "//" of the probe's URL in echoedWords reaches GitHub as "/". No ref
			// name holds an empty path element (git check-ref-format refuses "//"), so a real base
			// is asked exactly as it came.
			if want := "/repos/acme/widgets/rules/branches/" + strings.ReplaceAll(echoedWords, "//", "/"); asked.Load() != want || rules.Unread == "" {
				t.Fatalf("the read asked for %v and came to %+v, want %q asked and the rules unread", asked.Load(), rules, want)
			}
			lines := loggedLines(t, logs.String(), rulesReadLine)
			if len(lines) != 1 || lines[0]["unread"] != rules.Unread {
				t.Fatalf("the read logged %v, want one line of the rules it read, with its Unread", lines)
			}
			line := fmt.Sprint(lines[0]["base"], " | ", lines[0]["unread"])
			requireHolds(t, line, append([]string{"refused;"}, tc.names...)...)
			oauth2calltest.RequireNoPiece(t, "daemon.answer-words/base-"+tc.name, line, echoedToken)
		})
	}
}
