package evals

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// deploy_repos.go is how the delivery and CI evals that read the deploy repository are kept
// (dispatch://INFRA-2916, R14): once for every repository that holds a delivery run, each
// repository's rows computed as if it were the deploy repository and keyed by it
// (eval_days.deploy_repo, migration 20261117100006), with their stored figures and their members.
// A read folds the rows of the repository the delivery settings name (DeployRepoOf), so a move of
// the deploy repository is a selection at read time: it rewrites no row and begins no walk, and a
// move back reads the old repository's rows as they stand, which every write of a day keeps
// beside the others.
//
// What reads the deploy repository is what a day reads of it (readDeliveryDay): its deploy runs
// (delivery.ReadDeploys), its apply runs, under which containment ships its pull requests alone
// (delivery.ContainingRun), and its CI runs (delivery.ReadCIRows). So the three lead times to
// production, deploy-frequency, deploy-run-success, pr-ci with its job series and the three deploy
// cards are kept per repository, and so are the members of the delivery and CI families those
// rows count: each pull request with the run of that repository that ships it, that repository's
// deploy runs, and its CI runs. pr-open-to-merge and rework-share read a pull request's opening,
// merge and rework and no deploy, and keep one row, as do the Legion evals, the logged evals and
// unowned-p0; the pull requests the first two count are kept once more under no deploy repository.
//
// The repositories kept are those that hold a delivery run (readDeployRepos). The intake keeps the
// runs of the deploy repository alone, so a repository's runs are those stored while it was the
// deploy repository; one that never was holds none, and so no row: a move to it reads "no data"
// for every eval that reads the deploy repository (a repository that never deployed has nothing to
// count), and the rest as before. When its first run arrives, the change that brings it writes its
// rows of the days the run names, as any change does.
//
// The first day of each of a repository's series, which a read names (data_since, each series its
// own) and before which a range reads "no data", is that repository's own, and never earlier than
// the start of the history the reconcile holds for it (keptRepo.since): no row of it is kept
// before its first deploy or PR-checks run. A lead time to production begins on the merge day of
// the first pull request the repository ships (dropUnshipped): a day before it whose pull requests
// none of the repository's deploys ships counts nothing toward a lead time, and holds no row of
// it, where a day after it holds its row, a zero one included.

// deployRepoSlugs is the evals (by slug) whose rows and items read the deploy repository: the
// delivery and CI evals a day keeps once per repository, and change-fail-rate and
// failed-deploy-recovery, which have no row (no failure source exists) and whose items are the
// deploy repository's deploys and the pull requests it ships.
var deployRepoSlugs = []string{
	SlugMergeToProduction, SlugFirstCommitToProduction, SlugOpenToProduction, SlugDeployFrequency,
	SlugDeployRunSuccess, SlugPRCI, SlugStagingE2E, SlugStagingDeploy, SlugProductionDeploy,
	SlugChangeFailRate, SlugFailedDeployRecovery,
}

// readsDeployRepo reports whether the eval slug names is one whose rows are kept per deploy
// repository (deployRepoSlugs).
func readsDeployRepo(slug string) bool {
	return slices.Contains(deployRepoSlugs, slug)
}

// KeyedByDeployRepo reports whether a read of definition's eval reads the rows of a deploy
// repository: a computed eval of deployRepoSlugs. A logged eval's rows are its readings', which no
// deploy repository moves, whatever its slug.
func KeyedByDeployRepo(definition Definition) bool {
	return definition.Computed() && readsDeployRepo(definition.Slug)
}

// DeployRepoOf is the deploy repository whose day rows, stored figures and members a read of
// definition's eval reads, where the delivery settings name deployRepo ("" where none are set):
// deployRepo for an eval keyed by it (KeyedByDeployRepo), and "" for every other, whose rows no
// deploy repository moves.
func DeployRepoOf(definition Definition, deployRepo string) string {
	if KeyedByDeployRepo(definition) {
		return deployRepo
	}
	return ""
}

// keyedEvals is the ids of entries' evals keyed by the deploy repository (KeyedByDeployRepo): the
// evals a read's `case` gives a repository's rows (deployRepoCase), every other eval reading the
// rows of none.
func keyedEvals(entries []Entry) []string {
	keyed := []string{}
	for _, entry := range entries {
		if KeyedByDeployRepo(entry.Definition) {
			keyed = append(keyed, entry.Definition.EvalID)
		}
	}
	return keyed
}

// deployRepoCase is the condition that keeps, of the day rows or the stored figures of the evals
// it is applied to, those of the deploy repository each eval's read reads (DeployRepoOf): of the
// evals the array parameter keyed names (keyedEvals) the rows of the repository the text parameter
// deployRepo names, and of every other eval the rows of none.
func deployRepoCase(column, keyed, deployRepo string) string {
	return column + ` = case when eval_id = any(` + keyed + `::text[]) then ` + deployRepo + `::text else '' end`
}

