package evalbench

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// INFRA's delivery history, in deliverytest's shape at INFRA's stated rate: deliverytest writes
// about nineteen pull requests a day and runs of four and ten jobs, and one database holds one of
// its histories, so it cannot hold this one. The pull requests are spread over the last runDays
// days, as the runs are, so every deploy ships the pull requests merged since the one before it,
// as in production. Containment (delivery/containment.go) ships a pull request of the deploy
// repository with the first successful production job of a deploy run created after its merge.

// The deploy workflow's eight jobs, in the order they run, and the PR-checks workflow's roll-up
// job. The settings name the production apply, the staging apply and the staging end-to-end job.
const (
	productionJob   = "production-release / apply"
	stagingApplyJob = "staging-release / apply"
	stagingE2EJob   = "staging-e2e / run"
	rollupJob       = "checks-complete"
	firstRunID      = int64(9_000_000_001)
)

// authors are the population authors every pull request is written by.
var authors = []string{"dev-ana", "dev-ben", "dev-chi", "dev-dai", "dev-eli"}

// deployStages are the deploy workflow's jobs, each with how long it usually takes.
var deployStages = []struct {
	name   string
	median time.Duration
}{
	{"build / image", 6 * time.Minute}, {"build / assets", 4 * time.Minute}, {"migrate / check", 2 * time.Minute},
	{stagingApplyJob, 4 * time.Minute}, {stagingE2EJob, 16 * time.Minute}, {"production-release / plan", 2 * time.Minute},
	{productionJob, 6 * time.Minute}, {"notify / release", time.Minute},
}

// The deploy stages a run fails or is cancelled in, by their index in deployStages.
const (
	buildStage        = 0
	stagingApplyStage = 3
	stagingE2EStage   = 4
	productionStage   = 6
)

// checkStage is one PR-checks job besides the roll-up: how long it usually takes, how often it
// fails and the share of runs that run it.
type checkStage struct {
	name   string
	median time.Duration
	fails  float64
	runs   float64
}

// checkStages are the PR-checks workflow's 39 jobs besides the roll-up: deliverytest's nine and
// thirty shards of the test suite, so a run holds 40 jobs.
var checkStages = func() []checkStage {
	stages := []checkStage{
		{"build / compile", 3 * time.Minute, 0.02, 1}, {"build / unit", 8 * time.Minute, 0.05, 1},
		{"lint", 90 * time.Second, 0.03, 1}, {"typecheck", 150 * time.Second, 0.02, 1},
		{"test / api", 12 * time.Minute, 0.04, 1}, {"test / web", 9 * time.Minute, 0.03, 1},
		{"e2e / smoke", 15 * time.Minute, 0.06, 0.5}, {"docs", 2 * time.Minute, 0.01, 0.3},
		{"security / scan", 4 * time.Minute, 0.01, 1},
	}
	for shard := 1; len(stages) < checkJobs-1; shard++ {
		stages = append(stages, checkStage{fmt.Sprintf("test / shard-%02d", shard), 6 * time.Minute, 0.01, 1})
	}
	return stages
}()

// Settings is the one delivery_settings row the history is read by.
func Settings() delivery.DeliverySettings {
	return delivery.DeliverySettings{
		DeployRepo: Repos[0], DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{productionJob}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: authors, ExcludedRepos: []string{},
		StagingE2EJobName: new(stagingE2EJob), StagingApplyJobName: new(stagingApplyJob), PRChecksRollupJobName: new(rollupJob),
	}
}

// plannedPR is one pull request before it is written.
type plannedPR struct {
	repo           string
	number         int
	created        time.Time
	merged         time.Time
	firstCommit    *time.Time
	author         string
	additions      int32
	deletions      int32
	rework         bool
	issue          string
	session        string
	mergeCommitSHA string
}

// plannedRun is one run before it is written: what it is and when GitHub created it, and the
// random draw its jobs come from, so the jobs are drawn again, the same, when they are written.
type plannedRun struct {
	kind     delivery.DeliveryRunKind
	created  time.Time
	prNumber int
	draw     uint64
}

// deliveryRow is a run as delivery_runs holds it at the history's end, and its jobs.
type deliveryRow struct {
	id         int64
	kind       delivery.DeliveryRunKind
	prNumber   *int32
	sha        string
	headAt     time.Time
	started    time.Time
	created    time.Time
	completed  *time.Time
	conclusion *string
	branch     string
	event      string
	jobs       []jobRow
}

