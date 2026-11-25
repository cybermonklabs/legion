package replay

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// compareFrom is the start of every scorecard these tests make up: a window of compareHours hours.
var compareFrom = clock(9, 0)

// compareHours is the span of the windows these tests make up.
const compareHours = 6

// sampleCard is a scorecard over [compareFrom, compareFrom+hours) with a value in every measure of
// R4 and three issues with times: ACME-1 with a first slot at the start and a done two hours on,
// ACME-2 with a first slot half an hour on, ACME-3 with none.
func sampleCard(hours int) Scorecard {
	date := compareFrom.Format(time.DateOnly)
	return Scorecard{
		Source: "replay", From: compareFrom, To: compareFrom.Add(time.Duration(hours) * time.Hour),
		Done: 20, WorkLeft: 3,
		WaitOnlyForSlot:      Occupancy{Average: 2, Max: 4, Hours: 10},
		StartedWithoutSlot:   Occupancy{Average: 3, Max: 5, Hours: 15},
		PastReviewHolding:    Occupancy{Average: 1, Max: 2, Hours: 5},
		PastReviewNotHolding: Occupancy{Average: 1.5, Max: 3, Hours: 7.5},
		PriorityTaken:        0.2,
		Loops: map[string]LoopHours{string(CauseReviewChanges): {Rounds: 4, Hours: 10}, string(CauseConflict): {Rounds: 2, Hours: 10},
			string(CauseSendBack): {Rounds: 3, Hours: 6}, string(CauseTestFailed): {Rounds: 2, Hours: 4}},
		Waits:           map[string]WaitHours{string(WaitLinkRemoved): {Count: 2, Hours: 8}, string(WaitOpen): {Count: 1, Hours: 3}},
		SlotHoursInTurn: 10, SlotHoursNoTurn: 5,
		InFlight:              Occupancy{Average: 6, Max: 8, Hours: 30},
		SlotsGivenByDay:       map[string]int{date: 10},
		MedianSlotWaitMinutes: 10,
		FirstSlot:             map[string]time.Time{"ACME-1": compareFrom, "ACME-2": compareFrom.Add(30 * time.Minute)},
		DoneAt:                map[string]time.Time{"ACME-1": compareFrom.Add(2 * time.Hour)},
		Assumptions:           []int{},
		Scenario:              MainBranch.NeverStarted,
	}
}

// Each gap of R5 passes a replay at its bound and fails one past it (spec R5): 10% of the issues
// finished and of the slots given out on each day, 30% of the median wait for a slot, of the hours
// of started work that waits only for a slot and of every other measure, 0.05 of the share of
// priority taken, and 60 minutes at the median of each issue's first-slot error; a measure the
// record lacks stands at 0 there, so any replayed value of it fails; and the numbers R5 tests not —
// the issues the start state could not place and the first day apart — fail no branch.
func TestValidationGaps(t *testing.T) {
	date := compareFrom.Format(time.DateOnly)
	shiftFirstSlots := func(minutes float64) func(*Scorecard) {
		return func(card *Scorecard) {
			shifted := map[string]time.Time{}
			for key, at := range card.FirstSlot {
				shifted[key] = at.Add(time.Duration(minutes * float64(time.Minute)))
			}
			card.FirstSlot = shifted
		}
	}
	cases := []struct {
		name    string
		measure string
		bound   func(*Scorecard)
		past    func(*Scorecard)
	}{
		{"issues finished, 10%", "done", func(c *Scorecard) { c.Done = 22 }, func(c *Scorecard) { c.Done = 23 }},
		{"issues finished below, 10%", "done", func(c *Scorecard) { c.Done = 18 }, func(c *Scorecard) { c.Done = 17 }},
		{"slots given out on a day, 10%", "slotsGivenByDay[" + date + "]",
			func(c *Scorecard) { c.SlotsGivenByDay = map[string]int{date: 11} }, func(c *Scorecard) { c.SlotsGivenByDay = map[string]int{date: 12} }},
		{"median wait for a slot, 30%", "medianSlotWaitMinutes",
			func(c *Scorecard) { c.MedianSlotWaitMinutes = 13 }, func(c *Scorecard) { c.MedianSlotWaitMinutes = 13.01 }},
		{"hours waiting only for a slot, 30%", "waitOnlyForSlot.hours",
			func(c *Scorecard) { c.WaitOnlyForSlot.Hours = 13 }, func(c *Scorecard) { c.WaitOnlyForSlot.Hours = 13.01 }},
		{"priority taken, 0.05", "priorityTaken", func(c *Scorecard) { c.PriorityTaken = 0.25 }, func(c *Scorecard) { c.PriorityTaken = 0.2501 }},
		{"priority taken below, 0.05", "priorityTaken", func(c *Scorecard) { c.PriorityTaken = 0.15 }, func(c *Scorecard) { c.PriorityTaken = 0.1499 }},
		{"each issue's first slot, 60 minutes at the median", FirstSlotTest, shiftFirstSlots(60), shiftFirstSlots(61)},
		{"another measure, 30%", "slotHoursInTurn", func(c *Scorecard) { c.SlotHoursInTurn = 13 }, func(c *Scorecard) { c.SlotHoursInTurn = 13.01 }},
		{"a loop's hours below, 30%", "loops[conflict].hours",
			func(c *Scorecard) { c.Loops[string(CauseConflict)] = LoopHours{Rounds: 2, Hours: 7} },
			func(c *Scorecard) { c.Loops[string(CauseConflict)] = LoopHours{Rounds: 2, Hours: 6.99} }},
		{"a measure the record lacks", "loops[queue_drop].rounds",
			func(c *Scorecard) {}, func(c *Scorecard) { c.Loops[string(CauseQueueDrop)] = LoopHours{Rounds: 1} }},
	}
	clone := func(card Scorecard) Scorecard {
		card.Loops, card.Waits, card.SlotsGivenByDay = maps.Clone(card.Loops), maps.Clone(card.Waits), maps.Clone(card.SlotsGivenByDay)
		card.FirstSlot, card.DoneAt = maps.Clone(card.FirstSlot), maps.Clone(card.DoneAt)
		return card
	}
	recorded := sampleCard(compareHours)
	recorded.Source = "record"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atBound := clone(recorded)
			c.bound(&atBound)
			if got := ValidateBranch(recorded, MainBranchAt, []Scorecard{atBound}); !got.Pass() {
				t.Errorf("a replay at the bound of %s fails %v; it should pass", c.measure, got.Failed)
			}
			past := clone(recorded)
			c.past(&past)
			if got := ValidateBranch(recorded, MainBranchAt, []Scorecard{past}); !slices.Equal(got.Failed, []string{c.measure}) {
				t.Errorf("a replay past the bound of %s fails %v; it should fail %s alone", c.measure, got.Failed, c.measure)
			}
		})
	}
	t.Run("numbers no test of R5 reads", func(t *testing.T) {
		replay := clone(recorded)
		replay.Unplaced = 40
		first := sampleCard(compareHours)
		first.Done = 100
		replay.FirstDayApart = &first
		got := ValidateBranch(recorded, MainBranchAt, []Scorecard{replay})
		if !got.Pass() {
			t.Errorf("a replay that differs from the record only in its unplaced issues and its first day apart fails %v", got.Failed)
		}
		for _, check := range got.Measures {
			if (check.Measure == "unplaced" || strings.HasPrefix(check.Measure, firstDayPrefix)) && check.Tested {
				t.Errorf("the validation tests %s, which no gap of R5 names", check.Measure)
			}
		}
	})
}

// A validation prints, for each branch, every measure of R4 with its recorded value, its replayed
// value (the mean of the seeds, with the lowest and the highest seed's) and the gap, and each
// issue's error in its first slot and in its done with their median and 90th percentile (spec R5).
// The errors are worked by hand: over a six-hour window, ACME-1's first slot is 10 and 20 minutes
// late in the two seeds (15) and its done 20 early and 10 late (15); ACME-2's first slot is on
// time and an hour late (30); ACME-3 holds no first slot in the record, so the record's stands at
// the window's end, 360 minutes on, and the seeds' at 60 and at the end (150). The median of 15,
// 30 and 150 is 30 and the 90th percentile by the nearest rank is 150; the one done error is 15.
func TestValidationPrintsEveryMeasureAndIssueError(t *testing.T) {
	recorded := sampleCard(compareHours)
	recorded.Source = "record"
	seed := func(n int64, firsts map[string]float64, done float64, doneAt bool) Scorecard {
		card := sampleCard(compareHours)
		card.Seed, card.Done, card.PriorityTaken = n, 20+int(n), 0.2+0.01*float64(n)
		card.Loops = maps.Clone(card.Loops)
		card.Loops[string(CauseQueueDrop)] = LoopHours{Rounds: int(n), Hours: float64(n)}
		card.FirstSlot = map[string]time.Time{}
		for key, minutes := range firsts {
			card.FirstSlot[key] = compareFrom.Add(time.Duration(minutes) * time.Minute)
		}
		card.DoneAt = map[string]time.Time{}
		if doneAt {
			card.DoneAt["ACME-1"] = compareFrom.Add(time.Duration(done) * time.Minute)
		}
		return card
	}
	seeds := []Scorecard{
		seed(1, map[string]float64{"ACME-1": 10, "ACME-2": 30, "ACME-3": 60}, 100, true),
		seed(2, map[string]float64{"ACME-1": 20, "ACME-2": 90}, 130, true),
	}
	branch := ValidateBranch(recorded, MainBranchAt, seeds)
	if branch.FirstSlot != (ErrorSummary{Issues: 3, Median: 30, P90: 150}) || branch.Done != (ErrorSummary{Issues: 1, Median: 15, P90: 15}) {
		t.Errorf("the issues' errors sum up as first slot %+v and done %+v, want {3 30 150} and {1 15 15}", branch.FirstSlot, branch.Done)
	}
	var out bytes.Buffer
	v := Validation{Window: "acme-window", Rule: "main", Record: recorded, Branches: []BranchValidation{branch}}
	if err := v.Write(&out, 1, func(BranchAt) bool { return true }); err != nil {
		t.Fatal(err)
	}
	printed := out.String()
	recordedValues := valuesOf(recorded)
	perSeed := []map[string]float64{valuesOf(seeds[0]), valuesOf(seeds[1])}
	names := measureNames(recorded, seeds[0], seeds[1])
	if !slices.Contains(names, "loops[queue_drop].rounds") {
		t.Fatalf("the measures %v leave out the loop only the replay holds", names)
	}
	for _, name := range names {
		a, b := perSeed[0][name], perSeed[1][name]
		mean := (a + b) / 2
		row := fmt.Sprintf("| %s | %s | %s | %s |", name, formatNumber(recordedValues[name]), spreadCell(mean, min(a, b), max(a, b), 2),
			formatNumber(mean-recordedValues[name]))
		if !strings.Contains(printed, row) {
			t.Errorf("the validation prints no row %q for the measure %s", row, name)
		}
	}
	for _, row := range []string{"| ACME-1 | 15 | 15 |", "| ACME-2 | 30 | none |", "| ACME-3 | 150 | none |",
		"the first slot over 3 issues, median 30 and 90th percentile 150; the done over 1 issues, median 15 and 90th percentile 15"} {
		if !strings.Contains(printed, row) {
			t.Errorf("the validation prints no %q:\n%s", row, printed)
		}
	}
}

