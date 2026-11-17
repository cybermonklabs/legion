//go:build linux

package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The bounds on what a role's tree can make the sweep spend: time inside the walk of one entry,
// handles on directories of the Go build cache, the used-path set, the sweep's reason and its
// lines of the pod's log. The file builds only under Linux because the cases drive the sweep
// through volumetrim_test.go's fixture, which does.

// The listing reads an entry's age by walking its whole tree, and one entry a role made deep is
// a walk of any length: the sweep's three endings are asked before every directory that walk
// opens, so a budget that runs out inside it cuts the listing there, and the entry, whose newest
// time was never read, is not deleted. The clock moves a minute at every reading of it, as in
// TestTheSweepsBudgetRunsFromItsStartWithTheListingIncluded: it is read at the sweep's start, for
// the used-path set, at the entry's boundary, and before each directory of the walk, so the
// budget runs out before the walk opens the entry's third level. The negative control is the walk
// that asked nothing: it read the whole chain, the entry was listed, and the sweep deleted it,
// ending with every entry it listed deleted rather than on its budget.
func TestABudgetThatRunsOutInsideAnEntrysWalkCutsTheListingThere(t *testing.T) {
	f := newTrimFixture(t, 0)
	bun := f.roots[3]
	if bun.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache", bun)
	}
	old := f.clock.at.Add(-3 * time.Hour)
	entry := filepath.Join(bun.Path, "deep-tool")
	levels := []string{entry}
	for at := range 10 {
		levels = append(levels, filepath.Join(levels[len(levels)-1], fmt.Sprintf("level-%02d", at)))
	}
	mkdirFixture(t, levels[len(levels)-1])
	writeAged(t, filepath.Join(levels[len(levels)-1], "out"), old)
	for _, level := range slices.Backward(levels) {
		if err := os.Chtimes(level, old, old); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.step = time.Minute
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	out := f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

	if !f.exists(entry) {
		t.Errorf("the sweep deleted %s, whose walk ran past the budget: the listing read its age after the budget had run out", entry)
	}
	if out.deletions != 0 {
		t.Errorf("the sweep deleted %d entries, want none: the budget ran out inside the listing", out.deletions)
	}
	if !strings.Contains(out.reason, "budget") {
		t.Errorf("the sweep stopped because %q, want the budget named", out.reason)
	}
}

// Go names the 256 shard directories of its build cache with two lowercase hex digits, and the
// sweep holds a handle on every shard it lists until the sweep ends. A directory of any other
// name a role makes in the cache root is passed over, so it costs the launcher no handle, and the
// sweep says how many there were in one line rather than one each. The shards the sweep opened
// are counted at the deleter, which is handed every directory the listing opened. The negative
// control is the rule that took every directory of the root for a shard, which opened 300 more.
func TestOnlyADirectoryNamedAsGoNamesAShardIsOpenedAsOne(t *testing.T) {
	f := newTrimFixture(t, 0)
	cache := f.roots[1]
	if cache.Entry != GoCacheEntries {
		t.Fatalf("the second reclaimable root is %+v, want the Go build cache", cache)
	}
	const others = 300
	names := []string{"AB", "abc", "0g"} // upper case, three digits, a letter past f
	for at := len(names); at < others; at++ {
		names = append(names, fmt.Sprintf("made-by-a-role-%03d", at))
	}
	for _, name := range names {
		mkdirFixture(t, filepath.Join(cache.Path, name))
	}
	entry := filepath.Join(cache.Path, "ab", fmt.Sprintf("%064x-a", 0xab))
	mkdirFixture(t, filepath.Dir(entry))
	writeAged(t, entry, f.clock.at.Add(-3*time.Hour))
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	var shards []string
	tr.removals = func(cover RoleCover, dirs []*sweepDir) (entryRemover, error) {
		for _, dir := range dirs {
			if dir.shard != "" {
				shards = append(shards, dir.shard)
			}
		}
		return inProcessRemovals(cover, dirs)
	}
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	// "00" is the fixture's own shard, holding a name that is no entry (buildRoots).
	if want := []string{"00", "ab"}; !slices.Equal(shards, want) {
		t.Errorf("the sweep held %d shard directories open, want %v alone: %d of them were named by a role; held %v",
			len(shards), want, len(shards)-len(want), shards[:min(len(shards), 10)])
	}
	if f.exists(entry) {
		t.Errorf("the cache entry %s is still there, want it deleted: %s", entry, out.reason)
	}
	for _, name := range names {
		if !f.exists(filepath.Join(cache.Path, name)) {
			t.Errorf("the directory %s is gone, want a directory the sweep passed over left alone", name)
		}
	}
	if said := f.logged(fmt.Sprintf("holds %d directories not named as a shard is", others)); said != 1 {
		t.Errorf("the sweep said %d times that the root holds %d directories that are no shard, want once", said, others)
	}
	f.assertPrefixed()
}

