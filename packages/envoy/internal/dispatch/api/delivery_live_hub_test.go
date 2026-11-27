package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// hubRig is a delivery hub on the API test server: the handler a test writes through, the store
// behind it, and the hub with the event stream relay it reads its events from, both run on one
// lifetime of their own, which the test's cleanup ends and waits out.
type hubRig struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	hub      *deliveryHub
	relay    *eventStreamRelay
	// hubStore is the store the hub reads through: deps' own, or one of its own on the same
	// database (newMeasuredHubRig), so that a test measuring what an answer costs counts the
	// hub's connections alone.
	hubStore *store.Store
	// clock is the hub's clock; a test dates the rows it writes from it, so where they fall in a
	// view's window is the test's choice rather than the wall clock's.
	clock *fakeHubClock

	mu    sync.Mutex
	notes []deliveryHubNote
	stop  func()
}

// newHubRig starts a hub on a new test server, on a clock that runs with the wall clock from noon
// UTC (newNoonHubClock); configure, when given, sets the hub's seams before it starts.
func newHubRig(t *testing.T, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, newNoonHubClock(), configure)
}

// notifyChannel sends one notification on channel, as a trigger's commit does: the hub hears it
// on the connection its listener holds (pglisten.Listener).
func notifyChannel(t *testing.T, database *store.Store, channel, payload string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, channel, payload); err != nil {
		t.Fatalf("pg_notify(%q, %q): %v", channel, payload, err)
	}
}

// newClockedHubRig is newHubRig on a clock the test moves.
func newClockedHubRig(t *testing.T, clock *fakeHubClock) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, clock, nil)
}

// newMeasuredHubRig is newClockedHubRig with the hub reading through a pool of its own on the same
// database, which hubAcquires counts: the relay reads the event log on the shared pool, as the
// rest of the process does, and its reads are none of the hub's. So a test that measures what an
// answer costs the database counts the hub's own reads and waits for no other reader.
func newMeasuredHubRig(t *testing.T, clock *fakeHubClock) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOnStore(t, handler, database, deps, storetest.SecondProcess(t, database), clock, nil)
}

// fakeHubClock is the real clock moved forward by Advance: Now is the wall clock plus its offset,
// which every advance grows, and a timer fires once the wall clock plus the offset reaches its
// moment, so a test moves the hub past a threshold or a midnight without waiting for it. A timer
// armed for a moment the clock has already reached fires at once, however far an Advance moved it
// before the arm. A clock from newNoonHubClock starts with the offset that makes it read noon UTC,
// and runs with the wall clock from there. A clock from newStillHubClock stands still between
// advances instead: Now is noon UTC plus the advances. Its timers still fire on the wall clock as
// well, but the hub's one timer only wakes its worker, which compares each view's due moment with
// Now (deliveryHub.work, runDue), so how long the test's own writes take decides nothing.
type fakeHubClock struct {
	mu     sync.Mutex
	still  time.Time
	offset time.Duration
	timers []*fakeHubTimer
	// beforeArm, when set, runs at each arm, before the timer is placed, until it answers true: a
	// test's Advance landing between the worker's read of the clock and its arm.
	beforeArm func() bool
	// firing counts the timer callbacks that have won their timer's one run and not yet returned,
	// and returned is signalled as each returns (waitCallbacks).
	firing   int
	returned *sync.Cond
}

type fakeHubTimer struct {
	clock *fakeHubClock
	at    time.Time
	f     func()
	real  *time.Timer
	done  atomic.Bool
}

// noonToday is noon UTC of the present's day. Every preset view is due at the next UTC midnight
// (nextExpiry) and sends a delta then, so a hub clock started at noon is 12 hours from the one
// delta no test that reads its view's next delta expects.
func noonToday() time.Time { return nextUTCMidnight(time.Now()).Add(-12 * time.Hour) }

// newNoonHubClock is a fakeHubClock that reads noon UTC now and runs with the wall clock from there.
func newNoonHubClock() *fakeHubClock { return &fakeHubClock{offset: time.Until(noonToday())} }

// newStillHubClock is a fakeHubClock standing still at noon UTC until Advance moves it.
func newStillHubClock() *fakeHubClock { return &fakeHubClock{still: noonToday()} }

func (c *fakeHubClock) nowLocked() time.Time {
	if c.still.IsZero() {
		return time.Now().UTC().Add(c.offset)
	}
	return c.still.Add(c.offset)
}

func (c *fakeHubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked()
}

// standsStill is whether the clock moves only by Advance, so a view's recompute, due on this
// clock, waits for the test rather than for the wall clock.
func (c *fakeHubClock) standsStill() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.still.IsZero()
}

func (c *fakeHubClock) At(at time.Time, f func()) hubTimer {
	c.mu.Lock()
	hook := c.beforeArm
	c.mu.Unlock()
	cleared := hook != nil && hook()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cleared {
		c.beforeArm = nil
	}
	timer := &fakeHubTimer{clock: c, at: at, f: f}
	timer.real = time.AfterFunc(at.Sub(c.nowLocked()), timer.fire)
	c.timers = append(c.timers, timer)
	return timer
}

// onArm runs f inside each of the hub's arms of its timer, before the timer is placed, until f
// answers true.
func (c *fakeHubClock) onArm(f func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beforeArm = f
}

// returnedLocked is the condition waitCallbacks waits on, made on first use, with c.mu held.
func (c *fakeHubClock) returnedLocked() *sync.Cond {
	if c.returned == nil {
		c.returned = sync.NewCond(&c.mu)
	}
	return c.returned
}

// waitCallbacks returns once no timer callback that has begun is still running. A callback of an
// earlier arm that has won its timer's one run (fire) can be descheduled before it sends its wake,
// and a timer's Stop cannot take that run back; a wake it sends after a test read the worker's
// wakes as spent would wake the worker in place of the timer the test is about.
func (c *fakeHubClock) waitCallbacks() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.firing > 0 {
		c.returnedLocked().Wait()
	}
}

// fire runs the timer's callback once, whether the real timer or an Advance comes first. Its win
// of that one run and the count of callbacks under way change together under the clock's lock, as
// Stop's win does, so waitCallbacks sees every callback that has won and not yet returned.
func (t *fakeHubTimer) fire() {
	c := t.clock
	c.mu.Lock()
	won := t.done.CompareAndSwap(false, true)
	if won {
		c.firing++
	}
	c.mu.Unlock()
	if !won {
		return
	}
	t.f()
	c.mu.Lock()
	c.firing--
	c.returnedLocked().Broadcast()
	c.mu.Unlock()
}

func (t *fakeHubTimer) Stop() bool {
	t.real.Stop()
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	return t.done.CompareAndSwap(false, true)
}

// Advance moves the clock forward by d and fires every timer whose moment it reached.
func (c *fakeHubClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	now := c.nowLocked()
	var due []*fakeHubTimer
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if timer.done.Load() {
			continue
		}
		if !timer.at.After(now) {
			due = append(due, timer)
			continue
		}
		kept = append(kept, timer)
	}
	c.timers = kept
	c.mu.Unlock()
	for _, timer := range due {
		timer.real.Stop()
		timer.fire()
	}
}

// hubOn builds a hub and its relay on an existing test server, as a second Dispatch task's would
// be. Nothing runs until the first Subscribe starts a generation, which takes the relay's feed on
// a goroutine of its own; a test that must have the hub holding it, and acting on everything its
// setup wrote, waits with settle.
func hubOn(t *testing.T, handler http.Handler, database *store.Store, deps Deps, clock *fakeHubClock, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	return hubOnStore(t, handler, database, deps, deps.Store, clock, configure)
}

// hubOnStore is hubOn with the hub reading through hubStore: deps' own store, but for a test that
// counts the hub's connections (newMeasuredHubRig), which gives it a pool of its own. The relay
// keeps deps' store either way, since it is the process's one reader of the event log.
func hubOnStore(t *testing.T, handler http.Handler, database *store.Store, deps Deps, hubStore *store.Store, clock *fakeHubClock, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	rig := &hubRig{handler: handler, database: database, deps: deps, hubStore: hubStore, clock: clock}
	lifetime, cancel := context.WithCancel(context.Background())
	served := directServer(deps)
	rig.relay = newEventStreamRelay(served, lifetime)
	served.eventRelay = rig.relay
	hubDeps := deps
	hubDeps.Store = hubStore
	hubServed := directServer(hubDeps)
	hubServed.eventRelay = rig.relay
	rig.hub = newDeliveryHub(hubServed, lifetime, clock)
	rig.hub.observe = func(note deliveryHubNote) {
		rig.mu.Lock()
		rig.notes = append(rig.notes, note)
		rig.mu.Unlock()
	}
	if configure != nil {
		configure(rig.hub)
	}
	rig.stop = sync.OnceFunc(func() {
		cancel()
		rig.hub.wait()
		rig.relay.wait()
	})
	t.Cleanup(rig.stop)
	return rig
}

// hubAcquires is how many connections the hub's own pool has handed out so far.
func (r *hubRig) hubAcquires() int64 { return r.hubStore.Pool.Stat().AcquireCount() }

// changeCursor is where the running generation stands in the delivery change log.
func (r *hubRig) changeCursor() int64 {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	return r.hub.generation.change.Seq
}

// settle waits until the hub has acted on everything the test's setup wrote, so none of it counts
// against the measurement that follows: its change cursor at the delivery change log's newest
// entry (settings and pull requests seeded through saveDeliveryRule and its kin), its feed at
// or past the event log's head, and then nothing owed, which is what waitNothingOwed reads. The
// hub takes delivery changes independently of the safe horizon the relay reads events up to, and
// an event write of the test's database still open, the server's own on a loaded machine, can keep
// that horizon below the setup's events for a while, so either catch-up can land after the test's
// snapshots. On a clock standing still a dirty view the clock has not made due is owed nothing
// (viewOwes): such a test moves the clock itself and takes what the recompute then sends. Each wait
// ends on what it waits for, and its one bound is the test binary's deadline.
func (r *hubRig) settle(t *testing.T) {
	t.Helper()
	head, err := delivery.ChangesHead(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the newest delivery change: %v", err)
	}
	latest := head.Seq
	testwait.Until(t, fmt.Sprintf("the hub's change cursor to reach %d, the newest delivery change", latest), func() (int64, bool) {
		seq := r.changeCursor()
		return seq, seq >= latest
	})
	r.waitForEventCursor(t, eventHead(t, r.database), "the log's head before the measurement")
	r.waitNothingOwed(t, latest)
}

// waitNothingOwed waits until the hub owes no work for anything committed up to latest: nothing
// the intake has taken in and not run yet (intakeOwes), its change cursor at latest, and no view
// it will recompute or narrow without the test moving its clock (viewOwes), the last two read in
// one hold of its lock. A change marks the views it touches inside the same critical section that
// moves the cursor (catchUpChanges), a recompute clears the mark inside its read, once it holds
// the position it answers for, and computing stays set until that recompute has ended, in the
// hold of the lock that sends its frames. So a hub that owes nothing here has no recompute left
// to run that would read what the test writes next and send it as a change its subs had already
// heard, and every frame it owed its subs is queued on them.
//
// The intake is read first and the views last, with a barrier between them: the relay's feed is
// applied on the intake's goroutine and marks every view dirty there (applyFeed), so a read of
// the views taken before the "events seeded" note can hold none of those marks and find a hub
// that owes nothing while the apply is still to come. deliveryHub.catchUpNow runs on that same
// goroutine and returns once it has, so everything the intake had taken in when intakeOwes read
// it - that apply among them - has run before the views are read. The wait's one bound is the
// test binary's deadline.
func (r *hubRig) waitNothingOwed(t *testing.T, latest int64) {
	t.Helper()
	owed := func() (int64, []string) {
		now, still := r.clock.Now(), r.clock.standsStill()
		r.hub.mu.Lock()
		defer r.hub.mu.Unlock()
		generation := r.hub.generation
		var busy []string
		for key, view := range generation.views {
			if what := viewOwes(view, now, still); what != "" {
				busy = append(busy, key+"("+what+")")
			}
		}
		slices.Sort(busy)
		return generation.change.Seq, busy
	}
	testwait.Until(t, fmt.Sprintf("the hub to owe nothing for the delivery changes through %d (what it still owed)", latest), func() (string, bool) {
		intake := r.intakeOwes()
		r.hub.catchUpNow()
		changeSeq, busy := owed()
		return fmt.Sprintf("change cursor %d of %d, views owing work %v, the intake owes %q", changeSeq, latest, busy, intake),
			intake == "" && changeSeq >= latest && len(busy) == 0
	})
}

// viewOwes names what the hub will do to view without the test moving its clock, and is empty
// when it will do nothing: a recompute under way; a dirty view, whose recompute a clock running
// with the wall clock makes due of itself; a dirty view the clock standing still has made due;
// and an answer whose expiry the clock has reached. A dirty view on a clock standing still that
// the clock has not made due is owed nothing here: its recompute waits for the test's Advance.
func viewOwes(view *deliveryView, now time.Time, still bool) string {
	if view.computing {
		return "computing"
	}
	at, _, due := view.dueAt()
	switch {
	case !due:
		return ""
	case view.dirty && !still:
		return "dirty"
	case at.After(now):
		return ""
	case view.dirty:
		return "dirty and due"
	}
	return "its answer's expiry reached"
}

// requireNothingSent waits until the hub owes nothing for everything committed so far (settle),
// then reads conn, which must hold no frame: every frame a recompute or an expiry sends is queued
// on the conn in the hold of the hub's lock that clears what settle waits on, so a frame still
// there is one nobody should be sent rather than one yet to come. after names what the conn was
// read after. A verdict a delivery_freshness notification changes is sent where the hub applies
// it, which settle does not see: a caller that wrote one first waits for it (waitFreshnessHeard).
func (r *hubRig) requireNothingSent(t *testing.T, conn *fakeLiveConn, after string) {
	t.Helper()
	r.settle(t)
	conn.empty(t, after)
}

// waitFreshnessHeard waits until the freshness the hub has heard matches, which what names. The
// hub applies a delivery_freshness notification and sends the frame a changed verdict needs in one
// hold of its lock (applyFreshness), so once it has heard one, every frame that notification sends
// is queued on the open conns.
func (r *hubRig) waitFreshnessHeard(t *testing.T, what string, matches func(deliveryFreshnessHeard) bool) {
	t.Helper()
	testwait.Until(t, what, func() (deliveryFreshnessHeard, bool) {
		r.hub.mu.Lock()
		defer r.hub.mu.Unlock()
		heard := r.hub.generation.fresh
		return heard, matches(heard)
	})
}

// reconciledAt matches the freshness of a reconcile pass recorded at or after at. Postgres keeps
// the moment to the microsecond, so what the hub hears is never before at truncated to one.
func reconciledAt(at time.Time) func(deliveryFreshnessHeard) bool {
	return func(heard deliveryFreshnessHeard) bool {
		return heard.lastReconcileAt != nil && !heard.lastReconcileAt.Before(at.Truncate(time.Microsecond))
	}
}

// intakeOwes names what the intake has been handed and has still to run, and is empty when it has
// nothing: work neither the views nor the change cursor shows, each piece of which marks views
// when it does run.
//
// A wake the listener left the intake is a signal on a channel of one slot, which the test reads
// as it is left: for a change notification (onNotify) or for a connection whose LISTEN covers the
// notifications sent before it. The catch-up that either wake runs reads the log afresh, so it
// can mark a view for a change committed since the cursor reached latest.
//
// The relay subscription is taken on a goroutine of its own and handed to the intake on g.feeds
// (seedEvents), and applying it marks every view dirty (applyFeed). Until the "events seeded"
// note that apply ends with, the hand-over may be anywhere between the subscribe and the apply,
// which the channel alone would not show; settle's wait on the event cursor passes it by whenever
// the event log is empty, since then there is no event to wait for.
func (r *hubRig) intakeOwes() string {
	r.hub.mu.Lock()
	generation := r.hub.generation
	r.hub.mu.Unlock()
	seeded := func(note deliveryHubNote) bool { return note.what == "events seeded" }
	switch {
	case len(generation.wakeChanges) > 0:
		return "a catch-up of the change log"
	case len(generation.connected) > 0:
		return "the catch-up a connected listener is owed"
	case r.count(0, seeded) == 0:
		return "the relay's feed, whose apply marks every view dirty"
	}
	return ""
}

// eventsThrough is how far the hub's events have come: the highest event it has applied, or the
// head its feed began at, whose events its first read of the database already held.
func (r *hubRig) eventsThrough() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var highest int64
	for _, note := range r.notes {
		if note.what == "events seeded" || note.what == "event" {
			highest = max(highest, note.eventID)
		}
	}
	return highest
}

// eventsApplied is the events the hub has applied since mark, in the order it applied them.
func (r *hubRig) eventsApplied(since int) []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var applied []int64
	for _, note := range r.notes[since:] {
		if note.what == "event" {
			applied = append(applied, note.eventID)
		}
	}
	return applied
}

// cursors is where the hub stands in both logs: its generation's delivery change cursor, and how
// far its events have come.
func (r *hubRig) cursors() (changeSeq, eventCursor int64) {
	return r.changeCursor(), r.eventsThrough()
}

// waitForEventCursor waits until the hub holds every event through want. The relay publishes an
// event to the hub only once the server's event horizon has passed it, and an open event write of
// the server's database holds that back, so the wait is the horizon's (horizontest.PollInProcess).
func (r *hubRig) waitForEventCursor(t *testing.T, want int64, what string) {
	t.Helper()
	horizontest.PollInProcess(t, fmt.Sprintf("the hub's event log through %d (%s)", want, what), r.database, func() bool {
		return r.eventsThrough() >= want
	})
}