// A branch whose replay of the recorded rule fails a test of R5 gives no comparison, and a window
// whose main branch fails gives none at all; the validation names each such branch, and says
// whether the main branch passes every test of R5 or how many of them it fails (spec R5).
func TestBranchThatFailsGivesNoComparison(t *testing.T) {
	recorded := sampleCard(compareHours)
	good := sampleCard(compareHours)
	bad := sampleCard(compareHours)
	bad.Done = 40
	wall, err := BranchNamed("main+timing=wall")
	if err != nil {
		t.Fatal(err)
	}
	failing := BranchAt{Branch: wall, Slowdown: 1}
	slower := BranchAt{Branch: MainBranch, Slowdown: 1.2}
	validation := func(main Scorecard) Validation {
		return Validation{Window: "acme-window", Rule: "main", Record: recorded, Branches: []BranchValidation{
			ValidateBranch(recorded, MainBranchAt, []Scorecard{main}),
			ValidateBranch(recorded, failing, []Scorecard{bad}),
			ValidateBranch(recorded, slower, []Scorecard{good}),
		}}
	}
	labels := func(branches []BranchAt) []string {
		var out []string
		for _, at := range branches {
			out = append(out, at.Label())
		}
		return out
	}
	t.Run("a failing branch", func(t *testing.T) {
		v := validation(good)
		if !v.MainPasses() || !v.Compares(slower) || v.Compares(failing) {
			t.Errorf("main passes %t, %s compares %t and %s compares %t; want true, true and false", v.MainPasses(), slower.Label(),
				v.Compares(slower), failing.Label(), v.Compares(failing))
		}
		if got := labels(v.NoComparison()); !slices.Equal(got, []string{failing.Label()}) {
			t.Errorf("the branches that give no comparison are %v, want [%s]", got, failing.Label())
		}
		var out bytes.Buffer
		if err := v.Write(&out, 1, nil); err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"The window's main branch (main@1) passes every test of R5.",
			"The branches that give no comparison: `main+timing=wall@1`."} {
			if !strings.Contains(out.String(), line) {
				t.Errorf("the validation prints no %q:\n%s", line, out.String())
			}
		}
	})
	t.Run("a failing main branch", func(t *testing.T) {
		v := validation(bad)
		if v.MainPasses() || v.Compares(slower) {
			t.Errorf("main passes %t and %s compares %t; a window whose main branch fails gives no comparison at all", v.MainPasses(), slower.Label(),
				v.Compares(slower))
		}
		if got, want := labels(v.NoComparison()), []string{MainBranchAt.Label(), failing.Label(), slower.Label()}; !slices.Equal(got, want) {
			t.Errorf("the branches that give no comparison are %v, want %v", got, want)
		}
		var out bytes.Buffer
		if err := v.Write(&out, 1, nil); err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"The window's main branch (main@1) fails 1 of R5's tests.", "So the window gives no comparison at all"} {
			if !strings.Contains(out.String(), line) {
				t.Errorf("the validation prints no %q:\n%s", line, out.String())
			}
		}
	})
}

// A forecast across a recorded change compares rates per hour, averages over time and shares, never
// totals, each within twice its gap (spec R5): a window half as long with half the work forecasts
// the same rates, and each kind passes at twice its gap and fails past it.
func TestForecastScoresRatesNeverTotals(t *testing.T) {
	after := sampleCard(100)
	after.Done, after.SlotsGivenByDay = 100, map[string]int{"2001-03-05": 100, "2001-03-06": 100}
	after.Loops = map[string]LoopHours{string(CauseReviewChanges): {Rounds: 4, Hours: 10}, string(CauseConflict): {Rounds: 2, Hours: 10},
		string(CauseSendBack): {Rounds: 4, Hours: 6}, string(CauseTestFailed): {Rounds: 2, Hours: 4}}
	after.Waits = map[string]WaitHours{string(WaitLinkRemoved): {Count: 2, Hours: 8}, string(WaitOpen): {Count: 2, Hours: 4}}
	half := func() Scorecard {
		card := sampleCard(50)
		card.Done, card.SlotsGivenByDay = 50, map[string]int{"2001-03-05": 100}
		card.Loops = map[string]LoopHours{}
		for cause, loop := range after.Loops {
			card.Loops[cause] = LoopHours{Rounds: loop.Rounds / 2, Hours: loop.Hours / 2}
		}
		card.Waits = map[string]WaitHours{}
		for word, wait := range after.Waits {
			card.Waits[word] = WaitHours{Count: wait.Count / 2, Hours: wait.Hours / 2}
		}
		card.SlotHoursInTurn, card.SlotHoursNoTurn = after.SlotHoursInTurn/2, after.SlotHoursNoTurn/2
		return card
	}
	kinds := []string{QuantityRate, QuantityAverage, QuantityShare}
	checkOf := func(checks []ForecastCheck, name string) ForecastCheck {
		i := slices.IndexFunc(checks, func(c ForecastCheck) bool { return c.Quantity == name })
		if i < 0 {
			t.Fatalf("the forecast holds no %s", name)
		}
		return checks[i]
	}
	checks := ForecastChecks(after, []Scorecard{half()})
	for _, c := range checks {
		if !slices.Contains(kinds, c.Kind) {
			t.Errorf("the forecast scores %s as %q, which is no rate per hour, average over time or share", c.Quantity, c.Kind)
		}
		if !c.Pass {
			t.Errorf("the forecast fails %s (%v against %v), though the window half as long has half the work", c.Quantity, c.Forecast, c.After)
		}
	}
	for _, total := range []string{"done", "slotsGivenByDay[2001-03-05]", "waitOnlyForSlot.hours", "loops[review_changes].rounds", "slotHoursInTurn"} {
		if slices.ContainsFunc(checks, func(c ForecastCheck) bool { return c.Quantity == total }) {
			t.Errorf("the forecast scores the total %s", total)
		}
	}
	bounds := []struct {
		name       string
		bound      func(*Scorecard)
		past       func(*Scorecard)
		kind, want string
	}{
		{"done per hour", func(c *Scorecard) { c.Done = 60 }, func(c *Scorecard) { c.Done = 61 }, QuantityRate, "20%"},
		{"waitOnlyForSlot.average", func(c *Scorecard) { c.WaitOnlyForSlot.Average = 3.2 }, func(c *Scorecard) { c.WaitOnlyForSlot.Average = 3.21 },
			QuantityAverage, "60%"},
		{"priorityTaken", func(c *Scorecard) { c.PriorityTaken = 0.3 }, func(c *Scorecard) { c.PriorityTaken = 0.3001 }, QuantityShare, "0.1"},
	}
	for _, b := range bounds {
		atBound, past := half(), half()
		b.bound(&atBound)
		b.past(&past)
		check := checkOf(ForecastChecks(after, []Scorecard{atBound}), b.name)
		if !check.Pass || check.Kind != b.kind || check.Gap.String() != b.want {
			t.Errorf("%s at twice its gap: pass %t, kind %s, gap %s; want a pass, %s, %s", b.name, check.Pass, check.Kind, check.Gap, b.kind, b.want)
		}
		if check := checkOf(ForecastChecks(after, []Scorecard{past}), b.name); check.Pass {
			t.Errorf("%s past twice its gap passes (%v against %v)", b.name, check.Forecast, check.After)
		}
	}
}