// The used-path set keeps only what the guard can be asked about, a reclaimable root or a path
// under one: a process maps libraries and holds files all over the pod, and the set is read again
// every second while a sweep runs. The fixture process maps 400 files outside every root and
// holds 300 descriptors outside them, more than one batch of names (procNamesBatch), with one of
// each under a root. The negative control is the read that kept every absolute path.
func TestTheUsedPathSetKeepsOnlyPathsUnderTheRoots(t *testing.T) {
	const (
		mapped = "/legion/scratch/cache/ms-playwright/chromium-1200/chrome-linux/chrome"
		held   = "/legion/go/build/ab/abcdefabcdefabcdefabcdefabcdefab-a"
	)
	process := fixtureProcess{pid: 101, cwd: "/legion/workspaces/acme/widgets", exe: "/usr/bin/node", root: "/"}
	for at := range 400 {
		process.maps = append(process.maps, fmt.Sprintf("/usr/lib/x86_64-linux-gnu/lib%03d.so.1", at))
	}
	process.maps = append(process.maps, mapped, "/legion/scratch/cache-other/lib.so", "/legion/go/buildx/lib.so")
	for at := range 300 {
		process.fds = append(process.fds, fmt.Sprintf("/legion/workspaces/acme/widgets/src/file-%03d.go", at))
	}
	process.fds = append(process.fds, held)
	proc, _ := writeProcFixture(t, process)

	used := readUsedPaths(proc, reclaimableRootPaths(), os.Getpid())

	if len(used.Refused) > 0 {
		t.Fatalf("the fixture was refused: %v", used.Refused)
	}
	if want := []string{held, mapped}; !slices.Equal(used.paths, want) {
		t.Errorf("the set holds %d paths, want %v alone; the first of them are %v", len(used.paths), want, used.paths[:min(len(used.paths), 5)])
	}
	if !used.Covers("/legion/scratch/cache/ms-playwright") || !used.Covers("/legion/go/build/ab/abcdefabcdefabcdefabcdefabcdefab-a") {
		t.Errorf("Covers answers no for the two entries the process uses; the set holds %v", used.paths)
	}
}

// A line of a process's maps longer than the read's one buffer is a path deeper than any entry,
// and the read keeps it as far as the buffer holds it, cut back to its last whole name: that much
// of the path still lies under the entry the whole path lies under, so the entry is still in use,
// and the set holds no path longer than the buffer whatever a role maps. The lines after it are
// read as any other, and a long line outside every root is dropped. The negative control is the
// read that took the file whole, which kept the whole path, longer than any line the buffer holds.
func TestAMapsLineLongerThanTheBufferStillMarksItsEntryUsed(t *testing.T) {
	const (
		entry = "/legion/scratch/cache/deep-tool"
		after = "/legion/go/build/cd/cdefabcdefabcdefabcdefabcdefabcd-d"
	)
	long := entry + "/" + strings.Repeat("d/", procMapsReadBytes) + "lib.so"
	outside := "/usr/lib/" + strings.Repeat("d/", procMapsReadBytes) + "lib.so"
	proc, _ := writeProcFixture(t, fixtureProcess{pid: 102, maps: []string{outside, long, after}})

	used := readUsedPaths(proc, reclaimableRootPaths(), os.Getpid())

	if len(used.Refused) > 0 {
		t.Fatalf("the fixture was refused: %v", used.Refused)
	}
	if !used.Covers(entry) {
		t.Errorf("Covers(%s) = false, want the entry a process maps a file deep under to count as used", entry)
	}
	if !used.Covers(after) {
		t.Errorf("Covers(%s) = false, want the line after the long one read", after)
	}
	if used.Holds("/legion/scratch/cache") {
		t.Error("Holds(/legion/scratch/cache) = true, want the cut path to be no hold of the root it lies under")
	}
	for _, path := range used.paths {
		if len(path) > procMapsReadBytes {
			t.Errorf("the set holds a path of %d bytes, want none longer than the %d-byte buffer", len(path), procMapsReadBytes)
		}
		if strings.HasPrefix(path, "/usr/") {
			t.Errorf("the set holds %.60s…, a path outside every root", path)
		}
	}
}

