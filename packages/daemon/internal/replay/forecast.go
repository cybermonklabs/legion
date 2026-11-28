package replay

import (
	"io"
	"maps"
	"math"
	"slices"
	"strings"
)

// The kinds of number a forecast across a recorded change of the rule compares (spec R5): rates per
// hour, averages (over time, or over the issues of the wait of urgent work) and shares. The two
// windows differ in length and in work, so a total, which grows with the window, is never compared.
const (
	QuantityRate    = "rate per hour"
	QuantityAverage = "average"
	QuantityShare   = "share"
)

// LoopsAll is the name of the total of all send-back loops, every cause of Causes together, in a
// forecast: with LoopsRedefined, the one place a cause the workflow change redefines is scored
// (plan D3).
const LoopsAll = "loops[all]"

// LoopsRedefined is the name of the total of the send-back loops of the causes the workflow change
// redefines (ForecastGroups[GroupRedefined]) in a forecast: each such cause scored in its group of
// plan D3, never apart.
const LoopsRedefined = "loops[" + string(GroupRedefined) + "]"

// quantity is one number a forecast compares: its name, its kind, the gap of the measure it is read
// from — R5's, or, where measured, the replay's own error on the measure across the change
// (ForecastCheck.Measured) — the send-back causes it counts (for a loop's), whether it rests on the
// priority Dispatch resolves (the wait of urgent work's), and how it is read from a scorecard.
type quantity struct {
	name     string
	kind     string
	gap      Gap
	measured bool
	resolved bool
	causes   []Cause
	of       func(Scorecard) float64
}

