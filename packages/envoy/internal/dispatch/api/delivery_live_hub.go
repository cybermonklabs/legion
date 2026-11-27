package api

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/workflowstore"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// The delivery hub's side of GET /ws/live (LEGION-690). The socket keeps no view state of its own.
// It hands each subscribe frame for a delivery view and each unsubscribe of one to the hub
// (Subscribe, Unsubscribe), calls Close however it ends, and writes every frame the hub sends it
// through its deliveryLiveConn.

// deliveryLiveConn is one socket as the hub sees it. Send queues one frame (a DeliveryLiveSnapshot,
// DeliveryLiveDelta, DeliveryLiveFreshness or LiveError) and never blocks: false means the
// socket's queue is full, and the socket then closes itself (1013), so the page reconnects and
// resumes.
type deliveryLiveConn interface {
	Send(frame any) bool
}

// deliverySubscription is a subscribe frame for a delivery view as the socket hands it to the hub,
// its resume decoded as a delivery view's, with the reader whose view it is: Resolve, when set,
// resolves that reader afresh (readerFor over the socket's caller), as the subscribe and again on
// each event that can move what the caller sees (recheckReaders); Reader is read only when Resolve
// is nil, for the subscription's whole life. Holds says which OAuth scopes the socket's credential
// holds, which the view's GET row may require (liveReadRefusal); nil holds none.
type deliverySubscription struct {
	Sub     string
	View    DeliveryLiveView
	Resume  *DeliveryLiveResume
	Reader  model.Actor
	Resolve func(ctx context.Context) (model.Actor, error)
	Holds   func(scope string) bool
}

// Subscribe answers one subscribe frame on conn, and reports whether conn's sub now follows the
// frame's view. The socket has refused a frame with no sub (subscribeLive). A view the matching GET
// route would refuse is an error frame naming the sub and carrying the code that route answers; a
// malformed view, or a resume seq that is not a decimal change position, is INVALID_FRAME; and a
// sub id conn already holds is then left as it was. Otherwise conn's sub follows the view from now
// on: it is answered a snapshot, or, on a resume, the delta against the revisions the page holds
// (a snapshot when the page applied nothing since before the oldest kept change,
// OldestChangeSeq() - 1), and later only the deltas.
// Right after that first answer, or where it would be for a resume with nothing to catch up, conn
// is sent the freshness verdict the hub knows: the hub sends its open pages a verdict only when it
// changes, and a page keeps the last one it was sent, so a socket that was down at a change, or a
// page that held no delivery view then, would otherwise show the old one for good. Nothing follows
// before the hub knows a verdict: the first is set silently, from the read that answers the first
// snapshot (recompute).
// While delivery is not configured the sub is told DELIVERY_NOT_CONFIGURED once and stays open,
// and the settings' first save answers its snapshot. A sub id conn already holds is replaced. The
// first sub of a hub with none starts a generation (deliveryHub); once the server's lifetime has
// ended no sub joins, and none starts a goroutine of the hub's (goLocked), and the frame is
// answered nothing, as the socket closes with 1012.
// A view is its reader's: the pull requests, unowned P0 issues and issue facts it reads are of the
// projects that reader sees (readDeliveryRows), so subscribers share a view only when they see
// alike (deliveryReaderKey).
func (h *deliveryHub) Subscribe(conn deliveryLiveConn, frame deliverySubscription) bool {
	spec, refusal := parseDeliveryLiveView(frame.View)
	if refusal != nil {
		refusal.Sub = frame.Sub
		conn.Send(*refusal)
		return false
	}
	h.mu.Lock()
	checks := h.readerChecks
	h.mu.Unlock()
	reader := frame.Reader
	if frame.Resolve != nil {
		// Read before the hub's lock, which no database read is made under.
		resolved, err := frame.Resolve(store.WithTransactionTracking(h.lifetime))
		if err != nil {
			conn.Send(liveRefusal(frame.Sub, codeInternal, "the subscription could not resolve what its caller sees"))
			return false
		}
		reader = resolved
	}
	// The view keeps its GET row's rule (liveViewReads): a caller the row refuses, a restricted
	// member or a credential acting for one reading a row of the deployment, is refused here with
	// the row's code, and holds no view.
	if refusal := liveReadRefusal(store.WithTransactionTracking(h.lifetime), frame.Sub, spec.kind, frame.Holds,
		func(context.Context) (model.Actor, error) { return reader, nil }); refusal != nil {
		conn.Send(*refusal)
		return false
	}
	spec.readFor(reader)
	var held *DeliveryLiveRevs
	if frame.Resume != nil {
		seq, err := strconv.ParseInt(frame.Resume.Seq, 10, 64)
		if err != nil || seq < 0 {
			conn.Send(liveRefusal(frame.Sub, "INVALID_FRAME", "resume.seq must be a decimal change position"))
			return false
		}
		// Read before the hub's lock, which no database read is made under.
		oldest, err := delivery.OldestChangeSeq(store.WithTransactionTracking(h.lifetime), h.server.deps.Store.Pool)
		switch {
		case err != nil:
			slog.Warn("dispatch: delivery live resume answers a snapshot: read the oldest kept change", "error", err)
		case oldest > 0 && seq < oldest-1:
			// The page applied nothing since before the oldest kept change: it reads its window once.
		default:
			held = &frame.Resume.Revs
		}
	}

	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.lifetime.Err() != nil {
		return false
	}
	g := h.generation
	if g == nil {
		g = h.startLocked()
	}
	subs := g.conns[conn]
	if subs == nil {
		subs = map[string]*deliverySub{}
		g.conns[conn] = subs
	}
	if replaced := subs[frame.Sub]; replaced != nil {
		// conn keeps a sub, so the generation runs on.
		g.leaveLocked(replaced)
	}
	sub := h.joinLocked(g, conn, frame.Sub, spec, held, frame.Resolve, frame.Holds, now)
	if checks != h.readerChecks && frame.Resolve != nil {
		// A re-check of every sub's reader began after this subscribe resolved its reader, and may
		// have walked the subs before this one was among them. Every other sub it walked it read,
		// so this one alone is read again, after it: a re-check reads the database, which no hub
		// lock is held across.
		h.goLocked(func() {
			h.recheckMu.Lock()
			defer h.recheckMu.Unlock()
			h.recheckSubs(g, []*deliverySub{sub})
		})
	}
	return true
}

