package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
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
// ACME-2 with a first slot half an hour on, ACME-3 with none; it gives no urgent work, files no
// issue, and holds the readings only a line timeline gives, a daily maximum of each count among
// them.
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
		PriorityTakenBanded:  0.2,
		Loops: map[string]LoopHours{string(CauseReviewChanges): {Rounds: 4, Hours: 10}, string(CauseConflict): {Rounds: 2, Hours: 10},
			string(CauseSendBack): {Rounds: 3, Hours: 6}, string(CauseTestFailed): {Rounds: 2, Hours: 4}},
		Waits:           map[string]WaitHours{string(WaitLinkRemoved): {Count: 2, Hours: 8}, string(WaitOpen): {Count: 1, Hours: 3}},
		SlotHoursInTurn: 10, SlotHoursNoTurn: 5,
		InFlight:              Occupancy{Average: 6, Max: 8, Hours: 30},
		SlotsGivenByDay:       map[string]int{date: 10},
		MedianSlotWaitMinutes: 10,
		FirstSlot:             map[string]time.Time{"ACME-1": compareFrom, "ACME-2": compareFrom.Add(30 * time.Minute)},
		DoneAt:                map[string]time.Time{"ACME-1": compareFrom.Add(2 * time.Hour)},
		UrgentWaits:           map[string]UrgentWait{},
		Filed:                 map[string]time.Time{},
		SlotsGiven:            []time.Time{},
		DailyMaxima:           map[string]float64{"waitOnlyForSlot": 4, "startedWithoutSlot": 5, "pastReviewHolding": 2, "pastReviewNotHolding": 3, "inFlight": 8},
		UrgentMeans:           map[string]UrgentMean{},
		Assumptions:           []int{},
		Scenario:              MainBranch.NeverStarted,
	}
}

