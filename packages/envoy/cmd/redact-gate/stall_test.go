package main

import (
	"strings"
	"testing"
)

// TestARunPastItsBoundNamesItsPanicAndTheGoroutineRunningTheTest: of what a run past its bound
// printed (go test -json carries the test binary's stderr among its output events), stall keeps
// the timeout's panic line, the tests it was running and the goroutine running each, and leaves
// out the goroutines that run no test. Where the go command killed the binary a minute later and
// only TestMain runs (an end past the bound), it keeps the kill line and TestMain's goroutine. The
// dumps are go1.27.3's, cut short.
func TestARunPastItsBoundNamesItsPanicAndTheGoroutineRunningTheTest(t *testing.T) {
	panicked := strings.Join([]string{
		"=== RUN   TestSleeps",
		"panic: test timed out after 2s",
		"\trunning tests:",
		"\t\tTestSleeps (2s)",
		"",
		"goroutine 23 [running]:",
		"testing.(*M).startAlarm.func1()",
		"\t/go/src/testing/testing.go:2961 +0x34a",
		"created by time.goFunc",
		"",
		"goroutine 1 [chan receive]:",
		"testing.(*T).Run(0x5c0889ec008, {0x557515?, 0x5c0889aca68?}, 0x6dfd10)",
		"testing.(*M).Run(0x5c0889b8140)",
		"example.com/exp/slow.TestMain(0x725280?)",
		"main.main()",
		"",
		"goroutine 22 [sleep]:",
		"time.Sleep(0x12a05f200)",
		"example.com/exp/slow.TestSleeps(0x5c0889ec488?)",
		"\t/exp/slow/slow_test.go:24 +0x36",
		"testing.tRunner(0x5c0889ec488, 0x6dfd10)",
		"created by testing.(*T).Run in goroutine 1",
		"FAIL\texample.com/exp/slow\t2.010s",
	}, "\n")
	got := stall(panicked)
	for _, want := range []string{"panic: test timed out after 2s", "\t\tTestSleeps (2s)", "goroutine 22 [sleep]:", "example.com/exp/slow.TestSleeps("} {
		if !strings.Contains(got, want) {
			t.Errorf("the timeout's stall leaves out %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"goroutine 23 [running]:", "goroutine 1 [chan receive]:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the timeout's stall holds %q, a goroutine that runs no test:\n%s", unwanted, got)
		}
	}

	killed := strings.Join([]string{
		"--- PASS: TestQuick (0.00s)",
		"PASS",
		"SIGQUIT: quit",
		"PC=0x48e4a1 m=0 sigcode=0",
		"",
		"goroutine 0 gp=0x705500 m=0 mp=0x706500 [idle]:",
		"runtime.futex(0x706658, 0x80, 0x0, 0x0, 0x0, 0x0)",
		"",
		"goroutine 1 gp=0x118d5cc321e0 m=nil [sleep]:",
		"time.Sleep(0x104c533c00)",
		"example.com/exp/slow.TestMain(0x725280?)",
		"\t/exp/slow/slow_test.go:12 +0x38",
		"main.main()",
		"",
		"goroutine 2 gp=0x118d5cc32780 m=nil [force gc (idle)]:",
		"runtime.forcegchelper()",
		"",
		"*** Test killed with quit: ran too long (1m2s).",
		"FAIL\texample.com/exp/slow\t62.064s",
	}, "\n")
	got = stall(killed)
	for _, want := range []string{"*** Test killed with quit: ran too long (1m2s).", "goroutine 1 gp=0x118d5cc321e0 m=nil [sleep]:", "example.com/exp/slow.TestMain("} {
		if !strings.Contains(got, want) {
			t.Errorf("the kill's stall leaves out %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"goroutine 0 gp=", "goroutine 2 gp="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the kill's stall holds %q, a goroutine that runs no test:\n%s", unwanted, got)
		}
	}

	// Control: output with no dump names no goroutine, and says so.
	if got := stall("--- PASS: TestQuick (0.00s)\nFAIL\texample.com/exp/slow\t1.000s"); !strings.Contains(got, "no goroutine of the dump runs a test") {
		t.Errorf("a stall of output with no dump does not say it found no goroutine:\n%s", got)
	}
}
