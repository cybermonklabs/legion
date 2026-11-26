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
// 503 with Retry-After under earlierAttemptInProcessPrefix and listenerwire.CodeMaybeStored, and
// the request's record says outcomeEarlierAttemptInProcess. Where no send of the call went out
// unanswered, it costs the sender nothing (senderCharge.published), since the copy is the earlier
// publish's. Where one did, before or after a refusal, the error wraps bus.ErrUnacknowledged
// beside it and costs one: that send may have stored the copy itself.
var errEarlierAttemptInProcess = errors.New("the request ended while an earlier publish under its message id was still in process")

// publishPastInProcess makes publish, and makes it again inProcessPause later each time a
// clustered stream refuses it as still in process under another publish under the same message
// id (bus.ErrDuplicateInProcess), since the next answer names the copy that publish stored. It is
// this package's one handling of that refusal, for every publish of its own whose answer a request
// is answered or charged by - the send route's publish (sendHandler) and the publish route's of a
// team or captured topic (publishHandler), both through publishHeld; a role forward
// (startRoleForward), which the publish route's role sends (sendToRole), the role lane's
// deliveries (roleForwarder) and the kept lane's walks (walkKeptRole) forward through; and a
// hand-off to a stopped Legion agent's daemon (handOffToLegion), which a send, a role publish, a
// send marked with a role and an agent subject's give-up (offerAgentDeliveryToLegion) hand off
// through - and for a keep (storeKeptRoleCopy) and a takeover's copy (keepTakenRoleFrame). So a
// resend that meets its own earlier attempt still being stored, as a hub failover leaves one, is
// answered with that attempt's copy rather than as a bus that did not answer.
//
// The listener's other publishes under a message id do not take it, and the earlier publish the
// refusal names stores the one copy all the same. The webhook routes (webhookRoutes:
// internal/webhook's GitHub, Slack and Ghost Wispr handlers) answer their request by a publish
// under the delivery's id, so a refusal there is answered 503 and logged `<source> publish in
// process`, which no alarm counts (internal/webhook's failureOutcome); no sender limit charges a
// webhook, and a redelivery, where its sender makes one, is answered by that copy as a duplicate.
// The other three carry no request's answer or charge: the sweep's wake report
// (publishSweptRoleWake), which logs the report as not published and leaves the next sweep to
// raise it; a kept copy's dead letter (endKeptCopyWithDeadLetter), whose walk ends as a failed one
// with its claim left, so the walk that takes the claim over sends it again and is answered with
// that copy; and a delivery exception (publishDeliveryException), which logs
// exceptionPublishInProcessLine at WARN rather than exceptionPublishFailedLine.
//
// A call that ends after a refusal, because ctx ended or a later send failed, wraps
// errEarlierAttemptInProcess: with ctx's error and the last refusal when ctx ended, and with the
// failure itself otherwise.
//
// A send of the call that went out unanswered (bus.ErrUnacknowledged) may have stored the copy
// itself, whether it came before a refusal or after one: a stream leader change drops the ids the
// stream holds in process, and the next send under one is stored as a new copy. So the answer
// after such a send says so as the bus's own resend does: a duplicate comes back Resent, which may
// name this call's own copy (bus.StoredAck.Resent), and a failure wraps bus.ErrUnacknowledged
// (mayHaveStored), also when ctx's end replaces the last send's own error.
func publishPastInProcess(ctx context.Context, publish func() (bus.StoredAck, error)) (bus.StoredAck, error) {
	// refusal is the stream's last refusal of a send of this call as in process, and ownSend
	// whether a send of this call went out unanswered (its error wraps bus.ErrUnacknowledged).
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
		}
		ownSend = ownSend || mayHaveStored(err)
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w (after %w)", errEarlierAttemptInProcess, ctx.Err(), refusal)
		} else {
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
