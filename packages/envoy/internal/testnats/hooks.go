//go:build odatrace

// The hold points of the hooked nats-server. packages/envoy/scripts/forced-order.sh builds a
// test binary whose nats-server carries the hooks of testdata/hooked/v<version>: a call at each
// instrumented point inside the server, to natsserver.MeasureHook. Only that script's build
// compiles this file (-tags odatrace, against the patched copy of nats-server that declares
// MeasureHook); any other build with the tag fails to compile here, which is the loud answer to
// a tag given without the hooked server.
//
// A test installs the hook with Hook and arms a Hold at a point. The first goroutine of the
// server that reaches that point with an event every Match of the hold accepts is parked there,
// with whatever locks the server's own code holds at that point, until the test releases it. So
// a test forces an order the server's scheduling would only sometimes produce: a consumer's
// state write held while a consumer of the same name is deleted and created again, a JetStream
// API request held while its server shuts down, released before Stop returns, a committed entry
// held before one server stores it.
//
// The hook is the process's: one test of a binary hooks it at a time, and a test that hooks it
// must not run in parallel with one that does. A server's shutdown releases every hold that parks
// one of its goroutines, before it waits for its goroutines to end, and parks none of its
// goroutines again until that wait is over (ServerShutdown, ServerDown): an operation held across
// a Stop either lands or fails, and the Stop returns. Every hold is released, and the hook taken
// off, when the test that installed it ends, and each hold armed and never reached fails that
// test.
package testnats

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Point names an instrumented point of the hooked server: one of the hold points below, or a
// point the overlay reports without a constant here (store.loaded, write.done, delete.closed and
// the others testdata/hooked/v<version>/server.patch calls measure at).
type Point string

// The hold points: what the server is about to do when it reaches each one.
const (
	// WritePreIO is a consumer file store's state write (consumerFileStore.writeState) with the
	// state encoded and the store's lock released, before the file is opened.
	WritePreIO Point = "write.preIO"
	// WritePreRename is that write with the new state complete in o.dat.tmp, before the rename
	// puts it at o.dat. nats-server v2.10.29 writes o.dat in place and has no such point.
	WritePreRename Point = "write.preRename"
	// FlushEncoded is a consumer's flusher with the state it will write encoded, before the write.
	FlushEncoded Point = "flush.encoded"
	// StoreCreate is a consumer's file store being made (fileStore.ConsumerStore), before it
	// loads any state from disk.
	StoreCreate Point = "store.create"
	// APIWorker is a JetStream API request the server has taken and not yet served: popped off
	// the API queue by a worker, or, on v2.10.29, a request from the server's own client about to
	// be served in that client's read loop, which that server version never queues.
	APIWorker Point = "api.worker"
	// ClientPublish is a publish past its permission checks, before the server matches it
	// against any subscription.
	ClientPublish Point = "client.publish"
	// MetaApply is one consumer assignment the JetStream meta layer is about to apply on one
	// server (applyMetaEntries), before that server creates or updates the consumer. A lone
	// server has no meta layer and no such point.
	MetaApply Point = "meta.apply"
	// StreamApply is one committed entry of a clustered stream on one server, before that server
	// stores it (applyStreamMsgOp). A stream with no Raft group, a lone server's, has no such
	// point.
	StreamApply Point = "stream.apply"
	// DedupeReset is a stream update whose duplicate window changes, past the re-arm of the
	// stream's purge of remembered duplicate ids ("Let it fire right away", server/stream.go) and
	// before the store of the new window. The update re-arms that purge to fire at once, and only
	// while the stream remembers an id. A hold here parks the update, with the stream's own lock
	// held, which this server version's purge does not take: the purge the re-arm fired runs
	// during the park, reads the window being replaced, keeps an id the new window would drop and
	// re-arms itself for as long as that old window, the order the defect needs. Event.Window is
	// the old window and Event.NewWindow the one the update will store.
	DedupeReset Point = "dedupe.reset"
	// DedupePurged is a purge of remembered duplicate ids that has done its work, with the dedupe
	// lock still held: Event.Window is the window it measured the ids' ages against, which no
	// update can have changed meanwhile, and Event.Remembered the number it kept. A goroutine
	// held here holds that lock, so a publish of an id and an update's store of a new window wait
	// for the release.
	DedupePurged Point = "dedupe.purged"

	// ServerShutdown and ServerDown are no hold points: they are where a server's Shutdown or
	// lame duck begins, before it stops anything, and where its Shutdown has waited for every
	// goroutine of its own. Between the two the hook releases the server's held goroutines and
	// parks none of them again. A lame duck reports ServerShutdown before it shuts JetStream
	// down, which waits for each clustered stream's goroutines, and its Shutdown reports it a
	// second time. Each carries the ID of the server's run (Event.ServerID), and a
	// ServerShutdown of a run whose ServerDown the hook has seen changes nothing: v2.10.29 can
	// report one late, from a second concurrent Shutdown call, after a restart on the same store.
	ServerShutdown Point = "server.shutdown"
	ServerDown     Point = "server.down"
)

