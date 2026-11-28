// serve's expiry pass, bound to the NATS connection it runs on (passKeptExpiriesWhileNATSAnswers),
// and the watcher that ends it at a connection event or a silence (watchExpiryPass); lease.go
// holds serve itself.

package outbox

import (
	"context"
	"log/slog"
	"time"
)

// passKeptExpiriesWhileNATSAnswers is serve's expiry pass (passKeptExpiries), run only while the
// NATS connection it starts on answers: it runs no pass while the client reads disconnected, it
// ends the pass it runs at the client's next connection event (Publisher.ConnectionChanged), and
// it ends that pass when a probe finds the connection silent. It takes that event's channel before
// it reads the client's state, so an event between the read and the pass still ends the pass.
//
// The pass reads NATS through Deps.KeptLanes and the role and session buckets and sends its keeps,
// all on the client cmd/dispatch publishes through, and a call made while that client reconnects
// waits for the reconnect up to the call's own bound: 2 s for a read (probeTimeout for one through
// Deps.KeptLanes, boundedKeptLanes; readTimeout in internal/roles for one of the buckets), 5 s for
// a keep (publishAcknowledgementTimeout in internal/bus). serve reads the client's state and arms
// natsLossGrace only between its passes, so a holder cut inside such a call would start to count
// the loss only once the call gave up, and release the hold that much later than natsLossGrace
// after the cut (TestAnOutboxCutFromNATSInItsExpiryPassReleasesThePublisher). Ended at the event,
// the pass returns at once, and serve counts the loss from then.
//
// A silence - a connection that stays open while nothing answers - sends no event, and the client
// still reads connected, so each call the pass makes into it waits out its bound, one call after
// another, and the holder would give the hold up only at the next batch's failed probe, past the
// take-over too (TestAnOutboxWhoseNATSGoesSilentInItsExpiryPassGivesThePublisherToTheOther). So
// the pass is watched (watchExpiryPass): a probe of the connection, as answering makes one, is due
// probeTimeout after the last probe that answered was sent, or after the pass began, and is made
// then. One is made as the pass returns, too, when the pass reports a failure or ran probeTimeout
// or longer: a call that meets a silence waits out its bound, at least probeTimeout, and so makes
// the pass run that long. A pass that returns sooner, reporting no failure, probes nothing, so a
// healthy holder pays one timer per pass. A probe that fails with no connection event since the
// pass began ends the pass and reports silent, with one Error line, and serve gives the hold up
// for NATS lost, as a batch whose sends failed and whose probe failed too does (publishBatch); a
// probe that fails once an event has come leaves the loss to that event, as above.
//
// So a silence that one of the pass's NATS calls meets is found by a probe: one made when due, at
// most probeTimeout after the silence, or the one made as the pass returns, which that call brings
// on; the probe fails probeTimeout after it was made. A read in flight then is bounded from its own
// start at probeTimeout. In production the end of the pass's context does not end its flush
// (boundedKeptLanes), so the pass returns, and the holder gives the hold up, at most about
// 3 x probeTimeout (6 s) after the silence. A silence that starts after the pass's last NATS call,
// while the pass does its database work, meets no call of the pass and may meet no probe of it: it
// is found as a silence between passes is, by the next batch's failed sends and their probe, or by
// the next pass. The helper waits for the watcher's answer once the pass has returned, unless
// serve's ctx ends: a stopping process waits for no probe.
//
// The pass ends there as it ends at a process's stop: what NATS accepted is recorded and what the
// pass did not decide is released (keepExpiredCopiesAgain), a notice's record is one transaction,
// and a read the end cut short is no failure to log (lostToContextEnd). It reports full and failed
// as passKeptExpiries does, and neither when it runs no pass: serve then reads the client's state
// again before it waits, and arms natsLossGrace. A publisher whose ConnectionChanged is nil, as a
// test's is, never ends a pass at an event, and one whose probe always answers, as a test's does
// (publishOnly), never ends one at a silence.
func (o *Outbox) passKeptExpiriesWhileNATSAnswers(ctx context.Context, hold fence) (full, failed, silent bool) {
	changed := o.deps.Publisher.ConnectionChanged()
	if !o.deps.Publisher.Connected() {
		return false, false, false
	}
	// The pass's own context: the connection event or a failed probe ends it, while ctx, which is
	// the process's, still tells a stop from an end of this pass's own making.
	pass, endPass := context.WithCancel(ctx)
	defer endPass()
	returned := make(chan bool, 1)
	watched := make(chan expiryPassEnd, 1)
	began := o.clock.Now()
	go func() { watched <- o.watchExpiryPass(ctx, changed, returned, endPass, began) }()
	full, failed = o.passKeptExpiries(pass, hold)
	returned <- failed
	end := expiryPassFinished
	select {
	case end = <-watched:
	case <-ctx.Done():
	}
	switch end {
	case expiryPassSilent:
		return full, failed, true
	case expiryPassConnectionChanged:
		slog.Info("dispatch outbox: the expiry pass ended at a change of its NATS connection; it runs again once the client is connected",
			"instance", o.instance)
	}
	return full, failed, false
}

