//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The size walk's cases. They build a volume of their own, as the trimmer's cases do
// (volumetrim_test.go), because what the walk must get right is which directories it measures
// and in which order, and that is a question about a tree.

// walkTree builds a volume laid out as the pod's is and answers its root: the two children the
// walk measures first, the rest of the volume after them, one immediate child that holds no
// directory of its own and one child that is not a directory at all.
func walkTree(t *testing.T, at time.Time) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{
		"scratch/tmp", "scratch/cache", "scratch/bun",
		"go/build", "go/mod",
		"uv/cache", "uv/python",
		"workspaces/cybermonklabs",
	} {
		mkdirFixture(t, filepath.Join(root, dir))
		writeAged(t, filepath.Join(root, dir, "one"), at)
	}
	// An auxiliary directory with a file has no child directory of its own, so the walk measures
	// that child itself and no byte of the volume goes unaccounted.
	mkdirFixture(t, filepath.Join(root, "archive"))
	writeAged(t, filepath.Join(root, "archive", "issue.json"), at)
	// A file directly under the volume root, which is a child that holds no directory either.
	writeAged(t, filepath.Join(root, "lost+found.txt"), at)
	return root
}

// walkOf is one walk of root on a clock and a budget the case holds. A measurement is given the
// handle the walk holds on the directory that holds the target and the target's own name in it,
// as the trimmer's own seam is (trimmer.measure), and the path a size carries is the walk's to
// fill (sizeWalk.run).
func walkOf(root string, clock *fakeClock, ticks *fakeTicks, measure func(context.Context, *dirHandle, string, time.Time) measured) sizeWalk {
	return sizeWalk{root: root, now: clock.now, after: ticks.make, budget: volumeWalkBudget, measure: measure}
}

// The walk measures every directory exactly two levels under the volume root, and an immediate
// child that holds no directory of its own, in a fixed order: the scratch children first, then
// the go children, then the rest. A walk that ends early must lose the least useful numbers,
// and everything the trimmer can delete is under those two.
func TestTheWalkMeasuresTwoLevelsUnderTheRootInItsOwnOrder(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	want := []string{
		// scratch first, its children as they sort
		"scratch/bun", "scratch/cache", "scratch/tmp",
		// then go
		"go/build", "go/mod",
		// then the rest, in the order the root's own children sort
		"archive", "lost+found.txt", "uv/cache", "uv/python", "workspaces/cybermonklabs",
	}
	var got []string
	for _, size := range result.Sizes {
		got = append(got, filepath.ToSlash(size.Path[len(root)+1:]))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the walk measured\n%v\nwant\n%v", got, want)
	}
	if result.Partial || len(result.Missing) > 0 {
		t.Errorf("the walk of a readable tree is partial (%t) and missing %v, want neither", result.Partial, result.Missing)
	}
	if !result.At.Equal(clock.at) {
		t.Errorf("the walk was made at %s, want the clock's %s", result.At, clock.at)
	}
	// Nothing on the volume is left unaccounted: every file of the tree is under exactly one of
	// the directories the walk measured.
	files := 0
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files++
		under := 0
		for _, size := range result.Sizes {
			if path == size.Path || filepath.Dir(path) == size.Path || len(path) > len(size.Path) && path[:len(size.Path)+1] == size.Path+"/" {
				under++
			}
		}
		if under != 1 {
			t.Errorf("%s is under %d of the directories the walk measured, want exactly 1", path, under)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("the tree holds no file at all, so the case accounts for nothing")
	}
	for _, size := range result.Sizes {
		if size.Bytes == 0 {
			t.Errorf("the walk measured %s at no bytes, want the file this tree puts under every one", size.Path)
		}
	}
}

// Each size carries the bytes under it, and of those the bytes whose modification time is more
// than an hour and more than a day old: the same walk already stats every file for the first
// number, and the two splits are what tell a reader a working set from cold bytes.
func TestEachSizeCarriesItsTwoAgeSplits(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	measured := filepath.Join(root, "scratch", "tmp")
	if err := os.Remove(filepath.Join(measured, "one")); err != nil {
		t.Fatal(err)
	}
	written := map[string]struct {
		at    time.Time
		bytes int
	}{
		"fresh": {clock.at.Add(-10 * time.Minute), 100},
		"hour":  {clock.at.Add(-2 * time.Hour), 20},
		"day":   {clock.at.Add(-48 * time.Hour), 3},
	}
	for name, file := range written {
		path := filepath.Join(measured, name)
		if err := os.WriteFile(path, make([]byte, file.bytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, file.at, file.at); err != nil {
			t.Fatal(err)
		}
	}

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())
	at := slices.IndexFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == measured })
	if at < 0 {
		t.Fatalf("the walk did not measure %s at all: %+v", measured, result.Sizes)
	}
	size := result.Sizes[at]
	switch {
	case size.Bytes != 123:
		t.Errorf("%s holds %d B, want the 123 of the three files in it", measured, size.Bytes)
	case size.BytesOverHour != 23:
		t.Errorf("%s holds %d B older than an hour, want the 23 of the two files written before it", measured, size.BytesOverHour)
	case size.BytesOverDay != 3:
		t.Errorf("%s holds %d B older than a day, want the 3 of the one file written before it", measured, size.BytesOverDay)
	}
}

// A file exactly an hour old is not among a size's bytes over an hour, and one a second older is;
// the same holds of a day: R4 has each size carry "the bytes whose newest time is more than an hour
// old and the bytes more than a day old" (countFile). The case above writes files ten minutes, two
// hours and two days old, with which a split that took in the file at the hour or the day itself,
// or left out one a second past it, is as green. The clock stands on a whole second, so every time
// is one a file on this volume holds exactly, and each is read back before the walk.
func TestAFileExactlyAnHourOrADayOldIsNotOverItAndOneASecondOlderIs(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	measured := filepath.Join(root, "scratch", "tmp")
	if err := os.Remove(filepath.Join(measured, "one")); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name  string
		age   time.Duration
		bytes int
	}{
		{"an-hour", time.Hour, 1},
		{"a-second-past-an-hour", time.Hour + time.Second, 10},
		{"a-day", 24 * time.Hour, 100},
		{"a-second-past-a-day", 24*time.Hour + time.Second, 1000},
	} {
		path, at := filepath.Join(measured, file.name), clock.at.Add(-file.age)
		if err := os.WriteFile(path, make([]byte, file.bytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(path); err != nil || !info.ModTime().Equal(at) {
			t.Fatalf("%s does not read as written at %s (%v): the case would not stand where it says", path, at, err)
		}
	}

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())
	at := slices.IndexFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == measured })
	if at < 0 {
		t.Fatalf("the walk did not measure %s at all: %+v", measured, result.Sizes)
	}
	size := result.Sizes[at]
	if size.Bytes != 1111 {
		t.Errorf("%s holds %d B, want the 1111 of the four files in it", measured, size.Bytes)
	}
	if size.BytesOverHour != 1110 {
		t.Errorf("%s holds %d B older than an hour, want the 1110 of the three files a second or more past the hour: the file exactly an hour old is not more than an hour old", measured, size.BytesOverHour)
	}
	if size.BytesOverDay != 1000 {
		t.Errorf("%s holds %d B older than a day, want the 1000 of the file a second past the day: the file exactly a day old is not more than a day old", measured, size.BytesOverDay)
	}
}

