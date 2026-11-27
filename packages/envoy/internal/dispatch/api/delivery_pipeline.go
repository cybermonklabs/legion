package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deliveryPipelineResponse is GET /api/v1/delivery/pipeline: the Pipeline page's seven UTC days
// ending on `at`, the PR-checks workflow's run timing with its slowest and recently failing jobs,
// the deploy workflow's staging end-to-end, staging deploy and production deploy jobs (the first
// two null when the settings name none), the merge-to-production and open-to-merge lead times per
// merge day, the targets of the delivery evals (deliveryTargets) and whether each lead-time card
// meets its eval's (status), and the timeline's freshness.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliveryPipelineResponse exactly. It lives here
// beside deliveryMeasuresResponse, since the measures package imports delivery.
type deliveryPipelineResponse struct {
	Days              []string                       `json:"days"`
	ComputedAt        time.Time                      `json:"computed_at"`
	PRChecks          deliveryPipelinePRChecks       `json:"pr_checks"`
	StagingE2E        *measures.DeployJobTiming      `json:"staging_e2e"`
	StagingApply      *measures.DeployJobTiming      `json:"staging_apply"`
	ProductionApply   measures.DeployJobTiming       `json:"production_apply"`
	MergeToProduction measures.LeadTimeTiming        `json:"merge_to_production"`
	OpenedToMerge     measures.LeadTimeTiming        `json:"opened_to_merge"`
	Targets           measures.Targets               `json:"targets"`
	Status            deliveryPipelineStatus         `json:"status"`
	Freshness         delivery.DeliveryFreshnessView `json:"freshness"`
}

// deliveryPipelineStatus is whether each lead-time card of the Pipeline meets its eval's target:
// merge-to-production's and pr-open-to-merge's verdict of the card's figures over the shown days
// (evals.LeadTimeStatistics, deliveryVerdict), so the page states the eval's own verdict rather
// than comparing a figure with the target itself. Null where the eval's target is off, or where no
// figure it judges misses the target and one has no value (evals.VerdictOf).
type deliveryPipelineStatus struct {
	MergeToProduction *bool `json:"merge_to_production"`
	OpenedToMerge     *bool `json:"opened_to_merge"`
}

// deliveryPipelinePRChecks is the PR-checks workflow's part of the Pipeline page: its whole runs'
// timing, its slowest jobs and the jobs that failed in the last 24 hours, both lists never null,
// and the timed results over the shown days a job needed to be ranked among the slowest: the
// min_n of pr-ci's job series in the project whose deploy sources read every source repository
// (deliveryEvals), or measures.SlowestMinResults where two or more projects own them, none is
// read, or that project holds no pr-ci eval yet (evals.PipelineMinN), and 1 where that min_n is
// null, no minimum, since a job needs a timed result to have a median to rank by
// (measures.FoldSlowestJobs); so the page says what it ranked by.
type deliveryPipelinePRChecks struct {
	Run               measures.Timing       `json:"run"`
	SlowestJobs       []measures.JobTiming  `json:"slowest_jobs"`
	SlowestMinResults int                   `json:"slowest_min_results"`
	FailingJobs       []measures.FailingJob `json:"failing_jobs"`
}

// getDeliveryPipeline answers GET /api/v1/delivery/pipeline, computed from stored facts on
// request. Its clock is `at` (RFC3339, default now; parsePipelineAt): the seven shown days end on
// at's UTC day, and the failing-jobs table looks back 24 hours from at, so a test or a screenshot
// pins "today" to a fixture's day. A query naming at more than once is refused
// (repeatedQueryKey), since it would be answered its first value's day. Runs are read by start
// over the whole of the shown days, today included past at. The PR checks are the pull_request
// runs; the deploy jobs are read from the push runs on main (decision 6); the lead times are every
// population pull request merged in the shown days, before any facet, since the Pipeline page
// follows neither the timeline's window nor its facets.
func (s *server) getDeliveryPipeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	if err := repeatedQueryKey(query, deliveryPipelineOneValueKeys); err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	at, err := parsePipelineAt(query, time.Now().UTC())
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	rules, ok := s.deliveryRules(w, r)
	if !ok {
		return
	}
	population, err := s.readPipelinePopulation(r.Context(), rules, at)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryPipelineCompute(rules, population, at, time.Now().UTC()))
}

