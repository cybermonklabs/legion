package going_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
)

// TestTheRecordReadsBack reads each entry the bake-off wrote back through DecisionRecord, by its
// model's name, as the file holds it, and holds the chosen entry to R13: one model is chosen, it
// was measured on at least 80 cases and 20 chart-worthy ones, it "chooses the right action on at
// least 95% of the 117 cases of packages/envoy/going/bakeoff/cases.json, and 95% of its answers
// end in under 500 ms" (R13's floors, JevRightFloor and JevP95Ceiling: the right action on 95% of
// its answers to those cases, all together, and a p95 under 500 ms), and a model a tie-break
// named names who broke the tie and why.
func TestTheRecordReadsBack(t *testing.T) {
	raw, err := os.ReadFile("decision_models.json")
	if err != nil {
		t.Fatal(err)
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decision_models.json: %v", err)
	}
	if len(fields) == 0 {
		t.Fatal("the record holds no measured model")
	}
	chosen := 0
	for _, entry := range committedRecords(t) {
		record, held := going.DecisionRecord(entry.Model)
		if !held {
			t.Errorf("DecisionRecord(%q) holds no entry, though the file has one", entry.Model)
			continue
		}
		if !reflect.DeepEqual(record, entry) {
			t.Errorf("%s reads back as %+v, want the file's %+v", entry.Model, record, entry)
		}
		if !record.Chosen {
			continue
		}
		chosen++
		switch {
		case record.Cases < going.LeastCases || record.ChartWorthyCases < going.LeastChartCases:
			t.Errorf("the chosen %s was measured on %d cases, %d chart-worthy; R13 needs %d and %d",
				record.Model, record.Cases, record.ChartWorthyCases, going.LeastCases, going.LeastChartCases)
		case record.RightShare() < going.JevRightFloor || record.P95MS >= float64(going.JevP95Ceiling.Milliseconds()):
			t.Errorf("the chosen %s chooses the right action on %d of its %d answers at p95 %.0f ms; R13 needs %v of them and under %v",
				record.Model, record.Right(), record.Cases*record.Repeats, record.P95MS, going.JevRightFloor, going.JevP95Ceiling)
		case record.ChosenBy == going.ChosenByTieBreak && (strings.TrimSpace(record.Chooser) == "" || strings.TrimSpace(record.ChosenReason) == ""):
			t.Errorf("the chosen %s was named by a tie-break that names chooser %q and reason %q, want both", record.Model, record.Chooser, record.ChosenReason)
		}
	}
	if chosen != 1 {
		t.Errorf("%d entries are chosen, want one", chosen)
	}
	if _, held := going.DecisionRecord("a-model-nobody-measured"); held {
		t.Error("an unmeasured model reads back an entry")
	}
}