// The walk is bounded at sixty seconds, and a walker that blocks is cut where it stands: the
// sizes it did produce are the first in the order the rule names, and everything it had not
// reached is named as missing rather than reported as a directory of no bytes.
func TestTheWalkEndsOnItsBudgetAgainstAWalkerThatBlocks(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	ticks := &fakeTicks{}
	reached := make(chan struct{})
	calls := 0
	measure := func(ctx context.Context, _ *dirHandle, _ string, _ time.Time) measured {
		calls++
		if calls <= 2 {
			return measured{size: shimwire.LauncherVolumeSize{Bytes: int64(calls)}}
		}
		close(reached)
		<-ctx.Done()
		return measured{cut: true}
	}

	results := make(chan walkResult, 1)
	go func() { results <- walkOf(root, clock, ticks, measure).run(context.Background()) }()
	<-reached

	if period := ticks.periodOf(0); period != volumeWalkBudget {
		t.Errorf("the walk asked for a budget of %s, want the %s the rule names", period, volumeWalkBudget)
	}
	if !ticks.offer(t, 0, beforeDeadline(t)) {
		t.Fatal("the walk is not waiting on its budget: nothing took the tick")
	}
	var result walkResult
	select {
	case result = <-results:
	case <-time.After(beforeDeadline(t)):
		t.Fatal("the walk did not end when its budget ran out")
	}

	want := []string{filepath.Join(root, "scratch", "bun"), filepath.Join(root, "scratch", "cache")}
	var got []string
	for _, size := range result.Sizes {
		got = append(got, size.Path)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the walk produced %v, want the first two of its own order %v", got, want)
	}
	if !result.Partial {
		t.Error("the walk that was cut is not marked partial")
	}
	missing := missingPaths(result.Missing)
	if len(missing) == 0 || missing[0] != filepath.Join(root, "scratch", "tmp") {
		t.Errorf("the walk named %v as missing, want the directory it was cut in and everything after it", missing)
	}
	if slices.Contains(missing, want[0]) {
		t.Errorf("the walk named %s both measured and missing", want[0])
	}
}

// A read the kernel refuses under a target ends nothing and is no early end: the walk goes on to
// its end, so it is not partial, and each directory it could read only in part is a size that names
// the first read refused there and how many were, so that its bytes are never read as the
// directory's whole. Four shapes are staged: two mode-0000 directories under one auxiliary
// directory, which the walk reads the rest of; a mode-0000 directory at the foot of a chain thirty
// directories deep, which is named by the target, its own name and its depth and never by the path
// between, as the deleter's removal failure is (removalFailure); a target that is itself mode 0000,
// whose lstat in its readable parent succeeds and whose open is refused, so the walk reads nothing
// of it and names it as unmeasured with the read the kernel refused, and as no size; and an
// immediate child of the root that is mode 0000, which the walk cannot list and names as unmeasured
// the same way. The negative control is every other target of the tree, which carries no refusal.
func TestARefusedReadUnderATargetIsNamedOnItsSizeAndIsNoEarlyEnd(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	auxiliary := filepath.Join(root, "archive", "pack")
	mkdirFixture(t, auxiliary)
	writeAged(t, filepath.Join(auxiliary, "issue.json"), clock.at.Add(-2*time.Hour))
	first, second := filepath.Join(auxiliary, "a-locked"), filepath.Join(auxiliary, "b-locked")
	locked := filepath.Join(root, "workspaces", "locked")
	sealed := filepath.Join(root, "sealed")
	// The chain a role can make under any target: thirty levels of 100-byte names, which the
	// walk descends by handle, and a refused directory at its foot.
	deep := filepath.Join(root, "uv", "deep")
	foot := deep
	for level := range 30 {
		foot = filepath.Join(foot, fmt.Sprintf("%02d", level)+strings.Repeat("d", 98))
	}
	footLocked := filepath.Join(foot, "locked")
	for _, dir := range []string{first, second, locked, sealed, footLocked} {
		mkdirFixture(t, dir)
		writeAged(t, filepath.Join(dir, "two"), clock.at.Add(-2*time.Hour))
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	if result.Partial {
		t.Error("a walk that went on past every refusal to its end is marked partial, which a reader reads as a walk that ended early")
	}
	permission := "openat: " + syscall.EACCES.Error()
	if want := []shimwire.LauncherVolumeMissing{{Path: sealed, Reason: permission}, {Path: locked, Reason: permission}}; !slices.Equal(result.Missing, want) {
		t.Errorf("the walk named %q as missing, want %q: the child it could not list and the target it could not open, each with the read refused", result.Missing, want)
	}
	sizeOf := func(path string) (shimwire.LauncherVolumeSize, bool) {
		at := slices.IndexFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == path })
		if at < 0 {
			t.Errorf("the walk did not measure %s: %+v", path, result.Sizes)
			return shimwire.LauncherVolumeSize{}, false
		}
		return result.Sizes[at], true
	}
	if size, ok := sizeOf(auxiliary); ok && (size.Bytes != 1 || size.Refused != first || size.Refusals != 2) {
		t.Errorf("the walk measured the auxiliary directory as %+v, want the 1 B it could read, refused first at %s and 2 refusals in all", size, first)
	}
	if at := slices.IndexFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == locked }); at >= 0 {
		t.Errorf("the walk reports the directory it could not open as the size %+v, want it named as unmeasured and no size at all", result.Sizes[at])
	}
	if size, ok := sizeOf(deep); ok {
		want := deep + "/.../locked (depth 31)"
		if size.Refused != want || size.Refusals != 1 {
			t.Errorf("the walk names the refusal at the foot of a chain 31 levels deep as %q (%d bytes) with %d refusals, want %q, 1 refusal: the target, the name and the depth, and never the %d-byte path between",
				size.Refused, len(size.Refused), size.Refusals, want, len(footLocked))
		}
	}
	for _, size := range result.Sizes {
		if size.Path != auxiliary && size.Path != locked && size.Path != deep && (size.Refused != "" || size.Refusals != 0) {
			t.Errorf("the walk marks %s, which it read whole, as read in part: %+v", size.Path, size)
		}
	}
	// Everything after the refusals was still measured: the walk went on.
	last := filepath.Join(root, "workspaces", "cybermonklabs")
	if !slices.ContainsFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == last }) {
		t.Errorf("the walk stopped at the refusal: it never measured %s", last)
	}
}

