// Package admission decides whether the server has room for a request before the request takes
// any, and answers the requests it could not serve: refused here with a retryable 429, out of
// time or out of database connections with a retryable 503, rather than with an error. It also
// answers the one failure that is neither served nor safe to repeat - a call the database never
// answered that may have committed - with a 500 naming that unknown outcome (WriteUnserved).
// Without it every request queued, with no bound, for one pool of connections: past capacity the
// queue grew until clients gave up, and almost nothing succeeded. A Limiter admits a request only
// while its class has room and lets one that finds none wait for room a bounded time: it refuses
// the request only when that wait runs out, or at once where the queue already holds the cap that
// bounds its memory, so the requests it admits finish and the rest are told to retry.
package admission

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Class is the kind of request a Limiter counts separately: reads and writes each have a limit
// of their own, so writers parked on a lock - the commit order makes writes wait on each other -
// can never take every connection from the readers, nor a flood of reads every one from writes.
type Class int

const (
	// Read is a GET or HEAD.
	Read Class = iota
	// Write is every other method.
	Write
	classCount
)

// Reads reports whether a request with method only reads: it runs no statement that can commit,
// so a call of its own the database never answered left nothing behind and the client may send
// it again. It is the one place the server decides a method that question, for the class a
// request is counted under (ClassOf), for the layers a route is wrapped in (api.routeHandler), and
// for the answer a call whose outcome nobody knows is given (WriteUnserved).
//
// A GET is one, and so is a HEAD: net/http's mux serves a HEAD with the route's GET handler, so
// it runs that read's statements and writes nothing. Every other method the server serves is a
// write. OPTIONS is not among them: no route of the table is mounted for it, and net/http
// answers it 405 without reaching a handler, so no handler's error is ever a request of its.
//
// A handler that writes on a read route decides for itself, rather than by its method: the
// sign-in callback is the one of them, and the routes package names it where it answers
// (routes.writeUnservedOr500).
func Reads(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// ClassOf is the class of a request with method.
func ClassOf(method string) Class {
	if Reads(method) {
		return Read
	}
	return Write
}

func (c Class) String() string {
	if c == Read {
		return "read"
	}
	return "write"
}

// Reason is why a Limiter refused a request.
type Reason string

const (
	// QueueFull is a request that arrived with its wait queue already at its cap, the bound on
	// the memory its waiters take: a class's burst cap (ReadBurstFactor, WriteBurstFactor), and
	// ReadBurstFactor times the limit for a lane's queue and the outside places' (fifoGate).
	QueueFull Reason = "queue_full"
	// WaitExceeded is a request that waited in the queue for its class's wait
	// (Limits.ReadWait, Limits.WriteWait) without room coming free.
	WaitExceeded Reason = "wait_exceeded"
)

// Refusal is a request a Limiter did not admit. Nothing of the request ran, so its client may
// retry it whatever its method.
type Refusal struct {
	Class Class
	// Lane is the heavy read lane that refused the request (EnterLane), NoLane for a refusal by
	// the request's class.
	Lane   Lane
	Reason Reason
}

func (r *Refusal) Error() string {
	if r.Lane != NoLane {
		return fmt.Sprintf("admission refused a %s request of the %s lane: %s", r.Class, r.Lane, r.Reason)
	}
	return fmt.Sprintf("admission refused a %s request: %s", r.Class, r.Reason)
}

// The limits. The request pool (store.SharedPoolSize) holds 48 connections, and an admitted
// request holds at most one of them at a time, or Weight of them for a route that reads side by
// side. unadmittedConnections of the 48 are left to the pool's users that admission does not
// count, which outside.go lists: the DPoP decisions, the sockets' heartbeat checks and the
// document sockets' admission reads among them take a place of that share before they read the
// pool (OutsideLimit, Limiter.Outside), so however many of them arrive at once they hold no more
// of it. The other 40 are all admission may hand out at once, whatever the mix of classes:
// MaxAdmittedUnits, the pool's own size less those, so a pool that shrinks takes this cap down
// with it and every unit admission hands out stays one the pool can serve. Under that combined
// cap writes, which wait on each other at the commit order, hold at most 24 (MaxWriteLimit) and
// reads at most 28 (MaxReadLimit): the two add to 52, so a class reaches its own cap only while
// the other is quiet.
//
// Each class's limit follows the CPUs the process may use (runtime.GOMAXPROCS, which follows the
// container's CPU limit) up to its cap: readsPerCPU reads and writesPerCPU writes per CPU, at least
// one CPU's worth. Two CPUs, the production task's, already reach both caps, 28 reads and 24 writes,
// and so does every larger pod: at 16 CPUs the limits are still 28 and 24, because the request
// pool's 48 connections bound what admission may let in, so a larger pod's limit comes from the
// pool, not its cores. One CPU gets 14 reads and 12 writes.
//
// INFRA-589's reproduction on 2 CPUs chose them, with INFRA-591's commit-order change in, each step
// offered with the same seed (1x is 4 writes/s and 60 reads/s for 180 s, 10x is 40 and 600), as
// committed events/s at 10x and requests refused at 1x (reads / writes):
//
//	reads  writes  wait    queue     10x commits  1x refused
//	8      8       75 ms   1x limit  4.67         629 / 34
//	16     8       75 ms   1x limit  3.01         889 / 118
//	28     8       75 ms   1x limit  2.47         454 / 111
//	8      8       500 ms  1x limit  4.57         384 / 12
//	8      8       1 s     1x limit  4.91         638 / 6
//	8      8       1 s     8x limit  5.32         724 / 30
//	16     8       1 s     2x limit               112 / 25
//	28     8       1 s     2x limit  2.81         0 / 2
//	28     12      1 s     2x limit  2.92         0 / 5, and 9 / 11 on a second run
//
// The combined cap is what keeps the two class caps from oversubscribing the request pool: 28
// reads and 24 writes are 52 units against the 40 the pool has for admitted requests. Capped by
// class alone, a 10x step is not refused here at all and queues on the pool instead - 29 waiters
// measured - where each request ends as a 503 DATABASE_BUSY after the 3 s acquire bound rather
// than as the 429 a refusal answers at once. The combined cap puts that refusal back here: a
// class may take its whole cap only while the other is quiet, and every unit admission hands out
// is one the pool can serve.
//
// Why writes 24: edits, issue creates and ask creates weigh 2 (their related-item check reads
// twice, side by side), and at 1x on the same reproduction writes in flight reach 19-24 units. In
// adjacent pairs on one pod (the same seed and offer), 28 reads and 12 writes refused at 1x 4, 32
// and 0 writes and 0, 320 and 0 reads in three runs; 28 reads, 24 writes and a combined 40 refused
// nothing in all three. At 10x the same pair refused only with 429 and dropped no connection:
// 12 writes committed 4.09 events/s and served 14.2% of the requests, 24 writes under the combined
// 40 committed 8.34 and served 8.7%, since writes then hold 24 of the 40 units and reads the rest;
// the request pool's waiters peaked at 1 against the 29 that 24 writes without the combined cap
// left on it.
//
// Of the 40, reads keep ReservedReadUnits (22) while they are waiting for room: a write is
// admitted only while the units it takes, beside the reserved read units the reads have not
// taken yet, are within the 40, so under read pressure writes hold at most 40 - 22 = 18 and the
// reads reach their 22. The room is held back only while a read is queued for it, so with the
// reads light nothing is reserved and writes still reach their whole cap of 24, which 1x needs:
// its write units in flight reach 19-24, and the 28 reads and 24 writes above refused nothing
// of it in three runs.
//
// What it is for: at 10x the writes took 24 of the 40 units and the reads, left about 16, were
// served about 50/s, below the 60/s the 1x step offers them. Holding the writes to 18 leaves
// the reads the 22 units that rate asks for, and the writes half again the 12 the base ran with.
//
// Measured in adjacent pairs on one pod against the base (28 reads, 12 writes, no combined cap):
// at 10x the reservation served 65 reads/s (base 86, 1x offers 60) and committed 6.02 events/s
// (base 4.12), with reads holding at most 22 units and writes 18, refusing only with 429; at 1x it
// refused 0, 1 and 3 writes and 0, 0 and 17 reads in three runs (base 1, 0, 1 and 0, 0, 0), the 17
// in one 1.5-second stall that slowed every issue read at once.
const (
	MaxReadLimit  = 28
	MaxWriteLimit = 24
	// unadmittedConnections is how many of the request pool's connections are left to the users
	// admission does not count, the ones outside.go lists, and so OutsideLimit.
	unadmittedConnections = 8
	MaxAdmittedUnits      = store.SharedPoolSize - unadmittedConnections
	ReservedReadUnits     = 22
	readsPerCPU           = 14
	writesPerCPU          = 12
)

// readLimit and writeLimit are the limits for procs usable CPUs: perCPU each, at least one CPU's
// worth and at most the class's cap.
func readLimit(procs int) int  { return perCPULimit(procs, readsPerCPU, MaxReadLimit) }
func writeLimit(procs int) int { return perCPULimit(procs, writesPerCPU, MaxWriteLimit) }

func perCPULimit(procs, perCPU, ceiling int) int {
	return min(ceiling, perCPU*max(procs, 1))
}

// DefaultReadWait is how long a read waits in the read queue for room before it is refused: 1 s.
// At 1x on INFRA-589's 2-core reproduction every read slot stays busy for over a second in a
// stall (the measurements above), so a shorter wait refuses reads whose slot is about to come
// free, at a load the server serves: with 28 reads, 12 writes and the queue then held to twice the
// limit a 1 s wait refused 0-9 reads in 180 s. A refused request is answered once its wait has run
// out, so a refusal's p99 is about the wait.
const DefaultReadWait = time.Second

// DefaultWriteWait is how long a write waits in the write queue for room before it is refused:
// 2 s, twice a read's. At 1x on the same reproduction the seeded open-loop schedule (rng seed 1)
// puts about 20 writes into the 3 seconds around the window start, five times the mean, while the
// server is still loading cold rooms and settling the documents its boot resumed. For about 5 s
// both classes are at their caps - reads 22 in flight and 19 queued, writes 18-21 in flight and 2
// queued - and a write takes 2-6 s, so a 1 s wait refuses 0-2 of those writes 429 OVERLOADED
// (one measured run refused 2, three others 1, 0 and 0) where the server goes on to serve them.
// A shorter wait refuses more of them: with 8 reads a 75 ms wait refused 34 writes in 180 s at
// 1x where a 1 s wait refused 6, and with 28 reads, 12 writes and the queue then held to twice the
// limit a 1 s wait refused 5-11 writes. A refused write is sent again after Retry-After: 1, so
// from the client's side the second second of waiting costs nothing that a refusal would save it,
// and the write is served rather than retried. Reads keep the 1 s: a read's slot comes free within
// it, and a read's work is the shorter of the two to redo.
const DefaultWriteWait = 2 * time.Second

// ReadBurstFactor and WriteBurstFactor are how many requests per unit of its limit a class's
// queue holds: its burst cap, the queue's one bound. A request that finds no room waits in the
// queue for what is left of its one wait and is refused only when that wait runs out
// (WaitExceeded), or at once where the queue already holds its cap (QueueFull). The cap is a
// bound on memory rather than a refusal policy: it keeps a flood within the memory its waiters
// take - a read's goroutine and request, 616 of them at the read limit of 28, and a write's as
// well as its body, of which a queued write holds up to a MiB in memory (api's
// spooledBodyInMemory), so the 96 writes the cap holds at the write limit of 24 hold at most
// 96 MiB of bodies. The heavy read lanes and the request pool's places outside admission hold
// ReadBurstFactor times their own limit, for the same reason (fifoGate).
//
// Each is what arrives within one wait of its class at the load test's 10x on the cluster copy,
// 600 reads and 40 writes a second (c-scale, 10 times 1x's 60 and 4; INFRA-592, test handoff v39,
// committed 23,931 events in 600 s): 600 reads a second over a read's 1 s wait is 600 reads, and
// 22 times the read limit of 28 is 616; 40 writes a second over a write's 2 s wait is 80 writes,
// and 4 times the write limit of 24 is 96. They are factors of the limit rather than counts
// because what a server serves within one wait is its limit's worth of requests over and over: a
// pod with half the CPUs has half the limit (readsPerCPU, writesPerCPU), serves half the burst in
// that wait and holds half the queue for it.
//
// Nothing below the cap refuses a request whose wait still has room: a place that frees within
// the wait is no reason to refuse (the architect, INFRA-592, 2026-11-25 01:08Z). Such bounds stood
// here before - a class's queue that had stood non-empty for one whole wait refused at once past
// twice its limit, 56 reads, and a lane's queue, and the outside places', past twice its limit at
// any time, 14 at a lane of 7 - and the c-scale run at eb23d38e (run lt-20261124t150237-3446)
// showed what they cost. Of step head-j-10x-5's 150 refusals, 121 came at once from the read
// class's standing bound while its queue drained within its 1 s wait - one sample held 97
// waiters, draining, while arrivals were refused - and 29 at once from the lanes' fixed queue of
// 14. The run's one refused event write, in step head-j-10x-1, was an upload whose first read met
// the standing bound, and main, which has no limiter, served all of them late. Held to twice the
// limit at all times before that, the read queue refused bursts of 85 and 129 reads during a
// 0.4-0.7 s stall that main served late (INFRA-592, test handoff v39, c-scale at c605071b).
//
// What a queue serves first is its own state's (gate.next, fifoGate.next): adaptive LIFO, which the
// architect ruled for every class queue, every lane and the outside places (INFRA-592, 2026-11-25
// 10:10Z; Maurer, "Fail at Scale", ACM Queue 13(8), 2015). A queue that has emptied within its last
// wait is absorbing a burst it can serve, and serves it first come, first served, so the burst
// keeps its order. One that has stood non-empty for a whole wait is sustained overload, where first
// come, first served makes every request it serves wait out the queue first: with nothing below the
// cap refused (f51a56de), the bed's standing ten times the load answered its 2xx at a p50 of
// 1,067-1,085 ms, against 252-277 ms before. So a queue that stands hands each freed place to its
// newest waiter until it empties, and the waiters it leaves are its oldest, refused at their own
// wait as every waiter is and never before it: it serves as many as it did, the ones that have
// waited least, and refuses the ones that have waited longest.
const (
	ReadBurstFactor  = 22
	WriteBurstFactor = 4
)

// Limits are a Limiter's concurrency limits in units (a request takes its route's weight): one
// per class, one over the two together, how much of that one is kept for reads, and how long a
// request of each class may wait for room.
type Limits struct {
	Read, Write int
	// Total is the units both classes may hold together. Zero is no combined cap beyond the
	// classes' own, Read plus Write, so a caller that raises one class - a test that wants
	// sixteen writes at once - gets the room it asked for without naming a total;
	// DefaultLimits sets MaxAdmittedUnits. A Total below a class's limit is allowed: that
	// class then reaches its own limit only while the other class is empty, and never past
	// the total.
	Total int
	// ReadWait is how long a read may wait in the read queue for room; zero is DefaultReadWait.
	ReadWait time.Duration
	// WriteWait is how long a write may wait in the write queue for room; zero is
	// DefaultWriteWait. A write waits longer than a read because a refused write costs its
	// client a whole retry of work the server was about to serve (DefaultWriteWait).
	WriteWait time.Duration
	// ReservedReads is how many of Total's units writes leave to the reads while a read is
	// waiting for room: a write is admitted only while its units, beside the reserved units
	// the reads have not taken yet, are within Total, so the reads reach the reservation
	// under write pressure and the writes hold at most Total minus it. Zero is no
	// reservation; DefaultLimits sets ReservedReadUnits. New clamps it to the read limit and
	// to Total.
	ReservedReads int
	// HeavyReadLane is how many requests of one heavy read route may run at once
	// (heavyReadLaneLimit, lane.go: one a processor, at least MinHeavyReadLane and at most
	// MaxHeavyReadLane); zero and below are one. The lanes are the same width: each of them is
	// one route's hold on the database, and the measurement that sized them sized them together.
	HeavyReadLane int
	// OutsideWait is how long the request-pool work outside admission waits for a place, counted
	// from when it asks (Outside.Take); zero and below are DefaultReadWait, the one second a read
	// waits for room, which every server runs with. A test sets a shorter one.
	OutsideWait time.Duration
}

// DefaultLimits are the limits the server runs with: reads and writes by the CPUs the process may
// use (readLimit and writeLimit of runtime.GOMAXPROCS), MaxAdmittedUnits over the two of them,
// ReservedReadUnits of those kept for the reads, DefaultReadWait for a read and
// DefaultWriteWait for a write.
func DefaultLimits() Limits {
	procs := runtime.GOMAXPROCS(0)
	return Limits{
		Read: readLimit(procs), Write: writeLimit(procs), Total: MaxAdmittedUnits,
		ReservedReads: ReservedReadUnits, ReadWait: DefaultReadWait, WriteWait: DefaultWriteWait,
		HeavyReadLane: heavyReadLaneLimit(procs),
	}
}

// Limiter admits requests per class under Limits. A read is admitted while its class and both
// classes together are under their limits; a write while its class is and its units fit under
// Limits.Total beside the read units Limits.ReservedReads still keeps for waiting reads. One
// that finds no room waits in its class's queue - served in the order the requests arrived
// (Arrive) while the queue absorbs a burst, and newest first once it has stood a whole wait
// (gate.next) - for at most its class's wait over the whole request - Limits.ReadWait for a read,
// Limits.WriteWait for a write - and is refused when that wait runs out, or at once where the
// queue already holds its burst cap, the bound on its memory (ReadBurstFactor says why). A Limiter
// is safe for concurrent use.
type Limiter struct {
	// arrived counts the arrivals so far, which is each one's place in the arrival order (Arrive):
	// the order a queue absorbing a burst serves in, and the one the two classes' candidates are
	// granted in (grant). It is atomic rather than under mu, so an arrival takes no lock.
	arrived atomic.Uint64
	// now is the clock the queues' standing decision reads (gate.next, fifoGate.next), and nothing
	// else reads it: time.Now, or a test's (newWithClock), which stands a queue without a real wait.
	now func() time.Time
	// mu guards every count and queue below: the combined cap is one number over both
	// classes, so granting reads and moves the two together.
	mu      sync.Mutex
	used    int // the units held across the classes
	classes [classCount]*gate
	// lanes are the heavy read routes' own limits, each with a queue and a mutex of its own: a
	// request takes its lane before it asks its class for units (EnterLane, lane.go).
	lanes [laneCount]*laneGate

	total         int // the combined cap and the read reservation under it, both set once by New
	reservedReads int

	// outside is the request pool's share admission does not hand out (OutsideLimit), which the
	// work outside admission takes places of (outside.go).
	outside Outside
}

// Outside is the Limiter's places for the request-pool work outside admission: one for each
// Limiter, as the request pool both share is one for each process.
func (l *Limiter) Outside() *Outside { return &l.outside }

// New returns a Limiter with limits. A limit below one is one, a Total of zero is Read plus
// Write (no combined cap beyond the classes' own), a zero wait is its class's default
// (DefaultReadWait, DefaultWriteWait), and a ReservedReads is kept within zero and the smaller of
// the read limit and the total, which is all the reads could ever hold.
func New(limits Limits) *Limiter { return newWithClock(limits, time.Now) }

// newWithClock is New with now as the clock the queues' standing decision reads (Limiter.now).
func newWithClock(limits Limits, now func() time.Time) *Limiter {
	l := &Limiter{now: now}
	l.outside.limit = OutsideLimit
	l.outside.wait = limits.OutsideWait
	if l.outside.wait <= 0 {
		l.outside.wait = DefaultReadWait
	}
	l.outside.now = now
	waits := [classCount]time.Duration{Read: limits.ReadWait, Write: limits.WriteWait}
	defaults := [classCount]time.Duration{Read: DefaultReadWait, Write: DefaultWriteWait}
	burstFactors := [classCount]int{Read: ReadBurstFactor, Write: WriteBurstFactor}
	for class, limit := range [classCount]int{Read: limits.Read, Write: limits.Write} {
		wait := waits[class]
		if wait <= 0 {
			wait = defaults[class]
		}
		limit = max(limit, 1)
		l.classes[class] = &gate{class: Class(class), limit: limit, wait: wait, burstCap: burstFactors[class] * limit}
		l.total += limit
	}
	if limits.Total > 0 {
		l.total = limits.Total
	}
	for _, lane := range HeavyReadLanes() {
		l.lanes[lane] = &laneGate{fifoGate: fifoGate{limit: max(limits.HeavyReadLane, 1), wait: l.classes[Read].wait, now: now}, lane: lane}
	}
	l.reservedReads = min(max(limits.ReservedReads, 0), l.classes[Read].limit, l.total)
	return l
}

// Arrive gives the request arriving now its place in the order requests reach the Limiter, with
// the class and weight it will ask room for: a weight below one is one, and one above the
// class's limit or above the combined cap is that cap, so a heavy route can still run alone.
//
// A request that must do something before it asks for room - a write reading its body off the
// connection, which it does holding no unit (the API's drain) - takes its place here first, and a
// queue absorbing a burst then serves it where it arrived, ahead of everything that arrived while
// it read, rather than behind it; a queue that stands serves it as the newest waiter it is
// (gate.next).
func (l *Limiter) Arrive(class Class, weight int) *Arrival {
	return &Arrival{
		limiter: l, class: class, seq: l.arrived.Add(1),
		units: min(max(weight, 1), l.classes[class].limit, l.total),
	}
}

// Arrival is the place one request holds in a Limiter's arrival order, with the class and units
// it asks for and how long it has waited for them so far. A request waits more than once when
// it waits for a heavy read's lane before it asks for units (EnterLane), and when it gives its
// units back to wait for something else (Ticket.Park), waits for it, and takes them back after
// (Ticket.Resume): each ask joins its queue with the place it arrived at - where a queue absorbing
// a burst serves it, and a queue that stands serves it as its newest waiter (gate.next) - each wait
// outside admission is counted back by its holder (Ticket.Waited), and its class's wait is its wait
// over all of them, so everything it waits for costs it one wait between them.
type Arrival struct {
	limiter *Limiter
	class   Class
	units   int
	seq     uint64
	// waited is how long the request has spent waiting so far: in its lane (EnterLane), in
	// its class's queue, and parked for something outside admission (Ticket.Waited). Only the
	// request's own goroutine touches it: a request waits for one thing at a time.
	waited time.Duration
	// ticket is the hold the arrival is admitted with (Admit, AdmitNow): a field of the arrival
	// rather than an allocation of its own, since an arrival is admitted once.
	ticket Ticket
}

// admitted is the arrival's ticket, holding the units the arrival has just been given.
func (a *Arrival) admitted() *Ticket {
	a.ticket.arrival, a.ticket.held = a, true
	return &a.ticket
}

// Admit takes the arrival's units, waiting in its class's queue (gate.next says in what order) for
// what is left of its class's wait. It returns a *Refusal when the queue is full or the wait
// runs out, and ctx's error when ctx ends first. The caller releases the Ticket once it is done.
// An arrival is admitted once, by Admit or AdmitNow.
func (a *Arrival) Admit(ctx context.Context) (*Ticket, error) {
	if err := a.limiter.acquire(ctx, a, true); err != nil {
		return nil, err
	}
	return a.admitted(), nil
}

// AdmitNow takes the arrival's units while the Limiter has room for them and nobody is queued
// for its class, and answers nil where the request would have to wait for them - which is where
// its caller has something to do before it waits, a write its body to read off the connection.
func (a *Arrival) AdmitNow() *Ticket {
	if !a.limiter.acquireNow(a) {
		return nil
	}
	return a.admitted()
}

// Admit takes weight units of class for a request arriving now, waiting in the class's queue
// when it has no room: Arrive and Arrival.Admit in one, for a caller with nothing to do between
// the two.
func (l *Limiter) Admit(ctx context.Context, class Class, weight int) (*Ticket, error) {
	return l.Arrive(class, weight).Admit(ctx)
}

// ClassStats is what one class of a Limiter holds and has refused.
type ClassStats struct {
	Class Class
	// Limit is the class's limit in units; InFlight the units admitted requests hold now, and
	// Queued the requests waiting for room.
	Limit, InFlight, Queued int
	// QueueFull and WaitExceeded count the requests refused for each reason since the Limiter
	// was made.
	QueueFull, WaitExceeded uint64
	// NewestFirst counts the requests the class's queue granted newest first, its queue standing
	// (gate.next), since the Limiter was made.
	NewestFirst uint64
	// LongestRefusedWait is the longest a request of this class refused for waiting was queued
	// before the limiter refused it: its whole time in the queue, over every time it asked for
	// room, up to the time its wait's timer reports firing. A heavy read its lane refused is one
	// of those, and that refusal counts in LaneStats.WaitExceeded rather than in the
	// WaitExceeded above, so this figure can move while the count beside it does not. Nothing
	// exports it -- no metric, no route, no log line -- so it is a figure the admission's own
	// tests read. It is the admission's own share of a refusal's time, without the time the
	// refused request's goroutine then waits to run, which a loaded machine stretches.
	LongestRefusedWait time.Duration
}

// Stats reads each class's counts, reads first. Every count of both classes is read together.
func (l *Limiter) Stats() []ClassStats {
	stats := make([]ClassStats, 0, classCount)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.classes {
		stats = append(stats, g.stats())
	}
	return stats
}

// Totals is what a Limiter holds across both classes.
type Totals struct {
	// Limit is the combined cap in units (Limits.Total), InFlight the units the admitted
	// requests of both classes hold under it now, and ReservedReads the units of the cap
	// writes leave to the reads while a read waits for room (Limits.ReservedReads).
	Limit, InFlight, ReservedReads int
}

// Totals reads the combined cap, the read room reserved under it and what is held under it, the
// three together.
func (l *Limiter) Totals() Totals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Totals{Limit: l.total, InFlight: l.used, ReservedReads: l.reservedReads}
}