// jobRow is a job as delivery_run_jobs holds it at the history's end: a job still going has no
// completion and no conclusion.
type jobRow struct {
	name       string
	started    time.Time
	completed  *time.Time
	conclusion *string
}

// sha is a commit id from rng.
func sha(rng *rand.Rand) string {
	return fmt.Sprintf("%016x%016x%08x", rng.Uint64(), rng.Uint64(), rng.Uint32())
}

// within is a moment in [0, span), to the second.
func within(rng *rand.Rand, span time.Duration) time.Duration {
	return time.Duration(rng.Int64N(int64(span/time.Second))) * time.Second
}

// between is a duration in [low, high], to the second.
func between(rng *rand.Rand, low, high time.Duration) time.Duration {
	return low + time.Duration(rng.Int64N(int64((high-low)/time.Second)+1))*time.Second
}

// planPullRequests draws the pull requests: spread over the last runDays days, three in five into
// the deploy repository, each open about seventy minutes, one in five a rework, and most naming an
// INFRA issue whose life holds the pull request, with that issue's implementer as its session.
func planPullRequests(spec Spec, history infraHistory) []plannedPR {
	rng := newRand(spec.Seed, "pull requests")
	count := spec.scaled(pullRequests, 3)
	start := spec.Now.Add(-runDays * 24 * time.Hour)
	opened := make([]time.Time, len(history.issues))
	for i, issue := range history.issues {
		opened[i] = issue.opened
	}
	prs := make([]plannedPR, count)
	for i := range prs {
		pr := &prs[i]
		switch u := rng.Float64(); {
		case u < 0.6:
			pr.repo = Repos[0]
		case u < 0.85:
			pr.repo = Repos[1]
		default:
			pr.repo = Repos[2]
		}
		pr.merged = start.Add(within(rng, runDays*24*time.Hour))
		pr.created = pr.merged.Add(-min(max(lasting(rng, 70*time.Minute, 1.1), 3*time.Minute), 5*24*time.Hour))
		if rng.Float64() >= 0.1 {
			pr.firstCommit = new(pr.created.Add(-between(rng, 10*time.Minute, 8*time.Hour)))
		}
		pr.author, pr.mergeCommitSHA = authors[rng.IntN(len(authors))], sha(rng)
		pr.additions, pr.deletions = int32(1+rng.IntN(400)), int32(rng.IntN(200))
		pr.rework = rng.Float64() < reworkShare
		pr.session = fmt.Sprintf("s-helper-%02d", rng.IntN(helperSessions))
		if rng.Float64() < 0.85 {
			// An issue opened in the fortnight before the pull request and not closed before
			// it merged, by a few draws among them.
			last := sort.Search(len(opened), func(i int) bool { return opened[i].After(pr.created) })
			first := sort.Search(len(opened), func(i int) bool { return !opened[i].Before(pr.created.Add(-14 * 24 * time.Hour)) })
			for try := 0; try < 8 && last > first; try++ {
				issue := history.issues[first+rng.IntN(last-first)]
				if issue.closed == nil || !issue.closed.Before(pr.merged) {
					pr.issue, pr.session = issue.key, "s-implementer-"+issue.key
					break
				}
			}
		}
	}
	// Numbers rise with creation within each repository, as GitHub's do.
	slices.SortStableFunc(prs, func(a, b plannedPR) int {
		return cmp.Or(a.created.Compare(b.created), a.merged.Compare(b.merged), cmp.Compare(a.repo, b.repo))
	})
	next := map[string]int{}
	for i := range prs {
		next[prs[i].repo]++
		prs[i].number = next[prs[i].repo]
	}
	return prs
}

// dailyRuns is how many deploy runs and PR-checks runs planRuns draws a day: deployRuns and
// checkRuns at the spec's scale, ten times each under TenTimes. The history holds runDays days of
// them, whose run ids are firstRunID on, in creation order.
func (s Spec) dailyRuns() (deploys, checks int) {
	rate := 1
	if s.TenTimes {
		rate = 10
	}
	return s.scaled(deployRuns, 1) * rate, s.scaled(checkRuns, 1) * rate
}