// A volume root the walk cannot open is named as what it did not measure, with the read the
// kernel refused, and nothing cut the walk, so it is not partial: the report still goes, and a
// reader is not told the walk ended early when it never began.
func TestAVolumeRootTheWalkCannotOpenIsNamedMissingAndIsNoEarlyEnd(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := filepath.Join(t.TempDir(), "gone")
	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())
	want := []shimwire.LauncherVolumeMissing{{Path: root, Reason: "openat: " + syscall.ENOENT.Error()}}
	if result.Partial || !slices.Equal(result.Missing, want) || len(result.Sizes) != 0 {
		t.Errorf("the walk of a root that is not there is partial %t, missing %q, with %d sizes; want not partial, missing %q alone, and no size",
			result.Partial, result.Missing, len(result.Sizes), want)
	}
}

// The walk keeps no more unmeasured directories than a report names, and counts the rest: the
// trimmer holds a walk for the hour its numbers are reported (trimmer.sizes), and a role can make
// any number of directories under /legion/scratch that the walk cannot open. The case makes
// thirty more of them than the bound, each a target the walk is refused, and the walk's own list
// holds the bound with the count of the rest beside it, each kept entry naming the read refused.
// The negative control is the walk keeping every one, as it did at b7085e21, where only the
// report's copy was cut: its list held all eighty.
func TestTheWalkKeepsAsManyUnmeasuredDirectoriesAsAReportNamesAndCountsTheRest(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which opens a mode-000 directory: a refused open cannot be staged for it")
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	const planted = shimwire.VolumeReportSizesMissing + 30
	for at := range planted {
		lockedUnder(t, filepath.Join(root, "scratch", fmt.Sprintf("sealed-%03d", at)), clock.at)
	}

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	if len(result.Missing) != shimwire.VolumeReportSizesMissing || result.MissingOmitted != planted-shimwire.VolumeReportSizesMissing {
		t.Errorf("the walk keeps %d unmeasured directories and counts %d more, want the %d a report names and the %d past them",
			len(result.Missing), result.MissingOmitted, shimwire.VolumeReportSizesMissing, planted-shimwire.VolumeReportSizesMissing)
	}
	for _, missing := range result.Missing {
		if missing.Reason != "openat: "+syscall.EACCES.Error() {
			t.Errorf("the walk names %s as unmeasured with %q, want the open the kernel refused", missing.Path, missing.Reason)
		}
	}
	if result.Partial {
		t.Error("a walk that went on past every directory it could not open is marked partial, which a reader reads as a walk that ended early")
	}
}

// The walk's budget is armed before it lists its targets, and the listing is cut by it as a
// measurement is: a role can make any number of directories under /legion/scratch, and the walk
// opens and lists each child of the volume root to find its targets. The budget here is spent
// before the walk begins, and the volume root holds two thousand children, eight batches of
// names, each with a directory of its own, and one child more, which the budget's clock makes as
// the walk arms it. The walk ends on its budget, measures nothing, and names what it did not
// measure, with no reason: the volume root alone where the budget's cancel reaches the listing
// while the root's own names are read
// (TestTheWalksBudgetCutsTheListingOfTheVolumeRootBetweenItsBatches), and otherwise each of the
// 2,001 children once, as the target it holds where the walk had listed the child and as itself
// where it had not, up to the report's cap and counted past it (cut). Where the cut lands is the
// scheduler's, since the budget cancels the walk from a goroutine of its own (run), and a cut
// after fifty children names only targets the walk had listed, so the case holds the walk to
// what it names and not to where the cut landed. The negative control is the walk arming its
// budget after the listing, as it did at b7085e21: it listed 2,000 children, the one made as the
// budget was armed not among them, and measured a target before it saw the budget spent in 49
// of 50 runs; it failed all 50.
func TestTheWalksBudgetCutsTheListingOfItsTargets(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	const children = 2000
	for at := range children {
		mkdirFixture(t, filepath.Join(root, fmt.Sprintf("child-%04d", at), "inner"))
	}
	measurements := 0
	walk := sizeWalk{root: root, now: clock.now, budget: volumeWalkBudget,
		after: func(time.Duration) (<-chan time.Time, func()) {
			// One child more, made as the walk arms its budget: a walk that lists its targets
			// after arming the budget lists this child too, and one that armed it after its
			// listing does not.
			mkdirFixture(t, filepath.Join(root, fmt.Sprintf("child-%04d", children), "inner"))
			spent := make(chan time.Time)
			close(spent)
			return spent, func() {}
		},
		measure: func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
			measurements++
			return measureTree(ctx, dir, name, at)
		},
	}

	result := walk.run(context.Background())

	if !result.Partial || len(result.Sizes) != 0 || result.SizesOmitted != 0 || measurements != 0 {
		t.Errorf("the walk whose budget was spent before it began is partial %t with %d sizes kept and %d counted after %d measurements, want partial and nothing measured",
			result.Partial, len(result.Sizes), result.SizesOmitted, measurements)
	}
	if result.MissingOmitted == 0 && slices.Equal(result.Missing, []shimwire.LauncherVolumeMissing{{Path: root}}) {
		return
	}
	named := map[string]bool{}
	for _, missing := range result.Missing {
		child := missing.Path
		if filepath.Base(missing.Path) == "inner" {
			child = filepath.Dir(missing.Path)
		}
		if filepath.Dir(child) != root || missing.Reason != "" || named[child] {
			t.Errorf("the walk names %+v as unmeasured, want each child of the root once, as itself or as the target it holds, with no reason", missing)
		}
		named[child] = true
	}
	if total := len(result.Missing) + result.MissingOmitted; total != children+1 {
		t.Errorf("the walk names %d entries as unmeasured and counts %d more, %d in all, want the volume root alone or one for each of the %d children the root held when the budget was armed",
			len(result.Missing), result.MissingOmitted, total, children+1)
	}
}

