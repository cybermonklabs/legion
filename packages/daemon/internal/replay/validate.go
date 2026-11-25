package replay

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Gap is how far a replayed number may stand from the record's and still pass a test of spec R5:
// a share of the recorded value, or an amount in the number's own unit.
type Gap struct {
	// Share is the gap as a share of the recorded value, 0.1 for 10%; 0 for a gap that is an
	// amount.
	Share float64 `json:"share,omitempty"`
	// Amount is the gap in the number's own unit, where Share is 0.
	Amount float64 `json:"amount,omitempty"`
}

// Allowed is the most a value may differ from recorded under g.
func (g Gap) Allowed(recorded float64) float64 {
	if g.Share != 0 {
		return g.Share * math.Abs(recorded)
	}
	return g.Amount
}

// Within says whether value stands within g of recorded: a difference at the bound itself is
// within, and one past it is not.
func (g Gap) Within(recorded, value float64) bool {
	allowed := g.Allowed(recorded)
	return math.Abs(value-recorded) <= allowed+tolerance(allowed, recorded, value)
}

// Twice is g doubled: the gap of a forecast across a recorded change of the rule, which carries
// the model's error twice, once for the window and once for the rule (spec R5).
func (g Gap) Twice() Gap { return Gap{Share: 2 * g.Share, Amount: 2 * g.Amount} }

// String is g as a report prints it: a share as a percentage of the recorded value, an amount as
// a number.
func (g Gap) String() string {
	if g.Share != 0 {
		return formatNumber(100*g.Share) + "%"
	}
	return formatNumber(g.Amount)
}

// tolerance is the rounding of floats a comparison of a difference with a bound forgives: a
// billionth of the largest of values, and of 1.
func tolerance(values ...float64) float64 {
	largest := 1.0
	for _, v := range values {
		largest = max(largest, math.Abs(v))
	}
	return 1e-9 * largest
}

// The gaps of spec R5: 10% of the issues finished and of the slots given out on each day; 0.05 of
// the share of holders that rank below a waiting issue; 60 minutes at the median of each issue's
// first slot; and 30% of every other measure of R4, the median wait for a slot and the hours of
// started work that waits only for a slot among them.
var (
	gapTenth     = Gap{Share: 0.1}
	gapThirty    = Gap{Share: 0.3}
	gapPriority  = Gap{Amount: 0.05}
	gapFirstSlot = Gap{Amount: 60}
)

// firstDayPrefix begins the name of each measure MeasuresOf reads over the window's first day
// apart (Scorecard.FirstDayApart).
const firstDayPrefix = "firstDayApart."

// FirstSlotTest is the name of R5's test of each issue's first slot: the median, over the issues,
// of the error of an issue's first slot, in minutes.
const FirstSlotTest = "firstSlot error median"

// R5Gap is spec R5's gap of the measure name (a name MeasuresOf gives), and false for a number of
// a scorecard that no test of R5 reads: the issues the start state could not place, which is no
// measure of R4; the measures of the window's first day apart (assumption 6), which a report
// shows beside the window's own and does not test; and priority taken in today's line's order
// (priorityTakenBanded), which is not one of R5's tests, whose list names the share of priority
// taken once, read by rank alone (priorityTaken), the order of the recorded windows' lines. R7
// reads its error across the change of the rule.
func R5Gap(name string) (Gap, bool) {
	switch {
	case strings.HasPrefix(name, firstDayPrefix), name == "unplaced", name == "priorityTakenBanded":
		return Gap{}, false
	case name == "done", strings.HasPrefix(name, "slotsGivenByDay["):
		return gapTenth, true
	case name == "priorityTaken":
		return gapPriority, true
	default:
		return gapThirty, true
	}
}