// planRuns draws the runs of the last runDays days (dailyRuns a day): deploy runs on main of the
// deploy repository and PR-checks runs of its pull requests, in creation order.
func planRuns(spec Spec, prs []plannedPR) []plannedRun {
	rng := newRand(spec.Seed, "runs")
	deploys, checks := spec.dailyRuns()
	start := spec.Now.Add(-runDays * 24 * time.Hour)
	var deployPRs []plannedPR
	for _, pr := range prs {
		if pr.repo == Repos[0] {
			deployPRs = append(deployPRs, pr)
		}
	}
	slices.SortStableFunc(deployPRs, func(a, b plannedPR) int { return a.merged.Compare(b.merged) })
	var runs []plannedRun
	for day := range runDays {
		from := start.AddDate(0, 0, day)
		for range deploys {
			runs = append(runs, plannedRun{kind: delivery.DeliveryRunKindDeploy, created: from.Add(within(rng, 24*time.Hour)), draw: rng.Uint64()})
		}
		for range checks {
			created := from.Add(within(rng, 24*time.Hour))
			// A pull request of the deploy repository merged within three days of the run.
			low := sort.Search(len(deployPRs), func(i int) bool { return !deployPRs[i].merged.Before(created.Add(-72 * time.Hour)) })
			high := sort.Search(len(deployPRs), func(i int) bool { return deployPRs[i].merged.After(created.Add(72 * time.Hour)) })
			number := 1
			if high > low {
				number = deployPRs[low+rng.IntN(high-low)].number
			} else if len(deployPRs) > 0 {
				number = deployPRs[rng.IntN(len(deployPRs))].number
			}
			runs = append(runs, plannedRun{kind: delivery.DeliveryRunKindPRChecks, created: created, prNumber: number, draw: rng.Uint64()})
		}
	}
	slices.SortStableFunc(runs, func(a, b plannedRun) int { return a.created.Compare(b.created) })
	return runs
}

// build is the run as the database holds it at the history's end, with its jobs, drawn from the
// run's own draw so a second build of it is the same.
func (run plannedRun) build(spec Spec, id int64) deliveryRow {
	rng := rand.New(rand.NewPCG(spec.Seed, run.draw))
	row := deliveryRow{id: id, kind: run.kind, sha: sha(rng), created: run.created}
	var jobs []jobRow
	var last time.Time
	conclusion := string(delivery.DeliveryRunConclusionSuccess)
	if run.kind == delivery.DeliveryRunKindDeploy {
		row.branch, row.event = "main", "push"
		row.headAt = run.created.Add(-between(rng, 10*time.Second, 3*time.Minute))
		row.started = run.created.Add(between(rng, 5*time.Second, time.Minute))
		jobs, conclusion, last = deployJobsOf(rng, row.started)
	} else {
		row.prNumber = new(int32(run.prNumber))
		row.branch, row.event = fmt.Sprintf("feature/%d", run.prNumber), "pull_request"
		row.headAt = run.created.Add(-between(rng, time.Minute, 30*time.Minute))
		row.started = run.created.Add(between(rng, 0, 30*time.Second))
		jobs, conclusion, last = checkJobsOf(rng, row.started)
	}
	completed := last.Add(between(rng, 3*time.Second, 15*time.Second))
	// A job GitHub had not started by the end is not listed yet, a job still going has no
	// completion and no conclusion, and neither has the run while any of its jobs is going.
	finished := completed.Before(spec.Now)
	for _, job := range jobs {
		if !job.started.Before(spec.Now) {
			finished = false
			continue
		}
		if !job.completed.Before(spec.Now) {
			job.completed, job.conclusion, finished = nil, nil, false
		}
		row.jobs = append(row.jobs, job)
	}
	if finished {
		row.completed, row.conclusion = &completed, &conclusion
	}
	return row
}

// finishedJob is one job GitHub reports once it has finished.
func finishedJob(name string, started, completed time.Time, conclusion delivery.DeliveryJobConclusion) jobRow {
	return jobRow{name: name, started: started, completed: &completed, conclusion: new(string(conclusion))}
}

