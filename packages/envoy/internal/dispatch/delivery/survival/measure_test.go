package survival_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// added is a patch that adds count lines to the new file path.
func added(path string, count int) []survival.FileDiff {
	return []survival.FileDiff{{NewPath: path, Status: survival.StatusAdded,
		Hunks: []survival.Hunk{{OldStart: 1, NewStart: 1, Added: count}}}}
}

// removed is a patch that removes count lines of path from its line from.
func removed(path string, from, count int) []survival.FileDiff {
	return []survival.FileDiff{{OldPath: path, NewPath: path, Status: survival.StatusModified,
		Hunks: []survival.Hunk{{OldStart: from, Removed: count, NewStart: from}}}}
}

// sha is a 40-character SHA of its own for each n.
func sha(n int) string { return fmt.Sprintf("%040x", n+1) }

// step is one commit chainOf reads: its time, its facts and its patch.
type step struct {
	at      time.Time
	facts   survival.ChangeFacts
	patches []survival.FileDiff
}

// chainOf is the chain of steps, each commit with a SHA of its own (sha of its index).
func chainOf(steps ...step) replayed {
	var r replayed
	for index, commit := range steps {
		r.chain = append(r.chain, survival.Commit{SHA: sha(index), CommittedAt: commit.at, Change: commit.facts})
		r.patches = append(r.patches, commit.patches)
	}
	return r
}

// sortedByChain is changes in the order of their commits in r's chain.
func sortedByChain(r replayed, changes []survival.Change) []survival.Change {
	index := map[string]int{}
	for i, commit := range r.chain {
		index[commit.SHA] = i
	}
	out := slices.Clone(changes)
	slices.SortFunc(out, func(a, b survival.Change) int { return index[a.SHA] - index[b.SHA] })
	return out
}

// pairsByChain is pairs in the order of their adders' and then their removers' commits in r's
// chain.
func pairsByChain(r replayed, pairs []survival.Pair) []survival.Pair {
	index := map[string]int{}
	for i, commit := range r.chain {
		index[commit.SHA] = i
	}
	out := slices.Clone(pairs)
	slices.SortFunc(out, func(a, b survival.Pair) int {
		if d := index[a.AdderSHA] - index[b.AdderSHA]; d != 0 {
			return d
		}
		return index[a.RemoverSHA] - index[b.RemoverSHA]
	})
	return out
}

func TestComputeAnswersTheSameOutputTwice(t *testing.T) {
	t.Parallel()
	repo, facts, cohort := buildControlRepo(t)
	h := synthetic(3, 70)
	runs := []struct {
		name string
		read func() replayed
		days []time.Time
	}{
		{"the control repository, its patches read from git each time", func() replayed { return replay(t, repo, facts) }, days(1, 1)},
		{"a synthetic branch", func() replayed { return h }, days(0, 27)},
	}
	for _, run := range runs {
		first, second := run.read().measure(t, run.days), run.read().measure(t, run.days)
		if len(first.Changes) == 0 || len(first.Fingerprints) == 0 || len(first.Days) == 0 {
			t.Fatalf("%s: measured %d changes, %d fingerprints and %d days, want some", run.name,
				len(first.Changes), len(first.Fingerprints), len(first.Days))
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("%s: the two measurements differ:\n%+v\n%+v", run.name, first, second)
		}
	}
	if got := changeOf(t, runs[0].read().measure(t, days(1, 1)), cohort); got.Added != 65 {
		t.Errorf("the control repository's cohort reads %d added, want 65", got.Added)
	}
}

// roundTripFunc is an http.RoundTripper of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestTheReplayRunsWithNoNetworkAndNoGit(t *testing.T) {
	// Not parallel: it sets PATH (t.Setenv) and replaces http.DefaultTransport, which it puts back.
	r, cohort := controlRepo(t)

	t.Setenv("PATH", t.TempDir())
	if path, err := exec.LookPath("git"); err == nil {
		t.Fatalf("git is still found, at %s", path)
	}
	var asked atomic.Int64
	saved := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		asked.Add(1)
		return nil, errors.New("this test allows no network")
	})
	t.Cleanup(func() { http.DefaultTransport = saved })
	if response, err := http.Get("https://api.github.com/"); err == nil {
		response.Body.Close()
		t.Fatal("a request reached the network")
	}
	if asked.Swap(0) != 1 {
		t.Fatal("the failing transport saw no request: it is not the one requests take")
	}

	change := changeOf(t, r.measure(t, days(1, 1)), cohort)
	if change.Added != 65 || change.AliveAtMark != 24 {
		t.Errorf("the cohort reads %d added and %d alive, want 65 and 24", change.Added, change.AliveAtMark)
	}
	if got := asked.Load(); got != 0 {
		t.Errorf("the measurement made %d requests", got)
	}
}

