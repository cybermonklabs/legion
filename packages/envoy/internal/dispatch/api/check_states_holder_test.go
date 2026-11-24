// check_states_holder_test.go holds the settler to whom an application of a pull request's history
// owes its publication: the issue that holds the pull request when the application commits, not
// the issue a writer saw when it recorded the change. The rig is check_states_test.go's.
package api

import (
	"testing"
)

// learnRename records that oldRepo was renamed to newRepo where the rename pairs are stored
// (repo_projects.renamed_to), which is where pull_request_canonical_repo reads them. No route
// writes a pair since a project's default repository replaced the repository mappings, so the
// row is written by SQL, as the delivery tests' seedRenamedGadgets writes one.
func learnRename(t *testing.T, rig *checkStatesRig, oldRepo, newRepo string) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, created_by, is_default, renamed_to)
		values ($1, $2, '{"kind":"system","id":"check-states-test"}'::jsonb, false, $3)
		on conflict (repo) do update set renamed_to = excluded.renamed_to
	`, oldRepo, ciJobProject, newRepo); err != nil {
		t.Fatalf("record %s as renamed to %s: %v", oldRepo, newRepo, err)
	}
}

// TestAppliedHistoryOwesTheIssueThatHoldsThePullRequestWhenItIsApplied: #70 arrived under its
// repository's old name, credited to no issue, and left H1 for H2 there, and arrived under the
// canonical name at H3, credited to the first issue. The re-key's delete of the old row runs and
// is held open. Meanwhile #70 moves to the other issue, which settles everything it is owed: H2,
// the old row's head, is no former head yet. Then the delete commits and is applied: H2 is a former
// head of #70, which the other issue holds now, so that issue is owed a settle and publishes
// states at H2, though its own owed work settled before the delete committed.
func TestAppliedHistoryOwesTheIssueThatHoldsThePullRequestWhenItIsApplied(t *testing.T) {
	const oldRepo = "oldorg/example-repo"
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	learnRename(t, rig, oldRepo, ciJobRepository)
	other := rig.fileIssue(t, "Where the pull request moves")
	uploadDocument(t, rig.handler, "/api/v1/issues/"+other, "verification.md", checkStatesTwoChecks)
	writeHead(t, rig.database, oldRepo, 70, eachHeadH1, "open", "")
	writeHead(t, rig.database, oldRepo, 70, eachHeadH2, "open", "")
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", rig.issue)
	settleOwed(t, rig)

	reKey, err := rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the re-key: %v", err)
	}
	defer reKey.Rollback(t.Context())
	if _, err := reKey.Exec(t.Context(), `delete from pull_request_heads where repo = $1 and number = 70`, oldRepo); err != nil {
		t.Fatalf("the re-key's delete of %s#70: %v", oldRepo, err)
	}
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", other)
	settleOwed(t, rig)
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH2, ""); held {
		t.Fatalf("%s published states at H2 before the delete that makes it a former head committed: this test holds nothing", other)
	}
	if err := reKey.Commit(t.Context()); err != nil {
		t.Fatalf("commit the re-key's delete: %v", err)
	}
	settleOwed(t, rig)
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH2, ""); !held {
		t.Fatalf("once the re-key's delete committed and was applied, %s, which holds #70, published no state at H2, the old row's head: the application owed it nothing", other)
	}
}

// TestAJoinOfTwoNamesOwesEachIssueThatHoldsThePullRequest: before Dispatch knows the rename, #70
// under its repository's old name is credited to the first issue and leaves H1 for H2, and #70
// under the canonical name is credited to the other issue and stands at H3. Each issue's settle
// applies its own name's records apart, so the two names are two pull requests, H1 a former head
// of the first alone, and both issues settle until neither holds a mark. Dispatch then learns the
// rename with both rows still stored, so no write records anything and nothing is pending. The
// first issue is owed, as a change of its checks' words owes it, and its settle alone joins the
// two names into one pull request, whose former head H1 the other issue now holds too. The join
// changed the other issue's former heads, so it owes the other issue, which settles and publishes
// at H1: no record of either name is left to apply, so nothing else would owe it.
func TestAJoinOfTwoNamesOwesEachIssueThatHoldsThePullRequest(t *testing.T) {
	const oldRepo = "oldorg/example-repo"
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	other := rig.fileIssue(t, "Holds the canonical name")
	writeHead(t, rig.database, oldRepo, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, oldRepo, 70, eachHeadH2, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", other)
	settleOwed(t, rig)
	if _, held := publishedRow(t, rig.database, rig.issue, oldRepo, eachHeadH1, ""); !held {
		t.Fatalf("%s published nothing at H1 before the rename is known, want its former head published: this test holds no history to join", rig.issue)
	}
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH3, ""); !held {
		t.Fatalf("%s published nothing at H3, its open head: this test cannot see what it publishes", other)
	}
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH1, ""); held {
		t.Fatalf("%s published states at H1 before the rename is known, want none: this test holds nothing to join", other)
	}

	learnRename(t, rig, oldRepo, ciJobRepository)
	var rows, identities, moves, pending int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select (select count(*) from pull_request_heads where number = 70 and state = 'open'),
			(select count(distinct pull) from pull_request_former_heads_aliases where number = 70),
			(select count(*) from pull_request_former_heads_moves where number = 70),
			(select count(*) from pull_request_former_heads_pending where issue_key in ($1, $2))`,
		rig.issue, other).Scan(&rows, &identities, &moves, &pending); err != nil {
		t.Fatalf("read #70's rows, identities and records: %v", err)
	}
	if marks, otherMarks := marksOf(t, rig.database, rig.issue), marksOf(t, rig.database, other); rows != 2 || identities != 2 || moves != 0 || pending != 0 || marks != 0 || otherMarks != 0 {
		t.Fatalf("once the rename is known #70 has %d open rows and %d identities, %d records unapplied, %d rows pending, %d and %d marks; want both rows and both identities, nothing left to apply and no mark: a delete, a re-key or a mark left over would owe the other issue in the join's place",
			rows, identities, moves, pending, marks, otherMarks)
	}

	if _, err := rig.database.Pool.Exec(t.Context(), `select check_states_owe($1)`, rig.issue); err != nil {
		t.Fatalf("owe %s: %v", rig.issue, err)
	}
	settleBy(t, rig, rig.settler)
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(distinct pull) from pull_request_former_heads_aliases where number = 70`).Scan(&identities); err != nil {
		t.Fatalf("read #70's identities: %v", err)
	}
	if identities != 1 {
		t.Fatalf("after the settle of %s #70 has %d identities, want one: the settle joined nothing", rig.issue, identities)
	}
	if marks := marksOf(t, rig.database, other); marks == 0 {
		t.Fatalf("the join gave %s, which holds #70 under its canonical name, the former head H1, and owed it nothing", other)
	}
	settleOwed(t, rig)
	if _, held := publishedRow(t, rig.database, other, ciJobRepository, eachHeadH1, ""); !held {
		t.Fatalf("once the two names are joined %s, which holds #70, published no state at H1, its former head", other)
	}
}
