package record

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The links of each issue to pull requests (PullRequestLink) are the table pull_request_links
// (migration 20261127031536), keyed by the pull request's repository and number, with a unique
// index on the lowered repository and the number that every write and read below keys on, as the
// reads of pull_requests do: a link names a repository as its URL spells it.

// ReplacePullRequestLinks deletes each link of issue that links does not name, and writes each of
// links that the record does not hold for issue, taken and untold, in one statement. A link that
// stays keeps its row as it stands, its refusal, its told mark and its spelling. A link another
// issue holds moves to issue, taken and untold. A link that names no repository or no pull request
// is refused before anything is written. It keeps the mark of a record write where a link was
// deleted or written (RecordsWritten).
func (s *Postgres) ReplacePullRequestLinks(ctx context.Context, tx pgx.Tx, issue string, links []PullRequestLink) error {
	describe := fmt.Sprintf("replace the links of %s to pull requests", issue)
	type named struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	}
	given := make([]named, len(links))
	for at, link := range links {
		if link.Repo == "" || link.Number < 1 {
			return fmt.Errorf("%s: a link names a repository and a pull request; it names %q#%d", describe, link.Repo, link.Number)
		}
		given[at] = named{Repo: link.Repo, Number: link.Number}
	}
	encoded, err := json.Marshal(given)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	var changed int
	if err := tx.QueryRow(ctx, `with given as (
			select distinct on (lower(repo), number) repo, number
			from jsonb_to_recordset($2::jsonb) as link(repo text, number integer) order by lower(repo), number, repo),
		gone as (
			delete from pull_request_links stored where stored.issue = $1
				and not exists (select 1 from given where lower(given.repo) = lower(stored.repo) and given.number = stored.number)
			returning 1),
		written as (
			insert into pull_request_links (issue, repo, number) select $1, repo, number from given
			on conflict ((lower(repo)), number) do update set issue = excluded.issue, repo = excluded.repo, refused = '', told_at = null
			where pull_request_links.issue <> excluded.issue
			returning 1)
		select (select count(*) from gone) + (select count(*) from written)`, issue, encoded).Scan(&changed); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if changed > 0 {
		noteRecordWrite(ctx)
	}
	return nil
}

// pullRequestLinkColumns are the columns of a pull_request_links row, in PullRequestLink's order.
const pullRequestLinkColumns = "issue, repo, number, refused, told_at"

func (s *Postgres) PullRequestLinksOf(ctx context.Context, tx pgx.Tx, issue string) ([]PullRequestLink, error) {
	return readPullRequestLinks(ctx, tx, fmt.Sprintf("read the links of %s to pull requests", issue),
		"select "+pullRequestLinkColumns+" from pull_request_links where issue = $1 order by lower(repo), number", issue)
}

func (s *Postgres) PullRequestLinks(ctx context.Context, tx pgx.Tx) ([]PullRequestLink, error) {
	return readPullRequestLinks(ctx, tx, "read the links of every issue to pull requests",
		"select "+pullRequestLinkColumns+" from pull_request_links order by lower(repo), number")
}

func (s *Postgres) PullRequestLinkOf(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequestLink, error) {
	links, err := readPullRequestLinks(ctx, tx, fmt.Sprintf("read the link of %s#%d", repo, number),
		"select "+pullRequestLinkColumns+" from pull_request_links where lower(repo) = lower($1) and number = $2", repo, number)
	if err != nil || len(links) == 0 {
		return nil, err
	}
	return &links[0], nil
}

// readPullRequestLinks is each link query selects, naming the read in any failure as describe.
func readPullRequestLinks(ctx context.Context, tx pgx.Tx, describe, query string, args ...any) ([]PullRequestLink, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PullRequestLink, error) {
		var link PullRequestLink
		err := row.Scan(&link.Issue, &link.Repo, &link.Number, &link.Refused, &link.ToldAt)
		return link, err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return links, nil
}

func (s *Postgres) SetPullRequestLinkTold(ctx context.Context, tx pgx.Tx, repo string, number int, at *time.Time) error {
	tag, err := tx.Exec(ctx, `update pull_request_links set told_at = $3 where lower(repo) = lower($1) and number = $2`, repo, number, at)
	if err != nil {
		return fmt.Errorf("mark the link of %s#%d told: %w", repo, number, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

func (s *Postgres) SetPullRequestLinkRefused(ctx context.Context, tx pgx.Tx, repo string, number int, refused LinkRefusal) error {
	tag, err := tx.Exec(ctx, `update pull_request_links set refused = $3 where lower(repo) = lower($1) and number = $2`, repo, number, string(refused))
	if err != nil {
		return fmt.Errorf("write the refusal %q of the link of %s#%d: %w", refused, repo, number, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}
