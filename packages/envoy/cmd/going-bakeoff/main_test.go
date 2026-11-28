package main

//ci:reads packages/envoy/going/bakeoff/cases.json TestSavedAnswersScoreAgainstTheCasesGiven reads the committed case set (moduleRoot, going/bakeoff/cases.json)

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
)

// score pools every repeat: each repeat's right choices add up to the count R13's rule ranks by,
// each case is counted once however many times it was asked, the accuracies divide by every
// answer, and the latencies and the display rates take every answer of every repeat, not the
// last repeat's alone.
func TestScorePoolsEveryRepeat(t *testing.T) {
	closing := testCase{Words: "close DEMO-12", Action: "close_issue", NamesAction: true}
	trend := testCase{Words: "how is DEMO trending", Action: going.ActionNone, ChartWorthy: true}
	status := testCase{Words: "what changed on DEMO-12", Action: going.ActionNone}
	chose := func(c testCase, action string, confidence float64, display string, displayConfidence float64, ms int) answer {
		return answer{
			c: c, answered: true,
			action:  agentloop.Decision{Choice: action, Confidence: confidence},
			display: agentloop.Decision{Choice: display, Confidence: displayConfidence},
			latency: time.Duration(ms) * time.Millisecond,
		}
	}
	m := &measured{record: going.Record{Model: "demo-model", Repeats: 2}, repeats: [][]answer{
		{
			chose(closing, "close_issue", 0.9, going.DisplayProse, 0.9, 100),
			chose(trend, going.ActionNone, 0.6, going.DisplayChart, 0.7, 200),
			chose(status, "close_issue", 0.7, going.DisplayProse, 0.9, 300),
		},
		{
			{c: closing, fault: "timeout: no answer", latency: 400 * time.Millisecond},
			chose(trend, going.ActionNone, 0.8, going.DisplayChart, 0.9, 500),
			chose(status, going.ActionNone, 0.95, going.DisplayProse, 0.9, 600),
		},
	}}
	score(m)
	r := &m.record

	if want := []going.RepeatRight{{Named: 1, Other: 1, Total: 2}, {Named: 0, Other: 2, Total: 2}}; !slices.Equal(r.RepeatRights, want) {
		t.Errorf("the repeats' right choices are %+v, want %+v", r.RepeatRights, want)
	}
	if r.Right() != 4 {
		t.Errorf("the right choices pooled over the repeats are %d, want 4", r.Right())
	}
	if want := [][]bool{{true, true, false}, {false, true, true}}; !slices.EqualFunc(m.rights, want, slices.Equal) {
		t.Errorf("the right choices the paired test pairs are %v, want %v", m.rights, want)
	}
	if r.Cases != 3 || r.NamedCases != 1 || r.OtherCases != 2 || r.ChartWorthyCases != 1 || r.Unanswered != 1 {
		t.Errorf("the counts are %d cases, %d named, %d other, %d chart-worthy, %d unanswered; want 3, 1, 2, 1 and 1",
			r.Cases, r.NamedCases, r.OtherCases, r.ChartWorthyCases, r.Unanswered)
	}
	if r.NamedAccuracy != 0.5 || r.OtherAccuracy != 0.75 {
		t.Errorf("the accuracies are %v named and %v on the rest, want 1 of 2 and 3 of 4 answers", r.NamedAccuracy, r.OtherAccuracy)
	}
	if want := (going.Spread{Mean: 2, Min: 2, Max: 2}); r.TotalSpread != want {
		t.Errorf("the spread is %+v, want %+v", r.TotalSpread, want)
	}
	if want := []going.CaseConsistency{{Words: closing.Words, Right: 1, Of: 2}, {Words: status.Words, Right: 1, Of: 2}}; !slices.Equal(r.InconsistentCases, want) {
		t.Errorf("the cases answered both ways are %+v, want %+v", r.InconsistentCases, want)
	}
	if r.P50MS != 300 || r.P95MS != 600 {
		t.Errorf("the latencies are p50 %v and p95 %v ms, want 300 and 600 over all six calls", r.P50MS, r.P95MS)
	}
	for _, want := range []going.DisplayRate{
		{Label: going.LabelChartWorthy, Answered: going.DisplayChart, Count: 2, Of: 2, Rate: 1},
		{Label: going.LabelNotChartWorthy, Answered: going.DisplayUnanswered, Count: 1, Of: 4, Rate: 0.25},
	} {
		if !slices.Contains(r.DisplayRates, want) {
			t.Errorf("the display rates %+v lack %+v", r.DisplayRates, want)
		}
	}
	if r.MeetsFloors() {
		t.Error("a model right on 4 of its 6 answers, at a p95 of 600 ms, meets R13's floors")
	}
}

