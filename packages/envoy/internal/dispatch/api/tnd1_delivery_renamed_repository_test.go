package api

import (
	"hash/fnv"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// recordDeliveryRepositoryIDs records a GitHub id of its own for each of repositories, derived from
// its name, as the delivery intake records the repository of each pull request it fetches.
func recordDeliveryRepositoryIDs(t *testing.T, seed privateSeed, repositories ...string) {
	t.Helper()
	for _, repository := range repositories {
		sum := fnv.New32a()
		sum.Write([]byte(repository))
		if err := refs.RecordRepositoryID(t.Context(), seed.database.Pool, repository, int64(sum.Sum32())+1); err != nil {
			t.Fatal(err)
		}
	}
}

// movedDeliveryPRs are tnd1's pull requests that name no issue: acme/renamednow#20, in PRIV's
// acme/privrenamed under the name GitHub renamed it to, other/privmoved#21, in PRIV's
// acme/privmoved under the owner GitHub moved it to, and acme/unknown#22, in a repository whose id
// Dispatch does not know.
var movedDeliveryPRs = []string{"acme/renamednow#20", "other/privmoved#21", "acme/unknown#22"}

// seedMovedDeliveryPRs is seedDeliveryReads' whole seed, with PRIV-1 naming acme/privrenamed and
// PRIV-2 acme/privmoved, each repository's GitHub id recorded as the intake and the proxy record it
// (one id for acme/privrenamed and acme/renamednow, one for acme/privmoved and other/privmoved, and
// none for acme/unknown), and, with prs, movedDeliveryPRs written as the intake writes them.
func seedMovedDeliveryPRs(t *testing.T, prs bool) privateSeed {
	t.Helper()
	seed := seedDeliveryReads(t, true, true)
	ctx, pool := t.Context(), seed.database.Pool
	if _, err := pool.Exec(ctx, `
		update issues set repo = 'acme/privrenamed' where key = 'PRIV-1';
		update issues set repo = 'acme/privmoved' where key = 'PRIV-2';
	`); err != nil {
		t.Fatalf("name PRIV's repositories on PRIV-1 and PRIV-2: %v", err)
	}
	for repository, id := range map[string]int64{
		"acme/privrenamed": 7007, "acme/renamednow": 7007, "acme/privmoved": 7008, "other/privmoved": 7008,
	} {
		if err := refs.RecordRepositoryID(ctx, pool, repository, id); err != nil {
			t.Fatal(err)
		}
	}
	recordDeliveryRepositoryIDs(t, seed, "acme/other", "acme/widgets")
	if prs {
		merged := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
		opened := merged.Add(-2 * time.Hour)
		for number, repository := range map[int]string{20: "acme/renamednow", 21: "other/privmoved", 22: "acme/unknown"} {
			upsertPR(t, seed.database, delivery.DeliveryPullRequest{
				Repo: repository, Number: number, Title: "moved change", Author: "octocat",
				URL: "https://github.com/" + repository + "/pull/1", CreatedAt: &opened, MergedAt: &merged,
			})
		}
	}
	return seed
}

// TestADeliveryReadHidesAPullRequestOfAPrivateRepositoryUnderItsNewNameOrOwner: a pull request that
// names no issue is shown to an open viewer only where its repository's GitHub id is the id of no
// repository a private project hidden from that viewer owns, and only where that id is known while a
// project is private. Over seedMovedDeliveryPRs' seed alice, who does not see PRIV, is shown none of
// acme/renamednow#20 (PRIV's acme/privrenamed renamed), other/privmoved#21 (PRIV's acme/privmoved
// moved) and acme/unknown#22 (an id Dispatch does not know): her timeline, measures and Pipeline
// page equal, field for field with computed_at and the freshness masked, those of the same seed
// without the three, and she is still shown acme/other#3. carol, who sees PRIV, is shown all three.
//
// Controls: the name rule (refs.RepositoryClause without `learned.github_id = <id>`) shows alice #20
// and #21; a clause that lets an unknown id pass (without `<id> is null`) shows her #22.
func TestADeliveryReadHidesAPullRequestOfAPrivateRepositoryUnderItsNewNameOrOwner(t *testing.T) {
	with, without := seedMovedDeliveryPRs(t, true), seedMovedDeliveryPRs(t, false)
	for _, read := range deliveryReadsTargets {
		got := maskedDeliveryRead(t, with, callerAlice, read.target)
		want := maskedDeliveryRead(t, without, callerAlice, read.target)
		for _, apart := range deliveryFieldsApart(got, want) {
			t.Errorf("alice's %s differs from the seed without %v, in %s", read.name, movedDeliveryPRs, apart)
		}
	}
	shown := readShownDelivery(t, with, callerAlice).prs
	for _, pr := range movedDeliveryPRs {
		if slices.Contains(shown, pr) {
			t.Errorf("alice is shown %s, want it hidden: %v", pr, shown)
		}
	}
	if !slices.Contains(shown, "acme/other#3") {
		t.Errorf("alice is shown %v, want acme/other#3 among them: its repository's id is known and no project owns it", shown)
	}
	carol := readShownDelivery(t, with, callerCarol).prs
	for _, pr := range movedDeliveryPRs {
		if !slices.Contains(carol, pr) {
			t.Errorf("carol, who sees PRIV, is shown %v, want %s among them", carol, pr)
		}
	}
}

// TestADeliveryReadOfNoPrivateProjectHoldsNoPullRequestToAnID (R9): with every project public, alice
// is shown every pull request that names no issue, whatever Dispatch knows of its repository's id,
// as before any project could be private: #20, #21, #22 and acme/other#3.
func TestADeliveryReadOfNoPrivateProjectHoldsNoPullRequestToAnID(t *testing.T) {
	seed := seedMovedDeliveryPRs(t, true)
	if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'public'; delete from repository_ids`); err != nil {
		t.Fatalf("make every project public and forget every id: %v", err)
	}
	shown := readShownDelivery(t, seed, callerAlice).prs
	for _, pr := range append([]string{"acme/other#3"}, movedDeliveryPRs...) {
		if !slices.Contains(shown, pr) {
			t.Errorf("with every project public alice is shown %v, want %s among them", shown, pr)
		}
	}
}
