//go:build linux

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
func walkOf(root string, clock *fakeClock, ticks *fakeTicks, measure func(context.Context, *os.Root, string, time.Time) measured) sizeWalk {
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
	measure := func(ctx context.Context, _ *os.Root, _ string, _ time.Time) measured {
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
	if len(result.Missing) == 0 || result.Missing[0] != filepath.Join(root, "scratch", "tmp") {
		t.Errorf("the walk named %v as missing, want the directory it was cut in and everything after it", result.Missing)
	}
	if slices.Contains(result.Missing, want[0]) {
		t.Errorf("the walk named %s both measured and missing", want[0])
	}
}

// A read the kernel refuses beneath a directory names a partial reading and ends nothing: the
// pod's feed is a read-only mount inside the same claim, so the walk reaches it and its bytes
// are already in the statfs reading the marks are taken from. A directory the walk cannot list
// at all is named as missing, and neither stops the walk or the report.
func TestARefusedReadMakesTheWalkPartialAndEndsNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: a refused read cannot be staged for it")
	}
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	// The feed as workspace-fetch writes it: a directory of its own under the mount, which is
	// what the walk measures, with a directory under that the reader is refused.
	feed := filepath.Join(root, "feed", "pack")
	mkdirFixture(t, feed)
	writeAged(t, filepath.Join(feed, "issue.json"), clock.at.Add(-2*time.Hour))
	refused := filepath.Join(feed, "under")
	mkdirFixture(t, refused)
	writeAged(t, filepath.Join(refused, "two"), clock.at.Add(-2*time.Hour))
	if err := os.Chmod(refused, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(refused, 0o755) })

	result := walkOf(root, clock, &fakeTicks{}, measureTree).run(context.Background())

	if !result.Partial {
		t.Error("a walk that was refused a read is not marked partial")
	}
	if len(result.Sizes) == 0 {
		t.Fatal("the walk produced no sizes at all, want the refusal to end nothing")
	}
	at := slices.IndexFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == feed })
	if at < 0 {
		t.Errorf("the walk did not measure the feed at %s: %+v", feed, result.Sizes)
	} else if result.Sizes[at].Bytes == 0 {
		t.Errorf("the walk measured the feed at no bytes, want what it could read of it")
	}
	// Everything after the refusal was still measured: the walk went on.
	last := filepath.Join(root, "workspaces", "cybermonklabs")
	if !slices.ContainsFunc(result.Sizes, func(size shimwire.LauncherVolumeSize) bool { return size.Path == last }) {
		t.Errorf("the walk stopped at the refusal: it never measured %s", last)
	}
}

// Nothing outside the volume root is measured or named, whatever the tree under the root
// becomes while the walk runs. /legion/scratch is writable by the role uid (`drwx--S--- legion
// legion`, measured in an issue pod of this deployment at 2026-11-11T08:05Z), and the launcher
// walks in the container's own mount namespace, where /var/run/legion/roles holds every role's
// private, state and agent-secrets directories: a role that renames a child of the root and
// leaves a symbolic link in its place after the walk has listed it used to aim every target
// under that child at a tree of its own, whose bytes then went to the issue's architect in the
// trim report's sizes (INFRA-2009, review 6, the same shape as the sweep's).
func TestTheWalkMeasuresNothingThroughAChildSwappedForALink(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	root := walkTree(t, clock.at.Add(-2*time.Hour))
	// The bait: a tree outside the volume whose directories carry the names the walk has just
	// listed under scratch, so that every target the walk is holding resolves into it the
	// moment the link is in place. A role names both trees, so matching them costs it nothing.
	bait := t.TempDir()
	const baitFileBytes = 64 * 1024
	baitBytes := int64(0)
	for _, name := range []string{"bun", "cache", "tmp"} {
		mkdirFixture(t, filepath.Join(bait, name))
		if err := os.WriteFile(filepath.Join(bait, name, "secrets.json"), make([]byte, baitFileBytes), 0o600); err != nil {
			t.Fatal(err)
		}
		baitBytes += baitFileBytes
	}
	if baitBytes == 0 {
		t.Fatal("the bait tree holds no bytes at all, so a walk that read it whole would look like one that read none of it")
	}

	// The swap is made at the first measurement: the walk has read the root and the root's
	// scratch by then and holds the names it will measure, and nothing it does afterwards may
	// resolve one of them through the link.
	swapped := filepath.Join(root, volumeScratchDir)
	swaps := 0
	measure := func(ctx context.Context, dir *os.Root, name string, at time.Time) measured {
		if swaps == 0 {
			swaps++
			if err := os.Rename(swapped, swapped+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(bait, swapped); err != nil {
				t.Fatal(err)
			}
		}
		return measureTree(ctx, dir, name, at)
	}

	result := walkOf(root, clock, &fakeTicks{}, measure).run(context.Background())

	if swaps != 1 {
		t.Fatalf("the swap was made %d times, want the one the walk's first measurement makes: the case staged nothing", swaps)
	}
	// Every target of the swapped child is accounted for: the walk measured it before the
	// link was there, or named it as one it could not reach. The one thing it may not hold is
	// a byte of the bait, whose files are each far larger than the whole of the volume's.
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
			t.Errorf("the walk measured %s at %d B, and no directory of this volume holds a file over one byte: it read the tree the link points at", size.Path, size.Bytes)
		}
	}
	for _, missing := range result.Missing {
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
		t.Errorf("the walk measured %d B over %d directories, and every file the volume itself holds is one byte: it read the %d B tree the link points at",
			measured, len(result.Sizes), baitBytes)
	}
	if !result.Partial {
		t.Error("a walk whose targets it could no longer reach is not marked partial")
	}
}

// The walk is made at most once an hour, and a report between walks carries the last walk's
// numbers with the time they were read: the walk stats every file on the volume, and a pod that
// is full sweeps every interval.
func TestNoSecondWalkIsMadeInsideTheHour(t *testing.T) {
	f := newTrimFixture(t, 1)
	tr := f.trimmer()
	calls := 0
	tr.measure = func(_ context.Context, _ *os.Root, _ string, _ time.Time) measured {
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
