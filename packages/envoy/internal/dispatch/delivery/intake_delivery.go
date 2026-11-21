package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// intakeDelivery is one subscription of an intake's: the client it subscribed on, the
// subscription, the keyed runner its callback (receive) hands messages to, the hold it delivers
// under, and the context every handler of it runs under. Each subscribe makes one of its own, so
// stopping one ends that subscription and nothing a later subscribe starts.
type intakeDelivery struct {
	nats *bus.Client
	sub  *natsgo.Subscription
	keys *keyedRunner[intakeMessage]
	// key reads whether this delivery wants a message at all and the key its handler runs under
	// (heldDurable.key).
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool)
	// hold is the hold the subscription delivers under: once its loss deadline has passed, every
	// message is handed back rather than handled, since this intake may no longer act on the
	// durable's messages.
	hold *duty.Hold
	// subject is the deliver subject this subscription was bound to: the subject the hold's
	// token records while this delivery is the one the durable delivers to
	// (heldDurable.finishTakeOver), and the one every message of it was fetched on.
	subject string
	// handling is the parent of every handler's context (deliver), and abandon cancels it: the stop
	// gives up the handlers still running when its context ends.
	handling context.Context
	abandon  context.CancelFunc
	stopping atomic.Bool
	stopped  sync.Once

	mu sync.Mutex
	// held is what the stop hands back to NATS once no handler of this delivery is running: the
	// messages that were waiting behind their keys, and those the callback was handed since.
	held []*natsgo.Msg
	// released is set once held has been handed back; the callback then hands a message back at
	// once (giveBack).
	released bool
	// handBackAllowed is false at the start and set once from the drain result: true when the
	// drain answered with a handle. A failed connected read during a stop clears it; the drain
	// cannot undo that read because no read runs while the flag is false. The stop withholds its
	// hand-backs while it is false (mayHandBack).
	handBackAllowed bool
	// publishing counts the hand-back publishes NATS has not answered yet (publishHandBack), and
	// published is closed each time that count falls to zero. It is made again each time the
	// count rises from zero, and once made is never set back to nil, so handedBack reads it. The
	// hold's release waits for them (heldDurable.handBacksReached).
	publishing int
	published  chan struct{}
	// sealed is set once no hand-back of this delivery may begin any more: at the end of its stop,
	// and when its hold is lost (seal). A hand-back is admitted and counted in publishing in one step
	// with reading it (admitHandBack), so every hand-back is either counted before the seal, and the
	// hold's release waits for it, or never published.
	sealed bool

	// beforeHandBackPublish, when set, runs before each hand-back is admitted and published, once
	// the rule has let it through (publishHandBack). It is nil outside tests: a test sets it to take
	// nats.go's connection lock in that moment, or to hold the hand-back there while a stop seals.
	beforeHandBackPublish atomic.Pointer[func()]
}

// intakeMessage is one message on its way to its key's handler, with its envelope when keying
// decoded it, so the handler does not decode it again. Keying decodes every message it wants, a
// pull request's, a workflow run's and a push's alike, so the envelope is absent only when that
// decode failed and deliver reports the failure.
type intakeMessage struct {
	msg      *natsgo.Msg
	envelope *contracts.Envelope
}

