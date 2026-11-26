package replay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
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