// missing are the points a hooked server version does not have, each with the reason.
var missing = map[string]map[Point]string{
	"2.10.29": {
		WritePreRename:               "it writes a consumer's o.dat in place, with no temporary file and no rename",
		Point("msgget.timeSelected"): "its message get takes no start_time, so no sequence is selected by time",
		DedupeReset:                  "its purge of remembered ids takes the stream's own lock, which its update holds across the re-arm and the store of the new window, so no purge runs between the two and the point is not hooked",
		DedupePurged:                 "its purge of remembered ids is not hooked: the order dedupe.reset forces is impossible on this version",
	},
}

// Exists reports whether the linked nats-server has the point, and why not when it has not.
func (p Point) Exists() (bool, string) {
	if why, ok := missing[natsserver.VERSION][p]; ok {
		return false, why
	}
	return true, ""
}

// Event is one report of the hooked server, as natsserver.MeasureEvent carries it, with its
// state decoded.
type Event struct {
	// N is the event's place among the events this Hooks saw, counting from 1.
	N     int
	Point Point
	// Gen is the consumer file store's generation, one per store made; 0 at a server point.
	Gen uint64
	// Consumer is the consumer's name at a file store point and at MetaApply.
	Consumer string
	// Store is the stream's store directory at a file store point, and the server's own store
	// directory at a server point.
	Store string
	// Path is the file or directory involved at a file store point; at APIWorker, "queued" or
	// "inline".
	Path string
	// State is the consumer state the event carries (the bytes being written), decoded, or nil.
	State *natsserver.ConsumerState
	Seq   uint64
	Err   string
	// Server, Account, Subject, Stream, Client and Kind describe a server point's request or
	// message: Account is the account that sent the request at APIWorker, the publishing
	// connection's at ClientPublish, and the stream's at StreamApply and at MetaApply; Stream is
	// the stream's name at StreamApply, at MetaApply, at msgget.timeSelected and at the
	// duplicate-window points, DedupeReset and DedupePurged; Client is the publishing
	// connection's name, Kind its kind. ServerID is the ID of the server's run at ServerShutdown
	// and ServerDown.
	Server, ServerID, Account, Subject, Stream, Client, Kind string
	// Recovering is set at StreamApply and at MetaApply while the server replays its log at start.
	Recovering bool
	// Window is the duplicate window the point read: at DedupeReset the one being replaced, which
	// a purge running there measures the remembered ids against, and at DedupePurged the one the
	// purge did measure them against. NewWindow is the window the update will store, at
	// DedupeReset alone, and Remembered the ids the purge kept, at DedupePurged alone.
	Window, NewWindow time.Duration
	Remembered        int
}