// deployJobsOf draws a deploy run's eight jobs one after another: a job that fails or is
// cancelled leaves the jobs after it skipped. About three in four runs reach production.
func deployJobsOf(rng *rand.Rand, started time.Time) ([]jobRow, string, time.Time) {
	failed, cancelled, timedOut := -1, -1, false
	switch u := rng.Float64(); {
	case u < 0.04:
		failed = buildStage
	case u < 0.07:
		failed = stagingApplyStage
	case u < 0.15:
		failed, timedOut = stagingE2EStage, rng.Float64() < 0.25
	case u < 0.20:
		failed = productionStage
	case u < 0.23:
		cancelled = stagingE2EStage
	case u < 0.26:
		cancelled = productionStage
	}
	conclusion := delivery.DeliveryRunConclusionSuccess
	jobs := make([]jobRow, 0, len(deployStages))
	at := started.Add(between(rng, 5*time.Second, 20*time.Second))
	stopped := false
	for stage, step := range deployStages {
		if stopped {
			jobs = append(jobs, finishedJob(step.name, at, at, delivery.DeliveryJobConclusionSkipped))
			continue
		}
		begun := at.Add(between(rng, 2*time.Second, 20*time.Second))
		took := max(lasting(rng, step.median, 0.3).Round(time.Second), time.Second)
		switch stage {
		case cancelled:
			at = begun.Add(time.Duration(float64(took) * (0.2 + 0.6*rng.Float64())).Round(time.Second))
			jobs = append(jobs, finishedJob(step.name, begun, at, delivery.DeliveryJobConclusionCancelled))
			conclusion, stopped = delivery.DeliveryRunConclusionCancelled, true
		case failed:
			result := delivery.DeliveryJobConclusionFailure
			at = begun.Add(time.Duration(float64(took) * (0.3 + 0.7*rng.Float64())).Round(time.Second))
			if timedOut {
				result, at = delivery.DeliveryJobConclusionTimedOut, begun.Add(45*time.Minute)
			}
			jobs = append(jobs, finishedJob(step.name, begun, at, result))
			conclusion, stopped = delivery.DeliveryRunConclusionFailure, true
		default:
			at = begun.Add(took)
			jobs = append(jobs, finishedJob(step.name, begun, at, delivery.DeliveryJobConclusionSuccess))
		}
	}
	return jobs, string(conclusion), at
}

// checkJobsOf draws a PR-checks run's 39 checks side by side, each failing at its own rate, and
// the roll-up after them, which fails when any failed; one run in twenty is cancelled partway,
// which cancels every check still running and the roll-up.
func checkJobsOf(rng *rand.Rand, started time.Time) ([]jobRow, string, time.Time) {
	jobs := make([]jobRow, 0, checkJobs)
	last, anyFailed := started, false
	for _, check := range checkStages {
		begun := started.Add(between(rng, 0, 90*time.Second))
		if rng.Float64() >= check.runs {
			jobs = append(jobs, finishedJob(check.name, begun, begun, delivery.DeliveryJobConclusionSkipped))
			continue
		}
		took := max(lasting(rng, check.median, 0.35).Round(time.Second), time.Second)
		result, ended := delivery.DeliveryJobConclusionSuccess, begun.Add(took)
		if rng.Float64() < check.fails {
			anyFailed = true
			result, ended = delivery.DeliveryJobConclusionFailure, begun.Add(time.Duration(float64(took)*(0.3+0.7*rng.Float64())).Round(time.Second))
		}
		jobs = append(jobs, finishedJob(check.name, begun, ended, result))
		if ended.After(last) {
			last = ended
		}
	}
	conclusion := delivery.DeliveryRunConclusionSuccess
	if rng.Float64() < 0.05 {
		stop := started.Add(time.Duration(float64(last.Sub(started)) * (0.1 + 0.8*rng.Float64())).Round(time.Second))
		for i, job := range jobs {
			if *job.conclusion == string(delivery.DeliveryJobConclusionSkipped) || !job.completed.After(stop) {
				continue
			}
			jobs[i] = finishedJob(job.name, earlier(job.started, stop), stop, delivery.DeliveryJobConclusionCancelled)
		}
		jobs = append(jobs, finishedJob(rollupJob, stop, stop, delivery.DeliveryJobConclusionCancelled))
		return jobs, string(delivery.DeliveryRunConclusionCancelled), stop
	}
	begun := last.Add(between(rng, 2*time.Second, 10*time.Second))
	result := delivery.DeliveryJobConclusionSuccess
	if anyFailed {
		conclusion, result = delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure
	}
	last = begun.Add(between(rng, 3*time.Second, 15*time.Second))
	jobs = append(jobs, finishedJob(rollupJob, begun, last, result))
	return jobs, string(conclusion), last
}

// jobSource streams every run's jobs into COPY, drawing each run's jobs again from its draw, so
// no list of every job is held.
type jobSource struct {
	spec   Spec
	runs   []plannedRun
	run    int
	jobs   []jobRow
	id     int64
	at     int
	values []any
}

func (s *jobSource) Next() bool {
	for s.at >= len(s.jobs) {
		if s.run >= len(s.runs) {
			return false
		}
		s.id = firstRunID + int64(s.run)
		s.jobs, s.at = s.runs[s.run].build(s.spec, s.id).jobs, 0
		s.run++
	}
	s.at++
	return true
}

