package record

import (
	"context"
	"reflect"
	"strings"
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
		{"cleared_pull_requests.repo", "select repo from cleared_pull_requests where issue = $1"},
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

// The writer writes a pair's new name, as its caller hands it, on every stored column of an
// issue that holds the old name in any spelling — the record's repository and GitHub's spelling,
// the pull request, the production check, the repository note and the pull request a new
// generation cleared — and on nothing else: an issue on another repository and an issue of another
// project on the old name are left as they are.
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
		for _, cleared := range []struct{ issue, repo string }{{"LEGION-1", "Sjawhar/legion"}, {"LEGION-2", "acme/widgets"}, {"OTHER-1", from}} {
			_, err := tx.Exec(ctx, `insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at) values ($1, 6, $2, now(), now())`,
				cleared.repo, cleared.issue)
			must(t, err)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		candidates, err := RenameCandidates(ctx, tx, "LEGION", from, nil)
		must(t, err)
		want := []RenameCandidate{
			{Issue: "LEGION-1", Rows: map[RenameTable]int{RenameTableIssues: 1, RenameTablePullRequests: 1, RenameTableClearedPullRequests: 1,
				RenameTableProductionChecks: 1, RenameTableRepoNotes: 1},
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
		if want := map[RenameTable]int{RenameTableIssues: 1, RenameTablePullRequests: 1, RenameTableClearedPullRequests: 1, RenameTableProductionChecks: 1,
			RenameTableRepoNotes: 2}; !reflect.DeepEqual(left, want) {
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
			"pull_requests.repo": to, "production_checks.repo": to, "repo_notes.resolved_repo": to, "cleared_pull_requests.repo": to}; !reflect.DeepEqual(got, want) {
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
		none := map[RenameTable]int{RenameTableIssues: 0, RenameTablePullRequests: 0, RenameTableClearedPullRequests: 0, RenameTableProductionChecks: 0,
			RenameTableRepoNotes: 0}
		if left := renameLeft(t, ctx, tx, "LEGION", from); !reflect.DeepEqual(left, none) {
			t.Fatalf("rows on %s after the write = %v, want none", from, left)
		}
		again, err := RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
		if !reflect.DeepEqual(again.Rows, none) || again.Deleted != nil {
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

// The rename's deletion of a row the new name would hold twice keeps what the row shows of the
// merge (clearPullRequests). LEGION-1's merged row collides with LEGION-2's row on the new name and
// the same branch: the rename saves its merge as LEGION-1's snapshot, under the new name, and
// LEGION-1 derives ANSWERED. LEGION-3's closed row collides with LEGION-4's: the rename keeps its
// clock, moved to the new name with LEGION-3's other clocks, and LEGION-3 derives PARTIAL. Neither
// issue holds any other row that bears on the merge. A write of each record, and each clear of a
// generation, keep both. A second rename changes nothing.
func TestARenameKeepsTheDeletedIssuesMerge(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const from, to = "sjawhar/legion", "cybermonklabs/legion"
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	head, sha := strings.Repeat("a", 40), strings.Repeat("b", 40)
	recorded := func(key, repo string) Issue {
		issue := issueFixture(key)
		issue.Tree, issue.Parent, issue.Repo, issue.GitHubRepo = key, nil, repo, repo
		return issue
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, seed := range []struct {
			key, repo, branch string
			number            int
			state             PullRequestState
		}{
			{"LEGION-1", from, "release/shared", 7, PullRequestMerged},
			{"LEGION-2", to, "release/shared", 8, PullRequestOpen},
			{"LEGION-3", from, "release/other", 9, PullRequestClosed},
			{"LEGION-4", to, "release/other", 10, PullRequestOpen},
		} {
			must(t, records.PutIssue(ctx, tx, recorded(seed.key, seed.repo)))
			pr := PullRequest{Issue: seed.key, Repo: seed.repo, Number: seed.number, Branch: seed.branch, Base: "main", HeadSHA: head,
				HeadUpdatedAt: at, State: seed.state}
			if seed.state == PullRequestMerged {
				pr.MergeSHA = sha
			}
			must(t, records.PutPullRequest(ctx, tx, pr))
		}
	})
	rename := func(t *testing.T) map[string]RenameWrite {
		t.Helper()
		written := map[string]RenameWrite{}
		inTx(t, st, func(tx pgx.Tx) {
			for _, key := range []string{"LEGION-1", "LEGION-3"} {
				write, err := RenameIssueRepository(ctx, tx, key, from, to)
				must(t, err)
				written[key] = write
			}
		})
		return written
	}
	first := rename(t)
	if first["LEGION-1"].Deleted == nil || first["LEGION-3"].Deleted == nil {
		t.Fatalf("the renames = %+v, want each issue's colliding row deleted", first)
	}
	held := func(t *testing.T, after string) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			merged, err := records.Issue(ctx, tx, "LEGION-1")
			must(t, err)
			if want := (Merge{Number: 7, Head: head, SHA: sha, Base: "main", Repo: to}); merged.Merge != want {
				t.Fatalf("LEGION-1's snapshot after %s = %+v, want its deleted row's merge under the new name, %+v", after, merged.Merge, want)
			}
			if got := rowsText(t, tx, `select repo || ' ' || number from cleared_pull_requests where issue = 'LEGION-3'`); !reflect.DeepEqual(got, []string{to + " 9"}) {
				t.Fatalf("LEGION-3's clocks after %s = %v, want its deleted row's clock under the new name", after, got)
			}
			for key, want := range map[string]MergeHistoryClass{"LEGION-1": MergeHistoryAnswered, "LEGION-3": MergeHistoryPartial} {
				history, err := records.MergeHistoryOf(ctx, tx, key)
				must(t, err)
				if history.Class != want {
					t.Fatalf("%s after %s derives %s, want %s", key, after, history.Class, want)
				}
			}
		})
	}
	held(t, "the rename")
	for _, write := range []struct {
		name  string
		write func(tx pgx.Tx, key string) error
	}{
		{"PutIssue", func(tx pgx.Tx, key string) error { return records.PutIssue(ctx, tx, recorded(key, to)) }},
		{"ClearGeneration", func(tx pgx.Tx, key string) error { return records.ClearGeneration(ctx, tx, key, at.Add(time.Hour)) }},
		{"ClearTreeGeneration", func(tx pgx.Tx, key string) error { return records.ClearTreeGeneration(ctx, tx, key, at.Add(time.Hour)) }},
		{"ClearChangeGeneration", func(tx pgx.Tx, key string) error {
			return records.ClearChangeGeneration(ctx, tx, key, at.Add(time.Hour))
		}},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			for _, key := range []string{"LEGION-1", "LEGION-3"} {
				must(t, write.write(tx, key))
			}
		})
		held(t, write.name)
	}
	for key, again := range rename(t) {
		for table, rows := range again.Rows {
			if rows != 0 {
				t.Errorf("a second rename of %s wrote %d rows of %s, want none", key, rows, table)
			}
		}
		if again.Deleted != nil {
			t.Errorf("a second rename of %s deleted %+v, want nothing", key, again.Deleted)
		}
	}
	held(t, "a second rename")
}

