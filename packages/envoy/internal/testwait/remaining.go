package testwait

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"
)

// unbounded is Remaining's answer under -timeout 0, which sets no deadline: 1<<60 ns, about 36
// years, a span no wait reaches that time.Now().Add and a timer still take.
const unbounded = time.Duration(1 << 60)

// Remaining is how long a wait that begins now in t may last when it must leave its test spare
// before the test binary's deadline, the one go test's -timeout sets, to fail in and run its
// cleanups: that deadline less spare, less now; under -timeout 0, a span no wait reaches. A wait
// bounded by it ends on its fact or gives up with spare left, never at a fixed span. With spare or
// less left there is no span to wait, and Remaining fails t at once (Fatal) on the one line Left
// gives, written to stderr first, as a wait that gives up writes its line (gaveUp).
func Remaining(t *testing.T, spare time.Duration) time.Duration {
	t.Helper()
	left, err := Left(t, spare)
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		t.Fatal(err.Error())
	}
	return left
}

// Left is Remaining for a caller that must not end its goroutine, such as a helper a hook calls on
// a goroutine other than the test's, where t.Fatal would end the hook rather than the test: with
// spare or less of the deadline left it fails nothing and returns, as an error, the line Remaining
// fails with, "testwait: <test>: " and the cause (spent).
func Left(t *testing.T, spare time.Duration) (time.Duration, error) {
	deadline, bounded := t.Deadline()
	if !bounded {
		return unbounded, nil
	}
	left := time.Until(deadline)
	if left <= spare {
		timeout, known := binaryTimeout()
		return 0, errors.New(oneLine.Replace("testwait: " + t.Name() + ": " + spent(left, spare, timeout, known)))
	}
	return left - spare, nil
}

// spent says why a wait that must leave spare of the test binary's deadline found only left. A
// -timeout of spare or less leaves no test room to wait: the run asks too little, and the text says
// to raise it. A longer one was used up before the wait began, by the tests the binary ran first or
// by this test's own earlier waits: the run is not set up wrong, and the text says how long the
// binary had been running its tests, the -timeout it began with less left (testing.M sets the
// deadline that long after the tests begin, M.startAlarm). known is false when that -timeout could
// not be read.
func spent(left, spare, timeout time.Duration, known bool) string {
	switch {
	case !known:
		return fmt.Sprintf("the test binary's deadline leaves %s, not more than the %s this wait must leave its test: what ran before this wait used up that deadline (the -timeout the binary began with could not be read)",
			left.Round(time.Millisecond), spare)
	case timeout <= spare:
		return fmt.Sprintf("the test binary's -timeout is %s, not more than the %s this wait must leave its test before the deadline, so no test has room to wait: raise -timeout",
			timeout, spare)
	default:
		return fmt.Sprintf("the test binary's deadline leaves %s, not more than the %s this wait must leave its test: the binary began its tests %s ago under -timeout %s, and what ran before this wait (an earlier test, or this test's own earlier waits) used up that deadline",
			left.Round(time.Millisecond), spare, (timeout - left).Round(time.Millisecond), timeout)
	}
}

// binaryTimeout is the -timeout the test binary began its tests under, testing's -test.timeout
// flag, and false when that flag cannot be read.
func binaryTimeout() (time.Duration, bool) {
	registered := flag.Lookup("test.timeout")
	if registered == nil {
		return 0, false
	}
	getter, ok := registered.Value.(flag.Getter)
	if !ok {
		return 0, false
	}
	timeout, ok := getter.Get().(time.Duration)
	return timeout, ok
}
