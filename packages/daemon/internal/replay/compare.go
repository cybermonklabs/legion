package replay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Costs is what a rule is judged on against main (spec R7), read from one run's scorecard: the
// issues finished, the owner's three costs and the wait of urgent work (costMeasures).
type Costs struct {
	// Done is the issues that reach done.
	Done float64 `json:"done"`
	// WaitOnlyForSlot is the hours of started work that waits only for a slot, reading (a).
	WaitOnlyForSlot float64 `json:"waitOnlyForSlot"`
	// PriorityTaken is the time-average share of pairs of a holder and an issue that waits for a
	// slot in which the holder ranks below, in today's line's order, the priority band first
	// (Scorecard.PriorityTakenBanded): every rule is judged on that one scale. Both windows were
	// recorded before the line put priority first, and the document gives the share by rank alone
	// (Scorecard.PriorityTaken) beside it.
	PriorityTaken float64 `json:"priorityTaken"`
	// OtherDelays is the hours of every send-back loop, of every cause, and of every wait on
	// another issue, whatever ended it.
	OtherDelays float64 `json:"otherDelays"`
	// UrgentWaits is the wait of urgent work, P0's and P1's apart, each where an issue counts in it
	// (Scorecard.UrgentWaits): the median and the 90th percentile of its wait to the first slot and
	// to done are each a measure (urgentWaitMeasures).
	UrgentWaits map[string]UrgentWait `json:"urgentWaits,omitempty"`
}

// costsOf is the costs card gives, its loops and waits summed in the order of their keys so the
// same card always gives the same sum.
func costsOf(card Scorecard) Costs {
	c := Costs{Done: float64(card.Done), WaitOnlyForSlot: card.WaitOnlyForSlot.Hours, PriorityTaken: card.PriorityTakenBanded, UrgentWaits: card.UrgentWaits}
	for _, cause := range slices.Sorted(maps.Keys(card.Loops)) {
		c.OtherDelays += card.Loops[cause].Hours
	}
	for _, word := range slices.Sorted(maps.Keys(card.Waits)) {
		c.OtherDelays += card.Waits[word].Hours
	}
	return c
}

// SeedCosts is a rule's costs on one branch of one window at each seed it ran, by the seed: the
// runs its difference from main is taken over, seed by seed (difference).
type SeedCosts map[int64]Costs

// costMeasure is one of the measures a rule is judged on: its name, whether more of it is better,
// and its value among a Costs, false where the Costs give none (a wait of urgent work with no issue
// of its priority).
type costMeasure struct {
	name         string
	higherBetter bool
	of           func(Costs) (float64, bool)
}

// costMeasures is every measure a rule is judged on (spec R7): the issues finished; the owner's
// three costs, the hours of started work that waits only for a slot, priority taken by holders in
// today's line's order (priorityTakenBanded) and the hours of the other delays; and the wait of
// urgent work (urgentWaitMeasures).
var costMeasures = slices.Concat([]costMeasure{
	{name: "done", higherBetter: true, of: func(c Costs) (float64, bool) { return c.Done, true }},
	{name: "waitOnlyForSlot.hours", of: func(c Costs) (float64, bool) { return c.WaitOnlyForSlot, true }},
	{name: "priorityTakenBanded", of: func(c Costs) (float64, bool) { return c.PriorityTaken, true }},
	{name: "otherDelays.hours", of: func(c Costs) (float64, bool) { return c.OtherDelays, true }},
}, urgentWaitMeasures())

// urgentWaitMeasures is the wait of urgent work as measures a rule is judged on (spec R4, R7): for
// P0 and then for P1 (urgentPriorities), the median and the 90th percentile of the wait to the first
// slot and then of the wait to done, in hours, each none where no issue of the priority counts.
func urgentWaitMeasures() []costMeasure {
	var measures []costMeasure
	for _, priority := range urgentPriorities {
		key := priority.String()
		for _, wait := range []struct {
			name string
			of   func(UrgentWait) WaitMinutes
		}{{"firstSlot", func(w UrgentWait) WaitMinutes { return w.FirstSlot }}, {"done", func(w UrgentWait) WaitMinutes { return w.Done }}} {
			for _, at := range []struct {
				name string
				of   func(WaitMinutes) float64
			}{{"medianHours", func(w WaitMinutes) float64 { return w.MedianMinutes }}, {"p90Hours", func(w WaitMinutes) float64 { return w.P90Minutes }}} {
				measures = append(measures, costMeasure{name: "urgentWaits[" + key + "]." + wait.name + "." + at.name,
					of: func(c Costs) (float64, bool) {
						urgent, counts := c.UrgentWaits[key]
						return at.of(wait.of(urgent)) / 60, counts
					}})
			}
		}
	}
	return measures
}

// Verdict is how a rule stands against main on one measure of one branch (spec R7).
type Verdict string

const (
	// VerdictBetter is better than main: the difference's 95% interval excludes zero and its near
	// end is farther from zero than the replay's own error on the measure, or, for a single run, the
	// difference itself is.
	VerdictBetter Verdict = "better"
	// VerdictWorse is worse than main in the same sense.
	VerdictWorse Verdict = "worse"
	// VerdictTie is every other difference, one whose measure has no error measured among them.
	VerdictTie Verdict = "tie"
	// VerdictNone is a measure that no seed both rules ran gives a value of: a wait of urgent work
	// with no issue of its priority. It is neither better nor worse.
	VerdictNone Verdict = "none"
	// VerdictUnpaired is a difference on a window whose runs are not paired, because the window lists
	// an experiment in the random mode, which a run draws keyed to no issue (windowRecord.unpaired):
	// the two runs at one seed need not draw alike, so the seed-by-seed difference is no paired one,
	// and it is neither better nor worse, nor a tie that more seeds would tell apart. It is a verdict
	// of its own, not a field beside one, so every reader of a verdict (the trade, the standing, the
	// name of the best rule) counts it as neither better nor worse with no rule of its own.
	VerdictUnpaired Verdict = "not paired"
)

// seedsNeededZ is the normal quantile of the seeds a tie would need (Difference.SeedsNeeded), the
// 1.96 of ⌈(1.96 s / d)²⌉.
const seedsNeededZ = 1.96

// Difference is a rule's difference from main on one measure of one branch, taken seed by seed
// (spec R7): the rule's run less main's run at the same seed, whose draws match at every stretch
// both reach (drawsAgree), over the Seeds n at which both ran and gave the measure; its mean (d)
// and its seed-by-seed standard deviation (SD, s, 0 for one seed); its 95% interval, from Low to
// High, d ± t·s/√n with t the 0.975 quantile of Student's t at n − 1 degrees of freedom
// (studentQuantile), and for a single run (n 1: a pair of runs that draw nothing) the difference
// itself; the replay's own error on the measure it is held to, where one is measured (Error,
// Measured); its verdict; and for a tie the seeds a difference of its size would need,
// ⌈(1.96 s / d)²⌉ (SeedsNeeded, for more than one seed), or none at all where d is 0 (Never).
type Difference struct {
	Measure     string  `json:"measure"`
	Seeds       int     `json:"seeds"`
	Mean        float64 `json:"mean"`
	SD          float64 `json:"sd"`
	Low         float64 `json:"low"`
	High        float64 `json:"high"`
	Error       float64 `json:"error"`
	Measured    bool    `json:"measured"`
	Verdict     Verdict `json:"verdict"`
	SeedsNeeded float64 `json:"seedsNeeded,omitempty"`
	Never       bool    `json:"never,omitempty"`
}

// difference is rule's difference from main on m over the seeds both ran (Difference), held to
// held's error of m, the replay's own error on each measure by its name where one is measured;
// quantile is Student's 0.975 quantile by the degrees of freedom (studentQuantiles). A difference
// is better or worse only where its near end, d less t·s/√n toward zero, is farther from zero than
// the error, which also keeps an interval that touches zero a tie.
func difference(m costMeasure, rule, main SeedCosts, held map[string]float64, quantile func(int) float64) Difference {
	diff := Difference{Measure: m.name, Verdict: VerdictNone}
	diff.Error, diff.Measured = held[m.name]
	values := make([]float64, 0, len(rule))
	for _, seed := range slices.Sorted(maps.Keys(rule)) {
		mainCosts, ran := main[seed]
		if !ran {
			continue
		}
		value, gives := m.of(rule[seed])
		mainValue, mainGives := m.of(mainCosts)
		if gives && mainGives {
			values = append(values, value-mainValue)
		}
	}
	n := len(values)
	if n == 0 {
		return diff
	}
	diff.Seeds = n
	diff.Mean, _, _ = spread(values)
	half := 0.0
	if n > 1 {
		squares := 0.0
		for _, v := range values {
			squares += (v - diff.Mean) * (v - diff.Mean)
		}
		diff.SD = math.Sqrt(squares / float64(n-1))
		half = quantile(n-1) * diff.SD / math.Sqrt(float64(n))
	}
	diff.Low, diff.High = diff.Mean-half, diff.Mean+half
	if near := math.Abs(diff.Mean) - half; diff.Measured && near > diff.Error+tolerance(diff.Error, diff.Mean) {
		diff.Verdict = VerdictWorse
		if (diff.Mean > 0) == m.higherBetter {
			diff.Verdict = VerdictBetter
		}
		return diff
	}
	diff.Verdict = VerdictTie
	switch {
	case math.Abs(diff.Mean) <= tolerance(diff.Mean):
		diff.Never = true
	case n > 1:
		diff.SeedsNeeded = math.Ceil(math.Pow(seedsNeededZ*diff.SD/diff.Mean, 2))
	}
	return diff
}

// judgeBranch is rule's difference from main on each measure it is judged on (costMeasures), in
// their order, over one branch's seeds (difference).
func judgeBranch(rule, main SeedCosts, held map[string]float64, quantile func(int) float64) []Difference {
	diffs := make([]Difference, len(costMeasures))
	for i, m := range costMeasures {
		diffs[i] = difference(m, rule, main, held, quantile)
	}
	return diffs
}

// studentQuantile is the 0.975 quantile of Student's t at df degrees of freedom, with which a
// two-sided 95% interval of a mean over df + 1 seeds is read (spec R7): the t at which the
// distribution function, 1 − I_x(df/2, 1/2)/2 with x = df/(df + t²) (regularizedBeta), reaches
// 0.975, found by bisection; 12.7062 at one degree of freedom, 2.7764 at four, and toward 1.96 as
// they grow.
func studentQuantile(df int) float64 {
	v := float64(df)
	below := func(t float64) bool { return 1-regularizedBeta(v/(v+t*t), v/2, 0.5)/2 < 0.975 }
	low, high := 0.0, 2.0
	for below(high) {
		low, high = high, 2*high
	}
	for range 64 {
		if mid := (low + high) / 2; below(mid) {
			low = mid
		} else {
			high = mid
		}
	}
	return (low + high) / 2
}

// studentQuantiles is studentQuantile with each answer kept, for a reader that asks it of one
// number of seeds for many measures.
func studentQuantiles() func(df int) float64 {
	kept := map[int]float64{}
	return func(df int) float64 {
		t, ok := kept[df]
		if !ok {
			t = studentQuantile(df)
			kept[df] = t
		}
		return t
	}
}

// regularizedBeta is the regularized incomplete beta function I_x(a, b), by its continued fraction
// (betaFraction) on the side of x where it converges quickly, and by I_x(a, b) = 1 − I_{1−x}(b, a)
// on the other.
func regularizedBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	lab, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaFraction(x, a, b) / a
	}
	return 1 - front*betaFraction(1-x, b, a)/b
}

