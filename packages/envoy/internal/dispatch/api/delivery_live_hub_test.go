package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
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

// newClockedHubRig is newHubRig on a clock the test moves.
func newClockedHubRig(t *testing.T, clock *fakeHubClock) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, clock, nil)
}

// newMeasuredHubRig is newClockedHubRig with the hub reading through a pool of its own on the same
// database, which hubAcquires counts: the relay reads the event log on the shared pool, as the
// rest of the process does, and its reads - its first one among them, which its coalescing window
// places after the hub has its feed - are none of the hub's. So a test that measures what an
// answer costs the database counts the hub's own reads and waits for no other reader.
func newMeasuredHubRig(t *testing.T, clock *fakeHubClock) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOnStore(t, handler, database, deps, storetest.SecondProcess(t, database), clock, nil)
}

// fakeHubClock is the hub's clock in these tests: a clocktest.Fake that Advance moves, so a test
// moves the hub past a threshold or a midnight without waiting for it, whose timers also run on
// the wall clock. Each arm is placed on the fake and on a real timer of the same wait, and fires
// once, on whichever comes first, so a timer armed for a moment the fake has already reached fires
// at once, however far an Advance moved it before the arm. A clock from newNoonHubClock reads noon
// UTC now and runs with the wall clock from there; one from newHeldHubClock is the wall clock. A
// clock from newStillHubClock stands still between advances instead: Now is noon UTC plus the
// advances. Its timers still fire on the wall clock as well, but the hub's one timer only wakes its
// worker, which compares each view's due moment with Now (deliveryHub.work, runDue), so how long the
// test's own writes take decides nothing. A clock from newFakeOnlyHubClock stands still too, and
// arms the fake's timers alone. Now reads in UTC, as the hub's own clock does.
type fakeHubClock struct {
	*clocktest.Fake
	still bool
	// fakeOnly arms the fake's timers alone (newFakeOnlyHubClock).
	fakeOnly bool
}

type fakeHubTimer struct {
	f    func()
	fake clock.Timer
	real *time.Timer
	done atomic.Bool
}

// noonToday is noon UTC of the present's day. Every preset view is due at the next UTC midnight
// (nextExpiry) and sends a delta then, so a hub clock started at noon is 12 hours from the one
// delta no test that reads its view's next delta expects.
func noonToday() time.Time { return nextUTCMidnight(time.Now()).Add(-12 * time.Hour) }

// newNoonHubClock is a fakeHubClock that reads noon UTC now and runs with the wall clock from there.
func newNoonHubClock() *fakeHubClock {
	held := clocktest.Held()
	held.Set(noonToday())
	return &fakeHubClock{Fake: held}
}

// newHeldHubClock is a fakeHubClock on the wall clock, moved on by its advances.
func newHeldHubClock() *fakeHubClock { return &fakeHubClock{Fake: clocktest.Held()} }

// newStillHubClock is a fakeHubClock standing still at noon UTC until Advance moves it.
func newStillHubClock() *fakeHubClock {
	return &fakeHubClock{Fake: clocktest.New(noonToday()), still: true}
}

// newFakeOnlyHubClock is a fakeHubClock standing still at noon UTC whose timers fire only when the
// test moves it: no real timer stands beside the fake's, so nothing of the wall clock reaches the
// hub, and the fake's report of what is armed is the hub's whole timer.
func newFakeOnlyHubClock() *fakeHubClock {
	return &fakeHubClock{Fake: clocktest.New(noonToday()), still: true, fakeOnly: true}
}

func (c *fakeHubClock) Now() time.Time { return c.Fake.Now().UTC() }

// standsStill is whether the clock moves only by Advance, so a view's recompute, due on this
// clock, waits for the test rather than for the wall clock.
func (c *fakeHubClock) standsStill() bool { return c.still }

func (c *fakeHubClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	if c.fakeOnly {
		return c.Fake.AfterFunc(d, f)
	}
	timer := &fakeHubTimer{f: f}
	timer.fake = c.Fake.AfterFunc(d, timer.fire)
	timer.real = time.AfterFunc(d, timer.fire)
	return timer
}

func (t *fakeHubTimer) fire() {
	if t.done.CompareAndSwap(false, true) {
		t.f()
	}
}

