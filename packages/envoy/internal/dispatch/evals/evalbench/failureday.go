package evalbench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A failure day is a day of failed deploy runs written on top of the history Load wrote, so the
// measurement times the Delivery timeline on the day its read is largest: the timeline reads each
// deploy run's production jobs and every job of it that failed (delivery.ReadMergedRows with
// failedJobs), so the more jobs fail, the more it reads.
//
// The day INFRA's history had is the worst UTC day of its deploy workflow's runs on main from
// 2026-10-14 to 2026-11-14, every run paged from GitHub (dispatch://INFRA-2916, the worst-day
// research, worstday/README.md): 2026-11-12, 170 deploy runs on main, 36 of them concluded
// failure, with 45 failed jobs in their latest attempts, one to three a run (9 to 33 jobs a run).
// Its p90 day held 10 failed runs and 11 failed jobs, its median day 1 and 1. The history Load
// writes already fails about a fifth of its deploy runs, each in one job (deployJobsOf); a failure
// day's runs are added to that day's.

// FailureDayFirstRunID is the id of a failure day's first run, past every id the history Load
// writes takes (firstRunID on, 160,000 runs at ten times the run rate), so a reader tells the
// day's runs apart by their ids.
const FailureDayFirstRunID = int64(9_500_000_001)

// maxFailedJobsPerRun is the most failed jobs one failure-day run holds: INFRA's worst day failed
// one to three jobs a run, and a failure day's run runs its first three jobs side by side
// (failingDeployJobsOf), any of which can fail.
const maxFailedJobsPerRun = 3

// FailureDay is one day of failed deploy runs on main of the deploy repository, written on top of
// the history (AddFailureDay).
type FailureDay struct {
	// DaysAgo is the UTC day it falls on, as days before the day of Spec.Now: 1 is the day before,
	// 84 the first day of the 84-day window the first paint reads, and a day of the last 28 is in
	// the 28-day window too.
	DaysAgo int
	// Runs is how many failed deploy runs it holds and Jobs how many failed jobs across them, Runs
	// to three times Runs, at INFRA's stated rate: the spec's scale multiplies both, as it
	// multiplies every count, at least one run and one job a run.
	Runs, Jobs int
}

// AddedFailureDay is what AddFailureDay wrote: the day, its runs and their jobs, and how many of
// those jobs failed.
type AddedFailureDay struct {
	Day                    time.Time
	Runs, Jobs, FailedJobs int
}

