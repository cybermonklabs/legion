package evals

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestTheTargetsDirectionEnumIsTheEvalsDirections (R2-Q9): a delivery answer states each target's
// direction as its eval holds it (measures.Target), and the generated contract types that field
// from its enum tag, which the page reads in place of a direction of its own. measures cannot
// import this package, so the tag restates DirectionHigherBetter and DirectionLowerBetter; this
// ties the two, and holds every delivery eval of the catalogue to one of them, so a direction
// renamed or added here fails here rather than on a page that judges a day the wrong way.
func TestTheTargetsDirectionEnumIsTheEvalsDirections(t *testing.T) {
	field, _ := reflect.TypeFor[measures.Target]().FieldByName("Direction")
	directions := []string{DirectionHigherBetter, DirectionLowerBetter}
	if tag := field.Tag.Get("enum"); tag != strings.Join(directions, ",") {
		t.Errorf("measures.Target.Direction's enum is %q, want the evals' directions %q", tag, strings.Join(directions, ","))
	}
	for _, slug := range DeliveryTargetSlugs {
		index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
		if index < 0 {
			t.Fatalf("the catalogue holds no %s", slug)
		}
		if direction := catalogue[index].definition.Direction; !slices.Contains(directions, direction) {
			t.Errorf("%s's direction is %q, which the enum of a delivery target does not hold", slug, direction)
		}
	}
}

// TestTheTargetsJudgesEnumIsEveryDeliveryJudge (R5): a delivery answer states the statistics each
// target judges (measures.Target.Judges), and the generated contract types each of them from the
// field's enum tag, which the page names every judge by (lib/target.ts's STATISTICS, a record over
// that union). measures cannot import this package, so the tag restates the judges a register
// write takes of the delivery evals (deliveryJudges); this ties the two, and holds what every
// delivery eval of the catalogue judges, its target_judges or its primary series' headline
// (JudgesOf), to the enum, so a judge added or renamed here fails here, and the contract made from
// the tag fails the page's compile until the page names it.
func TestTheTargetsJudgesEnumIsEveryDeliveryJudge(t *testing.T) {
	field, _ := reflect.TypeFor[measures.Target]().FieldByName("Judges")
	every := map[string]bool{}
	for _, judges := range deliveryJudges {
		for _, judge := range judges {
			every[judge] = true
		}
	}
	enum := slices.Sorted(maps.Keys(every))
	if tag := field.Tag.Get("enum"); tag != strings.Join(enum, ",") {
		t.Errorf("measures.Target.Judges's enum is %q, want every judge the delivery evals take %q", tag, strings.Join(enum, ","))
	}
	for _, slug := range DeliveryTargetSlugs {
		index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
		if index < 0 {
			t.Fatalf("the catalogue holds no %s", slug)
		}
		entry := Entry{Definition: catalogue[index].definition, Series: catalogue[index].series}
		headline := entry
		headline.Definition.TargetJudges = nil
		for _, judge := range slices.Concat(JudgesOf(entry), JudgesOf(headline)) {
			if !every[judge] {
				t.Errorf("%s judges %q, which the enum of a delivery target's judges does not hold", slug, judge)
			}
		}
	}
}

// TestEveryJudgeADeliveryTargetTakesIsStatedByTheDeliveryAnswers (R5): a person may name any
// statistic an eval's read states as a judge of its target, and the measures answer and the
// Pipeline judge the six delivery targets' figures by the same targets (VerdictOf). So for each
// statistic each series of those six evals states in the eval route (FoldStatistics), a register
// write naming it as a judge is either taken, and then the measures answer states a value of it
// over a window that measured everything (ResultStatistics), as the Pipeline does of its two lead
// times (LeadTimeStatistics), or refused EVAL_FIELD_INVALID naming target_judges[0], since no
// delivery answer could judge it: a judge the eval route judged while a delivery answer left it
// null would be a target written and then ignored there.
func TestEveryJudgeADeliveryTargetTakesIsStatedByTheDeliveryAnswers(t *testing.T) {
	spread := measures.Spread{MedianMinutes: new(30.0), P90Minutes: new(50.0), MaxMinutes: new(70.0)}
	result := measures.Result{
		DeployFrequency: measures.DeployFrequency{SuccessfulDeploys: 14, DeploysWithPRs: 7, PerDay: 2, WithPRsPerDay: 1},
		LeadTime:        measures.LeadTime{MergeToProduction: spread, OpenedToMerge: spread},
		ChangeFailureRate: measures.ChangeFailureRate{
			PerPR: measures.FlagCounts{Confirmed: 1, Pending: 1, Rejected: 1, Reverts: 1, Total: 9, Rate: new(0.1)},
			PerDeploy: measures.DeployFlagCounts{
				FlagCounts:         measures.FlagCounts{Confirmed: 1, Pending: 1, Rejected: 1, Reverts: 1, Total: 8, Rate: new(0.125)},
				ConfirmedOrPending: 2, UpperBoundRate: new(0.25),
			},
		},
		DeployRunSuccess: measures.DeployRunSuccess{Concluded: 10, ReachedProduction: 9, Cancelled: 1, Rate: new(0.9)},
	}
	result.LeadTime.MergeToProduction.N, result.LeadTime.OpenedToMerge.N = 5, 5
	answer := ResultStatistics(result, 2)
	pipeline := LeadTimeStatistics(measures.LeadTimeCell{N: 5, MedianMinutes: new(30.0), P90Minutes: new(50.0), MaxMinutes: new(70.0)})
	taken, refused := 0, 0
	for _, slug := range DeliveryTargetSlugs {
		index := slices.IndexFunc(catalogue, func(entry catalogueEntry) bool { return entry.definition.Slug == slug })
		if index < 0 {
			t.Fatalf("the catalogue holds no %s", slug)
		}
		entry := catalogue[index]
		read := FoldStatistics(entry.definition, entry.series, nil, Range{})
		for _, series := range entry.series {
			for _, stat := range slices.Sorted(maps.Keys(read[series.Series])) {
				judge := series.Series + "." + stat
				err := validateRegistration(Registration{Slug: slug, TargetJudges: Set([]string{judge})})
				if err != nil {
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.Code != CodeFieldInvalid || refusal.Field != "target_judges[0]" {
						t.Errorf("%s judged by %s: the register write answers %v, want it taken or refused %s naming target_judges[0]", slug, judge, err, CodeFieldInvalid)
					}
					refused++
					t.Logf("%s judged by %s: refused, %v", slug, judge, err)
					continue
				}
				taken++
				if answer[slug][series.Series][stat] == nil {
					t.Errorf("%s judged by %s: the register write takes it, and the measures answer states no value of it", slug, judge)
				}
				if (slug == SlugMergeToProduction || slug == SlugPROpenToMerge) && pipeline[stat] == nil {
					t.Errorf("%s judged by %s: the register write takes it, and the Pipeline states no value of it", slug, judge)
				}
			}
		}
	}
	if taken == 0 {
		t.Fatal("no judge of the six delivery targets was taken, so this test examined nothing")
	}
	t.Logf("%d judges of the six delivery targets taken and stated by the delivery answers, %d refused", taken, refused)
}
