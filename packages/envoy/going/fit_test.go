package going_test

//ci:reads packages/envoy/going/bakeoff/cases.json committedCases os.ReadFile (bakeoff/cases.json), a directory of no package, for TestTheCaseSetCoversTheCatalogue and TestARandomModelScoresAtChance

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"strings"
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
	OutsideR13  bool              `json:"outside_r13"`
	Note        string            `json:"note"`
}

// r13Cases is how many cases of cases.json spec v61's R13 names: every case but those marked
// outside_r13, which were added after it and which no count of R13's or check c14's includes.
const r13Cases = 117

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

// The committed set is what R13 measures on: the 117 cases R13 names, 20 or more of them
// chart-worthy, every label an action of the catalogue or none, no request for a change labelled
// chart-worthy, and every catalogue action asked for both in words that name it and in words that
// do not. Each case outside R13's set says why it was added.
func TestTheCaseSetCoversTheCatalogue(t *testing.T) {
	catalogue, err := api.BuildGoingCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	measured, worthy := 0, 0
	named, unnamed := map[string]int{}, map[string]int{}
	for _, c := range committedCases(t) {
		_, held := catalogue.Lookup(c.Action)
		switch {
		case !held && c.Action != going.ActionNone:
			t.Errorf("%q is labelled %q, which is no catalogue action", c.Words, c.Action)
		case c.ChartWorthy && c.Action != going.ActionNone:
			t.Errorf("%q asks for a change and is labelled chart-worthy", c.Words)
		case c.OutsideR13 && strings.TrimSpace(c.Note) == "":
			t.Errorf("%q is outside R13's cases and has no note saying why it was added", c.Words)
		}
		if c.OutsideR13 {
			continue
		}
		measured++
		if c.ChartWorthy {
			worthy++
		}
		if c.NamesAction {
			named[c.Action]++
		} else {
			unnamed[c.Action]++
		}
	}
	if measured != r13Cases || worthy < going.LeastChartCases {
		t.Errorf("R13's set holds %d cases, %d chart-worthy; spec v61's R13 names %d, and R13 needs %d chart-worthy", measured, worthy, r13Cases, going.LeastChartCases)
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

// c14's control: a model choosing at random among the catalogue's actions and none scores at
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
