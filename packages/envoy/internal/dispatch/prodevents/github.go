package prodevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The GitHub adapter's writes and the signals' reads (dispatch://INFRA-978, R6 and R7). A GitHub
// deploy row follows its run: the adapter writes it where the repository's rule calls the run a
// deploy, moves it in place where the run moved (a re-run's times, a pull request it ships), and
// drops it where the rule no longer calls the run a deploy (prodevents/signals). Every write is a
// transaction of its own that takes the delivery change log's lock nowhere; its change rows wait in
// the outbox, and the write drains them after its commit.

// StatusSucceeded is the status of a GitHub deploy row: OTel's deployment.status of a deploy that
// reached production, which the production job's success under the rule says it did.
const StatusSucceeded = "succeeded"

// StatusDropped is the status of a GitHub deploy row whose run the repository's rule no longer
// calls a deploy but which a failure blames, so it stays: its failures are rejected with the
// reason its note holds, and no measure counts it as a deploy.
const StatusDropped = "dropped"

// droppedDeploy is the SQL condition of a dropped GitHub deploy row (StatusDropped), which no
// signal blames: the reads of a signal's deploy leave it out.
const droppedDeploy = `(source = 'github' and status = 'dropped')`

// UpsertGitHubDeploy stores the GitHub adapter's deploy row event, or moves the row its project
// already holds under its source_ref to event's fields (a re-run's times, the pull requests and
// commits it ships, its url, a dropped row the rule calls a deploy again), and answers the row it
// wrote and true. A row whose fields would not move is not written, so a second write of one run
// writes nothing, logs no change, and answers no row and false.
func UpsertGitHubDeploy(ctx context.Context, pool *store.Pool, event Event) (Event, bool, error) {
	if event.Kind != KindDeploy || event.Source != SourceGitHub {
		return Event{}, false, refuse(CodeInput, "the GitHub adapter writes deploys of source github alone")
	}
	actor, environment, err := prepared(event)
	if err != nil {
		return Event{}, false, err
	}
	stored, err := scan(pool.QueryRow(ctx, `
		insert into production_events (id, project_key, kind, environment, status, source, source_ref,
			started_at, finished_at, repo, commits, pull_requests, url, actor, note)
		values ($1, $2, 'deploy', $3, $4, 'github', $5, $6, $7, $8, $9, $10, $11, $12, $13)
		on conflict (project_key, source_ref) do update set
			status = excluded.status, started_at = excluded.started_at, finished_at = excluded.finished_at,
			repo = excluded.repo, commits = excluded.commits, pull_requests = excluded.pull_requests,
			url = excluded.url, note = excluded.note
		where production_events.kind = 'deploy' and production_events.source = 'github'
			and (production_events.status, production_events.started_at, production_events.finished_at,
				production_events.repo, production_events.commits, production_events.pull_requests,
				production_events.url, production_events.note)
			is distinct from (excluded.status, excluded.started_at, excluded.finished_at, excluded.repo,
				excluded.commits, excluded.pull_requests, excluded.url, excluded.note)
		returning `+columns,
		uuid.Must(uuid.NewV7()).String(), event.ProjectKey, environment, event.Status, event.SourceRef, event.StartedAt, event.FinishedAt, event.Repo,
		nonNil(event.Commits), nonNil(event.PullRequests), event.URL, actor, event.Note))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("write production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a GitHub deploy row write")
	return stored, true, nil
}

// DropGitHubDeploy takes the GitHub deploy row id of project out of the record, because the rule of
// its repository no longer calls its run a deploy, and answers whether it deleted the row. A row no
// failure blames and no restore names is deleted. One that a failure blames, or that a restore
// names as its restoring deploy, stays (the foreign keys hold it) with status StatusDropped and
// reason as its note, and each counted failure that blames it is rejected with reason, by actor at
// at. The row is locked first, so a failure written against it meanwhile waits for this write and
// then finds it as it left it. A row already dropped with this reason writes nothing.
func DropGitHubDeploy(ctx context.Context, pool *store.Pool, project, id, reason string, actor model.Actor, at time.Time) (bool, error) {
	by, err := json.Marshal(actor.Recorded())
	if err != nil {
		return false, fmt.Errorf("encode the writer of the drop of production event %s: %w", id, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the drop of production event %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	var kind, source string
	if err := tx.QueryRow(ctx, `select kind, source from production_events where project_key = $1 and id = $2 for update`,
		project, id).Scan(&kind, &source); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read production event %s: %w", id, err)
	}
	if kind != KindDeploy || source != SourceGitHub {
		return false, refuse(CodeInput, "row %s is no GitHub deploy, which the adapter alone drops", id)
	}
	var named bool
	if err := tx.QueryRow(ctx, `select exists (select from production_events where deploy_id = $1 or restored_by_deploy = $1)`,
		id).Scan(&named); err != nil {
		return false, fmt.Errorf("read the failures of production event %s: %w", id, err)
	}
	if !named {
		if _, err := tx.Exec(ctx, `delete from production_events where id = $1`, id); err != nil {
			return false, fmt.Errorf("delete production event %s: %w", id, err)
		}
	} else {
		rejected, err := tx.Exec(ctx, `
			update production_events set state = 'rejected', state_reason = $2, state_by = $3, state_at = $4
			where kind = 'failure' and deploy_id = $1 and state = 'counted'
		`, id, reason, by, at)
		if err != nil {
			return false, fmt.Errorf("reject the failures of production event %s: %w", id, err)
		}
		dropped, err := tx.Exec(ctx, `update production_events set status = $2, note = $3
			where id = $1 and (status, note) is distinct from ($2, $3)`, id, StatusDropped, reason)
		if err != nil {
			return false, fmt.Errorf("drop production event %s: %w", id, err)
		}
		if rejected.RowsAffected() == 0 && dropped.RowsAffected() == 0 {
			return false, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the drop of production event %s: %w", id, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a GitHub deploy row drop")
	return !named, nil
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

// DeployForPullRequest is the deploy row, of any project, that names pullRequest (owner/repo#n, in
// any case) among the pull requests it shipped, the earliest to reach production where several do,
// and false where none does. A dropped GitHub deploy is none.
func DeployForPullRequest(ctx context.Context, q delivery.Querier, pullRequest string) (Event, bool, error) {
	event, err := scan(q.QueryRow(ctx, `select `+columns+` from production_events
		where kind = 'deploy' and not `+droppedDeploy+`
		  and exists (select from unnest(pull_requests) shipped where lower(shipped) = lower($1))
		order by finished_at, id limit 1`, pullRequest))
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("read the deploy of %s: %w", pullRequest, err)
	}
	return event, true, nil
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

// HeldSourceRefs is the refs among refs that project holds a row under, each read through the
// unique (project_key, source_ref) index.
func HeldSourceRefs(ctx context.Context, q delivery.Querier, project string, refs []string) (map[string]bool, error) {
	held := map[string]bool{}
	if len(refs) == 0 {
		return held, nil
	}
	rows, err := q.Query(ctx, `select source_ref from production_events where project_key = $1 and source_ref = any($2)`,
		project, refs)
	if err != nil {
		return nil, fmt.Errorf("read which of %d source refs %s holds: %w", len(refs), project, err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read which of %d source refs %s holds: %w", len(refs), project, err)
	}
	for _, ref := range stored {
		held[ref] = true
	}
	return held, nil
}