// Each entry's repeats agree with its pooled numbers, each paired test is the mirror of the
// other model's, and the chosen entry is the one R13's rule names (ruleFault).
func TestTheRecordHoldsItsRepeatsAndTheRuleThatChose(t *testing.T) {
	records := committedRecords(t)
	entries := make(map[string]*going.Record, len(records))
	for i := range records {
		entries[records[i].Model] = &records[i]
	}
	for i := range records {
		record := &records[i]
		if record.Repeats < 1 || len(record.RepeatRights) != record.Repeats {
			t.Errorf("%s holds %d repeats and %d repeat counts, want at least one and one count for each", record.Model, record.Repeats, len(record.RepeatRights))
			continue
		}
		named, other := 0, 0
		totals := make([]int, 0, record.Repeats)
		for _, counted := range record.RepeatRights {
			if counted.Total != counted.Named+counted.Other || counted.Named > record.NamedCases || counted.Other > record.OtherCases {
				t.Errorf("%s holds the repeat count %+v of %d named and %d other cases", record.Model, counted, record.NamedCases, record.OtherCases)
			}
			named, other = named+counted.Named, other+counted.Other
			totals = append(totals, counted.Total)
		}
		if !sameShare(record.NamedAccuracy, named, record.NamedCases*record.Repeats) || !sameShare(record.OtherAccuracy, other, record.OtherCases*record.Repeats) {
			t.Errorf("%s pools %d named and %d other right choices over %d repeats, but holds the accuracies %v and %v",
				record.Model, named, other, record.Repeats, record.NamedAccuracy, record.OtherAccuracy)
		}
		if want := going.SpreadOf(totals); record.TotalSpread != want {
			t.Errorf("%s holds the spread %+v, want %+v from its repeats", record.Model, record.TotalSpread, want)
		}
		for _, c := range record.InconsistentCases {
			if c.Of != record.Repeats || c.Right < 1 || c.Right >= c.Of {
				t.Errorf("%s holds %+v as a case right in some repeats and wrong in others", record.Model, c)
			}
		}
		if len(record.PairedTests) != len(records)-1 {
			t.Errorf("%s holds %d paired tests, want one against each of the %d other models", record.Model, len(record.PairedTests), len(records)-1)
		}
		for _, test := range record.PairedTests {
			against, known := entries[test.Against]
			if !known {
				t.Errorf("%s holds a test against %q, which the record does not hold", record.Model, test.Against)
				continue
			}
			mirror, held := against.PairedAgainst(record.Model)
			switch {
			case !held:
				t.Errorf("%s holds a test against %q, which holds none against it", record.Model, test.Against)
			case test.Better != mirror.Worse || test.Worse != mirror.Better || test.PValue != mirror.PValue || test.Real != mirror.Real:
				t.Errorf("%s against %s reads %+v, but the mirror reads %+v", record.Model, test.Against, test, mirror)
			case test.Better-test.Worse != record.Right()-against.Right():
				t.Errorf("%s against %s: %d pairs against %d, but the right choices differ by %d", record.Model, test.Against, test.Better, test.Worse, record.Right()-against.Right())
			}
		}
	}
	if err := ruleFault(records); err != nil {
		t.Error(err)
	}
	rule := going.Choose(records)
	t.Logf("R13's rule on the committed record comes to %q, naming %q by %q; level: %v", rule.Outcome, rule.Model, rule.By, rule.Level)
	for i := range records {
		t.Logf("%s: %d right choices of %d, meets the floors %t", records[i].Model, records[i].Right(), records[i].Cases*records[i].Repeats, records[i].MeetsFloors())
	}
}

// The committed record is byte for byte what the bake-off writes for its own entries
// (going.EncodeRecords): a run's record goes in as the tool wrote it, with no formatter or hand
// edit between them, so every number in it is the run's own.
func TestTheRecordIsWhatTheBakeOffWrites(t *testing.T) {
	raw, err := os.ReadFile("decision_models.json")
	if err != nil {
		t.Fatal(err)
	}
	var records []going.Record
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("decision_models.json: %v", err)
	}
	written, err := going.EncodeRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, written) {
		return
	}
	committed, wrote := strings.Split(string(raw), "\n"), strings.Split(string(written), "\n")
	for i := range max(len(committed), len(wrote)) {
		if lineAt(committed, i) != lineAt(wrote, i) {
			t.Fatalf("line %d of the committed record is %q, but the bake-off writes %q", i+1, lineAt(committed, i), lineAt(wrote, i))
		}
	}
}