// Each gap of R5 passes a replay at its bound and fails one past it (spec R5): 10% of the issues
// finished and of the slots given out on each day, 30% of the median wait for a slot, of the hours
// of started work that waits only for a slot and of every other measure, 0.05 of the share of
// priority taken, and 60 minutes at the median of each issue's first-slot error; a measure the
// record lacks stands at 0 there, so any replayed value of it fails; and the numbers R5 tests not —
// the issues the start state could not place, the first day apart, and priority taken in today's
// line's order (priorityTakenBanded), which is not one of R5's tests — fail no branch.
func TestValidationGaps(t *testing.T) {
	t.Parallel()
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
		replay.PriorityTakenBanded = 0.9
		first := sampleCard(compareHours)
		first.Done = 100
		replay.FirstDayApart = &first
		got := ValidateBranch(recorded, MainBranchAt, []Scorecard{replay})
		if !got.Pass() {
			t.Errorf("a replay that differs from the record only in its unplaced issues, its priority taken in today's line's order and its first day apart fails %v",
				got.Failed)
		}
		shown := false
		for _, check := range got.Measures {
			shown = shown || check.Measure == "priorityTakenBanded"
			if (check.Measure == "unplaced" || check.Measure == "priorityTakenBanded" || strings.HasPrefix(check.Measure, firstDayPrefix)) && check.Tested {
				t.Errorf("the validation tests %s, which no gap of R5 names", check.Measure)
			}
		}
		if !shown {
			t.Errorf("the validation does not show priorityTakenBanded")
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
	t.Parallel()
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
	t.Parallel()
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

// A forecast across a recorded change compares rates per hour, averages and shares, never totals,
// each within twice its gap (spec R5): a window half as long with half the work forecasts the same
// rates, and each kind passes at twice its gap and fails past it.
func TestForecastScoresRatesNeverTotals(t *testing.T) {
	t.Parallel()
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
	checks := forecastAgainst(half(), after, half())
	for _, c := range checks {
		if !slices.Contains(kinds, c.Kind) {
			t.Errorf("the forecast scores %s as %q, which is no rate per hour, average or share", c.Quantity, c.Kind)
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
		check := checkOf(forecastAgainst(half(), after, atBound), b.name)
		if !check.Pass || check.Kind != b.kind || check.Gap.String() != b.want {
			t.Errorf("%s at twice its gap: pass %t, kind %s, gap %s; want a pass, %s, %s", b.name, check.Pass, check.Kind, check.Gap, b.kind, b.want)
		}
		if check := checkOf(forecastAgainst(half(), after, past), b.name); check.Pass {
			t.Errorf("%s past twice its gap passes (%v against %v)", b.name, check.Forecast, check.After)
		}
	}
}

// forecastAgainst is the checks of the forecast of after by forecast, one seed, from the window
// before whose record and whose replay under its own rule are both before, and whose window after's
// replay under its own rule is its record, so the replay's own error is 0 on every measure.
func forecastAgainst(before, after, forecast Scorecard) []ForecastCheck {
	return ForecastChecks(ForecastCards{Before: before, After: after, Forecasts: []Scorecard{forecast}, Own: []Scorecard{before}, AfterOwn: []Scorecard{after}})
}

// A forecast scores the causes the workflow change redefines only inside the total of all
// send-back loops and of the redefined causes together, and each cause both releases define the
// same way apart, and names each cause in each group (plan D3): a forecast whose loops are all red
// checks where the window after had conflicts and send-backs passes on both totals, holds no number
// of a redefined cause apart, and fails a cause defined the same way that it misses.
func TestForecastGroupsTheRedefinedCauses(t *testing.T) {
	t.Parallel()
	after := sampleCard(compareHours)
	after.Loops = map[string]LoopHours{string(CauseConflict): {Rounds: 10, Hours: 50}, string(CauseSendBack): {Rounds: 10, Hours: 50},
		string(CauseReviewChanges): {Rounds: 5, Hours: 20}, string(CauseTestFailed): {Rounds: 5, Hours: 10}}
	forecast := sampleCard(compareHours)
	forecast.Loops = map[string]LoopHours{string(CauseRedChecksOrConflict): {Rounds: 20, Hours: 100},
		string(CauseReviewChanges): {Rounds: 5, Hours: 20}, string(CauseTestFailed): {Rounds: 5, Hours: 10}}
	checks := forecastAgainst(forecast, after, forecast)
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
	for name, causes := range map[string][]Cause{LoopsAll + ".rounds per hour": Causes, LoopsAll + ".hours per hour": Causes,
		LoopsRedefined + ".rounds per hour": ForecastGroups[GroupRedefined], LoopsRedefined + ".hours per hour": ForecastGroups[GroupRedefined]} {
		i := slices.IndexFunc(checks, func(c ForecastCheck) bool { return c.Quantity == name })
		if i < 0 || !slices.Equal(checks[i].Causes, causes) {
			t.Errorf("the forecast holds no %s over the causes %v", name, causes)
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
	missedChecks := forecastAgainst(forecast, after, missed)
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
		"The causes the workflow change redefines (redefined): `red_checks_or_conflict`, `conflict`, `send_back`; each is scored only inside the total of all send-back loops, `loops[all]`, and of the redefined causes together, `loops[redefined]`.",
		"The causes both releases define the same way (same): `review_changes`, `test_failed`, `queue_drop`, `other_backward`; each is scored apart",
		"The forecast also carries the workflow commits between `1111111111111111111111111111111111111111` and `2222222222222222222222222222222222222222`",
	} {
		if !strings.Contains(out.String(), sentence) {
			t.Errorf("the forecast prints no %q:\n%s", sentence, out.String())
		}
	}
}

// judgedMain is main's costs at each seed in the tests of the decision: 100 issues finished, 50
// hours of started work waiting only for a slot, 0.2 of priority taken, 400 hours of other delays,
// and P0 work that waits 2 hours to its first slot at the median and 6 at the 90th percentile, and
// 10 and 20 hours to its done; no P1 issue counts, so each wait of P1 work is none.
func judgedMain() Costs {
	return Costs{Done: 100, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400, UrgentWaits: map[string]UrgentWait{
		"P0": {Issues: 3, FirstSlot: WaitMinutes{MedianMinutes: 120, P90Minutes: 360}, Done: WaitMinutes{MedianMinutes: 600, P90Minutes: 1200}}}}
}

// judgedErrors is the replay's own error across the change on each measure in the tests of the
// decision: 2 issues finished, 5 hours of started work waiting only for a slot, 0.02 of priority
// taken, 10 hours of other delays and half an hour on each wait of P0 work. None is measured on the
// waits of P1 work, which no issue gives.
var judgedErrors = map[string]float64{"done": 2, "waitOnlyForSlot.hours": 5, "priorityTakenBanded": 0.02, "otherDelays.hours": 10,
	"urgentWaits[P0].firstSlot.medianHours": 0.5, "urgentWaits[P0].firstSlot.p90Hours": 0.5, "urgentWaits[P0].done.medianHours": 0.5,
	"urgentWaits[P0].done.p90Hours": 0.5}

// fiveOffsets is an offset for each of the seeds 1 to 5, of mean 0 and of seed-by-seed standard
// deviation 1: ((-1)² + (-1)² + 0² + 1² + 1²) / 4 = 1. A rule whose measure is main's plus d plus
// k times each seed's offset differs from main by d with s = k, and with Student's t at 4 degrees of
// freedom, 2.7764, its 95% interval is d ± 2.7764 k / √5 = d ± 1.2417 k.
var fiveOffsets = []float64{-1, -1, 0, 1, 1}

// atSeeds is base's costs at the seeds 1 to len(offsets), each moved by change with its seed's
// offset (by nothing where change is nil); each seed's costs are made afresh by base, so a change
// to the wait of urgent work of one seed changes no other's.
func atSeeds(base func() Costs, offsets []float64, change func(c *Costs, offset float64)) SeedCosts {
	costs := SeedCosts{}
	for i, offset := range offsets {
		c := base()
		if change != nil {
			change(&c, offset)
		}
		costs[int64(i+1)] = c
	}
	return costs
}

// doneAhead is judgedMain's costs at five seeds with d more issues finished, moved by each seed's
// offset (fiveOffsets): a difference of d from main, s 1.
func doneAhead(d float64) SeedCosts {
	return atSeeds(judgedMain, fiveOffsets, func(c *Costs, offset float64) { c.Done += d + offset })
}

// decisionBranches is the four branches of the tests of the decision, the main branch at f 1 and at
// f 1.6 of the windows one and two, each passing R5 as passes says, with main at main's costs and
// each of rules at its own.
func decisionBranches(passes [4]bool, main SeedCosts, rules map[string]SeedCosts) []BranchCosts {
	slower := BranchAt{Branch: MainBranch, Slowdown: 1.6}
	var branches []BranchCosts
	for i, place := range []struct {
		window string
		at     BranchAt
	}{{"one", MainBranchAt}, {"one", slower}, {"two", MainBranchAt}, {"two", slower}} {
		costs := map[string]SeedCosts{record.MainRule.Name: main}
		maps.Copy(costs, rules)
		branches = append(branches, BranchCosts{Window: place.window, Branch: place.at, Passes: passes[i], Costs: costs})
	}
	return branches
}

// allPass is the four branches of decisionBranches each passing R5.
var allPass = [4]bool{true, true, true, true}

// decideAmong is the decision over branches of main and each rule names names, each the rule
// SlotRules keeps under it, each difference held to judgedErrors.
func decideAmong(t *testing.T, names []string, branches []BranchCosts) Decision {
	t.Helper()
	rules := []record.SlotRule{replayedRule(t, record.MainRule.Name)}
	for _, name := range names {
		rules = append(rules, replayedRule(t, name))
	}
	return Decide(rules, branches, judgedErrors)
}

// measureTrade is rule's trade on the measure named measure in d, which fails t where d gives none.
func measureTrade(t *testing.T, d Decision, rule, measure string) MeasureTrade {
	t.Helper()
	i := slices.IndexFunc(d.Trades, func(tr Trade) bool { return tr.Rule == rule })
	if i < 0 {
		t.Fatalf("the decision gives no trade of %s", rule)
	}
	j := slices.IndexFunc(d.Trades[i].Measures, func(m MeasureTrade) bool { return m.Measure == measure })
	if j < 0 {
		t.Fatalf("the trade of %s gives no measure %s: %+v", rule, measure, d.Trades[i].Measures)
	}
	return d.Trades[i].Measures[j]
}

// judgedOn is rule's difference from main on the measure named measure on the branch at of window
// in d, which fails t where d judged none there.
func judgedOn(t *testing.T, d Decision, window string, at BranchAt, rule, measure string) Difference {
	t.Helper()
	for _, j := range d.Judged {
		if j.Window != window || j.Branch.Label() != at.Label() || j.Rule != rule {
			continue
		}
		for _, diff := range j.Differences {
			if diff.Measure == measure {
				return diff
			}
		}
	}
	t.Fatalf("the decision judges %s on %s of %s on %s nowhere", rule, measure, window, at.Label())
	return Difference{}
}

// closeTo says whether got is want to the four decimals a document prints.
func closeTo(got, want float64) bool { return math.Abs(got-want) < 5e-5 }

// A comparison names a rule best only when it keeps the owner's direction and, on every branch of
// both windows that passes R5, is worse than main on no measure and better on at least one, each
// difference taken seed by seed (spec R7). Main's costs at each of five seeds are judgedMain's, and
// the replay's own error on the issues finished is 2 (judgedErrors). A rule that finishes d more
// issues, moved at each seed by its offset (doneAhead), differs from main by d with s = 1, and its
// 95% interval is d ± 1.2417 (fiveOffsets): at d = 3 it is [1.7583, 4.2417], which excludes zero
// but whose near end is under the error of 2, so the difference is a tie; at d = 4 it is [2.7583,
// 5.2417], past the error, so it is better. Every other measure ties, d = 0, and the waits of P1
// work, which no seed gives, are none.
func TestBestRuleDecision(t *testing.T) {
	t.Parallel()
	main := atSeeds(judgedMain, fiveOffsets, nil)
	t.Run("an interval past zero whose near end is under the error is a tie", func(t *testing.T) {
		t.Parallel()
		d := decideAmong(t, []string{"signals", "signals-backoff"}, decisionBranches(allPass, main,
			map[string]SeedCosts{"signals": doneAhead(3), "signals-backoff": doneAhead(4)}))
		if !slices.Equal(d.Best, []string{"signals-backoff"}) || d.Branches != 4 {
			t.Errorf("the decision names %v best over %d branches, want [signals-backoff] over 4", d.Best, d.Branches)
		}
		tie := judgedOn(t, d, "one", MainBranchAt, "signals", "done")
		if tie.Seeds != 5 || !closeTo(tie.Mean, 3) || !closeTo(tie.SD, 1) || !closeTo(tie.Low, 1.758336) || !closeTo(tie.High, 4.241664) || tie.Verdict != VerdictTie {
			t.Errorf("signals' difference on done is %+v, want d 3, s 1 over 5 seeds, the interval [1.7583, 4.2417] and a tie", tie)
		}
		if better := judgedOn(t, d, "one", MainBranchAt, "signals-backoff", "done"); !closeTo(better.Low, 2.758336) || better.Verdict != VerdictBetter {
			t.Errorf("signals-backoff's difference on done is %+v, want the interval from 2.7583 and better", better)
		}
		for rule, want := range map[string]MeasureTrade{"signals": {Measure: "done", Tie: 4}, "signals-backoff": {Measure: "done", Better: 4}} {
			if got := measureTrade(t, d, rule, "done"); got != want {
				t.Errorf("%s's trade on done is %+v, want %+v", rule, got, want)
			}
		}
	})
	t.Run("a rule that breaks the direction is not named", func(t *testing.T) {
		t.Parallel()
		d := decideAmong(t, []string{"cover", "signals"}, decisionBranches(allPass, main,
			map[string]SeedCosts{"cover": doneAhead(4), "signals": doneAhead(4)}))
		if !slices.Equal(d.Best, []string{"signals"}) || !slices.Equal(d.Outside, []string{"cover"}) {
			t.Errorf("the decision names %v best and %v outside the owner's direction, want [signals] and [cover]", d.Best, d.Outside)
		}
	})
	t.Run("none best, with each trade", func(t *testing.T) {
		t.Parallel()
		slower := atSeeds(judgedMain, fiveOffsets, func(c *Costs, offset float64) {
			c.Done, c.WaitOnlyForSlot = c.Done+4+offset, c.WaitOnlyForSlot+10+offset
		})
		d := decideAmong(t, []string{"signals", "signals-backoff"}, decisionBranches(allPass, main,
			map[string]SeedCosts{"signals": slower, "signals-backoff": doneAhead(-4)}))
		if len(d.Best) != 0 || len(d.Outside) != 0 {
			t.Errorf("the decision names %v best and %v outside the direction, though each rule is worse than main on one measure", d.Best, d.Outside)
		}
		for _, want := range []struct {
			rule  string
			trade MeasureTrade
		}{
			{"signals", MeasureTrade{Measure: "done", Better: 4}},
			{"signals", MeasureTrade{Measure: "waitOnlyForSlot.hours", Worse: 4}},
			{"signals", MeasureTrade{Measure: "priorityTakenBanded", Tie: 4}},
			{"signals", MeasureTrade{Measure: "otherDelays.hours", Tie: 4}},
			{"signals", MeasureTrade{Measure: "urgentWaits[P0].firstSlot.medianHours", Tie: 4}},
			{"signals", MeasureTrade{Measure: "urgentWaits[P1].done.p90Hours", None: 4}},
			{"signals-backoff", MeasureTrade{Measure: "done", Worse: 4}},
			{"signals-backoff", MeasureTrade{Measure: "waitOnlyForSlot.hours", Tie: 4}},
		} {
			if got := measureTrade(t, d, want.rule, want.trade.Measure); got != want.trade {
				t.Errorf("%s's trade is %+v, want %+v", want.rule, got, want.trade)
			}
		}
	})
}

// A tie gives the seeds a difference of its size would need, ⌈(1.96 s / d)²⌉, and never where d is
// 0 (spec R7). Over five seeds whose offsets give s = 1 (fiveOffsets), the interval is d ± 1.2417:
// at d = 0.5 it is [-0.7417, 1.7417], a tie, which would need ⌈(1.96 / 0.5)²⌉ = ⌈15.3664⌉ = 16
// seeds; at d = 0.25 it is [-0.9917, 1.4917] and needs ⌈(7.84)²⌉ = ⌈61.4656⌉ = 62; at d = 0 it is
// [-1.2417, 1.2417] and needs never; and at d = 3, [1.7583, 4.2417], its interval excludes zero
// already, and it ties only because its near end is under the error of 2: the formula gives
// ⌈(1.96 / 3)²⌉ = ⌈0.4268⌉ = 1 seed, which tells it apart from zero and not from the error.
func TestSeedsNeededForATie(t *testing.T) {
	t.Parallel()
	main := atSeeds(judgedMain, fiveOffsets, nil)
	quantile := studentQuantiles()
	for _, tc := range []struct {
		d, low, high, needed float64
		never                bool
		cell                 string
	}{
		{0.5, -0.741664, 1.741664, 16, false, "0.5 [-0.7417, 1.7417] over 5 seeds: tie, 16 seeds needed"},
		{0.25, -0.991664, 1.491664, 62, false, "0.25 [-0.9917, 1.4917] over 5 seeds: tie, 62 seeds needed"},
		{0, -1.241664, 1.241664, 0, true, "0 [-1.2417, 1.2417] over 5 seeds: tie, never"},
		{3, 1.758336, 4.241664, 1, false, "3 [1.7583, 4.2417] over 5 seeds: tie, 1 seed needed"},
	} {
		diff := difference(costMeasures[0], doneAhead(tc.d), main, judgedErrors, quantile)
		if diff.Verdict != VerdictTie || !closeTo(diff.Low, tc.low) || !closeTo(diff.High, tc.high) || diff.SeedsNeeded != tc.needed || diff.Never != tc.never {
			t.Errorf("at d = %v the difference is %+v, want a tie over [%v, %v] that needs %v seeds (never: %t)", tc.d, diff, tc.low, tc.high, tc.needed, tc.never)
		}
		if got := diff.cell(); got != tc.cell {
			t.Errorf("at d = %v the document gives %q, want %q", tc.d, got, tc.cell)
		}
	}
}

// The 95% interval of a paired difference reads the 0.975 quantile of Student's t at n − 1 degrees
// of freedom (spec R7): 12.7062 at one, 4.3027 at two, 2.7764 at four (the five seeds of a branch
// with a draw), 2.2281 at ten, 2.0423 at thirty and 1.9623 at a thousand, toward the normal 1.96.
func TestThePairedIntervalReadsStudentsQuantile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		df   int
		want float64
	}{{1, 12.7062}, {2, 4.3027}, {4, 2.7764}, {10, 2.2281}, {30, 2.0423}, {1000, 1.9623}} {
		if got := studentQuantile(tc.df); math.Abs(got-tc.want) > 1e-4 {
			t.Errorf("Student's 0.975 quantile at %d degrees of freedom is %v, want %v", tc.df, got, tc.want)
		}
	}
}

// The wait of urgent work is judged as every other measure (spec R4, R7): for P0 and for P1 apart,
// the median and the 90th percentile of the wait to the first slot and to done, in hours, each a
// measure. A rule 4 issues ahead of main (better, as TestBestRuleDecision works out) whose P0 work
// waits 2 hours longer at the median to its first slot, plus each seed's offset (fiveOffsets, s =
// 1), has the interval [0.7583, 3.2417] there, past the error of half an hour: it is worse, a trade,
// and is not named, where the same rule with main's waits is. A wait no seed gives, P1's here, is
// none, neither better nor worse.
func TestTheUrgentWaitsAreJudged(t *testing.T) {
	t.Parallel()
	var want []string
	for _, priority := range []string{"P0", "P1"} {
		for _, wait := range []string{"firstSlot", "done"} {
			for _, at := range []string{"medianHours", "p90Hours"} {
				want = append(want, "urgentWaits["+priority+"]."+wait+"."+at)
			}
		}
	}
	want = slices.Concat([]string{"done", "waitOnlyForSlot.hours", "priorityTakenBanded", "otherDelays.hours"}, want)
	if got := costMeasureNames(); !slices.Equal(got, want) {
		t.Errorf("a rule is judged on %v, want %v", got, want)
	}
	slower := atSeeds(judgedMain, fiveOffsets, func(c *Costs, offset float64) {
		c.Done += 4 + offset
		wait := c.UrgentWaits["P0"]
		wait.FirstSlot.MedianMinutes += 60 * (2 + offset)
		c.UrgentWaits["P0"] = wait
	})
	d := decideAmong(t, []string{"signals", "signals-backoff"}, decisionBranches(allPass, atSeeds(judgedMain, fiveOffsets, nil),
		map[string]SeedCosts{"signals": slower, "signals-backoff": doneAhead(4)}))
	if !slices.Equal(d.Best, []string{"signals-backoff"}) {
		t.Errorf("the decision names %v best, want signals-backoff alone: signals' P0 work waits longer for its first slot", d.Best)
	}
	worse := judgedOn(t, d, "two", MainBranchAt, "signals", "urgentWaits[P0].firstSlot.medianHours")
	if !closeTo(worse.Mean, 2) || !closeTo(worse.Low, 0.758336) || !closeTo(worse.High, 3.241664) || worse.Verdict != VerdictWorse {
		t.Errorf("signals' difference on the median wait of P0 work to its first slot is %+v, want d 2 over [0.7583, 3.2417], worse", worse)
	}
	for measure, want := range map[string]MeasureTrade{
		"urgentWaits[P0].firstSlot.medianHours": {Measure: "urgentWaits[P0].firstSlot.medianHours", Worse: 4},
		"urgentWaits[P0].done.p90Hours":         {Measure: "urgentWaits[P0].done.p90Hours", Tie: 4},
		"urgentWaits[P1].firstSlot.medianHours": {Measure: "urgentWaits[P1].firstSlot.medianHours", None: 4},
	} {
		if got := measureTrade(t, d, "signals", measure); got != want {
			t.Errorf("signals' trade on %s is %+v, want %+v", measure, got, want)
		}
	}
}

// Only a rule that keeps the owner's direction can be named best (spec R7), even where another is
// better than main on every measure on every branch: cover, which starts a P0 issue outside the
// cap as main does, and before-p0, each better on each measure a seed gives — 4 more issues
// finished, 10 fewer hours waiting only for a slot, 0.1 less priority taken (s 0.01), 30 fewer
// hours of other delays and every wait of P0 work an hour shorter (s 0.1), each interval's near
// end past its error — are measured beside it (Outside), and signals, as good, is named. The rule
// value decides, not its name: signals with the line's priority order off is not named.
func TestARuleOutsideTheOwnersDirectionIsNeverNamed(t *testing.T) {
	t.Parallel()
	better := atSeeds(judgedMain, fiveOffsets, func(c *Costs, offset float64) {
		c.Done += 4 + offset
		c.WaitOnlyForSlot += -10 + offset
		c.PriorityTaken += -0.1 + 0.01*offset
		c.OtherDelays += -30 + offset
		wait := c.UrgentWaits["P0"]
		for _, minutes := range []*float64{&wait.FirstSlot.MedianMinutes, &wait.FirstSlot.P90Minutes, &wait.Done.MedianMinutes, &wait.Done.P90Minutes} {
			*minutes -= 60 * (1 - 0.1*offset)
		}
		c.UrgentWaits["P0"] = wait
	})
	main := atSeeds(judgedMain, fiveOffsets, nil)
	names := []string{"cover", "before-p0", "signals"}
	costs := map[string]SeedCosts{}
	for _, name := range names {
		costs[name] = better
	}
	d := decideAmong(t, names, decisionBranches(allPass, main, costs))
	if !slices.Equal(d.Best, []string{"signals"}) || !slices.Equal(d.Outside, []string{"cover", "before-p0"}) {
		t.Errorf("the decision names %v best and %v outside the owner's direction, want [signals] and [cover before-p0]", d.Best, d.Outside)
	}
	for _, measure := range []string{"done", "waitOnlyForSlot.hours", "priorityTakenBanded", "otherDelays.hours", "urgentWaits[P0].firstSlot.medianHours",
		"urgentWaits[P0].firstSlot.p90Hours", "urgentWaits[P0].done.medianHours", "urgentWaits[P0].done.p90Hours"} {
		if got := measureTrade(t, d, "cover", measure); got.Better != 4 {
			t.Errorf("cover's trade on %s is %+v, want better on all 4 branches", measure, got)
		}
	}
	unordered := replayedRule(t, "signals")
	unordered.PriorityFirst = false
	if d := Decide([]record.SlotRule{record.MainRule, unordered}, decisionBranches(allPass, main, map[string]SeedCosts{"signals": better}), judgedErrors); len(d.Best) != 0 {
		t.Errorf("signals with the line's priority order off is named best (%v)", d.Best)
	}
}

// A pair of runs that draw nothing, one seed each, stands alone (spec R7): its interval is the
// difference itself, which counts as better or worse only where it is farther from zero than the
// replay's own error, and the document says it is a single run. With the error on the issues
// finished at 2, a single run 3 issues ahead of main's is better, 2 ahead, at the error itself, and
// 1 ahead are ties, 0 ahead is a tie no seed tells apart, and 3 behind is worse; a rule 3 ahead on
// each of four branches of one seed is named.
func TestASingleRunPairIsJudgedAgainstTheErrorAlone(t *testing.T) {
	t.Parallel()
	main := atSeeds(judgedMain, []float64{0}, nil)
	ahead := func(d float64) SeedCosts {
		return atSeeds(judgedMain, []float64{0}, func(c *Costs, _ float64) { c.Done += d })
	}
	quantile := studentQuantiles()
	for _, tc := range []struct {
		d    float64
		want Verdict
		cell string
	}{
		{3, VerdictBetter, "3, a single run: better"},
		{2, VerdictTie, "2, a single run: tie"},
		{1, VerdictTie, "1, a single run: tie"},
		{0, VerdictTie, "0, a single run: tie, never"},
		{-3, VerdictWorse, "-3, a single run: worse"},
	} {
		diff := difference(costMeasures[0], ahead(tc.d), main, judgedErrors, quantile)
		if diff.Seeds != 1 || diff.Verdict != tc.want || diff.Low != tc.d || diff.High != tc.d {
			t.Errorf("a single run %v ahead differs by %+v, want one seed, the interval [%v, %v] and %s", tc.d, diff, tc.d, tc.d, tc.want)
		}
		if got := diff.cell(); got != tc.cell {
			t.Errorf("a single run %v ahead reads %q, want %q", tc.d, got, tc.cell)
		}
	}
	if d := decideAmong(t, []string{"signals"}, decisionBranches(allPass, main, map[string]SeedCosts{"signals": ahead(3)})); !slices.Equal(d.Best, []string{"signals"}) {
		t.Errorf("a rule a single run 3 issues ahead on every branch is not named (%v)", d.Best)
	}
}

// A branch that fails R5 is read by nothing (spec R5, R7): a rule better only on a branch that
// fails is not named, its trade counts only the branches that pass, and no difference is judged on
// the failing branch; a rule worse only on a branch that fails is named; and with no branch that
// passes no rule is named.
func TestABranchThatFailsR5IsReadByNothing(t *testing.T) {
	t.Parallel()
	main := atSeeds(judgedMain, fiveOffsets, nil)
	failsSecond := [4]bool{true, false, true, true}
	betterOnFailed := decisionBranches(failsSecond, main, map[string]SeedCosts{"signals": main})
	betterOnFailed[1].Costs = map[string]SeedCosts{record.MainRule.Name: main, "signals": doneAhead(20)}
	d := decideAmong(t, []string{"signals"}, betterOnFailed)
	if len(d.Best) != 0 || d.Branches != 3 {
		t.Errorf("a rule better only on a branch that fails R5 is named best (%v over %d branches)", d.Best, d.Branches)
	}
	if got := measureTrade(t, d, "signals", "done"); got != (MeasureTrade{Measure: "done", Tie: 3}) {
		t.Errorf("signals' trade on done is %+v, want a tie on the 3 branches that pass and nothing of the one that fails", got)
	}
	for _, j := range d.Judged {
		if j.Window == betterOnFailed[1].Window && j.Branch.Label() == betterOnFailed[1].Branch.Label() {
			t.Errorf("the decision judges %s on %s of %s, which fails R5", j.Rule, j.Branch.Label(), j.Window)
		}
	}
	worseOnFailed := decisionBranches(failsSecond, main, map[string]SeedCosts{"signals": doneAhead(4)})
	worseOnFailed[1].Costs = map[string]SeedCosts{record.MainRule.Name: main, "signals": doneAhead(-20)}
	if d := decideAmong(t, []string{"signals"}, worseOnFailed); !slices.Equal(d.Best, []string{"signals"}) {
		t.Errorf("a rule better on every branch that passes R5 is not named best (%v): a branch that failed is read", d.Best)
	}
	if d := decideAmong(t, []string{"signals"}, decisionBranches([4]bool{}, main, map[string]SeedCosts{"signals": doneAhead(20)})); len(d.Best) != 0 || d.Branches != 0 || len(d.Judged) != 0 {
		t.Errorf("with no branch that passes R5 the decision names %v best over %d branches, judging %d", d.Best, d.Branches, len(d.Judged))
	}
}

// On a branch whose runs are not paired (BranchCosts.Unpaired: its window lists a workflow
// experiment in the random mode) every difference a seed gives is not paired, neither better nor
// worse nor a tie, with its mean over the seeds and no interval (spec R7). Signals, 4 issues ahead
// of main with s = 1 (doneAhead), is better on window one's two branches and not paired on window
// two's, so it is not named; its trade on done is better on 2 and not paired on 2, and the waits of
// P1 work, which no seed gives, stay none.
func TestAnUnpairedBranchJudgesNoRule(t *testing.T) {
	t.Parallel()
	branches := decisionBranches(allPass, atSeeds(judgedMain, fiveOffsets, nil), map[string]SeedCosts{"signals": doneAhead(4)})
	for i := range branches {
		if branches[i].Window == "two" {
			branches[i].Unpaired = "two lists the experiment `review-depth` in the random mode"
		}
	}
	d := decideAmong(t, []string{"signals"}, branches)
	if len(d.Best) != 0 || len(d.Outside) != 0 {
		t.Errorf("the decision names %v best and %v outside the direction, though no difference on window two is paired", d.Best, d.Outside)
	}
	for measure, want := range map[string]MeasureTrade{
		"done":                                  {Measure: "done", Better: 2, Unpaired: 2},
		"waitOnlyForSlot.hours":                 {Measure: "waitOnlyForSlot.hours", Tie: 2, Unpaired: 2},
		"urgentWaits[P1].firstSlot.medianHours": {Measure: "urgentWaits[P1].firstSlot.medianHours", None: 4},
	} {
		if got := measureTrade(t, d, "signals", measure); got != want {
			t.Errorf("signals' trade on %s is %+v, want %+v", measure, got, want)
		}
	}
	diff := judgedOn(t, d, "two", MainBranchAt, "signals", "done")
	if diff.Verdict != VerdictUnpaired || !closeTo(diff.Mean, 4) || diff.Seeds != 5 || diff.Low != 0 || diff.High != 0 || diff.SeedsNeeded != 0 {
		t.Errorf("signals' difference on done on window two is %+v, want d 4 over 5 seeds, not paired, with no interval", diff)
	}
	if got, want := diff.cell(), "4 over 5 seeds: not paired"; got != want {
		t.Errorf("the document gives %q, want %q", got, want)
	}
}

// The break-even search runs the main branch from 1.0 to 2.0 by 0.1, and a rule's break-even factor
// is the first at which its standing against main differs from its standing at 1.0, or none (spec
// R3, assumption 1). Each factor's main branch is a single run, judged against the replay's own
// error on the issues finished, 2: 105 issues against main's 100 is better, and 85 worse.
func TestBreakEvenFactor(t *testing.T) {
	t.Parallel()
	factors := BreakEvenSlowdowns()
	if want := []float64{1, 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9, 2}; !slices.Equal(factors, want) {
		t.Fatalf("the break-even search runs %v, want %v", factors, want)
	}
	held := map[string]float64{"done": 2}
	main := map[float64]SeedCosts{}
	rule := map[float64]SeedCosts{}
	for _, f := range factors {
		main[f] = SeedCosts{1: {Done: 100, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400}}
		done := 105.0
		if f >= 1.4 {
			done = 85
		}
		rule[f] = SeedCosts{1: {Done: done, WaitOnlyForSlot: 50, PriorityTaken: 0.2, OtherDelays: 400}}
	}
	got := BreakEven(factors, rule, main, held)
	if !got.Changes || got.Factor != 1.4 || got.Standings[0].Standing != StandingBetter || standingAt(got, 1.4) != StandingWorse {
		t.Errorf("the search gives %+v; want the factor 1.4, better below it and worse from it", got)
	}
	skipped := maps.Clone(rule)
	delete(skipped, 1.4)
	if got := BreakEven(factors, skipped, main, held); !got.Changes || got.Factor != 1.5 || len(got.Standings) != len(factors)-1 {
		t.Errorf("with no run at 1.4 the search gives %+v; want the factor 1.5 over the ten factors that ran", got)
	}
	steady := map[float64]SeedCosts{}
	for _, f := range factors {
		steady[f] = rule[1]
	}
	if got := BreakEven(factors, steady, main, held); got.Changes || len(got.Standings) != len(factors) {
		t.Errorf("a rule better at every factor gives %+v; no factor up to 2.0 changes its rank", got)
	}
}

// judgedSeeds is the seeds of a branch with a draw in the made-up comparisons whose decision a
// test reads (writeJudgedRuns).
const judgedSeeds = 3

// judgedRuleNames is the rules of the made-up comparisons whose decision a test reads, in the
// order of record.SlotRules: main, the rules the windows' daemons ran, keep-past-review and signals.
var judgedRuleNames = []string{record.MainRule.Name, "legion-v20.42.5", "legion-v19.57.3", "keep-past-review", "signals"}

// judgedPlan is the plan of the made-up comparisons whose decision a test reads: the main branch,
// the branch that asks git and the branch that draws every conflict, each at f 1, with judgedSeeds
// seeds for a branch with a draw, and the main branch at f 1.1 for the break-even search.
func judgedPlan(t *testing.T) runPlan {
	t.Helper()
	git, err := BranchNamed("main+conflicts=git")
	if err != nil {
		t.Fatal(err)
	}
	return runPlan{branches: []BranchAt{MainBranchAt, {Branch: git, Slowdown: 1}, {Branch: drawnConflictBranch, Slowdown: 1}}, breakEven: []float64{1.1},
		perDraw: judgedSeeds}
}

// judgedDone is the issues each run of a made-up comparison whose decision a test reads finishes
// (writeJudgedRuns), where each record finishes 20 (sampleCard): 22 under the window's own rule,
// so the replay's own error on done is 2 on each window; under signals 3 more than main's 20 on a
// branch of one seed and 5 more plus the seed's offset on a branch with a draw, the offsets of the
// seeds 1 to 3 being -1, 0 and 1; under keep-past-review 2 more on a branch of one seed and 1 more
// plus the seed's offset on a branch with a draw; and 20 under every other rule.
func judgedDone(w windowRecord, rule string, at BranchAt, seed int64) int {
	drawing := len(w.seeds(at.Branch, judgedSeeds)) > 1
	offset := int(seed) - 2
	switch {
	case rule == w.Rule:
		return 22
	case rule == "signals" && drawing:
		return 25 + offset
	case rule == "signals":
		return 23
	case rule == "keep-past-review" && drawing:
		return 21 + offset
	case rule == "keep-past-review":
		return 22
	}
	return 20
}

// writeJudgedRuns writes under dir a made-up comparison whose decision a test reads, and answers
// its windows: madeUpWindows' two, window-1 under legion-v19.57.3, none of whose stretches ended in
// a conflict, so each of its branches runs seed 1 alone, and window-2 under legion-v20.42.5, 3 of
// whose 10 fitted stretches ended in one, so its branches that draw conflicts run the seeds 1 to
// judgedSeeds and its main branch, which keeps the recorded conflicts, seed 1 alone; under each of
// judgedRuleNames on judgedPlan. Each run is madeUpRun's with done's issues finished in both its
// scorecards, and its run.json moved by change where change is not nil.
func writeJudgedRuns(t *testing.T, dir string, done func(w windowRecord, rule string, at BranchAt, seed int64) int,
	change func(w windowRecord, rule string, at BranchAt, seed int64, ran *runRecord)) []windowRecord {
	t.Helper()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[1].Stretches, windows[1].Conflicted = 10, 3
	rules := make([]record.SlotRule, 0, len(judgedRuleNames))
	for _, name := range judgedRuleNames {
		rules = append(rules, replayedRule(t, name))
	}
	writeMadeUpRuns(t, dir, windows, rules, judgedPlan(t), func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
		snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
		snapshot.Done = done(w, rule.Name, at, seed)
		line.Done = snapshot.Done
		if change != nil {
			change(w, rule.Name, at, seed, &ran)
		}
		return snapshot, line, ran
	})
	return windows
}

// judgedDocument is the document of the made-up comparison under dir.
func judgedDocument(t *testing.T, dir string) string {
	t.Helper()
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := runs.Comparison("comparison-issues.md")
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// The branch that keeps the recorded conflicts draws nothing, so it runs seed 1 alone even on a
// window whose stretches ended in conflicts, where the branches that draw them, git's and the drawn
// one, run every seed of a branch with a draw (windowRecord.seeds); and a difference there is a
// single run's, judged against the replay's own error alone (spec R7). The comparison is made up
// (writeJudgedRuns, judgedDone), and each window's replay under its own rule finishes 22 issues
// where its record finishes 20, so the error on done is 2. On window-2's main branch signals
// finishes 3 more issues than main, a single run past the error, better, and keep-past-review 2
// more, at the error, a tie. On its drawn branch at three seeds signals finishes 5 more plus each
// seed's offset of -1, 0 and 1 (s = 1, and Student's t at two degrees of freedom is 4.3027, so the
// interval is 5 ± 4.3027 / √3 = 5 ± 2.4841), [2.5159, 7.4841], past the error, better; and
// keep-past-review 1 more plus the offsets, [-1.4841, 3.4841], a tie that would need ⌈1.96²⌉ = 4
// seeds. The window's own rule, 2 more at each seed (s = 0), ties at the error itself, and
// ⌈(1.96 · 0 / 2)²⌉ = 0 more seeds would tell it from zero. Signals keeps the owner's direction
// and is better on every branch, so it is named best.
func TestOneSeedOnTheRecordedConflictBranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	windows := writeJudgedRuns(t, dir, judgedDone, nil)
	plan := judgedPlan(t)
	for _, tc := range []struct {
		w     windowRecord
		at    BranchAt
		seeds []int64
	}{
		{windows[1], plan.branches[0], []int64{1}}, {windows[1], plan.branches[1], []int64{1, 2, 3}}, {windows[1], plan.branches[2], []int64{1, 2, 3}},
		{windows[0], plan.branches[1], []int64{1}},
	} {
		if got := tc.w.seeds(tc.at.Branch, judgedSeeds); !slices.Equal(got, tc.seeds) {
			t.Errorf("%s runs %s at the seeds %v, want %v", tc.w.Name, tc.at.Label(), got, tc.seeds)
		}
	}
	document := judgedDocument(t, dir)
	decision := section(t, document, "## The rule named best (R7)", "## The rule named best (R7)", "\n## ")
	for _, line := range []string{"| done | 2 |", "`signals` is named best", "(4.3027 at the 3 seeds of a branch with a draw)"} {
		if !strings.Contains(decision, line) {
			t.Errorf("the decision holds no %q:\n%s", line, decision)
		}
	}
	matrix := "## Every measure for every rule, window and branch"
	for _, tc := range []struct {
		heading, row string
	}{
		{"### window-2, `main@1` (passes R5; seeds: 1)",
			"| done | 2 | 2, a single run: tie | 0, a single run: tie, never | 2, a single run: tie | 3, a single run: better |"},
		{"### window-2, `main+conflicts=drawn@1` (passes R5; seeds: 3)",
			"| done | 2 | 2 [2, 2] over 3 seeds: tie, 0 seeds needed | 0 [0, 0] over 3 seeds: tie, never | 1 [-1.4841, 3.4841] over 3 seeds: tie, 4 seeds needed | 5 [2.5159, 7.4841] over 3 seeds: better |"},
	} {
		table := section(t, document, matrix, tc.heading, "\n### ")
		assertComparisonRows(t, table, tc.heading, []string{tc.row})
	}
}

// A branch that fails R5 is read by nothing in the document either: in a made-up comparison
// (writeJudgedRuns) whose window-2 replay under its own rule finishes 40 issues on the branch that
// asks git, against the record's 20, that branch gives no comparison, its table judges no rule, and
// signals, which finishes 10 issues fewer than main there, is better on each of the five branches
// that pass, so it is named best.
func TestABranchThatFailsR5IsReadByNothingInTheDocument(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeJudgedRuns(t, dir, func(w windowRecord, rule string, at BranchAt, seed int64) int {
		if w.Name == "window-2" && at.Branch.asksGit() {
			switch rule {
			case w.Rule:
				return 40
			case "signals":
				return 10
			}
		}
		return judgedDone(w, rule, at, seed)
	}, nil)
	document := judgedDocument(t, dir)
	failing := section(t, document, "## Every measure for every rule, window and branch", "### window-2, `main+conflicts=git@1` (gives no comparison; seeds: 3)", "\n### ")
	if !strings.Contains(failing, "The branch gives no comparison, so no rule is judged on it.") || strings.Contains(failing, "| judged against `main` |") {
		t.Errorf("the table of the branch that fails R5 judges a rule, or does not say it judges none:\n%s", failing)
	}
	for _, line := range []string{"`signals` is named best: it keeps the owner's direction, and on each of the 5 branches that pass R5", "| `signals` | yes | better 5, worse 0, tie 0 |"} {
		if !strings.Contains(document, line) {
			t.Errorf("the document holds no %q", line)
		}
	}
}

// The document gives each run's admissions with no recorded work and the slot-hours they hold (spec
// R7), read from each run's run.json (Divergence): one row for each window, rule, branch and
// break-even factor, and seed. The comparison is made up (writeJudgedRuns): each run admits as many
// issues with no work as its rule's place among judgedRuleNames plus its seed, holding a quarter of
// a slot-hour each.
func TestTheDocumentGivesEachRunsNoWorkAdmissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	windows := writeJudgedRuns(t, dir, judgedDone, func(_ windowRecord, rule string, _ BranchAt, seed int64, ran *runRecord) {
		admitted := slices.Index(judgedRuleNames, rule) + int(seed)
		ran.Divergence = Divergence{NoWorkAdmissions: admitted, NoWorkSlotHours: float64(admitted) / 4}
	})
	plan := judgedPlan(t)
	var rows []string
	for _, w := range windows {
		for r, rule := range judgedRuleNames {
			for _, at := range slices.Concat(plan.branches, []BranchAt{{Branch: MainBranch, Slowdown: 1.1}}) {
				for _, seed := range w.seeds(at.Branch, judgedSeeds) {
					admitted := r + int(seed)
					rows = append(rows, fmt.Sprintf("| %s | `%s` | `%s` | %d | %d | %s |", w.Name, rule, at.Label(), seed, admitted, formatNumber(float64(admitted)/4)))
				}
			}
		}
	}
	document := judgedDocument(t, dir)
	table := section(t, document, "## Admissions with no recorded work", "| window | rule | branch | seed | admissions with no work | their slot-hours |", "\n\n")
	assertComparisonRows(t, table, "no-work admissions", rows)
	if got := strings.Count(table, "\n| window-"); got != len(rows) {
		t.Errorf("the table holds %d rows, want one for each of the %d runs:\n%s", got, len(rows), table)
	}
}

