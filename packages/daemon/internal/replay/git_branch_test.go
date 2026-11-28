package replay

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

func TestGitReadUsesExactlyTheVerdictPrefix(t *testing.T) {
	t.Parallel()
	head := GitHead{SHA: "head", Tip: "c", Paths: []string{"stock.go"}, Verdicts: []GitVerdict{{Base: "a", Conflict: true}, {Base: "b"}}}
	pull := GitPull{Number: 11, Merge: "own", Base: "main", Heads: []GitHead{head}}
	in := GitInput{Pulls: []GitPull{pull}, Commits: []GitCommit{{SHA: "a", Paths: head.Paths}, {SHA: "b", Paths: head.Paths}, {SHA: "c", Paths: head.Paths}}}
	for _, test := range []struct {
		name, want string
		shas       []string
		n          int
		pr         int
		drawn      bool
	}{
		{name: "zero", want: "clean", pr: 11},
		{name: "one", shas: []string{"a"}, want: "conflict", n: 1, pr: 11},
		{name: "two", shas: []string{"a", "b"}, want: "clean", n: 2, pr: 11},
		{name: "reordered", shas: []string{"b", "a"}, want: "order", pr: 11},
		{name: "past own merge", shas: []string{"c", "b"}, want: "after-merge", pr: 11},
		{name: "missing before after-merge", shas: []string{"c", "unknown"}, want: "missing", pr: 11},
		{name: "drawn before missing", shas: []string{"unknown"}, want: "never-ran", pr: 11, drawn: true},
		{name: "no recorded PR", want: "never-ran"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var commits []landedCommit
			for _, sha := range test.shas {
				commits = append(commits, landedCommit{sha: sha, at: clock(10, 0)})
			}
			r := newGitRepository("main", in, []Merge{{SHA: "a"}, {SHA: "b"}, {SHA: "own"}, {SHA: "c"}, {SHA: "unknown"}})
			got := r.read(test.pr, test.drawn, clock(11, 0), time.Time{}, commits)
			if got.kind != test.want || got.n != test.n {
				t.Errorf("read = %+v, want %s (n=%d)", got, test.want, test.n)
			}
			var counts GitReadCounts
			counts.add(got.kind)
			for _, kind := range gitReadKinds {
				want := 0
				if kind == test.want {
					want = 1
				}
				if counts[kind] != want {
					t.Errorf("%s count = %d, want %d", kind, counts[kind], want)
				}
			}
		})
	}
}

func TestGitStandingHeadPrefersAForcePushAtTheSameSecond(t *testing.T) {
	t.Parallel()
	at := clock(10, 0)
	pull := GitPull{Heads: []GitHead{{SHA: "old", At: clock(9, 0)}, {SHA: "a", At: at, From: GitHeadForcePush}, {SHA: "b", At: at, From: GitHeadRead}, {SHA: "c", At: at, From: GitHeadCommit}, {SHA: "later", At: clock(11, 0)}}}
	if got := pull.standingHead(at); got.SHA != "a" {
		t.Errorf("standing head = %s, want force-push a", got.SHA)
	}
}

func TestGitReadsCheckTheMostRecentBaseChange(t *testing.T) {
	t.Parallel()
	in := GitInput{Pulls: []GitPull{{Number: 11, Base: "main", BaseChanges: []time.Time{clock(9, 0), clock(10, 0)},
		Heads: []GitHead{{SHA: "head", At: clock(8, 0), Tip: "tip"}}}, {Number: 12, Base: "release", Heads: []GitHead{{SHA: "head", At: clock(8, 0), Tip: "tip"}}}}}
	r := newGitRepository("main", in, nil)
	for _, test := range []struct {
		pr   int
		at   time.Time
		want string
	}{{11, clock(9, 30), "missing"}, {11, clock(10, 0), "clean"}, {11, clock(11, 0), "clean"}, {12, clock(9, 30), "missing"}, {12, clock(11, 0), "missing"}} {
		if got := r.read(test.pr, false, test.at, time.Time{}, nil); got.kind != test.want {
			t.Errorf("#%d at %s = %s, want %s", test.pr, test.at, got.kind, test.want)
		}
	}
}