// subscribe subscribes the intake's handling to its durable's push deliveries at deliverSubject
// (subscribeDurable), for a caller that creates the durable with it right after (takeOver). The
// subscription's callback only routes each message (receive); its handler runs on the goroutine of
// the message's key (keyedRunner) and acknowledges it there. StopDelivering stops the subscription
// this made last, so a caller subscribing again stops the one before first. A subscribe that fails
// gives up its handling and the registration the client kept of it (bus.Client.ReleaseFailed): a
// reconnect would bind that registration otherwise, and its callback, its handling given up, would
// hand back every message until the intake's next subscribe replaced it.
func (d *heldDurable) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	handling, abandon := context.WithCancel(context.Background())
	delivery := &intakeDelivery{
		nats: d.nats, key: d.key, hold: d.hold, subject: deliverSubject,
		handling: handling, abandon: abandon,
	}
	delivery.keys = newKeyedRunner(func(message intakeMessage) { d.handle(delivery, message) })
	wanted := d.config()
	sub, err := subscribeDurable(d.nats, wanted, deliverSubject, delivery.receive)
	if err != nil {
		abandon()
		d.nats.ReleaseFailed(wanted.FilterSubject)
		return nil, err
	}
	delivery.sub = sub
	d.mu.Lock()
	d.delivering = delivery
	d.mu.Unlock()
	return sub, nil
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe at
// deliverSubject, acknowledging by hand. Bind with DeliverSubject and SkipConsumerLookup
// subscribes without looking the durable up, so the durable need not exist yet: nats.go sends
// the SUB and creates nothing (js.go, the SkipConsumerLookup branch of subscribe with Bind). It
// is for a caller that creates the durable with that deliver subject right after, under its
// consumer lease (takeOver).
func subscribeDurable(client *bus.Client, wanted natsgo.ConsumerConfig, deliverSubject string, deliver natsgo.MsgHandler) (*natsgo.Subscription, error) {
	options := []natsgo.SubOpt{
		natsgo.Bind(bus.Stream, wanted.Durable),
		natsgo.ManualAck(),
		natsgo.DeliverSubject(deliverSubject),
		natsgo.SkipConsumerLookup(),
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	return client.Subscribe(wanted.FilterSubject, deliver, options...)
}

// intakeStopBudget is the one deadline of an intake's stop, counted from the moment Run's ctx ends
// (stopContext): the messages in their handlers have until then to finish and be acknowledged
// before the stop gives them up (StopDelivering), the drain's wait ends by then, and a take-over
// under way as ctx ended ends by then too (takeOverFinishWithin, 10 s), so one that binds after
// ctx ended is stopped under the same deadline rather than one of its own. In cmd/dispatch that
// ctx is the signal context, so the stop begins at SIGTERM and runs beside the HTTP drain and the
// document shutdown, which take up to stopGrace (30 s) less a 5 s reserve: httpDrainBeforeDocuments
// then documentShutdownTimeout (cmd/dispatch/shutdown.go); StartIntakes' stop, which the shutdown
// waits for, adds no deadline of its own. The shutdown waits for the intakes after the document
// service and before it deregisters this process and closes the database pool, so that a handler's
// writes never meet a closed pool. 15 s leaves 10 s before the document budget's latest end for
// what follows the deadline -- the handlers given up returning, which they do as soon as their
// context is cancelled, the negative acknowledgements and the unsubscribe, each waiting for NATS at
// most bus.ConnLockWait, the drain's end, which the given-up handlers' cancelled context cuts short,
// and the clients closing. The flush that confirms the hand-backs (up to intakeFlushWait, 1 s)
// and the hold's release (up to intakeHoldRenew, 5 s) run before the close. Only a pending
// hand-back's finalizer, with its release, follows the close: Client.Close waits for nats.go's
// connection lock, which a write can hold until its one-minute write deadline; a kubelet kill
// then ends the client and renewal. It is also well past what a handler GitHub answers
// needs, a pull request and its commit pages with a page GitHub answered 502 asked again (after 1 s, then 2 s: githubPageRetryWait), so
// an ordinary handler finishes and is acknowledged rather than redone. A call GitHub does not
// answer holds one attempt for up to githubapp.RequestTimeout (20 s), and is then asked again,
// which no budget inside stopGrace could wait out. A var so a test can shrink it.
var intakeStopBudget = 15 * time.Second

// stopContext returns the stop deadline of an intake whose Run runs until ctx ends: a context that
// ends intakeStopBudget after ctx does, armed by ctx's end, so everything of the stop -- a
// take-over under way at that moment, the handler wait, the drain's wait -- runs under one
// deadline counted from it. release frees it, for run to defer.
func stopContext(ctx context.Context) (stopping context.Context, release func()) {
	// Read now rather than as ctx ends, so a test restoring intakeStopBudget after Run has
	// returned never races the read.
	budget := intakeStopBudget
	stopping, cancel := context.WithCancel(context.WithoutCancel(ctx))
	disarm := context.AfterFunc(ctx, func() { time.AfterFunc(budget, cancel) })
	return stopping, func() {
		disarm()
		cancel()
	}
}

// StopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait. It drains the subscription, so the server delivers it
// nothing more, and waits for the messages in their handlers until ctx ends: one that finishes by
// then is acknowledged, and the handlers still running when ctx ends are given up -- their context
// is cancelled, so a GitHub call that does not answer is abandoned -- and their messages
// negatively acknowledged as they return. Only once no handler is running does it negatively
// acknowledge every message that was waiting behind its key and every message the callback was
// handed meanwhile, those NATS had already delivered to the client's buffer included, so the server
// redelivers them at once -- to the next holder, which can then not start a pull request's or run's
// next message while this process still works on the one before it. A message the callback is
// handed after that is handed back at once. Each of those hand-backs is made or withheld by one
// rule (handBack): withheld once the hold's loss deadline has passed, and withheld while NATS does
// not answer the stop at once, the messages staying outstanding for the take-over or the ack wait.
// It returns once a drain it began has ended -- at the latest intakeDrainWait after the last
// handler returned, or as ctx ends if that is sooner, when it unsubscribes instead. The drain gives
// up the subscription's registration on the client (bus.Client.DrainSubscription), so that no
// reconnect binds it again and a later subscribe on the same client starts afresh. Every call to
// NATS the stop waits for -- its own, and the answers of the handlers it waits for, hand-backs
// included -- waits for NATS at most bus.ConnLockWait (callNATS): nats.go holds its connection
// lock, which every call to NATS waits for, through a reconnect's dial and handshake. A hand-back
// NATS has not taken by then is published later, and the hold is released only once every
// hand-back has reached the server, renewing until then (publishHandBack, heldDurable.stop). Once
// its drain has ended, or it has unsubscribed, no hand-back of this subscription begins any more
// (seal), so the hold's release waits for every hand-back there will be.
//
// run calls it as its ctx ends, under its stop deadline (stopContext), and when the hold is lost
// or the durable no longer delivers here, under a budget of its own (stopDeliveringWithin). It is
// safe from any goroutine and idempotent: a second call returns once the first has stopped, and a
// call with nothing subscribed returns at once. Every call's ctx bounds the handler wait of the
// stop under way, so a caller with a nearer deadline than the first has the handlers given up when
// its own ctx ends.
func (d *heldDurable) StopDelivering(ctx context.Context) {
	d.mu.Lock()
	delivery := d.delivering
	d.mu.Unlock()
	if delivery != nil {
		delivery.stop(ctx)
	}
}

// stop is StopDelivering for this subscription. The drain goes before any hand-back, so the server
// has the subscription's interest gone before any negative acknowledgement reaches it, and
// redelivers what this one gives back to the next subscriber rather than straight back here. It
// drains the subscription the client holds for this one's registration
// (bus.Client.DrainSubscription): a handle of the client's own once it has replaced its connection,
// not sub, which went with the replaced connection while the client's handle went on delivering
// here. The client holds nothing of this stopped callback's from the drain on. The messages waiting
// behind their keys are taken before the drain, so that none of them starts its handler meanwhile;
// they are held with the rest.
//
// Every hand-back is withheld until the drain has answered, and for good when NATS did not take the
// drain within bus.ConnLockWait, or took it with the link down, when DrainSubscription unsubscribes
// on the client alone and hands no drain back: nothing then says when the server has the
// subscription's interest gone, and a hand-back on a link that is down waits for the next attempt.
//
// stopping is set before the give-up is armed: a handler given up wakes from its context and calls
// mayHandBack, which reads stopping, so the stop's withholding covers every handler it gives up. A
// give-up armed first runs at once under a context that has already ended, and a handler it woke
// could read stopping before it was set and publish a hand-back the stop withholds.
func (d *intakeDelivery) stop(ctx context.Context) {
	d.stopping.Store(true)
	giveUp := context.AfterFunc(ctx, d.abandon)
	defer giveUp()
	if armed := stopGiveUpArmed.Load(); armed != nil {
		(*armed)()
	}
	d.stopped.Do(func() {
		for _, waiting := range d.keys.stop() {
			d.giveBack(waiting.msg)
		}
		draining, answered := callNATS("drain the intake's subscription", func() (*natsgo.Subscription, error) {
			return d.nats.DrainSubscription(d.sub)
		})
		d.mu.Lock()
		if answered && draining != nil {
			d.handBackAllowed = true
		}
		d.mu.Unlock()
		// Returns once every handler has, which a handler given up (abandon) does promptly: its
		// GitHub calls and its writes run under its context, and its answer to NATS waits at most
		// bus.ConnLockWait (handBack, acknowledge, handBackOwn).
		d.keys.wait()
		d.mu.Lock()
		d.released = true
		held := d.held
		d.held = nil
		d.mu.Unlock()
		for _, msg := range held {
			d.handBack(msg)
		}
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid. It ends
		// it under its connection lock, which a reconnect's dial and handshake hold, so the wait also
		// reads whether NATS is connected, within bus.ConnLockWait, and a read that does not answer,
		// or answers that it is not, ends the wait as the deadline does: the stop then waits out no
		// reconnect attempt for a drain nats.go cannot end during it.
		waitedFrom := time.Now()
		for draining != nil && draining.IsValid() {
			connected, answered := callNATS("read whether NATS is connected while the drain ends", func() (bool, error) {
				return d.nats.Connected(), nil
			})
			lost := !answered || !connected
			if lost || d.handling.Err() != nil || time.Since(waitedFrom) >= time.Duration(intakeDrainWait.Load()) {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", time.Since(waitedFrom), "connection_lost", lost)
				callNATS("unsubscribe the intake's subscription", func() (struct{}, error) {
					return struct{}{}, draining.Unsubscribe()
				})
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// An unsubscribe does not wait for a callback still running, which can be on its way to a
		// hand-back of a message the client had buffered (giveBack). From here no hand-back
		// begins, so the caller's release decision reads every hand-back there will be
		// (heldDurable.stop): one the callback has not been admitted for is left with the durable.
		d.seal()
		d.abandon()
	})
}

// stopGiveUpArmed is nil outside tests. A row sets it to run in stop once the give-up is armed,
// where it waits for the handler the give-up wakes to decide its hand-back.
var stopGiveUpArmed atomic.Pointer[func()]

// callNATS makes call, one call to NATS named by what, where the stop waits for it -- a call of the
// stop's own, or a handler's answer, which the stop waits for (keyedRunner.wait) -- and waits for it
// at most callNATSWait (bus.ConnLockWait, 100 ms). The call always starts, on a goroutine of its
// own, so a caller that keeps a count across the call (publishHandBack) sees the decrement when the
// call finishes, even when the goroutine is descheduled past the wait. A call NATS has not answered
// by then goes on, with a WARN line (callLeftRunning), and answered is false. That goroutine holds
// nothing of the stop's; a call through the bus client that holds the client's own lock while it
// reads nats.go's state (Connected, and DrainSubscription through it) holds that lock until nats.go
// answers. Its failure is logged when it ends, with attrs; a connection already closed is how a call
// that outlived the stop ends when the client closes at shutdown, and is logged as that rather than
// as an error; a publish refused because the connection is reconnecting (ErrReconnectBufExceeded) is
// logged at INFO.
func callNATS[T any](what string, call func() (T, error), attrs ...any) (value T, answered bool) {
	answers := make(chan T, 1)
	go func() {
		v, err := call()
		switch {
		case err == nil:
		case errors.Is(err, natsgo.ErrConnectionClosed):
			slog.Info("dispatch delivery: "+what+": the connection had closed", append(attrs, "error", err)...)
		case errors.Is(err, natsgo.ErrReconnectBufExceeded):
			slog.Info("dispatch delivery: "+what+": the connection is reconnecting, so the message is left with the durable for its ack wait", append(attrs, "error", err)...)
		default:
			slog.Error("dispatch delivery: "+what, append(attrs, "error", err)...)
		}
		answers <- v
	}()
	wait := time.Duration(callNATSWait.Load())
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case value = <-answers:
		return value, true
	case <-timer.C:
		slog.Warn(callLeftRunning, append(attrs, "call", what, "waited", wait)...)
		return value, false
	}
}

// callNATSWait is how long callNATS waits for the call's answer. It is bus.ConnLockWait (100 ms)
// outside tests. A row sets it to 0, so the timer is ready at once and a call may outlive the wait,
// to show the bookkeeping inside each call completes either way.
var callNATSWait atomic.Int64

func init() { callNATSWait.Store(int64(bus.ConnLockWait)) }

// callLeftRunning is the line callNATS logs for a call it goes on without.
const callLeftRunning = "dispatch delivery: NATS did not answer a call in time, so it goes on without waiting for it"

// giveBack hands msg back to NATS for the next holder: once no handler of this delivery is
// running, at once (handBack); until then it is held, and the stop hands it back with the rest.
// Handed back earlier, a message of a key whose handler is still running here could be
// redelivered to the next holder and run there beside it, and two fetches of one pull request or
// run at once can write the older answer last.
func (d *intakeDelivery) giveBack(msg *natsgo.Msg) {
	d.mu.Lock()
	if !d.released {
		d.held = append(d.held, msg)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	d.handBack(msg)
}

// handBack publishes msg's hand-back to the durable, so the server redelivers it at once, or
// withholds it. Every hand-back this delivery makes passes here -- the stop's, the callback's
// (giveBack), a given-up handler's (Intake.deliver) and one an acknowledgement NATS refused makes
// (acknowledge) -- but for the delayed hand-back a handler makes of its own (handBackOwn), which
// mayAnswer gates instead of the hold alone, and which can meet mayHandBack's connected read
// only when a stop begins between the two checks. This is the one place the rule is:
//
//   - Withheld once the hold's loss deadline has passed (holdCovers). Every hand-back is a publish
//     at the message's reply subject, which names the durable rather than the consumer the message
//     was fetched on. A take-over deletes the durable and creates it again under the same name
//     (heldDurable.takeOver), so a hand-back published after the lapse lands on the new holder's
//     consumer, where it moves that message's pending entry and has the new holder delivered it a
//     second time -- two processes fetching one pull request or run at once, which is what the hold
//     exists to stop. A holder whose hold is live still hands its messages back at once, which is
//     what makes a graceful stop's hand-back reach the standby within seconds rather than after the
//     ack wait: a stop begins intakeHoldMargin before the hold's loss deadline at the latest
//     (heldDurable.run), and only the part of it that runs past that deadline is withheld.
//   - Once the stop has begun, withheld unless NATS took the stop's drain on a live link (stop).
//   - Once the delivery is sealed -- its stop has ended, or its hold is lost -- withheld for good
//     (admitHandBack), so the hold's release has counted every hand-back that is still to land.
//   - Once the stop has begun, withheld when NATS does not answer a read of whether the connection
//     is connected within bus.ConnLockWait (callNATS), or answers that it is not (mayHandBack): a
//     hand-back on a link that is down, or in a reconnect's handshake, would wait for the
//     reconnect. That read clears handBackAllowed. Outside any stop, the connected read is not
//     made: a hand-back is published, left running when a write holds nats.go's lock, and on a
//     reconnecting link refused between attempts with an INFO line, or held until the attempt
//     ends.
//
// The publish itself waits for NATS at most bus.ConnLockWait too (publishHandBack), since the stop
// waits for the handlers that make it and for its own hand-backs: a write to a slow socket can take
// nats.go's connection lock between the read and the publish. One NATS has not taken by then is
// published later, and the hold is released only once every hand-back has reached the server,
// renewing until then, so none reaches a durable a standby recreated (heldDurable.stop).
//
// A message withheld is not lost: it was never acknowledged, so it stays outstanding, and the
// take-over's recreate at the durable's ack floor hands it to the new holder once, or the durable
// redelivers it after its ack wait. The rule reads the hold, and not the stop's flag as mayAnswer
// does: the stop is itself one of the callers, and a hand-back of a message this delivery was
// handed is sound while the hold covers it whether or not this subscription still delivers.
func (d *intakeDelivery) handBack(msg *natsgo.Msg) {
	if !d.holdCovers() {
		slog.Warn("dispatch delivery: the hold no longer covers this delivery, so the message is left with the durable for the next holder's take-over",
			"subject", msg.Subject)
		return
	}
	if !d.mayHandBack() {
		slog.Info("dispatch delivery: the hand-back is withheld, so the message is left with the durable", "subject", msg.Subject)
		return
	}
	d.publishHandBack(msg, 0)
}

// handBackOwn publishes msg's hand-back after after, the answer a handler makes of its own
// (Intake.deliver's ErrHeadNotStored case): only while this delivery may answer the durable
// (mayAnswer), which needs !stopping. So the connected read in mayHandBack can withhold only
// when a stop begins between the two checks. A handler makes it before or after any stop
// begins, and the stop waits for that handler.
func (d *intakeDelivery) handBackOwn(msg *natsgo.Msg, after time.Duration) {
	if !d.mayAnswer() {
		slog.Warn("dispatch delivery: the hold no longer covers this delivery, so the envelope whose head row was not stored is left with the durable for the next holder's take-over",
			"subject", msg.Subject)
		return
	}
	if !d.mayHandBack() {
		slog.Info("dispatch delivery: the hand-back is withheld, so the envelope whose head row was not stored is left with the durable", "subject", msg.Subject)
		return
	}
	d.publishHandBack(msg, after)
}

// publishHandBack publishes msg's hand-back after after, waiting for NATS at most
// bus.ConnLockWait (callNATS), unless the delivery is sealed (admitHandBack). A publish NATS has not
// taken by then goes on, on a goroutine of its own, and is counted until NATS takes it
// (publishing). The hold's release waits for every such publish of the delivery it stopped, and
// then for the server to have them: under the stop's deadline, and past it on a goroutine that
// keeps the hold renewing (heldDurable.finalizeHandBacks), so none reaches a durable a standby
// recreated. A hold lost meanwhile seals the delivery and severs the connection that carries a
// hand-back the server may not have yet (heldDurable.severHandBacks), so that none reaches the
// server after the hold has lapsed.
func (d *intakeDelivery) publishHandBack(msg *natsgo.Msg, after time.Duration) {
	if hook := d.beforeHandBackPublish.Load(); hook != nil {
		(*hook)()
	}
	if !d.admitHandBack() {
		slog.Info(handBackSealed, "subject", msg.Subject)
		return
	}
	callNATS("hand a message back for redelivery", func() (struct{}, error) {
		defer func() {
			d.mu.Lock()
			d.publishing--
			if d.publishing == 0 {
				close(d.published)
			}
			d.mu.Unlock()
		}()
		return struct{}{}, msg.NakWithDelay(after)
	}, "subject", msg.Subject, "delay", after)
}

// handBackSealed is the line of a hand-back withheld because its delivery was sealed before the
// hand-back was admitted (admitHandBack).
const handBackSealed = "dispatch delivery: the intake has stopped or lost its hold, so the hand-back is withheld and the message is left with the durable for the next holder's take-over"

// admitHandBack counts a hand-back about to be published (publishing) and reports true, or reports
// false once the delivery is sealed. The read of the seal and the count are one step under mu, as
// the seal is (seal): a hand-back the rule let through before the stop decided that none may begin
// any more is counted, and the hold's release waits for it; one after is never published. So a
// subscription callback that passed the rule, and was then descheduled while an unsubscribe went
// on without it, cannot hand a message back past a release that did not count it.
func (d *intakeDelivery) admitHandBack() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sealed {
		return false
	}
	if d.publishing == 0 {
		d.published = make(chan struct{})
	}
	d.publishing++
	return true
}

// seal decides that no hand-back of this delivery begins any more (admitHandBack). Every hand-back
// it lets begin is counted by the time it returns, so handedBack and handBacksLanded read every
// hand-back there will be. Sealing again changes nothing.
func (d *intakeDelivery) seal() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sealed = true
}

// handBacksLanded is closed once no hand-back publish of this delivery is still waiting for NATS
// (publishHandBack), and already closed when none is.
func (d *intakeDelivery) handBacksLanded() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.publishing == 0 {
		landed := make(chan struct{})
		close(landed)
		return landed
	}
	return d.published
}