// A case outside R13's set is asked with the others and counted in none of the record's numbers:
// not the cases, the right choices, the accuracies, the latencies, the display rates or the pairs
// the paired test reads. Its answers are kept apart in OutsideCases, the report gives it a line of
// its own with what each model chose, and the misses name only R13's cases.
func TestACaseOutsideR13IsCountedApart(t *testing.T) {
	closing := testCase{Words: "close DEMO-12", Action: "close_issue", NamesAction: true}
	status := testCase{Words: "what changed on DEMO-12", Action: going.ActionNone}
	drag := testCase{Words: "board drag", Action: going.ActionNone, OutsideR13: true, Note: "added after the spec named its cases"}
	chose := func(c testCase, action string, ms int) answer {
		return answer{
			c: c, answered: true,
			action:  agentloop.Decision{Choice: action, Confidence: 0.9},
			display: agentloop.Decision{Choice: going.DisplayChart, Confidence: 0.9},
			latency: time.Duration(ms) * time.Millisecond,
		}
	}
	answered := func(model string) *measured {
		return &measured{record: going.Record{Model: model, Repeats: 2}, repeats: [][]answer{
			{chose(closing, "close_issue", 100), chose(drag, "dispatch_issue_update", 900), chose(status, going.ActionNone, 200)},
			{chose(closing, "close_issue", 100), {c: drag, fault: "timeout: no answer", latency: 950 * time.Millisecond}, chose(status, going.ActionNone, 200)},
		}}
	}
	m := answered("model-a")
	score(m)
	r := &m.record
	if r.Cases != 2 || r.NamedCases != 1 || r.OtherCases != 1 || r.Right() != 4 || r.Unanswered != 0 || r.NamedAccuracy != 1 || r.OtherAccuracy != 1 {
		t.Errorf("the record holds %d cases (%d named, %d other), %d right, %d unanswered, accuracies %v and %v; want R13's 2 cases, 4 right of 4, none unanswered",
			r.Cases, r.NamedCases, r.OtherCases, r.Right(), r.Unanswered, r.NamedAccuracy, r.OtherAccuracy)
	}
	if r.P95MS != 200 {
		t.Errorf("the p95 is %v ms, want 200 over R13's answers, not the outside case's 900 and 950", r.P95MS)
	}
	if want := (going.DisplayRate{Label: going.LabelNotChartWorthy, Answered: going.DisplayChart, Count: 4, Of: 4, Rate: 1}); !slices.Contains(r.DisplayRates, want) {
		t.Errorf("the display rates %+v lack %+v, R13's four answers alone", r.DisplayRates, want)
	}
	if want := [][]bool{{true, true}, {true, true}}; !slices.EqualFunc(m.rights, want, slices.Equal) {
		t.Errorf("the paired test pairs %v, want R13's cases alone %v", m.rights, want)
	}
	want := []going.OutsideCase{{
		Words: drag.Words, Note: drag.Note, Action: going.ActionNone, Right: 0, Of: 2,
		Chose: []going.ChoiceCount{{Action: "dispatch_issue_update", Count: 1}}, Unanswered: 1,
	}}
	if !slices.EqualFunc(r.OutsideCases, want, func(a, b going.OutsideCase) bool {
		return a.Words == b.Words && a.Note == b.Note && a.Action == b.Action && a.Right == b.Right && a.Of == b.Of &&
			a.Unanswered == b.Unanswered && slices.Equal(a.Chose, b.Chose)
	}) {
		t.Errorf("the outside cases are %+v, want %+v", r.OutsideCases, want)
	}

	other := answered("model-b")
	score(other)
	report, err := render([]going.Record{m.record, other.record}, going.Choose([]going.Record{m.record, other.record}), "a test run")
	if err != nil {
		t.Fatal(err)
	}
	line := `- "board drag", labelled none (added after the spec named its cases): model-a chose dispatch_issue_update in 1 of 2, gave no answer in 1 of 2; model-b chose dispatch_issue_update in 1 of 2, gave no answer in 1 of 2.`
	if !strings.Contains(report, "2 of the cases R13 names per model") || !strings.Contains(report, "| model-a |  | 100.0% (4 of 4) |") || !strings.Contains(report, line) {
		t.Errorf("the report reads\n%s\nwant R13's 2 cases, model-a right on 4 of 4, and the line %s", report, line)
	}
	var misses bytes.Buffer
	printMisses(&misses, m)
	if strings.Contains(misses.String(), drag.Words) {
		t.Errorf("the misses name the outside case:\n%s", misses.String())
	}
}

