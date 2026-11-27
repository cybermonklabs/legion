package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const deployColumns = `id, repo, program, commit_sha, deployed_at, source, run_id, run_attempt, url, recorded_at, recorded_by`

func scanDeploy(row pgx.Row) (model.Deploy, error) {
	var deploy model.Deploy
	var actor []byte
	if err := row.Scan(&deploy.ID, &deploy.Repo, &deploy.Program, &deploy.Commit, &deploy.DeployedAt,
		&deploy.Source, &deploy.RunID, &deploy.RunAttempt, &deploy.URL, &deploy.RecordedAt, &actor); err != nil {
		return model.Deploy{}, err
	}
	if err := json.Unmarshal(actor, &deploy.RecordedBy); err != nil {
		return model.Deploy{}, err
	}
	return deploy, nil
}

// CurrentDeploys reads each program's newest deploy once, in program order.
func CurrentDeploys(ctx context.Context, q Querier, repo string) ([]model.Deploy, error) {
	rows, err := q.Query(ctx, `select distinct on (program) `+deployColumns+` from deploys
		where repo = $1 order by program, deployed_at desc, id desc`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deploys := []model.Deploy{}
	for rows.Next() {
		deploy, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		deploys = append(deploys, deploy)
	}
	return deploys, rows.Err()
}

func currentDeploy(ctx context.Context, q Querier, repo, program string) (model.Deploy, error) {
	deploy, err := scanDeploy(q.QueryRow(ctx, `select `+deployColumns+` from deploys
		where repo = $1 and program = $2 order by deployed_at desc, id desc limit 1`, repo, program))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Deploy{}, nil
	}
	return deploy, err
}

// DeployNotNewest names the current row a late route post must not replace.
type DeployNotNewest struct{ Current model.Deploy }

func (e *DeployNotNewest) Error() string {
	return fmt.Sprintf("the current deploy is %s at %s", e.Current.Commit, e.Current.DeployedAt.Format(time.RFC3339Nano))
}