// Ticket is one admitted request's hold on its class. It rides on the request's context
// (WithTicket), so whatever the request waits on elsewhere can give the units back while it
// waits (Park, GiveBack) and take them back after (Resume, Away.TakeBack). The methods of a nil
// Ticket do nothing, so a caller need not ask whether its request was admitted at all.
type Ticket struct {
	// arrival is the place the request holds in the Limiter's arrival order, which it keeps
	// for as long as it lives: a ticket that parks its units asks for them again there.
	arrival *Arrival

	mu       sync.Mutex
	held     bool
	released bool
	// takeBackRefusal is the refusal of the units the request asked back after a wait on the
	// network (Away.TakeBack), nil where none was refused (RefusedTakeBack).
	takeBackRefusal atomic.Pointer[Refusal]
	// carried is the context the ticket's first WithTicket returns, a field of the ticket so
	// the request allocates nothing for it; carrying is set once a WithTicket has claimed it.
	carried  ticketContext
	carrying atomic.Bool
}

// RefusedTakeBack is the refusal of the units the ticket's request asked back after a wait on the
// network (Away.TakeBack), and nil where none was refused, and for a nil Ticket. The request's
// entry point answers it, 429 OVERLOADED with Retry-After, in place of anything else the request
// would have run or written (api's admitted): the layers between may read the error that carried
// it as no caller at all and go on.
func (t *Ticket) RefusedTakeBack() *Refusal {
	if t == nil {
		return nil
	}
	return t.takeBackRefusal.Load()
}