// waitNoRecompute waits until the hub has begun or ended no recompute for quiet (awaitStill). It
// counts no other note: while a gap stands open above the safe horizon, which an event write of the
// test's database keeps open for as long as it stays open, the relay re-reads the log every 10-250
// ms, and a wait for no note at all could last as long as that write.
func (r *hubRig) waitNoRecompute(t *testing.T, quiet time.Duration) {
	t.Helper()
	awaitStill(t, "the hub's recompute notes", quiet, func() int64 {
		r.mu.Lock()
		defer r.mu.Unlock()
		var n int64
		for _, note := range r.notes {
			if note.what == "recompute begin" || note.what == "recompute end" {
				n++
			}
		}
		return n
	})
}

// isP0 is whether the hub's generation holds key among the open P0 issues, which it keeps from the
// issue events it acts on (applyEventLocked).
func (r *hubRig) isP0(key string) bool {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	_, p0 := r.hub.generation.p0[key]
	return p0
}

// mark is how many notes the hub has made, for counting the ones after it.
func (r *hubRig) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notes)
}

// count is how many notes since mark match.
func (r *hubRig) count(since int, match func(deliveryHubNote) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, note := range r.notes[since:] {
		if match(note) {
			n++
		}
	}
	return n
}

// waitFor waits for a note since mark that matches; what names the note. Its one bound is the test
// binary's deadline (waitUntil).
func (r *hubRig) waitFor(t *testing.T, since int, what string, match func(deliveryHubNote) bool) {
	t.Helper()
	waitUntil(t, what+" (a note of the hub's)", func() bool { return r.count(since, match) > 0 })
}

// waitForEvent waits for a note since mark that matches, made as the hub acted on what its event
// feed carried: that feed stands behind the server's event horizon, so the wait is the horizon's.
func (r *hubRig) waitForEvent(t *testing.T, since int, what string, match func(deliveryHubNote) bool) {
	t.Helper()
	horizontest.PollInProcess(t, what, r.database, func() bool { return r.count(since, match) > 0 })
}

func recomputeBegin(cause string) func(deliveryHubNote) bool {
	return func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && (cause == "" || note.cause == cause)
	}
}

// fakeLiveConn is a socket as the hub sees it, recording every frame it is sent and when.
type fakeLiveConn struct {
	frames chan sentFrame
	full   atomic.Bool
}

// sentFrame is one frame the hub sent a fakeLiveConn, and the moment Send took it: when the hub
// sent it, whatever later moment the test reads it at.
type sentFrame struct {
	frame any
	at    time.Time
}

func newFakeLiveConn() *fakeLiveConn { return &fakeLiveConn{frames: make(chan sentFrame, 256)} }

func (c *fakeLiveConn) Send(frame any) bool {
	if c.full.Load() {
		return false
	}
	select {
	case c.frames <- sentFrame{frame: frame, at: time.Now()}:
		return true
	default:
		return false
	}
}

// next is the next frame the conn is sent. Its one bound is the test binary's deadline
// (testwait.Receive).
func (c *fakeLiveConn) next(t *testing.T, what string) any {
	t.Helper()
	return testwait.Receive(t, what, c.frames).frame
}

// eventNext is the next frame the conn is sent once the hub has acted on an event of its feed,
// which stands behind database's event horizon, so the wait is the horizon's.
func (c *fakeLiveConn) eventNext(t *testing.T, database *store.Store, what string) any {
	t.Helper()
	return horizontest.WaitInProcess(t, what, c.frames, database).frame
}

// none fails when the conn is sent a frame within within.
func (c *fakeLiveConn) none(t *testing.T, within time.Duration, after string) {
	t.Helper()
	select {
	case sent := <-c.frames:
		raw, _ := json.Marshal(sent.frame)
		t.Fatalf("after %s, a frame nobody should be sent: %s", after, raw)
	case <-time.After(within):
	}
}

// empty fails when the conn holds a frame. Its caller has waited for the fact that would have
// sent one, the hub owing nothing (hubRig.requireNothingSent) or its having heard a notification
// (hubRig.waitFreshnessHeard), and every frame is queued before that fact holds, so a frame here
// is one nobody should be sent rather than one still to come. after names what the conn was read
// after.
func (c *fakeLiveConn) empty(t *testing.T, after string) {
	t.Helper()
	select {
	case sent := <-c.frames:
		raw, _ := json.Marshal(sent.frame)
		t.Fatalf("after %s, a frame nobody should be sent: %s", after, raw)
	default:
	}
}

func (c *fakeLiveConn) snapshot(t *testing.T, sub string) DeliveryLiveSnapshot {
	t.Helper()
	frame := c.next(t, "the snapshot of "+sub)
	snapshot, ok := frame.(DeliveryLiveSnapshot)
	if !ok || snapshot.Sub != sub {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want the snapshot of %s", raw, sub)
	}
	return snapshot
}

func (c *fakeLiveConn) delta(t *testing.T, sub string) DeliveryLiveDelta {
	t.Helper()
	delta, _ := c.deltaSent(t, sub)
	return delta
}

// deltaSent is the next frame the conn is sent, which must be a delta of sub, and the moment the
// hub sent it (sentFrame), for a verdict on how soon the hub sent it rather than on how soon the
// test got round to reading it.
func (c *fakeLiveConn) deltaSent(t *testing.T, sub string) (DeliveryLiveDelta, time.Time) {
	t.Helper()
	sent := testwait.Receive(t, "a delta of "+sub, c.frames)
	delta, ok := sent.frame.(DeliveryLiveDelta)
	if !ok || delta.Sub != sub {
		raw, _ := json.Marshal(sent.frame)
		t.Fatalf("frame = %s, want a delta of %s", raw, sub)
	}
	return delta, sent.at
}

// eventDelta is eventNext, which must be a delta of sub.
func (c *fakeLiveConn) eventDelta(t *testing.T, database *store.Store, sub string) DeliveryLiveDelta {
	t.Helper()
	frame := c.eventNext(t, database, "a delta of "+sub)
	delta, ok := frame.(DeliveryLiveDelta)
	if !ok || delta.Sub != sub {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want a delta of %s", raw, sub)
	}
	return delta
}

func (c *fakeLiveConn) refusal(t *testing.T, sub, code string) LiveError {
	t.Helper()
	frame := c.next(t, code+" for "+sub)
	refusal, ok := frame.(LiveError)
	if !ok || refusal.Sub != sub || refusal.Code != code {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want %s for %s", raw, code, sub)
	}
	return refusal
}

// firstSnapshots reads the first answers of subs, every one subscribed on conn before the hub knew a
// verdict, in whichever order the hub's recomputes, one at a time, send them: the first snapshot
// alone, since its read sets the hub's first verdict silently, and each later one followed by that
// verdict (Subscribe). It returns the snapshots by sub.
func (c *fakeLiveConn) firstSnapshots(t *testing.T, subs ...string) map[string]DeliveryLiveSnapshot {
	t.Helper()
	snapshots := map[string]DeliveryLiveSnapshot{}
	for len(snapshots) < len(subs) {
		frame := c.next(t, "each view's snapshot")
		snapshot, ok := frame.(DeliveryLiveSnapshot)
		if _, twice := snapshots[snapshot.Sub]; !ok || twice || !slices.Contains(subs, snapshot.Sub) {
			raw, _ := json.Marshal(frame)
			t.Fatalf("frame = %s, want the first snapshot of one of %v", raw, subs)
		}
		snapshots[snapshot.Sub] = snapshot
		if len(snapshots) > 1 {
			c.freshness(t, "the verdict after the snapshot of "+snapshot.Sub)
		}
	}
	return snapshots
}

// The views the tests subscribe to.
func timelineRange(from, to time.Time, query string) DeliveryLiveView {
	f, tt := from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)
	return DeliveryLiveView{Kind: deliveryLiveTimeline, Window: &DeliveryLiveWindow{From: &f, To: &tt}, Query: &query}
}

func measuresRange(from, to time.Time, query string) DeliveryLiveView {
	view := timelineRange(from, to, query)
	view.Kind = deliveryLiveMeasures
	return view
}

func presetView(kind string, days int, query string) DeliveryLiveView {
	return DeliveryLiveView{Kind: kind, Window: &DeliveryLiveWindow{Preset: &days}, Query: &query}
}

func livePipelineView(at *time.Time) DeliveryLiveView {
	if at == nil {
		return DeliveryLiveView{Kind: deliveryLivePipeline}
	}
	s := at.Format(time.RFC3339Nano)
	return DeliveryLiveView{Kind: deliveryLivePipeline, At: &s}
}

// subscribeFrame is a subscribe frame for a delivery view as the socket hands it to the hub.
func subscribeFrame(sub string, view DeliveryLiveView) deliverySubscription {
	return deliverySubscription{Sub: sub, View: view}
}

// seedHubSettings writes the delivery settings the hub tests run under.
func seedHubSettings(t *testing.T, database *store.Store) {
	t.Helper()
	saveDeliveryRule(t, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	})
}

// hubPR is acme/widgets#n by octocat, opened two hours before it merged at merged.
func hubPR(n int, merged time.Time) delivery.DeliveryPullRequest {
	opened := merged.Add(-2 * time.Hour)
	return delivery.DeliveryPullRequest{
		Repo: "acme/widgets", Number: n, Title: "feat: change " + strconv.Itoa(n),
		URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(n), Author: "octocat",
		MergedAt: &merged, CreatedAt: &opened,
	}
}

func upsertPR(t *testing.T, database *store.Store, pr delivery.DeliveryPullRequest) {
	t.Helper()
	if err := delivery.UpsertPullRequest(context.Background(), database.Pool, pr); err != nil {
		t.Fatalf("upsert %s#%d: %v", pr.Repo, pr.Number, err)
	}
}

// upsertDeploy writes deploy run id on main with its production job, headed at head and done at done.
func upsertDeploy(t *testing.T, database *store.Store, id int64, head, done time.Time) {
	t.Helper()
	ctx := context.Background()
	success := delivery.DeliveryRunConclusionSuccess
	jobSuccess := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		HeadCommitAt: head, StartedAt: head, CompletedAt: &done, Conclusion: &success,
		URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(id, 10), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert deploy run %d: %v", id, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, "acme/widgets", id, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: id, Name: "release / release", StartedAt: &head, CompletedAt: &done, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("upsert deploy run %d's job: %v", id, err)
	}
}

// getBody is the GET route's body for target.
func getBody(t *testing.T, handler http.Handler, target string) json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
	}
	return json.RawMessage(response.Body.Bytes())
}

