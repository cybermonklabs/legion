package replay

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// The limit test of R11 (spec R11, R7): `legion replay limits` plans the replays that name each
// signal's limit and, given the directories of the runs, names it by R11's rule and prints its
// table. Nothing here runs a replay; the runs are `legion replay run`s, one for each entry of an
// indexed Job, whose output PlanLimits reads back.
//
// A limit is named from the runs in three steps. Step A runs each signal alone, with the others
// off, at each limit of its list and at off, on the main branch of both windows. Step B runs, on
// every branch of both windows that passes R5, the limit with the most issues finished among those
// no measure of which is worse than under today's rule on step A's branches, and the next tighter
// one, and where the first is worse on a branch, the next tighter limit again, until one holds or
// the list ends. Step C runs the two rules that admit by signals with every signal at its named
// limit on every branch of both windows. Each step runs at each of the sweep's ceilings. A
// comparison with today's rule is R7's (judgeBranch), and the code that makes it is the
// comparison's.

// The three steps of the limit test.
const (
	LimitStepA = "A"
	LimitStepB = "B"
	LimitStepC = "C"
)

// The statuses of a signal's limit at one ceiling.
const (
	LimitNamed   = "named"
	LimitNone    = "no limit"
	LimitWaiting = "waiting for runs"
)

// limitPercentiles are the percentiles of a signal over window one's record that its list of
// limits holds, and limitIdleAfters the d, beside an hour, at which the idle share's named limit is
// also run (spec R11).
var (
	limitPercentiles = []int{50, 75, 90, 95, 99}
	limitIdleAfters  = []time.Duration{10 * time.Minute, 4 * time.Hour}
)

// limitRule is the rule of steps A and B, which admits by signals with the cap as a ceiling; step C
// runs both rules of limitCombinedRules.
const limitRule = "signals"

var limitCombinedRules = []string{"signals", "signals-backoff"}

// LimitCandidate is one limit proposed for a signal: the percentile of the signal over window one's
// record that it is, its value, and why it is left out of the list, "" where the sweep runs it. A
// percentile outside the signal's range (signals.CheckLimit) is never clamped into it.
type LimitCandidate struct {
	Percentile int     `json:"percentile"`
	Limit      float64 `json:"limit"`
	Left       string  `json:"left,omitempty"`
}

// LimitGate says whether a signal has steps: the signals that come from a model have them only where
// the model passed R11's tests; why it has none where it has not; and what its limit rests on.
type LimitGate struct {
	Signal string   `json:"signal"`
	Gives  bool     `json:"gives"`
	Why    string   `json:"why,omitempty"`
	Rests  []string `json:"rests,omitempty"`
}

// LimitEntry is one replay of the sweep, one entry of an indexed Job: the run's place (Variant and
// Out, under the sweep's directory of runs), what it runs (the rule, branch, slowdown, seed, limits,
// d, cap and token limit) and the arguments of `legion replay run` that run it, but for the
// database it makes its own on. Ran says its directory holds a whole run already.
type LimitEntry struct {
	Index      int                `json:"index"`
	Step       string             `json:"step"`
	Signal     string             `json:"signal,omitempty"`
	Window     string             `json:"window"`
	Dir        string             `json:"dir"`
	Rule       string             `json:"rule"`
	Branch     string             `json:"branch"`
	Slowdown   float64            `json:"slowdown"`
	Seed       int64              `json:"seed"`
	Limits     map[string]float64 `json:"limits"`
	IdleAfter  Duration           `json:"idleAfter"`
	Cap        int                `json:"cap"`
	TokenLimit int64              `json:"tokenLimit,omitempty"`
	Variant    string             `json:"variant"`
	Out        string             `json:"out"`
	Ran        bool               `json:"ran"`
	Args       []string           `json:"args,omitempty"`
}

// args is the arguments of `legion replay run` that make e's run under runs, with no --pg-dsn.
func (e LimitEntry) args(runs string) []string {
	args := []string{"replay", "run", "--window", e.Dir, "--rule", e.Rule, "--branch", e.Branch, "--seed", strconv.FormatInt(e.Seed, 10),
		"--slowdown", strconv.FormatFloat(e.Slowdown, 'f', -1, 64)}
	if e.Cap > 0 {
		args = append(args, "--cap", strconv.Itoa(e.Cap))
	}
	for _, name := range slices.Sorted(maps.Keys(e.Limits)) {
		args = append(args, "--limit", name+"="+strconv.FormatFloat(e.Limits[name], 'g', -1, 64))
	}
	args = append(args, "--idle-after", time.Duration(e.IdleAfter).String())
	if e.TokenLimit > 0 {
		args = append(args, "--token-limit", strconv.FormatInt(e.TokenLimit, 10))
	}
	return append(args, "--out", filepath.Join(runs, e.Out))
}

// LimitWindow is a window of the sweep: its name in the runs and its directory.
type LimitWindow struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// LimitsInput is what the limit test reads. Windows[0] is window one, whose record the lists of
// limits are read from and whose main branch ranks the limits by the issues they finish, and
// Windows[1] window two. Models[0] is `legion replay models --against` on window one against the
// other record, and Models[1] the other record against window one. Baseline holds the runs of
// today's rule, main, and of each window's own rule on both windows' branches, with the windows'
// records, as a validation and a comparison write them; Compares says whether a branch of a
// window passes R5, from the baseline's validation of the window under its own rule where it is
// nil. RunsDir is where the sweep's runs are, the directory the entries' Out are relative to.
// ProviderSpans is the span of each window's provider record. Ceilings are the caps the sweep
// runs at, 0 for the recorded cap.
type LimitsInput struct {
	Windows       [2]LimitWindow
	Candidates    map[string][]LimitCandidate
	Models        [2]LoadModels
	TokenLimit    int64
	Ceilings      []int
	Baseline      Runs
	Compares      func(window string, at BranchAt) bool
	RunsDir       string
	ProviderSpans [2][]Span
}