// handedBack reports whether this delivery has published any hand-back (publishHandBack).
func (d *intakeDelivery) handedBack() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.published != nil
}

// mayHandBack reports whether a hand-back may be published now (handBack): outside any stop, always;
// once the stop has begun, only while handBackAllowed and NATS answers within bus.ConnLockWait
// that the connection is connected. A read that does not answer, or answers that the connection is
// not connected, clears handBackAllowed.
func (d *intakeDelivery) mayHandBack() bool {
	if !d.stopping.Load() {
		return true
	}
	d.mu.Lock()
	allowed := d.handBackAllowed
	d.mu.Unlock()
	if !allowed {
		return false
	}
	connected, answered := callNATS("read whether NATS is connected before a hand-back", func() (bool, error) {
		return d.nats.Connected(), nil
	})
	if answered && connected {
		return true
	}
	d.mu.Lock()
	d.handBackAllowed = false
	d.mu.Unlock()
	return false
}

// acknowledge acknowledges msg, waiting for NATS at most bus.ConnLockWait (callNATS): the stop
// waits for the handler that makes it, whether that acknowledgement began before the stop or after.
// An acknowledgement NATS has not taken by then goes on without the caller, which is sound whenever
// it lands, since the message's handling is done; if it then fails, the message stays outstanding
// for the ack wait. One NATS refused in time is handed back, under handBack's rule.
func (d *intakeDelivery) acknowledge(msg *natsgo.Msg) {
	failed, answered := callNATS("acknowledge message", func() (bool, error) {
		err := msg.Ack()
		return err != nil, err
	}, "subject", msg.Subject)
	if answered && failed {
		d.handBack(msg)
	}
}

