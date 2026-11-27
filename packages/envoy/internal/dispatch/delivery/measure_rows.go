package delivery

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The rows the delivery and CI measures count, read by one function for every reader that counts
// them: the delivery routes (api's readDeliveryRows and readPipelinePopulation) read their
// window's (ListPullRequestsInWindow, and ReadWindowRuns of each source repository), and the eval
// registry's day rows (the refresher's read of the days a page names, evals' readDeliveryDays) each
// UTC day's, every day of a page at once (ListPullRequestsOnDays, and ReadDaysDeploys and
// ReadCIDays of each repository the rows are kept for as the deploy repository), each day's part
// what ListPullRequestsInWindow, ReadDeploys and ReadCIRows read of the day, so a route and a day
// row count the same rows by the same rules, and a deploy's shipped pull requests by the same rule
// (Deploys.ShippedBy). Every read of runs takes one source repository's rule (a DeliverySettings of
// DeployRules, dispatch://INFRA-978) and reads that repository's runs by it alone, so a reader of
// several repositories reads each under its own rule.
//
// The reads an answer makes from the evals' day rows (api's evals items) take q, the pool or a
// read-only transaction whose one snapshot the answer reads (evals.Store.BeginSnapshot). A read of
// several statements runs them side by side on the pool and one after the other in a transaction
// (ReadAll).

// ReadAll runs reads, the reads of one answer in q: on the pool side by side, each on a connection
// of its own (store.ReadSideBySide), and in a transaction one after the other, since its one
// connection runs one statement at a time and every read then reads its one snapshot. The eval
// registry's reads of a day's rows take it too (evals' readAttribution, ComputeAtRead).
func ReadAll(ctx context.Context, q Querier, reads ...func(ctx context.Context) error) error {
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
	// ReadMergedRows reads every such run (listRuns); ReadWindowRuns every one created up to the
	// bound past which no run ships a pull request merged before to first, with no bound for a
	// window ending at the present (windowApplyRunsCreated). A production job on another branch
	// ships no pull request. ApplyJobs is their jobs: the jobs a deploy is judged by
	// (Production.JudgedJobNames), which judge each run as its every job would, and with them,
	// for the timeline, each of the run's jobs that failed.
	ApplyRuns []DeliveryRun
	ApplyJobs map[int64][]DeliveryRunJob
}

// ReadMergedRows reads MergedRows for [from, to) in q: the pull requests, and every apply run
// followed by their production jobs, side by side on the pool (ReadAll).
func ReadMergedRows(ctx context.Context, q Querier, settings DeliverySettings, from, to time.Time) (MergedRows, error) {
	return readMergedRows(ctx, q, settings, from, to, "", nil)
}

