package survival_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
)

// The controls of dispatch://INFRA-3117/artifact/method-md, each on a repository built with the
// real git, as the reference scripts drove them: controls 1 to 5 on scripts/07_controls.py's
// repository, 6 as scripts/13_controls2.py's control A, 7 on scripts/20_controls5.py's, and 8 and
// 9 on scripts/19_controls4.py's, each of the last three with a negative control that shows its
// assertion can fail.

// buildControlRepo is 07_controls.py's repository: a base commit, the cohort commit (a change,
// the one the controls measure), then a rename, a rewrite in place, a delete and the drop of one
// of two identical lines, one commit a day. It answers the repository, the cohort's facts and the
// cohort's SHA.
func buildControlRepo(t *testing.T) (*survivaltest.Repo, map[string]survival.ChangeFacts, string) {
	t.Helper()
	repo := survivaltest.New(t)
	repo.Write("unrelated.txt", lines("background line", 30))
	repo.Commit("base", at(0, 10, 0))
	repo.Write("moved/alpha.txt", lines("alpha content unique line", 20))
	repo.Write("rewritten.txt", lines("original rewritten body line", 20))
	repo.Write("deleted.txt", lines("doomed body line", 20))
	repo.Write("dup.txt", "dup head line\nDUPLICATED LINE OF INTEREST\ndup middle line\n"+
		"DUPLICATED LINE OF INTEREST\ndup tail line\n")
	cohort := repo.Commit("cohort commit (#1)", at(1, 10, 0))
	repo.Rename("moved/alpha.txt", "moved/beta.txt")
	repo.Commit("rename only", at(2, 10, 0))
	repo.Write("rewritten.txt", lines("entirely different replacement text", 20))
	repo.Commit("rewrite in place", at(3, 10, 0))
	repo.Delete("deleted.txt")
	repo.Commit("delete the file", at(4, 10, 0))
	repo.Write("dup.txt", "dup head line\nDUPLICATED LINE OF INTEREST\ndup middle line\ndup tail line\n")
	repo.Commit("drop one of the two copies", at(5, 10, 0))
	return repo, map[string]survival.ChangeFacts{cohort: pr(1, "agent/control", "agent-app[bot]", "")}, cohort
}

// controlRepo is buildControlRepo's repository replayed, and the cohort's SHA.
func controlRepo(t *testing.T) (replayed, string) {
	t.Helper()
	repo, facts, cohort := buildControlRepo(t)
	return replay(t, repo, facts), cohort
}

// replayAll replays every commit of r, following the lines of track.
func replayAll(r replayed, track ...string) *survival.Replay {
	replay := survival.NewReplay()
	replay.Track(track...)
	for index, commit := range r.chain {
		replay.Apply(commit, r.patches[index])
	}
	return replay
}

// removedBy is the lines of sha's that the commit at chain index by removed.
func removedBy(replay *survival.Replay, sha string, by int) int {
	for _, removal := range replay.Removals(sha) {
		if removal.By == by {
			return removal.Lines
		}
	}
	return 0
}

// ids is the origin line numbers of path's lines that origin added.
func ids(replay *survival.Replay, path, origin string) []int {
	var out []int
	for _, line := range replay.LinesOf(path) {
		if line.Origin == origin {
			out = append(out, line.ID)
		}
	}
	return out
}

func TestControl1RenamedFileSurvives(t *testing.T) {
	t.Parallel()
	r, cohort := controlRepo(t)
	replay := replayAll(r, cohort)
	if got := ids(replay, "moved/beta.txt", cohort); !slices.Equal(got, []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		11, 12, 13, 14, 15, 16, 17, 18, 19, 20}) {
		t.Errorf("moved/beta.txt holds the cohort's lines %v, want all 20 of moved/alpha.txt", got)
	}
	if got := replay.LinesOf("moved/alpha.txt"); len(got) != 0 {
		t.Errorf("moved/alpha.txt, renamed away, still holds %d lines", len(got))
	}
	// 07_controls.py's whole-pipeline totals: 65 added, 24 alive (the renamed file's 20 and 4 of
	// dup.txt's 5).
	change := changeOf(t, r.measure(t, days(1, 1)), cohort)
	if change.Added != 65 || change.AliveAtMark != 24 {
		t.Errorf("the cohort reads %d added and %d alive at its mark, want 65 and 24", change.Added, change.AliveAtMark)
	}
}

