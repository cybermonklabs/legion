package record

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The writer of this release's one-time rename pass (dispatch://INFRA-4401 A10): the daemon writes
// each configured rename's new name (`repository_renames`' `to`) over its old one (`from`) on every
// stored column that holds a repository name, once, at its boot and at the end of an issue's pod
// (internal/daemon's renamePass), and dispatch://INFRA-3532's own change removes it with these
// functions. PutIssue refuses a recorded issue another repository, so the pass writes through
// RenameIssueRepository and nothing else does.
//
// The columns are every one the migrations give a repository name: issues.repo and
// issues.github_repo (0047, 0048), pull_requests.repo (0004), production_checks.repo (0049) and
// repo_notes.resolved_repo (0047). No other column holds one: the claims' and claim
// capabilities' locators name a tmux pane or a Sandbox, the outbox's payloads and the bundles'
// rows carry no repository, and processed_events.event_id only embeds one in a dedupe key. Every
// comparison is made without case (`lower(col) = lower(from)`), as every read of a repository
// name in the record is, so a row in any spelling of the old name is written; the new name is
// written as the pair configures it. A pair never renames a repository to itself in another case
// (ghrepo.NewRenames), so a row on the new name never matches the old one, and a second run
// writes nothing.

// RenameTable is a table whose columns hold a repository name, as a count by table names it
// (RenameCandidate.Rows, RenameWrite.Rows).
type RenameTable string

// The tables whose columns hold a repository name, one constant each.
const (
	RenameTableIssues           RenameTable = "issues"
	RenameTablePullRequests     RenameTable = "pull_requests"
	RenameTableProductionChecks RenameTable = "production_checks"
	RenameTableRepoNotes        RenameTable = "repo_notes"
)

// RenameTables are the tables whose columns hold a repository name, in the order the rename pass
// writes and logs them.
var RenameTables = []RenameTable{RenameTableIssues, RenameTablePullRequests, RenameTableProductionChecks, RenameTableRepoNotes}

// RenameCandidate is one issue whose stored rows name a rename's old name: Rows is how many rows
// of each table (RenameTables) do, which is what its write changes, and Spellings each spelling
// of the old name those rows hold, sorted, which is what a workspace directory of the issue may be
// named by (workspace.Location keeps the case of the name it is handed).
type RenameCandidate struct {
	Issue     string
	Rows      map[RenameTable]int
	Spellings []string
}

