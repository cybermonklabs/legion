// flight.go holds the shared sync the callers of one architecture source join: the counter Close
// waits on, what a flight hands every caller that joined it, the newest ended flight of each
// source while a call of it runs, and the flight itself — the contexts it runs on and its
// errSuperseded retry chain.
// The Importer type, Sync and Close stay in importer.go: they are the type's own surface.

package architecture

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// syncCount counts the flights running and waits for none to be left. It has
// the shape of backgroundWork in api/server.go, and for its reason: a
// sync.WaitGroup is not it, because a flight may start while a wait is
// already waiting, and Add after Wait began is what a WaitGroup forbids.
// begin refuses once Close has begun, so a flight whose goroutine starts
// after Close took its decision does nothing rather than write into a
// closing pool.
type syncCount struct {
	mu      sync.Mutex
	closing bool
	running int
	// idle is closed when running last fell to zero, and replaced when a
	// flight starts again.
	idle chan struct{}
}

func (c *syncCount) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	if c.running == 0 {
		c.idle = make(chan struct{})
	}
	c.running++
	return true
}

func (c *syncCount) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running--
	if c.running == 0 {
		close(c.idle)
	}
}

// stop refuses every later flight and waits for the running ones to end.
func (c *syncCount) stop() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	for {
		c.mu.Lock()
		running, idle := c.running, c.idle
		c.mu.Unlock()
		if running == 0 {
			return
		}
		<-idle
	}
}

// syncFlight is one shared sync of one source: what the flight hands every
// caller that joined it, beside the flight's error. Every write to it happens
// in the flight's goroutine before the result is sent, and before the flight
// records itself as its source's newest ended flight (endedFlights), and every
// read after that receive or that record's read, so the channel and the
// record's mutex order them: the flight itself needs no mutex, and -race is
// quiet.
type syncFlight struct {
	// startedAt is when the flight began, its very first act, so that even a
	// flight which does nothing answers a time no earlier than its callers'
	// and so ends their loop.
	startedAt time.Time
	// headAt is when the flight issued its branch-head lookup — the moment the
	// lookup went out, never the moment it answered, because the head may have
	// moved in between. A retry round stamps it again.
	headAt time.Time
	source model.ArchitectureSource
}

// servedAt is how fresh the flight's answer is: when it asked GitHub for the
// head, or, for a flight that failed before asking, when it began.
func (f *syncFlight) servedAt() time.Time {
	if f.headAt.IsZero() {
		return f.startedAt
	}
	return f.headAt
}

// endedFlights holds, for each source a call of Sync is running for, the
// newest flight of that source to have ended, with its error, by the key Sync
// shares flights on. One flight of a key runs at a time (singleflight), and
// each records itself here before singleflight drops its key and sends its
// answer, so while a call runs, the newest flight of its source is either the
// one in flight or the one recorded here: a caller that looks for one finds
// it, however late it looks.
//
// A key's entry lives only while a call of that key runs: the first call to
// enter makes it, and the last to leave drops it, with what it recorded. So
// the record never holds more entries than there are calls running, whatever
// error a flight ended with and whatever key a caller names, a key that names
// no source among them. No caller loses by the drop: a flight is fresh only
// for a call made before its head lookup, and a call that is still running
// holds its key's entry open. A flight that ends once every call of its key
// has left is recorded nowhere. Its zero value is ready.
type endedFlights struct {
	mu    sync.Mutex
	byKey map[string]*keyFlights
}

// keyFlights is one key's entry: how many calls of it are running, and its
// newest ended flight, nil until one ends while a call runs.
type keyFlights struct {
	calls int
	ended *endedFlight
}

// endedFlight is one flight that has ended and the error it ended with.
type endedFlight struct {
	flight *syncFlight
	err    error
}

// enter counts one more running call of key, making its entry if it has none.
func (e *endedFlights) enter(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byKey == nil {
		e.byKey = map[string]*keyFlights{}
	}
	entry := e.byKey[key]
	if entry == nil {
		entry = &keyFlights{}
		e.byKey[key] = entry
	}
	entry.calls++
}

// leave counts one call of key fewer, and drops key's entry once none runs.
func (e *endedFlights) leave(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.byKey[key]
	entry.calls--
	if entry.calls == 0 {
		delete(e.byKey, key)
	}
}

