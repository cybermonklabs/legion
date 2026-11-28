package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The check_fail signal (dispatch://INFRA-978, R7 and D5): a check_results row with verdict fail
// on a check whose when is after_merge (the result.recorded event's When) is a failure of the
// deploy that first shipped its commit (prodevents.DeployForCommit, which leaves out a deploy
// another project posted), written under the project of the issue the result was recorded on,
// naming that issue's pull requests, keyed by the check and the deploy (CheckSourceRef), its actor
// the result's and its detected time the result's own. It is written only where that deploy counts
// for that project (deployCountsForRow), asked in the write's transaction. A result whose deploy
// counts for other projects alone writes no row and stays refused: its refusal commits in that
// transaction (check_fail_refusals, migration 20261128030142), and no later write writes its row,
// not even a derive of deploy rows once a source move, a rule change or a default-repository move
// has made that deploy count for the result's project
// (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). A result whose commit no
// deploy row holds yet is not refused, and makes its row when a deploy row that holds it lands
// (matchEarlyChecks); a result no deploy ever holds makes none. The result is bound to the row its
// first match wrote, or found under that key, in the transaction that writes it
// (check_fail_bindings, migration 20261127022136), and stays on it: a deploy row found later that
// shipped the commit earlier makes it no second row. A retraction of the result rejects the row it
// is bound to, with the retraction's reason and actor. A result's first match and its retraction's
// reject take the result's row in turn (lockResult), so neither runs inside the other.

// VerdictFail is the verdict of a failed check result (check_results.verdict).
const VerdictFail = "fail"

// IsCheckFail reports whether a result of verdict, of a check judged by when, is the check_fail
// signal's: a fail of an after_merge check. A retraction takes such a result back
// (checkResult.counts).
func IsCheckFail(verdict, when string) bool {
	return verdict == VerdictFail && when == model.CheckWhenAfterMerge
}

// checkResult is what the signal reads of one check_results row: its issue's project, its check,
// its commit, its verdict, the When its result.recorded event judged it by, its actor and time,
// whether a retraction stands against it, the check_fail row it is bound to, and whether it is
// refused.
type checkResult struct {
	id         string
	issueKey   string
	project    string
	artifactID string
	blockID    string
	commitSHA  string
	verdict    string
	when       string
	actor      model.Actor
	createdAt  time.Time
	retracted  bool
	// bound is the id of the check_fail row the result is bound to (check_fail_bindings), "" where
	// it is bound to none.
	bound string
	// refused is whether the deploy its first match blamed counted for other projects than its own
	// alone (check_fail_refusals): it makes no row and stands behind none, for good.
	refused bool
}

// checkResultSelect is checkResult's read, a whole select a caller adds its where clause to: the
// result, its issue's project, the When of the event it was recorded by (the event's own seq on the
// check's issue, check_results.event_seq), whether a retraction stands against it, its binding and
// its refusal.
const checkResultSelect = `
	select r.id::text, r.issue_key, i.project_key, r.check_artifact_id::text, r.check_block_id, r.commit_sha,
		r.verdict, coalesce(e.payload ->> 'when', ''), r.actor, r.created_at,
		exists (select from check_result_retractions x where x.result_id = r.id),
		coalesce((select b.failure_id::text from check_fail_bindings b where b.result_id = r.id), ''),
		exists (select from check_fail_refusals f where f.result_id = r.id)
	from check_results r
	join issues i on i.key = r.issue_key
	left join events e on e.issue_key = r.check_issue_key and e.seq = r.event_seq`

func scanCheckResult(row pgx.Row) (checkResult, error) {
	var result checkResult
	var actor []byte
	if err := row.Scan(&result.id, &result.issueKey, &result.project, &result.artifactID, &result.blockID, &result.commitSHA,
		&result.verdict, &result.when, &actor, &result.createdAt, &result.retracted, &result.bound, &result.refused); err != nil {
		return checkResult{}, err
	}
	if err := json.Unmarshal(actor, &result.actor); err != nil {
		return checkResult{}, fmt.Errorf("decode the actor of result %s: %w", result.id, err)
	}
	return result, nil
}

// readCheckResult reads the result id, and false where no row holds it.
func readCheckResult(ctx context.Context, q delivery.Querier, id string) (checkResult, bool, error) {
	result, err := scanCheckResult(q.QueryRow(ctx, checkResultSelect+` where r.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return checkResult{}, false, nil
	}
	if err != nil {
		return checkResult{}, false, fmt.Errorf("read check result %s: %w", id, err)
	}
	return result, true, nil
}

// counts reports whether the result is a check_fail signal: a fail of an after_merge check
// (IsCheckFail) that no retraction has taken back.
func (r checkResult) counts() bool {
	return IsCheckFail(r.verdict, r.when) && !r.retracted
}

// matched reports whether the result's first match is behind it: it is bound to a row, or it is
// refused. No later write of the result writes a row.
func (r checkResult) matched() bool {
	return r.bound != "" || r.refused
}

// CheckSourceRef is the source_ref of the check_fail row of a check blaming deploy:
// check:<document>/<block>@<deploy>, the check named by its Verification document's id
// (check_results.check_artifact_id) and its block id, since a block id names a check within one
// document alone.
func CheckSourceRef(artifactID, blockID, deployID string) string {
	return prodevents.CheckSourceRefPrefix + artifactID + "/" + blockID + "@" + deployID
}

// lockResult takes the row of result id in tx `for no key update`. A result's first match
// (RecordCheckFailure) and its retraction's reject (RejectRetractedCheck) take it before they read
// the result, so a reject waits for a first match in flight and then reads the binding it wrote,
// and a first match that waited reads the retraction committed before it. The level leaves a
// foreign key that names the result (`for key share`), a retraction's insert among them, unblocked.
func lockResult(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, `select 1 from check_results where id = $1::uuid for no key update`, id); err != nil {
		return fmt.Errorf("lock check result %s: %w", id, err)
	}
	return nil
}

// RecordCheckFailure writes the check_fail row of result id where it is one (a fail of an
// after_merge check, not retracted), a deploy row holds its commit and the result is neither bound
// to a row nor refused yet (checkResult.matched), and reports whether it wrote a row. The row blames
// the deploy that first shipped the commit, written only where that deploy counts for the result's
// project (deployCountsForRow); where another result of the check wrote that row already, under the
// same key, the result stands behind it and writes nothing. Either way the result is bound to the
// row in the transaction that writes the row, which takes the delivery change log's lock nowhere
// and drains after its commit: a repeat writes nothing, and a deploy row found later that shipped
// the commit earlier makes the result no second row. A result whose deploy counts for other
// projects alone is refused in the transaction that asked (refuseCheck), and no later call writes
// it. A result no deploy holds yet is logged with its id (unmatchedCheck) and stays bound to no
// row, and makes its row when a deploy row that holds its commit lands (matchEarlyChecks).
func RecordCheckFailure(ctx context.Context, pool *store.Pool, id string) (bool, error) {
	if result, found, err := readCheckResult(ctx, pool, id); err != nil || !found || !result.counts() || result.matched() {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the check_fail of result %s: %w", id, err)
	}
	defer tx.Rollback(ctx)
	if err := lockResult(ctx, tx, id); err != nil {
		return false, err
	}
	result, found, err := readCheckResult(ctx, tx, id)
	if err != nil || !found || !result.counts() || result.matched() {
		return false, err
	}
	deploy, held, err := prodevents.DeployForCommit(ctx, tx, result.project, result.commitSHA)
	if err != nil {
		return false, err
	}
	if !held {
		unmatchedCheck(result)
		return false, nil
	}
	pulls, err := issuePullRequests(ctx, tx, result.issueKey)
	if err != nil {
		return false, err
	}
	var refused bool
	row, created, err := prodevents.WriteIn(ctx, tx, prodevents.Event{
		ProjectKey: result.project, Kind: prodevents.KindFailure, Source: prodevents.SourceCheckResults,
		SourceRef: CheckSourceRef(result.artifactID, result.blockID, deploy.ID), PullRequests: pulls, Actor: result.actor,
		DeployID: new(deploy.ID), Signal: new(prodevents.SignalCheckFail), DetectedAt: new(result.createdAt.UTC()),
	}, countsForRow(result.project, deploy.ID, &refused))
	switch {
	case refused:
		return false, refuseCheck(ctx, tx, result, deploy.ID)
	case refusesBlame(err):
		// A deploy row gone since its lookup: the result is one whose commit no deploy row holds.
		unmatchedCheck(result)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("write the check_fail of result %s: %w", result.id, err)
	}
	if _, err := tx.Exec(ctx, `insert into check_fail_bindings (result_id, failure_id) values ($1::uuid, $2::uuid)`,
		result.id, row.ID); err != nil {
		return false, fmt.Errorf("bind result %s to its check_fail %s: %w", result.id, row.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the check_fail of result %s: %w", result.id, err)
	}
	if created {
		delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a check_fail write")
	}
	return created, nil
}

// refuseCheck records in tx, and commits, the refusal of result's check_fail on deploy, the deploy
// its first match blamed, which the write refused as counting for other projects than the result's
// alone (countsForRow, check_fail_refusals). The refused result makes no row, then or on any later
// write of it, whatever makes the deploy count for its project later
// (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706).
func refuseCheck(ctx context.Context, tx pgx.Tx, result checkResult, deploy string) error {
	if _, err := tx.Exec(ctx, `insert into check_fail_refusals (result_id, deploy_id) values ($1::uuid, $2::uuid)
		on conflict do nothing`, result.id, deploy); err != nil {
		return fmt.Errorf("refuse result %s the check_fail on deploy %s: %w", result.id, deploy, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the refusal of result %s's check_fail: %w", result.id, err)
	}
	slog.Info("dispatch: a failed after-merge check blames a deploy that counts for other projects alone; the result is refused and makes no failure, now or later",
		"result", result.id, "commit", result.commitSHA, "deploy", deploy)
	return nil
}

// unmatchedCheck logs result, a check_fail signal whose commit no deploy row holds yet: the deploy
// row that holds it, once it lands, makes its row (matchEarlyChecks).
func unmatchedCheck(result checkResult) {
	slog.Info("dispatch: a failed after-merge check names a commit no deploy row holds yet; its deploy row makes its failure",
		"result", result.id, "commit", result.commitSHA)
}

// countsForRow is the question an automatic signal's write of a row of project asks of the deploy
// it blames, blamed (prodevents.DeployCheck): whether that deploy counts for project
// (deployCountsForRow), asked in the write's transaction while the store holds the deploy's row, and
// refused prodevents.NoDeploy where it does not (refusesBlame), which it reports in refused, so the
// writer records that refusal in the same transaction (refuseCheck, writeSignal). A deploy row gone
// since its lookup, which the store refuses in the same words before it asks, leaves refused false.
// A restoring deploy the row names is asked nothing.
func countsForRow(project, blamed string, refused *bool) prodevents.DeployCheck {
	return func(ctx context.Context, tx pgx.Tx, deploy prodevents.Event) error {
		if deploy.ID != blamed {
			return nil
		}
		counts, err := deployCountsForRow(ctx, tx, project, deploy)
		if err == nil && !counts {
			*refused = true
			err = prodevents.NoDeploy(deploy.ID)
		}
		return err
	}
}

// refusesBlame reports whether err is the store's refusal of an automatic row's blamed deploy
// (prodevents.CodeNoDeploy): a deploy that counts for other projects than the row's alone
// (countsForRow), or a deploy row gone since its lookup. The signal writes no row of it, as it
// writes none where no deploy holds what it names; a check_fail, an incident_issue and a revert
// refused the first stay refused (refuseCheck, writeSignal).
func refusesBlame(err error) bool {
	var refused *prodevents.Error
	return errors.As(err, &refused) && refused.Code == prodevents.CodeNoDeploy
}

// writeSignal writes event, an automatic signal's failure blaming deploy blamed, in a transaction of
// its own that takes the delivery change log's lock nowhere and drains after its commit, as
// prodevents.Write does, and reports whether it wrote a row. It is written only where blamed counts
// for event's project (countsForRow); where blamed counts for other projects alone, refuse records
// that refusal in the same transaction, which commits it, so no later write of the signal blaming
// that deploy makes the row (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). A
// deploy row gone since its lookup writes nothing and records no refusal.
func writeSignal(ctx context.Context, pool *store.Pool, event prodevents.Event, blamed string, refuse func(context.Context, pgx.Tx) error) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the write of production event %s: %w", event.SourceRef, err)
	}
	defer tx.Rollback(ctx)
	var refused bool
	_, created, err := prodevents.WriteIn(ctx, tx, event, countsForRow(event.ProjectKey, blamed, &refused))
	switch {
	case refused:
		if err := refuse(ctx, tx); err != nil {
			return false, err
		}
	case refusesBlame(err):
		return false, nil
	case err != nil:
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit production event %s of %s: %w", event.SourceRef, event.ProjectKey, err)
	}
	if created {
		delivery.DrainAfterCommit(ctx, pool, "drain the change rows of an automatic signal's write")
	}
	return created, nil
}

// deployCountsForRow reads in tx whether deploy, the deploy an automatic signal's row of project
// blames, counts for project: the rule the failure measures count a deploy by (prodevents.DeployProjects)
// over the stored facts as the write's transaction reads them (storedDeployFacts), and not the
// refresher's published copy of them (eval_days_rules, eval_days_defaults), so a row whose deploy's
// rule is saved and not yet published is written, and counts once the refresher's next pass
// publishes it (dispatch://INFRA-978/comment/6da2d490-27d2-4d03-816a-a164a8d24dde).
func deployCountsForRow(ctx context.Context, tx pgx.Tx, project string, deploy prodevents.Event) (bool, error) {
	facts, err := storedDeployFacts(ctx, tx, deploy)
	if err != nil {
		return false, err
	}
	return slices.Contains(prodevents.DeployProjects(deploy, facts), project), nil
}

// storedDeployFacts reads in q the facts deploy is counted by (prodevents.DeployFacts) from what is
// stored: each project's default_repo, the project of the issue of each pull request deploy ships
// (a pull request no issue Dispatch holds attributes counts for none, so for the projects whose
// default repository is the deploy's), and, where it ships no pull request, the project whose
// deploy source reads its repository by the stored settings' rules (delivery.SourceRules). A posted
// and a dropped deploy read none of it.
func storedDeployFacts(ctx context.Context, q delivery.Querier, deploy prodevents.Event) (prodevents.DeployFacts, error) {
	facts := prodevents.DeployFacts{Defaults: map[string]string{}}
	if deploy.Source != prodevents.SourceGitHub || deploy.Dropped() {
		return facts, nil
	}
	var repos []string
	var numbers []int32
	for _, ref := range deploy.PullRequests {
		if repo, number, ok := prodevents.ParsePullRequest(ref); ok {
			repos, numbers = append(repos, repo), append(numbers, int32(number))
		}
	}
	if len(repos) == 0 {
		rules, err := delivery.ReadSourceRules(ctx, q)
		if err != nil {
			return prodevents.DeployFacts{}, err
		}
		if rule, read := rules.For(deploy.Repo); read {
			facts.Source = rule.Project
		}
	} else {
		rows, err := q.Query(ctx, `
			select coalesce(issue.project_key, '')
			from unnest($1::text[], $2::int[]) with ordinality as given(repo, number, at)
			left join delivery_pull_requests request on lower(request.repo) = lower(given.repo) and request.number = given.number
			left join issues issue on issue.key = request.issue_key
			order by given.at`, repos, numbers)
		if err != nil {
			return prodevents.DeployFacts{}, fmt.Errorf("read the projects of the pull requests deploy %s ships: %w", deploy.ID, err)
		}
		terminals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return prodevents.DeployFacts{}, fmt.Errorf("read the projects of the pull requests deploy %s ships: %w", deploy.ID, err)
		}
		for _, terminal := range terminals {
			facts.Pulls = append(facts.Pulls, prodevents.PullAttribution{Terminal: terminal})
		}
	}
	rows, err := q.Query(ctx, `select key, coalesce(default_repo, '') from projects`)
	if err != nil {
		return prodevents.DeployFacts{}, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	var project, repo string
	if _, err := pgx.ForEachRow(rows, []any{&project, &repo}, func() error {
		facts.Defaults[project] = repo
		return nil
	}); err != nil {
		return prodevents.DeployFacts{}, fmt.Errorf("read the projects' default repositories: %w", err)
	}
	return facts, nil
}

// issuePullRequests is the pull requests of issue (delivery_pull_requests attributed to it), as the
// record names them, oldest merge first.
func issuePullRequests(ctx context.Context, q delivery.Querier, issue string) ([]string, error) {
	rows, err := q.Query(ctx, `select repo, number from delivery_pull_requests where issue_key = $1
		order by merged_at nulls last, repo, number`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the pull requests of %s: %w", issue, err)
	}
	defer rows.Close()
	pulls := []string{}
	for rows.Next() {
		var repo string
		var number int
		if err := rows.Scan(&repo, &number); err != nil {
			return nil, fmt.Errorf("read the pull requests of %s: %w", issue, err)
		}
		pulls = append(pulls, delivery.PullRequestName(repo, number))
	}
	return pulls, rows.Err()
}

// RejectRetractedCheck rejects the check_fail row result id made or stands behind (rowOf), with the
// retraction's reason, actor and time, and reports whether it rejected one. A row another fail of
// the same check still stands behind (standsBehind) stays counted. A row rejected already, by a
// person or by another retraction, or one no result made, writes nothing, and so does a refused
// result, which made no row and stands behind none; a row the drop of its deploy rejected
// (rejectedByDrop) takes the retraction's reject over the drop's, so a later rule that calls that
// deploy a deploy again leaves it rejected. The decision and the reject commit together under the
// result's lock (lockResult) and the row's, in a transaction that takes the delivery change log's
// lock nowhere and drains after its commit.
func RejectRetractedCheck(ctx context.Context, pool *store.Pool, id, reason string, actor model.Actor, at time.Time) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the reject of result %s's check_fail: %w", id, err)
	}
	defer tx.Rollback(ctx)
	if err := lockResult(ctx, tx, id); err != nil {
		return false, err
	}
	result, found, err := readCheckResult(ctx, tx, id)
	if err != nil || !found || !IsCheckFail(result.verdict, result.when) || result.refused {
		return false, err
	}
	row, made, err := rowOf(ctx, tx, result)
	if err != nil || !made || row.State == prodevents.StateRejected && !rejectedByDrop(row) {
		return false, err
	}
	standing, err := standingFailsOf(ctx, tx, result)
	if err != nil {
		return false, err
	}
	for _, other := range standing {
		if behind, err := standsBehind(ctx, tx, other, row); err != nil || behind {
			return false, err
		}
	}
	if _, err := prodevents.SetStateIn(ctx, tx, result.project, row.ID, prodevents.StateChange{
		State: prodevents.StateRejected, Reason: reason, Actor: actor, At: at,
	}); err != nil {
		return false, fmt.Errorf("reject the check_fail of result %s: %w", result.id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit the reject of result %s's check_fail: %w", result.id, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of a check_fail reject")
	return true, nil
}

// rejectedByDrop reports whether row is rejected by the drop of the deploy it blames: its reject
// names loopActor, the drop's writer (prodevents.DropGitHubDeploy), and a later rule that calls
// that deploy a deploy again counts it again (prodevents.UpsertGitHubDeploy).
func rejectedByDrop(row prodevents.Event) bool {
	return row.State == prodevents.StateRejected && row.StateBy != nil && *row.StateBy == loopActor
}

// rowOf is the check_fail row result made or stands behind, locked in tx `for update` and read
// under that lock, and false where none is stored: the row it is bound to, or for a result bound to
// none, the row its first match would find, the one keyed by its check and the deploy that first
// shipped its commit. The lock is the one prodevents.SetStateIn takes, so a state written while
// this waited is the state read: a reject of another retraction, or a person's reject, is kept.
func rowOf(ctx context.Context, tx pgx.Tx, result checkResult) (prodevents.Event, bool, error) {
	id := result.bound
	if id == "" {
		deploy, held, err := prodevents.DeployForCommit(ctx, tx, result.project, result.commitSHA)
		if err != nil || !held {
			return prodevents.Event{}, false, err
		}
		row, made, err := prodevents.BySourceRef(ctx, tx, result.project, CheckSourceRef(result.artifactID, result.blockID, deploy.ID))
		if err != nil || !made {
			return prodevents.Event{}, false, err
		}
		id = row.ID
	}
	if _, err := tx.Exec(ctx, `select 1 from production_events where id = $1::uuid for update`, id); err != nil {
		return prodevents.Event{}, false, fmt.Errorf("lock the check_fail %s of result %s: %w", id, result.id, err)
	}
	row, err := prodevents.Get(ctx, tx, result.project, id)
	if errors.Is(err, prodevents.ErrNotFound) {
		return prodevents.Event{}, false, nil
	}
	return row, err == nil, err
}

// standsBehind reports whether other, another fail of row's check that still counts, stands
// behind row: it is bound to row, or it is bound to none, is not refused, and its first match would
// find row, the deploy that first shipped its commit being the one row blames.
func standsBehind(ctx context.Context, q delivery.Querier, other checkResult, row prodevents.Event) (bool, error) {
	if other.project != row.ProjectKey || other.refused {
		return false, nil
	}
	if other.bound != "" {
		return other.bound == row.ID, nil
	}
	blamed, held, err := prodevents.DeployForCommit(ctx, q, other.project, other.commitSHA)
	if err != nil || !held {
		return false, err
	}
	return blamed.ID == *row.DeployID, nil
}

// standingFailsOf is every other result of result's check that is a check_fail signal still
// (checkResult.counts).
func standingFailsOf(ctx context.Context, q delivery.Querier, result checkResult) ([]checkResult, error) {
	rows, err := q.Query(ctx, checkResultSelect+`
		where r.check_artifact_id = $1::uuid and r.check_block_id = $2 and r.id <> $3::uuid and r.verdict = $4`,
		result.artifactID, result.blockID, result.id, VerdictFail)
	if err != nil {
		return nil, fmt.Errorf("read the results of check %s: %w", result.blockID, err)
	}
	defer rows.Close()
	var standing []checkResult
	for rows.Next() {
		other, err := scanCheckResult(rows)
		if err != nil {
			return nil, fmt.Errorf("read the results of check %s: %w", result.blockID, err)
		}
		if other.counts() {
			standing = append(standing, other)
		}
	}
	return standing, rows.Err()
}

// matchEarlyChecks writes the check_fail rows deploys complete: every fail of an after_merge check,
// not retracted, bound to no row and not refused, whose commit one of them holds
// (RecordCheckFailure, which blames the deploy that first shipped the commit and binds the result
// to its row). So it binds only a result whose commit no deploy row held when the result was
// matched first: a result refused then, its deploy counting for other projects alone, stays
// refused once a source move, a rule change or a default-repository move makes that deploy count
// for the result's project (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706).
// deploys are every row a derive calls a deploy, whether its write moved the row or wrote nothing,
// so a derive run again after a pass that failed between a deploy row's write and its check_fail's
// matches that row all the same, and a result bound already, on this deploy or another, is left on
// its row. The results are read once for the commits of every row, then written oldest first, each
// once.
func matchEarlyChecks(ctx context.Context, pool *store.Pool, deploys []delivery.DeployRow) error {
	var commits []string
	for _, deploy := range deploys {
		commits = append(commits, deploy.Commits...)
	}
	if len(commits) == 0 {
		return nil
	}
	rows, err := pool.Query(ctx, `
		select r.id::text from check_results r
		join events e on e.issue_key = r.check_issue_key and e.seq = r.event_seq
		where r.verdict = $3 and r.commit_sha = any($1) and e.payload ->> 'when' = $2
		  and not exists (select from check_result_retractions x where x.result_id = r.id)
		  and not exists (select from check_fail_bindings b where b.result_id = r.id)
		  and not exists (select from check_fail_refusals f where f.result_id = r.id)
		order by r.created_at, r.id`, commits, model.CheckWhenAfterMerge, VerdictFail)
	if err != nil {
		return fmt.Errorf("read the failed checks of %d deploy rows: %w", len(deploys), err)
	}
	results, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the failed checks of %d deploy rows: %w", len(deploys), err)
	}
	for _, id := range results {
		if _, err := RecordCheckFailure(ctx, pool, id); err != nil {
			return err
		}
	}
	return nil
}

// feedEvent is one event an event feed reads (readEventPage): its id, its type, the issue it is on
// ("" for an event of no issue), its payload, its actor and when it was written.
type feedEvent struct {
	id        int64
	kind      string
	issueKey  string
	payload   []byte
	actor     model.Actor
	createdAt time.Time
}

// passChecks is one pass of the checks feed (passEventFeed): it writes the check_fail of each fail
// of an after_merge check (RecordCheckFailure) and rejects the row of each retracted result
// (RejectRetractedCheck).
func passChecks(ctx context.Context, pool *store.Pool, safe int64) error {
	return passEventFeed(ctx, pool, checksCursor, safe, []string{"result.recorded", "result.retracted"}, "",
		func(event feedEvent) error { return checkSignal(ctx, pool, event) })
}

// checkSignal is the write one result event makes.
func checkSignal(ctx context.Context, pool *store.Pool, event feedEvent) error {
	switch event.kind {
	case "result.recorded":
		var payload model.CheckResultEventPayload
		if err := json.Unmarshal(event.payload, &payload); err != nil {
			return fmt.Errorf("decode result event %d: %w", event.id, err)
		}
		if !IsCheckFail(payload.Verdict, payload.When) {
			return nil
		}
		_, err := RecordCheckFailure(ctx, pool, payload.ResultID)
		return err
	case "result.retracted":
		var payload model.CheckResultRetractedEventPayload
		if err := json.Unmarshal(event.payload, &payload); err != nil {
			return fmt.Errorf("decode retraction event %d: %w", event.id, err)
		}
		_, err := RejectRetractedCheck(ctx, pool, payload.ResultID, payload.Reason, event.actor, event.createdAt)
		return err
	}
	return nil
}

// passEventFeed is one pass of the event feed whose cursor is name: it reads the events of kinds
// after its cursor up to safe, the event log's safe horizon, that where keeps (readEventPage), page
// after page, runs handle on each, then moves its cursor over the page. A page shorter than a whole
// one read every event to safe, so the cursor moves to safe.
func passEventFeed(ctx context.Context, pool *store.Pool, name string, safe int64, kinds []string, where string, handle func(feedEvent) error) error {
	stored, err := readCursor(ctx, pool, name)
	if err != nil {
		return err
	}
	position := stored.position
	for position < safe {
		events, err := readEventPage(ctx, pool, position, safe, kinds, where)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := handle(event); err != nil {
				return err
			}
		}
		next := safe
		if len(events) == page {
			next = events[len(events)-1].id
		}
		if err := moveCursor(ctx, pool, name, next, nil, time.Now()); err != nil {
			return err
		}
		position = next
	}
	return nil
}

// readEventPage reads up to a page of the events of kinds after after and up to safe, in id order,
// those an extra condition on the alias event keeps where where is not empty.
func readEventPage(ctx context.Context, q delivery.Querier, after, safe int64, kinds []string, where string) ([]feedEvent, error) {
	condition := ""
	if where != "" {
		condition = " and (" + where + ")"
	}
	rows, err := q.Query(ctx, `
		select event.id, event.type, coalesce(event.issue_key, ''), event.payload, event.actor, event.created_at
		from events event
		where event.id > $1 and event.id <= $2 and event.type = any($3)`+condition+`
		order by event.id
		limit $4`, after, safe, kinds, page)
	if err != nil {
		return nil, fmt.Errorf("read the events after %d: %w", after, err)
	}
	defer rows.Close()
	var events []feedEvent
	for rows.Next() {
		var event feedEvent
		var actor []byte
		if err := rows.Scan(&event.id, &event.kind, &event.issueKey, &event.payload, &actor, &event.createdAt); err != nil {
			return nil, fmt.Errorf("read the events after %d: %w", after, err)
		}
		if err := json.Unmarshal(actor, &event.actor); err != nil {
			return nil, fmt.Errorf("decode the actor of event %d: %w", event.id, err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
