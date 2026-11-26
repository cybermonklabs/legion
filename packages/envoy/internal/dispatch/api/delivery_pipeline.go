package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
// min_n of pr-ci's job series in the deploy repository's project (the one whose default
// repository it is), or measures.SlowestMinResults where no project or more than one publishes
// the deploy repository as its default or that project holds no pr-ci eval yet
// (evals.PipelineMinN), and 1 where that min_n is null, no minimum, since a job needs a timed
// result to have a median to rank by (measures.FoldSlowestJobs); so the page says what it ranked
// by.
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
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	population, err := s.readPipelinePopulation(r.Context(), settings, at)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryPipelineCompute(settings, population, at, time.Now().UTC()))
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

// pipelinePopulation is every read the Pipeline page's answer for one `at` makes: the PR-checks
// pull_request runs and the deploy push runs on main started in the shown days, each with its
// jobs, the population pull requests merged in them (their lead times), and the evals of
// evals.PipelineSlugs the deploy repository's project holds
// (evals.Store.DeployRepoEvals): the targets the page states and its lead-time cards are judged
// by, and pr-ci, whose job series' min_n its slowest jobs are ranked by (evals.PipelineMinN).
type pipelinePopulation struct {
	checks, deploys delivery.RunsWithJobs
	leadTimes       deliveryPopulation
	deployRepoEvals map[string]evals.Entry
}

// readPipelinePopulation reads the Pipeline page's runs and pull requests for the shown days
// ending on at's UTC day, and the evals it judges and ranks by. Runs are read by start over the
// whole of the shown days, today included past at, by delivery.ReadCIRows, the read the CI evals'
// day rows make: the PR-checks runs with every job, which their job timings and failing jobs tally
// whatever their names (measures.PRChecks), and the deploy runs with the jobs the deploy cards
// follow and no other (measures.DeployJobs reads none else). The three reads depend on none of
// each other, and are read side by side.
func (s *server) readPipelinePopulation(ctx context.Context, settings delivery.DeliverySettings, at time.Time) (pipelinePopulation, error) {
	from := measures.WindowStart(at)
	to := from.AddDate(0, 0, measures.ShownDayCount)
	var pop pipelinePopulation
	if err := store.ReadSideBySide(ctx,
		func(ctx context.Context) error {
			ci, err := delivery.ReadCIRows(ctx, s.deps.Store.Pool, settings.DeployRepo, from, to, measures.DeployCardJobNames(measures.DeployCards(settings)))
			pop.checks, pop.deploys = ci.Checks, ci.Deploys
			return err
		},
		func(ctx context.Context) (err error) {
			pop.leadTimes, err = s.readDeliveryRows(ctx, settings, from, to, url.Values{}, false, false)
			return err
		},
		func(ctx context.Context) (err error) {
			pop.deployRepoEvals, err = s.registry().DeployRepoEvals(ctx, settings.DeployRepo, evals.PipelineSlugs)
			return err
		},
	); err != nil {
		return pipelinePopulation{}, err
	}
	return pop, nil
}

// deliveryPipelineCompute is GET /api/v1/delivery/pipeline's body as of at, from a population read
// for at or narrowed to it. It reads nothing; now is only computed_at. The PR checks, the deploy
// cards and the lead times build each shown day's rows into day parts and fold them with the folds
// the CI and delivery evals read stored parts with (measures.PRChecks, DeployJobs and LeadTimes),
// the slowest jobs ranked by pr-ci's min_n through the ranking pr-ci's items sorted by median read
// (measures.FoldSlowestJobs), and each lead-time card judged by its eval's target over the shown
// days' figures (deliveryVerdict); the failing-jobs table is read from the run jobs themselves
// (measures.FailingJobs).
func deliveryPipelineCompute(settings delivery.DeliverySettings, pop pipelinePopulation, at, now time.Time) deliveryPipelineResponse {
	days := measures.ShownDays(at)
	rollup := ""
	if settings.PRChecksRollupJobName != nil {
		rollup = *settings.PRChecksRollupJobName
	}
	minN := evals.PipelineMinN(pop.deployRepoEvals[evals.SlugPRCI])
	run, slowest, failing := measures.PRChecks(pop.checks.Runs, pop.checks.Jobs, days, at, rollup, minN)
	timings := measures.DeployJobs(pop.deploys.Runs, pop.deploys.Jobs, measures.DeployCards(settings), days)
	stagingE2E := optionalDeployJobTiming(timings, measures.CardStagingE2E)
	stagingApply := optionalDeployJobTiming(timings, measures.CardStagingApply)
	derived := pop.leadTimes.derive(settings)
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
		ProductionApply:   timings[measures.CardProductionApply],
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
func (p pipelinePopulation) narrowedTo(settings delivery.DeliverySettings, at time.Time) pipelinePopulation {
	start := measures.WindowStart(at)
	startedInShownDays := func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(start) }
	narrowed := pipelinePopulation{leadTimes: p.leadTimes.narrowedTo(settings, start), deployRepoEvals: p.deployRepoEvals}
	narrowed.checks.Runs, narrowed.checks.Jobs = runsWhere(p.checks.Runs, p.checks.Jobs, startedInShownDays)
	narrowed.deploys.Runs, narrowed.deploys.Jobs = runsWhere(p.deploys.Runs, p.deploys.Jobs, startedInShownDays)
	return narrowed
}

// pipelineNextExpiry is the next UTC midnight (measures.ShownDays, pipeline.go:154) or the earliest
// failing-jobs edge, whichever comes first: just past a timed PR-checks run's completion +
// measures.FailingWindow (pipeline.go:46), when it leaves the 24 hours the table counts, or a timed
// run's completion itself when that is after now (a clock ahead of Dispatch's), when it enters them.
func pipelineNextExpiry(pop pipelinePopulation, now time.Time) time.Time {
	next := nextUTCMidnight(now)
	consider := func(at time.Time) {
		if at.After(now) && at.Before(next) {
			next = at
		}
	}
	for _, run := range pop.checks.Runs {
		outcome, ok := measures.OutcomeOf(run.Conclusion)
		if !ok || (outcome != measures.OutcomeSuccess && outcome != measures.OutcomeFailure) || run.CompletedAt == nil {
			continue
		}
		consider(*run.CompletedAt)
		consider(run.CompletedAt.Add(measures.FailingWindow).Add(time.Nanosecond))
	}
	return next
}
