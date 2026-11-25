package admission

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// The request pool's work outside admission. Admission hands out MaxAdmittedUnits of the request
// pool's store.SharedPoolSize connections; the rest are the work it does not admit, and three kinds
// of that work take a place of OutsideLimit before they read the pool, so they hold at most that
// share of it however many of them arrive at once:
//
//   - a broker-issued token's DPoP presentation, which the API decides before admission (api's
//     decideBrokerToken) and whose decision writes the proof's single-use row on the request pool
//     (store.DPoPProofs). Every such request waited on the pool's acquire queue before admission
//     could refuse it, with nothing bounding how many did. The broker is asked whether the token
//     is live first, holding no place, and the place is taken only for what reads the pool, the
//     proof's check against the answer the broker just gave and its row, so a broker slow to answer
//     holds none of the places. A decision refused a place is answered admission's own refusal for
//     the same reason, 429 OVERLOADED with Retry-After, and its proof is not spent;
//   - a socket's heartbeat check, where it reads the request pool (api's whileCallerResolves: the
//     sign-in cookie's session, a personal token's row). One runs every heartbeat for every open
//     socket, so a restart's reconnect lines them up: 991 waited on the pool at once in INFRA-592's
//     stress step (tester round 38). A check refused a place is skipped, and a skip counts toward
//     the heartbeat's ceiling as a transient failure does (api's heartbeatTransientCeiling). A
//     check that reads no pool - a broker-issued token's, which asks the broker alone - takes no
//     place and always runs;
//   - a document socket's admission reads, made before the socket is handed to ygo (docs'
//     Service.ServeHTTP): its caller's session, its document's issue and the live server
//     processes, the last of which one socket a second makes for every socket (docs'
//     fleetCacheTTL), so most sockets make two. A restart's reconnect sends every editor's socket
//     through them at once. They hold one place between them for those reads (Hold), taken before
//     the first and given back before the recovery wait and the document's load, neither of which
//     reads the request pool, and long before the hand-off, after which the handler holds its
//     request for the socket's whole life; a socket refused a place is refused as the document
//     service refuses a socket it cannot serve now, 503 with Retry-After.
//
// The pool's other users outside admission take none, each for a reason of its own: the live
// socket's upgrade and an events subscription's head check are one statement for each socket
// opened and each subscription started, where a socket's heartbeat checks every 15 s for as long as
// the socket lives; the /auth routes and the GitHub proxy are a person's sign-in and page loads,
// and the sign-in callback's code is single use; the going run's stream polls, and its run's
// writes, are one person's question, one statement at a time; a broadcast's deliveries run after
// their request's answer, four at a time for each broadcast (api's broadcastDeliveryWorkers); the
// dashboard's assets of other builds are read through a cache that keeps them, one read in flight
// for each. The test hooks never run in production.
//
// Work that finds every place held waits for one, first in first out, as a request waits in its
// class's queue and a heavy read in its lane's: in a queue of at most OutsideQueueLimit, for at most
// a second counted from when it asked (Limits.OutsideWait). It was refused at once before, and on
// INFRA-592's stress step (ten times the load, 1,000 editors, a restart) that refused 47,962 of the
// 48,580 refused document sockets, skipped 4,136 heartbeat checks and closed 223 sockets at the
// heartbeat's ceiling: the places were held by work waiting on a request pool the load had filled,
// and each burst - 1,000 editors coming back after the restart - found them held for a moment and
// was refused whole and sent again, so the handshakes per editor rose from 52.8 to 70.9. Work that
// finds the queue full, or whose second runs out, is refused as before (OutsideRefusal: the same
// QueueFull and WaitExceeded a class's queue refuses for), and work whose context ends while it
// waits leaves the queue. A place is held for the whole of the work that reads the pool: a
// decision's proof check and the row it writes, once the broker has answered, a check's whole
// check, and a socket's three admission reads - all but a wait on another server inside them: the
// sign-in issuer's renewal of a lapsed confirmation, and IAP's key set read where the caller's
// assertion names a key the set in memory does not hold, or the set is due to be read again. The
// work gives its place back for that wait and takes it again after, for the renewal's own reads
// and writes and for what it reads next (GiveBack, Away.TakeBack: the one mechanism that gives an
// admitted request's units back for the same waits), so an issuer slow to answer holds none of the
// places, as a broker slow to answer holds none.

// OutsideLimit is how many places the work outside admission may hold at once: the request
// pool's connections admission does not hand out, unadmittedConnections, 8 of the 48. Admission's
// own cap is the pool's size less those (MaxAdmittedUnits), so the 8 stay the 8 whatever the
// pool's size, and admission's 40 is what moves with it. The DPoP decisions, the heartbeat checks
// and the document sockets' admission reads together then hold no more of the pool than admission
// leaves, however long the queue for a place. Nothing bounded them before, and the 8 did not last:
// at ten times the load with bearer tokens alone the request pool's connections in use reached 48
// in all four of the head's steps, with up to 15 callers waiting (INFRA-592, tester round 38).
const OutsideLimit = unadmittedConnections

