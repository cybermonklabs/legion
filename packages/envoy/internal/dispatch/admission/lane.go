package admission

// The heavy reads' lanes. A few read routes cost the database far more than the rest, and the
// class limits count them as one unit each like any other read, so nothing stops several of them
// running at once: the processors go to those few and every other request waits behind them.
// A lane is a limit of its own for one such route, taken before the request's class units.

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Lane is a heavy read route's own concurrency limit. A route carries one (api's heavyRead); every
// other route carries NoLane and is bounded by its class alone.
type Lane int

const (
	// NoLane is every route but the heavy reads.
	NoLane Lane = iota
	// Search is GET /api/v1/search, the full-text union.
	Search
	// References is the reference closure, GET /api/v1/issues/{key}/references.
	References
	// IssueArtifacts is GET /api/v1/issues/{key}/artifacts, an issue's documents and files.
	IssueArtifacts
	// IssueList is GET /api/v1/issues, the filtered issue list.
	IssueList
	laneCount
)

var laneNames = [laneCount]string{NoLane: "", Search: "search", References: "references",
	IssueArtifacts: "issue_artifacts", IssueList: "issue_list"}

func (l Lane) String() string {
	if l < 0 || l >= laneCount {
		return ""
	}
	return laneNames[l]
}

// HeavyReadLanes are the lanes in order, for a caller that reports on each of them (the metrics
// endpoint).
func HeavyReadLanes() []Lane { return []Lane{Search, References, IssueArtifacts, IssueList} }

// heavyReadLaneLimit is how many requests of one heavy read route may run at once in a process
// of procs processors: heavyReadsPerCPU a processor, at least MinHeavyReadLane and at most
// MaxHeavyReadLane.
//
// The width per processor is measured rather than chosen. On the load test's recovery step
// (packages/envoy/loadtest, 0.5x:60s,10x:60s,0.5x:120s) the windows that fail the recovery
// criterion are 3 to 4 second stalls in which Postgres, sampled every 250 ms, has three to five of
// these statements running at
// once and waiting on nothing - the search union at 283 to 983 ms of statement age, the issue list
// at 104 to 558 ms, the references closure, an issue's artifacts at 165 to 449 ms - while
// everything else on the box queues behind them. Over the two minutes after the drop those four
// statements are 101 seconds of the 124 seconds of statement time the whole process spends, on a
// box of three processors (INFRA-592, j-report Step 4):
//
//	the search union (websearch_to_tsquery)   46.4 s over 142 calls, 327 ms each
//	the references closure (recursive, refs)  25.8 s over 158 calls, 164 ms each
//	an issue's artifacts                      15.0 s over  52 calls, 288 ms each
//	the issue list (recursive, filtered)      14.1 s over  95 calls, 149 ms each
//
// Three - one a processor on that box - is measured too, against one and two, three runs each
// (INFRA-592, j-report Step 5). The three answer the same load differently and the difference is
// where the cost shows:
//
//	lane limit  2xx served  what the recovery windows failed on
//	1           8,300       the refusals: 85 to 98% of the requests of a window succeeded
//	2           8,083       the refusals, smaller: 97 to 99%
//	3           8,122       neither in one run of three (it recovered in 0 s); the p99 of the
//	                        40-60 s windows in the other two, and one window at 99.0%
//
// Below three a lane refuses requests the box would have served - at one, nearly every window of
// every run was refused past the criterion's 99% - and above it the point of the lane goes: at
// four, four searches at 327 ms each are more database processor than the box has. Three serves
// about 8% more requests than the same tree without lanes (7,161 to 7,541), so the lane is not
// costing throughput; what it buys is that one route cannot hold the database while everything
// else waits.
//
// It grows with the processors because the measurement that fixed it at three was of a
// three-processor box: on the full-size copy (packages/envoy/loadtest/cluster, a 16-processor
// Dispatch on a 16 vCPU database), a lane held at three refused requests at half the load, after
// the burst had gone, that the copy had the processors to serve, and failed the recovery's 99%
// on those refusals alone (INFRA-592, cluster run lt-20261105t223939-4a7a). MaxHeavyReadLane is
// the read class's own cap shared among the four lanes, so the heavy reads together can never hold
// more than the reads a process admits at all (MaxReadLimit).
//
// It never falls below MinHeavyReadLane, three, the width that measurement found below which a
// lane refuses requests the box would serve. One a processor alone gave a two-processor server
// lanes of two, and on the load test's two-processor bed at 2x those refused 16 heavy reads
// (15 of an issue's references, 1 of the issue list) that main, with no lanes, served with a p99
// of 540 and 368 ms (INFRA-592, tester round 38, steps s-head-2x and s-main-2x). Four lanes of
// three, each request one unit (a heavy read route sets no weight), hold 12 read units, within the
// 14 the read class admits on one processor, so the floor admits no more heavy reads than the read
// class can hold.
func heavyReadLaneLimit(procs int) int {
	return max(MinHeavyReadLane, perCPULimit(procs, heavyReadsPerCPU, MaxHeavyReadLane))
}

