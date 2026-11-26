package clock_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
)

// A deadline's written form reads back as itself: a moment in RFC 3339 with its nanoseconds, and
// none for no end. Anything else is refused, the zero value's empty form among it, and a bounded
// deadline at the zero time, which would read as production's duration, cannot be made.
func TestADeadlineReadsBackAsWritten(t *testing.T) {
	for _, deadline := range []clock.Deadline{
		clock.DeadlineAt(time.Date(2026, time.November, 24, 12, 0, 0, 123456789, time.UTC), true),
		clock.DeadlineAt(time.Time{}, false),
	} {
		read, err := clock.ParseDeadline(deadline.String())
		if err != nil || read.String() != deadline.String() {
			t.Errorf("ParseDeadline(%q) = %q, %v; want %q", deadline, read, err, deadline)
		}
	}
	if written := clock.DeadlineAt(time.Time{}, false).String(); written != "none" {
		t.Errorf("a deadline with no end is written %q, want none", written)
	}
	for _, invalid := range []string{"", "5s", "2026-11-24", "never"} {
		if read, err := clock.ParseDeadline(invalid); err == nil {
			t.Errorf("ParseDeadline(%q) = %q, want a refusal", invalid, read)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("DeadlineAt(the zero time, bounded) made a deadline, want a panic")
		}
	}()
	clock.DeadlineAt(time.Time{}, true)
}