// Two rules' runs at one seed share the draw of each stretch both reach (spec R7): each run records
// each draw of a stretch's conflict with its key, the issue, the recorded start and the phase of
// the piece whose end ends the stretch and its count, and its value (StretchDraw), and the
// comparison counts, for each pair of rules at each seed, the stretches both drew and those of them
// that drew another value (drawsAgree). Replayed at seed 1 under main and under keep-past-review
// (sharedDrawsWindow to 13:30, as TestTwoRulesAtOneSeedShareEachStretchsDraw replays it), the two
// runs share every stretch keep-past-review drew, and main draws one more, ACME-3's last, which ends
// after the window under keep-past-review: every shared stretch has one value. Each value drawn
// again with a key that holds the replayed moment of its draw, which keep-past-review moves by
// holding ACME-3 back, differs at some shared stretch: the count fails such a key. In a made-up
// comparison (writeJudgedRuns) of five rules, ten pairs, the runs on window-2's drawn branch draw
// ACME-1's stretch under every rule, ACME-2's under every rule but signals, ACME-3's under
// keep-past-review alone and ACME-4's under keep-past-review and signals alone, a stretch main
// never reaches. At each seed the pairs share 17 stretches: ACME-1's and ACME-2's in each of the six
// pairs of main, the two windows' rules and keep-past-review, ACME-1's in each of the three of
// those but keep-past-review with signals, and ACME-1's and ACME-4's in keep-past-review with
// signals. At seed 2 keep-past-review draws 0.9 at ACME-2's, where the other three draw 0.4, so
// three pairs differ; at seed 3 signals draws 0.7 at ACME-4's, where keep-past-review draws 0.6, so
// that pair alone differs, at a stretch that no pair with main holds. Window-1's runs draw nothing.
// The document gives a line for each window and seed, and names each pair, branch and stretch that
// differs with its two values.
func TestDrawsMatchAcrossRules(t *testing.T) {
	t.Parallel()
	t.Run("two replays at one seed", func(t *testing.T) {
		t.Parallel()
		dsn := replayDSN(t)
		window := sharedDrawsWindow(t, clock(13, 30))
		type replayed struct {
			dir   string
			draws []conflictDraw
		}
		replay := func(rule string) *replayed {
			r := &replayed{dir: t.TempDir()}
			if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, rule), Branch: drawnConflictBranch, Seed: 1, Slowdown: 1,
				PGDSN: dsn, Out: r.dir, onDraw: func(draw conflictDraw) { r.draws = append(r.draws, draw) }}); err != nil {
				t.Fatalf("replay the window under %s: %v", rule, err)
			}
			return r
		}
		read := func(r *replayed) []keyedDraw {
			read, err := readRun(r.dir, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			return read.draws
		}
		main, kept := replay(record.MainRule.Name), replay("keep-past-review")
		mainDraws, keptDraws := read(main), read(kept)
		if len(mainDraws) != len(main.draws) || len(keptDraws) != len(kept.draws) {
			t.Fatalf("run.json records %d and %d draws, and the runs drew %d and %d", len(mainDraws), len(keptDraws), len(main.draws), len(kept.draws))
		}
		if shared, differ := drawsAgree(mainDraws, keptDraws); shared != len(kept.draws) || len(main.draws) != shared+1 || len(differ) != 0 {
			t.Errorf("main drew %d stretches and keep-past-review %d, %d of them both, %d of those another value; want every stretch keep-past-review drew shared, one more under main, and none of another value",
				len(main.draws), len(kept.draws), shared, len(differ))
		}
		source := rand.NewPCG(0, 0)
		draws := rand.New(source)
		rekey := func(r *replayed) {
			path := filepath.Join(r.dir, runFile)
			var ran runRecord
			if err := readJSON(path, &ran); err != nil {
				t.Fatal(err)
			}
			for i, draw := range r.draws {
				moved := draw.at
				moved.start = draw.drawn
				source.Seed(mix64(1^conflictStream), moved.hash())
				ran.Draws[i].U = draws.Float64()
			}
			if err := writeJSON(path, ran); err != nil {
				t.Fatal(err)
			}
		}
		rekey(main)
		rekey(kept)
		if shared, differ := drawsAgree(read(main), read(kept)); len(differ) == 0 {
			t.Errorf("with each draw keyed by the replayed moment of the draw, the %d stretches both rules drew hold one value each: the count does not fail a key the rule moves", shared)
		}
	})
	t.Run("the document", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeJudgedRuns(t, dir, judgedDone, func(w windowRecord, rule string, at BranchAt, seed int64, ran *runRecord) {
			if w.Name != "window-2" || at.Branch.Name != drawnConflictBranch.Name {
				return
			}
			u := 0.1 * float64(seed)
			ran.Draws = []StretchDraw{{Issue: "ACME-1", Start: clock(9, 30), Phase: phase.Merging, U: u}}
			if rule == "keep-past-review" || rule == "signals" {
				fourth := StretchDraw{Issue: "ACME-4", Start: clock(12, 30), Phase: phase.Merging, U: 0.6}
				if rule == "signals" && seed == 3 {
					fourth.U = 0.7
				}
				ran.Draws = append(ran.Draws, fourth)
			}
			if rule == "signals" {
				return
			}
			second := StretchDraw{Issue: "ACME-2", Start: clock(10, 0), Phase: phase.Reviewing, U: 2 * u}
			if rule == "keep-past-review" {
				if seed == 2 {
					second.U = 0.9
				}
				ran.Draws = append(ran.Draws, StretchDraw{Issue: "ACME-3", Start: clock(12, 0), Phase: phase.Merging, U: 0.5})
			}
			ran.Draws = append(ran.Draws, second)
		})
		document := judgedDocument(t, dir)
		part := "## The draws that each pair of rules shares"
		at := func(hour, minute int) string { return clock(hour, minute).UTC().Format("2006-01-02T15:04Z") }
		acme2 := "ACME-2's stretch ended by its reviewing piece from " + at(10, 0) + ", draw 1, 0.4 and 0.9"
		for _, tc := range []struct {
			window string
			lines  []string
		}{
			{"window-1", []string{"- seed 1: 10 pairs of rules compared, 0 stretches that both runs of a pair drew, none of them another value."}},
			{"window-2", []string{
				"- seed 1: 10 pairs of rules compared, 17 stretches that both runs of a pair drew, none of them another value.",
				"- seed 2: 10 pairs of rules compared, 17 stretches that both runs of a pair drew, 3 of them another value:",
				"  - `main` and `keep-past-review` on `main+conflicts=drawn@1`: " + acme2,
				"  - `legion-v20.42.5` and `keep-past-review` on `main+conflicts=drawn@1`: " + acme2,
				"  - `legion-v19.57.3` and `keep-past-review` on `main+conflicts=drawn@1`: " + acme2,
				"- seed 3: 10 pairs of rules compared, 17 stretches that both runs of a pair drew, 1 of them another value:",
				"  - `keep-past-review` and `signals` on `main+conflicts=drawn@1`: ACME-4's stretch ended by its merging piece from " + at(12, 30) + ", draw 1, 0.6 and 0.7",
			}},
		} {
			assertComparisonRows(t, section(t, document, part, "### "+tc.window+"\n", "\n### "), tc.window+"'s draws", tc.lines)
		}
	})
}