// FlightRange is the range of work in flight that the windows record: from the lowest day's
// average of issues in flight in the models' days to the most issues in flight at any instant of
// either window's scorecard. A limit holds only inside it.
type FlightRange struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// LimitRow is one limit of a signal at one ceiling, or off, with what its runs measured on window
// one's main branch, the mean over the seeds: the issues finished, the three costs, and the signal's
// own reading (SignalSummary) where the limit is a signal's; the most issues in flight any of its
// runs held; whether it holds, and why not; and the refused minutes the tokens model gave under the
// supplied limit that the record cannot confirm.
type LimitRow struct {
	Limit           float64            `json:"limit"`
	Off             bool               `json:"off,omitempty"`
	Percentile      int                `json:"percentile,omitempty"`
	Ran             bool               `json:"ran"`
	Done            float64            `json:"done"`
	DoneVerdict     Verdict            `json:"doneVerdict"`
	WaitOnlyForSlot float64            `json:"waitOnlyForSlot"`
	PriorityTaken   float64            `json:"priorityTaken"`
	OtherDelays     float64            `json:"otherDelays"`
	Signal          SignalSummary      `json:"signal"`
	Peak            int                `json:"peak"`
	Holds           string             `json:"holds"`
	WorseOn         []string           `json:"worseOn,omitempty"`
	IdleAfter       Duration           `json:"idleAfter,omitempty"`
	Unconfirmed     map[string]float64 `json:"unconfirmed,omitempty"`
}

// CeilingLimits is a signal's limits at one ceiling: its rows, off first, the limit named and its
// status, which step the plan waits for where a run is missing, whether the named limit is an
// extrapolation (its runs hold more issues in flight than the windows record), and, for the idle
// share, its named limit read at the d beside an hour.
type CeilingLimits struct {
	Ceiling      int        `json:"ceiling"`
	Rows         []LimitRow `json:"rows"`
	Named        float64    `json:"named"`
	HasNamed     bool       `json:"hasNamed"`
	Percentile   int        `json:"percentile,omitempty"`
	Status       string     `json:"status"`
	Waits        string     `json:"waits,omitempty"`
	Extrapolates bool       `json:"extrapolates"`
	Peak         int        `json:"peak"`
	IdleReads    []LimitRow `json:"idleReads,omitempty"`
}

// SignalLimits is one signal's part of the result.
type SignalLimits struct {
	Signal     string           `json:"signal"`
	Gate       LimitGate        `json:"gate"`
	Candidates []LimitCandidate `json:"candidates"`
	Ceilings   []CeilingLimits  `json:"ceilings"`
}

// CombinedRow is step C at one ceiling under one rule: the rule with every named limit at once,
// how many branches that pass R5 it was judged on, and on how many it stood better, worse, as a
// trade or level against today's rule (Standing), with the measures it was worse on.
type CombinedRow struct {
	Ceiling  int                `json:"ceiling"`
	Rule     string             `json:"rule"`
	Limits   map[string]float64 `json:"limits"`
	Complete bool               `json:"complete"`
	Branches int                `json:"branches"`
	Better   int                `json:"better"`
	Worse    int                `json:"worse"`
	Trade    int                `json:"trade"`
	Level    int                `json:"level"`
	WorseOn  []string           `json:"worseOn,omitempty"`
}

// LimitsResult is the limit test as far as the runs go: the token limit every run takes, the range
// of work in flight, each signal's limits, step C, and every entry of the plan. Entries with Ran
// false are the runs still to make.
type LimitsResult struct {
	TokenLimit int64          `json:"tokenLimit,omitempty"`
	Range      FlightRange    `json:"range"`
	Signals    []SignalLimits `json:"signals"`
	Combined   []CombinedRow  `json:"combined"`
	Entries    []LimitEntry   `json:"entries"`
	Ceilings   []int          `json:"ceilings"`
	// Waits is the step the plan waits for the runs of, "" where it waits for none.
	Waits string `json:"waits,omitempty"`
}

// Pending is the entries of r still to run, each directory once.
func (r LimitsResult) Pending() []LimitEntry {
	var pending []LimitEntry
	seen := map[string]bool{}
	for _, e := range r.Entries {
		if !e.Ran && !seen[e.Out] {
			seen[e.Out] = true
			pending = append(pending, e)
		}
	}
	return pending
}

// LimitCandidates is each signal's list of limits over w's record, every signal of
// signals.Names(): the percentiles of its value at each minute's start from w.From to w.To, where w
// records it, the idle share at d of an hour, and the provider's headroom, with the token limit
// given where the record holds none (assumption 49).
func LimitCandidates(w Window, tokenLimit int64) (map[string][]LimitCandidate, error) {
	r, err := ReadSignalRecord(w, signals.DefaultIdleAfter, tokenLimit)
	if err != nil {
		return nil, err
	}
	reads := []struct {
		name string
		at   func(time.Time) signals.Value
	}{
		{signals.Idle, func(at time.Time) signals.Value { return idleAt(r, at) }},
		{signals.Queue, func(at time.Time) signals.Value { return queueAt(r, at) }},
		{signals.Provider, func(at time.Time) signals.Value { return providerAt(r, at) }},
		{signals.Refusals, func(at time.Time) signals.Value { return r.Refusals(at).Value() }},
		{signals.Kills, func(at time.Time) signals.Value { return killsAt(r, at) }},
		{signals.Approvals, func(at time.Time) signals.Value { return approvalsAt(r, at) }},
	}
	values := map[string][]float64{}
	for at := w.From; at.Before(w.To); at = at.Add(time.Minute) {
		for _, read := range reads {
			if value := read.at(at); value.Recorded {
				values[read.name] = append(values[read.name], value.Value)
			}
		}
	}
	candidates := map[string][]LimitCandidate{}
	for _, read := range reads {
		candidates[read.name] = candidatesOf(read.name, values[read.name])
	}
	return candidates, nil
}

// candidatesOf is the list of limits of signal over values, its value at each minute window one
// records it: its 50th, 75th, 90th, 95th and 99th percentile by the nearest rank, each through
// signals.CheckLimit, and one that repeats a lower percentile's limit left out as its repeat. A
// percentile outside the signal's range is left out with CheckLimit's reason and never clamped.
func candidatesOf(signal string, values []float64) []LimitCandidate {
	sorted := slices.Sorted(slices.Values(values))
	list := make([]LimitCandidate, 0, len(limitPercentiles))
	for _, p := range limitPercentiles {
		c := LimitCandidate{Percentile: p}
		if len(sorted) == 0 {
			c.Left = "window one records no value of the signal"
			list = append(list, c)
			continue
		}
		c.Limit = nearestRank(sorted, float64(p)/100)
		if err := signals.CheckLimit(signal, c.Limit); err != nil {
			c.Left = err.Error()
		} else if i := slices.IndexFunc(list, func(earlier LimitCandidate) bool { return earlier.Left == "" && earlier.Limit == c.Limit }); i >= 0 {
			c.Left = fmt.Sprintf("the same limit as the %dth percentile's", list[i].Percentile)
		}
		list = append(list, c)
	}
	return list
}

