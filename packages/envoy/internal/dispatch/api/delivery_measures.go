package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// deliveryMeasuresResponse is GET /api/v1/delivery/measures, and the timeline's `measures`: the
// measures for the window over the pull requests the timeline's search and facets select, the
// target of the eval that states each (deliveryTargets), whether each is met by that eval's own
// verdict (measuresStatus), the open P0 issues with no owner, and the timeline's freshness.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliveryMeasuresResponse exactly. It lives
// here rather than beside the timeline's views in delivery/model.go because the measures package
// imports delivery.
type deliveryMeasuresResponse struct {
	Window     delivery.DeliveryWindowView    `json:"window"`
	ComputedAt time.Time                      `json:"computed_at"`
	Measures   deliveryMeasures               `json:"measures"`
	UnownedP0  []delivery.UnownedIssue        `json:"unowned_p0"`
	Targets    measures.Targets               `json:"targets"`
	Status     measures.Status                `json:"status"`
	Freshness  delivery.DeliveryFreshnessView `json:"freshness"`
}

// deliveryMeasures is the `measures` of GET /api/v1/delivery/measures, of the timeline's measures
// and of the live measures view: what measures.Result holds, written through the one projection
// the three share (deliveryMeasuresOf). Only the failure measures are written otherwise. A
// Delivery page of the previous build is still served while the two builds roll
// (cmd/dispatch/README.md, "Two builds at once", "Response members"), and it reads
// change_failure_rate.per_pr, change_failure_rate.per_deploy and time_to_restore with no null
// guard. So change_failure_rate is always an object, never null, and carries those members with
// every leaf null beside its own failed, total and rate, and time_to_restore stays with a null
// median: this build's recovery time is failed_deployment_recovery, since that page would show a
// figure at time_to_restore as its own broken-PR-to-fix median.
type deliveryMeasures struct {
	DeployFrequency   measures.DeployFrequency `json:"deploy_frequency"`
	LeadTime          measures.LeadTime        `json:"lead_time"`
	ChangeFailureRate deliveryChangeFailRate   `json:"change_failure_rate"`
	// TimeToRestore is kept, its median always null, for the page of the previous build
	// alone, which reads time_to_restore.median_minutes with no null guard. It goes with the old
	// routes, after the roll (deliveryRetiredPRFlagCounts says when).
	TimeToRestore deliveryRetiredTimeToRestore `json:"time_to_restore"`
	// FailedDeploymentRecovery is failed-deploy-recovery's fold (measures.FoldRecovery), null
	// where the window holds no day part of it.
	FailedDeploymentRecovery *measures.Recovery `json:"failed_deployment_recovery"`
	// DeploymentReworkRate is deployment-rework-rate's fold (measures.FoldReworkRate), null where
	// the window holds no day part of it.
	DeploymentReworkRate *measures.ReworkRate      `json:"deployment_rework_rate"`
	ReworkShare          float64                   `json:"rework_share"`
	DeployRunSuccess     measures.DeployRunSuccess `json:"deploy_run_success"`
	Daily                []measures.DailyPoint     `json:"daily"`
}

// deliveryChangeFailRate is change-fail-rate's fold (measures.FoldChangeFailRate) as the measures
// answer writes it: the deploys with a counted failure, every deploy and the first over the
// second, each null where the window holds no day part of it ("no data", as the eval reads it),
// beside PerPR and PerDeploy, which no build measures.
type deliveryChangeFailRate struct {
	Failed *int     `json:"failed"`
	Total  *int     `json:"total"`
	Rate   *float64 `json:"rate"`
	// PerPR and PerDeploy are kept, every leaf null in every window, for the page of the build
	// before the failure measures changed: it reads change_failure_rate.per_pr.rate, per_deploy.rate and
	// per_deploy.upper_bound_rate with no null guard on the objects, and shows no value for a null
	// among them. They go with the old routes, after the roll (deliveryRetiredPRFlagCounts says
	// when).
	PerPR     deliveryRetiredPRFlagCounts     `json:"per_pr"`
	PerDeploy deliveryRetiredDeployFlagCounts `json:"per_deploy"`
}

