package testwait

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// margin is how long before the test binary's deadline Receive gives up: the time the failing
// test's cleanups and the failure itself have to run in before Go's alarm panics the binary.
const margin = 30 * time.Second

// Receive waits for one receive from ch (a value, or the close of ch, which gives the zero value)
// and sets no span of its own: its one bound is the test binary's deadline less margin, the one
// go test's -timeout sets, and under -timeout 0 it has none. A row whose verdict is what a call
// answers waits for that answer here, so a call that waits for ever fails by name and a call that
// a loaded machine makes slow passes. A wait that begins with margin or less of the deadline left,
// as every wait does under a -timeout of 30 s or less, takes only a value already sent or a close.
//
// A wait that gives up writes "testwait: <test>: gave up waiting for <what> after <waited>, 30s
// before the test binary's deadline" to stderr at once, then fails the test with the same text:
// without -v, Go's alarm panics without printing a running test's log, so a cleanup that outlasted
// the margin would otherwise take the message with it. The time waited tells a wait that hung from
// one that began after an earlier one had spent the deadline. The text is one line: a newline in
// what is written as \n. This is envoy's testwait.Receive, with the time waited in its text.
func Receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	start := time.Now()
	expired, stop := expiry(t)
	defer stop()
	select {
	case value := <-ch:
		return value
	case <-expired:
	}
	// The margin has passed: ask once more, so a value that came beside the timer is taken.
	select {
	case value := <-ch:
		return value
	default:
	}
	gaveUp(t, what, time.Since(start))
	var zero T
	return zero
}

// expiry is a channel that receives once the test binary's deadline less margin has passed, nil
// (which never receives) under -timeout 0, and the function that releases its timer.
func expiry(t *testing.T) (<-chan time.Time, func() bool) {
	deadline, bounded := t.Deadline()
	if !bounded {
		return nil, func() bool { return false }
	}
	timer := time.NewTimer(time.Until(deadline.Add(-margin)))
	return timer.C, timer.Stop
}

// oneLine writes a newline or a carriage return as its escape, so a failure text is one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// gaveUp ends a wait in t that the margin ended: it writes the failure's text to stderr at once,
// in one write, then fails t with the same text.
func gaveUp(t *testing.T, what string, waited time.Duration) {
	t.Helper()
	text := oneLine.Replace(fmt.Sprintf("testwait: %s: gave up waiting for %s after %s, %s before the test binary's deadline",
		t.Name(), what, waited.Round(time.Millisecond), margin))
	_, _ = os.Stderr.WriteString(text + "\n")
	t.Fatalf("%s", text)
}