// C is nil, as an AfterFunc timer's is.
func (t *fakeHubTimer) C() <-chan time.Time { return nil }

func (t *fakeHubTimer) Stop() bool {
	t.fake.Stop()
	t.real.Stop()
	return t.done.CompareAndSwap(false, true)
}

func (t *fakeHubTimer) Reset(d time.Duration) bool {
	t.fake.Stop()
	t.real.Stop()
	was := !t.done.Swap(false)
	t.fake.Reset(d)
	t.real.Reset(d)
	return was
}

// hubOn builds a hub and its relay on an existing test server, as a second Dispatch task's would
// be. Nothing runs until the first Subscribe starts a generation, which takes the relay's feed on
// a goroutine of its own; a test that must have the hub holding it, and acting on everything its
// setup wrote, waits with settle.
func hubOn(t *testing.T, handler http.Handler, database *store.Store, deps Deps, clock *fakeHubClock, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	return hubOnStore(t, handler, database, deps, deps.Store, clock, configure)
}

// hubOnStore is hubOn with the hub reading through hubStore. It is deps' own store but for a test
// that counts the hub's connections (newMeasuredHubRig), which gives it a pool of its own: the
// relay keeps deps' store either way, since it is the process's one reader of the event log.
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
	return r.hub.generation.changeSeq
}

// settle waits until the hub has acted on everything the test's setup wrote, so none of it counts
// against the measurement that follows: its feed at or past the event log's head, its change
// cursor at the delivery change log's newest entry (settings and pull requests seeded through
// delivery.PutSettings and its kin), and then nothing owed, which is what waitNothingOwed reads.
// The hub takes delivery changes independently of the safe horizon the relay reads events up to,
// and a transaction another package holds open on the shared test Postgres can keep that horizon
// below the setup's events for a while, so either catch-up can land after the test's snapshots.
func (r *hubRig) settle(t *testing.T) {
	t.Helper()
	latest, err := delivery.LatestChangeSeq(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the newest delivery change: %v", err)
	}
	r.waitForEventCursor(t, eventHead(t, r.database), "the log's head before the measurement")
	r.waitNothingOwed(t, latest)
}

// requireNothingSent waits until the hub owes nothing for everything committed so far (settle),
// and then reads conn, which must hold no frame. Every frame a recompute, an expiry or a verdict
// sends is queued on the conn in the hold of the hub's lock that clears what settle waits on, so
// a frame still there is one nobody should be sent rather than one yet to come. after names what
// the conn was read after.
func (r *hubRig) requireNothingSent(t *testing.T, conn *fakeLiveConn, after string) {
	t.Helper()
	r.settle(t)
	conn.empty(t, after)
}

// waitNothingOwed waits until the hub owes no work for anything committed up to latest: nothing
// the intake has taken in and not run yet (intakeOwes), its change cursor at latest, and no view
// it will recompute without the test moving its clock (viewOwes), the last two read in one hold
// of its lock. A change marks the views it touches inside the same critical section that moves
// the cursor (catchUpChanges), a recompute clears the mark inside its read, once it holds the
// position it answers for, and computing stays set until that recompute has ended, in the hold
// of the lock that sends its frames. So a hub that owes nothing here has no recompute left to
// run that would read what the test writes next and send it as a change its subs had already
// heard, and every frame it owed its subs is queued on them.
//
// The intake is read first and the views last, with a barrier between them: the relay's feed is
// applied on the intake's goroutine and marks every view dirty there (applyFeed), so a read of
// the views taken before the "events seeded" note can hold none of those marks and find a hub
// that owes nothing while the apply is still to come. deliveryHub.catchUpNow runs on that same
// goroutine and returns once it has, so everything the intake had taken in when intakeOwes read
// it - that apply among them - has run before the views are read.
func (r *hubRig) waitNothingOwed(t *testing.T, latest int64) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the hub to owe nothing for the delivery changes through %d", latest), func() (string, bool) {
		owed := r.owes(latest)
		return owed, owed == ""
	})
}

