package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// TestAnInProcessResendThatWaitsOutAReconnectNamesIt drives publishPastInProcess with a send the
// stream refuses as in process and a resend that waits for the connection until the caller's
// context ends, as bus.PublishStoredAck answers then (bus.ErrReconnecting with ctx's error). The
// call's error is still the earlier attempt's, carries the refusal, and names the reconnect, so the
// route's 503 maybe_stored says the bus was reconnecting rather than only that the budget ended.
func TestAnInProcessResendThatWaitsOutAReconnectNamesIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refusal := fmt.Errorf("publish notifications.team.x: %w", bus.ErrDuplicateInProcess)
	sends := 0
	_, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) {
		sends++
		if sends == 1 {
			return bus.StoredAck{}, refusal
		}
		cancel()
		return bus.StoredAck{}, fmt.Errorf("bus: %w: %w", bus.ErrReconnecting, ctx.Err())
	})
	if sends != 2 {
		t.Fatalf("the call sent %d times, want 2: the refused send and the resend", sends)
	}
	if !earlierAttemptInProcess(err) {
		t.Fatalf("err = %v, want it to wrap errEarlierAttemptInProcess", err)
	}
	if !errors.Is(err, bus.ErrDuplicateInProcess) {
		t.Errorf("err = %v, want it to carry the stream's in-process refusal", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry the context's end", err)
	}
	if !errors.Is(err, bus.ErrReconnecting) {
		t.Errorf("err = %v, want it to name the reconnect the resend waited out (bus.ErrReconnecting)", err)
	}
}