func TestAMeasurementIsTheSameWhateverRunMeasuresIt(t *testing.T) {
	t.Parallel()
	h := synthetic(7, 84+40)
	one := h.measure(t, days(0, 83))
	nonPR, dead := 0, 0
	for _, change := range one.Changes {
		nonPR += change.RemovedByNonPR
		if change.AliveAtMark < change.Added {
			dead++
		}
	}
	if len(one.Changes) < 100 || len(one.Pairs) < 30 || nonPR == 0 || dead == 0 || len(one.Fingerprints) != 84+30 {
		t.Fatalf("the 84-day run measured %d changes (%d with a dead line, %d lines removed by no pull request), "+
			"%d pairs and %d fingerprints: too little to compare", len(one.Changes), dead, nonPR, len(one.Pairs), len(one.Fingerprints))
	}
	fingerprints := map[time.Time]survival.Fingerprint{}
	for _, fingerprint := range one.Fingerprints {
		fingerprints[fingerprint.Day] = fingerprint
	}
	for _, size := range []int{28, 7} {
		var changes []survival.Change
		var pairs []survival.Pair
		compared := 0
		for from := 0; from < 84; from += size {
			out := h.measure(t, days(from, from+size-1))
			changes = append(changes, out.Changes...)
			pairs = append(pairs, out.Pairs...)
			for _, fingerprint := range out.Fingerprints {
				if want, ok := fingerprints[fingerprint.Day]; !ok || fingerprint != want {
					t.Errorf("runs of %d days: day %s reads %+v, the 84-day run %+v", size, fingerprint.Day.Format(time.DateOnly), fingerprint, want)
				}
				compared++
			}
			for _, state := range out.Days {
				if state.State != survival.DayMeasured {
					t.Errorf("runs of %d days: day %s reads %+v", size, state.Day.Format(time.DateOnly), state)
				}
			}
		}
		if compared < 84 {
			t.Errorf("runs of %d days compared %d fingerprints", size, compared)
		}
		if got, want := sortedByChain(h, changes), sortedByChain(h, one.Changes); !reflect.DeepEqual(got, want) {
			t.Errorf("runs of %d days measure %d changes that differ from the 84-day run's %d", size, len(got), len(want))
		}
		if got, want := pairsByChain(h, pairs), pairsByChain(h, one.Pairs); !reflect.DeepEqual(got, want) {
			t.Errorf("runs of %d days measure %d pairs that differ from the 84-day run's %d", size, len(got), len(want))
		}
	}
}

// cloneFiles is a deep copy of files, which shares no memory with them.
func cloneFiles(files []survival.FileDiff) []survival.FileDiff {
	out := slices.Clone(files)
	for index := range out {
		out[index].Hunks = slices.Clone(out[index].Hunks)
	}
	return out
}