// heavyReadsPerCPU is each heavy read lane's width per processor (heavyReadLaneLimit).
const heavyReadsPerCPU = 1

// MinHeavyReadLane is the narrowest a heavy read lane is, whatever the processors
// (heavyReadLaneLimit says why).
const MinHeavyReadLane = 3

// MaxHeavyReadLane is the widest a heavy read lane grows: the read class's cap over the four lanes.
const MaxHeavyReadLane = MaxReadLimit / int(laneCount-1)

// fifoGate is a limit of places and the queue that waits for one, served first in, first out, one
// place a holder: the gate a heavy read's lane (laneGate) and the request pool's work outside
// admission (Outside) are both built on. It has a mutex of its own, since neither holds any of the
// Limiter's while it waits, and its queue holds at most queueFactor times its limit, the shape of
// a class's queue (queueFactor says why). How long a waiter waits, and what a refusal is and is
// counted against, are the caller's (enter).
type fifoGate struct {
	limit int

	// mu guards the places held, the queue and its longest length.
	mu        sync.Mutex
	used      int
	waiters   list.List // of chan struct{}, in arrival order
	maxQueued int
}

// errGateFull is fifoGate.enter's answer where the queue already holds queueFactor times the
// limit; its caller answers its own refusal for a full queue (QueueFull).
var errGateFull = errors.New("the queue for a place is full")

// enter takes a place and answers nil once it holds one: at once where a place is free and nothing
// waits, else after waiting for one in the queue, behind every caller that asked before it. wait
// does the waiting: it waits on ready, which leave closes to hand the caller a place, and answers
// whether the place came and, where not, why - the caller's own refusal where its wait ran out,
// ctx's error where ctx ended. A caller refused or gone holds no place and has left the queue:
// enter answers errGateFull at once where the queue is full, and otherwise wait's failure, with
// leftQueue true. A place handed over in the moment the wait ended is the caller's where ctx is
// still live; where ctx has ended it is given straight on and enter answers wait's failure with
// leftQueue false, since that caller was handed a place rather than refused one.
func (g *fifoGate) enter(ctx context.Context, wait func(ready <-chan struct{}) (bool, error)) (leftQueue bool, err error) {
	g.mu.Lock()
	if g.waiters.Len() == 0 && g.used < g.limit {
		g.used++
		g.mu.Unlock()
		return false, nil
	}
	if g.waiters.Len() >= queueFactor*g.limit {
		g.mu.Unlock()
		return false, errGateFull
	}
	ready := make(chan struct{})
	element := g.waiters.PushBack(ready)
	g.maxQueued = max(g.maxQueued, g.waiters.Len())
	g.mu.Unlock()

	granted, failure := wait(ready)
	if granted {
		return false, nil
	}

	g.mu.Lock()
	select {
	case <-ready:
		// Handed a place in the moment the wait ended: a caller still there takes it, one whose
		// context ended gives it straight on.
		g.mu.Unlock()
		if ctx.Err() == nil {
			return false, nil
		}
		g.leave()
		return false, failure
	default:
	}
	g.waiters.Remove(element)
	g.mu.Unlock()
	return true, failure
}