// A forecast scores the causes the workflow change redefines only inside the total of all
// send-back loops and each cause both releases define the same way apart, and names each cause in
// each group (plan D3): a forecast whose loops are all red checks where the window after had
// conflicts and send-backs passes on the total, holds no number of a redefined cause, and fails a
// cause defined the same way that it misses.
func TestForecastGroupsTheRedefinedCauses(t *testing.T) {
	after := sampleCard(compareHours)
	after.Loops = map[string]LoopHours{string(CauseConflict): {Rounds: 10, Hours: 50}, string(CauseSendBack): {Rounds: 10, Hours: 50},
		string(CauseReviewChanges): {Rounds: 5, Hours: 20}, string(CauseTestFailed): {Rounds: 5, Hours: 10}}
	forecast := sampleCard(compareHours)
	forecast.Loops = map[string]LoopHours{string(CauseRedChecksOrConflict): {Rounds: 20, Hours: 100},
		string(CauseReviewChanges): {Rounds: 5, Hours: 20}, string(CauseTestFailed): {Rounds: 5, Hours: 10}}
	checks := ForecastChecks(after, []Scorecard{forecast})
	for _, c := range checks {
		if !c.Pass {
			t.Errorf("the forecast fails %s (%v against %v): a cause the change redefines is scored only inside the total", c.Quantity, c.Forecast, c.After)
		}
		for _, cause := range ForecastGroups[GroupRedefined] {
			if strings.HasPrefix(c.Quantity, "loops["+string(cause)+"]") {
				t.Errorf("the forecast scores %s apart, a cause the workflow change redefines", c.Quantity)
			}
		}
	}
	for _, name := range []string{LoopsAll + ".rounds per hour", LoopsAll + ".hours per hour"} {
		i := slices.IndexFunc(checks, func(c ForecastCheck) bool { return c.Quantity == name })
		if i < 0 || !slices.Equal(checks[i].Causes, Causes) {
			t.Errorf("the forecast holds no %s over every cause", name)
		}
	}
	for _, cause := range ForecastGroups[GroupSame] {
		for _, suffix := range []string{".rounds per hour", ".hours per hour"} {
			name := "loops[" + string(cause) + "]" + suffix
			if !slices.ContainsFunc(checks, func(c ForecastCheck) bool { return c.Quantity == name }) {
				t.Errorf("the forecast holds no %s: each cause both releases define the same way is scored apart", name)
			}
		}
	}
	missed := forecast
	missed.Loops = maps.Clone(forecast.Loops)
	missed.Loops[string(CauseReviewChanges)] = LoopHours{Rounds: 20, Hours: 20}
	missedChecks := ForecastChecks(after, []Scorecard{missed})
	if i := slices.IndexFunc(missedChecks, func(c ForecastCheck) bool { return c.Quantity == "loops[review_changes].rounds per hour" }); i < 0 || missedChecks[i].Pass {
		t.Errorf("a forecast four times the review rounds of the window after passes them")
	}
	var out bytes.Buffer
	result := ForecastResult{Before: "window-two", After: "window-one", Rule: "legion-v20.42.5", Branch: MainBranchAt, Seeds: []int64{1}, Checks: checks,
		FromCommit: "1111111111111111111111111111111111111111", ToCommit: "2222222222222222222222222222222222222222"}
	if err := result.Write(&out, 1); err != nil {
		t.Fatal(err)
	}
	for _, sentence := range []string{
		"The causes the workflow change redefines (redefined): `red_checks_or_conflict`, `conflict`, `send_back`; each is scored only inside the total of all send-back loops, `loops[all]`.",
		"The causes both releases define the same way (same): `review_changes`, `test_failed`, `queue_drop`, `other_backward`; each is scored apart",
		"The forecast also carries the workflow commits between `1111111111111111111111111111111111111111` and `2222222222222222222222222222222222222222`",
	} {
		if !strings.Contains(out.String(), sentence) {
			t.Errorf("the forecast prints no %q:\n%s", sentence, out.String())
		}
	}
}

// A comparison names a rule best only when, on every branch of both windows that passes R5, it is
// no worse than main by more than R5's gap on the issues finished and on each of the owner's three
// costs, and better on at least one (spec R7). Main's costs are 100 issues finished, 50 hours
// waiting only for a slot, 0.2 of priority taken and 400 hours of other delays.
func TestBestRuleDecision(t *testing.T) {
	main := Costs{Done: 100, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400}
	with := func(change func(*Costs)) Costs {
		c := main
		change(&c)
		return c
	}
	branch := func(window string, at BranchAt, passes bool, costs map[string]Costs) BranchCosts {
		all := map[string]Costs{record.MainRule.Name: main}
		maps.Copy(all, costs)
		return BranchCosts{Window: window, Branch: at, Passes: passes, Costs: all}
	}
	slower := BranchAt{Branch: MainBranch, Slowdown: 1.6}
	both := func(passes [4]bool, costs map[string]Costs) []BranchCosts {
		return []BranchCosts{branch("one", MainBranchAt, passes[0], costs), branch("one", slower, passes[1], costs),
			branch("two", MainBranchAt, passes[2], costs), branch("two", slower, passes[3], costs)}
	}
	allPass := [4]bool{true, true, true, true}
	trade := func(d Decision, rule string) Trade {
		i := slices.IndexFunc(d.Trades, func(tr Trade) bool { return tr.Rule == rule })
		if i < 0 {
			t.Fatalf("the decision gives no trade of %s", rule)
		}
		return d.Trades[i]
	}
	t.Run("one rule best", func(t *testing.T) {
		d := Decide([]string{"main", "keep-past-review", "cover"}, both(allPass, map[string]Costs{
			"keep-past-review": with(func(c *Costs) { c.Done = 105 }),
			"cover":            with(func(c *Costs) { c.Done, c.PriorityTaken = 105, 0.26 }),
		}))
		if !slices.Equal(d.Best, []string{"keep-past-review"}) || d.Branches != 4 {
			t.Errorf("the decision names %v best over %d branches, want [keep-past-review] over 4", d.Best, d.Branches)
		}
		if got := trade(d, "cover").Measures[2]; got != (MeasureTrade{Measure: "priorityTaken", Worse: 4}) {
			t.Errorf("cover's trade on priority taken is %+v, want worse on all 4 branches", got)
		}
	})
	t.Run("none best, with each trade", func(t *testing.T) {
		d := Decide([]string{"keep-past-review", "returned-by-rank"}, both(allPass, map[string]Costs{
			"keep-past-review": with(func(c *Costs) { c.Done, c.WaitOnlyForSlot = 110, 70 }),
			"returned-by-rank": with(func(c *Costs) { c.Done = 85 }),
		}))
		if len(d.Best) != 0 {
			t.Errorf("the decision names %v best, though each rule is worse than main by more than R5's gap on one measure", d.Best)
		}
		want := map[string][]MeasureTrade{
			"keep-past-review": {{Measure: "done", Better: 4}, {Measure: "waitOnlyForSlot.hours", Worse: 4},
				{Measure: "priorityTaken", Level: 4}, {Measure: "otherDelays.hours", Level: 4}},
			"returned-by-rank": {{Measure: "done", Worse: 4}, {Measure: "waitOnlyForSlot.hours", Level: 4},
				{Measure: "priorityTaken", Level: 4}, {Measure: "otherDelays.hours", Level: 4}},
		}
		for rule, measures := range want {
			if got := trade(d, rule).Measures; !slices.Equal(got, measures) {
				t.Errorf("%s's trade is %+v, want %+v", rule, got, measures)
			}
		}
	})
	t.Run("better on one cost and worse on another", func(t *testing.T) {
		d := Decide([]string{"blockers-first", "cover"}, both(allPass, map[string]Costs{
			"blockers-first": with(func(c *Costs) { c.WaitOnlyForSlot, c.OtherDelays = 40, 520.5 }),
			"cover":          with(func(c *Costs) { c.WaitOnlyForSlot, c.OtherDelays = 40, 520 }),
		}))
		if !slices.Equal(d.Best, []string{"cover"}) {
			t.Errorf("the decision names %v best; blockers-first is worse than main past R5's gap on the other delays and cover is at it, so cover alone", d.Best)
		}
		if got := standingOf(with(func(c *Costs) { c.WaitOnlyForSlot, c.OtherDelays = 40, 520.5 }), main); got != StandingTrade {
			t.Errorf("a rule better on one cost and worse on another stands %s against main, want %s", got, StandingTrade)
		}
	})
	t.Run("a branch that failed R5 cannot name a rule", func(t *testing.T) {
		betterOnFailed := []BranchCosts{
			branch("one", MainBranchAt, true, map[string]Costs{"cover": main}),
			branch("one", slower, false, map[string]Costs{"cover": with(func(c *Costs) { c.Done = 120 })}),
		}
		if d := Decide([]string{"cover"}, betterOnFailed); len(d.Best) != 0 || d.Branches != 1 {
			t.Errorf("a rule better only on a branch that failed R5 is named best (%v over %d branches)", d.Best, d.Branches)
		}
		worseOnFailed := []BranchCosts{
			branch("one", MainBranchAt, true, map[string]Costs{"cover": with(func(c *Costs) { c.Done = 105 })}),
			branch("one", slower, false, map[string]Costs{"cover": with(func(c *Costs) { c.Done = 50 })}),
		}
		if d := Decide([]string{"cover"}, worseOnFailed); !slices.Equal(d.Best, []string{"cover"}) {
			t.Errorf("a rule better on every branch that passes R5 is not named best (%v): a branch that failed is read", d.Best)
		}
		if d := Decide([]string{"cover"}, both([4]bool{}, map[string]Costs{"cover": with(func(c *Costs) { c.Done = 120 })})); len(d.Best) != 0 || d.Branches != 0 {
			t.Errorf("with no branch that passes R5 the decision names %v best over %d branches", d.Best, d.Branches)
		}
	})
}