// joinLocked makes id conn's sub of the view spec names, creating the view if no sub holds it, and
// answers it as a new subscription is answered: at once from a body the view holds, else by the
// recompute under way or one it is marked for. It answers the sub.
func (h *deliveryHub) joinLocked(g *hubGeneration, conn deliveryLiveConn, id string, spec deliveryLiveSpec, held *DeliveryLiveRevs,
	resolve func(context.Context) (model.Actor, error), holds func(string) bool, now time.Time) *deliverySub {
	view := g.views[spec.key]
	if view == nil {
		view = &deliveryView{spec: spec, subs: map[*deliverySub]struct{}{}}
		g.views[spec.key] = view
	}
	sub := &deliverySub{id: id, conn: conn, view: view, needsSnapshot: held == nil, needsVerdict: true, resolve: resolve, holds: holds}
	if held != nil {
		sub.revs = *held
	}
	view.subs[sub] = struct{}{}
	g.conns[conn][id] = sub
	switch {
	case view.body != nil && !view.dirty && !view.computing:
		h.answerLocked(g, sub)
	case view.computing || view.dirty:
		// The recompute under way, or the one the view already waits for, answers it.
	default:
		h.markDirtyLocked(g, view, now, deliveryCauseSubscribe)
	}
	return sub
}

// recheckReaders resolves the reader of every sub that resolves one (deliverySub.resolve) again
// (recheckSubs).
func (h *deliveryHub) recheckReaders(g *hubGeneration) {
	h.mu.Lock()
	var subs []*deliverySub
	for _, held := range g.conns {
		for _, sub := range held {
			if sub.resolve != nil {
				subs = append(subs, sub)
			}
		}
	}
	h.mu.Unlock()
	h.recheckSubs(g, subs)
}