func lockDeployRepository(ctx context.Context, tx pgx.Tx, repo string) error {
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('deploy:' || $1, 0))`, repo)
	return err
}

// liveCheckIssues reads stored result attributes, never a document. Both an open recording
// issue and an open check owner are candidates, only in this deploy's resolved repository.
func liveCheckIssues(ctx context.Context, q Querier, repo, program string) ([]string, error) {
	rows, err := q.Query(ctx, `select distinct candidate.issue_key
		from check_results r
		cross join lateral (values (r.issue_key), (r.check_issue_key)) candidate(issue_key)
		join issues i on i.key = candidate.issue_key
		where i.closed_at is null and r.check_subject in ('production', 'graph')
			and (r.check_program = $1 or r.check_program = '')
			and not exists (select 1 from check_result_retractions x where x.result_id = r.id)
		order by candidate.issue_key`, program)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, issue)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	issues := candidates[:0]
	for _, issue := range candidates {
		resolved, err := refs.IssueRepository(ctx, q, issue)
		if err != nil {
			return nil, err
		}
		if resolved != nil && strings.EqualFold(*resolved, repo) {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func lockDeployIssues(ctx context.Context, tx pgx.Tx, broker *events.Broker, repo, program string) ([]model.Event, error) {
	issues, err := liveCheckIssues(ctx, tx, repo, program)
	if err != nil {
		return nil, err
	}
	owners := make([]model.Event, len(issues))
	for index := range issues {
		owners[index].IssueKey = &issues[index]
	}
	if err := broker.LockOwners(ctx, tx, owners...); err != nil {
		return nil, err
	}
	return owners, nil
}

func appendDeployEvents(ctx context.Context, tx pgx.Tx, broker *events.Broker, owners []model.Event, deploy model.Deploy) ([]model.Event, error) {
	appended := make([]model.Event, 0, len(owners))
	for _, owner := range owners {
		owner.Type, owner.Actor = "deploy.recorded", deploy.RecordedBy
		owner.Payload = struct {
			Repo       string    `json:"repo"`
			Program    string    `json:"program"`
			Commit     string    `json:"commit"`
			DeployedAt time.Time `json:"deployed_at"`
			Source     string    `json:"source"`
		}{deploy.Repo, deploy.Program, deploy.Commit, deploy.DeployedAt, deploy.Source}
		event, err := broker.Append(ctx, tx, owner)
		if err != nil {
			return nil, err
		}
		appended = append(appended, event)
	}
	return appended, nil
}

// RecordDeploy serializes the current-row decision and its events under the repository lock.
// A repeat of the current build changes nothing, even when the caller supplies a later time.
func RecordDeploy(ctx context.Context, pool *store.Pool, broker *events.Broker, deploy model.Deploy) (model.Deploy, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return model.Deploy{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockDeployRepository(ctx, tx, deploy.Repo); err != nil {
		return model.Deploy{}, false, err
	}
	current, err := currentDeploy(ctx, tx, deploy.Repo, deploy.Program)
	if err != nil {
		return model.Deploy{}, false, err
	}
	if current.ID != 0 && current.Commit == deploy.Commit {
		return current, false, nil
	}
	if current.ID != 0 && deploy.DeployedAt.Before(current.DeployedAt) {
		return model.Deploy{}, false, &DeployNotNewest{Current: current}
	}
	owners, err := lockDeployIssues(ctx, tx, broker, deploy.Repo, deploy.Program)
	if err != nil {
		return model.Deploy{}, false, err
	}
	actor, err := json.Marshal(deploy.RecordedBy)
	if err != nil {
		return model.Deploy{}, false, err
	}
	deploy, err = scanDeploy(tx.QueryRow(ctx, `insert into deploys (repo, program, commit_sha, deployed_at, source, url, recorded_by)
		values ($1, $2, $3, $4, 'route', $5, $6) returning `+deployColumns,
		deploy.Repo, deploy.Program, deploy.Commit, deploy.DeployedAt, deploy.URL, actor))
	if err != nil {
		return model.Deploy{}, false, err
	}
	appended, err := appendDeployEvents(ctx, tx, broker, owners, deploy)
	if err != nil {
		return model.Deploy{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Deploy{}, false, err
	}
	for _, event := range appended {
		broker.Publish(event)
	}
	return deploy, true, nil
}

// RecordProductionDeploys observes each successful production job in completion order. The
// source completion is stored separately from deployed_at: a rerun of the current build must
// be remembered without starting a new interval for its existing results.
func RecordProductionDeploys(ctx context.Context, pool *store.Pool, settings DeliverySettings, broker *events.Broker) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockDeployRepository(ctx, tx, settings.DeployRepo); err != nil {
		return 0, err
	}
	var boot bool
	if err := tx.QueryRow(ctx, `select not exists(select 1 from deploys where repo = $1 and source = 'delivery_run')`, settings.DeployRepo).Scan(&boot); err != nil {
		return 0, err
	}
	production := settings.Production()
	// Production.Job selects the earliest successful production job, breaking ties by name;
	// Production.Conclusion excludes a configured apply step that did not succeed (model.go).
	rows, err := tx.Query(ctx, `select r.run_id, r.head_sha, r.run_attempt, r.url, job.completed_at
		from delivery_runs r
		join lateral (select j.completed_at from delivery_run_jobs j
			where j.repo = r.repo and j.run_id = r.run_id and j.name = any($2)
				and j.conclusion = 'success' and j.completed_at is not null
				and ($3::text is null or j.apply_step is distinct from $3 or j.apply_step_conclusion = 'success')
			order by j.completed_at, j.name limit 1) job on true
		left join deploys d on d.source = 'delivery_run' and d.run_id = r.run_id
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main'
			and (d.id is null or (d.commit_sha, d.run_completed_at, d.run_attempt, d.url)
				is distinct from (r.head_sha, job.completed_at, r.run_attempt, r.url))
		order by job.completed_at, r.run_id`, settings.DeployRepo, production.JobNames, production.ApplyStep)
	if err != nil {
		return 0, err
	}
	var candidates []model.Deploy
	for rows.Next() {
		deploy := model.Deploy{Repo: settings.DeployRepo, Program: "delivery", Source: "delivery_run", RecordedBy: DeployedEventActor}
		if err := rows.Scan(&deploy.RunID, &deploy.Commit, &deploy.RunAttempt, &deploy.URL, &deploy.DeployedAt); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, deploy)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	var owners []model.Event
	if !boot {
		owners, err = lockDeployIssues(ctx, tx, broker, settings.DeployRepo, "delivery")
		if err != nil {
			return 0, err
		}
	}
	actor, err := json.Marshal(DeployedEventActor)
	if err != nil {
		return 0, err
	}
	var appended []model.Event
	for _, deploy := range candidates {
		current, err := currentDeploy(ctx, tx, deploy.Repo, deploy.Program)
		if err != nil {
			return 0, err
		}
		completed := deploy.DeployedAt
		same := current.ID != 0 && current.Commit == deploy.Commit
		if same {
			deploy.DeployedAt = current.DeployedAt
		}
		deploy, err = scanDeploy(tx.QueryRow(ctx, `insert into deploys
			(repo, program, commit_sha, deployed_at, source, run_id, run_attempt, url, recorded_by, run_completed_at)
			values ($1, 'delivery', $2, $3, 'delivery_run', $4, $5, $6, $7, $8)
			on conflict (source, run_id) where run_id is not null do update set
				commit_sha = excluded.commit_sha, deployed_at = excluded.deployed_at,
				run_attempt = excluded.run_attempt, url = excluded.url, run_completed_at = excluded.run_completed_at
			returning `+deployColumns, deploy.Repo, deploy.Commit, deploy.DeployedAt, deploy.RunID, deploy.RunAttempt, deploy.URL, actor, completed))
		if err != nil {
			return 0, err
		}
		if !boot && !same {
			events, err := appendDeployEvents(ctx, tx, broker, owners, deploy)
			if err != nil {
				return 0, err
			}
			appended = append(appended, events...)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, event := range appended {
		broker.Publish(event)
	}
	return len(candidates), nil
}
