package wakepace

import (
	"slices"
	"strconv"
	"sync"
	"time"
)

// Queue is which of a Pacer's two queues a wake waits in.
type Queue int

const (
	// Second is the queue of every paced wake nothing marks: a rule resume, a catch-up, a
	// displaced relaunch. It is the zero value, so a wake goes ahead only when its caller says it
	// must.
	Second Queue = iota
	// First is the queue of the wakes that must not wait behind a backlog: a wake for a role with
	// an Envoy message waiting, which Envoy drops once its keep window ends, and the relaunch after
	// a death charged to the role's own process.
	First
)

// firstRun is how many First let-outs in a row the share lets run while Second holds a place. The
// turn after them is Second's, so First takes three turns in four while both queues wait, and
// Second, which holds the restart's backlog, still drains.
const firstRun = 3

// String is the queue's name in a log line: "message" for First, the queue of the wakes that carry
// one, and "other" for Second.
func (q Queue) String() string {
	switch q {
	case First:
		return "message"
	case Second:
		return "other"
	}
	return "Queue(" + strconv.Itoa(int(q)) + ")"
}

// Pacer is two FIFO queues in front of one Limiter. The limiter's token bucket gives the turns and
// so keeps the rate; the pacer decides only which waiting wake takes each turn. A turn goes to the
// head of First, except that while both queues hold places the turn after three First let-outs in
// a row goes to the head of Second. A First place with k First places ahead of it is so let out
// within k+2+floor(k/3) turns of its Join, whatever Second holds.
//
// The pacer reserves one turn at a time, the pending turn, and only while a place waits, so a
// waiting place has drawn no token: a message wake that passes it, or a Leave that drops it, moves
// no moment anyone was handed.
//
// A Pacer is safe for use from several goroutines. It holds no clock: now is the caller's, and at
// arms the caller's timer, so the daemon paces on its own clock and a test decides every turn.
type Pacer struct {
	limiter *Limiter
	now     func() time.Time
	at      func(time.Time, func())

	mu sync.Mutex
	// queues holds each queue's waiting places, head first, indexed by Queue.
	queues [2][]*Place
	// run is how many First let-outs came in a row since the last Second let-out. It goes on
	// counting while Second is empty, but buys Second no more than its next turn: a share that
	// built credit would give a Second that fills after a long First run a row of turns, and hold
	// back the message wakes the pacer exists to let ahead.
	run int
	// turns is how many turns have come, which is how a place counts the turns it waited.
	turns int
	// pending is the moment of the one turn reserved and not yet come, while held says one is. A
	// pending turn outlives a Leave that empties the queues: its token is drawn, so it comes and
	// lets out whoever waits then, or nobody.
	pending time.Time
	held    bool
}

// NewPacer builds a pacer over l. now is the caller's clock, and at arms a timer on it that calls f
// once the clock reaches the moment, at once when it already has, so a caller hands in the clock
// it already keeps. A limiter whose Interval is zero is the pacing off, and every place is then
// let out at its Join.
//
// The pacer never stops a timer it armed, so at answers nothing: a pending turn has drawn its
// token, and stopping it would give back nothing.
func NewPacer(l *Limiter, now func() time.Time, at func(time.Time, func())) *Pacer {
	return &Pacer{limiter: l, now: now, at: at}
}

// Place is one wake's place in a Pacer's queues, from its Join until it is let out or leaves.
type Place struct {
	pacer *Pacer

	// Every field below is the pacer's, read and written under its mu.
	queue Queue
	state placeState
	// joined is the pacer's turns at the Join, from which Let.Turns counts.
	joined  int
	let     Let
	release func(Let)
}

// placeState is where a place is in its life: waiting in a queue, let out, or gone by a Leave.
type placeState int

const (
	stateWaiting placeState = iota
	stateLetOut
	stateGone
)

// Let is how a place was let out.
type Let struct {
	// At is the caller's now at the let-out, stripped of its monotonic reading as every moment the
	// limiter hands out is, so that it compares with what Estimate said.
	At time.Time
	// Queue is the queue the place was let out of: First for a place Mark moved there.
	Queue Queue
	// Turns is how many turns came from the place's Join to its let-out, the turn that let it out
	// among them: 0 for a place let out at its Join, 1 for one let out at the turn pending then.
	Turns int
}

