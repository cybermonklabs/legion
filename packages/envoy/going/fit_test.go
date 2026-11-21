package going_test

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/api"
)

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

// c12's control: a model choosing at random among the catalogue's actions and none scores at
// chance, one in the number of choices.
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
			outcomes = append(outcomes, going.ActionOutcome{Answered: true, Right: choice == c.Action})
		}
		total += going.Accuracy(outcomes)
	}
	chance := 1 / float64(len(choices))
	if mean := total / trials; math.Abs(mean-chance) > 0.01 {
		t.Errorf("a random model scores %.4f on average, want chance, %.4f", mean, chance)
	}
}
