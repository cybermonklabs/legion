package signals

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The revert signal (dispatch://INFRA-978, R7 and D5): a merged revert of a deploy repository
// (delivery_reverts) is a failure of the deploy that shipped the pull request it reverts, which the
// revert's own deploy restores. It is written once the deploy that ships the revert has its deploy
// row, under the owner of the repository's source (delivery.DeployRules), keyed by the revert
// (RevertSourceRef), detected when the revert merged, restored at the revert's deploy's finish by
// that deploy. A revert whose reverted pull request no deploy row holds makes none.

// RevertSourceRef is the source_ref of the revert row of repo#number: revert:<repo>#<number>, the
// repository in lower case (delivery.PullRequestName).
func RevertSourceRef(repo string, number int) string {
	return "revert:" + delivery.PullRequestName(repo, number)
}

// revertFailures writes the revert row of each revert of rule's repository whose deploy has its
// deploy row and has none yet: blaming the deploy row that names the reverted pull request among
// those it shipped (prodevents.DeployForPullRequest), else the one whose commits hold the reverted
// pull request's merge commit (prodevents.DeployForCommit), and restored by the revert's deploy row
// (prodevents.DeployForRun of the run that ships the revert, delivery.ListRevertShips). Which of
// the shipped reverts have their row already is read in one statement (prodevents.HeldSourceRefs).
func revertFailures(ctx context.Context, pool *store.Pool, rule delivery.DeliverySettings) error {
	ships, err := delivery.ListRevertShips(ctx, pool, rule)
	if err != nil {
		return err
	}
	var refs []string
	for _, ship := range ships {
		if ship.ShipRunID != nil {
			refs = append(refs, RevertSourceRef(ship.Repo, ship.Number))
		}
	}
	made, err := prodevents.HeldSourceRefs(ctx, pool, rule.Project, refs)
	if err != nil {
		return err
	}
	for _, ship := range ships {
		if ship.ShipRunID == nil {
			continue
		}
		ref := RevertSourceRef(ship.Repo, ship.Number)
		if made[ref] {
			continue
		}
		restoring, held, err := prodevents.DeployForRun(ctx, pool, rule.DeployRepo, *ship.ShipRunID)
		if err != nil {
			return err
		}
		if !held {
			continue
		}
		reverted := delivery.PullRequestName(ship.RevertsRepo, ship.RevertsNumber)
		blamed, held, err := prodevents.DeployForPullRequest(ctx, pool, reverted)
		if err != nil {
			return err
		}
		if !held && ship.RevertsMergeCommitSHA != nil {
			if blamed, held, err = prodevents.DeployForCommit(ctx, pool, *ship.RevertsMergeCommitSHA); err != nil {
				return err
			}
		}
		if !held {
			continue
		}
		if _, _, err := prodevents.Write(ctx, pool, prodevents.Event{
			ProjectKey: rule.Project, Kind: prodevents.KindFailure, Source: prodevents.SourceReverts, SourceRef: ref,
			Repo: strings.ToLower(ship.Repo), PullRequests: []string{reverted},
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", ship.Repo, ship.Number), Actor: Actor,
			DeployID: new(blamed.ID), Signal: new(prodevents.SignalRevert), DetectedAt: new(ship.MergedAt.UTC()),
			RestoredAt: restoring.FinishedAt, RestoredBy: new(Actor), RestoredByDeploy: new(restoring.ID),
		}); err != nil {
			return fmt.Errorf("write the revert of %s#%d: %w", ship.Repo, ship.Number, err)
		}
	}
	return nil
}