func TestControl2RewrittenFileDies(t *testing.T) {
	t.Parallel()
	r, cohort := controlRepo(t)
	replay := replayAll(r, cohort)
	if got := ids(replay, "rewritten.txt", cohort); len(got) != 0 {
		t.Errorf("rewritten.txt still holds the cohort's lines %v", got)
	}
	if got := len(replay.LinesOf("rewritten.txt")); got != 0 {
		t.Errorf("rewritten.txt holds %d tracked lines; the rewrite's own are not tracked", got)
	}
	if got := removedBy(replay, cohort, 3); got != 20 {
		t.Errorf("the rewrite removed %d of the cohort's lines, want 20", got)
	}
}

func TestControl3DeletedFileDies(t *testing.T) {
	t.Parallel()
	r, cohort := controlRepo(t)
	replay := replayAll(r, cohort)
	if got := replay.LinesOf("deleted.txt"); len(got) != 0 {
		t.Errorf("deleted.txt still holds %d lines", len(got))
	}
	if got := removedBy(replay, cohort, 4); got != 20 {
		t.Errorf("the delete removed %d of the cohort's lines, want 20", got)
	}
}

func TestControl4LineIdentityIsPlaceNotText(t *testing.T) {
	t.Parallel()
	r, cohort := controlRepo(t)
	replay := replayAll(r, cohort)
	alive := ids(replay, "dup.txt", cohort)
	copies := 0
	for _, id := range alive {
		if id == 2 || id == 4 {
			copies++
		}
	}
	if len(alive) != 4 || copies != 1 {
		t.Errorf("dup.txt holds the cohort's lines %v: want 4 alive, one of the two copies at 2 and 4", alive)
	}
	if got := removedBy(replay, cohort, 5); got != 1 {
		t.Errorf("the drop removed %d of the cohort's lines, want the one copy", got)
	}
}

// restrict is a patch as a pass that reads only the paths of touched sees it: a file renamed out
// of those paths is gone, one renamed into them is new, and every other file is not there.
func restrict(files []survival.FileDiff, touched map[string]bool) []survival.FileDiff {
	var out []survival.FileDiff
	for _, file := range files {
		old, added := touched[file.OldPath], touched[file.NewPath]
		switch {
		case old && added, old && file.NewPath == "", added && file.OldPath == "":
			out = append(out, file)
		case old:
			out = append(out, survival.FileDiff{OldPath: file.OldPath, Status: survival.StatusRemoved})
		case added:
			file.OldPath, file.Status = "", survival.StatusAdded
			out = append(out, file)
		}
	}
	return out
}

func TestControl5RestrictedPassReadsTheRenameAsDead(t *testing.T) {
	t.Parallel()
	r, cohort := controlRepo(t)
	touched := map[string]bool{}
	for _, file := range r.patches[1] {
		touched[file.NewPath] = true
	}
	if len(touched) != 4 || !touched["moved/alpha.txt"] {
		t.Fatalf("the cohort touched %v, want its four files", touched)
	}
	restricted := replayed{chain: r.chain}
	for _, files := range r.patches {
		restricted.patches = append(restricted.patches, restrict(files, touched))
	}
	whole := changeOf(t, r.measure(t, days(1, 1)), cohort)
	narrow := changeOf(t, restricted.measure(t, days(1, 1)), cohort)
	if whole.AliveAtMark != 24 || narrow.AliveAtMark != 4 || narrow.Added != whole.Added {
		t.Errorf("the whole pass reads %d of %d alive and the pass restricted to %v reads %d of %d: want "+
			"24 and 4 of 65, the restricted pass losing the renamed file's 20", whole.AliveAtMark, whole.Added,
			touched, narrow.AliveAtMark, narrow.Added)
	}
}