// fresh is key's newest ended flight, and whether there is one whose head
// lookup came no earlier than calledAt.
func (e *endedFlights) fresh(key string, calledAt time.Time) (endedFlight, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry := e.byKey[key]
	if entry == nil || entry.ended == nil {
		return endedFlight{}, false
	}
	return *entry.ended, !entry.ended.flight.servedAt().Before(calledAt)
}

// record stores flight, which ended with err, as key's newest ended flight
// while a call of key runs, and drops it when none does.
func (e *endedFlights) record(key string, flight *syncFlight, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if entry := e.byKey[key]; entry != nil {
		entry.ended = &endedFlight{flight: flight, err: err}
	}
}

// sharedFlight is what a flight of key, started by a caller of Sync that asked
// at calledAt, runs: the newest flight of the source to have ended, when its
// head lookup came no earlier than calledAt, and otherwise a flight of its own,
// which it records as the newest once it ends. So a caller that refused a
// flight older than its call and looks again after the next one has ended takes
// that one's answer, as it would have had it joined it, and makes no GitHub
// call of its own.
func (i *Importer) sharedFlight(ctx context.Context, key, project, repo string, calledAt time.Time) (*syncFlight, error) {
	if ended, ok := i.ended.fresh(key, calledAt); ok {
		return ended.flight, ended.err
	}
	flight, err := i.flight(ctx, project, repo)
	i.ended.record(key, flight, err)
	return flight, err
}

// flight is the shared sync singleflight runs for one source: one set of
// GitHub calls and one errSuperseded retry chain, however many callers joined
// it. ctx is whichever caller started it, and only its values are kept.
func (i *Importer) flight(ctx context.Context, project, repo string) (*syncFlight, error) {
	flight := &syncFlight{startedAt: time.Now()}
	if !i.running.begin() {
		return flight, i.owner.Err()
	}
	defer i.running.done()
	// The starter is one of several callers and may leave before the others,
	// so its cancellation is dropped: one hang-up fails nobody else's sync.
	detached := context.WithoutCancel(ctx)
	// A sync opens its own transactions on the shared pool, so it is marked like an API
	// request: a read it makes while one is open must come from that transaction, never from a
	// second connection (store.ErrNestedAcquire). The mark has to be the flight's own, because
	// WithoutCancel keeps the starter's values: sharing an HTTP request's mark would make
	// SyncProject's next Sync a nested acquire, and WithTransactionTracking, a no-op when a
	// mark is already present, would hand that shared one back.
	marked, err := store.ForConcurrentRead(detached)
	if err != nil {
		return flight, err
	}
	base, abandon := context.WithCancel(marked)
	defer abandon()
	// Nothing but the process stopping ends a flight early.
	stop := context.AfterFunc(i.owner, abandon)
	defer stop()
	// The deadline is the flight's own, counted from when it began rather than
	// from when its first caller arrived.
	syncCtx, cancel := context.WithTimeout(base, syncTimeout)
	defer cancel()
	// Each try runs in a held scope (githubreads.Hold) of the attempt syncCtx carries when a
	// leased import started the flight, since syncCtx keeps the starter's values: a try another
	// write superseded is asked again, so only the last try that reached GitHub counts. syncOnce
	// notes on try alone: base, its parent, is only where it records a failure on the source's
	// row once try has ended.
	//
	// standing is the scope of the newest try that reached GitHub (githubreads.Held.Reached),
	// unsettled: a later try that reaches GitHub drops it, since only that try's outcome replaces
	// its notes, and the flight's end keeps it. A try that reached nothing holds nothing, and is
	// dropped at once: a retry can end before it asks GitHub anything, at its load on the
	// flight's spent deadline or on a database error, and a flight that ended so with the
	// superseded try's failure dropped would record a GitHub read that failed as one that
	// succeeded.
	var standing *githubreads.Held
	for {
		try, held := githubreads.Hold(syncCtx)
		source, err := i.syncOnce(try, base, project, repo, flight)
		if held.Reached() {
			standing.Drop()
			standing = held
		} else {
			held.Drop()
		}
		if errors.Is(err, errSuperseded) {
			continue
		}
		standing.Keep()
		flight.source = source
		return flight, err
	}
}