// A case outside R13's set says why it was added, and a case of the set carries no note.
func TestACaseOutsideR13CarriesItsNote(t *testing.T) {
	actions := []going.Action{{Name: "close_issue"}}
	for name, tc := range map[string]struct {
		cases string
		says  string
	}{
		"an outside case with no note": {`[{"words":"board drag","action":"none","outside_r13":true}]`, "no note"},
		"a note on a case of the set":  {`[{"words":"close DEMO-12","action":"close_issue","note":"why"}]`, "has a note"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cases.json")
			if err := os.WriteFile(path, []byte(tc.cases), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readCases(path, actions); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("readCases reads %v, want a refusal saying %q", err, tc.says)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(path, []byte(`[{"words":"board drag","action":"none","outside_r13":true,"note":"added later"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if cases, err := readCases(path, actions); err != nil || len(cases) != 1 || !cases[0].OutsideR13 || cases[0].Note != "added later" {
		t.Errorf("readCases reads %+v, %v; want the outside case with its note", cases, err)
	}
}

// A run the rule decides is written with the rule's model chosen, by the right choices.
func TestARunTheRuleDecidesIsWrittenWithItsChoice(t *testing.T) {
	out := filepath.Join(t.TempDir(), "decision_models.json")
	if err := conclude(io.Discard, []going.Record{floored("model-a", 115), floored("model-b", 116)}, "a test run", tieBreak{}, out); err != nil {
		t.Fatal(err)
	}
	if chosen := chosenIn(t, out); len(chosen) != 1 || chosen[0].Model != "model-b" || chosen[0].ChosenBy != going.ChosenByRightChoices || chosen[0].Chooser != "" || chosen[0].ChosenReason != "" {
		t.Errorf("the record chooses %+v, want model-b by its right choices", chosen)
	}
}

// A full tie on right choices is written with no model chosen, and the tool fails, saying the
// architect names one; a tie-break on that record then names one of the level models, with who
// broke the tie and why kept.
func TestAFullTieWaitsForTheArchitect(t *testing.T) {
	out := filepath.Join(t.TempDir(), "decision_models.json")
	tied := []going.Record{floored("model-a", 113, 112), floored("model-b", 112, 113)}
	err := conclude(io.Discard, tied, "a test run", tieBreak{}, out)
	if err == nil || !strings.Contains(err.Error(), "--tie-break") || !strings.Contains(err.Error(), "--tie-chooser") {
		t.Fatalf("a full tie concludes with %v, want an error naming --tie-break and --tie-chooser", err)
	}
	if chosen := chosenIn(t, out); len(chosen) != 0 {
		t.Fatalf("a full tie is written with %+v chosen, want none", chosen)
	}
	written, err := readRecords(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := conclude(io.Discard, written, "a test run", tieBreak{"model-b", "architect of DEMO-1", "the cheaper of two level models"}, out); err != nil {
		t.Fatal(err)
	}
	if chosen := chosenIn(t, out); len(chosen) != 1 || chosen[0].Model != "model-b" || chosen[0].ChosenBy != going.ChosenByTieBreak ||
		chosen[0].Chooser != "architect of DEMO-1" || chosen[0].ChosenReason != "the cheaper of two level models" {
		t.Errorf("the broken tie chooses %+v, want model-b by a tie-break of the architect of DEMO-1, with the reason", chosen)
	}
}

// The choice line says what R13's rule came to on the run (going.Choice.Outcome), from the choice
// alone: the model it names, with the right choices of the candidates in the order they rank, or
// none, offering the tie-break only on a full tie. It ranks nothing again: beside records whose
// right choices rank otherwise, it prints the same.
func TestTheChoiceLineSaysWhatTheRuleCameTo(t *testing.T) {
	tied := func() []going.Record {
		return []going.Record{floored("model-a", 113), floored("model-b", 113)}
	}
	unfloored := floored("model-a", 117)
	unfloored.P95MS = float64(going.JevP95Ceiling.Milliseconds())
	for _, tc := range []struct {
		name     string
		records  []going.Record
		tieBreak string
		outcome  going.ChoiceOutcome
		named    string
		// facts is what the line holds, in this order.
		facts []string
	}{
		{"a decided run", []going.Record{floored("model-c", 113), floored("model-a", 115), floored("model-b", 116)}, "",
			going.Named, "model-b", []string{"116 of 117", "model-a", "115", "model-c", "113"}},
		{"a full tie", tied(), "", going.FullTie, "", []string{"model-a", "model-b", "113 of 117"}},
		{"a full tie the architect broke", tied(), "model-b", going.Named, "model-b", []string{"architect of DEMO-1", "model-a", "model-b", "113 of 117", "the cheaper of two level models"}},
		{"no model meeting the floors", []going.Record{unfloored}, "", going.NoCandidate, "", []string{"floors"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choice := going.Choose(tc.records)
			if tc.tieBreak != "" {
				var err error
				if choice, err = choice.Break(tc.tieBreak, "architect of DEMO-1", "the cheaper of two level models"); err != nil {
					t.Fatal(err)
				}
			}
			if choice.Outcome != tc.outcome || choice.Model != tc.named {
				t.Errorf("Choose comes to %q, naming %q; want %q, naming %q", choice.Outcome, choice.Model, tc.outcome, tc.named)
			}
			printed, err := says(tc.records, choice)
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := strings.Cut(printed, "\n")
			if named := namedIn(line); named != choice.Model || (named != "") != (choice.Outcome == going.Named) {
				t.Errorf("the line %q names %q, where Choose comes to %q, naming %q", line, named, choice.Outcome, choice.Model)
			}
			if offered := strings.Contains(line, "--tie-break") && strings.Contains(line, "--tie-chooser"); offered != (choice.Outcome == going.FullTie) {
				t.Errorf("the line %q offers a tie-break: %t, where Choose comes to %q", line, offered, choice.Outcome)
			}
			rest := line
			for n, fact := range tc.facts {
				_, after, held := strings.Cut(rest, fact)
				if !held {
					t.Errorf("the line %q does not hold %q after %v", line, fact, tc.facts[:n])
					break
				}
				rest = after
			}
			if again, err := says(reranked(tc.records), choice); err != nil || again != printed {
				t.Errorf("beside records that rank otherwise, says prints\n%s (%v)\nwant\n%s", again, err, printed)
			}
		})
	}
}

// The entries of a record were measured on one case set, so a record whose entries hold other
// case counts is refused, naming the entry, as one with other repeats is.
func TestARecordOfOtherCasesIsRefused(t *testing.T) {
	for name, differ := range map[string]func(*going.Record){
		"more cases":                 func(r *going.Record) { r.Cases = 150 },
		"other cases naming actions": func(r *going.Record) { r.NamedCases, r.OtherCases = 46, 71 },
		"other cases in the rest":    func(r *going.Record) { r.OtherCases = 80 },
		"other repeats":              func(r *going.Record) { r.Repeats, r.RepeatRights = 2, append(r.RepeatRights, r.RepeatRights[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			other := floored("model-b", 112)
			differ(&other)
			raw, err := going.EncodeRecords([]going.Record{floored("model-a", 113), other})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "decision_models.json")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := readRecords(path); err == nil || !strings.Contains(err.Error(), "model-b") {
				t.Errorf("readRecords reads %v, want a refusal naming model-b", err)
			}
		})
	}
}

// A record whose entry is chosen by a way R13 does not know - the "person" of an earlier record,
// or any other word - is refused, naming the entry and the word, rather than read as a tie-break
// or the rule's choice and written back as a choice nobody made.
func TestARecordOfAnUnknownChosenByIsRefused(t *testing.T) {
	for _, by := range []string{"person", "noise"} {
		t.Run(by, func(t *testing.T) {
			tied := []going.Record{floored("model-a", 113), floored("model-b", 113)}
			tied[1].Chosen, tied[1].ChosenBy, tied[1].ChosenReason = true, by, "the cheaper model"
			raw, err := going.EncodeRecords(tied)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "decision_models.json")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			// The refusal's own words, never the bare word: every error readRecords answers names
			// the record's path, and t.TempDir() names that path after this subtest, so the word is
			// in any error at all.
			want := fmt.Sprintf("holds model-b chosen by %q, which is neither %s nor %s", by, going.ChosenByRightChoices, going.ChosenByTieBreak)
			if _, err := readRecords(path); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("readRecords reads %v, want the refusal %q", err, want)
			}
		})
	}
}

// The choice line has a case for each outcome of R13's rule and each way a model is named, and
// refuses an outcome or a way it does not know rather than print it as one it does; the rule's
// own choice of the same records prints.
func TestTheChoiceLineRefusesWhatItDoesNotKnow(t *testing.T) {
	records := []going.Record{floored("model-a", 113), floored("model-b", 112)}
	named := going.Choose(records)
	if line, err := says(records, named); err != nil || namedIn(line) != "model-a" {
		t.Fatalf("the rule's own choice prints %q (%v), want model-a named", line, err)
	}
	for name, choice := range map[string]going.Choice{
		"a way of naming nobody knows": {Outcome: going.Named, Model: "model-a", By: "person", Ranked: named.Ranked, Of: named.Of, Level: named.Level},
		"an outcome nobody knows":      {Outcome: "made_up", Ranked: named.Ranked, Of: named.Of},
	} {
		t.Run(name, func(t *testing.T) {
			if line, err := says(records, choice); err == nil {
				t.Errorf("says prints %q, want a refusal", line)
			}
		})
	}
}

// A plain --from-record keeps the architect's recorded tie-break where the rule still bears it
// out: it writes the record it read, byte for byte.
func TestARecordedTieBreakIsKept(t *testing.T) {
	dir := t.TempDir()
	broken, again := filepath.Join(dir, "broken.json"), filepath.Join(dir, "again.json")
	tied := []going.Record{floored("model-a", 113), floored("model-b", 113)}
	if err := conclude(io.Discard, tied, "a test run", tieBreak{"model-b", "architect of DEMO-1", "the cheaper of two level models"}, broken); err != nil {
		t.Fatal(err)
	}
	read, err := readRecords(broken)
	if err != nil {
		t.Fatal(err)
	}
	if err := conclude(io.Discard, read, "a test run", tieBreak{}, again); err != nil {
		t.Fatalf("a plain read of the broken tie concludes with %v", err)
	}
	want, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(again); err != nil || !bytes.Equal(got, want) {
		t.Errorf("a plain read of the broken tie writes\n%s\nwant\n%s (%v)", got, want, err)
	}
}

// A recorded tie-break the rule no longer bears out is refused on a plain --from-record, and
// nothing is written, rather than the record's choice being dropped or kept unchecked.
func TestARecordedTieBreakTheRuleDoesNotBearOutIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []going.Record
		model   string
	}{
		{"the rule names another", []going.Record{floored("model-a", 114), floored("model-b", 113)}, "model-b"},
		{"a model outside the tie", []going.Record{floored("model-a", 113), floored("model-b", 113), floored("model-c", 112)}, "model-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.records {
				if r := &tc.records[i]; r.Model == tc.model {
					r.Chosen, r.ChosenBy, r.Chooser, r.ChosenReason = true, going.ChosenByTieBreak, "architect of DEMO-1", "the cheaper model"
				}
			}
			out := filepath.Join(t.TempDir(), "decision_models.json")
			if err := conclude(io.Discard, tc.records, "a test run", tieBreak{}, out); err == nil {
				t.Error("conclude takes the recorded tie-break")
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a record is written (%v)", err)
			}
		})
	}
}

// Nothing is written where no model meets the floors or a tie-break is refused: on a run the
// rule decides, for a model outside the tie, with no chooser, or with no reason.
func TestARunWithNoChoiceToRecordWritesNothing(t *testing.T) {
	level := []going.Record{floored("model-a", 113), floored("model-b", 113)}
	unfloored := floored("model-a", 117)
	unfloored.P95MS = float64(going.JevP95Ceiling.Milliseconds())
	for _, tc := range []struct {
		name    string
		records []going.Record
		tie     tieBreak
	}{
		{"no model meeting the floors", []going.Record{unfloored}, tieBreak{}},
		{"a tie-break on a run the right choices decide", []going.Record{floored("model-a", 114), floored("model-b", 113)}, tieBreak{"model-b", "architect of DEMO-1", "a reason"}},
		{"a tie-break for a model outside the tie", level, tieBreak{"model-c", "architect of DEMO-1", "a reason"}},
		{"a tie-break naming no chooser", level, tieBreak{"model-a", " ", "a reason"}},
		{"a tie-break with no reason", level, tieBreak{"model-a", "architect of DEMO-1", " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "decision_models.json")
			if err := conclude(io.Discard, tc.records, "a test run", tc.tie, out); err == nil {
				t.Error("conclude takes it")
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a record is written (%v)", err)
			}
		})
	}
}

// floored is a measured model, measured on 117 cases, 45 naming their action, with its right
// choices in each repeat: at least 112 in each, 95% of its answers, make it meet R13's floors.
func floored(model string, rights ...int) going.Record {
	r := going.Record{Model: model, Cases: 117, NamedCases: 45, OtherCases: 72, NamedAccuracy: 0.95, OtherAccuracy: 0.9, P95MS: 150, Repeats: len(rights)}
	for _, right := range rights {
		r.RepeatRights = append(r.RepeatRights, going.RepeatRight{Other: right, Total: right})
	}
	return r
}

// reranked is records with more right choices in every repeat the earlier a model stands, so
// their counts and their order are not the ones the rule ranked.
func reranked(records []going.Record) []going.Record {
	moved := slices.Clone(records)
	for i := range moved {
		moved[i].RepeatRights = slices.Clone(moved[i].RepeatRights)
		for j := range moved[i].RepeatRights {
			moved[i].RepeatRights[j].Other += 10 * (len(moved) - i)
			moved[i].RepeatRights[j].Total += 10 * (len(moved) - i)
		}
	}
	return moved
}

// namedIn is the model a choice line names, "" for a line that names none.
func namedIn(line string) string {
	rest, named := strings.CutPrefix(line, "**Choice:** `")
	if !named {
		return ""
	}
	model, _, _ := strings.Cut(rest, "`")
	return model
}

// chosenIn is the entries the written record marks chosen.
func chosenIn(t *testing.T, path string) []going.Record {
	t.Helper()
	records, err := readRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(records, func(r going.Record) bool { return !r.Chosen })
}

// Answers --answers-out saved are scored by --score against the cases it is given, with no call:
// a model that chose each committed case's label is right on every case R13 names, with the
// latencies it saved, and its answer to each case outside them is kept apart; the same answers
// against the case set with 10 labels changed are right on 10 fewer, under 95%; and against a
// case set whose words they do not answer they are refused.
func TestSavedAnswersScoreAgainstTheCasesGiven(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	cases, _, err := caseSet(filepath.Join(root, "going", "bakeoff", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	answers := make([]answer, len(cases))
	for i, c := range cases {
		answers[i] = answer{
			c: c, answered: true, latency: 120 * time.Millisecond,
			action:  agentloop.Decision{Choice: c.Action, Confidence: 0.9},
			display: agentloop.Decision{Choice: going.DisplayProse, Confidence: 0.9},
		}
	}
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := saveAnswers(path, []*measured{{record: going.Record{Model: "jev-test", Endpoint: "https://jev.example", Repeats: 1}, repeats: [][]answer{answers}}}); err != nil {
		t.Fatal(err)
	}
	scored := func(against []testCase) (going.Record, error) {
		results, err := loadAnswers(path, against)
		if err != nil {
			return going.Record{}, err
		}
		score(results[0])
		return results[0].record, nil
	}

	record, err := scored(cases)
	if err != nil {
		t.Fatal(err)
	}
	r13 := len(slices.DeleteFunc(slices.Clone(cases), func(c testCase) bool { return c.OutsideR13 }))
	if record.Model != "jev-test" || record.Cases != r13 || record.Right() != r13 || record.P95MS != 120 || len(record.OutsideCases) != len(cases)-r13 {
		t.Errorf("the saved answers scored %s right on %d of %d cases at p95 %v ms, with %d kept apart; want jev-test right on every one of R13's %d at 120 ms, and the other %d kept apart",
			record.Model, record.Right(), record.Cases, record.P95MS, len(record.OutsideCases), r13, len(cases)-r13)
	}

	named := slices.IndexFunc(cases, func(c testCase) bool { return c.Action != going.ActionNone })
	relabelled := slices.Clone(cases)
	for i := range 10 {
		if relabelled[i].Action == going.ActionNone {
			relabelled[i].Action = cases[named].Action
		} else {
			relabelled[i].Action = going.ActionNone
		}
	}
	record, err = scored(relabelled)
	if err != nil {
		t.Fatal(err)
	}
	if share := record.RightShare(); record.Right() != r13-10 || share >= going.JevRightFloor {
		t.Errorf("the saved answers against 10 changed labels are right on %d of %d (%.3f); want %d, under 95%%",
			record.Right(), r13, share, r13-10)
	}

	other := slices.Clone(cases)
	other[3].Words = "a case no answer was saved for"
	if _, err := scored(other); err == nil || !strings.Contains(err.Error(), "case 4") {
		t.Errorf("the saved answers against cases they do not answer: %v; want a refusal naming case 4", err)
	}
}