// deliveryRetiredPRFlagCounts is change_failure_rate.per_pr as the previous Delivery page reads
// it: the per-PR counts of failed-change flags and their rate, which that
// build computed and no build computes now, so nothing sets a field and each is always null. The
// three retired members (this, deliveryRetiredDeployFlagCounts and deliveryRetiredTimeToRestore)
// and their nulls go when the old routes go: the next release, which removes GET and PUT
// /api/v1/settings/delivery once the roll is over and no page of the earlier build is served,
// removes them too (cmd/dispatch/README.md, "Two builds at once": a member is removed only once
// no page still serving reads it).
type deliveryRetiredPRFlagCounts struct {
	Confirmed *int     `json:"confirmed"`
	Pending   *int     `json:"pending"`
	Rejected  *int     `json:"rejected"`
	Reverts   *int     `json:"reverts"`
	Total     *int     `json:"total"`
	Rate      *float64 `json:"rate"`
}

// deliveryRetiredDeployFlagCounts is change_failure_rate.per_deploy as that page reads it: the
// per-deploy counts of failed-change flags, their rate and its upper bound, every field always
// null (deliveryRetiredPRFlagCounts says until when).
type deliveryRetiredDeployFlagCounts struct {
	Confirmed          *int     `json:"confirmed"`
	Total              *int     `json:"total"`
	Rate               *float64 `json:"rate"`
	ConfirmedOrPending *int     `json:"confirmed_or_pending"`
	UpperBoundRate     *float64 `json:"upper_bound_rate"`
}

// deliveryRetiredTimeToRestore is time_to_restore as that page reads it: its median from a broken
// pull request's deploy to its fix's, always null (deliveryRetiredPRFlagCounts says until when).
// The recovery time this build measures is deliveryMeasures.FailedDeploymentRecovery, under a name
// of its own, since that page would show a figure here as its broken-PR-to-fix median.
type deliveryRetiredTimeToRestore struct {
	MedianMinutes *float64 `json:"median_minutes"`
}

// deliveryMeasuresOf is result as the measures answer writes it (deliveryMeasures): the change
// fail rate's counts and rate where its fold has a value, null otherwise, and every retired leaf
// null.
func deliveryMeasuresOf(result measures.Result) deliveryMeasures {
	written := deliveryMeasures{
		DeployFrequency: result.DeployFrequency, LeadTime: result.LeadTime,
		FailedDeploymentRecovery: result.TimeToRestore, DeploymentReworkRate: result.DeploymentReworkRate,
		ReworkShare: result.ReworkShare, DeployRunSuccess: result.DeployRunSuccess, Daily: result.Daily,
	}
	if rate := result.ChangeFailureRate; rate != nil {
		written.ChangeFailureRate = deliveryChangeFailRate{Failed: &rate.Failed, Total: &rate.Total, Rate: rate.Rate}
	}
	return written
}

