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

// TestEveryCatalogueSourceNamesAnExportedFunctionOfTheServer (k5): the function each computed
// eval's source names is an exported function, with no receiver, that go/parser finds declared
// in the server source of the package directory the source names; and the eval's method names
// that function, so a reader of the section is sent to the code that is the eval's rule.
func TestEveryCatalogueSourceNamesAnExportedFunctionOfTheServer(t *testing.T) {
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
// failure mode with its guard, a kind, a unit and a series; every target judge names one of its
// series; and no eval has more than one primary series, which is listed and a headline one.
func TestEveryCatalogueEvalIsComputedAndRestatesNoRule(t *testing.T) {
	slugs := map[string]bool{}
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
		for _, judge := range definition.TargetJudges {
			series, _, _ := strings.Cut(judge, ".")
			if !names[series] {
				t.Errorf("%s: target judge %s names no series of the eval", slug, judge)
			}
		}
		if (definition.Target == nil) != (len(definition.TargetJudges) == 0) {
			t.Errorf("%s: target %v with judges %v, want judges exactly where a target is set", slug, definition.Target, definition.TargetJudges)
		}
	}
	t.Logf("%d computed evals, every one with no number rule", len(catalogue))
}

// TestTheSeededTargetsAreMainsDefaultTargets: each eval main judges (measures.DefaultTargets and
// KPIStatus's comparisons) is seeded with main's target, inclusive where main's test is "at least"
// or "exactly" and strict where it is "under", in the direction the test reads; every other eval's
// target is off.
func TestTheSeededTargetsAreMainsDefaultTargets(t *testing.T) {
	main := measures.DefaultTargets
	want := map[string]struct {
		target    float64
		inclusive bool
		direction string
	}{
		SlugDeployFrequency:   {main.DeploysPerDay, true, DirectionHigherBetter},
		SlugChangeFailRate:    {main.ChangeFailureRate, false, DirectionLowerBetter},
		SlugMergeToProduction: {main.MergeToProductionMinutes, false, DirectionLowerBetter},
		SlugPROpenToMerge:     {main.OpenedToMergeMedianMinutes, false, DirectionLowerBetter},
		SlugDeployRunSuccess:  {main.DeployRunSuccessRate, true, DirectionHigherBetter},
		SlugUnownedP0:         {float64(main.UnownedP0), true, DirectionLowerBetter},
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
// LegionSeries', every cut included, and each delivery and CI eval's are exactly those of its
// rows in the day builders' zero day (ZeroDeliveryDay, under settings naming both staging jobs),
// pr-ci's job series row beside them. change-fail-rate and failed-deploy-recovery, which get no
// day rows, are the only evals with no row of the zero day.
func TestTheCatalogueSeriesAreTheSeriesTheDayBuildersBuild(t *testing.T) {
	built := map[string][]string{}
	for _, slug := range legionDaySlugs {
		built[slug] = slices.Sorted(slices.Values(LegionSeries(slug)))
	}
	settings := delivery.DeliverySettings{StagingE2EJobName: new("e2e"), StagingApplyJobName: new("staging-apply")}
	zero := ZeroDeliveryDay(settings, "EVAL", time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	for _, row := range zero.Rows {
		slug := strings.TrimPrefix(row.EvalID, "EVAL/")
		built[slug] = append(built[slug], row.Series)
	}
	built[SlugUnownedP0] = []string{SeriesUnownedP0}
	built[SlugPRCI] = append(built[SlugPRCI], SeriesJobs)
	for slug := range built {
		slices.Sort(built[slug])
	}
	compared := 0
	for _, entry := range catalogue {
		slug := entry.definition.Slug
		want, has := built[slug]
		if slug == SlugChangeFailRate || slug == SlugFailedDeployRecovery {
			if has {
				t.Errorf("%s has day rows %v, and it gets none", slug, want)
			}
			continue
		}
		if got := seriesNames(entry); !slices.Equal(got, want) {
			t.Errorf("%s: the catalogue holds series %v, the builders build %v", slug, got, want)
		}
		compared += len(want)
		delete(built, slug)
	}
	if len(built) != 0 {
		t.Errorf("the builders build the series of %v, which the catalogue holds no eval of", slices.Sorted(maps.Keys(built)))
	}
	t.Logf("%d series of %d evals are the series the day builders build", compared, len(catalogue)-2)
}

// TestTheRequiredMethodsSayWhatTheSpecRequires: slot wait's method states flow analysis version
// 1's rule and value, this eval's rule and why the rule changed (R6, D3); deploy frequency's states
// the with_prs rule change and its measured difference; and idle time's says it is not phase
// time's idle segment and states D3's rule.
func TestTheRequiredMethodsSayWhatTheSpecRequires(t *testing.T) {
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
			"is not `working`. No claim counts as not working", "merger: `retro`, `merging` and `awaiting_merge`",
			"before that observation reads \"no data\"",
		},
	} {
		method := entryOf(t, slug).method()
		for _, phrase := range phrases {
			if !strings.Contains(method, phrase) {
				t.Errorf("%s's method does not say %q", slug, phrase)
			}
		}
	}
}

// TestASeriesRowNamesTheSeriesThatDefinesAStoredSeries: a job series of pr-ci is defined by
// SeriesJobs, and every other stored series, a series of another eval named like a job's
// included, by its own row.
func TestASeriesRowNamesTheSeriesThatDefinesAStoredSeries(t *testing.T) {
	for _, test := range []struct{ slug, series, want string }{
		{SlugPRCI, "job:lint", SeriesJobs},
		{SlugPRCI, measures.SeriesMinutes, measures.SeriesMinutes},
		{"my-eval", "job:lint", "job:lint"},
		{SlugLegionWorkTime, "hours:roots", "hours:roots"},
	} {
		if got := SeriesRow(test.slug, test.series); got != test.want {
			t.Errorf("SeriesRow(%s, %s) = %s, want %s", test.slug, test.series, got, test.want)
		}
	}
}
