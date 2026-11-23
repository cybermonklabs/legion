package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// forwardEndingInProcessFirst is the listener's own forward (startRoleForward) to client's stream,
// except that the first forward, which the stream stores, then reports what a forward's wait
// reports when the stream had answered a send of it as still in process under an earlier forward
// of the same Nats-Msg-Id and its resend then left nothing on the wire
// (publishPastInProcess: errEarlierAttemptInProcess wrapping bus.ErrNotSent). after runs between
// the store and that report.
func forwardEndingInProcessFirst(client *bus.Client, forwards *atomic.Int32, after func()) func(context.Context, string, contracts.Envelope, string, time.Duration) (func() (bus.StoredAck, error), error) {
	real := startRoleForward(client)
	return func(ctx context.Context, subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() (bus.StoredAck, error), error) {
		wait, err := real(ctx, subject, item, msgID, timeout)
		if err != nil {
			return nil, err
		}
		if forwards.Add(1) > 1 {
			return wait, nil
		}
		return func() (bus.StoredAck, error) {
			if _, err := wait(); err != nil {
				return bus.StoredAck{}, err
			}
			after()
			return bus.StoredAck{}, fmt.Errorf("%w: %w", errEarlierAttemptInProcess,
				fmt.Errorf("%w: the resend's window was spent before its publish", bus.ErrNotSent))
		}, nil
	}
}

// A forward whose wait ended after the stream said an earlier forward under its Nats-Msg-Id was
// still in process may be stored by that earlier forward, even when its own resend then left
// nothing on the wire. So the message is pinned to that holder, and the role's move to a second
// session before the next attempt changes nothing: the next attempt is the same forward to the
// first holder, which the stream drops as the duplicate it is. The stream holds one copy, the
// first holder's. A lane that read the resend's bus.ErrNotSent alone resolves the holder again,
// and the second holder gets a second copy.
func TestAForwardThatMetAnEarlierAttemptInProcessStaysWithItsHolder(t *testing.T) {
	const second = "ses_in_process_second"
	r := newPinRig(t, "pin-in-process-moved", "ses_in_process_first")
	var forwards atomic.Int32
	r.deliver(t, "pin-in-process-moved", forwardEndingInProcessFirst(r.harness.client, &forwards, func() {
		moveRole(t, r.harness, r.role, second)
	}))

	if got := forwards.Load(); got != 2 {
		t.Fatalf("the lane made %d forwards, want 2: the one that met the earlier attempt, and the same forward once more:\n%s", got, r.harness.logs.String())
	}
	if atFirst, atSecond := r.storedFor(t, r.first), r.storedFor(t, second); atFirst != 1 || atSecond != 0 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 1 and 0:\n%s", atFirst, atSecond, r.harness.logs.String())
	}
}
