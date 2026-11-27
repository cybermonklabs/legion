// Package testexec writes the files a test then runs (a fake command, a stand-in script, a stub on
// PATH), tests a place that writes one, and holds a module to writing each one under the fork
// lock. WriteFile writes such a file with programfile.Write, whose doc says what a write by
// package os meets while another goroutine of the process forks. RunsBeside runs the writes and
// runs of a test of such a place beside forks, and fails the test if any run is refused as busy.
// Unguarded names each write a module makes by package os that can meet the fault, and
// RequireGuarded, which each module's internal/textcheck runs, fails the test on one.
// docs/testing.md, "A file a test runs", states the rule.
package testexec

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/programfile"
)

// WriteFile is os.WriteFile for a file a test will run: programfile.Write with os.O_TRUNC, so a
// file already at name is written over, as os.WriteFile writes over one, and given perm exactly.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm) // CONTROL ONLY (never merge): bypass the fork lock
}

// RunsBeside runs run(i) for i from 0 to runs-1 while eight goroutines start true and start it
// again as soon as it exits, until the test ends: the forks that the code a test calls meets in a
// process whose other goroutines start commands, as a daemon's do and as tests running beside
// each other do. run writes a file as the code under test writes it, runs it at once, and answers
// the run's error. A run refused as busy, whose error is ETXTBSY, is counted and the trials go on;
// any other error fails the test at once, naming the run. At the end the test fails if any run was
// refused, naming how many and the first refusal's error. Nothing is run again. A machine with no
// true command fails the test, and one whose kernel runs a program held open for writing skips or
// fails it as busyRunVerdict says, since no run there can meet the fault.
func RunsBeside(t testing.TB, runs int, run func(i int) error) {
	t.Helper()
	forkBeside(t)
	countBusyRuns(t, runs, run)
}

// countBusyRuns runs RunsBeside's trials and counts and reports their refusals, as its doc says.
func countBusyRuns(t testing.TB, runs int, run func(i int) error) {
	t.Helper()
	busy := 0
	var first error
	for i := range runs {
		switch err := run(i); {
		case errors.Is(err, syscall.ETXTBSY):
			busy++
			if first == nil {
				first = err
			}
		case err != nil:
			t.Fatalf("run %d of %d: %v", i+1, runs, err)
		}
	}
	if busy != 0 {
		t.Errorf("%d of %d runs were refused as busy (ETXTBSY) while the process forked; the first: %v", busy, runs, first)
	}
}

// forkBeside starts RunsBeside's eight forking goroutines, which stop when the test ends, once
// busyRunVerdict has said the test runs here.
func forkBeside(t testing.TB) {
	t.Helper()
	forker, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("no true command to fork beside the test: %v", err)
	}
	switch skip, fail := busyRunVerdict(runHeldOpen(t), os.Getenv("GITHUB_ACTIONS") == "true"); {
	case fail != "":
		t.Fatal(fail)
	case skip != "":
		t.Skip(skip)
	}
	var stop atomic.Bool
	var forks sync.WaitGroup
	for range 8 {
		forks.Go(func() {
			for !stop.Load() {
				_ = exec.Command(forker).Run()
			}
		})
	}
	t.Cleanup(func() {
		stop.Store(true)
		forks.Wait()
	})
}

// busyRunVerdict says whether a test of RunsBeside runs, skips or fails, from answer, the error of
// a run of a program the process held open for writing, where ci says GITHUB_ACTIONS is set. It
// answers both empty to run the test, or the reason to skip or to fail it, saying what the run
// answered. ETXTBSY runs the test: the kernel refuses such a run, as Linux does every time. A run
// (nil) means no run of the test can meet the fault: the test skips, as under gVisor, except under
// GITHUB_ACTIONS, where it fails, since CI's runners refuse such a run and one that does not is a
// broken runner, never a green job with the proof gone from it. Any other answer, such as EACCES
// from a TMPDIR mounted noexec or ENOENT from a machine with no /bin/sh, fails the test.
func busyRunVerdict(answer error, ci bool) (skip, fail string) {
	const (
		ran   = "this kernel ran a program this process held open for writing rather than refuse it with ETXTBSY, "
		blind = "so no run of this test can meet the fault it looks for"
	)
	switch {
	case errors.Is(answer, syscall.ETXTBSY):
		return "", ""
	case answer == nil && ci:
		return "", ran + blind + "; under GITHUB_ACTIONS that is a broken runner, not a test to skip"
	case answer == nil:
		return ran + "as gVisor does, " + blind, ""
	default:
		return "", "a run of a program this process held open for writing answered " + answer.Error() +
			", neither ETXTBSY nor a run, " + blind
	}
}

// runHeldOpen writes a program, opens it again for writing, runs it while it is open and answers
// the run's error: ETXTBSY on a kernel that refuses to run a file some process holds open for
// writing, as Linux does every time, and nil on one that runs it.
func runHeldOpen(t testing.TB) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "held-open")
	if err := programfile.Write(path, os.O_EXCL, 0o700, strings.NewReader("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatalf("write the program the kernel's refusal is read from: %v", err)
	}
	held, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("hold the program open for writing: %v", err)
	}
	defer held.Close()
	return exec.Command(path).Run()
}

// RequireGuarded fails the test once if Unguarded names a write under root, a module's root,
// saying how many it named, each as file:line, and the writer to write each with, or if Unguarded
// refuses root, saying what it could not read.
func RequireGuarded(t testing.TB, root string) {
	t.Helper()
	offences, err := Unguarded(root)
	if err != nil {
		t.Fatalf("read the module's Go files: %v", err)
	}
	if len(offences) > 0 {
		t.Fatalf("%d writes of a file that runs can meet a fork beside it:\n\t%s\n"+
			"write each with programfile.Write, or with testexec.WriteFile in a test (docs/testing.md at the repository's root, \"A file a test runs\")",
			len(offences), strings.Join(offences, "\n\t"))
	}
}
