package delivery

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A read-through writes no row of a closed pull request that was not merged and that Dispatch
// holds no row of: such a pull request is no head a result stands at, and its row would cost a
// read of its commit messages. A closed pull request that Dispatch holds a row of is written, so
// the row takes GitHub's state (R2): a pass at the head of a pull request that GitHub says is
// closed does not stand. GitHub names three pull requests at one commit: #51, closed, which
// Dispatch holds as open; #52, closed, which Dispatch does not hold; and #53, open.
func TestAReadThroughWritesNoRowOfAClosedPullRequestItDoesNotHold(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 51)
	const sha = "5151515151515151515151515151515151515151"
	before := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 51, HeadSHA: sha, HeadRef: "legion/" + issue, BaseRef: "main",
		State: "open", Title: issue + " the work", URL: "https://github.com/acme/widgets/pull/51", EventAt: &before,
	}); err != nil {
		t.Fatalf("store the open row of #51: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		pull := func(number int, state string) map[string]any {
			return map[string]any{
				"number": number, "title": issue + " the work", "body": "", "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
				"state": state, "merged": false, "updated_at": "2026-11-18T01:00:00Z",
				"head": map[string]any{"ref": "legion/" + issue, "sha": sha, "repo": map[string]any{"full_name": "acme/widgets"}},
				"base": map[string]any{"ref": "main"},
			}
		}
		mustEncode(t, w, []any{pull(51, "closed"), pull(52, "closed"), pull(53, "open")})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("number") == "52" {
			t.Error("the read-through paged the commit messages of #52, a closed pull request that Dispatch does not hold")
		}
		mustEncode(t, w, []any{})
	})

	written, _, err := ReadThroughCommitPulls(ctx, pool, fake.newTestClient(), "acme/widgets", sha, time.Now())
	if err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	t.Logf("written=%d rows_of_51=%d rows_of_52=%d rows_of_53=%d", written,
		headRowsOf(t, ctx, pool, "acme/widgets", 51), headRowsOf(t, ctx, pool, "acme/widgets", 52), headRowsOf(t, ctx, pool, "acme/widgets", 53))
	if rows := headRowsOf(t, ctx, pool, "acme/widgets", 52); rows != 0 {
		t.Errorf("%d rows of #52, want none: a closed pull request that was not merged gets no row", rows)
	}
	if held := readHead(t, ctx, pool, "acme/widgets", 51); held.State != "closed" {
		t.Errorf("#51 is %s, want closed: the read-through writes GitHub's state over a row Dispatch holds", held.State)
	}
	if open := readHead(t, ctx, pool, "acme/widgets", 53); open.State != "open" || open.HeadSHA != sha {
		t.Errorf("#53 is %s at %.7s, want open at %.7s", open.State, open.HeadSHA, sha)
	}
	if written != 2 {
		t.Errorf("the read-through wrote %d rows, want 2: #51 and #53", written)
	}
}
