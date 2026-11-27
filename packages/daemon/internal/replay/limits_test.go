package replay

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// limitAnswer is what the run of one entry of the sweep measured, in the runs these tests make up:
// the issues it finished, the hours of started work that waited only for a slot, and the most
// issues it held in flight.
type limitAnswer struct {
	done     int
	waitOnly float64
	peak     int
}

// passingModels is the two outputs of `legion replay models --against` over a window and the other
// record, each with the three models passing R11's daily test and its test across the two records:
// the days hold 5 and 7 issues in flight, and the tokens model has the one tested day of the
// project's limit, 26 refused minutes recorded against 32 modelled.
func passingModels() [2]LoadModels {
	num := func(v float64) *float64 { return &v }
	build := func(project string, from time.Time) LoadModels {
		model := func(name, signal string, day ModelDay) LoadModel {
			return LoadModel{Name: name, Signal: signal, Fitted: true, Verdict: VerdictPasses, Across: VerdictPasses, GivesLimit: true, Days: []ModelDay{day}}
		}
		models := LoadModels{Project: project, From: from, To: from.Add(48 * time.Hour), Models: []LoadModel{
			model(ModelTokens, SignalProvider, ModelDay{Day: "2026-11-23", InFlight: 7, RecordHours: 1.8, Recorded: num(26), Modelled: num(32), Verdict: VerdictPasses}),
			model(ModelMemory, SignalKills, ModelDay{Day: "2026-11-22", InFlight: 5, RecordHours: 24, Recorded: num(99), Modelled: num(99), Verdict: VerdictPasses}),
			model(ModelQueue, SignalQueue, ModelDay{Day: "2026-11-22", InFlight: 6, RecordHours: 24, Recorded: num(0.86), Modelled: num(0.86), Verdict: VerdictPasses}),
		}}
		ratio := 1.0218
		models.Across = &AcrossRecords{Project: project, From: from, To: from.Add(48 * time.Hour), Rates: []AcrossRate{
			{Model: ModelTokens, Ratio: &ratio, Verdict: VerdictPasses}, {Model: ModelMemory, Ratio: &ratio, Verdict: VerdictPasses},
			{Model: ModelQueue, Ratio: &ratio, Verdict: VerdictPasses}}}
		return models
	}
	return [2]LoadModels{build("ACME", clock(0, 0)), build("ACME", clock(0, 0).Add(72*time.Hour))}
}

// candidateList is the limits of a signal as the sweep lists them, the first of the percentiles
// 50, 75, 90, 95 and 99 for each limit given.
func candidateList(limits ...float64) []LimitCandidate {
	list := make([]LimitCandidate, 0, len(limits))
	for i, limit := range limits {
		list = append(list, LimitCandidate{Percentile: limitPercentiles[i], Limit: limit})
	}
	return list
}

// limitsFixture is the limit test over made-up runs: window-2, window one of the sweep, which
// compare reads after the change, under legion-v20.42.5 with 3 of its 10 stretches in a conflict,
// and window-1, window two, under legion-v19.57.3 with none. Today's rule, main, and each window's
// own ran the main branch and the branch that asks git at f 1: three seeds on window-2's git
// branch, whose conflicts are drawn, and one on every other run. Each made-up run of main
// finishes 20 issues (sampleCard) and waits 10 hours only for a slot. The sweep's runs go to a
// directory of their own.
func limitsFixture(t *testing.T, candidates map[string][]LimitCandidate, ceilings []int) (LimitsInput, BranchAt) {
	t.Helper()
	base := t.TempDir()
	windows := madeUpWindows("legion-v19.57.3", "legion-v20.42.5")
	windows[1].Stretches, windows[1].Conflicted = 10, 3
	git, err := BranchNamed("main+conflicts=git")
	if err != nil {
		t.Fatal(err)
	}
	gitAt := BranchAt{Branch: git, Slowdown: 1}
	rules := []record.SlotRule{replayedRule(t, "main"), replayedRule(t, "legion-v20.42.5"), replayedRule(t, "legion-v19.57.3")}
	writeMadeUpRuns(t, base, windows, rules, runPlan{branches: []BranchAt{MainBranchAt, gitAt}, perDraw: 3}, madeUpRun)
	runs, err := ReadRuns(base)
	if err != nil {
		t.Fatal(err)
	}
	return LimitsInput{
		Windows:    [2]LimitWindow{{Name: "window-2", Dir: "windows/window-2"}, {Name: "window-1", Dir: "windows/window-1"}},
		Candidates: candidates, Models: passingModels(), TokenLimit: 6_000_000, Ceilings: ceilings, Baseline: runs, RunsDir: t.TempDir(),
		Compares: func(string, BranchAt) bool { return true },
	}, gitAt
}