// AddFailureDay writes day into a database Load wrote spec's history into: its failed deploy runs,
// each with the deploy workflow's eight jobs, through delivery.UpsertRun and delivery.UpsertRunJobs,
// the writes the intake and the reconcile make. The rows' change triggers stay on, as they are for
// every run GitHub reports, so the server's consumers read the runs as the change they are. A
// database that holds a failure day already, or no delivery history, is refused.
func AddFailureDay(ctx context.Context, pool *store.Pool, spec Spec, day FailureDay) (AddedFailureDay, error) {
	switch {
	case spec.Scale <= 0 || spec.Scale > 1:
		return AddedFailureDay{}, fmt.Errorf("the scale is above 0 and at most 1, not %g", spec.Scale)
	case spec.Now.IsZero():
		return AddedFailureDay{}, errors.New("name the moment the history ends")
	case day.DaysAgo < 1 || day.DaysAgo > 84:
		return AddedFailureDay{}, fmt.Errorf("a failure day is one of the 84 days before the history's last, 1 to 84 days before it, not %d", day.DaysAgo)
	case day.Runs < 1:
		return AddedFailureDay{}, fmt.Errorf("a failure day holds a failed run at least, not %d", day.Runs)
	case day.Jobs < day.Runs || day.Jobs > maxFailedJobsPerRun*day.Runs:
		return AddedFailureDay{}, fmt.Errorf("%d failed runs hold %d to %d failed jobs, not %d", day.Runs, day.Runs, maxFailedJobsPerRun*day.Runs, day.Jobs)
	}
	settings, err := delivery.GetSettings(ctx, pool)
	if err != nil {
		return AddedFailureDay{}, fmt.Errorf("read the delivery settings Load writes: %w", err)
	}
	if settings.DeployRepo != Repos[0] {
		return AddedFailureDay{}, fmt.Errorf("the delivery settings name %s, not the history's deploy repository %s", settings.DeployRepo, Repos[0])
	}
	var held int64
	if err := pool.QueryRow(ctx, `select count(*) from delivery_runs where repo = $1 and run_id >= $2`, Repos[0], FailureDayFirstRunID).Scan(&held); err != nil {
		return AddedFailureDay{}, fmt.Errorf("read whether a failure day is written: %w", err)
	}
	if held > 0 {
		return AddedFailureDay{}, fmt.Errorf("the database holds %d runs of a failure day already", held)
	}

	rng := newRand(spec.Seed, "failure-day")
	runs := spec.scaled(float64(day.Runs), 1)
	failing := make([]int, runs)
	for i := range failing {
		failing[i] = 1
	}
	// The failed jobs past one a run go to runs drawn at random, three a run at most.
	for extra := min(maxFailedJobsPerRun*runs, spec.scaled(float64(day.Jobs), runs)) - runs; extra > 0; {
		if i := rng.IntN(runs); failing[i] < maxFailedJobsPerRun {
			failing[i]++
			extra--
		}
	}
	end := spec.Now.UTC().Truncate(24 * time.Hour)
	added := AddedFailureDay{Day: end.AddDate(0, 0, -day.DaysAgo), Runs: runs}
	for i, failed := range failing {
		id := FailureDayFirstRunID + int64(i)
		// Created in the day's first 23 hours, so each run ends inside it, before the history's end.
		created := added.Day.Add(within(rng, 23*time.Hour))
		started := created.Add(between(rng, 5*time.Second, time.Minute))
		jobs, last := failingDeployJobsOf(rng, id, started, failed)
		run := delivery.DeliveryRun{
			Repo: Repos[0], RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: sha(rng),
			HeadCommitAt: created.Add(-between(rng, 10*time.Second, 3*time.Minute)), StartedAt: started, CreatedAt: &created,
			CompletedAt: new(last.Add(between(rng, 3*time.Second, 15*time.Second))), Conclusion: new(delivery.DeliveryRunConclusionFailure),
			URL:        "https://github.com/" + Repos[0] + "/actions/runs/" + strconv.FormatInt(id, 10),
			HeadBranch: new("main"), Event: new("push"),
		}
		if err := delivery.UpsertRun(ctx, pool, run); err != nil {
			return AddedFailureDay{}, fmt.Errorf("write the failure day's run %d: %w", id, err)
		}
		if err := delivery.UpsertRunJobs(ctx, pool, Repos[0], id, jobs, true); err != nil {
			return AddedFailureDay{}, fmt.Errorf("write the failure day's run %d's jobs: %w", id, err)
		}
		added.Jobs += len(jobs)
		added.FailedJobs += failed
	}
	// Load takes the statistics a long-running store holds before the measurement reads plans;
	// these rows are taken into them the same way.
	if _, err := pool.Exec(ctx, `analyze delivery_runs, delivery_run_jobs`); err != nil {
		return AddedFailureDay{}, fmt.Errorf("analyze the failure day's tables: %w", err)
	}
	return added, nil
}

// failingDeployJobsOf draws a failure day's deploy run runID of the deploy workflow's eight jobs:
// its first three (the two builds and the migration check) run side by side and failing of them,
// drawn at random, fail; every job after them is skipped, so the run reaches neither staging nor
// production and ships nothing. It answers the jobs and when the last of them ended.
func failingDeployJobsOf(rng *rand.Rand, runID int64, started time.Time, failing int) ([]delivery.DeliveryRunJob, time.Time) {
	fails := rng.Perm(maxFailedJobsPerRun)[:failing]
	jobs := make([]delivery.DeliveryRunJob, 0, len(deployStages))
	last := started
	for stage := range maxFailedJobsPerRun {
		step := deployStages[stage]
		begun := started.Add(between(rng, 5*time.Second, 20*time.Second))
		took := max(lasting(rng, step.median, 0.3).Round(time.Second), time.Second)
		conclusion := delivery.DeliveryJobConclusionSuccess
		if slices.Contains(fails, stage) {
			conclusion = delivery.DeliveryJobConclusionFailure
			took = max(time.Duration(float64(took)*(0.3+0.7*rng.Float64())).Round(time.Second), time.Second)
		}
		ended := begun.Add(took)
		jobs = append(jobs, delivery.DeliveryRunJob{Repo: Repos[0], RunID: runID, Name: step.name, StartedAt: new(begun), CompletedAt: new(ended), Conclusion: new(conclusion)})
		if ended.After(last) {
			last = ended
		}
	}
	for _, step := range deployStages[maxFailedJobsPerRun:] {
		jobs = append(jobs, delivery.DeliveryRunJob{Repo: Repos[0], RunID: runID, Name: step.name, StartedAt: new(last), CompletedAt: new(last), Conclusion: new(delivery.DeliveryJobConclusionSkipped)})
	}
	return jobs, last
}
