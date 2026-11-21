package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// inProcessPause is how long a publish waits before it is made again under a message id a
// clustered stream refused as still in process under another publish (bus.ErrDuplicateInProcess):
// that publish is being stored, so the next answer is a duplicate naming its copy. It is short
// because a store takes milliseconds, and the retries end with the caller's context.
const inProcessPause = 50 * time.Millisecond

// publishPastInProcess makes publish, and makes it again inProcessPause later each time a
// clustered stream refuses it as still in process under another publish under the same message
// id (bus.ErrDuplicateInProcess), since the next answer names the copy that publish stored. It is
// the one handling of that refusal for every listener path that publishes under a message id: the
// send route and the publish route's team and captured topics (sendHandler, publishHandler), a
// keep (storeKeptRoleCopy) and a takeover's copy (keepTakenRoleFrame). So a resend that meets its
// own earlier attempt still being stored, as a hub failover leaves one, is answered with that
// attempt's copy rather than as a bus that did not answer.
//
// A refused send that went out unanswered first (bus.ErrUnacknowledged) may have stored the copy
// itself, and the answer after it says so as the bus's own resend does: a duplicate comes back
// Resent, which may name this call's own copy (bus.StoredAck.Resent), and a failure wraps
// bus.ErrUnacknowledged (mayHaveStored). A wait that ctx ends is the error, after the last refusal.
func publishPastInProcess(ctx context.Context, publish func() (bus.StoredAck, error)) (bus.StoredAck, error) {
	ownSend := false
	answer := func(ack bus.StoredAck, err error) (bus.StoredAck, error) {
		if !ownSend {
			return ack, err
		}
		if err != nil && !mayHaveStored(err) {
			return ack, fmt.Errorf("%w: %w", bus.ErrUnacknowledged, err)
		}
		ack.Resent = true
		return ack, err
	}
	for {
		ack, err := publish()
		if !errors.Is(err, bus.ErrDuplicateInProcess) {
			return answer(ack, err)
		}
		ownSend = ownSend || mayHaveStored(err)
		if waitErr := waitUnlessDone(ctx, inProcessPause); waitErr != nil {
			return answer(bus.StoredAck{}, fmt.Errorf("%w (after %w)", waitErr, err))
		}
	}
}