func (s *jobSource) Values() ([]any, error) {
	job := s.jobs[s.at-1]
	s.values = append(s.values[:0], Repos[0], s.id, job.name, job.started, job.completed, job.conclusion)
	return s.values, nil
}

func (s *jobSource) Err() error { return nil }

// writeDelivery writes the delivery settings, the pull requests, the runs and their jobs.
//
// The three tables' triggers log each row into delivery_changes, the 24-hour feed of what changed
// that the refresher and the live views read from their cursors. The history is no change any
// reader is owed: Load starts each reader at the feed's head, as on a store a seeder wrote before
// any pass. So the rows are written in one transaction that turns the three triggers off for its
// own length (a DDL Postgres rolls back with the transaction, so a load that fails leaves them on),
// rather than logging millions of rows no reader reads before the retention prunes them.
func writeDelivery(ctx context.Context, pool *store.Pool, spec Spec, history infraHistory) error {
	if _, err := delivery.PutSettings(ctx, pool, Settings(), actor); err != nil {
		return fmt.Errorf("write the delivery settings: %w", err)
	}
	prs := planPullRequests(spec, history)
	runs := planRuns(spec, prs)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the delivery history: %w", err)
	}
	defer tx.Rollback(ctx)
	triggers := [][2]string{
		{"delivery_pull_requests", "delivery_pull_requests_changed"},
		{"delivery_runs", "delivery_runs_changed"},
		{"delivery_run_jobs", "delivery_run_jobs_changed"},
	}
	for _, trigger := range triggers {
		if _, err := tx.Exec(ctx, `alter table `+trigger[0]+` disable trigger `+trigger[1]); err != nil {
			return fmt.Errorf("turn %s off for the load: %w", trigger[1], err)
		}
	}
	prRows := make([][]any, len(prs))
	for i, pr := range prs {
		short := pr.repo[len("acme/"):]
		title := fmt.Sprintf("Change %d to %s", pr.number, short)
		titleKeys := []string{}
		var issue *string
		if pr.issue != "" {
			title, titleKeys, issue = pr.issue+": "+title, []string{pr.issue}, &pr.issue
		}
		url := "https://github.com/" + pr.repo + "/pull/" + strconv.Itoa(pr.number)
		prRows[i] = []any{pr.repo, int32(pr.number), title, url, pr.author, pr.created, pr.merged, pr.firstCommit,
			pr.mergeCommitSHA, pr.additions, pr.deletions, pr.rework, issue, []string{pr.session}, false,
			titleKeys, []string{}, []string{}, []string{}}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"delivery_pull_requests"}, []string{"repo", "number", "title", "url", "author",
		"created_at", "merged_at", "first_commit_at", "merge_commit_sha", "additions", "deletions", "rework", "issue_key",
		"sessions", "partial", "attribution_title_keys", "attribution_cited_issues", "attribution_branch_keys",
		"attribution_commit_keys"}, pgx.CopyFromRows(prRows)); err != nil {
		return fmt.Errorf("copy the pull requests: %w", err)
	}
	runRows := make([][]any, len(runs))
	for i, run := range runs {
		row := run.build(spec, firstRunID+int64(i))
		url := "https://github.com/" + Repos[0] + "/actions/runs/" + strconv.FormatInt(row.id, 10)
		runRows[i] = []any{Repos[0], row.id, string(row.kind), row.prNumber, row.sha, row.headAt, row.started, row.created,
			row.completed, row.conclusion, url, row.branch, row.event, row.completed != nil}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"delivery_runs"}, []string{"repo", "run_id", "kind", "pr_number", "head_sha",
		"head_commit_at", "started_at", "created_at", "completed_at", "conclusion", "url", "head_branch", "event",
		"jobs_complete"}, pgx.CopyFromRows(runRows)); err != nil {
		return fmt.Errorf("copy the runs: %w", err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"delivery_run_jobs"}, []string{"repo", "run_id", "name", "started_at",
		"completed_at", "conclusion"}, &jobSource{spec: spec, runs: runs}); err != nil {
		return fmt.Errorf("copy the run jobs: %w", err)
	}
	for _, trigger := range triggers {
		if _, err := tx.Exec(ctx, `alter table `+trigger[0]+` enable trigger `+trigger[1]); err != nil {
			return fmt.Errorf("turn %s on again: %w", trigger[1], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the delivery history: %w", err)
	}
	return nil
}

// earlier is the earlier of two moments.
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