// modelOf is the model named name of models.
func modelOf(models LoadModels, name string) (LoadModel, bool) {
	for _, m := range models.Models {
		if m.Name == name {
			return m, true
		}
	}
	return LoadModel{}, false
}

// limitGate is signal's gate over the two outputs of `legion replay models` (LimitsInput.Models). The
// idle share and the approvals come from the replay itself, have no model and always have steps. A
// signal that a model gives has them where the model passes R11's daily test on at least one of the
// two records and fails it on neither, and passes the test across the two records on both; the
// provider's headroom and refusals, which the tokens model gives, also where a token limit is
// given and the model fails neither test, since the window records no limit of the project's own
// to test it against (assumption 49). A signal without steps names why. What a limit rests on is
// each tested day with the recorded and the modelled value and the record it is in, the test
// across the two records, and, for the tokens model, the token limit given.
func limitGate(signal string, models [2]LoadModels, tokenLimit int64) LimitGate {
	gate := LimitGate{Signal: signal}
	name, modelled := signalModels[signal]
	if !modelled {
		gate.Gives, gate.Rests = true, []string{"the signal comes from the replay itself and has no model"}
		return gate
	}
	labels := [2]string{"window one's record", "the other record"}
	var found [2]LoadModel
	for i := range models {
		m, ok := modelOf(models[i], name)
		if !ok {
			gate.Why = fmt.Sprintf("the output of `legion replay models` on %s holds no %s model", labels[i], name)
			return gate
		}
		found[i] = m
	}
	for i, m := range found {
		if m.Verdict == VerdictFails {
			gate.Why = fmt.Sprintf("the %s model fails R11's daily test on %s", name, labels[i])
			return gate
		}
	}
	for i, m := range found {
		if m.Across != VerdictPasses {
			reason := ""
			if m.AcrossReason != "" {
				reason = ": " + m.AcrossReason
			}
			gate.Why = fmt.Sprintf("the %s model does not pass R11's test across the two records on %s (%s%s)", name, labels[i], m.Across, reason)
			return gate
		}
	}
	supplied := name == ModelTokens && tokenLimit > 0
	if found[0].Verdict != VerdictPasses && found[1].Verdict != VerdictPasses && !supplied {
		gate.Why = fmt.Sprintf("the %s model gives no daily test on either record, and no token limit is given", name)
		return gate
	}
	gate.Gives = true
	if supplied {
		gate.Rests = append(gate.Rests, fmt.Sprintf("the token limit given: %d tokens a minute, supplied in each minute whose provider record holds no limit of the project's own (assumption 49)", tokenLimit))
	}
	for i, m := range found {
		for _, day := range m.Days {
			if day.Recorded != nil && day.Modelled != nil {
				gate.Rests = append(gate.Rests, dayLine(name, labels[i], day))
			}
		}
	}
	for i, m := range models {
		if m.Across == nil {
			continue
		}
		for _, rate := range m.Across.Rates {
			if rate.Model != name || rate.Ratio == nil {
				continue
			}
			where := ""
			if rate.Repository != "" {
				where = " of " + rate.Repository
			}
			gate.Rests = append(gate.Rests, fmt.Sprintf("the test across the two records, read on %s: %s%s, ratio %s (%s)", labels[i], rate.Rate, where,
				strconv.FormatFloat(*rate.Ratio, 'f', 4, 64), rate.Verdict))
		}
	}
	return gate
}

// dayLine is a day of a model that R11's daily test read, as the table gives it: the day, the
// record it is in, and the recorded and the modelled value.
func dayLine(model, label string, day ModelDay) string {
	hours := strconv.FormatFloat(day.RecordHours, 'f', 1, 64)
	switch model {
	case ModelTokens:
		return fmt.Sprintf("tested day %s of %s: limit recorded for %s hours, %s refused minutes recorded, %s modelled (%s)", day.Day, label, hours,
			formatNumber(*day.Recorded), formatNumber(*day.Modelled), day.Verdict)
	case ModelMemory:
		return fmt.Sprintf("tested day %s of %s: %s kills recorded, %s modelled, over %s hours (%s)", day.Day, label, formatNumber(*day.Recorded),
			formatNumber(*day.Modelled), hours, day.Verdict)
	}
	return fmt.Sprintf("tested day %s of %s: median wait %s hours recorded, %s modelled, over %s hours (%s)", day.Day, label, formatNumber(*day.Recorded),
		formatNumber(*day.Modelled), hours, day.Verdict)
}

// limitVariant is what one directory of the sweep's runs shares: the limits, d and the ceiling. Its
// name is the directory's, under the sweep's runs.
type limitVariant struct {
	limits  map[string]float64
	idle    time.Duration
	ceiling int
}

// name is v's directory name: the limits, signal by signal with their values, or off; d where the
// idle share is limited at another than an hour; and the ceiling.
func (v limitVariant) name() string {
	name := "off"
	if len(v.limits) > 0 {
		parts := make([]string, 0, len(v.limits))
		for _, signal := range slices.Sorted(maps.Keys(v.limits)) {
			parts = append(parts, signal+"="+strconv.FormatFloat(v.limits[signal], 'g', -1, 64))
		}
		name = strings.Join(parts, "+")
	}
	if _, limited := v.limits[signals.Idle]; limited && v.idle != signals.DefaultIdleAfter {
		name += ".d" + v.idle.String()
	}
	if v.ceiling == 0 {
		return name + ".recorded-cap"
	}
	return name + ".cap-" + strconv.Itoa(v.ceiling)
}

// branchRun is a branch of a window the baseline ran, with its seeds.
type branchRun struct {
	at    BranchAt
	seeds []int64
}

// limitScope is which branches of a window a set of runs is on.
type limitScope int

const (
	// scopeMain is the main branch at f 1 alone (step A).
	scopeMain limitScope = iota
	// scopePassing is every branch that passes R5 (step B).
	scopePassing
	// scopeEvery is every branch the baseline ran (step C).
	scopeEvery
)