// A sweep whose first read of /proc was refused names the refusals in its reason, which is a
// line of the pod's log, and a role that starts processes the launcher cannot read decides how
// many there are: the reason names the first 25, the most a report names
// (shimwire.VolumeReportRefusedReads), and how many more there were. The negative control is the
// reason that joined every one.
func TestASweepsReasonNamesAtMostTwentyFiveRefusals(t *testing.T) {
	const refusedProcesses = 40
	f := newTrimFixture(t, 1)
	for at := range refusedProcesses {
		placeRefusedProcess(t, f.proc, 6000+at, filepath.Join(f.root, "scratch"))
	}
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	tr := f.trimmer()
	out := tr.sweep(context.Background(), *trimSettings(), steady(90)(0))

	if carried := len(tr.refusedReads) + tr.refusedDropped; carried != refusedProcesses {
		t.Fatalf("the sweep handed the trimmer %d refusals, kept or counted, want the %d processes this case made unreadable", carried, refusedProcesses)
	}
	if named := strings.Count(out.reason, "process "); named != 25 {
		t.Errorf("the reason names %d refused processes, want 25: %q", named, out.reason)
	}
	if !strings.Contains(out.reason, "; and 15 more)") {
		t.Errorf("the reason does not say how many more there were: %q", out.reason)
	}
}

// A reason names every refusal up to twenty-five of them and says nothing of more, and at
// twenty-six names the first twenty-five and says there is one more (refusalsSaid). The case above
// stands forty refusals, fifteen past the bound, with which a reason that cut at the bound itself,
// or a refusal past it, is as green. Twenty-five is written out rather than read off
// shimwire.VolumeReportRefusedReads, which a case staged from would move with.
func TestASweepsReasonNamesTwentyFiveRefusalsWholeAndSaysOfATwentySixth(t *testing.T) {
	const most = 25
	refusals := make([]string, most+1)
	for at := range refusals {
		refusals[at] = fmt.Sprintf("process %d: fd: permission denied", 6000+at)
	}
	if said := refusalsSaid(refusals[:most]); said != strings.Join(refusals[:most], "; ") {
		t.Errorf("the reason names %d refusals as %q, want all %d of them joined and nothing said of more", most, said, most)
	}
	said := refusalsSaid(refusals)
	if want := strings.Join(refusals[:most], "; ") + "; and 1 more"; said != want {
		t.Errorf("the reason names %d refusals as %q, want the first %d and one more counted: %q", most+1, said, most, want)
	}
}

// A sweep names the first ten entries of a root it passes over, one line each, and counts the
// rest into one line when it ends: a role may fill a root with links, and a line per link is a
// line per link on every sweep. The negative control is the sweep that wrote a line for every
// one, fifty here.
func TestASweepNamesTenEntriesOfARootItPassesOverAndCountsTheRest(t *testing.T) {
	const links = 50
	f := newTrimFixture(t, 0)
	bun := f.roots[3]
	if bun.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache", bun)
	}
	target := t.TempDir()
	for at := range links {
		if err := os.Symlink(target, filepath.Join(bun.Path, fmt.Sprintf("link-%02d", at))); err != nil {
			t.Fatal(err)
		}
	}
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

	if named := f.logged("is passed over: "); named != 10 {
		t.Errorf("the sweep named %d links it passed over, want the first 10", named)
	}
	if said := f.logged(fmt.Sprintf("%d more entries of the reclaimable root %s were passed over in this sweep", links-10, bun.Path)); said != 1 {
		t.Errorf("the sweep said %d times how many more it passed over, want once; it wrote %d lines", said, len(f.linesWritten()))
	}
	f.assertPrefixed()
}