// MeasureCheck is one measure of R4 in a validation: its value in the record, the mean of its
// value over the replay's seeds with the lowest and the highest seed's value, R5's gap, and
// whether the replay passes R5's test of it. Tested is false for a number R5 tests not (R5Gap).
type MeasureCheck struct {
	Measure  string  `json:"measure"`
	Recorded float64 `json:"recorded"`
	Replayed float64 `json:"replayed"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
	Gap      Gap     `json:"gap"`
	Tested   bool    `json:"tested"`
	Pass     bool    `json:"pass"`
}

// IssueError is one issue's error in a validation: how far, in minutes, the replay's first slot of
// the issue and its done stand from the record's, each the mean over the replay's seeds. A time
// one side does not give stands at the window's end, the soonest it could come. HasFirstSlot and
// HasDone say the record or a seed gives the issue a first slot, and a done: an issue neither
// gives one of them is in no median of that error.
type IssueError struct {
	Issue        string  `json:"issue"`
	FirstSlot    float64 `json:"firstSlot"`
	HasFirstSlot bool    `json:"hasFirstSlot"`
	Done         float64 `json:"done"`
	HasDone      bool    `json:"hasDone"`
}

// ErrorSummary is the median and the 90th percentile of the issues' errors of one kind, in
// minutes, and how many issues they are over.
type ErrorSummary struct {
	Issues int     `json:"issues"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
}

// BranchValidation is one branch's replay of the rule its window's daemon ran, against the record
// (spec R5): every measure of R4 with R5's test of it, each issue's error in its first slot and in
// its done with their median and 90th percentile, R5's test of the median first-slot error, and the
// tests it fails, by name. A branch that fails one gives no comparison. Beside the tests it gives
// two diagnostics, which no test reads: each seed's first starts against the record's (Starts), and
// each tree whose hours of a closed design gate stand past gapGateHours from the record's
// (GateTrees). On R5's branch of the recorded conflicts it names, seed by seed, each issue that
// breaks the branch (Breakers), which gates nothing.
type BranchValidation struct {
	Branch        BranchAt       `json:"branch"`
	Seeds         []int64        `json:"seeds"`
	Measures      []MeasureCheck `json:"measures"`
	Issues        []IssueError   `json:"issues"`
	FirstSlot     ErrorSummary   `json:"firstSlot"`
	Done          ErrorSummary   `json:"done"`
	FirstSlotPass bool           `json:"firstSlotPass"`
	Failed        []string       `json:"failed"`
	Starts        []SeedStarts   `json:"starts"`
	GateTrees     []TreeGate     `json:"gateTrees"`
	Breakers      []SeedBreakers `json:"breakers,omitempty"`
}

// SeedBreakers is one seed's issues that break R5's branch of the recorded conflicts (spec R5),
// which keeps the window's recorded conflicts and draws none, so that the replay of the recorded
// rule should start no issue the record did not start and file every issue the record filed in the
// window: each issue the seed gives a first slot in the window that the record gives none
// (StartedNotRecorded), and each issue the record filed in the window that the seed does not file
// in it (FiledLeftOut, Scorecard.Filed), each by key in order.
type SeedBreakers struct {
	Seed               int64    `json:"seed"`
	StartedNotRecorded []string `json:"startedNotRecorded"`
	FiledLeftOut       []string `json:"filedLeftOut"`
}

// SeedStarts is one seed's first starts against the record's: how many issues the seed gave a
// first slot in the window that the record gave none (ReplayOnly), how many the record gave one
// that the seed gave none (RecordOnly), and the seed's own median absolute error of the first slot,
// in minutes, over the issues the record or the seed gives one, a missing one standing at --to
// (FirstSlotMedianMinutes), beside R5's statistic, the mean over the seeds of each issue's error
// (assumption 44). A diagnostic, never a test of R5.
type SeedStarts struct {
	Seed                   int64   `json:"seed"`
	ReplayOnly             int     `json:"replayOnly"`
	RecordOnly             int     `json:"recordOnly"`
	FirstSlotMedianMinutes float64 `json:"firstSlotMedianMinutes"`
}

