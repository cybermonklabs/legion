package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The conflict model is fitted by maximum likelihood and printed with its interval (spec R2): on
// stretches drawn from a known base and merges term, each coefficient's 95% interval holds the
// true value in about 95 of 100 draws, and the estimates centre on it. The test draws 40 sets of
// 2000 synthetic stretches, M uniform on 0 to 15, from base −2 and term 0.8 with seeded sources,
// fits each, and fails where an interval holds its true value in fewer than 32 of the 40 (a 95%
// interval misses that often with a chance near 1 in 10,000), or where the mean estimate stands
// further than 0.1 from the base or 0.05 from the term. On the fixture window
// conflict-from-fitted-rate, whose stretches separate (the one with M = 1 conflicted, the two with
// M = 0 merged), the fit reaches a chance below 10⁻⁶ at M = 0 and above 1 − 10⁻⁶ at M = 1, the
// limits its README requires, and gives each coefficient an interval with no end.
func TestConflictRateFit(t *testing.T) {
	t.Parallel()
	t.Run("synthetic stretches", func(t *testing.T) {
		const base, term, draws, size = -2.0, 0.8, 40, 2000
		covered, sums := [2]int{}, [2]float64{}
		for seed := range uint64(draws) {
			source := rand.New(rand.NewPCG(seed, 0x66697474696e67))
			stretches := make([]Stretch, size)
			for i := range stretches {
				merges := source.IntN(16)
				chance := logistic(base + term*math.Log2(1+float64(merges)))
				stretches[i] = Stretch{Issue: fmt.Sprintf("ACME-%d", i+1), Merges: merges, Conflict: source.Float64() < chance}
			}
			model := FitConflicts(stretches)
			if model.Stretches != size || model.Level != conflictLevel {
				t.Fatalf("draw %d: the model was fitted on %d stretches at level %v, want %d at %v", seed, model.Stretches, model.Level, size, conflictLevel)
			}
			for i, c := range []struct {
				name string
				got  Coefficient
				want float64
			}{{"base", model.Base, base}, {"merges term", model.Term, term}} {
				if math.IsInf(c.got.Low, 0) || math.IsInf(c.got.High, 0) || c.got.Low > c.got.Estimate || c.got.Estimate > c.got.High {
					t.Fatalf("draw %d: the %s is %v with the interval [%v, %v], want a finite interval around the estimate", seed, c.name, c.got.Estimate,
						c.got.Low, c.got.High)
				}
				if c.got.Low <= c.want && c.want <= c.got.High {
					covered[i]++
				}
				sums[i] += c.got.Estimate
			}
		}
		for i, c := range []struct {
			name      string
			want, tol float64
		}{{"base", base, 0.1}, {"merges term", term, 0.05}} {
			t.Logf("the %s: true %v, mean estimate %v over %d draws, its 95%% interval holding the true value in %d", c.name, c.want,
				sums[i]/draws, draws, covered[i])
			if covered[i] < 32 {
				t.Errorf("the %s's 95%% interval held its true value %v in %d of %d draws, want at least 32", c.name, c.want, covered[i], draws)
			}
			if mean := sums[i] / draws; math.Abs(mean-c.want) > c.tol {
				t.Errorf("the %s's mean estimate over %d draws is %v, want within %v of %v", c.name, draws, mean, c.tol, c.want)
			}
		}
	})
	t.Run("the fixture window's stretches separate", func(t *testing.T) {
		window := fixtureWindow(t, "conflict-from-fitted-rate")
		trace, err := BuildTrace(window)
		if err != nil {
			t.Fatalf("build the window's trace: %v", err)
		}
		stretches := recordedStretches(trace, newBaseMerges(window, trace))
		want := []Stretch{
			{Issue: "ACME-1", Start: clock(9, 45), End: clock(11, 0), Merges: 1, Conflict: true, OwnMergeUnknown: true},
			{Issue: "ACME-1", Start: clock(11, 30), End: clock(12, 45), Merges: 0},
			{Issue: "ACME-2", Start: clock(9, 50), End: clock(10, 45), Merges: 0},
		}
		if !slices.EqualFunc(stretches, want, sameStretch) {
			t.Fatalf("the window's stretches are %+v, want %+v", stretches, want)
		}
		model := FitConflicts(stretches)
		if model.OwnMergeUnknown != 1 {
			t.Errorf("the model counts %d stretches whose M may hold their own merge, want 1: the window's merges name no pull request",
				model.OwnMergeUnknown)
		}
		t.Logf("the fixture window's conflict model: %s; base %v [%v, %v], merges term %v [%v, %v]; chance at M = 0 %v, at M = 1 %v", model,
			model.Base.Estimate, model.Base.Low, model.Base.High, model.Term.Estimate, model.Term.Low, model.Term.High, model.Chance(phase.Merging, 0),
			model.Chance(phase.Merging, 1))
		if chance := model.Chance(phase.Merging, 0); chance > 1e-6 {
			t.Errorf("the fitted chance at M = 0 is %v, want below 1e-6", chance)
		}
		if chance := model.Chance(phase.Merging, 1); chance < 1-1e-6 {
			t.Errorf("the fitted chance at M = 1 is %v, want above 1 - 1e-6", chance)
		}
		for name, c := range map[string]Coefficient{"base": model.Base, "merges term": model.Term} {
			if !math.IsInf(c.Low, -1) || !math.IsInf(c.High, 1) {
				t.Errorf("the %s's interval is [%v, %v], and stretches that separate give it no end", name, c.Low, c.High)
			}
		}
		if _, err := json.Marshal(model); err != nil {
			t.Errorf("the model of stretches that separate does not write as JSON: %v", err)
		}
	})
}

// sameStretch says whether a and b are one stretch, its moments compared as instants.
func sameStretch(a, b Stretch) bool {
	return a.Issue == b.Issue && a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.Merges == b.Merges && a.Conflict == b.Conflict &&
		a.OwnMergeUnknown == b.OwnMergeUnknown
}

// linkedPullRequest is window with key's first Dispatch event linking the pull request url among
// its external links, as Dispatch serves a link: an object with its url.
func linkedPullRequest(t *testing.T, window Window, key, url string) Window {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal(window.Events[key][0], &event); err != nil {
		t.Fatal(err)
	}
	event["payload"].(map[string]any)["external_links"] = []map[string]string{{"url": url, "kind": "url"}}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string][]json.RawMessage{}
	for issue, list := range window.Events {
		events[issue] = slices.Clone(list)
	}
	events[key][0] = raw
	window.Events = events
	return window
}

// A stretch's M leaves out the issue's own merge: the commit of the pull request its Dispatch
// events link, which GitHub lands a little before the daemon sees the merge, inside the stretch
// the merge ends. Where the issue links no pull request, or a commit inside the stretch names none,
// M counts every commit and the stretch is counted as one whose own merge could not be told apart.
// The test reads conflictWindow, ACME-1's stretches 09:45 to 11:00 and 11:30 to 12:45, over merges
// at 10:30 (pull request 7, another issue's) and 12:44 (pull request 41). With ACME-1's events
// linking pull request 41 of acme/widgets, its second stretch counts M = 0, its own merge left out,
// and its first M = 1, none counted unknown. With no link, the second counts M = 1 and both are
// counted. With the link and a merge at 12:00 that names no pull request, the second counts M = 1
// (the 12:00 merge) and is counted. The squash suffix the merges are read from is ` (#<n>)` at the
// end of a commit's first line.
func TestAStretchCountsNoMergeOfItsOwn(t *testing.T) {
	t.Parallel()
	merges := []Merge{
		{SHA: "7a", At: clock(10, 30), Branch: "main", PR: 7},
		{SHA: "41b", At: clock(12, 44), Branch: "main", PR: 41},
	}
	unnamed := Merge{SHA: "c0", At: clock(12, 0), Branch: "main"}
	link := "https://github.com/acme/widgets/pull/41"
	for _, c := range []struct {
		name    string
		linked  bool
		merges  []Merge
		want    []Stretch
		unknown int
	}{
		{name: "its own pull request linked", linked: true, merges: merges, want: []Stretch{
			{Issue: "ACME-1", Start: clock(9, 45), End: clock(11, 0), Merges: 1, Conflict: true},
			{Issue: "ACME-1", Start: clock(11, 30), End: clock(12, 45), Merges: 0},
		}},
		{name: "no pull request linked", merges: merges, unknown: 2, want: []Stretch{
			{Issue: "ACME-1", Start: clock(9, 45), End: clock(11, 0), Merges: 1, Conflict: true, OwnMergeUnknown: true},
			{Issue: "ACME-1", Start: clock(11, 30), End: clock(12, 45), Merges: 1, OwnMergeUnknown: true},
		}},
		{name: "a merge naming no pull request", linked: true, merges: append(slices.Clone(merges), unnamed), unknown: 1, want: []Stretch{
			{Issue: "ACME-1", Start: clock(9, 45), End: clock(11, 0), Merges: 1, Conflict: true},
			{Issue: "ACME-1", Start: clock(11, 30), End: clock(12, 45), Merges: 1, OwnMergeUnknown: true},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			window := conflictWindow(t)
			window.Merges = map[string][]Merge{builtRepository: c.merges}
			if c.linked {
				window = linkedPullRequest(t, window, "ACME-1", link)
			}
			trace, err := BuildTrace(window)
			if err != nil {
				t.Fatalf("build the window's trace: %v", err)
			}
			if got, want := ownPullRequest(trace.Issues["ACME-1"]), map[bool]int{true: 41}[c.linked]; got != want {
				t.Errorf("the trace reads ACME-1's own pull request as %d, want %d", got, want)
			}
			stretches := recordedStretches(trace, newBaseMerges(window, trace))
			if !slices.EqualFunc(stretches, c.want, sameStretch) {
				t.Errorf("the window's stretches are %+v, want %+v", stretches, c.want)
			}
			if got := FitConflicts(stretches).OwnMergeUnknown; got != c.unknown {
				t.Errorf("the model counts %d stretches whose M may hold their own merge, want %d", got, c.unknown)
			}
		})
	}
	for message, want := range map[string]int{
		"fix(daemon): a replay counts no merge of its own (#41)":              41,
		"fix(daemon): a replay counts no merge of its own (#41)\n\nbody (#9)": 41,
		"Merge pull request #41 from acme/widgets":                            0,
		"fix(daemon): see (#41) for why":                                      0,
	} {
		if got := squashedPullRequest(message); got != want {
			t.Errorf("the commit message %q names pull request %d, want %d", message, got, want)
		}
	}
}

