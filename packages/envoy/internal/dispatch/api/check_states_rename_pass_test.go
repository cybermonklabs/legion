// check_states_rename_pass_test.go holds the boot's rename pass (store.RewriteRenamedRepositories)
// to the states the check states settler published under a repository's old name. The pass writes
// no column of check_states and no table of the former heads: it writes the pull request's row,
// whose triggers record the row's move from the old name to the new one and owe its issue a
// settle, and that settle publishes each state under the new name. The rig is check_states_test.go's.
package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheRenamePassLeavesEachStateToBePublishedUnderTheNewName: before Dispatch knows the rename,
// #70 is stored under its repository's old name, credited to the rig's issue, and leaves H1 for
// H2 there, and is stored under the canonical name at H3, credited to the same issue. The issue's
// settle applies each name's records apart, so the two names are two pull requests, and it
// publishes H1 and H2 under the old name and H3 under the new one. Then the rename pair is
// recorded, and the boot's rename pass runs over it: it deletes the row under the old name, whose
// key the new name holds. The settle it owes joins the two names into one pull request, which
// keeps both names and the history of each, and publishes H1 and H2, now its former heads, under
// the new name, each with an event that names the new name, so a reader that keys a state by its
// repository (the Legion daemon's check states) learns it there, and it leaves no state under the
// old name.
func TestTheRenamePassLeavesEachStateToBePublishedUnderTheNewName(t *testing.T) {
	const oldRepo = "oldorg/example-repo"
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, oldRepo, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, oldRepo, 70, eachHeadH2, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", rig.issue)
	settleOwed(t, rig)
	for _, published := range []struct{ repo, head string }{
		{oldRepo, eachHeadH1}, {oldRepo, eachHeadH2}, {ciJobRepository, eachHeadH3},
	} {
		if _, held := publishedRow(t, rig.database, rig.issue, published.repo, published.head, ""); !held {
			t.Fatalf("before the rename is known %s published nothing at %s of %s: this test holds no state to move", rig.issue, published.head, published.repo)
		}
	}

	learnRename(t, rig, oldRepo, ciJobRepository)
	pairs, err := store.ReadRepoRenames(t.Context(), rig.database.Pool)
	if err != nil {
		t.Fatalf("read the rename pairs: %v", err)
	}
	if pass := store.RewriteRenamedRepositories(t.Context(), rig.database.Pool, pairs); pass.Err() != nil {
		t.Fatalf("the rename pass = %+v (%v), want no failure and nothing left", pass, pass.Err())
	}
	var oldRows int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from pull_request_heads where repo = $1`, oldRepo).Scan(&oldRows); err != nil {
		t.Fatalf("count the rows under %s: %v", oldRepo, err)
	}
	if oldRows != 0 {
		t.Fatalf("after the pass %d rows are stored under %s, want the row deleted for its twin under the new name: this test holds no rename", oldRows, oldRepo)
	}
	events := settleOwed(t, rig)

	for _, head := range []string{eachHeadH1, eachHeadH2, eachHeadH3} {
		if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, head, ""); !held {
			t.Errorf("after the pass and its settle %s publishes nothing at %s under %s, the name its pull request has now", rig.issue, head, ciJobRepository)
		}
		if _, held := publishedRow(t, rig.database, rig.issue, oldRepo, head, ""); held {
			t.Errorf("after the pass and its settle %s still publishes a state at %s under %s, the name its repository was renamed away from", rig.issue, head, oldRepo)
		}
	}
	for _, head := range []string{eachHeadH1, eachHeadH2} {
		named := false
		for _, event := range events {
			named = named || (event.Payload.Head == head && event.Payload.Repo == ciJobRepository)
		}
		if !named {
			t.Errorf("the settle after the pass appended no event at %s naming %s, so a reader that keys a state by its repository never learns it there; events: %v",
				head, ciJobRepository, eventLines(events))
		}
	}
	var identities, names int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(distinct pull), count(*) from pull_request_former_heads_aliases where number = 70`).Scan(&identities, &names); err != nil {
		t.Fatalf("read #70's identities: %v", err)
	}
	if identities != 1 || names != 2 {
		t.Errorf("after the pass and its settle #70 has %d identities under %d names, want one under both names", identities, names)
	}
}
