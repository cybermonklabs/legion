//go:build survivalref

package survival_test

import (
	"fmt"
	"math"
	"os"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
)

// The archives prepare.sh's step 1 unpacks under <dir>/archives: the second-repository run's
// output (lg/ and ai/) and the analysis pass's output (out/).
var (
	refSecondRepo = []string{"archives", "feasibility-second-repo-output-tar-gz", "feasibility-second-repo"}
	refPassOutput = []string{"archives", "survival-pass-output-tar-gz", "out"}
)

// refDay is one UTC day.
const refDay = 24 * time.Hour

// refColumn is the group column of the reference's files (lg/commits.tsv, merge_keys.tsv) for
// each of survivaltest.ReferenceGroups' names; a commit that merged no pull request is "none".
var refColumn = map[string]string{"Legion": "legion", "Other": "other"}

// refGroups is survivaltest.ReferenceGroups as the survival package holds them.
func refGroups() []survival.Group {
	var groups []survival.Group
	for _, group := range survivaltest.ReferenceGroups() {
		groups = append(groups, survival.Group{Name: group.Name, BranchPrefixes: group.BranchPrefixes,
			AuthorLogins: group.AuthorLogins})
	}
	return groups
}

// refLegion is the reference's chain of cybermonklabs/legion as Measure reads it: each commit with
// the facts of the pull request it merged (lg/pulls.json) and the issue lg/pr_issue.tsv gives it.
type refLegion struct {
	ref    survivaltest.Reference
	chain  []survival.Commit
	groups []survival.Group
	// window is every UTC day from the chain's first commit's to its tip's.
	window []time.Time
}

// refCache is what the reference tests read once and share: the chain, and the single pass of
// the whole window.
var refCache struct {
	sync.Mutex
	legion *refLegion
	pass   *survival.MeasureOutput
}

// refLegionChain is the reference's chain of cybermonklabs/legion, read once.
func refLegionChain(t *testing.T) *refLegion {
	t.Helper()
	ref := survivaltest.OpenReference(t)
	refCache.Lock()
	defer refCache.Unlock()
	if refCache.legion != nil {
		return refCache.legion
	}
	merged := map[string]survivaltest.Pull{}
	for _, pull := range ref.Pulls(t) {
		if !pull.Merged() {
			continue
		}
		if _, twice := merged[pull.MergeSHA]; twice {
			t.Fatalf("two merged pull requests of lg/pulls.json merged as %s", pull.MergeSHA)
		}
		merged[pull.MergeSHA] = pull
	}
	issues := map[string]string{}
	for _, issue := range ref.PullIssues(t) {
		issues[issue.MergeSHA] = issue.Issue
	}
	legion := &refLegion{ref: ref, groups: refGroups()}
	for _, commit := range ref.Chain(t) {
		next := survival.Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt}
		if pull, ok := merged[commit.SHA]; ok {
			next.Change = survival.ChangeFacts{PR: pull.Number, HeadRef: pull.HeadRef, Author: pull.Author,
				IssueKey: issues[commit.SHA]}
		}
		legion.chain = append(legion.chain, next)
	}
	if len(legion.chain) == 0 {
		t.Fatalf("lg/commits.txt of %s holds no commit", ref.Dir)
	}
	legion.window = refDaysFrom(survival.DayOf(legion.chain[0].CommittedAt),
		survival.DayOf(legion.chain[len(legion.chain)-1].CommittedAt))
	refCache.legion = legion
	return legion
}

// tip is the chain's last commit's committer time, the moment 16_window.py counts its weeks and
// its horizons back from.
func (l *refLegion) tip() time.Time { return l.chain[len(l.chain)-1].CommittedAt }

// refDaysFrom is every UTC midnight from first to last, both included.
func refDaysFrom(first, last time.Time) []time.Time {
	var out []time.Time
	for day := first; !day.After(last); day = day.Add(refDay) {
		out = append(out, day)
	}
	return out
}