// The break-even search runs the main branch from 1.0 to 2.0 by 0.1, and a rule's break-even factor
// is the first at which its standing against main differs from its standing at 1.0, or none (spec
// R3, assumption 1).
func TestBreakEvenFactor(t *testing.T) {
	factors := BreakEvenSlowdowns()
	if want := []float64{1, 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9, 2}; !slices.Equal(factors, want) {
		t.Fatalf("the break-even search runs %v, want %v", factors, want)
	}
	main := map[float64]Costs{}
	rule := map[float64]Costs{}
	for _, f := range factors {
		main[f] = Costs{Done: 100, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400}
		done := 105.0
		if f >= 1.4 {
			done = 85
		}
		rule[f] = Costs{Done: done, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400}
	}
	got := BreakEven(factors, rule, main)
	if !got.Changes || got.Factor != 1.4 || got.Standings[0].Standing != StandingBetter || standingAt(got, 1.4) != StandingWorse {
		t.Errorf("the search gives %+v; want the factor 1.4, better below it and worse from it", got)
	}
	skipped := maps.Clone(rule)
	delete(skipped, 1.4)
	if got := BreakEven(factors, skipped, main); !got.Changes || got.Factor != 1.5 || len(got.Standings) != len(factors)-1 {
		t.Errorf("with no run at 1.4 the search gives %+v; want the factor 1.5 over the ten factors that ran", got)
	}
	steady := map[float64]Costs{}
	for _, f := range factors {
		steady[f] = rule[1]
	}
	if got := BreakEven(factors, steady, main); got.Changes || len(got.Standings) != len(factors) {
		t.Errorf("a rule better at every factor gives %+v; no factor up to 2.0 changes its rank", got)
	}
}

