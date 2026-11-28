package evals

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// TestAPullRequestTwoKindsOfRowTimeCountsOnceAtTheDeployThatFinishedFirst
// (dispatch://INFRA-978/comment/9cdcf9d2-532d-4431-9332-3ced4d4329f2): OMEGA reads the
// merge-to-production of acme/app's #1, #2 and #3 in the plain owner key's row of a day, timed to
// acme/app's GitHub deploys (79, 79 and 50 minutes), and in its own events repository's row of the
// same day, timed to the deploys posted to it (30, 120 and 50). shippedOnce keeps each pull
// request's sample of the deploy that finished first: #1's posted 30, #2's GitHub 79, and #3's
// GitHub 50, where both finished at once. The owner key's part, which OMEGA reads through the
// repository, loses #1's sample from its samples, n, sum and max, and one of its via_repository;
// the events repository's part loses #2's and #3's. The parts are the same whatever order each
// row's samples are in, and the rows keep their own samples. A row whose keys an earlier release
// left null, or whose keys are not one for each sample, is taken as it stands, and so is a row of
// another range: no pull request is dropped from it or for it.
func TestAPullRequestTwoKindsOfRowTimeCountsOnceAtTheDeployThatFinishedFirst(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	owner := OwnerKey("acme/app", nil, nil)
	// row is a merge-to-production row of key, stored under deployRepo, of samples keyed by keys.
	row := func(key, deployRepo string, via int, samples []float64, keys []string) WindowRow {
		part := measures.SamplesPart(day, measures.SeriesMinutes, samples, nil)
		part.ViaRepository = via
		return WindowRow{Range: RangeWindow, EvalID: EvalID("OMEGA", SlugMergeToProduction), Part: part, owner: key, deployRepo: deployRepo, sampleKeys: keys}
	}
	prs := []string{"acme/app#1", "acme/app#2", "acme/app#3"}
	github := func() WindowRow { return row(owner, "acme/app", 3, []float64{79, 79, 50}, slices.Clone(prs)) }
	posted := func() WindowRow {
		return row("OMEGA", EventsRepo("OMEGA"), 0, []float64{30, 120, 50}, slices.Clone(prs))
	}
	// shown is a part as a failure names it.
	shown := func(part measures.DayPart) string {
		return fmt.Sprintf("n %d, sum %s, max %s, samples %v, via %d", part.N, showValue(part.Sum), showValue(part.Max), part.Samples, part.ViaRepository)
	}
	expect := func(what string, rows []WindowRow, want []measures.DayPart) {
		t.Helper()
		held := make([][]float64, len(rows))
		for index, one := range rows {
			held[index] = slices.Clone(one.Part.Samples)
		}
		got := shippedOnce(rows)
		for index := range rows {
			if !slices.Equal(rows[index].Part.Samples, held[index]) {
				t.Errorf("%s: row %d's samples moved from %v to %v; want them kept", what, index, held[index], rows[index].Part.Samples)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d parts, want %d", what, len(got), len(want))
		}
		for index := range want {
			sorted := slices.Sorted(slices.Values(got[index].Samples))
			if got[index].N != want[index].N || !reflect.DeepEqual(got[index].Sum, want[index].Sum) || !reflect.DeepEqual(got[index].Max, want[index].Max) ||
				!slices.Equal(sorted, want[index].Samples) || got[index].ViaRepository != want[index].ViaRepository {
				t.Errorf("%s: part %d is %s, want %s", what, index, shown(got[index]), shown(want[index]))
			}
		}
	}
	// part is the part a row of samples (in ascending order) holds, with via.
	part := func(via int, samples ...float64) measures.DayPart {
		built := measures.SamplesPart(day, measures.SeriesMinutes, samples, nil)
		built.ViaRepository = via
		return built
	}

	expect("a day both rows time", []WindowRow{github(), posted()}, []measures.DayPart{part(2, 50, 79), part(0, 30)})

	reordered := github()
	reordered.Part.Samples, reordered.sampleKeys = []float64{50, 79, 79}, []string{"acme/app#3", "acme/app#2", "acme/app#1"}
	expect("the owner key's samples in another order", []WindowRow{posted(), reordered}, []measures.DayPart{part(0, 30), part(2, 50, 79)})

	throughIssue := github()
	throughIssue.Part.ViaRepository = 0
	expect("an owner key the project reads through its issues", []WindowRow{throughIssue, posted()}, []measures.DayPart{part(0, 50, 79), part(0, 30)})

	unkeyed := github()
	unkeyed.sampleKeys = nil
	expect("an owner key's row an earlier release wrote", []WindowRow{unkeyed, posted()}, []measures.DayPart{part(3, 50, 79, 79), part(0, 30, 50, 120)})

	short := posted()
	short.sampleKeys = prs[:2]
	expect("an events row whose keys are not one for each sample", []WindowRow{github(), short}, []measures.DayPart{part(3, 50, 79, 79), part(0, 30, 50, 120)})

	compared := posted()
	compared.Range = RangeCompare
	expect("an events row of the compare range", []WindowRow{github(), compared}, []measures.DayPart{part(3, 50, 79, 79), part(0, 30, 50, 120)})
}

// TestAPullRequestAnyTwoDeployRepositoriesTimeCountsOnceAtTheFirstDeploy
// (dispatch://INFRA-978/comment/66926e49-2f97-4c05-8eb0-d62d1f31fee8): every row of a lead time to
// production names its samples' pull requests, and a read of several deploy repositories keeps
// each pull request's smallest sample. OMEGA's own row of acme/app times #1 at 79 and #2 at 50,
// its events repository's row #1 at 30 and #2 at 50: the read keeps #1's post (30) and #2's GitHub
// deploy (50, the tie going to the GitHub deploy). Two GitHub repositories' rows that time #3 at
// once keep the sample of the one first by name. A read of one deploy repository names none
// (WindowRow.deployRepo ""), and takes its rows as they stand. Red where the fold keeps both
// samples of a pull request a project's own row and a post time, or of a tie of two GitHub
// repositories.
func TestAPullRequestAnyTwoDeployRepositoriesTimeCountsOnceAtTheFirstDeploy(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	row := func(deployRepo string, samples []float64, keys ...string) WindowRow {
		return WindowRow{Range: RangeWindow, EvalID: EvalID("OMEGA", SlugMergeToProduction), owner: "OMEGA", deployRepo: deployRepo,
			Part: measures.SamplesPart(day, measures.SeriesMinutes, samples, nil), sampleKeys: keys}
	}
	for _, test := range []struct {
		name string
		rows []WindowRow
		// kept is each row's samples the read keeps, in ascending order.
		kept [][]float64
	}{
		{"a project's own GitHub row and its events row",
			[]WindowRow{row("acme/app", []float64{79, 50}, "acme/app#1", "acme/app#2"), row(EventsRepo("OMEGA"), []float64{30, 50}, "acme/app#1", "acme/app#2")},
			[][]float64{{50}, {30}}},
		{"two GitHub repositories at once",
			[]WindowRow{row("acme/b", []float64{40}, "acme/app#3"), row("acme/a", []float64{40}, "acme/app#3")},
			[][]float64{{}, {40}}},
		{"a read of one deploy repository",
			[]WindowRow{row("", []float64{79}, "acme/app#1"), row("", []float64{30}, "acme/app#1")},
			[][]float64{{79}, {30}}},
	} {
		parts := shippedOnce(test.rows)
		for index, part := range parts {
			if kept := slices.Sorted(slices.Values(part.Samples)); !slices.Equal(kept, test.kept[index]) || part.N != len(test.kept[index]) {
				t.Errorf("%s: row %d keeps %v (n %d), want %v", test.name, index, kept, part.N, test.kept[index])
			}
		}
	}
}