// windowTarget is the GET route of kind for what a body answered, built of what that route reads.
// A timeline or measures body names its window, and query is the rest of what its route reads; a
// legion body names its project and a legion_issue body its issue, and each of their routes reads
// its path alone, so neither takes a query. A pipeline body can name neither: its route reads `at`
// and nothing else, and the body's days carry only at's UTC day while its failing-jobs table looks
// 24 hours back from at itself, so query is the view's own `at=`, and a call without one fails
// rather than read the present on both sides.
func windowTarget(t *testing.T, kind string, body json.RawMessage, query string) string {
	t.Helper()
	switch kind {
	case legionBoardView:
		if query != "" {
			t.Fatalf("the board's route reads its path alone, so query %q would be read by nothing", query)
		}
		return "/api/v1/projects/" + legionBodyProject(t, body) + "/legion/board"
	case legionIssueView:
		if query != "" {
			t.Fatalf("one issue's Legion route reads its path alone, so query %q would be read by nothing", query)
		}
		var shape struct {
			Issue string `json:"issue"`
		}
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Fatalf("read the legion_issue body's issue: %v", err)
		}
		return "/api/v1/issues/" + shape.Issue + "/legion"
	case deliveryLivePipeline:
		if values, err := url.ParseQuery(query); err != nil || len(values) != 1 || values.Get("at") == "" {
			t.Fatalf("the pipeline route reads at alone and a pipeline body names none, so query must be the view's at=, not %q", query)
		}
		return "/api/v1/delivery/pipeline?" + query
	}
	var shape struct {
		Window delivery.DeliveryWindowView `json:"window"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the body's window: %v", err)
	}
	target := "/api/v1/delivery/" + kind + "?from=" + shape.Window.From.Format(time.RFC3339Nano) + "&to=" + shape.Window.To.Format(time.RFC3339Nano)
	if query != "" {
		target += "&" + query
	}
	return target
}

// withoutComputedAt is a body decoded, without the computed_at it moves on every compute.
func withoutComputedAt(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	delete(decoded, "computed_at")
	if measures, ok := decoded["measures"].(map[string]any); ok {
		delete(measures, "computed_at")
	}
	return decoded
}

func requireSameBody(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	if g, w := withoutComputedAt(t, got), withoutComputedAt(t, want); !reflect.DeepEqual(g, w) {
		t.Fatalf("%s: the live body differs from the GET route's:\nlive: %s\nGET:  %s", what, got, want)
	}
}

// requireLandsOnGET applies delta to the body held as a page applies it and requires the GET route's
// body for the delta's window.
func requireLandsOnGET(t *testing.T, handler http.Handler, kind string, held json.RawMessage, delta DeliveryLiveDelta, query string) json.RawMessage {
	t.Helper()
	applied := applyLiveDelta(t, kind, held, delta)
	want := mustEncodeLive(t, kind, getBody(t, handler, windowTarget(t, kind, applied, query)))
	if got, wantBody := liveComparable(t, mustEncodeLive(t, kind, applied)), liveComparable(t, want); !reflect.DeepEqual(got, wantBody) {
		t.Fatalf("the delta does not land on the GET route's body:\napplied: %v\nGET:     %v", got, wantBody)
	}
	return applied
}

// applyLiveDelta is the body held with delta applied, as a page applies it.
func applyLiveDelta(t *testing.T, kind string, held json.RawMessage, delta DeliveryLiveDelta) json.RawMessage {
	t.Helper()
	applied := liveApply(t, mustEncodeLive(t, kind, held), delta)
	raw, err := json.Marshal(applied.body)
	if err != nil {
		t.Fatalf("encode the applied body: %v", err)
	}
	return raw
}

// truncateChanges empties the delivery change log and restarts its numbering, as e2e/seed.ts:150's
// reset does.
func truncateChanges(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `truncate delivery_changes restart identity`); err != nil {
		t.Fatalf("truncate delivery_changes: %v", err)
	}
}

// latestChange is the change log's newest seq once its outbox is drained, as the hub drains it
// before each read (delivery.DrainPending): a test that writes eval or facts rows by SQL, outside
// the writers that drain after their commit, reads their changes in the log this way.
func latestChange(t *testing.T, database *store.Store) int64 {
	t.Helper()
	if _, err := delivery.DrainPending(context.Background(), database.Pool); err != nil {
		t.Fatalf("drain the change log's outbox: %v", err)
	}
	head, err := delivery.ChangesHead(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the newest change: %v", err)
	}
	return head.Seq
}

func TestAProductionJobFlipsTheWaitingPullRequestsItShips(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	for n := 1; n <= 3; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(10-n)*time.Hour)))
	}
	// The deploy run is in progress: its production job has not finished.
	head, started := now.Add(-5*time.Hour), now.Add(-4*time.Hour)
	if err := delivery.UpsertRun(t.Context(), rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 300, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha300",
		HeadCommitAt: head, StartedAt: started, URL: "https://github.com/acme/widgets/actions/runs/300",
		HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert the running deploy: %v", err)
	}
	if err := delivery.UpsertRunJobs(t.Context(), rig.database.Pool, "acme/widgets", 300, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 300, Name: "release / release", StartedAt: &started},
	}, false); err != nil {
		t.Fatalf("upsert the running production job: %v", err)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "p")
	// The hub acts on everything the setup wrote before the job finishes. The relay's feed reaches
	// the generation after the snapshot, on its own goroutine (seedEvents), and taking it marks the
	// view dirty (applyFeed); a recompute for that mark whose reads straddled the job's commit would
	// send a delta torn across it, and the read below would take that one rather than the job's.
	// Settled, the job's change is the one mark left, and every read of its recompute follows it.
	rig.settle(t)

	// The production job finishes: every pull request the run's head holds ships.
	done := now.Add(-3 * time.Hour)
	success := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRunJobs(t.Context(), rig.database.Pool, "acme/widgets", 300, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 300, Name: "release / release", StartedAt: &started, CompletedAt: &done, Conclusion: &success},
	}, false); err != nil {
		t.Fatalf("finish the production job: %v", err)
	}
	delta := conn.delta(t, "p")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1", "acme/widgets#2", "acme/widgets#3"}) {
		t.Fatalf("the job's delta upserts %v, want the three pull requests run 300 ships", got)
	}
	if got := liveUpsertIDs(t, delta, "runs"); !slices.Equal(got, []string{"300"}) {
		t.Fatalf("the job's delta upserts runs %v, want run 300", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAPRChecksRunMarksOnlyPipelineViews(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.firstSnapshots(t, "t", "m", "p")

	rig.settle(t)
	start := rig.mark()
	now := rig.clock.Now()
	started, done := now.Add(-time.Hour), now.Add(-50*time.Minute)
	success := delivery.DeliveryRunConclusionSuccess
	if err := delivery.UpsertRun(t.Context(), rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 400, Kind: delivery.DeliveryRunKindPRChecks, HeadSHA: "sha400",
		HeadCommitAt: started, StartedAt: started, CompletedAt: &done, Conclusion: &success,
		URL: "https://github.com/acme/widgets/actions/runs/400", HeadBranch: new("feature"), Event: new("pull_request"),
	}); err != nil {
		t.Fatalf("upsert the PR-checks run: %v", err)
	}
	if delta := conn.delta(t, "p"); len(delta.Set) == 0 {
		t.Fatalf("the pipeline delta sets nothing: %+v", delta)
	}
	rig.requireNothingSent(t, conn, "the pipeline's delta")
	if n := rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && !strings.HasPrefix(note.view, deliveryLivePipeline)
	}); n != 0 {
		t.Fatalf("a PR-checks run recomputed %d timeline or measures views, want only the pipeline view", n)
	}
}

func TestASessionGroupEditReachesEveryView(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	pr := hubPR(1, rig.clock.Now().Add(-3*time.Hour))
	pr.Sessions = []string{"s-1"}
	upsertPR(t, rig.database, pr)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.firstSnapshots(t, "t", "m", "p")

	start := rig.mark()
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Night shift", "sessions": []string{"s-1"},
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the session group: status = %d, body = %s", response.Code, response.Body.String())
	}
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "session_groups"); len(got) != 1 {
		t.Fatalf("the timeline's delta upserts session groups %v, want the new group", got)
	}
	rig.waitFor(t, start, "the measures view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	rig.waitFor(t, start, "the pipeline view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && strings.HasPrefix(note.view, deliveryLivePipeline)
	})
}

func TestAResetChangeLogIsCaughtAndEveryViewRecomputed(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	for n := 1; n <= 40; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(100+n)*time.Hour)))
	}
	if seq := latestChange(t, rig.database); seq < 40 {
		t.Fatalf("changes before the reset reach seq %d, want at least 40", seq)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	timeline := conn.firstSnapshots(t, "t", "m")["t"].Body

	// The log restarts at seq 1: one merge, below the hub's cursor.
	truncateChanges(t, rig.database)
	start := rig.mark()
	upsertPR(t, rig.database, hubPR(41, rig.clock.Now().Add(-time.Hour)))
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Contains(got, "acme/widgets#41") {
		t.Fatalf("after the reset the delta upserts %v, want acme/widgets#41", got)
	}
	timeline = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, delta, "")
	rig.waitFor(t, start, "the measures view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	if seq := rig.changeCursor(); seq != 1 {
		t.Fatalf("the hub's change cursor after the reset = %d, want 1", seq)
	}

	// Again: the hub's cursor moves to 2, then the log restarts and three merges (seq 1-3) commit
	// together before the hub reads. The log holds a row at the cursor, written at another time than
	// the hub read for it, so that is a reset too.
	upsertPR(t, rig.database, hubPR(42, rig.clock.Now().Add(-50*time.Minute)))
	timeline = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, conn.delta(t, "t"), "")
	if seq := rig.changeCursor(); seq != 2 {
		t.Fatalf("the hub's change cursor = %d, want 2", seq)
	}
	truncateChanges(t, rig.database)
	start = rig.mark()
	merged := rig.clock.Now().Add(-30 * time.Minute)
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at)
		select 'acme/widgets', n, 'feat: change ' || n, 'https://github.com/acme/widgets/pull/' || n, 'octocat', $1::timestamptz, $1::timestamptz
		from generate_series(43, 45) n
	`, merged); err != nil {
		t.Fatalf("merge three pull requests in one commit: %v", err)
	}
	delta = conn.delta(t, "t")
	if got := slices.Sorted(slices.Values(liveUpsertIDs(t, delta, "prs"))); !slices.Equal(got, []string{"acme/widgets#43", "acme/widgets#44", "acme/widgets#45"}) {
		t.Fatalf("after the second reset the delta upserts %v, want the three merges", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, delta, "")
	rig.waitFor(t, start, "the measures view's recompute after the second reset", func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	if seq := rig.changeCursor(); seq != 3 {
		t.Fatalf("the hub's change cursor after the second reset = %d, want 3", seq)
	}
}

func TestResumeWithRevsGetsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedHubSettings(t, database)
	clock := newNoonHubClock()
	now := clock.Now()
	for n := 1; n <= 4; n++ {
		upsertPR(t, database, hubPR(n, now.Add(-time.Duration(10*n)*time.Hour)))
	}
	first := hubOn(t, handler, database, deps, clock, nil)
	conn := newFakeLiveConn()
	view := timelineRange(now.Add(-7*24*time.Hour), now, "")
	first.hub.Subscribe(conn, subscribeFrame("t", view))
	snapshot := conn.snapshot(t, "t")
	// The task the page was on stops; two pull requests change before the page reaches another.
	first.stop()
	changed := hubPR(2, now.Add(-20*time.Hour))
	changed.Title = "feat: a title the page has not seen"
	upsertPR(t, database, changed)
	upsertPR(t, database, hubPR(5, now.Add(-time.Hour)))

	second := hubOn(t, handler, database, deps, clock, nil)
	resumed := newFakeLiveConn()
	frame := subscribeFrame("t", view)
	frame.Resume = &DeliveryLiveResume{Seq: snapshot.Seq, Revs: snapshot.Revs}
	second.hub.Subscribe(resumed, frame)
	delta := resumed.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2", "acme/widgets#5"}) {
		t.Fatalf("the resume's delta upserts %v, want the two pull requests that changed", got)
	}
	requireLandsOnGET(t, handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// The hub's cursor decides where the first subscribe's snapshot stands, and nothing of the hub
// runs until that subscribe starts its generation (deliveryHub.Subscribe), so the cursor is read
// after the prune below and stands on a kept change, with no catch-up racing it (INFRA-1077). A
// catch-up that found its own cursor row gone would read the log as reset (catchUpChanges); a
// recompute that read across a reset keeps no change position (recompute); and the snapshot would
// then stand at 0, before the oldest kept change, so the second resume below would be answered
// with a snapshot of its own rather than with the nothing this test is about. The listener here
// reaches no connection, so every catch-up is this test's own, and one of them runs inside the
// first recompute's read, which is the order CI took.
func TestResumeFromBeforeTheKeptDayGetsASnapshot(t *testing.T) {
	t.Parallel()
	var drove sync.Once
	rig := newHubRig(t, func(hub *deliveryHub) {
		hub.dial = func(context.Context) (*pgx.Conn, error) { return nil, context.DeadlineExceeded }
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "recompute position" {
				drove.Do(hub.catchUpNow)
			}
		}
	})
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	for n := 1; n <= 3; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(n)*time.Hour)))
	}
	// The change log keeps a day: rows older than that are pruned, so the oldest kept is past seq 2.
	if _, err := rig.database.Pool.Exec(t.Context(), `update delivery_changes set at = now() - interval '2 days' where seq <= 2`); err != nil {
		t.Fatalf("age the first changes: %v", err)
	}
	if _, err := delivery.PruneChanges(t.Context(), rig.database.Pool); err != nil {
		t.Fatalf("prune the change log: %v", err)
	}
	conn := newFakeLiveConn()
	frame := subscribeFrame("t", presetView(deliveryLiveTimeline, 7, ""))
	frame.Resume = &DeliveryLiveResume{Seq: "1", Revs: DeliveryLiveRevs{
		Items: map[string]map[string]string{}, Maps: map[string]map[string]string{},
		Counts: map[string]map[string]map[string]string{}, Fields: map[string]string{},
	}}
	rig.hub.Subscribe(conn, frame)
	snapshot := conn.snapshot(t, "t")
	requireSameBody(t, "a resume from before the kept day", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))

	// A resume from a kept change answers the delta, here none, since the view's body reflects that
	// change; the verdict the hub knows follows all the same. The snapshot stands on a kept change,
	// or the resume from it is the case above once more and says nothing about this one.
	oldest, err := delivery.OldestChangeSeq(t.Context(), rig.database.Pool)
	if err != nil {
		t.Fatalf("read the oldest kept change: %v", err)
	}
	if seq, err := strconv.ParseInt(snapshot.Seq, 10, 64); err != nil || seq < oldest {
		t.Fatalf("the snapshot stands at change %q, want one the log still keeps (from %d)", snapshot.Seq, oldest)
	}

	kept := newFakeLiveConn()
	frame.Sub, frame.Resume = "u", &DeliveryLiveResume{Seq: snapshot.Seq, Revs: snapshot.Revs}
	rig.hub.Subscribe(kept, frame)
	kept.freshness(t, "the verdict for a resume with nothing to catch up")
	rig.requireNothingSent(t, kept, "a resume from the change the view's body reflects")
}

// TestAHubThatReadNothingForADayCatchesUpWhatThePruneTook: a hub that has read nothing of the
// change log for longer than its retention (a catch-up failing for a day; here no listener wakes
// it) finds the change its cursor rests on pruned, and with it a merge in its view's window that
// it never read. It reads the log as lost and recomputes every view, so the merge reaches the
// view. Read as a change pruned in its time, the cursor would read only the change the prune kept
// after it, which touches no timeline, and the view would never learn of the merge.
func TestAHubThatReadNothingForADayCatchesUpWhatThePruneTook(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, func(hub *deliveryHub) {
		hub.dial = func(context.Context) (*pgx.Conn, error) { return nil, context.DeadlineExceeded }
	})
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	upsertPR(t, rig.database, hubPR(1, now.Add(-2*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "t")
	if rig.changeCursor() == 0 {
		t.Fatal("the hub's cursor rests on no change")
	}
	upsertPR(t, rig.database, hubPR(2, now.Add(-time.Hour)))
	for _, statement := range []string{
		`update delivery_changes set at = now() - interval '2 days'`,
		`delete from delivery_changes where at < now() - interval '1 day'`,
		`insert into delivery_changes (kind, repo, ref) values ('eval_readings', 'NONE', 'NONE/kept')`,
	} {
		if _, err := rig.database.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// The hub last read the log, and the change at its cursor, a day and an hour ago.
	rig.hub.mu.Lock()
	lastRead := now.Add(-25 * time.Hour)
	rig.hub.generation.change.At, rig.hub.generation.change.ReadAt = &lastRead, lastRead
	epoch := rig.hub.generation.epoch
	rig.hub.mu.Unlock()
	rig.hub.catchUpNow()
	rig.hub.mu.Lock()
	resets := rig.hub.generation.epoch - epoch
	rig.hub.mu.Unlock()
	if resets != 1 {
		t.Fatalf("the catch-up after a day with no read counted %d resets of the log, want 1: it read past the merge the prune took as a change pruned in its time", resets)
	}
	if got := liveUpsertIDs(t, conn.delta(t, "t"), "prs"); !slices.Contains(got, "acme/widgets#2") {
		t.Fatalf("after a day with no read the delta upserts %v, want acme/widgets#2, the merge the prune took", got)
	}
}

func TestAReconnectCatchesUpWhatTheListenerMissed(t *testing.T) {
	t.Parallel()
	var blind atomic.Bool
	// The listener's own connections, so the test ends the one it listens on by its backend's pid
	// (terminateListener).
	var dials listenerDials
	rig := newHubRig(t, func(hub *deliveryHub) {
		dial := hub.dial
		hub.dial = dials.wrap(func(ctx context.Context) (*pgx.Conn, error) {
			if blind.Load() {
				return nil, context.DeadlineExceeded
			}
			return dial(ctx)
		})
		hub.backoff = func(int) time.Duration { return 50 * time.Millisecond }
	})
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "t")
	rig.waitFor(t, 0, "the listener's first connection", func(note deliveryHubNote) bool { return note.what == "connected" })
	// The first connection's catch-up may have dirtied the view behind the snapshot (the seeded
	// settings' changes), and a recompute still to run would read the merge below and send it as
	// though the listener had heard it: the hub settles first.
	rig.settle(t)
	start := rig.mark()

	// The listener loses its connection and cannot dial again; a merge commits while it is blind,
	// once the listener's backend is gone (terminateListener).
	blind.Store(true)
	terminateListener(t, rig.database, &dials)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-time.Hour)))
	// No state of the hub tells a merge it could not hear from one it has yet to catch up: its
	// change cursor stands below the merge until a catch-up runs either way. So this stays a
	// window, and it cannot fail a correct hub, which hears the merge only once the reconnection
	// below lets it; the window is long enough for a hub that heard it to send its delta (dueAt).
	conn.none(t, viewMaxWait+time.Second, "a merge the listener could not hear")
	blind.Store(false)
	rig.waitFor(t, start, "the listener's reconnection", func(note deliveryHubNote) bool { return note.what == "connected" })
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("after the reconnection the delta upserts %v, want the merge it missed", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// terminateListener ends the backend of every connection the hub's listener dialed (dials) that
// is still connected, and returns once each has left pg_stat_activity. pg_terminate_backend
// answers as soon as it has signalled the backend, which exits after: a NOTIFY committed in
// between still reaches the listener through that backend, so a test that must commit a change
// the listener cannot hear waits for the backend to be gone. The backends are the listener's own,
// by the pid each dial recorded: a predicate on the database and a backend's last statement would
// also end a pooled connection of the server that pgxpool pinged ("-- ping") as it was acquired,
// and the relay's listener, which this test does not blind.
//
// The caller has already made the hub blind, so a dial that begins now fails; one already under
// way can still connect after a round has ended what it found. So the rounds go on until one finds
// no backend to end while no dial is under way. Only the first round must find one. The rounds, and
// each round's wait for the backends it ended to exit, end on those facts, and their one bound is
// the test binary's deadline: a listener that keeps connecting fails here, naming what the last
// round found, rather than running to the package's test timeout.
func terminateListener(t *testing.T, database *store.Store, dials *listenerDials) {
	t.Helper()
	round := 0
	testwait.UntilPolling(t, 5*time.Millisecond, "a round that finds no backend of the listener's to end while no dial of it is under way (what the last round found)", func() (string, bool) {
		defer func() { round++ }()
		conns, settled := dials.settled()
		pids := pidsOf(conns)
		var ended []int32
		if err := database.Pool.QueryRow(context.Background(), `
			with listener as materialized (
				select pid from pg_stat_activity where pid = any($1) and datname = current_database()
			)
			select coalesce(array_agg(pid), '{}') from listener where pg_terminate_backend(pid)
		`, pids).Scan(&ended); err != nil {
			t.Fatalf("terminate the listener's backend: ended %v, err %v", ended, err)
		}
		if len(ended) == 0 {
			if round == 0 {
				t.Fatalf("terminate the listener's backend: none of the %d connections it dialed (%v) is connected", len(pids), pids)
			}
			return fmt.Sprintf("round %d ended no backend, a dial under way: %t", round, !settled), settled
		}
		testwait.UntilPolling(t, 5*time.Millisecond, fmt.Sprintf("the listener's terminated backends %v to leave pg_stat_activity (how many are still there)", ended), func() (int, bool) {
			var left int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*) from pg_stat_activity where pid = any($1)`, ended).Scan(&left); err != nil {
				t.Fatalf("read whether the listener's backend has exited: %v", err)
			}
			return left, left == 0
		})
		return fmt.Sprintf("round %d ended backends %v", round, ended), false
	})
}

// createHubIssue creates issue title in project HUB (creating the project the first time), and
// returns its key.
func createHubIssue(t *testing.T, handler http.Handler, fields map[string]any) string {
	t.Helper()
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "HUB", "name": "Hub"}, "alice")
	body := map[string]any{"project": "HUB"}
	for name, value := range fields {
		body[name] = value
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %v: status = %d, body = %s", fields, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

func patchHubIssue(t *testing.T, handler http.Handler, key string, patch map[string]any) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, patch, "alice"); response.Code != http.StatusOK {
		t.Fatalf("patch %s %v: status = %d, body = %s", key, patch, response.Code, response.Body.String())
	}
}

// prNaming is pull request n merged at merged, naming issue.
func prNaming(n int, merged time.Time, issue string) delivery.DeliveryPullRequest {
	pr := hubPR(n, merged)
	pr.IssueKey = &issue
	return pr
}

// seedHubComponents gives project HUB a component tree, platform > api.
func seedHubComponents(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('HUB', 1, 'acme/hub', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (901, 'HUB', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('HUB', 'platform', 'The platform', '', null, 901, 1), ('HUB', 'api', 'Public API', '', 'platform', 901, 1)`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed components: %v\n%s", err, statement)
		}
	}
}

func TestAnIssueTitleChangeReachesPagesThatNameTheIssue(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	named := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	other := createHubIssue(t, rig.handler, map[string]any{"title": "Tidy docs"})
	now := rig.clock.Now()
	upsertPR(t, rig.database, prNaming(1, now.Add(-3*time.Hour), named))
	upsertPR(t, rig.database, prNaming(2, time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC), other))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("names", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("other", timelineRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	held := conn.firstSnapshots(t, "names", "other")["names"].Body

	rig.settle(t)
	start := rig.mark()
	patchHubIssue(t, rig.handler, named, map[string]any{"title": "Ship the widgets"})
	delta := conn.eventDelta(t, rig.database, "names")
	if got := liveMapValues(t, delta, "issue_titles"); got[named] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled", got, named)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
	rig.requireNothingSent(t, conn, "a title change the other page does not name")
	if n := rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.Contains(note.view, "2024-06-01")
	}); n != 0 {
		t.Fatalf("a title change the other page does not name recomputed it %d times, want 0", n)
	}
}

func TestAParentsComponentChangeReachesPagesHoldingItsChildren(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	parent := createHubIssue(t, rig.handler, map[string]any{"title": "Platform work"})
	seedHubComponents(t, rig.database)
	child := createHubIssue(t, rig.handler, map[string]any{"title": "A child", "parent": parent})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), child))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "t")

	// The child inherits what its parent chooses.
	patchHubIssue(t, rig.handler, parent, map[string]any{"components": map[string]any{"mode": "explicit", "ids": []string{"api"}}})
	delta := conn.eventDelta(t, rig.database, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the parent's component change upserts %v, want the child's pull request", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// TestTheHubRecomputesAViewAgainAfterAnEventDuringItsRead: a view's first read finishes before an
// issue it shows is retitled, and the retitle's event reaches the hub while that recompute still
// holds no issue of the view's. The view is recomputed once more and its page gets the new title.
//
// The relay's feed stands before the view subscribes. The feed starts at the event log's newest id
// as the relay's start read finds it (firstRelayCursor), and that read runs on a goroutine of the
// hub's own (seedEvents), which nothing orders before the test's next write: a start read that a
// loaded machine schedules after the retitle starts the feed at the retitle, which then never
// arrives on it, and the view's second recompute is the feed's (applyFeed marks every view dirty)
// rather than the one the event heard during the read asks for (recheckHeardLocked).
func TestTheHubRecomputesAViewAgainAfterAnEventDuringItsRead(t *testing.T) {
	t.Parallel()
	reading := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "recompute read" && strings.HasPrefix(note.view, deliveryLiveTimeline) && held.CompareAndSwap(false, true) {
				// Hold the timeline view's first recompute between its read and its answer.
				close(reading)
				<-release
			}
		}
	})
	// Registered after the rig's, so it runs first: a failed test never leaves the hub held.
	t.Cleanup(releaseOnce)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))
	// A measures view starts the generation, and settle waits for its feed ("events seeded") and
	// for the hub to owe nothing, so the retitle below stands above the feed's head.
	warm := newFakeLiveConn()
	rig.hub.Subscribe(warm, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	warm.snapshot(t, "m")
	rig.settle(t)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	testwait.Receive(t, "the timeline view's first recompute, held between its read and its answer", reading)

	start := rig.mark()
	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	var newest int64
	if err := rig.database.Pool.QueryRow(t.Context(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	rig.waitForEvent(t, start, "the retitle on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == newest
	})
	releaseOnce()

	snapshot := conn.snapshot(t, "t")
	var read struct {
		IssueTitles map[string]string `json:"issue_titles"`
	}
	if err := json.Unmarshal(snapshot.Body, &read); err != nil {
		t.Fatalf("decode the snapshot: %v", err)
	}
	if got := read.IssueTitles[key]; got != "Ship widgets" {
		t.Fatalf("the snapshot, read before the retitle, titles %s %q, want %q", key, got, "Ship widgets")
	}
	// The hub tells a sub the verdict it knows right after its first answer (Subscribe), which
	// the measures view's read set before this view subscribed.
	conn.freshness(t, "the verdict after the timeline's first snapshot")
	delta := conn.delta(t, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// TestASessionWriteDuringAViewsReadThatItsAnswerDoesNotShowRecomputesNothing: a session's event
// reaches the hub while a view's first recompute holds its read, and that read's answer shows no
// such session, so the view is not recomputed again.
func TestASessionWriteDuringAViewsReadThatItsAnswerDoesNotShowRecomputesNothing(t *testing.T) {
	t.Parallel()
	reading := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "recompute read" && strings.HasPrefix(note.view, deliveryLiveTimeline) && held.CompareAndSwap(false, true) {
				close(reading)
				<-release
			}
		}
	})
	// Registered after the rig's, so it runs first: a failed test never leaves the hub held.
	t.Cleanup(releaseOnce)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))
	// A measures view starts the generation and holds it open while the timeline view, the one
	// measured here, subscribes after it: the relay's feed, taken on a goroutine of the
	// generation's own, marks every view it finds dirty when it arrives (applyFeed), and that
	// recompute belongs to none of what this measures.
	warm := newFakeLiveConn()
	rig.hub.Subscribe(warm, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	warm.snapshot(t, "m")
	rig.settle(t)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	testwait.Receive(t, "the timeline view's first recompute, held between its read and its answer", reading)

	start := rig.mark()
	written := appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated",
		model.Actor{Kind: "session", ID: "session-not-shown"}, map[string]any{}))
	rig.waitForEvent(t, start, "the session's event on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == written.ID
	})
	releaseOnce()

	snapshot := conn.snapshot(t, "t")
	var read struct {
		SessionTitles map[string]string `json:"session_titles"`
	}
	if err := json.Unmarshal(snapshot.Body, &read); err != nil {
		t.Fatalf("decode the snapshot: %v", err)
	}
	if _, shown := read.SessionTitles["session-not-shown"]; shown {
		t.Fatalf("the snapshot's session_titles = %v, want the writing session not shown", read.SessionTitles)
	}
	// The hub tells a sub the verdict it knows right after its first answer (Subscribe), which
	// the measures view's read set before this view subscribed.
	conn.freshness(t, "the verdict after the timeline's first snapshot")
	rig.requireNothingSent(t, conn, "a session write the view's answer does not show")
	if n := rig.count(start, recomputeBegin("")); n != 0 {
		t.Fatalf("the session write during the read recomputed the view %d times more, want none", n)
	}
}