// The document of a comparison holds every rule, window, branch and measure (spec R7; check k6): it
// is written from made-up runs of two windows under every rule the repository keeps, every branch
// with its seeds and the break-even search, and is read for each window's scorecard, validation
// and table of each branch, every rule in each table, every measure in each, what each rule with a
// grace keeps on each window, the forecast with the workflow commits it carries, which window
// measures conflicts, the name of the issues file, each rule's break-even factor, the decision,
// the assumptions and the build commit of the runs: each window's runs were made by a binary of
// its own, the window after's first, so the document names after's commit before before's, each
// with its count of runs. With LEGION_TEST_PG_DSN set it also replays two fixture windows whole
// and in parts and holds the two documents equal, and the two issues files: what is written from
// one process's runs and what is written from the runs of parts, each run in a process of its own,
// are the same, and the document names the build commit the runs were given.
func TestTheDocumentHasEveryRuleWindowBranchAndMeasure(t *testing.T) {
	dir := t.TempDir()
	to := compareFrom.Add(compareHours * time.Hour)
	recorded := func(name string, order int, rule, commit string) windowRecord {
		return windowRecord{Name: name, Order: order, Project: "ACME", From: compareFrom, To: to, Rule: rule, Scorecard: sampleCard(compareHours),
			Parts: []Part{{From: compareFrom, To: to, Release: "fixture", Commit: commit, Rule: rule}}}
	}
	before := recorded("window-two", 0, "legion-v19.57.3", "1111111111111111111111111111111111111111")
	before.Stretches = 4
	before.Scorecard.Priorities = comparisonUrgentIssues()
	after := recorded("window-one", 1, "legion-v20.42.5", "2222222222222222222222222222222222222222")
	after.Stretches, after.Conflicted = 10, 3
	after.Scorecard.Priorities = comparisonUrgentIssues()
	after.Conflicts = ConflictModel{Stretches: 10, Conflicts: 3, Base: Coefficient{Estimate: -2, Low: -3, High: -1},
		Term: Coefficient{Estimate: 0.5, Low: -0.25, High: 1.25}, Level: conflictLevel, Phases: map[phase.Phase]PhaseFit{
			phase.Merging: {Stretches: 6, Conflicts: 1}, phase.Testing: {Stretches: 4, Conflicts: 2, Term: &Coefficient{Estimate: 1.5, Low: 0.5, High: 2.5}}}}
	type build struct {
		commit  string
		started time.Time
		runs    int
	}
	builds := map[string]*build{before.Name: {commit: "3333333333333333333333333333333333333333", started: compareFrom.Add(48 * time.Hour)},
		after.Name: {commit: "4444444444444444444444444444444444444444", started: compareFrom.Add(24 * time.Hour)}}
	plan := fullPlan()
	var cards []Scorecard
	rules := record.SlotRules()
	var noWorkRows, signalRows []string
	// Only the synthetic comparison uses these additional run.json fields. The limit test
	// will supply the same readings from its sweep, rather than from the ordinary branches.
	signalReadings := map[string]map[string]any{}
	branchIndex := map[string]int{}
	for index, at := range plan.branches {
		branchIndex[at.Label()] = index
	}
	writeMadeUpRuns(t, dir, []windowRecord{before, after}, rules, plan,
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot := sampleCard(compareHours)
			snapshot.Rule, snapshot.Branch, snapshot.Seed, snapshot.Slowdown = rule.Name, at.Branch.Name, seed, at.Slowdown
			snapshot.Done += r
			snapshot.PriorityTaken += 0.001 * float64(seed)
			snapshot.Priorities = comparisonUrgentIssues()
			slotWait := comparisonSlotWait(w.Order, r, branchIndex[at.Label()])
			snapshot.FirstSlot["ACME-2"] = compareFrom.Add(time.Duration(slotWait * float64(time.Hour)))
			line := snapshot
			line.SlotHoursInTurn += 0.5
			cards = append(cards, snapshot)
			b := builds[w.Name]
			b.runs++
			noWork := (int(seed) + r + branchIndex[at.Label()] + w.Order) % 4
			noWorkHours := float64(noWork) / 4
			noWorkRows = append(noWorkRows, fmt.Sprintf("| %s | `%s` | `%s` | %d | %d | %s |",
				w.Name, rule.Name, at.Label(), seed, noWork, formatNumber(noWorkHours)))
			if w.Name == after.Name && rule.Name == "main" && (at.Label() == "main@1" || at.Label() == "main@1.2") && seed == 1 {
				readings := map[string]any{}
				for i, name := range []string{"idle", "queue", "provider", "kills", "approvals"} {
					scale := map[string]float64{"idle": 0.1, "provider": 0.1}[name]
					if scale == 0 {
						scale = 1
					}
					limit := float64(i+1) * scale
					if at.Slowdown == 1.2 {
						limit *= 2
					}
					value := limit + scale
					readings[name] = map[string]float64{"limit": limit, "value": value}
					signalRows = append(signalRows, fmt.Sprintf("| `%s` | %s | %s |", name, formatNumber(limit), formatNumber(value)))
				}
				signalReadings[runDir(dir, w.Name, rule.Name, at, seed)] = readings
			}
			return snapshot, line, runRecord{Rule: rule.Name, RuleCommit: rule.Commit, BuildCommit: b.commit, Branch: at.Branch, Seed: seed,
				Slowdown: at.Slowdown, Scenario: at.Branch.NeverStarted, Assumptions: []int{1, 3, 4},
				Divergence: Divergence{NoWorkAdmissions: noWork, NoWorkSlotHours: noWorkHours},
				StartedAt:  b.started.Add(time.Duration(b.runs) * time.Minute), Gaps: Gaps(snapshot, line)}
		}, func(directory string, ran runRecord) any {
			if readings := signalReadings[directory]; readings != nil {
				return struct {
					runRecord
					Signals map[string]any `json:"signals"`
				}{ran, readings}
			}
			return ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison(filepath.Join(dir, "comparison-issues.md"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, w := range []windowRecord{before, after} {
		want = append(want, "### "+w.Name+"\n", "### "+w.Name+" under `"+w.Rule+"`")
	}
	want = append(want, "## The rule named best (R7)", "## The scorecard of each window, read from its record", "## The validation of each window (R5)",
		"## What each grace keeps, read from each window's record",
		"## The forecast across the change (R5)", "replayed under the rule after it, `legion-v20.42.5`",
		"The forecast also carries the workflow commits between `1111111111111111111111111111111111111111` and `2222222222222222222222222222222222222222`",
		"window-two does not measure how a rule changes conflicts", "window-one measures how a rule changes conflicts",
		"The rate fitted on window-one: base -2 [-3, -1] (merging: stretches 6, conflicts 1), testing term 1.5 [0.5, 2.5] (stretches 4, conflicts 2), "+
			"reviewing term none (stretches 0, conflicts 0), accepting term none (stretches 0, conflicts 0), retro term none (stretches 0, conflicts 0), "+
			"merges term 0.5 [-0.25, 1.25]",
		"## Each issue's first slot and done under each rule", "`comparison-issues.md`, beside this document, gives for each window and each rule",
		"## The gap between the snapshot and the line timelines", "## The break-even slowdown factor of each rule against main",
		"## The assumptions and scenarios each run used", "3. "+Assumptions[2].Text)
	total := builds[before.Name].runs + builds[after.Name].runs
	want = append(want, fmt.Sprintf("- The build commit of each run, the commit of the `legion` binary that made it: `%s` (%d of the %d runs), `%s` (%d of the %d runs).\n",
		builds[after.Name].commit, builds[after.Name].runs, total, builds[before.Name].commit, builds[before.Name].runs, total))
	for _, rule := range record.SlotRules() {
		if rule.Name != record.MainRule.Name {
			want = append(want, "| `"+rule.Name+"` | no factor up to 2 changes it")
		}
		want = append(want, "- `"+rule.Name+"`, `main@1.1`")
		if rule.WaitGrace > 0 {
			for _, w := range []windowRecord{before, after} {
				want = append(want, "| "+w.Name+" | `"+rule.Name+"` | "+rule.WaitGrace.String()+" | ")
			}
		}
	}
	for _, line := range want {
		if !strings.Contains(document, line) {
			t.Errorf("the document holds no %q", line)
		}
	}
	assertComparisonRows(t, document, "no-work admissions and slot-hours", noWorkRows)
	assertComparisonRows(t, document, "signal values at their limits", signalRows)
	names := measureNames(cards...)
	head := tableHead("measure", coded(namesOf(record.SlotRules())))
	for _, w := range []windowRecord{before, after} {
		for index, at := range plan.branches {
			heading := "### " + w.Name + ", `" + at.Label() + "` ("
			start := strings.Index(document, heading)
			if start < 0 {
				t.Errorf("the document holds no table of %s on %s", w.Name, at.Label())
				continue
			}
			table := document[start:]
			if end := strings.Index(table[len(heading):], "\n### "); end >= 0 {
				table = table[:len(heading)+end]
			}
			if !strings.Contains(table, head) {
				t.Errorf("the table of %s on %s does not head a column for every rule:\n%s", w.Name, at.Label(), table)
			}
			for _, name := range names {
				if !strings.Contains(table, "\n| "+name+" |") {
					t.Errorf("the table of %s on %s holds no row of %s", w.Name, at.Label(), name)
				}
			}
			var urgentRows []string
			// P0: first slots at 0 and slotWait, done at 2 and 6 (one cut).
			// P1: first todo at hour 1, no slot or done (both waits cut at hour 6).
			for _, measure := range []struct {
				name  string
				value func(slotWait float64) float64
			}{
				{"P0 issues", func(float64) float64 { return 2 }},
				{"P0 to first slot, median", func(wait float64) float64 { return wait / 2 }},
				{"P0 to first slot, 90th percentile", func(wait float64) float64 { return wait }},
				{"P0 to first slot, cut waits", func(float64) float64 { return 0 }},
				{"P0 to done, median", func(float64) float64 { return 4 }},
				{"P0 to done, 90th percentile", func(float64) float64 { return 6 }},
				{"P0 to done, cut waits", func(float64) float64 { return 1 }},
				{"P1 issues", func(float64) float64 { return 1 }},
				{"P1 to first slot, median", func(float64) float64 { return 5 }},
				{"P1 to first slot, 90th percentile", func(float64) float64 { return 5 }},
				{"P1 to first slot, cut waits", func(float64) float64 { return 1 }},
				{"P1 to done, median", func(float64) float64 { return 5 }},
				{"P1 to done, 90th percentile", func(float64) float64 { return 5 }},
				{"P1 to done, cut waits", func(float64) float64 { return 1 }},
			} {
				row := "| " + measure.name + " |"
				for r := range rules {
					row += " " + formatNumber(measure.value(comparisonSlotWait(w.Order, r, index))) + " |"
				}
				urgentRows = append(urgentRows, row)
			}
			assertComparisonRows(t, table, fmt.Sprintf("%s on %s: urgent waits in hours", w.Name, at.Label()), urgentRows)
		}
	}
	t.Run("whole or in parts", func(t *testing.T) {
		dsn := replayDSN(t)
		windows := []string{filepath.Join(fixtureWindowsDir, "review-and-test-kept"), filepath.Join(fixtureWindowsDir, "conflict-from-fitted-rate")}
		small := &runPlan{branches: []BranchAt{MainBranchAt}, breakEven: []float64{1.1}, perDraw: 2}
		const fixtureBuild = "5555555555555555555555555555555555555555"
		config := CompareConfig{Windows: windows, Rules: []record.SlotRule{record.MainRule}, PGDSN: dsn, Parallel: 2, BuildCommit: fixtureBuild, plan: small}
		whole := config
		whole.Runs = t.TempDir()
		if err := RunCompare(context.Background(), whole); err != nil {
			t.Fatal(err)
		}
		wholeRuns, err := ReadRuns(whole.Runs)
		if err != nil {
			t.Fatal(err)
		}
		wholeDocument, wholeIssues, err := wholeRuns.Comparison("comparison-issues.md")
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, window := range []string{"review-and-test-kept", "conflict-from-fitted-rate"} {
			for _, rule := range []string{"main", "legion-v20.42.5"} {
				part := config
				part.Runs, part.OnlyWindow, part.OnlyRule = t.TempDir(), window, rule
				if err := RunCompare(context.Background(), part); err != nil {
					t.Fatal(err)
				}
				parts = append(parts, part.Runs)
			}
		}
		partRuns, err := ReadRuns(parts...)
		if err != nil {
			t.Fatal(err)
		}
		partDocument, partIssues, err := partRuns.Comparison("comparison-issues.md")
		if err != nil {
			t.Fatal(err)
		}
		if line, a, b, differ := firstDifference([]byte(wholeDocument), []byte(partDocument)); differ {
			t.Fatalf("the document written in parts differs from the one written whole at line %d:\nwhole: %s\nparts: %s", line, a, b)
		}
		if line, a, b, differ := firstDifference([]byte(wholeIssues), []byte(partIssues)); differ {
			t.Fatalf("the issues file written in parts differs from the one written whole at line %d:\nwhole: %s\nparts: %s", line, a, b)
		}
		for _, line := range []string{"## The rule named best (R7)", "### review-and-test-kept, `main@1`", "### conflict-from-fitted-rate, `main@1`",
			"- `legion-v20.42.5`, `main@1.1`", "- The build commit of each run, the commit of the `legion` binary that made it: `" + fixtureBuild + "` ("} {
			if !strings.Contains(wholeDocument, line) {
				t.Errorf("the document of the fixture windows holds no %q:\n%s", line, wholeDocument)
			}
		}
		for _, section := range []string{"## review-and-test-kept under `main` (seeds: ", "## conflict-from-fitted-rate under `legion-v20.42.5` (seeds: "} {
			if !strings.Contains(wholeIssues, section) {
				t.Errorf("the issues file of the fixture windows holds no %q:\n%s", section, wholeIssues)
			}
		}
	})
}

func comparisonUrgentIssues() map[string]IssuePriority {
	return map[string]IssuePriority{
		"ACME-1": {"P0", compareFrom}, "ACME-2": {"P0", compareFrom},
		"ACME-3": {"P1", compareFrom.Add(time.Hour)},
	}
}

func comparisonSlotWait(window, rule, branch int) float64 {
	return 0.5 + 0.5*float64((window+rule+branch)%2)
}

func assertComparisonRows(t *testing.T, text, label string, want []string) {
	t.Helper()
	lines := make(map[string]bool)
	for line := range strings.SplitSeq(text, "\n") {
		lines[line] = true
	}
	var missing []string
	for _, row := range want {
		if !lines[row] {
			missing = append(missing, row)
		}
	}
	if len(missing) != 0 {
		examples := missing[:min(14, len(missing))]
		t.Errorf("%s: %d of %d rows absent or wrong:\n%s", label, len(missing), len(want), strings.Join(examples, "\n"))
	}
}

// writeMadeUpRuns writes under dir the record of each of windows and, for each window under each of
// rules, the part plan runs with each of its runs, whose snapshot and line scorecards and run.json
// run makes up from the window, the rule's place among rules, the rule, the branch and the seed:
// the directory of runs a comparison reads (ReadRuns), made with no replay.
func writeMadeUpRuns(t *testing.T, dir string, windows []windowRecord, rules []record.SlotRule, plan runPlan,
	run func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (snapshot, line Scorecard, ran runRecord),
	wire ...func(directory string, ran runRecord) any) {
	t.Helper()
	for _, w := range windows {
		if err := writeJSON(filepath.Join(dir, w.Name, recordFile), w); err != nil {
			t.Fatal(err)
		}
		for r, rule := range rules {
			part := partRecord{Window: w.Name, Rule: rule.Name, RuleCommit: rule.Commit, Branches: []partRun{}, BreakEven: []partRun{}, Parallel: 1}
			write := func(at BranchAt) partRun {
				seeds := w.seeds(at.Branch, plan.perDraw)
				for _, seed := range seeds {
					snapshot, line, ran := run(w, r, rule, at, seed)
					runDirectory := runDir(dir, w.Name, rule.Name, at, seed)
					var runJSON any = ran
					if len(wire) != 0 {
						runJSON = wire[0](runDirectory, ran)
					}
					for name, value := range map[string]any{runFile: runJSON, scorecardFile: snapshot, lineScorecardFile: line} {
						if err := writeJSON(filepath.Join(runDirectory, name), value); err != nil {
							t.Fatal(err)
						}
					}
				}
				return partRun{Branch: at.Branch.Name, Slowdown: at.Slowdown, Seeds: seeds}
			}
			for _, at := range plan.branches {
				part.Branches = append(part.Branches, write(at))
			}
			for _, f := range plan.breakEven {
				part.BreakEven = append(part.BreakEven, write(BranchAt{Branch: MainBranch, Slowdown: f}))
			}
			for _, at := range plan.diagnostics {
				part.Diagnostics = append(part.Diagnostics, write(at))
			}
			if err := writeJSON(filepath.Join(dir, w.Name, rule.Name, partFile), part); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// madeUpRun is a run of writeMadeUpRuns whose own numbers a test does not read: sampleCard as both
// its snapshot and its line scorecard, under the rule, the branch and the seed it is written for.
func madeUpRun(_ windowRecord, _ int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
	card := sampleCard(compareHours)
	card.Rule, card.Branch, card.Seed, card.Slowdown = rule.Name, at.Branch.Name, seed, at.Slowdown
	return card, card, runRecord{Rule: rule.Name, RuleCommit: rule.Commit, Branch: at.Branch, Seed: seed, Slowdown: at.Slowdown,
		Scenario: at.Branch.NeverStarted, Assumptions: []int{1}, StartedAt: compareFrom, Gaps: Gaps(card, card)}
}

// madeUpWindows is two windows of compareHours hours from compareFrom for a comparison of made-up
// runs, window-1 before the change of the rule and window-2 after it, whose daemons ran the rules
// before and after, each with sampleCard as its record's scorecard.
func madeUpWindows(before, after string) []windowRecord {
	to := compareFrom.Add(compareHours * time.Hour)
	var windows []windowRecord
	for i, rule := range []string{before, after} {
		windows = append(windows, windowRecord{Name: fmt.Sprintf("window-%d", i+1), Order: i, Project: "ACME", From: compareFrom, To: to, Rule: rule,
			Scorecard: sampleCard(compareHours), Parts: []Part{{From: compareFrom, To: to, Release: "fixture", Commit: strings.Repeat("1", 40), Rule: rule}}})
	}
	return windows
}

// specRules is the rules the spec names: main, the rules the recorded windows' daemons ran, and the
// spec's own four candidates. Every other rule the repository keeps is a candidate added to them
// (plan D6), whose reason the comparison's document gives.
var specRules = []string{"main", "legion-v20.42.5", "legion-v19.57.3", "legion-v22.14.1", "keep-past-review", "blockers-first", "returned-by-rank", "cover"}

// The document gives, beside the row of each candidate added to the spec's rules, the reason it was
// added (plan D6), and gives none beside a rule the spec names (specRules). The document is written
// from made-up runs of two windows under every rule the repository keeps, on the main branch, and
// read in the column "why it was added" of the table of each rule's trade against main, which holds
// a row for every rule but main, the rule each is judged against. So a rule the repository comes to
// keep beyond the spec's fails here until its reason is given. A reason names no issue key and no
// Dispatch link, since the repository is public.
func TestTheDocumentGivesEachAddedCandidatesReason(t *testing.T) {
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, madeUpWindows("legion-v19.57.3", "legion-v20.42.5"), record.SlotRules(),
		runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}, madeUpRun)
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison("comparison-issues.md")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(document, "Each rule's trade against `main`")
	if start < 0 {
		t.Fatalf("the document holds no table of each rule's trade against main:\n%s", document)
	}
	// The paragraph, a blank line, the table's head and its separator, then a row for each rule up
	// to a blank line.
	lines := strings.Split(document[start:], "\n")
	if len(lines) < 4 {
		t.Fatalf("the trade against main ends at its paragraph:\n%s", document[start:])
	}
	cells := func(row string) []string {
		parts := strings.Split(strings.Trim(row, "|"), "|")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	column := slices.Index(cells(lines[2]), "why it was added")
	reasons := map[string]string{}
	for _, row := range lines[4:] {
		if row == "" {
			break
		}
		c := cells(row)
		reason := ""
		if column >= 0 && column < len(c) {
			reason = c[column]
		}
		reasons[strings.Trim(c[0], "`")] = reason
	}
	key := regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[0-9]+\b|dispatch://`)
	for _, rule := range record.SlotRules() {
		if rule.Name == record.MainRule.Name {
			continue
		}
		reason, ok := reasons[rule.Name]
		added := !slices.Contains(specRules, rule.Name)
		switch {
		case !ok:
			t.Errorf("the trade against main holds no row of %s:\n%s", rule.Name, document[start:])
		case !added && reason != "":
			t.Errorf("the document gives the reason %q beside %s, a rule the spec names", reason, rule.Name)
		case added && reason == "":
			t.Errorf("the document gives no reason beside %s, a candidate added to the spec's rules", rule.Name)
		case key.MatchString(reason):
			t.Errorf("the reason beside %s names an issue key or a Dispatch link: %q", rule.Name, reason)
		}
	}
}

// A validation with MainOnly runs the main branch at f 1 alone (MainBranchAt), with the seeds of a
// branch with a draw (SeedsPerDraw), in place of every branch: the branch R5 judges a window by,
// at a fraction of the runs. Without it a validation runs every branch the tool offers (Branches),
// each with the same seeds; and a test's own plan replaces either.
func TestAMainOnlyValidationRunsTheMainBranchAlone(t *testing.T) {
	for _, run := range []struct {
		name     string
		cfg      ValidateConfig
		branches []BranchAt
		perDraw  int
	}{
		{"main only", ValidateConfig{MainOnly: true}, []BranchAt{MainBranchAt}, SeedsPerDraw},
		{"every branch", ValidateConfig{}, Branches(), SeedsPerDraw},
		{"a test's plan", ValidateConfig{MainOnly: true, plan: &runPlan{branches: Branches()[:2], perDraw: 1}}, Branches()[:2], 1},
	} {
		plan := validatePlan(run.cfg)
		if !slices.Equal(plan.branches, run.branches) || plan.perDraw != run.perDraw {
			t.Errorf("%s: the validation runs the branches %v with %d seeds for a draw, want %v with %d", run.name,
				branchNames(plan.branches), plan.perDraw, branchNames(run.branches), run.perDraw)
		}
	}
}

// branchNames is each of branches as a run's directory names it, `<branch>@<f>`.
func branchNames(branches []BranchAt) []string {
	names := make([]string, len(branches))
	for i, at := range branches {
		names[i] = fmt.Sprintf("%s@%v", at.Branch.Name, at.Slowdown)
	}
	return names
}

// The issues file of a comparison gives, for each window and each rule, each issue that held a slot
// in the record or on the main branch of the rule's replay, with its first slot and its done in the
// record and in the replay, each replayed time the median over the main branch's seeds, and the
// document names it. The comparison is made up: two windows recorded under main, each with the same
// record, compared under main and returned-by-first-slot on the main branch with three seeds. In
// the record, ACME-2 takes its first slot at 09:00 and is done at 11:00, ACME-1 at 09:30 and 12:00,
// and ACME-3 at 10:00 and not done. Each seed's times are below, in minutes from 09:00, and each
// median is worked by hand: a seed that gives none counts as later than every time, so ACME-1's
// done under main, given by one seed of three, is not done, and ACME-4's, given by two, is the
// middle seed's, 14:00. ACME-4, ACME-5 and ACME-6 hold no slot in the record and come last, ACME-6
// before ACME-4 by their replayed first slots; under returned-by-first-slot ACME-3 holds no slot,
// and ACME-4 and ACME-6 are in no row.
func TestTheIssuesFileGivesEachIssuesTimesUnderEachRule(t *testing.T) {
	const none = -1
	type seeds [3]int
	replays := map[string]struct{ firstSlot, doneAt map[string]seeds }{
		record.MainRule.Name: {
			firstSlot: map[string]seeds{"ACME-2": {0, 10, 20}, "ACME-1": {30, 30, 90}, "ACME-3": {60, 60, none}, "ACME-4": {200, 210, 220}, "ACME-6": {100, 100, 100}},
			doneAt:    map[string]seeds{"ACME-2": {120, 150, 130}, "ACME-1": {180, none, none}, "ACME-4": {300, 300, none}},
		},
		"returned-by-first-slot": {
			firstSlot: map[string]seeds{"ACME-2": {40, 40, 40}, "ACME-1": {0, 0, 0}, "ACME-5": {100, 100, none}},
			doneAt:    map[string]seeds{"ACME-2": {200, 200, 200}, "ACME-1": {100, 100, 100}},
		},
	}
	moment := func(minutes int) time.Time { return compareFrom.Add(time.Duration(minutes) * time.Minute) }
	times := func(byIssue map[string]seeds, seed int64) map[string]time.Time {
		out := map[string]time.Time{}
		for key, minutes := range byIssue {
			if m := minutes[seed-1]; m != none {
				out[key] = moment(m)
			}
		}
		return out
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)
	for i := range windows {
		windows[i].Stretches, windows[i].Conflicted = 2, 1
		windows[i].Scorecard.FirstSlot = map[string]time.Time{"ACME-2": moment(0), "ACME-1": moment(30), "ACME-3": moment(60)}
		windows[i].Scorecard.DoneAt = map[string]time.Time{"ACME-2": moment(120), "ACME-1": moment(180)}
	}
	rules := []record.SlotRule{replayedRule(t, record.MainRule.Name), replayedRule(t, "returned-by-first-slot")}
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, rules, runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 3},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			line.FirstSlot, line.DoneAt = times(replays[rule.Name].firstSlot, seed), times(replays[rule.Name].doneAt, seed)
			ran.Gaps = Gaps(snapshot, line)
			return snapshot, line, ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	issuesFile := IssuesFile(filepath.Join(dir, "comparison.md"))
	if want := filepath.Join(dir, "comparison-issues.md"); issuesFile != want {
		t.Errorf("the issues file beside comparison.md is %s, want %s", issuesFile, want)
	}
	if got := IssuesFile("comparison"); got != "comparison-issues.md" {
		t.Errorf("the issues file beside comparison is %s, want comparison-issues.md", got)
	}
	document, issues, err := runs.Comparison(issuesFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document, "`comparison-issues.md`, beside this document, gives for each window and each rule each issue") {
		t.Errorf("the document does not name the issues file and what it holds:\n%s", document)
	}
	stamp := func(minutes int) string { return moment(minutes).UTC().Format("2006-01-02T15:04Z") }
	row := func(issue, recordedSlot, replayedSlot, recordedDone, replayedDone string) string {
		return "| " + issue + " | " + recordedSlot + " | " + replayedSlot + " | " + recordedDone + " | " + replayedDone + " |"
	}
	want := map[string][]string{
		record.MainRule.Name: {
			row("ACME-2", stamp(0), stamp(10), stamp(120), stamp(130)),
			row("ACME-1", stamp(30), stamp(30), stamp(180), "not done"),
			row("ACME-3", stamp(60), stamp(60), "not done", "not done"),
			row("ACME-6", "no slot", stamp(100), "not done", "not done"),
			row("ACME-4", "no slot", stamp(210), "not done", stamp(300)),
		},
		"returned-by-first-slot": {
			row("ACME-2", stamp(0), stamp(40), stamp(120), stamp(200)),
			row("ACME-1", stamp(30), stamp(0), stamp(180), stamp(100)),
			row("ACME-3", stamp(60), "no slot", "not done", "not done"),
			row("ACME-5", "no slot", stamp(100), "not done", "not done"),
		},
	}
	head := tableHead("issue", []string{"first slot, recorded", "first slot, replayed", "done, recorded", "done, replayed"})
	for _, w := range windows {
		for _, rule := range rules {
			section := "## " + w.Name + " under `" + rule.Name + "` (seeds: 3)\n\n" + head + "\n" + strings.Join(want[rule.Name], "\n") + "\n"
			if !strings.Contains(issues, section) {
				t.Errorf("the issues file holds no section\n%s\nin:\n%s", section, issues)
			}
		}
	}
	t.Run("an even count of seeds", func(t *testing.T) {
		card := func(minutes int) Scorecard {
			if minutes == none {
				return Scorecard{}
			}
			return Scorecard{FirstSlot: map[string]time.Time{"ACME-1": moment(minutes)}}
		}
		firstSlot := func(c Scorecard) map[string]time.Time { return c.FirstSlot }
		if got := medianTime([]Scorecard{card(10), card(30)}, "ACME-1", firstSlot); !got.Equal(moment(20)) {
			t.Errorf("the median of 09:10 and 09:30 is %s, want 09:20", got)
		}
		if got := medianTime([]Scorecard{card(10), card(none)}, "ACME-1", firstSlot); !got.IsZero() {
			t.Errorf("the median of 09:10 and none is %s, want none", got)
		}
	})
}

// Beside R5's tests a validation counts, for each branch and seed, the first starts only one side
// made (the red team's finding 1): the issues the seed gave a first slot in the window that the
// record gave none, and those the record gave one that the seed gave none; it prints both in a
// table of their own, which tests nothing. The record gives ACME-1 and ACME-2 a first slot; seed 1
// gives ACME-1, ACME-3 and ACME-4 one, and seed 2 ACME-2 alone: seed 1 made two starts the record
// did not and missed one, and seed 2 made none and missed one.
func TestTheValidationCountsTheFirstStartsOnlyOneSideMade(t *testing.T) {
	recorded := sampleCard(compareHours)
	recorded.Source = "record"
	seed := func(n int64, keys ...string) Scorecard {
		card := sampleCard(compareHours)
		card.Seed, card.FirstSlot = n, map[string]time.Time{}
		for _, key := range keys {
			card.FirstSlot[key] = compareFrom.Add(time.Hour)
		}
		return card
	}
	branch := ValidateBranch(recorded, MainBranchAt, []Scorecard{seed(1, "ACME-1", "ACME-3", "ACME-4"), seed(2, "ACME-2")})
	if want := []SeedStarts{{Seed: 1, ReplayOnly: 2, RecordOnly: 1}, {Seed: 2, RecordOnly: 1}}; !slices.Equal(branch.Starts, want) {
		t.Errorf("the validation counts the starts %+v, want %+v", branch.Starts, want)
	}
	var out bytes.Buffer
	v := Validation{Window: "acme-window", Rule: "main", Record: recorded, Branches: []BranchValidation{branch}}
	if err := v.Write(&out, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| `main@1` | 1 | 2 | 1 |", "| `main@1` | 2 | 0 | 1 |"} {
		if !strings.Contains(out.String(), row) {
			t.Errorf("the validation prints no %q:\n%s", row, out.String())
		}
	}
}

// Beside R5's tests a validation names each tree whose hours of a closed design gate in the replay,
// the mean over the seeds, stand more than 30% from the record's (the red team's finding 5), and
// prints them, testing nothing. The record's gate of ACME-1 stands closed 2 hours and ACME-2's 1;
// the seeds close ACME-1's for 2.5 and 3 hours (2.75, 37.5% more), ACME-2's for 1 and 1.2 (1.1, 10%
// more), and one of them ACME-3's for half an hour, which the record never closes: ACME-1 and ACME-3
// are named, and ACME-2 is not.
func TestTheValidationNamesTheTreesWhoseGateHoursMoved(t *testing.T) {
	recorded := sampleCard(compareHours)
	recorded.Source, recorded.GateClosedHours = "record", map[string]float64{"ACME-1": 2, "ACME-2": 1}
	seeds := []Scorecard{sampleCard(compareHours), sampleCard(compareHours)}
	seeds[0].Seed, seeds[0].GateClosedHours = 1, map[string]float64{"ACME-1": 2.5, "ACME-2": 1, "ACME-3": 0.5}
	seeds[1].Seed, seeds[1].GateClosedHours = 2, map[string]float64{"ACME-1": 3, "ACME-2": 1.2}
	branch := ValidateBranch(recorded, MainBranchAt, seeds)
	want := []TreeGate{{Tree: "ACME-1", Recorded: 2, Replayed: 2.75, Low: 2.5, High: 3}, {Tree: "ACME-3", Replayed: 0.25, High: 0.5}}
	if !slices.Equal(branch.GateTrees, want) {
		t.Errorf("the validation names the trees %+v, want %+v", branch.GateTrees, want)
	}
	var out bytes.Buffer
	v := Validation{Window: "acme-window", Rule: "main", Record: recorded, Branches: []BranchValidation{branch}}
	if err := v.Write(&out, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| ACME-1 | 2 | 2.75 (2.5, 3) |", "| ACME-3 | 0 | 0.25 (0, 0.5) |"} {
		if !strings.Contains(out.String(), row) {
			t.Errorf("the validation prints no %q:\n%s", row, out.String())
		}
	}
	if strings.Contains(out.String(), "| ACME-2 |") {
		t.Errorf("the validation names ACME-2, whose replayed hours stand within 30%% of the record's:\n%s", out.String())
	}
}

// The document gives, beside Legion's done, the issues Dispatch shows done by anyone (the red team's
// finding 9): in each window's scorecard read from its record, and for each rule and branch read
// from each run's line timeline, below the measures the snapshot timeline gives. The comparison is
// made up: two windows under main, each record with 20 issues done by Legion and 25 by anyone, and
// runs whose line scorecards count 23 done by anyone.
func TestTheDocumentGivesDoneByAnyoneBesideLegionsDone(t *testing.T) {
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)
	for i := range windows {
		windows[i].Scorecard.DoneByAnyone = 25
	}
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, record.MainRule.Name)}, runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			line.DoneByAnyone = 23
			return snapshot, line, ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison("comparison-issues.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| done | 20 |", "| doneByAnyone | 25 |", "| doneByAnyone (the line timeline) | 23 |"} {
		if !strings.Contains(document, row) {
			t.Errorf("the document gives no %q:\n%s", row, document)
		}
	}
}

// A validation runs its diagnostic branches beside R5's tests (DiagnosticBranches): the plan of
// every validation holds them, with the main branch alone or with every branch, and a comparison's
// plan holds none. The validation read from the runs holds a diagnostic branch apart from its
// branches, so whether the window's main branch passes and which branches give a comparison never
// read it, and it prints the diagnostic's table as one that tests nothing and never stands in place
// of the main branch. The runs are made up: the main branch passes R5, and the diagnostic branch,
// whose issues finished stand at twice the record's, fails it.
func TestTheDiagnosticBranchStandsBesideTheValidation(t *testing.T) {
	for _, run := range []struct {
		name string
		cfg  ValidateConfig
	}{{"main only", ValidateConfig{MainOnly: true}}, {"every branch", ValidateConfig{}}} {
		if got := validatePlan(run.cfg).diagnostics; !slices.Equal(got, DiagnosticBranches()) {
			t.Errorf("%s: the validation runs the diagnostic branches %v, want %v", run.name, branchNames(got), branchNames(DiagnosticBranches()))
		}
	}
	if got := fullPlan().diagnostics; len(got) != 0 {
		t.Errorf("a comparison runs the diagnostic branches %v, want none", branchNames(got))
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)[:1]
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, record.MainRule.Name)},
		runPlan{branches: []BranchAt{MainBranchAt}, diagnostics: DiagnosticBranches(), perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			if at.Branch.recordedConflicts() {
				line.Done = 40
			}
			return snapshot, line, ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := runs.Validation(windows[0].Name, record.MainRule.Name)
	if err != nil {
		t.Fatal(err)
	}
	labels := func(branches []BranchValidation) []string {
		var out []string
		for _, b := range branches {
			out = append(out, b.Branch.Label())
		}
		return out
	}
	if got := labels(v.Branches); !slices.Equal(got, []string{MainBranchAt.Label()}) {
		t.Errorf("the validation's branches are %v, want the main branch alone", got)
	}
	if got := labels(v.Diagnostics); !slices.Equal(got, []string{"main+conflicts=recorded@1"}) {
		t.Fatalf("the validation's diagnostics are %v, want main+conflicts=recorded@1", got)
	}
	if v.Diagnostics[0].Pass() {
		t.Errorf("the diagnostic branch passes R5's tests, want it to fail its issues finished")
	}
	if !v.MainPasses() || len(v.NoComparison()) != 0 {
		t.Errorf("main passes %t and the branches that give no comparison are %v; the failing diagnostic branch moves neither", v.MainPasses(),
			v.NoComparison())
	}
	var out bytes.Buffer
	if err := v.Write(&out, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"## `main+conflicts=recorded@1`: a diagnostic, never a test of R5", "never in place of the main branch"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("the validation prints no %q:\n%s", line, out.String())
		}
	}
}

