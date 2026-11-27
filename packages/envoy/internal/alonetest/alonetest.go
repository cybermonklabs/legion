// Package alonetest runs one test again in a run of its test binary of its own, for a test that
// measures something the whole process shares. testing.AllocsPerRun counts every allocation the
// process makes while it measures, and the other tests of a package leave servers and watchers
// running that allocate meanwhile, so such a test measures in a process where it runs alone. It is
// an ordinary package rather than a _test.go file because Go cannot share test-only code across
// packages (see internal/ordertest).
package alonetest

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// measuredPrefix begins each line Measured logs, which Run counts.
const measuredPrefix = "measured alone: "

// Child reports whether this process is the run that env marks: the run of one test alone that
// Run starts.
func Child(env string) bool { return os.Getenv(env) != "" }

// Run runs the test binary again for t's test alone, with env set, and logs what that run printed.
// It fails t unless that run passed and logged exactly want measurements (Measured), so a run that
// measured nothing is no pass.
func Run(t *testing.T, env string, want int) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), env+"=1")
	output, err := child.CombinedOutput()
	t.Logf("the run of this test alone:\n%s", output)
	if err != nil {
		t.Fatalf("the run of this test alone failed: %v", err)
	}
	if measured := strings.Count(string(output), measuredPrefix); measured != want {
		t.Fatalf("the run of this test alone reported %d measurements; want %d", measured, want)
	}
}

// Measured logs one measurement of the run alone, in the form Run counts.
func Measured(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(measuredPrefix + fmt.Sprintf(format, args...))
}