// expiryPassEnd is why an expiry pass ended, as its watcher found it (watchExpiryPass).
type expiryPassEnd int

const (
	// expiryPassFinished: the pass returned of its own accord and no probe that was due failed, or
	// the process is stopping.
	expiryPassFinished expiryPassEnd = iota
	// expiryPassConnectionChanged: the client's connection changed before the pass returned, and
	// the watcher ended the pass there.
	expiryPassConnectionChanged
	// expiryPassSilent: a probe failed with no connection event since the pass began, and the
	// watcher ended the pass; the holder gives the publisher hold up for NATS lost.
	expiryPassSilent
)

// watchExpiryPass watches one expiry pass (passKeptExpiriesWhileNATSAnswers) and answers why it
// ended. changed is the client's connection-change channel, taken before the client's state was
// read; returned is sent, once, whether the pass reported failed, as the pass returns; endPass
// ends the pass's context; began is the moment before the pass began.
//
// A probe is due probeTimeout after the last probe that answered was sent, or after began, and a
// timer fires then. A probe runs on its own goroutine, with changed watched beside it, and the
// watcher waits for its answer even once the pass has returned: the probe decides. One that
// answers re-arms the timer, or answers finished once the pass has returned. One that fails once
// changed has closed - a cut or a reconnect, which the connection's own event decides - is
// answered as that event is, and one that fails otherwise ends the pass and answers silent, with
// one Error line. changed closing while the pass runs ends the pass and answers
// connectionChanged, and the pass's return wins over changed whenever both are there to read, so
// a pass that did its work is never reported as one the event ended: that is the one rule of a
// connection event (atConnectionEvent). The return answers finished at once unless the pass
// reported failed or ran probeTimeout or longer since began, when a probe is made first and
// decides. serve's ctx ending answers finished at once.
func (o *Outbox) watchExpiryPass(ctx context.Context, changed <-chan struct{}, returned <-chan bool, endPass context.CancelFunc, began time.Time) expiryPassEnd {
	// done is whether the pass has returned, and failed what it reported then.
	done, failed := false, false
	returnedNow := func() bool {
		if !done {
			select {
			case failed = <-returned:
				done = true
			default:
			}
		}
		return done
	}
	// atConnectionEvent is the one answer to a change of the client's connection: finished when the
	// pass has returned, since it did its work, and otherwise the pass is ended there and the
	// answer is connectionChanged.
	atConnectionEvent := func() expiryPassEnd {
		if returnedNow() {
			return expiryPassFinished
		}
		endPass()
		return expiryPassConnectionChanged
	}
	due := o.clock.NewTimer(began.Add(probeTimeout).Sub(o.clock.Now()))
	defer due.Stop()
	for {
		select {
		case <-ctx.Done():
			return expiryPassFinished
		case failed = <-returned:
			done = true
		case <-changed:
			return atConnectionEvent()
		case <-due.C():
		}
		switch {
		case !done:
		case connectionReplaced(changed):
			return atConnectionEvent()
		case !failed && o.clock.Now().Sub(began) < probeTimeout:
			return expiryPassFinished
		}
		// A probe left behind, at a stop or a connection event, answers into its buffer.
		sent := o.clock.Now()
		answer := make(chan error, 1)
		go func() { answer <- o.deps.Publisher.WindowUntil(nil).Probe(probeTimeout) }()
		var err error
		select {
		case <-ctx.Done():
			return expiryPassFinished
		case <-changed:
			return atConnectionEvent()
		case err = <-answer:
		}
		switch {
		case err == nil && returnedNow():
			return expiryPassFinished
		case err == nil:
			due.Reset(sent.Add(probeTimeout).Sub(o.clock.Now()))
			continue
		case connectionReplaced(changed):
			return atConnectionEvent()
		case ctx.Err() != nil:
			return expiryPassFinished
		}
		slog.Error("dispatch outbox: the expiry pass's NATS connection does not answer, so the holder gives the publisher hold up",
			"instance", o.instance, "error", err)
		endPass()
		return expiryPassSilent
	}
}
