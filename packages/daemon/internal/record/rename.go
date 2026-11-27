package record

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The writer of this release's one-time rename pass (dispatch://INFRA-4401 A10): the daemon writes
// each configured rename's new name (`repository_renames`' `to`) over its old one (`from`) on every
// stored column that holds a repository name, once GitHub shows the repository's transfer, at its
// boot and at the end of an issue's pod (internal/daemon's renamePass), and
// dispatch://INFRA-3532's own change removes it with these functions. PutIssue refuses a recorded
// issue another repository, so the pass writes through RenameIssueRepository and nothing else does.
//
// The columns are every one the migrations give a repository name: issues.repo and
// issues.github_repo (0047, 0048), pull_requests.repo (0004), cleared_pull_requests.repo
// (20261120144241), production_checks.repo (0049), repo_notes.resolved_repo (0047) and
// pull_request_links.repo (20261127031536), and the key repo of the payload of each outbox row
// that names a pull request by its repository and number (renamedPayloadKinds). No other column
// holds one: the claims' and claim capabilities' locators name a tmux pane or a Sandbox, and
// processed_events.event_id only embeds one in a dedupe key. pull_request_settled_heads.repo
// follows pull_requests.repo by its foreign key (on update cascade). Every comparison is made
// without case (`lower(col) = lower(from)`), as every read of a repository name in the record is,
// so a row in any spelling of the old name is written; the new name is written as the caller hands
// it, on every column alike, which the pass hands in GitHub's spelling of the pair's `to`
// (Issue.GitHubRepo is the spelling GitHub intake's subjects are built from). A pair never renames
// a repository to itself in another case (ghrepo.NewRenames), so a row on the new name never
// matches the old one, and a second run writes nothing.

// RenameTable is a table whose columns hold a repository name, as a count by table names it
// (RenameCandidate.Rows, RenameWrite.Rows).
type RenameTable string

// The tables whose columns hold a repository name, one constant each.
const (
	RenameTableIssues              RenameTable = "issues"
	RenameTablePullRequests        RenameTable = "pull_requests"
	RenameTableClearedPullRequests RenameTable = "cleared_pull_requests"
	RenameTableProductionChecks    RenameTable = "production_checks"
	RenameTableRepoNotes           RenameTable = "repo_notes"
	RenameTablePullRequestLinks    RenameTable = "pull_request_links"
	RenameTableOutbox              RenameTable = "outbox"
)

// RenameTables are the tables whose columns hold a repository name, in the order the rename pass
// writes and logs them.
var RenameTables = []RenameTable{RenameTableIssues, RenameTablePullRequests, RenameTableClearedPullRequests, RenameTableProductionChecks,
	RenameTableRepoNotes, RenameTablePullRequestLinks, RenameTableOutbox}

// renamedPayloadKinds is each outbox kind whose payload names the repository of its pull request
// (its key repo, since migration 20261127031536), as the SQL list the rename's reads and writes
// name: a row of a kind of the release before names none, and means its issue's repository.
var renamedPayloadKinds = "'" + strings.Join([]string{string(OutboxKindMergeQueueRead), string(OutboxKindMergeQueueRemoval),
	string(OutboxKindConflictConfirm), string(OutboxKindMergeForward), string(OutboxKindTaskPullRequestsClose),
	string(OutboxKindPullRequestLinkRead)}, "', '") + "'"

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
		{RenameTableClearedPullRequests, `select c.issue, c.repo, ''::text from cleared_pull_requests c join issues i on i.key = c.issue
			where i.project = $1 and lower(c.repo) = lower($2) and ($3::text[] is null or c.issue = any($3))`},
		{RenameTableProductionChecks, `select c.issue, c.repo, ''::text from production_checks c join issues i on i.key = c.issue
			where i.project = $1 and lower(c.repo) = lower($2) and ($3::text[] is null or c.issue = any($3))`},
		{RenameTableRepoNotes, `select n.issue, n.resolved_repo, ''::text from repo_notes n
			where lower(n.resolved_repo) = lower($2) and ($3::text[] is null or n.issue = any($3))
			and (starts_with(n.issue, $1 || '-') or n.issue in (select key from issues where project = $1))`},
		{RenameTablePullRequestLinks, `select l.issue, l.repo, ''::text from pull_request_links l join issues i on i.key = l.issue
			where i.project = $1 and lower(l.repo) = lower($2) and ($3::text[] is null or l.issue = any($3))`},
		{RenameTableOutbox, `select o.issue, o.payload->>'repo', ''::text from outbox o join issues i on i.key = o.issue
			where i.project = $1 and o.kind in (` + renamedPayloadKinds + `) and lower(o.payload->>'repo') = lower($2)
			and ($3::text[] is null or o.issue = any($3))`},
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
// (RenameTables) it wrote the new name on, and each pull request row of the issue it deleted, in
// number order, none for none.
type RenameWrite struct {
	Rows    map[RenameTable]int
	Deleted []RenameCollision
}