// Park gives the ticket's units back to its class, and to the combined cap, while its holder
// waits for something else - a document's writer gate, where writers queued on one document
// would otherwise hold every write unit from writers to other documents. Resume takes them back.
// Parking a ticket that is parked or released does nothing.
func (t *Ticket) Park() { t.park() }

// park is Park, reporting whether it gave units back: false for a nil ticket, and for one parked
// or released already (GiveBack).
func (t *Ticket) park() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.held {
		return false
	}
	t.held = false
	t.arrival.limiter.release(t.arrival.class, t.arrival.units)
	return true
}

// Resume takes a parked ticket's units back, waiting in its class's queue for what is left of the
// one wait the queue costs it - at the place the request arrived, ahead of every request that
// arrived after it, while the queue absorbs a burst, and as its newest waiter once it stands
// (gate.next) - and returns Admit's refusal or ctx's error when it cannot; the ticket then stays
// parked, and its holder answers the refusal as a refused request. Resuming a ticket that is not
// parked does nothing.
func (t *Ticket) Resume(ctx context.Context) error { return t.resume(ctx, true) }

// resume is Resume, refusing as admission refuses where refusable is set and never refusing where
// it is not: a resume that cannot be refused queues past the queue's bound and waits for its units
// for as long as ctx lives (Away.TakeBackNeverRefused), and its one error is ctx's own.
func (t *Ticket) resume(ctx context.Context, refusable bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	parked := !t.held && !t.released
	t.mu.Unlock()
	if !parked {
		return nil
	}
	if err := t.arrival.limiter.acquire(ctx, t.arrival, refusable); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		// Released while it waited: the units it took are nobody's.
		t.arrival.limiter.release(t.arrival.class, t.arrival.units)
		return nil
	}
	t.held = true
	return nil
}

