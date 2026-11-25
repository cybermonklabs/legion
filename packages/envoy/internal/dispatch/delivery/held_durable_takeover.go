package delivery

// held_durable_takeover.go is the held durable's take-over (heldDurable.takeOver): how an intake
// that holds its durable's hold makes the durable deliver to it, the bounds on the steps that
// finish it, and the JetStream requests it sends on the client's connection.

import (
	"context"
	"errors"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// consumerInfo is the take-over's request for the durable's state, on the context call: the
// request onConnection makes for lookupDurable and readDurable.
func (d *heldDurable) consumerInfo(call context.Context) (*natsgo.ConsumerInfo, error) {
	return d.nats.JS().ConsumerInfo(bus.Stream, d.name, natsgo.Context(call))
}

// onConnection makes request, a JetStream request of the take-over on the client's connection,
// ending it with bus.ErrRequestLostWithConnection as soon as that connection is lost before the
// answer comes (bus.RequestOnConnection). nats.go fails no request its connection drops, so one
// whose reply died with the old socket would otherwise wait out its whole deadline, and the
// take-over with it, for an answer nothing will send. The take-over's read of the durable
// (readDurable) and each step of finishTakeOver (retryHeld) send such a request again at once.
// lookupDurable sends it once, and its callers treat the error as any failed lookup: attempt goes
// on to the take-over under the consumer lease, whose own read (readDurable) asks again, and check
// changes nothing and looks again intakeHoldRenew later, at run's next check.
func onConnection[T any](ctx context.Context, client *bus.Client, request func(context.Context) (T, error)) (T, error) {
	return bus.RequestOnConnection(ctx, client, request)
}

// readDurable is the take-over's read of the durable's state, under its consumer lease. A read lost
// with its connection (onConnection) is sent again at once on the connection that came back, while
// this intake holds its hold: the loss says nothing about the durable.
func (d *heldDurable) readDurable(ctx context.Context) (*natsgo.ConsumerInfo, error) {
	for {
		info, err := onConnection(ctx, d.nats, d.consumerInfo)
		if !errors.Is(err, bus.ErrRequestLostWithConnection) || !d.holds() {
			return info, err
		}
		d.tuning.log.Info("dispatch delivery: the NATS consumer read was lost with its connection; reading it again", "name", d.name)
	}
}

// takeOver makes the durable deliver to this intake, under the hold and the durable's consumer
// lease. A durable that does not exist is created with this build's config, at the start a
// take-over before this one recorded when it deleted the durable and did not create it again
// (intakeHoldToken). A durable bound to a subject no hold recorded is left as it is
// (boundElsewhere). Any other -- unbound, or bound to the subject the hold records, which is a
// holder that died or was cut off -- is deleted and created again at its ack floor on a fresh
// deliver subject (takeOverConfig), and that start is recorded as the hold's token before the
// delete. The durable's state is read again here, under the lease, since a process of the build
// before holds can bind it between attempt's read and this one.
//
// This intake subscribes to the fresh deliver subject before it creates the durable, so the
// durable is push-bound from the moment it exists. A process of the build before holds binds by
// looking the durable up and binding it when the lookup reports it unbound; a durable created
// before its subscriber would leave a moment in which that lookup finds it unbound, and both
// processes would then be handed every message on one subject. The subscribe sends its SUB on
// the client's connection before the create's request goes out on the same connection, and the
// server handles one connection's protocol in order, so the subject has interest before the
// create is even read (TestATakeOverCreatesTheDurableAlreadyBound). Then this intake records the
// subject alone as the hold's token, still under the lease.
//
// From the delete on -- or from the lookup, when it finds no durable -- the take-over runs to its
// end inside this one lease (finishTakeOver, which says what a lease given up with the durable
// absent leaves behind).
func (d *heldDurable) takeOver(ctx context.Context) {
	wanted := d.config(d.tuning)
	name := wanted.Durable
	create := wanted
	deleteFirst := false
	info, err := d.readDurable(ctx)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if d.recorded.start != 0 {
			create = startingAt(wanted, d.recorded.start)
		}
	case err != nil:
		d.tuning.log.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return
	case d.boundElsewhere(info):
		return
	default:
		create = takeOverConfig(info, wanted)
		if !d.holds() {
			return
		}
		pending := intakeHoldToken{subject: d.recorded.subject, start: create.OptStartSeq}
		if err := d.hold.SetToken(ctx, pending.String()); err != nil {
			d.tuning.log.Error("dispatch delivery: record the take-over's start before deleting NATS consumer", "name", name, "start_seq", create.OptStartSeq, "error", err)
			return
		}
		d.recorded = pending
		deleteFirst = true
	}
	d.finishTakeOver(create, deleteFirst)
}