// limitRunSet is the runs of one variant under one rule on a scope of branches, read back: whether
// all of them are in, each branch's runs by window and branch, and the most issues in flight any
// of them held.
type limitRunSet struct {
	complete bool
	reads    map[string][]runRead
	peak     int
}

// readsKey is where a window's branch's runs are in a limitRunSet.
func readsKey(window int, at BranchAt) string { return strconv.Itoa(window) + "|" + at.Label() }

// costs is the costs of the runs of window's branch at, by seed.
func (s *limitRunSet) costs(window int, at BranchAt) SeedCosts {
	reads := s.reads[readsKey(window, at)]
	if len(reads) == 0 {
		return nil
	}
	costs := make(SeedCosts, len(reads))
	for _, read := range reads {
		costs[read.seed] = read.costs
	}
	return costs
}

// limitJudgement is how a set of runs stands against today's rule on the branches that pass R5 in
// its scope: the measures it is worse on (each by name, branch and window), the verdict on the
// issues finished on window one's main branch, how many branches it was judged on, and how many of
// them it stood each way on (Standing).
type limitJudgement struct {
	worseOn     []string
	doneVerdict Verdict
	branches    int
	standings   map[Standing]int
}

// limitPlanner is the limit test's reading of its input and the entries it plans.
type limitPlanner struct {
	in          LimitsInput
	windows     [2]windowRecord
	branches    [2][]branchRun
	held        map[string]float64
	quantile    func(int) float64
	high        float64
	validations [2]*Validation
	entries     []LimitEntry
	sets        map[string]*limitRunSet
	err         error
}

// fail keeps the first error.
func (p *limitPlanner) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// newLimitPlanner reads in's baseline: each window's record, the branches and seeds its runs of
// today's rule had, and the replay's own error on each measure (Runs.ownErrors).
func newLimitPlanner(in LimitsInput) (*limitPlanner, error) {
	p := &limitPlanner{in: in, quantile: studentQuantiles(), sets: map[string]*limitRunSet{}}
	for i, w := range in.Windows {
		rec, ok := in.Baseline.windows[w.Name]
		if !ok {
			return nil, fmt.Errorf("replay: the baseline runs hold no record of the window %s", w.Name)
		}
		part, ok := in.Baseline.parts[partKey{window: w.Name, rule: record.MainRule.Name}]
		if !ok {
			return nil, fmt.Errorf("replay: the baseline runs hold no part of %s under today's rule, %s", w.Name, record.MainRule.Name)
		}
		p.windows[i] = rec
		p.high = max(p.high, float64(rec.Scorecard.InFlight.Max))
		for _, run := range part.plan.Branches {
			at, err := run.at()
			if err != nil {
				return nil, err
			}
			p.branches[i] = append(p.branches[i], branchRun{at: at, seeds: run.Seeds})
		}
		if !slices.ContainsFunc(p.branches[i], func(b branchRun) bool { return b.at.Label() == MainBranchAt.Label() }) {
			return nil, fmt.Errorf("replay: the baseline runs of %s hold no run of the main branch at f 1", w.Name)
		}
	}
	before, after := p.windows[0], p.windows[1]
	if before.Order > after.Order {
		before, after = after, before
	}
	held, err := in.Baseline.ownErrors(before, after)
	if err != nil {
		return nil, err
	}
	p.held = held
	return p, nil
}

// compares says whether window w's branch at passes R5.
func (p *limitPlanner) compares(w int, at BranchAt) bool {
	if p.in.Compares != nil {
		return p.in.Compares(p.windows[w].Name, at)
	}
	if p.validations[w] == nil {
		v, err := p.in.Baseline.Validation(p.windows[w].Name, p.windows[w].Rule)
		if err != nil {
			p.fail(err)
			return false
		}
		p.validations[w] = &v
	}
	return p.validations[w].Compares(at)
}

// scoped is window w's branches in scope.
func (p *limitPlanner) scoped(w int, scope limitScope) []branchRun {
	var out []branchRun
	for _, b := range p.branches[w] {
		switch {
		case scope == scopeMain && b.at.Label() != MainBranchAt.Label():
			continue
		case scope == scopePassing && !p.compares(w, b.at):
			continue
		}
		out = append(out, b)
	}
	return out
}

// entry is the run of seed on window w's branch b under rule at variant v, as step lists it for
// signal.
func (p *limitPlanner) entry(step, signal string, v limitVariant, rule string, w int, b branchRun, seed int64) LimitEntry {
	e := LimitEntry{Step: step, Signal: signal, Window: p.in.Windows[w].Name, Dir: p.in.Windows[w].Dir, Rule: rule, Branch: b.at.Branch.Name,
		Slowdown: b.at.Slowdown, Seed: seed, Limits: maps.Clone(v.limits), IdleAfter: Duration(v.idle), Cap: v.ceiling, TokenLimit: p.in.TokenLimit,
		Variant: v.name(), Out: runDir(v.name(), p.in.Windows[w].Name, rule, b.at, seed)}
	if e.Limits == nil {
		e.Limits = map[string]float64{}
	}
	if p.in.RunsDir != "" {
		e.Args = e.args(p.in.RunsDir)
		_, e.Ran = runComplete(filepath.Join(p.in.RunsDir, e.Out))
	}
	return e
}

// read is the run e plans, read back from the sweep's directory of runs, where it is whole there. A
// run that ran with other limits, d, cap or token limit than the entry names is refused.
func (p *limitPlanner) read(e LimitEntry) (runRead, bool) {
	dir := filepath.Join(p.in.RunsDir, e.Out)
	read, err := readRun(dir, false, nil)
	if err != nil {
		p.fail(err)
		return runRead{}, false
	}
	if read.cap != e.Cap || read.tokenLimit != e.TokenLimit || read.idleAfter != time.Duration(e.IdleAfter) || !maps.Equal(read.limits, e.Limits) {
		p.fail(fmt.Errorf("replay: the run in %s ran with the limits %v, d %s, the cap %d and the token limit %d, and the plan names the limits %v, d %s, the cap %d and the token limit %d",
			dir, read.limits, read.idleAfter, read.cap, read.tokenLimit, e.Limits, time.Duration(e.IdleAfter), e.Cap, e.TokenLimit))
		return runRead{}, false
	}
	return read, true
}

