package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// WithShownDeadline hands on a context that reports its deadline as it is. A context that reports
// none, as a fake's deadline context does, is shown a deadline, and still ends where its own end
// comes, here the fake reaching its deadline.
func TestWithShownDeadlineShowsADeadlineOnlyWhereTheContextReportsNone(t *testing.T) {
	reported, cancelReported := context.WithTimeout(context.Background(), time.Hour)
	defer cancelReported()
	if shown, release := clock.WithShownDeadline(reported); shown != reported {
		release()
		t.Fatal("a context that reports its deadline was shown another; want it handed on as it is")
	}

	fake := clocktest.New(time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC))
	hidden, cancelHidden := fake.WithTimeout(context.Background(), time.Minute)
	defer cancelHidden()
	if _, has := hidden.Deadline(); has {
		t.Fatal("control: the fake's deadline context reports a deadline, so nothing here is shown one")
	}
	shown, release := clock.WithShownDeadline(hidden)
	defer release()
	if _, has := shown.Deadline(); !has {
		t.Fatal("a context that reports no deadline was shown none; want one shown")
	}
	if err := shown.Err(); err != nil {
		t.Fatalf("the shown context ended before the fake reached its deadline: %v", err)
	}
	fake.Advance(time.Minute)
	testwait.Receive(t, "the shown context to end once the fake reached its deadline", shown.Done())
	if err := shown.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the shown context ended with %v, want the fake's context.DeadlineExceeded", err)
	}
}

// WithShownTimeout's one cancel releases the clock's own deadline as well as the shown one: on a
// fake, the context is shown a deadline and the fake holds the timeout's, and once the cancel has
// run the context has ended and the fake holds nothing armed.
func TestWithShownTimeoutsCancelReleasesTheClocksDeadline(t *testing.T) {
	epoch := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(epoch)
	shown, cancel := clock.WithShownTimeout(fake, context.Background(), time.Minute)
	if _, has := shown.Deadline(); !has {
		cancel()
		t.Fatal("the fake's timeout was shown no deadline; want one shown")
	}
	if armed, want := fake.Armed(), epoch.Add(time.Minute); len(armed) != 1 || !armed[0].Equal(want) {
		cancel()
		t.Fatalf("the fake holds %v armed, want only the timeout's deadline %v", armed, want)
	}
	cancel()
	if err := shown.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the shown context ended with %v once its cancel ran, want context.Canceled", err)
	}
	if armed := fake.Armed(); len(armed) != 0 {
		t.Fatalf("the fake still holds %v armed once the cancel ran, want nothing: the cancel left the clock's own deadline", armed)
	}
}