// intakeDrainWait bounds how long stop waits for the drain to end once every handler has returned.
// By then all that is left is nats.go's: the callback handing back what the client had buffered,
// which never blocks, and the drain's flush to the server. So the bound is the one nats.go itself
// gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections never set their own --
// rather than intakeMessageTimeout, which bounds a handler, and the handlers are done. The stop's
// context ending cuts it shorter: by then the drain has had the whole handler wait to end. A
// variable so a row can keep it from ending the wait, and see what else does; atomic, since a stop
// the row gave up on may still read it as the row ends.
var intakeDrainWait atomic.Int64

func init() { intakeDrainWait.Store(int64(natsgo.DefaultDrainTimeout)) }

// receive is the subscription's callback, and only routes: a message once stopping is set, or once
// the hold's loss deadline has passed, is handed back (giveBack) -- this intake may no longer act
// on the durable's messages, and the server redelivers them to the next holder, or, once that
// deadline has passed, the next holder's take-over hands them over instead (handBack); a subject
// the intake's key does not want is acknowledged; and any other message goes to its key's handler
// (deliver).
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	if d.stopping.Load() || !d.holdCovers() {
		d.giveBack(msg)
		return
	}
	key, envelope, wanted := d.key(msg)
	if !wanted {
		d.acknowledge(msg)
		return
	}
	if !d.keys.run(key, intakeMessage{msg: msg, envelope: envelope}) {
		d.giveBack(msg)
	}
}

