package testwait

import (
	"flag"
	"testing"
	"time"
)

// BinaryDeadline is go test's -timeout reckoned from the call: read just before the call, now
// plus the timeout is no later than its answer, and read just after it no earlier. Under
// -timeout 0 it answers no deadline. The reads and the answer come from one monotonic clock, so
// the bounds need no tolerance.
func TestBinaryDeadlineIsTheTimeoutReckonedFromTheCall(t *testing.T) {
	timeout := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration)
	before := time.Now()
	got, ok := BinaryDeadline()
	after := time.Now()
	if timeout <= 0 {
		if ok {
			t.Fatalf("under -timeout %s BinaryDeadline = %s, want none", timeout, got.Format(time.RFC3339Nano))
		}
		return
	}
	if !ok {
		t.Fatalf("under -timeout %s BinaryDeadline answered none", timeout)
	}
	if least, most := before.Add(timeout), after.Add(timeout); got.Before(least) || got.After(most) {
		t.Fatalf("BinaryDeadline = %s, want between %s and %s: -timeout %s from the call",
			got.Format(time.RFC3339Nano), least.Format(time.RFC3339Nano), most.Format(time.RFC3339Nano), timeout)
	}
}