// reachedConflictWindow is a window from 12:00 to 18:00 recorded under legion-v20.42.5, its inputs
// reaching back to 00:00, whose reach and window hold stretches at two rates of conflict. ACME-1, a
// root promoted at 00:30, is in implementing from 01:00. In the reach it goes to testing and back 30
// times, 10 minutes each way, its tester failing every round: 30 stretches, none a conflict, the
// last ending at 11:00. It goes to testing again at 11:50, and a conflict at 12:05 ends that stretch
// inside the window, though it began in the reach. In the window it goes to testing and back 10
// times from 12:20, its head conflicting in every second round and its tester failing in the
// others: 10 stretches, 5 of them conflicts. It goes to testing a last time at 15:40 and stands
// there at the window's end, a stretch with no outcome. Its base branch took no merge, so every
// stretch has M = 0.
func reachedConflictWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(12, 0), clock(18, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(12 * time.Hour)
	b.capped(2)
	b.boot(clock(0, 30))
	b.file(clock(0, 30), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(0, 30), "ACME-1")
	b.rootDesigned(clock(0, 45), "ACME-1")
	b.segmentEnds(clock(1, 0), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	round := func(at time.Time, back workflow.TriggerKind) {
		b.segmentEnds(at, "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
		b.segmentEnds(at.Add(10*time.Minute), "ACME-1", phase.Testing, phase.Implementing, back)
	}
	for i := range 30 {
		round(clock(1, 10).Add(time.Duration(i)*20*time.Minute), workflow.TriggerTesterFailed)
	}
	b.segmentEnds(clock(11, 50), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	b.segmentEnds(clock(12, 5), "ACME-1", phase.Testing, phase.Implementing, workflow.TriggerConflict)
	for i := range 10 {
		back := workflow.TriggerTesterFailed
		if i%2 == 0 {
			back = workflow.TriggerConflict
		}
		round(clock(12, 20).Add(time.Duration(i)*20*time.Minute), back)
	}
	b.segmentEnds(clock(15, 40), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// The conflict model is fitted on the stretches inside the window alone (plan D3): a stretch that
// began in the reach ran under what the reach recorded, and a reach of a release with no conflict
// trigger records each of its conflicts as another move back. The test reads
// reachedConflictWindow, whose record holds 41 ended stretches, 6 of them conflicts: 30 in the reach
// with none, one from the reach into the window that ends in one, and 10 in the window, 5 of them
// conflicts. It fails where the model is fitted on other than the window's 10 over the window's
// span; where its base is not the window's rate, the log-odds of 5 conflicts to 5 stretches
// without one (0), or its interval does not hold that; where the interval holds the base of the 41
// pooled (the log-odds of 6 to 35, about −1.76); where the printed model does not give the span;
// and where the window's record in a directory of runs counts other stretches than the model.
func TestTheConflictModelIsFittedOnTheWindowAlone(t *testing.T) {
	t.Parallel()
	window := reachedConflictWindow(t)
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build the window's trace: %v", err)
	}
	merges := newBaseMerges(window, trace)
	pooled := FitConflicts(recordedStretches(trace, merges))
	if pooled.Stretches != 41 || pooled.Conflicts != 6 {
		t.Fatalf("the record holds %d ended stretches, %d of them conflicts; want 41 and 6: 30 in the reach, one from the reach into the window and 10 in the window",
			pooled.Stretches, pooled.Conflicts)
	}
	const stretches, conflicts = 10, 5
	model := windowConflicts(trace, merges)
	if model.Stretches != stretches || model.Conflicts != conflicts || !model.From.Equal(window.From) || !model.To.Equal(window.To) {
		t.Fatalf("the model was fitted on %d stretches, %d of them conflicts, over %s to %s; want the window's %d and %d over %s to %s",
			model.Stretches, model.Conflicts, stamp(model.From), stamp(model.To), stretches, conflicts, stamp(window.From), stamp(window.To))
	}
	t.Logf("the window's conflict model: %s; the base of the window's and the reach's stretches pooled: %v [%v, %v]", model,
		pooled.Base.Estimate, pooled.Base.Low, pooled.Base.High)
	rate := math.Log(float64(conflicts) / float64(stretches-conflicts))
	if base := model.Base; math.Abs(base.Estimate-rate) > 1e-6 || base.Low > rate || rate > base.High {
		t.Errorf("the base is %v [%v, %v], want the window's rate %v within its interval", base.Estimate, base.Low, base.High, rate)
	}
	if base := model.Base; base.Low <= pooled.Base.Estimate && pooled.Base.Estimate <= base.High {
		t.Errorf("the base's interval [%v, %v] holds %v, the base of the window's and the reach's stretches pooled", base.Low, base.High,
			pooled.Base.Estimate)
	}
	if span := "began and ended inside the window from 2001-03-05T12:00:00Z to 2001-03-05T18:00:00Z"; !strings.Contains(model.String(), span) {
		t.Errorf("the printed model %q does not give the span it was fitted on, %q", model, span)
	}
	rec, err := newWindowRecord(window, "window-one", 1)
	if err != nil {
		t.Fatalf("the window's record: %v", err)
	}
	if rec.Stretches != stretches || rec.Conflicted != conflicts {
		t.Errorf("the window's record counts %d stretches, %d of them conflicts; want the model's %d and %d", rec.Stretches, rec.Conflicted,
			stretches, conflicts)
	}
}

// reviewConflictLoop is key, a root promoted at 09:00 at rank, working 15 minutes in each phase:
// its tester fails it at 10:00, and at 10:45 a conflict moves it back from reviewing; its loop is
// 15 minutes in implementing and 15 in testing, and it is back in reviewing at 11:15.
func reviewConflictLoop(b *windowBuilder, key, rank string) {
	b.t.Helper()
	rootThrough(b, key, rank, clock(9, 0), clock(9, 15), moveAt(9, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(9, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 0, phase.Implementing, workflow.TriggerTesterFailed),
		moveAt(10, 15, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 30, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(10, 45, phase.Implementing, workflow.TriggerConflict), moveAt(11, 0, phase.Testing, workflow.TriggerImplementationReady),
		moveAt(11, 15, phase.Reviewing, workflow.TriggerTesterPassed))
}

// phaseLoopsRoots is ACME-1 and ACME-2 of phaseLoopsWindow: ACME-1, a root promoted at 07:05,
// moves to testing at 08:50, and its tester fails it at 09:30; ACME-2 is reviewConflictLoop's.
func phaseLoopsRoots(b *windowBuilder) {
	b.t.Helper()
	rootThrough(b, "ACME-1", "a1", clock(7, 5), clock(7, 20), moveAt(7, 35, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 30, phase.Implementing, workflow.TriggerTesterFailed))
	reviewConflictLoop(b, "ACME-2", "a2")
}

// phaseLoopsWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00, whose recorded conflict loops left reviewing and,
// where fromTesting, testing. ACME-1, a root promoted at 07:05, moves to testing at 08:50 and stands
// there at the window's start; its tester fails it at 09:30, and it works in implementing to the
// window's end. ACME-2, a root promoted at 09:00, works 15 minutes in each phase: its tester fails
// it at 10:00, and at 10:45 a conflict moves it back from reviewing; its loop is 15 minutes in
// implementing and 15 in testing, and it is back in reviewing at 11:15 to the window's end. With
// fromTesting, ACME-3, a root promoted at 11:00, works 15 minutes in each phase to testing at
// 11:45, and a conflict moves it back at 12:00; its loop is 25 minutes in implementing, and it is
// back in testing at 12:25 to the window's end. The base branch took a commit inside ACME-1's
// stretch, at 09:10, and one inside each stretch that ends in a conflict, at 10:40 and 11:50: the
// stretches inside the window separate, so the chance fitted at M = 1 is 1 and at M = 0 is 0, and
// ACME-1's, which began in the reach and is no part of the fit, draws a conflict at 09:30 under any
// seed.
func phaseLoopsWindow(t *testing.T, fromTesting bool) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(3)
	b.boot(clock(7, 0))
	phaseLoopsRoots(b)
	merges := []Merge{
		{SHA: "3e9a7c1b5d2f4068a1b2c3d4e5f60718293a4b5c", At: clock(9, 10), Branch: "main"},
		{SHA: "7c4d2e9f1a3b5067b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 40), Branch: "main"},
	}
	if fromTesting {
		testingConflictLoop(b)
		merges = append(merges, Merge{SHA: "9f2b6d4a8c1e3057c2d3e4f5a6b7c8d9e0f1a2b3", At: clock(11, 50), Branch: "main"})
	}
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: merges}
	return window
}

// testingConflictLoop is ACME-3 of phaseLoopsWindow's loop from testing: a root promoted at 11:00
// that works 15 minutes in each phase to testing at 11:45, where a conflict moves it back at 12:00;
// its loop is 25 minutes in implementing, and it is back in testing at 12:25 to the window's end.
func testingConflictLoop(b *windowBuilder) {
	b.t.Helper()
	rootThrough(b, "ACME-3", "a3", clock(11, 0), clock(11, 15), moveAt(11, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(11, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 0, phase.Implementing, workflow.TriggerConflict),
		moveAt(12, 25, phase.Testing, workflow.TriggerImplementationReady))
}

// backInTesting is ACME-1's moves in a replay of phaseLoopsWindow whose drawn conflict returns it
// to the phase the conflict left: back to implementing by conflict, to testing at the end of the
// loop's implementing, and its own recorded testing outcome, its tester's failure.
var backInTesting = []fixtureMove{
	{From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerConflict},
	{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady},
	{From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerTesterFailed},
}

// replayWindowOn replays window under rule on branch at seed, and answers the result and the
// replay's log.
func replayWindowOn(t *testing.T, window Window, rule string, branch Branch, seed int64) (RunResult, []replayLine) {
	t.Helper()
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, rule), Branch: branch, Seed: seed,
		Slowdown: 1, PGDSN: replayDSN(t), Out: out})
	if err != nil {
		t.Fatalf("replay the window under %s on %s at seed %d: %v", rule, branch.Name, seed, err)
	}
	return result, replayLog(t, out)
}

// replayLoopWindow replays window under rule on the main branch at seed, and answers the result,
// the replay's log and each issue's moves in it.
func replayLoopWindow(t *testing.T, window Window, rule string, seed int64) (RunResult, []replayLine, map[string][]fixtureMove) {
	t.Helper()
	result, log := replayWindowOn(t, window, rule, MainBranch, seed)
	return result, log, replayedMoves(log)
}

// checkMovesAt fails where key's moves in a replay at seed (moved, from its log) or their moments
// differ from want and at.
func checkMovesAt(t *testing.T, seed int64, key string, log []replayLine, moved map[string][]fixtureMove, want []fixtureMove, at []time.Time) {
	t.Helper()
	moments := linesAt(log, func(line replayLine) bool { return line.Msg == phaseChangedLine && line.Issue == key })
	if !slices.Equal(moved[key], want) || !slices.EqualFunc(moments, at, time.Time.Equal) {
		t.Errorf("at seed %d %s moved %s at %s, want %s at %s", seed, key, describeMoves(moved[key]), describeTimes(moments),
			describeMoves(want), describeTimes(at))
	}
}

// checkACME1Moves replays window under legion-v20.42.5, the rule it ran, on the main branch at
// seed, and fails where ACME-1's moves in the replay or their moments differ from want and at, or
// where the replay refused a move. It answers the replay's log.
func checkACME1Moves(t *testing.T, window Window, seed int64, want []fixtureMove, at []time.Time) []replayLine {
	t.Helper()
	result, log, moved := replayLoopWindow(t, window, "legion-v20.42.5", seed)
	checkMovesAt(t, seed, "ACME-1", log, moved, want, at)
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("at seed %d the replay refused %+v", seed, refused)
	}
	return log
}

// checkImplementerTurns fails where ACME-1's implementer in a replay at seed (its log) starts or
// ends its turns from 09:30 to 10:00 at other moments than wantStarted and wantEnded.
func checkImplementerTurns(t *testing.T, seed int64, log []replayLine, wantStarted, wantEnded []time.Time) {
	t.Helper()
	implementer := string(builtToken(t, "ACME-1", claim.RoleImplementer))
	inLoop := func(line replayLine, msg string) bool {
		return line.Msg == msg && line.Claim == implementer && !line.Time.Before(clock(9, 30)) && line.Time.Before(clock(10, 0))
	}
	started := linesAt(log, func(line replayLine) bool { return inLoop(line, turnStartedLine) })
	ended := linesAt(log, func(line replayLine) bool { return inLoop(line, turnEndedLine) })
	if !slices.EqualFunc(started, wantStarted, time.Time.Equal) || !slices.EqualFunc(ended, wantEnded, time.Time.Equal) {
		t.Errorf("at seed %d ACME-1's implementer started turns at %s and ended turns at %s from 09:30 to 10:00, want started at %s and ended at %s",
			seed, describeTimes(started), describeTimes(ended), describeTimes(wantStarted), describeTimes(wantEnded))
	}
}

// A drawn conflict's loop is drawn among the recorded loops whose conflict left the same phase
// (spec R2): a loop recorded after a conflict in a later phase holds the work of that phase's
// return, which would carry the issue past the phase its own conflict left. The test replays
// phaseLoopsWindow with its loop from testing under legion-v20.42.5, the rule it ran, at the seeds
// 1 to 5. At each, ACME-1's stretch draws a conflict at 09:30 and is given ACME-3's loop, the one
// left from testing, not ACME-2's, left from reviewing: ACME-1 moves back to implementing by
// conflict at 09:30, to testing at 09:55 after the loop's 25 minutes of implementing, and runs its
// own recorded testing, 40 minutes, to its tester's failure at 10:35. The test fails for a seed
// whose replay moves ACME-1 otherwise or at other moments, or refuses a move.
func TestADrawnConflictIsGivenALoopLeftFromItsPhase(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	window := phaseLoopsWindow(t, true)
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			checkACME1Moves(t, window, seed, backInTesting, []time.Time{clock(9, 30), clock(9, 55), clock(10, 35)})
		})
	}
}

// unendedLoopWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00, whose recorded conflict loops left reviewing and
// testing, one of those from testing still under way at the window's end. ACME-1 and ACME-2 are
// phaseLoopsWindow's: ACME-1 stands in testing at the window's start, its tester fails it at
// 09:30, and it works in implementing to the window's end; ACME-2's loop leaves reviewing at 10:45
// and is back there at 11:15. ACME-3, a root promoted at 11:00, works 15 minutes in each phase to
// testing at 11:45, and a conflict moves it back at 12:00; its loop is 25 minutes in implementing,
// in two turns of its implementer: one from 12:00 that ends at 12:10 with no move, and one from
// 12:15 that ends at 12:25 with the move back to testing, where it stands to the window's end.
// ACME-4, a root promoted at 11:05, works 15 minutes in each phase to testing at 11:55, and a
// conflict moves it back at 12:15; the record shows no return: it is in implementing to the
// window's end, its implementer in a turn from 12:15. The base branch took a commit inside
// ACME-1's stretch, at 09:10, and one inside each stretch that ends in a conflict, at 10:40, 11:50
// and 12:05: the stretches inside the window separate, so the chance fitted at M = 1 is 1 and at
// M = 0 is 0, and ACME-1's, which began in the reach and is no part of the fit, draws a conflict at
// 09:30 under any seed.
func unendedLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(4)
	b.boot(clock(7, 0))
	phaseLoopsRoots(b)
	rootThrough(b, "ACME-3", "a3", clock(11, 0), clock(11, 15), moveAt(11, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(11, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 0, phase.Implementing, workflow.TriggerConflict))
	b.turnEnds(clock(12, 10), "ACME-3", claim.RoleImplementer)
	b.turnStarts(clock(12, 15), "ACME-3", claim.RoleImplementer)
	b.segmentEnds(clock(12, 25), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	rootThrough(b, "ACME-4", "a4", clock(11, 5), clock(11, 20), moveAt(11, 35, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(11, 55, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 15, phase.Implementing, workflow.TriggerConflict))
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "3e9a7c1b5d2f4068a1b2c3d4e5f60718293a4b5c", At: clock(9, 10), Branch: "main"},
		{SHA: "7c4d2e9f1a3b5067b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 40), Branch: "main"},
		{SHA: "9f2b6d4a8c1e3057c2d3e4f5a6b7c8d9e0f1a2b3", At: clock(11, 50), Branch: "main"},
		{SHA: "c4e8a2f6b0d14079d3e4f5a6b7c8d9e0f1a2b3c4", At: clock(12, 5), Branch: "main"},
	}}
	return window
}

// A drawn conflict's loop is drawn among the recorded loops whose work the record shows ending
// (spec R2): a loop still under way at the window's end has no known length, and an issue given it
// would stand in its last segment to the window's end, its worker in the turn the record leaves
// open there, and never run its own work after the loop. The test replays unendedLoopWindow under
// legion-v20.42.5, the rule it ran, at the seeds 1 to 8. At each, ACME-1's stretch draws a
// conflict at 09:30 and is given ACME-3's loop, not ACME-4's: ACME-1 moves back to implementing by
// conflict at 09:30; its implementer works the loop's two turns, from 09:30 to 09:40, which ends
// with no move, and from 09:45 to 09:55, which ends with the move to testing; and it runs its own
// recorded testing, 40 minutes, to its tester's failure at 10:35. The test fails for a seed whose
// replay moves ACME-1 otherwise or at other moments, starts or ends its implementer's turns from
// 09:30 to 10:00 otherwise, or refuses a move.
func TestADrawnConflictIsGivenNoLoopTheRecordShowsUnended(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	window := unendedLoopWindow(t)
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			log := checkACME1Moves(t, window, seed, backInTesting, []time.Time{clock(9, 30), clock(9, 55), clock(10, 35)})
			checkImplementerTurns(t, seed, log, []time.Time{clock(9, 30), clock(9, 45)}, []time.Time{clock(9, 40), clock(9, 55)})
		})
	}
}

// openLoopWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00, with two stretches that began in the reach, hold no
// commit of the base branch and end in a conflict inside the window. ACME-1, a root promoted at
// 07:05, moves to testing at 08:50, and a conflict moves it back at 09:30; the record shows no
// return: it is in implementing to the window's end, its implementer's turn from 09:30 ending at
// 09:40 with no move and its next, from 09:45, under way at 13:00. ACME-2, a root promoted at
// 07:10, moves to testing at 08:40, and a conflict moves it back at 09:20; its loop is 30 minutes
// in implementing, it is back in testing at 09:50, its tester passes it at 10:20, and it is in
// reviewing to the window's end. ACME-3, a root promoted at 09:00, works 15 minutes in each phase:
// its tester fails it at 10:00, and at 10:45 a conflict moves it back from reviewing, its stretch
// holding the base branch's one commit, at 10:40; its loop is 15 minutes in implementing and 15 in
// testing, and it is back in reviewing at 11:15 to the window's end. ACME-3's two stretches, the
// window's only ones in the fit, separate, so the chance fitted at M = 0 is 0 and at M = 1 is 1,
// and ACME-1's and ACME-2's stretches, with M = 0, draw no conflict under any seed.
func openLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(3)
	b.boot(clock(7, 0))
	rootThrough(b, "ACME-1", "a1", clock(7, 5), clock(7, 20), moveAt(7, 35, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 30, phase.Implementing, workflow.TriggerConflict))
	b.turnEnds(clock(9, 40), "ACME-1", claim.RoleImplementer)
	b.turnStarts(clock(9, 45), "ACME-1", claim.RoleImplementer)
	rootThrough(b, "ACME-2", "a2", clock(7, 10), clock(7, 25), moveAt(7, 40, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 40, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 20, phase.Implementing, workflow.TriggerConflict),
		moveAt(9, 50, phase.Testing, workflow.TriggerImplementationReady), moveAt(10, 20, phase.Reviewing, workflow.TriggerTesterPassed))
	reviewConflictLoop(b, "ACME-3", "a3")
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "7c4d2e9f1a3b5067b1c2d3e4f5a6b7c8d9e0f1a2", At: clock(10, 40), Branch: "main"},
	}}
	return window
}

// A stretch whose recorded conflict loop runs to the window's end keeps that conflict, whatever the
// draw would say, and its issue runs its own loop (assumption 35); a stretch whose loop returns is
// drawn as before. The test replays openLoopWindow, where neither ACME-1's stretch nor ACME-2's
// would draw a conflict. Under legion-v20.42.5, the rule it ran, at the seeds 1 to 5: ACME-1 keeps
// its conflict, moving back to implementing by conflict at 09:30, and its implementer works its own
// loop's turns, from 09:30 to 09:40 and from 09:45 on; ACME-2's stretch draws no conflict and goes
// on past its loop, running its testing after the loop, 30 minutes, to its tester's pass at 09:50;
// and the run counts one conflict kept and names assumption 35. Under main at seed 1, ACME-1 keeps
// its conflict as a merge forward asked at 09:30, which lasts as its own loop's implementing round,
// unended, and never lands. The test fails for a replay that moves either issue otherwise or at
// other moments, starts or ends ACME-1's implementer's turns from 09:30 to 10:00 otherwise, counts
// other than one conflict kept or does not name assumption 35, and for a replay under main that
// asks ACME-1's merge forward at another moment or lands it.
func TestAStretchWhoseLoopRunsToTheWindowsEndKeepsItsConflict(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	window := openLoopWindow(t)
	replay := func(t *testing.T, rule string, seed int64) (RunResult, []replayLine, map[string][]fixtureMove) {
		t.Helper()
		result, log, moved := replayLoopWindow(t, window, rule, seed)
		if result.Divergence.KeptOpenLoops != 1 || !slices.Contains(result.Assumptions, assumeKeptOpenLoop) {
			t.Errorf("under %s at seed %d the run kept %d conflicts and named the assumptions %v, want one kept and assumption %d named",
				rule, seed, result.Divergence.KeptOpenLoops, result.Assumptions, assumeKeptOpenLoop)
		}
		return result, log, moved
	}
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("legion-v20.42.5/seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			result, log, moved := replay(t, "legion-v20.42.5", seed)
			checkMovesAt(t, seed, "ACME-1", log, moved, []fixtureMove{{From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerConflict}},
				[]time.Time{clock(9, 30)})
			checkMovesAt(t, seed, "ACME-2", log, moved, []fixtureMove{{From: phase.Testing, To: phase.Reviewing, Trigger: workflow.TriggerTesterPassed}},
				[]time.Time{clock(9, 50)})
			checkImplementerTurns(t, seed, log, []time.Time{clock(9, 30), clock(9, 45)}, []time.Time{clock(9, 40)})
			if refused := result.Divergence.Refused; len(refused) > 0 {
				t.Errorf("at seed %d the replay refused %+v", seed, refused)
			}
		})
	}
	t.Run("main", func(t *testing.T) {
		t.Parallel()
		_, log, _ := replay(t, "main", 1)
		asked := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardAskedLine && line.Issue == "ACME-1" })
		landed := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardLandedLine && line.Issue == "ACME-1" })
		if !slices.EqualFunc(asked, []time.Time{clock(9, 30)}, time.Time.Equal) || len(landed) > 0 {
			t.Errorf("under main the merge forward of ACME-1 was asked at %s and landed at %s, want asked at 09:30 and never landed",
				describeTimes(asked), describeTimes(landed))
		}
	})
}

// Where the window records no loop left from the phase of a drawn conflict, the loop is drawn
// among those that left a later phase and cut at its first piece in that phase or a phase after
// it, so the issue returns to the phase its conflict left. The test replays phaseLoopsWindow
// without its loop from testing under legion-v20.42.5: ACME-1's stretch draws a conflict at 09:30
// and is given ACME-2's loop, left from reviewing, up to its testing. ACME-1 moves back to
// implementing by conflict at 09:30, to testing at 09:45 after the loop's 15 minutes of
// implementing, and runs its own recorded testing to its tester's failure at 10:25, rather than
// going on to reviewing by the loop's testing.
func TestALoopFromALaterPhaseReturnsTheIssueToThePhaseItsConflictLeft(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	checkACME1Moves(t, phaseLoopsWindow(t, false), 1, backInTesting, []time.Time{clock(9, 30), clock(9, 45), clock(10, 25)})
}

// earlierLoopWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, its inputs
// reaching back to the daemon's boot at 07:00, whose one recorded conflict loop left testing.
// ACME-1, a root promoted at 07:05, moves to testing at 08:40 and stands there at the window's
// start; its tester passes it at 09:10, its reviewer sends it back to implementing at 09:40, and
// it works in implementing to the window's end. ACME-2, a root promoted at 11:00, works 15 minutes
// in each phase: its tester fails it at 12:00, and at 12:30 a conflict moves it back from testing;
// its loop is 20 minutes in implementing, and it is back in testing at 12:50 to the window's end.
// The base branch took a commit inside ACME-1's stretch, at 09:20, and one inside ACME-2's stretch
// that ends in a conflict, at 12:20: the stretches inside the window separate, so the chance
// fitted at M = 1 is 1 and at M = 0 is 0, and ACME-1's, which began in the reach and is no part of
// the fit, draws a conflict in reviewing at 09:40 under any seed.
func earlierLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(2)
	b.boot(clock(7, 0))
	rootThrough(b, "ACME-1", "a1", clock(7, 5), clock(7, 20), moveAt(7, 35, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(8, 40, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 10, phase.Reviewing, workflow.TriggerTesterPassed),
		moveAt(9, 40, phase.Implementing, workflow.TriggerReviewRejected))
	rootThrough(b, "ACME-2", "a2", clock(11, 0), clock(11, 15), moveAt(11, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(11, 45, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 0, phase.Implementing, workflow.TriggerTesterFailed),
		moveAt(12, 15, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 30, phase.Implementing, workflow.TriggerConflict),
		moveAt(12, 50, phase.Testing, workflow.TriggerImplementationReady))
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "5a8c3e1f7b2d4096d3e4f5a6b7c8d9e0f1a2b3c4", At: clock(9, 20), Branch: "main"},
		{SHA: "b1d7f3a9c5e2408be4f5a6b7c8d9e0f1a2b3c4d5", At: clock(12, 20), Branch: "main"},
	}}
	return window
}