// holdCovers reports whether the hold this delivery delivers under has not passed its loss
// deadline (duty.Hold.Deadline), and so whether this intake may still act on the durable's
// messages. A delivery with no hold is one no hold governs, and is covered.
func (d *intakeDelivery) holdCovers() bool {
	return d.hold == nil || time.Now().Before(d.hold.Deadline())
}

// mayAnswer reports whether this delivery may answer the durable with a hand-back of its own,
// which is the one answer no stop of this intake makes for it (Intake.deliver's ErrHeadNotStored
// case). Two things have to hold, and both are the hold's:
//
//   - the hold has not passed its loss deadline (holdCovers), so no other process can hold the
//     durable yet;
//   - this subscription is still the one the hold's durable delivers to (stopping), so the
//     durable this answer names is the one the message was fetched on, at the deliver subject
//     the hold's token records (subject).
//
// Either gone, the answer is withheld rather than published. A take-over deletes the durable and
// creates it again under the same name (heldDurable.takeOver), and the server reads a message's
// reply subject for that name: a hand-back published after it would move the new holder's
// pending entry for that message and have the new holder delivered it a second time. Nothing is
// lost by withholding it -- the message is still outstanding, and the take-over's recreate at
// the durable's ack floor hands it to the new holder once.
//
// The second reading is the stop's flag rather than the hold's token read from the database:
// this path is one a database that refused a write put us on, so a read of the row here would
// be refused with it, and the token can move to another deliver subject only once this
// subscription has been stopped. The intake stops it the moment it is no longer the one the
// durable delivers to -- the hold lost, a check finding the durable bound elsewhere, the
// process stopping -- and every one of those sets stopping before anything else (stop); its run
// loop takes the durable over again only while it delivers nothing (heldDurable.run), so the
// subject the hold records cannot change under a delivery that is still running. What the flag
// does not do, and no reading of the row would do either, is cover the moment between this
// subscription binding and the take-over recording its subject: there the hold's token still
// names the holder before this one, while the durable this answers was created by this process,
// under this hold and the durable's consumer lease, and delivers here alone. A message handed
// over in that moment is this delivery's to answer.
func (d *intakeDelivery) mayAnswer() bool {
	return !d.stopping.Load() && d.holdCovers()
}

