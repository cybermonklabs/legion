// Package hostproctest runs part of a test in a child of its own test binary, for the tests of a
// hostproc refusal: a refusal ends the binary it happens in (hostproc.Check), so a test that wants
// to see one, or to see that one does not happen, runs the part that may refuse where the end of a
// binary is something it can read.
//
// It is an ordinary package rather than a _test.go file so every test package can import it; Go
// cannot share test-only code across packages.
package hostproctest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
)

// childEnv names the one test a child of the test binary runs body for: the full name of the test
// that started it. The name is no LEGION_ variable, which a package's TestMain takes out of the
// process (fixtureenv.Guard).
const childEnv = "HOSTPROCTEST_CHILD"

// ranToItsEnd is the line a child writes once body has returned.
const ranToItsEnd = "hostproctest: the body ran to its end"

// Refused runs body in a child test binary that runs this test alone, and fails t unless the child
// ended with hostproc's refusal of scanner before body returned. It answers what the child wrote.
// In the child it runs body and then ends the child's test (t.SkipNow), so nothing after the call
// runs there.
func Refused(t *testing.T, scanner string, body func()) string {
	t.Helper()
	if inChild(t, body) {
		return ""
	}
	output, code := runChild(t)
	want := hostproc.Refusal + " " + scanner + " would list "
	if code != hostproc.RefusalStatus || !strings.Contains(output, want) || strings.Contains(output, ranToItsEnd) {
		t.Fatalf("the child exited %d, want hostproc's refusal of %s (exit %d, %q):\n%s", code, scanner, hostproc.RefusalStatus, want, output)
	}
	return output
}

// Allowed runs body in a child test binary as Refused does, and fails t unless the child ran body to
// its end and passed: no scanner body reached was refused.
func Allowed(t *testing.T, body func()) string {
	t.Helper()
	if inChild(t, body) {
		return ""
	}
	output, code := runChild(t)
	if code != 0 || !strings.Contains(output, ranToItsEnd) || strings.Contains(output, hostproc.Refusal) {
		t.Fatalf("the child exited %d, want body run to its end with no refusal:\n%s", code, output)
	}
	return output
}

// inChild runs body when this process is the child the test started, and then ends the child's
// test. It answers false in the test that starts the child.
func inChild(t *testing.T, body func()) bool {
	t.Helper()
	if os.Getenv(childEnv) != t.Name() {
		return false
	}
	body()
	fmt.Fprintln(os.Stderr, ranToItsEnd)
	t.SkipNow()
	return true
}

// runChild runs the test binary again on this one test, with childEnv naming it and the record's
// TestEnv naming its top-level test (hostproc.ChildEnv), and answers what it wrote and its exit
// status.
func runChild(t *testing.T) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bound := 2 * time.Minute
	if deadline, ok := t.Deadline(); ok && time.Until(deadline)-5*time.Second < bound {
		bound = time.Until(deadline) - 5*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	command := exec.CommandContext(ctx, self, "-test.run="+runPattern(t.Name()), "-test.count=1", "-test.v")
	command.Env = append(os.Environ(), childEnv+"="+t.Name(), hostproc.ChildEnv(t))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return output.String(), 0
	case errors.As(err, &exit) && exit.Exited():
		return output.String(), exit.ExitCode()
	}
	t.Fatalf("the child did not exit by itself: %v\n%s", err, output.String())
	return "", 0
}

// runPattern is the -test.run pattern that matches the test named name and no other: each level of
// the name, quoted and anchored.
func runPattern(name string) string {
	levels := strings.Split(name, "/")
	for i, level := range levels {
		levels[i] = "^" + regexp.QuoteMeta(level) + "$"
	}
	return strings.Join(levels, "/")
}
