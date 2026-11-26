package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sjawhar/legion/daemon/internal/programbound"
)

// testDeadlineEnv names, to this package's test binary run as `legion` (TestMain), the deadline of
// the test binary that started it, in RFC 3339 with nanoseconds. Only this test binary reads it;
// the legion binary has no such variable.
const testDeadlineEnv = "LEGION_CMD_TEST_DEADLINE"

// testBoundMargin is how long before the test binary's deadline a wait of a test here gives up:
// the time a row whose fact never came has to fail by name, and its cleanups to run, before go
// test's alarm panics the binary. It is testwait.Receive's, and programbound's.
const testBoundMargin = 30 * time.Second

// testBinaryDeadline is the deadline go test's -timeout gives this binary, reckoned from now, and
// false under -timeout 0, which gives none. TestMain calls it before m.Run, which starts its own
// alarm a moment later, so this deadline is never the later of the two.
func testBinaryDeadline() (time.Time, bool) {
	if !flag.Parsed() {
		flag.Parse()
	}
	timeout := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration)
	if timeout <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(timeout), true
}

// boundByTheDeadline gives every bound legion registers with programbound, each beside its
// declaration, what is left of deadline less 30 s when its wait starts, in this process and in
// each run of this binary as `legion` whose environment is this process's own (testDeadlineEnv).
// Each product bound fits the real program or service, and none is what a test here reads: each
// is all the time a stand-in has to start and answer, and a loaded machine makes starts slow.
// At a box load of about 1,100 a git behind a one-line script ran out
// ghGitReadBound's 30 s in TestGhRepoGitConfigRefusalWithNoGitOrAnOldGit, and at a load average of
// 744 a run of this binary as `legion gh` reached the stand-in gh after visibilityAnswerBound's
// 15 s in TestLegionGhReadsOnlyWhatTheSharedCheckReadsAsARead. A program that never answers still
// fails its row by name before the binary's deadline, however late in the run its wait starts. A
// test that reads a bound's own stop sets that bound itself, and a bound a test has set keeps that
// test's value.
func boundByTheDeadline(deadline time.Time) error {
	programbound.SetDeadline(deadline)
	return os.Setenv(testDeadlineEnv, deadline.Format(time.RFC3339Nano))
}

// boundByTheEnvironment is boundByTheDeadline for a run of this binary as `legion`, by the deadline
// testDeadlineEnv names; a run that names none keeps the product's bounds.
func boundByTheEnvironment() error {
	named, ok := os.LookupEnv(testDeadlineEnv)
	if !ok {
		return nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, named)
	if err != nil {
		return fmt.Errorf("%s=%q is no RFC 3339 time: %w", testDeadlineEnv, named, err)
	}
	return boundByTheDeadline(deadline)
}

// testDeadlineEnviron is the variable that hands a run of this binary as `legion` the deadline of
// the binary that started it, for an environment a test builds itself rather than from os.Environ,
// which already holds it; none under -timeout 0.
func testDeadlineEnviron() []string {
	if deadline, ok := os.LookupEnv(testDeadlineEnv); ok {
		return []string{testDeadlineEnv + "=" + deadline}
	}
	return nil
}
