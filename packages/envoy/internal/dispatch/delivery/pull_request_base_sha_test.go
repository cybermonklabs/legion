package delivery

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// baseSHAOf is the base revision pull_request_heads stores for repo#number, "" where it stores
// null.
func baseSHAOf(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) string {
	t.Helper()
	var sha *string
	if err := pool.QueryRow(ctx, `select base_sha from pull_request_heads where repo = $1 and number = $2`, repo, number).Scan(&sha); err != nil {
		t.Fatalf("read the base revision of %s#%d: %v", repo, number, err)
	}
	if sha == nil {
		return ""
	}
	return *sha
}

// TestEachWriterStoresTheBaseRevisionGitHubSent: the base branch of a pull request can move with
// no change of its name, so each writer of a pull request's row stores the commit GitHub said the
// base branch was at with the name: the `pull_request` envelope's base_sha, and base.sha of a
// read of the pull request, here the read-through a recorded result makes. A later envelope that
// carries another base revision stores it, and one that carries none, as an envelope of a
// publisher older than the field, stores the revision as not known.
func TestEachWriterStoresTheBaseRevisionGitHubSent(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	own := seedGadgetsIssue(t, ctx, pool, 5)
	const sha = "8686868686868686868686868686868686868686"
	const baseFirst, baseLater = "8787878787878787878787878787878787878787", "8888888888888888888888888888888888888888"
	title := own + ": the work"

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/gadgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})
	fake.handle("GET /repos/acme/gadgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{
			"number": 32, "title": title, "body": "", "html_url": "https://github.com/acme/gadgets/pull/32",
			"state": "open", "merged": false, "updated_at": "2026-02-05T00:00:00Z",
			"head": map[string]any{"ref": "legion/work", "sha": sha, "repo": map[string]any{"full_name": "acme/gadgets"}},
			"base": map[string]any{"ref": "main", "sha": baseFirst},
		}})
	})
	client := fake.newTestClient()

	intake := newIntake(nil, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), nil, unrenamed())
	envelope := func(baseSHA, updatedAt string) {
		t.Helper()
		payload := map[string]string{
			"kind": "pr", "action": "synchronize", "merged": "false", "repo": "acme/gadgets", "number": "31",
			"title": title, "body": "", "head_ref": "legion/work", "head_repo": "acme/gadgets",
			"head_sha": sha, "base_ref": "main", "url": "https://github.com/acme/gadgets/pull/31",
			"updated_at": updatedAt,
		}
		if baseSHA != "" {
			payload["base_sha"] = baseSHA
		}
		if _, err := intake.recordPullRequestHead(ctx, payload, 31); err != nil {
			t.Fatalf("record the head the envelope reports: %v", err)
		}
	}
	envelope(baseFirst, "2026-02-05T00:00:00Z")
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 31); got != baseFirst {
		t.Fatalf("the envelope's write of #31 stores the base revision %q, want %s, the envelope's base_sha", got, baseFirst)
	}
	envelope(baseLater, "2026-02-05T00:01:00Z")
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 31); got != baseLater {
		t.Fatalf("a later envelope of #31 leaves the base revision %q, want %s: the base branch moved", got, baseLater)
	}
	envelope("", "2026-02-05T00:02:00Z")
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 31); got != "" {
		t.Fatalf("an envelope that names no base revision leaves %q on #31, want none: the base branch may have moved since %s", got, baseLater)
	}

	if _, _, err := unrenamed().ReadThroughCommitPulls(ctx, pool, client, nil, "acme/gadgets", sha, time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 32); got != baseFirst {
		t.Fatalf("the read-through's write of #32 stores the base revision %q, want %s, GitHub's base.sha", got, baseFirst)
	}
}