// The rule ranks the candidates by their right choices pooled over every repeat, and nothing
// else separates them: not a lower p95, and not the order the models were measured in, so a
// level count names no model. The choice keeps the candidates in that order with the count each
// ranked by, and what the rule came to.
func TestTheRuleRanksByRightChoices(t *testing.T) {
	lowP95 := func(r going.Record) going.Record { r.P95MS = 90; return r }
	for _, tc := range []struct {
		name      string
		records   []going.Record
		outcome   going.ChoiceOutcome
		model, by string
		level     []string
		ranked    []going.Candidate
	}{
		{
			name:    "the most right choices over every repeat, though another led one repeat with a lower p95",
			records: []going.Record{lowP95(candidate("model-b", 114, 112)), candidate("model-a", 112, 115)},
			outcome: going.Named, model: "model-a", by: going.ChosenByRightChoices, level: []string{"model-a"},
			ranked: []going.Candidate{{Model: "model-a", Right: 227}, {Model: "model-b", Right: 226}},
		},
		{
			name:    "level: a lower p95 names none",
			records: []going.Record{lowP95(candidate("model-a", 114)), candidate("model-b", 114)},
			outcome: going.FullTie, level: []string{"model-a", "model-b"},
			ranked: []going.Candidate{{Model: "model-a", Right: 114}, {Model: "model-b", Right: 114}},
		},
		{
			name:    "three, two level at the most: none, and the third ranks last",
			records: []going.Record{candidate("model-a", 113), lowP95(candidate("model-b", 114)), candidate("model-c", 114)},
			outcome: going.FullTie, level: []string{"model-b", "model-c"},
			ranked: []going.Candidate{{Model: "model-b", Right: 114}, {Model: "model-c", Right: 114}, {Model: "model-a", Right: 113}},
		},
		{
			name:    "the one candidate",
			records: []going.Record{candidate("model-a", 115)},
			outcome: going.Named, model: "model-a", by: going.ChosenByRightChoices, level: []string{"model-a"},
			ranked: []going.Candidate{{Model: "model-a", Right: 115}},
		},
		{
			name:    "no record",
			records: nil,
			outcome: going.NoCandidate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := going.Choose(tc.records)
			if got.Outcome != tc.outcome || got.Model != tc.model || got.By != tc.by || got.Reason != "" || !slices.Equal(got.Level, tc.level) || !slices.Equal(got.Ranked, tc.ranked) {
				t.Errorf("Choose comes to %q naming %q by %q, level %v, ranked %v; want %q naming %q by %q, level %v, ranked %v",
					got.Outcome, got.Model, got.By, got.Level, got.Ranked, tc.outcome, tc.model, tc.by, tc.level, tc.ranked)
			}
		})
	}
}

// Only a model that meets R13's two floors is a candidate: one that misses either of them is never
// ranked or named, however many right choices it made. The floor of right choices is of R13's
// cases all together: a model under 90% on the cases that name their action is a candidate where
// it is right on 95% of them all.
func TestOnlyAModelThatMeetsTheFloorsIsACandidate(t *testing.T) {
	for name, miss := range map[string]func(*going.Record){
		// Asked twice, right on 222 of its 234 answers: more right choices than model-weak's 112
		// of 117, and under 95% of its answers.
		"right under 95%": func(r *going.Record) {
			r.Repeats, r.RepeatRights = 2, append(r.RepeatRights, going.RepeatRight{Other: 105, Total: 105})
		},
		"p95 at the ceiling": func(r *going.Record) { r.P95MS = float64(going.JevP95Ceiling.Milliseconds()) },
	} {
		t.Run(name, func(t *testing.T) {
			strong := candidate("model-strong", 117)
			miss(&strong)
			got := going.Choose([]going.Record{strong, candidate("model-weak", 112)})
			if got.Outcome != going.Named || got.Model != "model-weak" || got.By != going.ChosenByRightChoices || !slices.Equal(got.Ranked, []going.Candidate{{Model: "model-weak", Right: 112}}) {
				t.Errorf("Choose comes to %q naming %q by %q, ranked %v; want model-weak, the one model meeting the floors and the one ranked", got.Outcome, got.Model, got.By, got.Ranked)
			}
			if got := going.Choose([]going.Record{strong}); got.Outcome != going.NoCandidate || got.Model != "" || len(got.Level) != 0 || len(got.Ranked) != 0 {
				t.Errorf("with no model meeting the floors, Choose comes to %q naming %q, level %v, ranked %v", got.Outcome, got.Model, got.Level, got.Ranked)
			}
		})
	}
	// 40 of the 45 cases that name their action and all 72 of the rest: 112 of 117.
	split := going.Record{Model: "model-split", Cases: 117, NamedCases: 45, OtherCases: 72, NamedAccuracy: 40.0 / 45, OtherAccuracy: 1, P95MS: 150,
		Repeats: 1, RepeatRights: []going.RepeatRight{{Named: 40, Other: 72, Total: 112}}}
	if !split.MeetsFloors() {
		t.Errorf("a model right on %d of %d cases, %.3f of those that name their action, is no candidate; want one, R13's floor being of its cases all together",
			split.Right(), split.Cases, split.NamedAccuracy)
	}
}