// intakeKey reads from a message's subject, notifications.github.<owner>.<repo>.<kind>..., and
// from its envelope, whether the intake wants it and the key its handler runs under. Only three
// shapes can carry a kind route handles: a pull request's own subject, pr.<n> and nothing after
// it (pr.<n>.review and pr.<n>.comment carry other kinds, contracts' githubTopic), a workflow
// run's, workflow.<file>.<action>, and a push of a branch's, push.branch.<branch>, whose branch is
// one segment however many dots or slashes it holds (contracts.GithubPushSubject). Every other
// subject -- checks, comments, issues, a push of a tag (push.tag.<tag>), job events -- is not
// wanted, and is decided with no decode at all.
//
// Every wanted message is decoded, and one whose subject names a repository other than the one
// its payload names is not wanted. That message is the second copy the listener publishes of a
// renamed repository's envelopes, under the repository's former name (internal/ghrename): the
// same event_id, the same dedupe_key and the same payload as the original, differing only in its
// topic. Keyed by its own subject it would key differently from the original, so keyedRunner
// would start a goroutine for each and two handlers would run at once on one pull request's row,
// each doing the full FetchPullRequest and paging the same commits, where the older answer can
// be written last; and a push's copy would hand the importer one push twice. Dropping the copy
// loses nothing: the original carries that same event to that same handler. The two repositories
// are compared one subject segment at a time -- contracts.SanitizeSubjectSegment of the payload's
// owner and of its name against subject tokens 2 and 3, since a name may hold a dot, which the
// sanitizer replaces -- and without case, as GitHub compares repository names.
//
// The envelope decode costs nothing new: the envelope decoded here is handed on (intakeMessage),
// and deliver would otherwise decode it itself. The payload is another matter -- route parses the
// same payload string again for its kind and its fields -- so a pull request's or a push's message
// pays one extra JSON parse, which is what the copy rule costs. A pull request is keyed by its
// subject's repository and number. A push of a branch is keyed by its subject's repository alone,
// `push <owner>/<repo>`: its handler is the importer's one read of that repository's sources,
// which returns without waiting for an import (PushImports), so the pushes of one repository,
// handled in delivery order, wait for one another for that read alone. A workflow run's subject
// names its file and not its run, so its key is its payload's repo and run_id.
//
// A message this cannot judge is wanted rather than dropped on a comparison that could not be
// made, and a pull request or a workflow run keeps the key it had before the rule. The canonical
// repository is the one the payload names, and a message whose envelope does not decode, whose
// payload does not decode, or whose payload names no repository that splits into an owner and a
// name -- an empty repo above all -- names none to compare the subject with. A pull request and a
// push are keyed by their subjects whatever the payload holds. A workflow message whose envelope
// or payload does not decode is keyed by its subject, and its handler logs it; an envelope that
// does not decode is handed on undecoded, so deliver reports the decode failure.
func intakeKey(msg *natsgo.Msg) (key string, envelope *contracts.Envelope, wanted bool) {
	tokens := strings.Split(msg.Subject, ".")
	if len(tokens) < 5 || tokens[0] != "notifications" || tokens[1] != "github" {
		return "", nil, false
	}
	pullRequest := tokens[4] == "pr" && len(tokens) == 6
	workflow := tokens[4] == "workflow" && len(tokens) == 7
	branchPush := tokens[4] == "push" && len(tokens) == 7 && tokens[5] == "branch"
	if !pullRequest && !workflow && !branchPush {
		return "", nil, false
	}
	decoded, err := decodeEnvelope(msg.Data)
	if err != nil {
		return subjectKey(msg.Subject, tokens), nil, true
	}
	var event struct {
		Repo  string `json:"repo"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(decoded.Payload), &event); err != nil {
		return subjectKey(msg.Subject, tokens), &decoded, true
	}
	if !subjectNamesRepo(tokens, event.Repo) {
		return "", nil, false
	}
	if workflow {
		return "workflow " + event.Repo + "#" + event.RunID, &decoded, true
	}
	return subjectKey(msg.Subject, tokens), &decoded, true
}

// subjectKey is the key a message keeps when its subject is all there is to key it by: a pull
// request by its subject's repository and number, a push of a branch by its subject's repository,
// and a workflow run by its whole subject, since that subject names the workflow file and not the
// run. It is built where it is returned, so a message pays for the one key it is handled under.
func subjectKey(subject string, tokens []string) string {
	switch tokens[4] {
	case "pr":
		return "pr " + tokens[2] + "/" + tokens[3] + "#" + tokens[5]
	case "push":
		return "push " + tokens[2] + "/" + tokens[3]
	}
	return "workflow " + subject
}

// subjectNamesRepo reports whether a GitHub subject's owner and repository tokens (2 and 3) name
// repo, the "<owner>/<name>" a payload carries, each side sanitized as the publisher sanitizes it
// (contracts.SanitizeSubjectSegment) and compared without case. A repo refs.SplitGitHubRepo
// refuses -- an empty one above all, and one whose owner or name GitHub's names do not allow --
// names no repository to compare, and answers true, so intakeKey wants that message as it always
// did rather than dropping it; its handler then refuses the name when it splits it, before any
// GitHub call.
func subjectNamesRepo(tokens []string, repo string) bool {
	owner, name, err := refs.SplitGitHubRepo(repo)
	if err != nil {
		return true
	}
	return strings.EqualFold(contracts.SanitizeSubjectSegment(owner), tokens[2]) &&
		strings.EqualFold(contracts.SanitizeSubjectSegment(name), tokens[3])
}

// decodeEnvelope decodes one message's envelope. A var so a test can count the decodes.
var decodeEnvelope = func(data []byte) (contracts.Envelope, error) {
	var envelope contracts.Envelope
	err := json.Unmarshal(data, &envelope)
	return envelope, err
}