// A window that lists a workflow experiment in the random mode is paired by nothing (spec R7): a run
// draws such an experiment from one source of its seed, afresh at each boot and in the order it
// records its issues, keyed to no issue, so run.json names those draws apart (UnkeyedDraws) and the
// comparison gives each difference on the window as not paired. The fixture window
// random-experiment is priority-inversion with one experiment in the random mode over the unit
// issue, review-depth, and its README works out its four draws by hand: one for each of its four
// roots, which the daemon records at their filings. A comparison of priority-inversion, the window
// before, and random-experiment, the window after, both recorded under main, under main and
// keep-past-review on the main branch at one seed, writes 4 such draws into each run of
// random-experiment and none into priority-inversion's, 8 over random-experiment's two runs. It
// gives every difference that a seed gives on random-experiment as not paired and none on
// priority-inversion, and its document says of each window whether its window.json lists an
// experiment, and that no difference on random-experiment is paired.
func TestAWindowThatListsARandomExperimentIsNotPaired(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	root := t.TempDir()
	cfg := CompareConfig{Windows: []string{filepath.Join(fixtureWindowsDir, "priority-inversion"), filepath.Join(fixtureWindowsDir, "random-experiment")},
		Rules: []record.SlotRule{record.MainRule, replayedRule(t, "keep-past-review")}, PGDSN: dsn, Parallel: 2, Runs: root,
		plan: &runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1}}
	if err := RunCompare(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		window string
		draws  int
	}{{"priority-inversion", 0}, {"random-experiment", 4}} {
		for _, rule := range []string{record.MainRule.Name, "keep-past-review"} {
			var ran runRecord
			if err := readJSON(filepath.Join(runDir(root, tc.window, rule, MainBranchAt, 1), runFile), &ran); err != nil {
				t.Fatal(err)
			}
			if ran.UnkeyedDraws.Experiments != tc.draws {
				t.Errorf("the run of %s under %s names %d draws it does not key, want %d", tc.window, rule, ran.UnkeyedDraws.Experiments, tc.draws)
			}
		}
	}
	runs, err := ReadRuns(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := runs.readComparison()
	if err != nil {
		t.Fatal(err)
	}
	unpaired := map[string]int{}
	for _, j := range c.decision().Judged {
		for _, diff := range j.Differences {
			switch {
			case diff.Verdict == VerdictUnpaired:
				unpaired[j.Window]++
			case j.Window == "random-experiment" && diff.Verdict != VerdictNone:
				t.Errorf("the comparison gives %s's difference on %s on %s as %s, a paired result on a window that lists a random experiment",
					j.Rule, diff.Measure, j.Branch.Label(), diff.Verdict)
			}
		}
	}
	if unpaired["random-experiment"] == 0 || unpaired["priority-inversion"] != 0 {
		t.Errorf("the comparison gives %d differences on random-experiment and %d on priority-inversion as not paired, want some and none", unpaired["random-experiment"],
			unpaired["priority-inversion"])
	}
	document, _, err := runs.Comparison("comparison-issues.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"priority-inversion lists no experiment in its window.json, as collect writes none, so its replays draw no experiment arm, and no draw of its runs is an unkeyed experiment draw.",
		"random-experiment lists the experiments `review-depth` in the random mode in its window.json, and its runs drew 8 values for them that they key to no stretch.",
		"No difference on random-experiment is paired: random-experiment lists the experiment `review-depth` in the random mode",
		" not paired: the window lists an experiment in the random mode |",
	} {
		if !strings.Contains(document, line) {
			t.Errorf("the document holds no %q", line)
		}
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
	t.Parallel()
	skipUntilBuilt(t)
	dir := t.TempDir()
	to := compareFrom.Add(compareHours * time.Hour)
	recorded := func(name string, order int, rule, commit string) windowRecord {
		return windowRecord{Name: name, Order: order, Project: "ACME", From: compareFrom, To: to, Rule: rule, Scorecard: sampleCard(compareHours),
			Parts: []Part{{From: compareFrom, To: to, Release: "fixture", Commit: commit, Rule: rule}}, MissingAncestors: []string{}}
	}
	before := recorded("window-two", 0, "legion-v19.57.3", "1111111111111111111111111111111111111111")
	before.Stretches = 4
	before.Scorecard.UrgentWaits = comparisonUrgentWaits(0.5)
	after := recorded("window-one", 1, "legion-v20.42.5", "2222222222222222222222222222222222222222")
	after.Stretches, after.Conflicted = 10, 3
	after.Scorecard.UrgentWaits = comparisonUrgentWaits(0.5)
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
			slotWait := comparisonSlotWait(w.Order, r, branchIndex[at.Label()])
			snapshot.FirstSlot["ACME-2"] = compareFrom.Add(time.Duration(slotWait * float64(time.Hour)))
			snapshot.UrgentWaits = comparisonUrgentWaits(slotWait)
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
				Divergence:      Divergence{NoWorkAdmissions: noWork, NoWorkSlotHours: noWorkHours},
				SignalSummaries: map[string]SignalSummary{}, SuppliedRefused: map[string]int{}, MissingAncestors: []string{},
				StartedAt: b.started.Add(time.Duration(b.runs) * time.Minute), Gaps: Gaps(snapshot, line)}
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
		"window-two does not measure how a rule changes conflicts",
		"window-one fits the generator on 10 stretches, 3 of which ended in a conflict. Main keeps the recorded conflicts. The git branch draws for unanswered reads, and the drawn branch uses the generator throughout.",
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
			// As comparisonUrgentWaits gives them, in hours: P0's first slots at 0 and slotWait, done
			// at 2 and 6 (one cut); P1's first todo at hour 1, no slot or done (both waits cut at hour
			// 6). Every seed of a branch gives the same, so each cell is that value over the seeds.
			seeds := len(w.seeds(at.Branch, plan.perDraw))
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
					value := measure.value(comparisonSlotWait(w.Order, r, index))
					row += " " + spreadCell(value, value, value, seeds) + " |"
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

// comparisonUrgentWaits is the wait of urgent work of a made-up run whose ACME-2 takes its first
// slot slotWait hours on, worked by hand over the window of compareHours from compareFrom, with no
// run-out: ACME-1 and ACME-2, P0 at their first todo at the start, take their first slots at 0 and
// slotWait, ACME-1 is done 2 hours on and ACME-2 not by hour 6, a cut wait; ACME-3, P1 from hour 1,
// has no slot and no done by hour 6, two cut waits of 5 hours.
func comparisonUrgentWaits(slotWait float64) map[string]UrgentWait {
	minutes := slotWait * 60
	return map[string]UrgentWait{
		"P0": {Issues: 2, FirstSlot: WaitMinutes{MedianMinutes: minutes / 2, P90Minutes: minutes}, Done: WaitMinutes{MedianMinutes: 240, P90Minutes: 360, Cut: 1}},
		"P1": {Issues: 1, FirstSlot: WaitMinutes{MedianMinutes: 300, P90Minutes: 300, Cut: 1}, Done: WaitMinutes{MedianMinutes: 300, P90Minutes: 300, Cut: 1}},
	}
}

// comparisonSlotWait is the hours ACME-2 of a made-up run of
// TestTheDocumentHasEveryRuleWindowBranchAndMeasure waits for its first slot: half an hour or an
// hour by the parity of the window's place and the rule's, and a quarter of an hour more on a
// branch of odd place. So each branch's table reads its own runs, and each rule's difference from
// main on the wait of urgent work, which the rules are judged on, is the same on every branch and
// at every factor of the break-even search.
func comparisonSlotWait(window, rule, branch int) float64 {
	return 0.5 + 0.5*float64((window+rule)%2) + 0.25*float64(branch%2)
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
		Scenario: at.Branch.NeverStarted, Assumptions: []int{1}, StartedAt: compareFrom, Gaps: Gaps(card, card),
		SignalSummaries: map[string]SignalSummary{}, SuppliedRefused: map[string]int{}, MissingAncestors: []string{}}
}

