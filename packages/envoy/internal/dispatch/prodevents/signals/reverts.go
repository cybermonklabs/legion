package signals

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
// whose reverted pull request no deploy row holds makes none, and neither does one whose blamed
// deploy finished after the revert's own deploy (prodevents.RestoresTooEarly), which is logged. One
// whose blamed deploy counts for other projects than the owner alone (deployCountsForRow) makes
// none and stays refused on that deploy: its refusal commits in the transaction that asked
// (revert_refusals, migration 20261128030142), so no later pass writes a row blaming that deploy,
// not even once a source move, a rule change or a default-repository move has made it count for the
// repository's owner (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706).

// RevertSourceRef is the source_ref of the revert row of repo#number: revert:<repo>#<number>, the
// repository in lower case (delivery.PullRequestName).
func RevertSourceRef(repo string, number int) string {
	return prodevents.RevertSourceRefPrefix + delivery.PullRequestName(repo, number)
}

// revertFailures writes the revert row of each revert of rule's repository that can make one now
// (delivery.ListRevertShips: shipped by a deploy run, after the earliest day a project counts
// reverts from, and held by no revert row of any project yet): blaming the deploy row that names
// the reverted pull request among those it shipped (prodevents.DeployForPullRequest), else the one
// whose commits hold the reverted pull request's merge commit (prodevents.DeployForCommit), each a
// deploy of any project but one another project posted, and restored by the revert's deploy row
// (prodevents.DeployForRun of the run that ships the revert). A blamed deploy that finished after
// the revert's deploy is no failure that deploy restores, so that revert is logged and writes no
// row. One whose blamed deploy counts for other projects than rule's alone writes none either, and
// its refusal of that deploy commits in the transaction that asked (writeSignal, revert_refusals): a
// revert that holds a refusal of the deploy it blames now writes nothing.
func revertFailures(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings) error {
	ships, err := delivery.ListRevertShips(ctx, pool, rule)
	if err != nil {
		return err
	}
	for _, ship := range ships {
		ref := RevertSourceRef(ship.Repo, ship.Number)
		restoring, held, err := prodevents.DeployForRun(ctx, pool, rule.DeployRepo, ship.ShipRunID)
		if err != nil {
			return err
		}
		if !held {
			continue
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
			continue
		}
		var refused bool
		if err := pool.QueryRow(ctx, `select exists (select from revert_refusals where repo = $1 and number = $2 and deploy_id = $3::uuid)`,
			ship.Repo, ship.Number, blamed.ID).Scan(&refused); err != nil {
			return fmt.Errorf("read the refusals of the revert %s#%d: %w", ship.Repo, ship.Number, err)
		}
		if refused {
			continue
		}
		if prodevents.RestoresTooEarly(restoring, *blamed.FinishedAt) {
			slog.Info("dispatch: a revert's deploy finished before the deploy the revert blames; it restores no failure of that deploy, and the revert makes no row",
				"revert", ref, "blamed", blamed.ID, "restoring", restoring.ID)
			continue
		}
		if _, err := writeSignal(ctx, pool, prodevents.Event{
			ProjectKey: rule.Project, Kind: prodevents.KindFailure, Source: prodevents.SourceReverts, SourceRef: ref,
			Repo: strings.ToLower(ship.Repo), PullRequests: []string{reverted},
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", ship.Repo, ship.Number), Actor: loopActor,
			DeployID: new(blamed.ID), Signal: new(prodevents.SignalRevert), DetectedAt: new(ship.MergedAt.UTC()),
			RestoredAt: restoring.FinishedAt, RestoredBy: new(loopActor), RestoredByDeploy: new(restoring.ID),
		}, blamed.ID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `insert into revert_refusals (repo, number, deploy_id) values ($1, $2, $3::uuid)
				on conflict do nothing`, ship.Repo, ship.Number, blamed.ID); err != nil {
				return fmt.Errorf("refuse the revert %s#%d on deploy %s: %w", ship.Repo, ship.Number, blamed.ID, err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("write the revert of %s#%d: %w", ship.Repo, ship.Number, err)
		}
	}
	return nil
}