// Remaining is what is left of the one wait the queues cost the ticket's request: its class's
// wait less the time the request has already spent waiting - in its lane, in its class's queue,
// and parked for something outside admission that counted its wait back (Waited). A holder that
// waits for something outside admission - a document's writer gate, where a parked write would
// otherwise wait for as long as its request lives - bounds that wait by this, so the request is
// answered within the wait its class states whatever it waited for. It is zero for a request
// whose wait is spent, and for a nil Ticket, a request admission did not count, whose caller
// bounds its own wait (docs.AcquireDocumentWriter).
func (t *Ticket) Remaining() time.Duration {
	if t == nil {
		return 0
	}
	return max(t.arrival.limiter.classes[t.arrival.class].wait-t.arrival.waited, 0)
}

// Waited counts waited into the one wait the request has: the time its holder spent parked for
// something outside admission - the writer gate's queue (docs.AcquireDocumentWriter) - which the
// Limiter's own queues cannot see. Remaining, the bound of whatever the request waits for next,
// and the wait of the Resume that takes the parked units back then have only what is left, so a
// write that parks for a document, resumes and gates again is answered within one wait of its
// arrival rather than one per thing it waited for. A wait on another server that a caller's
// resolution gave the units back for (GiveBack) is not counted: it is no wait for room, and the
// take-back after it has what is left of the class's queue wait, never more (Away.TakeBack). Only
// the request's own goroutine calls it, as with every other wait of its arrival: a request waits
// for one thing at a time.
func (t *Ticket) Waited(waited time.Duration) {
	if t == nil || waited <= 0 {
		return
	}
	t.arrival.waited += waited
}

