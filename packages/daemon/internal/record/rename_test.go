package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// renameFixture records issue key of project on repo, with a pull request on prRepo and branch,
// a production check on repo and a repository note resolved to repo: one row in each table whose
// columns hold a repository name.
func renameFixture(t *testing.T, ctx context.Context, tx pgx.Tx, key, project, repo, prRepo, branch string) {
	t.Helper()
	records := NewStore()
	issue := issueFixture(key)
	issue.Project, issue.Tree, issue.Parent, issue.Repo, issue.GitHubRepo = project, key, nil, repo, repo
	must(t, records.PutIssue(ctx, tx, issue))
	must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: key, Repo: prRepo, Number: 7, Branch: branch, HeadSHA: "head",
		State: PullRequestMerged}))
	opened := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	must(t, records.PutProductionCheck(ctx, tx, ProductionCheck{Issue: key, Project: project, Tree: key, Repo: repo, Number: 7,
		OpenedAt: opened, BoundAt: opened.Add(time.Hour)}))
	must(t, records.PutRepoNote(ctx, tx, RepoNote{Issue: key, ResolvedRepo: repo, Note: "kept", PostedAt: opened}))
}

// storedNames is each column of key's rows that holds a repository name, by table and column.
func storedNames(t *testing.T, ctx context.Context, tx pgx.Tx, key string) map[string]string {
	t.Helper()
	names := map[string]string{}
	for _, read := range []struct{ column, sql string }{
		{"issues.repo", "select repo from issues where key = $1"},
		{"issues.github_repo", "select github_repo from issues where key = $1"},
		{"pull_requests.repo", "select repo from pull_requests where issue = $1"},
		{"production_checks.repo", "select repo from production_checks where issue = $1"},
		{"repo_notes.resolved_repo", "select resolved_repo from repo_notes where issue = $1"},
	} {
		var name string
		switch err := tx.QueryRow(ctx, read.sql, key).Scan(&name); err {
		case nil:
			names[read.column] = name
		case pgx.ErrNoRows:
		default:
			t.Fatalf("read %s of %s: %v", read.column, key, err)
		}
	}
	return names
}

// renameLeft is how many rows of each table of project's issues still name from, as the rename
// pass counts them once it has run: the rows of the whole project's candidates
// (RenameCandidates with no keys), summed by table, every table named.
func renameLeft(t *testing.T, ctx context.Context, tx pgx.Tx, project, from string) map[RenameTable]int {
	t.Helper()
	candidates, err := RenameCandidates(ctx, tx, project, from, nil)
	must(t, err)
	left := make(map[RenameTable]int, len(RenameTables))
	for _, table := range RenameTables {
		left[table] = 0
	}
	for _, candidate := range candidates {
		for table, rows := range candidate.Rows {
			left[table] += rows
		}
	}
	return left
}

