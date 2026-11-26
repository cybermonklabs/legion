package leakguard

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The install's parent is under the scratch every role writes, so a role can swap it for a fifo
// while the install runs. The install removes its staging directory, and the old install it moved
// aside, without a plain open of that parent (rolefile.RemoveTree), so it returns without waiting and
// leaves the fifo as it was. os.RemoveAll opens the parent plainly when it cannot unlink the path
// outright, and that open waits for a writer for as long as none comes. Each case
// makes the swap from inside the install, which is only how the test times it: a role makes it
// from its own process.
func TestInstallServedAnswersAParentSwappedForAFifo(t *testing.T) {
	// swap moves scratch aside to out and leaves a fifo where it was.
	swap := func(scratch, out string) error {
		if err := os.Rename(scratch, out); err != nil {
			return err
		}
		return syscall.Mkfifo(scratch, 0o644)
	}
	t.Run("during the verification", func(t *testing.T) {
		// The check's own --help makes the swap, before the staged copy is moved into place.
		scratch := filepath.Join(t.TempDir(), "scratch")
		swapping := "#!/bin/sh\nmv '" + scratch + "' '" + scratch + ".moved' && mkfifo '" + scratch + "'\n" +
			"echo '# Usage: check-public-text FILE..., --commits REV..., --tree DIR, gh-guard ARGS, or REMOTE URL.'\nexit 1\n"
		body := served(t, wholeCheck(t, swapping))
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		err := returnsWithoutAWriter(t, func() string { return scratch }, "the install", func() error {
			_, err := install(t, filepath.Join(scratch, "leak-check"), theTag, body)
			return err
		})
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("the install answered %v, want its staged copy refused once the scratch is no directory (ENOTDIR)", err)
		}
		stillFifo(t, scratch)
	})
	t.Run("after the old install is moved aside", func(t *testing.T) {
		// The install's line that it replaced the old install comes after both renames and before
		// its removals, so the swap made there lands before the aside's removal every time: the
		// aside is then left in the moved-out scratch, which a removal that ran first would have
		// emptied.
		root := t.TempDir()
		scratch, out := filepath.Join(root, "scratch"), filepath.Join(root, "out")
		dir := filepath.Join(scratch, "leak-check")
		body := served(t, wholeCheck(t, answersItsHelp))
		if _, err := install(t, dir, theTag, body); err != nil {
			t.Fatal(err)
		}
		var swapped error
		err := returnsWithoutAWriter(t, func() string { return scratch }, "the install", func() error {
			return InstallServed(bound(t), ServedInstall{Dir: dir, Tag: theTag, Body: body, Start: procgroup.NoReaper, Log: func(format string, _ ...any) {
				if strings.Contains(format, "replaced") {
					swapped = swap(scratch, out)
				}
			}})
		})
		if swapped != nil || err != nil {
			t.Fatalf("the swap answered %v and the install %v, want both nil", swapped, err)
		}
		if left, _ := filepath.Glob(filepath.Join(out, ".leak-check-install-*.replaced")); len(left) != 1 {
			t.Fatalf("the moved-out scratch holds %v, want the one aside the install could not remove", left)
		}
		stillFifo(t, scratch)
	})
}

