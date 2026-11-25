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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
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

// A name that became a link or a file between the unlink the removal tried and the open that
// follows it is refused by the open and unlinked as what it is now, and nothing behind it is
// read or removed (removeEntryAt). The window is the span between two syscalls, so the case
// makes its swap in it every time, through the seam the removal calls there
// (betweenUnlinkAndOpen): the entry, a directory, so its unlink fails, is renamed away and a
// link to a directory of bait, or a file, is left at its name. The bait must be read back whole,
// the name swapped in gone and the renamed directory untouched. The negative control is the
// removal opening that name with an open that follows a link (`O_RDONLY|O_DIRECTORY` with no
// O_NOFOLLOW in openElementAt's place), which emptied the bait and answered the last unlinkat's
// ENOTDIR.
func TestARemovalUnlinksWhatWasSwappedInBetweenItsUnlinkAndItsOpen(t *testing.T) {
	for _, swap := range []struct {
		name string
		put  func(t *testing.T, path, bait string)
	}{
		{"a link to a directory of bait", func(t *testing.T, path, bait string) {
			if err := os.Symlink(bait, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file", func(t *testing.T, path, _ string) { writeAged(t, path, time.Now()) }},
	} {
		t.Run(swap.name, func(t *testing.T) {
			dir, bait := t.TempDir(), t.TempDir()
			at := time.Now().Add(-2 * time.Hour)
			planted := plantBait(t, bait, at, filepath.Join("tester", RolePrivateDir, "notes.md"), "TOKEN")
			entry := filepath.Join(dir, "entry")
			agedUnder(t, dir, filepath.Join(entry, "sub", "kept"), at)
			held, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			swapped := 0
			betweenUnlinkAndOpen = func(name string) {
				if name != "entry" || swapped > 0 {
					return
				}
				swapped++
				if err := os.Rename(entry, entry+".real"); err != nil {
					t.Fatal(err)
				}
				swap.put(t, entry, bait)
			}
			t.Cleanup(func() { betweenUnlinkAndOpen = nil })

			removal := removeEntryAt(held, "entry", make([]byte, volumeRemovalNamesBytes))

			if swapped != 1 {
				t.Fatalf("the swap was made %d times, want the once the removal reached the window: the case staged nothing", swapped)
			}
			if removal != nil {
				t.Errorf("the removal answered %v, want the name swapped in unlinked as what it is now", removal)
			}
			assertBaitHeld(t, planted, "the removal opened the link swapped in between its unlink and its open")
			if _, err := os.Lstat(entry); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the name swapped in is still there (%v), want it unlinked", err)
			}
			if _, err := os.Stat(filepath.Join(entry+".real", "sub", "kept")); err != nil {
				t.Errorf("the directory renamed away lost what it held: %v; the removal reaches it by no name", err)
			}
		})
	}
}

// A directory something keeps creating names in does not hold its removal. The removal reads a
// directory's names again after each pass for what was created while it removed, and the sweep
// waits on its deleter's answer with no deadline, reading its own budget only between entries:
// so the removal makes volumeRemovalPasses passes over the directory and answers errStillFilling
// once names are still appearing after them, which the sweep passes over as a removal that
// failed part way (trimmer.remove).
//
// Which of the writer and the removal is faster is the scheduler's to decide: a removal that
// empties the directory between two of the writer's names finds it empty, removes it and answers
// nil, and the writer then has nowhere to write. That outcome is no answer on the bound, so the
// case stages the race again, up to filledTries times, until the writer stayed ahead; a removal
// with no bound on its passes never answers while the writer runs, which the helper fails on.
// On 2026-11-15 one run of the package in this pod met that nil once (round 23's surface run).
func TestARemovalOfADirectoryStillBeingWrittenInEnds(t *testing.T) {
	const filledTries = 20
	dir := t.TempDir()
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	filling := filepath.Join(dir, "entry", "filling")

	var removal error
	for try := 1; try <= filledTries && removal == nil; try++ {
		mkdirFixture(t, filling)
		removal = removeWhileFilling(t, held, filling)
	}
	if !errors.Is(removal, errStillFilling) {
		t.Fatalf("the removal answered %v, want %v for the directory still being written in", removal, errStillFilling)
	}
	if want := strconv.Quote(filepath.Join("entry", "filling")); !strings.Contains(removal.Error(), want+":") {
		t.Errorf("the removal answered %q, want it to name %s, the directory that was still filling", removal, want)
	}
}

// A removal makes two passes over a directory whose names keep appearing, and then answers
// errStillFilling (empty, volumeRemovalPasses). The case above races a writer, so it holds that
// the removal answers and not after how many passes: a bound of one pass, or of three, answers as
// well. Here the writer is the removal's own window between the unlink of a name and its open
// (betweenUnlinkAndOpen): each subdirectory of the entry the removal reaches has the next made
// beside it there, so every pass meets one new name, and the passes are counted by the
// subdirectories it reached. Two is written out rather than read off the constant.
func TestARemovalMakesTwoPassesOverADirectoryWhoseNamesKeepAppearing(t *testing.T) {
	const passes = 2
	dir := t.TempDir()
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	entry := filepath.Join(dir, "entry")
	mkdirFixture(t, filepath.Join(entry, "n1"))
	reached := 0
	betweenUnlinkAndOpen = func(name string) {
		if !strings.HasPrefix(name, "n") {
			return // the entry itself
		}
		reached++
		if err := os.Mkdir(filepath.Join(entry, fmt.Sprintf("n%d", reached+1)), 0o755); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { betweenUnlinkAndOpen = nil })

	removal := removeEntryAt(held, "entry", make([]byte, volumeRemovalNamesBytes))

	if !errors.Is(removal, errStillFilling) {
		t.Fatalf("the removal answered %v, want %v for a directory a new name appeared in at every pass", removal, errStillFilling)
	}
	if reached != passes {
		t.Errorf("the removal made %d passes over the entry, want %d: the first, and one more for the names made while it ran", reached, passes)
	}
}

// A directory still being written in that also holds a name whose removal fails for a reason of
// its own is answered with that reason, not with errStillFilling: the writer is what made the
// removal stop at its bound, but the name that failed is what would still stand once the writer
// stopped, and it is the one an operator reading the sweep's line can act on. The name here is a
// directory made 0500 holding a file, whose unlink fails with EACCES on every pass. The negative
// control is empty answering errStillFilling at its bound whatever its passes' names had failed
// with.
func TestARemovalOfADirectoryStillBeingWrittenInNamesTheErrorItMet(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks in a directory whatever its mode, so a directory made 0500 is no failure that lasts for it")
	}
	dir := t.TempDir()
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	filling := filepath.Join(dir, "entry", "filling")
	locked := filepath.Join(filling, "locked")
	mkdirFixture(t, locked)
	if err := os.WriteFile(filepath.Join(locked, "kept"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	removal := removeWhileFilling(t, held, filling)
	if !errors.Is(removal, syscall.EACCES) {
		t.Fatalf("the removal answered %v, want the %v its unremovable name failed with", removal, syscall.EACCES)
	}
	if !strings.Contains(removal.Error(), "kept") {
		t.Errorf("the removal answered %q, want it to name kept, the name that could not be removed", removal)
	}
}

// Among the names of one directory a subdirectory still being written in is no more the answer
// than it is within one: an entry holding such a subdirectory beside a name whose removal fails
// for a reason of its own is answered with that reason, whichever of the two the directory lists
// first, so the case runs once with each order. The subdirectory a writer keeps filling answers
// errStillFilling, and beside it a directory made 0500 holding a file fails its unlink with
// EACCES. Each order is the negative control of one way to get the choice wrong: with the
// filling subdirectory listed first, empty keeping the first error its pass met answers
// errStillFilling, and with it listed last, empty keeping the last error it met does.
func TestARemovalNamesASiblingsErrorBeforeASubdirectoryStillFilling(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks in a directory whatever its mode, so a directory made 0500 is no failure that lasts for it")
	}
	for _, order := range []struct {
		name         string
		fillingFirst bool
	}{
		{"filling listed first", true},
		{"filling listed last", false},
	} {
		t.Run(order.name, func(t *testing.T) {
			dir := t.TempDir()
			held, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			entry := filepath.Join(dir, "entry")
			locked := filepath.Join(entry, "locked")
			filling := stageListedPair(t, entry, locked, order.fillingFirst)
			if err := os.WriteFile(filepath.Join(locked, "kept"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			if listed := listedNames(t, entry); (listed[0] == filepath.Base(filling)) != order.fillingFirst {
				t.Fatalf("%s lists %q, which is not the order %q stages", entry, listed, order.name)
			}

			removal := removeWhileFilling(t, held, filling)
			if !errors.Is(removal, syscall.EACCES) {
				t.Fatalf("the removal answered %v, want the %v its sibling's unremovable name failed with", removal, syscall.EACCES)
			}
			if !strings.Contains(removal.Error(), "kept") {
				t.Errorf("the removal answered %q, want it to name kept, the name that could not be removed", removal)
			}
		})
	}
}

// stageListedPair makes locked and a subdirectory of a fresh name in entry, and answers that
// subdirectory once entry's getdents lists it where fillingFirst says: before locked, or after
// it, which is the order removeEntryAt reads them in. A directory lists its names in its
// filesystem's own order — the order they were made in on some, an order of the names on
// others, as this pod's /legion, a 9p mount, does (measured in an issue pod of this deployment
// on 2026-11-15: `mkdir zz locked filling-9 aa` and then `ls -f` listed `filling-9 zz aa
// locked`) — so the pair is made again under a fresh name, each time in the other order of
// making, until the entry lists it as the case asks, 64 times at most.
func stageListedPair(t *testing.T, entry, locked string, fillingFirst bool) string {
	t.Helper()
	for try := range 64 {
		candidate := filepath.Join(entry, fmt.Sprintf("filling-%d", try))
		made := []string{candidate, locked}
		if try%2 == 1 {
			made = []string{locked, candidate}
		}
		for _, dir := range made {
			mkdirFixture(t, dir)
		}
		if listed := listedNames(t, entry); (listed[0] == filepath.Base(candidate)) == fillingFirst {
			return candidate
		}
		if err := os.RemoveAll(entry); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("no pair of names made %s list its filling subdirectory where the case needs it (first: %t), so the case would stage nothing", entry, fillingFirst)
	return ""
}

// listedNames is the names of dir in the order its getdents lists them.
func listedNames(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// fillingLead is how many names removeWhileFilling's writer may keep ahead of the removal: it makes
// a name only for a name the removal has reached, past its first fillingLead. So no pass of the
// removal reads more than about fillingLead names, two passes reach at most about twice that, and
// what stands for t.TempDir's cleanup to remove is bounded by a count, not by how long the writer
// ran. It is far more than a pass needs to meet new names, since the removal's work per name is
// four calls and the writer's one.
const fillingLead = 4096

// removeWhileFilling removes held's entry while a writer creates names in filling, a directory
// under it, as fast as it can and at most fillingLead ahead of the removal, and answers the
// removal. The question is whether the removal ends while something keeps writing, and its passes
// are finite, so no verdict rides on a clock and the removal's share of a loaded machine's CPUs (a
// fixed 30 s once failed a correct removal under 32 pinned hogs):
//
//   - the writer runs until the removal answers, paced by the removal's own progress, which the
//     case reads through betweenUnlinkAndOpen, called once for each name of the writer's the
//     removal reaches;
//   - a removal that has reached three times fillingLead of the writer's names has made more than
//     two passes' worth, and one with no bound on its passes fails there, on its own line, by
//     count;
//   - a removal that never answers fails at the hang guard, beforeDeadline, on its own line.
//
// The writer is stopped, and the hook put back, in a cleanup registered after the caller's
// t.TempDir, so it runs before that directory's removal and on every way out of the case, a
// failure before the removal starts among them. With no -v, go test prints a case's lines only once
// its cleanups end, and at most about fillingLead names are left to remove then, so the line comes
// before the binary's alarm. The removal works on a descriptor of its own, a duplicate of held's
// that it closes when it answers, so a removal still running after the case has closed held
// reaches no descriptor the number is reused for.
func removeWhileFilling(t *testing.T, held *os.File, filling string) error {
	t.Helper()
	tokens := make(chan struct{}, fillingLead)
	for range fillingLead {
		tokens <- struct{}{}
	}
	var reached atomic.Int64
	overrun := make(chan struct{})
	var overrunOnce sync.Once
	previous := betweenUnlinkAndOpen
	betweenUnlinkAndOpen = func(name string) {
		if !isFillingName(name) {
			return
		}
		select {
		case tokens <- struct{}{}:
		default:
		}
		if reached.Add(1) > 3*fillingLead {
			overrunOnce.Do(func() { close(overrun) })
		}
	}
	// The writer creates names until it is told to stop, as fast as its tokens let it: a removal
	// that read the directory again for as long as it found new names would never answer while it
	// runs. Each name is an empty directory, which is one mkdir to the writer and an unlink, an
	// open, a read and a second unlink to the removal, so the writer keeps its lead: 80 runs of 80
	// answered errStillFilling, on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3.
	stop, stopped := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	stopWriter := func() {
		stopOnce.Do(func() { close(stop) })
		<-stopped
	}
	t.Cleanup(func() {
		stopWriter()
		betweenUnlinkAndOpen = previous
	})
	go func() {
		defer close(stopped)
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			case <-tokens:
			}
			_ = os.Mkdir(filepath.Join(filling, fmt.Sprintf("d%07d", n)), 0o755)
		}
	}()
	testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, "the writer to have created names in the directory", func() bool {
		names, err := os.ReadDir(filling)
		return err == nil && len(names) >= 100
	})

	fd, err := unix.Dup(int(held.Fd()))
	if err != nil {
		t.Fatalf("a descriptor of the removal's own on the entry's directory: %v", err)
	}
	own := os.NewFile(uintptr(fd), held.Name())
	answered := make(chan error, 1)
	go func() {
		removal := removeEntryAt(own, "entry", make([]byte, volumeRemovalNamesBytes))
		_ = own.Close()
		answered <- removal
	}()
	// drained is what the removal did once the writer had stopped: it has at most about
	// fillingLead names left to remove, so it answers soon, unless it never answers at all.
	drained := func() string {
		select {
		case removal := <-answered:
			return fmt.Sprintf("answered %v once the writer stopped", removal)
		case <-time.After(cleanupBeforeDeadline(t)):
			return "had not answered when the case gave up on it, at cleanupBeforeDeadline"
		}
	}
	guard := beforeDeadline(t)
	select {
	case removal := <-answered:
		stopWriter()
		return removal
	case <-overrun:
		stopWriter()
		t.Fatalf("the removal had reached %d of the writer's names while the writer kept at most %d ahead of it, more than the %d two passes reach: a removal with no bound on its passes, which a writer still creating names holds for as long as it writes; it %s",
			reached.Load(), fillingLead, 2*fillingLead, drained())
	case <-time.After(guard):
		stopWriter()
		t.Fatalf("the removal had not answered %v after it began, to 30 s before the test binary's deadline, and had reached %d of the writer's names: a removal that never answers; it %s",
			guard.Round(time.Second), reached.Load(), drained())
	}
	return nil
}

// isFillingName is whether name is one removeWhileFilling's writer makes: d and seven digits.
func isFillingName(name string) bool {
	if len(name) != 8 || name[0] != 'd' {
		return false
	}
	_, err := strconv.Atoi(name[1:])
	return err == nil
}

// A removal that fails deep in a tree answers an error that names the entry, the name that
// failed and how deep that name lay, and nothing of the path between: the error is the deleter's
// one line of answer and the sweep's one line of log (trimmer.remove), so its size must be one
// this package chose rather than one the roles' tree happened to have. The tree is a chain 1000
// directories deep of 100-byte names, the last made 0500 and holding a file, whose unlink fails
// with EACCES. The negative control is under renaming the error to its whole path on the way
// back up, which answered an error as long as the path of more than 100 KB.
func TestARemovalThatFailsDeepInATreeAnswersABoundedError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks in a directory whatever its mode, so a directory made 0500 is no failure that lasts for it")
	}
	const depth = 1000
	const errorBytes = 512
	name := strings.Repeat("d", 100)
	held, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if err := unix.Mkdirat(int(held.Fd()), "entry", 0o700); err != nil {
		t.Fatal(err)
	}
	// The chain is made through descriptors, a name at a time, since its path is far past
	// PATH_MAX; the descriptor of its last directory is kept, which is how the cleanup makes it
	// writable again.
	fd, err := unix.Openat(int(held.Fd()), "entry", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= depth; level++ {
		if err := unix.Mkdirat(fd, name, 0o700); err != nil {
			t.Fatalf("make level %d: %v", level, err)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatalf("open level %d: %v", level, err)
		}
		fd = next
	}
	kept, err := unix.Openat(fd, "kept", unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(kept)
	if err := unix.Fchmod(fd, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Fchmod(fd, 0o700)
		_ = unix.Close(fd)
		if err := removeEntryAt(held, "entry", make([]byte, volumeRemovalNamesBytes)); err != nil {
			t.Errorf("remove the chain once its last directory is writable again: %.200s", err)
		}
	})

	removal := removeEntryAt(held, "entry", make([]byte, volumeRemovalNamesBytes))
	if !errors.Is(removal, syscall.EACCES) {
		t.Fatalf("the removal answered %.200v, want the %v its unremovable name failed with", removal, syscall.EACCES)
	}
	said := removal.Error()
	pathBytes := len("entry") + depth*(len(name)+1) + len("/kept")
	if len(said) > errorBytes {
		t.Errorf("the removal answered an error of %d bytes for a failure %d bytes of path below the entry, want at most %d: %.200s…",
			len(said), pathBytes, errorBytes, said)
	}
	if want := fmt.Sprintf("%q (depth %d)", "entry/.../kept", depth+1); !strings.Contains(said, want) {
		t.Errorf("the removal answered %.200q…, want it to name %q", said, want)
	}
}

// A removal's error quotes the names it carries, each one a role chose: a name holding a newline
// is written with the newline escaped, so the deleter's reply and the sweep's line in the pod's
// log stay one line however a role named its files (removalFailure). The names are a file below
// the entry whose unlink fails, and an entry whose own removal fails. The negative control is
// removalFailure writing the names as they are, which put the newline in the error, and for the
// entry itself removeEntryAt answering the fs.PathError of its own removal, which does the same.
func TestARemovalErrorQuotesTheNamesARoleChose(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks in a directory whatever its mode, so a directory made 0500 is no failure that lasts for it")
	}
	const forged = "x\nvolume trim: a line the role wrote"
	answer := func(t *testing.T, dir, name, want string) {
		t.Helper()
		held, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		removal := removeEntryAt(held, name, make([]byte, volumeRemovalNamesBytes))
		if !errors.Is(removal, syscall.EACCES) {
			t.Fatalf("the removal answered %q, want the %v its unremovable name failed with", removal, syscall.EACCES)
		}
		if strings.Contains(removal.Error(), "\n") {
			t.Errorf("the removal answered %q, which holds a newline of the name a role chose", removal)
		}
		if !strings.Contains(removal.Error(), want) {
			t.Errorf("the removal answered %q, want it to name %s", removal, want)
		}
	}
	t.Run("a name below the entry", func(t *testing.T) {
		dir := t.TempDir()
		locked := filepath.Join(dir, "entry", "locked")
		mkdirFixture(t, locked)
		if err := os.WriteFile(filepath.Join(locked, forged), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		answer(t, dir, "entry", strconv.Quote("entry/.../"+forged)+" (depth 2): ")
	})
	t.Run("the entry itself", func(t *testing.T) {
		dir := t.TempDir()
		mkdirFixture(t, filepath.Join(dir, forged))
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		answer(t, dir, forged, strconv.Quote(forged)+": ")
	})
}

// The sweeps TestASweepsOwnDeleterIsNoHoldOnTheRootsItDeletesFrom runs, each in a child of its own
// that is PID 1 of its namespaces (TestASweepsOwnDeleterIsNoHoldHelper).
const (
	ownHoldContained = "the contained deleter"
	ownHoldInProcess = "the in-process remover"
	ownHoldSeparate  = "a separate process holding the root"
)

// ownHoldEntries is how many entries the one root of those sweeps holds: a few thousand, so the
// set is read again in the middle of the deletions (refreshMidway), with the deleter holding the
// directories it opened for the requests before it, after the refresh the case forces before the
// first of them.
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
// root a separate process takes after the listing, by holding a descriptor open on it as a
// `flock` does, is still skipped with the one line that says so, which is what makes the change
// leave the deleter out and not the guard: with the refresh's reading replaced by an empty set,
// that sweep deleted all 2000 entries and wrote no line, on 2026-11-15, kernel 4.19.0-gvisor,
// under go1.27.3. That process stands at / so that the descriptor is the only path it names on
// the root: a process standing in a root is no hold on it (usedPaths.Holds,
// TestAProcessStandingInARootIsNoHoldOnIt).
//
// Build and remove the child's fixture on a memory filesystem in its own mount namespace
// (TestASweepsOwnDeleterIsNoHoldHelper). The separate-holder control leaves all 2000 entries to
// cleanup, so putting them on the machine's disk would make fixture I/O, not the sweep, consume
// the child's deadline. refreshMidway forces the used-path refresh independently of the
// filesystem's speed.
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
			// No bound of the case's own on the child's end (reentry): its -test.timeout ends it,
			// and so its sweep, its cleanups and its alarm all reach the case's output.
			child := exec.Command(self, append(reentry(t, "TestASweepsOwnDeleterIsNoHoldHelper"), "-test.v")...)
			child.Dir = "/"
			// The child's temporary directory, under both names a test reads one by (t.TempDir
			// reads GOTMPDIR first): an empty mount point here, which the child covers with a
			// memory filesystem of its own before anything is made in it.
			memory := t.TempDir()
			child.Env = append(os.Environ(), volumeTrimOwnHoldEnv+"="+sweep, "TMPDIR="+memory, "GOTMPDIR="+memory)
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
	// The temporary directory the case gave this child, which every directory of the fixture is
	// made in: a memory filesystem of this namespace's own, which ends with it. t.TempDir reads
	// GOTMPDIR before TMPDIR, and the case names the directory as both.
	memory := os.Getenv("GOTMPDIR")
	if memory == "" || os.Getenv("TMPDIR") != memory {
		t.Fatalf("GOTMPDIR is %q and TMPDIR %q, want the one directory the case gave this sweep for its fixture", memory, os.Getenv("TMPDIR"))
	}
	if err := mountTmpfs(memory, "size=64m"); err != nil {
		t.Fatal(err)
	}
	start := inProcessRemovals
	if sweep != ownHoldInProcess {
		useContainedDeleter(t)
		start = startRemover(&pidGuard{})
	}
	f := newTrimFixture(t, 0)
	f.cover = coverUnder(t, namespaceScratch(t, ""))
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
		// running and, for the control, the separate process holding the root open.
		if sweep == ownHoldSeparate {
			holdOpen(t, root.Path)
		}
		ahead = volumeUsedPathsMaxAge + time.Millisecond
		remover, err := start(cover, dirs)
		if err != nil {
			return nil, err
		}
		return &refreshMidway{entryRemover: remover, at: ownHoldEntries / 2, advance: func() { ahead += volumeUsedPathsMaxAge }}, nil
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

// refreshMidway is a sweep's remover that moves the case's clock on by volumeUsedPathsMaxAge once,
// as it makes its at-th removal, so the sweep reads the used-path set again before the removal
// after it: in the middle of the deletions, whatever the speed of the filesystem they are made
// on. The deleter then holds what it opened for the requests before it, besides what the listing
// passed it, and the set read before the first deletion saw only the latter.
type refreshMidway struct {
	entryRemover
	removals, at int
	advance      func()
}

func (r *refreshMidway) removeAll(dir *sweepDir, name string) error {
	r.removals++
	if r.removals == r.at {
		r.advance()
	}
	return r.entryRemover.removeAll(dir, name)
}

// holdOpen starts a process of its own that holds a descriptor open on dir, which is a hold on dir
// through that process's /proc fd, and returns once the /proc this sweep reads names it there.
// The process stands at /, and this one closes its own descriptor once the other has it.
func holdOpen(t *testing.T, dir string) {
	t.Helper()
	handle, err := os.Open(dir)
	if err != nil {
		t.Fatalf("a descriptor on %s: %v", dir, err)
	}
	holder := exec.Command("sleep", "300")
	holder.Dir = "/"
	holder.ExtraFiles = []*os.File{handle}
	started := holder.Start()
	_ = handle.Close()
	if started != nil {
		t.Fatalf("a process holding %s open: %v", dir, started)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	// ExtraFiles[0] is the child's descriptor 3.
	fd := filepath.Join("/proc", strconv.Itoa(holder.Process.Pid), "fd", "3")
	testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, fd+" to name "+dir, func() bool {
		target, err := os.Readlink(fd)
		return err == nil && target == dir
	})
}

// The sweep's deleter is the launcher's own child, and PID 1's reaper reaps every exited process
// of the container's PID namespace but the ones it is told to leave (reapOrphans). Started through
// the launcher's pidGuard, the deleter is one of those: a pass of the reaper made after the
// deleter has exited and before the remover's close waits on it leaves the exit to that wait,
// which reads the deleter's own exit status, and the guard forgets the pid once it has.
func TestTheReaperLeavesTheSweepsDeleterToItsOwnWait(t *testing.T) {
	requireRoleNamespaces(t)
	useContainedDeleter(t)
	guard := &pidGuard{}
	remover, err := startContainedRemover(guard, coverUnder(t, namespaceScratch(t, "")), nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := remover.process()
	// The end of the requests, which the deleter answers by exiting: it then waits, a zombie,
	// for whoever reaps it.
	_ = remover.requests.Close()
	testwait.WithinPolling(t, beforeDeadline(t), testwait.Poll, fmt.Sprintf("the deleter %d to exit", pid), func() bool { return zombie(pid) })

	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal)
	done := make(chan struct{})
	managers := []*manager{{role: "architect", namespaced: true, log: &roleLog{}}}
	go func() { reapOrphans(ctx, signals, managers, guard); close(done) }()
	// The channel is unbuffered, so the second signal is taken only once the first pass is over.
	signals <- syscall.SIGCHLD
	signals <- syscall.SIGCHLD
	cancel()
	<-done

	remover.close()
	if state := remover.cmd.ProcessState; state == nil || state.ExitCode() != 0 {
		t.Errorf("the remover's close read %v for the deleter, want its own exit 0: the reaper took the exit before the remover's wait", state)
	}
	guard.mu.Lock()
	held := guard.held()
	guard.mu.Unlock()
	if len(held) != 0 {
		t.Errorf("the guard still holds %v after the close read the deleter's exit, want nothing", held)
	}
}

// The pod's memory guard never ends the sweep's deleter. Outside every role the guard spares the
// launcher and each process the launcher holds through its pidGuard, with everything under it
// (podClassifier.classify), and the deleter is the launcher's own child started through that
// same guard (startContainedRemover), the one Run hands both the trimmer and the memory guard. So
// a deleter still running when the pod nears its memory limit is no process the guard may end.
// The case starts the real deleter through a pidGuard, and the classifier the launcher builds
// over that guard reads a scan of a /proc whose only processes are the test's and the deleter's,
// each a link to the process's own directory in the machine's /proc, as
// TestMemoryGuardOnRealRoleNamespaces reads its role's processes.
func TestTheMemoryGuardSparesTheSweepsRunningDeleter(t *testing.T) {
	requireRoleNamespaces(t)
	useContainedDeleter(t)
	guard := &pidGuard{}
	remover, err := startContainedRemover(guard, coverUnder(t, namespaceScratch(t, "")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remover.close)
	deleter := remover.process()
	pod := newGuardPod(t)
	for _, pid := range []int{os.Getpid(), deleter} {
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(pod.proc, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	table := scanTable(t, pod)
	if p, ok := table.Get(deleter); !ok || p.PPID != os.Getpid() {
		t.Fatalf("the scan reads the running deleter %d as %+v, want the launcher's child", deleter, p)
	}
	if class := newPodClassifier(t.TempDir(), nil, guard).classify(table)[deleter]; !class.Protected {
		t.Errorf("the sweep's running deleter %d is classified %+v, want protected: the memory guard could end it mid-sweep", deleter, class)
	}
}

// The real deleter answers a removal it cannot make as a refusal of that entry, in its own
// words, and goes on answering: the sweep passes such an entry over and asks for the next one
// (trimmer.remove). The child runs in a user namespace whose root is the test's own uid, which
// unlinks in a directory of that uid whatever its mode, so the refusals are of names no removal
// can make: a name longer than NAME_MAX, which the kernel answers ENAMETOOLONG, and a name with a
// separator in it, which the deleter refuses before it resolves anything. An entry it can remove
// is asked for last and goes, so the refusals left the deleter answering. The negative control is
// the child answering `removed` for a removal that failed, which every deleter case before this
// one left green.
func TestTheDeleterAnswersARemovalItCannotMakeAsARefusal(t *testing.T) {
	requireRoleNamespaces(t)
	useContainedDeleter(t)
	dir := t.TempDir()
	handle, err := openVolumeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	listed := &sweepDir{root: ReclaimableRoot{Path: dir}, handle: handle}
	remover, err := startContainedRemover(&pidGuard{}, coverUnder(t, namespaceScratch(t, "")), []*sweepDir{listed})
	if err != nil {
		t.Fatal(err)
	}
	defer remover.close()

	for _, refused := range []struct {
		name, why string
	}{
		{strings.Repeat("x", 256), syscall.ENAMETOOLONG.Error()},
		{"entry/inside", "is not one name in one directory"},
	} {
		err := remover.removeAll(listed, refused.name)
		switch {
		case err == nil:
			t.Errorf("the deleter answered the removal of %.40q… as made, want its refusal", refused.name)
		case errors.Is(err, errRemoverGone):
			t.Fatalf("the deleter is gone after the removal of %.40q…: %v", refused.name, err)
		case !strings.Contains(err.Error(), refused.why):
			t.Errorf("the deleter refused the removal of %.40q… with %q, want %q", refused.name, err, refused.why)
		}
	}
	entry := filepath.Join(dir, "entry")
	agedUnder(t, dir, filepath.Join(entry, "inside", "kept"), time.Now().Add(-2*time.Hour))
	if err := remover.removeAll(listed, "entry"); err != nil {
		t.Fatalf("the deleter answered %v for an entry it can remove, after the refusals", err)
	}
	if _, err := os.Lstat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the entry the deleter answered removed is still there: %v", err)
	}
}

// A request naming a descriptor below the first the trimmer passes is refused before anything is
// opened (removeOneRequest): the directories begin at the fourth descriptor, and the trimmer
// numbers them from 0, which every deleter case asks for, so -1 is the request one below them.
// A refusal at -1 is no reach into the deleter's own standard error, the third descriptor, which
// the number would otherwise name.
func TestARequestNamingADescriptorBelowTheFirstPassedIsRefused(t *testing.T) {
	err := removeOneRequest(map[int]*os.File{}, `-1 "entry"`, make([]byte, volumeRemovalNamesBytes))
	if err == nil || !strings.Contains(err.Error(), "names the descriptor -1, which the trimmer passed none at") {
		t.Errorf("the request for descriptor -1 was answered %v, want it refused as naming no descriptor the trimmer passed", err)
	}
}

// A deleter that does not end when its requests do is killed once the grace is over, and the
// remover's close returns: the launcher's Run waits on the trimmer's goroutine at the
// container's stop, and a close that waited on a child for ever would hold it there, in the
// process that is PID 1 of the roles container. The child here is a `sleep` that never reads
// its standard input, started through the guard as the deleter is. The close must return no
// sooner than volumeRemoverGrace and no later than that grace and 30 s, with the child killed.
// A close that kills only after some longer budget fails on that bound; one that never kills
// fails on it and on the signal too, once the sleep's own 300 s are up, or at the hang guard the
// wait for the close runs under, if that ends sooner.
func TestADeleterThatIgnoresTheEndOfItsRequestsIsKilledAtTheGrace(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	requests, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	guard := &pidGuard{}
	if _, err := guard.start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	remover := &containedRemover{cmd: cmd, guard: guard, requests: requests, at: map[*sweepDir]int{}, said: &saidBuffer{}}

	began := time.Now()
	closed := make(chan struct{})
	go func() { remover.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(beforeDeadline(t)):
		t.Fatalf("the remover's close had not returned %v after it began, with a child that ignores the end of its requests", time.Since(began))
	}
	took := time.Since(began)
	if took < volumeRemoverGrace {
		t.Errorf("the close returned after %v, before the %v grace: the child ended some other way", took, volumeRemoverGrace)
	}
	if took > volumeRemoverGrace+30*time.Second {
		t.Errorf("the close returned after %v, past the %v grace and 30 s: it did not kill the child at the grace", took, volumeRemoverGrace)
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Errorf("the child ended %v, want killed with %v at the end of the grace", cmd.ProcessState, syscall.SIGKILL)
	}
	guard.mu.Lock()
	held := guard.held()
	guard.mu.Unlock()
	if len(held) != 0 {
		t.Errorf("the guard still holds %v after the close read the child's exit", held)
	}
}