// recheckSubs resolves the reader of each of subs again, and moves each sub whose reader now reads
// other rows (deliveryReaderKey) to the view of its new reader, which answers it with a snapshot:
// the page drops the rows of a project its caller no longer sees and reads one it now sees. A sub
// whose new reader its view's GET row refuses (liveReadRefusal) is dropped and sent that refusal
// instead. A sub replaced or dropped meanwhile is left as it is. A sub whose reader does not
// resolve is dropped and sent an ended frame (dropped), on which its page subscribes again and is
// answered as a subscribe is: the re-check cannot show the caller still reads the view's rows, and
// keeping the old reader would go on sending them rows of a project it may have lost.
//
// The key of a reader that sees every object (seesEveryObject) names none of the projects it sees,
// so a new project moves no such sub: its view is read again, for the reader that sees the project
// now, which the statements that bind the viewer read (the unowned P0 list, the unfetchable count,
// a Legion board's project), and its pages are sent what changed, as before a project could be
// private (R9).
func (h *deliveryHub) recheckSubs(g *hubGeneration, subs []*deliverySub) {
	for _, sub := range subs {
		reader, err := sub.resolve(g.readContext())
		if err != nil {
			if g.ctx.Err() != nil {
				continue
			}
			slog.Warn("dispatch: delivery live hub could not resolve a subscriber's reader again; its view ends and its page subscribes again", "error", err)
			h.mu.Lock()
			if h.generation == g && g.conns[sub.conn][sub.id] == sub {
				h.dropLocked(sub.conn, sub.id)
				sub.conn.Send(liveEnded(sub.id, liveEndedDropped))
			}
			h.mu.Unlock()
			continue
		}
		refusal := liveReadRefusal(g.readContext(), sub.id, sub.view.spec.kind, sub.holds,
			func(context.Context) (model.Actor, error) { return reader, nil })
		viewer := deliveryReaderKey(reader)
		h.mu.Lock()
		switch {
		case h.generation != g || g.conns[sub.conn][sub.id] != sub:
		case refusal != nil:
			h.dropLocked(sub.conn, sub.id)
			sub.conn.Send(*refusal)
		case sub.view.spec.viewer != viewer:
			spec := sub.view.spec
			spec.readFor(reader)
			g.leaveLocked(sub)
			h.joinLocked(g, sub.conn, sub.id, spec, nil, sub.resolve, sub.holds, h.clock.Now())
		case seesEveryObject(reader) && !sameSet(sub.view.spec.reader.Viewer.Sees, reader.Viewer.Sees):
			sub.view.spec.reader = reader
			h.markDirtyLocked(g, sub.view, h.clock.Now(), deliveryCauseChange)
		}
		h.mu.Unlock()
	}
}

// Unsubscribe drops conn's sub; a view's last sub drops the view, and the hub's last its generation.
func (h *deliveryHub) Unsubscribe(conn deliveryLiveConn, sub string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(conn, sub)
}

// Close drops every sub conn holds. The socket calls it however it ends: shutdown (1012), its
// caller no longer resolving (1008), a full queue (1013), a read error.
func (h *deliveryHub) Close(conn deliveryLiveConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.generation == nil {
		return
	}
	for id := range h.generation.conns[conn] {
		h.dropLocked(conn, id)
	}
}

// Holds reports whether conn holds sub, a delivery view's subscription. The hub drops one on its
// own when a re-check finds that the view's GET row refuses the sub's new reader (recheckReaders),
// and the socket asks here before it counts its subscriptions against the cap (subscribeLive).
func (h *deliveryHub) Holds(conn deliveryLiveConn, sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.generation != nil && h.generation.conns[conn][sub] != nil
}

// dropLocked drops conn's sub id, the view it leaves without a sub, and conn once it holds none. The
// drop that leaves the hub no conn stops its generation: its listener closes its connection, its
// intake cancels its relay subscription, and nothing reads either log until the next Subscribe.
func (h *deliveryHub) dropLocked(conn deliveryLiveConn, id string) {
	g := h.generation
	if g == nil {
		return
	}
	subs := g.conns[conn]
	sub := subs[id]
	if sub == nil {
		return
	}
	delete(subs, id)
	if len(subs) == 0 {
		delete(g.conns, conn)
	}
	g.leaveLocked(sub)
	if len(g.conns) == 0 {
		g.stop()
		h.generation = nil
	}
}

// leaveLocked takes sub out of its view, and drops the view it leaves without a sub, and with the
// last view of its reader what g holds of that reader (forgetReaderLocked).
func (g *hubGeneration) leaveLocked(sub *deliverySub) {
	delete(sub.view.subs, sub)
	if len(sub.view.subs) == 0 && g.views[sub.view.spec.key] == sub.view {
		delete(g.views, sub.view.spec.key)
		g.forgetReaderLocked(sub.view.spec.viewer)
	}
}