// The walk's budget cuts the listing of one child of the volume root between the batches of the
// child's names, and not only before the child is opened: a role may make any number of names in
// one child, /legion/scratch above all, and a listing that asked only before each child read
// every one of them past the walk's minute. The case lists a volume root of three children, go,
// scratch and uv, where scratch holds two thousand directories, eight batches of names, through
// a context that answers live to its first four asks — the one the walk makes after the one
// batch of the root's own names, the one before it lists go, the one after go's one batch (a
// listing of fewer names than a batch answers them with no error, and the read after them
// answers the end), and the one before it lists scratch — and cancelled to every ask after, the
// first of which the walk makes once it has read the first batch of scratch's names. The
// listing is partial, answers no target to measure, and names three directories as unmeasured:
// go/build, the target it had listed, scratch whole, and uv, which it never listed. With three
// live asks the walk is cut before it opens scratch and reads none of scratch's batches, so the
// cut between a child's batches is the fourth ask's to reach. The negative control is readChild
// reading a child's names whole, as it did at ba4e3d98, beside the root's listing as it stands
// here: the walk asks nothing inside go or scratch, its four live asks run out at none of its
// children, and the listing is not partial, with 2,002 targets to measure.
func TestTheWalksBudgetCutsTheListingOfOneChildBetweenItsBatches(t *testing.T) {
	t.Parallel()
	const names = 2000
	root := t.TempDir()
	mkdirFixture(t, filepath.Join(root, "go", "build"))
	mkdirFixture(t, filepath.Join(root, "uv", "cache"))
	for at := range names {
		mkdirFixture(t, filepath.Join(root, "scratch", fmt.Sprintf("%04d", at)))
	}
	held, err := openVolumeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	ctx := &liveFor{Context: context.Background(), asks: 4}

	targets, result := sizeWalk{root: root}.targets(ctx, held)

	if !result.Partial || len(targets) != 0 {
		t.Errorf("the listing cut inside scratch is partial %t with %d targets to measure, want partial and none", result.Partial, len(targets))
	}
	want := []shimwire.LauncherVolumeMissing{
		{Path: filepath.Join(root, "go", "build")},
		{Path: filepath.Join(root, "scratch")},
		{Path: filepath.Join(root, "uv")},
	}
	if total := len(result.Missing) + result.MissingOmitted; total != len(want) || !slices.Equal(result.Missing, want) {
		t.Errorf("the listing names %d directories as unmeasured, beginning %v, want %v: the target it had listed, scratch whole and the child it never listed",
			total, result.Missing[:min(3, len(result.Missing))], want)
	}
}

// The walk's budget cuts the listing of the volume root itself between the batches of its names:
// /legion is writable by the role group (readBatched), so a role may make any number of names
// directly in it, and a root listing read whole read every one of them past the walk's minute.
// The case lists a volume root of six hundred files, three batches of names, through a context
// that answers live to its first ask, the one the walk makes after the first batch, and cancelled
// to every ask after, the first of which comes after the second batch. The listing is partial,
// answers no target to measure, and names the volume root alone as unmeasured, with no reason, as
// a cut walk names what it did not reach. The negative control is the root's listing at
// ba4e3d98, read whole and not cut: the listing was cut only before the root's second child, and
// named the first child, a target it had listed, and the other 599 children, 600 in all.
func TestTheWalksBudgetCutsTheListingOfTheVolumeRootBetweenItsBatches(t *testing.T) {
	t.Parallel()
	const names = 600
	root := t.TempDir()
	for at := range names {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", at)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held, err := openVolumeDir(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	ctx := &liveFor{Context: context.Background(), asks: 1}

	targets, result := sizeWalk{root: root}.targets(ctx, held)

	if !result.Partial || len(targets) != 0 {
		t.Errorf("the listing cut inside the root's own names is partial %t with %d targets to measure, want partial and none", result.Partial, len(targets))
	}
	want := []shimwire.LauncherVolumeMissing{{Path: root}}
	if total := len(result.Missing) + result.MissingOmitted; total != len(want) || !slices.Equal(result.Missing, want) {
		t.Errorf("the listing names %d directories as unmeasured, beginning %v, want %v: the volume root alone",
			total, result.Missing[:min(3, len(result.Missing))], want)
	}
}

// A listing nothing cuts keeps every batch of its names, and not only the first: a child of the
// volume root holding more names than a batch (walkEntriesBatch) is listed whole, and each of its
// directories is a target the walk measures. The case walks a volume root whose scratch holds
// three hundred directories, two batches of names, under a budget that never runs out. The
// negative control is readBatched keeping its first batch alone: the walk measured 256 of the
// three hundred, named none of the rest as unmeasured and was not partial.
func TestTheWalkListsEveryBatchOfAChildItIsNotCutIn(t *testing.T) {
	t.Parallel()
	const dirs = 300
	if dirs <= walkEntriesBatch {
		t.Fatalf("the case's scratch holds %d directories, want more than the %d of one batch", dirs, walkEntriesBatch)
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	want := make([]string, 0, dirs)
	for at := range dirs {
		name := fmt.Sprintf("%04d", at)
		mkdirFixture(t, filepath.Join(root, "scratch", name))
		want = append(want, name)
	}
	var got []string
	result := walkOf(root, clock, &fakeTicks{}, func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		got = append(got, name)
		return measureTree(ctx, dir, name, at)
	}).run(context.Background())

	if !slices.Equal(got, want) {
		t.Errorf("the walk measured %d targets, %v … %v, want the %d directories of scratch in the order they sort",
			len(got), got[:min(3, len(got))], got[max(0, len(got)-3):], dirs)
	}
	if total := len(result.Sizes) + result.SizesOmitted; result.Partial || len(result.Missing) != 0 || total != dirs {
		t.Errorf("the walk is partial %t, names %d entries as unmeasured and carries %d sizes, want %d sizes and nothing unmeasured",
			result.Partial, len(result.Missing), total, dirs)
	}
}

// liveFor is a context whose Err answers nil to its first asks and context.Canceled to every ask
// after them: the walk's budget running out at a point of the listing a case chooses, since the
// listing asks Err between the batches of the root's own names, before each child of the root
// and between the batches of one child's names (sizeWalk.targets, readBatched).
type liveFor struct {
	context.Context
	asks, asked int
}

func (c *liveFor) Err() error {
	c.asked++
	if c.asked > c.asks {
		return context.Canceled
	}
	return nil
}

// A path openVolumeDir refuses is quoted in its error, as every error of the trimmer's that
// names a path is (quotedPathText): a name holding a newline would otherwise begin a line of its
// own in the pod's log.
func TestAPathOpenVolumeDirRefusesIsQuotedInItsError(t *testing.T) {
	t.Parallel()
	const relative = "scratch\nthe trimmer is fine"
	_, err := openVolumeDir(relative)
	if !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("openVolumeDir(%q) answered %v, want %v for a path that is not absolute", relative, err, syscall.EINVAL)
	}
	if want := "open " + strconv.Quote(relative) + ": " + syscall.EINVAL.Error(); err.Error() != want {
		t.Errorf("openVolumeDir(%q) answered %q, want %q", relative, err, want)
	}
}