// unownedP0Fields is the unowned_p0 and status a body of kind shows, decoded: a measures body's
// own, a timeline's from its measures.
func unownedP0Fields(t *testing.T, body json.RawMessage, kind string) map[string]any {
	t.Helper()
	if kind == deliveryLiveTimeline {
		var timeline struct {
			Measures json.RawMessage `json:"measures"`
		}
		if err := json.Unmarshal(body, &timeline); err != nil {
			t.Fatalf("decode the timeline's measures: %v", err)
		}
		body = timeline.Measures
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the measures: %v", err)
	}
	return map[string]any{"unowned_p0": decoded["unowned_p0"], "status": decoded["status"]}
}

func TestAnUnownedP0ChangeReachesEveryTimelineAndMeasuresView(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-3*time.Hour)))
	seed := createHubIssue(t, rig.handler, map[string]any{"title": "Raised later", "priority": 1})
	conn := newFakeLiveConn()
	kinds := map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures}
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	held := map[string]json.RawMessage{}
	for sub, snapshot := range conn.firstSnapshots(t, "t", "m") {
		held[sub] = snapshot.Body
	}
	var created string
	for _, step := range []struct {
		name string
		act  func()
	}{
		{"a P0 created", func() {
			created = createHubIssue(t, rig.handler, map[string]any{"title": "Production is down", "priority": 0})
		}},
		{"a P0 claimed", func() {
			if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+created+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("claim: status = %d, body = %s", response.Code, response.Body.String())
			}
		}},
		{"a claimed P0 released", func() {
			if response := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/issues/"+created+"/claim", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("release: status = %d, body = %s", response.Code, response.Body.String())
			}
		}},
		{"a P0 closed", func() { patchHubIssue(t, rig.handler, created, map[string]any{"status": "done"}) }},
		{"a P1 raised to P0", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"priority": 0}) }},
		{"a P0 routed", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"route": "role:reviewer"}) }},
		{"a routed P0 unrouted", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"route": ""}) }},
		{"a P0 lowered to P1", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"priority": 1}) }},
	} {
		step.act()
		for range 2 {
			delta, ok := conn.eventNext(t, rig.database, "each view's delta after "+step.name).(DeliveryLiveDelta)
			if !ok {
				t.Fatalf("after %s: want a delta for each view", step.name)
			}
			kind := kinds[delta.Sub]
			applied := applyLiveDelta(t, kind, held[delta.Sub], delta)
			// GET /measures for the window the view answers.
			want := unownedP0Fields(t, getBody(t, rig.handler, windowTarget(t, deliveryLiveMeasures, applied, "")), deliveryLiveMeasures)
			if got := unownedP0Fields(t, applied, kind); !reflect.DeepEqual(got, want) {
				t.Fatalf("after %s, %s's measures hold %v; GET /measures answers %v", step.name, delta.Sub, got, want)
			}
			held[delta.Sub] = applied
		}
	}
}

func TestAnArchitectureSyncReachesEveryView(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Anything"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.firstSnapshots(t, "t", "m", "p")
	start := rig.mark()
	appendHubEvent(t, rig, projectOwner("HUB").event("architecture.synced", model.Actor{Kind: "system", ID: "architecture-importer"}, map[string]any{"commit": "c2"}))
	for _, kind := range []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline} {
		rig.waitForEvent(t, start, "the "+kind+" view's recompute", func(note deliveryHubNote) bool {
			return note.what == "recompute begin" && strings.HasPrefix(note.view, kind)
		})
	}
}

// appendHubEvent commits event through the server's broker, as a route does. The hub hears of it
// from the relay, which reads it back out of the log.
func appendHubEvent(t *testing.T, rig *hubRig, event model.Event) model.Event {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	appended, err := rig.deps.Events.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append %s: %v", event.Type, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", event.Type, err)
	}
	return appended
}

// TestAResetEventLogIsCaughtAndEveryViewRecomputed: the relay ends its feed when the event log is
// reset under its cursor, and the hub answers the closed feed by recomputing every view, which
// reads what the new log's events changed.
func TestAResetEventLogIsCaughtAndEveryViewRecomputed(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	for range 299 {
		appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated", model.Actor{Kind: "user", ID: "alice"}, map[string]any{}))
	}
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, "t").Body
	// The hub takes its feed on a goroutine of its own once the relay's first cursor stands, which
	// waits on the event horizon; each reset below is of a feed it holds.
	rig.waitForEvent(t, 0, "the hub's event feed", func(note deliveryHubNote) bool { return note.what == "events seeded" })

	// The event log restarts at id 1, as e2e/seed.ts:20 and :150 reset it; then a title the view
	// names changes, its event (id 1) below the relay's cursor.
	firstReset := rig.mark()
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("truncate events: %v", err)
	}
	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	// The delta comes from the first recompute that reads the retitle. When the recompute the hub's
	// first feed asked for (applyFeed) read before it, that is the recompute of every view the hub
	// takes once it has a feed again, which stands once the relay's next generation does, behind a
	// horizon proven after the reset (firstRelayCursor); or, when the relay found the reset before
	// the retitle committed and the new feed began below the retitle's event, the one that event
	// asks for, which the relay publishes once the horizon has passed it. An event write of the
	// server's database held open puts off either, so the wait is the horizon's.
	delta := conn.eventDelta(t, rig.database, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("after the event log's reset the delta's issue_titles = %v, want %s retitled", got, key)
	}
	held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
	// The hub takes a feed again once the relay's next generation stands, behind the horizon. The
	// reset below is told from a quiet log by the row at the relay's cursor, so the cursor must
	// stand on a row of this log first, and the relay publishes only up to the horizon.
	rig.waitForEvent(t, firstReset, "the hub's event feed after the reset", func(note deliveryHubNote) bool { return note.what == "events seeded" })
	firstOfNewLog := eventHead(t, rig.database)
	horizontest.PollInProcess(t, "the relay's cursor on the reset log's newest event", rig.database, func() bool {
		return relayPublished(rig.relay) >= firstOfNewLog
	})

	// Again, with the log refilled past the relay's cursor in the commit that resets it: the row at
	// its cursor was created at another time than the relay read, so that is a reset too. The title
	// changes in that commit with no event of its own, so only a recompute that reads after it can
	// show it. The reset ends the hub's feed too, and the recompute of every view the hub takes once
	// it has a feed again (applyFeed) reads after it; that feed stands once a horizon proven after
	// the reset does (firstRelayCursor). So the wait for that feed is the horizon's, and the delta
	// after it waits on nothing more.
	reset := rig.mark()
	if _, err := rig.database.Pool.Exec(t.Context(), `
		truncate events restart identity cascade;
		update issues set title = 'Ship every widget' where key = '`+key+`';
		insert into events (project_key, seq, type, actor, payload, notify, created_at)
		select 'HUB', n, 'user_state.updated', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '1 hour'
		from generate_series(1, 5) n;
		update projects set last_seq = 5 where key = 'HUB';
	`); err != nil {
		t.Fatalf("reset and refill the event log: %v", err)
	}
	rig.waitForEvent(t, reset, "the hub losing its feed to the second reset", func(note deliveryHubNote) bool { return note.what == "feed closed" })
	rig.waitForEvent(t, reset, "the hub's event feed after the second reset", func(note deliveryHubNote) bool { return note.what == "events seeded" })
	delta = conn.delta(t, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship every widget" {
		t.Fatalf("after the second reset the delta's issue_titles = %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
}

// TestEventsNoViewReadsRecomputeNothing: 500 events of a type no view reads, written without the
// broker as another task's would be, reach the hub on the relay's feed and mark no view.
func TestEventsNoViewReadsRecomputeNothing(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Anything"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "t")

	rig.settle(t)
	start := rig.mark()
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (project_key, seq, type, actor, payload, notify)
		select 'HUB', (select last_seq from projects where key = 'HUB') + n, 'artifact.version', '{"kind":"user","id":"alice"}', '{}', false
		from generate_series(1, 500) n
	`); err != nil {
		t.Fatalf("append 500 events: %v", err)
	}
	var newest int64
	if err := rig.database.Pool.QueryRow(t.Context(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	rig.waitForEvent(t, start, "the newest event on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID == newest
	})
	if n := rig.count(start, func(note deliveryHubNote) bool { return note.what == "event" }); n != 500 {
		t.Fatalf("the hub received %d events, want each of the 500 once", n)
	}
	rig.requireNothingSent(t, conn, "500 events no view reads")
	if n := rig.count(start, recomputeBegin("")); n != 0 {
		t.Fatalf("500 events no view reads recomputed %d views, want none", n)
	}
}

// TestAClosedFeedReloadsTheOpenP0sAndRecomputesEveryView: when the relay's feed closes (here its
// generation ended, as a reset does, and as an overflow closes one subscription), events may have
// gone by that the hub never received. It subscribes again, reads the open P0 issues afresh and
// recomputes every view.
// An issue raised to P0 while the hub had no feed reaches every timeline and measures view, though
// no view names it, and lowering it again does too, which only the reloaded P0s can tell the hub:
// the lowering's payload says P1, so without the reload the issue was never a P0 to it.
func TestAClosedFeedReloadsTheOpenP0sAndRecomputesEveryView(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "feed closed" {
				// Hold the hub between losing its feed and subscribing again, while the test commits
				// an event no feed carries to it.
				<-release
			}
		}
	})
	// Registered after the rig's, so it runs first: a failed test never leaves the hub held.
	t.Cleanup(releaseOnce)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-3*time.Hour)))
	raised := createHubIssue(t, rig.handler, map[string]any{"title": "Raised while unheard", "priority": 1})
	conn := newFakeLiveConn()
	held := map[string]json.RawMessage{}
	kinds := map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures}
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	for range 2 {
		snapshot, ok := conn.next(t, "each view's snapshot").(DeliveryLiveSnapshot)
		if !ok {
			t.Fatal("want a snapshot for each view")
		}
		held[snapshot.Sub] = snapshot.Body
	}
	requireEveryViewShowsTheUnownedP0s := func(after string) {
		t.Helper()
		for range 2 {
			// The hub tells a subscription the freshness verdict when its generation reconnects to
			// the feed; the deltas are what this test reads.
			var delta DeliveryLiveDelta
			for ok := false; !ok; {
				frame := conn.next(t, "each view's delta after "+after)
				if _, fresh := frame.(DeliveryLiveFreshness); fresh {
					continue
				}
				if delta, ok = frame.(DeliveryLiveDelta); !ok {
					t.Fatalf("after %s: frame = %+v, want a delta for each view", after, frame)
				}
			}
			kind := kinds[delta.Sub]
			applied := applyLiveDelta(t, kind, held[delta.Sub], delta)
			want := unownedP0Fields(t, getBody(t, rig.handler, windowTarget(t, deliveryLiveMeasures, applied, "")), deliveryLiveMeasures)
			if got := unownedP0Fields(t, applied, kind); !reflect.DeepEqual(got, want) {
				t.Fatalf("after %s, %s's measures hold %v; GET /measures answers %v", after, delta.Sub, got, want)
			}
			held[delta.Sub] = applied
		}
	}

	// The hub takes its feed on a goroutine of its own, so a generation that has not taken one
	// yet would lose nothing when the relay's generation ends below.
	rig.settle(t)
	start := rig.mark()
	endRelayGeneration(rig.relay)
	rig.waitFor(t, start, "the hub losing its feed", func(note deliveryHubNote) bool { return note.what == "feed closed" })
	patchHubIssue(t, rig.handler, raised, map[string]any{"priority": 0})
	releaseOnce()
	// The hub takes its feed again once the relay's next generation has started, which waits on
	// the event horizon; the recompute of every view that follows waits on nothing more.
	rig.waitForEvent(t, start, "the hub's feed taken again", func(note deliveryHubNote) bool { return note.what == "events seeded" })
	requireEveryViewShowsTheUnownedP0s("a P1 raised to P0 while the hub had no feed")

	lowering := rig.mark()
	patchHubIssue(t, rig.handler, raised, map[string]any{"priority": 1})
	lowered := eventHead(t, rig.database)
	rig.waitForEvent(t, lowering, "the lowering on the hub's feed", func(note deliveryHubNote) bool {
		return note.what == "event" && note.eventID >= lowered
	})
	requireEveryViewShowsTheUnownedP0s("the reloaded P0 lowered to P1")
}

// TestAHubActsOnAnEventOnlyOnceEveryLowerIDIsFinal: event ids are not commit order, so a write
// that drew the lower id a can still be open after one that drew b > a has committed. The hub acts
// only on what the relay publishes, and the relay publishes only up to the safe horizon, which a's
// open transaction holds below a: so the hub acts on neither while a is open (acting on b would
// leave a to a feed that will never carry it), and on both once a commits. Each event makes its
// own issue a P0 in the hub's view, which is what shows it was acted on.
func TestAHubActsOnAnEventOnlyOnceEveryLowerIDIsFinal(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	first := createHubIssue(t, rig.handler, map[string]any{"title": "First"})
	second := createHubIssue(t, rig.handler, map[string]any{"title": "Second"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "t")
	rig.settle(t)
	start := rig.mark()

	alice := model.Actor{Kind: "user", ID: "alice"}
	ctx := t.Context()
	open, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower id's transaction: %v", err)
	}
	t.Cleanup(func() { _ = open.Rollback(context.Background()) })
	low, err := rig.deps.Events.Append(ctx, open, issueOwner(first).event("issue.updated", alice,
		map[string]any{"key": first, "priority": 0, "status": "open"}))
	if err != nil {
		t.Fatalf("append the lower id: %v", err)
	}
	high := appendHubEvent(t, rig, issueOwner(second).event("issue.updated", alice,
		map[string]any{"key": second, "priority": 0, "status": "open"}))
	if high.ID <= low.ID {
		t.Fatalf("event ids %d (open) and %d (committed) are not out of commit order, so this test proves nothing", low.ID, high.ID)
	}

	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit the lower id: %v", err)
	}
	rig.waitForEventCursor(t, high.ID, "both ids committed")
	// The hub applies what the relay publishes, in the order it publishes it, and its notes keep
	// that order: a relay that had published the committed higher id while the lower was open would
	// have reached the hub with it first, which the lower id's commit cannot undo.
	applied := rig.eventsApplied(start)
	if at := slices.Index(applied, low.ID); at < 0 || at > slices.Index(applied, high.ID) {
		t.Fatalf("the hub applied the events %v, want the open id %d before the committed id %d", applied, low.ID, high.ID)
	}
	if !rig.isP0(first) || !rig.isP0(second) {
		t.Fatalf("after both commits: %s a P0 = %v, %s a P0 = %v; want both acted on", first, rig.isP0(first), second, rig.isP0(second))
	}
}

// An event write of Dispatch's database that stays open keeps every safe horizon of the event log
// from being proven past it, and a migration or a stalled request can hold one open for as long as
// it likes. A fresh hub still serves its pages meanwhile: the delivery change log has nothing to
// do with that horizon, and the relay subscription that waits on it is taken on a goroutine of the
// generation's own (seedEvents), so the hub is ready at once, answers a page its snapshot and
// carries a delivery change to it while the write is open, and the issue event committed meanwhile
// reaches the page once the horizon is proven.
func TestAFreshHubServesDeliveryChangesWhileATransactionHoldsTheEventHorizon(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))

	// The event write stands until the row releases it, once the delivery change has reached the
	// page and the retitle has committed, so both happen while it holds the horizon however long the
	// row takes to get there.
	release := holdEventWrite(t, rig.database)
	// The page's subscribe starts the hub's generation.
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.mu.Lock()
	ready := rig.hub.generation.ready
	rig.hub.mu.Unlock()
	testwait.Receive(t, "the hub's ready, while a transaction holds the event horizon", ready)
	held := conn.snapshot(t, "t").Body

	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the change's delta upserts %v, want acme/widgets#2", got)
	}
	held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")

	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	release()
	delta = conn.eventDelta(t, rig.database, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("after the hold the delta's issue_titles = %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
}

// listenerDials records the connections one listener dials, so a test reads pg_stat_activity for
// those backends alone. A predicate on the test's database and a backend's last statement also
// takes in any other backend of that database whose last statement reads as a listener's: a
// pooled connection that pgxpool pinged ("-- ping") as it was acquired, before the statement it
// was acquired for, is one.
type listenerDials struct {
	mu    sync.Mutex
	conns []*pgx.Conn
	// inFlight is how many dials have begun and not yet returned.
	inFlight int
}

// wrap is dial, recording each connection it opens and each dial still under way.
func (l *listenerDials) wrap(dial func(context.Context) (*pgx.Conn, error)) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		l.mu.Lock()
		l.inFlight++
		l.mu.Unlock()
		conn, err := dial(ctx)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.inFlight--
		if err == nil {
			l.conns = append(l.conns, conn)
		}
		return conn, err
	}
}

// dialed is every connection dialed so far.
func (l *listenerDials) dialed() []*pgx.Conn {
	conns, _ := l.settled()
	return conns
}

// settled is every connection dialed so far, and whether no dial is under way, so none can add
// another to them before the next one begins.
func (l *listenerDials) settled() ([]*pgx.Conn, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.conns), l.inFlight == 0
}

// pids is the backend pid of every connection dialed so far (pidsOf).
func (l *listenerDials) pids() []int32 { return pidsOf(l.dialed()) }

// pidsOf is the backend pid of each of conns: fixed once a dial returns, where a connection's other
// state is its listener's own, so it can be read while that listener runs.
func pidsOf(conns []*pgx.Conn) []int32 {
	pids := make([]int32, 0, len(conns))
	for _, conn := range conns {
		pids = append(pids, int32(conn.PgConn().PID()))
	}
	return pids
}

// waitListening waits for one of the connections dials recorded to hold a LISTEN: its backend's last
// statement was the listener's LISTEN, or the idle ping the listener sends after it
// (pglisten.Listener). The ping replaces the LISTEN as the last statement one listen idle period
// after it, so a wait that reads the LISTEN alone reads nothing once a wait before it, such as one
// behind a held event horizon, has outlasted that period. The ping still says the LISTEN stands:
// the listener pings a connection only once its LISTEN has succeeded there, and a recorded
// connection runs no other statement before it (store.Pool.Listener hands the listener a new
// connection, hijacked from a pool that keeps none idle, so the pool never pings it). The wait
// ends on that backend's LISTEN, and its one bound is the test binary's deadline.
func waitListening(t *testing.T, database *store.Store, what string, dials *listenerDials) {
	t.Helper()
	testwait.UntilPolling(t, 20*time.Millisecond, what+" (how many of its backends hold one)", func() (int, bool) {
		pids := dials.pids()
		var count int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where pid = any($1) and (query like 'listen %' or query = '-- ping')`, pids,
		).Scan(&count); err != nil {
			t.Fatalf("count the backends of %s: %v", what, err)
		}
		return count, count > 0
	})
}