// The old install the install moves aside is the scratch's too, so a role can leave in it a link
// to a directory of another role's, under a directory the removal cannot unlink it from (mode
// 0555). The aside's removal (rolefile.RemoveTree) leaves the link and what it names as they are:
// it opens no link. os.RemoveAll opens such an entry with O_DIRECTORY|O_NOFOLLOW, which a gVisor
// release can follow a link through (rolefile.OpenDirectory), so in such a pod it deletes what the
// link names; Linux, and newer gVisor, refuse that open, so only such a gVisor shows that control
// fail.
func TestInstallServedRemovesNothingALinkInTheOldInstallNames(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a 0555 directory does not stop the unlink, so the removal never opens the link")
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(victim, "precious")
	if err := os.WriteFile(precious, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	dir := filepath.Join(scratch, "leak-check")
	body := served(t, wholeCheck(t, answersItsHelp))
	if _, err := install(t, dir, theTag, body); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(locked, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	// The aside keeps the locked directory, which the test's own cleanup could not remove.
	t.Cleanup(func() {
		left, _ := filepath.Glob(filepath.Join(scratch, ".leak-check-install-*.replaced", "locked"))
		for _, dir := range left {
			_ = os.Chmod(dir, 0o755)
		}
	})
	if _, err := install(t, dir, theTag, body); err != nil {
		t.Fatalf("the second install: %v", err)
	}
	if got, err := os.ReadFile(precious); err != nil || string(got) != "kept\n" {
		t.Fatalf("%s was touched by the old install's removal: %q, %v", precious, got, err)
	}
}

// returnsWithoutAWriter runs run, what it is, and answers what it returned. A run that opened the
// fifo at fifo() plainly waits there for its other end, and one that waits so inside
// programfile.Write holds syscall.ForkLock for reading, so every later fork of this test binary,
// and every later beforeTheFirstWrite, would wait with it until the binary's deadline. So a run
// that has not returned within two minutes of its own, or one before the binary's deadline, fails
// the test there, and before it does, the fifo gets both ends (bothEnds), the run ends, and it is
// joined; each later run gets its own two minutes. A run that a loaded host makes slow, unpacking
// and running the check, still returns in time.
func returnsWithoutAWriter(t *testing.T, fifo func() string, what string, run func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	limit := testwait.Bounded(t, 2*time.Minute, time.Minute)
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
	}
	select {
	case err := <-done:
		return err
	default:
	}
	path := fifo()
	// Deferred, so the fifo is closed and its drain ends when the join below gives up too.
	defer bothEnds(t, path)()
	err := testwait.Receive(t, what+" once the fifo "+path+" had both ends", done)
	t.Fatalf("%s did not return within %s while the fifo %s had no other end; given both, it returned %v", what, limit, path, err)
	return nil
}

// bothEnds opens the fifo at path for reading and writing, which lets an open of it for either
// return, and reads all that is written to it until the function it answers closes it.
func bothEnds(t *testing.T, path string) func() {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("give %s both ends: it is no fifo (%v, %v)", path, info, err)
		return func() {}
	}
	// For reading and writing rather than for reading alone: an open for reading alone, made
	// before the blocked open has returned, can read the fifo's end at once and stop reading while
	// the write is still to come, and it does not release a run that opened the fifo for reading
	// and waits for a writer.
	fifo, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Errorf("give the fifo %s both ends: %v", path, err)
		return func() {}
	}
	go func() { _, _ = io.Copy(io.Discard, fifo) }()
	return func() { _ = fifo.Close() }
}

// beforeTheFirstWrite calls write, what it is, which makes a directory pattern matches and then
// writes into it by programfile.Write, and calls plant with that directory before the first of
// those writes: it holds syscall.ForkLock for writing, which Write takes for reading, from before
// write starts until the directory is there and plant has returned. plant answers the path it
// planted at. It answers what write answered, by returnsWithoutAWriter, so a write that waits on a
// fifo planted there fails the test by name and is ended and joined before any later test runs.
func beforeTheFirstWrite(t *testing.T, pattern string, plant func(dir string) (string, error), what string, write func() error) error {
	t.Helper()
	type planting struct {
		path string
		err  error
	}
	planted := make(chan planting, 1)
	syscall.ForkLock.Lock()
	go func() {
		defer syscall.ForkLock.Unlock()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if made, _ := filepath.Glob(pattern); len(made) == 1 {
				path, err := plant(made[0])
				planted <- planting{path, err}
				return
			}
		}
		planted <- planting{err: errors.New("nothing matched " + pattern + " within 10s")}
	}()
	// The planter answers within its own 10s poll whatever it finds, so the wait here ends.
	plantingOnce := sync.OnceValue(func() planting { return <-planted })
	err := returnsWithoutAWriter(t, func() string { return plantingOnce().path }, what, write)
	if p := plantingOnce(); p.err != nil {
		t.Fatalf("the plant: %v", p.err)
	}
	return err
}

// stillFifo fails t unless path is the fifo it was made.
func stillFifo(t *testing.T, path string) {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fifo %s is no longer there as it was (%v, %v)", path, info, err)
	}
}