func TestGitTouchingMergesIgnoreOtherPaths(t *testing.T) {
	t.Parallel()
	in := GitInput{Commits: []GitCommit{{SHA: "a", Paths: []string{"stock.go"}}, {SHA: "b", Paths: []string{"billing.go"}}}}
	r := newGitRepository("main", in, []Merge{{SHA: "a"}, {SHA: "b"}})
	got, missing := r.touching([]landedCommit{{sha: "a"}, {sha: "b"}}, []string{"stock.go"}, "", "", time.Time{})
	if missing || len(got) != 1 || got[0].sha != "a" {
		t.Errorf("touching = %v, missing = %t; want a alone", got, missing)
	}
}

func TestGitGeneratorChanceIncreasesWithTouchingMerges(t *testing.T) {
	t.Parallel()
	m := ConflictModel{Stretches: 4, Conflicts: 2, Base: Coefficient{Estimate: -2}, Term: Coefficient{Estimate: 1}}
	if zero, one, two := m.profileChance(phase.Merging, 0, clock(10, 0), false), m.profileChance(phase.Merging, 1, clock(10, 0), false), m.profileChance(phase.Merging, 2, clock(10, 0), false); zero != 0 || !(zero < one && one < two) {
		t.Errorf("profile chances at M = 0, 1, 2: %v, %v, %v; want zero then increasing", zero, one, two)
	}
}

func TestDrawnWorkUsesItsSourcesProfile(t *testing.T) {
	t.Parallel()
	p := clonePieces([]*piece{{issue: "ACME-1"}}, "ACME-2")[0]
	d := driver{trace: Trace{Issues: map[string]*IssueTrace{
		"ACME-1": {Repo: builtRepository, LinkedPullRequests: map[string]int{builtRepository: 11}},
		"ACME-2": {Repo: builtRepository},
	}}}
	r := newGitRepository("main", GitInput{Pulls: []GitPull{{Number: 11, Heads: []GitHead{{Tip: "a", Paths: []string{"stock.go"}}}}}}, nil)
	if got := d.drawProfile(p, r, clock(10, 0)); got.MergeBase != "" || !slices.Equal(got.Paths, []string{"stock.go"}) {
		t.Errorf("drawn work profile = %+v, want stock.go without a merge base", got)
	}
}

func TestDrawnWorkKeepsOrRedrawsItsSourcesConflict(t *testing.T) {
	t.Parallel()
	window := fixtureWindow(t, "drawn-work-conflict")
	for _, test := range []struct {
		branch string
		rounds int
	}{{"main+neverStarted=drawn", 2}, {"main+conflicts=drawn+neverStarted=drawn", 1}} {
		t.Run(test.branch, func(t *testing.T) {
			t.Parallel()
			branch, err := BranchNamed(test.branch)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: branch, Seed: 1, Slowdown: 1, PGDSN: replayDSN(t), Out: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if rounds := got.Scorecard.Loops[string(CauseConflict)].Rounds; rounds != test.rounds {
				t.Errorf("conflict rounds = %d, want %d", rounds, test.rounds)
			}
		})
	}
}

// reviewLoopGitWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5 whose
// conflicting head moves back to implementing, with git input for acme/widgets. ACME-1, a root
// promoted at 09:00 that links acme/widgets#11, works 15 minutes in each phase to reviewing at
// 10:00, and a conflict moves it back at 10:15: its loop is implementing to 10:30 and testing to
// its return to reviewing at 10:45, and it then works 15 minutes in each phase to its merge at
// 11:45. Its first stretch runs from 09:45 to 10:15 and its second from 10:30 to 11:45. Its pull
// request's first head stands from 09:40, its merge base a commit before the window; its second
// stands from 10:25, in the loop's implementing round, its merge base the commit of 10:10. Both
// change a.go alone. Main takes three commits of other pull requests that change a.go, at 09:50,
// 10:10 and 10:40, and ACME-1's own merge at 11:45. So the fit counts M = 2 in the first stretch,
// which conflicted, and M = 1 in the second (the commit of 10:40, after the second head's merge
// base), which merged: the stretches separate, and the chance fitted at M = 2 is 1 and at M = 1 is
// 0.
func reviewLoopGitWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.capped(2)
	b.boot(clock(9, 0))
	b.file(clock(9, 0), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootThroughReview(clock(9, 0), "ACME-1")
	for _, step := range []struct {
		at       time.Time
		from, to phase.Phase
		trigger  workflow.TriggerKind
	}{
		{clock(10, 15), phase.Reviewing, phase.Implementing, workflow.TriggerConflict},
		{clock(10, 30), phase.Implementing, phase.Testing, workflow.TriggerImplementationReady},
		{clock(10, 45), phase.Testing, phase.Reviewing, workflow.TriggerTesterPassed},
		{clock(11, 0), phase.Reviewing, phase.Accepting, workflow.TriggerReviewApproved},
		{clock(11, 15), phase.Accepting, phase.Retro, workflow.TriggerSignOff},
		{clock(11, 30), phase.Retro, phase.Merging, workflow.TriggerRetroCompleted},
		{clock(11, 45), phase.Merging, phase.Done, workflow.TriggerPullRequestMerged},
	} {
		b.segmentEnds(step.at, "ACME-1", step.from, step.to, step.trigger)
	}
	b.leaves(clock(11, 45), "ACME-1", record.DoneStatus)
	window := linkedPullRequest(t, b.build(), "ACME-1", "https://github.com/acme/widgets/pull/11")
	const (
		at0950 = "d1e3f5a7b9c0d2e4f6a8b0c2d4e6f8a0b1c3d5e7"
		at1010 = "e2f4a6b8c0d1e3f5a7b9c1d3e5f7a9b1c2d4e6f8"
		at1040 = "f3a5b7c9d1e2f4a6b8c0d2e4f6a8b0c2d3e5f7a9"
		own    = "a4b6c8d0e2f3a5b7c9d1e3f5a7b9c1d3e4f6a8b0"
	)
	window.Merges = map[string][]Merge{builtRepository: {
		{SHA: at0950, At: clock(9, 50), Branch: "main", PR: 20},
		{SHA: at1010, At: clock(10, 10), Branch: "main", PR: 21},
		{SHA: at1040, At: clock(10, 40), Branch: "main", PR: 22},
		{SHA: own, At: clock(11, 45), Branch: "main", PR: 11},
	}}
	paths := []string{"a.go"}
	window.Git = map[string]GitInput{builtRepository: {
		Commits: []GitCommit{{SHA: at0950, Paths: paths}, {SHA: at1010, Paths: paths}, {SHA: at1040, Paths: paths}, {SHA: own, Paths: paths}},
		Pulls: []GitPull{{Number: 11, Merge: own, Base: "main", Heads: []GitHead{
			{SHA: "b5c7d9e1f3a4b6c8d0e2f4a6b8c0d2e4f5a7b9c1", At: clock(9, 40), From: GitHeadCommit, Tip: at1040,
				MergeBase: "c6d8e0f2a4b5c7d9e1f3a5b7c9d1e3f5a6b8c0d2", Paths: paths},
			{SHA: "d7e9f1a3b5c6d8e0f2a4b6c8d0e2f4a6b7c9d1e3", At: clock(10, 25), From: GitHeadCommit, Tip: at1040, MergeBase: at1010, Paths: paths},
		}}},
	}}
	return window
}

// After a conflict loop whose work the replay drew, under a rule that moves the issue back, the
// issue's head holds the base as it stood when the loop's implementing round ended, as the record's
// head after a conflict loop holds the base merged in that round, so a later stretch counts M from
// there, as the fit counts it from that head's merge base: one count of M in the fit and the draw
// (spec R2). The test replays reviewLoopGitWindow under legion-v20.42.5 on the branch that draws
// every conflict, at seed 1. ACME-1's first stretch draws at 10:15 with M = 2 and conflicts; the
// loop drawn for it is its own, the one recorded loop that left reviewing, whose implementing round
// ends at 10:30; and its second stretch draws at its merge at 11:45 with M = 1, the commit of 10:40,
// which landed after the round's end and before the loop's return to reviewing at 10:45, and draws
// no conflict. It fails for a draw that counts from the loop's return, M = 0, or a fit that counts
// other than M = 2 and M = 1.
func TestAStretchAfterADrawnLoopCountsMergesFromItsImplementingRound(t *testing.T) {
	t.Parallel()
	window := reviewLoopGitWindow(t)
	trace, err := BuildTrace(window)
	if err != nil {
		t.Fatalf("build the window's trace: %v", err)
	}
	if model := windowConflicts(trace, newBaseMerges(window, trace)); model.Stretches != 2 || model.Conflicts != 1 || model.NoTouchStretches != 0 {
		t.Fatalf("the model is fitted on %d stretches, %d of them conflicts, and leaves out %d with no touching merge; want 2, 1 and 0",
			model.Stretches, model.Conflicts, model.NoTouchStretches)
	}
	var draws []conflictDraw
	if _, err := Run(context.Background(), RunConfig{Window: window, Rule: replayedRule(t, "legion-v20.42.5"), Branch: drawnConflictBranch, Seed: 1,
		Slowdown: 1, PGDSN: replayDSN(t), Out: t.TempDir(), onDraw: func(draw conflictDraw) { draws = append(draws, draw) }}); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		at         time.Time
		merges     int
		conflicted bool
	}{{clock(10, 15), 2, true}, {clock(11, 45), 1, false}}
	if len(draws) != len(want) {
		t.Fatalf("the run drew %d times, want %d", len(draws), len(want))
	}
	for i, w := range want {
		if got := draws[i]; !got.drawn.Equal(w.at) || got.merges != w.merges || got.conflicted != w.conflicted {
			t.Errorf("draw %d is at %s with M = %d, a conflict: %t; want at %s with M = %d, a conflict: %t", i+1, stamp(got.drawn), got.merges,
				got.conflicted, stamp(w.at), w.merges, w.conflicted)
		}
	}
}