// A root whose entries the sweep passes over ten times has them named in ten lines and no line
// counting more, and one passed over eleven times has the eleventh counted in one line of one more
// (passedOver). The case above passes fifty over, forty past the ten, with which a count line
// written at ten, or only from twelve, is as green. Ten is written out rather than read off
// volumePassedOverLines, which a case staged from would move with.
func TestASweepNamesTenPassedOverEntriesWithNoCountAndCountsAnEleventh(t *testing.T) {
	const named = 10
	for _, links := range []int{named, named + 1} {
		t.Run(fmt.Sprintf("%d links", links), func(t *testing.T) {
			f := newTrimFixture(t, 0)
			bun := f.roots[3]
			if bun.Entry != ImmediateChildren {
				t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache", bun)
			}
			target := t.TempDir()
			for at := range links {
				if err := os.Symlink(target, filepath.Join(bun.Path, fmt.Sprintf("link-%02d", at))); err != nil {
					t.Fatal(err)
				}
			}
			f.useVolume(steady(90))
			f.setSettings(trimSettings())

			f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

			if said := f.logged("is passed over: "); said != named {
				t.Errorf("the sweep named %d of the %d links it passed over, want %d", said, links, named)
			}
			counted := f.logged("more entries of the reclaimable root")
			switch {
			case links == named && counted != 0:
				t.Errorf("the sweep that passed over %d links wrote %d lines counting more, want none: it named every one; it wrote %q", links, counted, f.linesWritten())
			case links > named && (counted != 1 || f.logged(fmt.Sprintf("1 more entries of the reclaimable root %s were passed over", bun.Path)) != 1):
				t.Errorf("the sweep that passed over %d links wrote %d lines counting more, want one counting the one past the %d named; it wrote %q", links, counted, named, f.linesWritten())
			}
			f.assertPrefixed()
		})
	}
}

// The line a sweep writes for an entry whose age it could not read names what it failed on, and a
// role decides how deep that is: it makes a chain of directories inside an entry and takes the
// read permission off the last one. The line is of a size set by the names in it and not by the
// depth, as the deleter's refusal of a removal is (removalFailure): the walk is a minute's work
// for every sweep, and each such line is written again on every sweep. The case reads the line at
// two depths of the same names, the deeper one the deepest the guards' walk opens
// (volumeWalkDepth), and the deeper line may be longer only by the digits of its depth.
// Measured on this code at eaf96c43 in an issue pod: a chain 1000 deep of 250-byte names wrote a
// line of 251307 bytes on every sweep, against 2813 bytes at 10 deep.
func TestAnAgeThatCannotBeReadIsLoggedAtASizeItsDepthDoesNotSet(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test process is root, which reads a mode-000 directory: an age read that fails cannot be staged for it")
	}
	line := func(depth int) string {
		f := newTrimFixture(t, 0)
		entry := filepath.Join(f.roots[3].Path, "chain-tool")
		chain := entry
		for level := range depth {
			chain = filepath.Join(chain, fmt.Sprintf("%04d-level", level))
		}
		locked := filepath.Join(chain, "locked")
		mkdirFixture(t, locked)
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		f.useVolume(steady(90))
		f.setSettings(trimSettings())

		f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))
		var said []string
		for _, written := range f.linesWritten() {
			if strings.Contains(written, "chain-tool") && strings.Contains(written, "could not be read") {
				said = append(said, written)
			}
		}
		if len(said) != 1 {
			t.Fatalf("the sweep wrote %d lines of the entry %s whose age it could not read at %d deep, want one: %q", len(said), entry, depth, f.linesWritten())
		}
		if !strings.Contains(said[0], "locked") {
			t.Errorf("the line at %d deep does not name locked, the directory the read failed on: %.300s", depth, said[0])
		}
		return said[0]
	}

	shallow, deep := line(2), line(volumeWalkDepth-1)
	if grown := len(deep) - len(shallow); grown > 16 {
		t.Errorf("the line for an entry %d deep is %d bytes, %d more than the %d of one 2 deep: its size is set by the depth a role made, not by the names in it: %.200s…",
			volumeWalkDepth-1, len(deep), grown, len(shallow), deep)
	}
}

