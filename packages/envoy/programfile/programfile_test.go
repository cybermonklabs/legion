package programfile_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/programfile"
	"github.com/sjawhar/envoy/testexec"
)

// A program Write writes runs at once however many goroutines of the process fork beside it: this
// test writes programs one at a time and runs each as soon as it is written, beside
// testexec.RunsBeside's forks. A write by os.WriteFile there met ETXTBSY in 99 runs of 2,000 on a
// dev box with eight CPUs, so the 400 runs here would meet it about 20 times without Write. Once
// internal/daemon's tests ran beside each other, a gate test whose fake command os.WriteFile wrote
// failed so under load (TestTheSessionStorageProbeClassifiesWhatOhMyPiSaid/refuses, one run in
// 660).
//
// Not parallel: its forks are the condition it measures.
func TestAProgramWrittenWhileTheProcessForksRuns(t *testing.T) {
	dir := t.TempDir()
	testexec.RunsBeside(t, 400, func(i int) error {
		path := filepath.Join(dir, fmt.Sprintf("program-%d", i))
		if err := programfile.Write(path, os.O_EXCL, 0o755, strings.NewReader("#!/bin/sh\nexit 0\n")); err != nil {
			return fmt.Errorf("write the program: %w", err)
		}
		if err := exec.Command(path).Run(); err != nil {
			return fmt.Errorf("run the program: %w", err)
		}
		return nil
	})
}

// A program Append adds to runs at once in the same way: each run writes a program's first line
// with Write, which holds the lock, adds its last with Append and runs it, so the one span a fork
// beside it could meet the program open for writing in is Append's.
//
// Not parallel: its forks are the condition it measures.
func TestAProgramAppendedToWhileTheProcessForksRuns(t *testing.T) {
	dir := t.TempDir()
	testexec.RunsBeside(t, 400, func(i int) error {
		path := filepath.Join(dir, fmt.Sprintf("program-%d", i))
		if err := programfile.Write(path, os.O_EXCL, 0o755, strings.NewReader("#!/bin/sh\n")); err != nil {
			return fmt.Errorf("write the program: %w", err)
		}
		if err := programfile.Append(path, strings.NewReader("exit 0\n")); err != nil {
			return fmt.Errorf("append to the program: %w", err)
		}
		if err := exec.Command(path).Run(); err != nil {
			return fmt.Errorf("run the program: %w", err)
		}
		return nil
	})
}

// Write gives the file the mode it names whatever the umask, writes what the reader holds, and
// opens with the caller's flag: os.O_EXCL refuses a file already there and leaves it as it was,
// and os.O_TRUNC writes over one.
//
// Not parallel: it sets the process's umask.
func TestWriteGivesTheModeItNamesAndOpensWithTheFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "program")
	umask := syscall.Umask(0o077)
	err := programfile.Write(path, os.O_EXCL, 0o755, strings.NewReader("#!/bin/sh\necho first\n"))
	syscall.Umask(umask)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("the program's mode = %v under umask 077, want -rwxr-xr-x", info.Mode().Perm())
	}
	if out, err := exec.Command(path).Output(); err != nil || string(out) != "first\n" {
		t.Errorf("the program printed %q, %v, want first", out, err)
	}

	if err := programfile.Write(path, os.O_EXCL, 0o700, strings.NewReader("#!/bin/sh\necho second\n")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("Write with os.O_EXCL over a file = %v, want it refused as existing", err)
	}
	if out, err := exec.Command(path).Output(); err != nil || string(out) != "first\n" {
		t.Errorf("after the refused write the program printed %q, %v, want first", out, err)
	}

	if err := programfile.Write(path, os.O_TRUNC, 0o700, strings.NewReader("#!/bin/sh\necho third\n")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("after the write over it the program's mode = %v, want -rwx------", info.Mode().Perm())
	}
	if out, err := exec.Command(path).Output(); err != nil || string(out) != "third\n" {
		t.Errorf("after the write over it the program printed %q, %v, want third", out, err)
	}
}
