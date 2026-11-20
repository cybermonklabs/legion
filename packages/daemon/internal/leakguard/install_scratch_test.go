package leakguard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The install's parent is under the scratch every role writes, so a role can swap it for a fifo
// while the install runs. The install removes its staging directory, and the old install it moved
// aside, without a plain open of that parent (rolefile.RemoveTree), so it returns at once and
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
		err := returnsWithoutAWriter(t, scratch, "the install", func() error {
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
		err := returnsWithoutAWriter(t, scratch, "the install", func() error {
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

// returnsWithoutAWriter runs run, what it is, and fails t unless it returns within 10 s of the
// moment path became a fifo, the moment the bound guards: before it an install unpacks and runs the
// check, which a loaded host makes slow without anything blocking. A run that opened the fifo
// plainly would wait there for a writer, so when the bound passes it opens the fifo for writing,
// and the blocked open and the test's goroutine end.
func returnsWithoutAWriter(t *testing.T, path, what string, run func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var bound <-chan time.Time
	for {
		select {
		case err := <-done:
			return err
		case <-poll.C:
			if info, err := os.Lstat(path); bound == nil && err == nil && info.Mode()&os.ModeNamedPipe != 0 {
				bound = time.After(10 * time.Second)
			}
		case <-bound:
			if writer, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
				_ = writer.Close()
			}
			<-done
			t.Fatalf("%s was still running 10s after %s became a fifo", what, path)
		}
	}
}

// stillFifo fails t unless path is the fifo it was made.
func stillFifo(t *testing.T, path string) {
	t.Helper()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the fifo %s is no longer there as it was (%v, %v)", path, info, err)
	}
}