// waitReturned waits for every goroutine running counts to have returned, failing the test with
// what when the test binary's deadline comes first (testwait.Receive).
func waitReturned(t *testing.T, what string, running *sync.WaitGroup) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		running.Wait()
		close(returned)
	}()
	testwait.Receive(t, what, returned)
}

// TestAHubWithNoViewReadsNothingAndHoldsNoListener: the hub runs only while a page holds a view.
// Before the first subscribe and after the last view's drop, commits to the event log and the
// change log cost the hub and its relay no read, and neither holds a LISTEN connection; while a
// view is open both do, and the subscribe after the drop starts a generation that follows both logs
// again.
//
// The test reads only the hub's and the relay's own listeners, through their dial (listenerDials).
// The relay's listener dials once its generation's first cursor has awaited the event horizon
// (firstRelayCursor), which an event write of this database open then holds back, so the wait for
// it is the horizon's. After the drop the test waits for every goroutine of the hub and of the
// relay to return: a listener's Run returns only once it has closed its connection.
func TestAHubWithNoViewReadsNothingAndHoldsNoListener(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	// The hub and its relay read through a pool of their own, so its acquires are theirs alone.
	deps.Store = storetest.SecondProcess(t, database)
	var hubDials listenerDials
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), func(hub *deliveryHub) { hub.dial = hubDials.wrap(hub.dial) })
	// The relay starts nothing before the hub's first Subscribe.
	relayDials := recordRelayDials(rig.relay, deps)
	allDials := func() []*pgx.Conn { return slices.Concat(hubDials.dialed(), relayDials.dialed()) }
	seedHubSettings(t, database)
	key := createHubIssue(t, handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))
	merges := 1
	idle := func(when string) {
		t.Helper()
		dialed := len(allDials())
		// Neither the hub nor its relay runs a generation, so neither listens or reads: that is the
		// state the commits below must cost nothing from, and what keeps the span below from
		// failing a correct hub, however late it ends.
		rig.hub.mu.Lock()
		hubRunning := rig.hub.generation != nil
		rig.hub.mu.Unlock()
		rig.relay.mu.Lock()
		relayRunning := rig.relay.generation != nil
		rig.relay.mu.Unlock()
		if hubRunning || relayRunning {
			t.Fatalf("a generation runs %s: the hub's = %t, the relay's = %t, want neither", when, hubRunning, relayRunning)
		}
		before := acquires(deps.Store)
		patchHubIssue(t, handler, key, map[string]any{"title": "Retitled " + when})
		merges++
		upsertPR(t, database, hubPR(merges, rig.clock.Now().Add(-2*time.Hour)))
		// Longer than the relay's coalescing window and the hub's quiet, either of which a reader
		// that heard the commits would have waited out before its read.
		time.Sleep(time.Second)
		if got := acquires(deps.Store); got != before {
			t.Fatalf("the hub and its relay took %d connections %s, want none", got-before, when)
		}
		if got := len(allDials()); got != dialed {
			t.Fatalf("the hub and its relay dialed %d listener connections %s, want none", got-dialed, when)
		}
	}

	if dialed := len(allDials()); dialed != 0 {
		t.Fatalf("the hub and its relay dialed %d listener connections before any page subscribed, want none", dialed)
	}
	idle("before any page subscribed")

	first := newFakeLiveConn()
	rig.hub.Subscribe(first, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	first.snapshot(t, "t")
	waitListening(t, database, "the hub's listener holding a LISTEN", &hubDials)
	horizontest.PollInProcess(t, "the relay's generation, started for the hub's feed", database, func() bool {
		return relaySubscribers(rig.relay) > 0
	})
	waitListening(t, database, "the relay's listener holding a LISTEN", relayDials)
	rig.hub.Close(first)
	// The hub's goroutines first: seedEvents, one of them, is what subscribes to the relay, so once
	// they have returned nothing starts a relay generation while the relay's are waited for.
	waitReturned(t, "every goroutine of the hub to return after the last view's drop", &rig.hub.running)
	waitReturned(t, "every goroutine of the relay to return after the last view's drop", &rig.relay.running)
	for _, conn := range allDials() {
		// Read after the waits above, which order each listener's close before this read.
		if !conn.IsClosed() {
			t.Fatalf("the listener connection to backend %d is still open after the last view's drop", conn.PgConn().PID())
		}
	}

	idle("after the last view's drop")

	second := newFakeLiveConn()
	rig.hub.Subscribe(second, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := second.snapshot(t, "t").Body
	merges++
	upsertPR(t, database, prNaming(merges, rig.clock.Now().Add(-time.Hour), key))
	delta := second.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#" + strconv.Itoa(merges)}) {
		t.Fatalf("the new generation's merge delta upserts %v, want acme/widgets#%d", got, merges)
	}
	held = requireLandsOnGET(t, handler, deliveryLiveTimeline, held, delta, "")
	patchHubIssue(t, handler, key, map[string]any{"title": "Ship every widget"})
	// The retitle is an event, which reaches the hub on the relay's feed (eventDelta).
	delta = second.eventDelta(t, database, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship every widget" {
		t.Fatalf("the new generation's retitle delta has issue_titles %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, handler, deliveryLiveTimeline, held, delta, "")
}

// handOverRounds is how many generations TestAGenerationEndingDuringTheFeedHandOverLeavesTheRelayNoSubscriber
// ends during the hand-over. Against a g.feeds with one slot, ten runs of the test on 2026-11-16
// all failed, leaking in 10 of the 18 rounds they made: about the half the intake's select gives.
// At a half, twenty rounds all miss a leak once in about a million runs.
const handOverRounds = 20

// TestAGenerationEndingDuringTheFeedHandOverLeavesTheRelayNoSubscriber: seedEvents hands the
// relay subscription it took to the intake, and the generation can end during that hand-over. No
// subscription may outlive the generation's goroutines: one the intake never took and nobody
// cancelled keeps the relay's generation, its LISTEN connection and its reads of the event log
// running with no page open.
//
// Each round holds the intake inside its "connected" note, where it takes no feed, until
// seedEvents has made its "feed ready" note; then it drops the generation's one sub, lets the
// intake go and waits for every goroutine of the hub to return. A hand-over that left the feed in a
// slot (g.feeds) leaves the intake's next select a choice between that feed and the generation's
// end, which Go makes at random, so the round is repeated (handOverRounds).
func TestAGenerationEndingDuringTheFeedHandOverLeavesTheRelayNoSubscriber(t *testing.T) {
	t.Parallel()
	type handOver struct {
		hold, ready      sync.Once
		held, readied    chan struct{}
		release, stopped chan struct{}
	}
	stopped := make(chan struct{})
	var mu sync.Mutex
	var round *handOver
	current := func() *handOver {
		mu.Lock()
		defer mu.Unlock()
		return round
	}
	rig := newHubRig(t, func(hub *deliveryHub) {
		noted := hub.observe
		hub.observe = func(note deliveryHubNote) {
			noted(note)
			r := current()
			if r == nil {
				return
			}
			switch note.what {
			case "connected":
				// The intake makes this note: it is held here, out of its select.
				r.hold.Do(func() {
					close(r.held)
					select {
					case <-r.release:
					case <-r.stopped:
					}
				})
			case "feed ready":
				// seedEvents makes this one as its hand-over begins, which waits for the intake to
				// be held.
				select {
				case <-r.held:
				case <-r.stopped:
				}
				r.ready.Do(func() { close(r.readied) })
			}
		}
	})
	// After the rig's cleanup is registered, so it runs first and lets a held goroutine go before
	// the rig's stop waits for it.
	t.Cleanup(func() { close(stopped) })
	seedHubSettings(t, rig.database)
	for i := range handOverRounds {
		r := &handOver{held: make(chan struct{}), readied: make(chan struct{}), release: make(chan struct{}), stopped: stopped}
		mu.Lock()
		round = r
		mu.Unlock()
		conn := newFakeLiveConn()
		rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
		// The relay's generation, which seedEvents subscribes to, starts behind the event horizon.
		horizontest.WaitInProcess(t, fmt.Sprintf("round %d: seedEvents' hand-over of the relay's feed", i), r.readied, rig.database)
		rig.hub.Close(conn)
		close(r.release)
		waitReturned(t, fmt.Sprintf("round %d: every goroutine of the hub to return after its generation ended", i), &rig.hub.running)
		mu.Lock()
		round = nil
		mu.Unlock()
		if n := relaySubscribers(rig.relay); n != 0 {
			t.Fatalf("round %d: the relay keeps %d subscriber(s) after the hub's generation ended during the feed's hand-over, want none", i, n)
		}
	}
}

func TestAnEventAnotherTaskCommittedReachesThisHub(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "t")

	// Another task: its own broker and documents, on the same database.
	other := events.NewBroker()
	documents := docs.New(docs.Deps{Store: rig.database, Events: other, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: rig.database, Identity: headerIdentity(rig.database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example", Docs: documents, Events: other,
	})
	if err != nil {
		t.Fatalf("the other task's dependencies: %v", err)
	}
	otherTask := http.NewServeMux()
	t.Cleanup(Register(otherTask, t.Context(), deps).Wait)
	patchHubIssue(t, otherTask, key, map[string]any{"title": "Ship the widgets"})
	delta := conn.eventDelta(t, rig.database, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled by the other task", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// acquires is how many connections the shared pool has handed out so far.
func acquires(database *store.Store) int64 { return database.Pool.Stat().AcquireCount() }

// waitConnected waits for the hub's listener to have connected and caught the change log up, for
// the hub to hold the relay's feed, and then for the shared pool to stand still for 300 ms: the
// generation's start, its reads of the change log and the relay's reset check all take a
// connection, and a measurement of what one change costs is taken after them.
func (r *hubRig) waitConnected(t *testing.T) {
	t.Helper()
	r.waitFor(t, 0, "the listener's connection", func(note deliveryHubNote) bool { return note.what == "connected" })
	r.waitForEvent(t, 0, "the hub's event feed", func(note deliveryHubNote) bool { return note.what == "events seeded" })
	awaitStill(t, "the shared pool's acquires", 300*time.Millisecond, func() int64 { return acquires(r.deps.Store) })
}

// freshness is the next frame conn is sent, which must be a freshness frame.
func (c *fakeLiveConn) freshness(t *testing.T, what string) DeliveryLiveFreshness {
	t.Helper()
	frame := c.next(t, what)
	fresh, ok := frame.(DeliveryLiveFreshness)
	if !ok {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want %s", raw, what)
	}
	return fresh
}

// The clock stands still at noon, so the pull request's leaving, two seconds on, is the view's
// first expiry, never the midnight one, and the test moves the clock past it rather than waiting.
func TestAPresetViewSendsItsOldestPullRequestLeavingWithoutAReadingTheDatabase(t *testing.T) {
	t.Parallel()
	clock := newStillHubClock()
	rig := newMeasuredHubRig(t, clock)
	seedHubSettings(t, rig.database)
	now := clock.Now()
	upsertPR(t, rig.database, hubPR(1, now.Add(-28*24*time.Hour+2*time.Second)))
	upsertPR(t, rig.database, hubPR(2, now.Add(-time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 28, "")))
	snapshot := conn.snapshot(t, "p")
	rig.waitConnected(t)
	rig.settle(t)
	// A delivery change the snapshot did not hold may have marked the view dirty, and on this
	// clock, standing still, its recompute is due only once the clock moves. Moving it by less than
	// the two seconds the oldest pull request has left runs that recompute before the measurement,
	// and its delta, if any, is applied to the page's copy here. The row waits for that recompute to
	// have ended with nothing else owed (settle): the move made every mark the setup left due.
	held := snapshot.Body
	clock.Advance(2 * viewQuiet)
	rig.settle(t)
	for drained := false; !drained; {
		select {
		case sent := <-conn.frames:
			frame := sent.frame
			earlier, ok := frame.(DeliveryLiveDelta)
			if !ok {
				t.Fatalf("frame before the expiry = %+v, want a delta of a change the snapshot did not hold", frame)
			}
			held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, earlier, "")
		default:
			drained = true
		}
	}
	before := rig.hubAcquires()

	clock.Advance(3 * time.Second)
	delta := conn.delta(t, "p")
	if got := rig.hubAcquires(); got != before {
		t.Fatalf("the hub took %d connections while the oldest pull request left, want none", got-before)
	}
	if got := delta.Items["prs"].Remove; !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the expiry's delta removes %v, want acme/widgets#1", got)
	}
	rig.requireNothingSent(t, conn, "the expiry's one delta")
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
}

// The clock reads noon UTC (newNoonHubClock) and runs on from there, so the row's run itself
// crossing a real UTC midnight, which would move the days before the row's own Advance, decides
// nothing.
func TestAPipelineViewMovesItsDaysAtMidnightWithoutReadingTheDatabase(t *testing.T) {
	t.Parallel()
	clock := newNoonHubClock()
	rig := newMeasuredHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.snapshot(t, "p")
	rig.waitConnected(t)
	rig.settle(t)
	before := rig.hubAcquires()

	now := clock.Now()
	clock.Advance(nextUTCMidnight(now).Sub(now) + time.Second)
	delta := conn.delta(t, "p")
	if got := rig.hubAcquires(); got != before {
		t.Fatalf("the hub took %d connections at midnight, want none", got-before)
	}
	var days []string
	if err := json.Unmarshal(delta.Set["days"], &days); err != nil {
		t.Fatalf("decode the delta's days %s: %v", delta.Set["days"], err)
	}
	var want struct {
		Days []string `json:"days"`
	}
	if err := json.Unmarshal(getBody(t, rig.handler, "/api/v1/delivery/pipeline?at="+clock.Now().Format(time.RFC3339Nano)), &want); err != nil {
		t.Fatalf("decode GET /pipeline: %v", err)
	}
	if !slices.Equal(days, want.Days) {
		t.Fatalf("after midnight the delta's days = %v, GET /pipeline answers %v", days, want.Days)
	}
}

// TestAHubClockMovedBetweenTheWorkersReadAndItsArmStillFiresItsTimer moves the clock past midnight
// inside the worker's arm of its timer, after the worker has read the clock and before the timer is
// placed, where a test's Advance can land when the race detector slows the worker. It moves it at
// the first arm once the relay's feed has been applied and the view holds its answer with no wake
// left pending, so only the timer can wake the worker: the expiry the clock already passed is due,
// so the timer fires and the expiry's delta follows. The clock reads noon UTC (newNoonHubClock)
// until then, so the run itself crossing a real UTC midnight decides nothing.
//
// The feed is waited for because a generation takes it on a goroutine of its own (seedEvents), so
// it can reach the intake at any moment until then, and its apply marks every view dirty
// (applyFeed). Landing after the clock moved, that mark makes the view due as a change rather than
// as the expiry: a change recompute then sends the moved days and no expiry recompute runs. That
// recompute reads at the moved clock, so the page is still answered right, but the row then proves
// nothing about the timer.
//
// So the hook is installed only once the feed's apply has made its note, which it makes after its
// mark: no arm can then ask the hook while the feed is still to come, and no arm can be missed by
// a hook that saw the mark's wake spent before the note was made. The row then wakes the worker
// itself, as any mark would, so that the hook is asked. It installs the hook and sends that wake
// in one hold of the hub's lock, which the hook takes (catchUpNow, then its read of the view)
// before it can answer: a hook asked in between, by the arm that follows the feed's change
// recompute, waits for the lock and then finds the row's wake pending, so it answers false, and
// the row's wake is spent before the clock moves rather than standing in for the timer after it.
// A wake can also still be on its way from an earlier arm's timer: its callback can have won its
// timer's one run and be descheduled before it sends, and the worker's Stop of that timer cannot
// take the run back. Sent after the clock moved, it too would wake the worker in the timer's place.
// So the hook waits for every such callback to return (waitCallbacks) before it reads the worker's
// wakes. No timer can begin a run after that: the worker stopped its last timer before this arm,
// and this arm's timer is placed only once the hook has answered.
//
// Each arm at which the hook answers false is followed by another: the view is dirty or
// computing, so a recompute is due, or a wake is pending. Nothing else the intake can still owe
// here, a catch-up of the change log after a notification or a connection, marks the view, since
// the row writes nothing after the generation starts.
func TestAHubClockMovedBetweenTheWorkersReadAndItsArmStillFiresItsTimer(t *testing.T) {
	t.Parallel()
	clock := newNoonHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.snapshot(t, "p")

	// The feed stands behind the server's event horizon, so the wait for it is the horizon's.
	rig.waitForEvent(t, 0, "the hub's event feed", func(note deliveryHubNote) bool { return note.what == "events seeded" })
	// No hub code holds the fake clock's lock while it takes the hub's, and the hook runs outside
	// the clock's lock (fakeHubClock.At), so the clock's onArm under the hub's lock cannot deadlock.
	rig.hub.mu.Lock()
	clock.onArm(func() bool {
		// catchUpNow returns once the intake has finished whatever it had already taken off its
		// channels, such as a catch-up under way, so a mark that work made is on the view by the
		// time the view is read.
		rig.hub.catchUpNow()
		clock.waitCallbacks()
		rig.hub.mu.Lock()
		g := rig.hub.generation
		ready := g != nil && len(g.views) == 1 && len(g.wakeWork) == 0
		if ready {
			for _, view := range g.views {
				ready = ready && view.body != nil && !view.dirty && !view.computing && !view.expiry.IsZero()
			}
		}
		rig.hub.mu.Unlock()
		if ready {
			now := clock.Now()
			clock.Advance(nextUTCMidnight(now).Sub(now) + time.Second)
		}
		return ready
	})
	g := rig.hub.generation
	signal(g.wakeWork)
	rig.hub.mu.Unlock()
	// The expiry's recompute is what the row waits for, with the test binary's deadline as its only
	// bound. It sends its delta, when it has one, before it makes its note (expire, finishRecompute),
	// so the read below takes a delta already sent, and an expiry that sent nothing fails it by name
	// before the test binary's deadline (testwait.Receive).
	rig.waitFor(t, 0, "the expiry's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseExpiry
	})
	delta := conn.delta(t, "p")
	var days []string
	if err := json.Unmarshal(delta.Set["days"], &days); err != nil {
		t.Fatalf("decode the delta's days %s: %v", delta.Set["days"], err)
	}
	var want struct {
		Days []string `json:"days"`
	}
	if err := json.Unmarshal(getBody(t, rig.handler, "/api/v1/delivery/pipeline?at="+clock.Now().Format(time.RFC3339Nano)), &want); err != nil {
		t.Fatalf("decode GET /pipeline: %v", err)
	}
	if !slices.Equal(days, want.Days) {
		t.Fatalf("after midnight the delta's days = %v, GET /pipeline answers %v", days, want.Days)
	}
}

func TestAPresetRecomputeWhereOnlyTheWindowMovedSendsNothing(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	// A pull request of another repository, merged before the window: no view shows it, but a change
	// to it is before the timeline's end, so it recomputes the view.
	old := hubPR(1, rig.clock.Now().Add(-30*24*time.Hour))
	old.Repo, old.URL = "acme/other", "https://github.com/acme/other/pull/1"
	upsertPR(t, rig.database, old)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "p")

	start := rig.mark()
	old.Title = "feat: a title no view shows"
	upsertPR(t, rig.database, old)
	rig.waitFor(t, start, "the recompute of the change", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	})
	rig.requireNothingSent(t, conn, "a recompute whose window alone moved")
}