// writeLimitRun writes the run entry e plans into the sweep's directory of runs, as a replay of it
// would leave it, with a's measures.
func writeLimitRun(t *testing.T, in LimitsInput, e LimitEntry, a limitAnswer) {
	t.Helper()
	branch, err := BranchNamed(e.Branch)
	if err != nil {
		t.Fatal(err)
	}
	card := sampleCard(compareHours)
	card.Rule, card.Branch, card.Seed, card.Slowdown = e.Rule, e.Branch, e.Seed, e.Slowdown
	card.Done, card.WaitOnlyForSlot.Hours, card.InFlight.Max = a.done, a.waitOnly, a.peak
	summaries := map[string]SignalSummary{}
	for name, limit := range e.Limits {
		summaries[name] = SignalSummary{Mean: limit / 2, P95: limit, Max: limit, Recorded: 60, Minutes: 60, HeldShare: 0.25}
	}
	ran := runRecord{Rule: e.Rule, RuleCommit: replayedRule(t, e.Rule).Commit, Branch: branch, Seed: e.Seed, Slowdown: e.Slowdown, Cap: e.Cap,
		TokenLimit: e.TokenLimit, IdleAfter: e.IdleAfter, Limits: e.Limits, Scenario: branch.NeverStarted, Assumptions: []int{1},
		SignalSummaries: summaries, SuppliedRefused: map[string]int{}, MissingAncestors: []string{}, StartedAt: compareFrom, Gaps: Gaps(card, card)}
	dir := filepath.Join(in.RunsDir, e.Out)
	for name, value := range map[string]any{runFile: ran, scorecardFile: card, lineScorecardFile: card} {
		if err := writeJSON(filepath.Join(dir, name), value); err != nil {
			t.Fatal(err)
		}
	}
}

// settleLimits plans the limit test, writes the run of each entry still to run with answer's
// measures, and plans again, until no entry is left to run: the sweep as a Job would run it. It
// answers the last plan and each entry that was run.
func settleLimits(t *testing.T, in LimitsInput, answer func(LimitEntry) limitAnswer) (LimitsResult, []LimitEntry) {
	t.Helper()
	var ran []LimitEntry
	for range 40 {
		result, err := PlanLimits(in)
		if err != nil {
			t.Fatal(err)
		}
		pending := result.Pending()
		if len(pending) == 0 {
			return result, ran
		}
		for _, e := range pending {
			writeLimitRun(t, in, e, answer(e))
			ran = append(ran, e)
		}
	}
	t.Fatal("the plan never settled: it plans entries after 40 rounds of runs")
	return LimitsResult{}, nil
}

