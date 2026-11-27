package delivery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The merged reverts of the deploy repositories (dispatch://INFRA-978, R7 and D5), kept in
// delivery_reverts (migration 20261123041500): a merged pull request whose body says
// `Reverts <owner>/<repo>#<n>`, the line GitHub's revert button writes, recorded whoever authored
// it. The pull-request intake records one as it merges (handlePullRequestEnvelope), and the
// reconcile's search of each deploy repository records one the intake missed (reconcileReverts).
// The failure a revert makes is the production signals' (prodevents/signals): it is written once
// the deploy that ships the revert has its deploy row (ListRevertShips).

// revertLine is the line GitHub's revert button writes into the body of the pull request it opens:
// `Reverts <owner>/<repo>#<number>`, alone on its line. The number is bounded to nine digits, as a
// linked pull request's is (linkedPullRequestTerms): delivery_reverts keeps it in an integer
// column, which would refuse a larger one at every write of its row.
var revertLine = regexp.MustCompile(`^Reverts ([^\s/#]+/[^\s/#]+)#([1-9][0-9]{0,8})$`)

// ParseRevert reads the pull request a body says it reverts: the first line that is GitHub's
// revert-button line (revertLine), anywhere in the body, the repository in lower case. ok is false
// for a body with no such line, and for one whose repository is not a GitHub owner/name.
func ParseRevert(body string) (repo string, number int, ok bool) {
	for line := range strings.SplitSeq(body, "\n") {
		match := revertLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		if _, _, err := refs.SplitGitHubRepo(match[1]); err != nil {
			continue
		}
		number, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		return strings.ToLower(match[1]), number, true
	}
	return "", 0, false
}

// Revert is one row of delivery_reverts: the merged revert of a deploy repository, the pull request
// it reverts, and the two merge commits.
type Revert struct {
	// Repo is the deploy repository the revert merged into, spelled as its rule holds it (the
	// spelling its runs are stored under), and Number the revert's number.
	Repo   string
	Number int
	// RevertsRepo and RevertsNumber are the pull request the revert's body names (ParseRevert).
	RevertsRepo   string
	RevertsNumber int
	// RevertsMergeCommitSHA is that pull request's merge commit, nil where GitHub named none.
	RevertsMergeCommitSHA *string
	MergedAt              time.Time
	// MergeCommitSHA is the revert's own merge commit, nil where GitHub named none.
	MergeCommitSHA *string
}

// revertOf is the Revert of a merged pull request of repo as GitHub's REST API answered it, and of
// the pull request its body names, and false where the pull request is not merged or its body names
// no pull request (ParseRevert). reverted is GitHub's answer for the named pull request, nil where it
// was not read; its merge commit is the one thing read of it.
func revertOf(repo string, revert pullRequestPayload, reverted *pullRequestPayload) (Revert, bool) {
	if revert.MergedAt == nil {
		return Revert{}, false
	}
	revertsRepo, revertsNumber, ok := ParseRevert(revert.Body)
	if !ok {
		return Revert{}, false
	}
	recorded := Revert{
		Repo: repo, Number: revert.Number, RevertsRepo: revertsRepo, RevertsNumber: revertsNumber,
		MergedAt: revert.MergedAt.UTC(), MergeCommitSHA: lowerSHA(revert.MergeCommitSHA),
	}
	if reverted != nil {
		recorded.RevertsMergeCommitSHA = lowerSHA(reverted.MergeCommitSHA)
	}
	return recorded, true
}

// lowerSHA is a commit as delivery_reverts keeps it: lower case, nil for none.
func lowerSHA(sha *string) *string {
	if sha == nil || *sha == "" {
		return nil
	}
	return new(strings.ToLower(*sha))
}

// RecordRevert stores revert, or moves the stored row of the same pull request to it. A row that
// would not change is not written, so a second record of one revert notifies nothing.
func RecordRevert(ctx context.Context, pool *store.Pool, revert Revert) error {
	_, err := pool.Exec(ctx, `
		insert into delivery_reverts (repo, number, reverts_repo, reverts_number, reverts_merge_commit_sha,
			merged_at, merge_commit_sha)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (repo, number) do update set
			reverts_repo = excluded.reverts_repo,
			reverts_number = excluded.reverts_number,
			reverts_merge_commit_sha = coalesce(excluded.reverts_merge_commit_sha, delivery_reverts.reverts_merge_commit_sha),
			merged_at = excluded.merged_at,
			merge_commit_sha = coalesce(excluded.merge_commit_sha, delivery_reverts.merge_commit_sha),
			recorded_at = now()
		where (delivery_reverts.reverts_repo, delivery_reverts.reverts_number, delivery_reverts.merged_at)
				is distinct from (excluded.reverts_repo, excluded.reverts_number, excluded.merged_at)
			or (excluded.reverts_merge_commit_sha is not null
				and excluded.reverts_merge_commit_sha is distinct from delivery_reverts.reverts_merge_commit_sha)
			or (excluded.merge_commit_sha is not null
				and excluded.merge_commit_sha is distinct from delivery_reverts.merge_commit_sha)
	`, revert.Repo, revert.Number, revert.RevertsRepo, revert.RevertsNumber, revert.RevertsMergeCommitSHA,
		revert.MergedAt, revert.MergeCommitSHA)
	if err != nil {
		return fmt.Errorf("record the revert %s#%d: %w", revert.Repo, revert.Number, err)
	}
	return nil
}

// recordRevertFromGitHub reads pull request number of the deploy repository repo from GitHub and,
// where it merged and its body says it reverts a pull request, reads that pull request's merge
// commit (revertedPullRequest) and records the revert (RecordRevert). Every fact it stores is
// GitHub's own answer, never an envelope's, which any bearer of the bus can publish.
func recordRevertFromGitHub(ctx context.Context, pool *store.Pool, client *githubapp.Client, repo string, number int) error {
	owner, name, token, err := repositoryRead(ctx, client, repo)
	if err != nil {
		return err
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, name, number)
	if err != nil {
		return err
	}
	revert, ok := revertOf(repo, payload, nil)
	if !ok {
		return nil
	}
	reverted, err := revertedPullRequest(ctx, client, repo, token, revert.RevertsRepo, revert.RevertsNumber)
	if err != nil {
		return err
	}
	if reverted != nil {
		revert.RevertsMergeCommitSHA = lowerSHA(reverted.MergeCommitSHA)
	}
	return RecordRevert(ctx, pool, revert)
}

// revertedPullRequest is GitHub's answer for pull request number of revertsRepo, which a revert
// of repo names, read with token, repo's own, where revertsRepo is repo. It is nil where GitHub no
// longer answers for that pull request, where no installation reads its repository, and where
// revertsRepo is no GitHub owner/name: the revert is then recorded with no merge commit of it, and
// the deploy that shipped it is the one whose deploy row names it.
func revertedPullRequest(ctx context.Context, client *githubapp.Client, repo, token, revertsRepo string, number int) (*pullRequestPayload, error) {
	owner, name, err := refs.SplitGitHubRepo(revertsRepo)
	if err != nil {
		return nil, nil
	}
	if !strings.EqualFold(revertsRepo, repo) {
		token, err = client.RepositoryToken(ctx, owner, name)
		if errors.Is(err, githubapp.ErrNoInstallation) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("mint installation token for %s: %w", revertsRepo, err)
		}
	}
	read, err := fetchPullRequestPayload(ctx, client, token, owner, name, number)
	switch {
	case err == nil:
		return &read, nil
	case errors.Is(err, ErrPullRequestNotFound) || errors.Is(err, githubapp.ErrRepositoryUnreadable) ||
		errors.Is(err, githubapp.ErrNoInstallation):
		return nil, nil
	}
	return nil, err
}