func (e Event) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s", e.N, e.Point)
	for _, field := range []struct{ name, value string }{
		{"consumer", e.Consumer}, {"server", e.Server}, {"account", e.Account}, {"stream", e.Stream},
		{"subject", e.Subject}, {"client", e.Client}, {"kind", e.Kind}, {"path", e.Path}, {"err", e.Err},
	} {
		if field.value != "" {
			fmt.Fprintf(&b, " %s=%s", field.name, field.value)
		}
	}
	if e.Gen != 0 {
		fmt.Fprintf(&b, " gen=%d", e.Gen)
	}
	if e.Seq != 0 {
		fmt.Fprintf(&b, " seq=%d", e.Seq)
	}
	if e.State != nil {
		fmt.Fprintf(&b, " delivered=%d/%d ackfloor=%d/%d pending=%d", e.State.Delivered.Consumer, e.State.Delivered.Stream,
			e.State.AckFloor.Consumer, e.State.AckFloor.Stream, len(e.State.Pending))
	}
	if e.Window != 0 || e.NewWindow != 0 {
		fmt.Fprintf(&b, " window=%s", e.Window)
		if e.NewWindow != 0 {
			fmt.Fprintf(&b, "->%s", e.NewWindow)
		}
	}
	if e.Point == DedupePurged {
		fmt.Fprintf(&b, " remembered=%d", e.Remembered)
	}
	if e.Recovering {
		b.WriteString(" recovering")
	}
	return b.String()
}

func eventOf(me *natsserver.MeasureEvent) Event {
	e := Event{
		Point: Point(me.Point), Gen: me.Gen, Consumer: me.Name, Store: me.Store, Path: me.Path, Seq: me.Seq,
		Server: me.Server, ServerID: me.ServerID, Account: me.Account, Subject: me.Subject, Stream: me.Stream, Client: me.Client,
		Kind: me.Kind, Recovering: me.Recovering, Window: me.Window, NewWindow: me.NewWindow, Remembered: me.Remembered,
	}
	if me.Err != nil {
		e.Err = me.Err.Error()
	}
	if len(me.State) > 0 {
		if state, err := natsserver.MeasureDecodeConsumerState(me.State); err == nil {
			e.State = state
		}
	}
	return e
}

// Match is one condition an event must meet for a hold or a search to take it.
type Match struct {
	what string
	ok   func(Event) bool
}

func (m Match) String() string { return m.what }

// Where is a Match of the test's own, described by what.
func Where(what string, ok func(Event) bool) Match { return Match{what, ok} }

// OnServer takes the events of server s alone: a file store point under its store, or a server
// point it reported. The store directory of a server is the same across its Stop and Start.
func OnServer(s *Server) Match {
	return Match{"on the server whose store is " + s.store, func(e Event) bool { return underStore(s.store, e.Store) }}
}