// OutsideQueueLimit is how much work may wait for a place at once: ReadBurstFactor times
// OutsideLimit, 176, the cap of every fifoGate at its own limit, as a heavy read's lane's is 22
// times its own. It bounds the memory the waiters take, a goroutine and its request each, and
// refuses nothing whose wait still has room: work that finds it full is refused at once
// (QueueFull), and work below it waits its second.
const OutsideQueueLimit = ReadBurstFactor * OutsideLimit

// OutsideUser is a kind of work that takes a place of OutsideLimit, which the refusals are counted
// by (OutsideStats.Refused).
type OutsideUser int

const (
	// DPoPDecision is the decision of a broker-issued token's DPoP presentation (api's
	// decideBrokerToken); one refused a place is answered 429 OVERLOADED.
	DPoPDecision OutsideUser = iota
	// HeartbeatCheck is a socket's heartbeat resolving its caller again where that reads the
	// request pool (api's whileCallerResolves); one refused a place is skipped.
	HeartbeatCheck
	// SocketAdmission is a document socket's admission reads, its caller's session, its issue and
	// the live server processes (docs' Service.ServeHTTP); one refused a place is answered 503
	// with Retry-After.
	SocketAdmission
	outsideUserCount
)

var outsideUserNames = [outsideUserCount]string{
	DPoPDecision: "dpop_decision", HeartbeatCheck: "heartbeat_check", SocketAdmission: "socket_admission",
}

func (u OutsideUser) String() string { return outsideUserNames[u] }

// OutsideUsers are the users in order, for a caller that reports on each of them (the metrics
// endpoint).
func OutsideUsers() []OutsideUser {
	return []OutsideUser{DPoPDecision, HeartbeatCheck, SocketAdmission}
}

// OutsideRefusal is work Outside gave no place: Reason is QueueFull, the queue for a place held
// OutsideQueueLimit already, or WaitExceeded, no place came free within the wait.
type OutsideRefusal struct {
	User   OutsideUser
	Reason Reason
}

func (r *OutsideRefusal) Error() string {
	return fmt.Sprintf("no place of the request pool's share outside admission for a %s: %s", r.User, r.Reason)
}

// Outside is a Limiter's places for the request-pool work outside admission (OutsideLimit), and
// the queue the work waits in for one: a fifoGate, as a heavy read's lane is, with its own wait
// and its refusals counted by user. A nil Outside limits nothing, so a server a test builds
// without a Limiter takes every place it asks for.
type Outside struct {
	// fifoGate's limit is OutsideLimit, set once by New.
	fifoGate
	// wait is how long work waits for a place (Limits.OutsideWait), set once by New.
	wait time.Duration

	refused [outsideUserCount]outsideRefusalCounts
}

// outsideRefusalCounts counts one user's refusals by reason.
type outsideRefusalCounts struct {
	queueFull, waitExceeded atomic.Uint64
}

// Take takes a place for user's work and answers nil once it holds one. Where every place is held
// it waits for one in the queue, behind every work that asked before it, for at most the wait,
// and answers an *OutsideRefusal - QueueFull at once where the queue is full, WaitExceeded where
// the wait runs out - or ctx's error where ctx ends first, holding no place and having left the
// queue either way. A caller that took a place gives it back with Release once its work has given
// its connection back.
func (o *Outside) Take(ctx context.Context, user OutsideUser) error {
	if o == nil {
		return nil
	}
	leftQueue, err := o.enter(ctx, func(ready <-chan struct{}) (bool, error) {
		timer := time.NewTimer(o.wait)
		defer timer.Stop()
		select {
		case <-ready:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, &OutsideRefusal{User: user, Reason: WaitExceeded}
		}
	})
	switch {
	case err == nil:
		return nil
	case err == errGateFull:
		o.refused[user].queueFull.Add(1)
		return &OutsideRefusal{User: user, Reason: QueueFull}
	}
	if _, refused := err.(*OutsideRefusal); refused && leftQueue {
		o.refused[user].waitExceeded.Add(1)
	}
	return err
}

// Release gives back a place Take took, handing it to the work at the head of the queue where any
// waits.
func (o *Outside) Release() {
	if o == nil {
		return
	}
	o.leave()
}

// Hold is Take for work that gives its place back at a point of its own and must give it back on
// every other way out as well: a document socket's admission reads, whose place goes back before
// the socket is handed to ygo, and as the handler returns where the socket was refused, and a
// heartbeat check. It answers the place, held, and Take's answer; where Take refused one it answers
// a nil Place, which holds nothing.
func (o *Outside) Hold(ctx context.Context, user OutsideUser) (*Place, error) {
	if err := o.Take(ctx, user); err != nil {
		return nil, err
	}
	return &Place{outside: o, user: user, held: true}, nil
}

