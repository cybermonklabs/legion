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

const (
	sharedWatchChildEnv = "TESTWAIT_SHARED_WATCH_CHILD"
	manyEndingRows      = 200
)

// Many tests ending at once, each while a goroutine of its own still hands bounds to Watch, leave
// the shared watch with no race and fail no row but those that fail on their own. The child runs
// 200 parallel rows; half fail. Under -race, any report is the failure this row names.
func TestManyTestsEndingAtOnceLeaveTheSharedWatchNoRace(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("this row needs the test binary's deadline: run it under a -timeout")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestManyTestsEndingAtOnceSharedWatchChild$", "-test.timeout="+(time.Until(deadline)-time.Second).String(), "-test.v")
	child.Env = append(os.Environ(), sharedWatchChildEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	err := child.Run()
	out := stdout.String() + stderr.String()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the child = %v, want exit status 1 from its rows that fail on their own:\n%.4000s", err, out)
	}
	if strings.Contains(out, "WARNING: DATA RACE") {
		t.Fatalf("the shared watch raced:\n%s", out)
	}
	failed := strings.Count(stdout.String(), "    --- FAIL: TestManyTestsEndingAtOnceSharedWatchChild/")
	passed := strings.Count(stdout.String(), "    --- PASS: TestManyTestsEndingAtOnceSharedWatchChild/")
	if failed != manyEndingRows/2 || passed != manyEndingRows/2 {
		t.Fatalf("the child's rows: %d failed and %d passed, want %d of each", failed, passed, manyEndingRows/2)
	}
}

// TestManyTestsEndingAtOnceSharedWatchChild is the child of
// TestManyTestsEndingAtOnceLeaveTheSharedWatchNoRace.
func TestManyTestsEndingAtOnceSharedWatchChild(t *testing.T) {
	if os.Getenv(sharedWatchChildEnv) == "" {
		t.Skip(sharedWatchChildEnv + " is not set: this test is the child of TestManyTestsEndingAtOnceLeaveTheSharedWatchNoRace")
	}
	for row := range manyEndingRows {
		t.Run(fmt.Sprint(row), func(t *testing.T) {
			t.Parallel()
			stop, stopped := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(stop); <-stopped })
			go func() {
				defer close(stopped)
				for {
					select {
					case <-stop:
						return
					default:
					}
					Watch(t, Bound{Function: "plant", End: time.Now().Add(time.Millisecond), Span: true, EndDescription: "the plant span"})
					time.Sleep(100 * time.Microsecond)
				}
			}()
			for range 20 {
				<-time.After(time.Millisecond)
				Watch(t, Bound{Function: "plant", End: time.Now().Add(time.Millisecond), Span: true, EndDescription: "the plant span"})
			}
			if row%2 == 0 {
				t.Error("half the rows fail, so their shared watch ends")
			}
		})
	}
}
