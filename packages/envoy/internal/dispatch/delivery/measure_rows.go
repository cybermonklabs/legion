package delivery

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rows the delivery and CI measures count, read by one function for every reader that counts
// them: the delivery routes (api's readDeliveryRows and readPipelinePopulation) read their
// window's, and the eval registry's day rows (evals.ReadDeliveryDay) one UTC day's, so a route and
// a day row count the same rows by the same reads, and a deploy's shipped pull requests by the
// same rule (Deploys.ShippedBy).

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
	return readMergedRows(ctx, pool, settings, from, to, nil, everyJob)
}

// readMergedRows is ReadMergedRows, and with createdBefore set only the apply runs GitHub created
// before it.
func readMergedRows(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time, createdBefore *time.Time, everyJob bool) (MergedRows, error) {
	var rows MergedRows
	err := readSideBySide(ctx,
		func(ctx context.Context) (err error) {
			rows.PullRequests, err = ListPullRequestsInWindow(ctx, pool, from, to)
			return err
		},
		func(ctx context.Context) error {
			applies, err := readApplyRows(ctx, pool, settings, from, createdBefore, everyJob)
			rows.ApplyRuns, rows.ApplyJobs = applies.Runs, applies.Jobs
			return err
		})
	if err != nil {
		return MergedRows{}, err
	}
	return rows, nil
}

// ApplyRows is MergedRows' apply runs and their jobs read once from a moment on (ReadApplyRows),
// for a reader of many windows that start at or after it, each of which takes its own part
// (MergedRowsWith): the refresher's walk over the days, whose every day's read would otherwise
// read every deploy run created from that day to now again.
type ApplyRows struct {
	from time.Time
	Runs []DeliveryRun
	Jobs map[int64][]DeliveryRunJob
}

// ReadApplyRows reads the apply runs and their production jobs (the jobs a deploy is judged by)
// of every window starting at or after from: MergedRows.ApplyRuns and ApplyJobs as ReadMergedRows
// reads them for a window from from.
func ReadApplyRows(ctx context.Context, pool *store.Pool, settings DeliverySettings, from time.Time) (ApplyRows, error) {
	return readApplyRows(ctx, pool, settings, from, nil, false)
}

// readApplyRows is ReadApplyRows, with createdBefore and everyJob as readMergedRows takes them.
func readApplyRows(ctx context.Context, pool *store.Pool, settings DeliverySettings, from time.Time, createdBefore *time.Time, everyJob bool) (ApplyRows, error) {
	runs, err := listRunsCreated(ctx, pool, settings.DeployRepo, DeliveryRunKindDeploy, "main", from, createdBefore)
	if err != nil {
		return ApplyRows{}, err
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
		return ApplyRows{}, err
	}
	return ApplyRows{from: from, Runs: runs, Jobs: jobs}, nil
}

// MergedRowsWith is ReadMergedRows of [from, to), its production jobs alone, with the apply rows
// taken from applies, read from from or earlier: the runs GitHub created at or after from, oldest
// creation first, and their jobs, the rows ReadMergedRows reads itself. Only the pull requests
// are read.
func MergedRowsWith(ctx context.Context, pool *store.Pool, applies ApplyRows, from, to time.Time) (MergedRows, error) {
	if from.Before(applies.from) {
		return MergedRows{}, fmt.Errorf("apply rows read from %s hold no run of a window from %s", applies.from.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	prs, err := ListPullRequestsInWindow(ctx, pool, from, to)
	if err != nil {
		return MergedRows{}, err
	}
	rows := MergedRows{PullRequests: prs, ApplyRuns: []DeliveryRun{}, ApplyJobs: map[int64][]DeliveryRunJob{}}
	for _, run := range applies.Runs {
		if run.Created().Before(from) {
			continue
		}
		rows.ApplyRuns = append(rows.ApplyRuns, run)
		if jobs, held := applies.Jobs[run.RunID]; held {
			rows.ApplyJobs[run.RunID] = jobs
		}
	}
	return rows, nil
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
	// Earlier is the pull requests merged before the window that Applies can ship, and the deploy
	// runs on main GitHub created before the window, which can ship them first: MergedRows from
	// since (ReadDeploys says what it is) up to the window's start, its apply runs those created
	// in that span alone. Empty where no pull request merged before the window can ship with one
	// of Applies.
	Earlier MergedRows
}

// ReadDeploys reads Deploys for [from, to). The earlier rows start at since: the latest creation
// among the deploys that succeeded before the first of Applies completed (readLatestShipBefore; the
// beginning of time where none did), since a pull request merged before it had shipped by then
// and so ships with none of Applies; or the creation of one of Applies, where one was created
// earlier still, so that run is among the apply runs. Nothing earlier is read where the window
// holds no deploy, or since is not before from.
func ReadDeploys(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time) (Deploys, error) {
	production := settings.Production()
	started, err := ListRunsWithJobsStartedIn(ctx, pool, settings.DeployRepo, DeliveryRunKindDeploy, "main", "", from, to, production.JudgedJobNames())
	if err != nil {
		return Deploys{}, err
	}
	deploys := Deploys{Started: started, Applies: ProductionApplies(started.Runs, started.Jobs, production).Applies}
	if len(deploys.Applies) == 0 {
		return deploys, nil
	}
	since, err := readLatestShipBefore(ctx, pool, settings.DeployRepo, production, deploys.Applies[0].CompletedAt)
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
	if deploys.Earlier, err = readMergedRows(ctx, pool, settings, since, from, &from, false); err != nil {
		return Deploys{}, err
	}
	return deploys, nil
}

// ShippedBy is, for each of d.Applies that ships a pull request, the pull requests it ships first
// (ContainingRun over the apply runs of window and of d.Earlier), whenever they merged: those of
// window (ReadMergedRows of d's window) and those merged before it, of d.Earlier, each list in that
// order. A deploy is one with pull requests when it ships one, whenever the pull request merged
// (dispatch://INFRA-2916): the deploy-frequency measure's with_prs (measures.Run.ShippedPRs) is
// this rule on the delivery routes and on the eval day rows alike. The pointers are into window's
// and d.Earlier's lists.
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