// getDeliveryMeasures answers GET /api/v1/delivery/measures, computed from stored facts on
// request: the window's population read once (readDeliveryPopulation) and measured by
// deliveryMeasuresCompute, the two halves the live hub calls too.
func (s *server) getDeliveryMeasures(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	from, to, err := parseDeliveryWindowQuery(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	rules, ok := s.deliveryRules(w, r)
	if !ok {
		return
	}
	population, err := s.readDeliveryPopulation(r.Context(), rules, from, to, query, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryMeasuresCompute(rules, population, from, to, time.Now().UTC()))
}

// measuresPRs is the derived pull requests that pass the search and facets, as the measures
// package reads them. A partial row's lead-time fields are not yet knowable, so it is skipped
// rather than read as zero.
func (d deliveryDerived) measuresPRs() []measures.PR {
	prs := make([]measures.PR, 0, len(d.rows))
	for _, row := range d.rows {
		view := row.view
		if !row.matches(d.selection, "") || view.CreatedAt == nil || view.MergedAt == nil {
			continue
		}
		prs = append(prs, measures.PR{
			ID: view.ID, CreatedAt: *view.CreatedAt, MergedAt: *view.MergedAt, FirstCommitAt: view.FirstCommitAt,
			DeployRunID: view.DeployRun, DeployedAt: view.DeployedAt, Rework: view.Rework,
		})
	}
	return prs
}

// deliveryMeasuresCompute is GET /api/v1/delivery/measures' body for [from, to) from a population
// read for that window or narrowed to it, each source repository's runs judged by its own rule of
// rules. It reads nothing; now is only computed_at.
func deliveryMeasuresCompute(rules *delivery.SourceRules, pop deliveryPopulation, from, to, now time.Time) deliveryMeasuresResponse {
	return deliveryMeasuresOver(rules, pop, pop.derive(rules), from, to, now)
}

// deliveryMeasuresOver measures [from, to) over population and what derive computed from it.
// Facets narrow the pull-request measures (lead time, rework share) exactly as they narrow the
// timeline's prs[]; deploys and runs follow the window alone: every deploy-workflow run on main
// started in [from, to), each a deploy when its production job
// succeeded. A deploy is one with pull requests (deploys_with_prs, with_prs_per_day) when it ships
// any population pull request first, whenever that pull request merged and whatever the facets
// (pop.windowShipped, delivery.Deploys.ShippedBy), so deploys_with_prs belongs to the deploy, not
// to the window or the filter, and is the rule the deploy-frequency eval's day rows count by
// (measures.DeployFrequencyDay). Counting only the pull requests merged inside the window would
// leave out a deploy that shipped only pull requests merged before the window. The measures route
// and the timeline both answer through it,
// so the two never disagree. measures.Compute builds the window's rows into day parts with the
// day builders and folds them with the folds the delivery evals read stored parts with, one fold
// per measure, and each KPI is judged by its eval's target as the population read it.
//
// The three failure measures fold their evals' stored day parts (measures.FailureParts), those of
// the window's days (evalDaysOf, the days an eval read of the window folds) of the one delivery-on
// project (evals.DeploymentFailures.Within): this route is deployment-wide, and a deploy that
// counts for two projects would count twice in a sum of theirs, so with no delivery-on project or
// several they read null, "no data", each with no verdict. So do they for a window that starts
// before the project's failures_measured_since, as its evals read one; and otherwise they are the
// values its evals' read of the same window answers (dispatch://INFRA-978, k11), written through
// deliveryMeasuresOf.
func deliveryMeasuresOver(rules *delivery.SourceRules, pop deliveryPopulation, derived deliveryDerived, from, to, now time.Time) deliveryMeasuresResponse {
	reached := map[int64]bool{}
	for _, applies := range deliveryApplies(rules, pop.windowRuns, pop.windowJobs) {
		for _, apply := range applies {
			reached[apply.RunID] = true
		}
	}
	runs := make([]measures.Run, len(pop.windowRuns))
	for i, run := range pop.windowRuns {
		runs[i] = measures.Run{
			ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
			ReachedProduction: reached[run.RunID], ShippedPRs: len(pop.windowShipped[run.RunID]),
		}
	}
	unowned := pop.unownedP0
	if unowned == nil {
		unowned = []delivery.UnownedIssue{}
	}
	failures := pop.failures.Within(evalDaysOf(from, to))
	result := measures.Compute(derived.measuresPRs(), runs, failures, measures.Window{From: from, To: to})
	return deliveryMeasuresResponse{
		Window:     delivery.DeliveryWindowView{From: from, To: to},
		ComputedAt: now,
		Measures:   deliveryMeasuresOf(result),
		UnownedP0:  unowned,
		Targets:    deliveryTargets(pop.targetEvals),
		Status:     measuresStatus(pop.targetEvals, evals.ResultStatistics(result, len(unowned))),
		Freshness:  derived.freshness,
	}
}

// measuresStatus judges each KPI of a measures answer by its eval's target: figures is the
// answer's own statistics of each eval (evals.ResultStatistics), so each verdict is the one the
// eval's read states of the same figures (deliveryVerdictOf), judged once per eval.
// MergeToProductionMedian is the verdict of one statistic of an eval, the median merge to
// production, against that eval's target, answered only where the eval's judges name that
// statistic (deliveryJudgeVerdict): a figure no judge holds to the target has no verdict, so the
// judges a person sets decide which figures are judged (R5). The catalogue's judges answer the
// median's verdict (merge to production judges its longest change and its median).
func measuresStatus(entries map[string]evals.Entry, figures map[string]map[string]evals.Statistics) measures.Status {
	verdict := func(slug string) *evals.Verdict {
		return deliveryVerdictOf(entries, slug, figures[slug])
	}
	mergeToProduction := verdict(evals.SlugMergeToProduction)
	return measures.Status{
		DeploysPerDay:           verdictMet(verdict(evals.SlugDeployFrequency)),
		ChangeFailureRate:       verdictMet(verdict(evals.SlugChangeFailRate)),
		MergeToProductionMedian: deliveryJudgeVerdict(mergeToProduction, measures.SeriesMinutes+".median"),
		MergeToProduction:       verdictMet(mergeToProduction),
		OpenedToMerge:           verdictMet(verdict(evals.SlugPROpenToMerge)),
		DeployRunSuccess:        verdictMet(verdict(evals.SlugDeployRunSuccess)),
		UnownedP0:               verdictMet(verdict(evals.SlugUnownedP0)),
	}
}

// deliveryVerdictOf is the verdict of slug's eval of figures, a delivery answer's own statistics
// of that eval, by the rule every eval read judges by (evals.VerdictOf): nil where entries, the
// evals the answer's population read (evals.Store.DeployRepoEvals), holds no such eval, or its
// target is off.
func deliveryVerdictOf(entries map[string]evals.Entry, slug string, figures map[string]evals.Statistics) *evals.Verdict {
	entry, held := entries[slug]
	if !held {
		return nil
	}
	return evals.VerdictOf(entry, figures)
}

// deliveryVerdict is whether figures, a delivery answer's own statistics of slug's eval, meet that
// eval's target (deliveryVerdictOf): null where there is no verdict, or where no figure it judges
// misses the target and one has no value (evals.VerdictOf).
func deliveryVerdict(entries map[string]evals.Entry, slug string, figures map[string]evals.Statistics) *bool {
	return verdictMet(deliveryVerdictOf(entries, slug, figures))
}

// verdictMet is whether verdict is met, null where there is no verdict or it cannot be judged.
func verdictMet(verdict *evals.Verdict) *bool {
	if verdict == nil {
		return nil
	}
	return verdict.Met
}

// deliveryJudgeVerdict is whether judge, one `<series>.<stat>` of an eval, meets that eval's target
// in verdict, the eval's verdict of a delivery answer's figures (deliveryVerdictOf): null where
// there is no verdict, the eval's judges (evals.JudgesOf) do not name judge, or judge has no value.
func deliveryJudgeVerdict(verdict *evals.Verdict, judge string) *bool {
	if verdict == nil {
		return nil
	}
	if index := slices.IndexFunc(verdict.Judges, func(judged evals.Judge) bool { return judged.Judge == judge }); index >= 0 {
		return verdict.Judges[index].Met
	}
	return nil
}

// deliveryTargets is the target of each eval of evals.DeliveryTargetSlugs entries holds, its value,
// inclusiveness and direction and the statistics its verdict judges (evals.JudgesOf), null where
// its target is off: every delivery answer's `targets`, so the page names and colours by the
// figures each verdict of `status` is of, in the direction the eval states.
func deliveryTargets(entries map[string]evals.Entry) measures.Targets {
	target := func(slug string) *measures.Target {
		entry := entries[slug]
		definition := entry.Definition
		if definition.Target == nil {
			return nil
		}
		return &measures.Target{
			Value: *definition.Target, Inclusive: definition.TargetInclusive, Direction: definition.Direction, Judges: evals.JudgesOf(entry),
		}
	}
	return measures.Targets{
		DeploysPerDay:              target(evals.SlugDeployFrequency),
		ChangeFailureRate:          target(evals.SlugChangeFailRate),
		MergeToProductionMinutes:   target(evals.SlugMergeToProduction),
		OpenedToMergeMedianMinutes: target(evals.SlugPROpenToMerge),
		DeployRunSuccessRate:       target(evals.SlugDeployRunSuccess),
		UnownedP0:                  target(evals.SlugUnownedP0),
	}
}
