package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// deliveryPipelineResponse is GET /api/v1/delivery/pipeline: the Pipeline page's UTC days (the
// seven ending on `at`, or the window `from` and `to` name), the PR-checks workflow's run timing
// with its slowest and recently failing jobs, the deploy workflow's staging end-to-end, staging
// deploy and production deploy jobs (the first two null when the settings name none), the
// merge-to-production and open-to-merge lead times per merge day, the targets the two lead-time
// cards are judged against, and the timeline's freshness.
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
	Freshness         delivery.DeliveryFreshnessView `json:"freshness"`
}

// deliveryPipelinePRChecks is the PR-checks workflow's part of the Pipeline page: its whole runs'
// timing, its slowest jobs and the jobs that failed over the window's last day, both lists never
// null.
type deliveryPipelinePRChecks struct {
	Run         measures.Timing       `json:"run"`
	SlowestJobs []measures.JobTiming  `json:"slowest_jobs"`
	FailingJobs []measures.FailingJob `json:"failing_jobs"`
}

// pipelineWindow is the span one Pipeline answer covers: the whole UTC days it shows and the
// instant it stops at, which the failing-jobs table looks back measures.FailingWindow from and
// past which no result is counted.
type pipelineWindow struct {
	// lastDay is an instant on the last UTC day shown.
	lastDay time.Time
	// dayCount is how many UTC days are shown, ending on lastDay's.
	dayCount int
	// end is where the window stops: `at` for the at form, `to` for the from/to form, and the
	// present for a live view whose window rolls.
	end time.Time
}

// pipelineWindowEndingAt is the dayCount UTC days ending on at's, stopping at at: what `at` alone
// names, and what a live view's preset names at the present.
func pipelineWindowEndingAt(at time.Time, dayCount int) pipelineWindow {
	return pipelineWindow{lastDay: at, dayCount: dayCount, end: at}
}

// pipelineWindowBetween is the days [from, to) covers, stopping at to: its length rounded up to a
// whole day, ending on the last day to includes, so a window of whole days (the only kind the page
// sends) shows exactly its own days and its last 24 hours are its last day.
func pipelineWindowBetween(from, to time.Time) pipelineWindow {
	const day = 24 * time.Hour
	dayCount := int((to.Sub(from) + day - 1) / day)
	return pipelineWindow{lastDay: to.Add(-time.Nanosecond), dayCount: max(dayCount, 1), end: to}
}

// days is the window's UTC days, oldest first.
func (w pipelineWindow) days() []string { return measures.ShownDays(w.lastDay, w.dayCount) }

// start is midnight UTC of the window's first day, where its reads begin.
func (w pipelineWindow) start() time.Time { return measures.WindowStart(w.lastDay, w.dayCount) }

// readTo is the midnight after the window's last day: runs are read over whole days, the last one
// included past end, as a day's bucket holds every run that started in it.
func (w pipelineWindow) readTo() time.Time { return w.start().AddDate(0, 0, w.dayCount) }

// parsePipelineWindow reads GET /api/v1/delivery/pipeline's window. `at` (RFC3339, default now) is
// the measures.ShownDayCount days ending on at's UTC day, stopping at at: what the Pipeline page
// and its tests send. `from` and `to`, read by the shared parseDeliveryWindow and bounded by the
// same maxDeliveryWindow, are the window's own days, stopping at to. A request naming at beside
// either is refused: the two name the same thing, and a reader whose window moved would otherwise
// be answered the other one.
func parsePipelineWindow(query url.Values) (pipelineWindow, error) {
	windowed := query.Has("from") || query.Has("to")
	if !query.Has("at") && windowed {
		from, to, err := parseDeliveryWindow(query)
		if err != nil {
			return pipelineWindow{}, err
		}
		return pipelineWindowBetween(from.UTC(), to.UTC()), nil
	}
	if windowed {
		return pipelineWindow{}, errors.New("at is the window's own end, so from and to must not come with it")
	}
	at, err := parseOptionalRFC3339(query, "at", time.Now().UTC())
	if err != nil {
		return pipelineWindow{}, err
	}
	return pipelineWindowEndingAt(at.UTC(), measures.ShownDayCount), nil
}

// getDeliveryPipeline answers GET /api/v1/delivery/pipeline, computed from stored facts on
// request. Its clock is its window's end (parsePipelineWindow): the shown days end on it and the
// failing-jobs table looks back measures.FailingWindow from it, so a test or a screenshot pins
// "today" to a fixture's day. Runs are read by start over the whole of the shown days, the last
// one included past the end. The PR checks are the pull_request runs; the deploy jobs are read
// from the push runs on main (decision 6); the lead times are every population pull request merged
// in the shown days, before any facet, since the Pipeline page follows the window it is given and
// none of the timeline's facets.
func (s *server) getDeliveryPipeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	window, err := parsePipelineWindow(r.URL.Query())
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	population, err := s.readPipelinePopulation(r.Context(), settings, window)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryPipelineCompute(settings, population, window, time.Now().UTC()))
}

