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

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Costs is what a rule is judged on against main (spec R7): the issues finished and the owner's
// three costs, each read from one scorecard or the mean of a branch's seeds.
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
}

// costsOf is the costs card gives, its loops and waits summed in the order of their keys so the
// same card always gives the same sum.
func costsOf(card Scorecard) Costs {
	c := Costs{Done: float64(card.Done), WaitOnlyForSlot: card.WaitOnlyForSlot.Hours, PriorityTaken: card.PriorityTakenBanded}
	for _, cause := range slices.Sorted(maps.Keys(card.Loops)) {
		c.OtherDelays += card.Loops[cause].Hours
	}
	for _, word := range slices.Sorted(maps.Keys(card.Waits)) {
		c.OtherDelays += card.Waits[word].Hours
	}
	return c
}

// meanCosts is the mean of the costs of runs, summed in their order.
func meanCosts(runs []runRead) Costs {
	var sum Costs
	for _, run := range runs {
		sum.Done += run.costs.Done
		sum.WaitOnlyForSlot += run.costs.WaitOnlyForSlot
		sum.PriorityTaken += run.costs.PriorityTaken
		sum.OtherDelays += run.costs.OtherDelays
	}
	if n := float64(len(runs)); n > 0 {
		sum.Done, sum.WaitOnlyForSlot, sum.PriorityTaken, sum.OtherDelays = sum.Done/n, sum.WaitOnlyForSlot/n, sum.PriorityTaken/n, sum.OtherDelays/n
	}
	return sum
}

// costMeasure is one of the measures a rule is judged on: its name, whether more of it is better,
// R5's gap of it, and its value among a Costs.
type costMeasure struct {
	name         string
	higherBetter bool
	gap          Gap
	of           func(Costs) float64
}

// costMeasures is the issues finished and the owner's three costs, each with R5's gap: 10% of the
// issues finished, 30% of the hours of started work that waits only for a slot, 0.05 of the share
// of priority taken, read in today's line's order (priorityTakenBanded, which no test of R5 reads,
// judged within the gap R5 gives the share by rank alone), and 30% of the hours of the other delays.
var costMeasures = []costMeasure{
	{name: "done", higherBetter: true, gap: gapTenth, of: func(c Costs) float64 { return c.Done }},
	{name: "waitOnlyForSlot.hours", gap: gapThirty, of: func(c Costs) float64 { return c.WaitOnlyForSlot }},
	{name: "priorityTakenBanded", gap: gapPriority, of: func(c Costs) float64 { return c.PriorityTaken }},
	{name: "otherDelays.hours", gap: gapThirty, of: func(c Costs) float64 { return c.OtherDelays }},
}

// verdict is how a rule stands against main on one measure.
type verdict int

const (
	// verdictLevel is no worse than main by more than R5's gap, and not better.
	verdictLevel verdict = iota
	// verdictBetter is better than main by any amount.
	verdictBetter
	// verdictWorse is worse than main by more than R5's gap.
	verdictWorse
)

// judge is how value, a rule's, stands against main's on m: worse where it is worse by more than
// R5's gap of main's value, better where it is better at all, and level otherwise.
func judge(m costMeasure, value, main float64) verdict {
	worse := value - main
	if m.higherBetter {
		worse = -worse
	}
	allowed := m.gap.Allowed(main)
	switch {
	case worse > allowed+tolerance(allowed, value, main):
		return verdictWorse
	case worse < -tolerance(value, main):
		return verdictBetter
	}
	return verdictLevel
}

// Standing is how a rule stands against main on one branch of one window, over the issues finished
// and the owner's three costs (costMeasures): the rank whose change the break-even search looks for.
type Standing string

const (
	// StandingBetter is no worse than main by more than R5's gap on each measure and better on at
	// least one: what a rule must be on every branch that passes R5 to be named best.
	StandingBetter Standing = "better"
	// StandingWorse is worse than main by more than R5's gap on at least one measure and better on
	// none.
	StandingWorse Standing = "worse"
	// StandingTrade is worse than main by more than R5's gap on one measure and better on another.
	StandingTrade Standing = "trade"
	// StandingLevel is no worse than main by more than R5's gap on any measure and better on none.
	StandingLevel Standing = "level"
)