// Nothing outside the volume root is measured or named, whatever the tree under the root becomes
// while the walk runs. /legion/scratch is writable by the role uid (`drwx--S--- legion legion`,
// measured in an issue pod of this deployment at 2026-11-11T08:05Z), and the launcher walks in
// the container's own mount namespace: a role that renames a child of the root and leaves a
// symbolic link in its place after the walk has listed it used to aim every target under that
// child at a tree of its own, whose bytes then went to the issue's architect in the trim
// report's sizes (INFRA-2009, review 6, the same shape as the sweep's).
//
// The link shape this case leaves is the one that tells the open the walk makes now from the one
// it replaced: `scratch -> zz/sh` beside `zz -> /bait`, a relative link that stays lexically
// inside the volume root crossing an absolute one that leaves it. An absolute link at the
// swapped child reads the same against the walk this change replaced and against this one, since
// os.Root refused one for a final element on its own; this shape does not. os.Root followed a
// final element whose link stayed lexically inside the root and then resolved that link's own
// elements with the `openat(dirfd, name, O_RDONLY|O_DIRECTORY|O_NOFOLLOW)` this kernel follows a
// link under, so the walk landed in /bait/sh and measured a tree that is not on the volume
// (volumedir.go, where both are measured). The open each level of the walk makes now carries
// O_NOFOLLOW and no O_DIRECTORY and reads the kind off the descriptor's own fstat, so a child
// that is a link is refused whatever it names and whatever it crosses (dirHandle.open), and
// TestAShardSwappedForARelativeLinkCrossingAnAbsoluteOneIsSkipped drives this shape for the
// sweep's shards.
func TestTheWalkMeasuresNothingThroughAChildSwappedForARelativeLinkCrossingAnAbsoluteOne(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	// The absolute link the relative one crosses, left in the volume root as a role can leave
	// one: the root's own listing reads it as the link it is and opens it never, so it is the
	// relative link's crossing of it that has to be refused.
	bait := t.TempDir()
	if err := os.Symlink(bait, filepath.Join(root, "zz")); err != nil {
		t.Fatal(err)
	}
	// The tree under the name the relative link crosses into, whose directories carry the names
	// the walk has just listed under scratch, so that every target the walk is holding resolves
	// into it the moment the link is in place. A role names both trees, so matching them costs
	// it nothing.
	const baitFileBytes = 64 * 1024
	baitBytes := int64(0)
	for _, name := range []string{"bun", "cache", "tmp"} {
		under := filepath.Join(bait, "sh", name)
		mkdirFixture(t, under)
		if err := os.WriteFile(filepath.Join(under, "secrets.json"), make([]byte, baitFileBytes), 0o600); err != nil {
			t.Fatal(err)
		}
		baitBytes += baitFileBytes
	}
	if baitBytes == 0 {
		t.Fatal("the bait tree holds no bytes at all, so a walk that read it whole would look like one that read none of it")
	}

	// The swap is made at the first measurement: the walk has read the root and
	// the root's scratch by then and holds the names it will measure, and nothing it does
	// afterwards may resolve one of them through the link.
	swapped := filepath.Join(root, volumeScratchDir)
	swaps := 0
	measure := func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		if swaps == 0 {
			swaps++
			if err := os.Rename(swapped, swapped+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("zz", "sh"), swapped); err != nil {
				t.Fatal(err)
			}
		}
		return measureTree(ctx, dir, name, at)
	}

	result := walkOf(root, clock, &fakeTicks{}, measure).run(context.Background())

	if swaps != 1 {
		t.Fatalf("the swap was made %d times, want the one the walk's first measurement makes: the case staged nothing", swaps)
	}
	// Every target of the swapped child is accounted for, and no byte and no name of the tree
	// the two links lead to is in the report.
	accounted, inside := 0, swapped+string(filepath.Separator)
	var measured int64
	for _, size := range result.Sizes {
		measured += size.Bytes
		if strings.HasPrefix(size.Path, inside) {
			accounted++
		}
		if strings.HasPrefix(size.Path, bait) {
			t.Errorf("the walk names %s in its sizes, which is outside the volume root %s", size.Path, root)
		}
		if size.Bytes >= baitFileBytes {
			t.Errorf("the walk measured %s at %d B, and no directory of this volume holds a file over one byte: it read through the relative link and across the absolute one, into %s", size.Path, size.Bytes, bait)
		}
	}
	for _, missing := range missingPaths(result.Missing) {
		if strings.HasPrefix(missing, inside) {
			accounted++
		}
		if strings.HasPrefix(missing, bait) {
			t.Errorf("the walk names %s as missing, which is outside the volume root %s", missing, root)
		}
	}
	if accounted == 0 {
		t.Fatalf("the walk neither measured nor missed one directory under %s, so the link stood in front of nothing it did: %+v, missing %v",
			swapped, result.Sizes, result.Missing)
	}
	if measured >= baitFileBytes {
		t.Errorf("the walk measured %d B over %d directories, and every file the volume itself holds is one byte: it read the %d B tree the links lead to",
			measured, len(result.Sizes), baitBytes)
	}
	if result.Partial {
		t.Error("a walk that named the targets it could no longer reach as missing and went on to its end is marked partial, which a reader reads as a walk that ended early")
	}
}

// The walk is made at most once an hour, and a report between walks carries the last walk's
// numbers with the time they were read: the walk stats every file on the volume, and a pod that
// is full sweeps every interval.
func TestNoSecondWalkIsMadeInsideTheHour(t *testing.T) {
	t.Parallel()
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	calls := 0
	tr.measure = func(_ context.Context, _ *dirHandle, _ string, _ time.Time) measured {
		calls++
		return measured{size: shimwire.LauncherVolumeSize{Bytes: int64(calls)}}
	}

	first := tr.sizes(context.Background())
	if calls == 0 {
		t.Fatal("the first walk measured nothing at all")
	}
	made := calls
	second := tr.sizes(context.Background())
	if calls != made {
		t.Errorf("the trimmer measured %d directories on a second walk inside the hour, want it to answer with the first walk's numbers", calls-made)
	}
	if !second.At.Equal(first.At) || !slices.EqualFunc(second.Sizes, first.Sizes, func(a, b shimwire.LauncherVolumeSize) bool { return a == b }) {
		t.Errorf("the second answer is %+v at %s, want the first walk's %+v at %s", second.Sizes, second.At, first.Sizes, first.At)
	}

	// A report made between walks carries those same numbers with the time they were read.
	report := tr.report(context.Background(), sweepOutcome{reason: "a sweep that ran between walks"})
	if !report.SizesAt.Equal(first.At) {
		t.Errorf("the report carries sizes read at %s, want the %s of the last walk", report.SizesAt, first.At)
	}
	if len(report.Sizes) != len(first.Sizes) {
		t.Errorf("the report carries %d sizes, want the %d of the last walk", len(report.Sizes), len(first.Sizes))
	}

	f.clock.advance(volumeWalkInterval + time.Minute)
	third := tr.sizes(context.Background())
	if calls == made {
		t.Error("the trimmer made no new walk an hour after the last one")
	}
	if third.At.Equal(first.At) {
		t.Errorf("the walk an hour later was read at %s, the same as the first: it is the same measurement", third.At)
	}
}