// priorityWorkWindow is a window from 09:00 to 14:00 under main at a cap of 2, its inputs reaching
// back to the daemon's boot at 08:00, whose issues are worked by hand. ACME-1, at P0, is filed todo
// and promoted at 09:00, and ACME-2, at P1, at 09:10; each works 15 minutes in each phase, gives its
// slot back at its accepting 75 minutes after its promotion and is done 2 hours after it. ACME-3, at
// P0, filed todo at 09:20, takes the slot ACME-1 gives back at 10:15, and ACME-4, at P1, filed todo
// at 09:40, the slot ACME-2 gives back at 10:25; each is then worked the same way, to its done at
// 12:15 and 12:25. ACME-5, at P2, filed todo at 09:50, and ACME-6, with no priority, filed todo at
// 09:55, are never promoted; and ACME-7, at P0, filed todo at 08:30, before the window, is never
// promoted either, its rank changing at 10:00 while it stays todo.
func priorityWorkWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(14, 0))
	b.capped(2)
	b.boot(clock(8, 0))
	level := func(n int) *int { return &n }
	filed := func(at time.Time, key, rank string, priority *int) {
		b.file(at, key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: rank, Labels: handedToLegion, Priority: priority})
	}
	worked := func(key string, promoted time.Time) {
		b.rootThroughReview(promoted, key)
		stepThrough(b, builtStep{promoted.Add(75 * time.Minute), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
			builtStep{promoted.Add(90 * time.Minute), key, phase.Accepting, phase.Retro, workflow.TriggerSignOff},
			builtStep{promoted.Add(105 * time.Minute), key, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
			builtStep{promoted.Add(120 * time.Minute), key, phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	}
	filed(clock(8, 30), "ACME-7", "a7", level(0))
	filed(clock(9, 0), "ACME-1", "a1", level(0))
	worked("ACME-1", clock(9, 0))
	filed(clock(9, 10), "ACME-2", "a2", level(1))
	worked("ACME-2", clock(9, 10))
	filed(clock(9, 20), "ACME-3", "a3", level(0))
	worked("ACME-3", clock(10, 15))
	filed(clock(9, 40), "ACME-4", "a4", level(1))
	worked("ACME-4", clock(10, 25))
	filed(clock(9, 50), "ACME-5", "a5", level(2))
	filed(clock(9, 55), "ACME-6", "a6", nil)
	b.edit(clock(10, 0), "ACME-7", func(issue *builtIssue) { issue.Rank = "a0" })
	return b.build()
}

// The comparison's document gives, for each window, the measures of its P0 issues and of its P1
// issues, in its record and under each rule: how many turned todo in the window with that priority,
// how many took a first slot at or after that first todo and how many reached done after it, and the
// median and the 90th percentile (nearest rank) of the hours from the first todo to each. The
// issues, their priorities and their first todos are read from the record's events, by one code
// for the record and the replay (traceMeasures).
//
// The record is priorityWorkWindow's, worked by hand. Its scorecard names ACME-1 (P0, 09:00),
// ACME-2 (P1, 09:10), ACME-3 (P0, 09:20), ACME-4 (P1, 09:40) and ACME-5 (P2, 09:50); not ACME-6,
// which has no priority, nor ACME-7, which turned todo before the window. In the record the P0
// issues take their first slot 0 and 55 minutes after their first todo (median 0.4583 hours, 90th
// percentile 0.9167) and are done 2 hours and 2 hours 55 minutes after it (2.4583, 2.9167); the P1
// issues 0 and 45 minutes (0.375, 0.75) and 2 hours and 2 hours 45 minutes (2.375, 2.75).
//
// The runs are made up, one seed of the main branch under each of two rules, both windows holding
// that record. Under main only ACME-1 takes a slot, at 09:00, and is done at 11:00, so the P0 issues
// have one time of each and the P1 issues none. Under p0-at-once ACME-1 and ACME-2 run as in the
// record, ACME-3 takes its slot at its first todo, 09:20, and is done at 11:20, and ACME-4 takes a
// slot at 09:30, before its first todo, which counts in no time, and is done at 12:25.
func TestTheComparisonGivesTheP0AndP1MeasuresOfEachRule(t *testing.T) {
	card, err := Score(priorityWorkWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	wantPriorities := map[string]IssuePriority{"ACME-1": {"P0", clock(9, 0)}, "ACME-2": {"P1", clock(9, 10)}, "ACME-3": {"P0", clock(9, 20)},
		"ACME-4": {"P1", clock(9, 40)}, "ACME-5": {"P2", clock(9, 50)}}
	if !maps.EqualFunc(card.Priorities, wantPriorities, func(a, b IssuePriority) bool { return a.Priority == b.Priority && a.FirstTodo.Equal(b.FirstTodo) }) {
		t.Errorf("the record's scorecard gives the priorities %+v, want %+v", card.Priorities, wantPriorities)
	}
	wantSlots := map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 10), "ACME-3": clock(10, 15), "ACME-4": clock(10, 25)}
	wantDone := map[string]time.Time{"ACME-1": clock(11, 0), "ACME-2": clock(11, 10), "ACME-3": clock(12, 15), "ACME-4": clock(12, 25)}
	if !maps.EqualFunc(card.FirstSlot, wantSlots, time.Time.Equal) || !maps.EqualFunc(card.DoneAt, wantDone, time.Time.Equal) {
		t.Fatalf("the record's scorecard gives the first slots %v and the done %v, want %v and %v", card.FirstSlot, card.DoneAt, wantSlots, wantDone)
	}
	recorded := func(times map[string]time.Time) func(string) time.Time {
		return func(key string) time.Time { return times[key] }
	}
	for priority, want := range map[string]PriorityMeasure{
		"P0": {Issues: 2, Slotted: 2, ToSlotMedian: 0.458333, ToSlotP90: 0.916667, Done: 2, ToDoneMedian: 2.458333, ToDoneP90: 2.916667},
		"P1": {Issues: 2, Slotted: 2, ToSlotMedian: 0.375, ToSlotP90: 0.75, Done: 2, ToDoneMedian: 2.375, ToDoneP90: 2.75},
	} {
		got := priorityMeasures(card.Priorities, priority, recorded(card.FirstSlot), recorded(card.DoneAt))
		if got.Issues != want.Issues || got.Slotted != want.Slotted || got.Done != want.Done {
			t.Errorf("the record's %s issues measure %+v, want %+v", priority, got, want)
		}
		for _, value := range []struct {
			name      string
			got, want float64
		}{{"to the first slot, median", got.ToSlotMedian, want.ToSlotMedian}, {"to the first slot, 90th percentile", got.ToSlotP90, want.ToSlotP90},
			{"to done, median", got.ToDoneMedian, want.ToDoneMedian}, {"to done, 90th percentile", got.ToDoneP90, want.ToDoneP90}} {
			assertNear(t, priority+" "+value.name, value.got, value.want)
		}
	}

	replays := map[string]struct{ firstSlot, doneAt map[string]time.Time }{
		record.MainRule.Name: {firstSlot: map[string]time.Time{"ACME-1": clock(9, 0)}, doneAt: map[string]time.Time{"ACME-1": clock(11, 0)}},
		"p0-at-once": {
			firstSlot: map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 10), "ACME-3": clock(9, 20), "ACME-4": clock(9, 30)},
			doneAt:    map[string]time.Time{"ACME-1": clock(11, 0), "ACME-2": clock(11, 10), "ACME-3": clock(11, 20), "ACME-4": clock(12, 25)},
		},
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)
	for i := range windows {
		windows[i].Scorecard.Priorities, windows[i].Scorecard.FirstSlot, windows[i].Scorecard.DoneAt = card.Priorities, card.FirstSlot, card.DoneAt
	}
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, record.MainRule.Name), replayedRule(t, "p0-at-once")},
		runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			line.FirstSlot, line.DoneAt = replays[rule.Name].firstSlot, replays[rule.Name].doneAt
			ran.Gaps = Gaps(snapshot, line)
			return snapshot, line, ran
		})
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison("comparison-issues.md")
	if err != nil {
		t.Fatal(err)
	}
	head := tableHead("priority", []string{"rule", "issues", "with a first slot", "to the first slot, median", "to the first slot, 90th percentile",
		"done", "to done, median", "to done, 90th percentile"})
	rows := []string{
		"| P0 | the record | 2 | 2 | 0.4583 | 0.9167 | 2 | 2.4583 | 2.9167 |",
		"| P0 | `main` | 2 | 1 | 0 | 0 | 1 | 2 | 2 |",
		"| P0 | `p0-at-once` | 2 | 2 | 0 | 0 | 2 | 2 | 2 |",
		"| P1 | the record | 2 | 2 | 0.375 | 0.75 | 2 | 2.375 | 2.75 |",
		"| P1 | `main` | 2 | 0 | none | none | 0 | none | none |",
		"| P1 | `p0-at-once` | 2 | 1 | 0 | 0 | 2 | 2.375 | 2.75 |",
	}
	if !strings.Contains(document, "## P0 and P1 work under each rule\n") {
		t.Errorf("the document holds no part of P0 and P1 work:\n%s", document)
	}
	for _, w := range windows {
		section := "### " + w.Name + "\n\n" + head + "\n" + strings.Join(rows, "\n") + "\n"
		if !strings.Contains(document, section) {
			t.Errorf("the document holds no section\n%s\nin:\n%s", section, document)
		}
	}
}