// set plans, under step for signal, the runs of variant v under rule on scope's branches of both
// windows, once however often it is asked, and reads each back that is whole.
func (p *limitPlanner) set(step, signal string, v limitVariant, rule string, scope limitScope) *limitRunSet {
	key := fmt.Sprintf("%s|%s|%s|%d", step, v.name(), rule, scope)
	if s, planned := p.sets[key]; planned {
		return s
	}
	s := &limitRunSet{complete: true, reads: map[string][]runRead{}}
	for w := range p.windows {
		for _, b := range p.scoped(w, scope) {
			for _, seed := range b.seeds {
				e := p.entry(step, signal, v, rule, w, b, seed)
				p.entries = append(p.entries, e)
				if !e.Ran {
					s.complete = false
					continue
				}
				read, ok := p.read(e)
				if !ok {
					s.complete = false
					continue
				}
				read.seed = seed
				s.reads[readsKey(w, b.at)] = append(s.reads[readsKey(w, b.at)], read)
				s.peak = max(s.peak, read.snapshot.InFlight.Max)
			}
		}
	}
	p.sets[key] = s
	return s
}

// judge is how s, a complete set of runs, stands against today's rule on each branch of both windows
// in scope that passes R5, by the comparison's own code (judgeBranch): a measure other than the
// issues finished that is worse on a branch is a reason the limit does not hold, and the issues
// finished are read on its own, never a reason to reject (spec R11).
func (p *limitPlanner) judge(s *limitRunSet, scope limitScope) limitJudgement {
	j := limitJudgement{doneVerdict: VerdictNone, standings: map[Standing]int{}}
	for w := range p.windows {
		for _, b := range p.scoped(w, scope) {
			if !p.compares(w, b.at) || p.windows[w].unpaired() != "" {
				continue
			}
			costs := s.costs(w, b.at)
			main := p.in.Baseline.seedCostsAt(p.windows[w].Name, record.MainRule.Name, b.at.Label())
			if costs == nil || main == nil {
				p.fail(fmt.Errorf("replay: no runs of %s on %s to judge against today's rule, %s", p.windows[w].Name, b.at.Label(), record.MainRule.Name))
				continue
			}
			diffs := judgeBranch(costs, main, p.held, p.quantile)
			j.branches++
			j.standings[standingOf(diffs)]++
			if w == 0 && b.at.Label() == MainBranchAt.Label() {
				j.doneVerdict = diffs[0].Verdict
			}
			for i, d := range diffs {
				if i > 0 && d.Verdict == VerdictWorse {
					j.worseOn = append(j.worseOn, fmt.Sprintf("%s on %s in %s", d.Measure, b.at.Label(), p.windows[w].Name))
				}
			}
		}
	}
	return j
}

// row is the row of cand, or of off, from s's runs on window one's main branch, each number the mean
// over the seeds.
func (p *limitPlanner) row(signal string, cand LimitCandidate, off bool, s *limitRunSet) LimitRow {
	r := LimitRow{Limit: cand.Limit, Off: off, Percentile: cand.Percentile, Ran: s.complete, Peak: s.peak, Holds: "not run", DoneVerdict: VerdictNone}
	reads := s.reads[readsKey(0, MainBranchAt)]
	n := float64(len(reads))
	for _, read := range reads {
		r.Done += read.costs.Done / n
		r.WaitOnlyForSlot += read.costs.WaitOnlyForSlot / n
		r.PriorityTaken += read.costs.PriorityTaken / n
		r.OtherDelays += read.costs.OtherDelays / n
		if sum, limited := read.summaries[signal]; limited {
			r.Signal.Mean += sum.Mean / n
			r.Signal.P95 += sum.P95 / n
			r.Signal.Max = max(r.Signal.Max, sum.Max)
			r.Signal.HeldShare += sum.HeldShare / n
		}
	}
	return r
}

// unconfirmed is, by window and day, the refused minutes the tokens model gave under the supplied
// limit that the record cannot confirm, the mean over the seeds of each window's main branch.
func (p *limitPlanner) unconfirmed(s *limitRunSet) map[string]float64 {
	out := map[string]float64{}
	for w := range p.windows {
		reads := s.reads[readsKey(w, MainBranchAt)]
		for _, read := range reads {
			for day, minutes := range read.supplied {
				out[p.windows[w].Name+" "+day] += float64(minutes) / float64(len(reads))
			}
		}
	}
	return out
}

// holdsText is the reason a judgement gives for a limit holding or not.
func holdsText(j limitJudgement, scope string) string {
	if len(j.worseOn) == 0 {
		return "yes, on " + scope
	}
	text := "worse on " + j.worseOn[0]
	if len(j.worseOn) > 1 {
		text += fmt.Sprintf(" and %d more", len(j.worseOn)-1)
	}
	return text
}