// The architect names a model only where the rule names none: one of the models level on right
// choices, naming who broke the tie, with a reason.
func TestATieBreakIsTakenOnlyOnAFullTie(t *testing.T) {
	tie := going.Choose([]going.Record{candidate("model-a", 113), candidate("model-b", 113)})
	if tie.Outcome != going.FullTie {
		t.Fatalf("a run level on right choices comes to %q, want a full tie", tie.Outcome)
	}
	broken, err := tie.Break("model-b", "architect of DEMO-1", "the cheaper of two level models")
	if err != nil || broken.Outcome != going.Named || broken.Model != "model-b" || broken.By != going.ChosenByTieBreak ||
		broken.Chooser != "architect of DEMO-1" || broken.Reason != "the cheaper of two level models" {
		t.Fatalf("a full tie broken for model-b reads %+v, %v", broken, err)
	}
	for name, refused := range map[string]func() (going.Choice, error){
		"a run the right choices decide": func() (going.Choice, error) {
			return going.Choose([]going.Record{candidate("model-a", 114), candidate("model-b", 113)}).Break("model-b", "architect of DEMO-1", "a reason")
		},
		"a model outside the tie": func() (going.Choice, error) { return tie.Break("model-c", "architect of DEMO-1", "a reason") },
		"an empty chooser":        func() (going.Choice, error) { return tie.Break("model-a", "", "a reason") },
		"a chooser of spaces":     func() (going.Choice, error) { return tie.Break("model-a", "  ", "a reason") },
		"an empty reason":         func() (going.Choice, error) { return tie.Break("model-a", "architect of DEMO-1", "") },
		"a reason of spaces":      func() (going.Choice, error) { return tie.Break("model-a", "architect of DEMO-1", "  ") },
		"a tie already broken":    func() (going.Choice, error) { return broken.Break("model-a", "architect of DEMO-1", "a reason") },
		"no model meeting the floors": func() (going.Choice, error) {
			return going.Choose(nil).Break("model-a", "architect of DEMO-1", "a reason")
		},
	} {
		if got, err := refused(); err == nil {
			t.Errorf("%s: the tie-break is taken, naming %q by %q", name, got.Model, got.By)
		}
	}
}

// Break refuses each outcome a tie-break cannot break with that outcome's own reason.
func TestABreakRefusalSaysWhatTheRuleCameTo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choice  going.Choice
		says    string
		notSays string
	}{
		{"a run the rule decides", going.Choose([]going.Record{candidate("model-a", 114), candidate("model-b", 113)}), "R13's rule names model-a", "floors, so"},
		{"no model meeting the floors", going.Choose(nil), "no model meets R13's floors", "R13's rule names"},
		{"an outcome Break does not know", going.Choice{Outcome: "made_up"}, `"made_up"`, "floors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.choice.Break("model-b", "architect of DEMO-1", "a reason")
			if err == nil || !strings.Contains(err.Error(), tc.says) || strings.Contains(err.Error(), tc.notSays) {
				t.Errorf("Break on %q refuses with %v, want it to say %q and not %q", tc.choice.Outcome, err, tc.says, tc.notSays)
			}
		})
	}
}

