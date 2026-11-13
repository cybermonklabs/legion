package going_test

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/api"
)

// right and wrong are answered action outcomes at a confidence; unanswered is a call that gave
// none.
func right(c float64) going.ActionOutcome {
	return going.ActionOutcome{Answered: true, Right: true, Confidence: c}
}
func wrong(c float64) going.ActionOutcome { return going.ActionOutcome{Answered: true, Confidence: c} }

var unanswered = going.ActionOutcome{}

// repeat is n copies of one outcome.
func repeat[T any](n int, o T) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = o
	}
	return out
}

func TestTheActionThresholdIsTheLowestValueWithNoWrongChoiceAtOrAboveIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcomes  []going.ActionOutcome
		exists    bool
		threshold float64
		share     float64
		holds     bool
	}{
		{
			name:     "a right choice tied with a wrong one has a wrong choice at its value",
			outcomes: []going.ActionOutcome{wrong(0.8), right(0.8), right(0.85), right(0.9), right(0.95)},
			exists:   true, threshold: 0.85, share: 0.6, holds: true,
		},
		{
			name:     "exactly 60% at or above holds",
			outcomes: slices.Concat(repeat(12, right(0.9)), []going.ActionOutcome{wrong(0.6)}, repeat(7, right(0.5))),
			exists:   true, threshold: 0.9, share: 0.6, holds: true,
		},
		{
			name:     "under 60% at or above does not hold",
			outcomes: slices.Concat(repeat(11, right(0.9)), []going.ActionOutcome{wrong(0.6)}, repeat(8, right(0.5))),
			exists:   true, threshold: 0.9, share: 0.55, holds: false,
		},
		{
			name:     "with no wrong choice the lowest confidence admits every case",
			outcomes: []going.ActionOutcome{right(0.3), right(0.5), right(0.9)},
			exists:   true, threshold: 0.3, share: 1, holds: true,
		},
		{
			name:     "a wrong choice above every right one admits no value",
			outcomes: []going.ActionOutcome{right(0.5), right(0.9), wrong(0.99)},
		},
		{
			name:     "an unanswered case counts against the share and is no wrong choice",
			outcomes: slices.Concat(repeat(3, right(0.9)), repeat(2, unanswered)),
			exists:   true, threshold: 0.9, share: 0.6, holds: true,
		},
		{
			name:     "too many unanswered cases leave the fit under 60%",
			outcomes: slices.Concat(repeat(3, right(0.9)), repeat(3, unanswered)),
			exists:   true, threshold: 0.9, share: 0.5, holds: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fit := going.FitActionThreshold(tc.outcomes)
			if fit.Exists != tc.exists || fit.Holds != tc.holds {
				t.Fatalf("fit %+v, want exists %t and holds %t", fit, tc.exists, tc.holds)
			}
			if tc.exists && (fit.Threshold != tc.threshold || math.Abs(fit.Share-tc.share) > 1e-9) {
				t.Errorf("fit %+v, want the threshold %v leaving %v at or above it", fit, tc.threshold, tc.share)
			}
		})
	}
}

// chart and prose are display outcomes: a chart-worthy case or not, answered chart or prose at a
// confidence.
func chart(worthy bool, c float64) going.DisplayOutcome {
	return going.DisplayOutcome{ChartWorthy: worthy, Chart: true, Confidence: c}
}
func prose(worthy bool, c float64) going.DisplayOutcome {
	return going.DisplayOutcome{ChartWorthy: worthy, Confidence: c}
}

func TestTheChartThresholdIsTheLowestChartWorthyChartAboveEveryWrongChart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outcomes  []going.DisplayOutcome
		exists    bool
		threshold float64
		share     float64
		holds     bool
	}{
		{
			name:     "a chart-worthy chart tied with a wrong chart is no candidate",
			outcomes: []going.DisplayOutcome{chart(false, 0.7), chart(true, 0.7), chart(true, 0.8), chart(true, 0.9), chart(true, 0.95)},
			exists:   true, threshold: 0.8, share: 0.75, holds: true,
		},
		{
			name:     "chart-worthy cases answered prose count against the 60%: six of ten holds",
			outcomes: slices.Concat(repeat(6, chart(true, 0.9)), repeat(4, prose(true, 0.99)), []going.DisplayOutcome{chart(false, 0.5)}),
			exists:   true, threshold: 0.9, share: 0.6, holds: true,
		},
		{
			name:     "chart-worthy cases answered prose count against the 60%: five of ten does not hold",
			outcomes: slices.Concat(repeat(5, chart(true, 0.9)), repeat(5, prose(true, 0.99)), []going.DisplayOutcome{chart(false, 0.5)}),
			exists:   true, threshold: 0.9, share: 0.5, holds: false,
		},
		{
			name:     "a prose answer, chart-worthy or not, is no wrong display",
			outcomes: []going.DisplayOutcome{prose(true, 0.99), prose(false, 0.99), chart(true, 0.6), chart(true, 0.7), chart(false, 0.65)},
			exists:   true, threshold: 0.7, share: 1.0 / 3, holds: false,
		},
		{
			name:     "with no wrong chart the lowest chart-worthy chart admits them all",
			outcomes: []going.DisplayOutcome{chart(true, 0.4), chart(true, 0.8), prose(false, 0.9)},
			exists:   true, threshold: 0.4, share: 1, holds: true,
		},
		{
			name:     "a wrong chart above every chart-worthy one admits no value",
			outcomes: []going.DisplayOutcome{chart(true, 0.6), chart(true, 0.8), chart(false, 0.85)},
		},
		{
			name:     "no chart-worthy case answered chart admits no value",
			outcomes: []going.DisplayOutcome{prose(true, 0.9), prose(false, 0.9)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fit := going.FitChartThreshold(tc.outcomes)
			if fit.Exists != tc.exists || fit.Holds != tc.holds {
				t.Fatalf("fit %+v, want exists %t and holds %t", fit, tc.exists, tc.holds)
			}
			if tc.exists && (fit.Threshold != tc.threshold || math.Abs(fit.Share-tc.share) > 1e-9) {
				t.Errorf("fit %+v, want the threshold %v leaving %v of the chart-worthy cases at or above it", fit, tc.threshold, tc.share)
			}
		})
	}
}