// betaFraction is the continued fraction of the incomplete beta function at x, a and b, by the
// modified Lentz method, to a relative step of 1e-15 or 300 terms.
func betaFraction(x, a, b float64) float64 {
	const tiny = 1e-300
	floor := func(v float64) float64 {
		if math.Abs(v) < tiny {
			return tiny
		}
		return v
	}
	c, d := 1.0, 1/floor(1-(a+b)*x/(a+1))
	h := d
	for m := 1.0; m <= 300; m++ {
		even := m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		d, c = 1/floor(1+even*d), floor(1+even/c)
		h *= d * c
		odd := -(a + m) * (a + b + m) * x / ((a + 2*m) * (a + 2*m + 1))
		d, c = 1/floor(1+odd*d), floor(1+odd/c)
		step := d * c
		h *= step
		if math.Abs(step-1) < 1e-15 {
			break
		}
	}
	return h
}

// Standing is how a rule stands against main on one branch of one window over every measure it is
// judged on (costMeasures): the rank whose change the break-even search looks for.
type Standing string

const (
	// StandingBetter is better than main on at least one measure and worse on none: what a rule
	// must be on every branch that passes R5 to be named best.
	StandingBetter Standing = "better"
	// StandingWorse is worse than main on at least one measure and better on none.
	StandingWorse Standing = "worse"
	// StandingTrade is better than main on one measure and worse on another.
	StandingTrade Standing = "trade"
	// StandingLevel is neither better nor worse than main on any measure.
	StandingLevel Standing = "level"
)

// standingOf is how a rule whose differences from main on one branch are diffs stands against it.
func standingOf(diffs []Difference) Standing {
	better := slices.ContainsFunc(diffs, func(d Difference) bool { return d.Verdict == VerdictBetter })
	worse := slices.ContainsFunc(diffs, func(d Difference) bool { return d.Verdict == VerdictWorse })
	switch {
	case better && worse:
		return StandingTrade
	case worse:
		return StandingWorse
	case better:
		return StandingBetter
	}
	return StandingLevel
}

// BranchCosts is every rule's costs at each seed on one branch of one window, main's among them, by
// rule name, whether the branch gives a comparison (its window's main branch and it pass R5), and
// why its runs are not paired, "" where they are (windowRecord.unpaired).
type BranchCosts struct {
	Window   string
	Branch   BranchAt
	Passes   bool
	Unpaired string
	Costs    map[string]SeedCosts
}

// MeasureTrade is how a rule stood against main on one measure over the branches a decision read:
// on how many it was better, worse and a tie, on how many no seed gave the measure, and on how
// many its runs were not paired (VerdictUnpaired).
type MeasureTrade struct {
	Measure  string `json:"measure"`
	Better   int    `json:"better"`
	Worse    int    `json:"worse"`
	Tie      int    `json:"tie"`
	None     int    `json:"none"`
	Unpaired int    `json:"unpaired"`
}

// Trade is how a rule stood against main on each measure it is judged on (costMeasures), and
// whether it keeps the owner's direction (record.SlotRule.KeepsOwnersDirection).
type Trade struct {
	Rule           string         `json:"rule"`
	KeepsDirection bool           `json:"keepsDirection"`
	Measures       []MeasureTrade `json:"measures"`
}

// Judged is a rule's difference from main on each measure (judgeBranch) on one branch of one window
// that passes R5.
type Judged struct {
	Window      string       `json:"window"`
	Branch      BranchAt     `json:"branch"`
	Rule        string       `json:"rule"`
	Differences []Difference `json:"differences"`
}

// Decision is which rule a comparison names best (spec R7): each rule that keeps the owner's
// direction and, on every branch of the windows that passes R5, is worse than main on no measure
// it is judged on and better on at least one (Best); each rule that meets that test of the
// measures and does not keep the direction, so it is measured beside and not named (Outside); how
// many branches pass; each rule's trade over them; each rule's differences on each of them
// (Judged); and the replay's own error each difference is held to, by measure (Errors). No branch
// that fails R5 is read, so none can name a rule, and with no branch that passes no rule is named.
type Decision struct {
	Best     []string           `json:"best"`
	Outside  []string           `json:"outside"`
	Branches int                `json:"branches"`
	Trades   []Trade            `json:"trades"`
	Judged   []Judged           `json:"judged"`
	Errors   map[string]float64 `json:"errors"`
}

// Decide is the decision over branches for each of rules, main left out of them, each difference
// held to held, the replay's own error on each measure by its name where one is measured: main's
// costs and each rule's on each branch are its branch's Costs. On a branch whose runs are not
// paired (BranchCosts.Unpaired) each difference a seed gives is not paired (VerdictUnpaired): its
// mean over the seeds stands, with no interval and no seeds needed.
func Decide(rules []record.SlotRule, branches []BranchCosts, held map[string]float64) Decision {
	var passing []BranchCosts
	for _, b := range branches {
		if b.Passes {
			passing = append(passing, b)
		}
	}
	decision := Decision{Best: []string{}, Outside: []string{}, Branches: len(passing), Trades: []Trade{}, Judged: []Judged{}, Errors: held}
	quantile := studentQuantiles()
	for _, rule := range rules {
		if rule.Name == record.MainRule.Name {
			continue
		}
		trade := Trade{Rule: rule.Name, KeepsDirection: rule.KeepsOwnersDirection()}
		for _, m := range costMeasures {
			trade.Measures = append(trade.Measures, MeasureTrade{Measure: m.name})
		}
		meets := len(passing) > 0
		for _, b := range passing {
			costs, ran := b.Costs[rule.Name]
			main, mainRan := b.Costs[record.MainRule.Name]
			if !ran || !mainRan {
				meets = false
				continue
			}
			diffs := judgeBranch(costs, main, held, quantile)
			if b.Unpaired != "" {
				for i, diff := range diffs {
					if diff.Verdict != VerdictNone {
						diffs[i] = Difference{Measure: diff.Measure, Seeds: diff.Seeds, Mean: diff.Mean, Error: diff.Error, Measured: diff.Measured, Verdict: VerdictUnpaired}
					}
				}
			}
			decision.Judged = append(decision.Judged, Judged{Window: b.Window, Branch: b.Branch, Rule: rule.Name, Differences: diffs})
			for i, diff := range diffs {
				switch diff.Verdict {
				case VerdictBetter:
					trade.Measures[i].Better++
				case VerdictWorse:
					trade.Measures[i].Worse++
				case VerdictTie:
					trade.Measures[i].Tie++
				case VerdictUnpaired:
					trade.Measures[i].Unpaired++
				default:
					trade.Measures[i].None++
				}
			}
			if standingOf(diffs) != StandingBetter {
				meets = false
			}
		}
		switch {
		case meets && trade.KeepsDirection:
			decision.Best = append(decision.Best, rule.Name)
		case meets:
			decision.Outside = append(decision.Outside, rule.Name)
		}
		decision.Trades = append(decision.Trades, trade)
	}
	return decision
}

// FactorStanding is a rule's standing against main on the main branch at one slowdown factor.
type FactorStanding struct {
	Slowdown float64  `json:"slowdown"`
	Standing Standing `json:"standing"`
}

// BreakEvenResult is the break-even search of one rule against main (spec R3, assumption 1): its
// standing at each factor searched, and the first factor above the first searched at which its
// standing differs from its standing there (Changes), the factor at which its rank against main
// changes; Changes is false where no factor of the search changes it.
type BreakEvenResult struct {
	Standings []FactorStanding `json:"standings"`
	Factor    float64          `json:"factor"`
	Changes   bool             `json:"changes"`
}

// BreakEven is the break-even search of a rule whose costs at each seed on the main branch at each
// slowdown factor are rule, against main's, main: over factors in order, each that both ran, its
// standing judged as the decision judges a branch, each difference held to held (judgeBranch).
func BreakEven(factors []float64, rule, main map[float64]SeedCosts, held map[string]float64) BreakEvenResult {
	result := BreakEvenResult{Standings: []FactorStanding{}}
	quantile := studentQuantiles()
	for _, f := range factors {
		costs, ran := rule[f]
		mainCosts, mainRan := main[f]
		if !ran || !mainRan {
			continue
		}
		standing := standingOf(judgeBranch(costs, mainCosts, held, quantile))
		if len(result.Standings) > 0 && !result.Changes && standing != result.Standings[0].Standing {
			result.Factor, result.Changes = f, true
		}
		result.Standings = append(result.Standings, FactorStanding{Slowdown: f, Standing: standing})
	}
	return result
}

// runPlan is what each part of a validation or a comparison runs: its branches, the factors of
// the break-even search it runs the main branch at beyond them, and the seeds of a branch with a draw.
type runPlan struct {
	branches  []BranchAt
	breakEven []float64
	perDraw   int
}

// fullPlan is a comparison's plan: every branch of Branches, the main branch at each factor of
// BreakEvenSlowdowns no branch runs it at, and SeedsPerDraw seeds for a branch with a draw.
func fullPlan() runPlan {
	plan := runPlan{branches: Branches(), perDraw: SeedsPerDraw}
	for _, f := range BreakEvenSlowdowns() {
		if !slices.Contains(Slowdowns, f) {
			plan.breakEven = append(plan.breakEven, f)
		}
	}
	return plan
}

// CompareConfig is a comparison of slot rules on two recorded windows (RunCompare): the windows,
// the rules, the server the runs make their databases on, how many run at once, where they are
// written, which part of the comparison this process runs, the commit of the legion binary that
// makes the runs, and the project's token limit each run takes (RunConfig.TokenLimit).
type CompareConfig struct {
	// Windows is the directory of each recorded window, the window before the change of the rule
	// first.
	Windows []string
	// Rules is the rules compared; main and the rule each window's daemon ran are added where it
	// leaves them out, since every rule is judged against main, each window is validated under its
	// own rule, and the forecast runs the window before the change under the rule after it.
	Rules []record.SlotRule
	// PGDSN is the Postgres server each run makes its own database on.
	PGDSN string
	// Parallel is how many runs run at once.
	Parallel int
	// Runs is the directory of runs every run is written under.
	Runs string
	// OnlyWindow and OnlyRule, where set, run the part of that window, by its name, and of that
	// rule alone, so each process of an indexed Job runs one part; the document is then written
	// from every part's directory (ReadRuns, Runs.Comparison).
	OnlyWindow string
	OnlyRule   string
	// KeepLogs keeps each run's log.jsonl, which a comparison otherwise deletes once the run has
	// read it back.
	KeepLogs bool
	// Progress, where set, is told of each run as it ends.
	Progress func(RunDone)
	// BuildCommit is the commit of the legion binary that makes the runs, which each run's run.json
	// names (RunConfig.BuildCommit).
	BuildCommit string
	// TokenLimit is the project's token limit each run takes where the record holds none
	// (RunConfig.TokenLimit), zero for none.
	TokenLimit int64

	// plan, where set, replaces the comparison's plan: a test's smaller one.
	plan *runPlan
}

