package api

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// TestADeliveryReadHidesAnIssuelessPullRequestOfAHiddenRepository: a pull request that names no
// issue is shown to an open viewer only when its repository is not hidden from that viewer
// (refs.RepositoryClause), so alice, who does not see PRIV, is not shown acme/privrepo#12, which
// names no issue and is in the repository PRIV's issue names. Over seedDeliveryReads' whole seed,
// with PRIV-1 naming acme/privrepo, alice's timeline, measures and Pipeline page equal, field for field with computed_at and the
// freshness masked, those of the same seed without #12, and so does her count of pull requests that
// can no longer be fetched, which #12 is one of. She is still shown acme/other#3, which names no
// issue and is in a repository no project owns; carol, who sees PRIV, is shown #12.
func TestADeliveryReadHidesAnIssuelessPullRequestOfAHiddenRepository(t *testing.T) {
	const hiddenPR = "acme/privrepo#12"
	seeded := func(withPR bool) privateSeed {
		seed := seedDeliveryReads(t, true, true)
		ctx, pool := t.Context(), seed.database.Pool
		if _, err := pool.Exec(ctx, `update issues set repo = 'acme/privrepo' where key = $1`, seed.issue[seedPrivate]); err != nil {
			t.Fatalf("name acme/privrepo on PRIV's issue: %v", err)
		}
		if withPR {
			merged := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
			opened := merged.Add(-2 * time.Hour)
			upsertPR(t, seed.database, delivery.DeliveryPullRequest{
				Repo: "acme/privrepo", Number: 12, Title: "change 12", Author: "octocat",
				URL: "https://github.com/acme/privrepo/pull/12", CreatedAt: &opened, MergedAt: &merged,
			})
			if err := delivery.MarkPullRequestUnfetchable(ctx, pool, "acme/privrepo", 12, "the pull request is gone"); err != nil {
				t.Fatalf("mark %s unfetchable: %v", hiddenPR, err)
			}
		}
		return seed
	}
	with, without := seeded(true), seeded(false)

	for _, read := range deliveryReadsTargets {
		got := maskedDeliveryRead(t, with, callerAlice, read.target)
		want := maskedDeliveryRead(t, without, callerAlice, read.target)
		for _, apart := range deliveryFieldsApart(got, want) {
			t.Errorf("alice's %s differs from the seed without %s, in %s", read.name, hiddenPR, apart)
		}
	}
	shown, unshown := readShownDelivery(t, with, callerAlice), readShownDelivery(t, without, callerAlice)
	if shown.unfetchable != unshown.unfetchable {
		t.Errorf("alice counts %d pull requests that can no longer be fetched, want %d, the count without %s", shown.unfetchable, unshown.unfetchable, hiddenPR)
	}
	if slices.Contains(shown.prs, hiddenPR) || !slices.Contains(shown.prs, "acme/other#3") {
		t.Errorf("alice is shown the pull requests %v, want acme/other#3 and not %s", shown.prs, hiddenPR)
	}
	if carol := readShownDelivery(t, with, callerCarol); !slices.Contains(carol.prs, hiddenPR) {
		t.Errorf("carol, who sees PRIV, is shown the pull requests %v, want %s among them, or the seed holds nothing to hide from alice",
			carol.prs, hiddenPR)
	}
}
