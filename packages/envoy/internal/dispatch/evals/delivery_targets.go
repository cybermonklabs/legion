package evals

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The deployment-wide delivery answers (GET /api/v1/delivery/measures, the timeline's measures and
// GET /api/v1/delivery/pipeline) show figures the delivery evals state, computed from the
// deployment's population rather than read from one project's day rows. They judge them by those
// evals' targets and rank the Pipeline's slowest jobs by pr-ci's min_n as a person sets them (R5):
// the evals of the project whose deploy source reads the deploy repository (DeployRepoEvals),
// each figure judged by the one rule an eval read judges by (VerdictOf), so a delivery answer's
// verdict of a figure is its eval's verdict of the same figure. Each answer states every target
// with its eval's direction and the statistics that verdict judges (JudgesOf), so its reader names
// the figure a verdict is of rather than a statistic of its own choosing.

// DeliveryTargetSlugs are the evals whose targets the measures answer judges its KPIs by and
// states, and PipelineSlugs the evals the Pipeline answer reads: the same six, whose targets it
// states and two of which judge its lead-time cards, and pr-ci, whose job series' min_n ranks its
// slowest jobs (PipelineMinN).
// Targets of cards outside these six are judged on the Evals tab, which shows every card.
var (
	DeliveryTargetSlugs = []string{SlugDeployFrequency, SlugChangeFailRate, SlugMergeToProduction, SlugPROpenToMerge, SlugDeployRunSuccess, SlugUnownedP0}
	PipelineSlugs       = append(slices.Clone(DeliveryTargetSlugs), SlugPRCI)
)

// Judge is one statistic a target judges, `<series>.<stat>`, its value over the range and whether
// it meets the target (null where it has no value, or the eval names no direction).
type Judge struct {
	Judge string   `json:"judge"`
	Value *float64 `json:"value"`
	Met   *bool    `json:"met"`
}

// Verdict is an eval's target judged over a range: the target, its direction and inclusiveness,
// each statistic it judges (target_judges, or the primary series' headline statistic where it
// names none), and met, false where any judge misses, true where every judge meets, null where none
// misses and one cannot be judged.
type Verdict struct {
	Target    float64 `json:"target"`
	Direction string  `json:"direction"`
	Inclusive bool    `json:"inclusive"`
	Met       *bool   `json:"met"`
	Judges    []Judge `json:"judges"`
}

// JudgesOf is the statistics entry's target judges, each `<series>.<stat>`, in the order
// VerdictOf judges them: its target_judges or, where those are empty, the primary series' headline
// statistic (HeadlineStatistic), and none where it names none and holds no primary series. The
// delivery answers state them beside each target (measures.Target), so a reader names the figure
// the verdict is of rather than the one a card shows by default.
func JudgesOf(entry Entry) []string {
	if judges := entry.Definition.TargetJudges; len(judges) > 0 {
		return judges
	}
	if index := slices.IndexFunc(entry.Series, func(one Series) bool { return one.IsPrimary }); index >= 0 {
		primary := entry.Series[index]
		return []string{primary.Series + "." + HeadlineStatistic(entry.Definition, primary)}
	}
	return []string{}
}

// VerdictOf judges entry's target over window, the statistics of its series over one range by
// series name, null where the target is off. It judges each of JudgesOf(entry); a judge naming a
// series entry does not list, or a statistic window does not state, has no value. A higher_better
// eval meets its target at or above it where it is inclusive and above it where it is strict, a
// lower_better eval at or below it and below it; an eval with no direction judges nothing. Every
// reader that judges a figure by its eval's target judges it here: the eval reads, over their
// folded day rows, and the delivery answers, over their own figures (ResultStatistics,
// LeadTimeStatistics).
func VerdictOf(entry Entry, window map[string]Statistics) *Verdict {
	definition := entry.Definition
	if definition.Target == nil {
		return nil
	}
	judges := JudgesOf(entry)
	target := *definition.Target
	verdict := &Verdict{Target: target, Direction: definition.Direction, Inclusive: definition.TargetInclusive, Judges: []Judge{}}
	missed, unknown := false, len(judges) == 0
	for _, judge := range judges {
		name, stat, _ := strings.Cut(judge, ".")
		judged := Judge{Judge: judge}
		if slices.ContainsFunc(entry.Series, func(one Series) bool { return one.Series == name && one.Listed }) {
			judged.Value = window[name][stat]
		}
		if judged.Value != nil {
			value := *judged.Value
			switch {
			case definition.Direction == DirectionHigherBetter && definition.TargetInclusive:
				judged.Met = new(value >= target)
			case definition.Direction == DirectionHigherBetter:
				judged.Met = new(value > target)
			case definition.Direction == DirectionLowerBetter && definition.TargetInclusive:
				judged.Met = new(value <= target)
			case definition.Direction == DirectionLowerBetter:
				judged.Met = new(value < target)
			}
		}
		switch {
		case judged.Met == nil:
			unknown = true
		case !*judged.Met:
			missed = true
		}
		verdict.Judges = append(verdict.Judges, judged)
	}
	switch {
	case missed:
		verdict.Met = new(false)
	case !unknown:
		verdict.Met = new(true)
	}
	return verdict
}

// ResultStatistics is the statistics of each eval of DeliveryTargetSlugs over result, a window's
// delivery measures (measures.Compute), and unownedP0, the count of open P0 issues with no owner,
// by slug and then by series: each figure under the name the eval's own fold gives it, read off
// the folded value the fold reads it off (deployFrequencyOf, deployRunSuccessOf,
// changeFailRateOf, a lead time's spread and count), so a judge of the measures answer reads
// the figure the eval's read of the same parts states. It states every statistic the eval route
// states of those evals but one, unowned-p0's `n`, its count of the window's stored days, which a
// count of the issues open now has no figure of; a register write refuses that one as a judge
// (deliveryJudges).
func ResultStatistics(result measures.Result, unownedP0 int) map[string]map[string]Statistics {
	spread := func(spread measures.Spread) Statistics {
		return Statistics{"n": new(float64(spread.N)), "median": spread.MedianMinutes, "p90": spread.P90Minutes, "max": spread.MaxMinutes}
	}
	stats := map[string]map[string]Statistics{
		SlugDeployFrequency:   {measures.SeriesDeploys: deployFrequencyOf(&result.DeployFrequency)},
		SlugChangeFailRate:    {measures.SeriesPerDeploy: changeFailRateOf(result.ChangeFailureRate)},
		SlugMergeToProduction: {measures.SeriesMinutes: spread(result.LeadTime.MergeToProduction)},
		SlugPROpenToMerge:     {measures.SeriesMinutes: spread(result.LeadTime.OpenedToMerge)},
		SlugDeployRunSuccess:  {measures.SeriesRuns: deployRunSuccessOf(&result.DeployRunSuccess)},
		SlugUnownedP0:         {SeriesUnownedP0: {"value": new(float64(unownedP0))}},
	}
	for _, series := range stats {
		writable(series)
	}
	return stats
}

// deliveryJudges is the judges each eval of DeliveryTargetSlugs takes, by slug, in name order:
// every `<series>.<stat>` the measures answer states a figure of (ResultStatistics), which the
// Pipeline's two lead times state as well (LeadTimeStatistics). A register write refuses any other
// judge of those evals (validateRegistration): the delivery answers judge their figures by these
// evals' targets, and a judge they state no figure of would be a target written and then ignored
// there (R5).
var deliveryJudges = func() map[string][]string {
	judges := map[string][]string{}
	for slug, series := range ResultStatistics(measures.Result{}, 0) {
		for name, stats := range series {
			for stat := range stats {
				judges[slug] = append(judges[slug], name+"."+stat)
			}
		}
		slices.Sort(judges[slug])
	}
	return judges
}()

// DeployRepoEvals reads the current definition and series of each of slugs whose targets the
// deployment-wide delivery answers judge by (dispatch://INFRA-978, D9), by slug, in one snapshot:
// those of the project whose deploy sources read every deploy repository, where one delivery-on
// project owns them all, and none of any project where two or more own them or none is read, since
// no one project's targets judge figures of several projects. The deployment-wide answers count
// every project's rows, so no row names their project; the one a repository names is its owner,
// the delivery-on project whose deploy source reads it (delivery.DeployRules, compared without
// case; dispatch://INFRA-978, D1), where a source that names no repository reads its project's
// published default repository (eval_project_repos, the record the refresher's page commit moves
// in the commit that makes the figures folded under it visible; owners.go), so the delivery routes
// and the Evals tab judge by one state and a person's write of a default repository moves neither
// until that commit: until then the project that owns every repository under the published
// defaults judges, as it did before the write. Where no project is named, and where the project
// holds no version of a slug yet, the slug's catalogue entry stands in: the definition and series
// the seed writes, whose target is main's (measures.DefaultTargets). A slug neither holds is
// absent.
func (s *Store) DeployRepoEvals(ctx context.Context, slugs []string) (map[string]Entry, error) {
	tx, err := s.BeginSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the read of the deploy repositories' evals %v: %w", slugs, err)
	}
	defer tx.Rollback(ctx)
	projects, err := publishedSettingsIn(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read the projects whose deploy sources read the deploy repositories: %w", err)
	}
	entries := make(map[string]Entry, len(slugs))
	if rules := delivery.DeployRules(projects); len(rules) > 0 &&
		!slices.ContainsFunc(rules, func(rule delivery.DeliverySettings) bool { return rule.Project != rules[0].Project }) {
		if entries, err = projectEntries(ctx, tx, rules[0].Project, slugs); err != nil {
			return nil, err
		}
	}
	for _, slug := range slugs {
		if _, held := entries[slug]; held {
			continue
		}
		if index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug }); index >= 0 {
			entries[slug] = Entry{Definition: catalogue[index].definition, Series: catalogue[index].series}
		}
	}
	return entries, nil
}