// forecastQuantities is every number a forecast across the change compares, read the same way from
// each of cards: the issues finished and the slots given out per hour; the two readings of started
// work without a slot, the issues past their review with a slot and without, the issues in flight
// and the slots held with a worker in a turn and with none, each as an average over time, and each
// count's mean daily maximum (Scorecard.DailyMaxima); the share of priority taken; the rounds and
// hours per hour of all send-back loops together, of the causes the workflow change redefines
// together and of each cause both releases define the same way apart (ForecastGroups); the count and
// hours per hour of the waits on another issue by each word that ended one in any of cards; and,
// for P0 and for P1 apart, the issues of the wait of urgent work per hour, their mean wait to the
// first slot and to done (Scorecard.UrgentMeans) and the share of each that is cut. The numbers plan
// t34 (d) added, one for each measure of R4 the forecast had none for — the daily maxima, the
// redefined causes' loops and the wait of urgent work — take the replay's own error on their
// measure as their gap (measured); every other number R5's.
func forecastQuantities(cards ...Scorecard) []quantity {
	perHour := func(card Scorecard, total float64) float64 {
		hours := card.To.Sub(card.From).Hours()
		if hours <= 0 {
			return 0
		}
		return total / hours
	}
	rate := func(name string, gap Gap, causes []Cause, total func(Scorecard) float64) quantity {
		return quantity{name: name + " per hour", kind: QuantityRate, gap: gap, causes: causes,
			of: func(card Scorecard) float64 { return perHour(card, total(card)) }}
	}
	average := func(name string, gap Gap, of func(Scorecard) float64) quantity {
		return quantity{name: name, kind: QuantityAverage, gap: gap, of: of}
	}
	measured := func(q quantity) quantity {
		q.gap, q.measured = Gap{}, true
		return q
	}
	share := func(part, whole int) float64 {
		if whole == 0 {
			return 0
		}
		return float64(part) / float64(whole)
	}
	loops := func(causes []Cause, of func(LoopHours) float64) func(Scorecard) float64 {
		return func(card Scorecard) float64 {
			total := 0.0
			for _, cause := range causes {
				total += of(card.Loops[string(cause)])
			}
			return total
		}
	}
	rounds := func(l LoopHours) float64 { return float64(l.Rounds) }
	hours := func(l LoopHours) float64 { return l.Hours }
	// loopRates is the rounds and the hours per hour of the send-back loops of causes, named name.
	loopRates := func(name string, causes []Cause) []quantity {
		return []quantity{rate(name+".rounds", gapThirty, causes, loops(causes, rounds)), rate(name+".hours", gapThirty, causes, loops(causes, hours))}
	}
	quantities := []quantity{
		rate("done", gapTenth, nil, func(card Scorecard) float64 { return float64(card.Done) }),
		rate("slotsGiven", gapTenth, nil, func(card Scorecard) float64 {
			total := 0
			for _, given := range card.SlotsGivenByDay {
				total += given
			}
			return float64(total)
		}),
		average("waitOnlyForSlot.average", gapThirty, func(card Scorecard) float64 { return card.WaitOnlyForSlot.Average }),
		average("startedWithoutSlot.average", gapThirty, func(card Scorecard) float64 { return card.StartedWithoutSlot.Average }),
		average("pastReviewHolding.average", gapThirty, func(card Scorecard) float64 { return card.PastReviewHolding.Average }),
		average("pastReviewNotHolding.average", gapThirty, func(card Scorecard) float64 { return card.PastReviewNotHolding.Average }),
		average("inFlight.average", gapThirty, func(card Scorecard) float64 { return card.InFlight.Average }),
		average("slotHoursInTurn per hour", gapThirty, func(card Scorecard) float64 { return perHour(card, card.SlotHoursInTurn) }),
		average("slotHoursNoTurn per hour", gapThirty, func(card Scorecard) float64 { return perHour(card, card.SlotHoursNoTurn) }),
	}
	for _, count := range dailyMaximaCounts {
		quantities = append(quantities, measured(average(count.name+".max daily mean", Gap{},
			func(card Scorecard) float64 { return card.DailyMaxima[count.name] })))
	}
	quantities = append(quantities, quantity{name: "priorityTaken", kind: QuantityShare, gap: gapPriority, of: func(card Scorecard) float64 { return card.PriorityTaken }})
	quantities = append(quantities, loopRates(LoopsAll, Causes)...)
	for _, q := range loopRates(LoopsRedefined, ForecastGroups[GroupRedefined]) {
		quantities = append(quantities, measured(q))
	}
	for _, cause := range ForecastGroups[GroupSame] {
		quantities = append(quantities, loopRates("loops["+string(cause)+"]", []Cause{cause})...)
	}
	words := map[string]struct{}{}
	for _, card := range cards {
		for word := range card.Waits {
			words[word] = struct{}{}
		}
	}
	for _, word := range slices.Sorted(maps.Keys(words)) {
		quantities = append(quantities,
			rate("waits["+word+"].count", gapThirty, nil, func(card Scorecard) float64 { return float64(card.Waits[word].Count) }),
			rate("waits["+word+"].hours", gapThirty, nil, func(card Scorecard) float64 { return card.Waits[word].Hours }))
	}
	resolved := func(q quantity) quantity {
		q.resolved = true
		return measured(q)
	}
	for _, priority := range urgentPriorities {
		key := priority.String()
		wait := "urgentWaits[" + key + "]"
		quantities = append(quantities,
			resolved(rate(wait+".issues", Gap{}, nil, func(card Scorecard) float64 { return float64(card.UrgentWaits[key].Issues) })),
			resolved(average(wait+".firstSlot.meanMinutes", Gap{}, func(card Scorecard) float64 { return card.UrgentMeans[key].FirstSlotMinutes })),
			resolved(quantity{name: wait + ".firstSlot.cut share", kind: QuantityShare, of: func(card Scorecard) float64 {
				return share(card.UrgentWaits[key].FirstSlot.Cut, card.UrgentWaits[key].Issues)
			}}),
			resolved(average(wait+".done.meanMinutes", Gap{}, func(card Scorecard) float64 { return card.UrgentMeans[key].DoneMinutes })),
			resolved(quantity{name: wait + ".done.cut share", kind: QuantityShare, of: func(card Scorecard) float64 {
				return share(card.UrgentWaits[key].Done.Cut, card.UrgentWaits[key].Issues)
			}}))
	}
	return quantities
}

