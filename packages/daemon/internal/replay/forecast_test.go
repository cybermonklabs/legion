package replay

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// forecastCheckJSON is one number of a forecast as its JSON gives it, and forecastJSON the forecast.
type forecastCheckJSON struct {
	Quantity   string  `json:"quantity"`
	Kind       string  `json:"kind"`
	Causes     []Cause `json:"causes"`
	Forecast   float64 `json:"forecast"`
	After      float64 `json:"after"`
	Gap        Gap     `json:"gap"`
	Pass       bool    `json:"pass"`
	Measured   bool    `json:"measured"`
	Error      float64 `json:"error"`
	Own        float64 `json:"own"`
	Moved      bool    `json:"moved"`
	Control    float64 `json:"control"`
	CannotTell bool    `json:"cannotTell"`
}

type forecastJSON struct {
	BeforeRule string              `json:"beforeRule"`
	Checks     []forecastCheckJSON `json:"checks"`
}

// check is the number of f named quantity, which the test fails where f holds none.
func (f forecastJSON) check(t *testing.T, quantity string) forecastCheckJSON {
	t.Helper()
	i := slices.IndexFunc(f.Checks, func(c forecastCheckJSON) bool { return c.Quantity == quantity })
	if i < 0 {
		t.Fatalf("the forecast holds no %s", quantity)
	}
	return f.Checks[i]
}

// The windows of a made-up forecast across the change (madeUpForecast): window-1, recorded before
// the change under forecastBefore, and window-2, recorded after it under forecastAfter.
const (
	forecastBefore = "legion-v19.57.3"
	forecastAfter  = "legion-v20.42.5"
)

// madeUpForecast is the forecast across the change from window-1 to window-2 under forecastAfter,
// read back (Runs.Forecast) from made-up runs of the main branch at one seed of each window under
// each of the two rules: every record's and every run's line scorecard is sampleCard, changed by
// change and then, as JSON, by patch, each given the window and the rule of the run ("" for the
// window's record).
func madeUpForecast(t *testing.T, change func(window, rule string, card *Scorecard), patch func(window, rule string, fields map[string]any)) (ForecastResult, forecastJSON) {
	t.Helper()
	windows := madeUpWindows(forecastBefore, forecastAfter)
	for i := range windows {
		change(windows[i].Name, "", &windows[i].Scorecard)
	}
	dir := t.TempDir()
	rules := []record.SlotRule{replayedRule(t, forecastBefore), replayedRule(t, forecastAfter)}
	writeMadeUpRuns(t, dir, windows, rules, runPlan{branches: []BranchAt{MainBranchAt}, perDraw: 1},
		func(w windowRecord, r int, rule record.SlotRule, at BranchAt, seed int64) (Scorecard, Scorecard, runRecord) {
			snapshot, line, ran := madeUpRun(w, r, rule, at, seed)
			change(w.Name, rule.Name, &line)
			return snapshot, line, ran
		})
	for _, w := range windows {
		patchJSON(t, filepath.Join(dir, w.Name, recordFile), func(fields map[string]any) { patch(w.Name, "", fields["scorecard"].(map[string]any)) })
		for _, rule := range rules {
			patchJSON(t, filepath.Join(runDir(dir, w.Name, rule.Name, MainBranchAt, 1), lineScorecardFile), func(fields map[string]any) {
				patch(w.Name, rule.Name, fields)
			})
		}
	}
	runs, err := ReadRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := runs.Forecast(windows[0].Name, windows[1].Name, forecastAfter)
	if err != nil {
		t.Fatal(err)
	}
	var read forecastJSON
	asJSON(t, f, &read)
	return f, read
}

