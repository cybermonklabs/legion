//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// volumeTrimOwnHoldEnv names the sweep TestASweepsOwnDeleterIsNoHoldHelper runs as PID 1 of
// namespaces of its own (TestASweepsOwnDeleterIsNoHoldOnTheRootsItDeletesFrom).
const volumeTrimOwnHoldEnv = "LEGION_LAUNCHER_VOLUME_TRIM_OWN_HOLD"

// R3 by itself: the deleter's removal follows no symbolic link, with no containment to bound one
// it did follow. The removal is called in this process, outside any namespace, against the race
// the containment cases drive (buildRacedEntry, raceTheSwap): while it removes the names of a
// subdirectory of the entry, the subdirectory is renamed away and a link to a directory of bait
// outside the entry is left in its place. removeEntryAt holds the subdirectory by the descriptor
// it opened it with, so it goes on removing what was renamed, and it meets the link only at its
// last unlinkat(AT_REMOVEDIR), which refuses a link and opens nothing.
//
// Every trial reads the bait back. A trial counts as raced when the swap was made while the
// removal was inside the subdirectory and the link still stands after it, which is a link the
// removal met and did not unlink as a directory; the case fails when no trial raced, since then
// it drove nothing. On 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3, ten runs raced 80
// trials of 80 with every bait held, each removal answering `unlinkat racer-victim/sub: not a
// directory`; with followingRemoval (Root.RemoveAll) called in place of removeEntryAt, the first
// trial raced and deleted every bait.
func TestTheDeletersRemovalFollowsNoLinkWithNoContainment(t *testing.T) {
	dir := t.TempDir()
	bait := t.TempDir() // a sibling of the entry's directory, which no removal of it may reach
	at := time.Now().Add(-2 * time.Hour)
	planted := plantBait(t, bait, at,
		filepath.Join("tester", RolePrivateDir, "notes.md"),
		filepath.Join("tester", RoleLauncherDir, LauncherTokenFile),
		"TOKEN",
	)
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	victim := filepath.Join(dir, "racer-victim")
	sub := filepath.Join(victim, "sub")

	names := make([]byte, volumeRemovalNamesBytes)
	const enough, tries = 8, 16
	raceSwaps(t, victim, bait, enough, tries, at, func(trial int, swapped <-chan bool) bool {
		raced := false
		removal := removeEntryAt(held, filepath.Base(victim), names)
		if <-swapped {
			if info, err := os.Lstat(sub); err == nil && info.Mode()&os.ModeSymlink != 0 {
				raced = true
				t.Logf("trial %d: the swap landed inside the removal, which answered %v", trial, removal)
				// The other half: what the removal held by its descriptor it removed, wherever
				// the role had renamed it to.
				if left, err := os.ReadDir(sub + ".real"); err != nil || len(left) != 0 {
					t.Errorf("trial %d: the renamed subdirectory holds %d names (%v), want none: the removal stopped removing what it held open", trial, len(left), err)
				}
			}
		} else if _, err := os.Lstat(victim); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("trial %d: the entry is still there (%v) after a removal no swap raced, which answered %v", trial, err, removal)
		}
		assertBaitHeld(t, planted, "the removal followed a link a role left where a subdirectory of the entry stood, with no containment behind it")
		return raced
	})
}