// takeOverConfig is the config a take-over recreates the durable deployed describes with: the
// deployed config when a newer build stamped it with a higher generation, this build's (wanted)
// otherwise, on a fresh deliver subject, starting at the stream sequence after the durable's ack
// floor. Starting there, the recreated durable redelivers at once every message the durable had
// not acknowledged -- what a dead holder had been handed, what a stopped one handed back --
// replays nothing it had, and skips nothing (TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse,
// and TestATakeOverWhoseCreateFailsCreatesTheDurableAtItsFloorOnTheNextAttempt for a create retried).
// The take-over records that start in its hold's token before it deletes the durable, and
// finishTakeOver says what a create that fails after the delete leaves behind.
// The deployed metadata keeps its own keys and drops the ones nats-server sets itself (the
// `_nats.` prefix), which the server sets again on create.
func takeOverConfig(deployed *natsgo.ConsumerInfo, wanted natsgo.ConsumerConfig) natsgo.ConsumerConfig {
	config := wanted
	if bus.MetadataGeneration(deployed.Config.Metadata, consumerGenerationKey) > consumerConfigGeneration {
		config = deployed.Config
		config.DeliverSubject = natsgo.NewInbox()
		config.OptStartTime = nil
		config.Metadata = bus.OwnMetadata(deployed.Config.Metadata, 0)
	}
	return startingAt(config, deployed.AckFloor.Stream+1)
}

// startingAt is config delivering from stream sequence start on.
func startingAt(config natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	return config
}

// The bounds on finishTakeOver, the part of a take-over that runs after it has recorded the
// durable's start, and that must not give up the consumer lease with the durable deleted.
//
// takeOverFinishWithin bounds the whole of it, from its first step to its last try. It is under
// intakeHoldTTL - intakeHoldMargin (13 s), how long a hold lasts from its taking or its last
// renewal. A hold takeHold just took has at most 13 s left when attempt reaches these steps, less
// the lookup of the durable (lookupDurable, up to 5 s), the consumer lease (duty.Exclusive) and
// takeOver's read of the durable before them: with every renewal after the taking failing, the
// hold covers most of the 10 s of the steps, not always all of them. A take-over that runs again
// after a check found the durable delivering elsewhere (check) begins at any point in the renewal
// cycle of a hold this intake already held, so up to intakeHoldRenew (5 s) after its last renewal:
// with the renewals from there on failing, about 8 s of the hold are left against the 10 s bound.
// In both cases retryHeld stops at the hold's deadline. That exit is the safe one -- no step acts
// once another process can take the hold, and the next attempt takes the durable over again.
// takeOverFinishWithin is also inside intakeStopBudget, and so inside stopGrace (cmd/dispatch),
// since a stop that comes meanwhile waits for it to end.
//
// takeOverCallTimeout bounds each try of each step, so that one call that draws no answer -- a
// JetStream request during a leader change -- leaves room for the tries after it. A JetStream
// leader election ordinarily ends within it.
//
// A failed try waits takeOverFirstRetry before the next one, doubling after each try up to
// takeOverLastRetry: with calls that fail at once, that is eight tries within takeOverFinishWithin.
// A try the connection's grant refuses (bus.ErrPublishDenied) is not tried again: the server
// refuses it again however often it is sent, so the step gives up at once and the consumer lease
// is released, rather than held for the rest of takeOverFinishWithin.
const (
	takeOverFinishWithin = 10 * time.Second
	takeOverCallTimeout  = 3 * time.Second
	takeOverFirstRetry   = 250 * time.Millisecond
	takeOverLastRetry    = 2 * time.Second
)