// noProfileWindow is a window from 09:00 to 13:00 recorded under legion-v20.42.5 whose base branch,
// main of acme/widgets, has git input and takes no commit, and whose four roots link no pull request,
// so none of their stretches has a profile. ACME-1 to ACME-4 are promoted at 09:00 and work 5
// minutes in planning and 5 in implementing, to testing at 09:15; a conflict moves ACME-1 and ACME-2
// back to implementing at 09:40, a tester's failure ACME-3 and ACME-4, and each implements to the
// window's end. Every stretch has M = 0.
func noProfileWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.window.Parts[0].Rule = "legion-v20.42.5"
	b.boot(clock(9, 0))
	for i, key := range []string{"ACME-1", "ACME-2", "ACME-3", "ACME-4"} {
		back := workflow.TriggerTesterFailed
		if i < 2 {
			back = workflow.TriggerConflict
		}
		rootThrough(b, key, "a"+key[len(key)-1:], clock(9, 0), clock(9, 5), moveAt(9, 10, phase.Implementing, workflow.TriggerPlannerCompleted),
			moveAt(9, 15, phase.Testing, workflow.TriggerImplementationReady), moveAt(9, 40, phase.Implementing, back))
	}
	window := b.build()
	window.Git = map[string]GitInput{builtRepository: {}}
	return window
}

// An issue with no profile, whose pull request the record does not hold, draws at the rate fitted
// on the window's stretches that have no profile, with M every commit its base took since the
// stretch's start, so a stretch whose base took no commit draws at that rate, not at the chance of
// 0 that the fit of the stretches with a profile gives one with no touching merge, which it never
// saw a stretch with no profile at (spec R2). The test fits noProfileWindow, whose four stretches
// have no profile and M = 0, two of them conflicts, and draws ACME-1's stretch at its end through
// the driver's own draw (drawAt): M = 0, and the chance 2/4 = 0.5, and the run names the
// assumption the draw rests on (52). It fails for a draw at a chance of 0.
func TestAnIssueWithNoProfileDrawsAtTheRateOfTheStretchesWithNone(t *testing.T) {
	t.Parallel()
	in, err := newWindowInputs(noProfileWindow(t))
	if err != nil {
		t.Fatal(err)
	}
	source := rand.NewPCG(0, 0)
	d := driver{trace: in.trace, merges: in.merges, conflicts: in.conflicts, drawSource: source, draws: rand.New(source),
		stretchDraws: map[stretchDraw]int{}, lastDraw: map[string]stretchDraw{}, used: map[int]bool{}}
	d.setupGit()
	draw := d.drawAt(&piece{issue: "ACME-1", phase: phase.Testing, start: clock(9, 15)}, clock(9, 15), clock(9, 40), false)
	t.Logf("the conflict model: %s", in.conflicts)
	if draw.merges != 0 || math.Abs(draw.chance-0.5) > 1e-6 {
		t.Errorf("ACME-1's stretch draws with M = %d at a chance of %.6f, want M = 0 at 0.5", draw.merges, draw.chance)
	}
	if !d.used[assumeNoProfileRate] {
		t.Errorf("the draw with no profile names no assumption %d", assumeNoProfileRate)
	}
}