// Place is a place of Outside one work holds (Hold), which the work gives back for a wait inside
// it on another server and takes again for what reads the pool after (GiveBack, Away.TakeBack). A
// document socket's admission and a heartbeat check resolve their caller, and the resolution may
// wait on an issuer - a cookie whose person's confirmation has lapsed is renewed with the sign-in
// issuer first (identity.Membership.Confirm), a round trip of up to 15 s, and an IAP assertion's
// verification may read IAP's key set (identity.IAPIdentity.Login), one of up to 10 s - that reads
// nothing of the pool: a place held across it would let an issuer slow to answer fill the share
// with no database work at all, as a place held across the broker's answer would (api's
// decidedBrokerToken). The work carries its place on its context (WithPlace) to the call that
// waits. One goroutine uses a Place at a time: the work's own, and the calls it makes in turn.
type Place struct {
	outside *Outside
	user    OutsideUser
	held    bool
	// takeBackRefusal is the refusal of the place asked back after a wait on the network
	// (Away.TakeBack), nil where none was refused (RefusedTakeBack).
	takeBackRefusal *OutsideRefusal
}

// RefusedTakeBack is the refusal of the place its work asked back after a wait on the network
// (Away.TakeBack), and nil where none was refused, and for a nil Place. A document socket answers
// it 429 OVERLOADED with Retry-After before its upgrade (WriteOutsideRefused,
// docs.Service.ServeHTTP); a heartbeat check has no answer to give and skips.
func (p *Place) RefusedTakeBack() *OutsideRefusal {
	if p == nil {
		return nil
	}
	return p.takeBackRefusal
}

// WriteOutsideRefused answers work Outside refused a place (an *OutsideRefusal) 429 OVERLOADED
// with Retry-After, as WriteRefused answers a request a Limiter refused: nothing of the work ran,
// so its client may send it again. A document socket refused the place it asked back after its
// caller's wait on an issuer answers so before its upgrade.
func WriteOutsideRefused(w http.ResponseWriter, refusal *OutsideRefusal) {
	writeRetryable(w, http.StatusTooManyRequests, CodeOverloaded, "the server has no room for a "+
		refusal.User.String()+" ("+string(refusal.Reason)+"); nothing ran, retry after Retry-After")
}

// Release gives the place back where it is held, handing it to the work at the head of the queue
// where any waits, and does nothing where it is not, so a caller may call it where its work ends
// and defer it too. A nil Place holds nothing.
func (p *Place) Release() { p.release() }

// release is Release, reporting whether it gave the place back: false for a nil Place, and for one
// not held (GiveBack).
func (p *Place) release() bool {
	if p == nil || !p.held {
		return false
	}
	p.held = false
	p.outside.Release()
	return true
}

// Retake takes the place again where it was given back, waiting for it as Take waits, behind every
// work that asked before it, and answers Take's answer. A place still held, and a nil Place, answer
// nil at once.
func (p *Place) Retake(ctx context.Context) error {
	if p == nil || p.held {
		return nil
	}
	if err := p.outside.Take(ctx, p.user); err != nil {
		return err
	}
	p.held = true
	return nil
}

// placeKey is the context key a Place is carried under (WithPlace).
type placeKey struct{}

// WithPlace is ctx carrying place to a call that gives it back while it waits on another server
// (PlaceFrom, GiveBack).
func WithPlace(ctx context.Context, place *Place) context.Context {
	return context.WithValue(ctx, placeKey{}, place)
}

// PlaceFrom is the place ctx carries (WithPlace), and nil where it carries none: a request inside
// admission holds a ticket (TicketFrom) and no place, and GiveBack gives back whichever of the two
// it finds.
func PlaceFrom(ctx context.Context) *Place {
	place, _ := ctx.Value(placeKey{}).(*Place)
	return place
}

// OutsideStats is what an Outside holds, queues and has refused.
type OutsideStats struct {
	// Limit is OutsideLimit, InUse the places held now, Queued the work waiting for one now and
	// MaxQueued the most that has waited at once since the Limiter was made.
	Limit, InUse, Queued, MaxQueued int
	// Refused counts, by user, the work refused a place since the Limiter was made: a DPoP
	// decision answered 429 OVERLOADED, a heartbeat check skipped, a document socket answered 503.
	Refused [outsideUserCount]OutsideRefusals
}

// OutsideRefusals is one user's refusals by reason.
type OutsideRefusals struct {
	QueueFull, WaitExceeded uint64
}

// Stats reads the counts: the places, the queue and its longest length together, and each
// refusal count on its own, so a scrape that races a refusal may see it in one count and not yet
// in another.
func (o *Outside) Stats() OutsideStats {
	used, queued, maxQueued := o.counts()
	stats := OutsideStats{Limit: o.limit, InUse: used, Queued: queued, MaxQueued: maxQueued}
	for user := range outsideUserCount {
		stats.Refused[user] = OutsideRefusals{
			QueueFull: o.refused[user].queueFull.Load(), WaitExceeded: o.refused[user].waitExceeded.Load(),
		}
	}
	return stats
}