// The forecast's control (spec R5): forecast also replays the window after the change under the
// rule before it, and the window before under its own rule. A quantity the change moves is one where
// the forecast stands more than twice its gap from that replay of the window before under its own
// rule; where the control too stands within twice its gap of the record after the change on such a
// quantity, the forecast cannot tell the two rules apart on it, and the result says so. The runs are
// made up, each window's record sampleCard over six hours, with 20 done and 0.2 of priority taken:
// window-1, the window before, replays 10 done and 0.5 of priority taken under its own rule and 20
// and 0.2 under the rule after; the control, window-2 under the rule before, replays 21 done and 0.5
// of priority taken. So the change moves the issues finished, from 10 to 20, and the control's 21
// stands within 20% of the record's 20: the forecast cannot tell the rules apart on done per hour. It
// moves priority taken, from 0.5 to 0.2, and the control's 0.5 stands 0.3 from the record's 0.2, past
// 0.1: the forecast tells them apart there. It moves nothing else, inFlight.average among the rest. A
// forecast run on two built windows replays the window after under the rule before:
// laterStartWindow, recorded under keep-past-review, forecast under main against firstStartsWindow,
// recorded under main, replays firstStartsWindow under keep-past-review too.
func TestForecastControlSaysWhereItCannotTell(t *testing.T) {
	f, read := madeUpForecast(t, func(window, rule string, card *Scorecard) {
		switch {
		case window == "window-1" && rule == forecastBefore:
			card.Done, card.PriorityTaken = 10, 0.5
		case window == "window-2" && rule == forecastBefore:
			card.Done, card.PriorityTaken = 21, 0.5
		}
	}, func(string, string, map[string]any) {})
	if read.BeforeRule != forecastBefore {
		t.Errorf("the forecast's control runs under %q, want the rule before the change, %s", read.BeforeRule, forecastBefore)
	}
	const hours = compareHours
	done := read.check(t, "done per hour")
	assertNear(t, "done per hour, the window before under its own rule", done.Own, 10.0/hours)
	assertNear(t, "done per hour, the control", done.Control, 21.0/hours)
	if !done.Pass || !done.Moved || !done.CannotTell {
		t.Errorf("done per hour passes %t, moved %t, cannot tell %t; want a pass that the change moves and that the control cannot tell apart",
			done.Pass, done.Moved, done.CannotTell)
	}
	taken := read.check(t, "priorityTaken")
	assertNear(t, "priorityTaken, the control", taken.Control, 0.5)
	if !taken.Pass || !taken.Moved || taken.CannotTell {
		t.Errorf("priorityTaken passes %t, moved %t, cannot tell %t; want a pass that the change moves and the control tells apart",
			taken.Pass, taken.Moved, taken.CannotTell)
	}
	for _, c := range read.Checks {
		if c.Quantity != "done per hour" && c.Quantity != "priorityTaken" && (c.Moved || c.CannotTell) {
			t.Errorf("%s reads moved %t and cannot tell %t, want neither: the change moves only the issues finished and priority taken",
				c.Quantity, c.Moved, c.CannotTell)
		}
	}
	var out bytes.Buffer
	if err := f.Write(&out, 1); err != nil {
		t.Fatal(err)
	}
	said := ""
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "cannot tell the two rules apart on") {
			said = line
		}
	}
	if !strings.Contains(said, "cannot tell the two rules apart on done per hour") || strings.Contains(said, "priorityTaken") {
		t.Errorf("the forecast says %q, want that it cannot tell the two rules apart on done per hour alone:\n%s", said, out.String())
	}

	dir := t.TempDir()
	for name, window := range map[string]Window{"window-before": laterStartWindow(t), "window-after": firstStartsWindow(t)} {
		if err := window.Write(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	out2 := t.TempDir()
	built, err := RunForecast(context.Background(), ForecastConfig{Before: filepath.Join(dir, "window-before"), After: filepath.Join(dir, "window-after"),
		Rule: mainRule(t), PGDSN: replayDSN(t), Parallel: 2, Out: out2, perDraw: 1})
	if err != nil {
		t.Fatalf("the forecast of the built windows: %v", err)
	}
	var builtRead forecastJSON
	asJSON(t, built, &builtRead)
	if builtRead.BeforeRule != "keep-past-review" {
		t.Errorf("the forecast of the built windows names the rule before the change %q, want keep-past-review", builtRead.BeforeRule)
	}
	if _, err := os.Stat(filepath.Join(out2, "window-after", "keep-past-review", partFile)); err != nil {
		t.Errorf("the forecast of the built windows replays no control, the window after under the rule before: %v", err)
	}
	if len(built.Reused) != 0 {
		t.Errorf("the first forecast into an empty directory reuses %+v, want nothing", built.Reused)
	}
	again, err := RunForecast(context.Background(), ForecastConfig{Before: filepath.Join(dir, "window-before"), After: filepath.Join(dir, "window-after"),
		Rule: mainRule(t), PGDSN: replayDSN(t), Parallel: 2, Out: out2, perDraw: 1})
	if err != nil {
		t.Fatalf("the forecast of the built windows again: %v", err)
	}
	reused := map[string]bool{}
	for _, part := range again.Reused {
		reused[part.Window+" under "+part.Rule] = part.Build == unknownBuild
	}
	wantReused := map[string]bool{"window-before under main": true, "window-before under keep-past-review": true, "window-after under main": true,
		"window-after under keep-past-review": true}
	if !maps.Equal(reused, wantReused) {
		t.Errorf("the forecast run again into its own runs reuses %v, want each of its four parts, made by the build %s", reused, unknownBuild)
	}
	var printed bytes.Buffer
	if err := again.Write(&printed, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "The runs held window-after under `keep-past-review` already, made by the legion binary of `"+unknownBuild+"`") {
		t.Errorf("the forecast does not say which parts it reused:\n%s", printed.String())
	}
	if _, err := RunForecast(context.Background(), ForecastConfig{Before: filepath.Join(dir, "window-before"), After: filepath.Join(dir, "window-after"),
		Rule: mainRule(t), PGDSN: replayDSN(t), Parallel: 2, Out: out2, BuildCommit: strings.Repeat("2", 40), perDraw: 1}); err == nil ||
		!strings.Contains(err.Error(), "made by the legion binary of "+unknownBuild) {
		t.Errorf("a forecast by another build into runs another build made is not refused: %v", err)
	}

	// A forecast or a validation of the main branch alone, run into a validation's directory,
	// leaves the part every branch and branch of the recorded conflicts the directory held.
	wall, err := BranchNamed("main+timing=wall")
	if err != nil {
		t.Fatal(err)
	}
	held := partRecord{Branches: []partRun{{Branch: "main", Slowdown: 1, Seeds: []int64{1, 2}}, {Branch: wall.Name, Slowdown: 1, Seeds: []int64{1}}},
		Diagnostics: []partRun{{Branch: "main+conflicts=recorded", Slowdown: 1, Seeds: []int64{1}}}}
	merged := held.with(partRecord{Branches: []partRun{{Branch: "main", Slowdown: 1, Seeds: []int64{1}}}, BreakEven: []partRun{}})
	if len(merged.Branches) != 2 || merged.Branches[0].label() != MainBranchAt.Label() || !slices.Equal(merged.Branches[0].Seeds, []int64{1}) ||
		merged.Branches[1].Branch != wall.Name || len(merged.Diagnostics) != 1 {
		t.Errorf("the part held there already, merged with a run of the main branch alone, reads %+v", merged)
	}
}

// The forecast has a quantity for every measure of R4 (plan t34 (d)), each a rate per hour, an average
// or a share: beside those it had, each maximum as the mean over the window's days of each day's
// maximum (dailyMaxima), the wait of urgent work of P0 and of P1 apart as its issues per hour, its
// mean wait to the first slot and to done, a cut wait at its cut length (urgentMeans), and its cut
// waits as a share of its issues, and the send-back loops of the causes the workflow change redefines
// together, in their group of plan D3 (loops[redefined]). The gap of each new quantity is the
// replay's own error on its measure, which R7 reads: the larger of the two windows' errors, each
// window replayed under its own rule against its own record. The runs are made up over sampleCard:
// the issues in flight reach a daily maximum of 7 in each record, 8 in window-1's replay under its own
// rule and 9.5 in window-2's, so the error is 2.5 and the forecast's 11 stands 4 from the record
// after, within twice it; P0's mean wait to its first slot is 30 minutes in each record and in
// window-2's replay under its own rule and 40 in window-1's, so the error is 10, and the forecast's 55
// stands 25 from the record after, past twice it. Each number the forecast had keeps R5's gap.
func TestForecastHasAQuantityForEveryMeasure(t *testing.T) {
	maxima := func(inFlight float64) map[string]any {
		return map[string]any{"waitOnlyForSlot": 3.0, "startedWithoutSlot": 4.0, "pastReviewHolding": 2.0, "pastReviewNotHolding": 2.0, "inFlight": inFlight}
	}
	urgent := func(firstSlot float64) map[string]any {
		return map[string]any{"P0": map[string]any{"firstSlotMinutes": firstSlot, "doneMinutes": 120.0}}
	}
	f, read := madeUpForecast(t, func(string, string, *Scorecard) {}, func(window, rule string, fields map[string]any) {
		fields["urgentWaits"] = map[string]any{"P0": map[string]any{"issues": 2, "firstSlot": map[string]any{"medianMinutes": 30, "p90Minutes": 60, "cut": 0},
			"done": map[string]any{"medianMinutes": 120, "p90Minutes": 240, "cut": 1}}}
		fields["dailyMaxima"], fields["urgentMeans"] = maxima(7), urgent(30)
		switch {
		case window == "window-1" && rule == forecastBefore:
			fields["dailyMaxima"], fields["urgentMeans"] = maxima(8), urgent(40)
		case window == "window-2" && rule == forecastAfter:
			fields["dailyMaxima"] = maxima(9.5)
		case window == "window-1" && rule == forecastAfter:
			fields["dailyMaxima"], fields["urgentMeans"] = maxima(11), urgent(55)
		}
	})
	type measure struct{ measure, quantity, kind string }
	measures := []measure{
		{"done", "done per hour", QuantityRate},
		{"slotsGivenByDay", "slotsGiven per hour", QuantityRate},
		{"priorityTaken", "priorityTaken", QuantityShare},
		{"slotHoursInTurn", "slotHoursInTurn per hour", QuantityAverage},
		{"slotHoursNoTurn", "slotHoursNoTurn per hour", QuantityAverage},
	}
	added := map[string]bool{}
	for _, reading := range []string{"waitOnlyForSlot", "startedWithoutSlot", "pastReviewHolding", "pastReviewNotHolding", "inFlight"} {
		measures = append(measures, measure{reading + ".average", reading + ".average", QuantityAverage},
			measure{reading + ".hours", reading + ".average", QuantityAverage}, measure{reading + ".max", reading + ".max daily mean", QuantityAverage})
		added[reading+".max daily mean"] = true
	}
	for _, cause := range Causes {
		loop := "loops[" + string(cause) + "]"
		if slices.Contains(ForecastGroups[GroupRedefined], cause) {
			loop = "loops[" + string(GroupRedefined) + "]"
			added[loop+".rounds per hour"], added[loop+".hours per hour"] = true, true
		}
		measures = append(measures, measure{"loops[" + string(cause) + "].rounds", loop + ".rounds per hour", QuantityRate},
			measure{"loops[" + string(cause) + "].hours", loop + ".hours per hour", QuantityRate})
	}
	for _, word := range []WaitEnd{WaitLinkRemoved, WaitOpen} {
		measures = append(measures, measure{"waits[" + string(word) + "].count", "waits[" + string(word) + "].count per hour", QuantityRate},
			measure{"waits[" + string(word) + "].hours", "waits[" + string(word) + "].hours per hour", QuantityRate})
	}
	for _, priority := range []string{"P0", "P1"} {
		wait := "urgentWaits[" + priority + "]"
		for _, m := range []measure{{wait + ".issues", wait + ".issues per hour", QuantityRate},
			{wait + ".firstSlot", wait + ".firstSlot.meanMinutes", QuantityAverage}, {wait + ".firstSlot.cut", wait + ".firstSlot.cut share", QuantityShare},
			{wait + ".done", wait + ".done.meanMinutes", QuantityAverage}, {wait + ".done.cut", wait + ".done.cut share", QuantityShare}} {
			measures = append(measures, m)
			added[m.quantity] = true
		}
	}
	for _, m := range measures {
		i := slices.IndexFunc(read.Checks, func(c forecastCheckJSON) bool { return c.Quantity == m.quantity })
		switch {
		case i < 0:
			t.Errorf("the forecast has no quantity for the measure %s of R4: it holds no %s", m.measure, m.quantity)
		case read.Checks[i].Kind != m.kind:
			t.Errorf("the forecast reads %s as %s, a %s, want a %s", m.measure, m.quantity, read.Checks[i].Kind, m.kind)
		}
	}
	for _, c := range read.Checks {
		if !slices.Contains([]string{QuantityRate, QuantityAverage, QuantityShare}, c.Kind) {
			t.Errorf("the forecast scores %s as %q, which is no rate per hour, average or share", c.Quantity, c.Kind)
		}
		if c.Measured != added[c.Quantity] {
			t.Errorf("%s's gap is the replay's own error: %t, want %t, which only a new quantity's is", c.Quantity, c.Measured, added[c.Quantity])
		}
	}
	if redefined := read.check(t, "loops[redefined].rounds per hour"); !slices.Equal(redefined.Causes, ForecastGroups[GroupRedefined]) {
		t.Errorf("loops[redefined] counts the causes %v, want the redefined group's %v", redefined.Causes, ForecastGroups[GroupRedefined])
	}
	if done := read.check(t, "done per hour"); done.Gap.String() != "20%" {
		t.Errorf("done per hour stands within %s, want twice R5's gap, 20%%", done.Gap)
	}
	inFlight := read.check(t, "inFlight.max daily mean")
	assertNear(t, "inFlight.max daily mean, the forecast", inFlight.Forecast, 11)
	assertNear(t, "inFlight.max daily mean, after the change", inFlight.After, 7)
	assertNear(t, "inFlight.max daily mean, the replay's own error", inFlight.Error, 2.5)
	assertNear(t, "inFlight.max daily mean, twice the error", inFlight.Gap.Allowed(inFlight.After), 5)
	if !inFlight.Pass {
		t.Errorf("inFlight.max daily mean fails, 4 from the record after the change and within twice the replay's own error of 2.5")
	}
	firstSlot := read.check(t, "urgentWaits[P0].firstSlot.meanMinutes")
	assertNear(t, "P0's mean wait to its first slot, the replay's own error", firstSlot.Error, 10)
	if firstSlot.Pass {
		t.Errorf("P0's mean wait to its first slot passes, 25 minutes from the record after the change and past twice the replay's own error of 10")
	}
	assertNear(t, "P0's issues per hour", read.check(t, "urgentWaits[P0].issues per hour").After, 2.0/compareHours)
	assertNear(t, "P0's cut waits to done, a share of its issues", read.check(t, "urgentWaits[P0].done.cut share").After, 0.5)
	var out bytes.Buffer
	if err := f.Write(&out, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "the replay's own error") {
		t.Errorf("the forecast does not say which gaps are the replay's own error:\n%s", out.String())
	}
}
