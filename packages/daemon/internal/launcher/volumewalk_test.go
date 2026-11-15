//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
	// The pod's feed: a read-only mount of files inside the same claim, with no directory of
	// its own, so the walk measures the child itself and nothing on the volume goes unaccounted.
	mkdirFixture(t, filepath.Join(root, "feed"))
	writeAged(t, filepath.Join(root, "feed", "issue.json"), at)
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
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	want := []string{
		// scratch first, its children as they sort
		"scratch/bun", "scratch/cache", "scratch/tmp",
		// then go
		"go/build", "go/mod",
		// then the rest, in the order the root's own children sort
		"feed", "lost+found.txt", "uv/cache", "uv/python", "workspaces/cybermonklabs",
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

// The walk is bounded at sixty seconds, and a walker that blocks is cut where it stands: the
// sizes it did produce are the first in the order the rule names, and everything it had not
// reached is named as missing rather than reported as a directory of no bytes.
func TestTheWalkEndsOnItsBudgetAgainstAWalkerThatBlocks(t *testing.T) {
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
	if !ticks.offer(t, 0, testwait.Bounded(t, 10*time.Second, time.Second)) {
		t.Fatal("the walk is not waiting on its budget: nothing took the tick")
	}
	var result walkResult
	select {
	case result = <-results:
	case <-time.After(testwait.Bounded(t, 10*time.Second, time.Second)):
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

// A read the kernel refuses under a target ends nothing and is no early end: the pod's feed is a
// read-only mount inside the same claim, so the walk reaches it and its bytes are already in the
// statfs reading the marks are taken from. The walk goes on to its end, so it is not partial,
// and each directory it could read only in part is a size that names the first read refused
// there and how many were, so that its bytes are never read as the directory's whole. Four
// shapes are staged: two mode-0000 directories under the feed's own directory, which the walk
// reads the rest of; a mode-0000 directory at the foot of a chain thirty directories deep, which
// is named by the target, its own name and its depth and never by the path between, as the
// deleter's removal failure is (removalFailure); a target that is itself mode 0000, whose lstat
// in its readable parent succeeds and whose open is refused, so the walk reads nothing of it and
// names it as unmeasured with the read the kernel refused, and as no size; and an immediate
// child of the root that is mode 0000, which the walk cannot list and names as unmeasured the
// same way. The negative control is every other target of the tree, which carries no refusal.
func TestARefusedReadUnderATargetIsNamedOnItsSizeAndIsNoEarlyEnd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	// The feed as workspace-fetch writes it: a directory of its own under the mount, which is
	// what the walk measures, with two directories under that the reader is refused.
	feed := filepath.Join(root, "feed", "pack")
	mkdirFixture(t, feed)
	writeAged(t, filepath.Join(feed, "issue.json"), clock.at.Add(-2*time.Hour))
	first, second := filepath.Join(feed, "a-locked"), filepath.Join(feed, "b-locked")
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
	if size, ok := sizeOf(feed); ok && (size.Bytes != 1 || size.Refused != first || size.Refusals != 2) {
		t.Errorf("the walk measured the feed as %+v, want the 1 B it could read, refused first at %s and 2 refusals in all", size, first)
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
		if size.Path != feed && size.Path != locked && size.Path != deep && (size.Refused != "" || size.Refusals != 0) {
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
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which opens a mode-000 directory: a refused open cannot be staged for it")
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	const planted = volumeReportSizesMissing + 30
	for at := range planted {
		lockedUnder(t, filepath.Join(root, "scratch", fmt.Sprintf("sealed-%03d", at)), clock.at)
	}

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	if len(result.Missing) != volumeReportSizesMissing || result.MissingOmitted != planted-volumeReportSizesMissing {
		t.Errorf("the walk keeps %d unmeasured directories and counts %d more, want the %d a report names and the %d past them",
			len(result.Missing), result.MissingOmitted, volumeReportSizesMissing, planted-volumeReportSizesMissing)
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
// before the walk begins, and the volume root holds two thousand children, each with a
// directory of its own: the walk ends on its budget part way through the listing, measures
// nothing, and names among what it did not measure a child of the root it never listed, by the
// child's own path. The negative control is the walk arming its budget after the listing, as it
// did at b7085e21: it listed every child and named only the targets two levels down.
func TestTheWalksBudgetCutsTheListingOfItsTargets(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := t.TempDir()
	const children = 2000
	for at := range children {
		mkdirFixture(t, filepath.Join(root, fmt.Sprintf("child-%04d", at), "inner"))
	}
	measurements := 0
	walk := sizeWalk{root: root, now: clock.now, budget: volumeWalkBudget,
		after: func(time.Duration) (<-chan time.Time, func()) {
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

	if !result.Partial || len(result.Sizes) != 0 || measurements != 0 {
		t.Errorf("the walk whose budget was spent before it began is partial %t with %d sizes after %d measurements, want partial and nothing measured",
			result.Partial, len(result.Sizes), measurements)
	}
	unlisted := slices.ContainsFunc(result.Missing, func(missing shimwire.LauncherVolumeMissing) bool {
		return filepath.Dir(missing.Path) == root
	})
	if !unlisted {
		t.Errorf("the walk names %d of the %d children's targets as unmeasured, all two levels down: it listed every child after its budget ran out", len(result.Missing)+result.MissingOmitted, children)
	}
	if total := len(result.Missing) + result.MissingOmitted; total != children {
		t.Errorf("the walk names %d directories as unmeasured, want one for each of the %d children, listed or not", total, children)
	}
}

// A path openVolumeDir refuses is quoted in its error, as every error of the trimmer's that
// names a path is (quotedPathText): a name holding a newline would otherwise begin a line of its
// own in the pod's log.
func TestAPathOpenVolumeDirRefusesIsQuotedInItsError(t *testing.T) {
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
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	f := newTrimFixture(t, 1)
	pack := filepath.Join(f.root, "feed", "pack")
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
// with what it did not reach: the target it was cut in and every one after it.
func TestAWalkThatWasCutSaysSoWithWhatItDidNotReach(t *testing.T) {
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
	for _, want := range []string{"was cut by its budget or the container's stop before it reached every directory",
		"measured 2 directories", fmt.Sprintf("left %d unmeasured", len(report.SizesMissing)), strconv.Quote(report.SizesMissing[0].Path)} {
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
// is one a role sets the size of.
func TestDirectoriesReadInPartAreBoundedInTheReportAndTheLog(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	f := newTrimFixture(t, 1)
	scratch := filepath.Join(f.root, "scratch")
	const planted = 2 * volumeReportSizes
	plantWalkTargets(t, scratch, planted)
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	refusedName := strings.Repeat("r", 255)
	locked := 0
	for _, entry := range entries {
		if len(entry.Name()) == 255 {
			lockedUnder(t, filepath.Join(scratch, entry.Name(), refusedName), f.clock.at)
			locked++
		}
	}
	if locked != planted {
		t.Fatalf("the case locked a directory under %d of the %d it planted", locked, planted)
	}
	tr := f.trimmer()

	report := tr.report(context.Background(), sweepOutcome{reason: "the sweep this walk's report goes with"})

	if len(report.Sizes) != volumeReportSizes {
		t.Fatalf("the report carries %d sizes, want the %d this launcher bounds it to", len(report.Sizes), volumeReportSizes)
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
	if listed := strings.Count(lines[0], `" at "`); listed != volumeReportSizes {
		t.Errorf("the walk's line names %d directories read in part, want the %d a report carries", listed, volumeReportSizes)
	}
	if want := fmt.Sprintf("%d of them only in part", planted); !strings.Contains(lines[0], want) {
		t.Errorf("the walk's line does not say %q: %.400s", want, lines[0])
	}
	if want := fmt.Sprintf(", and %d more)", planted-volumeReportSizes); !strings.Contains(lines[0], want) {
		t.Errorf("the walk's line does not say %q, how many it left out: %.400s", want, lines[0])
	}
}

// The volume root's two first children are named by the roots the trimmer sweeps: the walk's
// order is worth nothing if it names a directory the pod does not have.
func TestTheWalksFirstTwoChildrenAreTheOnesTheReclaimableRootsLieUnder(t *testing.T) {
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