// RenameCollision is an issue's pull request row that RenameIssueRepository deleted rather than
// write the new name on, since another row (Holder's, the issue's own among them) already holds the
// same pull request under the new name: Repo is the old name the deleted row held, Number its pull
// request and Branch its branch, To the name Holder's row holds.
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
// requests and what each remembers of its settled heads (by the foreign key's cascade), the pull
// requests a new generation of it cleared, its production checks, its repository note, its links
// to pull requests and the outbox rows of it that name a pull request's repository. It runs in the
// caller's transaction, so the rows of one issue are written together or not at all, and a write of
// rows that name to already changes nothing.
//
// A pull request row of the issue is deleted instead where the new name would make it a second row
// of one pull request: pull_requests is unique on the lowered repository and the number (migration
// 20261127031536), so a row of the new name in any spelling and of the same number, of another
// issue or of this one, already holds that pull request. The row under the new name stays, with
// what it remembers, the colliding row alone goes, with what it remembers, and each other row of
// the issue takes the new name; each deletion is returned for the pass to log, naming both issues.
// A link of the issue is merged the same way: a link of the new name and the same number already
// links that pull request, so the issue's link under the old name goes, and its others take the
// new name.
//
// A cleared pull request's row is merged into the row to already holds for the same number, where
// there is one, since cleared_pull_requests is keyed on (repo, number): the merged row keeps the
// later clock and the later clear of the two and names issue. A row of to in another spelling is
// another key and stays beside it; the clock's read takes the latest of every spelling
// (ClearedPullRequestClock).
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

	rows, err := tx.Query(ctx, `delete from pull_requests mine using pull_requests other
		where mine.issue = $1 and lower(mine.repo) = lower($2) and lower(other.repo) = lower($3) and other.number = mine.number
		returning mine.issue, other.issue, mine.repo, other.repo, mine.branch, mine.number`, issue, from, to)
	if err != nil {
		return RenameWrite{}, fmt.Errorf("delete the pull requests of %s that %s holds twice: %w", issue, to, err)
	}
	written.Deleted, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RenameCollision, error) {
		var deleted RenameCollision
		err := row.Scan(&deleted.Issue, &deleted.Holder, &deleted.Repo, &deleted.To, &deleted.Branch, &deleted.Number)
		return deleted, err
	})
	if err != nil {
		return RenameWrite{}, fmt.Errorf("delete the pull requests of %s that %s holds twice: %w", issue, to, err)
	}
	slices.SortFunc(written.Deleted, func(a, b RenameCollision) int { return cmp.Compare(a.Number, b.Number) })
	if len(written.Deleted) == 0 {
		written.Deleted = nil
	}
	if _, err := tx.Exec(ctx, `delete from pull_request_links mine using pull_request_links other
		where mine.issue = $1 and lower(mine.repo) = lower($2) and lower(other.repo) = lower($3) and other.number = mine.number`,
		issue, from, to); err != nil {
		return RenameWrite{}, fmt.Errorf("delete the links of %s that %s holds twice: %w", issue, to, err)
	}

	var cleared int
	if err := tx.QueryRow(ctx, `with moved as (
			delete from cleared_pull_requests where issue = $1 and lower(repo) = lower($2)
			returning number, head_updated_at, cleared_at),
		merged as (
			insert into cleared_pull_requests (repo, number, issue, head_updated_at, cleared_at)
			select $3, number, $1, max(head_updated_at), max(cleared_at) from moved group by number
			on conflict (repo, number) do update set issue = excluded.issue,
				head_updated_at = greatest(cleared_pull_requests.head_updated_at, excluded.head_updated_at),
				cleared_at = greatest(cleared_pull_requests.cleared_at, excluded.cleared_at))
		select count(*) from moved`, issue, from, to).Scan(&cleared); err != nil {
		return RenameWrite{}, fmt.Errorf("write %s over %s on the cleared pull requests of %s: %w", to, from, issue, err)
	}
	written.Rows[RenameTableClearedPullRequests] = cleared

	for _, write := range []struct {
		table RenameTable
		sql   string
	}{
		{RenameTablePullRequests, "update pull_requests set repo = $3 where issue = $1 and lower(repo) = lower($2)"},
		{RenameTableProductionChecks, "update production_checks set repo = $3 where issue = $1 and lower(repo) = lower($2)"},
		{RenameTableRepoNotes, "update repo_notes set resolved_repo = $3 where issue = $1 and lower(resolved_repo) = lower($2)"},
		{RenameTablePullRequestLinks, "update pull_request_links set repo = $3 where issue = $1 and lower(repo) = lower($2)"},
		{RenameTableOutbox, `update outbox set payload = jsonb_set(payload, '{repo}', to_jsonb($3::text))
			where issue = $1 and kind in (` + renamedPayloadKinds + `) and lower(payload->>'repo') = lower($2)`},
	} {
		tag, err := tx.Exec(ctx, write.sql, issue, from, to)
		if err != nil {
			return RenameWrite{}, fmt.Errorf("write %s over %s on the %s row of %s: %w", to, from, write.table, issue, err)
		}
		written.Rows[write.table] = int(tag.RowsAffected())
	}
	return written, nil
}