// madeUpWindows is two windows of compareHours hours from compareFrom for a comparison of made-up
// runs, window-1 before the change of the rule and window-2 after it, whose daemons ran the rules
// before and after, each with sampleCard as its record's scorecard.
func madeUpWindows(before, after string) []windowRecord {
	to := compareFrom.Add(compareHours * time.Hour)
	var windows []windowRecord
	for i, rule := range []string{before, after} {
		windows = append(windows, windowRecord{Name: fmt.Sprintf("window-%d", i+1), Order: i, Project: "ACME", From: compareFrom, To: to, Rule: rule,
			Scorecard: sampleCard(compareHours), Parts: []Part{{From: compareFrom, To: to, Release: "fixture", Commit: strings.Repeat("1", 40), Rule: rule}},
			MissingAncestors: []string{}})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		windows[i].Scorecard.FirstSlot = map[string]time.Time{"ACME-2": moment(0), "ACME-1": moment(30), "ACME-3": moment(60)}
		windows[i].Scorecard.DoneAt = map[string]time.Time{"ACME-2": moment(120), "ACME-1": moment(180)}
	}
	rules := []record.SlotRule{replayedRule(t, record.MainRule.Name), replayedRule(t, "returned-by-first-slot")}
	dir := t.TempDir()
	// Main no longer draws conflicts. Give the report three synthetic runs
	// explicitly so the assertions still exercise its median and missing-time rules.
	for _, w := range windows {
		if err := writeJSON(filepath.Join(dir, w.Name, recordFile), w); err != nil {
			t.Fatal(err)
		}
		for r, rule := range rules {
			branch := partRun{Branch: MainBranch.Name, Slowdown: 1, Seeds: []int64{1, 2, 3}}
			for _, seed := range branch.Seeds {
				snapshot, line, ran := madeUpRun(w, r, rule, MainBranchAt, seed)
				line.FirstSlot, line.DoneAt = times(replays[rule.Name].firstSlot, seed), times(replays[rule.Name].doneAt, seed)
				ran.Gaps = Gaps(snapshot, line)
				for name, value := range map[string]any{runFile: ran, scorecardFile: snapshot, lineScorecardFile: line} {
					if err := writeJSON(filepath.Join(runDir(dir, w.Name, rule.Name, MainBranchAt, seed), name), value); err != nil {
						t.Fatal(err)
					}
				}
			}
			part := partRecord{Window: w.Name, Rule: rule.Name, RuleCommit: rule.Commit, Branches: []partRun{branch}}
			if err := writeJSON(filepath.Join(dir, w.Name, rule.Name, partFile), part); err != nil {
				t.Fatal(err)
			}
		}
	}
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
	t.Parallel()
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
	starts := make([]SeedStarts, 0, len(branch.Starts))
	for _, s := range branch.Starts {
		starts = append(starts, SeedStarts{Seed: s.Seed, ReplayOnly: s.ReplayOnly, RecordOnly: s.RecordOnly})
	}
	if want := []SeedStarts{{Seed: 1, ReplayOnly: 2, RecordOnly: 1}, {Seed: 2, RecordOnly: 1}}; !slices.Equal(starts, want) {
		t.Errorf("the validation counts the starts %+v, want %+v", starts, want)
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

// Beside R5's first-slot test, whose statistic is, per issue, the mean over the seeds of the
// absolute error of its first slot (assumption 44), a validation gives each seed's own median
// absolute first-slot error, over the issues the record or that seed gives a first slot in the
// window, a missing one standing at --to; it prints it in the table of first starts and gives it in
// --json, and tests nothing by it. The record starts ACME-1 and ACME-2 at 10:00 in a window to
// 13:00; seed 3 starts them at 10:10 and 10:30, errors of 10 and 30 minutes, median 20; seed 7
// starts ACME-1 at 10:40 and not ACME-2, errors of 40 and 180, median 110. R5's statistic stays the
// median of the issues' mean errors, 25 and 105, which is 65, and fails its bound of 60 minutes.
func TestTheValidationGivesEachSeedsMedianFirstSlotError(t *testing.T) {
	t.Parallel()
	recorded := Scorecard{From: clock(9, 0), To: clock(13, 0), FirstSlot: map[string]time.Time{"ACME-1": clock(10, 0), "ACME-2": clock(10, 0)}}
	cards := []Scorecard{
		{Seed: 3, FirstSlot: map[string]time.Time{"ACME-1": clock(10, 10), "ACME-2": clock(10, 30)}},
		{Seed: 7, FirstSlot: map[string]time.Time{"ACME-1": clock(10, 40)}},
	}
	branch := ValidateBranch(recorded, MainBranchAt, cards)
	if branch.FirstSlot.Median != 65 || branch.FirstSlotPass {
		t.Errorf("R5's first-slot test gives a median of %v and a pass of %t, want 65 and a fail", branch.FirstSlot.Median, branch.FirstSlotPass)
	}
	var out bytes.Buffer
	if err := (Validation{Branches: []BranchValidation{branch}}).Write(&out, 2, nil); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"| `main@1` | 3 | 0 | 0 | 20 |", "| `main@1` | 7 | 0 | 1 | 110 |"} {
		if !strings.Contains(out.String(), row) {
			t.Errorf("the validation prints no %q:\n%s", row, out.String())
		}
	}
	data, err := json.Marshal(branch)
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		Starts []struct {
			Seed   int64   `json:"seed"`
			Median float64 `json:"firstSlotMedianMinutes"`
		} `json:"starts"`
	}
	if err := json.Unmarshal(data, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Starts) != 2 || read.Starts[0].Median != 20 || read.Starts[1].Median != 110 {
		t.Errorf("the validation's JSON gives the seeds' medians %+v, want 20 for seed 3 and 110 for seed 7", read.Starts)
	}
}