// RunCompare runs the comparison cfg names, or its part where cfg names one, into cfg.Runs: under
// each rule, each window on every branch with its seeds and the main branch at each factor of the
// break-even search, up to cfg.Parallel runs at once, each in a database of its own on cfg.PGDSN's
// server. It writes no document: Runs.Comparison writes it from the runs, which is what makes one
// written from a single process's runs and one written from many parts the same.
func RunCompare(ctx context.Context, cfg CompareConfig) error {
	if len(cfg.Windows) != 2 {
		return fmt.Errorf("replay: a comparison reads two windows, the window before the change of the rule first; %d given", len(cfg.Windows))
	}
	heads := make([]Window, len(cfg.Windows))
	names := make([]string, len(cfg.Windows))
	for i, dir := range cfg.Windows {
		var err error
		if names[i], err = windowName(dir); err != nil {
			return err
		}
		if heads[i], err = readWindowHead(dir); err != nil {
			return err
		}
	}
	if err := distinctNames(names[0], names[1]); err != nil {
		return err
	}
	if heads[0].From.After(heads[1].From) {
		return fmt.Errorf("replay: the window %s starts after %s; give the window before the change of the rule first", names[0], names[1])
	}
	rules, err := compareRules(cfg.Rules, names, heads)
	if err != nil {
		return err
	}
	if cfg.OnlyWindow != "" && !slices.Contains(names, cfg.OnlyWindow) {
		return fmt.Errorf("replay: --only-window %s names neither window (%s)", cfg.OnlyWindow, strings.Join(names, ", "))
	}
	if cfg.OnlyRule != "" && !slices.ContainsFunc(rules, func(rule record.SlotRule) bool { return rule.Name == cfg.OnlyRule }) {
		return fmt.Errorf("replay: --only-rule %s names no rule of the comparison (%s)", cfg.OnlyRule, ruleNames(rules))
	}
	plan := fullPlan()
	if cfg.plan != nil {
		plan = *cfg.plan
	}
	var specs []windowSpec
	for i, dir := range cfg.Windows {
		if cfg.OnlyWindow != "" && names[i] != cfg.OnlyWindow {
			continue
		}
		spec := windowSpec{dir: dir, name: names[i], order: i}
		for _, rule := range rules {
			if cfg.OnlyRule == "" || rule.Name == cfg.OnlyRule {
				spec.parts = append(spec.parts, partSpec{rule: rule, branches: plan.branches, breakEven: plan.breakEven})
			}
		}
		specs = append(specs, spec)
	}
	return runWindows(ctx, specs, runOptions{root: cfg.Runs, pgDSN: cfg.PGDSN, parallel: cfg.Parallel, perDraw: plan.perDraw,
		keepLogs: cfg.KeepLogs, progress: cfg.Progress, build: cfg.BuildCommit, tokenLimit: cfg.TokenLimit})
}

// compareRules is the rules a comparison runs, in the order of record.SlotRules: named, main, and
// the rule each window's daemon ran, which its parts name (a window whose parts name more than one,
// or none, is refused).
func compareRules(named []record.SlotRule, names []string, heads []Window) ([]record.SlotRule, error) {
	wanted := map[string]bool{record.MainRule.Name: true}
	for _, rule := range named {
		wanted[rule.OrMain().Name] = true
	}
	for i, head := range heads {
		rule := partsRule(head.Parts)
		if rule == "" {
			return nil, fmt.Errorf("replay: the parts of the window %s name no one rule its daemon ran, so it cannot be validated", names[i])
		}
		if _, ok := record.SlotRuleNamed(rule); !ok {
			return nil, fmt.Errorf("replay: the window %s ran the rule %s, which the repository does not keep", names[i], rule)
		}
		wanted[rule] = true
	}
	var rules []record.SlotRule
	for _, rule := range record.SlotRules() {
		if wanted[rule.Name] {
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

// ruleNames is rules' names, separated by commas.
func ruleNames(rules []record.SlotRule) string {
	return strings.Join(namesOf(rules), ", ")
}

// namesOf is the name of each of rules.
func namesOf(rules []record.SlotRule) []string {
	names := make([]string, 0, len(rules))
	for _, rule := range rules {
		names = append(names, rule.Name)
	}
	return names
}

// distinctNames refuses two windows that go by one name, before's and after's: a window goes by its
// directory's name, and a comparison and a forecast read each window by it.
func distinctNames(before, after string) error {
	if before == after {
		return fmt.Errorf("replay: both windows go by the name %s; a window goes by its directory's name", before)
	}
	return nil
}

// ValidateConfig is a validation of one window (RunValidate): the window, the rule its daemon ran,
// the server the runs make their databases on, how many run at once, the directory of runs they
// are written under, whether the main branch runs alone, whether the validation is the cap
// backtest and at which flat cap, the commit of the legion binary that makes them
// (RunConfig.BuildCommit), and the project's token limit each run takes (RunConfig.TokenLimit).
type ValidateConfig struct {
	Window      string
	Rule        record.SlotRule
	PGDSN       string
	Parallel    int
	Out         string
	KeepLogs    bool
	Progress    func(RunDone)
	BuildCommit string
	// TokenLimit is the project's token limit each run takes where the record holds none
	// (RunConfig.TokenLimit), zero for none.
	TokenLimit int64
	// MainOnly runs the main branch at f 1 alone (MainBranchAt), with its seeds, in place of every
	// branch: the check that the replay of the recorded rule meets R5's gaps on the branch R5 judges
	// a window by, at a fraction of the runs of every branch.
	MainOnly bool
	// CapBacktest makes the validation the cap backtest (spec R5, CapBacktest): the main branch at
	// f 1 alone, with its seeds, at the record's caps into Out and at the flat cap Cap, which
	// replaces the window's caps from its start to its end (RunConfig.Cap), into CapBacktestDir.
	// Cap is above zero exactly where CapBacktest is set.
	CapBacktest bool
	Cap         int

	// plan, where set, replaces the validation's branches and seeds: a test's smaller one.
	plan *runPlan
}

// validatePlan is the branches and seeds cfg's validation runs: a test's own plan where it set one
// (ValidateConfig.plan); with MainOnly or CapBacktest, the main branch at f 1 alone (MainBranchAt)
// with the seeds of a branch with a draw; and every branch the tool offers, each with its seeds
// (fullPlan), otherwise.
func validatePlan(cfg ValidateConfig) runPlan {
	if cfg.plan != nil {
		return *cfg.plan
	}
	plan := runPlan{branches: []BranchAt{MainBranchAt}, perDraw: SeedsPerDraw}
	if cfg.CapBacktest {
		return plan
	}
	if !cfg.MainOnly {
		plan = fullPlan()
	}
	return plan
}

// CapBacktestDir is the directory of runs the cap backtest of a validation into out writes its
// runs at the flat cap limit under: cap-<limit> in out, a directory of runs of its own beside the
// windows', which a reader of out passes over (ReadRuns).
func CapBacktestDir(out string, limit int) string {
	return filepath.Join(out, fmt.Sprintf("cap-%d", limit))
}

// RunValidate replays cfg.Window under cfg.Rule on every branch or on main alone, as
// validatePlan specifies. It writes up to cfg.Parallel concurrent runs into cfg.Out and
// reads their R5 validation. The cap backtest also replays main at cfg.Cap in CapBacktestDir.
// A rule other than the window's recorded rule is refused. A cap override requires a cap
// backtest and must be above zero.
func RunValidate(ctx context.Context, cfg ValidateConfig) (Validation, error) {
	name, err := windowName(cfg.Window)
	if err != nil {
		return Validation{}, err
	}
	switch {
	case cfg.CapBacktest && cfg.Cap <= 0:
		return Validation{}, fmt.Errorf("replay: the cap backtest replays the window at one flat cap above zero, not %d", cfg.Cap)
	case !cfg.CapBacktest && cfg.Cap != 0:
		return Validation{}, fmt.Errorf("replay: a validation replays the window at the caps its record shows; a flat cap (%d) is the cap backtest's alone", cfg.Cap)
	}
	head, err := readWindowHead(cfg.Window)
	if err != nil {
		return Validation{}, err
	}
	rule := cfg.Rule.OrMain()
	if ran := partsRule(head.Parts); ran != "" && ran != rule.Name {
		return Validation{}, fmt.Errorf("replay: the window %s ran the rule %s, and a validation replays the rule the daemon ran, not %s", name, ran, rule.Name)
	}
	plan := validatePlan(cfg)
	options := runOptions{root: cfg.Out, pgDSN: cfg.PGDSN, parallel: cfg.Parallel, perDraw: plan.perDraw, keepLogs: cfg.KeepLogs, progress: cfg.Progress,
		build: cfg.BuildCommit, tokenLimit: cfg.TokenLimit}
	spec := windowSpec{dir: cfg.Window, name: name, parts: []partSpec{{rule: rule, branches: plan.branches}}}
	if err := runWindows(ctx, []windowSpec{spec}, options); err != nil {
		return Validation{}, err
	}
	flat := options
	if cfg.CapBacktest {
		flat.root, flat.cap = CapBacktestDir(cfg.Out, cfg.Cap), cfg.Cap
		spec.parts = []partSpec{{rule: rule, branches: plan.branches}}
		if err := runWindows(ctx, []windowSpec{spec}, flat); err != nil {
			return Validation{}, err
		}
	}
	runs, err := ReadRuns(cfg.Out)
	if err != nil {
		return Validation{}, err
	}
	v, err := runs.Validation(name, rule.Name)
	if err != nil || !cfg.CapBacktest {
		return v, err
	}
	flatRuns, err := ReadRuns(flat.root)
	if err != nil {
		return Validation{}, err
	}
	backtest, err := runs.CapBacktest(name, rule.Name, flatRuns, cfg.Cap)
	if err != nil {
		return Validation{}, err
	}
	v.CapBacktest = &backtest
	return v, nil
}

// ForecastConfig is a forecast across a recorded change of the rule (RunForecast): the window
// before the change, the window after it, the rule after it, the server the runs make their
// databases on, how many run at once, the directory of runs they are written under, the commit of
// the legion binary that makes them (RunConfig.BuildCommit), and the project's token limit each
// run takes (RunConfig.TokenLimit).
type ForecastConfig struct {
	Before      string
	After       string
	Rule        record.SlotRule
	PGDSN       string
	Parallel    int
	Out         string
	KeepLogs    bool
	Progress    func(RunDone)
	BuildCommit string
	// TokenLimit is the project's token limit each run takes where the record holds none
	// (RunConfig.TokenLimit), zero for none.
	TokenLimit int64

	// perDraw, where above zero, replaces SeedsPerDraw: a test's fewer seeds.
	perDraw int
}

// RunForecast replays, on the main branch with its seeds, cfg.Before under cfg.Rule, the forecast,
// and under the rule its daemon ran, cfg.After under the rule its daemon ran and, the forecast's
// control, under the rule cfg.Before's daemon ran, into cfg.Out, up to cfg.Parallel runs at once,
// and answers the forecast across the change (spec R5), read back from the runs. Each run cfg.Out
// holds already, a validate's or an earlier forecast's, is not run again where the legion binary of
// cfg.BuildCommit made it and refused where another did (runOptions.sameBuild); the forecast names
// each part whose every run it so kept (ForecastResult.Reused). A window whose parts name no one
// rule its daemon ran is refused: it has no replay under its own rule.
func RunForecast(ctx context.Context, cfg ForecastConfig) (ForecastResult, error) {
	before, err := windowName(cfg.Before)
	if err != nil {
		return ForecastResult{}, err
	}
	after, err := windowName(cfg.After)
	if err != nil {
		return ForecastResult{}, err
	}
	if err := distinctNames(before, after); err != nil {
		return ForecastResult{}, err
	}
	rule := cfg.Rule.OrMain()
	ran := make([]record.SlotRule, 2)
	for i, dir := range []string{cfg.Before, cfg.After} {
		head, err := readWindowHead(dir)
		if err != nil {
			return ForecastResult{}, err
		}
		name := partsRule(head.Parts)
		if name == "" {
			return ForecastResult{}, fmt.Errorf("replay: the parts of the window %s name no one rule its daemon ran, so the forecast has no replay of it under its own rule", dir)
		}
		var kept bool
		if ran[i], kept = record.SlotRuleNamed(name); !kept {
			return ForecastResult{}, fmt.Errorf("replay: the window %s ran the rule %s, which the repository does not keep", dir, name)
		}
	}
	parts := func(rules ...record.SlotRule) []partSpec {
		var parts []partSpec
		for _, r := range rules {
			if !slices.ContainsFunc(parts, func(p partSpec) bool { return p.rule.Name == r.Name }) {
				parts = append(parts, partSpec{rule: r, branches: []BranchAt{MainBranchAt}})
			}
		}
		return parts
	}
	specs := []windowSpec{
		{dir: cfg.Before, name: before, parts: parts(rule, ran[0])},
		{dir: cfg.After, name: after, order: 1, parts: parts(ran[1], ran[0])},
	}
	type key struct{ window, rule string }
	made, kept, builds := map[key]int{}, map[key]int{}, map[key]string{}
	progress := func(done RunDone) {
		k := key{done.Window, done.Rule}
		made[k]++
		if done.Kept {
			kept[k]++
			builds[k] = done.Build
		}
		if cfg.Progress != nil {
			cfg.Progress(done)
		}
	}
	if err := runWindows(ctx, specs, runOptions{root: cfg.Out, pgDSN: cfg.PGDSN, parallel: cfg.Parallel, perDraw: cmp.Or(cfg.perDraw, SeedsPerDraw),
		keepLogs: cfg.KeepLogs, progress: progress, build: cfg.BuildCommit, tokenLimit: cfg.TokenLimit, sameBuild: true}); err != nil {
		return ForecastResult{}, err
	}
	runs, err := ReadRuns(cfg.Out)
	if err != nil {
		return ForecastResult{}, err
	}
	f, err := runs.Forecast(before, after, rule.Name)
	if err != nil {
		return ForecastResult{}, err
	}
	for _, spec := range specs {
		for _, part := range spec.parts {
			if k := (key{spec.name, part.rule.Name}); made[k] > 0 && kept[k] == made[k] {
				f.Reused = append(f.Reused, ReusedPart{Window: k.window, Rule: k.rule, Build: builds[k]})
			}
		}
	}
	return f, nil
}

// comparison is what the document of a comparison is written from, read from the runs and checked
// whole: the two windows in order, the rules in the order of record.SlotRules, the branches and the
// factors of the main branch every part ran, and each window's validation.
type comparison struct {
	runs        Runs
	windows     []windowRecord
	rules       []string
	commits     map[string]string
	branches    []BranchAt
	factors     []float64
	validations []Validation
}

// readComparison checks that r holds a whole comparison — two windows, each part of each under the
// same rules, main and each window's own among them, every part with the same branches and factors
// — and gathers it.
func (r Runs) readComparison() (comparison, error) {
	c := comparison{runs: r, commits: map[string]string{}}
	c.windows = slices.SortedFunc(maps.Values(r.windows), windowOrder)
	if len(c.windows) != 2 || c.windows[0].Order == c.windows[1].Order {
		return comparison{}, fmt.Errorf("replay: a comparison reads two windows, one before the change of the rule and one after it; the runs hold %d", len(c.windows))
	}
	ruled := map[string]map[string]bool{}
	for key, part := range r.parts {
		if ruled[key.window] == nil {
			ruled[key.window] = map[string]bool{}
		}
		ruled[key.window][key.rule] = true
		c.commits[key.rule] = part.plan.RuleCommit
	}
	for _, rule := range record.SlotRules() {
		in := 0
		for _, w := range c.windows {
			if ruled[w.Name][rule.Name] {
				in++
			}
		}
		switch in {
		case len(c.windows):
			c.rules = append(c.rules, rule.Name)
		case 0:
		default:
			return comparison{}, fmt.Errorf("replay: the runs hold the rule %s for one window and not the other, so the part of the other is missing", rule.Name)
		}
	}
	for _, rule := range append([]string{record.MainRule.Name}, c.windows[0].Rule, c.windows[1].Rule) {
		if !slices.Contains(c.rules, rule) {
			return comparison{}, fmt.Errorf("replay: the runs hold no part under the rule %s, which a comparison needs (main, and each window's own)", cmp.Or(rule, "of a window"))
		}
	}
	var plan []string
	for i, w := range c.windows {
		for j, rule := range c.rules {
			part := r.parts[partKey{window: w.Name, rule: rule}]
			var labels []string
			for _, run := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
				labels = append(labels, run.label())
			}
			if i == 0 && j == 0 {
				plan = labels
				for _, run := range part.plan.Branches {
					at, err := run.at()
					if err != nil {
						return comparison{}, err
					}
					c.branches = append(c.branches, at)
				}
				for _, run := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
					if run.Branch == MainBranch.Name && !slices.Contains(c.factors, run.Slowdown) {
						c.factors = append(c.factors, run.Slowdown)
					}
				}
				continue
			}
			if !slices.Equal(labels, plan) {
				return comparison{}, fmt.Errorf("replay: the part of %s under %s ran other branches than the part of %s under %s", w.Name, rule, c.windows[0].Name, c.rules[0])
			}
		}
	}
	slices.Sort(c.factors)
	if len(c.factors) == 0 {
		return comparison{}, errors.New("replay: the runs hold no run of the main branch, which every rule is ranked against main on")
	}
	for _, w := range c.windows {
		v, err := r.Validation(w.Name, w.Rule)
		if err != nil {
			return comparison{}, err
		}
		c.validations = append(c.validations, v)
	}
	return c, nil
}

// seedCostsAt is the costs of each of rule's runs on the branch labelled label of window, by its
// seed; none where the part or the branch holds no run.
func (r Runs) seedCostsAt(window, rule, label string) SeedCosts {
	part, ok := r.parts[partKey{window: window, rule: rule}]
	if !ok {
		return nil
	}
	runs := part.runs[label]
	if len(runs) == 0 {
		return nil
	}
	costs := make(SeedCosts, len(runs))
	for _, run := range runs {
		costs[run.seed] = run.costs
	}
	return costs
}

// seedCostsAt is Runs.seedCostsAt over the comparison's runs.
func (c comparison) seedCostsAt(window, rule, label string) SeedCosts {
	return c.runs.seedCostsAt(window, rule, label)
}

// ownErrors is the replay's own error across the recorded change of the rule on each measure a
// rule is judged on (costMeasures), by its name, where either window measures one: the forecast's
// measured error (ForecastCards.ownError), the larger of the two windows' errors, each window's
// replay under its own rule on the main branch (MainBranchAt), the mean over its seeds read from its
// line timelines, against its record's scorecard. Each difference between two rules is held to it
// (spec R7). before is the window before the change and after the window after it; a window whose
// runs hold no part under the rule its daemon ran has no such replay, and is refused.
func (r Runs) ownErrors(before, after windowRecord) (map[string]float64, error) {
	own := func(w windowRecord) ([]Scorecard, error) {
		part, ok := r.parts[partKey{window: w.Name, rule: w.Rule}]
		if !ok {
			return nil, fmt.Errorf("replay: the runs hold no part of %s under the rule its daemon ran, %q, which the replay's own error is read from", w.Name, w.Rule)
		}
		return lineCards(part.runs[MainBranchAt.Label()]), nil
	}
	owned, err := own(before)
	if err != nil {
		return nil, err
	}
	afterOwned, err := own(after)
	if err != nil {
		return nil, err
	}
	cards := ForecastCards{Before: before.Scorecard, After: after.Scorecard, Own: owned, AfterOwn: afterOwned}
	held := map[string]float64{}
	for _, m := range costMeasures {
		if largest, measured := cards.ownError(func(card Scorecard) (float64, bool) { return m.of(costsOf(card)) }); measured {
			held[m.name] = largest
		}
	}
	return held, nil
}

// ownErrors is Runs.ownErrors over the comparison's two windows, each of whose parts under its own
// rule readComparison checked.
func (c comparison) ownErrors() map[string]float64 {
	held, _ := c.runs.ownErrors(c.windows[0], c.windows[1])
	return held
}

// writeSignalReadings writes, for each run whose run.json gives the signals it limited with the
// limit of each and the value it read at it (runRecord.Signals, which every run of a signal rule
// that limits a signal writes), a table of them under the run's window, rule, branch and seed, in
// the order of the parts' plans. A comparison whose runs limit no signal writes nothing of it.
func (c comparison) writeSignalReadings(p *printer) {
	var written bool
	for _, w := range c.windows {
		for _, rule := range c.rules {
			part, ok := c.runs.parts[partKey{window: w.Name, rule: rule}]
			if !ok {
				continue
			}
			for _, planned := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
				for _, run := range part.runs[planned.label()] {
					if len(run.readings) == 0 {
						continue
					}
					if !written {
						p.line("## Signal values at their limits")
						p.line("")
						p.line("Each run below limited the signals it lists, and read each at the value given: its mean over the minutes where the window records it.")
						written = true
					}
					p.line("")
					p.line("### %s, `%s`, `%s`, seed %d", w.Name, rule, planned.label(), run.seed)
					p.line("")
					p.line("| signal | limit | value |")
					p.line("| --- | --- | --- |")
					for _, name := range slices.Sorted(maps.Keys(run.readings)) {
						p.line("| `%s` | %s | %s |", name, formatNumber(run.readings[name].Limit), formatNumber(run.readings[name].Value))
					}
				}
			}
		}
	}
	if written {
		p.line("")
	}
}