// A record whose chosen model is not the one R13's rule names fails the check the committed
// record is held to, however it says the model was chosen; a tie-break passes only where the
// record itself shows the full tie and keeps who broke it and why.
func TestARecordTheRuleDidNotNameFails(t *testing.T) {
	chose := func(records []going.Record, model, by, chooser, reason string) []going.Record {
		for i := range records {
			if records[i].Model == model {
				records[i].Chosen, records[i].ChosenBy, records[i].Chooser, records[i].ChosenReason = true, by, chooser, reason
			}
		}
		return records
	}
	decided := func() []going.Record {
		return []going.Record{candidate("model-a", 115), candidate("model-b", 113)}
	}
	level := func() []going.Record {
		return []going.Record{candidate("model-a", 113), candidate("model-b", 113)}
	}
	const architect = "architect of DEMO-1"
	for _, tc := range []struct {
		name    string
		records []going.Record
		passes  bool
	}{
		{"the rule's model by the right choices", chose(decided(), "model-a", going.ChosenByRightChoices, "", ""), true},
		{"a full tie the architect broke, with a reason", chose(level(), "model-b", going.ChosenByTieBreak, architect, "the cheaper of two level models"), true},
		{"a hand-chosen model where the rule names another", chose(decided(), "model-b", going.ChosenByTieBreak, architect, "it reads better"), false},
		{"another model said to be the rule's", chose(decided(), "model-b", going.ChosenByRightChoices, "", ""), false},
		{"the rule's model said to be a tie-break's", chose(decided(), "model-a", going.ChosenByTieBreak, architect, "it reads better"), false},
		{"the rule's model by a paired test's verdict", chose(decided(), "model-a", "noise", "", ""), false},
		{"the rule's model with a reason no tie-break gave", chose(decided(), "model-a", going.ChosenByRightChoices, "", "a reason"), false},
		{"the rule's model with a chooser", chose(decided(), "model-a", going.ChosenByRightChoices, architect, ""), false},
		{"a full tie broken with no reason", chose(level(), "model-b", going.ChosenByTieBreak, architect, " "), false},
		{"a full tie broken by nobody named", chose(level(), "model-b", going.ChosenByTieBreak, "", "the cheaper of two level models"), false},
		{"a full tie said to be the rule's", chose(level(), "model-b", going.ChosenByRightChoices, "", ""), false},
		{"no model chosen", decided(), false},
		{"two models chosen", chose(chose(decided(), "model-a", going.ChosenByRightChoices, "", ""), "model-b", going.ChosenByRightChoices, "", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ruleFault(tc.records); (err == nil) != tc.passes {
				t.Errorf("ruleFault reads %v; want it to pass: %t", err, tc.passes)
			}
		})
	}
}