// The walk an hour old is made again, and one a second younger is not: the trimmer walks again
// once the last walk is an hour old and answers with the last one while it is less (sizes). The
// case above moves the clock an hour and a minute, and reads the walk again at once, with which a
// walk made again only past the hour, or a minute short of it, is as green. The clock does not
// move on reading, so every reading of it is where the case put it.
func TestAWalkAnHourOldIsMadeAgainAndOneASecondYoungerIsNot(t *testing.T) {
	t.Parallel()
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	calls := 0
	tr.measure = func(_ context.Context, _ *dirHandle, _ string, _ time.Time) measured {
		calls++
		return measured{size: shimwire.LauncherVolumeSize{Bytes: int64(calls)}}
	}

	first := tr.sizes(context.Background())
	made := calls
	if made == 0 {
		t.Fatal("the first walk measured nothing at all")
	}
	f.clock.advance(time.Hour - time.Second)
	if young := tr.sizes(context.Background()); calls != made || !young.At.Equal(first.At) {
		t.Errorf("the trimmer walked again %s after the last walk, want the last walk's numbers until it is an hour old", f.clock.now().Sub(first.At))
	}
	f.clock.advance(time.Second)
	if age := f.clock.now().Sub(first.At); age != time.Hour {
		t.Fatalf("the last walk is %s old, want exactly the hour this case stands on", age)
	}
	if old := tr.sizes(context.Background()); calls == made || !old.At.Equal(first.At.Add(time.Hour)) {
		t.Errorf("the trimmer answered a walk read at %s an hour after the walk read at %s, having measured %d more targets, want a new walk", old.At, first.At, calls-made)
	}
}

// The walk a report carries is given a budget of a minute: R4 has the size walk "bounded at sixty
// seconds", and the cases of the budget arm their walks with volumeWalkBudget and read the budget
// a walk asked for against it (TestTheWalkEndsOnItsBudgetAgainstAWalkerThatBlocks), with which a
// budget of any length is as green. The trimmer's own walk is made here (sizes), and the budget it
// asked the clock for is read against the minute written out: a budget a second either side of it
// is a clock of another period.
func TestTheTrimmersWalkIsGivenABudgetOfAMinute(t *testing.T) {
	t.Parallel()
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	before := f.ticks.count()
	if walk := tr.sizes(context.Background()); walk.Partial {
		t.Fatalf("the walk was cut, want it whole: the budget's clock never ticks in this case: %+v", walk)
	}
	if made := f.ticks.count() - before; made != 1 {
		t.Fatalf("the walk made %d clocks, want the one its budget runs on", made)
	}
	if period := f.ticks.periodOf(before); period != time.Minute {
		t.Errorf("the trimmer's walk asked for a budget of %s, want a minute", period)
	}
}

// walkLines is every line the trimmer wrote of a size walk.
func walkLines(f *trimFixture) []string {
	var lines []string
	for _, line := range f.linesWritten() {
		if strings.Contains(line, "the size walk") {
			lines = append(lines, line)
		}
	}
	return lines
}

// lockedUnder makes a directory of the case's tree mode 0000, with one file in it, and gives it
// back its mode when the case ends, so the tree can be removed.
func lockedUnder(t *testing.T, dir string, at time.Time) {
	t.Helper()
	mkdirFixture(t, dir)
	writeAged(t, filepath.Join(dir, "two"), at)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// The report and the log line of a walk that met a refused read say what it met and no more.
// The walk reached its end, so neither says that it ended early, and the directory it read only
// in part is named in both with the read refused there, its bytes given as what the walk could
// read. Nothing went unmeasured, so neither carries a list of unmeasured directories: the line
// used to say `ended early` and `left 0 unmeasured ([], and 0 more)` of exactly this walk, and
// named the unreadable directory nowhere.
func TestAWalkThatMetARefusedReadSaysWhereAndNotThatItEndedEarly(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	f := newTrimFixture(t, 1)
	pack := filepath.Join(f.root, "unprotected", "pack")
	refused := filepath.Join(pack, "under")
	mkdirFixture(t, pack)
	writeAged(t, filepath.Join(pack, "issue.json"), f.clock.at)
	lockedUnder(t, refused, f.clock.at)
	tr := f.trimmer()

	report := tr.report(context.Background(), sweepOutcome{reason: "the sweep this walk's report goes with"})

	if report.SizesPartial || len(report.SizesMissing) != 0 || report.SizesMissingOmitted != 0 {
		t.Errorf("the report of a walk that reached its end is partial %t and names %q and %d more as unmeasured, want neither",
			report.SizesPartial, report.SizesMissing, report.SizesMissingOmitted)
	}
	partly := 0
	for _, size := range report.Sizes {
		if size.Refused == "" && size.Refusals == 0 {
			continue
		}
		partly++
		if size.Path != pack || size.Refused != refused || size.Refusals != 1 || size.Bytes != 1 {
			t.Errorf("the report carries %+v, want %s at the 1 B the walk could read, refused once at %s", size, pack, refused)
		}
	}
	if partly != 1 {
		t.Errorf("the report marks %d sizes as read in part, want the one under which a read was refused: %+v", partly, report.Sizes)
	}
	lines := walkLines(f)
	if len(lines) != 1 {
		t.Fatalf("the trimmer wrote %d lines of the walk, want one naming the directory it read in part: %q", len(lines), f.linesWritten())
	}
	for _, want := range []string{"measured", "only in part", strconv.Quote(pack) + " at " + strconv.Quote(refused)} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the walk's line does not say %q: %s", want, lines[0])
		}
	}
	for _, not := range []string{"ended early", "was cut", "unmeasured", "[]"} {
		if strings.Contains(lines[0], not) {
			t.Errorf("the walk's line says %q of a walk that reached its end with nothing unmeasured: %s", not, lines[0])
		}
	}
}

// A walk its budget cut is the one that ended early, and its report and its log line say so,
// with what it did not reach: the target it was cut in and every one after it. The line says
// the walk was cut before it reached every target, and not every directory, since a regular
// file can be a target of its own (sizeWalk.targets); the negative control for that word is the
// line as it stood at a3f5d0965f3f, which said "every directory".
func TestAWalkThatWasCutSaysSoWithWhatItDidNotReach(t *testing.T) {
	t.Parallel()
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	calls := 0
	tr.measure = func(ctx context.Context, dir *dirHandle, name string, at time.Time) measured {
		calls++
		if calls > 2 {
			return measured{cut: true}
		}
		return measureTree(ctx, dir, name, at)
	}

	report := tr.report(context.Background(), sweepOutcome{reason: "the sweep this walk's report goes with"})

	if !report.SizesPartial || len(report.Sizes) != 2 || len(report.SizesMissing) == 0 {
		t.Fatalf("the report of a walk cut at its third target is partial %t with %d sizes and %q unmeasured, want partial, the two it measured and the rest named",
			report.SizesPartial, len(report.Sizes), report.SizesMissing)
	}
	lines := walkLines(f)
	if len(lines) != 1 {
		t.Fatalf("the trimmer wrote %d lines of the walk, want one: %q", len(lines), f.linesWritten())
	}
	for _, want := range []string{"was cut by its budget or the container's stop before it reached every target",
		"measured 2 sizes", fmt.Sprintf("left %d unmeasured", len(report.SizesMissing)), strconv.Quote(report.SizesMissing[0].Path)} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the walk's line does not say %q: %s", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "only in part") {
		t.Errorf("the walk's line says a directory was read in part, and no read was refused: %s", lines[0])
	}
}

