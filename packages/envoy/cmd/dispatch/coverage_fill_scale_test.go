package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// coverageFillURLEnv names the database TestTheServerFillsTheCoverageCountsAtScale starts the
// server on: the copy of the corpus at S that embedqueue's TestCoverageStatementScale leaves under
// COVERAGE_SCALE_SERVE=1 (copyForServerStart there), with done_at unset, which
// scripts/coverage-scale.sh names here and drops once the test ends. CI never sets it.
const coverageFillURLEnv = "DISPATCH_COVERAGE_FILL_URL"

// coverageFillTimeoutHook is the server's own test hook for the fill's timeout. The test hands it
// to both starts when it is set in its own environment, so the check's negative control can run
// the server with a timeout below four times the fill, or below the fill; a Job leaves it unset,
// and the server runs under the coverageFillTimeout it was built with.
const coverageFillTimeoutHook = "DISPATCH_TEST_COVERAGE_FILL_TIMEOUT"

// The lines of the counts loop (embedqueue/counts.go) the fill-length check reads beside
// coverage_counts_test.go's: the start of a fill and of a recount, each naming the timeout it runs
// under, and a recount that failed or reached its bound.
const (
	coverageFillStartedLine    = `msg="dispatch embedqueue: coverage counts fill started"`
	coverageRecountStartedLine = `msg="dispatch embedqueue: coverage counts recount started"`
	coverageRecountFailedLine  = `level=ERROR msg="dispatch embedqueue: coverage counts recount failed; it runs again later"`
)

// The test's own bounds, none of them the check's: how long a start at scale may take to listen,
// its migrations and boot included; how long after it listens the counts loop's first run may
// take to log its start; and how long past the timeout that start line names the test waits for
// the run's end or its failure, which a run that reaches its timeout logs at that timeout.
const (
	fillLengthListenWithin = 10 * time.Minute
	fillLengthStartWithin  = 2 * time.Minute
	fillLengthEndMargin    = 2 * time.Minute
)

// countsStart is what one start of the server logged, read from its own lines: when it listened
// (the listening line's time) and its since_start_ms; the timeout the counts loop's first run
// names in its start line, coverageFillTimeout for a fill and coverageRecountTimeout for a
// recount; that run's elapsed_ms; and when it ended (its end line's time).
type countsStart struct {
	listened   time.Time
	sinceStart int64
	timeout    time.Duration
	elapsed    time.Duration
	ended      time.Time
}

// endedAfterListening is how long after the server listened its run ended, negative when it ended
// first.
func (s countsStart) endedAfterListening() time.Duration { return s.ended.Sub(s.listened) }