// publishedSettingsIn is every project's delivery settings read in q (delivery.ListProjectSettings)
// with each project's default repository its published one (eval_project_repos), none where it
// publishes none yet: the settings a source that names no repository is read under by a reader of
// the published state (DeployRepoEvals).
func publishedSettingsIn(ctx context.Context, q delivery.Querier) ([]delivery.ProjectSettings, error) {
	projects, err := delivery.ListProjectSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `select project_key, repo from eval_project_repos`)
	if err != nil {
		return nil, fmt.Errorf("read the published default repositories: %w", err)
	}
	published := map[string]string{}
	var project, repo string
	if _, err := pgx.ForEachRow(rows, []any{&project, &repo}, func() error {
		published[project] = repo
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the published default repositories: %w", err)
	}
	for i := range projects {
		projects[i].DefaultRepo = nil
		if repo, held := published[projects[i].Project]; held {
			projects[i].DefaultRepo = new(repo)
		}
	}
	return projects, nil
}

// ProjectEvals reads the current definition and series of each of slugs project holds, by slug,
// in one snapshot (evals_current, eval_series_current): what the delivery settings route answers
// the targets of (dispatch://INFRA-978, R3). A slug the project holds no version of is absent.
func (s *Store) ProjectEvals(ctx context.Context, project string, slugs []string) (map[string]Entry, error) {
	tx, err := s.BeginSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the read of %s's evals %v: %w", project, slugs, err)
	}
	defer tx.Rollback(ctx)
	return projectEntries(ctx, tx, project, slugs)
}

