package evals

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// dispatchSource is the directory every catalogue source's package path is under.
const dispatchSource = ".."

// sourceFunction is the package path and the function a computed source names: for
// `computed:delivery/measures.FoldLeadTime`, `delivery/measures` and `FoldLeadTime`.
func sourceFunction(t *testing.T, source string) (string, string) {
	t.Helper()
	named, ok := strings.CutPrefix(source, SourceComputedPrefix)
	dot := strings.LastIndex(named, ".")
	if !ok || dot <= 0 || dot == len(named)-1 {
		t.Fatalf("source %q is not %s<package path>.<function>", source, SourceComputedPrefix)
	}
	return named[:dot], named[dot+1:]
}

// TestEveryCatalogueSourceNamesAnExportedFunctionOfTheServer: the function each computed
// eval's source names is an exported function, with no receiver, that go/parser finds declared
// in the server source of the package directory the source names; and the eval's method names
// that function, so a reader of the section is sent to the code that is the eval's rule.
func TestEveryCatalogueSourceNamesAnExportedFunctionOfTheServer(t *testing.T) {
	t.Parallel()
	files := token.NewFileSet()
	declared := map[string]map[string]string{}
	resolved := 0
	for _, entry := range catalogue {
		packagePath, function := sourceFunction(t, entry.definition.Source)
		if declared[packagePath] == nil {
			dir := filepath.Join(dispatchSource, filepath.FromSlash(packagePath))
			sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
			if err != nil || len(sources) == 0 {
				t.Fatalf("%s: the source names package %s, and %s holds no Go file (%v)", entry.definition.Slug, packagePath, dir, err)
			}
			declared[packagePath] = map[string]string{}
			for _, source := range sources {
				if strings.HasSuffix(source, "_test.go") {
					continue
				}
				file, err := parser.ParseFile(files, source, nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatalf("parse %s: %v", source, err)
				}
				for _, declaration := range file.Decls {
					if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv == nil {
						declared[packagePath][fn.Name.Name] = file.Name.Name
					}
				}
			}
		}
		packageName, found := declared[packagePath][function]
		if !found || !ast.IsExported(function) {
			t.Errorf("%s: source %s names %s, which no non-test file of %s declares as an exported function", entry.definition.Slug, entry.definition.Source, function, packagePath)
			continue
		}
		if packageName != path.Base(packagePath) {
			t.Errorf("%s: %s is declared in package %s, and the source's path ends %s", entry.definition.Slug, function, packageName, path.Base(packagePath))
		}
		if named := packageName + "." + function; !strings.Contains(entry.method(), named) {
			t.Errorf("%s: the method never names %s, the function its source names", entry.definition.Slug, named)
		}
		resolved++
	}
	if resolved != len(catalogue) || resolved == 0 {
		t.Fatalf("resolved %d of the %d sources", resolved, len(catalogue))
	}
	t.Logf("%d computed sources, each an exported function found in the server source of its package and named by its method, over %d packages",
		resolved, len(declared))
}