// Where the window records only loops that left a phase before the drawn conflict's, the loop is
// drawn among them, and after it the issue goes back from the phase the loop returned it to, to
// the phase its conflict left, through copies of its own most recent work in each phase between.
// The test replays earlierLoopWindow under legion-v20.42.5: ACME-1 moves to reviewing at 09:10 as
// recorded, its stretch draws a conflict in reviewing at 09:40, and it is given ACME-2's loop, left
// from testing. ACME-1 moves back to implementing by conflict at 09:40, to testing at 10:00 after
// the loop's 20 minutes of implementing, runs a copy of its own testing, 30 minutes, to reviewing
// at 10:30, and runs its own recorded reviewing, 30 minutes, to its reviewer's send-back at 11:00.
func TestALoopFromAnEarlierPhaseTakesTheIssueBackThroughItsOwnWork(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	want := []fixtureMove{
		{From: phase.Testing, To: phase.Reviewing, Trigger: workflow.TriggerTesterPassed},
		{From: phase.Reviewing, To: phase.Implementing, Trigger: workflow.TriggerConflict},
		{From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady},
		{From: phase.Testing, To: phase.Reviewing, Trigger: workflow.TriggerTesterPassed},
		{From: phase.Reviewing, To: phase.Implementing, Trigger: workflow.TriggerReviewRejected},
	}
	checkACME1Moves(t, earlierLoopWindow(t), 1, want, []time.Time{clock(9, 10), clock(9, 40), clock(10, 0), clock(10, 30), clock(11, 0)})
}

// cyclingLoopWindow is phaseLoopsWindow with its loop from testing, to 15:00 and with no commit of
// the base branch: ACME-1 stands in testing at the window's start, its tester fails it at 09:30, and
// it works in implementing to the window's end; ACME-2's loop leaves reviewing at 10:45 and is back
// there at 11:15; ACME-3's loop leaves testing at 12:00 and is back there at 12:25. The three
// stretches inside the window, ACME-2's two and ACME-3's first, hold two conflicts and no merge, so
// every stretch draws at a chance of 2/3, ACME-1's too, which began in the reach and is no part of
// the fit; and a conflict drawn in testing is given ACME-3's loop, the window's one loop left from
// testing.
func cyclingLoopWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.reaches(2 * time.Hour)
	b.capped(3)
	b.boot(clock(7, 0))
	phaseLoopsRoots(b)
	testingConflictLoop(b)
	return b.build()
}

// A stretch whose work a drawn conflict's loop gives its issue once more draws again at that work's
// end, by a draw of its own: the place of each draw counts the draws of its stretch before it
// (stretchDraw), so the stretch after a loop is drawn as the model says, never by the draw that
// ended the one before it. The test replays cyclingLoopWindow under legion-v20.42.5, the rule it
// ran, at seed 1, where the first three draws of ACME-1's stretch are 0.37, 0.23 and 0.73 against
// its chance of 2/3. The first, at 09:30, makes a conflict: ACME-1 moves back to implementing, to
// testing at 09:55 after ACME-3's loop, and runs its own testing, 40 minutes, once more. The second,
// at 10:35, makes a conflict too, and the same loop and testing follow; the third, at 11:40, makes
// none, and ACME-1's tester fails it as recorded. A run that drew each draw after the second at the
// second's place would make a conflict at every end of that testing, and send ACME-1 round the loop
// every 65 minutes to the window's end. The test fails where ACME-1 moves otherwise or at other
// moments, where a draw's place does not count the draws its stretch made before it in the run, or
// where the replay refuses a move.
func TestAStretchAfterADrawnLoopTakesADrawOfItsOwn(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	var draws []conflictDraw
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: cyclingLoopWindow(t), Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch,
		Seed: 1, Slowdown: 1, PGDSN: replayDSN(t), Out: out, onDraw: func(draw conflictDraw) { draws = append(draws, draw) }})
	if err != nil {
		t.Fatalf("replay the window under legion-v20.42.5: %v", err)
	}
	log := replayLog(t, out)
	checkMovesAt(t, 1, "ACME-1", log, replayedMoves(log), slices.Concat(backInTesting[:2], backInTesting),
		[]time.Time{clock(9, 30), clock(9, 55), clock(10, 35), clock(11, 0), clock(11, 40)})
	before := map[stretchDraw]int{}
	for _, draw := range draws {
		t.Logf("%s: %v against %v, a conflict: %t", drawPlace(draw.at), draw.u, draw.chance, draw.conflicted)
		stretch := draw.at
		stretch.n = 0
		if draw.at.n != before[stretch] {
			t.Errorf("the run's draw %d of a stretch drew at the place %s", before[stretch]+1, drawPlace(draw.at))
		}
		before[stretch]++
	}
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("the replay refused %+v", refused)
	}
}

// The branch Merges zero sets the conflict model's merges term to zero and keeps its base and its
// phase terms (spec R2, assumption 3, plan D7): every stretch draws at the chance its phase gives
// whatever M its base took. The test reads a model with a base, a term for testing and a merges
// term, and fails where the zero branch's chance at any M from 0 to 20 is not the logistic of the
// base in merging and of the base and testing's term in testing, or where the fitted model's chance
// at an M above 0 is. It then replays the fixture window conflict-from-fitted-rate under main on the
// branch main+merges=zero: ACME-1's first stretch, which conflicts at the fitted rate (its M is 1),
// draws at the base chance, below 10⁻⁶, and does not conflict, so no merge forward is asked, no
// loop of conflict is counted, and ACME-1 goes on with its last merging segment from 11:00 and is
// done at 11:15.
func TestTheZeroBranchDrawsWithoutTheMergesTerm(t *testing.T) {
	t.Parallel()
	base, testingTerm := -1.5, 0.7
	fitted := ConflictModel{Stretches: 10, Conflicts: 4, Base: Coefficient{Estimate: base}, Term: Coefficient{Estimate: 0.9}, Level: conflictLevel,
		Phases: map[phase.Phase]PhaseFit{phase.Testing: {Term: &Coefficient{Estimate: testingTerm}}}}
	zero := fitted.WithoutTerm()
	for merges := range 21 {
		for in, want := range map[phase.Phase]float64{phase.Merging: logistic(base), phase.Testing: logistic(base + testingTerm)} {
			if got := zero.Chance(in, merges); got != want {
				t.Errorf("under the zero branch the chance in %s at M = %d is %v, want %v, its base and phase term's", in, merges, got, want)
			}
			if merges > 0 && fitted.Chance(in, merges) == zero.Chance(in, merges) {
				t.Errorf("the fitted model's chance in %s at M = %d is %v, without its merges term of 0.9", in, merges, fitted.Chance(in, merges))
			}
		}
	}

	dsn := replayDSN(t)
	branch, err := BranchNamed("main+merges=zero")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: fixtureWindow(t, "conflict-from-fitted-rate"), Rule: mainRule(t), Branch: branch,
		Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay the window on %s: %v", branch.Name, err)
	}
	if !result.Conflicts.TermZero {
		t.Errorf("the run's conflict model %s keeps its merges term on the branch %s", result.Conflicts, branch.Name)
	}
	if result.Conflicts.DrawnOwnMergeUnknown != 1 {
		t.Errorf("the run counts %d drawn stretches whose M may hold their own merge, want 1: ACME-1's first, over the 10:45 merge, "+
			"which names no pull request", result.Conflicts.DrawnOwnMergeUnknown)
	}
	if loop, counted := result.Scorecard.Loops[string(CauseConflict)]; counted {
		t.Errorf("the replay counts %+v of conflict, and on %s no stretch conflicts", loop, branch.Name)
	}
	if done := result.Scorecard.DoneAt["ACME-1"]; !done.Equal(clock(11, 15)) {
		t.Errorf("ACME-1 is done at %s, want 11:15, its last merging segment run from its stretch's end at 11:00", stamp(done))
	}
	for _, line := range replayLog(t, out) {
		if line.Msg == mergeForwardAskedLine {
			t.Errorf("the replay asked a merge forward of %s at %s, and on %s no stretch conflicts", line.Issue, stamp(line.Time), branch.Name)
		}
	}
}

// A stretch's chance of a conflict depends on how long its head waited only through the merges
// its base took meanwhile (spec R2: a rule that changes how long a branch waits changes its
// conflicts only as far as the merges term does). The test counts M over stretches of the window's
// base branch, main of acme/widgets, which took commits at 10:00 and 11:00 (and a commit of
// another branch at 10:30, which no stretch of it counts): a stretch of 20 minutes and one of
// three hours that each hold the 10:00 commit alone have M = 1 and the same chance; a stretch
// that also holds the 11:00 commit has M = 2 and another chance where the term is not zero, and
// the same under the zero branch; and a commit at a stretch's own start or end is not inside it.
func TestAConflictDependsOnTheWaitOnlyThroughMerges(t *testing.T) {
	t.Parallel()
	window := Window{Merges: map[string][]Merge{builtRepository: {
		{SHA: "1a", At: clock(10, 0), Branch: "main"}, {SHA: "2b", At: clock(10, 30), Branch: "release"}, {SHA: "3c", At: clock(11, 0), Branch: "main"},
	}}}
	merges := newBaseMerges(window, Trace{Branches: map[string]string{builtRepository: "main"}})
	model := ConflictModel{Stretches: 20, Conflicts: 5, Base: Coefficient{Estimate: -2}, Term: Coefficient{Estimate: 1.1}, Level: conflictLevel}
	for _, c := range []struct {
		name       string
		start, end time.Time
		merges     int
	}{
		{"a stretch of 20 minutes", clock(9, 50), clock(10, 10), 1},
		{"a stretch of three hours", clock(8, 0), clock(10, 59), 1},
		{"a stretch over both commits", clock(8, 0), clock(11, 30), 2},
		{"a stretch from one commit to the next", clock(10, 0), clock(11, 0), 0},
	} {
		if got, _ := merges.between(builtRepository, 0, c.start, c.end); got != c.merges {
			t.Errorf("%s from %s to %s counts M = %d, want %d", c.name, stamp(c.start), stamp(c.end), got, c.merges)
		}
	}
	merged := func(start, end time.Time) int {
		count, _ := merges.between(builtRepository, 0, start, end)
		return count
	}
	short := model.Chance(phase.Merging, merged(clock(9, 50), clock(10, 10)))
	long := model.Chance(phase.Merging, merged(clock(8, 0), clock(10, 59)))
	if short != long {
		t.Errorf("a stretch of 20 minutes draws at %v and one of three hours at %v, and both took one merge", short, long)
	}
	both := merged(clock(8, 0), clock(11, 30))
	if model.Chance(phase.Merging, both) == long {
		t.Errorf("a stretch that took two merges draws at %v, the chance of one that took one, under a merges term of 1.1", long)
	}
	if zero := model.WithoutTerm(); zero.Chance(phase.Merging, both) != zero.Chance(phase.Merging, 1) {
		t.Errorf("under the zero branch a stretch that took two merges draws at %v and one that took one at %v", zero.Chance(phase.Merging, both),
			zero.Chance(phase.Merging, 1))
	}
	if got := (ConflictModel{Stretches: 3}).Chance(phase.Merging, 5); got != 0 {
		t.Errorf("a window that records no conflict draws a chance of %v, want 0", got)
	}
	if rule := record.MainRule.OrMain(); rule.Conflict != record.ConflictMergeForward {
		t.Errorf("the rule main's conflicting head %s, want it to keep its phase while the base is merged forward", rule.Conflict)
	}
}

// pastReviewConflictWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5, a release
// before cybermonklabs/legion#4979, whose conflicting head moved back to implementing from every
// phase past the review: one conflict moved its issue back from merging, the other from accepting.
// ACME-1, a root promoted at 09:00, works 15 minutes in each phase to merging at 10:45, and a
// conflict moves it back at 11:00; its loop is 30 minutes in implementing and 15 in each phase after
// it, it is back in merging at 12:30, and it merges at 12:45. ACME-2, a root promoted at 09:05,
// works 15 minutes in each phase to accepting at 10:20, and a conflict moves it back at 10:35; its
// loop is 20 minutes in implementing and 15 in testing and in reviewing, it is back in accepting at
// 11:25, and it works 15 minutes in each phase after it to its merge at 12:10. The base branch took
// one commit, at 10:30, inside the first stretch of each issue (09:45 to 11:00 and 09:50 to 10:35)
// and inside neither's second (11:30 to 12:45 and 10:55 to 12:10): the stretches separate, so the
// chance fitted at M = 1 is 1 and at M = 0 is 0, and under any seed each issue's first stretch
// draws a conflict, ACME-1's in merging and ACME-2's in accepting, and its second draws none.
func pastReviewConflictWindow(t *testing.T) Window {
	return pastReviewConflictWindowWith(t, false)
}

// pastReviewConflictWindowWith is pastReviewConflictWindow where, with quietMerger, the record shows
// nothing of ACME-2's merger in its last visit to merging, from 11:55 to its merge at 12:10: no
// launch of the merger's claim and no turn of it.
func pastReviewConflictWindowWith(t *testing.T, quietMerger bool) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(2)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.file(clock(9, 5), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.rootThroughReview(clock(9, 0), "ACME-1")
	b.rootThroughReview(clock(9, 5), "ACME-2")
	for _, step := range []struct {
		at       time.Time
		key      string
		from, to phase.Phase
		trigger  workflow.TriggerKind
	}{
		{clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		{clock(10, 20), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		{clock(10, 30), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		{clock(10, 35), "ACME-2", phase.Accepting, phase.Implementing, workflow.TriggerConflict},
		{clock(10, 45), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		{clock(10, 55), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		{clock(11, 0), "ACME-1", phase.Merging, phase.Implementing, workflow.TriggerConflict},
		{clock(11, 10), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
		{clock(11, 25), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		{clock(11, 30), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		{clock(11, 40), "ACME-2", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		{clock(11, 45), "ACME-1", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
		{clock(11, 55), "ACME-2", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		{clock(12, 0), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		{clock(12, 10), "ACME-2", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged},
		{clock(12, 15), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		{clock(12, 30), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		{clock(12, 45), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged},
	} {
		switch {
		case quietMerger && step.key == "ACME-2" && step.to == phase.Merging:
			b.turnEnds(step.at, step.key, claim.RoleImplementer)
			b.moved(step.at, step.key, step.from, step.to, step.trigger)
		case quietMerger && step.key == "ACME-2" && step.from == phase.Merging:
			b.moved(step.at, step.key, step.from, step.to, step.trigger)
		default:
			b.segmentEnds(step.at, step.key, step.from, step.to, step.trigger)
		}
		if step.to == phase.Done {
			b.leaves(step.at, step.key, record.DoneStatus)
		}
	}
	window := b.build()
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: "2a6f0c9d4b1e3057a8c9d0e1f2a3b4c5d6e7f809", At: clock(10, 30), Branch: "main"},
	}}
	return window
}

// A conflict past the review is played by the conflict path of the rule the replay runs
// (record.SlotRule.Conflict, assumption 37). Under main the daemon the replay runs leaves it to the
// merger (cybermonklabs/legion#4979): in merging the merger merges the base forward; in accepting
// the conflict waits for merging, where the merger merges it forward, and nothing moves the issue
// back. Under legion-v20.42.5, the rule that recorded the window, the conflict moves the issue back
// to implementing from merging and from accepting, as the release that ran the rule did. The test
// replays pastReviewConflictWindow at the seeds 1 to 3. Under main ACME-1 stays in merging at
// 11:00, its merger merges the base forward for its loop's 30 minutes of implementing, the merge
// forward lands at 11:30, and its last merging segment runs from then to its merge at 11:45. ACME-2
// stays in accepting at 10:35 and goes on with the work the record shows after its loop came back
// there: its architect's 15 minutes to retro at 10:50 and its implementer's 15 to merging at 11:05,
// where its merger is asked and merges forward for its loop's 20 minutes of implementing, landing
// at 11:25, and its last merging segment runs to its merge at 11:40. Under legion-v20.42.5 each
// issue moves back and is given the one recorded loop left from its phase, its own, so it runs as
// recorded: ACME-1 moves back at 11:00 and merges at 12:45, and ACME-2 moves back at 10:35 and
// merges at 12:10. The test fails for a replay that moves either issue from 10:10 on otherwise or
// at other moments, asks or lands either's merge forward at other moments, starts the turns of
// either's merger or implementer from 10:10 on at other moments, finishes either at another time,
// refuses a move, counts other than the conflicts it left to the merger (two under main, none
// under legion-v20.42.5), or names assumption 37 otherwise than where it left one.
func TestAConflictPastTheReviewIsTheMergers(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	window := pastReviewConflictWindow(t)
	type course struct {
		moves                    []fixtureMove
		at                       []time.Time
		asked, landed            []time.Time
		mergerTurns, implementer []time.Time
		done                     time.Time
	}
	move := func(from, to phase.Phase, trigger workflow.TriggerKind) fixtureMove {
		return fixtureMove{From: from, To: to, Trigger: trigger}
	}
	accepted := move(phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved)
	signedOff := move(phase.Accepting, phase.Retro, workflow.TriggerSignOff)
	toMerging := move(phase.Retro, phase.Merging, workflow.TriggerRetroCompleted)
	merged := move(phase.Merging, phase.Done, workflow.TriggerPullRequestMerged)
	waitedForTheMerger := course{
		moves: []fixtureMove{accepted, signedOff, toMerging, merged}, at: []time.Time{clock(10, 20), clock(10, 50), clock(11, 5), clock(11, 40)},
		asked: []time.Time{clock(11, 5)}, landed: []time.Time{clock(11, 25)},
		mergerTurns: []time.Time{clock(11, 5), clock(11, 25)}, implementer: []time.Time{clock(10, 50)}, done: clock(11, 40),
	}
	for _, c := range []struct {
		rule   string
		toThem int
		want   map[string]course
	}{
		{rule: "main", toThem: 2, want: map[string]course{
			"ACME-1": {
				moves: []fixtureMove{accepted, signedOff, toMerging, merged}, at: []time.Time{clock(10, 15), clock(10, 30), clock(10, 45), clock(11, 45)},
				asked: []time.Time{clock(11, 0)}, landed: []time.Time{clock(11, 30)},
				mergerTurns: []time.Time{clock(10, 45), clock(11, 0), clock(11, 30)}, implementer: []time.Time{clock(10, 30)}, done: clock(11, 45),
			},
			"ACME-2": waitedForTheMerger,
		}},
		{rule: "legion-v20.42.5", toThem: 0, want: map[string]course{
			"ACME-1": {
				moves: []fixtureMove{accepted, signedOff, toMerging, move(phase.Merging, phase.Implementing, workflow.TriggerConflict),
					move(phase.Implementing, phase.Testing, workflow.TriggerImplementationReady), move(phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed),
					accepted, signedOff, toMerging, merged},
				at: []time.Time{clock(10, 15), clock(10, 30), clock(10, 45), clock(11, 0), clock(11, 30), clock(11, 45), clock(12, 0), clock(12, 15),
					clock(12, 30), clock(12, 45)},
				mergerTurns: []time.Time{clock(10, 45), clock(12, 30)}, implementer: []time.Time{clock(10, 30), clock(11, 0), clock(12, 15)},
				done: clock(12, 45),
			},
			"ACME-2": {
				moves: []fixtureMove{accepted, move(phase.Accepting, phase.Implementing, workflow.TriggerConflict),
					move(phase.Implementing, phase.Testing, workflow.TriggerImplementationReady), move(phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed),
					accepted, signedOff, toMerging, merged},
				at:          []time.Time{clock(10, 20), clock(10, 35), clock(10, 55), clock(11, 10), clock(11, 25), clock(11, 40), clock(11, 55), clock(12, 10)},
				mergerTurns: []time.Time{clock(11, 55)}, implementer: []time.Time{clock(10, 35), clock(11, 40)}, done: clock(12, 10),
			},
		}},
	} {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%s/seed-%d", c.rule, seed), func(t *testing.T) {
				t.Parallel()
				result, log, moved := replayLoopWindow(t, window, c.rule, seed)
				for _, key := range []string{"ACME-1", "ACME-2"} {
					want := c.want[key]
					fromTen := func(line replayLine) bool { return !line.Time.Before(clock(10, 10)) }
					at := linesAt(log, func(line replayLine) bool { return line.Msg == phaseChangedLine && line.Issue == key })
					var moves []fixtureMove
					var movedAt []time.Time
					for i, m := range moved[key] {
						if i < len(at) && !at[i].Before(clock(10, 10)) {
							moves, movedAt = append(moves, m), append(movedAt, at[i])
						}
					}
					if !slices.Equal(moves, want.moves) || !slices.EqualFunc(movedAt, want.at, time.Time.Equal) {
						t.Errorf("%s moved %s at %s from 10:10 on, want %s at %s", key, describeMoves(moves), describeTimes(movedAt),
							describeMoves(want.moves), describeTimes(want.at))
					}
					asked := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardAskedLine && line.Issue == key })
					landed := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardLandedLine && line.Issue == key })
					if !slices.EqualFunc(asked, want.asked, time.Time.Equal) || !slices.EqualFunc(landed, want.landed, time.Time.Equal) {
						t.Errorf("%s's merge forward was asked at %s and landed at %s, want asked at %s and landed at %s", key, describeTimes(asked),
							describeTimes(landed), describeTimes(want.asked), describeTimes(want.landed))
					}
					for _, role := range []struct {
						role claim.Role
						want []time.Time
					}{{claim.RoleMerger, want.mergerTurns}, {claim.RoleImplementer, want.implementer}} {
						token := string(builtToken(t, key, role.role))
						started := linesAt(log, func(line replayLine) bool {
							return line.Msg == turnStartedLine && line.Claim == token && fromTen(line)
						})
						if !slices.EqualFunc(started, role.want, time.Time.Equal) {
							t.Errorf("%s's %s started turns at %s from 10:10 on, want at %s", key, role.role, describeTimes(started), describeTimes(role.want))
						}
					}
					if done := result.Scorecard.DoneAt[key]; !done.Equal(want.done) {
						t.Errorf("%s is done at %s, want %s", key, stamp(done), stamp(want.done))
					}
				}
				if refused := result.Divergence.Refused; len(refused) > 0 {
					t.Errorf("the replay refused %+v", refused)
				}
				got, named := result.Divergence.ConflictsToMerger, slices.Contains(result.Assumptions, assumeConflictToMerger)
				if got != c.toThem || named != (c.toThem > 0) {
					t.Errorf("the run left %d conflicts past the review to the merger and named the assumptions %v, want %d, and assumption %d named: %t",
						got, result.Assumptions, c.toThem, assumeConflictToMerger, c.toThem > 0)
				}
			})
		}
	}
}

// A conflict in accepting that waits for merging is the merger's merge forward on the issue's move
// into merging, and the issue's own work in merging comes after it (mergerMergesForward,
// mergedForward). Where the record shows no launch and no turn of the merger in that visit, the
// work opens on the move into merging (Visit.Workerless), and it must still wait for the merge
// forward to land: the merger merges the base forward before anything is merged. The test replays
// pastReviewConflictWindowWith, whose ACME-2 shows no merger in its last visit to merging, under
// main at the seeds 1 to 3. ACME-2's conflict in accepting waits for merging as in
// TestAConflictPastTheReviewIsTheMergers: it reaches merging at 11:05, its merger merges the base
// forward for its loop's 20 minutes of implementing, the merge forward lands at 11:25, and the
// issue's work in merging, 15 minutes, runs from then to its merge at 11:40; the merger starts one
// turn, at 11:05, the merge forward's. It fails for a replay that opens the work in merging on the
// move at 11:05, beside the merge forward, and merges ACME-2 at 11:20, before the merge forward
// lands.
func TestAWorkerlessMergingVisitWaitsForTheMergeForwardOfAConflictThatWaitedForMerging(t *testing.T) {
	t.Parallel()
	window := pastReviewConflictWindowWith(t, true)
	for seed := int64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			result, log, moved := replayLoopWindow(t, window, "main", seed)
			var moves []fixtureMove
			var movedAt []time.Time
			at := linesAt(log, func(line replayLine) bool { return line.Msg == phaseChangedLine && line.Issue == "ACME-2" })
			for i, m := range moved["ACME-2"] {
				if i < len(at) && !at[i].Before(clock(10, 10)) {
					moves, movedAt = append(moves, m), append(movedAt, at[i])
				}
			}
			wantMoves := []fixtureMove{
				{From: phase.Reviewing, To: phase.Accepting, Trigger: workflow.TriggerReviewApproved},
				{From: phase.Accepting, To: phase.Retro, Trigger: workflow.TriggerSignOff},
				{From: phase.Retro, To: phase.Merging, Trigger: workflow.TriggerRetroCompleted},
				{From: phase.Merging, To: phase.Done, Trigger: workflow.TriggerPullRequestMerged},
			}
			wantAt := []time.Time{clock(10, 20), clock(10, 50), clock(11, 5), clock(11, 40)}
			if !slices.Equal(moves, wantMoves) || !slices.EqualFunc(movedAt, wantAt, time.Time.Equal) {
				t.Errorf("ACME-2 moved %s at %s from 10:10 on, want %s at %s", describeMoves(moves), describeTimes(movedAt),
					describeMoves(wantMoves), describeTimes(wantAt))
			}
			asked := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardAskedLine && line.Issue == "ACME-2" })
			landed := linesAt(log, func(line replayLine) bool { return line.Msg == mergeForwardLandedLine && line.Issue == "ACME-2" })
			if !slices.EqualFunc(asked, []time.Time{clock(11, 5)}, time.Time.Equal) || !slices.EqualFunc(landed, []time.Time{clock(11, 25)}, time.Time.Equal) {
				t.Errorf("ACME-2's merge forward was asked at %s and landed at %s, want asked at 11:05 and landed at 11:25", describeTimes(asked),
					describeTimes(landed))
			}
			token := string(builtToken(t, "ACME-2", claim.RoleMerger))
			turns := linesAt(log, func(line replayLine) bool {
				return line.Msg == turnStartedLine && line.Claim == token && !line.Time.Before(clock(10, 10))
			})
			if !slices.EqualFunc(turns, []time.Time{clock(11, 5)}, time.Time.Equal) {
				t.Errorf("ACME-2's merger started turns at %s from 10:10 on, want at 11:05, the merge forward's", describeTimes(turns))
			}
			if done := result.Scorecard.DoneAt["ACME-2"]; !done.Equal(clock(11, 40)) {
				t.Errorf("ACME-2 is done at %s, want %s", stamp(done), stamp(clock(11, 40)))
			}
			if refused := result.Divergence.Refused; len(refused) > 0 {
				t.Errorf("the replay refused %+v", refused)
			}
		})
	}
}

