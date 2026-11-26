package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
)

// gitHubReadAttemptTimeout bounds each statement of GitHubReadAttempts: one row written, or the
// rows read, one for each read, which a healthy database answers in milliseconds.
const gitHubReadAttemptTimeout = 5 * time.Second

// GitHubReadAttempts is the record of each GitHub read's newest attempt (migration
// 20261118020802, github_read_attempts), which GET /healthz answers from each process's copy
// (githubreads.Copy). The loop that holds a read's lease writes that read's row when its pass
// ends; every process reads every row.
type GitHubReadAttempts struct {
	pool *Pool
}

// NewGitHubReadAttempts is the record on pool.
func NewGitHubReadAttempts(pool *Pool) *GitHubReadAttempts { return &GitHubReadAttempts{pool: pool} }

// Record writes read's newest attempt over the one before it, at the database's now(), with no
// cause when it succeeded. It overwrites rather than keeps a history because only the process
// holding read's lease writes it, one period at a time, and the probe answers the newest attempt
// alone. Its method value is a githubreads.Recorder.
func (g *GitHubReadAttempts) Record(ctx context.Context, read githubreads.Read, succeeded bool, cause string) error {
	ctx, cancel := context.WithTimeout(ctx, gitHubReadAttemptTimeout)
	defer cancel()
	var stored *string
	if !succeeded {
		stored = &cause
	}
	if _, err := g.pool.Exec(ctx, `insert into github_read_attempts (read, at, succeeded, cause) values ($1, now(), $2, $3)
		on conflict (read) do update set at = excluded.at, succeeded = excluded.succeeded, cause = excluded.cause`,
		string(read), succeeded, stored); err != nil {
		return fmt.Errorf("record the newest attempt of GitHub read %s: %w", read, err)
	}
	return nil
}

// All reads every row of the record, a cause of a succeeded attempt reading "". It selects
// githubreads.Recorded's fields in their order, which is how each row fills one (RowToStructByPos).
func (g *GitHubReadAttempts) All(ctx context.Context) ([]githubreads.Recorded, error) {
	ctx, cancel := context.WithTimeout(ctx, gitHubReadAttemptTimeout)
	defer cancel()
	rows, err := g.pool.Query(ctx, `select read, at, succeeded, coalesce(cause, '') from github_read_attempts`)
	if err != nil {
		return nil, fmt.Errorf("read the GitHub read attempts: %w", err)
	}
	recorded, err := pgx.CollectRows(rows, pgx.RowToStructByPos[githubreads.Recorded])
	if err != nil {
		return nil, fmt.Errorf("read the GitHub read attempts: %w", err)
	}
	return recorded, nil
}