// decision is the comparison's decision over every branch of both windows (Decide), each branch
// passing where its window's validation says it gives a comparison, each difference held to the
// replay's own error (ownErrors).
func (c comparison) decision() Decision {
	var branches []BranchCosts
	for i, w := range c.windows {
		for _, at := range c.branches {
			b := BranchCosts{Window: w.Name, Branch: at, Passes: c.validations[i].Compares(at), Unpaired: w.unpaired(), Costs: map[string]SeedCosts{}}
			for _, rule := range c.rules {
				if costs := c.seedCostsAt(w.Name, rule, at.Label()); costs != nil {
					b.Costs[rule] = costs
				}
			}
			branches = append(branches, b)
		}
	}
	rules := make([]record.SlotRule, 0, len(c.rules))
	for _, name := range c.rules {
		rule, _ := record.SlotRuleNamed(name)
		rules = append(rules, rule)
	}
	return Decide(rules, branches, c.ownErrors())
}

// breakEven is the break-even search of rule against main on window, over the comparison's factors,
// each difference held to held (BreakEven).
func (c comparison) breakEven(window, rule string, held map[string]float64) BreakEvenResult {
	ruled, mained := map[float64]SeedCosts{}, map[float64]SeedCosts{}
	for _, f := range c.factors {
		label := BranchAt{Branch: MainBranch, Slowdown: f}.Label()
		if costs := c.seedCostsAt(window, rule, label); costs != nil {
			ruled[f] = costs
		}
		if costs := c.seedCostsAt(window, record.MainRule.Name, label); costs != nil {
			mained[f] = costs
		}
	}
	return BreakEven(c.factors, ruled, mained, held)
}

// keyedDraw is one draw of a stretch's conflict as a comparison reads it from run.json (StretchDraw):
// its key, the stretch's place folded into one number (stretchDraw.hash), and its value.
type keyedDraw struct {
	key uint64
	u   float64
}

// keyedDraws is draws by their keys (keyedDraw), in the order of the keys, then of the values; it
// writes the place of each into places by its key, where places is not nil, so a report can name a
// stretch it reads by its key alone (stretchDraw.name).
func keyedDraws(draws []StretchDraw, places map[uint64]stretchDraw) []keyedDraw {
	keyed := make([]keyedDraw, 0, len(draws))
	for _, draw := range draws {
		at := stretchDraw{issue: draw.Issue, start: draw.Start, phase: draw.Phase, n: draw.N}
		key := at.hash()
		if places != nil {
			places[key] = at
		}
		keyed = append(keyed, keyedDraw{key: key, u: draw.U})
	}
	slices.SortFunc(keyed, func(a, b keyedDraw) int { return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.u, b.u)) })
	return keyed
}

// name is the stretch's draw at as the comparison's document names it: the issue's stretch ended by
// its piece of at's phase from its recorded start, and the draw's count among that stretch's.
func (at stretchDraw) name() string {
	return fmt.Sprintf("%s's stretch ended by its %s piece from %s, draw %d", at.issue, at.phase, at.start.UTC().Format("2006-01-02T15:04Z"), at.n+1)
}

// drawDifference is a stretch two runs both drew another value at: its key and each run's value.
type drawDifference struct {
	key  uint64
	a, b float64
}