// readMergedRows is ReadMergedRows, narrowed to repo's pull requests when repo is non-empty and,
// with createdBefore set, to only the apply runs GitHub created before it.
func readMergedRows(ctx context.Context, q Querier, settings DeliverySettings, from, to time.Time, repo string, createdBefore *time.Time) (MergedRows, error) {
	var rows MergedRows
	err := ReadAll(ctx, q,
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

// ApplyRows is MergedRows' apply runs and their jobs read once from a moment on (ReadApplyRows),
// for a reader of many windows that start at or after it, each of which takes its own part
// (Since): the refresher's walk over the days, whose every day's read would otherwise read every
// deploy run created from that day to now again. The apply runs of a page's days (ReadDaysDeploys)
// are read the bounded way the routes read a window's (windowApplyRunsCreated) instead, every
// day's window in one statement, and only those days take a part of them (windows).
type ApplyRows struct {
	from time.Time
	// windows is each day whose window's apply runs a read of several days holds
	// (DaysDeploys.Applies), by its Unix time, nil for rows that hold every run created from from
	// on.
	windows map[int64]bool
	Runs    []DeliveryRun
	Jobs    map[int64][]DeliveryRunJob
}

// ReadApplyRows reads the apply runs and their production jobs (the jobs a deploy is judged by)
// of every window starting at or after from: MergedRows.ApplyRuns and ApplyJobs as ReadMergedRows
// reads them for a window from from.
func ReadApplyRows(ctx context.Context, q Querier, settings DeliverySettings, from time.Time) (ApplyRows, error) {
	return readApplyRows(ctx, q, settings, from, nil)
}

// readApplyRows is ReadApplyRows, with createdBefore as readMergedRows takes it.
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
	return ApplyRows{from: from, Runs: runs, Jobs: jobs}, nil
}

// ReadWindowRuns is a delivery route's read of one source repository's runs for [from, to), under
// that repository's rule (settings): MergedRows' apply runs and their jobs, with failedJobs each
// apply run's failed jobs beside its production jobs, which the timeline lists each deploy run with
// (its failed jobs and the job that failed first, FailedJobs and RootFailingJob, can be any job,
// and keep the failed ones alone), and with deploys the window's Deploys, as ReadDeploys reads
// them. The window's pull requests are every repository's (ListPullRequestsInWindow), which a route
// reading several source repositories reads once beside each repository's runs, so this reads
// none. The two lists of runs are nearly one, the runs of the window, so they are read in one
// statement (listWindowRuns) and the production jobs of both in one more, where reading each list
// and its jobs apart read the window's production jobs twice, three times the window's for the
// measures of a compare range, whose apply runs reached to the present (dispatch://INFRA-2916, R8).
// The apply runs stop at the bound past which no run ships first a pull request of the window or
// of Deploys.Earlier (windowApplyRunsCreated), so every pull request's deploy is the one
// ReadMergedRows' runs give it.
func ReadWindowRuns(ctx context.Context, pool *store.Pool, settings DeliverySettings, from, to time.Time, failedJobs, deploys bool) (RunsWithJobs, Deploys, error) {
	applies, started, err := readWindowRuns(ctx, pool, settings, from, to, failedJobs, deploys)
	if err != nil || !deploys {
		return applies, Deploys{}, err
	}
	window, err := deploysOf(ctx, pool, settings, from, started)
	if err != nil {
		return RunsWithJobs{}, Deploys{}, err
	}
	return applies, window, nil
}

// readWindowRuns reads ReadWindowRuns' runs with their jobs: the apply runs, and with deploys the
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
		applyIDs := runIDsOf(listed.apply)
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

// Since is the apply rows of a window from from, taken from a, read from from or earlier: the runs
// GitHub created at or after from, oldest creation first, and their jobs, the rows ReadApplyRows
// reads itself from from. Rows a read of several days holds (DaysDeploys.Applies) answer only a
// window from one of those days: the runs of that day's window, which are every run its rows'
// answers read, and those of the later days' windows created after it.
func (a ApplyRows) Since(from time.Time) (ApplyRows, error) {
	if from.Before(a.from) {
		return ApplyRows{}, fmt.Errorf("apply rows read from %s hold no run of a window from %s", a.from.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	if a.windows != nil && !a.windows[from.Unix()] {
		return ApplyRows{}, fmt.Errorf("apply rows read of %d days' windows hold no window from %s", len(a.windows), from.Format(time.RFC3339))
	}
	rows := ApplyRows{from: from, Runs: []DeliveryRun{}, Jobs: map[int64][]DeliveryRunJob{}}
	for _, run := range a.Runs {
		if run.Created().Before(from) {
			continue
		}
		rows.Runs = append(rows.Runs, run)
		if jobs, held := a.Jobs[run.RunID]; held {
			rows.Jobs[run.RunID] = jobs
		}
	}
	return rows, nil
}

// HistoryRows is what every day of the eval registry's delivery and CI day rows reads but its CI
// runs, read once over the whole history: every population pull request merged before a moment,
// in ListPullRequestsInWindow's order, and, for each repository asked for as the deploy
// repository, every deploy run on main of it, whatever its event, oldest start first and those
// started in one instant by run id (listRunsStartedIn's order), with its jobs of the names asked
// for, each run's in ListRunJobs' order. A day's deploys, its apply runs and its deploy cards'
// push runs are all among those runs, so a reader that builds every day at once (the day-row
// refresher's rewrite of a settings change, evals' rewriteSettings) gives each day its part, where
// a read of each day would read every deploy run created from that day on again. No limit bounds
// it, as one bounds a window's reads (MaxPullRequestsPerWindow, MaxRunsPerWindow): the whole
// history is what such a reader builds, and a limit would cut it short without a word.
type HistoryRows struct {
	PullRequests []DeliveryPullRequest
	// Deploys is each repository's deploy runs and their jobs, by the repository.
	Deploys map[string]RunsWithJobs
}

// ReadHistoryRows reads HistoryRows of deployRepos: the pull requests merged before to, and each
// repository's deploy runs with their jobs named names, every read side by side.
func ReadHistoryRows(ctx context.Context, pool *store.Pool, deployRepos []string, names []string, to time.Time) (HistoryRows, error) {
	rows := HistoryRows{Deploys: make(map[string]RunsWithJobs, len(deployRepos))}
	deploys := make([]RunsWithJobs, len(deployRepos))
	reads := []func(context.Context) error{func(ctx context.Context) (err error) {
		rows.PullRequests, err = listPullRequestsMergedBefore(ctx, pool, to)
		return err
	}}
	for i, repo := range deployRepos {
		reads = append(reads,
			func(ctx context.Context) (err error) {
				deploys[i].Runs, err = listDeployRunsOnMain(ctx, pool, repo)
				return err
			},
			func(ctx context.Context) (err error) {
				deploys[i].Jobs, err = listDeployRunJobsOnMain(ctx, pool, repo, names)
				return err
			})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return HistoryRows{}, err
	}
	for i, repo := range deployRepos {
		rows.Deploys[repo] = deploys[i]
	}
	return rows, nil
}

// ReadPullRequestsMergedBefore reads HistoryRows' pull requests alone: every population pull
// request merged before to, in ListPullRequestsInWindow's order, for a reader that attributes them
// beside its read of the deploy runs (ReadDeployHistory).
func ReadPullRequestsMergedBefore(ctx context.Context, pool *store.Pool, to time.Time) ([]DeliveryPullRequest, error) {
	return listPullRequestsMergedBefore(ctx, pool, to)
}

// ReadDeployHistory reads HistoryRows' Deploys alone, the same answer ReadHistoryRows gives of
// deployRepos and names (each repository's deploy runs on main, and the jobs named names of each
// run), read another way: each repository's runs listed on a connection of its own, and the jobs of
// those runs read by their ids (ListRunJobsForRuns), runsPerRead runs a statement, by readers
// readers side by side, each taking the next runsPerRead runs as the lists read them, so the jobs
// of a list's first runs are read while it reads on. A read of the jobs of every deploy run on main
// by a subselect (what ReadHistoryRows does) passed, through delivery_run_jobs' primary key (repo,
// run_id, name), over every run of the repository, PR-checks runs among them, whose 24,000 jobs a
// day at ten times the stated run rate it read for the few it kept: 190 to 500 ms of the 2.2 s of
// a settings rewrite there (dispatch://INFRA-2916). Read by the ids, the jobs are an index-only
// scan of delivery_run_jobs_named (runJobsNamedForRunsQuery), a few leaf pages for each name. It
// answers too how long the lists of the runs took, the jobs of their last runs being read after
// them.
func ReadDeployHistory(ctx context.Context, pool *store.Pool, deployRepos, names []string, readers, runsPerRead int) (map[string]RunsWithJobs, time.Duration, error) {
	began := time.Now()
	type read struct {
		repo int
		ids  []int64
		jobs map[int64][]DeliveryRunJob
	}
	deploys := make([]RunsWithJobs, len(deployRepos))
	listsTook := make([]time.Duration, len(deployRepos))
	// queue is the runs the lists have read whose jobs no reader has taken yet, runsPerRead runs a
	// read, and lists how many lists have not ended. A list hands its runs on and never waits for a
	// reader, so it holds its connection only while it reads, and a reader waits for a list holding
	// none: no read waits on a connection another read holds while it waits.
	var queued sync.Mutex
	ready := sync.NewCond(&queued)
	var queue []read
	lists := len(deployRepos)
	reads := make([]func(context.Context) error, 0, len(deployRepos)+max(readers, 1))
	for i, repo := range deployRepos {
		deploys[i] = RunsWithJobs{Runs: []DeliveryRun{}, Jobs: make(map[int64][]DeliveryRunJob)}
		reads = append(reads, func(ctx context.Context) error {
			defer func() {
				queued.Lock()
				lists--
				queued.Unlock()
				ready.Broadcast()
			}()
			var ids []int64
			hand := func() {
				queued.Lock()
				queue = append(queue, read{repo: i, ids: ids})
				queued.Unlock()
				ready.Signal()
				ids = nil
			}
			rows, err := pool.Query(ctx, `select `+RunColumns+` `+deployRunsOnMain+` order by started_at, run_id`, repo)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				run, err := ScanRun(rows)
				if err != nil {
					return err
				}
				deploys[i].Runs = append(deploys[i].Runs, run)
				if ids = append(ids, run.RunID); len(ids) == max(runsPerRead, 1) {
					hand()
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			listsTook[i] = time.Since(began)
			if len(ids) > 0 {
				hand()
			}
			return nil
		})
	}
	done := make([][]read, max(readers, 1))
	for d := range done {
		reads = append(reads, func(ctx context.Context) error {
			for {
				queued.Lock()
				for len(queue) == 0 && lists > 0 {
					ready.Wait()
				}
				if len(queue) == 0 {
					queued.Unlock()
					return nil
				}
				one := queue[0]
				queue = queue[1:]
				queued.Unlock()
				jobs, err := ListRunJobsForRuns(ctx, pool, deployRepos[one.repo], one.ids, names)
				if err != nil {
					return err
				}
				one.jobs = jobs
				done[d] = append(done[d], one)
			}
		})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return nil, 0, err
	}
	for _, reader := range done {
		for _, one := range reader {
			maps.Copy(deploys[one.repo].Jobs, one.jobs)
		}
	}
	byRepo := make(map[string]RunsWithJobs, len(deployRepos))
	for i, repo := range deployRepos {
		byRepo[repo] = deploys[i]
	}
	var runsRead time.Duration
	if len(listsTook) > 0 {
		runsRead = slices.Max(listsTook)
	}
	return byRepo, runsRead, nil
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
// window (ReadMergedRows, or ListPullRequestsInWindow and ReadWindowRuns, of d's window) and those
// merged before it, of
// d.Earlier, each list in that order. A deploy is one with pull requests when it ships one,
// whenever the pull request merged (dispatch://INFRA-2916): the deploy-frequency measure's
// with_prs (measures.Run.ShippedPRs) is this rule on the delivery routes and on the eval day rows
// alike. The pointers are into window's and d.Earlier's lists.
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
// on the pool (ReadAll); deployJobNames is every job name the deploy cards follow.
func ReadCIRows(ctx context.Context, q Querier, deployRepo string, from, to time.Time, deployJobNames []string) (CIRows, error) {
	var rows CIRows
	err := ReadAll(ctx, q,
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

// prChecksCompletedInQuery is ReadPRChecksCompletedIn's read of the runs: the PR-checks runs of a
// window as listRunsStartedIn lists them (kind $2, event $3, started in [$4, $5)), of those the
// ones completed in [$6, $7], oldest start first and those started in one instant by run id. The
// runs are read by their completion alone, one range of delivery_runs_kind_completed_at (migration
// 20261123042253), (repo, kind, completed_at), in a materialized CTE that names no start, so the
// read reads the runs of those hours alone; the start is checked on the runs it answers, which are
// then ordered and bounded. Named beside the completion, the start's range and the order by it let
// the planner read delivery_runs_kind_started_at's range of every run started in the window
// instead, in the order asked for, and test each run's completion on the way: the plan it takes
// where it holds no statistics of the two ranges, as under a generic plan, which reads every run
// of 84 days to answer a day's (dispatch://INFRA-2918, R12; api's
// TestTheFirstPaintsReadsReadNoEventsAndScanNoIssues).
const prChecksCompletedInQuery = `
		with completed as materialized (
			select ` + RunColumns + `
			from delivery_runs
			where repo = $1 and kind = $2 and event = $3 and completed_at >= $6 and completed_at <= $7
		)
		select ` + RunColumns + `
		from completed
		where started_at >= $4 and started_at < $5
		order by started_at, run_id
		limit $8`

// ReadPRChecksCompletedIn reads the PR-checks workflow's pull_request runs of the deploy repository
// in q that started in [from, to), the runs ReadCIRows reads of that window, and completed in
// [completedFrom, completedTo], both ends counted as measures.FailingJobs counts a completion, each
// with every job it ran: the runs of a window whose jobs the failing-jobs tally reads, read by their
// completion rather than as every run of the window (dispatch://INFRA-2918, D9), so the read of
// their jobs reads the jobs of a day's runs, never those of every run of an 84-day window.
// Bounded by MaxRunsPerWindow, as listRunsStartedIn is.
func ReadPRChecksCompletedIn(ctx context.Context, q Querier, deployRepo string, from, to, completedFrom, completedTo time.Time) (RunsWithJobs, error) {
	rows, err := q.Query(ctx, prChecksCompletedInQuery, deployRepo, string(DeliveryRunKindPRChecks), "pull_request",
		from, to, completedFrom, completedTo, MaxRunsPerWindow)
	if err != nil {
		return RunsWithJobs{}, err
	}
	runs, err := collectRuns(rows)
	if err != nil {
		return RunsWithJobs{}, err
	}
	runIDs := make([]int64, len(runs))
	for i, run := range runs {
		runIDs[i] = run.RunID
	}
	jobs, err := listEveryRunJobForRuns(ctx, q, deployRepo, runIDs)
	if err != nil {
		return RunsWithJobs{}, err
	}
	return RunsWithJobs{Runs: runs, Jobs: jobs}, nil
}

// The reads of several UTC days at once, the day-row refresher's read of the days a page names
// (evals' readDeliveryDays): each read one day of the page needs is made once for every day of
// the page rather than once a day, so a page naming N days makes as many statements of each as a
// page naming one, and each answers, for each day, what the read of that day alone answers:
// ListPullRequestsOnDays what ListPullRequestsInWindow answers, ReadDaysDeploys what ReadDeploys
// answers and the apply runs each day takes its part of (DaysDeploys.Applies), and ReadCIDays what
// ReadCIRows answers. A day is the window [day, day+1) of a UTC midnight, and the days are given in
// order, each once; each answer is index-aligned with them. A read of several days answers what
// the day's own read answers only up to the bound of a window's pull requests
// (MaxPullRequestsPerWindow): where a day's window, or the earlier rows of a day's deploys
// (Deploys.Earlier), holds more, ListPullRequestsOnDays and ReadDaysDeploys refuse the read, and
// the day's own read answers its oldest pull requests up to the bound (listPullRequestsInWindows).

// daysSideBySide is how many days' reads of their own the reads of several days run at once, each
// on a connection of its own: ReadCIDays' read of every job of each day's PR-checks runs, the one
// read of a day whose rows grow with the day's runs rather than with the page's days (24,000 jobs a
// day at ten times the stated run rate), which one statement for every day would read on one
// connection.
const daysSideBySide = 4

// dayWindows is the window of each of days, [day, day+1), as the two lists of their starts and
// ends a statement unnests; days in order and each once, or an error.
func dayWindows(days []time.Time) (from, to []time.Time, err error) {
	from, to = make([]time.Time, len(days)), make([]time.Time, len(days))
	for i, day := range days {
		if i > 0 && !day.After(days[i-1]) {
			return nil, nil, fmt.Errorf("the days of a read of several days are in order and each once: %s follows %s",
				day.Format(time.DateOnly), days[i-1].Format(time.DateOnly))
		}
		from[i], to[i] = day, day.AddDate(0, 0, 1)
	}
	return from, to, nil
}

// dayOf is the index of the window of from and to (dayWindows) that holds at, false for none.
func dayOf(from, to []time.Time, at time.Time) (int, bool) {
	i, _ := slices.BinarySearchFunc(from, at, func(start, at time.Time) int { return start.Compare(at) })
	if i < len(from) && from[i].Equal(at) {
		return i, true
	}
	if i > 0 && at.Before(to[i-1]) {
		return i - 1, true
	}
	return 0, false
}

// readEachDay runs read for each of days days, daysSideBySide at once, each on a hold mark of its
// own (store.ForConcurrentRead), as store.ReadSideBySide runs its reads, and answers the first
// error.
func readEachDay(ctx context.Context, days int, read func(ctx context.Context, day int) error) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(daysSideBySide)
	for day := range days {
		readCtx, err := store.ForConcurrentRead(groupCtx)
		if err != nil {
			_ = group.Wait()
			return err
		}
		group.Go(func() error { return read(readCtx, day) })
	}
	return group.Wait()
}

// ListPullRequestsOnDays lists, for each of days, what ListPullRequestsInWindow lists of its
// window, in one statement, and refuses the read where a day holds more than
// MaxPullRequestsPerWindow pull requests (listPullRequestsInWindows).
func ListPullRequestsOnDays(ctx context.Context, q Querier, days []time.Time) ([][]DeliveryPullRequest, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return nil, err
	}
	return listPullRequestsInWindows(ctx, q, from, to, "")
}

// listPullRequestsInWindows lists, for each window [from[i], to[i]), what ListPullRequestsInWindow
// lists of it, in its order, in one statement, a pull request of two windows in each; narrowed to
// repo's pull requests when repo is non-empty, as readMergedRows narrows a window's earlier rows
// (Deploys.Earlier). Each window is bounded on its own: the statement reads at most
// MaxPullRequestsPerWindow+1 pull requests of each window, whatever the others hold, and a window
// that holds more than MaxPullRequestsPerWindow refuses the whole read, naming the window and the
// bound, where ListPullRequestsInWindow answers the oldest MaxPullRequestsPerWindow. Which of a
// window's rows that read takes does not matter, since it answers them only where they are every
// one of the window's. A limit the windows shared (MaxPullRequestsPerWindow times their number)
// let a window take the share another left unused, and past it kept the oldest of every window's
// together without a word, leaving out a window's newest, which its deploys ship
// (dispatch://INFRA-2916).
func listPullRequestsInWindows(ctx context.Context, q Querier, from, to []time.Time, repo string) ([][]DeliveryPullRequest, error) {
	listed := make([][]DeliveryPullRequest, len(from))
	for i := range listed {
		listed[i] = []DeliveryPullRequest{}
	}
	if len(from) == 0 {
		return listed, nil
	}
	rows, err := q.Query(ctx, `
		select `+PullRequestColumns+`, d.i
		from unnest($1::timestamptz[], $2::timestamptz[]) with ordinality as d(from_at, to_at, i)
		cross join lateral (
			select `+PullRequestColumns+`
			from delivery_pull_requests
			where merged_at >= d.from_at and merged_at < d.to_at and ($4 = '' or repo = $4)
			limit $3
		) p
		order by d.i, p.merged_at, (p.repo || '#' || p.number) collate "C"
	`, from, to, MaxPullRequestsPerWindow+1, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var window int
		pr, err := scanPullRequest(rows, &window)
		if err != nil {
			return nil, err
		}
		w := window - 1
		if len(listed[w]) == MaxPullRequestsPerWindow {
			what := "pull requests"
			if repo != "" {
				what += " of " + repo
			}
			return nil, fmt.Errorf("more than %d %s merged in [%s, %s), the bound of a window's read", MaxPullRequestsPerWindow, what,
				from[w].Format(time.RFC3339Nano), to[w].Format(time.RFC3339Nano))
		}
		listed[w] = append(listed[w], pr)
	}
	return listed, rows.Err()
}

// listRunsStartedOnDays lists, for each window [from[i], to[i]) of a list of days (dayWindows),
// what listRunsStartedIn lists of it, in one statement.
func listRunsStartedOnDays(ctx context.Context, q Querier, repo string, kind DeliveryRunKind, branch, event string, from, to []time.Time) ([][]DeliveryRun, error) {
	listed := make([][]DeliveryRun, len(from))
	for i := range listed {
		listed[i] = []DeliveryRun{}
	}
	if len(from) == 0 {
		return listed, nil
	}
	rows, err := q.Query(ctx, `
		select `+RunColumns+`, d.i
		from unnest($3::timestamptz[], $4::timestamptz[]) with ordinality as d(from_at, to_at, i)
		join delivery_runs r on r.repo = $1 and r.kind = $2 and r.started_at >= d.from_at and r.started_at < d.to_at
		where ($5 = '' or r.head_branch = $5) and ($6 = '' or r.event = $6)
		order by r.started_at, r.run_id
		limit $7
	`, repo, string(kind), from, to, branch, event, MaxRunsPerWindow*len(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var window int
		run, err := scanRun(rows, &window)
		if err != nil {
			return nil, err
		}
		listed[window-1] = append(listed[window-1], run)
	}
	return listed, rows.Err()
}

// listDeployRunsCreatedInWindows lists, for each window [from[i], to[i]), what listRuns lists of
// repo's deploy runs on main created in it, in one statement, a run of two windows in each.
func listDeployRunsCreatedInWindows(ctx context.Context, q Querier, repo string, from, to []time.Time) ([][]DeliveryRun, error) {
	listed := make([][]DeliveryRun, len(from))
	for i := range listed {
		listed[i] = []DeliveryRun{}
	}
	if len(from) == 0 {
		return listed, nil
	}
	rows, err := q.Query(ctx, `
		select `+RunColumns+`, d.i
		from unnest($2::timestamptz[], $3::timestamptz[]) with ordinality as d(from_at, to_at, i)
		join delivery_runs r on r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main'
			and `+runCreatedAt+` >= d.from_at and `+runCreatedAt+` < d.to_at
		order by d.i, `+runCreatedAt+`, r.run_id
		limit $4
	`, repo, from, to, MaxRunsPerWindow*len(from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var window int
		run, err := scanRun(rows, &window)
		if err != nil {
			return nil, err
		}
		listed[window-1] = append(listed[window-1], run)
	}
	return listed, rows.Err()
}

// daysRunsQuery is ReadDaysDeploys' read of the runs of a list of days: the deploy runs on main of
// $1 that are an apply run of a day's window (applyRunsCreated, the window's bound read once a
// day) or started on a day, each once, oldest creation first, $2 and $3 the days' starts and ends
// and $4 and $5 Production.JobNames and Production.ApplyStep. Each day's two parts are ranges of
// delivery_runs_kind_created and delivery_runs_kind_started_at.
//
// The days' lists (daysRunsListed) are a materialized CTE, built once whatever join the planner
// takes for the runs they name. Inside the IN of the read of the runs, a planner that estimated one
// deploy run, as it does over a fresh database's tables no ANALYZE has read, took a nested loop
// semi join of each deploy run to the lists and built every day's lists, and each day's bound, again
// for each run. A bound that finds no production job succeeded after its day walks every later run
// and probes its jobs, so with the jobs not yet committed in the read's snapshot and their probe
// planned while delivery_run_jobs was nearly empty (the repository's jobs read for each probe) the
// read of 278 runs over 17 days probed the jobs 77,284 times and took 29 s, the eval day-row
// refresher's pass waiting on it (dispatch://INFRA-2916).
var daysRunsQuery = `
		with listed_runs as materialized (` + daysRunsListed + `
		)
		select ` + RunColumns + `
		from delivery_runs r
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.run_id in (select run_id from listed_runs)
		order by ` + runCreatedAt + `, r.run_id
		limit $6`

// daysRunsListed is the run ids daysRunsQuery reads, each day's apply runs and the runs started on
// it, over the alias d of the day's window.
var daysRunsListed = `
			select listed.run_id
			from unnest($2::timestamptz[], $3::timestamptz[]) as d(from_at, to_at)
			cross join lateral (
				select r.run_id from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and ` + applyRunsCreated("d.from_at", "d.to_at") + `
				union all
				select r.run_id from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.started_at >= d.from_at and r.started_at < d.to_at
			) as listed`

// DaysDeploys is what ReadDaysDeploys reads of several days of one deploy repository: each day's
// Deploys, index-aligned with the days, and, where it was asked for them, the apply runs of every
// day's window (Applies).
type DaysDeploys struct {
	Deploys []Deploys
	// Applies is the apply runs of each day's window as ReadWindowRows bounds them
	// (windowApplyRunsCreated), with the runs started on the days, and their production jobs (the
	// jobs a deploy is judged by). A day takes its part (ApplyRows.Since): every run of its window,
	// which holds the run that first ships each pull request merged before the window's end, and
	// some runs created after that bound, which ship first none of those, so it answers what the
	// day's read of every run created from its start on (ReadApplyRows) answers.
	Applies ApplyRows
}

// ReadDaysDeploys reads, for each of days, what ReadDeploys reads of its window of
// settings.DeployRepo, and with applies the apply runs of every day's window (DaysDeploys.Applies):
// the runs of every day in one statement (daysRunsQuery, or listRunsStartedOnDays' alone without
// applies) and their production jobs in one more; the latest ship before each day's first deploy
// completes in one (latestShipBefore of each); and the earlier rows each day's deploys read
// (Deploys.Earlier), the deploy repository's pull requests of every day in one statement and the
// apply runs in one, side by side, and their jobs in one more. Where a day's earlier rows hold more
// than MaxPullRequestsPerWindow of those pull requests, the read is refused, naming the day's
// earlier window and the bound (listPullRequestsInWindows), where ReadDeploys answers the oldest up
// to the bound.
func ReadDaysDeploys(ctx context.Context, pool *store.Pool, settings DeliverySettings, days []time.Time, applies bool) (DaysDeploys, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return DaysDeploys{}, err
	}
	read := DaysDeploys{Deploys: make([]Deploys, len(days))}
	if len(days) == 0 {
		return read, nil
	}
	repo, production := settings.DeployRepo, settings.Production()
	names := production.JudgedJobNames()
	var runs []DeliveryRun
	if applies {
		rows, err := pool.Query(ctx, daysRunsQuery, repo, from, to, production.JobNames, production.ApplyStep, MaxRunsPerWindow*len(days))
		if err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
		if runs, err = collectRuns(rows); err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
	} else {
		started, err := listRunsStartedOnDays(ctx, pool, repo, DeliveryRunKindDeploy, "main", "", from, to)
		if err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
		runs = slices.Concat(started...)
	}
	ids := make([]int64, len(runs))
	for i, run := range runs {
		ids[i] = run.RunID
	}
	jobs, err := ListRunJobsForRuns(ctx, pool, repo, ids, names)
	if err != nil {
		return DaysDeploys{}, fmt.Errorf("read the production jobs of %d deploy runs: %w", len(ids), err)
	}
	if applies {
		read.Applies = ApplyRows{from: days[0], windows: make(map[int64]bool, len(days)), Runs: runs, Jobs: jobs}
		for _, day := range days {
			read.Applies.windows[day.Unix()] = true
		}
	}
	started := make([]RunsWithJobs, len(days))
	for i := range started {
		started[i] = RunsWithJobs{Runs: []DeliveryRun{}, Jobs: map[int64][]DeliveryRunJob{}}
	}
	for _, run := range runs {
		if i, held := dayOf(from, to, run.StartedAt); held {
			started[i].Runs = append(started[i].Runs, run)
			if own, named := jobs[run.RunID]; named {
				started[i].Jobs[run.RunID] = own
			}
		}
	}
	// The days whose deploys can ship pull requests merged before them, and when each first deploy
	// completed (ReadDeploys' rule).
	var shipping []int
	var completed []time.Time
	for i := range days {
		slices.SortFunc(started[i].Runs, func(a, b DeliveryRun) int {
			return cmp.Or(a.StartedAt.Compare(b.StartedAt), cmp.Compare(a.RunID, b.RunID))
		})
		read.Deploys[i] = Deploys{Started: started[i], Applies: ProductionApplies(started[i].Runs, started[i].Jobs, production).Applies}
		if len(read.Deploys[i].Applies) > 0 {
			shipping, completed = append(shipping, i), append(completed, read.Deploys[i].Applies[0].CompletedAt)
		}
	}
	if len(shipping) == 0 {
		return read, nil
	}
	rows, err := pool.Query(ctx, `
		select (`+latestShipBefore("$1", "$2", "$3", "d.at")+`)
		from unnest($4::timestamptz[]) with ordinality as d(at, i)
		order by d.i
	`, repo, production.JobNames, production.ApplyStep, completed)
	if err != nil {
		return DaysDeploys{}, fmt.Errorf("read the last deploy of %s to succeed before %d days' first deploys: %w", repo, len(shipping), err)
	}
	latest, err := pgx.CollectRows(rows, pgx.RowTo[*time.Time])
	if err != nil {
		return DaysDeploys{}, fmt.Errorf("read the last deploy of %s to succeed before %d days' first deploys: %w", repo, len(shipping), err)
	}
	// The window of each day's earlier rows: from since, the latest ship (the beginning of time
	// where none succeeded) or the creation of a deploy of the day created earlier still, up to
	// the day's start; none where since is not before it.
	var earlier []int
	var earlierFrom, earlierTo []time.Time
	for k, i := range shipping {
		var since time.Time
		if latest[k] != nil {
			since = latest[k].UTC()
		}
		for _, apply := range read.Deploys[i].Applies {
			if apply.CreatedAt.Before(since) {
				since = apply.CreatedAt
			}
		}
		if since.Before(from[i]) {
			earlier, earlierFrom, earlierTo = append(earlier, i), append(earlierFrom, since), append(earlierTo, from[i])
		}
	}
	if len(earlier) == 0 {
		return read, nil
	}
	var prs [][]DeliveryPullRequest
	var earlierRuns [][]DeliveryRun
	var earlierJobs map[int64][]DeliveryRunJob
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			prs, err = listPullRequestsInWindows(ctx, pool, earlierFrom, earlierTo, repo)
			return err
		},
		func(ctx context.Context) (err error) {
			if earlierRuns, err = listDeployRunsCreatedInWindows(ctx, pool, repo, earlierFrom, earlierTo); err != nil {
				return err
			}
			var ids []int64
			seen := map[int64]bool{}
			for _, run := range slices.Concat(earlierRuns...) {
				if !seen[run.RunID] {
					seen[run.RunID], ids = true, append(ids, run.RunID)
				}
			}
			earlierJobs, err = ListRunJobsForRuns(ctx, pool, repo, ids, names)
			return err
		}); err != nil {
		return DaysDeploys{}, fmt.Errorf("read the rows before %d days that their deploys can ship: %w", len(earlier), err)
	}
	for k, i := range earlier {
		own := make(map[int64][]DeliveryRunJob, len(earlierRuns[k]))
		for _, run := range earlierRuns[k] {
			if held, named := earlierJobs[run.RunID]; named {
				own[run.RunID] = held
			}
		}
		read.Deploys[i].Earlier = MergedRows{PullRequests: prs[k], ApplyRuns: earlierRuns[k], ApplyJobs: own}
	}
	return read, nil
}

// ReadCIDays reads, for each of days, what ReadCIRows reads of its window of deployRepo: the
// PR-checks runs of every day in one statement and every job of each day's in a statement of its
// own, daysSideBySide days at once; and beside them the deploy push runs of every day in one
// statement and their jobs the deploy cards follow in one more.
func ReadCIDays(ctx context.Context, pool *store.Pool, deployRepo string, days []time.Time, deployJobNames []string) ([]CIRows, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return nil, err
	}
	read := make([]CIRows, len(days))
	if len(days) == 0 {
		return read, nil
	}
	err = store.ReadSideBySide(ctx,
		func(ctx context.Context) error {
			checks, err := listRunsStartedOnDays(ctx, pool, deployRepo, DeliveryRunKindPRChecks, "", "pull_request", from, to)
			if err != nil {
				return err
			}
			return readEachDay(ctx, len(days), func(ctx context.Context, day int) error {
				ids := make([]int64, len(checks[day]))
				for i, run := range checks[day] {
					ids[i] = run.RunID
				}
				jobs, err := listEveryRunJobForRuns(ctx, pool, deployRepo, ids)
				if err != nil {
					return err
				}
				read[day].Checks = RunsWithJobs{Runs: checks[day], Jobs: jobs}
				return nil
			})
		},
		func(ctx context.Context) error {
			pushes, err := listRunsStartedOnDays(ctx, pool, deployRepo, DeliveryRunKindDeploy, "main", "push", from, to)
			if err != nil {
				return err
			}
			var ids []int64
			for _, runs := range pushes {
				for _, run := range runs {
					ids = append(ids, run.RunID)
				}
			}
			jobs, err := ListRunJobsForRuns(ctx, pool, deployRepo, ids, deployJobNames)
			if err != nil {
				return err
			}
			for day, runs := range pushes {
				own := make(map[int64][]DeliveryRunJob, len(runs))
				for _, run := range runs {
					if held, named := jobs[run.RunID]; named {
						own[run.RunID] = held
					}
				}
				read[day].Deploys = RunsWithJobs{Runs: runs, Jobs: own}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return read, nil
}
