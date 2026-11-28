package delivery

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The deploys of a repository as the record of production events keeps them (dispatch://INFRA-978,
// R6): one deploy row per deploy run of a deploy repository that the repository's rule calls a
// deploy, read by the rules every reader of a deploy reads (ReadDeploys, ProductionApplies and
// Deploys.ShippedBy), so the record's deploys are the measures' deploys. The GitHub adapter that
// writes them is prodevents/signals.

// DeployRow is one deploy run of a deploy repository as its deploy row holds it: the run, when its
// production job completed (the deploy's finish), the commits it shipped and the pull requests it
// shipped first.
type DeployRow struct {
	Run DeliveryRun
	// FinishedAt is the production job's completion (Apply.CompletedAt).
	FinishedAt time.Time
	// Commits is the run's head and the merge commit of each pull request it ships, in lower case,
	// each once, the head first.
	Commits []string
	// PullRequests is each pull request it ships first (Deploys.ShippedBy), as owner/repo#n with the
	// repository in lower case, oldest merge first.
	PullRequests []string
}

// PullRequestName is a pull request as the record of production events names it: owner/repo#n,
// the repository in lower case.
func PullRequestName(repo string, number int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), number)
}

// DeployRowsIn reads the deploy row of every deploy run on main of rule's repository started in
// [from, to) that rule calls a deploy, with the pull requests each ships first
// (Deploys.ShippedBy), and the ids of every deploy run on main started in the window, deploy or
// not, which a writer compares its stored rows with. It reads the window as a delivery route does:
// its runs (ReadWindowRuns, whose Deploys are ReadDeploys' and whose apply runs give each pull
// request the deploy ReadMergedRows' give it) and its pull requests (ListPullRequestsInWindow).
func DeployRowsIn(ctx context.Context, pool *store.Pool, rule DeliverySettings, from, to time.Time) ([]DeployRow, []int64, error) {
	applies, deploys, err := ReadWindowRuns(ctx, pool, rule, from, to, false, true)
	if err != nil {
		return nil, nil, fmt.Errorf("read the deploys of %s: %w", rule.DeployRepo, err)
	}
	started := make([]int64, len(deploys.Started.Runs))
	runs := make(map[int64]DeliveryRun, len(deploys.Started.Runs))
	for i, run := range deploys.Started.Runs {
		started[i] = run.RunID
		runs[run.RunID] = run
	}
	if len(deploys.Applies) == 0 {
		return nil, started, nil
	}
	pulls, err := ListPullRequestsInWindow(ctx, pool, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("read the pull requests %s's deploys can ship: %w", rule.DeployRepo, err)
	}
	shipped := deploys.ShippedBy(MergedRows{PullRequests: pulls, ApplyRuns: applies.Runs, ApplyJobs: applies.Jobs}, rule)
	rows := make([]DeployRow, 0, len(deploys.Applies))
	for _, apply := range deploys.Applies {
		run := runs[apply.RunID]
		row := DeployRow{Run: run, FinishedAt: apply.CompletedAt.UTC(), Commits: []string{strings.ToLower(run.HeadSHA)}, PullRequests: []string{}}
		prs := slices.Clone(shipped[apply.RunID])
		slices.SortStableFunc(prs, func(a, b *DeliveryPullRequest) int {
			return cmp.Or(a.MergedAt.Compare(*b.MergedAt), strings.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
		})
		for _, pr := range prs {
			row.PullRequests = append(row.PullRequests, PullRequestName(pr.Repo, pr.Number))
			if pr.MergeCommitSHA != nil && *pr.MergeCommitSHA != "" {
				if sha := strings.ToLower(*pr.MergeCommitSHA); !slices.Contains(row.Commits, sha) {
					row.Commits = append(row.Commits, sha)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, started, nil
}

// FirstDeployRunStart is when the first deploy run of repo started, of any branch, and false where
// it holds none: where a whole derive of its deploy rows starts.
func FirstDeployRunStart(ctx context.Context, q Querier, repo string) (time.Time, bool, error) {
	var first *time.Time
	if err := q.QueryRow(ctx, `select min(started_at) from delivery_runs where repo = $1 and kind = 'deploy'`, repo).Scan(&first); err != nil {
		return time.Time{}, false, fmt.Errorf("read the first deploy run of %s: %w", repo, err)
	}
	if first == nil {
		return time.Time{}, false, nil
	}
	return first.UTC(), true, nil
}

// DeployRowsMovedSince is the earliest start of a deploy run of rule's repository whose deploy
// row a write of the deploy runs runIDs, or of pull requests of the repository merged at merges, can
// have moved, and false where it can have moved none (no such run is stored and no merge is
// given). A run changed (a run written, its jobs, its re-run) can take the pull requests merged
// since the creation of the latest deploy that completed before the run was created
// (ReadLatestShipBefore), and give up its own, which merged after that too; every run that can ship
// one of those was created after its merge, so it started after that creation. A pull request
// changed can move only the run that ships it, created after its merge. That latest deploy only
// moves later as the moment does, so the earliest of the changed runs' is the one before the run
// created first, which is read alone.
func DeployRowsMovedSince(ctx context.Context, q Querier, rule DeliverySettings, runIDs []int64, merges []time.Time) (time.Time, bool, error) {
	var since time.Time
	found := false
	earlier := func(at time.Time) {
		if !found || at.Before(since) {
			since, found = at, true
		}
	}
	for _, merged := range merges {
		earlier(merged)
	}
	if len(runIDs) == 0 {
		return since, found, nil
	}
	rows, err := q.Query(ctx, `select `+RunColumns+` from delivery_runs where repo = $1 and run_id = any($2)`, rule.DeployRepo, runIDs)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the changed runs of %s: %w", rule.DeployRepo, err)
	}
	runs, err := collectRuns(rows)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the changed runs of %s: %w", rule.DeployRepo, err)
	}
	if len(runs) == 0 {
		return since, found, nil
	}
	first := runs[0]
	for _, run := range runs[1:] {
		if run.Created().Before(first.Created()) {
			first = run
		}
	}
	earlier(first.Created())
	latest, err := ReadLatestShipBefore(ctx, q, rule.DeployRepo, rule.Production(), first.Created())
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the deploy before run %d of %s: %w", first.RunID, rule.DeployRepo, err)
	}
	if latest.IsZero() {
		// No deploy before it: every run of the repository can have moved.
		return time.Time{}, true, nil
	}
	earlier(latest)
	return since, found, nil
}