// underStore reports whether path is the store directory root or a directory under it. A server
// with no store directory reports its events under "", which every path is taken to be under.
func underStore(root, path string) bool {
	return root == "" || path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Consumer takes the events of the consumer named name.
func Consumer(name string) Match {
	return Match{"consumer " + name, func(e Event) bool { return e.Consumer == name }}
}

// Stream takes the events of the stream named name: a StreamApply of it, a MetaApply of one of its
// consumers, a message get from it (msgget.timeSelected), a duplicate-window point of it
// (DedupeReset, DedupePurged), or a file store point of one of its consumers.
func Stream(name string) Match {
	return Match{"stream " + name, func(e Event) bool {
		if e.Stream != "" {
			return e.Stream == name
		}
		return e.Consumer != "" && filepath.Base(e.Store) == name && filepath.Base(filepath.Dir(e.Store)) == "streams"
	}}
}

// Subject takes the events whose request or message subject is subject exactly.
func Subject(subject string) Match {
	return Match{"subject " + subject, func(e Event) bool { return e.Subject == subject }}
}

// InAccount takes the events of the account named name: a request it sent, a message one of its
// connections published, or an entry of one of its streams. A test on the shared server (URL)
// holds its own account's requests alone with it.
func InAccount(name string) Match {
	return Match{"account " + name, func(e Event) bool { return e.Account == name }}
}

// ClientNamed takes the publishes of the connection named name.
func ClientNamed(name string) Match {
	return Match{"client " + name, func(e Event) bool { return e.Client == name }}
}

// After takes the events reported after mark, a value Hooks.Mark returned.
func After(mark int) Match {
	return Match{fmt.Sprintf("after event #%d", mark), func(e Event) bool { return e.N > mark }}
}

func describe(point Point, match []Match) string {
	parts := []string{string(point)}
	for _, m := range match {
		parts = append(parts, m.what)
	}
	return strings.Join(parts, ", ")
}

func accepts(e Event, point Point, match []Match) bool {
	if e.Point != point {
		return false
	}
	for _, m := range match {
		if !m.ok(e) {
			return false
		}
	}
	return true
}

// installed is set while a test's hook is the process's.
var installed atomic.Bool

// Hooks is the hook a test installed: every event the hooked servers of this process report
// while it is installed, and the holds the test armed.
type Hooks struct {
	t       testing.TB
	mu      sync.Mutex
	events  []Event
	holds   []*Hold
	changed chan struct{}
	// stopping maps the ID of each server run between ServerShutdown and ServerDown to that
	// server's store directory, and down holds the IDs of the runs whose ServerDown was reported.
	// Keyed by run, two servers that report one store, such as two with none, each keep their
	// own mark.
	stopping map[string]string
	down     map[string]bool
}

// Hook installs the process's hook for t and returns it. Call it after starting the servers the
// test holds: the cleanups of a test run last-registered first, so the hook's own, which releases
// every hold, then runs before those servers stop. It fails t when another test's hook is
// installed.
func Hook(t testing.TB) *Hooks {
	t.Helper()
	if !installed.CompareAndSwap(false, true) {
		t.Fatal("another test's forced-order hook is installed: tests that hook the server cannot run in parallel")
	}
	h := &Hooks{t: t, changed: make(chan struct{}), stopping: map[string]string{}, down: map[string]bool{}}
	hook := h.report
	natsserver.MeasureHook.Store(&hook)
	t.Cleanup(func() {
		natsserver.MeasureHook.Store(nil)
		h.mu.Lock()
		holds := append([]*Hold(nil), h.holds...)
		h.mu.Unlock()
		for _, hold := range holds {
			hold.Release()
		}
		installed.Store(false)
		for _, hold := range holds {
			if !hold.wasReached() {
				t.Errorf("the hold at %s was armed and never reached", hold)
			}
		}
	})
	return h
}

// report is the hook itself: it records the event, and parks the calling goroutine when the
// event is the first to meet a live hold's matches. A hold released before it was reached parks
// nothing and takes no event from a live hold: the first event to meet its matches marks it
// reached and goes on to the next hold. A server's ServerShutdown releases the holds that park
// its goroutines, and none of its goroutines is parked until its ServerDown. A ServerShutdown of
// a run already down, reported late, changes nothing.
func (h *Hooks) report(me *natsserver.MeasureEvent) {
	e := eventOf(me)
	var released []*Hold
	h.mu.Lock()
	e.N = len(h.events) + 1
	h.events = append(h.events, e)
	switch {
	case e.Point == ServerShutdown && !h.down[e.ServerID]:
		h.stopping[e.ServerID] = e.Store
		for _, hold := range h.holds {
			if hold.fired && underStore(e.Store, hold.event.Store) {
				released = append(released, hold)
			}
		}
	case e.Point == ServerDown:
		h.down[e.ServerID] = true
		delete(h.stopping, e.ServerID)
	}
	var parked *Hold
	if !h.stoppingLocked(e.Store) {
		for _, hold := range h.holds {
			if hold.fired || !accepts(e, hold.point, hold.match) {
				continue
			}
			// Whether the hold was released before it was reached is read before reached is
			// closed: once it is, the test can wake and release the hold, which is a release of
			// a reached hold and must not move the event on to the next one.
			hold.fired, hold.event = true, e
			wasReleased := hold.Released()
			close(hold.reached)
			if f := hold.onReached.Load(); f != nil {
				(*f)()
			}
			if wasReleased {
				continue
			}
			parked = hold
			break
		}
	}
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
	for _, hold := range released {
		hold.Release()
	}
	if parked != nil {
		<-parked.release
	}
}

// stoppingLocked reports whether the event store belongs to a server whose shutdown is under way.
// h.mu is held.
func (h *Hooks) stoppingLocked(store string) bool {
	for _, root := range h.stopping {
		if underStore(root, store) {
			return true
		}
	}
	return false
}

// Mark is the number of events reported so far, for After.
func (h *Hooks) Mark() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

// Events are the events reported so far at point that every match accepts, oldest first.
func (h *Hooks) Events(point Point, match ...Match) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []Event
	for _, e := range h.events {
		if accepts(e, point, match) {
			found = append(found, e)
		}
	}
	return found
}