// parsePipelineAt is the Pipeline's at in UTC: query's at as every delivery read parses a moment
// (parseOptionalRFC3339), or fallback where the query names none, refused too where the first of
// the seven days it shows falls before the year 1 (R18). The years 1 to 9999 bound at itself, and
// an at in the first six days of the year 1 shows days of the year 0, which the eval and delivery
// reads refuse in a from. The first shown day is measures.WindowStart, the day the answer's own
// day list starts on (measures.ShownDays). The route (getDeliveryPipeline) and the live hub's
// pipeline view (parsePipelineLiveView) both read at here, so the two refuse one at alike.
func parsePipelineAt(query url.Values, fallback time.Time) (time.Time, error) {
	at, err := parseOptionalRFC3339(query, "at", fallback)
	if err != nil {
		return time.Time{}, err
	}
	at = at.UTC()
	if first := measures.WindowStart(at); first.Year() < 1 {
		return time.Time{}, fmt.Errorf("at %s shows the %d days from %s, a day of the year %d: the Pipeline's days must lie in the UTC years 1 to 9999",
			at.Format(time.RFC3339Nano), measures.ShownDayCount, first.Format(time.DateOnly), first.Year())
	}
	return at, nil
}

// pipelinePopulation is every read the Pipeline page's answer for one `at` makes: every source
// repository's PR-checks pull_request runs and deploy push runs on main started in the shown days,
// each with its jobs and carrying its repository, the population pull requests merged in them
// (their lead times), and the evals of evals.PipelineSlugs the answer judges by (deliveryEvals):
// the targets the page states and its lead-time cards are judged by, and pr-ci, whose job series'
// min_n its slowest jobs are ranked by (evals.PipelineMinN).
type pipelinePopulation struct {
	checks, deploys delivery.RunsWithJobs
	leadTimes       deliveryPopulation
	deployRepoEvals map[string]evals.Entry
}

// readPipelinePopulation reads the Pipeline page's runs and pull requests for the shown days
// ending on at's UTC day, and the evals it judges and ranks by. Each source repository's runs are
// read by start over the whole of the shown days, today included past at, by delivery.ReadCIRows
// under that repository's rule, the read the CI evals' day rows make: the PR-checks runs with every
// job, which their job timings and failing jobs tally whatever their names
// (measures.PRChecksTimings and FailingJobs), and the deploy runs with the jobs the repository's
// deploy cards follow and no other (measures.DeployJobsOf reads none else). The reads depend on
// none of each other, and are read side by side; the repositories' runs are held together, each in
// start order as one repository's are.
func (s *server) readPipelinePopulation(ctx context.Context, rules *delivery.SourceRules, at time.Time) (pipelinePopulation, error) {
	from := measures.WindowStart(at)
	to := from.AddDate(0, 0, measures.ShownDayCount)
	var pop pipelinePopulation
	sources := rules.All()
	ci := make([]delivery.CIRows, len(sources))
	reads := []func(ctx context.Context) error{
		func(ctx context.Context) (err error) {
			pop.leadTimes, err = s.readDeliveryRows(ctx, rules, from, to, url.Values{}, false, false)
			return err
		},
		func(ctx context.Context) (err error) {
			pop.deployRepoEvals, err = s.deliveryEvals(ctx, rules, evals.PipelineSlugs)
			return err
		},
	}
	for i, rule := range sources {
		reads = append(reads, func(ctx context.Context) (err error) {
			ci[i], err = delivery.ReadCIRows(ctx, s.deps.Store.Pool, rule.DeployRepo, from, to, measures.DeployCardJobNames(measures.DeployCards(rule)))
			return err
		})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return pipelinePopulation{}, err
	}
	pop.checks.Jobs, pop.deploys.Jobs = map[int64][]delivery.DeliveryRunJob{}, map[int64][]delivery.DeliveryRunJob{}
	for _, rows := range ci {
		pop.checks.Runs = append(pop.checks.Runs, rows.Checks.Runs...)
		maps.Copy(pop.checks.Jobs, rows.Checks.Jobs)
		pop.deploys.Runs = append(pop.deploys.Runs, rows.Deploys.Runs...)
		maps.Copy(pop.deploys.Jobs, rows.Deploys.Jobs)
	}
	if len(sources) > 1 {
		byStart := func(a, b delivery.DeliveryRun) int { return a.StartedAt.Compare(b.StartedAt) }
		slices.SortStableFunc(pop.checks.Runs, byStart)
		slices.SortStableFunc(pop.deploys.Runs, byStart)
	}
	return pop, nil
}