// keptRepo is one repository whose rows of the evals that read the deploy repository are kept,
// whether it holds a deploy run, which the production rule and the deploy cards read, and since,
// the UTC midnight of its first deploy or PR-checks run, before which no row of it is kept.
//
// since is the start of the history the reconcile holds for the repository: the reconcile lists
// the deploy repository's runs from delivery.BackfillWindow before the first pass after the
// settings naming it were saved, and the intake keeps its runs alone, so its first stored run is
// the first moment its history holds; nothing else records that moment per repository, since a
// settings change starts the reconcile's progress anew. It is deliveryInputStart's bound of the
// runs, per repository. The pull requests reach further back, the whole population's history, so
// a first deploy that ships a pull request merged before the repository's history began would
// otherwise time a lead time from before it: such a day holds no row of the repository.
type keptRepo struct {
	name    string
	deploys bool
	since   time.Time
}

// readDeployRepos reads the repositories whose rows of the evals that read the deploy repository
// are kept: every repository that holds a delivery run, in the byte order of their names, each
// with whether it holds a deploy run and the start of its history (keptRepo.since). One
// statement walks delivery_runs' primary key from one repository to the next (repo, run_id), one
// probe each, and reads delivery_runs_kind_started_at's first entry of each repository and kind,
// so it reads a handful of index entries however many runs there are.
func readDeployRepos(ctx context.Context, q delivery.Querier) ([]keptRepo, error) {
	rows, err := q.Query(ctx, `
		with recursive held as (
			(select repo from delivery_runs order by repo limit 1)
			union all
			select (select run.repo from delivery_runs run where run.repo > held.repo order by run.repo limit 1)
			from held where held.repo is not null
		), first as (
			select held.repo,
				(select min(started_at) from delivery_runs run where run.repo = held.repo and run.kind = $1) as deploy,
				(select min(started_at) from delivery_runs run where run.repo = held.repo and run.kind = $2) as checks
			from held where held.repo is not null
		)
		select repo, deploy is not null, least(deploy, checks) from first
	`, string(delivery.DeliveryRunKindDeploy), string(delivery.DeliveryRunKindPRChecks))
	if err != nil {
		return nil, fmt.Errorf("read the repositories that hold a delivery run: %w", err)
	}
	var repos []keptRepo
	var one keptRepo
	if _, err := pgx.ForEachRow(rows, []any{&one.name, &one.deploys, &one.since}, func() error {
		one.since = measures.Midnight(one.since)
		repos = append(repos, one)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the repositories that hold a delivery run: %w", err)
	}
	slices.SortFunc(repos, func(a, b keptRepo) int { return cmp.Compare(a.name, b.name) })
	return repos, nil
}

// deployRepoNames is the names of repos, in their order.
func deployRepoNames(repos []keptRepo) []string {
	names := make([]string, len(repos))
	for i, repo := range repos {
		names[i] = repo.name
	}
	return names
}

// evalKey is one eval's rows of one deploy repository ("" for an eval no deploy repository moves):
// what the refresher keeps each series' first input day by, since each repository's rows of an
// eval that reads it begin on that repository's own first day.
type evalKey struct {
	eval, deployRepo string
}

// keyOf is row's evalKey.
func keyOf(row DayRow) evalKey {
	return evalKey{eval: row.EvalID, deployRepo: row.DeployRepo}
}

// compareEvalKeys orders eval keys by eval and then by deploy repository, each by its bytes.
func compareEvalKeys(a, b evalKey) int {
	return cmp.Or(cmp.Compare(a.eval, b.eval), cmp.Compare(a.deployRepo, b.deployRepo))
}

// leadTimeSlugs is the evals that time a pull request to the deploy that ships it, each of which
// counts in a repository's rows the pull requests that repository ships alone.
var leadTimeSlugs = []string{SlugMergeToProduction, SlugFirstCommitToProduction, SlugOpenToProduction}

// dropUnshipped is rows, a project's rows of day as a day's reads build them, without each lead
// time to production of a deploy repository whose all-repositories row counts no pull request
// (n 0), where the eval holds no row on or before day (first, firstOf's): a day before the first
// pull request the repository ships counts nothing toward its lead times, as a day before a
// series' first input day is no day of it. From the first day that counts one on, a day that
// counts none holds its row as it is built, a zero one.
func dropUnshipped(rows []DayRow, first map[evalKey]time.Time, day time.Time) []DayRow {
	unshipped := map[evalKey]bool{}
	for _, row := range rows {
		if row.DeployRepo == "" || row.Repo != "" || row.N != 0 || !slices.Contains(leadTimeSlugs, slugOf(row.ProjectKey, row.EvalID)) {
			continue
		}
		if since, held := first[keyOf(row)]; !held || day.Before(since) {
			unshipped[keyOf(row)] = true
		}
	}
	if len(unshipped) == 0 {
		return rows
	}
	return slices.DeleteFunc(slices.Clone(rows), func(row DayRow) bool { return unshipped[keyOf(row)] })
}
