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
// publisher older than the field sends, keeps the revision stored: the check states settler's
// read of the pull request finds a base branch that moved since (delivery.CommitMembership.Stale),
// and decides nothing on it, so a revision kept costs nothing, where a revision lost leaves every
// candidate of the pull request unread until a reconcile stores one again.
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

	intake := newIntake(tuningFor(t), nil, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), nil, unrenamed())
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
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 31); got != baseLater {
		t.Fatalf("an envelope that names no base revision leaves %q on #31, want %s, the revision stored before", got, baseLater)
	}

	if _, _, err := unrenamed().ReadThroughCommitPulls(ctx, pool, client, nil, "acme/gadgets", sha, time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	if got := baseSHAOf(t, ctx, pool, "acme/gadgets", 32); got != baseFirst {
		t.Fatalf("the read-through's write of #32 stores the base revision %q, want %s, GitHub's base.sha", got, baseFirst)
	}
}

// The revisions of the tests of StorePullRequestBaseSHA below.
const (
	storeBaseHead     = "1111111111111111111111111111111111111111"
	storeBaseOther    = "2222222222222222222222222222222222222222"
	storeBaseRead     = "3333333333333333333333333333333333333333"
	storeBaseStanding = "4444444444444444444444444444444444444444"
)

// TestStorePullRequestBaseSHAWritesOnlyTheRowTheReadIsOf: StorePullRequestBaseSHA stores the
// revision read for an open row that stands at the head the read found and stores none, and for
// no other row, so the two revisions a row stores are always of one read of GitHub. A row that
// stores a revision keeps it, since that revision is its own writer's; a row that has moved to
// another head keeps none, since the revision read is of the head GitHub held; and a closed row
// keeps none, since its history ended. Each row that is not written answers false, which the
// check states settler reads as a base revision it has not supplied
// (api/check_states_former_heads.go).
func TestStorePullRequestBaseSHAWritesOnlyTheRowTheReadIsOf(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 41)
	attributor := unrenamed()
	write := func(number int, head, base, state string) {
		t.Helper()
		key := issue
		if err := attributor.UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
			Repo: deliveryRepo, Number: number, HeadSHA: head, HeadRef: "legion/work", BaseRef: "main",
			BaseSHA: base, State: state, IssueKey: &key, AttributionSource: "title", AttributionComplete: true,
			Title: issue + " A change", URL: "https://github.com/" + deliveryRepo + "/pull/1",
		}); err != nil {
			t.Fatalf("write %s#%d at %s: %v", deliveryRepo, number, head, err)
		}
	}
	for _, row := range []struct {
		name   string
		number int
		// head and base are what the row stores, state its lifecycle, and read the head the read
		// of the pull request found, which the write goes by.
		head, base, state, read string
		// want is the base revision the row stores after the write, and stored whether the write
		// stored one.
		want   string
		stored bool
	}{
		{name: "an open row at the head read, storing none", number: 101, head: storeBaseHead, state: "open",
			read: storeBaseHead, want: storeBaseRead, stored: true},
		{name: "an open row storing a revision of its own", number: 102, head: storeBaseHead, base: storeBaseStanding,
			state: "open", read: storeBaseHead, want: storeBaseStanding},
		{name: "an open row that has moved to another head", number: 103, head: storeBaseOther, state: "open",
			read: storeBaseHead, want: ""},
		{name: "a row that is no longer open", number: 104, head: storeBaseHead, state: "closed",
			read: storeBaseHead, want: ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			write(row.number, row.head, row.base, row.state)
			stored, err := StorePullRequestBaseSHA(ctx, pool, deliveryRepo, row.number, row.read, storeBaseRead)
			if err != nil {
				t.Fatalf("store the base revision of %s#%d: %v", deliveryRepo, row.number, err)
			}
			if stored != row.stored {
				t.Fatalf("the write of %s#%d's base revision answered %v, want %v", deliveryRepo, row.number, stored, row.stored)
			}
			if got := baseSHAOf(t, ctx, pool, deliveryRepo, row.number); got != row.want {
				t.Fatalf("%s#%d stores the base revision %q after the write, want %q", deliveryRepo, row.number, got, row.want)
			}
		})
	}
}

// TestStorePullRequestBaseSHAWritesNothingForARevisionItDoesNotHold: a read that answered no head
// or no base revision supplies neither, and the write makes no statement at all then, so a row
// with no revision stored keeps none.
func TestStorePullRequestBaseSHAWritesNothingForARevisionItDoesNotHold(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 42)
	key := issue
	if err := unrenamed().UpsertPullRequestHead(ctx, pool, nil, PullRequestHead{
		Repo: deliveryRepo, Number: 105, HeadSHA: storeBaseHead, HeadRef: "legion/work", BaseRef: "main",
		State: "open", IssueKey: &key, AttributionSource: "title", AttributionComplete: true,
		Title: issue + " A change", URL: "https://github.com/" + deliveryRepo + "/pull/105",
	}); err != nil {
		t.Fatalf("write %s#105: %v", deliveryRepo, err)
	}
	for _, row := range []struct{ name, head, base string }{
		{name: "no head read", base: storeBaseRead},
		{name: "no base revision read", head: storeBaseHead},
	} {
		t.Run(row.name, func(t *testing.T) {
			stored, err := StorePullRequestBaseSHA(ctx, pool, deliveryRepo, 105, row.head, row.base)
			if err != nil || stored {
				t.Fatalf("the write of %s#105's base revision answered %v, %v, want false and no error", deliveryRepo, stored, err)
			}
			if got := baseSHAOf(t, ctx, pool, deliveryRepo, 105); got != "" {
				t.Fatalf("%s#105 stores the base revision %q after a write that holds none, want none", deliveryRepo, got)
			}
		})
	}
}
