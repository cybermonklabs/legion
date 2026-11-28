package daemon

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// scratchDispatchLogTail is how many of a scratch Dispatch's last log lines a failed test prints
// whole (logOnFailure).
const scratchDispatchLogTail = 400

// scratchDispatchTrouble matches a line of a scratch Dispatch's log that tells of a failure: an
// error or a warning, a panic, or a request answered 5xx. Dispatch logs a request it answers 500
// twice: its error (`level=ERROR msg="dispatch: API handler failed" error=…`, api/handler_errors.go)
// and its access line (`msg="dispatch: request" … status=500`).
var scratchDispatchTrouble = regexp.MustCompile(`level=(ERROR|WARN)|panic|status=5[0-9][0-9]\b`)

// logOnFailure prints, once the test has failed, what the scratch Dispatch wrote across every
// start and stop (scratchDispatchLog), so a test that fails on Dispatch's answer, a 500 above all,
// shows why Dispatch gave it. The rig keeps Dispatch's output in memory and otherwise prints it
// only where the server fails to start or stop.
func (d *scratchDispatch) logOnFailure() {
	if !d.t.Failed() {
		return
	}
	var log strings.Builder
	for _, output := range d.starts {
		log.WriteString(output.String())
	}
	d.t.Logf("scratch Dispatch's log, %s:\n%s", d.base, scratchDispatchLog(log.String(), scratchDispatchLogTail))
}

// scratchDispatchLog is log as a failed test prints it: whole where it holds at most tail lines;
// else each earlier line that tells of a failure (scratchDispatchTrouble), wherever it lies, then
// the last tail lines whole.
func scratchDispatchLog(log string, tail int) string {
	lines := strings.SplitAfter(log, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= tail {
		return log
	}
	earlier, last := lines[:len(lines)-tail], lines[len(lines)-tail:]
	var shown strings.Builder
	trouble := 0
	for _, line := range earlier {
		if scratchDispatchTrouble.MatchString(line) {
			shown.WriteString(line)
			trouble++
		}
	}
	header := fmt.Sprintf("[%d lines; of the first %d, the %d that tell of an error, a warning, a panic or a 5xx answer; then the last %d whole]\n",
		len(lines), len(earlier), trouble, tail)
	return header + shown.String() + fmt.Sprintf("[the last %d lines]\n", tail) + strings.Join(last, "")
}

// A failed test prints a long scratch Dispatch log as the lines that tell of a failure before its
// tail, and then the tail whole, so a 500 early in a long test is printed with its error; a short
// log is printed whole.
func TestScratchDispatchLogKeepsEachFailureBeforeItsTail(t *testing.T) {
	t.Parallel()
	short := "time=1 level=INFO msg=\"dispatch: listening\"\ntime=2 level=INFO msg=\"dispatch: request\" method=GET status=200\n"
	if got := scratchDispatchLog(short, 3); got != short {
		t.Fatalf("a log of 2 lines with a tail of 3 printed %q, want it whole", got)
	}
	failed := "time=3 level=ERROR msg=\"dispatch: API handler failed\" error=\"insert issue: deadlock detected (SQLSTATE 40P01)\"\n"
	answered := "time=3 level=INFO msg=\"dispatch: request\" method=POST route=/api/v1/issues status=500 elapsed_ms=640\n"
	var log strings.Builder
	log.WriteString(failed + answered)
	for i := range 10 {
		fmt.Fprintf(&log, "time=%d level=INFO msg=\"dispatch: request\" method=GET status=200\n", 4+i)
	}
	log.WriteString("time=14 level=WARN msg=\"dispatch: slow request\"\n")
	for i := range 3 {
		fmt.Fprintf(&log, "time=%d level=INFO msg=\"dispatch: request\" method=GET status=200\n", 15+i)
	}
	got := scratchDispatchLog(log.String(), 3)
	want := "[16 lines; of the first 13, the 3 that tell of an error, a warning, a panic or a 5xx answer; then the last 3 whole]\n" +
		failed + answered + "time=14 level=WARN msg=\"dispatch: slow request\"\n" + "[the last 3 lines]\n" +
		"time=15 level=INFO msg=\"dispatch: request\" method=GET status=200\n" +
		"time=16 level=INFO msg=\"dispatch: request\" method=GET status=200\n" +
		"time=17 level=INFO msg=\"dispatch: request\" method=GET status=200\n"
	if got != want {
		t.Fatalf("the log printed\n%s\nwant\n%s", got, want)
	}
}