// pipelineDeploys is the deploy runs of pop, each source repository's with its jobs and the deploy
// cards its own rule follows (measures.DeployCards), in the order of rules.
func pipelineDeploys(rules *delivery.SourceRules, deploys delivery.RunsWithJobs) []measures.SourceDeployRuns {
	sources := rules.All()
	bySource := make([]measures.SourceDeployRuns, len(sources))
	index := make(map[string]int, len(sources))
	for i, rule := range sources {
		bySource[i] = measures.SourceDeployRuns{Jobs: deploys.Jobs, Cards: measures.DeployCards(rule)}
		index[delivery.RuleKey(rule.DeployRepo)] = i
	}
	for _, run := range deploys.Runs {
		if i, ok := index[delivery.RuleKey(run.Repo)]; ok {
			bySource[i].Runs = append(bySource[i].Runs, run)
		}
	}
	return bySource
}

// pipelineJobsBesideRollups is the jobs of each PR-checks run that completed in the failing jobs'
// window up to at (measures.InFailingWindow, the runs whose jobs measures.FailingJobs reads) but
// the roll-up job its repository's rule names (PRChecksRollupJobName), which the failing-jobs table
// leaves out: every repository names its own.
func pipelineJobsBesideRollups(rules *delivery.SourceRules, checks delivery.RunsWithJobs, at time.Time) map[int64][]delivery.DeliveryRunJob {
	jobs := map[int64][]delivery.DeliveryRunJob{}
	for _, run := range checks.Runs {
		runJobs, held := checks.Jobs[run.RunID]
		if !held || !measures.InFailingWindow(run, at) {
			continue
		}
		if rule, ok := rules.For(run.Repo); ok && rule.PRChecksRollupJobName != nil {
			rollup := *rule.PRChecksRollupJobName
			runJobs = slices.DeleteFunc(slices.Clone(runJobs), func(job delivery.DeliveryRunJob) bool { return job.Name == rollup })
		}
		jobs[run.RunID] = runJobs
	}
	return jobs
}