// Release gives the ticket's units back for good. It runs once; every later call, and a call on
// a parked ticket after its units went back, does nothing.
func (t *Ticket) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	if t.held {
		t.held = false
		t.arrival.limiter.release(t.arrival.class, t.arrival.units)
	}
}

type ticketKey struct{}

// WithTicket returns ctx carrying ticket. The context the first call for a ticket returns is a
// field of the ticket (Ticket.carried), so admitting a request and carrying its ticket on its
// context, which the API does once for every request it admits, allocates nothing for the
// context; a later call for the same ticket, and a call for a nil one, make a context of their
// own.
func WithTicket(ctx context.Context, ticket *Ticket) context.Context {
	if ticket == nil || !ticket.carrying.CompareAndSwap(false, true) {
		return context.WithValue(ctx, ticketKey{}, ticket)
	}
	ticket.carried = ticketContext{Context: ctx, ticket: ticket}
	return &ticket.carried
}

// ticketContext is a context carrying a ticket (WithTicket): every other value it reads from the
// context it was made from, as context.WithValue's does.
type ticketContext struct {
	context.Context
	ticket *Ticket
}

func (c *ticketContext) Value(key any) any {
	if key == (ticketKey{}) {
		return c.ticket
	}
	return c.Context.Value(key)
}

// TicketFrom is the ticket ctx carries, or nil for a request admission did not count.
func TicketFrom(ctx context.Context) *Ticket {
	ticket, _ := ctx.Value(ticketKey{}).(*Ticket)
	return ticket
}

// Away is what a request gave back of the server's room for a wait on the network (GiveBack),
// which TakeBack takes again once the wait is over. The zero Away gave back nothing and takes
// nothing back, so a caller that gives back only where the wait comes - from the before-wait hook
// a verifier runs where it has decided to wait (oidc.WithBeforeWait) - takes back on every way out
// alike, whether or not the hook ran.
type Away struct {
	// ticket is the ticket whose units GiveBack parked, nil where it parked none.
	ticket *Ticket
	// place is the place GiveBack gave back, nil where it gave none.
	place *Place
}