// Await returns the first event at point that every match accepts, waiting for one until the
// test's deadline (testDeadline, computed once, when Await begins), and fails t naming the point
// and the matches when none is reported by then, however many other events come meanwhile.
func (h *Hooks) Await(t testing.TB, point Point, match ...Match) Event {
	t.Helper()
	bound := boundOf(t)
	for {
		h.mu.Lock()
		changed := h.changed
		for _, e := range h.events {
			if accepts(e, point, match) {
				h.mu.Unlock()
				return e
			}
		}
		h.mu.Unlock()
		if bound.over() || !awaitSignal(bound, changed) {
			t.Fatalf("the hooked server reported no event at %s by %s, short of the test's deadline", describe(point, match), bound)
		}
	}
}

// Hold arms a hold at point: the first goroutine to reach it with an event every match accepts
// is parked there until Release. It fails the test when the linked nats-server has no such
// point. The hold is released when the test ends, and a hold never reached by then fails the
// test.
func (h *Hooks) Hold(point Point, match ...Match) *Hold {
	h.t.Helper()
	if ok, why := point.Exists(); !ok {
		h.t.Fatalf("nats-server %s has no hold point %s: %s", natsserver.VERSION, point, why)
	}
	hold := &Hold{point: point, match: match, reached: make(chan struct{}), release: make(chan struct{})}
	h.mu.Lock()
	h.holds = append(h.holds, hold)
	h.mu.Unlock()
	h.t.Cleanup(hold.Release)
	return hold
}

// Hold is one armed hold.
type Hold struct {
	point   Point
	match   []Match
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	// fired and event are written under the Hooks' lock when the hold is reached; event is read
	// after reached is closed.
	fired bool
	event Event
	// onReached is the test seam SetOnReached sets on this hold.
	onReached atomic.Pointer[func()]
}

// SetOnReached makes f run in the hook right after it closes hold's reached channel, before the
// hook decides whether the event parks on hold, with the Hooks' lock held: the harness's own tests
// use it to act at that instant, as a test released from Reached does, without a sleep. Set it
// before the event can come. f must not call into the Hooks.
func SetOnReached(hold *Hold, f func()) { hold.onReached.Store(&f) }

func (hold *Hold) String() string { return describe(hold.point, hold.match) }

// Reached waits for the hold to be reached, until the test's deadline (testDeadline), and returns
// the event that reached it. That event's goroutine is parked at the hold until Release, unless the
// hold had been released before it was reached, when the event went on. It fails t, naming the
// point and the matches, when none did.
func (hold *Hold) Reached(t testing.TB) Event {
	t.Helper()
	if bound := boundOf(t); !awaitSignal(bound, hold.reached) {
		t.Fatalf("the hold at %s was not reached by %s, short of the test's deadline", hold, bound)
	}
	return hold.event
}

func (hold *Hold) wasReached() bool {
	select {
	case <-hold.reached:
		return true
	default:
		return false
	}
}

// Release lets the parked goroutine go on, or, for a hold not yet reached, disarms it: the first
// event to meet its matches marks it reached and goes on, to park on a live hold armed after it
// if one matches. It may be called more than once. A shutdown of the held server calls it too.
func (hold *Hold) Release() { hold.once.Do(func() { close(hold.release) }) }

// Released reports whether the hold has been released: by Release, by the test's end, or by a
// shutdown of the server whose goroutine it parked.
func (hold *Hold) Released() bool {
	select {
	case <-hold.release:
		return true
	default:
		return false
	}
}
