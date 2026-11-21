package api

import (
	"net/http"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// deliveryFlagsSource is flags_source: whether stored failed-change flags fed the change failure
// rate and time to restore. packages/contracts/src/dispatch-api.ts's DeliveryMeasuresResponse
// declares its closed set, "none" | "stored"; this server writes only "none".
type deliveryFlagsSource string

// deliveryFlagsSourceNone is every answer while Dispatch stores no failed-change flags: no failure
// source exists (a flag counts only once the dora role holder confirms it, LEGION-294), so
// measures.Compute is handed none, the change failure rate's rates are null and its counts 0, and
// time to restore holds no median, each with no verdict, as their evals read "no data" (root R11:
// 0 is a value).
const deliveryFlagsSourceNone deliveryFlagsSource = "none"

// deliveryMeasuresResponse is GET /api/v1/delivery/measures, and the timeline's `measures`: the
// measures for the window over the pull requests the timeline's search and facets select, the
// target of the eval that states each (deliveryTargets), whether each is met by that eval's own
// verdict (measuresStatus), the open P0 issues with no owner, and the timeline's freshness.
// Mirrors packages/contracts/src/dispatch-api.ts's DeliveryMeasuresResponse exactly. It lives
// here rather than beside the timeline's views in delivery/model.go because the measures package
// imports delivery.
type deliveryMeasuresResponse struct {
	Window      delivery.DeliveryWindowView    `json:"window"`
	ComputedAt  time.Time                      `json:"computed_at"`
	Measures    measures.Result                `json:"measures"`
	FlagsSource deliveryFlagsSource            `json:"flags_source"`
	UnownedP0   []delivery.UnownedIssue        `json:"unowned_p0"`
	Targets     measures.Targets               `json:"targets"`
	Status      measures.Status                `json:"status"`
	Freshness   delivery.DeliveryFreshnessView `json:"freshness"`
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
	settings, ok := s.deliverySettings(w, r)
	if !ok {
		return
	}
	population, err := s.readDeliveryPopulation(r.Context(), settings, from, to, query, false)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryMeasuresCompute(settings, population, from, to, time.Now().UTC()))
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
// read for that window or narrowed to it. It reads nothing; now is only computed_at.
func deliveryMeasuresCompute(settings delivery.DeliverySettings, pop deliveryPopulation, from, to, now time.Time) deliveryMeasuresResponse {
	return deliveryMeasuresOver(settings, pop, pop.derive(settings), from, to, now)
}

// deliveryMeasuresOver measures [from, to) over population and what derive computed from it.
// Facets narrow the pull-request measures (lead time, rework share, the per-PR failure rate)
// exactly as they narrow the timeline's prs[]; deploys and runs follow the window alone: every
// deploy-workflow run on main started in [from, to), each a deploy when its production job
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
func deliveryMeasuresOver(settings delivery.DeliverySettings, pop deliveryPopulation, derived deliveryDerived, from, to, now time.Time) deliveryMeasuresResponse {
	applies := delivery.ProductionApplies(pop.windowRuns, pop.windowJobs, settings.Production()).Applies
	reached := map[int64]bool{}
	for _, apply := range applies {
		reached[apply.RunID] = true
	}
	runs := make([]measures.Run, len(pop.windowRuns))
	for i, run := range pop.windowRuns {
		runs[i] = measures.Run{
			ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
			ReachedProduction: reached[run.RunID], ShippedPRs: pop.windowShipped[run.RunID],
		}
	}
	unowned := pop.unownedP0
	if unowned == nil {
		unowned = []delivery.UnownedIssue{}
	}
	result := measures.Compute(derived.measuresPRs(), runs, nil, measures.Window{From: from, To: to})
	return deliveryMeasuresResponse{
		Window:      delivery.DeliveryWindowView{From: from, To: to},
		ComputedAt:  now,
		Measures:    result,
		FlagsSource: deliveryFlagsSourceNone,
		UnownedP0:   unowned,
		Targets:     deliveryTargets(pop.targetEvals),
		Status:      measuresStatus(pop.targetEvals, evals.ResultStatistics(result, len(unowned))),
		Freshness:   derived.freshness,
	}
}

// measuresStatus judges each KPI of a measures answer by its eval's target: figures is the
// answer's own statistics of each eval (evals.ResultStatistics), so each verdict is the one the
// eval's read states of the same figures (deliveryVerdictOf), judged once per eval.
// MergeToProductionMedian and ChangeFailureRateUpperBound are each the verdict of one statistic of
// an eval, the median merge to production and the failure rate's upper bound, against that eval's
// target, answered only where the eval's judges name that statistic (deliveryJudgeVerdict): a
// figure no judge holds to the target has no verdict, so the judges a person sets decide which
// figures are judged (R5). The catalogue's judges answer the median's verdict (merge to production
// judges its longest change and its median) and no upper-bound verdict (the change failure rate
// judges its per-deploy rate).
func measuresStatus(entries map[string]evals.Entry, figures map[string]map[string]evals.Statistics) measures.Status {
	verdict := func(slug string) *evals.Verdict {
		return deliveryVerdictOf(entries, slug, figures[slug])
	}
	changeFailRate, mergeToProduction := verdict(evals.SlugChangeFailRate), verdict(evals.SlugMergeToProduction)
	return measures.Status{
		DeploysPerDay:               verdictMet(verdict(evals.SlugDeployFrequency)),
		ChangeFailureRate:           verdictMet(changeFailRate),
		ChangeFailureRateUpperBound: deliveryJudgeVerdict(changeFailRate, measures.SeriesUpperBound+".rate"),
		MergeToProductionMedian:     deliveryJudgeVerdict(mergeToProduction, measures.SeriesMinutes+".median"),
		MergeToProduction:           verdictMet(mergeToProduction),
		OpenedToMerge:               verdictMet(verdict(evals.SlugPROpenToMerge)),
		DeployRunSuccess:            verdictMet(verdict(evals.SlugDeployRunSuccess)),
		UnownedP0:                   verdictMet(verdict(evals.SlugUnownedP0)),
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
// eval's target (deliveryVerdictOf): null where there is no verdict, or no figure it judges has a
// value.
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