// Beside R5's tests a validation names each tree whose hours of a closed design gate in the replay,
// the mean over the seeds, stand more than 30% from the record's (the red team's finding 5), and
// prints them, testing nothing. The record's gate of ACME-1 stands closed 2 hours and ACME-2's 1;
// the seeds close ACME-1's for 2.5 and 3 hours (2.75, 37.5% more), ACME-2's for 1 and 1.2 (1.1, 10%
// more), and one of them ACME-3's for half an hour, which the record never closes: ACME-1 and ACME-3
// are named, and ACME-2 is not.
func TestTheValidationNamesTheTreesWhoseGateHoursMoved(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// A failing drawn branch is not a comparison cell, but does not suppress the other branches.
func TestConflictBranchesStandInTheValidation(t *testing.T) {
	t.Parallel()
	git, err := BranchNamed("main+conflicts=git")
	if err != nil {
		t.Fatal(err)
	}
	drawn := BranchAt{Branch: drawnConflictBranch, Slowdown: 1}
	for _, branch := range []Branch{MainBranch, git, drawn.Branch} {
		if !slices.Contains(validatePlan(ValidateConfig{}).branches, BranchAt{Branch: branch, Slowdown: 1}) {
			t.Errorf("validation omits %s", branch.Name)
		}
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)[:1]
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{record.MainRule},
		runPlan{branches: []BranchAt{MainBranchAt, {Branch: git, Slowdown: 1}, drawn}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			if at == drawn {
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
	if !v.MainPasses() || !slices.Equal(v.NoComparison(), []BranchAt{drawn}) {
		t.Errorf("main passes %t; no comparison on %v, want drawn alone", v.MainPasses(), v.NoComparison())
	}
	var out bytes.Buffer
	if err := v.Write(&out, 1, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "the pull request does not merge while it fails on window one") {
		t.Error("validation does not say a failing generator on window one blocks the pull request")
	}
}

// R5 names the unexpected starts and omitted filings on main at each slowdown factor, without
// changing the validation verdict.
// scorecard give each issue of the window's project filed in the window, at its filing (filed):
// priorityWorkWindow files ACME-1 to ACME-6 from 09:00 to 09:55 and ACME-7 at 08:30, before the
// window; firstStartsWindow files ACME-1, ACME-2 and ACME-3, and a replay of it under main on the
// branch, which keeps each person's filing at its recorded time, files the same three at the same
// times. The validation is made up: the record starts ACME-1 and ACME-2 and files ACME-4 at 10:00
// and ACME-5 at 11:00; at f 1 the branch starts ACME-3 too and files ACME-4 alone, at f 1.2 it starts
// ACME-3 and ACME-6 and files neither, and at f 1.6 it starts and files what the record did. The main
// branch passes R5, and the names move neither its verdict nor which branches give a comparison.
func TestTheRecordedConflictBranchNamesEveryBreaker(t *testing.T) {
	t.Parallel()
	filedOf := func(card Scorecard) map[string]time.Time {
		var read struct {
			Filed map[string]time.Time `json:"filed"`
		}
		scorecardJSON(t, card, &read)
		return read.Filed
	}
	card, err := Score(priorityWorkWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	wantFiled := map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 10), "ACME-3": clock(9, 20), "ACME-4": clock(9, 40),
		"ACME-5": clock(9, 50), "ACME-6": clock(9, 55)}
	if got := filedOf(card); !maps.EqualFunc(got, wantFiled, time.Time.Equal) {
		t.Errorf("the record's scorecard gives the issues filed in the window as %v, want %v", got, wantFiled)
	}
	recorded := MainBranch
	window := firstStartsWindow(t)
	card, err = Score(window)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: recorded, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir()})
	if err != nil {
		t.Fatalf("replay the window on %s: %v", recorded.Name, err)
	}
	wantFiled = map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 1), "ACME-3": clock(10, 20)}
	for name, got := range map[string]map[string]time.Time{"the record's scorecard": filedOf(card), "the replay's line scorecard": filedOf(result.LineScorecard)} {
		if !maps.EqualFunc(got, wantFiled, time.Time.Equal) {
			t.Errorf("%s of firstStartsWindow gives the issues filed in the window as %v, want %v", name, got, wantFiled)
		}
	}

	recordFiled := map[string]time.Time{"ACME-4": clock(10, 0), "ACME-5": clock(11, 0)}
	type replayed struct {
		started []string
		filed   map[string]time.Time
	}
	byFactor := map[float64]replayed{
		1:   {started: []string{"ACME-3"}, filed: map[string]time.Time{"ACME-4": clock(10, 0)}},
		1.2: {started: []string{"ACME-3", "ACME-6"}, filed: map[string]time.Time{}},
		1.6: {filed: recordFiled},
	}
	var branches []BranchAt
	for _, f := range Slowdowns {
		branches = append(branches, BranchAt{Branch: recorded, Slowdown: f})
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)[:1]
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, record.MainRule.Name)},
		runPlan{branches: branches, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			if at.Branch.recordedConflicts() {
				for _, key := range byFactor[at.Slowdown].started {
					line.FirstSlot[key] = compareFrom.Add(time.Hour)
				}
			}
			return snapshot, line, ran
		})
	patchJSON(t, filepath.Join(dir, windows[0].Name, recordFile), func(fields map[string]any) {
		fields["scorecard"].(map[string]any)["filed"] = recordFiled
	})
	for _, at := range branches {
		filed := byFactor[at.Slowdown].filed
		patchJSON(t, filepath.Join(runDir(dir, windows[0].Name, record.MainRule.Name, at, 1), lineScorecardFile), func(fields map[string]any) {
			fields["filed"] = filed
		})
	}
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := runs.Validation(windows[0].Name, record.MainRule.Name)
	if err != nil {
		t.Fatal(err)
	}
	// At f=1.2 the added first slots move the median past the R5 gap. The breaker
	// names do not disqualify main@1, which also has breakers.
	if !v.MainPasses() || !slices.Equal(v.NoComparison(), []BranchAt{{Branch: MainBranch, Slowdown: 1.2}}) {
		t.Errorf("main passes %t and no comparison on %v, want only main@1.2 disqualified", v.MainPasses(), v.NoComparison())
	}
	var read struct {
		Branches []struct {
			Branch struct {
				Slowdown float64 `json:"slowdown"`
			} `json:"branch"`
			Breakers []struct {
				Seed               int64    `json:"seed"`
				StartedNotRecorded []string `json:"startedNotRecorded"`
				FiledLeftOut       []string `json:"filedLeftOut"`
			} `json:"breakers"`
		} `json:"branches"`
	}
	asJSON(t, v, &read)
	want := map[float64][2][]string{1: {{"ACME-3"}, {"ACME-5"}}, 1.2: {{"ACME-3", "ACME-6"}, {"ACME-4", "ACME-5"}}, 1.6: {nil, nil}}
	if len(read.Branches) != len(want) {
		t.Errorf("the validation holds %d main branches, want %d", len(read.Branches), len(want))
	}
	for _, b := range read.Branches {
		names, ok := want[b.Branch.Slowdown]
		if !ok || len(b.Breakers) != 1 || b.Breakers[0].Seed != 1 || !slices.Equal(b.Breakers[0].StartedNotRecorded, names[0]) ||
			!slices.Equal(b.Breakers[0].FiledLeftOut, names[1]) {
			t.Errorf("at f %v the branch names the breakers %+v, want, at the seed 1, %v started that the record did not start and %v filed by the record and left out",
				b.Branch.Slowdown, b.Breakers, names[0], names[1])
		}
	}
	var out bytes.Buffer
	if err := v.Write(&out, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"| `main@1` | 1 | ACME-3 | ACME-5 |", "| `main@1.2` | 1 | ACME-3, ACME-6 | ACME-4, ACME-5 |",
		"| `main@1.6` | 1 | none | none |", "gates nothing"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("the validation prints no %q:\n%s", line, out.String())
		}
	}
}