// drawsAgree is how many stretches two runs both drew, a and b in the order of their keys
// (keyedDraws), and each of those they drew another value at: none where every draw is keyed to
// the run's seed, the issue and the stretch alone, as two runs of two rules at one seed are paired
// on (spec R7).
func drawsAgree(a, b []keyedDraw) (shared int, differ []drawDifference) {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].key < b[j].key:
			i++
		case a[i].key > b[j].key:
			j++
		default:
			shared++
			if a[i].u != b[j].u {
				differ = append(differ, drawDifference{key: a[i].key, a: a[i].u, b: b[j].u})
			}
			i, j = i+1, j+1
		}
	}
	return shared, differ
}

// unpaired is why no run of r is paired with another (spec R7), "" where its runs may be: r's
// window.json lists an experiment in the random mode, which each run draws from one source of its
// seed in the order it records its trees and issues, keyed to no issue (UnkeyedDraws), so two rules'
// runs at one seed need not draw alike.
func (r windowRecord) unpaired() string {
	var random []string
	for _, experiment := range r.Experiments {
		if experiment.Mode == string(config.ExperimentRandom) {
			random = append(random, experiment.Name)
		}
	}
	if len(random) == 0 {
		return ""
	}
	return fmt.Sprintf("%s lists the experiment %s in the random mode, which each run draws from one source of its seed in the order it records its trees and issues, keyed to no issue, so two rules' runs at one seed need not draw alike",
		r.Name, codeList(random))
}

// experimentsText is what the comparison's document says of the experiments r lists and of its
// runs' draws of them, unkeyed in all, over every rule, branch and seed.
func (r windowRecord) experimentsText(unkeyed int) string {
	if len(r.Experiments) == 0 {
		return fmt.Sprintf("%s lists no experiment in its window.json, as collect writes none, so its replays draw no experiment arm, and no draw of its runs is an unkeyed experiment draw.", r.Name)
	}
	named := make([]string, 0, len(r.Experiments))
	for _, experiment := range r.Experiments {
		named = append(named, fmt.Sprintf("`%s` in the %s mode", experiment.Name, experiment.Mode))
	}
	return fmt.Sprintf("%s lists the experiments %s in its window.json, and its runs drew %d values for them that they key to no stretch.", r.Name, strings.Join(named, ", "), unkeyed)
}

// Comparison is the document of a comparison of slot rules (spec R7), written in Markdown from the
// runs alone, so it is the same whether one process made every run or many processes each made a
// part: the windows, rules, build commits of the runs, branches, seeds and factors it read; the
// rule it names best, or that none is, with the replay's own error each difference is held to,
// whether each window lists an experiment, whose random draws leave its runs unpaired, each rule's
// trade, whether it keeps the owner's direction and why each candidate added to the spec's rules is
// compared; each window's scorecard read from its record, the wait of its P0 and P1 work
// among it; what each rule's grace keeps over the waits that gave a slot back in each window's
// record; each window's validation; the forecast across the change, with the workflow commits it
// carries too; which window measures how a rule changes conflicts; the value of every measure for
// every rule, window and branch, read from the snapshot timeline, with the lowest and the highest
// seed, the wait of P0 and P1 work among them (urgentRows), and on each branch that passes R5 each
// rule's difference from main on each judged measure with its verdict (Difference); each run's
// admissions with no recorded work; the draws each pair of rules' runs share; where each
// issue's times are; the gap of each measure between the snapshot and the line timelines; each
// rule's break-even slowdown factor; and the assumptions and the scenario each run used. Every
// number in it is computed from the runs. Beside it, issues is the comparison's issues file
// (writeIssues), which the document names by the base name of issuesFile, the path it is written
// to (IssuesFile).
func (r Runs) Comparison(issuesFile string) (document, issues string, err error) {
	c, err := r.readComparison()
	if err != nil {
		return "", "", err
	}
	before, after := c.windows[0], c.windows[1]
	forecast, err := r.Forecast(before.Name, after.Name, after.Rule)
	if err != nil {
		return "", "", err
	}
	decision := c.decision()
	var doc strings.Builder
	p := &printer{w: &doc}
	c.writeHead(p, forecast)
	c.writeDecision(p, forecast, decision)
	c.writeScorecards(p)
	c.writeGraces(p)
	p.line("## The validation of each window (R5)")
	p.line("")
	p.line("Each window is replayed under the rule its daemon ran, on every branch, and each branch's line timeline is compared with the record's.")
	p.line("Each issue's error is listed for the main branch; each other branch gives its median and 90th percentile.")
	for _, v := range c.validations {
		p.line("")
		if p.err == nil {
			p.err = v.Write(&doc, 3, func(at BranchAt) bool { return at.Label() == MainBranchAt.Label() })
		}
	}
	p.line("")
	p.line("## The forecast across the change (R5)")
	p.line("")
	if p.err == nil {
		p.err = forecast.Write(&doc, 3)
	}
	p.line("")
	c.writeConflicts(p)
	c.writeMatrix(p, decision)
	c.writeNoWork(p)
	c.writeSignalReadings(p)
	c.writeDraws(p)
	p.line("## Each issue's first slot and done under each rule")
	p.line("")
	p.line("`%s`, beside this document, gives for each window and each rule each issue that held a slot in the record or in the rule's replay on the main branch (`%s`): its first slot and its done in the record and in the replay, each replayed time the median over the main branch's seeds.",
		filepath.Base(issuesFile), MainBranchAt.Label())
	p.line("")
	c.writeGaps(p)
	c.writeBreakEven(p, decision.Errors)
	c.writeAssumptions(p)
	var list strings.Builder
	q := &printer{w: &list}
	c.writeIssues(q)
	return doc.String(), list.String(), cmp.Or(p.err, q.err)
}

// IssuesFile is the path `legion replay compare` writes a comparison's issues file to
// (Comparison), beside its document at out: out with its extension, where it has one, replaced by
// "-issues.md".
func IssuesFile(out string) string {
	return strings.TrimSuffix(out, filepath.Ext(out)) + "-issues.md"
}

// writeIssues writes the comparison's issues file: for each window and each rule, each issue that
// held a slot in the window's record or on the main branch of the rule's replay of it, with its
// first slot and its done in the record and in the replay. Both are read from line timelines, as
// the validation reads them: the record's from the window's record scorecard, and the replay's
// from the line scorecards of the main branch's seeds, each the median over them (medianTime). The
// rows follow the recorded first slot, then the replayed one, then the issue's key, so an issue
// the record gave no slot comes after every issue it gave one.
func (c comparison) writeIssues(p *printer) {
	before, after := c.windows[0], c.windows[1]
	p.line("# Each issue's first slot and done under each rule, on %s and %s", before.Name, after.Name)
	p.line("")
	p.line("`legion replay compare` wrote this file beside its document from the runs it read.")
	p.line("For each window and each rule, it gives each issue that held a slot in the window's record or in the rule's replay on the main branch (`%s`): its first slot and its done in the record and in the replay, in UTC.",
		MainBranchAt.Label())
	p.line("Each replayed time is the median over the main branch's seeds.")
	p.line("A seed that gives an issue no time counts as later than every time, so the median gives none where half the seeds or more give none.")
	p.line("`no slot` is no first slot in the window, and `not done` is no done in the window.")
	p.line("The rows follow the recorded first slot, and the issues that the record gives no slot come last.")
	firstSlot := func(card Scorecard) map[string]time.Time { return card.FirstSlot }
	doneAt := func(card Scorecard) map[string]time.Time { return card.DoneAt }
	for _, w := range c.windows {
		recorded := w.Scorecard.FirstSlot
		for _, rule := range c.rules {
			cards := lineCards(c.runs.parts[partKey{window: w.Name, rule: rule}].runs[MainBranchAt.Label()])
			replayed := map[string]time.Time{}
			for _, card := range append([]Scorecard{w.Scorecard}, cards...) {
				for key := range card.FirstSlot {
					if _, seen := replayed[key]; !seen {
						replayed[key] = medianTime(cards, key, firstSlot)
					}
				}
			}
			issues := slices.SortedFunc(maps.Keys(replayed), func(a, b string) int {
				return cmp.Or(noneLast(recorded[a], recorded[b]), noneLast(replayed[a], replayed[b]), strings.Compare(a, b))
			})
			p.line("")
			p.line("## %s under `%s` (seeds: %d)", w.Name, rule, len(cards))
			p.line("")
			p.line("%s", tableHead("issue", []string{"first slot, recorded", "first slot, replayed", "done, recorded", "done, replayed"}))
			for _, key := range issues {
				p.line("| %s | %s | %s | %s | %s |", key, issueTime(recorded[key], "no slot"), issueTime(replayed[key], "no slot"),
					issueTime(w.Scorecard.DoneAt[key], "not done"), issueTime(medianTime(cards, key, doneAt), "not done"))
			}
		}
	}
}

// urgentRow is one row the document gives the wait of urgent work in (urgentRows): its name, the
// priority whose wait it reads (UrgentWait, Scorecard.UrgentWaits), its value in that wait, and
// whether it is a median or a percentile, which a scorecard with no issue of the priority gives
// none of.
type urgentRow struct {
	name, priority string
	time           bool
	value          func(UrgentWait) float64
}

// urgentRows is each row the document gives the wait of urgent work in, for P0 and then for P1
// (urgentPriorities): the issues; and, of the wait to the first slot and then of the wait to done,
// the median and the 90th percentile in hours and the cut waits.
func urgentRows() []urgentRow {
	var rows []urgentRow
	for _, priority := range urgentPriorities {
		name := priority.String()
		rows = append(rows, urgentRow{name: name + " issues", priority: name, value: func(w UrgentWait) float64 { return float64(w.Issues) }})
		for _, wait := range []struct {
			to string
			of func(UrgentWait) WaitMinutes
		}{{"to first slot", func(w UrgentWait) WaitMinutes { return w.FirstSlot }}, {"to done", func(w UrgentWait) WaitMinutes { return w.Done }}} {
			rows = append(rows,
				urgentRow{name: name + " " + wait.to + ", median", priority: name, time: true,
					value: func(w UrgentWait) float64 { return wait.of(w).MedianMinutes / 60 }},
				urgentRow{name: name + " " + wait.to + ", 90th percentile", priority: name, time: true,
					value: func(w UrgentWait) float64 { return wait.of(w).P90Minutes / 60 }},
				urgentRow{name: name + " " + wait.to + ", cut waits", priority: name,
					value: func(w UrgentWait) float64 { return float64(wait.of(w).Cut) }})
		}
	}
	return rows
}

// cell is row's value over cards as a table prints a measure: the mean over them, with the lowest
// and the highest card's beside it for more than one (spreadCell). A card with no issue of the
// row's priority counts 0 in its issues and its cut waits, and is left out of a median or a
// percentile, which is none where no card gives one.
func (row urgentRow) cell(cards []Scorecard) string {
	values := make([]float64, 0, len(cards))
	for _, card := range cards {
		wait, ok := card.UrgentWaits[row.priority]
		if !ok && row.time {
			continue
		}
		values = append(values, row.value(wait))
	}
	if len(values) == 0 && row.time {
		return "none"
	}
	mean, low, high := spread(values)
	return spreadCell(mean, low, high, len(values))
}

// writeUrgentNote writes what the document says beside each table of the wait of urgent work
// (urgentRows).
func writeUrgentNote(p *printer) {
	p.line("The rows of P0 and P1 give the wait of urgent work (`urgentWaits`), in hours: an issue counts at the highest of its own Dispatch priorities from its first todo in the window to its first slot, and an issue that was todo before the window and stands todo at its start counts from the window's start.")
	p.line("Its waits run to its first slot and to its done by the end of the run, the window's run-out included; an issue with none by then counts with its wait to that end, a cut wait, and the median and the 90th percentile include the cut waits, whose number stands beside them.")
	p.line("A run with no issue of a priority counts 0 in its issues and its cut waits, and none in its median and its percentile.")
}