// A new generation's clear keeps the issue whose pull request row it deleted beside its clock
// (clearGeneration), so the pass reads and writes a cleared pull request by its issue, as it does
// every other row. cleared_pull_requests is keyed on the repository and the number, so where the
// new name already holds a row of the same number, the issue's row under the old name is merged
// into it: the merged row keeps the later clock and the later clear of the two, and names the
// issue whose row moved. So the clock under the new name is the later of the two
// (ClearedPullRequestClock), none is left under the old name, and a second write writes nothing.
// Control: with the row's repository written in place rather than merged, the write fails on the
// table's key, and the issue's whole write with it.
func TestRenameIssueRepositoryMergesAClearedPullRequestIntoTheRowTheNewNameHolds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const from, to = "sjawhar/legion", "cybermonklabs/legion"
	applied := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	records := NewStore()
	// LEGION-1's #42 is cleared under the old name with the later clock and the earlier clear, and
	// LEGION-2's under the new name with the earlier clock and the later clear.
	for _, seed := range []struct {
		key, repo        string
		clock, clearedAt time.Time
	}{
		{"LEGION-1", from, applied.Add(time.Hour), applied.Add(2 * time.Hour)},
		{"LEGION-2", to, applied, applied.Add(3 * time.Hour)},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			issue := issueFixture(seed.key)
			issue.Tree, issue.Parent, issue.Repo, issue.GitHubRepo = seed.key, nil, seed.repo, seed.repo
			must(t, records.PutIssue(ctx, tx, issue))
			must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: seed.key, Repo: seed.repo, Number: 42, Branch: "legion/" + seed.key,
				HeadSHA: "head", HeadUpdatedAt: seed.clock, State: PullRequestClosed}))
			must(t, records.ClearGeneration(ctx, tx, seed.key, seed.clearedAt))
		})
	}
	type clearedRow struct {
		repo, issue      string
		number           int
		clock, clearedAt time.Time
	}
	clearedRows := func(tx pgx.Tx) []clearedRow {
		rows, err := tx.Query(ctx, "select repo, number, issue, head_updated_at, cleared_at from cleared_pull_requests order by repo, number")
		must(t, err)
		defer rows.Close()
		var got []clearedRow
		for rows.Next() {
			var row clearedRow
			must(t, rows.Scan(&row.repo, &row.number, &row.issue, &row.clock, &row.clearedAt))
			got = append(got, row)
		}
		must(t, rows.Err())
		return got
	}
	merged := clearedRow{repo: to, number: 42, issue: "LEGION-1", clock: applied.Add(time.Hour), clearedAt: applied.Add(3 * time.Hour)}
	same := func(got []clearedRow) bool {
		return len(got) == 1 && got[0].repo == merged.repo && got[0].number == merged.number && got[0].issue == merged.issue &&
			got[0].clock.Equal(merged.clock) && got[0].clearedAt.Equal(merged.clearedAt)
	}

	inTx(t, st, func(tx pgx.Tx) {
		candidates, err := RenameCandidates(ctx, tx, "LEGION", from, nil)
		must(t, err)
		want := []RenameCandidate{{Issue: "LEGION-1", Rows: map[RenameTable]int{RenameTableIssues: 1, RenameTableClearedPullRequests: 1}, Spellings: []string{from}}}
		if !reflect.DeepEqual(candidates, want) {
			t.Fatalf("candidates = %+v, want %+v: the clear keeps its issue", candidates, want)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		written, err := RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
		if written.Rows[RenameTableIssues] != 1 || written.Rows[RenameTableClearedPullRequests] != 1 || written.Deleted != nil {
			t.Fatalf("the write of LEGION-1 = %+v, want its record and its cleared pull request written", written)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		if got := clearedRows(tx); !same(got) {
			t.Fatalf("cleared pull requests after the write = %+v, want the one row %+v", got, merged)
		}
		for repo, want := range map[string]time.Time{to: merged.clock, from: {}} {
			clock, err := records.ClearedPullRequestClock(ctx, tx, repo, 42)
			must(t, err)
			if !clock.Equal(want) {
				t.Fatalf("the clock of %s#42 = %s, want %s", repo, clock, want)
			}
		}
		again, err := RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
		if again.Rows[RenameTableClearedPullRequests] != 0 {
			t.Fatalf("a second write of LEGION-1 = %+v, want no cleared pull request written", again)
		}
		if got := clearedRows(tx); !same(got) {
			t.Fatalf("cleared pull requests after a second write = %+v, want the one row %+v", got, merged)
		}
	})
}
