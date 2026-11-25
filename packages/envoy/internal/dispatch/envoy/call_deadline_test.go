package envoy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A test's call deadline (WithCallDeadline) ends a held send's window at its own moment rather
// than CallWindow after the send began: the window the send arms on the client's clock is the
// deadline, a move past CallWindow leaves it armed and the send waiting, and the move to the
// deadline ends the send as a receipt timeout. A deadline with no end arms no window at all, so
// only the send's own context ends it.
func TestACallDeadlineEndsAHeldSendAtItsOwnMoment(t *testing.T) {
	held, reached := make(chan struct{}), make(chan struct{}, 2)
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
		if err := testwait.Receive(t, "the held send to end at the deadline", answered); !errors.Is(err, ErrReceiptTimeout) {
			t.Fatalf("a send held to its deadline = %v, want ErrReceiptTimeout", err)
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