// deliveryPipelineCompute is GET /api/v1/delivery/pipeline's body as of at, from a population read
// for at or narrowed to it, each source repository's runs judged by its own rule of rules. It reads
// nothing; now is only computed_at. The PR checks, the deploy cards and the lead times build each
// shown day's rows into day parts and fold them with the folds the CI and delivery evals read
// stored parts with (measures.PRChecksTimings, DeployJobsOf and LeadTimes), the slowest jobs ranked by
// pr-ci's min_n through the ranking pr-ci's items sorted by median read (measures.FoldSlowestJobs),
// and each lead-time card judged by its eval's target over the shown days' figures
// (deliveryVerdict); the failing-jobs table is read from the run jobs themselves, each run's
// without its repository's roll-up job (measures.FailingJobs, pipelineJobsBesideRollups). A card
// no source's rule names is null, and the production deploy card, which every github_workflow
// source names, is empty where no source reads a repository.
func deliveryPipelineCompute(rules *delivery.SourceRules, pop pipelinePopulation, at, now time.Time) deliveryPipelineResponse {
	days := measures.ShownDays(at)
	minN := evals.PipelineMinN(pop.deployRepoEvals[evals.SlugPRCI])
	run, slowest := measures.PRChecksTimings(pop.checks.Runs, pop.checks.Jobs, days, minN)
	failing := measures.FailingJobs(pop.checks.Runs, pipelineJobsBesideRollups(rules, pop.checks, at), at, "")
	timings := measures.DeployJobsOf(pipelineDeploys(rules, pop.deploys), days)
	production, named := timings[measures.CardProductionApply]
	if !named {
		production = measures.DeployJobs(nil, nil, []measures.DeployJobCard{{CardID: measures.CardProductionApply}}, days)[measures.CardProductionApply]
	}
	stagingE2E := optionalDeployJobTiming(timings, measures.CardStagingE2E)
	stagingApply := optionalDeployJobTiming(timings, measures.CardStagingApply)
	derived := pop.leadTimes.derive(rules)
	mergeToProduction, openedToMerge := measures.LeadTimes(derived.measuresPRs(), days)
	leadTime := func(slug string, timing measures.LeadTimeTiming) *bool {
		return deliveryVerdict(pop.deployRepoEvals, slug, map[string]evals.Statistics{measures.SeriesMinutes: evals.LeadTimeStatistics(timing.Total)})
	}
	return deliveryPipelineResponse{
		Days:       days,
		ComputedAt: now,
		PRChecks: deliveryPipelinePRChecks{
			Run: run, SlowestJobs: slowest, SlowestMinResults: max(minN, 1), FailingJobs: failing,
		},
		StagingE2E:        stagingE2E,
		StagingApply:      stagingApply,
		ProductionApply:   production,
		MergeToProduction: mergeToProduction,
		OpenedToMerge:     openedToMerge,
		Targets:           deliveryTargets(pop.deployRepoEvals),
		Status: deliveryPipelineStatus{
			MergeToProduction: leadTime(evals.SlugMergeToProduction, mergeToProduction),
			OpenedToMerge:     leadTime(evals.SlugPROpenToMerge, openedToMerge),
		},
		Freshness: derived.freshness,
	}
}

func optionalDeployJobTiming(timings map[measures.DeployJobCardID]measures.DeployJobTiming, card measures.DeployJobCardID) *measures.DeployJobTiming {
	timing, exists := timings[card]
	if !exists {
		return nil
	}
	return &timing
}

// narrowedTo is the population for a later `at`: when at's UTC day moved, the runs started before
// the new first shown day leave and the lead times' pull requests narrow to it; the failing-jobs
// table needs nothing taken out, since deliveryPipelineCompute reads its 24 hours from at, and the
// evals are the ones the population was read with.
func (p pipelinePopulation) narrowedTo(rules *delivery.SourceRules, at time.Time) pipelinePopulation {
	start := measures.WindowStart(at)
	startedInShownDays := func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(start) }
	narrowed := pipelinePopulation{leadTimes: p.leadTimes.narrowedTo(rules, start), deployRepoEvals: p.deployRepoEvals}
	narrowed.checks.Runs, narrowed.checks.Jobs = runsWhere(p.checks.Runs, p.checks.Jobs, startedInShownDays)
	narrowed.deploys.Runs, narrowed.deploys.Jobs = runsWhere(p.deploys.Runs, p.deploys.Jobs, startedInShownDays)
	return narrowed
}

// failingJobsNextExpiry is the next moment a failing-jobs list read at now from checks, the
// PR-checks runs it counts, moves by the clock alone: the next UTC midnight (measures.ShownDays,
// pipeline.go:154) or the earliest failing-jobs edge, whichever comes first: just past a timed run's
// completion + measures.FailingWindow (pipeline.go:46), when it leaves the 24 hours the list counts,
// or a timed run's completion itself when that is after now (a clock ahead of Dispatch's), when it
// enters them. The Pipeline's failing-jobs table and pr-ci's failures items list (failingJobRows)
// both expire by it.
func failingJobsNextExpiry(checks []delivery.DeliveryRun, now time.Time) time.Time {
	next := nextUTCMidnight(now)
	consider := func(at time.Time) {
		if at.After(now) && at.Before(next) {
			next = at
		}
	}
	for _, run := range checks {
		outcome, ok := measures.OutcomeOf(run.Conclusion)
		if !ok || (outcome != measures.OutcomeSuccess && outcome != measures.OutcomeFailure) || run.CompletedAt == nil {
			continue
		}
		consider(*run.CompletedAt)
		consider(run.CompletedAt.Add(measures.FailingWindow).Add(time.Nanosecond))
	}
	return next
}