// TestEveryCatalogueEvalIsComputedAndRestatesNoRule: every entry is computed, with an empty
// number_rule (R5: the source is the rule), a one-sentence question and decision, at least one
// failure mode with its guard, a kind, a unit and a series; every target judge names a series of
// the eval and a statistic its fold states; and no eval has more than one primary series, which is
// listed and a headline one.
func TestEveryCatalogueEvalIsComputedAndRestatesNoRule(t *testing.T) {
	t.Parallel()
	if len(catalogue) == 0 {
		t.Fatal("the catalogue holds no eval")
	}
	days := Range{From: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	slugs := map[string]bool{}
	judged := 0
	for _, entry := range catalogue {
		definition := entry.definition
		slug := definition.Slug
		if slugs[slug] {
			t.Errorf("%s is in the catalogue twice", slug)
		}
		slugs[slug] = true
		if !definition.Computed() || definition.NumberRule != "" {
			t.Errorf("%s: source %q and number rule %q, want a computed source and no number rule", slug, definition.Source, definition.NumberRule)
		}
		for name, sentence := range map[string]string{"question": definition.Question, "decision": definition.Decision} {
			if strings.Count(sentence, ". ")+strings.Count(sentence, "? ") != 0 || !strings.ContainsAny(sentence[len(sentence)-1:], ".?") {
				t.Errorf("%s: the %s %q is not one sentence", slug, name, sentence)
			}
		}
		if definition.Name == "" || definition.Unit == "" || definition.Kind == "" || len(entry.series) == 0 || len(definition.FailureModes) == 0 {
			t.Errorf("%s: name %q, unit %q, kind %q, %d series and %d failure modes, want each", slug, definition.Name, definition.Unit, definition.Kind, len(entry.series), len(definition.FailureModes))
		}
		for _, mode := range definition.FailureModes {
			if mode.Mode == "" || mode.Guard == "" {
				t.Errorf("%s: failure mode %+v has no mode or no guard", slug, mode)
			}
		}
		names := map[string]bool{}
		primaries := 0
		for _, series := range entry.series {
			if names[series.Series] {
				t.Errorf("%s: series %s is listed twice", slug, series.Series)
			}
			names[series.Series] = true
			if err := validSeriesName(series.Series); err != nil {
				t.Errorf("%s: series %q: %v", slug, series.Series, err)
			}
			if series.IsPrimary {
				primaries++
				if !series.Listed || series.Role != RoleHeadline {
					t.Errorf("%s: primary series %s is listed %v with role %s, want a listed headline series", slug, series.Series, series.Listed, series.Role)
				}
			}
		}
		if primaries > 1 {
			t.Errorf("%s has %d primary series, want at most one", slug, primaries)
		}
		folded := FoldStatistics(definition, entry.series, nil, days)
		for _, judge := range definition.TargetJudges {
			series, stat, found := strings.Cut(judge, ".")
			if !found || !names[series] {
				t.Errorf("%s: target judge %s names no series of the eval", slug, judge)
				continue
			}
			if _, stated := folded[series][stat]; !stated {
				t.Errorf("%s: target judge %s names statistic %q, which the fold of %s does not state: %v",
					slug, judge, stat, series, slices.Sorted(maps.Keys(folded[series])))
			}
			judged++
		}
		if (definition.Target == nil) != (len(definition.TargetJudges) == 0) {
			t.Errorf("%s: target %v with judges %v, want judges exactly where a target is set", slug, definition.Target, definition.TargetJudges)
		}
	}
	if judged == 0 {
		t.Error("no eval of the catalogue names a target judge, so no judge's series and statistic were checked")
	}
	t.Logf("%d computed evals, every one with no number rule; %d target judges, each a statistic its series' fold states", len(catalogue), judged)
}

// TestTheSeededTargetsAreMainsDefaultTargets: each eval main judges (measures.DefaultTargets, read
// as the prototype's dora.ts:280-295 compares) is seeded with main's target, inclusive where main's
// test is "at least" or "exactly" and strict where it is "under", in the direction the test reads;
// every other eval's target is off.
func TestTheSeededTargetsAreMainsDefaultTargets(t *testing.T) {
	t.Parallel()
	main := measures.DefaultTargets
	want := map[string]struct {
		target    float64
		inclusive bool
		direction string
	}{
		SlugDeployFrequency:   {main.DeploysPerDay.Value, true, DirectionHigherBetter},
		SlugChangeFailRate:    {main.ChangeFailureRate.Value, false, DirectionLowerBetter},
		SlugMergeToProduction: {main.MergeToProductionMinutes.Value, false, DirectionLowerBetter},
		SlugPROpenToMerge:     {main.OpenedToMergeMedianMinutes.Value, false, DirectionLowerBetter},
		SlugDeployRunSuccess:  {main.DeployRunSuccessRate.Value, true, DirectionHigherBetter},
		SlugUnownedP0:         {main.UnownedP0.Value, true, DirectionLowerBetter},
	}
	compared := 0
	for _, entry := range catalogue {
		definition := entry.definition
		expected, judged := want[definition.Slug]
		if !judged {
			if definition.Target != nil {
				t.Errorf("%s is seeded with target %v, and main sets it none", definition.Slug, *definition.Target)
			}
			continue
		}
		compared++
		if definition.Target == nil || *definition.Target != expected.target || definition.TargetInclusive != expected.inclusive || definition.Direction != expected.direction {
			t.Errorf("%s: target %v inclusive %v %s, want main's %v inclusive %v %s", definition.Slug, definition.Target, definition.TargetInclusive,
				definition.Direction, expected.target, expected.inclusive, expected.direction)
		}
	}
	if compared != len(want) {
		t.Fatalf("compared %d targets, want the %d main sets", compared, len(want))
	}
	t.Logf("%d seeded targets equal measures.DefaultTargets; every other eval's target is off", compared)
}

// entryOf is the catalogue's entry of slug.
func entryOf(t *testing.T, slug string) catalogueEntry {
	t.Helper()
	index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
	if index < 0 {
		t.Fatalf("the catalogue holds no %s", slug)
	}
	return catalogue[index]
}

// seriesNames is the names of an entry's series, sorted.
func seriesNames(entry catalogueEntry) []string {
	names := make([]string, len(entry.series))
	for i, series := range entry.series {
		names[i] = series.Series
	}
	slices.Sort(names)
	return names
}

// TestTheCatalogueSeriesAreTheSeriesTheDayBuildersBuild: each Legion eval's series are exactly
// those its rows in the day builders' zero day hold (ZeroLegionDay), every cut included, and by
// the rule of the splitting eval's root figures (metrics.RootFigures, which count roots alone) the
// children cut of each of those, which no day row is ever written for.
// Each delivery and CI eval's series are exactly those of its rows in the day builders' zero day
// (ZeroDeliveryDay, under settings naming both staging jobs), pr-ci's job series row beside them.
// Each code eval's series are exactly those of its rows of a measured day with no change, as
// fold.BuildDays builds them and WriteCodeDaysTx writes them (CodeDayRows), under two code groups:
// its status series and, for each group, the series its group forms name (catalogueEntry.groups).
// The three failure measures (change-fail-rate, failed-deploy-recovery and deployment-rework-rate),
// whose rows the record of deploys and failures builds rather than a delivery day, are the only
// evals with no row of the zero day.
func TestTheCatalogueSeriesAreTheSeriesTheDayBuildersBuild(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	built := map[string][]string{}
	for _, row := range ZeroLegionDay("EVAL", day, legionDaySlugs...).Rows {
		slug := strings.TrimPrefix(row.EvalID, "EVAL/")
		built[slug] = append(built[slug], row.Series)
	}
	unwritten := 0
	for _, figure := range metrics.RootFigures {
		children := metrics.CutSeries(figure, metrics.ScopeChildren)
		if slices.Contains(built[SlugLegionSplitting], children) {
			t.Errorf("the zero day writes %s, which holds no root and so no issue", children)
		}
		built[SlugLegionSplitting] = append(built[SlugLegionSplitting], children)
		unwritten++
	}
	settings := delivery.DeliverySettings{DeployRepo: "acme/deploy", StagingE2EJobName: new("e2e"), StagingApplyJobName: new("staging-apply")}
	// One repository kept as the deploy repository, whose zero rows are those of the evals that
	// read it; the evals that read none have theirs once.
	zero := ZeroDeliveryDay("EVAL", day, []delivery.DeliverySettings{settings})
	for _, row := range zero.Rows {
		slug := strings.TrimPrefix(row.EvalID, "EVAL/")
		built[slug] = append(built[slug], row.Series)
	}
	if len(built[SlugUnownedP0]) != 0 {
		t.Errorf("%s has zero-day rows %v from ZeroDeliveryDay, want none: its row is written on its own", SlugUnownedP0, built[SlugUnownedP0])
	}
	built[SlugUnownedP0] = append(built[SlugUnownedP0], SeriesUnownedP0)
	built[SlugPRCI] = append(built[SlugPRCI], SeriesJobs)
	survivalParts, reworkParts := fold.BuildDays(fold.BuildInput{Project: fold.Project{Key: "EVAL", DefaultRepo: codeRepo}, Groups: codeGroups,
		Days: []fold.Day{{Day: day, State: survival.DayMeasured}}})
	for _, row := range CodeDayRows("EVAL", "", survivalParts, reworkParts) {
		slug := strings.TrimPrefix(row.EvalID, "EVAL/")
		built[slug] = append(built[slug], row.Series)
	}
	for slug := range built {
		slices.Sort(built[slug])
	}
	compared := 0
	for _, entry := range catalogue {
		slug := entry.definition.Slug
		want, has := built[slug]
		if slug == SlugChangeFailRate || slug == SlugFailedDeployRecovery || slug == SlugDeploymentReworkRate {
			if has {
				t.Errorf("%s has day rows %v, and it gets none", slug, want)
			}
			continue
		}
		got := seriesNames(entry)
		for _, series := range entry.groupSeries(groupNames(codeGroups)) {
			got = append(got, series.Series)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the catalogue holds series %v, the builders build %v", slug, got, want)
		}
		compared += len(want)
		delete(built, slug)
	}
	if len(built) != 0 {
		t.Errorf("the builders build the series of %v, which the catalogue holds no eval of", slices.Sorted(maps.Keys(built)))
	}
	t.Logf("%d series of %d evals are the series the day builders build, %d of them the root figures' children cut no day row holds",
		compared, len(catalogue)-3, unwritten)
}

// TestEveryCatalogueSeriesIsStatedByItsHeadline: each listed series of each catalogue eval, and
// each listed series a code eval holds for a code group (catalogueEntry.groupSeries), states its
// headline statistic (HeadlineStatistic) among the statistics its fold answers, so a target
// written with no judges, which judges the primary series' headline, judges a figure the read
// states rather than one no read can give a value.
func TestEveryCatalogueSeriesIsStatedByItsHeadline(t *testing.T) {
	t.Parallel()
	days := Range{From: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}
	checked := 0
	for _, entry := range catalogue {
		held := append(slices.Clone(entry.series), entry.groupSeries(groupNames(codeGroups))...)
		folded := FoldStatistics(entry.definition, held, nil, days)
		for _, series := range held {
			if !series.Listed {
				continue
			}
			checked++
			headline := HeadlineStatistic(entry.definition, series)
			if _, stated := folded[series.Series][headline]; !stated {
				t.Errorf("%s's %s (%s) is stated by %q, which its fold does not state: %v", entry.definition.Slug, series.Series,
					series.Agg, headline, slices.Sorted(maps.Keys(folded[series.Series])))
			}
		}
	}
	if checked == 0 {
		t.Fatal("the catalogue holds no listed series, so the check compared nothing")
	}
	t.Logf("%d listed series of %d evals each state their headline statistic", checked, len(catalogue))
}

// TestTheRequiredMethodsSayWhatTheSpecRequires: slot wait's method states flow analysis version
// 1's rule and value, this eval's rule and why the rule changed (R6, D3); deploy frequency's states
// the with_prs rule change and its measured difference; idle time's says it is not phase time's
// idle segment and states D3's rule; and each code eval's links dispatch://INFRA-3117's method as
// its definition and its analysis as its measurement and says what the number is not, in the
// method's words (dispatch://INFRA-3206 R1 and R14), and the rework eval's says what it counts, how
// it differs from rework-share's title rule, and that the 8.2 per cent of the analysis is another
// reading (R6, R14).
func TestTheRequiredMethodsSayWhatTheSpecRequires(t *testing.T) {
	t.Parallel()
	for slug, phrases := range map[string][]string{
		SlugLegionSlotWait: {
			"dispatch://INFRA-979/artifact/flow-analysis-md", "status rule, at 39.3 percent of 14,458 hours",
			"phase is `admitted` and it holds no slot, with parked hours left out",
			"from an issue's first `todo` to its first worker event", "held a slot before any worker started",
			"counted no wait that came after a worker began", "Dispatch's statuses cannot see whether an issue holds a slot",
		},
		SlugDeployFrequency: {
			"shipped a pull request that merged inside the reader's window", "whenever that pull request merged",
			"A rule that depends on the reader's window cannot be a part of one day",
			"one deploy in 4 of 20 windows", "one deploy in 5 of 10 windows",
		},
		SlugLegionIdleTime: {
			"It is not the idle segment of phase time", "The issue holds its own slot",
			"is not `working`. No claim counts as not working", "implementer: `implementing` and `retro`",
			"merger: `merging` and `awaiting_merge`",
			"before that observation reads \"no data\"",
		},
		SlugCodeSurvival: codeMethodPhrases,
		SlugPostMergeRework: append(slices.Clone(codeMethodPhrases),
			"the lines of a group's changes that a later change of the same group for a different issue removed within 30 days",
			"Rework share counts pull requests whose title starts with fix, revert or hotfix",
			"The 8.2 per cent of dispatch://INFRA-3117/artifact/analysis-md is read at the tip with no age limit, over Legion's merges of any age, and is not this number",
		),
	} {
		method := entryOf(t, slug).method()
		for _, phrase := range phrases {
			if !strings.Contains(method, phrase) {
				t.Errorf("%s's method does not say %q", slug, phrase)
			}
		}
	}
}

// codeMethodPhrases is what each code eval's method says (dispatch://INFRA-3206 R1 and R14): the
// links of its definition and its measurement, and each point of "What the number is not" of
// dispatch://INFRA-3117/artifact/method-md, in its words.
var codeMethodPhrases = []string{
	"The definition of this number is dispatch://INFRA-3117/artifact/method-md",
	"Its measurement over cybermonklabs/legion is dispatch://INFRA-3117/artifact/analysis-md",
	"It is the replay's reading, not blame",
	"The replay reads 1.2 points above blame for Legion and 4.8 for the other agent at 30 days, and the gap is uneven",
	"It reads replacement and never whether the work was right",
	"An edited line is a dead line",
	"The diff is GitHub's, with three lines of context, no whitespace rule and no copy detection",
}

// TestTheCodeMethodsNeverStateTheMixedOrTheTipGap: neither code eval's method text, its question,
// decision, body and failure modes as the seed writes them, states the gap between the replay and
// blame at the tip (76.52 against 79.78 per cent, about 3 points) or the gap taken from the
// published figures (1.843 and 6.979 points), which dispatch://INFRA-3117/artifact/method-md says a
// page that carries the 30-day number never states (R14): only the same-age gap, 1.2 and 4.8.
func TestTheCodeMethodsNeverStateTheMixedOrTheTipGap(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, slug := range codeDaySlugs {
		method := entryOf(t, slug).method()
		if !strings.Contains(method, "1.2 points") || !strings.Contains(method, "4.8 for the other agent") {
			t.Errorf("%s's method does not state the same-age gap, so a check that it states no other proves little", slug)
		}
		for _, number := range []string{"1.8", "1.843", "7.0", "6.979", "76.52", "79.78"} {
			if strings.Contains(method, number) {
				t.Errorf("%s's method states %q, a gap taken at the tip or from the published figures", slug, number)
			}
		}
		checked++
	}
	if checked != 2 {
		t.Fatalf("checked %d code evals' methods, want both", checked)
	}
}