func TestControl6ADayBeforeAMergeReadsNotYetMerged(t *testing.T) {
	t.Parallel()
	repo := survivaltest.New(t)
	repo.Write("base.txt", lines("base line", 5))
	repo.Commit("base", at(0, 9, 0))
	repo.Branch("feature")
	repo.Write("feature.txt", lines("feature line", 12))
	repo.Commit("feature work", at(1, 9, 0))
	repo.Checkout("main")
	repo.Write("base.txt", lines("base line", 6))
	before := repo.Commit("main moves on", at(3, 9, 0))
	merge := repo.Merge("feature", "merge the feature (#7)", at(5, 9, 0))
	r := replay(t, repo, map[string]survival.ChangeFacts{merge: pr(7, "agent/feature", "agent-app[bot]", "")})
	if len(r.chain) != 3 || r.chain[2].SHA != merge || len(r.chain[2].Parents) != 2 {
		t.Fatalf("the first-parent chain is %+v, want base, %s and the merge commit", r.chain, before)
	}

	// The day of the sample, before the merge: the chain as it stood then holds no merge, so the
	// measurement holds no change of it, never a change with no line alive.
	sampled := replayed{chain: r.chain[:2], patches: r.patches[:2]}
	out := sampled.measure(t, days(3, 3))
	for _, change := range out.Changes {
		if change.SHA == merge {
			t.Errorf("the day before the merge reads the merge as %+v", change)
		}
	}
	if len(out.Days) != 1 || out.Days[0].State != survival.DayMeasured {
		t.Errorf("the sampled day reads %+v, want it measured", out.Days)
	}

	// While the merge's 30 days and the grace run, its day waits: no run selects it.
	measured := []survival.DayStatus{}
	for n := range 5 {
		measured = append(measured, survival.DayStatus{Day: day(n), State: survival.DayMeasured})
	}
	for _, now := range []int{6, 15, 37} {
		if got := survival.SelectDays(survival.Selection{Now: day(now), StartDay: day(0), Days: measured}); len(got) != 0 {
			t.Errorf("on day %d the run selects %v, before the merge's day waits", now, got)
		}
	}
	// Once its day and six more wait, a run reads it: the lines it merged, alive.
	selected := survival.SelectDays(survival.Selection{Now: day(5 + 33 + 6), StartDay: day(0), Days: measured})
	if !slices.Contains(selected, day(5)) {
		t.Fatalf("the run selects %v, want the merge's day among them", selected)
	}
	change := changeOf(t, r.measure(t, selected), merge)
	if change.Added != 12 || change.AliveAtMark != 12 {
		t.Errorf("the merge reads %d added and %d alive, want its 12 and 12", change.Added, change.AliveAtMark)
	}
}

// The kinds 20_controls5.py's repository reads: the cohort's issue against each remover's.
var control7Want = map[survival.Kind]int{
	survival.KindIteration: 3, survival.KindRework: 5, survival.KindNoIssue: 7, survival.KindOther: 3,
}

// control7Kinds is the lines of out's cohort removed in each kind, by classify, with the other
// group's removals and the commit that merged no pull request counted as other.
func control7Kinds(t *testing.T, out survival.MeasureOutput, cohort string,
	classify func(adder, remover survival.ChangeFacts) survival.Kind) map[survival.Kind]int {
	t.Helper()
	change := changeOf(t, out, cohort)
	kinds := map[survival.Kind]int{survival.KindOther: change.RemovedByNonPR}
	for _, pair := range out.Pairs {
		if pair.AdderSHA == cohort {
			kinds[classify(change.ChangeFacts, pair.Remover)] += pair.Lines
		}
	}
	return kinds
}