// bakeoffCase is one committed case, read as the bake-off reads it.
type bakeoffCase struct {
	Words       string            `json:"words"`
	Context     going.PageContext `json:"context"`
	Action      string            `json:"action"`
	NamesAction bool              `json:"names_action"`
	ChartWorthy bool              `json:"chart_worthy"`
}

func committedCases(t *testing.T) []bakeoffCase {
	t.Helper()
	raw, err := os.ReadFile("bakeoff/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []bakeoffCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("bakeoff/cases.json: %v", err)
	}
	return cases
}

// The committed set is what R7 measures on: at least 80 cases and 20 chart-worthy ones, every
// label an action of the catalogue or none, no request for a change labelled chart-worthy, and
// every catalogue action asked for both in words that name it and in words that do not.
func TestTheCaseSetCoversTheCatalogue(t *testing.T) {
	catalogue, err := api.BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	cases := committedCases(t)
	worthy := 0
	named, unnamed := map[string]int{}, map[string]int{}
	for _, c := range cases {
		_, held := catalogue.Lookup(c.Action)
		switch {
		case !held && c.Action != going.ActionNone:
			t.Errorf("%q is labelled %q, which is no catalogue action", c.Words, c.Action)
		case c.ChartWorthy && c.Action != going.ActionNone:
			t.Errorf("%q asks for a change and is labelled chart-worthy", c.Words)
		}
		if c.ChartWorthy {
			worthy++
		}
		if c.NamesAction {
			named[c.Action]++
		} else {
			unnamed[c.Action]++
		}
	}
	if len(cases) < going.LeastCases || worthy < going.LeastChartCases {
		t.Errorf("%d cases, %d chart-worthy; R7 needs at least %d and %d", len(cases), worthy, going.LeastCases, going.LeastChartCases)
	}
	for _, action := range catalogue.Actions() {
		if named[action.Action] < 2 || unnamed[action.Action] < 1 {
			t.Errorf("%s is asked for in %d case(s) that name it and %d that do not; want at least 2 and 1",
				action.Action, named[action.Action], unnamed[action.Action])
		}
	}
	if unnamed[going.ActionNone] < 10 {
		t.Errorf("%d plain questions; want at least 10", unnamed[going.ActionNone])
	}
}

// c12's first control: a model answering chart on every case either admits no display value or
// one that leaves under 60% of the chart-worthy cases at or above it.
func TestAModelAnsweringChartOnEveryCaseAdmitsNoDisplayFit(t *testing.T) {
	cases := committedCases(t)
	sure := make([]going.DisplayOutcome, 0, len(cases))
	for _, c := range cases {
		sure = append(sure, chart(c.ChartWorthy, 0.97))
	}
	if fit := going.FitChartThreshold(sure); fit.Exists {
		t.Errorf("always chart at one confidence: fit %+v, want no value", fit)
	}
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 1588))
		outcomes := make([]going.DisplayOutcome, 0, len(cases))
		for _, c := range cases {
			outcomes = append(outcomes, chart(c.ChartWorthy, r.Float64()))
		}
		if fit := going.FitChartThreshold(outcomes); fit.Holds {
			t.Fatalf("always chart at random confidences (seed %d): fit %+v holds", seed, fit)
		}
	}
}

// c12's second control: a model choosing at random among the catalogue's actions and none scores
// at chance, one in the number of choices, and admits no action fit.
func TestARandomModelScoresAtChance(t *testing.T) {
	catalogue, err := api.BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	choices := []string{going.ActionNone}
	for _, action := range catalogue.Actions() {
		choices = append(choices, action.Action)
	}
	cases := committedCases(t)
	const trials = 400
	total := 0.0
	for seed := range uint64(trials) {
		r := rand.New(rand.NewPCG(seed, 7))
		outcomes := make([]going.ActionOutcome, 0, len(cases))
		for _, c := range cases {
			choice := choices[r.IntN(len(choices))]
			outcomes = append(outcomes, going.ActionOutcome{Answered: true, Right: choice == c.Action, Confidence: r.Float64()})
		}
		total += going.Accuracy(outcomes)
		if fit := going.FitActionThreshold(outcomes); fit.Holds {
			t.Fatalf("a random model (seed %d) admits the action fit %+v", seed, fit)
		}
	}
	chance := 1 / float64(len(choices))
	if mean := total / trials; math.Abs(mean-chance) > 0.01 {
		t.Errorf("a random model scores %.4f on average, want chance, %.4f", mean, chance)
	}
}
