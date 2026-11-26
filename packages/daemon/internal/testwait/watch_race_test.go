// packages/envoy/internal/testwait/watch_race_test.go and
// packages/daemon/internal/testwait/watch_race_test.go are the same file, as watch.go is.

package testwait

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// manyEndingRows is how many parallel rows TestManyTestsEndingAtOnceChild runs; half of them fail.
const manyEndingRows = 200

// Many tests ending at once, each while a goroutine of its own still takes bounds, leave the watch
// with no data race and fail no row but those that fail on their own: the child runs 200 parallel
// rows, each of which takes fresh 1 ms spans in a loop on the row's goroutine and on one it starts,
// which stops only after the row's watch has ended; half the rows fail, so their watch's end reads
// and prints. Built with -race, the child's detector reports a race between a watch's end and a
// bound taken beside it, and fails rows that pass on their own; this row fails on either. The
// child runs under what this test's deadline leaves less 30 s.
func TestManyTestsEndingAtOnceLeaveTheWatchNoRace(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("this row needs the test binary's deadline: run it under a -timeout")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestManyTestsEndingAtOnceChild$", "-test.timeout="+(time.Until(deadline)-30*time.Second).String(), "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	out := stdout.String() + stderr.String()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the child = %v, want exit status 1, from the rows that fail on their own; its output:\n%.4000s", err, out)
	}
	if strings.Contains(out, "WARNING: DATA RACE") {
		_, report, _ := strings.Cut(out, "WARNING: DATA RACE")
		t.Fatalf("the child's race detector reported a race:\nWARNING: DATA RACE%.6000s", report)
	}
	failed := strings.Count(stdout.String(), "    --- FAIL: TestManyTestsEndingAtOnceChild/")
	passed := strings.Count(stdout.String(), "    --- PASS: TestManyTestsEndingAtOnceChild/")
	if failed != manyEndingRows/2 || passed != manyEndingRows/2 {
		t.Fatalf("the child's rows: %d failed and %d passed, want %d of each", failed, passed, manyEndingRows/2)
	}
}

// TestManyTestsEndingAtOnceChild is the child of TestManyTestsEndingAtOnceLeaveTheWatchNoRace. It
// runs only when the parent re-executes this binary.
func TestManyTestsEndingAtOnceChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestManyTestsEndingAtOnceLeaveTheWatchNoRace")
	}
	for row := range manyEndingRows {
		t.Run(fmt.Sprint(row), func(t *testing.T) {
			t.Parallel()
			stop, stopped := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				close(stop)
				<-stopped
			})
			go func() {
				defer close(stopped)
				for {
					select {
					case <-stop:
						return
					default:
					}
					Bounded(t, time.Millisecond, 0)
					time.Sleep(100 * time.Microsecond)
				}
			}()
			for range 20 {
				<-time.After(Bounded(t, time.Millisecond, 0))
			}
			if row%2 == 0 {
				t.Error("half the rows fail, so their watch's end reads and prints")
			}
		})
	}
}