// asJSON reads v as JSON writes it into into, so a test reads a value by its JSON name.
func asJSON(t *testing.T, v, into any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// patchJSON rewrites the JSON object in path with change, as a test makes a file of runs that a
// type does not write.
func patchJSON(t *testing.T, path string, change func(fields map[string]any)) {
	t.Helper()
	var fields map[string]any
	if err := readJSON(path, &fields); err != nil {
		t.Fatal(err)
	}
	change(fields)
	if err := writeJSON(path, fields); err != nil {
		t.Fatal(err)
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

// The comparison's document gives the wait of P0 work and of P1 work (urgentWaits) in hours: for
// each window in its record's scorecard, read from its record by the code a replay is measured by
// (Measure), and for each rule on each branch in the table of every measure, read from the runs'
// snapshot scorecards. The record is priorityWorkWindow's, worked by hand, its waits cut at the
// window's end, 14:00, as it holds no run-out:
//
//	P0: ACME-1, todo at 09:00, its first slot then and its done at 11:00: 0 and 120 minutes.
//	    ACME-3, todo at 09:20, its first slot at 10:15 and its done at 12:15: 55 and 175.
//	    ACME-7, todo from 08:30, before the window, and still todo at its start, so counted from
//	    09:00, and never promoted: 300 and 300, both cut.
//	    The first slot: a median of 55 (0.9167 hours) and a 90th percentile of 300 (5), one cut;
//	    the done: 175 (2.9167) and 300 (5), one cut.
//	P1: ACME-2, todo at 09:10: 0 and 120; ACME-4, todo at 09:40: 45 and 165. The first slot: 22.5
//	    (0.375) and 45 (0.75), none cut; the done: 142.5 (2.375) and 165 (2.75), none cut.
//
// ACME-5, at P2, and ACME-6, with no priority, count in neither. The runs are made up, one seed of
// the main branch under each of two rules: under before-p0, which starts no P0 issue outside the
// cap, the P0 work waits 5 hours to its first slot and to its done, both cut for two of its three
// issues, and no P1 issue counts, so its median and percentile read none; under main the waits are
// the record's.
func TestTheComparisonGivesTheWaitOfUrgentWorkOfEachRecordAndRule(t *testing.T) {
	t.Parallel()
	card, err := Score(priorityWorkWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	wantSlots := map[string]time.Time{"ACME-1": clock(9, 0), "ACME-2": clock(9, 10), "ACME-3": clock(10, 15), "ACME-4": clock(10, 25)}
	wantDone := map[string]time.Time{"ACME-1": clock(11, 0), "ACME-2": clock(11, 10), "ACME-3": clock(12, 15), "ACME-4": clock(12, 25)}
	if !maps.EqualFunc(card.FirstSlot, wantSlots, time.Time.Equal) || !maps.EqualFunc(card.DoneAt, wantDone, time.Time.Equal) {
		t.Fatalf("the record's scorecard gives the first slots %v and the done %v, want %v and %v", card.FirstSlot, card.DoneAt, wantSlots, wantDone)
	}
	recorded := map[string]UrgentWait{
		"P0": {Issues: 3, FirstSlot: WaitMinutes{MedianMinutes: 55, P90Minutes: 300, Cut: 1}, Done: WaitMinutes{MedianMinutes: 175, P90Minutes: 300, Cut: 1}},
		"P1": {Issues: 2, FirstSlot: WaitMinutes{MedianMinutes: 22.5, P90Minutes: 45}, Done: WaitMinutes{MedianMinutes: 142.5, P90Minutes: 165}},
	}
	if !maps.Equal(card.UrgentWaits, recorded) {
		t.Fatalf("the record's scorecard gives the urgent waits %+v, want %+v", card.UrgentWaits, recorded)
	}
	replayed := map[string]map[string]UrgentWait{
		"before-p0":          {"P0": {Issues: 3, FirstSlot: WaitMinutes{MedianMinutes: 300, P90Minutes: 300, Cut: 2}, Done: WaitMinutes{MedianMinutes: 300, P90Minutes: 300, Cut: 2}}},
		record.MainRule.Name: recorded,
	}
	windows := madeUpWindows(record.MainRule.Name, record.MainRule.Name)
	for i := range windows {
		windows[i].Scorecard.UrgentWaits = card.UrgentWaits
	}
	dir := t.TempDir()
	writeMadeUpRuns(t, dir, windows, []record.SlotRule{replayedRule(t, record.MainRule.Name), replayedRule(t, "before-p0")},
		runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			snapshot.UrgentWaits = replayed[rule.Name]
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
	recordRows := []string{"| P0 issues | 3 |", "| P0 to first slot, median | 0.9167 |", "| P0 to first slot, 90th percentile | 5 |",
		"| P0 to first slot, cut waits | 1 |", "| P0 to done, median | 2.9167 |", "| P0 to done, 90th percentile | 5 |", "| P0 to done, cut waits | 1 |",
		"| P1 issues | 2 |", "| P1 to first slot, median | 0.375 |", "| P1 to first slot, 90th percentile | 0.75 |", "| P1 to first slot, cut waits | 0 |",
		"| P1 to done, median | 2.375 |", "| P1 to done, 90th percentile | 2.75 |", "| P1 to done, cut waits | 0 |"}
	ruleRows := []string{"| P0 issues | 3 | 3 |", "| P0 to first slot, median | 0.9167 | 5 |", "| P0 to first slot, 90th percentile | 5 | 5 |",
		"| P0 to first slot, cut waits | 1 | 2 |", "| P0 to done, median | 2.9167 | 5 |", "| P0 to done, 90th percentile | 5 | 5 |",
		"| P0 to done, cut waits | 1 | 2 |", "| P1 issues | 2 | 0 |", "| P1 to first slot, median | 0.375 | none |",
		"| P1 to first slot, 90th percentile | 0.75 | none |", "| P1 to first slot, cut waits | 0 | 0 |", "| P1 to done, median | 2.375 | none |",
		"| P1 to done, 90th percentile | 2.75 | none |", "| P1 to done, cut waits | 0 | 0 |"}
	for _, w := range windows {
		scorecard := section(t, document, "## The scorecard of each window, read from its record", "### "+w.Name+"\n", "\n### ")
		assertComparisonRows(t, scorecard, w.Name+"'s record", recordRows)
		table := section(t, document, "## Every measure for every rule, window and branch", "### "+w.Name+", `"+MainBranchAt.Label()+"` (", "\n### ")
		assertComparisonRows(t, table, w.Name+" on "+MainBranchAt.Label(), ruleRows)
	}
	if !strings.Contains(document, "counts from the window's start") {
		t.Errorf("the document does not say that an issue todo before the window counts from its start")
	}
}

// section is the part of document after the heading part and from the heading within it to the
// next end after that heading, which the test fails where the document holds neither heading.
func section(t *testing.T, document, part, heading, end string) string {
	t.Helper()
	start := strings.Index(document, part)
	if start < 0 {
		t.Fatalf("the document holds no %q", part)
	}
	document = document[start:]
	start = strings.Index(document, heading)
	if start < 0 {
		t.Fatalf("the document holds no %q after %q", heading, part)
	}
	document = document[start:]
	if stop := strings.Index(document[len(heading):], end); stop >= 0 {
		document = document[:len(heading)+stop]
	}
	return document
}