// refPatch is the file diffs of the gzipped patch at path, read through reader, which holds one
// patch's text at a time, and parsed with ParseDiff.
func refPatch(reader *survivaltest.PatchReader, path string) ([]survival.FileDiff, error) {
	text, err := reader.Read(path)
	if err != nil {
		return nil, err
	}
	files, err := survival.ParseDiff(text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return files, nil
}

// measure runs Measure over days, the replay range of chain (the reference's chain, or one the
// test doctored), each patch read from its file when Measure asks for it.
func (l *refLegion) measure(t *testing.T, days []time.Time, chain []survival.Commit) (survival.MeasureOutput, *survivaltest.PatchReader) {
	t.Helper()
	span := survival.ReplayRange(chain, days[0], days[len(days)-1])
	reader := &survivaltest.PatchReader{}
	out, err := survival.Measure(survival.MeasureInput{Days: days, Chain: span,
		Patch: func(i int) (survival.Patch, error) {
			files, err := refPatch(reader, l.ref.PatchPath(span[i].SHA))
			return survival.Patch{Files: files}, err
		}})
	if err != nil {
		t.Fatalf("measure %d days from %s: %v", len(days), days[0].Format(time.DateOnly), err)
	}
	return out, reader
}

// onePass is the single pass of the whole window: every day of it measured in one Measure, read
// once and shared by the tests that compare with it.
func (l *refLegion) onePass(t *testing.T) survival.MeasureOutput {
	t.Helper()
	refCache.Lock()
	pass := refCache.pass
	refCache.Unlock()
	if pass != nil {
		return *pass
	}
	started := time.Now()
	out, reader := l.measure(t, l.window, l.chain)
	t.Logf("the single pass: %d days, %d commits, %d patch bytes read in %s; %d changes, %d pairs, peak %d tracked lines",
		len(l.window), reader.Reads(), reader.Total(), time.Since(started).Round(time.Millisecond), len(out.Changes), len(out.Pairs), out.PeakLines)
	refCache.Lock()
	refCache.pass = &out
	refCache.Unlock()
	return out
}

// refReplayEvery replays chain with every commit tracked, as replay.py does, each patch read from
// the file path names.
func refReplayEvery(t *testing.T, chain []survival.Commit, path func(sha string) string) *survival.Replay {
	t.Helper()
	replay := survival.NewReplay()
	for _, commit := range chain {
		replay.Track(commit.SHA)
	}
	reader := &survivaltest.PatchReader{}
	for _, commit := range chain {
		files, err := refPatch(reader, path(commit.SHA))
		if err != nil {
			t.Fatalf("read the patch of %s: %v", commit.SHA, err)
		}
		replay.Apply(commit, files)
	}
	return replay
}

// refRows is every line of the tab-separated file at path, split.
func refRows(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows
}

// refInt is field of a reference file read as an integer.
func refInt(t *testing.T, path, field string) int {
	t.Helper()
	n, err := strconv.Atoi(field)
	if err != nil {
		t.Fatalf("%s: %q is no integer: %v", path, field, err)
	}
	return n
}

// refCounts is the lines of a replay's two files of tallies (replay.py's <prefix>_added.tsv and
// <prefix>_deaths.tsv), each tally the sum of its four path categories: the lines each commit
// added, by its index in the chain (from 1), with its SHA, and the lines each (adder, remover)
// pair of indexes killed.
type refCounts struct {
	added  map[int]int
	shas   map[int]string
	deaths map[[2]int]int
}

// refReadCounts reads the tallies replay.py wrote at prefix.
func refReadCounts(t *testing.T, prefix string) refCounts {
	t.Helper()
	counts := refCounts{added: map[int]int{}, shas: map[int]string{}, deaths: map[[2]int]int{}}
	path := prefix + "_added.tsv"
	for _, row := range refRows(t, path) {
		if len(row) != 6 {
			t.Fatalf("%s: a row of %d fields, not 6: %v", path, len(row), row)
		}
		index := refInt(t, path, row[0])
		counts.shas[index] = row[1]
		for _, field := range row[2:] {
			counts.added[index] += refInt(t, path, field)
		}
	}
	path = prefix + "_deaths.tsv"
	for _, row := range refRows(t, path) {
		if len(row) != 6 {
			t.Fatalf("%s: a row of %d fields, not 6: %v", path, len(row), row)
		}
		key := [2]int{refInt(t, path, row[0]), refInt(t, path, row[1])}
		for _, field := range row[2:] {
			counts.deaths[key] += refInt(t, path, field)
		}
	}
	return counts
}

// refCountsOf is the same tallies of a replay of chain that tracked every commit.
func refCountsOf(replay *survival.Replay, chain []survival.Commit) refCounts {
	counts := refCounts{added: map[int]int{}, shas: map[int]string{}, deaths: map[[2]int]int{}}
	for at, commit := range chain {
		counts.shas[at+1] = commit.SHA
		if added := replay.Added(commit.SHA); added > 0 {
			counts.added[at+1] = added
		}
		for _, removal := range replay.Removals(commit.SHA) {
			counts.deaths[[2]int{at + 1, removal.By + 1}] = removal.Lines
		}
	}
	return counts
}

// refComparison is how two replays' tallies compare: the commits and pairs each side holds, and
// those whose lines differ, with the lines each side adds up to.
type refComparison struct {
	commits, commitsDiffer int
	pairs, pairsDiffer     int
	added, addedOther      int
	deaths, deathsOther    int
	first                  []string
}

// refCompare compares got, a replay's tallies over a chain of n commits, with want, a
// reference's: each commit's added lines (a commit a file names no line of added none) and each
// pair's lines. It also fails t at a row of want whose SHA is not the chain's commit of its index.
func refCompare(t *testing.T, n int, want, got refCounts) refComparison {
	t.Helper()
	var c refComparison
	for index, sha := range want.shas {
		if got.shas[index] != sha {
			t.Fatalf("the reference names commit %d %s, which is %q in the chain", index, sha, got.shas[index])
		}
	}
	for index := 1; index <= n; index++ {
		c.commits++
		c.added += want.added[index]
		c.addedOther += got.added[index]
		if want.added[index] != got.added[index] {
			c.commitsDiffer++
			if len(c.first) < 5 {
				c.first = append(c.first, fmt.Sprintf("commit %d %s: %d lines added in the reference, %d in the replay",
					index, got.shas[index], want.added[index], got.added[index]))
			}
		}
	}
	pairs := map[[2]int]bool{}
	for key := range want.deaths {
		pairs[key] = true
	}
	for key := range got.deaths {
		pairs[key] = true
	}
	for key := range pairs {
		c.pairs++
		c.deaths += want.deaths[key]
		c.deathsOther += got.deaths[key]
		if want.deaths[key] != got.deaths[key] {
			c.pairsDiffer++
			if len(c.first) < 10 {
				c.first = append(c.first, fmt.Sprintf("pair %d → %d: %d lines in the reference, %d in the replay",
					key[0], key[1], want.deaths[key], got.deaths[key]))
			}
		}
	}
	return c
}

// refChangeKey is a pair's two commits.
type refChangeKey struct{ adder, remover string }

// refDiffChanges is every difference between two measurements' changes and pairs: a change or a
// pair one holds and the other does not, or holds otherwise. Each is a line naming it.
func refDiffChanges(want, got survival.MeasureOutput) []string {
	var diffs []string
	wantChanges := map[string]survival.Change{}
	for _, change := range want.Changes {
		wantChanges[change.SHA] = change
	}
	gotChanges := map[string]survival.Change{}
	for _, change := range got.Changes {
		if _, twice := gotChanges[change.SHA]; twice {
			diffs = append(diffs, fmt.Sprintf("change %s measured twice", change.SHA))
		}
		gotChanges[change.SHA] = change
	}
	for sha, w := range wantChanges {
		g, ok := gotChanges[sha]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("change %s missing", sha))
		case !g.Day.Equal(w.Day) || !g.CommittedAt.Equal(w.CommittedAt) || g.ChangeFacts != w.ChangeFacts ||
			g.Added != w.Added || g.AliveAtMark != w.AliveAtMark || g.RemovedByNonPR != w.RemovedByNonPR:
			diffs = append(diffs, fmt.Sprintf("change %s: %+v, want %+v", sha, g, w))
		}
	}
	for sha := range gotChanges {
		if _, ok := wantChanges[sha]; !ok {
			diffs = append(diffs, fmt.Sprintf("change %s not in the single pass", sha))
		}
	}
	wantPairs := map[refChangeKey]survival.Pair{}
	for _, pair := range want.Pairs {
		wantPairs[refChangeKey{pair.AdderSHA, pair.RemoverSHA}] = pair
	}
	gotPairs := map[refChangeKey]survival.Pair{}
	for _, pair := range got.Pairs {
		key := refChangeKey{pair.AdderSHA, pair.RemoverSHA}
		if _, twice := gotPairs[key]; twice {
			diffs = append(diffs, fmt.Sprintf("pair %s → %s measured twice", key.adder, key.remover))
		}
		gotPairs[key] = pair
	}
	for key, w := range wantPairs {
		if g, ok := gotPairs[key]; !ok || g != w {
			diffs = append(diffs, fmt.Sprintf("pair %s → %s: %+v (held %v), want %+v", key.adder, key.remover, g, ok, w))
		}
	}
	for key := range gotPairs {
		if _, ok := wantPairs[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("pair %s → %s not in the single pass", key.adder, key.remover))
		}
	}
	slices.Sort(diffs)
	return diffs
}