// A record entry with every field set reads back through JSON as it was written: no two fields
// share a name, so the fields the server reads (the model and chosen) come back beside every
// field a repeated run adds.
func TestARecordEntryRoundTrips(t *testing.T) {
	want := going.Record{
		Model: "demo-model", Endpoint: "https://decide.example",
		MeasuredAt: "2026-11-16T12:00:00Z", ReleaseDate: "2026-10-22T02:00:00+00:00",
		Cases: 117, NamedCases: 45, OtherCases: 72, ChartWorthyCases: 25, Unanswered: 1,
		NamedAccuracy: 0.97, OtherAccuracy: 0.99, P50MS: 108.8, P95MS: 178.2,
		DisplayRates:      []going.DisplayRate{{Label: going.LabelChartWorthy, Answered: going.DisplayChart, Count: 50, Of: 50, Rate: 1}},
		Repeats:           2,
		RepeatRights:      []going.RepeatRight{{Named: 44, Other: 71, Total: 115}, {Named: 45, Other: 71, Total: 116}},
		TotalSpread:       going.Spread{Mean: 115.5, Min: 115, Max: 116, SD: 0.7071},
		InconsistentCases: []going.CaseConsistency{{Words: "close DEMO-12", Right: 1, Of: 2}},
		PairedTests: []going.PairedTest{{
			Against: "other-model", Pairs: 234, Better: 1, Worse: 3, PValue: 0.625, MeanDifference: -1, Interval: &[2]float64{-13.7, 11.7},
		}},
		OutsideCases: []going.OutsideCase{{
			Words: "board drag", Note: "added after the spec named its cases", Action: "none", Right: 0, Of: 2,
			Chose: []going.ChoiceCount{{Action: "dispatch_issue_update", Count: 2}},
		}},
		Chosen: true, ChosenBy: going.ChosenByTieBreak, Chooser: "architect of DEMO-1", ChosenReason: "the cheaper of two level models",
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got going.Record
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the entry reads back as\n%+v\nwant\n%+v", got, want)
	}
}

// committedRecords is the committed record's entries.
func committedRecords(t *testing.T) []going.Record {
	t.Helper()
	raw, err := os.ReadFile("decision_models.json")
	if err != nil {
		t.Fatal(err)
	}
	var records []going.Record
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("decision_models.json: %v", err)
	}
	return records
}

// ruleFault is what is wrong with a record's choice, nil where nothing is: one entry is chosen,
// and it is the model R13's rule names (going.Choose) by the key that named it, or, where the
// rule names none, the model a tie-break named among the level ones with its chooser and reason
// kept (going.Choice.Break). An entry not chosen holds no way it was chosen, no chooser and no
// reason.
func ruleFault(records []going.Record) error {
	var chosen *going.Record
	for i := range records {
		r := &records[i]
		switch {
		case r.Chosen && chosen != nil:
			return fmt.Errorf("%s and %s are both chosen", chosen.Model, r.Model)
		case r.Chosen:
			chosen = r
		case r.ChosenBy != "" || r.Chooser != "" || r.ChosenReason != "":
			return fmt.Errorf("%s is not chosen, yet holds chosen_by %q, chooser %q and chosen_reason %q", r.Model, r.ChosenBy, r.Chooser, r.ChosenReason)
		}
	}
	if chosen == nil {
		return errors.New("no entry is chosen")
	}
	want := going.Choose(records)
	if chosen.ChosenBy == going.ChosenByTieBreak {
		broken, err := want.Break(chosen.Model, chosen.Chooser, chosen.ChosenReason)
		if err != nil {
			return fmt.Errorf("a tie-break chose %s, which the record does not bear out: %w", chosen.Model, err)
		}
		want = broken
	}
	if chosen.Model != want.Model || chosen.ChosenBy != want.By || chosen.Chooser != want.Chooser || chosen.ChosenReason != want.Reason {
		return fmt.Errorf("%s is chosen by %q (chooser %q) with the reason %q, but R13's rule names %q by %q (level: %v)",
			chosen.Model, chosen.ChosenBy, chosen.Chooser, chosen.ChosenReason, want.Model, want.By, want.Level)
	}
	return nil
}

// candidate is a record of a model measured on 117 cases, with its right choices in each repeat:
// at least 112 in each, 95% of its answers, make it meet R13's floors.
func candidate(model string, rights ...int) going.Record {
	r := going.Record{Model: model, Cases: 117, NamedAccuracy: 0.95, OtherAccuracy: 0.9, P95MS: 150, Repeats: len(rights)}
	for _, right := range rights {
		r.RepeatRights = append(r.RepeatRights, going.RepeatRight{Other: right, Total: right})
	}
	return r
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "(past the end)"
}

// sameShare is whether share is right of n, as the bake-off divides it.
func sameShare(share float64, right, n int) bool {
	return n > 0 && share == float64(right)/float64(n)
}