// pipelinePopulation is every read the Pipeline page's answer for one window makes: the PR-checks
// pull_request runs and the deploy push runs on main started in the shown days, each with its
// jobs, and the population pull requests merged in them (their lead times).
type pipelinePopulation struct {
	checks, deploys delivery.RunsWithJobs
	leadTimes       deliveryPopulation
}

// readPipelinePopulation reads the Pipeline page's runs and pull requests for the window's days.
// Runs are read by start over the whole of those days, the last one included past the window's
// end, by delivery.ReadCIRows, the read the CI evals' day rows make: the PR-checks runs with every
// job, which their job timings and failing jobs tally whatever their names (measures.PRChecks),
// and the deploy runs with the jobs the deploy cards follow and no other (measures.DeployJobs
// reads none else).
func (s *server) readPipelinePopulation(ctx context.Context, settings delivery.DeliverySettings, window pipelineWindow) (pipelinePopulation, error) {
	from, to := window.start(), window.readTo()
	ci, err := delivery.ReadCIRows(ctx, s.deps.Store.Pool, settings.DeployRepo, from, to, measures.DeployCardJobNames(measures.DeployCards(settings)))
	if err != nil {
		return pipelinePopulation{}, err
	}
	pop := pipelinePopulation{checks: ci.Checks, deploys: ci.Deploys}
	if pop.leadTimes, err = s.readDeliveryRows(ctx, settings, from, to, url.Values{}, false, false); err != nil {
		return pipelinePopulation{}, err
	}
	return pop, nil
}

// deliveryPipelineCompute is GET /api/v1/delivery/pipeline's body for window, from a population
// read for it or narrowed to it. It reads nothing; now is only computed_at. The PR checks, the
// deploy cards and the lead times build each shown day's rows into day parts and fold them with
// the folds the CI and delivery evals read stored parts with (measures.PRChecks, DeployJobs and
// LeadTimes); the failing-jobs table is read from the run jobs themselves (measures.FailingJobs).
func deliveryPipelineCompute(settings delivery.DeliverySettings, pop pipelinePopulation, window pipelineWindow, now time.Time) deliveryPipelineResponse {
	days := window.days()
	rollup := ""
	if settings.PRChecksRollupJobName != nil {
		rollup = *settings.PRChecksRollupJobName
	}
	run, slowest, failing := measures.PRChecks(pop.checks.Runs, pop.checks.Jobs, days, window.end, rollup)
	timings := measures.DeployJobs(pop.deploys.Runs, pop.deploys.Jobs, measures.DeployCards(settings), days)
	stagingE2E := optionalDeployJobTiming(timings, measures.CardStagingE2E)
	stagingApply := optionalDeployJobTiming(timings, measures.CardStagingApply)
	derived := pop.leadTimes.derive(settings)
	mergeToProduction, openedToMerge := measures.LeadTimes(derived.measuresPRs(), days)
	return deliveryPipelineResponse{
		Days:              days,
		ComputedAt:        now,
		PRChecks:          deliveryPipelinePRChecks{Run: run, SlowestJobs: slowest, FailingJobs: failing},
		StagingE2E:        stagingE2E,
		StagingApply:      stagingApply,
		ProductionApply:   timings[measures.CardProductionApply],
		MergeToProduction: mergeToProduction,
		OpenedToMerge:     openedToMerge,
		Targets:           measures.DefaultTargets,
		Freshness:         derived.freshness,
	}
}

func optionalDeployJobTiming(timings map[measures.DeployJobCardID]measures.DeployJobTiming, card measures.DeployJobCardID) *measures.DeployJobTiming {
	timing, exists := timings[card]
	if !exists {
		return nil
	}
	return &timing
}

// narrowedTo is the population for a later window: when its last day moved, the runs started
// before the new first shown day leave and the lead times' pull requests narrow to it; the
// failing-jobs table needs nothing taken out, since deliveryPipelineCompute reads its 24 hours
// from the window's end.
func (p pipelinePopulation) narrowedTo(settings delivery.DeliverySettings, window pipelineWindow) pipelinePopulation {
	start := window.start()
	startedInShownDays := func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(start) }
	narrowed := pipelinePopulation{leadTimes: p.leadTimes.narrowedTo(settings, start)}
	narrowed.checks.Runs, narrowed.checks.Jobs = runsWhere(p.checks.Runs, p.checks.Jobs, startedInShownDays)
	narrowed.deploys.Runs, narrowed.deploys.Jobs = runsWhere(p.deploys.Runs, p.deploys.Jobs, startedInShownDays)
	return narrowed
}

// pipelineNextExpiry is the next UTC midnight (measures.ShownDays, pipeline.go:156) or the earliest
// failing-jobs edge, whichever comes first: just past a timed PR-checks run's completion +
// measures.FailingWindow (pipeline.go:48), when it leaves the 24 hours the table counts, or a timed
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