// ceilingLimits is the named limit of signal at ceiling c over swept, its list of limits in
// ascending order, and the rows of its runs. Step A runs each limit and off on the main branch of
// both windows; where all are in, the limit with the most issues finished on window one's main
// branch among those worse than today's rule on no other measure there is named a candidate, the
// loosest of a tie, and step B runs it and the next tighter limit that is a candidate on every
// branch that passes R5; the first of them that is worse on no measure on any such branch is
// named, and where one is, the next tighter limit is run on every branch, until one holds or the
// list ends.
func (p *limitPlanner) ceilingLimits(signal string, swept []LimitCandidate, c int) CeilingLimits {
	out := CeilingLimits{Ceiling: c, Status: LimitWaiting}
	variant := func(limit float64) limitVariant {
		return limitVariant{limits: map[string]float64{signal: limit}, idle: signals.DefaultIdleAfter, ceiling: c}
	}
	off := p.set(LimitStepA, "", limitVariant{idle: signals.DefaultIdleAfter, ceiling: c}, limitRule, scopeMain)
	offRow := p.row(signal, LimitCandidate{}, true, off)
	offRow.Holds = "the reference: no signal binds"
	type trial struct {
		cand  LimitCandidate
		a     *limitRunSet
		row   LimitRow
		judge limitJudgement
	}
	trials := make([]trial, len(swept))
	complete := true
	for i, cand := range swept {
		a := p.set(LimitStepA, signal, variant(cand.Limit), limitRule, scopeMain)
		trials[i] = trial{cand: cand, a: a, row: p.row(signal, cand, false, a)}
		complete = complete && a.complete
	}
	assemble := func() {
		out.Rows = []LimitRow{offRow}
		for _, t := range trials {
			out.Rows = append(out.Rows, t.row)
		}
	}
	if !complete {
		out.Waits = LimitStepA
		assemble()
		return out
	}
	var holders []int
	for i := range trials {
		trials[i].judge = p.judge(trials[i].a, scopeMain)
		trials[i].row.Holds, trials[i].row.WorseOn = holdsText(trials[i].judge, "the main branch of both windows"), trials[i].judge.worseOn
		trials[i].row.DoneVerdict = trials[i].judge.doneVerdict
		if len(trials[i].judge.worseOn) == 0 {
			holders = append(holders, i)
		}
	}
	if len(holders) == 0 {
		out.Status = LimitNone
		assemble()
		return out
	}
	best := holders[0]
	for _, i := range holders {
		if trials[i].row.Done >= trials[best].row.Done {
			best = i
		}
	}
	var chain []int
	for k := len(holders) - 1; k >= 0; k-- {
		if holders[k] <= best {
			chain = append(chain, holders[k])
		}
	}
	// The limit with the most issues finished and the next tighter one run first; a limit that is
	// worse on a branch has the next tighter limit run after it, until one holds or the list ends.
	p.set(LimitStepB, signal, variant(trials[chain[0]].cand.Limit), limitRule, scopePassing)
	if len(chain) > 1 {
		p.set(LimitStepB, signal, variant(trials[chain[1]].cand.Limit), limitRule, scopePassing)
	}
	for pos, i := range chain {
		b := p.set(LimitStepB, signal, variant(trials[i].cand.Limit), limitRule, scopePassing)
		if !b.complete {
			out.Waits = LimitStepB
			assemble()
			return out
		}
		j := p.judge(b, scopePassing)
		trials[i].row.Holds, trials[i].row.WorseOn = holdsText(j, fmt.Sprintf("each of the %d branches of both windows that pass R5", j.branches)), j.worseOn
		trials[i].row.Peak = max(trials[i].row.Peak, b.peak)
		if len(j.worseOn) > 0 {
			if pos+1 < len(chain) {
				p.set(LimitStepB, signal, variant(trials[chain[pos+1]].cand.Limit), limitRule, scopePassing)
			}
			continue
		}
		out.Status, out.Named, out.HasNamed, out.Percentile = LimitNamed, trials[i].cand.Limit, true, trials[i].cand.Percentile
		out.Peak = trials[i].row.Peak
		out.Extrapolates = float64(out.Peak) > p.high
		if signal == signals.Provider || signal == signals.Refusals {
			trials[i].row.Unconfirmed = p.unconfirmed(trials[i].a)
		}
		if signal == signals.Idle {
			for _, d := range limitIdleAfters {
				v := limitVariant{limits: map[string]float64{signal: out.Named}, idle: d, ceiling: c}
				s := p.set(LimitStepB, signal, v, limitRule, scopePassing)
				read := p.row(signal, trials[i].cand, false, s)
				read.IdleAfter = Duration(d)
				if s.complete {
					jd := p.judge(s, scopePassing)
					read.Holds, read.WorseOn, read.DoneVerdict = holdsText(jd, fmt.Sprintf("each of the %d branches of both windows that pass R5", jd.branches)), jd.worseOn, jd.doneVerdict
				}
				out.IdleReads = append(out.IdleReads, read)
			}
		}
		assemble()
		return out
	}
	out.Status = LimitNone
	assemble()
	return out
}