// standingOf is how rule's costs stand against main's.
func standingOf(rule, main Costs) Standing {
	better, worse := false, false
	for _, m := range costMeasures {
		switch judge(m, m.of(rule), m.of(main)) {
		case verdictBetter:
			better = true
		case verdictWorse:
			worse = true
		}
	}
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

// BranchCosts is every rule's costs on one branch of one window, main's among them, by rule name,
// and whether the branch gives a comparison: its window's main branch and it pass R5.
type BranchCosts struct {
	Window string
	Branch BranchAt
	Passes bool
	Costs  map[string]Costs
}

// MeasureTrade is how a rule stood against main on one measure over the branches a decision read:
// on how many it was better, worse by more than R5's gap, and neither.
type MeasureTrade struct {
	Measure string `json:"measure"`
	Better  int    `json:"better"`
	Worse   int    `json:"worse"`
	Level   int    `json:"level"`
}

// Trade is how a rule stood against main on each measure it is judged on (costMeasures).
type Trade struct {
	Rule     string         `json:"rule"`
	Measures []MeasureTrade `json:"measures"`
}

// Decision is which rule a comparison names best (spec R7): each rule that, on every branch of the
// windows that passes R5, is no worse than main by more than R5's gap on the issues finished and on
// each of the owner's three costs, and better on at least one; how many branches that is; and each
// rule's trade over them. No branch that fails R5 is read, so none can name a rule, and with no
// branch that passes no rule is named.
type Decision struct {
	Best     []string `json:"best"`
	Branches int      `json:"branches"`
	Trades   []Trade  `json:"trades"`
}

// Decide is the decision over branches for each of rules, main left out of them: main's costs and
// each rule's on each branch are its branch's Costs.
func Decide(rules []string, branches []BranchCosts) Decision {
	var passing []BranchCosts
	for _, b := range branches {
		if b.Passes {
			passing = append(passing, b)
		}
	}
	decision := Decision{Best: []string{}, Branches: len(passing), Trades: []Trade{}}
	for _, rule := range rules {
		if rule == record.MainRule.Name {
			continue
		}
		trade := Trade{Rule: rule}
		for _, m := range costMeasures {
			trade.Measures = append(trade.Measures, MeasureTrade{Measure: m.name})
		}
		best := len(passing) > 0
		for _, b := range passing {
			costs, ran := b.Costs[rule]
			main, mainRan := b.Costs[record.MainRule.Name]
			if !ran || !mainRan {
				best = false
				continue
			}
			// The rule stands better than main on the branch (standingOf) where it is better on some
			// measure and worse on none.
			better, worse := false, false
			for i, m := range costMeasures {
				switch judge(m, m.of(costs), m.of(main)) {
				case verdictBetter:
					trade.Measures[i].Better++
					better = true
				case verdictWorse:
					trade.Measures[i].Worse++
					worse = true
				default:
					trade.Measures[i].Level++
				}
			}
			if !better || worse {
				best = false
			}
		}
		if best {
			decision.Best = append(decision.Best, rule)
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

// BreakEven is the break-even search of a rule whose costs on the main branch at each slowdown
// factor are rule, against main's, main: over factors in order, each that both ran.
func BreakEven(factors []float64, rule, main map[float64]Costs) BreakEvenResult {
	result := BreakEvenResult{Standings: []FactorStanding{}}
	for _, f := range factors {
		costs, ran := rule[f]
		mainCosts, mainRan := main[f]
		if !ran || !mainRan {
			continue
		}
		standing := standingOf(costs, mainCosts)
		if len(result.Standings) > 0 && !result.Changes && standing != result.Standings[0].Standing {
			result.Factor, result.Changes = f, true
		}
		result.Standings = append(result.Standings, FactorStanding{Slowdown: f, Standing: standing})
	}
	return result
}

// runPlan is what each part of a validation or a comparison runs: its branches, the factors of
// the break-even search it runs the main branch at beyond them, the diagnostic branches a
// validation runs beside them (DiagnosticBranches), and the seeds of a branch with a draw.
type runPlan struct {
	branches    []BranchAt
	breakEven   []float64
	diagnostics []BranchAt
	perDraw     int
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
// written, which part of the comparison this process runs, and the commit of the legion binary
// that makes the runs.
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
		keepLogs: cfg.KeepLogs, progress: cfg.Progress, build: cfg.BuildCommit})
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
// backtest and at which flat cap, and the commit of the legion binary that makes them
// (RunConfig.BuildCommit).
type ValidateConfig struct {
	Window      string
	Rule        record.SlotRule
	PGDSN       string
	Parallel    int
	Out         string
	KeepLogs    bool
	Progress    func(RunDone)
	BuildCommit string
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
// (fullPlan), otherwise. Each but the cap backtest's runs R5's branches of the recorded conflicts
// beside them (DiagnosticBranches), which no comparison runs.
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
	plan.diagnostics = DiagnosticBranches()
	return plan
}

// CapBacktestDir is the directory of runs the cap backtest of a validation into out writes its
// runs at the flat cap limit under: cap-<limit> in out, a directory of runs of its own beside the
// windows', which a reader of out passes over (ReadRuns).
func CapBacktestDir(out string, limit int) string {
	return filepath.Join(out, fmt.Sprintf("cap-%d", limit))
}

// RunValidate runs cfg.Window under cfg.Rule on every branch with its seeds, or on the main branch
// alone with its seeds under cfg.MainOnly or cfg.CapBacktest (validatePlan), and on R5's branches of
// the recorded conflicts beside them but in the cap backtest, into cfg.Out, up to cfg.Parallel runs
// at once, and answers its validation (spec R5), read back from the runs. The cap backtest runs its
// branches again at the flat cap cfg.Cap into CapBacktestDir and gives the validation its
// CapBacktest. A rule other than the one the window's parts say its daemon ran is refused, as is a
// cap of the validation's own outside the cap backtest and a cap backtest at no cap above zero.
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
		build: cfg.BuildCommit}
	spec := windowSpec{dir: cfg.Window, name: name, parts: []partSpec{{rule: rule, branches: plan.branches, diagnostics: plan.diagnostics}}}
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
// databases on, how many run at once, the directory of runs they are written under, and the commit
// of the legion binary that makes them (RunConfig.BuildCommit).
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
		keepLogs: cfg.KeepLogs, progress: progress, build: cfg.BuildCommit, sameBuild: true}); err != nil {
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

// costsAt is the mean costs of rule's runs on the branch labelled label of window.
func (c comparison) costsAt(window, rule, label string) (Costs, bool) {
	part, ok := c.runs.parts[partKey{window: window, rule: rule}]
	if !ok {
		return Costs{}, false
	}
	runs, ok := part.runs[label]
	if !ok || len(runs) == 0 {
		return Costs{}, false
	}
	return meanCosts(runs), true
}

// decision is the comparison's decision over every branch of both windows (Decide), each branch
// passing where its window's validation says it gives a comparison.
func (c comparison) decision() Decision {
	var branches []BranchCosts
	for i, w := range c.windows {
		for _, at := range c.branches {
			b := BranchCosts{Window: w.Name, Branch: at, Passes: c.validations[i].Compares(at), Costs: map[string]Costs{}}
			for _, rule := range c.rules {
				if costs, ok := c.costsAt(w.Name, rule, at.Label()); ok {
					b.Costs[rule] = costs
				}
			}
			branches = append(branches, b)
		}
	}
	return Decide(c.rules, branches)
}

// breakEven is the break-even search of rule against main on window, over the comparison's factors.
func (c comparison) breakEven(window, rule string) BreakEvenResult {
	ruled, mained := map[float64]Costs{}, map[float64]Costs{}
	for _, f := range c.factors {
		label := BranchAt{Branch: MainBranch, Slowdown: f}.Label()
		if costs, ok := c.costsAt(window, rule, label); ok {
			ruled[f] = costs
		}
		if costs, ok := c.costsAt(window, record.MainRule.Name, label); ok {
			mained[f] = costs
		}
	}
	return BreakEven(c.factors, ruled, mained)
}

// Comparison is the document of a comparison of slot rules (spec R7), written in Markdown from the
// runs alone, so it is the same whether one process made every run or many processes each made a
// part: the windows, rules, build commits of the runs, branches, seeds and factors it read; the
// rule it names best, or that none is, with each rule's trade and why each candidate added to the
// spec's rules is compared; each window's scorecard read from its record, the wait of its P0 and
// P1 work among it; what each rule's grace keeps over the waits that gave a slot back in each
// window's record; each window's validation; the forecast across the change, with the workflow
// commits it carries too; which window measures how a rule changes conflicts; the value of every
// measure for every rule, window and branch, read from the snapshot timeline, with the lowest and
// the highest seed, the wait of P0 and P1 work among them (urgentRows); where each issue's times
// are; the gap of each measure between the snapshot and the line timelines; each rule's
// break-even slowdown factor; and the assumptions and the scenario each run used. Every
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
	var doc strings.Builder
	p := &printer{w: &doc}
	c.writeHead(p, forecast)
	c.writeDecision(p, forecast)
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
	c.writeMatrix(p)
	p.line("## Each issue's first slot and done under each rule")
	p.line("")
	p.line("`%s`, beside this document, gives for each window and each rule each issue that held a slot in the record or in the rule's replay on the main branch (`%s`): its first slot and its done in the record and in the replay, each replayed time the median over the main branch's seeds.",
		filepath.Base(issuesFile), MainBranchAt.Label())
	p.line("")
	c.writeGaps(p)
	c.writeBreakEven(p)
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
			draws = fmt.Sprintf("%d of its %d stretches ended in a conflict, so every branch draws conflicts", w.Conflicted, w.Stretches)
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

// writeDecision writes the rule the comparison names best, or that none is and why, and each
// rule's trade against main over the branches that pass R5, beside why each candidate added to the
// spec's rules is compared (candidateReasons).
func (c comparison) writeDecision(p *printer, forecast ForecastResult) {
	decision := c.decision()
	p.line("## The rule named best (R7)")
	p.line("")
	p.line("A rule is named best when, on every branch of both windows that passes R5, it is no worse than `main` by more than R5's gap on the issues finished (`done`, %s) and on each of the owner's three costs — started work that waits only for a slot (`waitOnlyForSlot.hours`, %s), priority taken by holders in today's line's order, the priority band first (`priorityTakenBanded`, %s, the gap R5 gives the share by rank alone) and the other delays (`otherDelays.hours`: the hours of every send-back loop and every wait on another issue, %s) — and better on at least one, each read from the snapshot timeline as the mean of the branch's seeds.",
		gapTenth, gapThirty, gapPriority, gapThirty)
	p.line("Both windows were recorded before the line put priority first, so the tables of every measure give priority taken by rank alone (`priorityTaken`), the order of their own lines, beside it.")
	for i, w := range c.windows {
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
		p.line("`%s` is named best: on each of the %d branches that pass R5 it is no worse than `main` by more than R5's gap on each measure, and better on at least one.",
			decision.Best[0], decision.Branches)
	case len(decision.Best) > 1:
		p.line("No one rule is named best: %s each meet the test on the %d branches that pass R5, so the comparison names none over the others.",
			codeList(decision.Best), decision.Branches)
	default:
		p.line("No rule is named best: of the %d branches that pass R5, each rule has one on which it is worse than `main` by more than R5's gap on some measure, or better on none.",
			decision.Branches)
	}
	p.line("")
	p.line("Each rule's trade against `main` over the %d branches that pass R5: on how many it is better, worse by more than R5's gap, and neither; and why each rule the spec does not name was added.",
		decision.Branches)
	p.line("")
	p.line("%s", tableHead("rule", slices.Concat(costMeasureNames(), []string{"why it was added"})))
	for _, trade := range decision.Trades {
		row := "| `" + trade.Rule + "` |"
		for _, m := range trade.Measures {
			row += fmt.Sprintf(" better %d, worse %d, neither %d |", m.Better, m.Worse, m.Level)
		}
		p.line("%s", row+" "+candidateReasons[trade.Rule]+" |")
	}
	p.line("")
	p.line("Each rule's costs and `main`'s on the main branch of each window:")
	p.line("")
	p.line("%s", tableHead("window", slices.Concat([]string{"rule"}, costMeasureNames(), []string{"standing against main"})))
	for _, w := range c.windows {
		main, _ := c.costsAt(w.Name, record.MainRule.Name, MainBranchAt.Label())
		for _, rule := range c.rules {
			costs, ok := c.costsAt(w.Name, rule, MainBranchAt.Label())
			if !ok {
				continue
			}
			standing := "(the rule judged against)"
			if rule != record.MainRule.Name {
				standing = string(standingOf(costs, main))
			}
			row := "| " + w.Name + " | `" + rule + "` |"
			for _, m := range costMeasures {
				row += " " + formatNumber(m.of(costs)) + " |"
			}
			p.line("%s", row+" "+standing+" |")
		}
	}
	p.line("")
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
			p.line("%s measures how a rule changes conflicts: %d of its %d stretches ended in a conflict, and each replay draws its conflicts from the rate fitted on them (assumption 3).",
				w.Name, w.Conflicted, w.Stretches)
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
// line timeline gives (Scorecard.DoneByAnyone).
func (c comparison) writeMatrix(p *printer) {
	p.line("## Every measure for every rule, window and branch (the snapshot timeline)")
	p.line("")
	writeUrgentNote(p)
	p.line("`priorityTaken` ranks by rank alone, the order of the recorded windows' lines, and `priorityTakenBanded` in today's line's order, the band of the Dispatch priority that places each issue first.")
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
		}
	}
	p.line("")
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
// its standing at each factor searched.
func (c comparison) writeBreakEven(p *printer) {
	p.line("## The break-even slowdown factor of each rule against main")
	p.line("")
	p.line("A rule's rank against `main` is its standing on the main branch: better, worse, a trade, or level (the rule named best is better on every branch).")
	p.line("The break-even factor is the first slowdown factor of the search at which that standing differs from its standing at the first.")
	p.line("")
	results := map[partKey]BreakEvenResult{}
	for _, rule := range c.rules {
		for _, w := range c.windows {
			if rule != record.MainRule.Name {
				results[partKey{window: w.Name, rule: rule}] = c.breakEven(w.Name, rule)
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
