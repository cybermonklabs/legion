package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// No admission unit, outside place or pinned request-pool connection is held across a wait on
// another server - the Envoy listener, NATS, GitHub, the embedder, the secrets broker - or across
// a wait on another goroutine's such call. A unit held across one lets a server slow to answer
// fill a class with requests doing no work, and every request that waits on nothing is refused
// 429 OVERLOADED until it answers. awayOnNetwork is the one way a request lets go for such a wait,
// as a caller's resolution does around an issuer (identity.Membership.Confirm,
// serviceTokenActor): admission.GiveBack before the wait and admission.Away.TakeBack after it, on
// the request's own goroutine, since only that goroutine may park or resume its ticket
// (admission.Ticket.Waited). A goroutine of its own that waits - a fetch, a stage, a lane - makes
// no pair: the request waiting on it does.
//
// A write holds no database connection or transaction across such a wait either: where it
// interleaves them, its outbound reads come first and its writes after, in one transaction. A
// refused take-back answers 429 only while nothing outside the request's own work has changed: a
// wait that changes another server's state - the listener's Unsubscribe and Send, a publish on
// the message bus, a broker's decision - runs after the request's database work, and the
// take-back after it, where database work must still follow, is never refused
// (awayOnNetworkNeverRefused), nor is one after the request committed writes of its own. A
// request whose last work is such a call takes nothing back at all (awayOnNetworkForGood). Work
// that outlives the request - a broadcast's delivery workers, a comment's mention lanes - runs on
// a context that carries no ticket (detachedFromRequest), so nothing parks or resumes a ticket off
// the request's own goroutine.

// awayOnNetwork runs wait, a wait on another server, holding nothing of the server's room: it
// gives ctx's request's unit back (admission.GiveBack, which lets go of its pinned connection
// too), runs wait on a context of its own connection mark (store.ForConcurrentRead), so a
// statement the wait makes - the GitHub client's read of the rate-limit ledger - takes a
// connection for that one statement and gives it back rather than pinning one again for the
// whole wait, and takes the unit back once wait has returned (admission.Away.TakeBack). ctx is
// the request's own context, never one a bound of the wait's derives from it: the take-back waits
// on ctx, and one a passed bound had ended would leave the request running without its unit.
//
// It answers the take-back's error ahead of wait's own: a take-back refused (admission's
// *Refusal, which the ticket records, so nothing of the request runs past it) is the request's
// answer, which reaches writeHandlerError and is answered 429 OVERLOADED; a caller that reads
// every other error of the wait as "unavailable" or "unreadable" reads that one first
// (refusedTakeBack). A request refused already is answered that refusal without waiting again. A
// request that holds a transaction, a cursor or a connection of its own must end it before it
// waits, since a held connection whose unit was given back breaks admission's count: the wait is
// not run and the request is answered the pool's ErrNestedAcquire.
func awayOnNetwork(ctx context.Context, wait func(context.Context) error) error {
	if refused := refusedTakeBack(ctx); refused != nil {
		return refused
	}
	return awayThen(ctx, wait, admission.Away.TakeBack)
}

// awayOnNetworkFor is awayOnNetwork for a wait that answers a value beside its error.
func awayOnNetworkFor[T any](ctx context.Context, wait func(context.Context) (T, error)) (T, error) {
	var value T
	err := awayOnNetwork(ctx, func(ctx context.Context) (err error) {
		value, err = wait(ctx)
		return err
	})
	return value, err
}

// awayOnNetworkNeverRefused is awayOnNetwork for a wait after which the request must not be
// refused: a call that changes another server's state - the listener's Send or Unsubscribe, a
// publish on the message bus - with database work of the request's still to follow it, and any
// wait of a request that has committed writes of its own before it (a message's send after the
// message committed, a write's related-item check after its write). 429 OVERLOADED then would
// tell the client nothing ran when something did, and its repeat would make it twice, a keyed
// repeat's send among it. It gives back as awayOnNetwork does and takes back with
// admission.Away.TakeBackNeverRefused, which waits for the unit, within ctx and past the queue's
// bound, for as long as it takes, and never refuses: so it never answers 429. Its own error, ahead
// of wait's, is only ctx's, where ctx ended first: the request then holds no unit, and every
// statement it makes after on that ended context fails. ctx is the request's own context, which
// the take-back waits on, as awayOnNetwork's is; a request that holds a connection is answered
// ErrNestedAcquire without the wait, as there. Run inside a wait that has given the unit back
// already - a stretch of a take-over loop (takeoverRetry.retry) around a send or a resolution that
// makes its own pair - it gives nothing back and takes nothing.
func awayOnNetworkNeverRefused(ctx context.Context, wait func(context.Context) error) error {
	return awayThen(ctx, wait, admission.Away.TakeBackNeverRefused)
}

// awayThen is the body of awayOnNetwork and awayOnNetworkNeverRefused: it gives ctx's request's
// unit back, runs wait on a context of its own connection mark, and takes the unit back with
// takeBack, answering takeBack's error ahead of wait's.
func awayThen(ctx context.Context, wait func(context.Context) error, takeBack func(admission.Away, context.Context) error) error {
	away := admission.GiveBack(ctx)
	waiting, err := store.ForConcurrentRead(ctx)
	if err == nil {
		err = wait(waiting)
	}
	if back := takeBack(away, ctx); back != nil {
		return back
	}
	return err
}

// networkWait is the shape of a network wait a caller hands on: awayOnNetwork,
// awayOnNetworkNeverRefused, givenBackAlready, or a function that hands its wait on to one.
type networkWait func(ctx context.Context, wait func(context.Context) error) error

// awayOnNetworkForGood runs call, a request's last work: a call that changes another server's
// state - the secrets broker's approve, deny, dismissal or revoke - after which the request reads
// and writes nothing of Dispatch's own and only relays the answer. It gives ctx's request's unit
// back for good (admission.GiveBack) and takes nothing back, so nothing is left to refuse, and
// runs call on a context of its own connection mark (store.ForConcurrentRead), answering call's
// error. A request that holds a connection is answered ErrNestedAcquire without the call, as
// awayOnNetwork answers it. Nothing of the request may touch the database after it: its unit is
// gone, and a statement made then would run where admission counts nothing for it.
func awayOnNetworkForGood(ctx context.Context, call func(context.Context) error) error {
	admission.GiveBack(ctx)
	waiting, err := store.ForConcurrentRead(ctx)
	if err != nil {
		return err
	}
	return call(waiting)
}

// refusedTakeBack is the refusal ctx's request met taking its unit back after a wait on another
// server (awayOnNetwork), and nil where it met none.
func refusedTakeBack(ctx context.Context) error {
	if refusal := admission.TicketFrom(ctx).RefusedTakeBack(); refusal != nil {
		return refusal
	}
	return nil
}

// detachedFromRequest is ctx for work that outlives its request and runs off the request's own
// goroutine - a broadcast's delivery workers, the mention sender's lanes and the claims they give
// back: ctx's values without its cancellation (context.WithoutCancel), and without its admission
// ticket (admission.WithTicket of nil). Only the request's own goroutine may park or resume its
// ticket (admission.Ticket.Waited), and such work waits on the listener through the same pairs
// the request's own sends make (awayOnNetworkNeverRefused): carrying the ticket, one of them could
// give the request's unit back while the request still runs, or refuse a request that has
// answered. With none, every give-back the work makes gives nothing and every take-back takes
// nothing, as for the delivery sweep, which runs from no request at all.
func detachedFromRequest(ctx context.Context) context.Context {
	return admission.WithTicket(context.WithoutCancel(ctx), nil)
}
