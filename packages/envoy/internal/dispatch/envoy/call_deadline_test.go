package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A test's call deadline (WithCallDeadline) ends a held send's window at its own moment rather
// than CallWindow after the send began: the window the send arms on the client's clock is the
// deadline, a move past CallWindow leaves it armed and the send waiting, and the move to the
// deadline ends the send as a receipt timeout whose error names that end. A client given
// WithTimeout as well arms the deadline, whichever option comes last. A deadline with no end arms
// no window at all, so only the send's own context ends it.
func TestACallDeadlineEndsAHeldSendAtItsOwnMoment(t *testing.T) {
	held, reached := make(chan struct{}), make(chan struct{}, 3)
	listener := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
		<-held
	}))
	defer func() {
		close(held)
		listener.Close()
	}()
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	send := func(ctx context.Context, client *Client) <-chan error {
		answered := make(chan error, 1)
		go func() {
			_, err := client.Send(ctx, SendInput{TargetSession: "s1", Message: "held", IdempotencyKey: "held:steer"})
			answered <- err
		}()
		return answered
	}

	t.Run("a moment", func(t *testing.T) {
		fake := clocktest.New(start)
		deadline := start.Add(2 * CallWindow)
		answered := send(context.Background(), New(listener.URL, WithClock(fake), WithCallDeadline(clock.DeadlineAt(deadline, true))))
		testwait.Receive(t, "the held send to reach the listener", reached)
		if armed := fake.AwaitArmed(t, "the send's window", 1); !armed[0].Equal(deadline) {
			t.Fatalf("the send's window ends at %v, want the deadline %v", armed[0], deadline)
		}
		fake.Advance(CallWindow)
		if armed := fake.Armed(); len(armed) != 1 || !armed[0].Equal(deadline) {
			t.Fatalf("a CallWindow after the send began, its clock holds %v armed, want its window at the deadline %v alone", armed, deadline)
		}
		fake.Set(deadline)
		err := testwait.Receive(t, "the held send to end at the deadline", answered)
		if !errors.Is(err, ErrReceiptTimeout) {
			t.Fatalf("a send held to its deadline = %v, want ErrReceiptTimeout", err)
		}
		if end := "the end " + deadline.Format(time.RFC3339Nano); !errors.Is(err, ErrCallWindow) || !strings.Contains(err.Error(), end) {
			t.Fatalf("a send held to its deadline = %v, want ErrCallWindow naming %q", err, end)
		}
	})

	t.Run("a moment beside WithTimeout", func(t *testing.T) {
		fake := clocktest.New(start)
		deadline := start.Add(2 * CallWindow)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		answered := send(ctx, New(listener.URL, WithClock(fake), WithCallDeadline(clock.DeadlineAt(deadline, true)), WithTimeout(CallWindow/2)))
		testwait.Receive(t, "the held send to reach the listener", reached)
		// The window is armed before the request leaves (roundTrip), so a send at the listener
		// has armed whatever window it has.
		if armed := fake.Armed(); len(armed) != 1 || !armed[0].Equal(deadline) {
			t.Fatalf("a send given WithTimeout after WithCallDeadline armed %v on its clock, want its window at the deadline %v alone", armed, deadline)
		}
		cancel()
		if err := testwait.Receive(t, "the held send to end with its context", answered); !errors.Is(err, context.Canceled) {
			t.Fatalf("a send given both options, its context cancelled = %v, want the context's error", err)
		}
	})

	t.Run("no end", func(t *testing.T) {
		fake := clocktest.New(start)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		answered := send(ctx, New(listener.URL, WithClock(fake), WithCallDeadline(clock.DeadlineAt(time.Time{}, false))))
		testwait.Receive(t, "the held send to reach the listener", reached)
		// The window is armed before the request leaves (roundTrip), so a send at the listener
		// has armed whatever window it has.
		if armed := fake.Armed(); len(armed) != 0 {
			t.Fatalf("a send with no end armed %v on its clock, want no window", armed)
		}
		cancel()
		if err := testwait.Receive(t, "the held send to end with its context", answered); !errors.Is(err, context.Canceled) {
			t.Fatalf("a send with no end, its context cancelled = %v, want the context's error", err)
		}
	})
}
