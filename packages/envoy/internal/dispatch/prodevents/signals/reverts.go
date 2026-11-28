package signals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The revert signal (dispatch://INFRA-978, R7 and D5): a merged revert of a deploy repository
// (delivery_reverts) is a failure of the deploy that shipped the pull request it reverts, which the
// revert's own deploy restores. It is written once the deploy that ships the revert has its deploy
// row, under the owner of the repository's source (delivery.DeployRules), keyed by the revert
// (RevertSourceRef), detected when the revert merged, restored at the revert's deploy's finish by
// that deploy. It is one row whatever project its repository's source moves to: it stays under the
// project it was written under, blaming the same deploy row, which moves with its run. A revert
// whose reverted pull request no deploy row holds makes none. One whose blamed deploy counts for
// other projects than the owner alone (deployCountsForRow) makes none and stays refused on that
// deploy: its refusal commits with a log line (revert_refusals, migration 20261128030142), so no
// later pass writes a row blaming that deploy or logs the revert again, not even once a source move,
// a rule change or a default-repository move has made that deploy count for the repository's owner
// (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). One whose blamed deploy
// finished after the revert's own deploy (prodevents.RestoresTooEarly), so that deploy restores none
// of its failures, or counts for no project, makes none for now and is not refused: it is held,
// logged once for each judgement it is held on (holdRevert), and every pass judges it again, so it
// makes its row once its own deploy finishes after the blamed one, as a GitHub re-run that moves
// that deploy's finish in place (prodevents.UpsertGitHubDeploy) or a rule change that makes a later
// run its ship run can make it do, or once the blamed deploy counts for the owner
// (dispatch://INFRA-978/comment/91d5e75b-57eb-4b76-ba55-5a101fec2c32).

// RevertSourceRef is the source_ref of the revert row of repo#number: revert:<repo>#<number>, the
// repository in lower case (delivery.PullRequestName).
func RevertSourceRef(repo string, number int) string {
	return prodevents.RevertSourceRefPrefix + delivery.PullRequestName(repo, number)
}

// revertFailures writes the revert row of each revert of rule's repository that can make one now
// (delivery.ListRevertShips: shipped by a deploy run, after the earliest day a project counts
// reverts from, and held by no revert row of any project yet, a row of source reverts keyed as
// RevertSourceRef keys it), each by revertFailure.
func revertFailures(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings) error {
	ships, err := delivery.ListRevertShips(ctx, pool, rule, prodevents.SourceReverts, prodevents.RevertSourceRefPrefix)
	if err != nil {
		return err
	}
	for _, ship := range ships {
		if err := revertFailure(ctx, pool, rule, ship); err != nil {
			return err
		}
	}
	return nil
}

// revertFailure writes the revert row of ship, a revert of rule's repository with the deploy run
// that ships it: blaming the deploy row that names the reverted pull request among those it shipped
// (prodevents.DeployForPullRequest), else the one whose commits hold the reverted pull request's
// merge commit (prodevents.DeployForCommit), each a deploy of any project but one another project
// posted, and restored by the revert's deploy row (prodevents.DeployForRun of the run that ships
// the revert). It writes nothing while either deploy has no row, and a revert that holds a refusal
// of the deploy it blames writes nothing and logs nothing. A blamed deploy that counts for other
// projects than rule's alone is no failure of rule's project (writeSignal): the revert is refused
// on that deploy, its refusal committed and logged once (revertRefusal). A blamed deploy that
// finished after the revert's deploy is no failure that deploy restores, and one that counts for
// no project is no failure of any project yet: the revert is held (holdRevert), and judged again
// on the next pass. The transaction that writes the revert's row, or its refusal, drops its holds.
func revertFailure(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings, ship delivery.RevertShip) error {
	restoring, held, err := prodevents.DeployForRun(ctx, pool, rule.DeployRepo, ship.ShipRunID)
	if err != nil || !held {
		return err
	}
	reverted := delivery.PullRequestName(ship.RevertsRepo, ship.RevertsNumber)
	blamed, held, err := prodevents.DeployForPullRequest(ctx, pool, rule.Project, reverted)
	if err != nil {
		return err
	}
	if !held && ship.RevertsMergeCommitSHA != nil {
		if blamed, held, err = prodevents.DeployForCommit(ctx, pool, rule.Project, *ship.RevertsMergeCommitSHA); err != nil {
			return err
		}
	}
	if !held {
		return nil
	}
	refused, stood, err := readRevertRefusal(ctx, pool, ship.Repo, ship.Number, blamed.ID)
	if err != nil || refused {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the revert of %s#%d: %w", ship.Repo, ship.Number, err)
	}
	defer tx.Rollback(ctx)
	judged := revertHold{reason: holdTooEarly, restoring: restoring.ID, finished: *restoring.FinishedAt}
	if prodevents.RestoresTooEarly(restoring, *blamed.FinishedAt) {
		return holdRevert(ctx, tx, ship, blamed.ID, stood, judged)
	}
	if _, err := tx.Exec(ctx, `delete from revert_refusals where repo = $1 and number = $2 and hold is not null`,
		ship.Repo, ship.Number); err != nil {
		return fmt.Errorf("drop the holds of the revert %s#%d: %w", ship.Repo, ship.Number, err)
	}
	_, matched, err := writeSignal(ctx, pool, tx, prodevents.Event{
		ProjectKey: rule.Project, Kind: prodevents.KindFailure, Source: prodevents.SourceReverts,
		SourceRef: RevertSourceRef(ship.Repo, ship.Number), Repo: strings.ToLower(ship.Repo), PullRequests: []string{reverted},
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", ship.Repo, ship.Number), Actor: loopActor,
		DeployID: new(blamed.ID), Signal: new(prodevents.SignalRevert), DetectedAt: new(ship.MergedAt.UTC()),
		RestoredAt: restoring.FinishedAt, RestoredBy: new(loopActor), RestoredByDeploy: new(restoring.ID),
	}, revertRefusal(ship.Repo, ship.Number), nil)
	if err != nil {
		return fmt.Errorf("write the revert of %s#%d: %w", ship.Repo, ship.Number, err)
	}
	if !matched {
		judged.reason = holdNoProject
		return holdRevert(ctx, tx, ship, blamed.ID, stood, judged)
	}
	return nil
}

// The holds of a revert (revert_refusals.hold): its own deploy finished before the deploy it blames
// (prodevents.RestoresTooEarly), or the deploy it blames counts for no project, or a deploy row the
// revert names is gone since its lookup (writeSignal).
const (
	holdTooEarly  = "too_early"
	holdNoProject = "no_project"
)

// heldMessage is the line each hold of a revert logs (holdRevert).
const heldMessage = "dispatch: a revert makes no failure of the deploy it blames for now and is not refused; every pass judges it again"

// revertHold is a judgement that holds a revert from its row: why (reason), and the revert's own
// deploy row, restoring, finished at finished, as the judgement read them. The zero hold is none.
type revertHold struct {
	reason    string
	restoring string
	finished  time.Time
}

// same reports whether h and other are one judgement.
func (h revertHold) same(other revertHold) bool {
	return h.reason == other.reason && h.restoring == other.restoring && h.finished.Equal(other.finished)
}

// why is the hold's reason as its log line states it.
func (h revertHold) why() string {
	if h.reason == holdTooEarly {
		return "the revert's own deploy finished before it, so it restores none of that deploy's failures"
	}
	return "it counts for no project, or a deploy row the revert names is gone since its lookup"
}

// readRevertRefusal reads what revert_refusals holds of repo#number, a revert, and deploy, the
// deploy it blames: whether the revert is refused on that deploy for good, and else the hold it
// stands logged on there, the zero hold where it holds none.
func readRevertRefusal(ctx context.Context, q delivery.Querier, repo string, number int, deploy string) (bool, revertHold, error) {
	var reason, restoring *string
	var finished *time.Time
	err := q.QueryRow(ctx, `select hold, restoring_deploy_id::text, restoring_finished_at from revert_refusals
		where repo = $1 and number = $2 and deploy_id = $3::uuid`, repo, number, deploy).Scan(&reason, &restoring, &finished)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, revertHold{}, nil
	case err != nil:
		return false, revertHold{}, fmt.Errorf("read the refusals of the revert %s#%d: %w", repo, number, err)
	case reason == nil:
		return true, revertHold{}, nil
	}
	return false, revertHold{reason: *reason, restoring: *restoring, finished: *finished}, nil
}