func TestFreshnessIsSentOnlyWhenItsVerdictChanges(t *testing.T) {
	t.Parallel()
	clock := &fakeHubClock{}
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	// A custom range has no expiry, so the clock's move sends nothing but freshness.
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	conn.snapshot(t, "m")
	rig.waitConnected(t)
	pool := rig.database.Pool

	for range 2 {
		at := clock.Now()
		if err := delivery.RecordReconcileSuccess(t.Context(), pool, "acme/widgets", at); err != nil {
			t.Fatalf("record a reconcile: %v", err)
		}
		rig.waitFreshnessHeard(t, "the hub to hear the reconcile that changed no verdict", reconciledAt(at))
		conn.empty(t, "a reconcile that changed no verdict")
	}
	if err := delivery.RecordReconcileError(t.Context(), pool, "acme/widgets", "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	failed := conn.freshness(t, "the freshness frame naming the error")
	if failed.Freshness.LastError == nil || *failed.Freshness.LastError != "GitHub answered 502" || len(failed.Stale) != 0 {
		t.Fatalf("the error's freshness frame = %+v, want last_error \"GitHub answered 502\" and nothing stale", failed)
	}
	cleared := clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), pool, "acme/widgets", cleared); err != nil {
		t.Fatalf("record the next reconcile: %v", err)
	}
	clearing := conn.freshness(t, "the freshness frame clearing the error")
	if clearing.Freshness.LastError != nil || len(clearing.Stale) != 0 {
		t.Fatalf("the clearing freshness frame = %+v, want no last_error and nothing stale", clearing)
	}
	rig.waitFreshnessHeard(t, "the hub to hear the reconcile that cleared the error", reconciledAt(cleared))
	conn.empty(t, "the clearing frame")

	// No reconcile for three intervals.
	clock.Advance(deliveryStaleAfter + time.Second)
	stale := conn.freshness(t, "the freshness frame naming the stale sources")
	if !slices.Equal(stale.Stale, []string{"prs", "runs", "ci"}) {
		t.Fatalf("the stale freshness frame names %v, want [prs runs ci]", stale.Stale)
	}
	// The pass that clears the stale verdict sends the next frame the page is sent, so a second
	// frame for the stale verdict would be read in its place here.
	passed := clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), pool, "acme/widgets", passed); err != nil {
		t.Fatalf("record the reconcile that clears the stale sources: %v", err)
	}
	again := conn.freshness(t, "the freshness frame clearing the stale sources")
	if len(again.Stale) != 0 || again.Freshness.LastError != nil {
		t.Fatalf("the freshness frame after the next pass = %+v, want nothing stale and no last_error", again)
	}
	rig.waitFreshnessHeard(t, "the hub to hear the pass that cleared the stale sources", reconciledAt(passed))
	conn.empty(t, "the frame clearing the stale sources")
}

// TestTheHubJoinsTheSourcesLastErrorsInTheGETRoutesOrder: two sources whose repositories sort one
// way by their bytes and the other way in lower case (Zeta/app before acme/web, zeta/app after),
// each with a last error: the hub's freshness (deliveryFreshnessHeard) joins the two errors in the
// rules' order, as the GET routes' deliveryFreshness does, so a page reads one text whichever of
// the two answered it. Red where the hub orders the sources by their lower-case keys.
func TestTheHubJoinsTheSourcesLastErrorsInTheGETRoutesOrder(t *testing.T) {
	t.Parallel()
	rules := delivery.NewSourceRules([]delivery.ProjectSettings{{Project: "ACME", Delivery: true, Sources: []delivery.DeploySource{
		{Kind: delivery.SourceKindGitHubWorkflow, Repo: new("acme/web"), LastError: new("GitHub answered 502")},
		{Kind: delivery.SourceKindGitHubWorkflow, Repo: new("Zeta/app"), LastError: new("the token was refused")},
	}}})
	want := deliveryFreshness(rules, 0).LastError
	if want == nil {
		t.Fatal("the GET routes' freshness states no last error of two sources that each hold one")
	}
	var heard deliveryFreshnessHeard
	heard.heardRules(rules)
	if heard.lastError == nil {
		t.Fatalf("the hub states no last error, want the GET routes' %q", *want)
	}
	if *heard.lastError != *want {
		t.Fatalf("the hub's last error = %q, want the GET routes' %q", *heard.lastError, *want)
	}
}

func TestAnUnfetchablePullRequestSendsAFreshnessFrame(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-3*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "p")
	rig.waitConnected(t)

	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 1, "the pull request is gone"); err != nil {
		t.Fatalf("mark the pull request unfetchable: %v", err)
	}
	delta := conn.delta(t, "p")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the mark's delta upserts %v, want acme/widgets#1", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
	fresh := conn.freshness(t, "the freshness frame carrying the unfetchable count")
	if fresh.Freshness.UnfetchableCount != 1 {
		t.Fatalf("the freshness frame's unfetchable_count = %d, want 1", fresh.Freshness.UnfetchableCount)
	}
	rig.requireNothingSent(t, conn, "the mark's delta and freshness frame")
}

// A page keeps the last verdict it was sent, and the hub sends one to its open pages only when the
// verdict changes, so a socket that was down at a change, and a page that held no delivery view
// then, would show the old verdict for good. A subscription is therefore sent the verdict the hub
// knows right after its first answer: a socket opened since, one that resumes with nothing to
// catch up, and a page's next view on a socket it already holds. The first verdict is set
// silently, from the read whose body the first subscription is answered, so that one is told
// nothing more.
func TestASubscriptionAfterAVerdictChangeIsSentThatVerdict(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	// A custom range and a pinned pipeline day have no expiry, so nothing but the answers and the
	// verdict reaches the conns.
	measures := measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")
	at := time.Date(2024, 6, 4, 12, 0, 0, 0, time.UTC)
	page := newFakeLiveConn()
	rig.hub.Subscribe(page, subscribeFrame("m", measures))
	held := page.snapshot(t, "m")
	rig.requireNothingSent(t, page, "the first snapshot, whose verdict is set silently")
	rig.waitConnected(t)

	if err := delivery.RecordReconcileError(t.Context(), rig.database.Pool, "acme/widgets", "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	requireVerdict := func(conn *fakeLiveConn, what string) {
		t.Helper()
		verdict := conn.freshness(t, what)
		if verdict.Freshness.LastError == nil || *verdict.Freshness.LastError != "GitHub answered 502" || len(verdict.Stale) != 0 {
			t.Fatalf("%s = %+v, want last_error \"GitHub answered 502\" and nothing stale", what, verdict)
		}
		rig.requireNothingSent(t, conn, what)
	}
	requireVerdict(page, "the verdict the change sent the open page")

	opened := newFakeLiveConn()
	rig.hub.Subscribe(opened, subscribeFrame("m", measures))
	opened.snapshot(t, "m")
	requireVerdict(opened, "the verdict after a new socket's snapshot")

	resumed := newFakeLiveConn()
	resume := subscribeFrame("m", measures)
	resume.Resume = &DeliveryLiveResume{Seq: held.Seq, Revs: held.Revs}
	rig.hub.Subscribe(resumed, resume)
	requireVerdict(resumed, "the verdict for a resume with nothing to catch up")

	rig.hub.Subscribe(page, subscribeFrame("p", livePipelineView(&at)))
	page.snapshot(t, "p")
	requireVerdict(page, "the verdict after the page's next view's snapshot")
}

func TestASubscriberGetsTheGETBodyAsItsSnapshot(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	fixed := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	for i, merged := range []time.Time{fixed, fixed.Add(3 * time.Hour), now.Add(-26 * time.Hour), now.Add(-2 * time.Hour)} {
		upsertPR(t, rig.database, hubPR(i+1, merged))
	}
	upsertDeploy(t, rig.database, 100, fixed.Add(time.Hour), fixed.Add(2*time.Hour))
	upsertDeploy(t, rig.database, 101, now.Add(-25*time.Hour), now.Add(-24*time.Hour))

	from, to := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)
	at := fixed.Add(30 * time.Hour)
	conn := newFakeLiveConn()
	for i, test := range []struct {
		sub   string
		view  DeliveryLiveView
		kind  string
		query string
	}{
		{sub: "timeline", view: timelineRange(from, to, "author=octocat"), kind: deliveryLiveTimeline, query: "author=octocat"},
		{sub: "measures", view: measuresRange(from, to, ""), kind: deliveryLiveMeasures},
		{sub: "pipeline", view: livePipelineView(&at), kind: deliveryLivePipeline, query: "at=" + at.Format(time.RFC3339Nano)},
		{sub: "preset", view: presetView(deliveryLiveTimeline, 7, ""), kind: deliveryLiveTimeline},
	} {
		rig.hub.Subscribe(conn, subscribeFrame(test.sub, test.view))
		snapshot := conn.snapshot(t, test.sub)
		if i > 0 {
			// The first view's read set the hub's verdict silently; every later view's snapshot is
			// followed by it.
			conn.freshness(t, "the verdict after the snapshot of "+test.sub)
		}
		requireSameBody(t, test.sub, snapshot.Body, getBody(t, rig.handler, windowTarget(t, test.kind, snapshot.Body, test.query)))
		if snapshot.Seq == "" || len(snapshot.Revs.Fields) == 0 {
			t.Fatalf("%s snapshot seq = %q, revs = %+v; want its change position and every revision", test.sub, snapshot.Seq, snapshot.Revs)
		}
	}
}

