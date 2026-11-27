package delivery

// held_durable_export.go is the held durable as a consumer outside this package uses it
// (HeldDurable, NewHeldDurable, HeldMessage): Dispatch's Legion state intake (legion.Intake)
// delivers from a durable of its own through it. The held durable itself is held_durable.go's.

import (
	"context"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// HeldDurable is a heldDurable for a consumer outside this package: Dispatch's Legion state intake
// (legion.Intake) delivers from its own durable under that durable's own hold and consumer lease,
// exactly as the two intakes here deliver from theirs, so that of every envoy-dispatch process one
// delivers from it at a time. Its owner names the durable's config, how a message is keyed, how
// one is handled, and what runs once a take-over has bound the durable; Run, Queue and Stop are
// what it does with them.
type HeldDurable struct {
	heldDurable
}

// HeldMessage is one message a HeldDurable hands its handler, on the goroutine of the message's
// key: the message, its envelope as the key decoded it (nil where it did not decode), the key it
// runs under (Key, as the owner's key function answered it for this delivery), and the delivery it
// arrived on, which answers it.
type HeldMessage struct {
	Msg      *natsgo.Msg
	Envelope *contracts.Envelope
	Key      string
	delivery *intakeDelivery
}

// NewHeldDurable builds the delivery from config's durable on natsClient, a client the owner opens
// for it alone (a bus.Client holds one JetStream subscription), taking the durable's hold and
// consumer lease on leases (ReserveDurableLeases), keying each message by key and handing it to
// handle on that key's goroutine. bound, when not nil, runs once each take-over has bound the
// durable, on the run loop: its first creation, and every take-over after it by this process. It
// should only queue work (Queue), since the run loop waits for it. The durable is timed by
// production's tuning (newIntakeTuning): its stop's budget and its calls' waits. config names the
// durable's whole config, its ack wait included, so the tuning's messageTimeout and maxAckPending,
// which only the two intakes here read, play no part.
func NewHeldDurable(natsClient *bus.Client, leases DurableLeases, config func() natsgo.ConsumerConfig,
	key func(*natsgo.Msg) (string, *contracts.Envelope, bool), handle func(HeldMessage), bound func(),
) *HeldDurable {
	consumer := func(*intakeTuning) natsgo.ConsumerConfig { return config() }
	held := &HeldDurable{heldDurable: newHeldDurable(newIntakeTuning(), natsClient, leases, consumer, key, func(delivery *intakeDelivery, message intakeMessage) {
		handle(HeldMessage{Msg: message.msg, Envelope: message.envelope, Key: message.key, delivery: delivery})
	})}
	held.bound = bound
	return held
}

// Run delivers from the durable while this process holds its hold, and stands by for the hold
// otherwise, until ctx ends; then it stops delivering and releases the hold under intakeStopBudget
// from ctx's end, as the intakes' Runs do (heldDurable.run), and returns.
func (h *HeldDurable) Run(ctx context.Context) {
	h.run(ctx)
}

// Stop, called once Run has returned, closes the durable's NATS client and then waits for any
// hand-back finalizer Run left behind, as StartIntakes' stop does for the intakes (stopIntakes). A
// closed client makes such a finalizer settle: its close reset each socket that carried an
// unconfirmed hand-back (bus.Client.MarkUnconfirmed), so the finalizer releases the hold through
// the still-open database pool before Stop returns. So an owner that closes the pool after Stop,
// as cmd/dispatch's shutdown does, closes it under no finalizer's write.
func (h *HeldDurable) Stop() {
	h.nats.Close()
	h.waitFinalizers()
}

// Queue hands work to key's goroutine of the subscription this process delivers on, behind the
// messages and the work key already has waiting, so it never runs beside one of key's messages,
// and reports whether it took work. It takes nothing while this process delivers nothing, once that
// subscription has begun to stop, or once its hold no longer covers it: the work then has no
// runner to wait on, and the next take-over's callback (bound) is where its owner queues it again.
// Work taken runs under the subscription's handling context, which the stop cancels when it gives
// its handlers up; work still waiting behind its key at the stop is dropped, since it holds no
// message to hand back (intakeDelivery.stop).
func (h *HeldDurable) Queue(key string, work func(context.Context)) bool {
	h.mu.Lock()
	delivery := h.delivering
	h.mu.Unlock()
	if delivery == nil || delivery.stopping.Load() || !delivery.holdCovers() {
		return false
	}
	return delivery.keys.run(key, intakeMessage{work: work})
}

// Context is the context the message's handling runs under: the stop cancels it when it gives the
// handlers still running up (StopDelivering), and a handler then hands its message back.
func (m HeldMessage) Context() context.Context {
	return m.delivery.handling
}

// Ack acknowledges the message, and hands it back for redelivery when the acknowledgement does not
// reach the server (intakeDelivery.acknowledge), logged through the delivery's own logger.
func (m HeldMessage) Ack() {
	m.delivery.acknowledge(m.delivery.tuning.log, m.Msg)
}

// HandBack hands the message back for redelivery at once, while the hold still covers this
// delivery, and leaves it with the durable for the next holder's take-over once it does not
// (intakeDelivery.handBack).
func (m HeldMessage) HandBack() {
	m.delivery.handBack(m.delivery.tuning.log, m.Msg)
}

// HandBackAfter hands the message back for redelivery in after: the handler's own answer that the
// message is to come round again later, not now (legion.Intake's owed read). It is published only
// while this delivery may still answer the durable, and withheld otherwise, the message left with
// the durable for the next holder's take-over (intakeDelivery.handBackOwn). It reports whether the
// hand-back was admitted for its publish (intakeDelivery.publishHandBack), not whether NATS took
// it: when it was not, it has logged one line that names why, and the message goes back to the
// stream only through that take-over.
func (m HeldMessage) HandBackAfter(after time.Duration) bool {
	return m.delivery.handBackOwn(m.delivery.tuning.log, m.Msg, after)
}
