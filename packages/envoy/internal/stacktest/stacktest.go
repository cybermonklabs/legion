// Package stacktest runs a test body under a goroutine stack smaller than Go's one-gigabyte
// default, so a recursion whose depth a caller controls is proved bounded rather than merely
// bounded by the largest input the test could afford to build.
//
// A stack overflow is a fatal error: no recover sees it, and it ends the process. So the bounded
// run happens in a child process - the test binary again, running that one test with the cap in
// its environment - and the parent reports the child's death as the test's own failure.
//
// It is an ordinary package rather than a _test.go file because Go cannot share test-only code
// across packages, and both pmdoc and docs bound a recursion this way.
package stacktest

import (
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// maxStackEnv carries the cap, in bytes, to the child process the parent runs, which caps its
// stacks at that value: a child run by hand with it set runs the body under the cap it names.
const maxStackEnv = "ENVOY_TEST_MAX_STACK_BYTES"

// childMargin is how long before the parent's deadline the child's ends: the time the parent takes
// to read the child's output and report it.
const childMargin = time.Second

// Under runs body with every goroutine's stack capped at maxStackBytes.
//
// In the parent it starts the test binary again for this one test, with the cap in the
// environment, and mirrors the child's result: a skipped child skips the parent, a passing child
// passes, and any failure shows the child's whole output - a stack overflow's fatal error among it.
// The child runs under what is left of the parent's deadline less childMargin (-test.timeout), so
// a wait in the body gives up by name (testwait) before the parent's own alarm and the parent has
// time left to print why; with less than that left, the child gets a millisecond and fails at once
// rather than run with no deadline. Under no deadline the child has none either. -test.timeout
// watches only the child's tests, so the parent runs the child on the shared wait's context
// (testwait.Context): a child still running once the parent's deadline less testwait's margin has
// passed, one held in its TestMain after its tests among them, is killed, and the parent fails
// with its output. In the child, where the cap is set, it runs body under that cap.
func Under(t *testing.T, maxStackBytes int, body func(t *testing.T)) {
	t.Helper()
	if value := os.Getenv(maxStackEnv); value != "" {
		capped, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("%s=%q is not a byte count: %v", maxStackEnv, value, err)
		}
		debug.SetMaxStack(capped)
		body(t)
		return
	}
	args := []string{"-test.run", runPattern(t.Name()), "-test.v", "-test.count=1"}
	if deadline, ok := t.Deadline(); ok {
		args = append(args, "-test.timeout="+max(time.Until(deadline)-childMargin, time.Millisecond).String())
	}
	child := exec.CommandContext(testwait.Context(t), os.Args[0], args...)
	child.Env = append(os.Environ(), maxStackEnv+"="+strconv.Itoa(maxStackBytes))
	output, err := child.CombinedOutput()
	if err == nil && hasVerdict(string(output), "SKIP", t.Name()) {
		t.Skipf("under a %d-byte stack cap, %s skipped:\n%s", maxStackBytes, t.Name(), output)
	}
	if ran := hasVerdict(string(output), "PASS", t.Name()); err != nil || !ran {
		t.Fatalf("under a %d-byte stack cap, %s: %v\n%s", maxStackBytes, t.Name(), err, output)
	}
}

// hasVerdict reports whether output holds go test's verdict line for the test name. go test ends
// that line with the test's duration, so a subtest's line, which carries name as a prefix, is not
// the test's own.
func hasVerdict(output, verdict, name string) bool {
	return strings.Contains(output, "--- "+verdict+": "+name+" (")
}

// runPattern is the -test.run value naming exactly name: go test splits the pattern on "/" and
// matches each part against one name of the test's path, so each part is anchored on its own.
func runPattern(name string) string {
	parts := strings.Split(name, "/")
	for index, part := range parts {
		parts[index] = "^" + regexp.QuoteMeta(part) + "$"
	}
	return strings.Join(parts, "/")
}