func TestControl7AnUnresolvedKeyIsCountedApart(t *testing.T) {
	t.Parallel()
	ledger := strings.Split(strings.TrimSuffix(lines("ledger entry: reconcile the account for the closing period", 20), "\n"), "\n")
	repo := survivaltest.New(t)
	repo.Write("unrelated.txt", lines("background line", 30))
	repo.Commit("base", at(0, 9, 0))
	repo.Write("ledger.txt", strings.Join(ledger, "\n")+"\n")
	facts := map[string]survival.ChangeFacts{}
	cohort := repo.Commit("add the ledger (#1)", at(1, 9, 0))
	facts[cohort] = pr(1, "agent/ALPHA-101", "agent-app[bot]", "ALPHA-101")
	gone := map[int]bool{}
	removers := []struct {
		facts       survival.ChangeFacts
		from, count int
	}{
		{pr(2, "agent/tighten-ledger", "agent-app[bot]", "ALPHA-101"), 0, 3},
		{pr(3, "agent/rewrite-ledger", "agent-app[bot]", "ALPHA-202"), 3, 5},
		{pr(4, "agent/tidy-ledger", "agent-app[bot]", ""), 8, 7},
		{pr(5, "other/fix-ledger", "other-person", "OTHER-303"), 15, 2},
		{survival.ChangeFacts{}, 17, 1},
	}
	for index, remover := range removers {
		for n := remover.from; n < remover.from+remover.count; n++ {
			gone[n] = true
		}
		var kept []string
		for n, line := range ledger {
			if !gone[n] {
				kept = append(kept, line)
			}
		}
		repo.Write("ledger.txt", strings.Join(kept, "\n")+"\n")
		sha := repo.Commit(fmt.Sprintf("remover %d", index), at(2+index, 9, 0))
		if remover.facts.PR != 0 {
			facts[sha] = remover.facts
		}
	}
	groups := []survival.Group{{Name: "agent", BranchPrefixes: []string{"agent/"}}, {Name: "rest"}}
	out := replay(t, repo, facts).measure(t, days(1, 1))
	kinds := control7Kinds(t, out, cohort, func(adder, remover survival.ChangeFacts) survival.Kind {
		return survival.KindOf("agent", adder, remover, groups)
	})
	change := changeOf(t, out, cohort)
	if change.Added != 20 || change.AliveAtMark != 2 || !mapsEqual(kinds, control7Want) {
		t.Errorf("the cohort reads %d added, %d alive and the kinds %v: want 20, 2 and %v", change.Added,
			change.AliveAtMark, kinds, control7Want)
	}

	// The negative control: a reading that takes a missing issue for an issue, as a different one,
	// reads the 7 lines of the change with no issue as rework, and fails the assertion above.
	collapsed := control7Kinds(t, out, cohort, func(adder, remover survival.ChangeFacts) survival.Kind {
		if group, _ := survival.GroupOf(remover, groups); group != "agent" {
			return survival.KindOther
		}
		if adder.IssueKey == remover.IssueKey {
			return survival.KindIteration
		}
		return survival.KindRework
	})
	if mapsEqual(collapsed, control7Want) || collapsed[survival.KindRework] != 12 {
		t.Errorf("the collapsed reading reads %v, want the 7 lines of no issue read as rework (12)", collapsed)
	}
}

// mapsEqual says whether got holds want's counts and no other count above zero.
func mapsEqual(got, want map[survival.Kind]int) bool {
	for kind, count := range got {
		if count != want[kind] {
			return false
		}
	}
	for kind, count := range want {
		if got[kind] != count {
			return false
		}
	}
	return true
}

func TestControl8NoChangeIsCreditedBeyondItsLines(t *testing.T) {
	t.Parallel()
	head := lines("# ledger module, header line", 4)
	tail := lines("# ledger module, footer line", 4)
	var block string
	for n := range 10 {
		block += fmt.Sprintf("    ledger_total_%02d = reconcile_quarterly_balance_of_account_%02d(region_code_%02d, closing_period_%02d)\n", n, n, n, n)
	}
	repo := survivaltest.New(t)
	repo.Write("unrelated.txt", lines("background line", 30))
	repo.Commit("base", at(0, 9, 0))
	repo.Write("ledger.py", head+block+tail)
	cohort := repo.Commit("cohort commit (#1)", at(1, 9, 0))
	repo.Write("ledger.py", head+block+tail+"# the same block again\n"+block)
	tip := repo.Commit("copy the block inside the same file", at(2, 9, 0))
	r := replay(t, repo, map[string]survival.ChangeFacts{cohort: pr(1, "agent/ledger", "agent-app[bot]", "")})
	out := r.measure(t, days(1, 1))
	change := changeOf(t, out, cohort)
	if change.Added != 18 || change.AliveAtMark != change.Added {
		t.Errorf("the cohort reads %d added and %d alive: want 18 and 18, nothing removed", change.Added, change.AliveAtMark)
	}
	for _, change := range out.Changes {
		if change.AliveAtMark+change.RemovedByNonPR > change.Added {
			t.Errorf("%s is credited %d alive and %d removed of %d added", change.SHA, change.AliveAtMark,
				change.RemovedByNonPR, change.Added)
		}
	}

	// The negative control: a reading by text, which counts every tip line whose text a cohort line
	// holds, credits the cohort with the copied block too.
	added := map[string]bool{}
	for _, line := range strings.Split(head+block+tail, "\n") {
		if line != "" {
			added[line] = true
		}
	}
	byText := 0
	for _, entry := range repo.Tree(tip) {
		if entry.Path != "ledger.py" {
			continue
		}
		for _, line := range strings.Split(string(repo.Blob(entry.Object)), "\n") {
			if added[line] {
				byText++
			}
		}
	}
	if byText != 28 || byText <= change.Added {
		t.Errorf("the text reading credits %d lines, want 28: more than the 18 added", byText)
	}
}

