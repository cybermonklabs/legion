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

// errEarlierAttemptInProcess is wrapped by the error of a publish whose call ended after the
// stream had refused a send of it as still in process under an earlier publish under the same
// message id (publishPastInProcess): its context ran out, or a later send failed. That earlier
// publish may still be stored, so the stream may hold the message: writeBusUnavailable answers it
// 503 with Retry-After under earlierAttemptInProcessPrefix and listenerwire.CodeMaybeStored, the
// request's record says outcomeEarlierAttemptInProcess, and it costs the sender nothing
// (senderCharge.published), since the copy is the earlier publish's. A send of the same call that
// went out unanswered before the refusal may have stored the copy itself, and then the error
// wraps bus.ErrUnacknowledged beside it, which costs one.
var errEarlierAttemptInProcess = errors.New("the request ended while an earlier publish under its message id was still in process")

// publishPastInProcess makes publish, and makes it again inProcessPause later each time a
// clustered stream refuses it as still in process under another publish under the same message
// id (bus.ErrDuplicateInProcess), since the next answer names the copy that publish stored. It is
// the one handling of that refusal for every listener path that publishes under a message id: the
// send route and the publish route's team and captured topics (sendHandler, publishHandler), a
// role forward (startRoleForward, which every role send, role-lane delivery and kept-lane walk
// forwards through), a keep (storeKeptRoleCopy) and a takeover's copy (keepTakenRoleFrame). So a
// resend that meets its own earlier attempt still being stored, as a hub failover leaves one, is
// answered with that attempt's copy rather than as a bus that did not answer.
//
// A call that ends after a refusal, because ctx ended or a later send failed, wraps
// errEarlierAttemptInProcess: with ctx's error and the last refusal when ctx ended, whatever the
// send it ended under answered, since a later send under an id the stream holds in process stores
// no copy of its own, and with the failure itself otherwise.
//
// A refused send that went out unanswered first (bus.ErrUnacknowledged) may have stored the copy
// itself, and the answer after it says so as the bus's own resend does: a duplicate comes back
// Resent, which may name this call's own copy (bus.StoredAck.Resent), and a failure wraps
// bus.ErrUnacknowledged (mayHaveStored).
func publishPastInProcess(ctx context.Context, publish func() (bus.StoredAck, error)) (bus.StoredAck, error) {
	// refusal is the stream's last refusal of a send of this call as in process, and ownSend
	// whether a refused send went out unanswered first (the refusal wraps bus.ErrUnacknowledged).
	var refusal error
	ownSend := false
	for {
		ack, err := publish()
		if errors.Is(err, bus.ErrDuplicateInProcess) {
			refusal = err
			ownSend = ownSend || mayHaveStored(err)
			if err = waitUnlessDone(ctx, inProcessPause); err == nil {
				continue
			}
		}
		switch {
		case refusal == nil:
			return ack, err
		case err == nil:
			ack.Resent = ack.Resent || ownSend
			return ack, nil
		case ctx.Err() != nil:
			err = fmt.Errorf("%w: %w (after %w)", errEarlierAttemptInProcess, ctx.Err(), refusal)
		default:
			err = fmt.Errorf("%w: %w", errEarlierAttemptInProcess, err)
		}
		if ownSend && !mayHaveStored(err) {
			err = fmt.Errorf("%w: %w", bus.ErrUnacknowledged, err)
		}
		return bus.StoredAck{}, err
	}
}

// earlierAttemptInProcess reports whether err ended a publish while an earlier publish under its
// message id was still in process (errEarlierAttemptInProcess), so the stream may yet store it.
func earlierAttemptInProcess(err error) bool {
	return errors.Is(err, errEarlierAttemptInProcess)
}