// Join takes a place at the back of q. On a pacer with both queues empty and no turn pending it
// reserves a turn, and when that turn is now — a fresh limiter, a burst still open, a bucket that
// refilled while nothing waited, the pacing off — the place is let out at once, which Bind and Let
// then report. Otherwise the place waits: the turn it reserved becomes the pending turn, or the
// turn already pending stands.
func (p *Pacer) Join(q Queue) *Place {
	pl := &Place{pacer: p, queue: q}
	now := p.now().Round(0)

	p.mu.Lock()
	if p.held || len(p.queues[First])+len(p.queues[Second]) > 0 {
		p.wait(pl)
		p.mu.Unlock()
		return pl
	}
	turn := p.limiter.Reserve(now)
	if !turn.After(now) {
		p.letOut(pl, now, 0)
		p.mu.Unlock()
		return pl
	}
	p.pending, p.held = turn, true
	p.wait(pl)
	p.mu.Unlock()

	// The timer is armed outside the lock: an at whose moment has passed may call the turn on this
	// goroutine, and the turn takes the lock.
	p.at(turn, p.turn)
	return pl
}

// wait puts pl at the back of its queue. The caller holds mu.
func (p *Pacer) wait(pl *Place) {
	pl.joined = p.turns
	p.queues[pl.queue] = append(p.queues[pl.queue], pl)
}

// letOut lets pl out at now, turns turns after its Join, and counts it in the share's run. The
// caller holds mu, and pl is in no queue.
func (p *Pacer) letOut(pl *Place, now time.Time, turns int) {
	pl.state = stateLetOut
	pl.let = Let{At: now, Queue: pl.queue, Turns: turns}
	if pl.queue == First {
		p.run++
	} else {
		p.run = 0
	}
}

// turn is the pending turn come, called on the goroutine of its timer. It lets out the head the
// share gives the turn to and, while a place still waits, reserves the next turn and arms its
// timer. A next turn that is already due — a burst still open, or a timer that fired late enough
// for the bucket to refill — is armed all the same, for a moment that has come, so it is a turn of
// its own on a timer of its own: each let-out's release then runs on its own goroutine under a
// clock that gives every timer one, and no wake waits for the launch of the wake let out a turn
// before it, which a machine's release holds its goroutine for. The next timer and the release
// are called once the lock is let go, so that a release may Join or Leave.
func (p *Pacer) turn() {
	now := p.now().Round(0)
	var (
		release func(Let)
		let     Let
		next    time.Time
		arm     bool
	)

	p.mu.Lock()
	p.held = false
	p.turns++
	if pl := p.head(); pl != nil {
		p.letOut(pl, now, p.turns-pl.joined)
		release, let, pl.release = pl.release, pl.let, nil
	}
	if len(p.queues[First])+len(p.queues[Second]) > 0 {
		next = p.limiter.Reserve(now)
		p.pending, p.held, arm = next, true, true
	}
	p.mu.Unlock()

	if arm {
		p.at(next, p.turn)
	}
	if release != nil {
		release(let)
	}
}

// share is the queue the share rule gives the coming turn to, given how many places First and
// Second hold and how many First let-outs ran in a row before it, and false when both are empty.
// It is the one statement of the rule: head lets out by it and ahead plays it forward.
func share(first, second, run int) (Queue, bool) {
	switch {
	case first > 0 && (second == 0 || run < firstRun):
		return First, true
	case second > 0:
		return Second, true
	}
	return Second, false
}

// head takes the place this turn goes to off its queue, or returns nil when both are empty. The
// caller holds mu.
func (p *Pacer) head() *Place {
	q, ok := share(len(p.queues[First]), len(p.queues[Second]), p.run)
	if !ok {
		return nil
	}
	pl := p.queues[q][0]
	p.queues[q][0] = nil
	p.queues[q] = p.queues[q][1:]
	return pl
}

