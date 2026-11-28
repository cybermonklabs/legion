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
// (Deploys.ShippedBy). Each day has its own pull request and run bounds; a day past a bound is
// refused alone and by name. Earlier apply runs still take the oldest MaxRunsPerWindow, as in
// ReadDeploys. Every read of runs takes one source repository's rule (a DeliverySettings of
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
// day's window in one statement, and only the days that read answered take a part of them
// (windows).
type ApplyRows struct {
	from time.Time
	// windows is each answered day of a read of several days (DaysDeploys.Applies), whose part
	// Since answers, by its Unix time; nil for rows that hold every run created from from on.
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
// window from an answered day: the runs of that day's window, which are every run its rows'
// answers read, and those of the later answered days' windows created after it.
// The part keeps only the answered days at or after from, so a later Since still requires an
// answered day. A part of ReadApplyRows has nil windows and answers any window at or after from.
//
// a's runs come oldest creation first (runCreatedAt, as every read of them orders them), so those
// created from from on are a's last ones, found by a binary search, and the answer's runs are that
// end of a's, which it shares rather than copies; its jobs are those of its runs alone. A reader of
// many days (the read-time path of the delivery evals, evals.ComputeAtRead, and the refresher's
// read of a page's days) takes a part of one read for each day, so a copy of every run created from
// each day on, grown run by run, made its reads of many days grow with the days times the runs.
func (a ApplyRows) Since(from time.Time) (ApplyRows, error) {
	if from.Before(a.from) {
		return ApplyRows{}, fmt.Errorf("apply rows read from %s hold no run of a window from %s", a.from.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	var windows map[int64]bool
	if a.windows != nil {
		fromUnix := from.Unix()
		if !a.windows[fromUnix] {
			return ApplyRows{}, fmt.Errorf("apply rows read of %d days' windows hold no window from %s", len(a.windows), from.Format(time.RFC3339))
		}
		windows = make(map[int64]bool)
		for day, answered := range a.windows {
			if day >= fromUnix {
				windows[day] = answered
			}
		}
	}
	first, _ := slices.BinarySearchFunc(a.Runs, from, func(run DeliveryRun, from time.Time) int {
		if run.Created().Before(from) {
			return -1
		}
		return 1
	})
	rows := ApplyRows{from: from, windows: windows, Runs: a.Runs[first:len(a.Runs):len(a.Runs)], Jobs: make(map[int64][]DeliveryRunJob, len(a.Runs)-first)}
	if rows.Runs == nil {
		rows.Runs = []DeliveryRun{}
	}
	for _, run := range rows.Runs {
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
// ReadCIRows answers. A day is the window [day, day+1) of a UTC midnight, and each day's window
// starts at or after the end of the one before it; each answer is index-aligned with the days.
// Each day is bounded on its own, by MaxPullRequestsPerWindow for its merged pull requests and by
// MaxRunsPerWindow for each list of its own runs. A day past either bound, or whose window of the
// earlier rows of its deploys (Deploys.Earlier) holds more than MaxPullRequestsPerWindow of the
// deploy repository's pull requests, is refused alone and by name (RefusedWindow), with none of
// the rows the refused window decides, and every other day of the read is answered whole. A day's
// earlier apply runs refuse nothing: they answer the oldest MaxRunsPerWindow, as listRuns does for
// one window.

// RefusedRows identifies the rows that exceeded a window's bound. Its zero value denotes pull
// requests merged in the window.
type RefusedRows string

const (
	RefusedPRChecksRuns RefusedRows = "PR-checks runs"
	RefusedDeployRuns   RefusedRows = "deploy runs on main"
	RefusedApplyRuns    RefusedRows = "apply runs"
)

// RefusedWindow is a window a read of several windows refused: it holds more than Bound rows of
// the kind Rows names for [From, To). Repo is empty for a day's own window of pull requests across
// repositories (ListPullRequestsOnDays), and the deploy repository for the window of the earlier
// rows of a day's deploys (ReadDaysDeploys, Deploys.Earlier) and for a day's own runs
// (ReadDaysDeploys, ReadCIDays). The read answers no row of the window.
type RefusedWindow struct {
	From, To time.Time
	Repo     string
	Bound    int
	Rows     RefusedRows
}

// Window is the refused window as the refusal names it, [From, To).
func (w RefusedWindow) Window() string {
	return fmt.Sprintf("[%s, %s)", w.From.Format(time.RFC3339Nano), w.To.Format(time.RFC3339Nano))
}

// String names the bound, the rows that exceeded it, their repository if set, and the window.
func (w RefusedWindow) String() string {
	what := "pull requests"
	when := "merged in"
	switch w.Rows {
	case RefusedPRChecksRuns, RefusedDeployRuns:
		what, when = string(w.Rows), "started in"
	case RefusedApplyRuns:
		what, when = string(w.Rows), "for"
	}
	if w.Repo != "" {
		what += " of " + w.Repo
	}
	return fmt.Sprintf("more than %d %s %s %s, the bound of a window's read", w.Bound, what, when, w.Window())
}

// daysSideBySide is how many days' reads of their own the reads of several days run at once, each
// on a connection of its own: ReadCIDays' read of every job of each day's PR-checks runs, the one
// read of a day whose rows grow with the day's runs rather than with the page's days (24,000 jobs a
// day at ten times the stated run rate), which one statement for every day would read on one
// connection.
const daysSideBySide = 4

// dayWindows returns the starts and ends a statement unnests for the windows [day, day+1).
// It refuses a day whose window starts before the end of the previous day's window.
func dayWindows(days []time.Time) (from, to []time.Time, err error) {
	from, to = make([]time.Time, len(days)), make([]time.Time, len(days))
	for i, day := range days {
		if i > 0 && day.Before(to[i-1]) {
			return nil, nil, fmt.Errorf("day %s follows day %s: each day's window [day, day+1) must start at or after the end of the previous day's window",
				day.Format(time.RFC3339), days[i-1].Format(time.RFC3339))
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
// window, in one statement, and answers, index-aligned with days too, the refusal of each day whose
// window holds more than MaxPullRequestsPerWindow pull requests, whose list is empty, and nil for
// every other day (listPullRequestsInWindows).
func ListPullRequestsOnDays(ctx context.Context, q Querier, days []time.Time) ([][]DeliveryPullRequest, []*RefusedWindow, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return nil, nil, err
	}
	return listPullRequestsInWindows(ctx, q, from, to, "")
}

// listPullRequestsInWindows lists, for each window [from[i], to[i]), what ListPullRequestsInWindow
// lists of it, in its order, in one statement, a pull request of two windows in each; narrowed to
// repo's pull requests when repo is non-empty, as readMergedRows narrows a window's earlier rows
// (Deploys.Earlier). Each window is bounded on its own: the statement reads at most
// MaxPullRequestsPerWindow+1 pull requests of each window, whatever the others hold, and a window
// that holds more than MaxPullRequestsPerWindow is refused alone, where ListPullRequestsInWindow
// answers the oldest MaxPullRequestsPerWindow: its list is empty, and the second answer, aligned
// with the windows, holds its refusal, naming the window and the bound, and nil for every window
// answered. No window answers part of its rows, so which of a window's rows the statement takes
// does not matter: it answers them only where they are every one of the window's. A refusal costs
// its own window alone, so the other windows, each another day's, are answered whatever one day
// holds: a refusal of the whole read failed every day of the refresher's page with the one day
// that held more, in every pass, so a day under the bound was never written either
// (dispatch://INFRA-2916/comment/0d936c90-6b07-4049-bca2-7b08ea901db3). A limit the windows shared
// (MaxPullRequestsPerWindow times their number) let a window take the share another left unused,
// and past it kept the oldest of every window's together without a word, leaving out a window's
// newest, which its deploys ship (dispatch://INFRA-2916).
func listPullRequestsInWindows(ctx context.Context, q Querier, from, to []time.Time, repo string) ([][]DeliveryPullRequest, []*RefusedWindow, error) {
	listed, refused := make([][]DeliveryPullRequest, len(from)), make([]*RefusedWindow, len(from))
	for i := range listed {
		listed[i] = []DeliveryPullRequest{}
	}
	if len(from) == 0 {
		return listed, refused, nil
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
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var window int
		pr, err := scanPullRequest(rows, &window)
		if err != nil {
			return nil, nil, err
		}
		// The row past the bound is its window's last: the statement reads one past it at most.
		if w := window - 1; len(listed[w]) == MaxPullRequestsPerWindow {
			refused[w], listed[w] = &RefusedWindow{From: from[w], To: to[w], Repo: repo, Bound: MaxPullRequestsPerWindow}, []DeliveryPullRequest{}
		} else {
			listed[w] = append(listed[w], pr)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return listed, refused, nil
}

// listRunsStartedOnDays lists what listRunsStartedIn lists for each day window, in its order,
// in one statement. Each window reads at most MaxRunsPerWindow+1 runs on its own; a window past
// the bound has an empty list and an aligned refusal, and every other window is answered whole.
func listRunsStartedOnDays(ctx context.Context, q Querier, repo string, kind DeliveryRunKind, branch, event string, from, to []time.Time) ([][]DeliveryRun, []*RefusedWindow, error) {
	listed, refused := make([][]DeliveryRun, len(from)), make([]*RefusedWindow, len(from))
	for i := range listed {
		listed[i] = []DeliveryRun{}
	}
	if len(from) == 0 {
		return listed, refused, nil
	}
	rows, err := q.Query(ctx, `
		select `+RunColumns+`, d.i
		from unnest($3::timestamptz[], $4::timestamptz[]) with ordinality as d(from_at, to_at, i)
		cross join lateral (
			select `+RunColumns+`
			from delivery_runs
			where repo = $1 and kind = $2 and started_at >= d.from_at and started_at < d.to_at
				and ($5 = '' or head_branch = $5) and ($6 = '' or event = $6)
			limit $7
		) r
		order by d.i, r.started_at, r.run_id
	`, repo, string(kind), from, to, branch, event, MaxRunsPerWindow+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	what := RefusedDeployRuns
	if kind == DeliveryRunKindPRChecks {
		what = RefusedPRChecksRuns
	}
	for rows.Next() {
		var window int
		run, err := scanRun(rows, &window)
		if err != nil {
			return nil, nil, err
		}
		// The statement reads at most one row past the bound of each window.
		if w := window - 1; len(listed[w]) == MaxRunsPerWindow {
			listed[w] = []DeliveryRun{}
			refused[w] = &RefusedWindow{From: from[w], To: to[w], Repo: repo, Bound: MaxRunsPerWindow, Rows: what}
		} else {
			listed[w] = append(listed[w], run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return listed, refused, nil
}

// listDeployRunsCreatedInWindows lists the earlier apply runs (Deploys.Earlier) in one statement.
// Each window takes its oldest MaxRunsPerWindow deploy runs on main by creation time and run ID,
// as listRuns does for one window. No window is refused, and a run of two windows is in each.
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
		cross join lateral (
			select `+RunColumns+`
			from delivery_runs r
			where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main'
				and `+runCreatedAt+` >= d.from_at and `+runCreatedAt+` < d.to_at
			order by `+runCreatedAt+`, r.run_id
			limit $4
		) r
		order by d.i, `+runCreatedAt+`, r.run_id
	`, repo, from, to, MaxRunsPerWindow)
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

// daysRunsQuery reads deploy runs on main of $1 that are apply runs of a day's window
// (applyRunsCreated) or started on a day. It answers each run once, oldest creation first,
// with the day ordinals that listed it in each part. $2 and $3 are the days' starts and ends,
// $4 and $5 are Production.JobNames and Production.ApplyStep, and $6 is MaxRunsPerWindow+1
// for each part of each day. The parts use ranges of delivery_runs_kind_created and
// delivery_runs_kind_started_at. collectDaysRuns refuses days past either bound.
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
		select ` + RunColumns + `, l.apply_days, l.started_days
		from delivery_runs r
		join (
			select run_id as listed_id,
				array_agg(day) filter (where applies) as apply_days,
				array_agg(day) filter (where not applies) as started_days
			from listed_runs
			group by run_id
		) l on l.listed_id = r.run_id
		where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main'
		order by ` + runCreatedAt + `, r.run_id`

// daysRunsListed lists each day's run IDs and their part (apply or started) separately.
// Each part reads up to $6 rows on its own, in any order: a part past the bound is never returned
// as a partial list.
var daysRunsListed = `
			select listed.run_id, d.i::integer as day, listed.applies
			from unnest($2::timestamptz[], $3::timestamptz[]) with ordinality as d(from_at, to_at, i)
			cross join lateral (
				(select r.run_id, true as applies from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and ` + applyRunsCreated("d.from_at", "d.to_at") + `
				limit $6)
				union all
				(select r.run_id, false as applies from delivery_runs r
				where r.repo = $1 and r.kind = 'deploy' and r.head_branch = 'main' and r.started_at >= d.from_at and r.started_at < d.to_at
				limit $6)
			) as listed`

// collectDaysRuns counts each day's two parts and refuses days past either bound, naming the
// started runs if both parts exceed it. A run stays only if at least one answered day listed it.
func collectDaysRuns(rows pgx.Rows, repo string, from, to []time.Time) ([]DeliveryRun, []*RefusedWindow, error) {
	defer rows.Close()
	type membership struct{ apply, started []int32 }
	var listedOn []membership
	runs := []DeliveryRun{}
	applyCounts, startedCounts := make([]int, len(from)), make([]int, len(from))
	for rows.Next() {
		var days membership
		run, err := scanRun(rows, &days.apply, &days.started)
		if err != nil {
			return nil, nil, err
		}
		runs, listedOn = append(runs, run), append(listedOn, days)
		for _, day := range days.apply {
			applyCounts[day-1]++
		}
		for _, day := range days.started {
			startedCounts[day-1]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	refused := make([]*RefusedWindow, len(from))
	anyRefused := false
	for i := range from {
		var what RefusedRows
		switch {
		case startedCounts[i] > MaxRunsPerWindow:
			what = RefusedDeployRuns
		case applyCounts[i] > MaxRunsPerWindow:
			what = RefusedApplyRuns
		default:
			continue
		}
		refused[i] = &RefusedWindow{From: from[i], To: to[i], Repo: repo, Bound: MaxRunsPerWindow, Rows: what}
		anyRefused = true
	}
	if !anyRefused {
		return runs, refused, nil
	}
	answered := func(day int32) bool { return refused[day-1] == nil }
	kept := runs[:0]
	for i, days := range listedOn {
		if slices.ContainsFunc(days.apply, answered) || slices.ContainsFunc(days.started, answered) {
			kept = append(kept, runs[i])
		}
	}
	clear(runs[len(kept):])
	return kept, refused, nil
}

// DaysDeploys is what ReadDaysDeploys reads of several days of one deploy repository: each day's
// Deploys and refusal, index-aligned with the days, and optionally the apply runs of the answered
// days (Applies).
type DaysDeploys struct {
	Deploys []Deploys
	// Refused names the rows and the window that exceeded its bound, or is nil for an answered
	// day. The window is the day itself for started or apply runs, or Deploys.Earlier's window
	// for earlier pull requests. In either case that day's Deploys is empty.
	Refused []*RefusedWindow
	// Applies holds the apply and started runs of days within their own run bounds, with their
	// production jobs (the jobs a deploy is judged by). Only an answered day can take its part
	// (ApplyRows.Since). Its part holds the run that first ships each pull request merged before
	// the day's end. It may also hold runs created after its apply bound from other days.
	Applies ApplyRows
}

// ReadDaysDeploys reads what ReadDeploys reads for each day of settings.DeployRepo, and with
// applies also the answered days' apply runs (DaysDeploys.Applies). It bounds each day's own
// started and apply runs separately. If either exceeds MaxRunsPerWindow, the day alone is refused
// by name, started runs taking precedence: it gets no Deploys, no earlier read for it, and no
// job read for runs listed only for refused days.
//
// For days that can ship earlier pull requests, it reads their earlier windows (Deploys.Earlier).
// A day whose earlier pull requests exceed MaxPullRequestsPerWindow is refused as well, with no
// Deploys and a refusal naming that earlier window. Earlier apply runs do not cause a refusal:
// each earlier window takes its oldest MaxRunsPerWindow as ReadDeploys does. All other days are
// answered whole.
func ReadDaysDeploys(ctx context.Context, pool *store.Pool, settings DeliverySettings, days []time.Time, applies bool) (DaysDeploys, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return DaysDeploys{}, err
	}
	read := DaysDeploys{Deploys: make([]Deploys, len(days))}
	if len(days) == 0 {
		read.Refused = []*RefusedWindow{}
		return read, nil
	}
	repo, production := settings.DeployRepo, settings.Production()
	names := production.JudgedJobNames()
	var runs []DeliveryRun
	if applies {
		rows, err := pool.Query(ctx, daysRunsQuery, repo, from, to, production.JobNames, production.ApplyStep, MaxRunsPerWindow+1)
		if err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
		if runs, read.Refused, err = collectDaysRuns(rows, repo, from, to); err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
	} else {
		started, refused, err := listRunsStartedOnDays(ctx, pool, repo, DeliveryRunKindDeploy, "main", "", from, to)
		if err != nil {
			return DaysDeploys{}, fmt.Errorf("read the deploy runs of %d days: %w", len(days), err)
		}
		read.Refused = refused
		runs = slices.Concat(started...)
	}
	ids := runIDsOf(runs)
	jobs, err := ListRunJobsForRuns(ctx, pool, repo, ids, names)
	if err != nil {
		return DaysDeploys{}, fmt.Errorf("read the production jobs of %d deploy runs: %w", len(ids), err)
	}
	if applies {
		read.Applies = ApplyRows{from: days[0], windows: make(map[int64]bool, len(days)), Runs: runs, Jobs: jobs}
		for i, day := range days {
			if read.Refused[i] == nil {
				read.Applies.windows[day.Unix()] = true
			}
		}
	}
	started := make([]RunsWithJobs, len(days))
	for i := range started {
		if read.Refused[i] == nil {
			started[i] = RunsWithJobs{Runs: []DeliveryRun{}, Jobs: map[int64][]DeliveryRunJob{}}
		}
	}
	for _, run := range runs {
		if i, held := dayOf(from, to, run.StartedAt); held && read.Refused[i] == nil {
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
		if read.Refused[i] != nil {
			continue
		}
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
	var refused []*RefusedWindow
	var earlierRuns [][]DeliveryRun
	var earlierJobs map[int64][]DeliveryRunJob
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			prs, refused, err = listPullRequestsInWindows(ctx, pool, earlierFrom, earlierTo, repo)
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
		if refused[k] != nil {
			read.Deploys[i], read.Refused[i] = Deploys{}, refused[k]
			delete(read.Applies.windows, days[i].Unix())
			continue
		}
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

// CIDay is a day's CI rows or its refusal. CIRows is empty when Refused is set.
type CIDay struct {
	CIRows
	Refused *RefusedWindow
}

// ReadCIDays answers what ReadCIRows answers for each day of deployRepo. It lists PR-checks runs
// and deploy pushes separately, each bounded on its own for each day. A day past either bound is
// refused alone by name, with no runs or jobs; the PR-checks refusal wins if both exceed the
// bound. For every other day it reads all PR-checks jobs (daysSideBySide days at once) and the
// named deploy jobs (one statement for the answered days).
func ReadCIDays(ctx context.Context, pool *store.Pool, deployRepo string, days []time.Time, deployJobNames []string) ([]CIDay, error) {
	from, to, err := dayWindows(days)
	if err != nil {
		return nil, err
	}
	read := make([]CIDay, len(days))
	if len(days) == 0 {
		return read, nil
	}
	var checks, pushes [][]DeliveryRun
	var checksRefused, pushesRefused []*RefusedWindow
	err = store.ReadSideBySide(ctx,
		func(ctx context.Context) (err error) {
			checks, checksRefused, err = listRunsStartedOnDays(ctx, pool, deployRepo, DeliveryRunKindPRChecks, "", "pull_request", from, to)
			return err
		},
		func(ctx context.Context) (err error) {
			pushes, pushesRefused, err = listRunsStartedOnDays(ctx, pool, deployRepo, DeliveryRunKindDeploy, "main", "push", from, to)
			return err
		})
	if err != nil {
		return nil, err
	}
	for day := range days {
		read[day].Refused = checksRefused[day]
		if read[day].Refused == nil {
			read[day].Refused = pushesRefused[day]
		}
	}
	err = store.ReadSideBySide(ctx,
		func(ctx context.Context) error {
			return readEachDay(ctx, len(days), func(ctx context.Context, day int) error {
				if read[day].Refused != nil {
					return nil
				}
				ids := runIDsOf(checks[day])
				jobs, err := listEveryRunJobForRuns(ctx, pool, deployRepo, ids)
				if err != nil {
					return err
				}
				read[day].Checks = RunsWithJobs{Runs: checks[day], Jobs: jobs}
				return nil
			})
		},
		func(ctx context.Context) error {
			var ids []int64
			for day, runs := range pushes {
				if read[day].Refused != nil {
					continue
				}
				for _, run := range runs {
					ids = append(ids, run.RunID)
				}
			}
			jobs, err := ListRunJobsForRuns(ctx, pool, deployRepo, ids, deployJobNames)
			if err != nil {
				return err
			}
			for day, runs := range pushes {
				if read[day].Refused != nil {
					continue
				}
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