// refScriptPair is one pair rework30.py keeps (rework30_pairs.tsv): the case it puts the pair in
// (same, different or unresolved), the lines, and the gap in days between the two merges.
type refScriptPair struct {
	kase  string
	lines int
	gap   float64
}

// refScriptPairs reads rework30_pairs.tsv at path.
func refScriptPairs(t *testing.T, path string) map[refChangeKey]refScriptPair {
	t.Helper()
	pairs := map[refChangeKey]refScriptPair{}
	for _, row := range refRows(t, path) {
		if len(row) != 5 {
			t.Fatalf("%s: a row of %d fields, not 5: %v", path, len(row), row)
		}
		gap, err := strconv.ParseFloat(row[4], 64)
		if err != nil {
			t.Fatalf("%s: the gap %q: %v", path, row[4], err)
		}
		key := refChangeKey{row[0], row[1]}
		if _, twice := pairs[key]; twice {
			t.Fatalf("%s holds the pair %s → %s twice", path, key.adder, key.remover)
		}
		pairs[key] = refScriptPair{kase: row[2], lines: refInt(t, path, row[3]), gap: gap}
	}
	return pairs
}

// refCaseOf is the case of rework30.py that each kind of a pair within one group is.
var refCaseOf = map[survival.Kind]string{
	survival.KindIteration: "same",
	survival.KindRework:    "different",
	survival.KindNoIssue:   "unresolved",
}