// The limit test names, for one signal at one ceiling, R11's limit: the limit with the most issues
// finished at which no other measure of the owner's three costs or of the wait of urgent work is
// worse than under today's rule, in R7's sense, on every branch that passes R5; a tie in the issues
// finished goes to the loosest limit. It is built over made-up runs of the signal kills at the
// limits 2, 4, 6 and 8 (the 50th, 75th, 90th and 95th percentiles) at the recorded cap, on two
// branches, main and git's, of two windows. Step A runs each limit on the main branch of both
// windows, and step B the loosest limit with the most issues finished and the next tighter one on
// every branch that passes R5, and, where the named limit is worse on one, the next tighter limit
// again. Today's rule finishes 20 issues and waits 10 hours only for a slot; no limit is better or
// worse on the issues finished (the error held to is 0, but done is never a reason to reject). The
// limits finish 21, 23, 28 and 28, and no limit at all, off, 30; a limit that waits 12 hours only for
// a slot on a branch is worse than main there.
//
//   - Every limit holds. The most issues, 28, are finished at 6 and at 8, the tie goes to the loosest,
//     8: it is named, off finishing 2 more, and the runs of step B, 8's and 6's, are on the git
//     branch, 2's and 4's are not.
//   - 8 waits 12 hours on window-2's git branch: it is worse there, and 6 is named, which step B ran
//     already as the next tighter limit.
//   - 8 and 6 are both worse there: the next tighter limit, 4, is run on every branch, holds and is
//     named, with 23 issues.
//   - Every limit is worse there: the list ends and the result names no limit.
//   - 8 is worse on window-2's git branch, which fails R5: that branch is not read, 8 is named, and
//     no run of step B is on that branch.
//   - Runs of 8 hold 12 issues in flight at most where the windows record 8: 8 is named, and an
//     extrapolation; 6, within the 8, is not.
func TestTheLimitTestNamesR11sLimit(t *testing.T) {
	t.Parallel()
	doneAt := map[float64]int{2: 21, 4: 23, 6: 28, 8: 28}
	kills := map[string][]LimitCandidate{SignalKills: candidateList(2, 4, 6, 8)}
	type branch struct{ window, branch string }
	cases := []struct {
		name      string
		worse     func(e LimitEntry, limit float64) bool
		peak      func(limit float64) int
		failsR5   bool
		named     float64
		hasNamed  bool
		status    string
		extrapol  bool
		gitRan    []float64
		noRunsOn  *branch
		wantLines []string
	}{
		{name: "every limit holds", named: 8, hasNamed: true, status: "named", gitRan: []float64{6, 8},
			wantLines: []string{"the named limit is 8 (the 95th percentile)", "28 issues finished on window one's main branch", "off finished 2 more"}},
		{name: "8 is worse on a branch", worse: func(e LimitEntry, limit float64) bool {
			return limit == 8 && e.Window == "window-2" && strings.HasPrefix(e.Branch, "main+conflicts=git")
		}, named: 6, hasNamed: true, status: "named", gitRan: []float64{6, 8},
			wantLines: []string{"the named limit is 6 (the 90th percentile)"}},
		{name: "8 and 6 are worse", worse: func(e LimitEntry, limit float64) bool {
			return limit >= 6 && e.Window == "window-2" && strings.HasPrefix(e.Branch, "main+conflicts=git")
		}, named: 4, hasNamed: true, status: "named", gitRan: []float64{4, 6, 8},
			wantLines: []string{"the named limit is 4 (the 75th percentile)", "23 issues finished on window one's main branch"}},
		{name: "every limit is worse", worse: func(e LimitEntry, limit float64) bool {
			return e.Window == "window-2" && strings.HasPrefix(e.Branch, "main+conflicts=git")
		}, status: "no limit", gitRan: []float64{2, 4, 6, 8},
			wantLines: []string{"no limit holds: each limit of the list is worse than today's rule on a branch that passes R5"}},
		{name: "a branch that fails R5", worse: func(e LimitEntry, limit float64) bool {
			return limit == 8 && e.Window == "window-2" && strings.HasPrefix(e.Branch, "main+conflicts=git")
		}, failsR5: true, named: 8, hasNamed: true, status: "named", gitRan: []float64{6, 8},
			noRunsOn: &branch{"window-2", "main+conflicts=git"}},
		{name: "a limit above the range", peak: func(limit float64) int {
			if limit == 8 {
				return 12
			}
			return 8
		}, named: 8, hasNamed: true, status: "named", extrapol: true, gitRan: []float64{6, 8},
			wantLines: []string{"extrapolation", "12 issues in flight", "the 8 that the windows record"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in, git := limitsFixture(t, kills, []int{0})
			if c.failsR5 {
				in.Compares = func(window string, at BranchAt) bool { return !(window == "window-2" && at.Label() == git.Label()) }
			}
			result, ran := settleLimits(t, in, func(e LimitEntry) limitAnswer {
				limit, limited := e.Limits[SignalKills]
				a := limitAnswer{done: 30, waitOnly: 10, peak: 8}
				if limited {
					a.done = doneAt[limit]
					if c.worse != nil && c.worse(e, limit) {
						a.waitOnly = 12
					}
					if c.peak != nil {
						a.peak = c.peak(limit)
					}
				}
				return a
			})
			var signal *SignalLimits
			for i := range result.Signals {
				if result.Signals[i].Signal == SignalKills {
					signal = &result.Signals[i]
				}
			}
			if signal == nil || len(signal.Ceilings) != 1 {
				t.Fatalf("the result holds %+v for the kills, want one ceiling", signal)
			}
			got := signal.Ceilings[0]
			if got.HasNamed != c.hasNamed || got.Named != c.named || got.Status != c.status || got.Extrapolates != c.extrapol {
				t.Errorf("the kills' limit at the recorded cap is %v (named %t), %q, extrapolation %t; want %v (named %t), %q, extrapolation %t",
					got.Named, got.HasNamed, got.Status, got.Extrapolates, c.named, c.hasNamed, c.status, c.extrapol)
			}
			if result.Range.High != 8 {
				t.Errorf("the range of work in flight ends at %v, want 8, the most the windows' scorecards record", result.Range.High)
			}
			onGit := map[float64]bool{}
			for _, e := range ran {
				if e.Step == LimitStepB && e.Branch == git.Branch.Name {
					onGit[e.Limits[SignalKills]] = true
				}
				if c.noRunsOn != nil && e.Window == c.noRunsOn.window && e.Branch == c.noRunsOn.branch && e.Step == LimitStepB {
					t.Errorf("step %s ran %s on %s, which fails R5", e.Step, e.Window, e.Branch)
				}
			}
			for _, limit := range []float64{2, 4, 6, 8} {
				if want := slices.Contains(c.gitRan, limit); onGit[limit] != want {
					t.Errorf("the limit %v ran on the branch that asks git: %t, want %t", limit, onGit[limit], want)
				}
			}
			var printed strings.Builder
			if err := result.Write(&printed); err != nil {
				t.Fatal(err)
			}
			for _, line := range c.wantLines {
				if !strings.Contains(printed.String(), line) {
					t.Errorf("the table holds no %q:\n%s", line, printed.String())
				}
			}
		})
	}
}