func TestTheReplayStreamsOnePatchAtATime(t *testing.T) {
	t.Parallel()
	h := synthetic(11, 60)
	selected := days(0, 27)
	chain := survival.ReplayRange(h.chain, selected[0], selected[len(selected)-1])
	index := map[string]int{}
	for i, commit := range h.chain {
		index[commit.SHA] = i
	}
	var calls []int
	var held []weak.Pointer[survival.FileDiff]
	reachable := func() []int {
		runtime.GC()
		var out []int
		for i, pointer := range held {
			if pointer.Value() != nil {
				out = append(out, i)
			}
		}
		return out
	}
	out, err := survival.Measure(survival.MeasureInput{Days: selected, Chain: chain,
		Patch: func(i int) (survival.Patch, error) {
			if still := reachable(); len(still) > 0 {
				t.Errorf("asked for patch %d while the patches %v are still reachable", i, still)
			}
			calls = append(calls, i)
			files := cloneFiles(h.patches[index[chain[i].SHA]])
			pointer := weak.Pointer[survival.FileDiff]{}
			if len(files) > 0 {
				pointer = weak.Make(&files[0])
			}
			held = append(held, pointer)
			return survival.Patch{Files: files}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]int, len(chain))
	for i := range want {
		want[i] = i
	}
	if len(chain) < 60 || !slices.Equal(calls, want) {
		t.Errorf("the patches were asked for in the order %v, want each of the range's %d once, in chain order", calls, len(chain))
	}
	if len(out.Changes) == 0 || out.PeakLines == 0 {
		t.Errorf("measured %d changes and a peak of %d lines, want some", len(out.Changes), out.PeakLines)
	}
	watched := 0
	for _, pointer := range held {
		if pointer != (weak.Pointer[survival.FileDiff]{}) {
			watched++
		}
	}
	if watched < 60 {
		t.Errorf("watched %d patches with a file, want the range's", watched)
	}
	if still := reachable(); len(still) > 0 {
		t.Errorf("after the measurement the patches %v are still reachable", still)
	}
}

func TestARemovalAtTheMarkCountsAndOneAfterDoesNot(t *testing.T) {
	t.Parallel()
	added0 := at(0, 12, 0)
	mark := survival.Mark(added0)
	if want := at(30, 12, 0); !mark.Equal(want) {
		t.Fatalf("the mark of a change at %s is %s, want %s", added0, mark, want)
	}
	r := chainOf(
		step{added0, pr(1, "agent/a", "agent-app[bot]", ""), added("x.txt", 10)},
		step{at(5, 0, 0), survival.ChangeFacts{}, added("y.txt", 1)},
		step{mark, pr(2, "agent/b", "agent-app[bot]", ""), removed("x.txt", 1, 2)},
		step{mark, survival.ChangeFacts{}, removed("x.txt", 1, 1)},
		step{mark.Add(time.Second), pr(3, "agent/c", "agent-app[bot]", ""), removed("x.txt", 1, 3)},
		step{mark.Add(time.Second), survival.ChangeFacts{}, removed("x.txt", 1, 1)},
	)
	out := r.measure(t, days(0, 0))
	change := changeOf(t, out, sha(0))
	if change.Added != 10 || change.AliveAtMark != 7 || change.RemovedByNonPR != 1 {
		t.Errorf("the change reads %d added, %d alive and %d removed by no pull request: want 10, 7 and 1",
			change.Added, change.AliveAtMark, change.RemovedByNonPR)
	}
	want := []survival.Pair{{AdderSHA: sha(0), RemoverSHA: sha(2), Remover: r.chain[2].Change, Lines: 2, DaysBetween: 30}}
	if !reflect.DeepEqual(out.Pairs, want) {
		t.Errorf("the pairs are %+v, want the removal at the mark alone: %+v", out.Pairs, want)
	}
}

func TestTheRangeHoldsAnOutOfOrderCommit(t *testing.T) {
	t.Parallel()
	r := chainOf(
		step{at(0, 10, 0), pr(1, "agent/a", "agent-app[bot]", ""), added("x.txt", 6)},
		step{at(31, 5, 0), survival.ChangeFacts{}, added("y.txt", 1)},
		step{at(30, 9, 0), pr(2, "agent/b", "agent-app[bot]", ""), removed("x.txt", 2, 2)},
		step{at(32, 0, 0), survival.ChangeFacts{}, removed("y.txt", 1, 1)},
	)
	rangeChain := survival.ReplayRange(r.chain, day(0), day(0))
	if len(rangeChain) != 3 || rangeChain[2].SHA != sha(2) {
		t.Fatalf("the range is %+v, want the first three commits: the out-of-order one and the later-dated one before it", rangeChain)
	}
	out := r.measure(t, days(0, 0))
	change := changeOf(t, out, sha(0))
	if change.AliveAtMark != 4 || len(out.Pairs) != 1 || out.Pairs[0].RemoverSHA != sha(2) {
		t.Errorf("the change reads %d alive and the pairs %+v: want 4, the out-of-order commit's removal counted",
			change.AliveAtMark, out.Pairs)
	}
}

func TestAnUnreadableCommitBlocksTheDaysWhoseThirtyDaysHoldIt(t *testing.T) {
	t.Parallel()
	h := synthetic(5, 90)
	selected := days(0, 27)
	whole := h.measure(t, selected)
	for _, blocked := range []struct {
		day, from, to int
	}{{40, 9, 27}, {5, 0, 5}} {
		position := slices.IndexFunc(h.chain, func(c survival.Commit) bool {
			return survival.DayOf(c.CommittedAt).Equal(day(blocked.day))
		})
		if position < 1 || h.chain[position].CommittedAt.Before(h.chain[position-1].CommittedAt) {
			t.Fatalf("the branch's first commit of day %d is at %d, dated before the commit before it", blocked.day, position)
		}
		blocker := h.chain[position]
		index := map[string]int{}
		for i, commit := range h.chain {
			index[commit.SHA] = i
		}
		chain := survival.ReplayRange(h.chain, selected[0], selected[len(selected)-1])
		out, err := survival.Measure(survival.MeasureInput{Days: selected, Chain: chain,
			Patch: func(i int) (survival.Patch, error) {
				if chain[i].SHA == blocker.SHA {
					return survival.Patch{Withheld: 3}, nil
				}
				return survival.Patch{Files: h.patches[index[chain[i].SHA]]}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		var wantDays []survival.DayResult
		for n := range 28 {
			state := survival.DayResult{Day: day(n), State: survival.DayMeasured}
			if n >= blocked.from && n <= blocked.to {
				state = survival.DayResult{Day: day(n), State: survival.DayBlocked, BlockedSHA: blocker.SHA, BlockedFiles: 3}
			}
			wantDays = append(wantDays, state)
		}
		if !reflect.DeepEqual(out.Days, wantDays) {
			t.Errorf("a commit of day %d that cannot be read: the days read\n%+v\nwant\n%+v", blocked.day, out.Days, wantDays)
		}
		var wantChanges []survival.Change
		for _, change := range whole.Changes {
			if n := int(change.Day.Sub(day(0)) / (24 * time.Hour)); n < blocked.from || n > blocked.to {
				wantChanges = append(wantChanges, change)
			}
		}
		if len(wantChanges) == 0 || len(wantChanges) == len(whole.Changes) {
			t.Fatalf("a commit of day %d: %d of the %d changes are of the days it leaves measured", blocked.day, len(wantChanges), len(whole.Changes))
		}
		if !reflect.DeepEqual(out.Changes, wantChanges) {
			t.Errorf("a commit of day %d: the measured days' changes differ from those of the run that read it", blocked.day)
		}
		for _, pair := range out.Pairs {
			if !slices.ContainsFunc(wantChanges, func(c survival.Change) bool { return c.SHA == pair.AdderSHA }) {
				t.Errorf("a commit of day %d: a pair of a blocked day's change: %+v", blocked.day, pair)
			}
		}
	}
}

func TestACommitWithNoPullRequestIsNoChange(t *testing.T) {
	t.Parallel()
	r := chainOf(
		step{at(0, 9, 0), survival.ChangeFacts{}, added("x.txt", 5)},
		step{at(0, 10, 0), pr(1, "agent/a", "agent-app[bot]", "KEY-1"), added("y.txt", 4)},
		step{at(3, 10, 0), survival.ChangeFacts{}, removed("y.txt", 2, 1)},
		step{at(4, 10, 0), survival.ChangeFacts{}, removed("x.txt", 1, 2)},
	)
	out := r.measure(t, days(0, 0))
	want := []survival.Change{{SHA: sha(1), Day: day(0), CommittedAt: at(0, 10, 0), ChangeFacts: r.chain[1].Change,
		Added: 4, AliveAtMark: 3, RemovedByNonPR: 1}}
	if !reflect.DeepEqual(out.Changes, want) || len(out.Pairs) != 0 {
		t.Errorf("measured %+v and the pairs %+v, want the one pull request's change, its removal by no pull request, and no pair: %+v",
			out.Changes, out.Pairs, want)
	}
	if name, ok := survival.GroupOf(r.chain[0].Change, []survival.Group{{Name: "everyone"}}); ok {
		t.Errorf("a commit with no pull request is in the group %q", name)
	}
}

func TestAFingerprintIsTheCountAndSha256OfTheDaysCommits(t *testing.T) {
	t.Parallel()
	r := chainOf(
		step{at(0, 9, 0), pr(1, "agent/a", "agent-app[bot]", ""), added("x.txt", 3)},
		step{at(0, 11, 0), survival.ChangeFacts{}, added("y.txt", 1)},
		step{at(2, 8, 0), survival.ChangeFacts{}, added("z.txt", 1)},
		step{at(1, 23, 0), survival.ChangeFacts{}, removed("z.txt", 1, 1)},
		step{at(2, 9, 0), survival.ChangeFacts{}, removed("x.txt", 1, 1)},
		step{at(20, 9, 0), survival.ChangeFacts{}, removed("y.txt", 1, 1)},
	)
	digest := func(shas ...string) [32]byte { return sha256.Sum256([]byte(strings.Join(shas, "\n"))) }
	wantOf := map[int]survival.Fingerprint{
		0:  {Day: day(0), Commits: 2, Digest: digest(sha(0), sha(1))},
		1:  {Day: day(1), Commits: 1, Digest: digest(sha(3))},
		2:  {Day: day(2), Commits: 2, Digest: digest(sha(2), sha(4))},
		20: {Day: day(20), Commits: 1, Digest: digest(sha(5))},
	}
	var want []survival.Fingerprint
	for n := range 31 {
		fingerprint, ok := wantOf[n]
		if !ok {
			fingerprint = survival.Fingerprint{Day: day(n), Digest: sha256.Sum256(nil)}
		}
		want = append(want, fingerprint)
	}
	if got := survival.Fingerprints(r.chain, day(0), day(30)); !reflect.DeepEqual(got, want) {
		t.Errorf("the fingerprints are\n%+v\nwant\n%+v", got, want)
	}
	// A measurement stores the fingerprint of every day it replayed: its days and the 30 after
	// the newest, those with no commit among them.
	out := r.measure(t, days(0, 0))
	if !reflect.DeepEqual(out.Fingerprints, want) {
		t.Errorf("the measurement's fingerprints are\n%+v\nwant\n%+v", out.Fingerprints, want)
	}
}
