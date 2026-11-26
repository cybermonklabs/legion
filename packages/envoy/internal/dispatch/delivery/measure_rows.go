package delivery

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rows the delivery and CI measures count, read by one function for every reader that counts
// them: the delivery routes (api's readDeliveryRows and readPipelinePopulation) read their
// window's (ReadWindowRows, ReadCIRows), and a window's deploys ship their pull requests by one
// rule (Deploys.ShippedBy).
//
// A read takes q, the pool or a transaction. A read of several statements runs them side by side
// on the pool and one after the other in a transaction (readAll).

// readAll runs reads, the reads of one answer in q: on the pool side by side, each on a connection
// of its own (store.ReadSideBySide), and in a transaction one after the other, since its one
// connection runs one statement at a time and every read then reads its one snapshot.
func readAll(ctx context.Context, q Querier, reads ...func(ctx context.Context) error) error {
	if _, pooled := q.(*store.Pool); pooled {
		return store.ReadSideBySide(ctx, reads...)
	}
	for _, read := range reads {
		if err := read(ctx); err != nil {
			return err
		}
	}
	return nil
}

// MergedRows is the population pull requests merged in a window and the deploy runs that can have
// shipped them.
type MergedRows struct {
	// PullRequests is every population pull request merged in [from, to), as
	// ListPullRequestsInWindow lists them.
	PullRequests []DeliveryPullRequest
	// ApplyRuns is the deploy runs on main of the deploy repository that GitHub created at or
	// after from, oldest creation first, not cut at to: a pull request merged just before to may
	// first ship after it, so bounding the read to [from, to) would read it as waiting.
	// ReadMergedRows reads every such run (listRuns); ReadWindowRows every one created up to the
	// bound past which no run ships a pull request merged before to first, with no bound for a
	// window ending at the present (windowApplyRunsCreated). A production job on another branch
	// ships no pull request. ApplyJobs is their jobs: the jobs a deploy is judged by
	// (Production.JudgedJobNames), which judge each run as its every job would, and with them,
	// for the timeline, each of the run's jobs that failed.
	ApplyRuns []DeliveryRun
	ApplyJobs map[int64][]DeliveryRunJob
}

// ReadMergedRows reads MergedRows for [from, to) in q: the pull requests, and every apply run
// followed by their production jobs, side by side on the pool (readAll).
func ReadMergedRows(ctx context.Context, q Querier, settings DeliverySettings, from, to time.Time) (MergedRows, error) {
	return readMergedRows(ctx, q, settings, from, to, "", nil)
}

// readMergedRows is ReadMergedRows, narrowed to repo's pull requests when repo is non-empty and,
// with createdBefore set, to only the apply runs GitHub created before it.
func readMergedRows(ctx context.Context, q Querier, settings DeliverySettings, from, to time.Time, repo string, createdBefore *time.Time) (MergedRows, error) {
	var rows MergedRows
	err := readAll(ctx, q,
		func(ctx context.Context) (err error) {
			if repo != "" {
				rows.PullRequests, err = listRepoPullRequestsInWindow(ctx, q, repo, from, to)
			} else {
				rows.PullRequests, err = ListPullRequestsInWindow(ctx, q, from, to)
			}
			return err
		},
		func(ctx context.Context) error {
			applies, err := readApplyRows(ctx, q, settings, from, createdBefore)
			rows.ApplyRuns, rows.ApplyJobs = applies.Runs, applies.Jobs
			return err
		})
	if err != nil {
		return MergedRows{}, err
	}
	return rows, nil
}

// ApplyRows is MergedRows' apply runs and their jobs, read from a moment on (readApplyRows).
type ApplyRows struct {
	Runs []DeliveryRun
	Jobs map[int64][]DeliveryRunJob
}

// readApplyRows reads the apply runs and their production jobs (the jobs a deploy is judged by)
// of a window from from, MergedRows.ApplyRuns and ApplyJobs as readMergedRows reads them, with
// createdBefore as readMergedRows takes it.
func readApplyRows(ctx context.Context, q Querier, settings DeliverySettings, from time.Time, createdBefore *time.Time) (ApplyRows, error) {
	runs, err := listRuns(ctx, q, settings.DeployRepo, DeliveryRunKindDeploy, "main", from, createdBefore)
	if err != nil {
		return ApplyRows{}, err
	}
	runIDs := make([]int64, len(runs))
	for i, run := range runs {
		runIDs[i] = run.RunID
	}
	jobs, err := ListRunJobsForRuns(ctx, q, settings.DeployRepo, runIDs, settings.Production().JudgedJobNames())
	if err != nil {
		return ApplyRows{}, err
	}
	return ApplyRows{Runs: runs, Jobs: jobs}, nil
}