// GiveBack gives back, for a wait on the network that reads no pool, the unit of the server's room
// ctx's request holds, and answers what it gave for TakeBack to take again once the wait is over.
// It is the one mechanism of the rule that no admission unit is held across an outbound network
// wait: IAP's key set read, or a wait for a read of it under way (identity.IAPIdentity.Login), the
// sign-in issuer's renewal of a lapsed confirmation (identity.Membership.Confirm), and a
// service-account issuer's discovery and key set read (api's serviceTokenActor), each up to ten or
// fifteen seconds on another server. A unit held across one let an issuer slow to answer, or a key
// rotation that sent every browser's assertion to the key set, fill a class with requests doing no
// work, and every request whose caller resolves from memory was refused 429 OVERLOADED until it
// answered.
//
// It gives back whichever unit the request holds:
//
//   - an admitted request's ticket (TicketFrom): its units go back to its class and the combined
//     cap (Ticket.Park). Units parked already - a write's body still arriving (api's drainedBody) -
//     are left to whoever parked them, and TakeBack takes nothing for them;
//   - a place of the request pool's share outside admission (PlaceFrom): a document socket's
//     admission, a heartbeat check (Place.Release);
//   - and the request-pool connection a pinned read holds (store.LetGoOfPinnedConnection), which
//     its unit stands for: the read's next statement takes one again.
func GiveBack(ctx context.Context) Away {
	var away Away
	if ticket := TicketFrom(ctx); ticket.park() {
		away.ticket = ticket
	}
	if place := PlaceFrom(ctx); place.release() {
		away.place = place
	}
	store.LetGoOfPinnedConnection(ctx)
	return away
}

// TakeBack takes back what GiveBack gave once the wait on the network is over, and answers nil
// once the request holds it again; the request then runs its reads and writes under it, as it
// would have had it never waited.
//
// A ticket's units come back as a parked ticket's do (Ticket.Resume): through its class's queue,
// which serves it where it arrived while the queue absorbs a burst and as its newest waiter once
// it stands (gate.next), waiting for what is left of the one wait its class's queue costs it
// (Remaining) - its class's wait, a second for a read and two for a write (DefaultReadWait,
// DefaultWriteWait), less what it already spent in admission's own queues.
// The wait on the network is not counted into that (Ticket.Waited): it was a wait on another
// server, not for room, and counted it would leave a request whose key-set read took its ten
// seconds no time to take its units back in. So the take-back waits at most its class's queue
// wait, and never longer, whatever the network took. Where no unit comes free in that time, or the
// class's queue is full, the request is refused as admission refuses one: its ticket records the
// refusal (Ticket.RefusedTakeBack) and is released for good, so no unit is counted for it and
// nothing resumes it again, and TakeBack answers the *Refusal. The request's entry point answers
// the recorded refusal 429 OVERLOADED with Retry-After: 1 and runs nothing more of the request,
// whatever the layers between do with the error (api's admitted and optionalActor). The refusal
// also goes on the request's store mark (store.RefuseRequest), so no statement or commit of the
// request runs after it either. A refusal after the issuer answered drops that answer, as a write a
// busy pool refused would. A request whose context ends while it waits is answered that context's
// error, holding nothing.
//
// A place comes back as Place.Retake takes one, waiting for it as Take waits. A place refused is a
// bound of this server's own that ended the work before it asked the database anything, so nothing
// of it was written. The place records the refusal (Place.RefusedTakeBack), and TakeBack answers it
// as a pool with no connection free is (store.ErrPoolBusy, wrapping the *OutsideRefusal or ctx's
// error): a document socket answers the recorded refusal 429 OVERLOADED with Retry-After: 1 before
// its upgrade, as admission answers a request it has no room for (docs.Service.ServeHTTP); a
// heartbeat check, which has no HTTP answer to give, counts the busy pool as a transient failure
// and skips (api's transientCheckFailure); and a request waiting on a sign-in's refresh another
// work leads tries again as a busy pool tells it to.
func (a Away) TakeBack(ctx context.Context) error {
	if err := a.ticket.Resume(ctx); err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			a.ticket.takeBackRefusal.Store(refusal)
			a.ticket.Release()
			store.RefuseRequest(ctx, refusal)
		}
		return err
	}
	return a.retakePlace(ctx)
}

// TakeBackNeverRefused is TakeBack for a request that must not be refused once its wait is over:
// one whose wait changed another server's state - the listener's Send or Unsubscribe, a publish on
// the message bus - or that committed writes of its own before it, with work of Dispatch's own
// still to do after it. Refused then, 429 OVERLOADED would tell its client that nothing of the
// request ran when something did, and the client's repeat would make it twice, a keyed repeat's
// send among it. Its units come back as TakeBack's do, through its class's queue, but past the
// queue's bound and with no bound on the wait, and a queue that stands serves it ahead of every
// waiter that may be refused, the oldest of such take-backs first (gate.next), so newest first
// never holds it for as long as the overload lasts: it waits for them for as long as ctx lives
// and is never refused, records no refusal and puts none on the request's store mark. Its one
// error is ctx's own, where ctx ends first, and the ticket then stays parked, holding nothing, so
// the request's statements after it fail on that same ended context. A place comes back as
// TakeBack takes one: only a document socket's admission and a heartbeat check hold one, and
// neither makes such a call.
func (a Away) TakeBackNeverRefused(ctx context.Context) error {
	if err := a.ticket.resume(ctx, false); err != nil {
		return err
	}
	return a.retakePlace(ctx)
}

// retakePlace takes back the place GiveBack gave, for TakeBack and TakeBackNeverRefused.
func (a Away) retakePlace(ctx context.Context) error {
	if err := a.place.Retake(ctx); err != nil {
		var refusal *OutsideRefusal
		if errors.As(err, &refusal) {
			a.place.takeBackRefusal = refusal
		}
		return fmt.Errorf("%w: no place of the request pool's share outside admission came free after a wait on the network: %w", store.ErrPoolBusy, err)
	}
	return nil
}

// gate is one class's units, wait and wait queue: a weighted semaphore whose queue hands room to
// one candidate at a time, the waiter it serves next (next) - its earliest arrival while it absorbs
// a burst, its newest waiter once it stands (ReadBurstFactor says why) - so a heavy candidate is
// not overtaken by lighter waiters of its class that would fit. Its wait is the class's own
// (Limits.ReadWait, Limits.WriteWait), so each class is refused at the bound its own requests are
// worth waiting out, and it is the wait the queue stands after. Its Limiter's mutex guards its
// counts and its queue, since the combined cap ties the classes together; only the refusal
// counters, which no decision reads, stand on their own.
type gate struct {
	class Class
	limit int
	wait  time.Duration
	// burstCap is the most the queue holds, the class's burst factor times its limit: a bound on
	// the memory its waiters take (ReadBurstFactor).
	burstCap int
	used     int
	// waiters is the queue in arrival order, by seq. joined is its waiters that may be refused,
	// in the order they joined it, the newest last, and kept its waiters that may not be, in
	// arrival order: the two a queue that stands serves from (next). Each waiter holds its own
	// elements of them (waiter.byArrival, waiter.byJoin), so one leaves the queue at no cost of
	// its length.
	waiters, joined, kept list.List // of *waiter
	// nonEmptySince is when the queue last went from empty to non-empty, by the Limiter's clock
	// (queue).
	nonEmptySince time.Time
	// newestFirst counts the waiters the queue granted newest first (grant).
	newestFirst uint64

	queueFull, waitExceeded atomic.Uint64
	longestRefusedWait      atomic.Int64
}