// builtStep is one end of a work segment a built window writes in time order (segmentEnds): its
// moment, its issue, the phases it moves between and its trigger; a move to done is also the issue's
// leave of the workflow.
type builtStep struct {
	at       time.Time
	key      string
	from, to phase.Phase
	trigger  workflow.TriggerKind
}

// stepThrough writes steps in their order, each the end of a work segment (segmentEnds) and, for a
// move to done, the issue's leave.
func stepThrough(b *windowBuilder, steps ...builtStep) {
	b.t.Helper()
	for _, step := range steps {
		b.segmentEnds(step.at, step.key, step.from, step.to, step.trigger)
		if step.to == phase.Done {
			b.leaves(step.at, step.key, record.DoneStatus)
		}
	}
}

// sharedDrawsWindow is a window from 09:00 to to, recorded under legion-v20.42.5 at a cap of 2, its
// inputs reaching back to the daemon's boot at 08:30, whose base branch took no commit, so each
// stretch draws at one chance. ACME-1, a root promoted at 09:00, works 15 minutes in each phase to
// its review and 30 in each phase after it: accepting at 10:15, retro at 10:45, merging at 11:15 and
// its merge at 11:45. ACME-2, a root promoted at 09:05, works 15 minutes in each phase; a conflict
// moves it back from reviewing at 10:20, its loop is 15 minutes in implementing and 15 in testing,
// it is back in reviewing at 10:50, and it merges at 11:50. ACME-3, a root handed to Legion at 09:10,
// waits for a slot to ACME-1's accepting at 10:15, the rule keeping no slot past the review; it
// works 15 minutes in each phase, its tester fails it at 11:15 and passes it at 11:45, and it merges
// at 12:45. The five recorded stretches inside the window, ACME-1's, ACME-2's two and ACME-3's two,
// hold one conflict, so each stretch draws at a chance of 0.2.
func sharedDrawsWindow(t *testing.T, to time.Time) Window {
	b := newWindowBuilder(t, clock(9, 0), to)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(2)
	b.boot(clock(8, 30))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.file(clock(9, 5), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a2", Labels: handedToLegion})
	b.file(clock(9, 10), "ACME-3", filing{Title: "Price the widgets by weight", Status: "todo", Rank: "a3", Labels: handedToLegion})
	b.rootThroughReview(clock(9, 0), "ACME-1")
	b.rootThroughReview(clock(9, 5), "ACME-2")
	stepThrough(b, builtStep{clock(10, 15), "ACME-1", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved})
	b.rootPromoted(clock(10, 15), "ACME-3")
	stepThrough(b, builtStep{clock(10, 20), "ACME-2", phase.Reviewing, phase.Implementing, workflow.TriggerConflict})
	b.rootDesigned(clock(10, 30), "ACME-3")
	stepThrough(b,
		builtStep{clock(10, 35), "ACME-2", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		builtStep{clock(10, 45), "ACME-1", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		builtStep{clock(10, 45), "ACME-3", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted},
		builtStep{clock(10, 50), "ACME-2", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
		builtStep{clock(11, 0), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		builtStep{clock(11, 5), "ACME-2", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		builtStep{clock(11, 15), "ACME-1", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		builtStep{clock(11, 15), "ACME-3", phase.Testing, phase.Implementing, workflow.TriggerTesterFailed},
		builtStep{clock(11, 20), "ACME-2", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		builtStep{clock(11, 30), "ACME-3", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		builtStep{clock(11, 35), "ACME-2", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		builtStep{clock(11, 45), "ACME-1", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged},
		builtStep{clock(11, 45), "ACME-3", phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
		builtStep{clock(11, 50), "ACME-2", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged},
		builtStep{clock(12, 0), "ACME-3", phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		builtStep{clock(12, 15), "ACME-3", phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		builtStep{clock(12, 30), "ACME-3", phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		builtStep{clock(12, 45), "ACME-3", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	return b.build()
}

// drawsUnder replays window under rule on the main branch at seed 1, and answers each draw of a
// stretch's conflict it made, in its order.
func drawsUnder(t *testing.T, window Window, rule string) []conflictDraw {
	t.Helper()
	var draws []conflictDraw
	if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, rule), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: t.TempDir(), onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
		t.Fatalf("replay the window under %s: %v", rule, err)
	}
	return draws
}

// drawPlace is the place of a stretch's draw as a test names it: the issue, the phase and the
// recorded start of the piece that ends the stretch, and the draw's count among that stretch's.
func drawPlace(at stretchDraw) string {
	return fmt.Sprintf("%s's stretch ended by its %s piece from %s, draw %d", at.issue, at.phase, at.start.UTC().Format("15:04"), at.n+1)
}

// byPlace is draws by their place (drawPlace).
func byPlace(draws []conflictDraw) map[string]conflictDraw {
	placed := make(map[string]conflictDraw, len(draws))
	for _, draw := range draws {
		placed[drawPlace(draw.at)] = draw
	}
	return placed
}

// Two rules run at one seed share the draw of each stretch both run (common random numbers): a
// stretch's draw is keyed by the run's seed, its issue and its place in the record, never by its
// place in one stream read in the order the run reaches its stretches, so a rule that moves when a
// stretch ends draws for it what any other rule draws. The test replays sharedDrawsWindow at seed 1
// under main and under keep-past-review, whose slots past the review hold ACME-3 back to the first
// merge, 90 minutes or so after main gives it ACME-1's slot at 10:15, so the two rules reach their
// stretches in another order. With the window to 18:00 every stretch ends inside it under both
// rules. The record holds five stretches, but neither replay makes ACME-2's recorded move back at
// 10:20: under both rules a conflicting head in reviewing keeps its phase while the implementer
// merges the base forward, and a stretch that draws no conflict passes over the recorded loop, so
// ACME-2's return to reviewing and its merge go on in the live stretch that drew in reviewing
// (liveStretch), which draws once. Both rules so draw four stretches, in another order, each the
// same draw and each the same conflict, ACME-2's once, in reviewing, and never in merging. With the
// window to 13:30, ACME-3's last stretch, which ends by 13:00 under main and after 13:50 under
// keep-past-review, draws under main alone, and every other stretch draws alike under both.
func TestTwoRulesAtOneSeedShareEachStretchsDraw(t *testing.T) {
	t.Parallel()
	replayDSN(t)
	places := func(draws []conflictDraw) []string {
		out := make([]string, len(draws))
		for i, draw := range draws {
			out[i] = drawPlace(draw.at)
		}
		return out
	}
	t.Run("no stretch moves", func(t *testing.T) {
		t.Parallel()
		window := sharedDrawsWindow(t, clock(18, 0))
		main, kept := drawsUnder(t, window, record.MainRule.Name), drawsUnder(t, window, "keep-past-review")
		for _, run := range []struct {
			rule  string
			draws []conflictDraw
		}{{record.MainRule.Name, main}, {"keep-past-review", kept}} {
			var phases []phase.Phase
			for _, draw := range run.draws {
				if draw.at.issue == "ACME-2" {
					phases = append(phases, draw.at.phase)
				}
			}
			if !slices.Equal(phases, []phase.Phase{phase.Reviewing}) {
				t.Errorf("under %s ACME-2 drew in %v, want once, in reviewing: its return and its merge go on in the stretch that drew there", run.rule, phases)
			}
		}
		if len(main) != 4 || len(kept) != 4 {
			t.Fatalf("main drew %v and keep-past-review %v, want four stretches each, ACME-2's return and merge in the live stretch that drew in reviewing",
				places(main), places(kept))
		}
		if slices.Equal(places(main), places(kept)) {
			t.Errorf("both rules drew the stretches in one order, %v, so the window moves no draw in the run's order", places(main))
		}
		other := byPlace(kept)
		for _, draw := range main {
			place := drawPlace(draw.at)
			k, both := other[place]
			switch {
			case !both:
				t.Errorf("%s drew under main and not under keep-past-review, which drew %v", place, places(kept))
			case k.u != draw.u || k.conflicted != draw.conflicted:
				t.Errorf("%s drew %v (a conflict: %t) under main and %v (a conflict: %t) under keep-past-review", place, draw.u, draw.conflicted,
					k.u, k.conflicted)
			}
		}
	})
	t.Run("one stretch moves", func(t *testing.T) {
		t.Parallel()
		window := sharedDrawsWindow(t, clock(13, 30))
		main, kept := byPlace(drawsUnder(t, window, record.MainRule.Name)), byPlace(drawsUnder(t, window, "keep-past-review"))
		moved := drawPlace(stretchDraw{issue: "ACME-3", start: clock(12, 30), phase: phase.Merging})
		if _, drew := main[moved]; !drew {
			t.Errorf("%s did not draw under main, which drew %v", moved, slices.Sorted(maps.Keys(main)))
		}
		for place := range kept {
			if _, both := main[place]; !both {
				t.Errorf("%s drew under keep-past-review and not under main", place)
			}
		}
		for place, draw := range main {
			k, both := kept[place]
			switch {
			case place == moved && both:
				t.Errorf("%s drew under keep-past-review too, where it ends after the window's end", place)
			case place != moved && !both:
				t.Errorf("%s drew under main and not under keep-past-review", place)
			case both && (k.u != draw.u || k.conflicted != draw.conflicted):
				t.Errorf("%s drew %v (a conflict: %t) under main and %v (a conflict: %t) under keep-past-review", place, draw.u, draw.conflicted,
					k.u, k.conflicted)
			}
		}
	})
}

// mergerConflictWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5 at a cap of 3,
// its inputs reaching back to the daemon's boot at 08:30. ACME-1, ACME-2 and ACME-3, roots
// promoted at 09:00, 09:05 and 09:10, work 15 minutes in each phase to merging at 10:45, 10:50 and
// 10:55. ACME-1's merger moves it back to implementing at 11:00, and the daemon posts the move's
// reason, which names a conflicting head; its loop is 15 minutes in each phase back to merging at
// 12:15, and it merges at 12:30. ACME-2's merger moves it back at 11:05 for red checks, which the
// daemon's post names, and its loop runs as ACME-1's, five minutes later. ACME-3's merger is asked to
// merge the base forward at 11:00 onto its conflicting head; the merge forward lands at 11:10, and
// it merges at 11:25.
func mergerConflictWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(3)
	b.boot(clock(8, 30))
	for i, key := range []string{"ACME-1", "ACME-2", "ACME-3"} {
		b.file(clock(9, 5*i), key, filing{Title: "Ship the " + key + " change", Status: "todo", Rank: fmt.Sprintf("a%d", i+1), Labels: handedToLegion})
		b.rootThroughReview(clock(9, 5*i), key)
	}
	at := func(hour, minute, i int) time.Time { return clock(hour, minute).Add(time.Duration(5*i) * time.Minute) }
	for i, key := range []string{"ACME-1", "ACME-2", "ACME-3"} {
		stepThrough(b, builtStep{at(10, 15, i), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
			builtStep{at(10, 30, i), key, phase.Accepting, phase.Retro, workflow.TriggerSignOff},
			builtStep{at(10, 45, i), key, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted})
	}
	b.info(clock(11, 0), mergeForwardAskedLine, "issue", "ACME-3", "tree", "ACME-3", "phase", string(phase.Merging), "pullRequest", 3,
		"head", "6d1c0a9e4b7f2358c0d1e2f3a4b5c6d7e8f90a1b", "base", "main", "why", "conflicting")
	for i, key := range []string{"ACME-1", "ACME-2"} {
		stepThrough(b, builtStep{at(11, 0, i), key, phase.Merging, phase.Implementing, workflow.TriggerBackward})
	}
	b.daemonPosts(clock(11, 0).Add(2*time.Second), "ACME-1",
		"Legion moved this issue from merging back to implementing at the merger's request: The head conflicts with the base, so it cannot merge until the base is merged forward.")
	b.daemonPosts(clock(11, 5).Add(2*time.Second), "ACME-2",
		"Legion moved this issue from merging back to implementing at the merger's request: The required checks are red on the head.")
	b.info(clock(11, 10), mergeForwardLandedLine, "issue", "ACME-3", "tree", "ACME-3", "pullRequest", 3)
	stepThrough(b, builtStep{clock(11, 25), "ACME-3", phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	for i, key := range []string{"ACME-1", "ACME-2"} {
		stepThrough(b, builtStep{at(11, 15, i), key, phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
			builtStep{at(11, 30, i), key, phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
			builtStep{at(11, 45, i), key, phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
			builtStep{at(12, 0, i), key, phase.Accepting, phase.Retro, workflow.TriggerSignOff},
			builtStep{at(12, 15, i), key, phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
			builtStep{at(12, 30, i), key, phase.Merging, phase.Done, workflow.TriggerPullRequestMerged})
	}
	return b.build()
}

// A merger's conflict is a conflict (plan D7): where the record shows a merger's move back out of
// merging whose reason, the daemon's post of it, names a conflicting head, or a merge forward the
// engine asked of a merger in merging, the trace counts the stretch as one that conflicted, in the
// stretches the rate is fitted on, and the move back as a round of conflict in the measures, left to
// the draw as every recorded conflict move is. A merger's move back for another reason stays a
// send-back. The test reads mergerConflictWindow: ACME-1's first stretch, which its merger's move
// back for a conflicting head ends at 11:00, and ACME-3's, in which its merger was asked a merge
// forward, conflicted; ACME-2's, which its merger ended for red checks, and each second stretch did
// not, so the model is fitted on 5 stretches with 2 conflicts. ACME-1's move back is of the cause
// conflict and left to the draw, and ACME-2's is a send-back entered by the daemon's handler of a
// backward move. The record's scorecard counts two rounds of conflict, ACME-1's loop and ACME-3's
// merge forward, and one of send_back, ACME-2's loop.
func TestAMergersSendBackForAConflictingHeadCountsAsAConflict(t *testing.T) {
	t.Parallel()
	window := mergerConflictWindow(t)
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatal(err)
	}
	merges := newBaseMerges(window, trace)
	conflicted := map[string][]bool{}
	for _, s := range recordedStretches(trace, merges) {
		conflicted[s.Issue] = append(conflicted[s.Issue], s.Conflict)
	}
	want := map[string][]bool{"ACME-1": {true, false}, "ACME-2": {false, false}, "ACME-3": {true}}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if !slices.Equal(conflicted[key], want[key]) {
			t.Errorf("%s's recorded stretches conflicted %v, want %v", key, conflicted[key], want[key])
		}
	}
	if model := windowConflicts(trace, merges); model.Stretches != 5 || model.Conflicts != 2 {
		t.Errorf("the conflict model is fitted on %d stretches with %d conflicts, want 5 with 2", model.Stretches, model.Conflicts)
	}
	for _, c := range []struct {
		key   string
		cause Cause
		way   MoveWay
	}{{"ACME-1", CauseConflict, MoveDraw}, {"ACME-2", CauseSendBack, MoveFact}} {
		var back *Move
		for _, move := range trace.Issues[c.key].Moves() {
			if move.From == phase.Merging && move.To == phase.Implementing {
				back = &move
				break
			}
		}
		if back == nil || back.Cause != c.cause || back.Source.Way != c.way {
			t.Errorf("%s's move back out of merging is %+v, want the cause %s and the way %s", c.key, back, c.cause, c.way)
		}
	}
	card, err := Score(window)
	if err != nil {
		t.Fatal(err)
	}
	if got := card.Loops[string(CauseConflict)].Rounds; got != 2 {
		t.Errorf("the record's scorecard counts %d rounds of conflict, want 2: ACME-1's loop and ACME-3's merge forward (%+v)", got, card.Loops)
	}
	if got := card.Loops[string(CauseSendBack)].Rounds; got != 1 {
		t.Errorf("the record's scorecard counts %d rounds of send_back, want 1: ACME-2's loop (%+v)", got, card.Loops)
	}
}

// Under the diagnostic branch main+conflicts=recorded a replay keeps the record's conflicts and
// draws none (assumption 44): each stretch the record ends in a conflict conflicts, made through the
// daemon's own path as a drawn one is, and runs its own loop as recorded, and each other stretch
// ends as recorded. The test replays phaseLoopsWindow with no loop from testing under
// legion-v20.42.5, the rule it ran, on that branch at seed 1. ACME-1's stretch, which draws a
// conflict at 09:30 on the main branch under any seed, ends as recorded, by its tester's failure at
// 09:30; ACME-2 moves as the record moves it, back from reviewing by conflict at 10:45 and back in
// reviewing at 11:15. No stretch draws, and a window's record runs the branch at the seed 1 alone.
func TestTheRecordedConflictsBranchKeepsTheRecordsConflicts(t *testing.T) {
	t.Parallel()
	window := phaseLoopsWindow(t, false)
	recorded := DiagnosticBranches()[0].Branch
	if !recorded.recordedConflicts() {
		t.Fatalf("the diagnostic branch %s keeps no recorded conflicts", recorded.Name)
	}
	if seeds := (windowRecord{Stretches: 3, Conflicted: 1}).seeds(recorded, SeedsPerDraw); !slices.Equal(seeds, []int64{1}) {
		t.Errorf("a window with a recorded conflict runs %s at the seeds %v, want the seed 1 alone", recorded.Name, seeds)
	}
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatal(err)
	}
	draws := 0
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: recorded, Seed: 1, Slowdown: 1,
		PGDSN: replayDSN(t), Out: out, onDraw: func(conflictDraw) { draws++ }})
	if err != nil {
		t.Fatalf("replay the window on %s: %v", recorded.Name, err)
	}
	if draws != 0 {
		t.Errorf("the run drew %d stretches, want none", draws)
	}
	log := replayLog(t, out)
	moved := replayedMoves(log)
	for _, key := range []string{"ACME-1", "ACME-2"} {
		var want []fixtureMove
		var at []time.Time
		for _, move := range trace.Issues[key].Moves() {
			if !move.At.Before(window.From) {
				want, at = append(want, fixtureMove{From: move.From, To: move.To, Trigger: move.Trigger}), append(at, move.At)
			}
		}
		checkMovesAt(t, 1, key, log, moved, want, at)
	}
	if refused := result.Divergence.Refused; len(refused) > 0 {
		t.Errorf("the replay refused %+v", refused)
	}
}

// drawRateAt is how often, over 4000 seeds, the stretch of a piece in ph that ends at end draws a
// conflict in a run of in's window, and the chance the run's draw read: one draw a seed, through
// the driver's own draw of a stretch (drawAt) at its end, every count of it started afresh. M is 0,
// the window's merges holding no commit.
func drawRateAt(t *testing.T, in *windowInputs, key string, ph phase.Phase, end time.Time) (rate, chance float64) {
	t.Helper()
	const samples = 4000
	source := rand.NewPCG(0, 0)
	d := driver{trace: in.trace, merges: in.merges, conflicts: in.conflicts, drawSource: source, draws: rand.New(source),
		stretchDraws: map[stretchDraw]int{}, lastDraw: map[string]stretchDraw{}}
	p := &piece{issue: key, phase: ph, start: end.Add(-time.Minute)}
	conflicts := 0
	for seed := range int64(samples) {
		d.cfg.Seed = seed
		clear(d.stretchDraws)
		draw := d.drawAt(p, p.start, end, false)
		chance = draw.chance
		if draw.conflicted {
			conflicts++
		}
	}
	return float64(conflicts) / samples, chance
}

// releasePartsWindow is a window from 09:00 to 15:00 recorded under legion-v20.42.5 in two release
// parts, early-release to 12:00 and late-release from it, with a cap of 20, its inputs reaching
// back to the daemon's boot at 06:00. In each part four roots are promoted on the hour, 09:00 and
// 12:00, and work 5 minutes in each phase to merging, where their stretches end 5 minutes in, at
// 09:40 and at 12:40. One stretch of the early part and three of the late part end in a conflict,
// whose issue implements to the window's end; the others merge. The window's merges hold no commit
// of the base branch, so M is 0 everywhere, and the fit gives each part the share of its merging
// stretches that conflicted: 0.25 in the early part and 0.75 in the late part.
func releasePartsWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.reaches(3 * time.Hour)
	b.capped(20)
	b.boot(clock(6, 0))
	early := &b.window.Parts[0]
	early.Release, early.To, early.Rule = "early-release", clock(12, 0), "legion-v20.42.5"
	late := *early
	late.From, late.To, late.Release = clock(12, 0), clock(15, 0), "late-release"
	b.window.Parts = append(b.window.Parts, late)
	for part := range 2 {
		for i := range 4 {
			key := fmt.Sprintf("ACME-%d", 1+4*part+i)
			at := func(minute int) time.Time { return clock(9+3*part, minute) }
			to, trigger := phase.Done, workflow.TriggerPullRequestMerged
			if i < 1+2*part {
				to, trigger = phase.Implementing, workflow.TriggerConflict
			}
			rootThrough(b, key, fmt.Sprintf("a%d", 1+4*part+i), at(0), at(5), builtMove{at(10), phase.Implementing, workflow.TriggerPlannerCompleted},
				builtMove{at(15), phase.Testing, workflow.TriggerImplementationReady}, builtMove{at(20), phase.Reviewing, workflow.TriggerTesterPassed},
				builtMove{at(25), phase.Accepting, workflow.TriggerReviewApproved}, builtMove{at(30), phase.Retro, workflow.TriggerSignOff},
				builtMove{at(35), phase.Merging, workflow.TriggerRetroCompleted}, builtMove{at(40), to, trigger})
			if to == phase.Done {
				b.leaves(at(40), key, record.DoneStatus)
			}
		}
	}
	return b.build()
}

// The conflict model fits merging's rate for each release part of the window (assumption 3): a
// release that posts the merger's conflicts records them as conflicts, and one that does not
// records them as send-backs, so one rate over both parts counts the earlier part's conflicts twice
// and the later part's half. A stretch that ends in merging draws at the rate of the part its end
// falls in, under every rule, and the run and the comparison print each part's rate. The test fits
// releasePartsWindow: the fit names both releases, and a stretch ending in merging at 09:40 draws at
// 0.25 and one ending at 12:40 at 0.75, by the chance the draw reads and by its count over 4000
// seeds, within 0.04 (about six standard deviations of 4000 draws at 0.25). It then replays the
// window under legion-v20.42.5 at the seeds 1 to 3: each draw of a stretch that ends at 09:40 reads
// 0.25, and each draw of one that ends at 12:40 reads 0.75. It fails for one rate over both parts,
// for which every draw reads 0.5.
func TestTheMergingRateIsFittedForEachReleasePart(t *testing.T) {
	t.Parallel()
	window := releasePartsWindow(t)
	in, err := newWindowInputs(window)
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range []string{"early-release", "late-release"} {
		if !strings.Contains(in.conflicts.terms(), release) {
			t.Errorf("the conflict model's terms name no %s: %s", release, in.conflicts.terms())
		}
	}
	for _, c := range []struct {
		key  string
		end  time.Time
		want float64
	}{{"ACME-2", clock(9, 40), 0.25}, {"ACME-8", clock(12, 40), 0.75}} {
		rate, chance := drawRateAt(t, in, c.key, phase.Merging, c.end)
		if math.Abs(chance-c.want) > 1e-6 || math.Abs(rate-c.want) > 0.04 {
			t.Errorf("a stretch ending in merging at %s draws at a chance of %.6f and conflicts in %.3f of 4000 seeds, want %.2f", stamp(c.end), chance,
				rate, c.want)
		}
	}
	for seed := int64(1); seed <= 3; seed++ {
		var draws []conflictDraw
		if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch, Seed: seed,
			Slowdown: 1, PGDSN: replayDSN(t), Out: t.TempDir(), onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
			t.Fatal(err)
		}
		if len(draws) == 0 {
			t.Errorf("at seed %d no stretch drew", seed)
		}
		for _, draw := range draws {
			want := 0.25
			if !draw.at.start.Before(clock(12, 0)) {
				want = 0.75
			}
			if math.Abs(draw.chance-want) > 1e-6 {
				t.Errorf("at seed %d the stretch of %s from %s draws at %.6f, want %.2f", seed, draw.at.issue, stamp(draw.at.start), draw.chance, want)
			}
		}
	}
}

// bootSpanWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5 with a cap of 20,
// its inputs reaching back to the daemon's boot at 06:00; a daemon process stops and the next
// starts at 10:30. Sixteen roots work 5 minutes in planning and in implementing: ACME-1 to ACME-8,
// promoted at 09:00, move into testing at 09:15 and end their stretch at 09:40, before the boot,
// and ACME-9 to ACME-16, promoted at 10:00, move into testing at 10:15 and end it at 10:40, after
// the boot. A conflict ends the stretches of two of the first eight and of six of the second, a
// tester's failure the others, and each issue is back in testing 10 minutes later, to the window's
// end. The window's merges hold no commit of the base branch, so M is 0, and the fit gives testing
// a chance of 0.25 for a stretch that spans no boot and 0.75 for one that spans the boot.
func bootSpanWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.capped(20)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(6, 0))
	b.stopped(clock(10, 30))
	b.boot(clock(10, 30))
	for group := range 2 {
		for i := range 8 {
			key := fmt.Sprintf("ACME-%d", 1+8*group+i)
			at := func(minute int) time.Time { return clock(9+group, minute) }
			trigger := workflow.TriggerTesterFailed
			if i < 2+4*group {
				trigger = workflow.TriggerConflict
			}
			rootThrough(b, key, fmt.Sprintf("a%02d", 1+8*group+i), at(0), at(5), builtMove{at(10), phase.Implementing, workflow.TriggerPlannerCompleted},
				builtMove{at(15), phase.Testing, workflow.TriggerImplementationReady}, builtMove{at(40), phase.Implementing, trigger},
				builtMove{at(50), phase.Testing, workflow.TriggerImplementationReady})
		}
	}
	return b.build()
}

// A boot re-reads each pull request (assumption 3), so the conflict model fits, for each phase, a
// chance of its own for a stretch that spans a recorded boot: a stretch the replay has under way at a
// boot draws there, once, at that chance, and conflicts at the boot; a stretch that spans no boot
// draws at its end at the other chance. The test fits bootSpanWindow: a testing stretch that ends at
// 09:40 with no boot draws at 0.25, by the chance its draw reads and by its count over 4000 seeds,
// within 0.04. It then replays the window under legion-v20.42.5 at the seeds 1 to 10: each first
// draw of a stretch of the first group reads 0.25, and one that conflicts moves its issue back at
// 09:40, its end; each first draw of a stretch of the second group reads 0.75, and one that
// conflicts moves its issue back at 10:30, the boot. Of the 80 draws of each group, those that
// conflict number from 8 to 32 in the first group and from 48 to 72 in the second, each about three
// standard deviations each side of 20 and of 60. It fails where a boot keeps the chance and moves
// only the moment, for which every draw reads 0.5.
func TestAStretchSpanningABootDrawsThereAtItsOwnChance(t *testing.T) {
	t.Parallel()
	window := bootSpanWindow(t)
	in, err := newWindowInputs(window)
	if err != nil {
		t.Fatal(err)
	}
	if rate, chance := drawRateAt(t, in, "ACME-1", phase.Testing, clock(9, 40)); math.Abs(chance-0.25) > 1e-6 || math.Abs(rate-0.25) > 0.04 {
		t.Errorf("a testing stretch that spans no boot draws at a chance of %.6f and conflicts in %.3f of 4000 seeds, want 0.25", chance, rate)
	}
	groups := []struct {
		start, back time.Time
		chance      float64
		low, high   int
	}{{clock(9, 15), clock(9, 40), 0.25, 8, 32}, {clock(10, 15), clock(10, 30), 0.75, 48, 72}}
	conflicted := make([]int, len(groups))
	drawn := make([]int, len(groups))
	for seed := int64(1); seed <= 10; seed++ {
		var draws []conflictDraw
		out := t.TempDir()
		if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: MainBranch, Seed: seed,
			Slowdown: 1, PGDSN: replayDSN(t), Out: out, onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
			t.Fatal(err)
		}
		log := replayLog(t, out)
		for _, draw := range draws {
			g := -1
			for i, group := range groups {
				if draw.at.start.Equal(group.start) && draw.at.n == 0 {
					g = i
				}
			}
			if g < 0 {
				continue
			}
			drawn[g]++
			if math.Abs(draw.chance-groups[g].chance) > 1e-6 {
				t.Errorf("at seed %d the stretch of %s from %s draws at %.6f, want %.2f", seed, draw.at.issue, stamp(draw.at.start), draw.chance,
					groups[g].chance)
			}
			if !draw.conflicted {
				continue
			}
			conflicted[g]++
			back := linesAt(log, func(line replayLine) bool {
				return line.Msg == phaseChangedLine && line.Issue == draw.at.issue && line.Trigger == workflow.TriggerConflict
			})
			if len(back) == 0 || !back[0].Equal(groups[g].back) {
				t.Errorf("at seed %d %s conflicted first at %s, want at %s", seed, draw.at.issue, describeTimes(back), stamp(groups[g].back))
			}
		}
	}
	for g, group := range groups {
		if drawn[g] != 80 || conflicted[g] < group.low || conflicted[g] > group.high {
			t.Errorf("the stretches from %s drew %d times and conflicted %d times, want 80 draws and from %d to %d conflicts", stamp(group.start),
				drawn[g], conflicted[g], group.low, group.high)
		}
	}
}

// A conflict drawn in a phase is given the work of a loop drawn among every recorded loop that left
// that phase and ends before the run-out's end, the record's reach before the window's start among
// them (recordedLoops), with no length the record does not show. The test reads the loops of a
// window from 09:00 to 13:00 with a run-out of two hours, its inputs reaching back to the daemon's
// boot at 06:00: ACME-1, a root promoted at 06:05, moves into testing at 07:00, a conflict moves it
// back at 07:10, and it is in testing again at 08:10, an hour later, all before the window's start;
// ACME-2, a root promoted at 11:00, moves into testing at 12:00, a conflict moves it back at 12:30,
// and it is in testing again at 14:00, in the run-out, an hour and a half later; a second conflict at
// 14:30 leaves it implementing to the end. The loops that left testing and end are the hour and the
// hour and a half. It fails where the pool leaves out the reach's loop.
func TestTheLoopPoolHoldsEveryLoopThatEndsBeforeTheRunOutsEnd(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.window.After = Duration(2 * time.Hour)
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(6, 50, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(7, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(7, 10, phase.Implementing, workflow.TriggerConflict),
		moveAt(8, 10, phase.Testing, workflow.TriggerImplementationReady), moveAt(8, 20, phase.Implementing, workflow.TriggerTesterFailed))
	rootThrough(b, "ACME-2", "a2", clock(11, 0), clock(11, 15), moveAt(11, 30, phase.Implementing, workflow.TriggerPlannerCompleted),
		moveAt(12, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(12, 30, phase.Implementing, workflow.TriggerConflict),
		moveAt(14, 0, phase.Testing, workflow.TriggerImplementationReady), moveAt(14, 30, phase.Implementing, workflow.TriggerConflict))
	in, err := newWindowInputs(b.build())
	if err != nil {
		t.Fatal(err)
	}
	s, err := buildScript(in.trace, in.events, in.lines)
	if err != nil {
		t.Fatal(err)
	}
	var lengths []time.Duration
	for _, loop := range recordedLoops(s) {
		if loop.left == phase.Testing && loop.endsIn(phase.Testing) {
			lengths = append(lengths, loop.round.recorded())
		}
	}
	if want := []time.Duration{time.Hour, 90 * time.Minute}; !slices.Equal(lengths, want) {
		t.Errorf("the loops that left testing and end give their issues work of %v, want %v", lengths, want)
	}
}