// A directory something keeps creating names in does not hold its removal. The removal reads a
// directory's names again after each pass for what was created while it removed, and the sweep
// waits on its deleter's answer with no deadline, reading its own budget only between entries:
// so the removal makes volumeRemovalPasses passes over the directory and answers errStillFilling
// once names are still appearing after them, which the sweep passes over as a removal that
// failed part way (trimmer.remove).
func TestARemovalOfADirectoryStillBeingWrittenInEnds(t *testing.T) {
	dir := t.TempDir()
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	filling := filepath.Join(dir, "entry", "filling")
	mkdirFixture(t, filling)

	// The writer creates names until it is told to stop, as fast as it can: a removal that read
	// the directory again for as long as it found new names would never answer while it runs.
	// Each name is an empty directory, which is one mkdir to the writer and an unlink, an open, a
	// read and a second unlink to the removal, so the writer keeps ahead of it: 80 runs of 80
	// answered errStillFilling, on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3.
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Mkdir(filepath.Join(filling, fmt.Sprintf("d%07d", n)), 0o755)
		}
	}()
	testwait.Eventually(t, "the writer to have created names in the directory", func() bool {
		names, err := os.ReadDir(filling)
		return err == nil && len(names) >= 100
	})

	answered := make(chan error, 1)
	go func() { answered <- removeEntryAt(held, "entry", make([]byte, volumeRemovalNamesBytes)) }()
	bound := testwait.Bounded(t, 30*time.Second, 5*time.Second)
	var removal error
	select {
	case removal = <-answered:
		close(stop)
		<-stopped
	case <-time.After(bound):
		close(stop)
		<-stopped
		<-answered // the writer has stopped, so the removal now runs out of names
		t.Fatalf("the removal had not answered %v after it began, with a writer still creating names in the directory it was emptying", bound)
	}
	if !errors.Is(removal, errStillFilling) {
		t.Fatalf("the removal answered %v, want %v for the directory still being written in", removal, errStillFilling)
	}
	if want := filepath.Join("entry", "filling"); !strings.Contains(removal.Error(), want+":") {
		t.Errorf("the removal answered %q, want it to name %s, the directory that was still filling", removal, want)
	}
}

// The sweeps TestASweepsOwnDeleterIsNoHoldOnTheRootsItDeletesFrom runs, each in a child of its own
// that is PID 1 of its namespaces (TestASweepsOwnDeleterIsNoHoldHelper).
const (
	ownHoldContained = "the contained deleter"
	ownHoldInProcess = "the in-process remover"
	ownHoldSeparate  = "a separate process holding the root"
)

// ownHoldEntries is how many entries the one root of those sweeps holds: a few thousand, so the
// deletions run past a second of the real clock and the set is read again while they run, after
// the refresh the case forces before the first of them.
const ownHoldEntries = 2000

// The sweep's own deleter is no hold on the roots it deletes from. The deleter child holds every
// directory the listing opened for the whole sweep (startContainedRemover), and the used-path set
// is read again once volumeUsedPathsMaxAge has passed. With the child read as a process of the
// pod, that refresh read the root as taken open after the listing: the contained sweep deleted
// none of its 2000 entries and wrote the line that says so, where the in-process remover deleted
// all 2000 (with the refresh leaving out the trimmer's pid alone, as it did before this change,
// on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3).
//
// Each sweep is the pod's composition: the deleter the pod runs (startRemover, re-entered as
// useContainedDeleter does), the used-path set read from a real procfs by the sweep's own
// readUsedPaths, and the real clock, set ahead by volumeUsedPathsMaxAge between the listing and
// the first deletion so that the set is read again before anything is deleted. The sweep runs
// in a child cloned with roleNamespaceAttr and given a procfs of its own PID namespace at /proc,
// where it is PID 1 as the launcher is in the roles container: the container's own /proc of the
// pod this case was written in refused a test process the readlink of /proc/1/cwd (2026-11-14,
// kernel 4.19.0-gvisor), which ends any sweep at its first read (sweep), and a namespace of its
// own is a /proc whose PID 1 is the sweep's own process.
//
// Two controls stand beside it. The same tree swept by the in-process remover is deleted whole,
// which is what makes the child, and not the tree, the hold the first sweep is read for. And a
// root a separate process takes after the listing, by standing in it, is still skipped with the
// one line that says so, which is what makes the change leave the deleter out and not the guard:
// with the refresh's reading replaced by an empty set, that sweep deleted all 2000 entries and
// wrote no line, on the same day and kernel.
func TestASweepsOwnDeleterIsNoHoldOnTheRootsItDeletesFrom(t *testing.T) {
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, sweep := range []string{ownHoldContained, ownHoldInProcess, ownHoldSeparate} {
		t.Run(sweep, func(t *testing.T) {
			attr, err := roleNamespaceAttr()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), testwait.Bounded(t, 3*time.Minute, 10*time.Second))
			defer cancel()
			child := exec.CommandContext(ctx, self, "-test.run=^TestASweepsOwnDeleterIsNoHoldHelper$", "-test.v")
			child.Dir = "/"
			child.Env = append(os.Environ(), volumeTrimOwnHoldEnv+"="+sweep)
			child.SysProcAttr = attr
			output, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("the sweep run as PID 1 of namespaces of its own: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), ownHoldSwept) {
				t.Fatalf("the child ran no sweep:\n%s", output)
			}
			t.Logf("%s", output)
		})
	}
}