// waiter is one queued acquire; ready closes once its units are granted. seq is its place among
// the arrivals of both classes, which orders a queue absorbing a burst and the two classes'
// candidates against each other. refusable is false for a take-back that must not be refused
// (Away.TakeBackNeverRefused), which a queue that stands serves ahead of the rest (gate.next).
// byArrival is its element of its gate's waiters, and byJoin its element of joined, or of kept
// where it may not be refused.
type waiter struct {
	units             int
	seq               uint64
	refusable         bool
	ready             chan struct{}
	byArrival, byJoin *list.Element
}

// queue puts queued in g's queue: among its waiters behind every waiter that arrived before it and
// ahead of every waiter that arrived after, and behind every waiter that joined before it - or,
// where it may not be refused, among the kept by arrival. A request joins the queue out of arrival
// order where it had something to do before it asked for room - a write reading its body off the
// connection (Limiter.Arrive) - or where it gave its units back and asked for them again
// (Ticket.Resume): a queue absorbing a burst serves it where it arrived, so the requests that
// arrived while it was busy do not overtake it, and a queue that stands serves it as the newest
// waiter it is (next). A queue it finds empty has been non-empty since now, the Limiter's clock.
// l.mu is held.
func (g *gate) queue(queued *waiter, now time.Time) {
	if g.waiters.Len() == 0 {
		g.nonEmptySince = now
	}
	queued.byArrival = insertByArrival(&g.waiters, queued)
	if queued.refusable {
		queued.byJoin = g.joined.PushBack(queued)
	} else {
		queued.byJoin = insertByArrival(&g.kept, queued)
	}
}

// insertByArrival puts queued in waiters, a list of *waiter by seq, behind every waiter that
// arrived before it and ahead of every waiter that arrived after, and answers its element. It
// looks from the back, where a request that asks in the order it arrived belongs.
func insertByArrival(waiters *list.List, queued *waiter) *list.Element {
	for element := waiters.Back(); element != nil; element = element.Prev() {
		if element.Value.(*waiter).seq < queued.seq {
			return waiters.InsertAfter(queued, element)
		}
	}
	return waiters.PushFront(queued)
}

// remove takes w out of g's queue. l.mu is held.
func (g *gate) remove(w *waiter) {
	g.waiters.Remove(w.byArrival)
	if w.refusable {
		g.joined.Remove(w.byJoin)
	} else {
		g.kept.Remove(w.byJoin)
	}
}

// next is the waiter g's queue serves next - its candidate - and whether it serves it newest
// first, or nil for an empty queue. The queue decides at each grant from when it last went from
// empty to non-empty (queue), by now, the Limiter's clock, which nothing but this decision reads.
//
// A queue that went from empty to non-empty less than a wait ago (g.wait) has emptied within its
// last wait and is absorbing a burst: it serves first come, first served, its earliest arrival
// first, so a burst keeps its order. One that has stood non-empty for a whole wait stands, and
// serves the waiter that joined it last, until it empties: not the latest arrival, since a ticket
// taking its units back (Ticket.Resume, Away.TakeBack) or a write that read its body first
// (Limiter.Arrive) joins late with the place it arrived at, and is the newest waiter there. Every
// waiter keeps its own wait and is refused only when that wait runs out, so the oldest waiters a
// queue that stands passes over are refused at their wait, and none before it (ReadBurstFactor).
//
// A waiter that may not be refused (Away.TakeBackNeverRefused, acquire with refusable false) has
// no wait, so newest first could pass it over for as long as the overload lasts, after its request
// changed another server's state or committed writes of its own. A queue that stands serves such
// waiters ahead of every waiter that may be refused, the oldest of them first. l.mu is held.
func (g *gate) next(now time.Time) (candidate *waiter, newestFirst bool) {
	front := g.waiters.Front()
	if front == nil {
		return nil, false
	}
	if now.Sub(g.nonEmptySince) < g.wait {
		return front.Value.(*waiter), false
	}
	if kept := g.kept.Front(); kept != nil {
		return kept.Value.(*waiter), false
	}
	return g.joined.Back().Value.(*waiter), true
}

// acquireNow takes a's units if they fit with nobody queued for its class, and reports whether
// it did: the caller of a request that would have to wait does something else first.
func (l *Limiter) acquireNow(a *Arrival) bool {
	g := l.classes[a.class]
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.waiters.Len() != 0 || !l.fits(g, a.units) {
		return false
	}
	g.used += a.units
	l.used += a.units
	return true
}

// acquire takes a's units, waiting in its class's queue (gate.next) for what is left of that
// class's wait (g.wait): a request that queues more than once - it parked its units and asked for
// them again (Ticket.Resume) - waits one wait over all of its asks. An acquire that is not
// refusable - a take-back that must not be refused (Away.TakeBackNeverRefused) - queues past the
// queue's bound, is served in a queue that stands ahead of every waiter that may be refused, and
// waits with no bound but ctx's, so its one error is ctx's own.
func (l *Limiter) acquire(ctx context.Context, a *Arrival, refusable bool) error {
	g := l.classes[a.class]
	l.mu.Lock()
	if g.waiters.Len() == 0 && l.fits(g, a.units) {
		g.used += a.units
		l.used += a.units
		l.mu.Unlock()
		return nil
	}
	// The queue's one bound is its burst cap, which bounds its memory (ReadBurstFactor).
	if refusable && g.waiters.Len() >= g.burstCap {
		l.mu.Unlock()
		g.queueFull.Add(1)
		return &Refusal{Class: a.class, Reason: QueueFull}
	}
	queued := &waiter{units: a.units, seq: a.seq, refusable: refusable, ready: make(chan struct{})}
	now := l.now()
	g.queue(queued, now)
	if next, _ := g.next(now); next == queued {
		// It is the waiter the queue serves next - it arrived before the waiters it found queued,
		// or it joined a queue that stands - so the room the queues were last offered was offered
		// to another.
		l.grant()
	}
	l.mu.Unlock()

	var refusal *Refusal
	if refusable {
		refusal = &Refusal{Class: a.class, Reason: WaitExceeded}
	}
	granted, failure := a.await(ctx, queued.ready, g.wait, refusal)
	if granted {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-queued.ready:
		// Granted in the moment the wait ended. A caller whose wait ran out takes it, since it
		// is here; one whose context ended gives it straight back, so no unit is left held.
		if ctx.Err() == nil {
			return nil
		}
		g.used -= a.units
		l.used -= a.units
		l.grant()
		return ctx.Err()
	default:
	}
	next, _ := g.next(l.now())
	g.remove(queued)
	if next == queued {
		// The waiter the queue served next may have been all that held back the waiters behind
		// it - and, when the combined cap was what held it, the other class's waiters too.
		l.grant()
	}
	if _, refused := failure.(*Refusal); refused {
		g.waitExceeded.Add(1)
		g.noteRefusedWait(a.waited)
	}
	return failure
}