// A percentile that falls outside the shared range of its signal is left out of the list with the
// reason, never clamped into it (signals.CheckLimit), and one that repeats the limit of a lower
// percentile is left out as its repeat. Over 20 minutes of a signal's values, a percentile is the
// value at place ⌈p·20⌉ of the sorted values, so the 50th, 75th, 90th, 95th and 99th are the 10th,
// 15th, 18th, 19th and 20th.
//
//   - The provider's headroom, 1 to 20: 10, 15, 18, 19 and 20. Only 10 is at most 10; the others are
//     left out with CheckLimit's reason.
//   - The kills, 12 minutes of none and then 1 to 8: the 10th place is 0, which is no whole number
//     from 1; the others are 3, 6, 7 and 8.
//   - The approvals, always 5: 5 for the 50th percentile, and the other four repeat it.
func TestAPercentileOutsideTheSharedRangeIsLeftOut(t *testing.T) {
	t.Parallel()
	series := func(values ...float64) []float64 { return values }
	upTo20 := make([]float64, 20)
	for i := range upTo20 {
		upTo20[i] = float64(i + 1)
	}
	killValues := slices.Concat(make([]float64, 12), series(1, 2, 3, 4, 5, 6, 7, 8))
	fives := slices.Repeat([]float64{5}, 20)
	for _, c := range []struct {
		signal string
		values []float64
		want   []LimitCandidate
	}{
		{SignalProvider, upTo20, []LimitCandidate{
			{Percentile: 50, Limit: 10},
			{Percentile: 75, Limit: 15, Left: "signals: the limit of provider is 15, which is not above 0 and at most 10"},
			{Percentile: 90, Limit: 18, Left: "signals: the limit of provider is 18, which is not above 0 and at most 10"},
			{Percentile: 95, Limit: 19, Left: "signals: the limit of provider is 19, which is not above 0 and at most 10"},
			{Percentile: 99, Limit: 20, Left: "signals: the limit of provider is 20, which is not above 0 and at most 10"}}},
		{SignalKills, killValues, []LimitCandidate{
			{Percentile: 50, Limit: 0, Left: "signals: the limit of kills is 0, which is no whole number from 1 to 1000000"},
			{Percentile: 75, Limit: 3}, {Percentile: 90, Limit: 6}, {Percentile: 95, Limit: 7}, {Percentile: 99, Limit: 8}}},
		{SignalApprovals, fives, []LimitCandidate{
			{Percentile: 50, Limit: 5},
			{Percentile: 75, Limit: 5, Left: "the same limit as the 50th percentile's"},
			{Percentile: 90, Limit: 5, Left: "the same limit as the 50th percentile's"},
			{Percentile: 95, Limit: 5, Left: "the same limit as the 50th percentile's"},
			{Percentile: 99, Limit: 5, Left: "the same limit as the 50th percentile's"}}},
	} {
		if got := candidatesOf(c.signal, c.values); !slices.Equal(got, c.want) {
			t.Errorf("the list of limits of %s is %+v, want %+v", c.signal, got, c.want)
		}
	}
}