// medianTime is the median over cards of the time that times gives key, a card that gives none
// counting as later than every time: the zero time, none, where half the cards or more give none,
// and for an even count the midpoint of the two middle times.
func medianTime(cards []Scorecard, key string, times func(Scorecard) map[string]time.Time) time.Time {
	given := make([]time.Time, 0, len(cards))
	for _, card := range cards {
		if at, ok := times(card)[key]; ok {
			given = append(given, at)
		}
	}
	if 2*len(given) <= len(cards) {
		return time.Time{}
	}
	slices.SortFunc(given, time.Time.Compare)
	mid := len(cards) / 2
	if len(cards)%2 == 1 {
		return given[mid]
	}
	return given[mid-1].Add(given[mid].Sub(given[mid-1]) / 2)
}

// noneLast orders the times a and b, the zero time, none, after every other.
func noneLast(a, b time.Time) int {
	if a.IsZero() != b.IsZero() {
		if a.IsZero() {
			return 1
		}
		return -1
	}
	return a.Compare(b)
}

// issueTime is at as the issues file writes it, in UTC to the minute, and none for the zero time.
func issueTime(at time.Time, none string) string {
	if at.IsZero() {
		return none
	}
	return at.UTC().Format("2006-01-02T15:04Z")
}

// writeHead writes the document's title and what it read: each window with its span, place and
// rule, the rules with their commits, the build commit of each run (buildsText), the branches, the
// seeds and the factors of the break-even search.
func (c comparison) writeHead(p *printer, forecast ForecastResult) {
	before, after := c.windows[0], c.windows[1]
	p.line("# Slot rules compared on %s and %s", before.Name, after.Name)
	p.line("")
	p.line("`legion replay compare` wrote this document from the runs it read; every number in it is computed from them.")
	p.line("")
	for _, w := range c.windows {
		place := "before"
		if w.Order == after.Order {
			place = "after"
		}
		runOut := ""
		if w.After > 0 {
			runOut = fmt.Sprintf(", replayed on through a run-out of %s after it and measured to its end", time.Duration(w.After))
		}
		p.line("- %s: project %s, from %s to %s%s, the window %s the change of the rule; its daemon ran the rule `%s` (%s).",
			w.Name, w.Project, w.From.UTC().Format("2006-01-02T15:04Z"), w.To.UTC().Format("2006-01-02T15:04Z"), runOut, place, w.Rule,
			partsText(w.Parts))
	}
	rules := make([]string, 0, len(c.rules))
	for _, rule := range c.rules {
		rules = append(rules, fmt.Sprintf("`%s` (commit `%s`)", rule, c.commits[rule]))
	}
	p.line("- The rules: %s.", strings.Join(rules, ", "))
	p.line("- The build commit of each run, the commit of the `legion` binary that made it: %s.", c.buildsText())
	labels := make([]string, 0, len(c.branches))
	for _, at := range c.branches {
		labels = append(labels, "`"+at.Label()+"`")
	}
	p.line("- The branches: %d of the %d the tool offers: %s.", len(c.branches), len(Branches()), strings.Join(labels, ", "))
	for _, w := range c.windows {
		part := c.runs.parts[partKey{window: w.Name, rule: c.rules[0]}]
		most := 0
		for _, run := range part.plan.Branches {
			most = max(most, len(run.Seeds))
		}
		draws := "none of its stretches ended in a conflict, so only a drawn scenario of an issue that never started draws"
		if w.Conflicted > 0 {
			draws = fmt.Sprintf("%d of its %d fitted stretches ended in a conflict, so the git and drawn branches use drawing seeds", w.Conflicted, w.Stretches)
		}
		p.line("- The seeds of %s: %d for a branch with a draw and 1 for a branch with none; %s.", w.Name, most, draws)
	}
	factors := make([]string, 0, len(c.factors))
	for _, f := range c.factors {
		factors = append(factors, formatNumber(f))
	}
	p.line("- The break-even search runs the main branch at the slowdown factors %s.", strings.Join(factors, ", "))
	p.line("- A branch's value of a measure is the mean of its seeds; where it ran more than one, the lowest and the highest seed's value follow in parentheses.")
	p.line("- The forecast across the change replays %s under `%s`, the rule of %s.", forecast.Before, forecast.Rule, forecast.After)
	p.line("")
}

// buildsText is each build commit the comparison's runs name (runRead.build) with how many of its
// runs name it, in the order of the first start of a run of each (runRead.started), then by commit,
// so the commits read in the order the code that made the runs ran.
func (c comparison) buildsText() string {
	type build struct {
		commit string
		runs   int
		first  time.Time
	}
	builds := map[string]*build{}
	total := 0
	for _, part := range c.runs.parts {
		for _, runs := range part.runs {
			for _, run := range runs {
				b, seen := builds[run.build]
				if !seen {
					b = &build{commit: run.build, first: run.started}
					builds[run.build] = b
				}
				b.runs++
				if run.started.Before(b.first) {
					b.first = run.started
				}
				total++
			}
		}
	}
	ordered := slices.SortedFunc(maps.Values(builds), func(a, b *build) int {
		return cmp.Or(a.first.Compare(b.first), strings.Compare(a.commit, b.commit))
	})
	texts := make([]string, 0, len(ordered))
	for _, b := range ordered {
		texts = append(texts, fmt.Sprintf("`%s` (%d of the %d runs)", b.commit, b.runs, total))
	}
	return strings.Join(texts, ", ")
}

// partsText is a window's parts as the document names them: each part's release, commit and rule.
func partsText(parts []Part) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, fmt.Sprintf("release %s at `%s` from %s to %s under `%s`", part.Release, part.Commit,
			part.From.UTC().Format("2006-01-02T15:04Z"), part.To.UTC().Format("2006-01-02T15:04Z"), part.Rule))
	}
	if len(texts) == 0 {
		return "no part recorded"
	}
	return strings.Join(texts, "; ")
}

// writeDecision writes how each rule is judged against main (spec R7), the rule the comparison
// names best, or that none is and why, the replay's own error each difference is held to, and each
// rule's trade against main over the branches that pass R5, with whether it keeps the owner's
// direction, beside why each candidate added to the spec's rules is compared (candidateReasons).
func (c comparison) writeDecision(p *printer, forecast ForecastResult, decision Decision) {
	p.line("## The rule named best (R7)")
	p.line("")
	p.line("Each rule is judged against `main`, today's rule, on each of these measures, read from the snapshot timeline: the issues finished, the owner's three costs — started work that waits only for a slot, priority taken by holders in today's line's order, the priority band first, and the other delays, the hours of every send-back loop and every wait on another issue — and the wait of urgent work, the median and the 90th percentile of the wait of P0 and of P1 work to its first slot and to its done, in hours: %s.",
		codeList(costMeasureNames()))
	p.line("Both windows were recorded before the line put priority first, so the tables of every measure give priority taken by rank alone (`priorityTaken`), the order of their own lines, beside it.")
	most := 1
	for _, w := range c.windows {
		for _, run := range c.runs.parts[partKey{window: w.Name, rule: c.rules[0]}].plan.Branches {
			most = max(most, len(run.Seeds))
		}
	}
	quantile := "no branch of this comparison runs more than one seed"
	if most > 1 {
		quantile = fmt.Sprintf("%s at the %d seeds of a branch with a draw", formatNumber(studentQuantile(most-1)), most)
	}
	p.line("On each branch that passes R5, a rule's difference from `main` on a measure is taken seed by seed: its run less `main`'s run at the same seed, whose draws match at every stretch both reach (the draws below). Its mean d over the n seeds both ran has the 95%% interval d ± t·s/√n, s the seed-by-seed standard deviation and t the 0.975 quantile of Student's t at n − 1 degrees of freedom (%s).",
		quantile)
	p.line("The rule is better or worse than `main` on the measure only where that interval excludes zero and its near end is farther from zero than the replay's own error on the measure across the recorded change of the rule: the larger of the two windows' errors, each window's replay under its own rule on `%s`, the mean over its seeds, against its record, as the forecast measures it.",
		MainBranchAt.Label())
	p.line("A branch that draws nothing, the main branch among them under a rule that starts only work the record ran, runs one seed: its difference is a single run's, better or worse only where it is itself farther from zero than that error.")
	p.line("Every other difference is a tie, and the tables of every measure give, for each tie over more than one seed, the seeds a difference of its size would need, ⌈(1.96 s / d)²⌉, or never where d is 0: the seeds at which its interval would exclude zero, while a tie within the error stays one at any number of seeds.")
	p.line("A rule that admits issues the record never started is read on the branches that draw their work (`neverStarted=drawn`, `neverStarted=drawn-done`) as on every other branch, and each run's admissions with no recorded work are given below.")
	var keeping []string
	for _, trade := range decision.Trades {
		if trade.KeepsDirection {
			keeping = append(keeping, trade.Rule)
		}
	}
	direction := "Only a rule that keeps the owner's direction of 2026-11-24 can be named best: R10 above P0, and below it the next issue in priority order while each signal of R11 is under its limit, with the cap as a ceiling only."
	switch {
	case len(keeping) == 0:
		p.line("%s No rule of this comparison keeps it, so none can be named; every rule is measured beside `main`.", direction)
	default:
		p.line("%s Of the rules compared, %s keep it, and every other rule is measured beside them.", direction, codeList(keeping))
	}
	if slices.Contains(c.rules, "before-p0") {
		p.line("`before-p0`, the rule before R10, is measured for reference and is never named.")
	}
	p.line("A rule is named best when it keeps that direction and, on every branch of both windows that passes R5, it is worse than `main` on no measure and better on at least one.")
	for i, w := range c.windows {
		unkeyed := 0
		for _, part := range c.runs.parts {
			if part.plan.Window != w.Name {
				continue
			}
			for _, runs := range part.runs {
				for _, run := range runs {
					unkeyed += run.unkeyedDraws.Experiments
				}
			}
		}
		p.line("%s", w.experimentsText(unkeyed))
		if reason := w.unpaired(); reason != "" {
			p.line("No difference on %s is paired: %s. So no rule is better or worse than `main` on any branch of it, each of its differences is given as not paired, and no rule can be named best while it is compared.",
				w.Name, reason)
		}
		if !c.validations[i].MainPasses() {
			p.line("%s gives no comparison: its main branch fails R5, so none of its branches is read.", w.Name)
		}
	}
	switch {
	case !forecast.Pass():
		p.line("No rule is named best: the forecast across the change fails R5, so the tool is fixed and checked again on a third window before it compares rules.")
	case decision.Branches == 0:
		p.line("No rule is named best: no branch of either window passes R5.")
	case len(decision.Best) == 1:
		p.line("`%s` is named best: it keeps the owner's direction, and on each of the %d branches that pass R5 it is worse than `main` on no measure and better on at least one.",
			decision.Best[0], decision.Branches)
	case len(decision.Best) > 1:
		p.line("No one rule is named best: %s each keep the owner's direction and meet the test on the %d branches that pass R5, so the comparison names none over the others.",
			codeList(decision.Best), decision.Branches)
	default:
		p.line("No rule is named best: of the %d branches that pass R5, each rule that keeps the owner's direction has one on which it is worse than `main` on some measure, or better on none.",
			decision.Branches)
	}
	if len(decision.Outside) > 0 {
		p.line("%s meet that test of the measures but do not keep the owner's direction, so they are measured beside it and not named.", codeList(decision.Outside))
	}
	p.line("")
	p.line("The replay's own error across the change that each difference is held to:")
	p.line("")
	p.line("| measure | error |")
	p.line("| --- | --- |")
	for _, m := range costMeasures {
		p.line("| %s | %s |", m.name, errorText(decision.Errors, m.name))
	}
	p.line("")
	p.line("Each rule's trade against `main` over the %d branches that pass R5: on how many it is better, worse and a tie on each measure, and on how many no seed gives the measure; whether it keeps the owner's direction; and why each rule the spec does not name was added.",
		decision.Branches)
	p.line("")
	p.line("%s", tableHead("rule", slices.Concat([]string{"keeps the owner's direction"}, costMeasureNames(), []string{"why it was added"})))
	for _, trade := range decision.Trades {
		keeps := "no"
		if trade.KeepsDirection {
			keeps = "yes"
		}
		row := "| `" + trade.Rule + "` | " + keeps + " |"
		for _, m := range trade.Measures {
			cell := fmt.Sprintf("better %d, worse %d, tie %d", m.Better, m.Worse, m.Tie)
			if m.None > 0 {
				cell += fmt.Sprintf(", none %d", m.None)
			}
			if m.Unpaired > 0 {
				cell += fmt.Sprintf(", not paired %d", m.Unpaired)
			}
			row += " " + cell + " |"
		}
		p.line("%s", row+" "+candidateReasons[trade.Rule]+" |")
	}
	p.line("")
}