// TestTheServerFillsTheCoverageCountsAtScale is the fill-length check's start at scale S: the
// head's binary started (startDispatchProcessWith) on the database DISPATCH_COVERAGE_FILL_URL
// names, scripts/coverage-scale.sh's copy of the corpus at S with done_at unset, as the first
// deploy of the counts finds production, and started again on it once its fill has ended, with
// done_at set. It reads every time from the server's own lines: since_start_ms of `dispatch:
// listening`; the first run's start line, which names the timeout it runs under; and its end
// line's elapsed_ms and time, against the listening line's time. The first start is stopped once
// its fill line is logged.
//
// It prints one line that starts `fill-length:` for each result, and last a line that starts
// `fill-length: pass` or `fill-length: FAIL` naming every number the verdict rests on. A missing
// line fails it at once. The first start fails it when it listened after its fill ended (or in
// the same millisecond), when the fill failed or reached its timeout, or when coverageFillTimeout
// is less than four times the fill's time; the second start fails it when it logged a fill, when
// it listened after its recount ended, when the recount failed or reached its bound, or when
// coverageRecountTimeout is less than four times the recount's time.
func TestTheServerFillsTheCoverageCountsAtScale(t *testing.T) {
	databaseURL := os.Getenv(coverageFillURLEnv)
	if databaseURL == "" {
		t.Skip(coverageFillURLEnv + " is unset: it must name the copy of the corpus at scale S that " +
			"packages/envoy/scripts/coverage-scale.sh makes under COVERAGE_SCALE_SERVE=1; CI never sets it")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("read %s: %v", coverageFillURLEnv, err)
	}
	database, err := store.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open %s: %v", config.Database, err)
	}
	t.Cleanup(database.Pool.Close)
	fillLengthLine("database %s", config.Database)
	if filled(t, database) {
		failFillLength(t, nil, "search_coverage_filled holds done_at before the first start, so %s is no first deploy's database", config.Database)
	}
	options := dispatchOptions{}
	if timeout := os.Getenv(coverageFillTimeoutHook); timeout != "" {
		options.Env = map[string]string{coverageFillTimeoutHook: timeout}
		fillLengthLine("both starts run with %s=%s from the test's environment", coverageFillTimeoutHook, timeout)
	}

	var failures []string
	first := startDispatchProcessWith(t, databaseURL, options)
	fill := readCountsStart(t, first, "the first start", "fill", coverageFillStartedLine, coverageFilledLine, coverageFillFailedLine)
	fillLengthLine("first start (done_at unset): listened at since_start_ms=%d", fill.sinceStart)
	fillLengthLine("first start: the fill took elapsed_ms=%d", fill.elapsed.Milliseconds())
	fillLengthLine("first start: the fill ended %d ms after the server listened", fill.endedAfterListening().Milliseconds())
	fillLengthLine("first start: coverageFillTimeout in force %v, 4 x the fill %v", fill.timeout, 4*fill.elapsed)
	if fill.endedAfterListening() <= 0 {
		failures = append(failures, fmt.Sprintf("the first start listened %d ms after its fill ended, so the start waited for the fill",
			-fill.endedAfterListening().Milliseconds()))
	}
	if fill.timeout < 4*fill.elapsed {
		failures = append(failures, fmt.Sprintf("coverageFillTimeout, %v, is less than four times the fill's %d ms", fill.timeout, fill.elapsed.Milliseconds()))
	}
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")
	if !filled(t, database) {
		failFillLength(t, first, "the first start logged its fill's end and left done_at unset")
	}

	second := startDispatchProcessWith(t, databaseURL, options)
	recount := readCountsStart(t, second, "the second start", "recount", coverageRecountStartedLine, coverageRecountedLine, coverageRecountFailedLine)
	loggedFill := second.logged(coverageFillStartedLine) || second.logged(coverageFilledLine) || second.logged(coverageFillFailedLine)
	fillLengthLine("second start (done_at set): listened at since_start_ms=%d", recount.sinceStart)
	fillLengthLine("second start: logged a fill: %t", loggedFill)
	fillLengthLine("second start: the recount took elapsed_ms=%d", recount.elapsed.Milliseconds())
	fillLengthLine("second start: the recount ended %d ms after the server listened", recount.endedAfterListening().Milliseconds())
	fillLengthLine("second start: coverageRecountTimeout in force %v, 4 x the recount %v", recount.timeout, 4*recount.elapsed)
	if loggedFill {
		failures = append(failures, "the second start, on counts a fill had filled, logged a fill")
	}
	if recount.endedAfterListening() <= 0 {
		failures = append(failures, fmt.Sprintf("the second start listened %d ms after its recount ended, so the start waited for the recount",
			-recount.endedAfterListening().Milliseconds()))
	}
	if recount.timeout < 4*recount.elapsed {
		failures = append(failures, fmt.Sprintf("coverageRecountTimeout, %v, is less than four times the recount's %d ms", recount.timeout, recount.elapsed.Milliseconds()))
	}
	second.Terminate(t)
	second.WaitExit(t, "SIGTERM")

	verdict := "pass"
	if len(failures) > 0 {
		verdict = "FAIL: " + strings.Join(failures, "; ")
	}
	fillLengthLine("%s: database %s; the first start listened at since_start_ms=%d, its fill took %d ms and ended %d ms after it listened, "+
		"coverageFillTimeout %v against 4 x the fill %v; the second start logged a fill: %t, listened at since_start_ms=%d, its recount took %d ms "+
		"and ended %d ms after it listened, coverageRecountTimeout %v against 4 x the recount %v",
		verdict, config.Database, fill.sinceStart, fill.elapsed.Milliseconds(), fill.endedAfterListening().Milliseconds(), fill.timeout, 4*fill.elapsed,
		loggedFill, recount.sinceStart, recount.elapsed.Milliseconds(), recount.endedAfterListening().Milliseconds(), recount.timeout, 4*recount.elapsed)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// readCountsStart reads start, the label of p's start, from p's own lines: it waits for p to