// RenameCandidates is each issue of project whose stored rows name from, compared without case,
// by key, or of keys alone when keys is not nil (the issues whose pod ended). An issue is of
// project when its record is (issues.project); a repository note is too when its issue's key is
// the project's (`<project>-…`, as ClaimedIssuesSince reads the claims), since the note of an
// issue the daemon refused to record has no record to name its project. With keys nil, its rows
// summed by table are what the rename pass logs as left once it has run, the count each pair
// waits on to be zero before it can leave `repository_renames`.
func RenameCandidates(ctx context.Context, tx pgx.Tx, project, from string, keys []string) ([]RenameCandidate, error) {
	byIssue := map[string]*RenameCandidate{}
	for _, read := range []struct {
		table RenameTable
		sql   string
	}{
		{RenameTableIssues, `select key, repo, github_repo from issues
			where project = $1 and (lower(repo) = lower($2) or lower(github_repo) = lower($2))
			and ($3::text[] is null or key = any($3))`},
		{RenameTablePullRequests, `select p.issue, p.repo, ''::text from pull_requests p join issues i on i.key = p.issue
			where i.project = $1 and lower(p.repo) = lower($2) and ($3::text[] is null or p.issue = any($3))`},
		{RenameTableProductionChecks, `select c.issue, c.repo, ''::text from production_checks c join issues i on i.key = c.issue
			where i.project = $1 and lower(c.repo) = lower($2) and ($3::text[] is null or c.issue = any($3))`},
		{RenameTableRepoNotes, `select n.issue, n.resolved_repo, ''::text from repo_notes n
			where lower(n.resolved_repo) = lower($2) and ($3::text[] is null or n.issue = any($3))
			and (starts_with(n.issue, $1 || '-') or n.issue in (select key from issues where project = $1))`},
	} {
		rows, err := tx.Query(ctx, read.sql, project, from, keys)
		if err != nil {
			return nil, fmt.Errorf("read the %s rows of %s that name %s: %w", read.table, project, from, err)
		}
		for rows.Next() {
			var issue string
			var names [2]string
			if err := rows.Scan(&issue, &names[0], &names[1]); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read the %s rows of %s that name %s: %w", read.table, project, from, err)
			}
			candidate := byIssue[issue]
			if candidate == nil {
				candidate = &RenameCandidate{Issue: issue, Rows: map[RenameTable]int{}}
				byIssue[issue] = candidate
			}
			candidate.Rows[read.table]++
			for _, name := range names {
				if strings.EqualFold(name, from) && !slices.Contains(candidate.Spellings, name) {
					candidate.Spellings = append(candidate.Spellings, name)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the %s rows of %s that name %s: %w", read.table, project, from, err)
		}
	}
	candidates := make([]RenameCandidate, 0, len(byIssue))
	for _, candidate := range byIssue {
		slices.Sort(candidate.Spellings)
		candidates = append(candidates, *candidate)
	}
	slices.SortFunc(candidates, func(a, b RenameCandidate) int { return cmp.Compare(a.Issue, b.Issue) })
	return candidates, nil
}

// RenameWrite is what one issue's write came to (RenameIssueRepository): the rows of each table
// (RenameTables) it wrote the new name on, and the issue's pull request row it deleted, nil for
// none.
type RenameWrite struct {
	Rows    map[RenameTable]int
	Deleted *RenameCollision
}

// RenameCollision is an issue's pull request row that RenameIssueRepository deleted rather than
// write the new name on, since another issue's row (Holder's) already holds the new name and the
// same branch: Repo is the old name the deleted row held, Number its pull request, To the name
// Holder's row holds.
type RenameCollision struct {
	Issue  string
	Holder string
	Repo   string
	To     string
	Branch string
	Number int
}

// RenameIssueRepository writes to over from, compared without case, on each column of issue's
// rows that holds a repository name: its record's repository and GitHub's spelling of it, its pull
// request, its production check and its repository note. It runs in the caller's transaction, so
// the rows of one issue are written together or not at all, and a write of rows that name to
// already changes nothing.
//
// Its pull request row is deleted instead where the new name would make it a second row of one
// repository and branch: pull_requests_repo_branch (0004) is unique on (repo, branch), and the
// reads that key the table on its repository compare the name without case (PullRequestByBranch),
// so another issue's row on the new name in any spelling and on the same branch already holds that
// key. The row under the new name stays, and the deletion is returned for the pass to log, naming
// both issues.
func RenameIssueRepository(ctx context.Context, tx pgx.Tx, issue, from, to string) (RenameWrite, error) {
	written := RenameWrite{Rows: map[RenameTable]int{}}
	tag, err := tx.Exec(ctx, `update issues set
		repo = case when lower(repo) = lower($2) then $3 else repo end,
		github_repo = case when lower(github_repo) = lower($2) then $3 else github_repo end
		where key = $1 and (lower(repo) = lower($2) or lower(github_repo) = lower($2))`, issue, from, to)
	if err != nil {
		return RenameWrite{}, fmt.Errorf("write %s over %s on the record of %s: %w", to, from, issue, err)
	}
	written.Rows[RenameTableIssues] = int(tag.RowsAffected())

	var collision RenameCollision
	err = tx.QueryRow(ctx, `select other.issue, other.repo, mine.repo, mine.branch, mine.number
		from pull_requests mine join pull_requests other
		on other.branch = mine.branch and lower(other.repo) = lower($3) and other.issue <> mine.issue
		where mine.issue = $1 and lower(mine.repo) = lower($2)
		order by other.issue limit 1`, issue, from, to).
		Scan(&collision.Holder, &collision.To, &collision.Repo, &collision.Branch, &collision.Number)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		tag, err := tx.Exec(ctx, "update pull_requests set repo = $3 where issue = $1 and lower(repo) = lower($2)", issue, from, to)
		if err != nil {
			return RenameWrite{}, fmt.Errorf("write %s over %s on the pull request of %s: %w", to, from, issue, err)
		}
		written.Rows[RenameTablePullRequests] = int(tag.RowsAffected())
	case err != nil:
		return RenameWrite{}, fmt.Errorf("read whether the pull request of %s would hold %s twice: %w", issue, to, err)
	default:
		if _, err := tx.Exec(ctx, "delete from pull_requests where issue = $1 and lower(repo) = lower($2)", issue, from); err != nil {
			return RenameWrite{}, fmt.Errorf("delete the pull request of %s, which %s holds on %s: %w", issue, collision.Holder, to, err)
		}
		collision.Issue = issue
		written.Deleted = &collision
	}

	for _, write := range []struct {
		table RenameTable
		sql   string
	}{
		{RenameTableProductionChecks, "update production_checks set repo = $3 where issue = $1 and lower(repo) = lower($2)"},
		{RenameTableRepoNotes, "update repo_notes set resolved_repo = $3 where issue = $1 and lower(resolved_repo) = lower($2)"},
	} {
		tag, err := tx.Exec(ctx, write.sql, issue, from, to)
		if err != nil {
			return RenameWrite{}, fmt.Errorf("write %s over %s on the %s row of %s: %w", to, from, write.table, issue, err)
		}
		written.Rows[write.table] = int(tag.RowsAffected())
	}
	return written, nil
}
