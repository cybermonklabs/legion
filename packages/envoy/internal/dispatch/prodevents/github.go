package prodevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The GitHub adapter's writes and the signals' reads (dispatch://INFRA-978, R6 and R7). A GitHub
// deploy row belongs to its run, not to a project (the architect's ruling,
// dispatch://INFRA-978/comment/e6c99e06-0589-4692-b30f-ed64684bd33a, point 4): the adapter writes
// it where the repository's rule calls the run a deploy, moves it in place where the run moved (a
// re-run's times, a pull request it ships) or the rule moved (a source moved to another project, a
// changed rule), its project with the rest, and drops it, keeping it, where the rule no longer
// calls the run a deploy (prodevents/signals). So every failure that blames a row, and every
// restore that names it, keeps it whatever moves. Every write is a transaction of its own that
// takes the delivery change log's lock nowhere; its change rows wait in the outbox, and the write
// drains them after its commit.

// StatusSucceeded is the status of a GitHub deploy row: OTel's deployment.status of a deploy that
// reached production, which the production job's success under the rule says it did.
const StatusSucceeded = "succeeded"

// StatusDropped is the status of a GitHub deploy row whose run the repository's rule no longer
// calls a deploy. The row stays, with the reason in its note; the failures it held counted are
// rejected with that reason, and no measure counts it as a deploy, until a rule calls its run a
// deploy again (UpsertGitHubDeploy).
const StatusDropped = "dropped"

// droppedDeploy is the SQL condition of a dropped GitHub deploy row (StatusDropped), which no
// signal blames: the reads of a signal's deploy leave it out. Event.Dropped is the same rule.
const droppedDeploy = `(source = '` + SourceGitHub + `' and status = '` + StatusDropped + `')`

// Dropped reports whether e is a dropped GitHub deploy row (droppedDeploy), which counts for no
// project: only the GitHub adapter drops a row, so a posted deploy is never one, whatever status
// its system sent.
func (e Event) Dropped() bool {
	return e.Source == SourceGitHub && e.Status == StatusDropped
}

// UpsertGitHubDeploy writes event, the GitHub adapter's deploy row of a run its repository's rule
// calls a deploy, and answers the row it wrote and true. The run's row, of any project (runRow),
// moves in place to event's project and fields with its id: a re-run's times, the pull requests
// and commits it ships, its url, a source moved to another project, a rule changed. A run that
// holds no row gains one. A dropped row (StatusDropped) counts again, and so does each failure
// that a drop rejected, whose reject names event's actor as its writer (DropGitHubDeploy): it is
// counted again with reason, by that actor at at. A failure a person or a controller rejected or
// counted stays as they left it. A row whose fields would not move is not written, so a second
// write of one run writes nothing, logs no change, and answers no row and false.
func UpsertGitHubDeploy(ctx context.Context, pool *store.Pool, event Event, reason string, at time.Time) (Event, bool, error) {
	if event.Kind != KindDeploy || event.Source != SourceGitHub {
		return Event{}, false, refuse(CodeInput, "the GitHub adapter writes deploys of source github alone")
	}
	actor, environment, err := prepared(event)
	if err != nil {
		return Event{}, false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Event{}, false, fmt.Errorf("begin the write of production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	defer tx.Rollback(ctx)
	id, dropped, err := runRow(ctx, tx, event)
	if err != nil {
		return Event{}, false, err
	}
	if id == "" {
		id = uuid.Must(uuid.NewV7()).String()
	}
	stored, err := scan(tx.QueryRow(ctx, `
		insert into production_events (id, project_key, kind, environment, status, source, source_ref,
			started_at, finished_at, repo, commits, pull_requests, url, actor, note)
		values ($1, $2, 'deploy', $3, $4, 'github', $5, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (id) do update set
			project_key = excluded.project_key, status = excluded.status, started_at = excluded.started_at,
			finished_at = excluded.finished_at, repo = excluded.repo, commits = excluded.commits,
			pull_requests = excluded.pull_requests, url = excluded.url, note = excluded.note
		where (production_events.project_key, production_events.status, production_events.started_at,
				production_events.finished_at, production_events.repo, production_events.commits,
				production_events.pull_requests, production_events.url, production_events.note)
			is distinct from (excluded.project_key, excluded.status, excluded.started_at, excluded.finished_at,
				excluded.repo, excluded.commits, excluded.pull_requests, excluded.url, excluded.note)
		returning `+columns,
		id, event.ProjectKey, environment, event.Status, event.SourceRef, event.StartedAt, event.FinishedAt, event.Repo,
		nonNil(event.Commits), nonNil(event.PullRequests), event.URL, actor, event.Note))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("write production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	if dropped && !stored.Dropped() {
		if _, err := tx.Exec(ctx, `
			update production_events set state = 'counted', state_reason = $2, state_by = $3, state_at = $4
			where kind = 'failure' and deploy_id = $1 and state = 'rejected' and state_by = $3
		`, id, reason, actor, at); err != nil {
			return Event{}, false, fmt.Errorf("count again the failures of production event %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, false, fmt.Errorf("commit production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a GitHub deploy row write")
	return stored, true, nil
}

// runRow is the id of the row event's run holds, "" where it holds none, and whether that row is
// dropped, locked in tx `for update`, the lock a drop takes (DropGitHubDeploy), so a failure being
// written against it is written before the row moves, or after: the GitHub deploy row of the run in
// its repository, of any project (production_events_github_runs), or the row event's project holds
// under the run's source_ref. Of two such rows, event's project's is the run's, so the write moves
// no row onto a key that project holds.
func runRow(ctx context.Context, tx pgx.Tx, event Event) (string, bool, error) {
	var id string
	var dropped bool
	err := tx.QueryRow(ctx, `select id::text, status = '`+StatusDropped+`' from production_events
		where kind = 'deploy' and source = 'github' and source_ref = $2 and (lower(repo) = lower($1) or project_key = $3)
		order by project_key = $3 desc, id
		limit 1
		for update`, event.Repo, event.SourceRef, event.ProjectKey).Scan(&id, &dropped)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the row of %s run %s: %w", event.Repo, event.SourceRef, err)
	}
	return id, dropped, nil
}

// DropGitHubDeploy takes the GitHub deploy row id of project out of the numbers, because the rule
// of its repository no longer calls its run a deploy. The row stays, so the failures that blame it
// and the restores that name it keep it, and a later rule that calls its run a deploy again counts
// it again (UpsertGitHubDeploy): its status is StatusDropped and its note reason, and each counted
// failure that blames it is rejected with reason, by actor at at. The row is locked first, so a
// failure written against it meanwhile waits for this write and then finds it as it left it. A row
// already dropped with this reason writes nothing.
func DropGitHubDeploy(ctx context.Context, pool *store.Pool, project, id, reason string, actor model.Actor, at time.Time) error {
	by, err := json.Marshal(actor.Recorded())
	if err != nil {
		return fmt.Errorf("encode the writer of the drop of production event %s: %w", id, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the drop of production event %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	var kind, source string
	if err := tx.QueryRow(ctx, `select kind, source from production_events where project_key = $1 and id = $2 for update`,
		project, id).Scan(&kind, &source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read production event %s: %w", id, err)
	}
	if kind != KindDeploy || source != SourceGitHub {
		return refuse(CodeInput, "row %s is no GitHub deploy, which the adapter alone drops", id)
	}
	rejected, err := tx.Exec(ctx, `
		update production_events set state = 'rejected', state_reason = $2, state_by = $3, state_at = $4
		where kind = 'failure' and deploy_id = $1 and state = 'counted'
	`, id, reason, by, at)
	if err != nil {
		return fmt.Errorf("reject the failures of production event %s: %w", id, err)
	}
	dropped, err := tx.Exec(ctx, `update production_events set status = $2, note = $3
		where id = $1 and (status, note) is distinct from ($2, $3)`, id, StatusDropped, reason)
	if err != nil {
		return fmt.Errorf("drop production event %s: %w", id, err)
	}
	if rejected.RowsAffected() == 0 && dropped.RowsAffected() == 0 {
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the drop of production event %s: %w", id, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a GitHub deploy row drop")
	return nil
}

// GitHubDeploys reads the GitHub deploy rows of repo, of any project, of the runs runIDs, or of
// every run of it where runIDs is nil, oldest finish first.
func GitHubDeploys(ctx context.Context, q delivery.Querier, repo string, runIDs []int64) ([]Event, error) {
	var refs []string
	if runIDs != nil {
		refs = make([]string, len(runIDs))
		for i, runID := range runIDs {
			refs[i] = GitHubSourceRef(runID)
		}
	}
	rows, err := q.Query(ctx, `select `+columns+` from production_events
		where kind = 'deploy' and source = 'github' and lower(repo) = lower($1)
		  and ($2::text[] is null or source_ref = any($2))
		order by finished_at, id`, repo, refs)
	if err != nil {
		return nil, fmt.Errorf("read the GitHub deploys of %s: %w", repo, err)
	}
	defer rows.Close()
	var deploys []Event
	for rows.Next() {
		event, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("read a GitHub deploy of %s: %w", repo, err)
		}
		deploys = append(deploys, event)
	}
	return deploys, rows.Err()
}

// DeployForPullRequest is the deploy row that names pullRequest (owner/repo#n, in any case) among
// the pull requests it shipped that a signal of project may blame (blamable), the earliest to reach
// production where several do, and false where none does: a deploy of any project but a dropped
// GitHub deploy and a deploy another project posted. Every deploy row names its pull requests in
// lower case (delivery.PullRequestName, ParsePullRequest), so the read is a probe of
// production_events_pull_requests (migration 20261127200929) for pullRequest in lower case.
func DeployForPullRequest(ctx context.Context, q delivery.Querier, project, pullRequest string) (Event, bool, error) {
	event, err := scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where kind = 'deploy' and pull_requests @> array[$1::text] and `+blamable+`
		order by finished_at, id limit 1`, strings.ToLower(pullRequest), project))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("read the deploy of %s: %w", pullRequest, err)
	}
	return event, true, nil
}

// ParsePullRequest reads ref, a pull request as a writer or the record names it, `owner/repo#n`,
// by the one reading of such a reference (refs.ParseExternalRef): its repository in the form
// Dispatch stores one, in lower case with no trailing `.git`, and its number, a positive int32 as a
// pull request's row holds one (delivery_pull_requests.number), and false for any other ref. The
// record stores a pull request as delivery.PullRequestName of the two, which this reads back as it
// was written, and every reader of the record's pull requests reads them through it.
func ParsePullRequest(ref string) (string, int, bool) {
	repo, digits, ok := refs.ParseExternalRef(ref)
	if !ok {
		return "", 0, false
	}
	number, err := strconv.ParseInt(digits, 10, 32)
	if err != nil {
		return "", 0, false
	}
	return repo, int(number), true
}

// BySourceRef reads the row project holds under sourceRef, and false where it holds none.
func BySourceRef(ctx context.Context, q delivery.Querier, project, sourceRef string) (Event, bool, error) {
	event, err := bySourceRef(ctx, q, project, sourceRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("read production event %s of %s: %w", sourceRef, project, err)
	}
	return event, true, nil
}

// bySourceRef reads the row project holds under sourceRef, pgx.ErrNoRows where it holds none.
func bySourceRef(ctx context.Context, q delivery.Querier, project, sourceRef string) (Event, error) {
	return scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where project_key = $1 and source_ref = $2`, project, sourceRef))
}
