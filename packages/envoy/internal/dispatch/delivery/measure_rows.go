package delivery

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rows the delivery and CI measures count, read by one function for every reader that counts
// them: the delivery routes (api's readDeliveryRows and readPipelinePopulation) read their
// window's, and the eval registry's day rows (evals.ReadDeliveryDay) one UTC day's, so a route and
// a day row count the same rows by the same reads.

// MergedRows is the population pull requests merged in a window and the deploy runs that can have
// shipped them.
type MergedRows struct {
	// PullRequests is every population pull request merged in [from, to), as
	// ListPullRequestsInWindow lists them.
	PullRequests []DeliveryPullRequest
	// ApplyRuns is every deploy run on main of the deploy repository that GitHub created at or
	// after from (ListRuns), unbounded past to: a pull request merged just before to may first
	// ship after it, so bounding the read to [from, to) would read it as waiting. A production job
	// on another branch ships no pull request. ApplyJobs is their jobs: the jobs a deploy is judged
	// by (Production.JudgedJobNames), which judge each run as its every job would, or every job.
	ApplyRuns []DeliveryRun
	ApplyJobs map[int64][]DeliveryRunJob
}

// ReadMergedRows reads MergedRows for [from, to): the pull requests, and the apply runs followed by
// their jobs, side by side. everyJob reads every job of each apply run, which the timeline lists
// each deploy run with (its failed jobs and the job that failed first, FailedJobs and
// RootFailingJob, can be any job); every other reader judges a run by its production jobs alone.
func ReadMergedRows(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time, everyJob bool) (MergedRows, error) {
	var rows MergedRows
	err := readSideBySide(ctx,
		func(ctx context.Context) (err error) {
			rows.PullRequests, err = ListPullRequestsInWindow(ctx, pool, from, to)
			return err
		},
		func(ctx context.Context) error {
			runs, err := ListRuns(ctx, pool, settings.DeployRepo, DeliveryRunKindDeploy, "main", from)
			if err != nil {
				return err
			}
			runIDs := make([]int64, len(runs))
			for i, run := range runs {
				runIDs[i] = run.RunID
			}
			var jobs map[int64][]DeliveryRunJob
			if everyJob {
				jobs, err = ListEveryRunJobForRuns(ctx, pool, settings.DeployRepo, runIDs)
			} else {
				jobs, err = ListRunJobsForRuns(ctx, pool, settings.DeployRepo, runIDs, settings.Production().JudgedJobNames())
			}
			if err != nil {
				return err
			}
			rows.ApplyRuns, rows.ApplyJobs = runs, jobs
			return nil
		})
	if err != nil {
		return MergedRows{}, err
	}
	return rows, nil
}

// ListDeployRunsStartedIn is the measures' deploys of [from, to): every deploy run on main of the
// deploy repository started in the window, whatever its event, with the jobs it is judged by
// (Production.JudgedJobNames). Whether a run reached production is decided on these runs and
// their jobs, never on the creation-time listing containment reads, so every run a window counts
// is judged.
func ListDeployRunsStartedIn(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time) (RunsWithJobs, error) {
	return ListRunsWithJobsStartedIn(ctx, pool, settings.DeployRepo, DeliveryRunKindDeploy, "main", "", from, to, settings.Production().JudgedJobNames())
}

// CIRows is the CI measures' runs of a window, each read by start over the window: the PR-checks
// workflow's pull_request runs with every job, which their job timings and failing jobs read
// whatever the names, and the deploy workflow's push runs on main with the jobs the deploy cards
// follow and no other.
type CIRows struct {
	Checks, Deploys RunsWithJobs
}

// ReadCIRows reads CIRows for [from, to) of the deploy repository, the two reads side by side;
// deployJobNames is every job name the deploy cards follow.
func ReadCIRows(ctx context.Context, pool *store.Pool, deployRepo string, from, to time.Time, deployJobNames []string) (CIRows, error) {
	var rows CIRows
	err := readSideBySide(ctx,
		func(ctx context.Context) (err error) {
			rows.Checks, err = ListRunsWithEveryJobStartedIn(ctx, pool, deployRepo, DeliveryRunKindPRChecks, "", "pull_request", from, to)
			return err
		},
		func(ctx context.Context) (err error) {
			rows.Deploys, err = ListRunsWithJobsStartedIn(ctx, pool, deployRepo, DeliveryRunKindDeploy, "main", "push", from, to, deployJobNames)
			return err
		})
	if err != nil {
		return CIRows{}, err
	}
	return rows, nil
}

// readSideBySide runs reads at once and answers the first error. Each runs on a hold mark of its
// own (store.ForConcurrentRead), so it takes one connection of the pool and one read's open cursor
// refuses no other; none holds a lock or opens a transaction, so none waits on another. A caller
// that holds a connection is refused (store.ErrNestedAcquire) before any read starts.
func readSideBySide(ctx context.Context, reads ...func(ctx context.Context) error) error {
	group, groupCtx := errgroup.WithContext(ctx)
	for _, read := range reads {
		readCtx, err := store.ForConcurrentRead(groupCtx)
		if err != nil {
			_ = group.Wait()
			return err
		}
		group.Go(func() error { return read(readCtx) })
	}
	return group.Wait()
}