// owes names what the hub has still to do for everything committed up to latest, and is empty
// when it has nothing. A hub no sub has started a generation on owes nothing: it reads neither
// log and holds no view.
func (r *hubRig) owes(latest int64) string {
	intake := r.intakeOwes()
	r.hub.catchUpNow()
	now := r.clock.Now()
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	generation := r.hub.generation
	if generation == nil {
		return ""
	}
	var owed []string
	if intake != "" {
		owed = append(owed, intake)
	}
	if generation.changeSeq < latest {
		owed = append(owed, fmt.Sprintf("the changes through %d, its cursor at %d", latest, generation.changeSeq))
	}
	for key, view := range generation.views {
		if what := viewOwes(view, now, r.clock.standsStill()); what != "" {
			owed = append(owed, key+" ("+what+")")
		}
	}
	slices.Sort(owed)
	return strings.Join(owed, "; ")
}

// viewOwes names what the hub will do to the view without the test moving its clock, and is empty
// when it will do nothing: a recompute under way; a dirty view, whose recompute a clock running
// with the wall clock makes due of itself; and an answer whose expiry the clock has reached. A
// dirty view on a clock standing still, due only once the test advances it, is owed nothing here:
// such a test moves the clock itself and takes what the recompute then sends.
func viewOwes(view *deliveryView, now time.Time, stillClock bool) string {
	if view.computing {
		return "computing"
	}
	at, _, due := view.dueAt()
	switch {
	case !due:
		return ""
	case view.dirty && !stillClock:
		return "dirty"
	case at.After(now):
		return ""
	case view.dirty:
		return "dirty and due"
	}
	return "its answer's expiry reached"
}

// waitFreshnessHeard waits until the freshness the hub has heard matches, naming it as what. The
// hub applies a delivery_freshness notification and sends the frame a changed verdict needs in
// one hold of its lock (applyFreshness), so once it has heard one, the frames that notification
// sends are queued on every open conn.
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
	if generation == nil {
		return ""
	}
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

// onlyViewBody is the JSON body the hub's one view last answered, read under the hub's lock. It
// fails unless exactly one view is open with a body, so a test that reads it holds the hub to the
// single view it set up.
func (r *hubRig) onlyViewBody(t *testing.T) json.RawMessage {
	t.Helper()
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	if n := len(r.hub.generation.views); n != 1 {
		t.Fatalf("the hub holds %d views, want one", n)
	}
	for _, view := range r.hub.generation.views {
		if view.body == nil {
			t.Fatal("the hub's view holds no body")
		}
		return view.body.snapshot("", view.seq, view.changedAt).Body
	}
	return nil
}

// cursors is where the hub stands in both logs: its generation's delivery change cursor, and how
// far its events have come.
func (r *hubRig) cursors() (changeSeq, eventCursor int64) {
	return r.changeCursor(), r.eventsThrough()
}