// control9Repo is 19_controls4.py's replay repository, and more of what a tree holds: files made
// (one in a subdirectory), lines inserted in the middle of a file, lines deleted and changed in
// place, a file renamed with an edit, a file deleted, a file appended to, and also a symbolic
// link and its new target, a binary file, an empty file, a file whose last line has no newline,
// paths with a space, a quote and a non-ASCII letter, a merge commit and a submodule. It answers
// the repository and the chain index of the commit the negative control drops.
func control9Repo(t *testing.T) (*survivaltest.Repo, int) {
	t.Helper()
	repo := survivaltest.New(t)
	split := func(prefix string, count int) []string {
		return strings.SplitAfter(lines(prefix, count), "\n")[:count]
	}
	a, b, d := split("alpha line", 10), split("bravo line", 10), split("delta line", 6)
	c := split("charlie line of the file renamed and edited in one commit", 8)
	write := func(path string, text []string) { repo.Write(path, strings.Join(text, "")) }
	write("a.txt", a)
	write("src/lib/b.txt", b)
	write("c.txt", c)
	write("d.txt", d)
	repo.Symlink("link", "a.txt")
	repo.WriteBytes("image.bin", []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3, 0, '\n', 4})
	repo.Write("empty.txt", "")
	repo.Write("tail.txt", "the first line\nthe last line, with no newline")
	repo.Write("spaced name.txt", lines("a line of a path with a space", 4))
	repo.Write(`quoted "name".txt`, lines("a line of a path with a quote", 3))
	repo.Write("naïve.txt", lines("a line of a path with a non-ASCII letter", 5))
	repo.Commit("create the files", at(0, 9, 0))

	a = slices.Insert(a, 5, split("alpha line inserted", 3)...)
	write("a.txt", a)
	repo.Commit("insert three lines into the middle of a.txt", at(1, 9, 0))
	drop := 1

	b = slices.Delete(b, 3, 5)
	a[1], a[9] = "alpha line 1, changed in place\n", "alpha line 6, changed in place\n"
	write("src/lib/b.txt", b)
	write("a.txt", a)
	repo.Write("spaced name.txt", lines("a line of a path with a space", 2)+"an edited line\n")
	repo.Write(`quoted "name".txt`, "a new first line\n"+lines("a line of a path with a quote", 3))
	repo.Write("naïve.txt", lines("a line of a path with a non-ASCII letter", 3))
	repo.Commit("delete from b.txt, change a.txt in place, edit the odd paths", at(2, 9, 0))

	c[2] = "charlie line 2, edited\n"
	c = append(c, "charlie line 8, added\n")
	repo.Delete("c.txt")
	write("docs/charlie.txt", c)
	repo.Rename("spaced name.txt", "spaced dir/renamed name.txt")
	repo.Commit("rename c.txt to docs/charlie.txt with an edit, and move the spaced file", at(3, 9, 0))

	repo.Delete("d.txt")
	repo.Delete("link")
	repo.Symlink("link", "src/lib/b.txt")
	repo.WriteBytes("image.bin", []byte{0x89, 'P', 'N', 'G', 0, 9, 9, '\n', 0, 5})
	repo.Commit("delete d.txt, point the link elsewhere, change the image", at(4, 9, 0))

	b = append(b, split("bravo line appended", 4)...)
	write("src/lib/b.txt", b)
	repo.Write("tail.txt", "the first line\nthe last line, with no newline\nand one more\n")
	repo.Commit("append to b.txt and tail.txt", at(5, 9, 0))

	repo.Branch("side")
	repo.Write("side.txt", lines("a line of the side branch", 7))
	a = append(a, "alpha line appended on the side branch\n")
	write("a.txt", a)
	repo.Commit("side branch work", at(6, 9, 0))
	repo.Checkout("main")
	repo.Write("main.txt", lines("a line of main meanwhile", 2))
	repo.Commit("main meanwhile", at(6, 12, 0))
	repo.Merge("side", "merge the side branch", at(7, 9, 0))

	repo.Submodule("vendored", repo.Head())
	repo.Commit("add a submodule entry", at(8, 9, 0))
	repo.Commit("an empty commit", at(9, 9, 0))
	return repo, drop
}

