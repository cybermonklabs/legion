package signals

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The GitHub adapter (dispatch://INFRA-978, R6): the deploy row of each run of a deploy repository
// that the repository's rule calls a deploy (delivery.DeployRowsIn), written under the repository's
// owner (delivery.DeployRules) after the write that made the run a deploy has committed: the
// intake's workflow write, the reconcile's run write and its backfills, and the job intake's job
// write each log a change of the run (delivery_changes, kinds run and run_job), and a pull request
// write logs one of the pull request it ships (kind pull_request), which this feed reads after them
// (passDeploys). None of those writes touches the record, so no transaction that takes the change
// log's lock locks a production_events row.

// runHorizon is how far past a pass's clock a derive reads runs: no run starts later than now, by
// GitHub's clock or this one, so a day is a bound that skips none.
const runHorizon = 24 * time.Hour

// deriveChunk is the span of run starts one read of a whole derive covers (deriveRepository), so a
// repository's history is read a month at a time rather than in one read the window reads bound.
const deriveChunk = 28 * 24 * time.Hour

// passDeploys is one pass of the deploys feed: every repository whose recorded rule is not its
// owner's rule now (or that has none, as on the first pass of this release, or after a lost cursor)
// derives every GitHub deploy row of it again (deriveMovedRules); then each page of the change log
// after the cursor moves the deploy rows of the runs and pull requests it names, each repository's
// from the earliest run start the changes can move (deriveChanges); then each revert whose deploy
// has its row makes its failure (revertFailures). Each derive matches the deploy rows it derived
// with the failed after-merge checks that came before them (matchEarlyChecks).
//
// The first page is read before the whole derives, so each change it holds committed before the
// derive read what that change wrote, and is skipped for a repository derived whole. A whole derive
// records its rule, and a page moves the cursor, only once its deploy rows and the check_fail rows
// they complete are written. A pass that fails before then (a deploy row committed, its check_fail
// not) leaves the rule unrecorded or the cursor where it stood, and the next pass derives the same
// rows again: their writes write nothing, and the match, which reads every row the derive calls a
// deploy and not only those a write moved, writes the check_fail then. A cursor lost
// (delivery.CursorLost) forgets every recorded rule, so every repository derives whole, and moves
// to the log's head.
func passDeploys(ctx context.Context, pool *store.Pool, now func() time.Time) error {
	stored, err := readCursor(ctx, pool, deploysCursor)
	if err != nil {
		return err
	}
	checked := now()
	position := delivery.ChangeCursor{Seq: stored.position, At: stored.at, ReadAt: stored.stamped}
	lost := !stored.held
	if stored.held {
		if lost, err = delivery.CursorLost(ctx, pool, position, checked); err != nil {
			return fmt.Errorf("judge the deploys cursor: %w", err)
		}
	}
	if lost {
		if _, err := pool.Exec(ctx, `delete from production_deploy_rules`); err != nil {
			return fmt.Errorf("forget the deploy rules: %w", err)
		}
		if position, err = delivery.ChangesHead(ctx, pool); err != nil {
			return fmt.Errorf("read the head of the delivery change log: %w", err)
		}
		position.ReadAt = checked
	}
	rules, err := delivery.ReadSourceRules(ctx, pool)
	if err != nil {
		return err
	}
	derived := map[string]bool{}
	for first := true; ; first = false {
		changes, err := delivery.ChangesSince(ctx, pool, position.Seq, page)
		if err != nil {
			return fmt.Errorf("read the delivery changes after %d: %w", position.Seq, err)
		}
		if first {
			if err := deriveMovedRules(ctx, pool, rules, checked, derived); err != nil {
				return err
			}
		}
		if err := deriveChanges(ctx, pool, rules, changes, derived, checked); err != nil {
			return err
		}
		position = position.Through(changes, page, checked)
		if err := moveCursor(ctx, pool, deploysCursor, position.Seq, position.At, position.ReadAt); err != nil {
			return err
		}
		if len(changes) < page {
			break
		}
	}
	for _, rule := range rules.All() {
		if err := revertFailures(ctx, pool, rule); err != nil {
			return err
		}
	}
	return nil
}

// deriveMovedRules derives whole every repository whose owner's rule is not the one its rows were
// recorded as derived under (production_deploy_rules), matches the rows with the failed after-merge
// checks that came before them (matchEarlyChecks), records the rule once its rows and the
// check_fail rows they complete are written, and forgets the record of a repository no rule reads
// any more. A repository no rule reads keeps its rows: nothing calls its runs a deploy or no deploy
// until a project's source reads it again, which derives it then. It adds each repository it
// derived to derived.
func deriveMovedRules(ctx context.Context, pool *store.Pool, rules *delivery.SourceRules, at time.Time, derived map[string]bool) error {
	records, err := readDeployRules(ctx, pool)
	if err != nil {
		return err
	}
	for _, rule := range rules.All() {
		repo := strings.ToLower(rule.DeployRepo)
		if record, held := records[repo]; held && record.is(rule) {
			continue
		}
		rows, err := deriveRepository(ctx, pool, rule, at)
		if err != nil {
			return err
		}
		if err := matchEarlyChecks(ctx, pool, rows); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			insert into production_deploy_rules (repo, project_key, production_job_names, production_apply_step_name, derived_at)
			values ($1, $2, $3, $4, $5)
			on conflict (repo) do update set project_key = excluded.project_key,
				production_job_names = excluded.production_job_names,
				production_apply_step_name = excluded.production_apply_step_name, derived_at = excluded.derived_at
		`, repo, rule.Project, rule.ProductionJobNames, rule.ProductionApplyStepName, at); err != nil {
			return fmt.Errorf("record the deploy rule of %s: %w", repo, err)
		}
		derived[repo] = true
	}
	for repo := range records {
		if _, read := rules.For(repo); read {
			continue
		}
		if _, err := pool.Exec(ctx, `delete from production_deploy_rules where repo = $1`, repo); err != nil {
			return fmt.Errorf("forget the deploy rule of %s: %w", repo, err)
		}
	}
	return nil
}

// deployRule is one row of production_deploy_rules: the owner and the rule a repository's rows
// stand derived under.
type deployRule struct {
	project   string
	jobNames  []string
	applyStep *string
}

// is reports whether rule is the rule the record holds.
func (r deployRule) is(rule delivery.DeliverySettings) bool {
	sameStep := (r.applyStep == nil) == (rule.ProductionApplyStepName == nil) &&
		(r.applyStep == nil || *r.applyStep == *rule.ProductionApplyStepName)
	return r.project == rule.Project && slices.Equal(r.jobNames, rule.ProductionJobNames) && sameStep
}

// readDeployRules reads production_deploy_rules by repository.
func readDeployRules(ctx context.Context, q delivery.Querier) (map[string]deployRule, error) {
	rows, err := q.Query(ctx, `select repo, project_key, production_job_names, production_apply_step_name from production_deploy_rules`)
	if err != nil {
		return nil, fmt.Errorf("read the deploy rules: %w", err)
	}
	defer rows.Close()
	records := map[string]deployRule{}
	for rows.Next() {
		var repo string
		var record deployRule
		if err := rows.Scan(&repo, &record.project, &record.jobNames, &record.applyStep); err != nil {
			return nil, fmt.Errorf("read the deploy rules: %w", err)
		}
		records[repo] = record
	}
	return records, rows.Err()
}

// deriveChanges moves the deploy rows a page of the change log names: for each repository a rule
// reads and this pass did not derive whole, the rows of its deploy runs the page's runs and jobs
// and pull requests can move (deriveChanged). Then it matches every row those derives call a
// deploy, moved or not, with the failed after-merge checks that came before them, in one read for
// the page (matchEarlyChecks).
func deriveChanges(ctx context.Context, pool *store.Pool, rules *delivery.SourceRules, changes []delivery.Change, derived map[string]bool, at time.Time) error {
	type touched struct {
		rule   delivery.DeliverySettings
		runs   []int64
		merges []time.Time
	}
	var order []string
	byRepo := map[string]*touched{}
	for _, change := range changes {
		rule, read := rules.For(change.Repo)
		repo := strings.ToLower(change.Repo)
		if !read || derived[repo] {
			continue
		}
		t := byRepo[repo]
		if t == nil {
			t = &touched{rule: rule}
			byRepo[repo] = t
			order = append(order, repo)
		}
		switch change.Kind {
		case delivery.ChangeKindRun, delivery.ChangeKindRunJob:
			if change.RunKind != string(delivery.DeliveryRunKindDeploy) {
				continue
			}
			runID, err := strconv.ParseInt(change.Ref, 10, 64)
			if err != nil {
				return fmt.Errorf("delivery change %d names run %q: %w", change.Seq, change.Ref, err)
			}
			if !slices.Contains(t.runs, runID) {
				t.runs = append(t.runs, runID)
			}
		case delivery.ChangeKindPullRequest:
			for _, merged := range []*time.Time{change.TOld, change.TNew} {
				if merged != nil {
					t.merges = append(t.merges, *merged)
				}
			}
		}
	}
	var held []delivery.DeployRow
	for _, repo := range order {
		t := byRepo[repo]
		if len(t.runs) == 0 && len(t.merges) == 0 {
			continue
		}
		rows, err := deriveChanged(ctx, pool, t.rule, t.runs, t.merges, at)
		if err != nil {
			return err
		}
		held = append(held, rows...)
	}
	return matchEarlyChecks(ctx, pool, held)
}

// deriveRepository derives every GitHub deploy row of rule's repository from its stored runs, a
// chunk of run starts at a time (deriveChunk), and writes them under rule's project: each deploy
// gains or keeps its row, moved in place where it moved, and every other GitHub deploy row of the
// repository, of any project, is dropped (apply). It answers every row it derived, each a deploy
// the rule calls one, whether its write moved it or wrote nothing.
func deriveRepository(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings, at time.Time) ([]delivery.DeployRow, error) {
	first, held, err := delivery.FirstDeployRunStart(ctx, pool, rule.DeployRepo)
	if err != nil {
		return nil, err
	}
	var desired []delivery.DeployRow
	if held {
		end := at.Add(runHorizon)
		for from := first; from.Before(end); from = from.Add(deriveChunk) {
			to := from.Add(deriveChunk)
			if to.After(end) {
				to = end
			}
			rows, _, err := delivery.DeployRowsIn(ctx, pool, rule, from, to)
			if err != nil {
				return nil, err
			}
			desired = append(desired, rows...)
		}
	}
	stored, err := prodevents.GitHubDeploys(ctx, pool, rule.DeployRepo, nil)
	if err != nil {
		return nil, err
	}
	if err := apply(ctx, pool, rule, desired, stored, at); err != nil {
		return nil, err
	}
	return desired, nil
}

// deriveChanged derives the GitHub deploy rows of rule's repository that a write of its deploy
// runs runIDs, or of pull requests of it merged at merges, can have moved: the rows of every deploy
// run started from the earliest start those writes can move (delivery.DeployRowsMovedSince), and the
// stored rows of those runs and of runIDs, which apply compares. It answers every row it derived,
// each a deploy the rule calls one, whether its write moved it or wrote nothing.
func deriveChanged(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings, runIDs []int64, merges []time.Time, at time.Time) ([]delivery.DeployRow, error) {
	since, moved, err := delivery.DeployRowsMovedSince(ctx, pool, rule, runIDs, merges)
	if err != nil {
		return nil, err
	}
	var desired []delivery.DeployRow
	considered := slices.Clone(runIDs)
	if moved {
		rows, started, err := delivery.DeployRowsIn(ctx, pool, rule, since, at.Add(runHorizon))
		if err != nil {
			return nil, err
		}
		desired = rows
		for _, runID := range started {
			if !slices.Contains(considered, runID) {
				considered = append(considered, runID)
			}
		}
	}
	if len(considered) == 0 {
		return nil, nil
	}
	stored, err := prodevents.GitHubDeploys(ctx, pool, rule.DeployRepo, considered)
	if err != nil {
		return nil, err
	}
	if err := apply(ctx, pool, rule, desired, stored, at); err != nil {
		return nil, err
	}
	return desired, nil
}

// apply writes desired, each deploy row of rule's repository the rule calls a deploy, under rule's
// project (prodevents.UpsertGitHubDeploy: a row that would not move is not written), and drops each
// row of stored that is not one of them under that project (prodevents.DropGitHubDeploy: deleted,
// or kept with its failures rejected where a failure blames it), naming the rule in the reason. A
// row dropped already stays as it is.
func apply(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings, desired []delivery.DeployRow, stored []prodevents.Event, at time.Time) error {
	wanted := make(map[string]bool, len(desired))
	for _, row := range desired {
		event := deployEvent(rule, row)
		wanted[event.SourceRef] = true
		if _, _, err := prodevents.UpsertGitHubDeploy(ctx, pool, event); err != nil {
			return err
		}
	}
	for _, old := range stored {
		if wanted[old.SourceRef] && old.ProjectKey == rule.Project || old.Status == prodevents.StatusDropped {
			continue
		}
		if _, err := prodevents.DropGitHubDeploy(ctx, pool, old.ProjectKey, old.ID, dropReason(rule, old, at), Actor, at); err != nil {
			return err
		}
	}
	return nil
}

// deployEvent is row as the GitHub adapter writes it, under rule's project: started at the run's
// creation, finished at its production job's completion, keyed by its run (github:<run id>).
func deployEvent(rule delivery.DeliverySettings, row delivery.DeployRow) prodevents.Event {
	return prodevents.Event{
		ProjectKey: rule.Project, Kind: prodevents.KindDeploy, Status: prodevents.StatusSucceeded,
		Source: prodevents.SourceGitHub, SourceRef: prodevents.GitHubSourceRef(row.Run.RunID),
		StartedAt: new(row.Run.Created().UTC()), FinishedAt: new(row.FinishedAt.UTC()), Repo: rule.DeployRepo,
		Commits: row.Commits, PullRequests: row.PullRequests, URL: row.Run.URL, Actor: Actor,
	}
}

// dropReason is why old is no deploy: the rule of its repository, as rule states it on at's day,
// does not call its run a deploy of old's project. It is the reason each failure on old is rejected
// with, so it names the rule.
func dropReason(rule delivery.DeliverySettings, old prodevents.Event, at time.Time) string {
	step := "no apply step"
	if rule.ProductionApplyStepName != nil {
		step = fmt.Sprintf("apply step %q", *rule.ProductionApplyStepName)
	}
	return fmt.Sprintf("the deploy rule of %s changed: on %s, project %s's rule (production jobs %s, %s) does not call %s (%s) a deploy of project %s",
		rule.DeployRepo, at.UTC().Format(time.DateOnly), rule.Project, strings.Join(rule.ProductionJobNames, ", "), step,
		old.SourceRef, old.URL, old.ProjectKey)
}