// forgetReaderLocked drops the unfetchable count, whether the reader reads the deployment, and the
// verdict last sent of the reader whose part of a view key is viewer once no view of g is that
// reader's, so a generation keeps nothing of a reader whose pages have all gone, however many come
// and go while it runs. A reader that comes back is one g never saw: its first read sets its
// verdict silently again (recompute).
func (g *hubGeneration) forgetReaderLocked(viewer string) {
	for _, view := range g.views {
		if view.spec.viewer == viewer {
			return
		}
	}
	delete(g.unfetchable, viewer)
	delete(g.readsDeployment, viewer)
	delete(g.sent, viewer)
}

// The channels the delivery hub listens on, all on its listener's one connection (startLocked).
// delivery_changes carries no payload and is notified as a delivery_changes row commits (migration
// 0100); delivery_freshness carries the settings row's freshness columns as JSON (0100);
// legion.StateChannel carries a project key and is notified after each read of that project's
// Legion daemon and after each write of its Legion setting (settings_legion.go), which with
// Dispatch's own events is all that moves a Legion board; and workflowstore.RunsChannel carries
// an issue key and is notified as a write of one of that issue's runs commits, which with the
// project's notifies on legion.StateChannel is all that moves the issue's legion_issue view.
const (
	deliveryChangesChannel   = "delivery_changes"
	deliveryFreshnessChannel = "delivery_freshness"
)

// deliveryHubFeedBuffer is the hub's slots on the relay's feed: more than one relay page
// (maxEventReplay), which the relay publishes at once, so a page never overflows an intake that
// was busy when it came.
const deliveryHubFeedBuffer = 1024