// ForecastCheck is one number of a forecast across a recorded change of the rule: its name and
// kind, the send-back causes it counts, the forecast's value (the mean over the seeds, with the
// lowest and the highest seed's), the value in the record of the window after the change, twice
// its gap, and whether the forecast stands within it. Beside it stand the forecast's control and
// whether the change moves the number (spec R5).
type ForecastCheck struct {
	Quantity string  `json:"quantity"`
	Kind     string  `json:"kind"`
	Causes   []Cause `json:"causes,omitempty"`
	Forecast float64 `json:"forecast"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
	After    float64 `json:"after"`
	Gap      Gap     `json:"gap"`
	Pass     bool    `json:"pass"`
	// Measured says the number's gap is the replay's own error on its measure (Error), which R7
	// reads (plan t34 (d)): the larger of the two windows' errors, each window replayed under its
	// own rule against its own record, the mean over the seeds against the record's value; otherwise
	// its gap is R5's. Scored is false for a measured number whose cards hold no replay of each
	// window under its own rule to measure the error from: it is not tested, and passes not.
	Measured bool    `json:"measured,omitempty"`
	Error    float64 `json:"error,omitempty"`
	Scored   bool    `json:"scored"`
	// Own is the mean over the seeds of the window before the change replayed under its own rule,
	// and Moved says the forecast stands more than twice the gap from it: the change moves the
	// number. Control is the mean over the seeds of the forecast's control, the window after the
	// change replayed under the rule before it, with its lowest and highest seed's; ControlWithin
	// says it too stands within twice the gap of the record after the change; and CannotTell says
	// the change moves the number, the forecast passes on it, and so does the control: the forecast
	// cannot tell the two rules apart on it.
	Own           float64 `json:"own"`
	Moved         bool    `json:"moved"`
	Control       float64 `json:"control"`
	ControlLow    float64 `json:"controlLow"`
	ControlHigh   float64 `json:"controlHigh"`
	ControlWithin bool    `json:"controlWithin"`
	CannotTell    bool    `json:"cannotTell"`
}

// ForecastResult is a forecast across a recorded change of the rule (spec R5): the window before the
// change replayed under the rule after it, on one branch with its seeds, against the record of the
// window after the change, with its control, the window after replayed under the rule before the
// change (BeforeRule), on the same branch with its seeds (ControlSeeds).
type ForecastResult struct {
	Before       string          `json:"before"`
	After        string          `json:"after"`
	Rule         string          `json:"rule"`
	BeforeRule   string          `json:"beforeRule"`
	Branch       BranchAt        `json:"branch"`
	Seeds        []int64         `json:"seeds"`
	ControlSeeds []int64         `json:"controlSeeds"`
	Checks       []ForecastCheck `json:"checks"`
	// FromCommit and ToCommit are the commits the daemons of the window before and of the window
	// after ran, whose workflow changes between them the forecast carries too (plan D1).
	FromCommit string `json:"fromCommit"`
	ToCommit   string `json:"toCommit"`
	// MissingAncestors is, for each of the two windows that lacks the history of an ancestor on
	// which the priority Dispatch resolves for an issue of it rests, by the window's name, each such
	// ancestor (windowRecord.MissingAncestors): the forecast then compares no number of the wait of
	// urgent work, which rests on that priority, and names the missing input in its place.
	MissingAncestors map[string][]string `json:"missingAncestors,omitempty"`
	// Reused is each part a forecast run found every run of in its directory of runs already, made
	// by the same legion binary, and so ran none of again (RunForecast); none for a forecast read
	// from runs alone.
	Reused []ReusedPart `json:"reused,omitempty"`
}

// ReusedPart is one part a forecast found whole in its directory of runs: the window, the rule and
// the commit of the legion binary that made its runs.
type ReusedPart struct {
	Window string `json:"window"`
	Rule   string `json:"rule"`
	Build  string `json:"build"`
}

// Pass says whether every number of the forecast is scored and stands within twice its gap.
func (f ForecastResult) Pass() bool {
	return !slices.ContainsFunc(f.Checks, func(c ForecastCheck) bool { return !c.Pass })
}

// ForecastCards is what a forecast across a recorded change of the rule is read from, one scorecard
// a seed, each read from a line timeline, as R5's validation reads them: the records of the window
// before the change (Before) and of the window after it (After); the forecast, the replays of the
// window before under the rule after the change (Forecasts); the replays of the window before under
// its own rule (Own) and of the window after under its own (AfterOwn), which say which numbers the
// change moves and the replay's own error on a measure; and the forecast's control, the replays of
// the window after under the rule before the change (Controls). BeforeMissing and AfterMissing are
// the ancestors whose history the window before and the window after lack, on which a priority
// Dispatch resolves rests (windowRecord.MissingAncestors): with any, no number of the wait of
// urgent work is compared, and the window gives no own error of a measure that rests on it.
type ForecastCards struct {
	Before, After                      Scorecard
	Forecasts, Own, AfterOwn, Controls []Scorecard
	BeforeMissing, AfterMissing        []string
}

// ownError is the replay's own error across the recorded change of the rule on the number that of
// reads from a scorecard (plan t34 (d)), which a measured number of the forecast takes as its gap and
// the comparison of rules holds each difference between two rules to (spec R7): the larger of the
// two windows' errors, each the mean over the seeds of the window's replays under its own rule (Own,
// AfterOwn) against its record's value (Before, After). of reads a scorecard of a window that lacks
// an ancestor's history (BeforeMissing, AfterMissing) told so (unresolved). A window whose record,
// or each of whose replays, gives no value of the number (of answers false: a wait of urgent work
// with no issue of its priority, or one that rests on a priority the window cannot give) gives no
// error; measured is false where cards hold no replay of one of the windows under its own rule, or
// where neither window gives an error.
func (cards ForecastCards) ownError(of func(card Scorecard, unresolved bool) (float64, bool)) (largest float64, measured bool) {
	if len(cards.Own) == 0 || len(cards.AfterOwn) == 0 {
		return 0, false
	}
	for _, w := range []struct {
		record     Scorecard
		replays    []Scorecard
		unresolved bool
	}{{cards.Before, cards.Own, len(cards.BeforeMissing) > 0}, {cards.After, cards.AfterOwn, len(cards.AfterMissing) > 0}} {
		recorded, ok := of(w.record, w.unresolved)
		if !ok {
			continue
		}
		values := make([]float64, 0, len(w.replays))
		for _, card := range w.replays {
			if value, ok := of(card, w.unresolved); ok {
				values = append(values, value)
			}
		}
		if len(values) == 0 {
			continue
		}
		mean, _, _ := spread(values)
		largest, measured = max(largest, math.Abs(mean-recorded)), true
	}
	return largest, measured
}

// ForecastChecks compares cards' forecasts with the record of the window after the change (spec
// R5): on rates per hour, averages and shares, never on totals, each within twice its gap of the
// record's value. A number's gap is R5's, or, for a measured one, the replay's own error on its
// measure, the larger of the two windows' errors; a cause the change redefines is scored only
// inside the total of all send-back loops and of the redefined causes together, and each cause
// both releases define the same way apart (plan D3). Each number also reads the window before under
// its own rule, whether the change moves it (by more than twice its gap), and the forecast's control
// (spec R5). No number of the wait of urgent work is compared where either window lacks an
// ancestor's history on which the priority it rests on rests (BeforeMissing, AfterMissing).
func ForecastChecks(cards ForecastCards) []ForecastCheck {
	quantities := forecastQuantities(slices.Concat([]Scorecard{cards.Before, cards.After}, cards.Forecasts, cards.Own, cards.AfterOwn, cards.Controls)...)
	unresolved := len(cards.BeforeMissing) > 0 || len(cards.AfterMissing) > 0
	over := func(q quantity, replays []Scorecard) (mean, low, high float64) {
		values := make([]float64, len(replays))
		for i, card := range replays {
			values[i] = q.of(card)
		}
		return spread(values)
	}
	checks := make([]ForecastCheck, 0, len(quantities))
	for _, q := range quantities {
		if q.resolved && unresolved {
			continue
		}
		check := ForecastCheck{Quantity: q.name, Kind: q.kind, Causes: q.causes, After: q.of(cards.After), Measured: q.measured, Scored: true}
		check.Forecast, check.Low, check.High = over(q, cards.Forecasts)
		gap := q.gap
		if q.measured {
			check.Error, check.Scored = cards.ownError(func(card Scorecard, _ bool) (float64, bool) { return q.of(card), true })
			gap = Gap{Amount: check.Error}
		}
		check.Gap = gap.Twice()
		check.Pass = check.Scored && check.Gap.Within(check.After, check.Forecast)
		if len(cards.Own) > 0 {
			check.Own, _, _ = over(q, cards.Own)
			check.Moved = check.Scored && !check.Gap.Within(check.Own, check.Forecast)
		}
		if len(cards.Controls) > 0 {
			check.Control, check.ControlLow, check.ControlHigh = over(q, cards.Controls)
			check.ControlWithin = check.Scored && check.Gap.Within(check.After, check.Control)
		}
		check.CannotTell = check.Moved && check.Pass && check.ControlWithin
		checks = append(checks, check)
	}
	return checks
}

// Write prints f as Markdown, its headings at depth: what was forecast from what, the commits the
// forecast carries beside the rule (plan D1), each cause of each group of the send-back causes and
// how it is scored (plan D3), the forecast's control, every number with its forecast, its value
// after the change, the difference, the allowance and the test, the window before under its own
// rule, whether the change moves it, the control and whether the rules are told apart on it, the
// numbers the forecast cannot tell the two rules apart on, and whether the forecast passes.
func (f ForecastResult) Write(w io.Writer, depth int) error {
	p := &printer{w: w}
	p.line("%s %s under `%s`, against the record of %s", strings.Repeat("#", depth), f.Before, f.Rule, f.After)
	p.line("")
	p.line("The window before the change, %s, is replayed under the rule after it, `%s`, on the branch `%s` with the seeds %s, and compared with the record of the window after the change, %s.",
		f.Before, f.Rule, f.Branch.Label(), seedList(f.Seeds), f.After)
	if f.FromCommit != f.ToCommit {
		p.line("The forecast also carries the workflow commits between `%s` and `%s`, the commits the two windows' daemons ran, so a gap may come from those commits as well as from the rule (plan D1).",
			f.FromCommit, f.ToCommit)
	} else {
		p.line("The two windows' daemons ran the one commit `%s`, so the forecast carries no workflow commit beside the rule (plan D1).", f.FromCommit)
	}
	p.line("It compares rates per hour, averages and shares, never totals, since the windows differ in length and in work, and each passes within twice its gap of its value after the change.")
	p.line("A measured number's gap is the replay's own error on its measure, which R7 reads: the larger of the two windows' errors, each window replayed under its own rule against its own record; each other number's gap is R5's.")
	p.line("The causes the workflow change redefines (%s): %s; each is scored only inside the total of all send-back loops, `%s`, and of the redefined causes together, `%s`.",
		GroupRedefined, codeList(ForecastGroups[GroupRedefined]), LoopsAll, LoopsRedefined)
	p.line("The causes both releases define the same way (%s): %s; each is scored apart, and inside the total too.",
		GroupSame, codeList(ForecastGroups[GroupSame]))
	p.line("The control replays the window after the change, %s, under the rule before it, `%s`, on the same branch with the seeds %s.", f.After, f.BeforeRule, seedList(f.ControlSeeds))
	p.line("The change moves a number where the forecast stands more than twice its gap from %s replayed under its own rule, `%s`; where the control too stands within twice its gap of the record after the change on such a number, the forecast cannot tell the two rules apart there.",
		f.Before, f.BeforeRule)
	for _, part := range f.Reused {
		p.line("The runs held %s under `%s` already, made by the legion binary of `%s`, so the forecast ran none of them again.", part.Window, part.Rule, part.Build)
	}
	for _, window := range slices.Sorted(maps.Keys(f.MissingAncestors)) {
		p.line("%s lacks the history of the Dispatch ancestors %s, on which the priority Dispatch resolves for an issue of it rests, so the forecast compares no number of the wait of urgent work (spec R10).",
			window, strings.Join(f.MissingAncestors[window], ", "))
	}
	p.line("")
	p.line("| quantity | kind | causes | forecast (lowest, highest seed) | after the change | difference | allowed | test | before, under its own rule | moved by the change | control (lowest, highest seed) | the two rules |")
	p.line("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	var failed, cannot, unscored []string
	for _, c := range f.Checks {
		allowed := formatNumber(c.Gap.Allowed(c.After)) + " (" + c.Gap.String() + ")"
		if c.Measured {
			allowed = formatNumber(c.Gap.Allowed(c.After)) + " (twice the replay's own error, " + formatNumber(c.Error) + ")"
		}
		test, moved, apart := passWord(c.Pass), "no", "not moved"
		if c.Moved {
			moved, apart = "yes", "told apart"
		}
		switch {
		case !c.Scored:
			test, allowed, apart = "not scored", "none", "not scored"
			unscored = append(unscored, c.Quantity)
		case c.CannotTell:
			apart = "cannot tell"
			cannot = append(cannot, c.Quantity)
		case c.Moved && !c.Pass:
			apart = "the forecast fails"
		}
		if c.Scored && !c.Pass {
			failed = append(failed, c.Quantity)
		}
		p.line("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |", c.Quantity, c.Kind, codeList(c.Causes), spreadCell(c.Forecast, c.Low, c.High, len(f.Seeds)),
			formatNumber(c.After), formatNumber(c.Forecast-c.After), allowed, test, formatNumber(c.Own), moved,
			spreadCell(c.Control, c.ControlLow, c.ControlHigh, len(f.ControlSeeds)), apart)
	}
	p.line("")
	if len(cannot) > 0 {
		p.line("The forecast cannot tell the two rules apart on %s: the change moves each, and the control too stands within twice its gap of the record after the change.",
			strings.Join(cannot, ", "))
	} else {
		p.line("The forecast tells the two rules apart on every number the change moves: on none of them does the control too stand within twice its gap of the record after the change.")
	}
	if len(unscored) > 0 {
		p.line("Not scored, since the runs hold no replay of each window under its own rule to measure the replay's own error from: %s.", strings.Join(unscored, ", "))
	}
	switch {
	case f.Pass():
		p.line("The forecast passes: every number stands within twice its gap.")
	case len(failed) > 0:
		p.line("The forecast fails on %s: the tool fails its check against a recorded change of the rule, and is fixed and checked again on a third window before it compares rules (R5).",
			strings.Join(failed, ", "))
	default:
		p.line("The forecast does not pass: it scores not every number.")
	}
	return p.err
}
