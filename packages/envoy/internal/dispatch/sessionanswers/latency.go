package sessionanswers

import (
	"sync"
	"time"
)

// SlowRead and SlowWithin are the keyword fill's pause threshold (textversions): the fill pauses
// while Dispatch takes more than SlowRead, 800 ms, to answer an agent's answers read
// (GET /api/v1/agents/{session_id}/answers), and continues once no read has taken that long, or
// run that long, for SlowWithin.
const (
	SlowRead   = 800 * time.Millisecond
	SlowWithin = 10 * time.Second
)

// Latency times the answers reads one process serves, for the keyword fill's pause: the reads
// running, each with the time it began, and the end of the latest read that took longer than
// SlowRead, all under one mutex. The answers route records each read from the moment it enters
// the route's handler chain, as the access-log line times it (Begin, Read.End), and the fill asks
// Slow before each batch. It is safe for many goroutines at once.
type Latency struct {
	now func() time.Time

	mu sync.Mutex
	// running holds the begin of each read that has not ended, by the number Begin gave it;
	// last is the number Begin gave last.
	running map[uint64]time.Time
	last    uint64
	// slowEnd is the end of the latest read that took longer than SlowRead, zero before any did.
	slowEnd time.Time
}

// NewLatency is a Latency that reads the time a read begins and ends from now: time.Now in a
// serving process.
func NewLatency(now func() time.Time) *Latency {
	return &Latency{now: now, running: make(map[uint64]time.Time)}
}

// Read is one answers read a Latency times, from Latency.Begin until its End.
type Read struct {
	latency *Latency
	number  uint64
	began   time.Time
}

// Begin records that an answers read begins now, and answers the Read whose End records that it
// ended.
func (l *Latency) Begin() Read {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last++
	began := l.now()
	l.running[l.last] = began
	return Read{latency: l, number: l.last, began: began}
}

// End records that the read ended now: it runs no longer, and it is the latest slow end when it
// took longer than SlowRead. Each now is read under the mutex, so a later end is never earlier
// than the slow end before it. A read ends once.
func (r Read) End() {
	l := r.latency
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.running, r.number)
	if ended := l.now(); ended.Sub(r.began) > SlowRead {
		l.slowEnd = ended
	}
}

// Slow is whether, at now, a read that took longer than SlowRead ended within the SlowWithin
// before now, or a read has run longer than SlowRead and not ended: the two conditions the keyword
// fill pauses on. A read that ran that long ends as a slow end, so Slow is false only once neither
// condition has held for the last SlowWithin.
func (l *Latency) Slow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.slowEnd.IsZero() && now.Sub(l.slowEnd) < SlowWithin {
		return true
	}
	for _, began := range l.running {
		if now.Sub(began) > SlowRead {
			return true
		}
	}
	return false
}