// await waits for ready - the place a gate hands the waiter it serves next - for what is left of
// a's one wait, and reports whether that place is a's. The wait is wait less what a has already
// waited, so a request that queues more than once (a heavy read's lane and then its class, a
// ticket asking its units back) waits one wait over all of its queues, and every arm adds what it
// waited here to a.waited, so the next queue it joins counts from there. A nil refusal waits with
// no bound but ctx's: a take-back that must not be refused (Away.TakeBackNeverRefused).
//
// A false answer carries why: refusal, the caller's own, when the wait ran out, and ctx's error
// when the caller's context ended. Either way the place may have been granted in the moment the
// wait ended, which only the gate's own lock can settle, so the caller re-locks and looks again
// before it gives the refusal (Limiter.acquire, fifoGate.enter for a lane). a is the request's own
// arrival, which only its goroutine touches.
func (a *Arrival) await(ctx context.Context, ready <-chan struct{}, wait time.Duration, refusal *Refusal) (bool, error) {
	queuedAt := time.Now()
	var expired <-chan time.Time
	if refusal != nil {
		timer := time.NewTimer(max(wait-a.waited, 0))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-ready:
		a.waited += time.Since(queuedAt)
		return true, nil
	case <-ctx.Done():
		a.waited += time.Since(queuedAt)
		return false, ctx.Err()
	case fired := <-expired:
		a.waited += fired.Sub(queuedAt)
		return false, refusal
	}
}

// noteRefusedWait records waited as the longest a refused request of this class has waited, if
// it is: the figure the admission's own tests read to see how long a refusal cost the request
// that got it, which nothing exports (ClassStats.LongestRefusedWait). A heavy read's lane
// records its own refusals here too, since a lane refusal is a refusal of the read class and
// spends the read's one wait (Limiter.EnterLane). It is a compare-and-swap rather than a read
// and a write, because a lane holds no lock of the Limiter's while it records one.
func (g *gate) noteRefusedWait(waited time.Duration) {
	for {
		longest := g.longestRefusedWait.Load()
		if int64(waited) <= longest {
			return
		}
		if g.longestRefusedWait.CompareAndSwap(longest, int64(waited)) {
			return
		}
	}
}

// release gives units of class back and grants the queues whatever now fits.
func (l *Limiter) release(class Class, units int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.classes[class].used -= units
	l.used -= units
	l.grant()
}

// fits reports whether units may start in g now: g's class and the combined cap, the reserved
// read room counted, both have room, and no queue's candidate (gate.next) is held up by the
// combined cap alone. Such a candidate waits for room of the whole Limiter, which this request
// would take from it. The clock the candidates are decided by is read only where a queue has a
// waiter. l.mu is held.
func (l *Limiter) fits(g *gate, units int) bool {
	if !l.hasRoom(g, units) {
		return false
	}
	if l.classes[Read].waiters.Len() == 0 && l.classes[Write].waiters.Len() == 0 {
		return true
	}
	return l.heldByTotal(l.now()) == nil
}

// hasRoom reports whether g's class and the combined cap hold units on top of what they hold
// now, the room reserved for the reads counted against a write. l.mu is held.
func (l *Limiter) hasRoom(g *gate, units int) bool {
	return g.used+units <= g.limit && l.used+units+l.reservedFor(g.class) <= l.total
}

// reservedFor is the room of the combined cap class may not take: for a write, the read units
// reserved that the reads have not taken yet, while a read is waiting for them. A read takes
// the reserved room itself, and reserving it against nobody would only idle the units, so it is
// held back only while the read queue has someone in it. l.mu is held.
func (l *Limiter) reservedFor(class Class) int {
	reads := l.classes[Read]
	if class == Read || reads.waiters.Len() == 0 {
		return 0
	}
	return max(l.reservedReads-reads.used, 0)
}

// heldByTotal is the earliest of the two class queues' candidates at now (gate.next) that fits its
// own class but not the combined cap itself, or nil when neither candidate is one. A write the
// read reservation refuses as well is not one: the reads it waits for must be able to take that
// room, so it holds back the writes queued beside it and nothing else. That covers the write the
// reservation alone holds up and the one both it and the cap refuse - a weight-2 candidate with the
// cap a unit short of it - whose room is the reads' until they have taken their reservation: a read
// queued beside such a candidate, which the cap has room for, is granted rather than held. l.mu is
// held.
func (l *Limiter) heldByTotal(now time.Time) *waiter {
	var earliest *waiter
	for _, g := range l.classes {
		candidate, _ := g.next(now)
		if candidate == nil {
			continue
		}
		if g.used+candidate.units > g.limit || l.used+candidate.units <= l.total {
			continue
		}
		if reserved := l.reservedFor(g.class); reserved > 0 && l.used+candidate.units+reserved > l.total {
			continue
		}
		if earliest == nil || candidate.seq < earliest.seq {
			earliest = candidate
		}
	}
	return earliest
}

// grant hands room to the candidates of the two class queues (gate.next) - the earlier arrival of
// the two first - while each fits, deciding each queue's candidate again after every grant. A
// candidate the combined cap itself alone holds up stops the pass for both classes: the room it
// waits for is the whole Limiter's, and the later arrivals of the other class would take it a unit
// at a time, so a weight-2 candidate is never starved by weight-1 requests beside it. A candidate
// its own class's limit, or the read reservation, holds up stops only its own queue. The pass reads
// the clock once, and not at all where both queues are empty. l.mu is held.
func (l *Limiter) grant() {
	if l.classes[Read].waiters.Len() == 0 && l.classes[Write].waiters.Len() == 0 {
		return
	}
	now := l.now()
	for {
		held := l.heldByTotal(now)
		var next *waiter
		var from *gate
		var newestFirst bool
		for _, g := range l.classes {
			candidate, newest := g.next(now)
			if candidate == nil || !l.hasRoom(g, candidate.units) {
				continue
			}
			if next == nil || candidate.seq < next.seq {
				next, from, newestFirst = candidate, g, newest
			}
		}
		if next == nil || (held != nil && held.seq < next.seq) {
			return
		}
		from.used += next.units
		l.used += next.units
		from.remove(next)
		if newestFirst {
			from.newestFirst++
		}
		close(next.ready)
	}
}

// stats is the class's counts. Its Limiter's mutex is held.
func (g *gate) stats() ClassStats {
	return ClassStats{
		Class: g.class, Limit: g.limit, InFlight: g.used, Queued: g.waiters.Len(),
		QueueFull: g.queueFull.Load(), WaitExceeded: g.waitExceeded.Load(), NewestFirst: g.newestFirst,
		LongestRefusedWait: time.Duration(g.longestRefusedWait.Load()),
	}
}