// errorText is the replay's own error on the measure name among held as the document gives it, and
// "not measured" where neither window measures one.
func errorText(held map[string]float64, name string) string {
	if value, measured := held[name]; measured {
		return formatNumber(value)
	}
	return "not measured"
}

// costMeasureNames is the name of each measure a rule's costs are judged on (costMeasures), in
// their order.
func costMeasureNames() []string {
	names := make([]string, 0, len(costMeasures))
	for _, m := range costMeasures {
		names = append(names, m.name)
	}
	return names
}

// codeList is names, each in code, separated by commas.
func codeList[S ~string](names []S) string {
	return strings.Join(coded(names), ", ")
}

// coded is each of names in code.
func coded[S ~string](names []S) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "`"+string(name)+"`")
	}
	return out
}

// writeScorecards writes each window's scorecard, read from its record, with the issues done by
// anyone beside Legion's done (Scorecard.DoneByAnyone) and the wait of its urgent work
// (urgentRows), whose cut waits run to the end of the window's record.
func (c comparison) writeScorecards(p *printer) {
	p.line("## The scorecard of each window, read from its record")
	p.line("")
	writeUrgentNote(p)
	p.line("The record's waits are cut at the end of the window's record.")
	for _, w := range c.windows {
		p.line("")
		p.line("### %s", w.Name)
		p.line("")
		p.line("| measure | value |")
		p.line("| --- | --- |")
		for _, m := range MeasuresOf(w.Scorecard) {
			p.line("| %s | %s |", m.Name, formatNumber(m.Value))
		}
		p.line("| doneByAnyone | %d |", w.Scorecard.DoneByAnyone)
		p.line("| issues with a first slot | %d |", len(w.Scorecard.FirstSlot))
		for _, row := range urgentRows() {
			p.line("| %s | %s |", row.name, row.cell([]Scorecard{w.Scorecard}))
		}
	}
	p.line("")
}

// writeGraces writes, for each window and each rule of the comparison with a grace
// (record.SlotRule.WaitGrace), what the grace keeps over the waits on another issue that gave a slot
// back in the window's record (graceKeeps): how many of them it keeps the slot through, of how
// many, and the slot-hours it keeps. It is the record's own, read from its trace under the grace and
// from no replay. A comparison with no rule with a grace writes nothing of it.
func (c comparison) writeGraces(p *printer) {
	var graced []record.SlotRule
	for _, name := range c.rules {
		if rule, _ := record.SlotRuleNamed(name); rule.WaitGrace > 0 {
			graced = append(graced, rule)
		}
	}
	if len(graced) == 0 {
		return
	}
	p.line("## What each grace keeps, read from each window's record")
	p.line("")
	p.line("A rule with a grace keeps a holder's slot through a wait on another issue until the wait has lasted the grace. " +
		"Over the waits on another issue that gave a slot back in each window's record, the grace keeps the slot through each wait that lasts no longer than it, a wait still open at the window's end counted at its length to the end. " +
		"Each wait holds its slot for the shorter of its length and the grace, and the sum over the waits is the slot-hours the grace keeps from the line.")
	p.line("")
	p.line("| window | rule | grace | waits it keeps the slot through | slot-hours it keeps |")
	p.line("| --- | --- | --- | --- | --- |")
	for _, w := range c.windows {
		for _, rule := range graced {
			kept := graceKeeps(rule.WaitGrace, w.WaitsGaveBack)
			p.line("| %s | `%s` | %s | %d of %d | %s |", w.Name, rule.Name, rule.WaitGrace, kept.Kept, kept.Waits, formatNumber(kept.SlotHours))
		}
	}
	p.line("")
}

// writeConflicts writes, for each window, whether it measures how a rule changes conflicts, and,
// for a window whose record carries its fitted rate, each term of that rate with its interval
// (ConflictModel.terms): the base, the term of each phase a stretch can end in and the merges term.
func (c comparison) writeConflicts(p *printer) {
	p.line("## Conflicts")
	p.line("")
	for _, w := range c.windows {
		rule, _ := record.SlotRuleNamed(w.Rule)
		switch {
		case rule.Conflict == record.ConflictChecksRed:
			p.line("%s does not measure how a rule changes conflicts: its daemon's rule `%s` moved a head with red checks or a conflict back by one trigger, so those loops are one cause, kept as recorded under every rule (assumption 22).",
				w.Name, w.Rule)
		case w.Conflicted == 0:
			p.line("%s does not measure how a rule changes conflicts: none of its %d stretches ended in a conflict, so no replay of it draws one.", w.Name, w.Stretches)
		default:
			p.line("%s fits the generator on %d stretches, %d of which ended in a conflict. Main keeps the recorded conflicts. The git branch draws for unanswered reads, and the drawn branch uses the generator throughout.",
				w.Name, w.Stretches, w.Conflicted)
			if w.Conflicts.fitted() {
				p.line("The rate fitted on %s: %s.", w.Name, w.Conflicts.terms())
			}
		}
	}
	p.line("")
}

// writeMatrix writes the value of every measure for every rule, window and branch, read from the
// snapshot timeline, a table for each window and branch with a column for each rule, the wait of
// urgent work among them (urgentRows), and below them the issues done by anyone, which only a run's
// line timeline gives (Scorecard.DoneByAnyone); and below each table of a branch that passes R5,
// each rule's difference from main on each measure it is judged on (decision's Judged), with the
// replay's own error it is held to (Difference.cell). A branch that fails R5 judges no rule.
func (c comparison) writeMatrix(p *printer, decision Decision) {
	judged := map[string][]Difference{}
	for _, j := range decision.Judged {
		judged[j.Window+"\x00"+j.Branch.Label()+"\x00"+j.Rule] = j.Differences
	}
	var rules []string
	for _, rule := range c.rules {
		if rule != record.MainRule.Name {
			rules = append(rules, rule)
		}
	}
	p.line("## Every measure for every rule, window and branch (the snapshot timeline)")
	p.line("")
	writeUrgentNote(p)
	p.line("`priorityTaken` ranks by rank alone, the order of the recorded windows' lines, and `priorityTakenBanded` in today's line's order, the band of the Dispatch priority that places each issue first.")
	p.line("Below the table of a branch that passes R5, each rule's difference from `main` on each measure it is judged on: d, the mean over the seeds both ran of the rule's run less `main`'s at the same seed, with its 95%% interval, or a single run's difference alone; its verdict; and for a tie the seeds a difference of its size would need, or never where d is 0. The error column is the replay's own error across the change that each difference is held to.")
	for i, w := range c.windows {
		for _, at := range c.branches {
			values := map[string][][]float64{}
			var cards []Scorecard
			for _, rule := range c.rules {
				for _, run := range c.runs.parts[partKey{window: w.Name, rule: rule}].runs[at.Label()] {
					cards = append(cards, run.snapshot)
				}
			}
			names := measureNames(cards...)
			seeds := 0
			for _, rule := range c.rules {
				runs := c.runs.parts[partKey{window: w.Name, rule: rule}].runs[at.Label()]
				seeds = max(seeds, len(runs))
				per := make([]map[string]float64, len(runs))
				for k, run := range runs {
					per[k] = valuesOf(run.snapshot)
				}
				for _, name := range names {
					column := make([]float64, len(runs))
					for k := range runs {
						column[k] = per[k][name]
					}
					values[name] = append(values[name], column)
				}
			}
			verdict := "passes R5"
			if !c.validations[i].Compares(at) {
				verdict = "gives no comparison"
			}
			p.line("")
			p.line("### %s, `%s` (%s; seeds: %d)", w.Name, at.Label(), verdict, seeds)
			p.line("")
			p.line("%s", tableHead("measure", coded(c.rules)))
			for _, name := range names {
				row := "| " + name + " |"
				for _, column := range values[name] {
					mean, low, high := spread(column)
					row += " " + spreadCell(mean, low, high, len(column)) + " |"
				}
				p.line("%s", row)
			}
			for _, urgent := range urgentRows() {
				row := "| " + urgent.name + " |"
				for _, rule := range c.rules {
					row += " " + urgent.cell(snapshotCards(c.runs.parts[partKey{window: w.Name, rule: rule}].runs[at.Label()])) + " |"
				}
				p.line("%s", row)
			}
			row := "| doneByAnyone (the line timeline) |"
			for _, rule := range c.rules {
				runs := c.runs.parts[partKey{window: w.Name, rule: rule}].runs[at.Label()]
				column := make([]float64, len(runs))
				for k, run := range runs {
					column[k] = float64(run.line.DoneByAnyone)
				}
				mean, low, high := spread(column)
				row += " " + spreadCell(mean, low, high, len(column)) + " |"
			}
			p.line("%s", row)
			p.line("")
			if !c.validations[i].Compares(at) {
				p.line("The branch gives no comparison, so no rule is judged on it.")
				continue
			}
			if reason := w.unpaired(); reason != "" {
				p.line("No difference on this branch is paired: %s.", reason)
				p.line("")
			}
			p.line("%s", tableHead("judged against `main`", slices.Concat([]string{"error"}, coded(rules))))
			for k, m := range costMeasures {
				row := "| " + m.name + " | " + errorText(decision.Errors, m.name) + " |"
				for _, rule := range rules {
					cell := "no run"
					if diffs, ok := judged[w.Name+"\x00"+at.Label()+"\x00"+rule]; ok {
						cell = diffs[k].cell()
					}
					row += " " + cell + " |"
				}
				p.line("%s", row)
			}
		}
	}
	p.line("")
}

// cell is d as the tables of every measure give it: none where no seed gives the measure; d with
// its 95% interval and the seeds it is taken over, or a single run's d alone; its verdict; and for a
// tie the seeds a difference of its size would need, or never where d is 0. A difference that is not
// paired gives d over its seeds alone, with no interval.
func (d Difference) cell() string {
	switch {
	case d.Verdict == VerdictNone:
		return "none"
	case d.Verdict == VerdictUnpaired && d.Seeds > 1:
		return fmt.Sprintf("%s over %d seeds: %s", formatNumber(d.Mean), d.Seeds, d.Verdict)
	}
	text := formatNumber(d.Mean) + ", a single run: " + string(d.Verdict)
	if d.Seeds > 1 {
		text = fmt.Sprintf("%s [%s, %s] over %d seeds: %s", formatNumber(d.Mean), formatNumber(d.Low), formatNumber(d.High), d.Seeds, d.Verdict)
	}
	switch {
	case d.Verdict != VerdictTie:
	case d.Never:
		text += ", never"
	case d.Seeds > 1 && d.SeedsNeeded == 1:
		text += ", 1 seed needed"
	case d.Seeds > 1:
		text += ", " + formatNumber(d.SeedsNeeded) + " seeds needed"
	}
	return text
}