// The guards' walk descends no more than volumeWalkDepth levels below an entry, as the size walk
// descends below a target: each level it opens holds a handle and its listing, three
// descriptors, in PID 1, whose one descriptor limit every open of the launcher shares, a role's
// start among them, and the walk runs on every sweep, in the listing and again before each
// delete. A directory deeper than that is not opened: it is the error the guard answers with, so
// the entry's age is not read and the sweep passes the entry over and deletes nothing of it. The
// case stands two entries of bun's install cache with every time in them three hours old: a
// chain of volumeWalkDepth directories, which the sweep deletes, and a chain one level deeper,
// which it passes over, naming the directory past the depth by the entry's name, its own and its
// depth. The negative control is the walk with no bound on its descent, as it stood at
// 8a30be78, which opened all sixty-five levels and deleted both.
func TestTheGuardWalkDescendsNoDeeperThanTheWalkDepthAndTheEntryIsPassedOver(t *testing.T) {
	f := newTrimFixture(t, 0)
	bun := f.roots[3]
	if bun.Entry != ImmediateChildren {
		t.Fatalf("the fourth reclaimable root is %+v, want bun's install cache", bun)
	}
	old := f.clock.at.Add(-3 * time.Hour)
	chain := func(name string, depth int) string {
		levels := []string{filepath.Join(bun.Path, name)}
		for level := 1; level <= depth; level++ {
			levels = append(levels, filepath.Join(levels[len(levels)-1], fmt.Sprintf("d%02d", level)))
		}
		mkdirFixture(t, levels[len(levels)-1])
		writeAged(t, filepath.Join(levels[len(levels)-1], "out"), old)
		for _, level := range slices.Backward(levels) {
			if err := os.Chtimes(level, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return levels[0]
	}
	reached, past := chain("reached-tool", volumeWalkDepth), chain("past-tool", volumeWalkDepth+1)
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	f.trimmer().sweep(context.Background(), *trimSettings(), steady(90)(0))

	if f.exists(reached) {
		t.Errorf("the sweep left %s, %d levels deep and every time in it old: the walk must reach its foot", reached, volumeWalkDepth)
	}
	if !f.exists(past) {
		t.Errorf("the sweep deleted %s, a level deeper than the guards' walk descends: its age was never read whole", past)
	}
	failed := fmt.Sprintf("walk %q (depth %d): %v", fmt.Sprintf("past-tool/.../d%02d", volumeWalkDepth+1), volumeWalkDepth+1, errWalkTooDeep)
	if said := f.logged(fmt.Sprintf("the age of %q could not be read and it is passed over: %s", past, failed)); said != 1 {
		t.Errorf("the sweep wrote %d lines passing %s over for the directory past the depth, want one ending %q: %q", said, past, failed, f.linesWritten())
	}
}

// The launcher cuts each list of a report to the cap the daemon holds a report to, from the one
// place both read (shimwire.VolumeReportSizes and the three beside it): a report built over every
// cap is one the daemon refuses, and the same report once bounded is one it takes, each list at
// its cap and the rest counted. The negative controls are each half on its own: a Validate that
// checked no list took the report over the caps, which this case refuses to read further, and a
// bound cutting at a cap of its own past the daemon's sends a report the daemon drops.
func TestEveryReportTheTrimmerBoundsIsOneTheDaemonTakes(t *testing.T) {
	const past = 7
	tr := newTrimFixture(t, 0).trimmer()
	over := shimwire.LauncherVolumeReport{
		Trimmer: tr.id, Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		Sizes:        make([]shimwire.LauncherVolumeSize, shimwire.VolumeReportSizes+past),
		SizesMissing: make([]shimwire.LauncherVolumeMissing, shimwire.VolumeReportSizesMissing+past),
		Faults:       make([]string, shimwire.VolumeReportFaults+past),
	}
	for at := range shimwire.VolumeReportRefusedReads + past {
		over.RefusedReads = append(over.RefusedReads, fmt.Sprintf("process %d: /proc/%d/fd: permission denied", at, at))
	}
	if err := over.Validate(); err == nil {
		t.Fatal("the daemon takes a report holding every list past its cap, so the bound below is held to nothing")
	}

	bounded := tr.bound(over)

	if err := bounded.Validate(); err != nil {
		t.Errorf("the daemon refuses the report this launcher bounded: %v", err)
	}
	for _, list := range []struct {
		name            string
		held, cap, left int
	}{
		{"sizes", len(bounded.Sizes), shimwire.VolumeReportSizes, bounded.SizesOmitted},
		{"unmeasured directories", len(bounded.SizesMissing), shimwire.VolumeReportSizesMissing, bounded.SizesMissingOmitted},
		{"refused reads", len(bounded.RefusedReads), shimwire.VolumeReportRefusedReads, bounded.RefusedReadsOmitted},
		{"faults", len(bounded.Faults), shimwire.VolumeReportFaults, bounded.FaultsOmitted},
	} {
		if list.held != list.cap || list.left != past {
			t.Errorf("the bounded report holds %d %s and counts %d left out, want the cap of %d and the %d past it", list.held, list.name, list.left, list.cap, past)
		}
	}
}

// Each list of a report one entry past its cap is cut to the cap with that one entry counted, and
// each list at its cap is carried whole with none counted (bound). The case above stands every
// list seven past its cap, and TestAReportThatReplacesAHeldOneSaysOnceWhatItLeavesOut its sizes two
// past it, with which a bound that let a list run one entry past its cap is as green, and the
// daemon refuses a report holding one entry past any cap (shimwire.LauncherVolumeReport.Validate),
// so that one entry would cost the pod its report. The caps are written out rather than read off
// shimwire's constants, which a case staged from would move with: fifty sizes, fifty unmeasured
// paths, twenty-five refused /proc reads and three faults.
func TestEveryListOfAReportOneEntryPastItsCapIsCutToItAndOneAtItIsCarriedWhole(t *testing.T) {
	const sizes, missing, refused, faults = 50, 50, 25, 3
	tr := newTrimFixture(t, 0).trimmer()
	for _, past := range []int{0, 1} {
		report := tr.bound(shimwire.LauncherVolumeReport{
			Trimmer: tr.id, Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
			Sizes:        make([]shimwire.LauncherVolumeSize, sizes+past),
			SizesMissing: make([]shimwire.LauncherVolumeMissing, missing+past),
			RefusedReads: make([]string, refused+past),
			Faults:       make([]string, faults+past),
		})
		for _, list := range []struct {
			name            string
			held, cap, left int
		}{
			{"sizes", len(report.Sizes), sizes, report.SizesOmitted},
			{"unmeasured paths", len(report.SizesMissing), missing, report.SizesMissingOmitted},
			{"refused reads", len(report.RefusedReads), refused, report.RefusedReadsOmitted},
			{"faults", len(report.Faults), faults, report.FaultsOmitted},
		} {
			if list.held != list.cap || list.left != past {
				t.Errorf("a report of %d %s is bounded to %d of them with %d counted left out, want the cap of %d and the %d past it counted", list.cap+past, list.name, list.held, list.left, list.cap, past)
			}
		}
		if err := report.Validate(); err != nil {
			t.Errorf("the daemon refuses the report this launcher bounded from lists %d past their caps: %v", past, err)
		}
	}
}

// The trimmer holds the newest three faults it recovered and counts the rest (fault): three are
// three held and none counted, and a fourth is held in place of the oldest, which is counted. A
// pod whose volume stays healthy never reports, so what fault holds is what the launcher process
// holds for its life, a stack of kilobytes each. A report cuts its faults to the same three again
// (bound), so a trimmer that held a fourth sends the same report as one that did not — the four
// faults of TestAReportOfAVolumeHoldingThousandsOfDirectoriesIsOneTheConnectionTakes reach the
// report as three and one counted either way — and what the trimmer holds is read here, after
// each fault. The faults are not in a row, as that case's four are not, since three in a row
// switch the trimmer off: each is a fault of the reading row, the readings' being the work no
// sweep is running in (faultRow), and a reading run to its end ends that row (tick), which is done
// here by hand between each two. Three is written out rather than read off
// shimwire.VolumeReportFaults.
func TestTheTrimmerHoldsTheNewestThreeFaultsAndCountsAFourth(t *testing.T) {
	const most = 3
	tr := newTrimFixture(t, 0).trimmer()
	for at := range most + 1 {
		if off := tr.fault(fmt.Sprintf("panic %d", at), []byte("the stack")); off {
			t.Fatalf("fault %d switched the trimmer off, want it on: no two of them are in a row", at+1)
		}
		tr.ran(readingRow) // a reading run to its end between two faults, which ends their row (tick)
		held, dropped := min(at+1, most), max(0, at+1-most)
		if len(tr.faults) != held || tr.faultsDropped != dropped {
			t.Errorf("after %d faults the trimmer holds %d and counts %d more, want %d held and %d counted", at+1, len(tr.faults), tr.faultsDropped, held, dropped)
		}
	}
	if !strings.HasPrefix(tr.faults[0], "panic 1\n") {
		t.Errorf("the trimmer holds %q first, want the newest three of the four, from the second on", tr.faults[0])
	}
}
