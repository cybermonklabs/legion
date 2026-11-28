package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// end ends the term being served. On a lost lease (ctx still live) it unsubscribes, the queued
// messages are dropped without an ack, to come back after the ack wait to whoever holds the lease,
// and the message in hand finishes, unless a shutdown comes meanwhile; a shutdown after the loss
// then gives the message in hand inflightGrace, cancels its handling and NAKs what is queued. A
// shutdown (ctx ended) of a term that still holds the lease hands its messages back (handBack),
// and one without a lease drains (endWithoutLease).
//
// The term stops electing this pod the relayer of its machine's session consumers before any of
// that (sessionRelays.setOwned, set by durableReceiver.begin): every relay is told to stop here,
// and each consumer's position in the stream is left for the pod that takes over.
func (r *durableReceiver) end(ctx context.Context) {
	r.mu.Lock()
	t := r.term
	r.term = nil
	r.state = receiveStandby
	// No term is receiving from here, whatever this one's end does with its subscription, so the
	// lease's signal reads this pod's connection alone again (durableReceiver.receiveUp).
	r.subscribed.Store(nil)
	r.mu.Unlock()
	r.relays.setOwned(false)
	switch {
	case ctx.Err() == nil:
		r.endLost(ctx, t)
	case r.lease == nil:
		r.endWithoutLease(t)
	case r.lease.Holding() && r.lease.Status().Term == t.leaseTerm:
		r.handBack(t)
	default:
		r.endLost(ctx, t)
	}
	t.cancel()
	close(t.stopped)
}

// endLost ends a term whose lease was lost: it unsubscribes and stops taking messages, drops what
// is queued while ctx is live and waits for the message in hand, and once ctx has ended (a
// shutdown) gives the message in hand inflightGrace, cancels its handling and NAKs what is queued.
// Either way the messages the term handed to the delivery paths end with it.
func (r *durableReceiver) endLost(ctx context.Context, t *receiveTerm) {
	r.mu.Lock()
	sub, conn := t.sub, t.conn
	t.sub = nil
	r.mu.Unlock()
	if sub != nil {
		_ = sub.Unsubscribe()
		_ = conn.FlushTimeout(time.Second)
	}
	t.close()
	if ctx.Err() == nil {
		dropped := len(t.takeQueued())
		r.logger.Info("listener lease lost; stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("dropped", dropped))
		select {
		case <-t.handled:
		case <-ctx.Done():
		}
		// The lease alone is gone: a pod that no longer holds it relays nothing more, and the
		// server gives the messages it held to the pod holding the lease now once their ack wait
		// runs out.
		return
	}
	// A shutdown: the message in hand gets its grace and its handling is cancelled, and what the
	// term holds is NAKed back to the durable at once.
	r.settleInHand(t)
	queued := t.takeQueued()
	for _, m := range queued {
		_ = m.Nak()
	}
	r.logger.Info("stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("naked", len(queued)))
}

// endWithoutLease ends a listener's receiving without a lease on a shutdown: the handler takes no
// more messages, the message in hand gets inflightGrace, the subscription drains, and everything
// queued is NAKed at once, in stream order, and flushed.
func (r *durableReceiver) endWithoutLease(t *receiveTerm) {
	t.stopHandling()
	r.settleInHand(t)
	conn := r.drain(t)
	t.close()
	queued := t.takeQueued()
	sortByStreamSequence(queued)
	for _, m := range queued {
		_ = m.Nak()
	}
	if conn != nil {
		_ = conn.FlushTimeout(time.Second)
	}
	r.logger.Info("stopped receiving the durable", slog.String("consumer", r.consumer), slog.Int("naked", len(queued)))
}

// handBack is a holder's graceful stop. The handler takes no more messages while the subscription
// stays and what arrives is queued; the message in hand gets inflightGrace. Then every message
// queued or held is NAKed with handBackDelay, as one batch in stream order, the lease's term is
// marked unsettled, so the next holder settles, and the NAKs are flushed before the lease is
// released. Then the subscription drains.
func (r *durableReceiver) handBack(t *receiveTerm) {
	t.stopHandling()
	r.settleInHand(t)
	t.handBack(r.timing.handBackDelay)
	r.lease.Unsettle(t.leaseTerm)
	r.mu.Lock()
	conn := t.conn
	r.mu.Unlock()
	_ = conn.FlushTimeout(time.Second)
	releaseCtx, cancel := context.WithTimeout(context.Background(), handBackReleaseWait)
	err := r.lease.Release(releaseCtx)
	cancel()
	if err != nil {
		r.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
	r.drain(t)
	t.close()
	handedBack, renaked, dropped := t.handBackCounts()
	fields := []slog.Attr{
		slog.String("consumer", r.consumer),
		slog.Int("handed_back", handedBack),
		slog.Int("renaked_repeats", renaked),
		slog.Int("dropped_repeats", dropped),
	}
	r.logger.Info("handed the durable's messages back to the next holder", fields...)
}

// drain ends the term's subscription as nats.go drains one: the server is told to stop sending,
// and what it sent before that still reaches intake. It waits handBackDrainWait at most, then
// unsubscribes, and returns the subscription's connection (nil when there was none).
func (r *durableReceiver) drain(t *receiveTerm) *nats.Conn {
	r.mu.Lock()
	sub, conn := t.sub, t.conn
	t.sub = nil
	r.mu.Unlock()
	if sub == nil {
		return nil
	}
	closed := sub.StatusChanged(nats.SubscriptionClosed)
	if sub.Drain() != nil {
		_ = sub.Unsubscribe()
		return conn
	}
	wait := time.NewTimer(handBackDrainWait)
	defer wait.Stop()
	select {
	case <-closed:
	case <-wait.C:
		_ = sub.Unsubscribe()
	}
	return conn
}

// settleInHand gives the message in hand inflightGrace to settle, then cancels its handling, which
// aborts a delivery in flight and NAKs the message, and waits inflightCancelWait more for the
// handler to return.
func (r *durableReceiver) settleInHand(t *receiveTerm) {
	grace := time.NewTimer(r.timing.inflightGrace)
	defer grace.Stop()
	select {
	case <-t.handled:
		return
	case <-grace.C:
	}
	r.logger.Info("cancelling the message in hand after the shutdown grace", slog.String("consumer", r.consumer), slog.String("grace", r.timing.inflightGrace.String()))
	t.cancel()
	wait := time.NewTimer(inflightCancelWait)
	defer wait.Stop()
	select {
	case <-t.handled:
	case <-wait.C:
		r.logger.Warn("the message in hand was still being handled after its cancel", slog.String("consumer", r.consumer))
	}
}