// writeNoWork writes each run's admissions with no recorded work and the slot-hours they hold
// (spec R7; runRead.noWork, runRead.noWorkHours): for each window, rule, branch and break-even
// factor, and seed, in the order of the parts' plans.
func (c comparison) writeNoWork(p *printer) {
	p.line("## Admissions with no recorded work")
	p.line("")
	p.line("Each run's admissions of an issue that holds its slot with no work, recorded or drawn: an issue the record never started, on a branch that draws no work for it (`neverStarted=none`) or in a window that holds no issue to draw its work from, with the slot-hours such issues hold in the window, read from the snapshot timeline.")
	p.line("")
	p.line("| window | rule | branch | seed | admissions with no work | their slot-hours |")
	p.line("| --- | --- | --- | --- | --- | --- |")
	for _, w := range c.windows {
		for _, rule := range c.rules {
			part := c.runs.parts[partKey{window: w.Name, rule: rule}]
			for _, run := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
				label := run.label()
				for _, read := range part.runs[label] {
					p.line("| %s | `%s` | `%s` | %d | %d | %s |", w.Name, rule, label, read.seed, read.noWork, formatNumber(read.noWorkHours))
				}
			}
		}
	}
	p.line("")
}

// writeDraws writes, for each window and each seed, the pairs of the comparison's rules whose runs
// it compared there, the stretches the two runs of a pair at the seed both drew, over every branch
// and break-even factor, and how many of them drew another value (drawsAgree), none where every
// draw is keyed to the seed, the issue and the stretch alone (spec R7): one line for each seed, and
// below a seed at which some pair drew another value, each such pair, branch and stretch with the
// two values. Two rules can share a stretch that main never reaches, so every pair is compared.
func (c comparison) writeDraws(p *printer) {
	p.line("## The draws that each pair of rules shares")
	p.line("")
	p.line("Each run records each draw of a stretch's conflict it made, with its key — the issue, the recorded start and the phase of the piece whose end ends the stretch, and how many draws of that stretch came before it — and its value. A draw is keyed to the run's seed, the issue and the stretch, so the runs of two rules at one seed draw one value at every stretch both reach, whatever either rule moves.")
	p.line("For each window and seed, a line gives the pairs of rules compared, the stretches that the two runs of a pair both drew on one branch, over every branch and pair, and how many of them drew another value, which must be none; a branch that draws no conflict adds none. Below a seed at which some did, each such pair, branch and stretch is named with the two values.")
	p.line("The work of an issue that never started (`neverStarted=drawn`, `neverStarted=drawn-done`) is drawn before the run from its seed and the record alone, so no rule moves it, and no load model draws a random value: the kills, the tokens and the merge queue are read from the window's record at the replay's work in flight.")
	p.line("A workflow experiment in the `random` mode, which a window's window.json may list, is drawn keyed to no issue: each run counts those draws apart, and the decision above says, for each window, whether it lists one, and pairs no difference on a window that does.")
	for _, w := range c.windows {
		parts := make([]*partRead, len(c.rules))
		seeds := map[int64]bool{}
		for i, rule := range c.rules {
			parts[i] = c.runs.parts[partKey{window: w.Name, rule: rule}]
			for _, runs := range parts[i].runs {
				for _, run := range runs {
					seeds[run.seed] = true
				}
			}
		}
		var labels []string
		for _, run := range slices.Concat(parts[0].plan.Branches, parts[0].plan.BreakEven) {
			labels = append(labels, run.label())
		}
		p.line("")
		p.line("### %s", w.Name)
		p.line("")
		for _, seed := range slices.Sorted(maps.Keys(seeds)) {
			pairs, shared := 0, 0
			var differing []string
			for i := range parts {
				for j := i + 1; j < len(parts); j++ {
					compared := false
					for _, label := range labels {
						a, b := parts[i].runAt(label, seed), parts[j].runAt(label, seed)
						if a == nil || b == nil {
							continue
						}
						compared = true
						both, other := drawsAgree(a.draws, b.draws)
						shared += both
						for _, d := range other {
							differing = append(differing, fmt.Sprintf("  - `%s` and `%s` on `%s`: %s, %s and %s", c.rules[i], c.rules[j], label,
								parts[i].places[d.key].name(), formatNumber(d.a), formatNumber(d.b)))
						}
					}
					if compared {
						pairs++
					}
				}
			}
			if len(differing) == 0 {
				p.line("- seed %d: %d pairs of rules compared, %d stretches that both runs of a pair drew, none of them another value.", seed, pairs, shared)
				continue
			}
			p.line("- seed %d: %d pairs of rules compared, %d stretches that both runs of a pair drew, %d of them another value:", seed, pairs, shared, len(differing))
			p.line("")
			for _, line := range differing {
				p.line("%s", line)
			}
			p.line("")
		}
	}
	p.line("")
}

// runAt is part's run on the branch labelled label at seed, nil where it ran none there.
func (part *partRead) runAt(label string, seed int64) *runRead {
	runs := part.runs[label]
	for k := range runs {
		if runs[k].seed == seed {
			return &runs[k]
		}
	}
	return nil
}

// tableHead is the head of a table whose first column is first and whose other columns are
// columns, with its separator line.
func tableHead(first string, columns []string) string {
	head, rule := "| "+first+" |", "| --- |"
	for _, column := range columns {
		head += " " + column + " |"
		rule += " --- |"
	}
	return head + "\n" + rule
}

// writeGaps writes, for each window, the widest gap of each measure between a run's line timeline
// and its snapshot timeline over every run of each rule, flagged where it is more than a third of
// R5's gap of the snapshot's value (design 10.1).
func (c comparison) writeGaps(p *printer) {
	p.line("## The gap between the snapshot and the line timelines")
	p.line("")
	p.line("Each cell is the widest gap, the line timeline's value less the snapshot timeline's, over every run of the rule in the window: the measured error of reading a course from its lines, which a record's scorecard rests on.")
	p.line("A cell marked (over a third) is more than a third of R5's gap of the snapshot's value.")
	for _, w := range c.windows {
		widest := map[string]map[string]MeasureGap{}
		var cards []Scorecard
		for _, rule := range c.rules {
			widest[rule] = map[string]MeasureGap{}
			part := c.runs.parts[partKey{window: w.Name, rule: rule}]
			for _, label := range slices.Sorted(maps.Keys(part.runs)) {
				for _, run := range part.runs[label] {
					cards = append(cards, run.snapshot, run.line)
					for _, gap := range run.gaps {
						if old, seen := widest[rule][gap.Measure]; !seen || math.Abs(gap.Gap()) > math.Abs(old.Gap()) {
							widest[rule][gap.Measure] = gap
						}
					}
				}
			}
		}
		p.line("")
		p.line("### %s", w.Name)
		p.line("")
		p.line("%s", tableHead("measure", coded(c.rules)))
		for _, name := range measureNames(cards...) {
			row := "| " + name + " |"
			for _, rule := range c.rules {
				gap := widest[rule][name]
				cell := formatNumber(gap.Gap())
				if bound, tested := R5Gap(name); tested && math.Abs(gap.Gap()) > bound.Allowed(gap.Snapshot)/3+tolerance(gap.Snapshot, gap.Line) {
					cell += " (over a third)"
				}
				row += " " + cell + " |"
			}
			p.line("%s", row)
		}
	}
	p.line("")
}

// writeBreakEven writes each rule's break-even slowdown factor against main on each window, and
// its standing at each factor searched, each difference held to held, the replay's own error on
// each measure (BreakEven).
func (c comparison) writeBreakEven(p *printer, held map[string]float64) {
	p.line("## The break-even slowdown factor of each rule against main")
	p.line("")
	p.line("A rule's rank against `main` is its standing on the main branch, each measure judged as the rule named best is judged (R7): better on some measure and worse on none, worse on some and better on none, a trade, or level, a tie or no value on each (the rule named best is better on every branch).")
	p.line("The break-even factor is the first slowdown factor of the search at which that standing differs from its standing at the first.")
	p.line("")
	results := map[partKey]BreakEvenResult{}
	for _, rule := range c.rules {
		for _, w := range c.windows {
			if rule != record.MainRule.Name {
				results[partKey{window: w.Name, rule: rule}] = c.breakEven(w.Name, rule, held)
			}
		}
	}
	p.line("%s", tableHead("rule", windowNames(c.windows)))
	top := formatNumber(c.factors[len(c.factors)-1])
	for _, rule := range c.rules {
		if rule == record.MainRule.Name {
			continue
		}
		row := "| `" + rule + "` |"
		for i, w := range c.windows {
			result := results[partKey{window: w.Name, rule: rule}]
			switch {
			case !c.validations[i].MainPasses():
				row += " no comparison: the window's main branch fails R5 |"
			case w.unpaired() != "":
				row += " not paired: the window lists an experiment in the random mode |"
			case len(result.Standings) == 0:
				row += " no run of the main branch |"
			case result.Changes:
				row += fmt.Sprintf(" %s: %s at %s, %s from %s |", formatNumber(result.Factor), result.Standings[0].Standing,
					formatNumber(result.Standings[0].Slowdown), standingAt(result, result.Factor), formatNumber(result.Factor))
			default:
				row += fmt.Sprintf(" no factor up to %s changes it (%s) |", top, result.Standings[0].Standing)
			}
		}
		p.line("%s", row)
	}
	for _, w := range c.windows {
		p.line("")
		p.line("The standing of each rule against `main` on %s at each factor:", w.Name)
		p.line("")
		factors := make([]string, 0, len(c.factors))
		for _, f := range c.factors {
			factors = append(factors, "f "+formatNumber(f))
		}
		p.line("%s", tableHead("rule", factors))
		for _, rule := range c.rules {
			if rule == record.MainRule.Name {
				continue
			}
			result := results[partKey{window: w.Name, rule: rule}]
			row := "| `" + rule + "` |"
			for _, f := range c.factors {
				row += " " + string(standingAt(result, f)) + " |"
			}
			p.line("%s", row)
		}
	}
	p.line("")
}

// standingAt is result's standing at the factor f, "none" where the search ran none there.
func standingAt(result BreakEvenResult, f float64) Standing {
	for _, s := range result.Standings {
		if s.Slowdown == f {
			return s.Standing
		}
	}
	return "none"
}

// windowNames is the names of windows, in their order.
func windowNames(windows []windowRecord) []string {
	names := make([]string, 0, len(windows))
	for _, w := range windows {
		names = append(names, w.Name)
	}
	return names
}

// writeAssumptions writes the scenario and the assumptions each run used, for each window, rule
// and branch the union over its seeds, and then each assumption named, with its text.
func (c comparison) writeAssumptions(p *printer) {
	p.line("## The assumptions and scenarios each run used")
	used := map[int]bool{}
	for _, w := range c.windows {
		p.line("")
		p.line("### %s", w.Name)
		p.line("")
		for _, rule := range c.rules {
			part := c.runs.parts[partKey{window: w.Name, rule: rule}]
			for _, run := range slices.Concat(part.plan.Branches, part.plan.BreakEven) {
				label := run.label()
				numbers := map[int]bool{}
				scenarios := map[string]bool{}
				for _, read := range part.runs[label] {
					for _, number := range read.assumptions {
						numbers[number], used[number] = true, true
					}
					scenarios[read.scenario] = true
				}
				p.line("- `%s`, `%s` (seeds %s): scenario %s; assumptions %s.", rule, label, seedList(run.Seeds),
					strings.Join(slices.Sorted(maps.Keys(scenarios)), ", "), numberList(slices.Sorted(maps.Keys(numbers))))
			}
		}
	}
	p.line("")
	p.line("### The assumptions named")
	p.line("")
	for _, assumption := range Assumptions {
		if used[assumption.Number] {
			p.line("%d. %s", assumption.Number, assumption.Text)
		}
	}
}

// seedList is seeds as the document lists them.
func seedList(seeds []int64) string {
	texts := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		texts = append(texts, fmt.Sprint(seed))
	}
	return strings.Join(texts, ", ")
}

// numberList is numbers as the document lists them, "none" for none.
func numberList(numbers []int) string {
	if len(numbers) == 0 {
		return "none"
	}
	texts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		texts = append(texts, fmt.Sprint(number))
	}
	return strings.Join(texts, ", ")
}