// TestTheHubServesAViewOverTwoSourceRepositoriesEachUnderItsOwnRule (dispatch://INFRA-978, D9):
// ALPHA reads two source repositories, acme/one under the production job `one / release` and
// acme/two under `two / release`, and each holds a run of its own job and a run of the other's:
// acme/one's 1001 and acme/two's 1002 are deploys by their own repository's rule, and acme/two's
// 1003 (one's job) and acme/one's 1004 (two's job) are none. A subscriber's first answer of the
// `measures` view counts two successful deploys and its first answer of the `pipeline` view holds a
// production card of both repositories' jobs with two deploys, each the body its route answers, as
// the routes count them (TestTheDeliveryRoutesReadEachSourceRepositoryUnderItsOwnRule).
func TestTheHubServesAViewOverTwoSourceRepositoriesEachUnderItsOwnRule(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	createTestProject(t, rig.handler, "ALPHA")
	saveProjectRules(t, rig.database, "ALPHA", deployRule("acme/one", "one / release"), deployRule("acme/two", "two / release"))
	seedSourceRun(t, rig.database, "acme/one", 1001, 1, "one / release")
	seedSourceRun(t, rig.database, "acme/two", 1002, 2, "two / release")
	seedSourceRun(t, rig.database, "acme/two", 1003, 3, "one / release")
	seedSourceRun(t, rig.database, "acme/one", 1004, 4, "two / release")
	from, to := sourcesDay, sourcesDay.AddDate(0, 0, 1)
	at := sourcesDay.Add(20 * time.Hour)

	conn := newFakeLiveConn()
	bodies := map[string]json.RawMessage{}
	for i, test := range []struct {
		sub, kind, query string
		view             DeliveryLiveView
	}{
		{"measures", deliveryLiveMeasures, "", measuresRange(from, to, "")},
		{"pipeline", deliveryLivePipeline, "at=" + at.Format(time.RFC3339Nano), livePipelineView(&at)},
	} {
		rig.hub.Subscribe(conn, subscribeFrame(test.sub, test.view))
		snapshot := conn.snapshot(t, test.sub)
		if i > 0 {
			// The first view's read set the hub's verdict silently; every later view's snapshot is
			// followed by it.
			conn.freshness(t, "the verdict after the snapshot of "+test.sub)
		}
		requireSameBody(t, test.sub, snapshot.Body, getBody(t, rig.handler, windowTarget(t, test.kind, snapshot.Body, test.query)))
		bodies[test.sub] = snapshot.Body
	}

	var measured deliveryMeasuresResponse
	if err := json.Unmarshal(bodies["measures"], &measured); err != nil {
		t.Fatalf("decode the measures snapshot: %v", err)
	}
	if deploys := measured.Measures.DeployFrequency.SuccessfulDeploys; deploys != 2 {
		t.Errorf("the hub's measures view counts %d successful deploys, want 2: acme/one's 1001 and acme/two's 1002, each by its own rule", deploys)
	}
	var pipeline deliveryPipelineResponse
	if err := json.Unmarshal(bodies["pipeline"], &pipeline); err != nil {
		t.Fatalf("decode the pipeline snapshot: %v", err)
	}
	if card := pipeline.ProductionApply; card.Job != "one / release or two / release" || card.Total.N != 2 {
		t.Errorf("the hub's pipeline view holds the production card %+v, want both repositories' jobs and their two deploys", card)
	}
}

func TestAMergedPullRequestReachesTheSubscriberAsOneDelta(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	upsertPR(t, rig.database, hubPR(1, now.Add(-30*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "p")

	// The second pull request was open five hours where the first was open two (hubPR), so the
	// opened-to-merge spread moves, and the timeline's measures with it: the one delta sets the
	// measures beside the window. No deploy shipped it, so no deploy or lead-time-to-production
	// measure moves.
	second := hubPR(2, rig.clock.Now().Add(-time.Hour))
	second.CreatedAt = new(second.MergedAt.Add(-5 * time.Hour))
	upsertPR(t, rig.database, second)
	delta := conn.delta(t, "p")
	set, items, mapsChanged, counts := liveSections(delta)
	if !slices.Equal(items, []string{"prs"}) || !slices.Equal(liveUpsertIDs(t, delta, "prs"), []string{"acme/widgets#2"}) || len(delta.Items["prs"].Remove) != 0 {
		t.Fatalf("delta items = %v, prs upsert %v remove %v; want acme/widgets#2 upserted alone", items, liveUpsertIDs(t, delta, "prs"), delta.Items["prs"].Remove)
	}
	if !slices.Equal(set, []string{"measures", "window"}) || len(mapsChanged) != 0 {
		t.Fatalf("delta set = %v, maps = %v; want measures and window, and no map", set, mapsChanged)
	}
	var held struct {
		FacetCounts map[string]map[string]int `json:"facet_counts"`
		ColorCounts map[string]map[string]int `json:"color_counts"`
	}
	if err := json.Unmarshal(snapshot.Body, &held); err != nil {
		t.Fatalf("decode the snapshot's counts: %v", err)
	}
	for _, name := range counts {
		before := map[string]map[string]map[string]int{"facet_counts": held.FacetCounts, "color_counts": held.ColorCounts}[name]
		for facet, values := range liveCounts(delta, name) {
			for value, count := range values {
				if previous, ok := before[facet][value]; ok && previous == count {
					t.Fatalf("delta %s[%s][%s] = %d, the count the page already holds", name, facet, value, count)
				}
			}
		}
	}
	if len(counts) == 0 {
		t.Fatal("the merge moved no count")
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
	rig.requireNothingSent(t, conn, "the merge's one delta")
}

func TestAReconcilePassThatFindsNothingNewSendsNothing(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, "acme/widgets", rig.clock.Now()); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	now := rig.clock.Now()
	prs := []delivery.DeliveryPullRequest{hubPR(1, now.Add(-30*time.Hour)), hubPR(2, now.Add(-3*time.Hour))}
	for _, pr := range prs {
		upsertPR(t, rig.database, pr)
	}
	upsertDeploy(t, rig.database, 100, now.Add(-29*time.Hour), now.Add(-28*time.Hour))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "p")

	// The pass writes every row again as it was, and records its success.
	for _, pr := range prs {
		upsertPR(t, rig.database, pr)
	}
	upsertDeploy(t, rig.database, 100, now.Add(-29*time.Hour), now.Add(-28*time.Hour))
	passed := rig.clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, "acme/widgets", passed); err != nil {
		t.Fatalf("record the pass: %v", err)
	}
	rig.waitFreshnessHeard(t, "the hub to hear the reconcile pass", reconciledAt(passed))
	rig.requireNothingSent(t, conn, "a reconcile pass that found nothing new")
}

// TestARecomputeAfterTiedRowsAreWrittenAgainSendsNothing: a reconcile pass writes a failed run's
// jobs and the pull requests a deploy shipped again as they were, which logs no change but puts each
// row's new version after the rest of its table. The jobs started and finished together and the
// pull requests merged in one second, so an order neither read decides would come back changed on
// the next recompute, here one for a change no view shows, and the hub would send both runs again.
// The hub reads on a sequential scan, the plan that answers rows as they lie.
func TestARecomputeAfterTiedRowsAreWrittenAgainSendsNothing(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	deps.Store = &store.Store{Pool: storetest.Planned(t, database.Pool, storetest.SequentialScan)}
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	seedHubSettings(t, rig.database)
	ctx := context.Background()
	now := rig.clock.Now().Truncate(time.Second)

	merged := now.Add(-30 * time.Hour)
	prs := []delivery.DeliveryPullRequest{hubPR(1, merged), hubPR(2, merged), hubPR(3, merged)}
	for _, pr := range prs {
		upsertPR(t, rig.database, pr)
	}
	upsertDeploy(t, rig.database, 101, now.Add(-29*time.Hour), now.Add(-28*time.Hour))

	started, finished := now.Add(-27*time.Hour), now.Add(-26*time.Hour)
	failure, jobFailure, jobSkipped := delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure, delivery.DeliveryJobConclusionSkipped
	if err := delivery.UpsertRun(ctx, rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 102, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha102",
		HeadCommitAt: started, StartedAt: started, CompletedAt: &finished, Conclusion: &failure,
		URL: "https://github.com/acme/widgets/actions/runs/102", HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert failed run 102: %v", err)
	}
	// GitHub's listing order, which is neither the start order nor the name order.
	jobs := []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 102, Name: "test / unit", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobFailure},
		{Repo: "acme/widgets", RunID: 102, Name: "lint", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobFailure},
		{Repo: "acme/widgets", RunID: 102, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobFailure},
		{Repo: "acme/widgets", RunID: 102, Name: "release / release", Conclusion: &jobSkipped},
	}
	if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", 102, jobs, true); err != nil {
		t.Fatalf("upsert run 102's jobs: %v", err)
	}
	// A pull request of another repository, merged before the window: a change to it recomputes
	// the view and changes nothing it shows.
	other := hubPR(9, now.Add(-30*24*time.Hour))
	other.Repo, other.URL = "acme/other", "https://github.com/acme/other/pull/9"
	upsertPR(t, rig.database, other)

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "p")

	// The pass writes the first-listed job and pull request again as they were.
	if err := delivery.UpsertRunJobs(ctx, rig.database.Pool, "acme/widgets", 102, jobs[:1], true); err != nil {
		t.Fatalf("upsert run 102's first job again: %v", err)
	}
	upsertPR(t, rig.database, prs[0])
	start := rig.mark()
	other.Title = "feat: a title no view shows"
	upsertPR(t, rig.database, other)
	rig.waitFor(t, start, "the recompute of the change", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	})
	rig.requireNothingSent(t, conn, "a recompute after tied rows were written again unchanged")
}

func TestIdenticalViewsShareOneRecompute(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-30*time.Hour)))
	// A measures view starts the generation and holds it open while the timeline views measured
	// here subscribe after it: the relay's feed, taken on a goroutine of the generation's own,
	// marks every view it finds dirty when it arrives (applyFeed), and that recompute belongs to
	// none of what this measures.
	warm := newFakeLiveConn()
	rig.hub.Subscribe(warm, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	warm.snapshot(t, "m")
	rig.settle(t)
	first, second := newFakeLiveConn(), newFakeLiveConn()
	start := rig.mark()
	rig.hub.Subscribe(first, subscribeFrame("a", presetView(deliveryLiveTimeline, 7, "author=octocat&repo=acme/widgets")))
	first.snapshot(t, "a")
	first.freshness(t, "the verdict the running generation already knows")
	// The same view, its query's parameters in another order.
	rig.hub.Subscribe(second, subscribeFrame("b", presetView(deliveryLiveTimeline, 7, "repo=acme%2Fwidgets&author=octocat")))
	second.snapshot(t, "b")
	second.freshness(t, "the verdict the first view's read set, after the second's snapshot")
	timeline := func(cause string) func(deliveryHubNote) bool {
		begin := recomputeBegin(cause)
		return func(note deliveryHubNote) bool {
			return begin(note) && strings.HasPrefix(note.view, deliveryLiveTimeline)
		}
	}
	if n := rig.count(start, timeline("")); n != 1 {
		t.Fatalf("recomputes for two identical views = %d, want 1", n)
	}

	merged := rig.mark()
	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
	first.delta(t, "a")
	second.delta(t, "b")
	if n := rig.count(merged, timeline(deliveryCauseChange)); n != 1 {
		t.Fatalf("recomputes for one merge both views show = %d, want 1", n)
	}
}

// TestABurstCoalescesIntoOneRecompute writes 50 merges on a clock that stands still, so the burst
// is one instant to the hub however long the writes take: nothing is due until the clock moves
// viewQuiet past the last mark, and then one recompute sends all 50 in one delta.
func TestABurstCoalescesIntoOneRecompute(t *testing.T) {
	t.Parallel()
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, "p")

	rig.settle(t)
	start := rig.mark()
	now := clock.Now()
	for n := 1; n <= 50; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(n)*time.Minute)))
	}
	rig.hub.catchUpNow()
	rig.requireNothingSent(t, conn, "a burst the clock has not moved past")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 0 {
		t.Fatalf("recomputes before the burst's quiet passed = %d, want 0", n)
	}

	clock.Advance(viewQuiet)
	delta := conn.delta(t, "p")
	if got := len(liveUpsertIDs(t, delta, "prs")); got != 50 {
		t.Fatalf("the burst's delta upserts %d pull requests, want all 50", got)
	}
	rig.requireNothingSent(t, conn, "the burst's one delta")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 1 {
		t.Fatalf("recomputes for a burst of 50 upserts = %d, want 1", n)
	}
}

func TestOneRecomputeAtATime(t *testing.T) {
	t.Parallel()
	var inFlight, most atomic.Int32
	var holding atomic.Bool
	begun, held := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			switch note.what {
			case "recompute begin":
				n := inFlight.Add(1)
				for {
					seen := most.Load()
					if n <= seen || most.CompareAndSwap(seen, n) {
						break
					}
				}
				if holding.CompareAndSwap(true, false) {
					// Hold the merge's first recompute open until the merge has left every other
					// view due one too, so a hub that ran two at once would begin a second by then.
					close(begun)
					<-held
				}
			case "recompute end":
				inFlight.Add(-1)
			}
			observe(note)
		}
	})
	// Registered after the rig's, so it runs first: a failed test never leaves the hub held.
	t.Cleanup(release)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	views := []DeliveryLiveView{
		presetView(deliveryLiveTimeline, 7, ""), presetView(deliveryLiveTimeline, 14, ""),
		presetView(deliveryLiveMeasures, 7, ""), presetView(deliveryLiveMeasures, 28, ""), livePipelineView(nil),
	}
	subs := make([]string, len(views))
	for i, view := range views {
		subs[i] = "v" + strconv.Itoa(i)
		rig.hub.Subscribe(conn, subscribeFrame(subs[i], view))
	}
	conn.firstSnapshots(t, subs...)
	rig.settle(t)

	holding.Store(true)
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-time.Hour)))
	testwait.Receive(t, "the merge's first recompute, held open", begun)
	testwait.Until(t, "the merge to leave every other view due a recompute, or a second recompute to begin (how many are due)", func() (int, bool) {
		now := rig.clock.Now()
		rig.hub.mu.Lock()
		defer rig.hub.mu.Unlock()
		due := 0
		for _, view := range rig.hub.generation.views {
			if at, _, ok := view.dueAt(); ok && view.dirty && !at.After(now) {
				due++
			}
		}
		return due, due == len(views)-1 || most.Load() > 1
	})
	release()
	for range views {
		if _, ok := conn.next(t, "every view's delta").(DeliveryLiveDelta); !ok {
			t.Fatal("want a delta for every view")
		}
	}
	if n := most.Load(); n != 1 {
		t.Fatalf("recomputes at once = %d, want 1", n)
	}
}