// finishTakeOver makes the durable deliver to this intake at create's start, under the consumer
// lease its caller (takeOver) holds: it deletes the durable when deleteFirst is set, subscribes
// to create's deliver subject, creates the durable with create, and records the subject alone as
// the hold's token. Each step that fails is tried again (retryHeld) until it succeeds, this intake
// no longer holds its hold, the grant refuses it, or takeOverFinishWithin has passed. It runs on a
// context of its own rather than the run's, so a stop that comes while the durable is deleted does
// not leave it so.
// Every step is safe to try again: a delete of a durable already gone succeeds, and a create of
// a durable that a timed-out try did create, with this same config, returns that durable.
//
// A create that fails after the delete is therefore tried again inside the one consumer lease, so
// among builds that take holds such a create loses neither the durable nor its start. The lease is
// given up with the durable absent only when this process is killed between the delete and the
// create, when it loses its hold, when the grant refuses the create, or when every try fails
// within takeOverFinishWithin. The start then stays in the hold's token (intakeHoldToken), and the
// next take-over of this build -- by
// this intake, or by the process that takes the hold next -- creates the durable at that old ack
// floor rather than at the default start (deliveryConsumerConfig names none, so the stream's first
// message). A process of the build before holds reconciles the durable under the same lease and
// never reads the token: finding the durable absent, it creates it at its own default start, the
// stream's first message for the events durable and the next new message for the jobs durable
// (TestATakeOverWhoseCreateFailsCreatesTheDurableBeforeAPreviousBuildReconciles).
func (d *heldDurable) finishTakeOver(create natsgo.ConsumerConfig, deleteFirst bool) {
	finish, cancel := context.WithTimeout(context.Background(), takeOverFinishWithin)
	defer cancel()
	if deleteFirst {
		if d.retryHeld(finish, "delete NATS consumer to take it over", func(call context.Context) error {
			_, err := onConnection(call, d.nats, func(request context.Context) (struct{}, error) {
				return struct{}{}, d.nats.JS().DeleteConsumer(bus.Stream, d.name, natsgo.Context(request))
			})
			if err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
				return err
			}
			return nil
		}) != nil {
			return
		}
	}
	if d.retryHeld(finish, "bind NATS consumer", func(context.Context) error {
		_, err := d.subscribe(create.DeliverSubject)
		return err
	}, "subject", create.FilterSubject) != nil {
		return
	}
	if d.retryHeld(finish, "create NATS consumer", func(call context.Context) error {
		_, err := onConnection(call, d.nats, func(request context.Context) (*natsgo.ConsumerInfo, error) {
			return d.tuning.createDurable(d.nats.JS(), &create, natsgo.Context(request))
		})
		return err
	}, "subject", create.FilterSubject, "start_seq", create.OptStartSeq) != nil {
		d.stopDeliveringWithin(intakeHoldRenew)
		return
	}
	bound := intakeHoldToken{subject: create.DeliverSubject}
	if d.retryHeld(finish, "record the intake's deliver subject", func(call context.Context) error {
		return d.hold.SetToken(call, bound.String())
	}) != nil {
		// The durable delivers here, but the hold does not say so: the next holder would take
		// the binding for a process that holds nothing and wait for it. Stop, and take the
		// durable over again on the next attempt.
		d.stopDeliveringWithin(intakeHoldRenew)
		return
	}
	d.recorded, d.subject = bound, create.DeliverSubject
	d.tuning.log.Info("dispatch delivery: NATS consumer bound", "name", d.name, "subject", create.FilterSubject,
		"start_seq", create.OptStartSeq, "generation", bus.MetadataGeneration(create.Metadata, consumerGenerationKey))
	if d.boundHook != nil {
		d.boundHook()
	}
}

// errHoldNotHeld is retryHeld finding that this intake no longer holds its hold.
var errHoldNotHeld = errors.New("the intake no longer holds its hold")

// retryHeld calls step until it succeeds, while this intake holds its hold and finish has not
// ended, waiting takeOverFirstRetry after the first failed try and twice as long after each one
// after it, up to takeOverLastRetry. A try lost with its connection
// (bus.ErrRequestLostWithConnection) is tried again at once, on the connection that came back,
// and leaves the wait where it was: the loss says nothing about the step. A try the connection's
// grant refuses (bus.ErrPublishDenied) ends the step at once, since no later try gets past it.
// Each try runs on a context that ends takeOverCallTimeout after it starts, or at the hold's loss
// deadline if that comes first, so no step acts once another process can hold the hold. A failed
// try is logged at WARN as doing what names, and the failure it gives up on at ERROR by
// failedStep, so a caller writes no step's text a second time and keeps only what it does next.
// It returns nil once step succeeds, and otherwise the last try's error, or errHoldNotHeld.
// attributes are the caller's own on that ERROR line.
func (d *heldDurable) retryHeld(finish context.Context, doing string, step func(context.Context) error, attributes ...any) error {
	wait := takeOverFirstRetry
	for {
		if !d.holds() {
			return d.failedStep(doing, errHoldNotHeld, attributes)
		}
		deadline := time.Now().Add(takeOverCallTimeout)
		if lost := d.hold.Deadline(); lost.Before(deadline) {
			deadline = lost
		}
		call, cancel := context.WithDeadline(finish, deadline)
		err := step(call)
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, bus.ErrPublishDenied) {
			return d.failedStep(doing, err, attributes)
		}
		retryIn := wait
		if errors.Is(err, bus.ErrRequestLostWithConnection) {
			retryIn = 0
		}
		d.tuning.log.Warn("dispatch delivery: take-over step failed; trying it again", "name", d.name, "step", doing, "retry_in", retryIn, "error", err)
		timer := time.NewTimer(retryIn)
		select {
		case <-finish.Done():
			timer.Stop()
			return d.failedStep(doing, err, attributes)
		case <-timer.C:
		}
		if retryIn > 0 {
			wait = min(2*wait, takeOverLastRetry)
		}
	}
}

// failedStep logs the take-over step retryHeld gave up on at ERROR, as "dispatch delivery: " and
// doing, with the durable's name, the caller's own attributes and err, and answers err.
func (d *heldDurable) failedStep(doing string, err error, attributes []any) error {
	d.tuning.log.Error("dispatch delivery: "+doing, append(append([]any{"name", d.name}, attributes...), "error", err)...)
	return err
}