// A signal gets steps only where its model passed R11's tests, and the plan lists every step for it:
// step A runs each limit alone (the others off) on the main branch of both windows at both
// ceilings, and off once for each window and ceiling; step B runs, for each signal and ceiling, the
// named limit and the next tighter one on every branch of both windows that passes R5, and the
// idle share's named limit also at d of 10 minutes and of 4 hours; step C runs signals and
// signals-backoff with every named limit on every branch of both windows at both ceilings.
// Counted by hand, over the five signals of 2 limits each, at the ceilings 0 (the recorded cap) and
// 171, on window-2 (the main branch one seed, git's three) and window-1 (one seed on each of two).
// The input also gives a list for the provider's refusals, as a list of limits read before they
// left the signals would (spec R11, v52): they are no signal, so the plan reads no list of them,
// lists no entry for them and gives them no part of its result.
//
//   - The kills' model, memory, fails the daily test on window one's record, so the kills get no
//     step, and the plan says so. The other four get steps: idle and the approvals come from the
//     replay and have no model, the merge queue's model passes, and the provider's headroom rests
//     on the tokens model, which gives no daily test on window one's record and passes on the
//     other, and passes the test across the two records, with a token limit given.
//   - Step A: 4 signals × 2 ceilings × 2 limits × 2 windows' main branch = 32, and off 2 ceilings × 2
//     windows = 4: 36.
//   - Step B: each of the 4 signals × 2 ceilings runs its 2 limits on 6 runs (window-2's 1 + 3,
//     window-1's 1 + 1) = 12, 3 × 2 × 12 = 72 with the idle share's 12 at 2 ceilings = 24 more at d
//     of 10 minutes and 4 hours (6 runs each, 2 d, 2 ceilings) and its own 24: 120.
//   - Step C: 2 rules × 2 ceilings × 6 runs = 24, each with the four named limits.
//
// Before any run the plan lists step A alone and says that B and C wait for the runs before them.
func TestTheLimitPlanListsEveryStep(t *testing.T) {
	t.Parallel()
	candidates := map[string][]LimitCandidate{SignalIdle: candidateList(0.3, 0.6), SignalQueue: candidateList(1, 2), SignalProvider: candidateList(0.5, 0.9),
		"refusals": candidateList(3, 6), SignalKills: candidateList(2, 4), SignalApprovals: candidateList(1, 2)}
	in, _ := limitsFixture(t, candidates, []int{0, 171})
	failed := LoadModel{Name: ModelMemory, Signal: SignalKills, Fitted: true, Verdict: VerdictFails, Across: VerdictPasses}
	in.Models[0].Models[1] = failed
	in.Models[0].Models[0].Verdict, in.Models[0].Models[0].Reason = VerdictNoTest, reasonNoLimitSpan
	in.Models[0].Models[0].GivesLimit = false

	first, err := PlanLimits(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := countSteps(first.Entries); got != "A=36" {
		t.Errorf("the plan before any run lists %s, want A=36 alone", got)
	}
	var printed strings.Builder
	if err := first.Write(&printed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed.String(), "step B waits for the runs of step A") {
		t.Errorf("the plan before any run does not say that step B waits for step A:\n%s", printed.String())
	}

	loosest := func(e LimitEntry) bool {
		for name, limit := range e.Limits {
			list := candidates[name]
			if limit != list[len(list)-1].Limit {
				return false
			}
		}
		return len(e.Limits) > 0
	}
	result, _ := settleLimits(t, in, func(e LimitEntry) limitAnswer {
		switch {
		case len(e.Limits) == 0:
			return limitAnswer{done: 30, waitOnly: 10, peak: 8}
		case loosest(e):
			return limitAnswer{done: 25, waitOnly: 10, peak: 8}
		}
		return limitAnswer{done: 21, waitOnly: 10, peak: 8}
	})
	if got := countSteps(result.Entries); got != "A=36 B=120 C=24" {
		t.Errorf("the settled plan lists %s, want A=36 B=120 C=24", got)
	}
	for _, e := range result.Entries {
		if _, kills := e.Limits[SignalKills]; kills || e.Signal == SignalKills {
			t.Errorf("the plan lists an entry for the kills, whose model failed: %+v", e)
		}
		if _, refusals := e.Limits["refusals"]; refusals || e.Signal == "refusals" {
			t.Errorf("the plan lists an entry for the provider's refusals, which are no signal: %+v", e)
		}
		if e.TokenLimit != 6_000_000 {
			t.Errorf("an entry carries the token limit %d, want 6000000 in every run: %+v", e.TokenLimit, e)
		}
	}
	var idleD [3]int
	var stepC []string
	for _, e := range result.Entries {
		if e.Step == LimitStepB && e.Signal == SignalIdle {
			switch time.Duration(e.IdleAfter) {
			case 10 * time.Minute:
				idleD[0]++
			case time.Hour:
				idleD[1]++
			case 4 * time.Hour:
				idleD[2]++
			}
		}
		if e.Step == LimitStepC && !slices.Contains(stepC, e.Rule) {
			stepC = append(stepC, e.Rule)
		}
	}
	if idleD != [3]int{12, 24, 12} {
		t.Errorf("step B runs the idle share at d of 10 minutes, 1 hour and 4 hours in %v runs, want [12 24 12]", idleD)
	}
	slices.Sort(stepC)
	if !slices.Equal(stepC, []string{"signals", "signals-backoff"}) {
		t.Errorf("step C runs the rules %v, want signals and signals-backoff", stepC)
	}
	for _, e := range result.Entries {
		if e.Step == LimitStepC && len(e.Limits) != 4 {
			t.Errorf("a run of step C has the limits %v, want each of the four signals that have a limit", e.Limits)
			break
		}
	}
	for _, s := range result.Signals {
		if s.Signal == "refusals" {
			t.Errorf("the result gives the provider's refusals a part, %+v, and they are no signal", s)
		}
	}
	if strings.Contains(renderLimits(t, result), "`refusals`") {
		t.Errorf("the table names the refusals:\n%s", renderLimits(t, result))
	}
	var killsGate *LimitGate
	for i := range result.Signals {
		if result.Signals[i].Signal == SignalKills {
			killsGate = &result.Signals[i].Gate
		}
	}
	if killsGate == nil || killsGate.Gives || !strings.Contains(killsGate.Why, "memory") || !strings.Contains(killsGate.Why, "daily test") {
		t.Errorf("the kills' gate is %+v, want no steps and the reason that the memory model fails the daily test", killsGate)
	}
	var want *LimitEntry
	for i, e := range result.Entries {
		if e.Step == LimitStepA && e.Signal == SignalProvider && e.Cap == 171 && e.Window == "window-2" && e.Limits[SignalProvider] == 0.9 {
			want = &result.Entries[i]
		}
	}
	if want == nil {
		t.Fatal("the plan lists no run of step A for the provider's headroom at 0.9 on window-2 at the cap 171")
	}
	wantArgs := []string{"replay", "run", "--window", "windows/window-2", "--rule", "signals", "--branch", "main", "--seed", "1", "--slowdown", "1",
		"--cap", "171", "--limit", "provider=0.9", "--idle-after", "1h0m0s", "--token-limit", "6000000", "--out", filepath.Join(in.RunsDir, want.Out)}
	if !slices.Equal(want.Args, wantArgs) {
		t.Errorf("the run's arguments are %q, want %q", want.Args, wantArgs)
	}
	for _, line := range []string{"the memory model fails R11's daily test", "token limit given: 6000000 tokens a minute", "the test across the two records"} {
		if !strings.Contains(renderLimits(t, result), line) {
			t.Errorf("the table holds no %q", line)
		}
	}
}

// countSteps is how many entries each step lists, as "A=44 B=144 C=24" for the steps that list any.
func countSteps(entries []LimitEntry) string {
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Step]++
	}
	var parts []string
	for _, step := range []string{LimitStepA, LimitStepB, LimitStepC} {
		if counts[step] > 0 {
			parts = append(parts, step+"="+strconv.Itoa(counts[step]))
		}
	}
	return strings.Join(parts, " ")
}