// projectEntries reads each of slugs project holds as Eval reads one, by slug, in tx: its newest
// definition and the newest version of each of its series, the primary series first, then by
// name.
func projectEntries(ctx context.Context, tx pgx.Tx, project string, slugs []string) (map[string]Entry, error) {
	ids := make([]string, len(slugs))
	for index, slug := range slugs {
		ids[index] = EvalID(project, slug)
	}
	rows, err := tx.Query(ctx, `select `+definitionColumns+` from evals_current where eval_id = any($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read %s's evals %v: %w", project, slugs, err)
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scanDefinition(row) })
	if err != nil {
		return nil, fmt.Errorf("read %s's evals %v: %w", project, slugs, err)
	}
	rows, err = tx.Query(ctx, `
		select `+seriesColumns+` from eval_series_current where eval_id = any($1) order by eval_id, is_primary desc, series
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the series of %s's evals %v: %w", project, slugs, err)
	}
	series, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Series, error) { return scanSeries(row) })
	if err != nil {
		return nil, fmt.Errorf("read the series of %s's evals %v: %w", project, slugs, err)
	}
	entries := make(map[string]Entry, len(definitions))
	slugOf := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		entries[definition.Slug] = Entry{Definition: definition, Series: []Series{}}
		slugOf[definition.EvalID] = definition.Slug
	}
	for _, one := range series {
		slug := slugOf[one.EvalID]
		entry := entries[slug]
		entry.Series = append(entry.Series, one)
		entries[slug] = entry
	}
	return entries, nil
}

// PipelineMinN is the fewest timed results a PR-checks job needs to be ranked among the Pipeline's
// slowest, from prCI, the pr-ci eval of the deploy repository's project
// (DeployRepoEvals): SlowestJobsMinN of its job series' min_n, so the Pipeline and that project's
// items sorted by median rank by one min_n, and measures.SlowestMinResults, the min_n the
// catalogue seeds, where it holds no job series.
func PipelineMinN(prCI Entry) int {
	index := slices.IndexFunc(prCI.Series, func(series Series) bool { return series.Series == SeriesJobs })
	if index < 0 {
		return measures.SlowestMinResults
	}
	return SlowestJobsMinN(prCI.Series[index].MinN)
}

// SlowestJobsMinN is the fewest timed results a PR-checks job needs over the days a read folds to
// be ranked among pr-ci's slowest jobs (measures.FoldSlowestJobs), from jobsMinN, the min_n of
// pr-ci's job series (SeriesJobs), which a person may set (R5): that count, or none (0) where it
// is null, the column's "no minimum".
func SlowestJobsMinN(jobsMinN *int) int {
	if jobsMinN == nil {
		return 0
	}
	return *jobsMinN
}