// deliveryHub keeps every open Delivery and Pipeline page, and every open view of the Legion state
// of a project or an issue, current (LEGION-690). It learns of a delivery change from Postgres
// (LISTEN delivery_changes and delivery_freshness, on one connection of its own,
// pglisten.Listener), of a read of a Legion daemon and of a write of a run on the same connection
// (legion.StateChannel, workflowstore.RunsChannel), and of a committed event from the event stream
// relay's feed, the live paths' one reader of the event log; marks dirty only the views the change
// can touch; recomputes each dirty view once per burst, one recompute at a time, as the GET
// routes' population read and pure compute; re-runs the compute alone, reading nothing, when
// something leaves a preset window; and sends each subscriber only the keys whose revision
// differs from what it holds.
//
// Each run of the hub is a generation (hubGeneration): the first Subscribe of a hub with no sub
// starts one, and the drop of its last sub stops it, at once, with no grace period, so a process
// with no open page holds no connection for the hub and reads neither log for it. A generation
// runs three goroutines on a context of its own: the listener; the intake, which alone moves the
// change log's cursor, applies the relay's events and marks views dirty; and the worker, which
// alone recomputes, so one recompute runs at a time. Its relay subscription, which waits for a
// safe horizon, is taken on a fourth that ends once it has (seedEventsLocked), at the start and
// after a feed closes. A generation owns its views and conns, and the drop of its last conn is
// what stops it, so a goroutine of a stopped generation reaches only its own empty maps, never the
// next generation's, and returns once its context ends.
type deliveryHub struct {
	server   *server
	clock    hubClock
	lifetime context.Context
	// dial, listenIdle, pingTimeout and backoff are the listener's (pglisten.Listener); dial is the
	// shared pool's Listener, read from the server when the listener dials, and a test sets them
	// otherwise before the first Subscribe.
	dial                    func(ctx context.Context) (*pgx.Conn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// observe, when set, is told of each recompute, connection and event; tests set it.
	observe func(deliveryHubNote)

	// running counts every generation's goroutines. Add runs under mu and only while closed is
	// false, so no Add can follow the Wait in wait.
	running sync.WaitGroup

	mu sync.Mutex
	// closed is set by wait: the lifetime has ended and no generation starts again.
	closed bool
	// generation is the running generation, nil while no conn holds a sub.
	generation *hubGeneration
	// readerChecks counts the re-checks of every sub's reader begun (recheckStaleReaders), under mu;
	// recheckMu runs one re-check at a time, a subscribe's own of its sub among them (Subscribe), so
	// the last to run reads the freshest readers.
	readerChecks uint64
	recheckMu    sync.Mutex
}

// hubGeneration is one run of the hub. ctx, stop and the channels are set once at its start; the
// rest is guarded by the hub's mu. views and conns are emptied by the drops that stop it, and no
// sub joins it once stopped.
type hubGeneration struct {
	ctx  context.Context
	stop context.CancelFunc
	// ready closes once the intake has read where the change log stands, which the worker waits
	// for. The relay subscription is taken apart (seedEventsLocked), so neither waits on the horizon.
	ready chan struct{}
	// Each wake holds at most one pending signal: what it asks for covers every signal before it.
	wakeWork, wakeChanges, connected chan struct{}
	barrier                          chan chan struct{}
	// readersStale is set when something may have moved what a subscriber sees: an access change
	// announced on store.AccessChangedChannel (an access write, or a person's member kind changed), a
	// relayed event that can move it (movesSight, receive), the listener's (re)connection, which may
	// have missed an announcement, and the socket's heartbeat, past which an access change no event
	// or announcement carries goes unread no longer (work). The worker resolves every sub's reader
	// again before its next recompute (recheckStaleReaders), once for however many came since its
	// last pass.
	readersStale atomic.Bool
	// feeds hands the intake the relay subscription seedEventsLocked took. It has no slot: a
	// hand-over completes only as the intake takes it, so a subscription is always either the
	// intake's, which it cancels as it returns, or still the subscribing goroutine's, which cancels
	// it once g has ended. A slot could hold one the intake never took past g's end, which neither
	// would cancel, and the relay would keep its generation, its LISTEN connection and its reads of
	// the event log for it.
	feeds chan hubEventFeed

	views map[string]*deliveryView // by canonical view key
	conns map[deliveryLiveConn]map[string]*deliverySub

	// changeSeq and changeAt are the last delivery change read and its at, to tell a reset log;
	// epoch counts the resets, so a recompute that read across one stores no change position.
	changeSeq int64
	changeAt  time.Time
	epoch     int64
	// p0 is the open P0 issues (OpenP0Keys), kept current from issue event payloads.
	p0 map[string]struct{}
	// fresh is the freshness row as last heard; known is false until the first recompute sets it,
	// silently. unfetchable is the unfetchable count each reader's views last read, readsDeployment
	// whether that reader reads the deployment's rows (readsTheDeployment), which decides whether
	// it is told the row's last_error, and sent the verdict last sent to that reader's pages, each
	// by the reader's part of a view key (deliveryLiveSpec.viewer): a reader is counted only the
	// pull requests it sees, so its verdict is its own, set silently by its first read.
	fresh           deliveryFreshnessHeard
	unfetchable     map[string]int
	readsDeployment map[string]bool
	sent            map[string]deliveryFreshnessVerdict
	known           bool
}

// hubClock is the hub's clock: what time it is, and a timer that calls f once the clock reaches
// at, at once when it already has. The timer names a moment rather than a wait, so a clock that
// moves between the hub's read of it and the arm (a test's, which moves forward without waiting)
// fires the timer all the same.
type hubClock interface {
	Now() time.Time
	At(at time.Time, f func()) hubTimer
}

type hubTimer interface{ Stop() bool }

// deliveryHubClock is the real clock.
type deliveryHubClock struct{}

func (deliveryHubClock) Now() time.Time { return time.Now().UTC() }

func (deliveryHubClock) At(at time.Time, f func()) hubTimer {
	return time.AfterFunc(time.Until(at), f)
}

// deliveryView is one view the open pages show, shared by every sub that names it.
type deliveryView struct {
	spec deliveryLiveSpec
	subs map[*deliverySub]struct{}
	// dirty is set when a change can have moved the view's answer; immediate when a sub waits for
	// its first answer, which no quiet delays. firstDirty and lastDirty time the coalescing, and
	// notBefore holds a failed read's retry back. A recompute clears dirty and immediate once its
	// read holds its change position (deliveryHub.read), not when it begins: whatever marked the
	// view until then was committed before the reads that follow, which answer it.
	dirty, immediate      bool
	firstDirty, lastDirty time.Time
	notBefore             time.Time
	cause                 string
	computing             bool
	// reading is set once recompute's read holds its change position and takes the view's marks
	// over, and until it installs that read's answer. heardIssues and heardSessions record what
	// the events applied meanwhile acted on (applyEventLocked): each issue with whether it was a
	// P0 before or after one of them, and each writing session with its newest title. The read
	// may have passed a row before its event committed, so recompute checks them against the
	// answer it installs (recheckHeardLocked).
	reading       bool
	heardIssues   map[string]bool
	heardSessions map[string]string
	// settings, pop and pipe are as of the last read, and readFrom the window start pop was read
	// for: an expiry narrows them without a read.
	settings delivery.DeliverySettings
	pop      deliveryPopulation
	pipe     pipelinePopulation
	readFrom time.Time
	// body is the last answer, nil until the first and while delivery is not configured; seq the
	// change position it reflects, and readingSeq the position a read under way reflects, so a
	// change at or before either marks nothing; and shownSeq and shownAt the position its frames
	// name to its reader and when that change was recorded (deliveryShownChange).
	body       *deliveryLiveEncoded
	seq        int64
	readingSeq int64
	shownSeq   int64
	shownAt    *time.Time
	// from and at are the window start and the pipeline clock the last answer resolved; expiry is
	// the next moment something leaves it, zero for a custom range or a pinned at.
	from, at time.Time
	expiry   time.Time
	// issues are the issue keys the population names, ancestors theirs, and sessions each
	// session the population names with the title it shows.
	issues, ancestors map[string]struct{}
	sessions          map[string]string
}

// deliverySub is one page's subscription to a view.
type deliverySub struct {
	id   string
	conn deliveryLiveConn
	view *deliveryView
	// revs is what the page holds; needsSnapshot is set until its first answer; needsVerdict is set
	// until its first answer too, which the verdict the hub knows follows; told is set once it was
	// told delivery is not configured, so it is told once.
	revs          DeliveryLiveRevs
	needsSnapshot bool
	needsVerdict  bool
	told          bool
	// resolve resolves the sub's reader again (deliverySubscription.Resolve); nil keeps its view's.
	// holds is the socket's scopes (deliverySubscription.Holds), which the re-check reads again.
	resolve func(context.Context) (model.Actor, error)
	holds   func(string) bool
}

// deliveryLiveSpec is a subscribed view, resolved: its kind, its window (a preset's length in days,
// or a custom range), its query without from and to, a pipeline view's pinned at, a Legion view's
// project, a legion_issue view's issue, and its shape, the key of the view whoever reads it; then
// the reader it is read for, that reader's part of the key (deliveryReaderKey), and the canonical
// key identical views share, the shape and that part (readFor).
type deliveryLiveSpec struct {
	kind     string
	preset   int
	from, to time.Time
	query    url.Values
	at       *time.Time
	// project is the project whose daemon a Legion view (viewReadsLegion) reads: the one a legion
	// view's board shows, and the one a legion_issue view's key names (parseLegionIssueLiveView).
	// A delivery view names none.
	project string
	// issue is the issue a legion_issue view shows (DeliveryLiveView.Issue), which a write of one
	// of its runs marks (dirtyLegionIssue); every other kind names none.
	issue  string
	shape  string
	reader model.Actor
	viewer string
	key    string
}

// deliveryFreshnessHeard is the freshness row as the hub last heard it.
type deliveryFreshnessHeard struct {
	lastEventAt, lastReconcileAt *time.Time
	lastError                    *string
}

// deliveryFreshnessVerdict is what a freshness frame is sent for: the last error, the sources past
// their threshold and the unfetchable count.
type deliveryFreshnessVerdict struct {
	lastError   *string
	stale       []string
	unfetchable int
}

// deliveryHubNote is one thing the hub did, for a test's observe.
type deliveryHubNote struct {
	what  string // "recompute begin", "recompute position", "recompute read", "recompute end", "connected", "feed ready", "events seeded", "event", "feed closed"
	view  string // the view's key
	cause string
	sent  int
	// through is the newest change position a recompute's read took, which a "recompute position"
	// note makes as the read takes it and a "recompute read" note carries on.
	through int64
	// eventID is the event an "event" note applied, or the head a "feed ready" or "events seeded"
	// note's feed began at.
	eventID int64
}

const (
	// A dirty view is recomputed once no change has marked it for viewQuiet, or viewMaxWait after
	// the first did, whichever comes first.
	viewQuiet   = 250 * time.Millisecond
	viewMaxWait = 2 * time.Second
	// deliveryLiveRetry is how long a view whose read failed waits before the next.
	deliveryLiveRetry = 5 * time.Second
	// deliveryLivePage is how many changes one catch-up read takes.
	deliveryLivePage = 1000
	// deliveryStaleAfter is how long the reconciled sources go without a pass before the verdict
	// names them stale: freshness.ts's three missed five-minute intervals.
	deliveryStaleAfter = 3 * delivery.ReconcileInterval

	deliveryCauseSubscribe = "subscribe"
	deliveryCauseChange    = "change"
	deliveryCauseExpiry    = "expiry"
)

// deliveryLivePresets are the preset windows a page offers, in days.
var deliveryLivePresets = []int{7, 14, 28, 84}

// deliveryReconciledSources are the sources a missed reconcile leaves stale; the events, which
// arrive irregularly, never are.
var deliveryReconciledSources = []string{"prs", "runs", "ci"}

// newDeliveryHub is the hub of s's open pages, on s's event stream relay. Nothing runs until the
// first Subscribe, which starts a generation on lifetime.
func newDeliveryHub(s *server, lifetime context.Context, clock hubClock) *deliveryHub {
	return &deliveryHub{
		server: s, clock: clock, lifetime: lifetime,
		dial: func(ctx context.Context) (*pgx.Conn, error) {
			return s.deps.Store.Pool.Listener(ctx)
		},
	}
}

// startLocked runs a new generation's listener, intake, worker and first relay subscribe, with mu
// held.
func (h *deliveryHub) startLocked() *hubGeneration {
	ctx, stop := context.WithCancel(h.lifetime)
	g := &hubGeneration{
		ctx: ctx, stop: stop,
		ready:    make(chan struct{}),
		wakeWork: make(chan struct{}, 1), wakeChanges: make(chan struct{}, 1), connected: make(chan struct{}, 1),
		barrier:         make(chan chan struct{}),
		feeds:           make(chan hubEventFeed),
		views:           map[string]*deliveryView{},
		conns:           map[deliveryLiveConn]map[string]*deliverySub{},
		p0:              map[string]struct{}{},
		unfetchable:     map[string]int{},
		readsDeployment: map[string]bool{},
		sent:            map[string]deliveryFreshnessVerdict{},
	}
	listener := &pglisten.Listener{
		Owner:       "dispatch",
		Dial:        h.dial,
		Channels:    []string{deliveryChangesChannel, deliveryFreshnessChannel, store.AccessChangedChannel, legion.StateChannel, workflowstore.RunsChannel},
		ListenIdle:  h.listenIdle,
		PingTimeout: h.pingTimeout,
		Backoff:     h.backoff,
		OnConnected: func() { g.readersStale.Store(true); signal(g.connected); signal(g.wakeWork) },
		OnNotify:    func(channel, payload string) { h.onNotify(g, channel, payload) },
	}
	h.goLocked(func() { listener.Run(ctx) })
	h.goLocked(func() { h.intake(g) })
	h.goLocked(func() { h.work(g) })
	h.seedEventsLocked(g)
	h.generation = g
	return g
}

// goLocked runs f on a goroutine of the hub's, which running counts, with mu held. Once the hub
// is closed (wait) it starts nothing: every Add of running is made under mu before the close, so
// none can meet wait's Wait.
func (h *deliveryHub) goLocked(f func()) {
	if h.closed {
		return
	}
	h.running.Add(1)
	go func() {
		defer h.running.Done()
		f()
	}()
}

// wait marks the hub closed, so no generation starts again and no goroutine of the hub's either
// (goLocked), then returns once every generation's goroutines have. It is called after the
// lifetime ended, which ends them all.
func (h *deliveryHub) wait() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.running.Wait()
}

