// Package leakguardtest writes the stand-in shared public leak checks the guard's own tests and
// the tests of everything that drives the guard are run against. The real check is a script no
// clone of this repository holds — a pod installs it from a private repository — and the guard
// refuses a write it can run no check for, so every test that reaches the guard carries a
// stand-in of its own: a shell script under the real check's name, which reads its standard input
// as the real one does, so nothing is left in a pipe when it exits, and whose exit status is the
// verdict.
//
// What a caller needs of that stand-in differs, so the shapes are offered rather than flattened
// into one: Passes, Refuses and CannotFinish are the plain ones, Refusing is a refusal whose
// report is the caller's own, Recording records the call it was handed, and Reading is the
// two-go shape of a call whose body is on standard input.
//
// It is an ordinary package rather than a _test.go file of one of them because cmd/legion is
// package main, which can import no other package's test-only helpers (internal/fixturegit says
// the same of itself), and because internal/rescue/rescuetest, which builds a pod's disk, needs
// the same scripts. It imports the guard not at all, so that the guard's own in-package tests can
// use it: a file of package leakguard importing a package that imported leakguard would be an
// import cycle.
package leakguardtest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// CheckName is the shared check's file name, which is how the guard finds it on PATH. The guard
// spells it for itself (leakguard.checkName, unexported), and the two parting is what the guard's
// own resolution tests fail on: they write a stand-in with Write and read it back through the
// guard's PATH lookup (leakguard/check_test.go).
const CheckName = "public-leak-check.sh"

// recordVar names the file a recording stand-in appends what it was asked to, which is how a test
// reads whether the check ran at all and with what. A stand-in run without it records nowhere.
const recordVar = "LEAK_CHECK_RECORD"

// stdinVar names the file the guard hands a check that answered the exit-4 handshake: the call's
// standard input, buffered. It is the guard's own variable (leakguard's gh.go), spelled here
// because this package imports the guard not at all.
const stdinVar = "PUBLIC_LEAK_CHECK_STDIN"

// readsStdin is what every plain stand-in begins with: the real check reads its standard input,
// and one that did not would leave the guard writing into a pipe nobody drains.
const readsStdin = "cat >/dev/null 2>&1\n"

// The plain stand-ins. Passes clears every write; Refuses refuses it as the check refuses one of
// ours, its report and exit 3; CannotFinish is the check that could not be made to run, which the
// guard reads as a setup failure rather than a verdict.
const (
	Passes       = readsStdin + "exit 0\n"
	Refuses      = readsStdin + "echo 'LEAK CHECK: refusing: one of ours'\nexit 3\n"
	CannotFinish = readsStdin + "echo 'LEAK CHECK SETUP ERROR: python3 did not start'\nexit 2\n"
)

// Refusing is Refuses with a report of the caller's own, the line the check prints before it
// exits 3 and the whole of what a refusal carries back from it. A caller whose tests read that
// text back — `legion gh`, whose report names the call it refused — builds its stand-in from
// here rather than from Refuses.
func Refusing(report string) string {
	return readsStdin + "echo '" + report + "'\nexit 3\n"
}

// Recording is script with a line before it that appends the call's own arguments to the file
// recordVar names, so a test can read that the guard ran the check and what it handed it.
func Recording(script string) string {
	return "printf 'checked %s\\n' \"$*\" >>\"${" + recordVar + ":-/dev/null}\"\n" + script
}

// Reading is the check's two-go shape for a call whose body is on standard input: the first run
// exits 4 to say the call reads standard input, and the second, handed that input as the file
// stdinVar names, records what it read in recordVar and ends with status — 0 for a call it
// clears, anything else for one it refuses, report being the line it prints first.
func Reading(status int, report string) string {
	refusal := ""
	if report != "" {
		refusal = "echo '" + report + "'\n"
	}
	return "if [ -z \"$" + stdinVar + "\" ]; then exit 4; fi\n" +
		"printf 'checked stdin %s\\n' \"$(cat \"$" + stdinVar + "\")\" >>\"${" + recordVar + ":-/dev/null}\"\n" +
		refusal + fmt.Sprintf("exit %d\n", status)
}

// WriteFile writes script as the body of a stand-in shared check in dir, under the real check's
// own name, and answers its path. The file is executable, since the guard runs it as a program,
// and a second call over the same directory replaces it, which is how a test moves a disk from
// one stand-in to another. It answers an error rather than failing, for the caller that has no
// *testing.T to fail: a TestMain, which runs before any test has one (internal/workspace's
// stand-in, written once for the package's own home).
func WriteFile(dir, script string) (string, error) {
	path := filepath.Join(dir, CheckName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// Write is WriteFile for a test, which it fails rather than answering the error to.
func Write(t testing.TB, dir, script string) string {
	t.Helper()
	path, err := WriteFile(dir, script)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