// ReadWindowRows is a delivery route's read of [from, to): MergedRows, with failedJobs each apply
// run's failed jobs beside its production jobs, which the timeline lists each deploy run with (its
// failed jobs and the job that failed first, FailedJobs and RootFailingJob, can be any job, and
// keep the failed ones alone), and with deploys the window's Deploys, as ReadDeploys reads them.
// The two lists of runs are nearly one, the runs of the window, so they are read in one statement
// (listWindowRuns) and the production jobs of both in one more, where reading each list and its
// jobs apart read the window's production jobs twice, three times the window's for the measures
// of a compare range, whose apply runs reached to the present (dispatch://INFRA-2916, R8). The apply
// runs stop at the bound past which no run ships first a pull request of the window or of
// Deploys.Earlier (windowApplyRunsCreated), so every pull request's deploy is the one
// ReadMergedRows' runs give it. The pull requests are read side by side with the runs and their
// jobs.
func ReadWindowRows(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time, failedJobs, deploys bool) (MergedRows, Deploys, error) {
	var rows MergedRows
	var window Deploys
	err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			rows.PullRequests, err = ListPullRequestsInWindow(ctx, pool, from, to)
			return err
		},
		func(ctx context.Context) error {
			applies, started, err := readWindowRuns(ctx, pool, settings, from, to, failedJobs, deploys)
			rows.ApplyRuns, rows.ApplyJobs = applies.Runs, applies.Jobs
			if err != nil || !deploys {
				return err
			}
			window, err = deploysOf(ctx, pool, settings, from, started)
			return err
		})
	if err != nil {
		return MergedRows{}, Deploys{}, err
	}
	return rows, window, nil
}

// readWindowRuns reads ReadWindowRows' runs with their jobs: the apply runs, and with deploys the
// deploys of the window, each with its production jobs, read for both lists in one statement, and
// with failedJobs the apply runs' failed jobs too, in another beside it.
func readWindowRuns(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time, failedJobs, deploys bool) (applies, started RunsWithJobs, err error) {
	production := settings.Production()
	listed, err := listWindowRuns(ctx, pool, settings.DeployRepo, production, from, to, deploys)
	if err != nil {
		return RunsWithJobs{}, RunsWithJobs{}, err
	}
	names := production.JudgedJobNames()
	var named, failed map[int64][]DeliveryRunJob
	reads := []func(ctx context.Context) error{func(ctx context.Context) (err error) {
		named, err = ListRunJobsForRuns(ctx, pool, settings.DeployRepo, listed.ids, names)
		return err
	}}
	if failedJobs {
		applyIDs := make([]int64, len(listed.apply))
		for i, run := range listed.apply {
			applyIDs[i] = run.RunID
		}
		reads = append(reads, func(ctx context.Context) (err error) {
			failed, err = listRunJobsFailedForRuns(ctx, pool, settings.DeployRepo, applyIDs, names)
			return err
		})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return RunsWithJobs{}, RunsWithJobs{}, err
	}
	return RunsWithJobs{Runs: listed.apply, Jobs: namedAndFailedJobs(listed.apply, named, failed)},
		RunsWithJobs{Runs: listed.started, Jobs: namedAndFailedJobs(listed.started, named, nil)}, nil
}

// Deploys is the measures' deploys of a window: every deploy run on main of the deploy repository
// started in it, whatever its event, with the jobs it is judged by (Production.JudgedJobNames), the
// deploys among them, and the rows from before the window that decide which pull requests merged
// before it those deploys ship. Whether a run reached production is decided on these runs and
// their jobs, never on the creation-time listing containment reads, so every run a window counts
// is judged.
type Deploys struct {
	Started RunsWithJobs
	// Applies is the deploys among Started (ProductionApplies), in compareApplies' order.
	Applies []Apply
	// Earlier is the deploy repository's pull requests merged before the window that Applies can
	// ship, and the deploy runs on main GitHub created before the window, which can ship them
	// first: MergedRows from since (ReadDeploys says what it is) up to the window's start, its
	// pull requests narrowed to settings.DeployRepo before the row cap and its apply runs those
	// created in that span alone. Empty where no pull request merged before the window can ship
	// with one of Applies.
	Earlier MergedRows
}

// ReadDeploys reads Deploys for [from, to) in q. The earlier rows start at since: the latest
// creation among the deploys that succeeded before the first of Applies completed
// (ReadLatestShipBefore; the beginning of time where none did), since a pull request merged before
// it had shipped by then and so ships with none of Applies; or the creation of one of Applies,
// where one was created earlier still, so that run is among the apply runs. Nothing earlier is
// read where the window holds no deploy, or since is not before from.
func ReadDeploys(ctx context.Context, q Querier, settings DeliverySettings, from, to time.Time) (Deploys, error) {
	started, err := listRunsWithJobsStartedIn(ctx, q, settings.DeployRepo, DeliveryRunKindDeploy, "main", "", from, to, settings.Production().JudgedJobNames())
	if err != nil {
		return Deploys{}, err
	}
	return deploysOf(ctx, q, settings, from, started)
}