// listen, reads the listening line's time and since_start_ms, waits for the counts loop's first
// run, run, to log its start (started) and reads the timeout that line names, then waits for the
// run's end (ended) and reads its elapsed_ms and time. A run that logs failed, whether it then
// reached its timeout or not, fails the test, as does any line that is missing or that names no
// value it should.
func readCountsStart(t *testing.T, p *dispatchProcess, start, run, started, ended, failed string) countsStart {
	t.Helper()
	if err := p.healthyWithin(fillLengthListenWithin); err != nil {
		failFillLength(t, nil, "%s never listened within %v: %v", start, fillLengthListenWithin, err)
	}
	var read countsStart
	listening := requireLogLine(t, p, start, p.listeningLine())
	read.listened = lineTime(t, p, start, listening)
	read.sinceStart = lineMilliseconds(t, p, start, listening, "since_start_ms")

	runStart := awaitLogLine(t, p, fillLengthStartWithin, start, run+"'s start", started, failed)
	if strings.Contains(runStart, failed) {
		failFillLength(t, p, "%s: the %s failed before it logged its start: %s", start, run, runStart)
	}
	timeout, ok := horizontest.Field(runStart, "timeout")
	if !ok {
		failFillLength(t, p, "%s: the %s's start line names no timeout: %s", start, run, runStart)
	}
	var err error
	if read.timeout, err = time.ParseDuration(timeout); err != nil {
		failFillLength(t, p, "%s: the %s's start line names the timeout %q, which is no duration: %s", start, run, timeout, runStart)
	}

	// The failure first: a run that failed is the check's failure even where a later run ended.
	runEnd := awaitLogLine(t, p, read.timeout+fillLengthEndMargin, start, run+"'s end", failed, ended)
	if strings.Contains(runEnd, failed) {
		reached := ""
		if strings.Contains(runEnd, "timed_out=true") || strings.Contains(runEnd, "deadline exceeded") || strings.Contains(runEnd, "SQLSTATE 57014") {
			reached = " and reached its timeout"
		}
		failFillLength(t, p, "%s: the %s failed%s: %s", start, run, reached, runEnd)
	}
	read.elapsed = time.Duration(lineMilliseconds(t, p, start, runEnd, "elapsed_ms")) * time.Millisecond
	read.ended = lineTime(t, p, start, runEnd)
	return read
}

// requireLogLine answers the first line of p's output that holds line, failing the test when there
// is none.
func requireLogLine(t *testing.T, p *dispatchProcess, start, line string) string {
	t.Helper()
	lines := p.logLines(line)
	if len(lines) == 0 {
		failFillLength(t, p, "%s logged no line holding %s", start, line)
	}
	return lines[0]
}

// awaitLogLine waits up to within for p to log a line holding one of lines, and answers the first
// line of p's output holding the first of lines, in their order, that p has logged; a process that
// exits first, or a bound that passes first, fails the test naming what, the line it waited for.
func awaitLogLine(t *testing.T, p *dispatchProcess, within time.Duration, start, what string, lines ...string) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, line := range lines {
			if found := p.logLines(line); len(found) > 0 {
				return found[0]
			}
		}
		select {
		case <-p.Exited:
			// Exited closes only once the output is whole, so one more read is final.
			for _, line := range lines {
				if found := p.logLines(line); len(found) > 0 {
					return found[0]
				}
			}
			failFillLength(t, p, "%s exited before it logged its %s", start, what)
		default:
		}
		if time.Now().After(deadline) {
			failFillLength(t, p, "%s logged no line of its %s within %v", start, what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lineTime is line's own time, which the text handler writes first, to the millisecond.
func lineTime(t *testing.T, p *dispatchProcess, start, line string) time.Time {
	t.Helper()
	value, ok := horizontest.Field(line, "time")
	if !ok {
		failFillLength(t, p, "%s: a line names no time: %s", start, line)
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		failFillLength(t, p, "%s: a line names the time %q, which is no time: %s", start, value, line)
	}
	return at
}

// lineMilliseconds is the whole number of milliseconds key names in line.
func lineMilliseconds(t *testing.T, p *dispatchProcess, start, line, key string) int64 {
	t.Helper()
	value, ok := horizontest.Field(line, key)
	if !ok {
		failFillLength(t, p, "%s: a line names no %s: %s", start, key, line)
	}
	milliseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		failFillLength(t, p, "%s: a line names %s=%q, which is no whole number: %s", start, key, value, line)
	}
	return milliseconds
}

// fillLengthLine prints one result of the check on standard output, on a line that starts
// `fill-length:`, so it begins the line in the Job's log, as the scale test's `coverage-scale:`
// lines do.
func fillLengthLine(format string, args ...any) {
	fmt.Printf("fill-length: "+format+"\n", args...)
}

// failFillLength prints the reason on a `fill-length: FAIL:` line and ends the test with it and,
// when p is not nil, p's whole output.
func failFillLength(t *testing.T, p *dispatchProcess, format string, args ...any) {
	t.Helper()
	reason := fmt.Sprintf(format, args...)
	fillLengthLine("FAIL: %s", reason)
	if p != nil {
		t.Fatalf("%s\n%s\n%s", reason, dispatchOutputHeading, p.Output.String())
	}
	t.Fatal(reason)
}
