package delivery

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACommitWithNoDateNamesItsSHAOnlyWhenItIsOne: GitHub, or whatever answers in its place,
// lists a pull request's first commit with no author and no committer date, under a sha that
// quotes a credential's URL (not-a-sha) and under a commit's own sha (sha). The fetch's error names
// the sha only where it is a commit's sha, forty hex digits, and otherwise says it is none, so it
// prints nothing else the answer wrote there. Captures delivery.answer-words/<case>.
func TestACommitWithNoDateNamesItsSHAOnlyWhenItIsOne(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, c := range []struct{ name, sha, want string }{
		{"not-a-sha", redacttest.Code, "commit (a sha that is not a commit's) carries neither an author nor a committer date"},
		{"sha", commit, "commit " + commit + " carries neither an author nor a committer date"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			fake.handle("GET /repos/acme/widgets/pulls/8", func(w http.ResponseWriter, r *http.Request) {
				mustEncode(t, w, map[string]any{"number": 8, "title": "x", "html_url": "u", "user": map[string]any{"login": "a"}})
			})
			fake.handle("GET /repos/acme/widgets/pulls/8/commits", func(w http.ResponseWriter, r *http.Request) {
				mustEncode(t, w, []map[string]any{{"sha": c.sha, "commit": map[string]any{}}})
			})
			_, err := FetchPullRequest(t.Context(), fake.newTestClient(), "acme", "widgets", 8)
			if err == nil {
				t.Fatal("FetchPullRequest answered no error")
			}
			redacttest.RequireNone(t, "delivery.answer-words/"+c.name, err.Error())
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("FetchPullRequest's error is %q, want %q in it", err, c.want)
			}
		})
	}
}