// leave gives one place back, handing it to the waiter at the head of the queue where any waits.
func (g *fifoGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if front := g.waiters.Front(); front != nil {
		g.waiters.Remove(front)
		close(front.Value.(chan struct{}))
		return
	}
	g.used--
}

// counts reads the places held, the waiters queued now and the most that have queued at once.
func (g *fifoGate) counts() (used, queued, maxQueued int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.waiters.Len(), g.maxQueued
}

// laneGate is one lane's limit and its queue: a fifoGate, since a lane is taken before the
// classes and holds none of their units while it waits. Its waiters are served first in, first
// out - a request takes its lane before anything else it waits for, right after the arrival that
// stamps its place (Limiter.Arrive), so the order it reaches the queue is the order it arrived
// in. Its wait is the read class's, which EnterLane counts against the wait the request has
// already spent.
type laneGate struct {
	fifoGate
	lane Lane
	wait time.Duration

	queueFull, waitExceeded atomic.Uint64
}

// LaneStats is what one lane holds and has refused.
type LaneStats struct {
	Lane Lane
	// Limit is how many requests of the lane may run at once, InFlight how many run now and
	// Queued how many wait for room.
	Limit, InFlight, Queued int
	// QueueFull and WaitExceeded count the requests the lane refused for each reason since the
	// Limiter was made.
	QueueFull, WaitExceeded uint64
}

// LaneStats reads every lane's counts.
func (l *Limiter) LaneStats() []LaneStats {
	stats := make([]LaneStats, 0, len(HeavyReadLanes()))
	for _, lane := range HeavyReadLanes() {
		g := l.lanes[lane]
		used, queued, _ := g.counts()
		stats = append(stats, LaneStats{Lane: lane, Limit: g.limit, InFlight: used,
			Queued: queued, QueueFull: g.queueFull.Load(), WaitExceeded: g.waitExceeded.Load()})
	}
	return stats
}

// EnterLane takes a place in lane for a request that has arrived (Limiter.Arrive) and is about to
// ask for its class's units, and returns the release that gives the place back. A request that
// finds the lane full waits in its queue, in arrival order, holding no admission unit while it
// waits - which is the point of taking the lane before the units: a heavy read waiting for its
// lane costs the other routes nothing. One that finds the queue already holding queueFactor times
// the lane's limit is refused at once, and one whose wait runs out is refused then: either way a
// *Refusal naming the lane, which its caller answers 429 OVERLOADED with Retry-After, as it
// answers a class's refusal.
//
// The wait is what is left of arrival's one wait (the read wait, Limits.ReadWait, for the class
// the lanes belong to), and the time spent here is added to it as a class queue's is: the lane
// and the class queue the request joins next cost it one wait between them, not one each. So a
// read held 0.9 s by a busy lane has 0.1 s left to wait in the read queue rather than a fresh
// second, and a refusal from either comes within the one wait its class states - where a lane
// route refused by its class answered at up to twice that wait (INFRA-592). The wait itself is
// the one both gates take (Arrival.await), and a refusal here is counted against the read class
// as well as the lane, since it spent that class's wait (gate.noteRefusedWait).
//
// A request of NoLane takes nothing and releases nothing.
func (l *Limiter) EnterLane(ctx context.Context, lane Lane, arrival *Arrival) (func(), error) {
	if lane <= NoLane || lane >= laneCount {
		return func() {}, nil
	}
	g := l.lanes[lane]
	leftQueue, err := g.enter(ctx, func(ready <-chan struct{}) (bool, error) {
		return arrival.await(ctx, ready, g.wait, &Refusal{Class: Read, Reason: WaitExceeded, Lane: lane})
	})
	switch {
	case err == nil:
		return func() { g.leave() }, nil
	case err == errGateFull:
		g.queueFull.Add(1)
		return nil, &Refusal{Class: Read, Reason: QueueFull, Lane: lane}
	}
	if _, refused := err.(*Refusal); refused && leftQueue {
		g.waitExceeded.Add(1)
		// The lane spends the read class's wait, so the longest a refused read waited is the
		// longest of either queue (gate.noteRefusedWait).
		l.classes[Read].noteRefusedWait(arrival.waited)
	}
	return nil, err
}