// combined is step C at each ceiling whose signals are all settled: the two rules that admit by
// signals, with every named limit at once, on every branch of both windows, judged on those that
// pass R5. A ceiling none of whose signals has a named limit has no step C.
func (p *limitPlanner) combined(signalsOut []SignalLimits) []CombinedRow {
	var rows []CombinedRow
	for _, c := range p.in.Ceilings {
		named := map[string]float64{}
		settled, gated := true, false
		for _, s := range signalsOut {
			for _, at := range s.Ceilings {
				if at.Ceiling != c {
					continue
				}
				gated = true
				switch {
				case at.Status == LimitWaiting:
					settled = false
				case at.HasNamed:
					named[s.Signal] = at.Named
				}
			}
		}
		if !gated || !settled || len(named) == 0 {
			continue
		}
		for _, rule := range limitCombinedRules {
			v := limitVariant{limits: named, idle: signals.DefaultIdleAfter, ceiling: c}
			s := p.set(LimitStepC, "", v, rule, scopeEvery)
			row := CombinedRow{Ceiling: c, Rule: rule, Limits: maps.Clone(named), Complete: s.complete}
			if s.complete {
				j := p.judge(s, scopeEvery)
				row.Branches, row.WorseOn = j.branches, j.worseOn
				row.Better, row.Worse, row.Trade, row.Level = j.standings[StandingBetter], j.standings[StandingWorse], j.standings[StandingTrade], j.standings[StandingLevel]
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// PlanLimits is the limit test over in, as far as the runs in in.RunsDir go: each signal's gate
// from the models, its list of limits, and at each ceiling its limit named from the runs read back,
// and every entry that the steps so far plan, with Ran says which are in. Step B plans once step
// A's runs for the signal are in, and step C once every signal's limit is settled, so a caller
// plans, runs the entries still to run (LimitsResult.Pending), and plans again until none is left.
func PlanLimits(in LimitsInput) (LimitsResult, error) {
	if len(in.Ceilings) == 0 {
		return LimitsResult{}, fmt.Errorf("replay: the limit test needs a ceiling to run at")
	}
	p, err := newLimitPlanner(in)
	if err != nil {
		return LimitsResult{}, err
	}
	result := LimitsResult{TokenLimit: in.TokenLimit, Range: FlightRange{High: p.high}, Ceilings: slices.Clone(in.Ceilings)}
	first := true
	for _, m := range in.Models {
		for _, model := range m.Models {
			for _, day := range model.Days {
				if day.InFlight > 0 && (first || day.InFlight < result.Range.Low) {
					result.Range.Low, first = day.InFlight, false
				}
			}
		}
	}
	for _, name := range signals.Names() {
		s := SignalLimits{Signal: name, Candidates: in.Candidates[name], Ceilings: []CeilingLimits{}}
		s.Gate = limitGate(name, in.Models, in.TokenLimit)
		var swept []LimitCandidate
		for _, c := range s.Candidates {
			if c.Left == "" {
				swept = append(swept, c)
			}
		}
		slices.SortFunc(swept, func(a, b LimitCandidate) int { return cmp.Compare(a.Limit, b.Limit) })
		switch {
		case len(s.Candidates) == 0:
			s.Gate = LimitGate{Signal: name, Why: "no list of limits is given for it"}
		case s.Gate.Gives && len(swept) == 0:
			s.Gate = LimitGate{Signal: name, Why: "every percentile of the signal is left out of its list"}
		}
		if s.Gate.Gives {
			for _, c := range in.Ceilings {
				s.Ceilings = append(s.Ceilings, p.ceilingLimits(name, swept, c))
			}
			if name == signals.Provider || name == signals.Refusals {
				s.Gate.Rests = append(s.Gate.Rests, p.providerRests(s.Ceilings)...)
			}
		}
		result.Signals = append(result.Signals, s)
	}
	result.Combined = p.combined(result.Signals)
	if p.err != nil {
		return LimitsResult{}, p.err
	}
	for _, s := range result.Signals {
		for _, c := range s.Ceilings {
			switch {
			case c.Waits == LimitStepA:
				result.Waits = LimitStepA
			case c.Waits == LimitStepB && result.Waits == "":
				result.Waits = LimitStepB
			}
		}
	}
	slices.SortStableFunc(p.entries, func(a, b LimitEntry) int { return cmp.Compare(a.Step, b.Step) })
	for i := range p.entries {
		p.entries[i].Index = i
	}
	result.Entries = p.entries
	return result, nil
}

// providerRests is what the provider's limits rest on beyond the model's tests: the span of each
// window's provider record, and the refused minutes the model gives under the token limit that the
// record cannot confirm, at each ceiling's named limit.
func (p *limitPlanner) providerRests(ceilings []CeilingLimits) []string {
	var rests []string
	for i, spans := range p.in.ProviderSpans {
		for _, span := range spans {
			rests = append(rests, fmt.Sprintf("the provider record of %s spans %s to %s", p.in.Windows[i].Name, span.From.UTC().Format(time.RFC3339),
				span.To.UTC().Format(time.RFC3339)))
		}
	}
	for _, c := range ceilings {
		for _, row := range c.Rows {
			if len(row.Unconfirmed) == 0 {
				continue
			}
			var days []string
			for _, key := range slices.Sorted(maps.Keys(row.Unconfirmed)) {
				days = append(days, fmt.Sprintf("%s: %s", key, formatNumber(row.Unconfirmed[key])))
			}
			rests = append(rests, fmt.Sprintf("under the token limit the model refuses these minutes at the limit %s, %s, that the record cannot confirm: %s",
				formatNumber(row.Limit), ceilingText(c.Ceiling), strings.Join(days, ", ")))
		}
	}
	return rests
}

// ceilingText is a ceiling as the table names it.
func ceilingText(c int) string {
	if c == 0 {
		return "at the recorded cap"
	}
	return fmt.Sprintf("at the cap %d", c)
}

// ordinal is p as the table names a percentile.
func ordinal(p int) string { return strconv.Itoa(p) + "th" }

// Write prints r as Markdown: the token limit, the range of work in flight, and for each signal
// what its limit rests on, its list of limits and, at each ceiling, the table of its limits with
// the limit named and whether it is an extrapolation; then step C.
func (r LimitsResult) Write(w io.Writer) error {
	p := &printer{w: w}
	p.line("# The limits of R11's signals")
	p.line("")
	if r.TokenLimit > 0 {
		p.line("Every run takes the token limit %d tokens a minute, in each minute whose provider record holds no limit of the project's own (assumption 49).", r.TokenLimit)
	}
	p.line("The windows record %s to %s issues in flight, the lowest day's average to the most at any instant. A limit holds only inside that range; a limit whose runs hold more issues in flight than the %s that the windows record is an extrapolation.",
		formatNumber(r.Range.Low), formatNumber(r.Range.High), formatNumber(r.Range.High))
	switch r.Waits {
	case LimitStepA:
		p.line("Nothing past step A is planned yet: step B waits for the runs of step A, and step C for those of step B.")
	case LimitStepB:
		p.line("Step C waits for the runs of step B.")
	}
	for _, s := range r.Signals {
		p.line("")
		p.line("## Signal `%s`", s.Signal)
		p.line("")
		if !s.Gate.Gives {
			p.line("No step is planned for `%s`: %s.", s.Signal, s.Gate.Why)
			continue
		}
		p.line("What the limit rests on:")
		p.line("")
		for _, rest := range s.Gate.Rests {
			p.line("- %s", rest)
		}
		p.line("")
		var kept, left []string
		for _, c := range s.Candidates {
			if c.Left == "" {
				kept = append(kept, fmt.Sprintf("the %s percentile, %s", ordinal(c.Percentile), formatNumber(c.Limit)))
			} else {
				left = append(left, fmt.Sprintf("the %s percentile, %s (%s)", ordinal(c.Percentile), formatNumber(c.Limit), c.Left))
			}
		}
		p.line("The limits listed, with off: %s.", strings.Join(kept, "; "))
		if len(left) > 0 {
			p.line("Left out: %s.", strings.Join(left, "; "))
		}
		for _, c := range s.Ceilings {
			p.line("")
			p.line("### `%s` %s", s.Signal, ceilingText(c.Ceiling))
			p.line("")
			p.line("| limit | percentile | done | waitOnlyForSlot.hours | priorityTakenBanded | otherDelays.hours | signal mean | signal 95th percentile | signal largest | held admission | most in flight | holds |")
			p.line("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
			for _, row := range c.Rows {
				writeLimitRow(p, row)
			}
			for _, row := range c.IdleReads {
				p.line("")
				p.line("The idle share's named limit %s at d of %s: %s issues finished on window one's main branch; %s.", formatNumber(row.Limit),
					time.Duration(row.IdleAfter), formatNumber(row.Done), row.Holds)
			}
			p.line("")
			writeLimitVerdict(p, s.Signal, c, r.Range)
		}
	}
	if len(r.Combined) > 0 {
		p.line("")
		p.line("## Step C: every named limit at once")
		p.line("")
		p.line("| ceiling | rule | limits | branches judged | better | worse | trade | level |")
		p.line("| --- | --- | --- | --- | --- | --- | --- | --- |")
		for _, row := range r.Combined {
			if !row.Complete {
				p.line("| %s | `%s` | %s | not run | | | | |", ceilingText(row.Ceiling), row.Rule, limitsText(row.Limits))
				continue
			}
			p.line("| %s | `%s` | %s | %d | %d | %d | %d | %d |", ceilingText(row.Ceiling), row.Rule, limitsText(row.Limits), row.Branches, row.Better, row.Worse, row.Trade, row.Level)
		}
	}
	return p.err
}

// limitsText is limits as a cell gives them.
func limitsText(limits map[string]float64) string {
	parts := make([]string, 0, len(limits))
	for _, name := range slices.Sorted(maps.Keys(limits)) {
		parts = append(parts, "`"+name+"` "+formatNumber(limits[name]))
	}
	return strings.Join(parts, ", ")
}

// writeLimitRow writes one row of a signal's table.
func writeLimitRow(p *printer, row LimitRow) {
	limit, percentile := formatNumber(row.Limit), ordinal(row.Percentile)
	if row.Off {
		limit, percentile = "off", ""
	}
	if !row.Ran {
		p.line("| %s | %s | not run | | | | | | | | | %s |", limit, percentile, row.Holds)
		return
	}
	signal := []string{"", "", "", ""}
	if !row.Off {
		signal = []string{formatNumber(row.Signal.Mean), formatNumber(row.Signal.P95), formatNumber(row.Signal.Max), formatNumber(100*row.Signal.HeldShare) + "%"}
	}
	p.line("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %d | %s |", limit, percentile, formatNumber(row.Done), formatNumber(row.WaitOnlyForSlot),
		formatNumber(row.PriorityTaken), formatNumber(row.OtherDelays), signal[0], signal[1], signal[2], signal[3], row.Peak, row.Holds)
}

// writeLimitVerdict writes the sentence that says what c names: its limit and what it rests on, or
// that none holds, with the extrapolation where the named limit is one.
func writeLimitVerdict(p *printer, signal string, c CeilingLimits, flight FlightRange) {
	var off *LimitRow
	for i := range c.Rows {
		if c.Rows[i].Off {
			off = &c.Rows[i]
		}
	}
	switch c.Status {
	case LimitWaiting:
		p.line("`%s` %s: the plan waits for the runs of step %s.", signal, ceilingText(c.Ceiling), c.Waits)
	case LimitNone:
		p.line("`%s` %s: no limit holds: each limit of the list is worse than today's rule on a branch that passes R5.", signal, ceilingText(c.Ceiling))
	case LimitNamed:
		var named LimitRow
		for _, row := range c.Rows {
			if !row.Off && row.Limit == c.Named {
				named = row
			}
		}
		text := fmt.Sprintf("`%s` %s: the named limit is %s (the %s percentile): %s issues finished on window one's main branch", signal, ceilingText(c.Ceiling),
			formatNumber(c.Named), ordinal(c.Percentile), formatNumber(named.Done))
		if off != nil && off.Ran {
			switch more := off.Done - named.Done; {
			case more > 0:
				text += fmt.Sprintf("; off finished %s more (%s)", formatNumber(more), formatNumber(off.Done))
			case more < 0:
				text += fmt.Sprintf("; off finished %s fewer (%s)", formatNumber(-more), formatNumber(off.Done))
			default:
				text += "; off finished as many"
			}
		}
		p.line("%s; no other measure is worse than today's rule on any branch of both windows that passes R5.", text)
		if c.Extrapolates {
			p.line("This limit is an extrapolation: its runs hold up to %d issues in flight, past the %s that the windows record.", c.Peak, formatNumber(flight.High))
		}
	}
}

// LimitsConfig names what ReadLimitsInput reads: window one's directory, whose record the lists of
// limits come from, and window two's; the two files `legion replay models --against --json` wrote,
// window one against the other record and the other record against window one; the directories of
// runs of validate and compare that hold today's rule's runs; the directory of the sweep's runs;
// the token limit every run takes; and the second ceiling beside the recorded cap.
type LimitsConfig struct {
	WindowOne, WindowTwo  string
	Models, ModelsReverse string
	Baseline              []string
	Runs                  string
	TokenLimit            int64
	Ceiling               int
}

// ReadLimitsInput reads cfg's files into the limit test's input: each window's name (its directory's)
// and the span of its provider record, window one's lists of limits (LimitCandidates), the models'
// outputs and the baseline's runs. It refuses two windows that go by one name.
func ReadLimitsInput(cfg LimitsConfig) (LimitsInput, error) {
	in := LimitsInput{TokenLimit: cfg.TokenLimit, RunsDir: cfg.Runs, Ceilings: []int{0, cfg.Ceiling}}
	for i, dir := range []string{cfg.WindowOne, cfg.WindowTwo} {
		name, err := windowName(dir)
		if err != nil {
			return LimitsInput{}, err
		}
		in.Windows[i] = LimitWindow{Name: name, Dir: dir}
		if in.ProviderSpans[i], err = providerSpans(dir); err != nil {
			return LimitsInput{}, err
		}
	}
	if err := distinctNames(in.Windows[0].Name, in.Windows[1].Name); err != nil {
		return LimitsInput{}, err
	}
	one, err := ReadWindow(cfg.WindowOne)
	if err != nil {
		return LimitsInput{}, err
	}
	if in.Candidates, err = LimitCandidates(one, cfg.TokenLimit); err != nil {
		return LimitsInput{}, err
	}
	for i, path := range []string{cfg.Models, cfg.ModelsReverse} {
		data, err := os.ReadFile(path)
		if err != nil {
			return LimitsInput{}, fmt.Errorf("replay: read the models: %w", err)
		}
		if err := json.Unmarshal(data, &in.Models[i]); err != nil {
			return LimitsInput{}, fmt.Errorf("replay: read %s: %w", path, err)
		}
	}
	if in.Baseline, err = ReadRuns(cfg.Baseline...); err != nil {
		return LimitsInput{}, err
	}
	return in, nil
}

// providerSpans is the span of the provider's record that the window in dir gives in its window.json,
// read from that file alone.
func providerSpans(dir string) ([]Span, error) {
	data, err := os.ReadFile(filepath.Join(dir, windowFile))
	if err != nil {
		return nil, fmt.Errorf("replay: read the window: %w", err)
	}
	var window struct {
		Signals map[string][]Span `json:"signals"`
	}
	if err := json.Unmarshal(data, &window); err != nil {
		return nil, fmt.Errorf("replay: read %s: %w", filepath.Join(dir, windowFile), err)
	}
	return window.Signals[SignalProvider], nil
}