// renderLimits is the table of result as it prints.
func renderLimits(t *testing.T, result LimitsResult) string {
	t.Helper()
	var printed strings.Builder
	if err := result.Write(&printed); err != nil {
		t.Fatal(err)
	}
	return printed.String()
}

// A run writes what each of its limited signals read over the window and the minutes of the tokens
// model's refusals under the token limit it was given, in run.json (signalSummaries,
// suppliedRefused). On noLimitWindow under signals with a limit of 1 on the provider's headroom and
// a token limit of 6,000, the window from 10:20 to 10:55 is 35 minutes, read at each minute's
// start; the provider's record holds minutes 10:20 to 10:54, so the headroom, the tokens of the
// minute before over 6,000, is recorded at 10:21 to 10:54, 34 minutes (the minute before 10:20 lies
// outside the record), worked from the token_limit tests:
//
//   - 10:21 to 10:31, 11 minutes: 12,000 at one issue in flight in the record and in the replay, 2.
//   - 10:32 to 10:40, 9 minutes: 12,000 at the replay's 2 of the record's 3, 8,000, 4/3.
//   - 10:41, 1 minute: 10:40's 3,000 at 2 of 3, 2,000, 1/3.
//   - 10:42 to 10:54, 13 minutes: 3,000 at 3 of 3, 0.5.
//
// Its mean is (11·2 + 9·4/3 + 1/3 + 13·0.5) / 34 = 40.8333 / 34 = 1.2009804, its largest 2, and its
// 95th percentile by the nearest rank is the ⌈0.95·34⌉ = 33rd of the 34 sorted values, 2. It holds
// admission, at or above 1, in the 20 minutes of the first two ranges, of 35: 20/35. The tokens
// model refuses a minute whose modelled tokens pass 6,000: the 11 minutes 10:20 to 10:30 and the 9
// minutes 10:31 to 10:39, 20 on 2001-03-05, none after. With no token limit the provider has no
// limit in the run and the two are empty. run.json's signals, which the comparison's document gives
// as the signal's value at its limit, holds the provider's limit, 1, and its mean, 1.2009804; with no
// token limit it holds none.
func TestARunWritesWhatItsSignalsRead(t *testing.T) {
	t.Parallel()
	w := noLimitWindow(t)
	dsn := replayDSN(t)
	run := func(tokenLimit int64) runRecord {
		out := t.TempDir()
		if _, err := Run(context.Background(), RunConfig{Window: w, Rule: replayedRule(t, "signals"), Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out,
			Limits: map[string]float64{SignalProvider: 1}, TokenLimit: tokenLimit}); err != nil {
			t.Fatalf("replay under signals with the token limit %d: %v", tokenLimit, err)
		}
		var ran runRecord
		if err := readJSON(filepath.Join(out, runFile), &ran); err != nil {
			t.Fatal(err)
		}
		return ran
	}
	t.Run("a token limit", func(t *testing.T) {
		t.Parallel()
		ran := run(suppliedTokenLimit)
		got, ok := ran.SignalSummaries[SignalProvider]
		want := SignalSummary{Mean: 40.833333333333336 / 34, P95: 2, Max: 2, Recorded: 34, Minutes: 35, HeldShare: 20.0 / 35}
		if !ok || len(ran.SignalSummaries) != 1 || got.Recorded != want.Recorded || got.Minutes != want.Minutes || !closeTo(got.Mean, want.Mean) ||
			!closeTo(got.P95, want.P95) || !closeTo(got.Max, want.Max) || !closeTo(got.HeldShare, want.HeldShare) {
			t.Errorf("run.json's signalSummaries are %+v, want the provider's %+v alone", ran.SignalSummaries, want)
		}
		if !maps.Equal(ran.SuppliedRefused, map[string]int{"2001-03-05": 20}) {
			t.Errorf("run.json's suppliedRefused is %v, want 20 refused minutes on 2001-03-05", ran.SuppliedRefused)
		}
		reading, ok := ran.Signals[SignalProvider]
		if !ok || len(ran.Signals) != 1 || reading.Limit != 1 || !closeTo(reading.Value, want.Mean) {
			t.Errorf("run.json's signals are %+v, want the provider's limit 1 and its mean %v alone", ran.Signals, want.Mean)
		}
	})
	t.Run("no token limit", func(t *testing.T) {
		t.Parallel()
		ran := run(0)
		if ran.SignalSummaries == nil || len(ran.SignalSummaries) != 0 || ran.SuppliedRefused == nil || len(ran.SuppliedRefused) != 0 {
			t.Errorf("run.json's signalSummaries are %v and its suppliedRefused %v, want both empty and not absent", ran.SignalSummaries, ran.SuppliedRefused)
		}
		if len(ran.Signals) != 0 {
			t.Errorf("run.json's signals are %+v, want none for a run that limited no signal", ran.Signals)
		}
	})
}

