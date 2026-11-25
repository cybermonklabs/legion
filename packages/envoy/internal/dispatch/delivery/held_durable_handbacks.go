package delivery

import (
	"context"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// held_durable_handbacks.go is the hand-back lifecycle of a stopped intake delivery, which
// heldDurable.stop begins: the check that every hand-back reached NATS before the hold is released
// (handBacksReached, flushHandBacks, handBacksSettled, releaseHold); the finalizer that keeps the
// hold renewing while one is still pending past the stop's deadline (finalizeHandBacks,
// waitFinalizers); and the loss watcher that severs the intake client's connection when the hold
// is lost while one may still reach the server (watchLoss, lostWith, confirmHandBacks,
// severHandBacks).

// intakeHoldFinalizing is the line of a stop whose hand-back had not reached NATS by its deadline,
// so its hold stays renewing until it does (stop).
const intakeHoldFinalizing = "dispatch delivery: a hand-back has not reached NATS by the stop's deadline, so the intake's hold stays renewing until it does"

// finalizeHandBacks keeps hold renewing while a hand-back of the stopped delivery is pending, and
// releases it once that hand-back has landed and a flush on the connection it was written to
// answers, or once the intake client's connection has closed (handBacksSettled). A connection that
// carries a hand-back no flush on it has confirmed resets its sockets as it closes
// (bus.Client.MarkUnconfirmed): at Close, which StartIntakes' stop makes, nats.go's final flush of
// its outbound buffer and whatever the kernel still holds of the hand-back are discarded rather than
// sent, and when nats.go gives a socket up as stale and reconnects, a flush on the new socket
// confirms nothing the old one could still send; only then is it safe to release the hold. It runs
// on a goroutine of its own that nothing in Run waits for, so it waits for nats.go's lock with no
// bound and makes one flush at a time. Each flush is nats.go's FlushTimeout of intakeFlushWait,
// whose span on the wall clock starts only once the flush holds that lock: a reconnect's handshake
// or a slow write that holds the lock for a second or more delays the flush rather than failing it.
// StartIntakes waits for it after it closes that client and before it returns to a caller that can
// close the database pool. A lost hold ends it, and severs the connection that carries the
// hand-back (severHandBacks): another holder may act once the row lapses, as after any loss of the
// hold, and the hand-back must not reach the durable that holder recreates.
func (d *heldDurable) finalizeHandBacks(delivery *intakeDelivery, hold *duty.Hold, lapse bool) {
	select {
	case <-delivery.handBacksLanded():
	case <-hold.Lost():
		// The delivery's loss watcher severs what may still reach NATS (watchLoss).
		return
	}
	retry := time.NewTicker(intakeFlushWait)
	defer retry.Stop()
	for {
		if d.handBacksSettled(flushWithin) {
			if before := d.tuning.beforeRelease.Load(); before != nil {
				(*before)()
			}
			select {
			case <-hold.Lost():
			default:
				delivery.markSettled()
				d.releaseHold(hold, lapse)
			}
			return
		}
		select {
		case <-hold.Lost():
			return
		case <-retry.C:
		}
	}
}

// intakeHandBacksSevered is the line of an intake whose hold was lost while a hand-back it began
// could still reach NATS, so it severs the connection that carries it (severHandBacks).
const intakeHandBacksSevered = "dispatch delivery: the intake's hold is lost with a hand-back NATS may not have yet, so its connection is severed and the hand-back never reaches the durable the next holder recreates"

// severHandBacks makes sure that no hand-back of this intake's stopped delivery reaches NATS, for a
// caller whose hold is lost while one may still reach the server: a publish NATS has not taken
// (intakeDelivery.handBacksLanded), or one nats.go took whose arrival no flush has confirmed. The
// delivery is sealed already (intakeDelivery.seal), so no hand-back begins any more. It severs the
// intake client's connection (bus.Client.Sever): every hand-back is a publish on that connection, or
// on one the client already closed, which sends nothing, since the client replaces only a closed
// connection. Sever resets the connection's sockets without waiting for nats.go's lock, which a write
// to a stalled socket holds until its write deadline, so a publish waiting behind that write never
// goes out, nor what the kernel had not sent yet; the client then dials a connection afresh.
//
// The hold reports itself lost intakeHoldMargin before its row lapses (duty.Hold), and no standby
// takes the durable over before the row lapses, so a sever made within that margin comes before any
// standby can recreate the durable. Keeping the hold renewing is no answer here: a lost hold is one
// this process could not renew, or one the database already names another holder in. The messages
// the hand-backs named stay outstanding, and the next holder's take-over recreates the durable at
// its ack floor and is handed them once (takeOverConfig).
func (d *heldDurable) severHandBacks() {
	d.tuning.log.Warn(intakeHandBacksSevered, "name", d.name)
	d.nats.Sever()
}

// watchLoss is a delivery's loss watcher, which subscribe starts for every delivery made under a
// hold. It waits for hold.Lost, which a release closes too, or for the delivery's seal at the end of
// its stop, whichever comes first:
//
//   - On the loss, it returns at once when the hold was released with every hand-back the connection
//     carries confirmed, which marks every delivery made under the hold (intakeDelivery.markSettled).
//     Otherwise it seals delivery, so no hand-back begins any more (intakeDelivery.seal), and severs
//     (severHandBacks) when a hand-back delivery began may still reach the server -- one NATS has not
//     taken (handBacksLanded), or one whose arrival a flush does not confirm within intakeFlushWait,
//     half the hold's margin (handBacksSettled).
//   - Once sealed, it returns when delivery handed nothing back, since nothing of it can reach the
//     server any more; otherwise it flushes until a flush confirms every hand-back, one flush at a
//     time, and returns then. A loss before that is handled as above.
//
// So a watcher ends with its delivery's hand-backs, and not with the hold: the intake subscribes
// again under one live hold after a create or a token write that fails, and after a check that finds
// the durable elsewhere, and a watcher that waited for the hold's end kept one goroutine and one
// stopped delivery per take-over for as long as the hold lasted, which can be days. A stopped
// delivery whose hand-back is still pending keeps its watcher, and is severed for on the loss.
//
// Each wait for a flush's answer once the hold is lost is bounded on this goroutine, by
// intakeFlushWait on the hold's clock (heldDurable.holdClock, the real clock in production): the
// flush itself takes nats.go's lock, which a write to a stalled socket can hold for a minute, and
// runs on a goroutine of its own, which the sever's reset of that socket ends. Nothing else the
// intake does -- the run loop's stop, a check of the durable, the finalizer's own flush -- comes
// between the loss and the sever. The pause between two flushes while the hold is live is
// intakeFlushWait on the wall clock.
//
// A delivery that handed nothing back, or whose hand-backs the flush confirms, keeps its
// connection, so the handlers the run loop's stop waits for still acknowledge their messages
// through it.
func (d *heldDurable) watchLoss(delivery *intakeDelivery, hold *duty.Hold) {
	defer d.lossWatchers.Add(-1)
	select {
	case <-hold.Lost():
		d.lostWith(delivery, nil)
		return
	case <-delivery.sealedSignal:
	}
	if !delivery.handedBack() {
		return
	}
	retry := time.NewTicker(intakeFlushWait)
	defer retry.Stop()
	for {
		confirming := d.confirmHandBacks(delivery)
		select {
		case <-hold.Lost():
			d.lostWith(delivery, confirming)
			return
		case settled := <-confirming:
			if settled {
				select {
				case <-hold.Lost():
					d.lostWith(delivery, nil)
				default:
				}
				return
			}
		}
		select {
		case <-hold.Lost():
			d.lostWith(delivery, nil)
			return
		case <-retry.C:
		}
	}
}

// lostWith is the loss of the hold delivery was made under (watchLoss): unless the hold was
// released with every hand-back confirmed, it seals delivery and severs unless confirming -- a
// confirmation already under way, or a new one when nil -- answers within intakeFlushWait on the
// hold's clock (heldDurable.holdClock) that every hand-back delivery made has reached the server.
func (d *heldDurable) lostWith(delivery *intakeDelivery, confirming <-chan bool) {
	if delivery.settledBeforeRelease() {
		return
	}
	delivery.seal()
	if !delivery.handedBack() {
		return
	}
	if confirming == nil {
		confirming = d.confirmHandBacks(delivery)
	}
	wait := d.holdClock.NewTimer(intakeFlushWait)
	defer wait.Stop()
	select {
	case settled := <-confirming:
		if settled {
			return
		}
	case <-wait.C():
	}
	d.severHandBacks()
}

// confirmHandBacks answers, on the channel it returns, whether every hand-back delivery made has
// reached the server: once each has landed (handBacksLanded), whether a flush confirms them
// (handBacksSettled). It runs on a goroutine of its own, which a sever ends.
func (d *heldDurable) confirmHandBacks(delivery *intakeDelivery) <-chan bool {
	confirmed := make(chan bool, 1)
	go func() {
		<-delivery.handBacksLanded()
		confirmed <- d.handBacksSettled(flushWithin)
	}()
	return confirmed
}

// waitFinalizers waits for hand-backs a stopped durable left running to settle after its NATS
// client closes. They can then no longer wait for that client, and release their holds through the
// still-open database pool before its caller closes it (StartIntakes).
func (d *heldDurable) waitFinalizers() { d.finalizers.Wait() }

// releaseHold releases hold, or lapses it when the next holder must read a take-over's token,
// within intakeHoldRenew on the stop's clock (heldDurable.clock).
func (d *heldDurable) releaseHold(hold *duty.Hold, lapse bool) {
	release := hold.Release
	if lapse {
		release = hold.Lapse
	}
	releasing, cancel := d.clock.WithTimeout(context.Background(), intakeHoldRenew)
	defer cancel()
	if err := release(releasing); err != nil {
		d.tuning.log.Error("dispatch delivery: release the intake's hold", "name", d.name, "error", err)
	}
}

// handBacksReached reports whether every hand-back delivery published has reached the server. A
// publish NATS had not taken when the stop went on without it (intakeDelivery.publishHandBack) is
// waited for first, until ctx ends or hold is lost: the stop can return with one still waiting, since
// its drain ends once nats.go has answered the drain's flush and run the messages it had buffered,
// which need not wait for the write that holds nats.go's lock. Then the connection is flushed, a round
// trip to the server, so the hand-backs nats.go had queued have reached it (flushHandBacks). The stop
// sealed delivery before this reads it (intakeDelivery.seal), so no hand-back it does not count can
// begin afterwards: a subscription callback the stop's unsubscribe went on without is either counted
// here or withheld.
//
// A delivery that handed nothing back reports true at once only while the connection carries no
// hand-back a flush has not confirmed (bus.Client.CarriesUnconfirmed). A take-over the intake made
// again under the same hold -- after a create or a token write that failed, or a check that found the
// durable elsewhere -- leaves an earlier, stopped delivery, whose hand-back can still sit in the
// socket's kernel send queue; the release waits for it as for the latest delivery's own, since the
// flush confirms only once every marked hand-back of every delivery has been taken and reached the
// server (bus.Client.ConfirmMarked).
func (d *heldDurable) handBacksReached(ctx context.Context, delivery *intakeDelivery, hold *duty.Hold) bool {
	if delivery == nil || !delivery.handedBack() {
		return !d.nats.CarriesUnconfirmed() || d.flushHandBacks()
	}
	landed := delivery.handBacksLanded()
	select {
	case <-landed:
	case <-hold.Lost():
	case <-ctx.Done():
	}
	select {
	case <-landed:
	default:
		return false
	}
	return d.flushHandBacks()
}

// flushHandBacks is handBacksSettled within intakeFlushWait on the stop's clock
// (heldDurable.clock), on a bound of its own as the release that follows it is: both are part of
// what follows the stop's deadline (intakeStopBudget), and a healthy link answers the flush in a
// round trip. The bound starts before the flush waits for nats.go's connection lock and covers that
// wait too, and the stop goes on at the bound whether or not the flush has answered (bus.Bounded).
// A flush that has not answered by then is false.
func (d *heldDurable) flushHandBacks() bool {
	// nats.go is shown a deadline it can read: a fake's is its own (clock.WithShownTimeout).
	flushing, cancel := clock.WithShownTimeout(d.clock, context.Background(), intakeFlushWait)
	defer cancel()
	settled, _, _ := bus.Bounded(flushing, func() (bool, error) {
		return d.handBacksSettled(func(conn *natsgo.Conn) error { return conn.FlushWithContext(flushing) }), nil
	})
	return settled
}

// handBacksSettled reports whether no hand-back nats.go had taken can reach the server later
// (bus.Client.ConfirmMarkedBy): a flush answered on the socket each was written to, or on a newer
// one after the older was reset, so each has reached the server or was discarded; or the
// connection has closed, which reset every socket that carried an unconfirmed hand-back. flush is
// the caller's flush of that connection, under the caller's bound: the stop's, on its clock from
// before the wait for nats.go's lock (flushHandBacks), or nats.go's FlushTimeout of
// intakeFlushWait, on the wall clock from once the flush holds that lock, for the finalizer and
// the loss watcher, which run on goroutines of their own (finalizeHandBacks, confirmHandBacks).
func (d *heldDurable) handBacksSettled(flush func(*natsgo.Conn) error) bool {
	return d.nats.ConfirmMarkedBy(flush)
}

// flushWithin is the flush the finalizer and the loss watcher confirm hand-backs with: nats.go's
// FlushTimeout of intakeFlushWait (handBacksSettled).
func flushWithin(conn *natsgo.Conn) error { return conn.FlushTimeout(intakeFlushWait) }

// intakeFlushWait bounds each flush that confirms a stop's hand-backs reached the server before its
// hold is released (handBacksSettled). In the stop itself it is a bound on the stop's clock that
// starts before the flush waits for nats.go's connection lock (flushHandBacks). In a finalizer and
// in a loss watcher it is nats.go's own span on the wall clock, which starts once the flush holds
// that lock (natsgo.Conn.FlushTimeout), and each waits it between two flushes too, on the wall clock
// (finalizeHandBacks, watchLoss). Once the hold is lost, a loss watcher waits it for a flush's
// answer on the hold's clock before it severs (lostWith).
const intakeFlushWait = time.Second