// A role can make any number of directories under /legion/scratch, and each can hold one the
// walk is refused: the report carries each refused read under this launcher's bound on a path,
// and the log line names no more of them than a report carries sizes, saying how many it left
// out. The names are the 255 bytes a directory component allows, so an unbounded line or path
// is one a role sets the size of. Each planted directory holds a 1 MiB file beside the one it is
// refused, so the fifty sizes the walk keeps (walkResult.keep) are planted ones and not the
// fixture's own few bytes.
func TestDirectoriesReadInPartAreBoundedInTheReportAndTheLog(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	f := newTrimFixture(t, 1)
	scratch := filepath.Join(f.root, "scratch")
	const planted = 2 * shimwire.VolumeReportSizes
	plantWalkTargets(t, scratch, planted)
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	refusedName := strings.Repeat("r", 255)
	locked := 0
	for _, entry := range entries {
		if len(entry.Name()) == 255 {
			sizedFile(t, filepath.Join(scratch, entry.Name(), "bulk"), 1<<20, f.clock.at)
			lockedUnder(t, filepath.Join(scratch, entry.Name(), refusedName), f.clock.at)
			locked++
		}
	}
	if locked != planted {
		t.Fatalf("the case locked a directory under %d of the %d it planted", locked, planted)
	}
	tr := f.trimmer()

	report := tr.report(context.Background(), sweepOutcome{reason: "the sweep this walk's report goes with"})

	if len(report.Sizes) != shimwire.VolumeReportSizes {
		t.Fatalf("the report carries %d sizes, want the %d this launcher bounds it to", len(report.Sizes), shimwire.VolumeReportSizes)
	}
	for _, size := range report.Sizes {
		if size.Refused == "" {
			continue
		}
		if runes := utf8.RuneCountInString(size.Refused); runes > volumeReportPathRunes || !strings.HasSuffix(size.Refused, "…") {
			t.Errorf("the report names a refused read of %d runes, %q, want it cut at the %d bound with an ellipsis", runes, size.Refused, volumeReportPathRunes)
		}
	}
	lines := walkLines(f)
	if len(lines) != 1 {
		t.Fatalf("the trimmer wrote %d lines of the walk, want one: %q", len(lines), f.linesWritten())
	}
	if listed := strings.Count(lines[0], `" at "`); listed != shimwire.VolumeReportSizes {
		t.Errorf("the walk's line names %d directories read in part, want the %d a report carries", listed, shimwire.VolumeReportSizes)
	}
	if want := fmt.Sprintf("%d of them only in part", planted); !strings.Contains(lines[0], want) {
		t.Errorf("the walk's line does not say %q: %.400s", want, lines[0])
	}
	if want := fmt.Sprintf(", and %d more)", planted-shimwire.VolumeReportSizes); !strings.Contains(lines[0], want) {
		t.Errorf("the walk's line does not say %q, how many it left out: %.400s", want, lines[0])
	}
}

// The volume root's two first children are named by the roots the trimmer sweeps: the walk's
// order is worth nothing if it names a directory the pod does not have.
func TestTheWalksFirstTwoChildrenAreTheOnesTheReclaimableRootsLieUnder(t *testing.T) {
	t.Parallel()
	under := map[string]int{}
	for _, root := range ReclaimableRoots {
		relative := root.Path[len(VolumeRoot)+1:]
		child, _, _ := strings.Cut(relative, "/")
		under[child]++
	}
	for _, want := range []string{volumeScratchDir, volumeGoDir} {
		if under[want] == 0 {
			t.Errorf("no reclaimable root lies under %s/%s, which the walk measures first: %v", VolumeRoot, want, ReclaimableRoots)
		}
	}
	if len(under) != 2 {
		t.Errorf("the reclaimable roots lie under %v, want the two children the walk measures first", under)
	}
}