// ahead is how many turns the share rule, played forward over the queues as they stand, lets out
// before the waiting place pl, and false when pl is not waiting. The caller holds mu.
func (p *Pacer) ahead(pl *Place) (int, bool) {
	i := slices.Index(p.queues[pl.queue], pl)
	if i < 0 {
		return 0, false
	}
	first, second, run := len(p.queues[First]), len(p.queues[Second]), p.run
	for n := 0; ; n++ {
		q, _ := share(first, second, run)
		if q == pl.queue {
			if i == 0 {
				return n, true
			}
			i--
		}
		if q == First {
			first, run = first-1, run+1
		} else {
			second, run = second-1, 0
		}
	}
}

// remove takes the waiting place pl off its queue, and is false when pl is not waiting. The caller
// holds mu.
func (p *Pacer) remove(pl *Place) bool {
	q := p.queues[pl.queue]
	i := slices.Index(q, pl)
	if i < 0 {
		return false
	}
	p.queues[pl.queue] = slices.Delete(q, i, i+1)
	return true
}

// Bind hands a waiting place the release to call when it is let out. Join and Bind are two calls so
// that a caller holds its place before its release can run.
//
// A place let out already — at its Join, or at a turn before this Bind — returns its Let and true,
// and release is never called: the caller goes on with the wake itself. A waiting place returns
// false, and release is called exactly once, when the place is let out, on the goroutine of the
// timer whose turn let it out and outside the pacer's lock, so that a release may Join or Leave.
// One turn lets out one place, so a release that takes long holds back no other place's.
// Bind may be called again while the place waits, as a machine does when it detaches a waiting
// wake and takes it up again later at the same place: the newer release replaces the earlier one,
// which is then never called. A place that left returns false, and release is never called.
func (pl *Place) Bind(release func(Let)) (Let, bool) {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	switch pl.state {
	case stateLetOut:
		return pl.let, true
	case stateWaiting:
		pl.release = release
	}
	return Let{}, false
}

// Leave gives up a waiting place. Nothing is charged for it, since a waiting place has drawn no
// token, and the turn it would have taken goes to the next head; its release is never called. A
// Leave of a place let out already, or gone, does nothing: a release already on its way still
// runs, and Let says which came first.
func (pl *Place) Leave() {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remove(pl) {
		pl.state, pl.release = stateGone, nil
	}
}

// Mark moves a place waiting in Second to the back of First, and reports whether it moved: false
// for a place in First already, let out, or gone. It is how a wake a later report marks — the
// listener's message for a claim already waiting — goes ahead without starting its wait again:
// its turns still count from its Join.
func (pl *Place) Mark() bool {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl.queue != Second || !p.remove(pl) {
		return false
	}
	pl.queue = First
	p.queues[First] = append(p.queues[First], pl)
	return true
}

// Estimate is the moment the place is let out: its let-out moment once it is, and while it waits,
// the pending turn plus one interval for each turn the share rule, played over the queues as they
// stand, gives ahead of it. It holds while the queues stand and the pace is exact; a Join, a Mark,
// a Leave, the jitter or a late timer moves it. A place that left estimates the zero time.
func (pl *Place) Estimate() time.Time {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	if pl.state == stateLetOut {
		return pl.let.At
	}
	n, ok := p.ahead(pl)
	if !ok {
		return time.Time{}
	}
	return p.pending.Add(time.Duration(n) * p.limiter.Interval())
}

// Before reports whether this waiting place would be let out before other under the share rule
// over the queues as they stand. It is false unless both wait in the same pacer.
func (pl *Place) Before(other *Place) bool {
	p := pl.pacer
	if other.pacer != p {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mine, ok := p.ahead(pl)
	if !ok {
		return false
	}
	theirs, ok := p.ahead(other)
	return ok && mine < theirs
}

// Queue is the queue the place waits in, was let out of, or left.
func (pl *Place) Queue() Queue {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	return pl.queue
}

// Let is how the place was let out, and false while it waits or once it left.
func (pl *Place) Let() (Let, bool) {
	p := pl.pacer
	p.mu.Lock()
	defer p.mu.Unlock()
	return pl.let, pl.state == stateLetOut
}