// holdRevert records in tx, and commits, hold, the judgement that holds ship's revert on deploy,
// the deploy it blames, and logs it with the revert, the deploy, the revert's own deploy and that
// deploy's finish, and why, where stood, the hold the revert stands logged on there, is another
// judgement: a pass that holds the revert on the judgement it stands logged on writes nothing and
// logs nothing. It records nothing where either deploy's row is gone since the revert's lookup, and
// never writes over a refusal.
func holdRevert(ctx context.Context, tx pgx.Tx, ship delivery.RevertShip, deploy string, stood, hold revertHold) error {
	if hold.same(stood) {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		insert into revert_refusals (repo, number, deploy_id, hold, restoring_deploy_id, restoring_finished_at)
		select $1::text, $2::integer, $3::uuid, $4::text, $5::uuid, $6::timestamptz
		where exists (select from production_events where id = $3::uuid) and exists (select from production_events where id = $5::uuid)
		on conflict (repo, number, deploy_id) do update set hold = excluded.hold, restoring_deploy_id = excluded.restoring_deploy_id,
			restoring_finished_at = excluded.restoring_finished_at, refused_at = excluded.refused_at
		where revert_refusals.hold is not null`,
		ship.Repo, ship.Number, deploy, hold.reason, hold.restoring, hold.finished); err != nil {
		return fmt.Errorf("hold the revert %s#%d on deploy %s: %w", ship.Repo, ship.Number, deploy, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the hold of the revert %s#%d on deploy %s: %w", ship.Repo, ship.Number, deploy, err)
	}
	slog.Info(heldMessage, "signal", prodevents.SignalRevert, "key", delivery.PullRequestName(ship.Repo, ship.Number), "deploy", deploy,
		"restoring", hold.restoring, "restoring_finished_at", hold.finished, "hold", hold.reason, "reason", hold.why())
	return nil
}

// revertRefusal is the refusal of repo#number, a revert, of a deploy it blames (revert_refusals): a
// later pass that finds the revert blaming the same deploy writes no row, and one that finds it
// blaming another deploy asks again.
func revertRefusal(repo string, number int) refusal {
	return refusal{signal: prodevents.SignalRevert, key: delivery.PullRequestName(repo, number), record: func(ctx context.Context, tx pgx.Tx, deploy string) error {
		if _, err := tx.Exec(ctx, `insert into revert_refusals (repo, number, deploy_id) values ($1, $2, $3::uuid)
			on conflict do nothing`, repo, number, deploy); err != nil {
			return fmt.Errorf("refuse the revert %s#%d on deploy %s: %w", repo, number, deploy, err)
		}
		return nil
	}}
}