// TreeGate is one tree's hours of a closed design gate in a validation (Scorecard.GateClosedHours):
// the record's, and the mean over the seeds with the lowest and the highest seed's value, a tree
// no scorecard holds standing at 0 there. A diagnostic, never a test of R5.
type TreeGate struct {
	Tree     string  `json:"tree"`
	Recorded float64 `json:"recorded"`
	Replayed float64 `json:"replayed"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
}

// Pass says whether the branch passes every test of R5.
func (b BranchValidation) Pass() bool { return len(b.Failed) == 0 }

// ValidateBranch is the validation of one branch at, whose replays of the window under the rule its
// daemon ran are replays, one scorecard a seed, against recorded, the window's record (spec R5).
// The record's scorecard and each replay's are read from line timelines (Score, and
// RunResult.LineScorecard), so the gaps compare like with like (design 10.1). A measure one side
// does not give stands at 0 there. Beside the tests it counts each seed's first starts against the
// record's (startsOf), names the trees whose gates the replay holds closed otherwise than the record
// (gateTreesOf) and, on R5's branch of the recorded conflicts, the issues that break it
// (breakersOf).
func ValidateBranch(recorded Scorecard, at BranchAt, replays []Scorecard) BranchValidation {
	v := BranchValidation{Branch: at, Seeds: make([]int64, 0, len(replays)), Failed: []string{}, Starts: startsOf(recorded, replays),
		GateTrees: gateTreesOf(recorded, replays)}
	if at.Branch.recordedConflicts() {
		v.Breakers = breakersOf(recorded, replays)
	}
	for _, card := range replays {
		v.Seeds = append(v.Seeds, card.Seed)
	}
	names := measureNames(append([]Scorecard{recorded}, replays...)...)
	recordedValues := valuesOf(recorded)
	seeds := make([]map[string]float64, len(replays))
	for i, card := range replays {
		seeds[i] = valuesOf(card)
	}
	for _, name := range names {
		values := make([]float64, len(replays))
		for i := range replays {
			values[i] = seeds[i][name]
		}
		mean, low, high := spread(values)
		gap, tested := R5Gap(name)
		check := MeasureCheck{Measure: name, Recorded: recordedValues[name], Replayed: mean, Low: low, High: high, Gap: gap, Tested: tested}
		if tested {
			check.Pass = gap.Within(check.Recorded, check.Replayed)
			if !check.Pass {
				v.Failed = append(v.Failed, name)
			}
		}
		v.Measures = append(v.Measures, check)
	}
	v.Issues = issueErrors(recorded, replays)
	var first, done []float64
	for _, issue := range v.Issues {
		if issue.HasFirstSlot {
			first = append(first, issue.FirstSlot)
		}
		if issue.HasDone {
			done = append(done, issue.Done)
		}
	}
	v.FirstSlot, v.Done = summarize(first), summarize(done)
	v.FirstSlotPass = gapFirstSlot.Within(0, v.FirstSlot.Median)
	if !v.FirstSlotPass {
		v.Failed = append(v.Failed, FirstSlotTest)
	}
	return v
}

// startsOf is each of replays' first starts against recorded's (SeedStarts), in their order: the
// issues each gives a first slot in the window that recorded gives none, those recorded gives one
// that it gives none, and its median absolute error of the first slot over the issues either gives
// one, a missing one standing at recorded's To, as R5's statistic counts it (issueErrors).
func startsOf(recorded Scorecard, replays []Scorecard) []SeedStarts {
	starts := make([]SeedStarts, 0, len(replays))
	for _, card := range replays {
		s := SeedStarts{Seed: card.Seed}
		errors := make([]float64, 0, len(recorded.FirstSlot)+len(card.FirstSlot))
		for key, replayed := range card.FirstSlot {
			if _, both := recorded.FirstSlot[key]; !both {
				s.ReplayOnly++
				errors = append(errors, math.Abs(recorded.To.Sub(replayed).Minutes()))
			}
		}
		for key, from := range recorded.FirstSlot {
			to, both := card.FirstSlot[key]
			if !both {
				s.RecordOnly++
				to = recorded.To
			}
			errors = append(errors, math.Abs(to.Sub(from).Minutes()))
		}
		s.FirstSlotMedianMinutes = median(errors)
		starts = append(starts, s)
	}
	return starts
}

// breakersOf is each of replays' issues that break R5's branch of the recorded conflicts
// (SeedBreakers), in their order: the issues each gives a first slot in the window that recorded
// gives none, and those recorded files in the window that it does not.
func breakersOf(recorded Scorecard, replays []Scorecard) []SeedBreakers {
	breakers := make([]SeedBreakers, 0, len(replays))
	for _, card := range replays {
		b := SeedBreakers{Seed: card.Seed, StartedNotRecorded: []string{}, FiledLeftOut: []string{}}
		for _, key := range slices.Sorted(maps.Keys(card.FirstSlot)) {
			if _, both := recorded.FirstSlot[key]; !both {
				b.StartedNotRecorded = append(b.StartedNotRecorded, key)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(recorded.Filed)) {
			if _, both := card.Filed[key]; !both {
				b.FiledLeftOut = append(b.FiledLeftOut, key)
			}
		}
		breakers = append(breakers, b)
	}
	return breakers
}

// gapGateHours is how far a tree's replayed hours of a closed design gate may stand from the
// record's before a validation names the tree (gateTreesOf): 30% of the record's, the gap of R5's
// measures it names no other gap of. A diagnostic, never a test of R5.
var gapGateHours = gapThirty

// gateTreesOf is each tree, in the order of its root's key, whose hours of a closed design gate
// (Scorecard.GateClosedHours) over replays, their mean, stand past gapGateHours from recorded's, a
// tree one scorecard does not give standing at 0 there (TreeGate).
func gateTreesOf(recorded Scorecard, replays []Scorecard) []TreeGate {
	trees := map[string]bool{}
	for tree := range recorded.GateClosedHours {
		trees[tree] = true
	}
	for _, card := range replays {
		for tree := range card.GateClosedHours {
			trees[tree] = true
		}
	}
	var named []TreeGate
	values := make([]float64, len(replays))
	for _, tree := range slices.Sorted(maps.Keys(trees)) {
		for i, card := range replays {
			values[i] = card.GateClosedHours[tree]
		}
		mean, low, high := spread(values)
		if held := recorded.GateClosedHours[tree]; !gapGateHours.Within(held, mean) {
			named = append(named, TreeGate{Tree: tree, Recorded: held, Replayed: mean, Low: low, High: high})
		}
	}
	return named
}

// measureNames is the name of every measure any of cards gives (MeasuresOf), each once, in
// MeasuresOf's order over a scorecard that holds every loop, wait and day any of them holds.
func measureNames(cards ...Scorecard) []string {
	measured := MeasuresOf(unionCard(cards))
	names := make([]string, 0, len(measured))
	for _, m := range measured {
		names = append(names, m.Name)
	}
	return names
}

// unionCard is a scorecard that holds every loop cause, wait word and day any of cards holds, and
// a first day apart where any of them has one, each at zero: the shape MeasuresOf reads the names
// of every measure of cards from.
func unionCard(cards []Scorecard) Scorecard {
	union := Scorecard{Loops: map[string]LoopHours{}, Waits: map[string]WaitHours{}, SlotsGivenByDay: map[string]int{}}
	var days []Scorecard
	for _, card := range cards {
		for cause := range card.Loops {
			union.Loops[cause] = LoopHours{}
		}
		for word := range card.Waits {
			union.Waits[word] = WaitHours{}
		}
		for day := range card.SlotsGivenByDay {
			union.SlotsGivenByDay[day] = 0
		}
		if card.FirstDayApart != nil {
			days = append(days, *card.FirstDayApart)
		}
	}
	if len(days) > 0 {
		day := unionCard(days)
		union.FirstDayApart = &day
	}
	return union
}

// valuesOf is each measure of card (MeasuresOf) by its name.
func valuesOf(card Scorecard) map[string]float64 {
	values := map[string]float64{}
	for _, m := range MeasuresOf(card) {
		values[m.Name] = m.Value
	}
	return values
}

// spread is the mean of values, summed in their order, and the lowest and the highest of them; all
// three 0 for none.
func spread(values []float64) (mean, low, high float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	low, high = values[0], values[0]
	sum := 0.0
	for _, v := range values {
		sum += v
		low, high = min(low, v), max(high, v)
	}
	return sum / float64(len(values)), low, high
}

// issueErrors is each issue's error in its first slot and in its done (IssueError), by issue key in
// order, over every issue the record or a replay gives a first slot or a done; each is read once
// from each scorecard's maps, so the work is the issues times the seeds.
func issueErrors(recorded Scorecard, replays []Scorecard) []IssueError {
	keys := map[string]struct{}{}
	for _, card := range append([]Scorecard{recorded}, replays...) {
		for key := range card.FirstSlot {
			keys[key] = struct{}{}
		}
		for key := range card.DoneAt {
			keys[key] = struct{}{}
		}
	}
	at := func(times map[string]time.Time, key string) (float64, bool) {
		moment, ok := times[key]
		if !ok {
			moment = recorded.To
		}
		return moment.Sub(recorded.From).Minutes(), ok
	}
	errorOf := func(key string, times func(Scorecard) map[string]time.Time) (float64, bool) {
		want, given := at(times(recorded), key)
		if len(replays) == 0 {
			return 0, given
		}
		sum := 0.0
		for _, card := range replays {
			got, ok := at(times(card), key)
			given = given || ok
			sum += math.Abs(got - want)
		}
		return sum / float64(len(replays)), given
	}
	issues := make([]IssueError, 0, len(keys))
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		issue := IssueError{Issue: key}
		issue.FirstSlot, issue.HasFirstSlot = errorOf(key, func(c Scorecard) map[string]time.Time { return c.FirstSlot })
		issue.Done, issue.HasDone = errorOf(key, func(c Scorecard) map[string]time.Time { return c.DoneAt })
		issues = append(issues, issue)
	}
	return issues
}

// summarize is the median and the 90th percentile of values (nearest rank), with their count.
func summarize(values []float64) ErrorSummary {
	return ErrorSummary{Issues: len(values), Median: median(values), P90: percentile(values, 0.9)}
}

// percentile is the p-th quantile of values by the nearest rank, the value at place ⌈p·n⌉ of
// their sorted order; 0 for none.
func percentile(values []float64, p float64) float64 {
	return nearestRank(slices.Sorted(slices.Values(values)), p)
}

// nearestRank is the p-th quantile of sorted, values in order, as percentile gives it.
func nearestRank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// MainBranchAt is the branch spec R5 fixes for a window's main run: the main branch at the
// slowdown factor 1.0, with the fitted conflict rate and the recorded order of first starts.
var MainBranchAt = BranchAt{Branch: MainBranch, Slowdown: 1}

// Validation is a window's validation (spec R5): the replay of the rule its daemon ran, on every
// branch, against its record. A branch that fails a test gives no comparison of rules, and a
// window whose main branch fails gives none at all. Diagnostics is R5's branch of the recorded
// conflicts at each slowdown factor the validation ran it at (DiagnosticBranches) against the same
// record: each names the issues that break it (BranchValidation.Breakers), and its tests are shown
// beside R5's; it gives no comparison and never stands in place of the main branch, so no method
// of a Validation and no decision reads it. CapBacktest is the cap backtest, where the validation
// is one (ValidateConfig.CapBacktest).
type Validation struct {
	Window      string             `json:"window"`
	Rule        string             `json:"rule"`
	Record      Scorecard          `json:"record"`
	Branches    []BranchValidation `json:"branches"`
	Diagnostics []BranchValidation `json:"diagnostics"`
	CapBacktest *CapBacktest       `json:"capBacktest,omitempty"`
}

// branch is the validation of the branch at, and false where the validation did not run it.
func (v Validation) branch(at BranchAt) (BranchValidation, bool) {
	for _, b := range v.Branches {
		if b.Branch.Label() == at.Label() {
			return b, true
		}
	}
	return BranchValidation{}, false
}

// MainPasses says whether the window's main branch (MainBranchAt) ran and passes every test of R5;
// a window whose main branch fails, or did not run, gives no comparison at all.
func (v Validation) MainPasses() bool {
	main, ok := v.branch(MainBranchAt)
	return ok && main.Pass()
}

// Compares says whether the branch at gives a comparison of rules on the window: the window's main
// branch passes R5, and so does at.
func (v Validation) Compares(at BranchAt) bool {
	b, ok := v.branch(at)
	return ok && b.Pass() && v.MainPasses()
}

// NoComparison is every branch of the validation that gives no comparison, in its order: each
// branch where the window's main branch fails, and otherwise each branch that fails a test.
func (v Validation) NoComparison() []BranchAt {
	var none []BranchAt
	for _, b := range v.Branches {
		if !v.Compares(b.Branch) {
			none = append(none, b.Branch)
		}
	}
	return none
}

// Write prints v as Markdown, its headings at depth (2 for "##"): whether the window's main branch
// passes every test of R5, or how many of them it fails, and which branches give no comparison, a
// line for each branch with its verdict, the tests it fails and its issues' errors, each branch's
// and each branch of the recorded conflicts' first starts that only one side made, seed by seed,
// the issues that break R5's branch of the recorded conflicts, seed by seed, and then each branch's
// every measure — recorded, replayed with its lowest and highest seed, the gap, R5's allowance and
// the test — the trees whose design gate it holds closed otherwise than the record and, for each
// branch issuesOf names, each issue's error. Each branch of the recorded conflicts follows, in a
// section of its own that says its measures test nothing, and last the cap backtest, where v holds
// one.
func (v Validation) Write(w io.Writer, depth int, issuesOf func(BranchAt) bool) error {
	p := &printer{w: w}
	heading := strings.Repeat("#", depth)
	p.line("%s %s under `%s`", heading, v.Window, v.Rule)
	p.line("")
	main, ran := v.branch(MainBranchAt)
	switch {
	case !ran:
		p.line("The window's main branch (%s) did not run.", MainBranchAt.Label())
	case main.Pass():
		p.line("The window's main branch (%s) passes every test of R5.", MainBranchAt.Label())
	default:
		p.line("The window's main branch (%s) fails %d of R5's tests.", MainBranchAt.Label(), len(main.Failed))
	}
	if !v.MainPasses() {
		p.line("So the window gives no comparison at all: every branch of it gives none.")
	}
	none := v.NoComparison()
	if len(none) == 0 {
		p.line("Every branch gives a comparison.")
	} else {
		labels := make([]string, 0, len(none))
		for _, at := range none {
			labels = append(labels, "`"+at.Label()+"`")
		}
		p.line("The branches that give no comparison: %s.", strings.Join(labels, ", "))
	}
	p.line("The first-slot test passes where the median error of each issue's first slot is within %s minutes; the measures of the first day apart are shown and not tested.", gapFirstSlot)
	p.line("`priorityTakenBanded`, priority taken in today's line's order, is shown and is not one of R5's tests, which read priority taken by rank alone (`priorityTaken`), the order of the recorded windows' lines; R7 reads its error across the change of the rule.")
	p.line("")
	p.line("| branch | seeds | R5 | tests failed | first slot error: median | 90th percentile | done error: median | 90th percentile |")
	p.line("| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, b := range v.Branches {
		verdict := passWord(b.Pass())
		failed := "none"
		if len(b.Failed) > 0 {
			failed = strings.Join(b.Failed, ", ")
		}
		p.line("| `%s` | %d | %s | %s | %s | %s | %s | %s |", b.Branch.Label(), len(b.Seeds), verdict, failed,
			formatNumber(b.FirstSlot.Median), formatNumber(b.FirstSlot.P90), formatNumber(b.Done.Median), formatNumber(b.Done.P90))
	}
	p.line("")
	p.line("Beside the tests, and testing nothing: the first starts that only one side made and the median absolute first-slot error for each branch and seed.")
	p.line("A first start is an issue's first slot in the window.")
	p.line("")
	p.line("| branch | seed | first starts the record did not make | first starts the replay did not make | median first-slot error (minutes) |")
	p.line("| --- | --- | --- | --- | --- |")
	for _, branches := range [][]BranchValidation{v.Branches, v.Diagnostics} {
		for _, b := range branches {
			for _, s := range b.Starts {
				p.line("| `%s` | %d | %d | %d | %s |", b.Branch.Label(), s.Seed, s.ReplayOnly, s.RecordOnly, formatNumber(s.FirstSlotMedianMinutes))
			}
		}
	}
	if len(v.Diagnostics) > 0 {
		p.line("")
		p.line("%s# The issues that break R5's branch of the recorded conflicts", heading)
		p.line("")
		p.line("On the branch that keeps the window's recorded conflicts and draws none, `%s`, at each slowdown factor, the replay of the recorded rule should start no issue that the record did not start in the window, and file each issue that the record filed in it.",
			DiagnosticBranches()[0].Branch.Name)
		p.line("Each seed's issues that break either are named here, and the naming gates nothing: no test of R5, no verdict of the window and no decision of a comparison reads them.")
		p.line("")
		p.line("| branch | seed | issues it starts that the record did not start | issues the record filed that it leaves out |")
		p.line("| --- | --- | --- | --- |")
		for _, b := range v.Diagnostics {
			for _, s := range b.Breakers {
				p.line("| `%s` | %d | %s | %s |", b.Branch.Label(), s.Seed, keyList(s.StartedNotRecorded), keyList(s.FiledLeftOut))
			}
		}
	}
	for _, b := range v.Branches {
		p.line("")
		p.line("%s# `%s`: each measure", heading, b.Branch.Label())
		p.line("")
		b.write(p, issuesOf)
	}
	for _, b := range v.Diagnostics {
		p.line("")
		p.line("%s# `%s`: R5's branch of the recorded conflicts, whose measures test nothing", heading, b.Branch.Label())
		p.line("")
		p.line("Its tests are shown and judge nothing: it gives no comparison, and it is read beside the main branch, never in place of the main branch.")
		p.line("")
		b.write(p, issuesOf)
	}
	if v.CapBacktest != nil && p.err == nil {
		p.line("")
		p.err = v.CapBacktest.Write(w, depth+1)
	}
	return p.err
}

// keyList is keys as a report lists them, separated by commas, or "none".
func keyList(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// write prints b's every measure with R5's test of it, its issues' errors, the trees whose design
// gate it holds closed otherwise than the record, and, where issuesOf names b, each issue's error.
func (b BranchValidation) write(p *printer, issuesOf func(BranchAt) bool) {
	p.line("| measure | recorded | replayed (lowest, highest seed) | gap | allowed | test |")
	p.line("| --- | --- | --- | --- | --- | --- |")
	for _, m := range b.Measures {
		allowed, test := "", "not tested"
		if m.Tested {
			allowed, test = formatNumber(m.Gap.Allowed(m.Recorded))+" ("+m.Gap.String()+")", passWord(m.Pass)
		}
		p.line("| %s | %s | %s | %s | %s | %s |", m.Measure, formatNumber(m.Recorded), spreadCell(m.Replayed, m.Low, m.High, len(b.Seeds)),
			formatNumber(m.Replayed-m.Recorded), allowed, test)
	}
	first := passWord(b.FirstSlotPass)
	p.line("| %s | 0 | %s | %s | %s | %s |", FirstSlotTest, formatNumber(b.FirstSlot.Median), formatNumber(b.FirstSlot.Median),
		formatNumber(gapFirstSlot.Amount), first)
	p.line("")
	p.line("Each issue's error, in minutes: the first slot over %d issues, median %s and 90th percentile %s; the done over %d issues, median %s and 90th percentile %s.",
		b.FirstSlot.Issues, formatNumber(b.FirstSlot.Median), formatNumber(b.FirstSlot.P90), b.Done.Issues, formatNumber(b.Done.Median), formatNumber(b.Done.P90))
	p.line("")
	if len(b.GateTrees) == 0 {
		p.line("No tree's design gate stood closed in the replay more than %s longer or shorter than in the record, a diagnostic that tests nothing.", gapGateHours)
	} else {
		p.line("The trees whose design gate stood closed in the replay more than %s longer or shorter than in the record, a diagnostic that tests nothing:", gapGateHours)
		p.line("")
		p.line("| tree | hours closed, recorded | hours closed, replayed (lowest, highest seed) |")
		p.line("| --- | --- | --- |")
		for _, tree := range b.GateTrees {
			p.line("| %s | %s | %s |", tree.Tree, formatNumber(tree.Recorded), spreadCell(tree.Replayed, tree.Low, tree.High, len(b.Seeds)))
		}
	}
	if issuesOf == nil || !issuesOf(b.Branch) {
		return
	}
	p.line("")
	p.line("| issue | first slot error (minutes) | done error (minutes) |")
	p.line("| --- | --- | --- |")
	for _, issue := range b.Issues {
		first, done := "none", "none"
		if issue.HasFirstSlot {
			first = formatNumber(issue.FirstSlot)
		}
		if issue.HasDone {
			done = formatNumber(issue.Done)
		}
		p.line("| %s | %s | %s |", issue.Issue, first, done)
	}
}

// passWord is the word a report writes for a test that passed, "passes", or failed, "fails".
func passWord(pass bool) string {
	if pass {
		return "passes"
	}
	return "fails"
}

// printer writes lines to w and keeps the first error, so a report is written without a check
// after each line.
type printer struct {
	w   io.Writer
	err error
}

// line writes one line, format with args, unless an earlier write failed.
func (p *printer) line(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
}

// formatNumber is v as every report prints a number: an integer as one, any other number to four
// decimals with no zeros after its last digit, and never a negative zero.
func formatNumber(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Sprint(v)
	}
	text := fmt.Sprintf("%.4f", v)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "-0" {
		return "0"
	}
	return text
}

// spreadCell is a mean over n seeds as a report prints it: the mean alone for one seed, and the
// mean with the lowest and the highest seed's value in brackets for more.
func spreadCell(mean, low, high float64, n int) string {
	if n <= 1 {
		return formatNumber(mean)
	}
	return fmt.Sprintf("%s (%s, %s)", formatNumber(mean), formatNumber(low), formatNumber(high))
}