// signal leaves one signal on wake unless one is already there.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// readContext is a context for one of g's reads of the shared pool, which ends with g and is marked
// so the pool refuses it a second connection.
func (g *hubGeneration) readContext() context.Context {
	return store.WithTransactionTracking(g.ctx)
}

func (h *deliveryHub) note(n deliveryHubNote) {
	if h.observe != nil {
		h.observe(n)
	}
}

// onNotify routes one notification to g, on the listener's goroutine: a change wakes the intake's
// catch-up of the change log, a freshness row is applied at once, reading nothing, a read of a
// Legion daemon or a write of its setting marks that project's Legion views, a write of a run
// marks the legion_issue views of its issue, and an access change marks every subscriber's reader
// stale and wakes the worker, which reads them again before it recomputes anything
// (recheckStaleReaders). One connection hears them all, in commit order, so a delivery change
// committed after an access change is recomputed for the readers it left.
func (h *deliveryHub) onNotify(g *hubGeneration, channel, payload string) {
	switch channel {
	case deliveryChangesChannel:
		signal(g.wakeChanges)
	case deliveryFreshnessChannel:
		h.applyFreshness(g, payload)
	case legion.StateChannel:
		h.dirtyLegionProject(g, payload)
	case workflowstore.RunsChannel:
		h.dirtyLegionIssue(g, payload)
	case store.AccessChangedChannel:
		g.readersStale.Store(true)
		signal(g.wakeWork)
	}
}