// deploysOf is the Deploys of a window from from whose deploy runs, with the jobs each is judged by,
// are started: the deploys among them, and the earlier rows ReadDeploys reads, in q.
func deploysOf(ctx context.Context, q Querier, settings DeliverySettings, from time.Time, started RunsWithJobs) (Deploys, error) {
	production := settings.Production()
	deploys := Deploys{Started: started, Applies: ProductionApplies(started.Runs, started.Jobs, production).Applies}
	if len(deploys.Applies) == 0 {
		return deploys, nil
	}
	since, err := ReadLatestShipBefore(ctx, q, settings.DeployRepo, production, deploys.Applies[0].CompletedAt)
	if err != nil {
		return Deploys{}, err
	}
	for _, apply := range deploys.Applies {
		if apply.CreatedAt.Before(since) {
			since = apply.CreatedAt
		}
	}
	if !since.Before(from) {
		return deploys, nil
	}
	if deploys.Earlier, err = readMergedRows(ctx, q, settings, since, from, settings.DeployRepo, &from); err != nil {
		return Deploys{}, err
	}
	return deploys, nil
}

// ShippedBy is, for each of d.Applies that ships a pull request, the pull requests it ships first
// (ContainingRun over the apply runs of window and of d.Earlier), whenever they merged: those of
// window (ReadMergedRows or ReadWindowRows of d's window) and those merged before it, of
// d.Earlier, each list in that order. A deploy is one with pull requests when it ships one,
// whenever the pull request merged (dispatch://INFRA-2916): the deploy-frequency measure's
// with_prs (measures.Run.ShippedPRs) is this rule on the delivery routes. The pointers are into
// window's and d.Earlier's lists.
func (d Deploys) ShippedBy(window MergedRows, settings DeliverySettings) map[int64][]*DeliveryPullRequest {
	production := settings.Production()
	// The two listings hold no run in common (Earlier's were created before the window, window's
	// in it or after), and a run's verdict reads its own jobs alone, so their deploys are the
	// deploys of the two together.
	applies := append(ProductionApplies(d.Earlier.ApplyRuns, d.Earlier.ApplyJobs, production).Applies,
		ProductionApplies(window.ApplyRuns, window.ApplyJobs, production).Applies...)
	deploys := make(map[int64]bool, len(d.Applies))
	for _, apply := range d.Applies {
		deploys[apply.RunID] = true
	}
	shipped := map[int64][]*DeliveryPullRequest{}
	for _, prs := range [][]DeliveryPullRequest{window.PullRequests, d.Earlier.PullRequests} {
		for i := range prs {
			if apply := ContainingRun(prs[i], settings.DeployRepo, applies); apply != nil && deploys[apply.RunID] {
				shipped[apply.RunID] = append(shipped[apply.RunID], &prs[i])
			}
		}
	}
	return shipped
}

// CIRows is the CI measures' runs of a window, each read by start over the window: the PR-checks
// workflow's pull_request runs with every job, which their job timings and failing jobs read
// whatever the names, and the deploy workflow's push runs on main with the jobs the deploy cards
// follow and no other.
type CIRows struct {
	Checks, Deploys RunsWithJobs
}

// ReadCIRows reads CIRows for [from, to) of the deploy repository in q, the two reads side by side
// on the pool (readAll); deployJobNames is every job name the deploy cards follow.
func ReadCIRows(ctx context.Context, q Querier, deployRepo string, from, to time.Time, deployJobNames []string) (CIRows, error) {
	var rows CIRows
	err := readAll(ctx, q,
		func(ctx context.Context) (err error) {
			rows.Checks, err = listRunsWithEveryJobStartedIn(ctx, q, deployRepo, DeliveryRunKindPRChecks, "", "pull_request", from, to)
			return err
		},
		func(ctx context.Context) (err error) {
			rows.Deploys, err = listRunsWithJobsStartedIn(ctx, q, deployRepo, DeliveryRunKindDeploy, "main", "push", from, to, deployJobNames)
			return err
		})
	if err != nil {
		return CIRows{}, err
	}
	return rows, nil
}

// daysRunsQuery reads the runs of a list of days in one statement: the deploy runs on main of $1
// that are an apply run of a day's window (applyRunsCreated, the window's bound read once a day)
// or started on a day, each once, oldest creation first, $2 and $3 the days' starts and ends and
// $4 and $5 Production.JobNames and Production.ApplyStep. Each day's two parts are ranges of
// delivery_runs_kind_created and delivery_runs_kind_started_at, which
// TestTheWindowsRunsReadIsBoundedThroughItsIndexes holds its plan to.
var daysRunsQuery = `
		select ` + RunColumns + `
		from delivery_runs r
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.run_id in (
			select listed.run_id
			from unnest($2::timestamptz[], $3::timestamptz[]) as d(from_at, to_at)
			cross join lateral (
				select r.run_id from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and ` + applyRunsCreated("d.from_at", "d.to_at") + `
				union all
				select r.run_id from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.started_at >= d.from_at and r.started_at < d.to_at
			) as listed
		)
		order by ` + runCreatedAt + `, r.run_id
		limit $6`