// TestTheHubRecomputesUpToEvalsRecomputesEvalsViewsAtOnce: evalsRecomputes+1 evals views of one
// project, a timeline and a measures view are open, and a day row of the project marks every evals
// view at one moment. The reads of the first evalsRecomputes evals views to take their position are
// held there: evalsRecomputes begin and are held at once, and the one that waits, which the worker
// has picked, begins no recompute while they hold every slot. All but the first of those held are
// let go and finish while the first is still held; the one that waited then takes a slot and
// finishes beside it; and a merge made while the first is held marks the timeline and the
// measures view, which the worker recomputes itself, one at a time, while the first is still held.
// No view is computed twice at once, each evals view is recomputed once, and every view is sent its
// one delta. A hub that recomputes one view at a time begins no second evals view while the first
// is held, so the wait for the held reads has a bound of its own rather than the test binary's
// deadline.
func TestTheHubRecomputesUpToEvalsRecomputesEvalsViewsAtOnce(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		computing = map[string]int{}
		// evalsNow and deliveryNow are the recomputes of each kind under way, and mostEvals and
		// mostDelivery the most of each there were at once; twice names each view a recompute began
		// on while another was under way.
		evalsNow, deliveryNow, mostEvals, mostDelivery int
		twice                                          []string
		// gates lets go each read held, by its view.
		gates   = map[string]chan struct{}{}
		holding atomic.Int32
		held    = make(chan string, evalsRecomputes)
	)
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe, lifetime := hub.observe, hub.lifetime
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			evals := strings.HasPrefix(note.view, evalsView+"|")
			switch note.what {
			case "recompute begin":
				mu.Lock()
				if computing[note.view]++; computing[note.view] > 1 {
					twice = append(twice, note.view)
				}
				if evals {
					evalsNow++
					mostEvals = max(mostEvals, evalsNow)
				} else {
					deliveryNow++
					mostDelivery = max(mostDelivery, deliveryNow)
				}
				mu.Unlock()
			case "recompute end":
				mu.Lock()
				computing[note.view]--
				if evals {
					evalsNow--
				} else {
					deliveryNow--
				}
				mu.Unlock()
			case "recompute position":
				if !evals || holding.Add(-1) < 0 {
					return
				}
				gate := make(chan struct{})
				mu.Lock()
				gates[note.view] = gate
				mu.Unlock()
				held <- note.view
				// The wait also ends with the hub's lifetime, so a test that fails while a read is
				// held still stops its hub.
				select {
				case <-gate:
				case <-lifetime.Done():
				}
			}
		}
	})
	release := func(key string) {
		mu.Lock()
		gate := gates[key]
		mu.Unlock()
		close(gate)
	}
	createTestProject(t, rig.handler, "ACME")
	saveProjectRules(t, rig.database, "ACME", deployRule("acme/widgets", "release / release"))
	today := rig.clock.Now().UTC().Truncate(24 * time.Hour)
	// The day row below is of acme/widgets', which a read of ACME's evals reads once the refresher's
	// record names it (eval_days_rules, evals.DeployReposIn).
	publishDeployRepo(t, rig.database, "ACME", deployRule("acme/widgets", "release / release"), today.AddDate(0, 0, -27))
	keyOf := func(frame DeliveryLiveView) string {
		spec, refusal := parseDeliveryLiveView(frame)
		if refusal != nil {
			t.Fatalf("read the %s view: %+v", frame.Kind, refusal)
		}
		return spec.key
	}

	delivered := newFakeLiveConn()
	timeline, measures := presetView(deliveryLiveTimeline, 7, ""), presetView(deliveryLiveMeasures, 7, "")
	rig.hub.Subscribe(delivered, subscribeFrame("t", timeline))
	rig.hub.Subscribe(delivered, subscribeFrame("m", measures))
	delivered.firstSnapshots(t, "t", "m")
	// evalsRecomputes+1 evals views of ACME: one of each preset, then windows of the days to
	// tomorrow, a day longer each, for the rest.
	frames := make([]DeliveryLiveView, evalsRecomputes+1)
	for i := range frames {
		window := evalsDays(today.AddDate(0, 0, -27-i), today.AddDate(0, 0, 1))
		if i < len(deliveryLivePresets) {
			window = DeliveryLiveWindow{Preset: new(deliveryLivePresets[i])}
		}
		frames[i] = evalsLiveView("ACME", window, "")
	}
	conns, keys := make([]*fakeLiveConn, len(frames)), make([]string, len(frames))
	for i, frame := range frames {
		sub := "e" + strconv.Itoa(i)
		keys[i], conns[i] = keyOf(frame), newFakeLiveConn()
		rig.hub.Subscribe(conns[i], subscribeFrame(sub, frame))
		conns[i].snapshot(t, sub)
		conns[i].freshness(t, "the verdict after the snapshot of "+sub)
	}
	rig.settle(t)

	start := rig.mark()
	holding.Store(evalsRecomputes)
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into eval_days (project_key, eval_id, deploy_repo, series, repo, day, listed, n, sum, counts, built_by)
		values ('ACME', 'ACME/deploy-frequency', 'acme/widgets', 'deploys', '', $1, true, 1, 1, '{"with_prs": 1}', 'test')
	`, today); err != nil {
		t.Fatalf("write ACME's day row of deploy-frequency: %v", err)
	}
	rig.hub.catchUpNow()
	bound := testwait.Bounded(t, 30*time.Second, time.Second)
	var reading []string
	for range evalsRecomputes {
		select {
		case key := <-held:
			reading = append(reading, key)
		case <-time.After(bound):
			t.Fatalf("%d evals views' reads were held %s after the day row marked %d, want %d at once (evalsRecomputes)", len(reading), bound, len(frames), evalsRecomputes)
		}
	}
	first, waiting := reading[0], ""
	for _, key := range keys {
		if !slices.Contains(reading, key) {
			waiting = key
		}
	}
	begins := func(key string) int {
		return rig.count(start, func(note deliveryHubNote) bool { return note.what == "recompute begin" && note.view == key })
	}
	ends := func(key string) int {
		return rig.count(start, func(note deliveryHubNote) bool { return note.what == "recompute end" && note.view == key })
	}
	rig.waitNoRecompute(t, 2*viewQuiet)
	if n := begins(waiting); n != 0 {
		t.Fatalf("the evals view that waited began %d recomputes while %d held every slot, want none", n, evalsRecomputes)
	}

	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-time.Hour)))
	rig.hub.catchUpNow()
	for _, key := range reading[1:] {
		release(key)
	}
	finished := append(slices.Clone(reading[1:]), waiting, keyOf(timeline), keyOf(measures))
	testwait.Until(t, "the evals views let go, the one that waited and both delivery views to finish while the first is held (those left)", func() ([]string, bool) {
		var left []string
		for _, key := range finished {
			if ends(key) == 0 {
				left = append(left, key)
			}
		}
		return left, len(left) == 0
	})
	if n := ends(first); n != 0 {
		t.Fatalf("the held evals view ended %d recomputes before it was let go", n)
	}
	release(first)

	for i, conn := range conns {
		conn.delta(t, "e"+strconv.Itoa(i))
	}
	for range 2 {
		if _, ok := delivered.next(t, "each delivery view's delta").(DeliveryLiveDelta); !ok {
			t.Fatal("want a delta for each delivery view")
		}
	}
	rig.settle(t)
	for _, key := range keys {
		if n := begins(key); n != 1 {
			t.Errorf("%s began %d recomputes for the one day row, want 1", key, n)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(twice) > 0 {
		t.Errorf("recomputes began on %v while another of the same view was under way, want none", twice)
	}
	if mostEvals != evalsRecomputes || mostDelivery != 1 {
		t.Errorf("at most %d evals views and %d delivery views were computed at once, want %d and 1", mostEvals, mostDelivery, evalsRecomputes)
	}
	t.Logf("%d evals views computed at once, one waiting; the delivery views one at a time, while the first evals view was held", mostEvals)
}

func TestAChangeOutsideAWindowRecomputesNothing(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	from, to := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(from, to, "")))
	conn.snapshot(t, "m")

	rig.settle(t)
	start := rig.mark()
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now()))
	rig.hub.catchUpNow()
	rig.requireNothingSent(t, conn, "a merge outside the view's window")
	if n := rig.count(start, recomputeBegin("")); n != 0 {
		t.Fatalf("recomputes for a merge outside the window = %d, want 0", n)
	}
}

func TestNotConfiguredIsAnErrorFrameThenASnapshotOnceConfigured(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	conn := newFakeLiveConn()
	view := presetView(deliveryLiveTimeline, 7, "")
	rig.hub.Subscribe(conn, subscribeFrame("p", view))
	conn.refusal(t, "p", "DELIVERY_NOT_CONFIGURED")

	// A change while delivery is still not configured tells the page nothing more.
	start := rig.mark()
	upsertPR(t, rig.database, hubPR(1, rig.clock.Now().Add(-time.Hour)))
	rig.waitFor(t, start, "a recompute of the merge", recomputeBegin(deliveryCauseChange))
	rig.requireNothingSent(t, conn, "a second recompute while delivery is not configured")

	seedHubSettings(t, rig.database)
	snapshot := conn.snapshot(t, "p")
	requireSameBody(t, "the first snapshot once configured", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))
}

// legionView is a subscribe frame's view for the Legion board of project.
func legionView(project string) DeliveryLiveView {
	return DeliveryLiveView{Kind: legionBoardView, Project: &project}
}

// legionBodyProject is the project a Legion board body names, which is the whole of what its GET
// route reads.
func legionBodyProject(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var shape struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the board body's project: %v", err)
	}
	return shape.Project
}

// TestALegionBoardsSnapshotAndDeltasLandOnItsGET: the fourth view kind holds the hub's own rule —
// the snapshot plus every delta a page applies is the GET route's body — through a record of the
// daemon the Legion intake announces (legion.StateChannel) and through a write of one of the
// board's issues, the two things that move it.
func TestALegionBoardsSnapshotAndDeltasLandOnItsGET(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	nameLegionDaemon(t, rig.handler, "INFRA", "http://legion-infra.internal:13370")
	seedLegionBoard(t, rig.database, "INFRA", rig.clock.Now())

	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	snapshot := conn.snapshot(t, "b")
	requireSameBody(t, "the board's first snapshot", snapshot.Body,
		getBody(t, rig.handler, windowTarget(t, legionBoardView, snapshot.Body, "")))
	// The listener's connection and the relay's feed each mark the board (dirtyLegionViews,
	// applyFeed): both are answered before the sampler's rows are written, so no recompute reads
	// them half written.
	rig.waitFor(t, 0, "the listener's connection", func(note deliveryHubNote) bool { return note.what == "connected" })
	rig.settle(t)

	// The intake's next record finds the implementer's child in review, with no role in a turn.
	held := snapshot.Body
	now := rig.clock.Now()
	seedLegionObservation(t, rig.database, "INFRA", "INFRA-4", legion.FieldPhase, `"reviewing"`, now)
	seedLegionObservation(t, rig.database, "INFRA", "INFRA-4", legion.ClaimFieldPrefix+"implementer", `"idle"`, now)
	seedLegionRead(t, rig.database, "INFRA", now, "")
	notifyChannel(t, rig.database, legion.StateChannel, "INFRA")
	held = requireLandsOnGET(t, rig.handler, legionBoardView, held, conn.delta(t, "b"), "")

	// A title Dispatch itself rewrites moves the card it is on, through the event log. Its event
	// reaches the hub on the relay's feed, which stands behind the event horizon (eventDelta).
	patched := dispatchRequest(t, rig.handler, http.MethodPatch, "/api/v1/issues/INFRA-4",
		map[string]any{"title": "The reviewer's child"}, "alice")
	if patched.Code != http.StatusOK {
		t.Fatalf("retitle the card's issue: status=%d body=%s", patched.Code, patched.Body.String())
	}
	held = requireLandsOnGET(t, rig.handler, legionBoardView, held, conn.eventDelta(t, rig.database, "b"), "")
	if !strings.Contains(string(held), "The reviewer's child") {
		t.Fatalf("the board a page holds after the deltas does not carry the new title: %s", held)
	}
}

// TestALegionBoardOfAProjectWithNoDaemonIsRefusedOnce: a view of a project that names no daemon is
// told LEGION_NOT_CONFIGURED once and stays open, as a delivery view with no settings is.
func TestALegionBoardOfAProjectWithNoDaemonIsRefusedOnce(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	seedLegionProjects(t, rig.handler, "INFRA")
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("INFRA")))
	conn.refusal(t, "b", "LEGION_NOT_CONFIGURED")
	rig.requireNothingSent(t, conn, "the refusal of a project with no daemon")
}

// TestALegionBoardOfAProjectThatDoesNotExistIsRefusedAsTheRouteRefusesIt: a view of a project
// that does not exist is told what GET /api/v1/projects/{key}/legion/board answers it,
// loadProject's PROJECT_NOT_FOUND with its words, once, rather than LEGION_NOT_CONFIGURED: what is
// missing is the project, not its daemon.
func TestALegionBoardOfAProjectThatDoesNotExistIsRefusedAsTheRouteRefusesIt(t *testing.T) {
	t.Parallel()
	rig := newHubRig(t, nil)
	routed := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/NOPE/legion/board", nil, "alice")
	answer := decodeBody[refusalAnswer](t, routed)
	if routed.Code != http.StatusNotFound || answer.Code != "PROJECT_NOT_FOUND" {
		t.Fatalf("GET the board of NOPE: status=%d body=%s, want 404 PROJECT_NOT_FOUND", routed.Code, routed.Body.String())
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("b", legionView("NOPE")))
	if refusal := conn.refusal(t, "b", answer.Code); refusal.Message != answer.Error {
		t.Errorf("the refusal's message = %q, want the board route's %q", refusal.Message, answer.Error)
	}
	rig.requireNothingSent(t, conn, "the refusal of a project that does not exist")
}

// TestALegionViewNamesItsProjectAndNothingElse: the kind's shape is held where every other kind's
// is, so a frame of another kind's shape is refused before anything is subscribed. A badly spelled
// project is refused as GET /api/v1/projects/{key}/legion/board refuses it, INVALID_PROJECT and its
// words alike, so the two doors to one board name one refusal.
func TestALegionViewNamesItsProjectAndNothingElse(t *testing.T) {
	t.Parallel()
	window := DeliveryLiveWindow{Preset: new(7)}
	project := "INFRA"
	for _, refused := range []struct {
		what string
		view DeliveryLiveView
	}{
		{"a legion view with no project", DeliveryLiveView{Kind: legionBoardView}},
		{"a legion view with a window", DeliveryLiveView{Kind: legionBoardView, Project: &project, Window: &window}},
		{"a timeline view with a project", DeliveryLiveView{Kind: deliveryLiveTimeline, Window: &window, Project: &project}},
	} {
		if _, refusal := parseDeliveryLiveView(refused.view); refusal == nil || refusal.Code != "INVALID_FRAME" {
			t.Errorf("%s: refusal = %+v, want INVALID_FRAME", refused.what, refusal)
		}
	}
	notAKey := "infra"
	routed := dispatchRequest(t, newTestHandler(t), http.MethodGet, "/api/v1/projects/"+notAKey+"/legion/board", nil, "alice")
	answer := decodeBody[refusalAnswer](t, routed)
	if routed.Code != http.StatusBadRequest || answer.Code != "INVALID_PROJECT" {
		t.Fatalf("GET the board of %q: status=%d body=%s, want 400 INVALID_PROJECT", notAKey, routed.Code, routed.Body.String())
	}
	_, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: legionBoardView, Project: &notAKey})
	if refusal == nil || refusal.Code != answer.Code || refusal.Message != answer.Error {
		t.Errorf("a legion view of %q: refusal = %+v, want the board route's %+v", notAKey, refusal, answer)
	}
	spec, refusal := parseDeliveryLiveView(legionView("INFRA"))
	if refusal != nil {
		t.Fatalf("a legion view of INFRA: %+v", refusal)
	}
	if spec.project != "INFRA" || spec.key != "legion|INFRA|" {
		t.Fatalf("the spec = %+v, want the project it names and a key of its own", spec)
	}
}

// TestADeliveryReadTakesItsFacetsRepeatedAndItsOtherKeysOnce: a delivery read takes each facet any
// number of times and every other key it reads once, so GET /api/v1/delivery/timeline and
// /api/v1/delivery/measures with the search named twice, and GET /api/v1/delivery/pipeline with
// its at named twice, are refused naming it, as one naming from or to twice is
// (TestADeliveryReadRefusesARepeatedWindowKey), and the timeline answers its facets repeated. A
// timeline or measures view is read as its GET route reads its query, so it refuses the search
// named twice too, and takes its facets repeated.
func TestADeliveryReadTakesItsFacetsRepeatedAndItsOtherKeysOnce(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedMeasuresSettings(t, database)
	facets := url.Values{"repo": {"acme/one", "acme/two"}, "priority": {"P0", "P1"}, "q": {"fix"}}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?"+facets.Encode(), nil, "alice"); response.Code != http.StatusOK {
		t.Errorf("GET the timeline of %s: status %d, body %.200s; want 200, its facets taken repeated", facets.Encode(), response.Code, response.Body.String())
	}
	for _, refused := range []struct {
		route, key string
		query      url.Values
	}{
		{"/api/v1/delivery/timeline", "q", url.Values{"q": {"fix", "revert"}}},
		{"/api/v1/delivery/measures", "q", url.Values{"q": {"fix", "revert"}}},
		{"/api/v1/delivery/pipeline", "at", url.Values{"at": {"2026-11-14T00:00:00Z", "2026-11-07T00:00:00Z"}}},
	} {
		target := refused.route + "?" + refused.query.Encode()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		var refusal evalRefusal
		if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &refusal) != nil || refusal.Code != "INVALID_QUERY" || !strings.Contains(refusal.Error, strconv.Quote(refused.key)) {
			t.Errorf("GET %s: status %d, body %.200s; want 400 INVALID_QUERY naming %s", target, response.Code, response.Body.String(), refused.key)
		}
	}
	window := DeliveryLiveWindow{Preset: new(7)}
	for _, kind := range []string{deliveryLiveTimeline, deliveryLiveMeasures} {
		twice := "q=fix&q=revert"
		if _, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: kind, Window: &window, Query: &twice}); refusal == nil || refusal.Code != "INVALID_QUERY" || !strings.Contains(refusal.Message, `"q"`) {
			t.Errorf("a %s view of %s: refusal = %+v, want INVALID_QUERY naming q", kind, twice, refusal)
		}
		repeated := facets.Encode()
		if _, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: kind, Window: &window, Query: &repeated}); refusal != nil {
			t.Errorf("a %s view of %s: refusal = %+v, want its facets taken repeated", kind, repeated, refusal)
		}
	}
}

// TestAPipelineViewWhoseShownDaysStartBeforeYearOneIsRefused: the live hub's pipeline view reads
// its at as GET /api/v1/delivery/pipeline reads it, so an at whose seven shown days start before
// the year 1 is refused INVALID_QUERY when it is subscribed, naming the first shown day and its
// year, as the route refuses it (TestAPipelineWhoseShownDaysStartBeforeYearOneIsRefused, R18),
// rather than held as a view whose every answer shows days of the year 0. The first at whose days
// all lie in the year 1 is held.
func TestAPipelineViewWhoseShownDaysStartBeforeYearOneIsRefused(t *testing.T) {
	t.Parallel()
	for _, refused := range []struct{ at, first string }{
		{"0001-01-01T12:00:00Z", "0000-12-26"},
		{"0001-01-06T23:59:59Z", "0000-12-31"},
	} {
		_, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: deliveryLivePipeline, At: &refused.at})
		if refusal == nil || refusal.Code != "INVALID_QUERY" || !strings.Contains(refusal.Message, refused.first) || !strings.Contains(refusal.Message, "year 0") {
			t.Errorf("a pipeline view at %s: refusal = %+v, want INVALID_QUERY naming its first shown day %s and the year 0", refused.at, refusal, refused.first)
		}
	}
	at := "0001-01-07T00:00:00Z"
	spec, refusal := parseDeliveryLiveView(DeliveryLiveView{Kind: deliveryLivePipeline, At: &at})
	if refusal != nil {
		t.Fatalf("a pipeline view at %s: refusal = %+v, want it held: its shown days are 0001-01-01 to 0001-01-07", at, refusal)
	}
	if want := time.Date(1, time.January, 7, 0, 0, 0, 0, time.UTC); spec.at == nil || !spec.at.Equal(want) {
		t.Errorf("the pipeline view at %s holds at %v, want %s", at, spec.at, want.Format(time.RFC3339))
	}
}