// waitForEventCursor waits until the hub holds every event through want. The wait has no bound of
// its own, and needs none: an open write anywhere on the test cluster holds back the safe horizon
// the relay publishes up to.
func (r *hubRig) waitForEventCursor(t *testing.T, want int64, what string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the hub to hold the event log through %d (%s)", want, what), func() (int64, bool) {
		got := r.eventsThrough()
		return got, got >= want
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

// waitFor waits for a note since mark that matches, and returns it.
func (r *hubRig) waitFor(t *testing.T, since int, what string, match func(deliveryHubNote) bool) deliveryHubNote {
	t.Helper()
	return testwait.Until(t, what, func() (deliveryHubNote, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, note := range r.notes[since:] {
			if match(note) {
				return note, true
			}
		}
		return deliveryHubNote{}, false
	})
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

// fakeLiveConn is a socket as the hub sees it, recording every frame it is sent.
type fakeLiveConn struct {
	frames chan any
	full   atomic.Bool
}

func newFakeLiveConn() *fakeLiveConn { return &fakeLiveConn{frames: make(chan any, 256)} }

func (c *fakeLiveConn) Send(frame any) bool {
	if c.full.Load() {
		return false
	}
	select {
	case c.frames <- frame:
		return true
	default:
		return false
	}
}

// next is the next frame the conn is sent.
func (c *fakeLiveConn) next(t *testing.T, what string) any {
	t.Helper()
	return testwait.Receive(t, what, c.frames)
}

// empty fails when the conn holds a frame nobody should be sent. Its caller has waited for the
// fact that would have sent one - the note of the recompute that sends it, or the hub owing
// nothing (hubRig.requireNothingSent) - and every frame is queued before either, so a frame here
// is one too many rather than one still to come. after names what the conn was read after.
func (c *fakeLiveConn) empty(t *testing.T, after string) {
	t.Helper()
	select {
	case frame := <-c.frames:
		raw, _ := json.Marshal(frame)
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
	frame := c.next(t, "a delta of "+sub)
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
	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
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

// windowTarget is the GET route of kind for the window a body answered.
func windowTarget(t *testing.T, kind string, body json.RawMessage, query string) string {
	t.Helper()
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

// latestChange is the newest delivery change's seq.
func latestChange(t *testing.T, database *store.Store) int64 {
	t.Helper()
	seq, err := delivery.LatestChangeSeq(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the newest change: %v", err)
	}
	return seq
}

func TestAProductionJobFlipsTheWaitingPullRequestsItShips(t *testing.T) {
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
	// The hub acts on everything the setup wrote before the job finishes. The relay's feed
	// reaches the generation after the snapshot, on its own goroutine (seedEvents), and taking it
	// marks the view dirty (applyFeed). A recompute's population reads run side by side, each on
	// its own snapshot (readDeliveryRows), so a recompute for that mark whose reads straddled the
	// job's commit answered the pull requests and the run after it and the measures' deploys
	// before it, and the delta read below was that torn answer rather than the job's own (the
	// recompute the job's change then made sent the rest). Settled, the job's change is the one
	// mark left, and every read of its recompute comes after the commit.
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

func TestResumeFromBeforeTheKeptDayGetsASnapshot(t *testing.T) {
	rig := newHubRig(t, nil)
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
	// change; the verdict the hub knows follows all the same.
	kept := newFakeLiveConn()
	frame.Sub, frame.Resume = "u", &DeliveryLiveResume{Seq: snapshot.Seq, Revs: snapshot.Revs}
	rig.hub.Subscribe(kept, frame)
	kept.freshness(t, "the verdict for a resume with nothing to catch up")
	rig.requireNothingSent(t, kept, "a resume from the change the view's body reflects")
}

func TestAReconnectCatchesUpWhatTheListenerMissed(t *testing.T) {
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
	// A hub that heard the merge all the same would recompute the view and send its delta; a hub
	// that could not hear it owes nothing for it, and the two read alike in every state the hub
	// keeps, since its change cursor stands below the merge until a catch-up runs either way.
	// The reconnection below is what makes the hub hear the merge, so the later action whose
	// effect would prove the absence is the one that ends it.
	testwait.Quiet(t, "the page's frames while the listener is blind", conn.frames, viewMaxWait+time.Second,
		"no state of the hub tells a merge it could not hear from one it has yet to catch up, and the only later action whose effect would, the listener's reconnection, is what makes it hear the merge")
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
// no backend to end while no dial is under way. Only the first round must find one, and the rounds
// wait for a dial under way and for each exit through the shared wait, so a listener that keeps
// connecting fails with the test binary's own deadline rather than running to the package's test
// timeout.
func terminateListener(t *testing.T, database *store.Store, dials *listenerDials) {
	t.Helper()
	for round := 0; ; round++ {
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
			if settled {
				return
			}
			// A dial under way when the round read the dials can connect after the round's scan:
			// the next round reads what it dialed once it has returned.
			testwait.Eventually(t, "the listener's dial under way to return", func() bool {
				_, settled := dials.settled()
				return settled
			})
			continue
		}
		testwait.Eventually(t, fmt.Sprintf("the listener's terminated backends %v to leave pg_stat_activity", ended), func() bool {
			var left int
			if err := database.Pool.QueryRow(context.Background(),
				`select count(*) from pg_stat_activity where pid = any($1)`, ended).Scan(&left); err != nil {
				t.Fatalf("read whether the listener's backend has exited: %v", err)
			}
			return left == 0
		})
	}
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
	delta := conn.delta(t, "names")
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
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the parent's component change upserts %v, want the child's pull request", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// TestTheHubRecomputesAViewAgainAfterAnEventDuringItsRead: a view's first read finishes before an
// issue it shows is retitled, and the retitle's event reaches the hub while that recompute still
// holds no issue of the view's. The view is recomputed once more and its page gets the new title.
func TestTheHubRecomputesAViewAgainAfterAnEventDuringItsRead(t *testing.T) {
	reading := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "recompute read" && held.CompareAndSwap(false, true) {
				// Hold the view's first recompute between its read and its answer.
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
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	testwait.Receive(t, "the view's first recompute to finish its read", reading)

	start := rig.mark()
	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	var newest int64
	if err := rig.database.Pool.QueryRow(t.Context(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	rig.waitFor(t, start, "the retitle on the hub's feed", func(note deliveryHubNote) bool {
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
	testwait.Receive(t, "the view's first recompute to finish its read", reading)

	start := rig.mark()
	written := appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated",
		model.Actor{Kind: "session", ID: "session-not-shown"}, map[string]any{}))
	rig.waitFor(t, start, "the session's event on the hub's feed", func(note deliveryHubNote) bool {
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
			delta, ok := conn.next(t, "each view's delta after "+step.name).(DeliveryLiveDelta)
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
		rig.waitFor(t, start, "the "+kind+" view's recompute", func(note deliveryHubNote) bool {
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
	// asks for, which the relay publishes once the horizon has passed it. A transaction held
	// anywhere on the Postgres instance puts off either, and the wait has no bound of its own.
	delta := conn.delta(t, "t")
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
	rig.waitFor(t, start, "the newest event on the hub's feed", func(note deliveryHubNote) bool {
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
	// The hub applies what the relay publishes, in the order it publishes it, and the notes keep
	// that order: a relay that had published the committed higher id while the lower was open
	// would have reached the hub with it first, which the commit below cannot undo.
	applied := rig.eventsApplied(start)
	if at := slices.Index(applied, low.ID); at < 0 || at > slices.Index(applied, high.ID) {
		t.Fatalf("the hub applied the events %v, want the open id %d before the committed id %d", applied, low.ID, high.ID)
	}
	if !rig.isP0(first) || !rig.isP0(second) {
		t.Fatalf("after both commits: %s a P0 = %v, %s a P0 = %v; want both acted on", first, rig.isP0(first), second, rig.isP0(second))
	}
}

// A transaction that holds an id anywhere on the Postgres instance keeps every safe horizon of the
// event log from being proven, and an operator's psql session, a migration or another database's
// job can hold one for as long as it likes. A fresh hub still serves its pages meanwhile: the
// delivery change log has nothing to do with that horizon, and the relay subscription that waits
// on it is taken on a goroutine of the generation's own (seedEvents), so the hub is ready at once,
// answers a page its snapshot and carries a delivery change to it while the transaction is open,
// and the issue event committed meanwhile reaches the page once the horizon is proven. The
// transaction is held until the test releases it, so all that comes before release() came while
// it held the horizon.
func TestAFreshHubServesDeliveryChangesWhileATransactionHoldsTheEventHorizon(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	rig := hubOn(t, handler, database, deps, newNoonHubClock(), nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, rig.clock.Now().Add(-3*time.Hour), key))

	release := holdClusterTransactionID(t)
	// The page's subscribe starts the hub's generation.
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.mu.Lock()
	ready := rig.hub.generation.ready
	rig.hub.mu.Unlock()
	testwait.Receive(t, "the hub to be ready while a transaction holds the event horizon", ready)
	held := conn.snapshot(t, "t").Body

	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
	delta := conn.delta(t, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2"}) {
		t.Fatalf("the change's delta upserts %v, want acme/widgets#2", got)
	}
	held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")

	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	release()
	delta = conn.delta(t, "t")
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
// statement was the listener's LISTEN (pglisten.Listener). The idle ping a listener sends after
// pglisten.DefaultListenIdle does not count, since a connection that never listened sends it too; a
// caller waits within seconds of the dial, well inside that idle period.
func waitListening(t *testing.T, database *store.Store, what string, dials *listenerDials) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		pids := dials.pids()
		var count int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where pid = any($1) and query like 'listen %'`, pids,
		).Scan(&count); err != nil {
			t.Fatalf("count the backends of %s: %v", what, err)
		}
		return count > 0
	})
}

// waitReturned waits for every goroutine running counts to have returned, failing the test with
// what when the run's own deadline comes first (testwait.Eventually).
func waitReturned(t *testing.T, what string, running *sync.WaitGroup) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		running.Wait()
		close(returned)
	}()
	testwait.Eventually(t, what, func() bool {
		select {
		case <-returned:
			return true
		default:
			return false
		}
	})
}

// TestAHubWithNoViewReadsNothingAndHoldsNoListener: the hub runs only while a page holds a view.
// Before the first subscribe and after the last view's drop, commits to the event log and the
// change log cost the hub and its relay no read, and neither holds a LISTEN connection; while a
// view is open both do, and the subscribe after the drop starts a generation that follows both logs
// again.
//
// The test reads only the hub's and the relay's own listeners, through their dial (listenerDials).
// The relay's listener dials once its generation's first cursor has awaited the event horizon
// (firstRelayCursor), which a transaction open anywhere on the instance holds back, so the wait for
// it is the horizon's. After the drop the test waits for every goroutine of the hub and of the
// relay to return: a listener's Run returns only once it has closed its connection.
func TestAHubWithNoViewReadsNothingAndHoldsNoListener(t *testing.T) {
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
		// Neither the hub nor its relay runs a generation, so neither listens or reads: that is
		// the state the commits below must cost nothing from.
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
		testwait.Span(t, time.Second, "with no generation running there is no reader whose state or report could tell a read it did not make; the span is longer than the relay's coalescing window and the hub's quiet, either of which a reader that heard the commits would have waited out before its read")
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
	// The retitle is an event, which reaches the hub on the relay's feed.
	delta = second.delta(t, "t")
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
	delta := conn.delta(t, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled by the other task", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// acquires is how many connections the shared pool has handed out so far.
func acquires(database *store.Store) int64 { return database.Pool.Stat().AcquireCount() }

// waitConnected waits for the hub's listener to have connected and caught the change log up, and
// for the hub to hold the relay's feed. A test that measures what an answer costs the database
// takes its reading after both, on a rig whose hub reads through a pool of its own, which the
// relay's own reads never touch (newMeasuredHubRig).
func (r *hubRig) waitConnected(t *testing.T) {
	t.Helper()
	r.waitFor(t, 0, "the listener's connection", func(note deliveryHubNote) bool { return note.what == "connected" })
	r.waitFor(t, 0, "the hub's event feed", func(note deliveryHubNote) bool { return note.what == "events seeded" })
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
	// and its delta, if any, is applied to the page's copy here.
	held := snapshot.Body
	clock.Advance(2 * viewQuiet)
	rig.settle(t)
	for drained := false; !drained; {
		select {
		case frame := <-conn.frames:
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

// A view's window is judged in memory against rows PostgreSQL stores and compares at microsecond
// resolution, so the moment a row leaves the window must fall on a microsecond the database can
// hold (nextExpiry). A pull request merged on a microsecond leaves a preset window a microsecond
// past its time plus the window's length; the test drives a fake clock to exactly that expiry and
// reads the view the hub recomputes there. The hub's narrowed view then holds the same pull
// requests the GET route's SQL does over the window it reports. With the leaving moment a
// nanosecond past instead - a step a timestamptz rounds back onto the row - the hub drops the
// leaving pull request while the GET route, reading the same window at microseconds, keeps it, and
// the two disagree.
func TestAViewsWindowEdgeAgreesWithPostgresAtASubMicrosecondExpiry(t *testing.T) {
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	now := clock.Now()
	merged := now.Add(-28*24*time.Hour + 2*time.Second)
	upsertPR(t, rig.database, hubPR(1, merged))
	upsertPR(t, rig.database, hubPR(2, now.Add(-time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 28, "")))
	conn.snapshot(t, "p")
	rig.waitConnected(t)
	rig.settle(t)
	// Run the recompute a seeded change left due, so the only timer left is the pull request's
	// expiry, and drain its delta.
	clock.Advance(2 * viewQuiet)
	rig.settle(t)
	for drained := false; !drained; {
		select {
		case <-conn.frames:
		default:
			drained = true
		}
	}
	// The view's one timer is the pull request's leaving, a moment just past merged + 28 days and
	// well before the midnight expiry; the fake clock reports the exact deadline it is armed for,
	// which the test moves to without asserting its sub-second part, since the agreement below is
	// what the moment's resolution decides.
	leaves, midnight := merged.Add(28*24*time.Hour), nextUTCMidnight(clock.Now())
	armed := clock.AwaitArmed(t, "the pull request's expiry", 1)
	if len(armed) != 1 || !armed[0].After(leaves) || !armed[0].Before(midnight) {
		t.Fatalf("the view's timer is armed for %v, want the pull request's leaving between %v and %v", armed, leaves, midnight)
	}
	start := rig.mark()
	clock.Set(armed[0])
	rig.waitFor(t, start, "the expiry's recompute to end", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseExpiry
	})
	body := rig.onlyViewBody(t)
	requireSameBody(t, "the view narrowed at a sub-microsecond expiry", body,
		getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, body, "")))
}

func TestAPipelineViewMovesItsDaysAtMidnightWithoutReadingTheDatabase(t *testing.T) {
	clock := newHeldHubClock()
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

// TestAHubClockMovedBetweenTheWorkersReadAndItsArmStillFiresItsTimer holds the hub's one timer to
// the moment the view's answer next expires (nextUTCMidnight), on a clock only the test moves: once
// the view holds its answer with nothing dirty or computing, the worker's timer is armed for the
// next UTC midnight, which the clock's report of what is armed shows (AwaitArmed); the test moves
// the clock to that very moment, and the timer fires, the expiry's recompute ends and its delta
// reaches the page. The row once moved the clock inside the worker's arm through a hook it said ran
// between the worker's read of the clock and the timer's placing, but that hook ran once the timer
// was placed (the ClockWorker note of INFRA-3149), so what the row can hold is the moment the timer
// is armed for, and that it fires when the clock reaches it - no later.
func TestAHubClockMovedBetweenTheWorkersReadAndItsArmStillFiresItsTimer(t *testing.T) {
	clock := newFakeOnlyHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.snapshot(t, "p")
	rig.waitConnected(t)
	// The relay's feed, seeded after the subscribe, marks every view dirty (applyFeed); on a clock
	// that stands still that recompute comes due only once the clock moves, so the test moves it a
	// quiet past the mark to run it and drains its delta. Without this the view can stay dirty for
	// good and the wait below, for a view holding its answer with nothing dirty, never ends.
	clock.Advance(viewQuiet)
	rig.settle(t)
	for drained := false; !drained; {
		select {
		case <-conn.frames:
		default:
			drained = true
		}
	}
	testwait.Eventually(t, "the view to hold its answer with nothing dirty, computing or waking", func() bool {
		rig.hub.mu.Lock()
		defer rig.hub.mu.Unlock()
		g := rig.hub.generation
		if g == nil {
			return false
		}
		ready := len(g.views) == 1 && len(g.wakeWork) == 0
		for _, view := range g.views {
			ready = ready && view.body != nil && !view.dirty && !view.computing && !view.expiry.IsZero()
		}
		return ready
	})
	expiry := nextUTCMidnight(clock.Now())
	armed := clock.AwaitArmed(t, fmt.Sprintf("the hub's timer for the view's expiry at %v", expiry), 1)
	if len(armed) != 1 || !armed[0].Equal(expiry) {
		t.Fatalf("the hub's timer is armed for %v, want the view's expiry at %v", armed, expiry)
	}
	start := rig.mark()
	clock.Set(expiry)
	rig.waitFor(t, start, "the expiry's recompute to end", func(note deliveryHubNote) bool {
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
	if note := rig.waitFor(t, start, "the recompute of the change", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	}); note.sent != 0 {
		t.Fatalf("the recompute whose window alone moved sent %d frames, want none", note.sent)
	}
	rig.requireNothingSent(t, conn, "a recompute whose window alone moved")
}

func TestFreshnessIsSentOnlyWhenItsVerdictChanges(t *testing.T) {
	clock := newHeldHubClock()
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
		if err := delivery.RecordReconcileSuccess(t.Context(), pool, at); err != nil {
			t.Fatalf("record a reconcile: %v", err)
		}
		rig.waitFreshnessHeard(t, "the hub to hear the reconcile that changed no verdict", reconciledAt(at))
		conn.empty(t, "a reconcile that changed no verdict")
	}
	if err := delivery.RecordReconcileError(t.Context(), pool, "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	failed := conn.freshness(t, "the freshness frame naming the error")
	if failed.Freshness.LastError == nil || *failed.Freshness.LastError != "GitHub answered 502" || len(failed.Stale) != 0 {
		t.Fatalf("the error's freshness frame = %+v, want last_error \"GitHub answered 502\" and nothing stale", failed)
	}
	cleared := clock.Now()
	if err := delivery.RecordReconcileSuccess(t.Context(), pool, cleared); err != nil {
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
	if err := delivery.RecordReconcileSuccess(t.Context(), pool, passed); err != nil {
		t.Fatalf("record the reconcile that clears the stale sources: %v", err)
	}
	again := conn.freshness(t, "the freshness frame clearing the stale sources")
	if len(again.Stale) != 0 || again.Freshness.LastError != nil {
		t.Fatalf("the freshness frame after the next pass = %+v, want nothing stale and no last_error", again)
	}
	rig.waitFreshnessHeard(t, "the hub to hear the pass that cleared the stale sources", reconciledAt(passed))
	conn.empty(t, "the frame clearing the stale sources")
}

func TestAnUnfetchablePullRequestSendsAFreshnessFrame(t *testing.T) {
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

	if err := delivery.RecordReconcileError(t.Context(), rig.database.Pool, "GitHub answered 502"); err != nil {
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
		get   string
	}{
		{sub: "timeline", view: timelineRange(from, to, "author=octocat"), kind: deliveryLiveTimeline, query: "author=octocat"},
		{sub: "measures", view: measuresRange(from, to, ""), kind: deliveryLiveMeasures},
		{sub: "pipeline", view: livePipelineView(&at), get: "/api/v1/delivery/pipeline?at=" + at.Format(time.RFC3339)},
		{sub: "preset", view: presetView(deliveryLiveTimeline, 7, ""), kind: deliveryLiveTimeline},
	} {
		rig.hub.Subscribe(conn, subscribeFrame(test.sub, test.view))
		snapshot := conn.snapshot(t, test.sub)
		if i > 0 {
			// The first view's read set the hub's verdict silently; every later view's snapshot is
			// followed by it.
			conn.freshness(t, "the verdict after the snapshot of "+test.sub)
		}
		target := test.get
		if target == "" {
			target = windowTarget(t, test.kind, snapshot.Body, test.query)
		}
		requireSameBody(t, test.sub, snapshot.Body, getBody(t, rig.handler, target))
		if snapshot.Seq == "" || len(snapshot.Revs.Fields) == 0 {
			t.Fatalf("%s snapshot seq = %q, revs = %+v; want its change position and every revision", test.sub, snapshot.Seq, snapshot.Revs)
		}
	}
}

func TestAMergedPullRequestReachesTheSubscriberAsOneDelta(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := rig.clock.Now()
	upsertPR(t, rig.database, hubPR(1, now.Add(-30*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, "p")

	upsertPR(t, rig.database, hubPR(2, rig.clock.Now().Add(-time.Hour)))
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
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, rig.clock.Now()); err != nil {
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
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, passed); err != nil {
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
	if note := rig.waitFor(t, start, "the recompute of the change", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	}); note.sent != 0 {
		t.Fatalf("the recompute after the tied rows were written again sent %d frames, want none", note.sent)
	}
	rig.requireNothingSent(t, conn, "a recompute after tied rows were written again unchanged")
}

func TestIdenticalViewsShareOneRecompute(t *testing.T) {
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
	var inFlight, most atomic.Int32
	var holding atomic.Bool
	begun := make(chan struct{}, 8)
	held := make(chan struct{})
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
				observe(note)
				if holding.Load() {
					// Hold the recompute open until the merge has left every other view due one
					// too, so a hub that ran two at once would have begun a second by then.
					begun <- struct{}{}
					<-held
				}
				return
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
	testwait.Receive(t, "the first of the merge's recomputes to begin", begun)
	testwait.Until(t, "the merge to leave every other view due a recompute", func() (int, bool) {
		rig.hub.mu.Lock()
		defer rig.hub.mu.Unlock()
		now := rig.clock.Now()
		due := 0
		for _, view := range rig.hub.generation.views {
			if at, _, ok := view.dueAt(); ok && view.dirty && !at.After(now) {
				due++
			}
		}
		return due, due == len(views)-1
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

func TestAChangeOutsideAWindowRecomputesNothing(t *testing.T) {
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