// lineCount is how many lines git's diff counts in a blob: its newlines, and one more for a last
// line with none.
func lineCount(content []byte) int {
	count := bytes.Count(content, []byte{'\n'})
	if len(content) > 0 && content[len(content)-1] != '\n' {
		count++
	}
	return count
}

// treeMismatches compares the lines replay holds with each file of repo's tree at tip: each text
// file's line count, a symbolic link as a one-line file, no line of a binary file or a submodule,
// and no path the tree does not hold. It answers the mismatches and how many files it compared.
func treeMismatches(repo *survivaltest.Repo, tip string, replay *survival.Replay) ([]string, int) {
	var mismatches []string
	compared := 0
	held := map[string]bool{}
	for _, entry := range repo.Tree(tip) {
		want := 0
		switch {
		case entry.Type == "commit":
		case entry.Mode == "120000":
			want = 1
		default:
			content := repo.Blob(entry.Object)
			if bytes.IndexByte(content, 0) < 0 {
				want = lineCount(content)
			}
		}
		held[entry.Path] = true
		compared++
		if got := len(replay.LinesOf(entry.Path)); got != want {
			mismatches = append(mismatches, fmt.Sprintf("%s (%s %s): %d lines replayed, %d in the tree",
				entry.Path, entry.Mode, entry.Type, got, want))
		}
	}
	for _, path := range replay.Paths() {
		if !held[path] {
			mismatches = append(mismatches, fmt.Sprintf("%s: %d lines replayed, not in the tree", path,
				len(replay.LinesOf(path))))
		}
	}
	return mismatches, compared
}

func TestControl9TheReplayReproducesTheTree(t *testing.T) {
	t.Parallel()
	repo, drop := control9Repo(t)
	tip := repo.Head()
	commits := repo.FirstParents(tip)
	var all []string
	for _, commit := range commits {
		all = append(all, commit.SHA)
	}
	read := map[string]func(survivaltest.Commit) []byte{
		"git log -p -U0 -M": func(c survivaltest.Commit) []byte { return c.Patch },
		"GitHub's shape":    func(c survivaltest.Commit) []byte { return repo.Diff(c.SHA) },
	}
	for source, patchOf := range read {
		replay := survival.NewReplay()
		replay.Track(all...)
		for _, commit := range commits {
			files, err := survival.ParseDiff(patchOf(commit))
			if err != nil {
				t.Fatalf("%s: parse %s: %v", source, commit.SHA, err)
			}
			replay.Apply(survival.Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt}, files)
		}
		mismatches, compared := treeMismatches(repo, tip, replay)
		if compared < 13 {
			t.Fatalf("%s: compared %d files of the tree, want its 13", source, compared)
		}
		if len(mismatches) > 0 {
			t.Errorf("%s: the replay does not reproduce the tree:\n%s", source, strings.Join(mismatches, "\n"))
		}
	}

	// The negative control: the same replay with the patch of one middle commit skipped does not
	// reproduce the tree.
	replay := survival.NewReplay()
	replay.Track(all...)
	for index, commit := range commits {
		files, err := survival.ParseDiff(commit.Patch)
		if err != nil {
			t.Fatal(err)
		}
		if index == drop {
			files = nil
		}
		replay.Apply(survival.Commit{SHA: commit.SHA}, files)
	}
	if mismatches, _ := treeMismatches(repo, tip, replay); len(mismatches) == 0 {
		t.Error("the replay that skipped one patch reproduces the tree: the comparison cannot fail")
	}
}