// dirtyLegionProject marks every Legion view of the project a read of a Legion daemon just recorded,
// or whose Legion setting a write just named or removed: its board, and the legion_issue view of
// each of its issues. The sampler announces every read, failed or not: the header and an issue's
// read carry the read itself, so a view whose daemon stopped answering moves as surely as one
// whose issues did. A setting's write moves every such view too: the board appears or is refused,
// and an issue's view says whether the project names a daemon (configured).
// Every reader's view of each is marked.
func (h *deliveryHub) dirtyLegionProject(g *hubGeneration, project string) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, view := range g.views {
		if view.spec.classes().reads == viewReadsLegion && view.spec.project == project {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
	}
}

// dirtyLegionIssue marks the legion_issue views of the issue a write of one of its runs just
// committed for, and nothing else: no other view shows an issue's runs, and a run's write appends
// no event the intake would hear (workflowstore.RunsChannel).
func (h *deliveryHub) dirtyLegionIssue(g *hubGeneration, issue string) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, view := range g.views {
		if view.spec.issue != "" && view.spec.issue == issue {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
	}
}

// dirtyLegionViews marks every Legion view (viewReadsLegion), the boards and the legion_issue
// views alike, at each connection of g's listener, the first included (intake). What a Legion
// view shows of a daemon's state and of an issue's runs reaches the hub on a notification alone
// (legion.StateChannel, workflowstore.RunsChannel), which Postgres sends only to a connection that
// listens: one sent while the listener was down, or before its first LISTEN while a view's first
// read ran, is lost, and the catch-up of the change log marks only the views a delivery change
// touches, which no Legion view is (touchedBy). So each is read again, and its read answers
// whatever the lost notification announced; one that finds nothing moved sends nothing, since the
// board's computed_at is in no delta and an issue's body has no computed time.
func (h *deliveryHub) dirtyLegionViews(g *hubGeneration) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, view := range g.views {
		if view.spec.classes().reads == viewReadsLegion {
			h.markDirtyLocked(g, view, now, deliveryCauseChange)
		}
	}
}

// recheckStaleReaders resolves every sub's reader again (recheckReaders), on the worker's goroutine,
// when an access change or a relayed event may have moved one since the last time
// (hubGeneration.readersStale), so no recompute after the change reads for a reader it moved.
func (h *deliveryHub) recheckStaleReaders(g *hubGeneration) {
	if !g.readersStale.Swap(false) {
		return
	}
	h.mu.Lock()
	h.readerChecks++
	h.mu.Unlock()
	h.recheckMu.Lock()
	defer h.recheckMu.Unlock()
	h.recheckReaders(g)
	h.note(deliveryHubNote{what: "readers rechecked"})
}

// catchUpNow catches the change log up and returns once it has, or once the running generation
// ended; with no generation running it returns at once.
func (h *deliveryHub) catchUpNow() {
	h.mu.Lock()
	g := h.generation
	h.mu.Unlock()
	if g == nil {
		return
	}
	done := make(chan struct{})
	select {
	case g.barrier <- done:
		<-done
	case <-g.ctx.Done():
	}
}

// liveRefusal is the error frame refusing sub with code.
func liveRefusal(sub, code, message string) LiveError {
	return LiveError{Type: "error", Sub: sub, Code: code, Message: message}
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func equalStringPointers(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