// A run written before run.json held the signals' summaries and the refused minutes under the
// token limit is refused by every report read from runs, with an error that names the run and says
// it must be run again, as a run written before the scorecards held the wait of urgent work is.
func TestRunsWrittenBeforeTheSignalSummariesAreRefused(t *testing.T) {
	t.Parallel()
	const window, rule = "old-signals", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	for _, field := range []string{"signalSummaries", "suppliedRefused"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			issueTimesRuns(t, dir, window, rule, true, from, from.Add(24*time.Hour), from.Add(time.Hour), from.Add(time.Hour))
			if _, err := ReadRuns(dir); err != nil {
				t.Fatalf("the runs as written are refused: %v", err)
			}
			path := filepath.Join(runDir(dir, window, rule, MainBranchAt, 1), runFile)
			var fields map[string]any
			if err := readJSON(path, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, field)
			if err := writeJSON(path, fields); err != nil {
				t.Fatal(err)
			}
			_, err := ReadRuns(dir)
			if err == nil {
				t.Fatalf("a run.json with no %s is read", field)
			}
			for _, want := range []string{filepath.Dir(path), "signalSummaries", "suppliedRefused", "again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// A run written while the provider's refusals were a signal, whose run.json limits them, is refused
// by every report read from runs (spec R11, v52), as a run written before its signals' summaries is:
// its admissions rest on a limit no run takes any more, so the error names the run, the refusals,
// and says it must be run again. A run that limits the five signals alone is read.
func TestRunsThatLimitedTheRefusalsAreRefused(t *testing.T) {
	t.Parallel()
	const window, rule = "old-refusals", "legion-v20.42.5"
	from := time.Date(2001, time.March, 5, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	issueTimesRuns(t, dir, window, rule, true, from, from.Add(24*time.Hour), from.Add(time.Hour), from.Add(time.Hour))
	path := filepath.Join(runDir(dir, window, rule, MainBranchAt, 1), runFile)
	var fields map[string]any
	if err := readJSON(path, &fields); err != nil {
		t.Fatal(err)
	}
	fields["limits"] = map[string]any{SignalKills: 2.0}
	if err := writeJSON(path, fields); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuns(dir); err != nil {
		t.Fatalf("a run that limits the kills alone is refused: %v", err)
	}
	fields["limits"] = map[string]any{SignalKills: 2.0, "refusals": 4.0}
	fields["signalSummaries"] = map[string]any{"refusals": map[string]any{"mean": 1, "p95": 4, "max": 4, "recorded": 60, "minutes": 60, "heldShare": 0.25}}
	if err := writeJSON(path, fields); err != nil {
		t.Fatal(err)
	}
	_, err := ReadRuns(dir)
	if err == nil {
		t.Fatal("a run.json that limits the refusals is read")
	}
	for _, want := range []string{filepath.Dir(path), "refusals", "again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}