// refReplayPairs is each pair of out whose adder is a change of group and whose kind, by
// survival.KindOf under groups, is rework, iteration or no issue: a remover of the same group,
// which is the join rework30.py makes, in its case.
func refReplayPairs(out survival.MeasureOutput, group string, groups []survival.Group) map[refChangeKey]refScriptPair {
	adders := map[string]survival.Change{}
	for _, change := range out.Changes {
		if name, ok := survival.GroupOf(change.ChangeFacts, groups); ok && name == group {
			adders[change.SHA] = change
		}
	}
	pairs := map[refChangeKey]refScriptPair{}
	for _, pair := range out.Pairs {
		adder, ok := adders[pair.AdderSHA]
		if !ok {
			continue
		}
		kase, within := refCaseOf[survival.KindOf(group, adder.ChangeFacts, pair.Remover, groups)]
		if !within {
			continue
		}
		pairs[refChangeKey{pair.AdderSHA, pair.RemoverSHA}] = refScriptPair{kase: kase, lines: pair.Lines, gap: pair.DaysBetween}
	}
	return pairs
}

// refGapSlack is how far two readings of one gap may stand apart, in days: the script divides
// seconds by 86400.0, Measure nanoseconds by a day's, so the two may differ in their last bit.
const refGapSlack = 1e-9

// refComparePairs is every difference between the pairs rework30.py keeps and the replay's: a
// pair one holds and the other does not, or one held in another case, of other lines or at
// another gap. Each is the pair, and the line naming the difference.
func refComparePairs(script, replay map[refChangeKey]refScriptPair) map[refChangeKey]string {
	diffs := map[refChangeKey]string{}
	for key, want := range script {
		got, ok := replay[key]
		switch {
		case !ok:
			diffs[key] = fmt.Sprintf("%s → %s: the script keeps %+v, the replay holds no such pair", key.adder, key.remover, want)
		case got.kase != want.kase || got.lines != want.lines || math.Abs(got.gap-want.gap) > refGapSlack:
			diffs[key] = fmt.Sprintf("%s → %s: the script keeps %+v, the replay %+v", key.adder, key.remover, want, got)
		}
	}
	for key, got := range replay {
		if _, ok := script[key]; !ok {
			diffs[key] = fmt.Sprintf("%s → %s: the replay holds %+v, the script keeps no such pair", key.adder, key.remover, got)
		}
	}
	return diffs
}

// refFirst is the first n of lines, in order, for a failure's message.
func refFirst(lines []string, n int) string {
	slices.Sort(lines)
	if len(lines) > n {
		lines = append(lines[:n:n], fmt.Sprintf("… and %d more", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

// refFigure is one survival figure: the changes it reads, the lines they added, and those of
// them alive at each change's mark.
type refFigure struct {
	changes, added, alive int
}

// share is the figure's share of lines alive, in per cent to three decimals, as published.
func (f refFigure) share() string {
	if f.added == 0 {
		return "-"
	}
	return strconv.FormatFloat(100*float64(f.alive)/float64(f.added), 'f', 3, 64)
}

// refHeapSampler samples the live heap after garbage collection (runtime/metrics
// /gc/heap/live:bytes) every millisecond until its stop, which answers the most it read.
func refHeapSampler() (stop func() uint64) {
	const name = "/gc/heap/live:bytes"
	read := func() uint64 {
		sample := []metrics.Sample{{Name: name}}
		metrics.Read(sample)
		if sample[0].Value.Kind() != metrics.KindUint64 {
			panic(name + " is no uint64 metric of this runtime")
		}
		return sample[0].Value.Uint64()
	}
	var peak atomic.Uint64
	keep := func(value uint64) {
		for {
			was := peak.Load()
			if value <= was || peak.CompareAndSwap(was, value) {
				return
			}
		}
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			keep(read())
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-finished
		keep(read())
		return peak.Load()
	}
}