// sizedFile makes a file of size bytes at path, sparse, aged at, with every directory above it:
// the walk counts a file's apparent size (countFile), so a case can stand megabytes on the
// volume without writing them.
func sizedFile(t *testing.T, path string, size int64, at time.Time) {
	t.Helper()
	mkdirFixture(t, filepath.Dir(path))
	writeAged(t, path, at)
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The walk keeps the largest sizes it measured, in the order it measured them, and no more of
// them than a report carries, counting the rest: the trimmer keeps a walk for the hour its
// numbers are reported (trimmer.sizes), and a role can make any number of directories under
// /legion/scratch, each a target, so a walk that kept every size held all of a role's for the
// hour and sorted them all at every report of it. It keeps the largest and not the first: the
// walk measures the scratch children before the go children, and fifty empty ones must not push
// the build cache out of the report. The case stands fifty-five empty directories under scratch
// and a 5 MiB file under go/build, and the walk the trimmer keeps must hold go/build and the
// forty-nine empty ones it measured first, in walk order, and the report carry those and say six
// were left out. The negative controls are the walk as it stood at 8a30be78, which kept all
// fifty-six for the hour and left the cut to the report's bound, and the cut as it stood at
// b7085e21, the first fifty, which carried fifty empty directories and left go/build out.
func TestTheWalkKeepsTheLargestSizesInTheOrderItMeasuredThem(t *testing.T) {
	t.Parallel()
	const empty, build = 55, 5 << 20
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	for at := range empty {
		mkdirFixture(t, filepath.Join(root, "scratch", fmt.Sprintf("empty-%02d", at)))
	}
	cache := filepath.Join(root, "go", "build")
	sizedFile(t, filepath.Join(cache, "ab", "abcdef-a"), build, clock.at.Add(-2*time.Hour))
	tr := newTrimFixture(t, 0).trimmer()
	tr.volumeRoot, tr.now = root, clock.now

	report := tr.report(context.Background(), sweepOutcome{reason: "the sweep this walk's report goes with"})

	var want []string
	for at := range shimwire.VolumeReportSizes - 1 {
		want = append(want, filepath.Join(root, "scratch", fmt.Sprintf("empty-%02d", at)))
	}
	want = append(want, cache)
	if got := pathsOf(tr.walk.Sizes); !slices.Equal(got, want) {
		t.Errorf("the walk the trimmer keeps for the hour holds %d sizes:\n%v\nwant the build cache and the first %d empty directories, in walk order:\n%v",
			len(got), got, shimwire.VolumeReportSizes-1, want)
	}
	if got := pathsOf(report.Sizes); !slices.Equal(got, want) || report.SizesOmitted != empty+1-shimwire.VolumeReportSizes {
		t.Errorf("the report carries\n%v\nand counts %d left out, want\n%v\nand %d", got, report.SizesOmitted, want, empty+1-shimwire.VolumeReportSizes)
	}
	if last := report.Sizes[len(report.Sizes)-1]; last.Bytes != build {
		t.Errorf("the report carries the build cache at %d bytes, want %d", last.Bytes, build)
	}
}

// The size walk holds a batch of one level's names at a time and not the whole level, as the
// guards' walk does (walkEntries): a role may make a directory of any number of names under
// /legion/scratch, and the walk runs in PID 1 below every target. The case measures a target
// whose one directory holds twenty thousand names of two hundred bytes, and reads the live heap
// after a collection at every two thousandth entry, through the walk's own question of its
// context before each entry it reads: it may rise by no more than a megabyte over the heap
// before the walk. The negative control is the level read whole, as fs.ReadDir read it at
// 8a30be78, which held every name's entry and lstat for as long as the walk was below that
// level.
func TestTheSizeWalkHoldsABatchOfALevelsNamesAndNotAllOfThem(t *testing.T) {
	// Not parallel: it measures the whole process's heap, which parallel tests allocate on.
	const names, sample, bound = 20000, 2000, 1 << 20
	dir := t.TempDir()
	wide := filepath.Join(dir, "target", "wide")
	mkdirFixture(t, wide)
	for at := range names {
		if err := os.WriteFile(filepath.Join(wide, fmt.Sprintf("%05d", at)+strings.Repeat("n", 195)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held, err := openVolumeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	live := func() uint64 {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	before := live()
	asked, peak := 0, before
	ctx := askedContext{Context: context.Background(), asked: func() {
		asked++
		if asked%sample == 0 {
			peak = max(peak, live())
		}
	}}
	out := measureTree(ctx, held, "target", time.Now())
	if out.unread != "" || out.refused != 0 || out.cut || out.size.Bytes != names {
		t.Fatalf("the walk read %+v, want the %d bytes of the %d files whole", out, names, names)
	}
	if asked != names+1 {
		t.Fatalf("the walk asked its context %d times, want once for the directory and once for each of its %d names", asked, names)
	}
	if rise := peak - before; rise > bound {
		t.Errorf("the live heap rose %d bytes over the walk of a directory of %d names, want no more than %d: the walk held the level whole", rise, names, bound)
	}
}

// askedContext is a context that calls asked whenever its Done is asked for, which the size walk
// does before each entry it reads (measureUnder): a case's one hook inside a walk.
type askedContext struct {
	context.Context
	asked func()
}

func (c askedContext) Done() <-chan struct{} {
	c.asked()
	return c.Context.Done()
}

// A regular file directly in an immediate child of the volume root that also holds directories
// is a size of its own: the walk measures the directories two levels down, and a file beside
// them would otherwise be in no size at all, bytes on the volume the report never names. The
// case stands a 7 MiB `scratch/big.img` beside `scratch/tmp`, and every file of the tree must lie
// under exactly one size. The negative control is the walk as it stood at b7085e21, which
// measured `scratch/tmp` alone and left the 7 MiB in nothing.
func TestAFileBesideTheDirectoriesOfAChildIsASizeOfItsOwn(t *testing.T) {
	t.Parallel()
	const big = 7 << 20
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	sizedFile(t, filepath.Join(root, "scratch", "tmp", "one"), 1, clock.at.Add(-2*time.Hour))
	image := filepath.Join(root, "scratch", "big.img")
	sizedFile(t, image, big, clock.at.Add(-2*time.Hour))
	if err := os.Symlink("tmp", filepath.Join(root, "scratch", "tmp-link")); err != nil {
		t.Fatal(err)
	}

	walk := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	want := []string{image, filepath.Join(root, "scratch", "tmp")}
	if got := pathsOf(walk.Sizes); !slices.Equal(got, want) {
		t.Fatalf("the walk measured %v, want %v: the file beside the directory, and no size for the link, which holds no bytes", got, want)
	}
	if walk.Sizes[0].Bytes != big || walk.Sizes[0].BytesOverHour != big {
		t.Errorf("the walk measured %s as %+v, want its %d bytes, all older than an hour", image, walk.Sizes[0], big)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		under := 0
		for _, size := range walk.Sizes {
			if path == size.Path || strings.HasPrefix(path, size.Path+"/") {
				under++
			}
		}
		if under != 1 {
			t.Errorf("%s lies under %d of the sizes the walk measured, want exactly 1", path, under)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The walk descends no more than volumeWalkDepth levels below a target: each level it descends
// holds a handle on the directory until the level is done, in PID 1, and a role can make a chain
// of directories of any depth. A directory deeper than that is not opened; it is a refused
// reading of the target, named by its name and depth as a refusal of the kernel's is, and the
// walk goes on to its end. The case stands a chain one level deeper than the walk descends, with
// a file in the deepest directory the walk opens, which it counts, and one in the directory past
// it, which it does not. The negative control is the walk with no bound on its descent, which
// opened every level, counted both files and named no refusal.
func TestTheWalkDescendsNoDeeperThanItsDepthAndCountsTheRestRefused(t *testing.T) {
	t.Parallel()
	const reached, past = 3, 5
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	target := filepath.Join(root, "scratch", "deep")
	chain := target
	for level := 1; level <= volumeWalkDepth+1; level++ {
		chain = filepath.Join(chain, fmt.Sprintf("d%02d", level))
		if level == volumeWalkDepth {
			sizedFile(t, filepath.Join(chain, "reached"), reached, clock.at.Add(-2*time.Hour))
		}
	}
	sizedFile(t, filepath.Join(chain, "past"), past, clock.at.Add(-2*time.Hour))

	walk := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	if walk.Partial || len(walk.Missing) != 0 {
		t.Errorf("the walk is partial %t and missing %v, want it at its end with nothing unmeasured: a level it does not descend to ends nothing", walk.Partial, walk.Missing)
	}
	at := slices.IndexFunc(walk.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == target })
	if at < 0 {
		t.Fatalf("the walk did not measure %s: %+v", target, walk.Sizes)
	}
	size := walk.Sizes[at]
	if size.Bytes != reached {
		t.Errorf("the walk measured %d bytes under the chain, want the %d of the file at the deepest level it opens and not the %d past it", size.Bytes, reached, past)
	}
	if want := refusedPath(target, fmt.Sprintf("d%02d", volumeWalkDepth+1), volumeWalkDepth+1); size.Refused != want || size.Refusals != 1 {
		t.Errorf("the walk names %q with %d refusals, want %q and 1: the directory past its depth, by its name and depth", size.Refused, size.Refusals, want)
	}
}
