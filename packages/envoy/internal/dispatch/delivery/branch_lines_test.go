package delivery

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// unreachablePool is a pool whose every statement fails: its database refuses the connection.
func unreachablePool(t *testing.T) *store.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://dispatch@127.0.0.1:1/dispatch?sslmode=disable&connect_timeout=5")
	if err != nil {
		t.Fatalf("a pool of a database that refuses the connection: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.NewPool(pool)
}

// defaultBranchStandIn is a GitHub App stand-in for acme/widgets that names its default branch
// branch and answers every compare read with compare's status and body.
func defaultBranchStandIn(t *testing.T, branch string, status int, compare string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"id": 1, "app_slug": "delivery-test",
			"permissions": map[string]string{"contents": "read", "pull_requests": "read"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		mustEncode(t, w, map[string]any{"token": "ghs_installation_token_for_the_branch", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"full_name": "acme/widgets", "default_branch": branch})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/compare/") {
			githubtest.Unexpected(t, w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(compare))
	})
	return mux
}

// loggedLine is the one line the default logger wrote under msg in logs, a slog JSON log, and
// fails t where there is none.
func loggedLine(t *testing.T, logs, msg string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var fields struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &fields) == nil && fields.Msg == msg {
			return line
		}
	}
	t.Fatalf("no line %q was logged; the log is %q", msg, logs)
	return ""
}

// TestADefaultBranchGitHubNamesPrintsThroughTheRuleInEveryBranchLine: GitHub, or whatever
// answers in its place, names acme/widgets' default branch with words that quote a credential's
// URL, and the reads around a commit of that branch fail: the stored answer cannot be read, since
// the database refuses the connection (read-failed), the compare read is answered 500
// (compare-failed), and once GitHub answers the compare, the answer cannot be written
// (write-failed). Each line CommitOnBranch logs names the branch only as the rule reads it, in
// its branch field and in its error, so the whole line holds no credential. Captures
// delivery.branch-lines/<case>.
func TestADefaultBranchGitHubNamesPrintsThroughTheRuleInEveryBranchLine(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct {
		name, msg string
		status    int
		compare   string
		want      BranchAnswer
	}{
		{"read-failed", "dispatch: read a commit's branch answer", http.StatusOK, `{"status":"behind"}`, BranchCarries},
		{"compare-failed", "dispatch: ask whether a commit is on a branch", http.StatusInternalServerError, `{"message":"Server Error"}`, BranchUnknown},
		{"write-failed", "dispatch: record a commit's branch answer", http.StatusOK, `{"status":"behind"}`, BranchCarries},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &logtest.Buffer{}
			logtest.SetDefault(t, slog.NewJSONHandler(logs, nil))
			client := newAppClient(t, githubtest.Spec{Handle: defaultBranchStandIn(t, redacttest.Words, c.status, c.compare).ServeHTTP})
			branches := &Branches{}
			if got := branches.CommitOnBranch(t.Context(), unreachablePool(t), client, "acme/widgets", DefaultBranch, commit, time.Now()); got != c.want {
				t.Errorf("CommitOnBranch answered %v; want %v", got, c.want)
			}
			line := loggedLine(t, logs.String(), c.msg)
			redacttest.RequireNone(t, "delivery.branch-lines/"+c.name, line)
			if want := `"branch":"` + redacttest.Shown + `"`; !strings.Contains(line, want) {
				t.Errorf("the line is %s; want the branch as the rule reads it, %s", line, want)
			}
		})
	}
}