// ownHoldSwept begins the line the helper writes once its sweep has returned, which the case
// reads so that a child that ran no sweep at all does not pass.
const ownHoldSwept = "the sweep ran as PID"

// TestASweepsOwnDeleterIsNoHoldHelper is one sweep of that case, as PID 1 of the namespaces it
// was cloned into.
func TestASweepsOwnDeleterIsNoHoldHelper(t *testing.T) {
	sweep := os.Getenv(volumeTrimOwnHoldEnv)
	if sweep == "" {
		return
	}
	if os.Getpid() != 1 {
		t.Fatalf("this sweep is PID %d, want 1 of a PID namespace of its own", os.Getpid())
	}
	if err := privateMounts(); err != nil {
		t.Fatal(err)
	}
	if err := mountProc("/proc"); err != nil {
		t.Fatal(err)
	}
	start := inProcessRemovals
	if sweep != ownHoldInProcess {
		useContainedDeleter(t)
		start = startRemover
	}
	f := newTrimFixture(t, 0)
	f.cover = coverUnder(t, namespaceScratch(t))
	f.proc, f.self = "/proc", os.Getpid()
	root := f.roots[3] // bun's install cache, whose entries are its immediate children
	for n := range ownHoldEntries {
		f.addTree(filepath.Join(root.Path, fmt.Sprintf("pkg-%05d", n)), root.Entry)
	}
	// A volume that falls a little with every reading and never reaches the low mark, so the
	// sweep ends when it runs out of entries rather than at the stall rule.
	f.useVolume(func(call int) volumeUsage { return usageAt(90 - float64(call)/1000) })
	f.setSettings(trimSettings())
	tr := f.trimmer()
	tr.roots = []ReclaimableRoot{root}
	var ahead time.Duration
	tr.now = func() time.Time { return time.Now().Add(ahead) }
	tr.removals = func(cover RoleCover, dirs []*sweepDir) (entryRemover, error) {
		// After the listing and before the first deletion, which is where the sweep starts its
		// deleter: the set the next entry is asked against is read again, with the deleter
		// running and, for the control, the separate process standing in the root.
		if sweep == ownHoldSeparate {
			standIn(t, root.Path)
		}
		ahead = volumeUsedPathsMaxAge + time.Millisecond
		return start(cover, dirs)
	}
	began := time.Now()
	out := tr.sweep(context.Background(), *trimSettings(), usageAt(90))
	t.Logf("%s %d over %s: %d of %d entries deleted in %v: %s", ownHoldSwept, os.Getpid(), tr.procRoot, out.deletions, len(f.entries), time.Since(began), out.reason)

	held := f.logged(fmt.Sprintf("the reclaimable root %s was taken open by a process of the pod after the listing", root.Path))
	if sweep == ownHoldSeparate {
		if out.deletions != 0 || len(f.gone()) != 0 {
			t.Errorf("the sweep deleted %d entries and %d are gone, want none of a root another process took after the listing", out.deletions, len(f.gone()))
		}
		if held != 1 {
			t.Errorf("the trimmer wrote %d lines naming the root as taken open, want one; it wrote %q", held, f.linesWritten())
		}
	} else {
		if out.deletions != len(f.entries) || len(f.gone()) != len(f.entries) {
			t.Errorf("the sweep deleted %d of the %d entries and %d are gone, want every one: the sweep's own remover is no hold on its root", out.deletions, len(f.entries), len(f.gone()))
		}
		if held != 0 {
			t.Errorf("the trimmer wrote %d lines naming the root as taken open, want none; it wrote %q", held, f.linesWritten())
		}
	}
	f.assertProtected()
}

// standIn starts a process of its own standing in dir, which is a hold on dir through that
// process's /proc cwd, and returns once the /proc this sweep reads names it there.
func standIn(t *testing.T, dir string) {
	t.Helper()
	holder := exec.Command("sleep", "300")
	holder.Dir = dir
	if err := holder.Start(); err != nil {
		t.Fatalf("a process standing in %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	cwd := filepath.Join("/proc", strconv.Itoa(holder.Process.Pid), "cwd")
	testwait.Eventually(t, cwd+" to name "+dir, func() bool {
		target, err := os.Readlink(cwd)
		return err == nil && target == dir
	})
}