// The writer writes a pair's new name, as the pair configures it, on every stored column of an
// issue that holds the old name in any spelling — the record's repository and GitHub's spelling,
// the pull request, the production check and the repository note — and on nothing else: an issue
// on another repository and an issue of another project on the old name are left as they are.
// The candidates are the issues of the project whose rows name the old name, the note of an issue
// the project never recorded among them, and a write of rows already on the new name writes
// nothing, so the pass that runs it twice writes once. Control: compared exactly in place of
// without case, the spellings SJAWHAR/Legion and Sjawhar/legion are neither read nor written.
func TestRenameIssueRepositoryWritesTheNewNameOnEveryColumnThatHoldsTheOld(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const from, to = "sjawhar/legion", "cybermonklabs/legion"
	inTx(t, st, func(tx pgx.Tx) {
		renameFixture(t, ctx, tx, "LEGION-1", "LEGION", "SJAWHAR/Legion", "Sjawhar/legion", "legion/LEGION-1")
		renameFixture(t, ctx, tx, "LEGION-2", "LEGION", "acme/widgets", "acme/widgets", "legion/LEGION-2")
		renameFixture(t, ctx, tx, "OTHER-1", "OTHER", from, from, "legion/OTHER-1")
		must(t, NewStore().PutRepoNote(ctx, tx, RepoNote{Issue: "LEGION-9", ResolvedRepo: from, Note: "not admitted", PostedAt: time.Now()}))
		must(t, NewStore().PutRepoNote(ctx, tx, RepoNote{Issue: "OTHER-9", ResolvedRepo: from, Note: "not admitted", PostedAt: time.Now()}))
	})

	inTx(t, st, func(tx pgx.Tx) {
		candidates, err := RenameCandidates(ctx, tx, "LEGION", from, nil)
		must(t, err)
		want := []RenameCandidate{
			{Issue: "LEGION-1", Rows: map[RenameTable]int{RenameTableIssues: 1, RenameTablePullRequests: 1, RenameTableProductionChecks: 1, RenameTableRepoNotes: 1},
				Spellings: []string{"SJAWHAR/Legion", "Sjawhar/legion"}},
			{Issue: "LEGION-9", Rows: map[RenameTable]int{RenameTableRepoNotes: 1}, Spellings: []string{from}},
		}
		if !reflect.DeepEqual(candidates, want) {
			t.Fatalf("candidates = %+v, want %+v: the project's issues whose rows name %s, in any spelling", candidates, want, from)
		}
		if only, err := RenameCandidates(ctx, tx, "LEGION", from, []string{"LEGION-9", "LEGION-2"}); err != nil || len(only) != 1 || only[0].Issue != "LEGION-9" {
			t.Fatalf("candidates of LEGION-9 and LEGION-2 = %+v, %v; want LEGION-9 alone", only, err)
		}
		left := renameLeft(t, ctx, tx, "LEGION", from)
		if want := map[RenameTable]int{RenameTableIssues: 1, RenameTablePullRequests: 1, RenameTableProductionChecks: 1, RenameTableRepoNotes: 2}; !reflect.DeepEqual(left, want) {
			t.Fatalf("rows on %s before the write = %v, want %v", from, left, want)
		}
	})

	for _, key := range []string{"LEGION-1", "LEGION-9"} {
		inTx(t, st, func(tx pgx.Tx) {
			written, err := RenameIssueRepository(ctx, tx, key, from, to)
			must(t, err)
			if written.Deleted != nil {
				t.Fatalf("the write of %s deleted %+v, want nothing deleted", key, written.Deleted)
			}
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := storedNames(t, ctx, tx, "LEGION-1"), map[string]string{"issues.repo": to, "issues.github_repo": to,
			"pull_requests.repo": to, "production_checks.repo": to, "repo_notes.resolved_repo": to}; !reflect.DeepEqual(got, want) {
			t.Fatalf("LEGION-1's names = %v, want %s on every one", got, to)
		}
		if got := storedNames(t, ctx, tx, "LEGION-9"); got["repo_notes.resolved_repo"] != to {
			t.Fatalf("LEGION-9's note = %v, want it on %s", got, to)
		}
		for key, repo := range map[string]string{"LEGION-2": "acme/widgets", "OTHER-1": from} {
			for column, name := range storedNames(t, ctx, tx, key) {
				if name != repo {
					t.Fatalf("%s of %s = %s, want it left on %s", column, key, name, repo)
				}
			}
		}
		if got := storedNames(t, ctx, tx, "OTHER-9"); got["repo_notes.resolved_repo"] != from {
			t.Fatalf("OTHER-9's note = %v, want another project's note left on %s", got, from)
		}
		left := renameLeft(t, ctx, tx, "LEGION", from)
		if want := map[RenameTable]int{RenameTableIssues: 0, RenameTablePullRequests: 0, RenameTableProductionChecks: 0, RenameTableRepoNotes: 0}; !reflect.DeepEqual(left, want) {
			t.Fatalf("rows on %s after the write = %v, want none", from, left)
		}
		again, err := RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
		if want := map[RenameTable]int{RenameTableIssues: 0, RenameTablePullRequests: 0, RenameTableProductionChecks: 0, RenameTableRepoNotes: 0}; !reflect.DeepEqual(again.Rows, want) || again.Deleted != nil {
			t.Fatalf("a second write of LEGION-1 = %+v, want nothing written", again)
		}
	})
}

// Where the new name would give pull_requests_repo_branch one repository and branch twice — another
// issue's pull request already holds the branch under the new name, in any spelling — the issue's
// row under the old name is deleted and the other's stays, and the deletion names both issues; the
// issue's other rows are written as ever. Control: with the collision read removed, the update of
// the row fails on the unique index and the issue's whole write with it.
func TestRenameIssueRepositoryDeletesAPullRequestTheNewNameWouldHoldTwice(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const from, to = "sjawhar/legion", "cybermonklabs/legion"
	inTx(t, st, func(tx pgx.Tx) {
		renameFixture(t, ctx, tx, "LEGION-1", "LEGION", from, from, "release/shared")
		renameFixture(t, ctx, tx, "LEGION-2", "LEGION", to, to, "release/shared")
	})
	var written RenameWrite
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		written, err = RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
	})
	want := &RenameCollision{Issue: "LEGION-1", Holder: "LEGION-2", Repo: from, To: to, Branch: "release/shared", Number: 7}
	if !reflect.DeepEqual(written.Deleted, want) {
		t.Fatalf("deleted = %+v, want %+v", written.Deleted, want)
	}
	if rows := written.Rows; rows[RenameTableIssues] != 1 || rows[RenameTablePullRequests] != 0 || rows[RenameTableProductionChecks] != 1 || rows[RenameTableRepoNotes] != 1 {
		t.Fatalf("rows written = %v, want the record, the check and the note, and no pull request", rows)
	}
	inTx(t, st, func(tx pgx.Tx) {
		if got, want := storedNames(t, ctx, tx, "LEGION-1"), map[string]string{"issues.repo": to, "issues.github_repo": to,
			"production_checks.repo": to, "repo_notes.resolved_repo": to}; !reflect.DeepEqual(got, want) {
			t.Fatalf("LEGION-1's names = %v, want %v and no pull request", got, want)
		}
		if got := storedNames(t, ctx, tx, "LEGION-2")["pull_requests.repo"]; got != to {
			t.Fatalf("LEGION-2's pull request = %q, want it kept on %s", got, to)
		}
	})
}