// RevertShip is a recorded revert with the deploy run that ships it: the first production deploy
// GitHub created after the revert merged, by the shipping rule every reader of a deploy reads
// (shippingRunJoin). ShipRunID and ShipCompletedAt are nil while no deploy has shipped it.
type RevertShip struct {
	Revert
	ShipRunID       *int64
	ShipCompletedAt *time.Time
}

// ListRevertShips reads every recorded revert of rule's repository, oldest merge first, each with
// the deploy run that ships it under rule.
func ListRevertShips(ctx context.Context, q Querier, rule DeliverySettings) ([]RevertShip, error) {
	production := rule.Production()
	rows, err := q.Query(ctx, `
		select pr.repo, pr.number, pr.reverts_repo, pr.reverts_number, pr.reverts_merge_commit_sha,
			pr.merged_at, pr.merge_commit_sha, ship.run_id, ship.completed_at
		from delivery_reverts pr
		`+shippingRunJoin("$1", "$2", "$3")+`
		where pr.repo = $1
		order by pr.merged_at, pr.number
	`, rule.DeployRepo, production.JobNames, production.ApplyStep)
	if err != nil {
		return nil, fmt.Errorf("read the reverts of %s: %w", rule.DeployRepo, err)
	}
	defer rows.Close()
	var ships []RevertShip
	for rows.Next() {
		var ship RevertShip
		if err := rows.Scan(&ship.Repo, &ship.Number, &ship.RevertsRepo, &ship.RevertsNumber, &ship.RevertsMergeCommitSHA,
			&ship.MergedAt, &ship.MergeCommitSHA, &ship.ShipRunID, &ship.ShipCompletedAt); err != nil {
			return nil, fmt.Errorf("read the reverts of %s: %w", rule.DeployRepo, err)
		}
		ships = append(ships, ship)
	}
	return ships, rows.Err()
}
