package api

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
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
// its resume decoded as a delivery view's.
type deliverySubscription struct {
	Sub    string
	View   DeliveryLiveView
	Resume *DeliveryLiveResume
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
// ended no generation starts and the frame is answered nothing, as the socket closes with 1012.
func (h *deliveryHub) Subscribe(conn deliveryLiveConn, frame deliverySubscription) bool {
	spec, refusal := parseDeliveryLiveView(frame.View)
	if refusal != nil {
		refusal.Sub = frame.Sub
		conn.Send(*refusal)
		return false
	}
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
	g := h.generation
	if g == nil {
		if h.closed || h.lifetime.Err() != nil {
			return false
		}
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
	view := g.views[spec.key]
	if view == nil {
		view = &deliveryView{spec: spec, subs: map[*deliverySub]struct{}{}}
		g.views[spec.key] = view
	}
	sub := &deliverySub{id: frame.Sub, conn: conn, view: view, needsSnapshot: held == nil, needsVerdict: true}
	if held != nil {
		sub.revs = *held
	}
	view.subs[sub] = struct{}{}
	subs[frame.Sub] = sub
	switch {
	case view.body != nil && !view.dirty && !view.computing:
		h.answerLocked(g, sub)
	case view.computing || view.dirty:
		// The recompute under way, or the one the view already waits for, answers it.
	default:
		h.markDirtyLocked(g, view, now, deliveryCauseSubscribe)
	}
	return true
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

// leaveLocked takes sub out of its view, and drops the view it leaves without a sub.
func (g *hubGeneration) leaveLocked(sub *deliverySub) {
	delete(sub.view.subs, sub)
	if len(sub.view.subs) == 0 && g.views[sub.view.spec.key] == sub.view {
		delete(g.views, sub.view.spec.key)
	}
}

// The channels the delivery hub listens on, all on its listener's one connection (startLocked).
// delivery.ChangesChannel carries no payload and is notified as a delivery_changes row commits
// (migration 0100), as a pending change row of the change log's outbox commits (20261117100001),
// whose drain the hub runs before it reads, and as a drain that moved rows commits, and the
// delivery package declares it beside the row; delivery_freshness carries the settings row's
// freshness columns as JSON (0100); legion.StateChannel carries a project key and is notified
// after each read of that project's Legion daemon and after each write of its Legion setting
// (settings_legion.go), which with Dispatch's own events and the change log's writes of the
// project's stored facts (touchedBy) is all that moves a Legion board; and
// workflowstore.RunsChannel carries an issue key and is notified as a write of one of that
// issue's runs commits, which with the project's notifies on legion.StateChannel is all that
// moves the issue's legion_issue view.
const deliveryFreshnessChannel = "delivery_freshness"

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
// can touch; recomputes each dirty view once per burst, as the GET routes' population read and
// pure compute, one at a time but for the evals views, up to evalsRecomputes of which run at once
// beside it; re-runs the compute alone, reading nothing, when something leaves a preset window;
// and sends each subscriber only the keys whose revision differs from what it holds.
//
// Each run of the hub is a generation (hubGeneration): the first Subscribe of a hub with no sub
// starts one, and the drop of its last sub stops it, at once, with no grace period, so a process
// with no open page holds no connection for the hub and reads neither log for it. A generation
// runs three goroutines on a context of its own: the listener; the intake, which alone moves the
// change log's cursor, applies the relay's events and marks views dirty; and the worker, which
// alone starts a recompute: it runs each recompute of a delivery view, a Legion view or an expiry
// itself, one at a time, and each of an evals view on a goroutine of its own, up to
// evalsRecomputes at once (recomputeAside). Its relay subscription, which waits for a safe
// horizon, is taken on another goroutine that ends once it has (seedEvents), at the start and
// after a feed closes. A generation owns its views and conns, and
// the drop of its last conn is what stops it, so a goroutine of a stopped generation reaches only
// its own empty maps, never the next generation's, and returns once its context ends.
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

	// running counts every generation's goroutines. A generation's own are added under mu and only
	// while closed is false, so no such Add can follow the Wait in wait; an evals view's recompute
	// is added by the worker (recomputeAside), a goroutine running already counts, so its Add
	// starts while the count is above zero, which sync.WaitGroup allows at any moment.
	running sync.WaitGroup

	mu sync.Mutex
	// closed is set by wait: the lifetime has ended and no generation starts again.
	closed bool
	// generation is the running generation, nil while no conn holds a sub.
	generation *hubGeneration
}

// evalsRecomputes is the most recomputes of evals views a generation runs at once
// (recomputeAside). One rewrite of a delivery settings change marks every evals view of each
// project whose rows it moves, and each recompute is one snapshot read of the shared pool: on the
// build farm, 168 evals views marked at once were answered one after another for 1.0-1.6 s, 6-10
// ms each, which with the rewrite's own commit passed the 2 s R14 gives a settings write. Four at
// once bring a project's 52 open views to 13 rounds, about 0.13-0.2 s, and hold at most four of
// the shared pool's 64 connections (store's sharedPoolSize), as a broadcast's delivery does for
// the same reason (broadcastDeliveryWorkers). Evals views alone run aside: they judge no
// freshness, where the delivery views' reads set the one verdict in the order they run.
const evalsRecomputes = 4

// hubGeneration is one run of the hub. ctx, stop and the channels are set once at its start; the
// rest is guarded by the hub's mu. views and conns are emptied by the drops that stop it, and no
// sub joins it once stopped.
type hubGeneration struct {
	ctx  context.Context
	stop context.CancelFunc
	// ready closes once the intake has read where the change log stands, which the worker waits
	// for. The relay subscription is taken apart (seedEvents), so neither waits on the horizon.
	ready chan struct{}
	// Each wake holds at most one pending signal: what it asks for covers every signal before it.
	wakeWork, wakeChanges, connected chan struct{}
	barrier                          chan chan struct{}
	// feeds hands the intake the relay subscription seedEvents took. It has no slot: a hand-over
	// completes only as the intake takes it, so a subscription is always either the intake's, which
	// it cancels as it returns, or still seedEvents', which cancels it once g has ended. A slot
	// could hold one the intake never took past g's end, which neither would cancel, and the relay
	// would keep its generation, its LISTEN connection and its reads of the event log for it.
	feeds chan hubEventFeed
	// evalsSlots holds one token for each recompute of an evals view under way, at most
	// evalsRecomputes (recomputeAside).
	evalsSlots chan struct{}

	views map[string]*deliveryView // by canonical view key
	conns map[deliveryLiveConn]map[string]*deliverySub

	// change is where the hub stands in the delivery change log (delivery.ChangeCursor): the last
	// change read and its at, to tell a reset log, and its ReadAt the hub's last read of the log to
	// its end, to tell one pruned past changes it never read; epoch counts the resets, so a
	// recompute that read across one stores no change position.
	change delivery.ChangeCursor
	epoch  int64
	// p0 is the open P0 issues (OpenP0Keys), kept current from issue event payloads.
	p0 map[string]struct{}
	// fresh is the freshness row as last heard, and sent the verdict last sent; known is false
	// until the first recompute sets them, silently.
	fresh deliveryFreshnessHeard
	sent  deliveryFreshnessVerdict
	known bool
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
	// dirty is set when a change can have moved the view's answer; immediate when the mark waits
	// for no quiet: a sub waits for its first answer, or a settings rewrite's day rows arrived
	// (markDirtyLocked). firstDirty and lastDirty time the coalescing, and notBefore holds a failed
	// read's retry back. A recompute clears dirty and immediate once its read holds its change
	// position (deliveryHub.read), not when it begins: whatever marked the view until then was
	// committed before the reads that follow, which answer it.
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
	// change position it reflects and changedAt when that change was recorded; and readingSeq the
	// position a read under way reflects, so a change at or before it marks nothing.
	body       *deliveryLiveEncoded
	seq        int64
	readingSeq int64
	changedAt  *time.Time
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
}

// deliveryLiveSpec is a subscribed view, resolved: its kind, its window (a preset's length in days,
// or a custom range), its query without from and to, a pipeline view's pinned at, a Legion or
// evals view's project, a legion_issue view's issue, and the canonical key identical views share.
type deliveryLiveSpec struct {
	kind     string
	preset   int
	from, to time.Time
	query    url.Values
	at       *time.Time
	// project is the project whose daemon a Legion view (viewReadsLegion) reads, or whose evals an
	// evals view lists: the one a legion view's board shows, the one a legion_issue view's key
	// names (parseLegionIssueLiveView), and the one an evals view names. A delivery view names
	// none.
	project string
	// issue is the issue a legion_issue view shows (DeliveryLiveView.Issue), which a write of one
	// of its runs marks (dirtyLegionIssue); every other kind names none.
	issue string
	key   string
}

// deliveryFreshnessHeard is the freshness row as the hub last heard it, and the population's
// unfetchable count as the last recompute computed it.
type deliveryFreshnessHeard struct {
	lastEventAt, lastReconcileAt *time.Time
	lastError                    *string
	unfetchable                  int
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
	// the first did, whichever comes first, but for a mark that waits for no quiet
	// (markDirtyLocked). The quiet coalesces a burst of marks into one recompute. viewMaxWait bounds
	// what marks that keep coming less than viewQuiet apart can hold a view back: under a stream of
	// changes the refresher answers each at once (evals.RefresherGap), so its passes and their
	// echoes mark a view every few hundred milliseconds, and R14 gives a change 2 s from its commit
	// to every view, which its passes and the recompute itself share. A second leaves them the
	// other second.
	viewQuiet   = 250 * time.Millisecond
	viewMaxWait = time.Second
	// deliveryLiveRetry is how long a view whose read failed waits before the next.
	deliveryLiveRetry = 5 * time.Second
	// deliveryLivePage is how many changes one catch-up read takes.
	deliveryLivePage = 1000
	// deliveryStaleAfter is how long the reconciled sources go without a pass before the verdict
	// names them stale: freshness.ts's three missed five-minute intervals.
	deliveryStaleAfter = 3 * delivery.ReconcileInterval

	deliveryCauseSubscribe       = "subscribe"
	deliveryCauseChange          = "change"
	deliveryCauseSettingsRewrite = "settings-rewrite"
	deliveryCauseRebuild         = "rebuild"
	deliveryCauseDefaults        = "defaults"
	deliveryCauseExpiry          = "expiry"
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
		barrier:    make(chan chan struct{}),
		feeds:      make(chan hubEventFeed),
		evalsSlots: make(chan struct{}, evalsRecomputes),
		views:      map[string]*deliveryView{},
		conns:      map[deliveryLiveConn]map[string]*deliverySub{},
		p0:         map[string]struct{}{},
	}
	listener := &pglisten.Listener{
		Owner:       "dispatch",
		Dial:        h.dial,
		Channels:    []string{delivery.ChangesChannel, deliveryFreshnessChannel, legion.StateChannel, workflowstore.RunsChannel},
		ListenIdle:  h.listenIdle,
		PingTimeout: h.pingTimeout,
		Backoff:     h.backoff,
		OnConnected: func() { signal(g.connected) },
		OnNotify:    func(channel, payload string) { h.onNotify(g, channel, payload) },
	}
	h.running.Add(3)
	go func() { defer h.running.Done(); listener.Run(ctx) }()
	go func() { defer h.running.Done(); h.intake(g) }()
	go func() { defer h.running.Done(); h.work(g) }()
	h.seedEvents(g)
	h.generation = g
	return g
}

// wait marks the hub closed, so no generation starts again, then returns once every generation's
// goroutines have. It is called after the lifetime ended, which ends them all.
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
// Legion daemon or a write of its setting marks that project's Legion views, and a write of a run
// marks the legion_issue views of its issue.
func (h *deliveryHub) onNotify(g *hubGeneration, channel, payload string) {
	switch channel {
	case delivery.ChangesChannel:
		signal(g.wakeChanges)
	case deliveryFreshnessChannel:
		h.applyFreshness(g, payload)
	case legion.StateChannel:
		h.dirtyLegionProject(g, payload)
	case workflowstore.RunsChannel:
		h.dirtyLegionIssue(g, payload)
	}
}

// dirtyLegionProject marks every Legion view of the project a read of a Legion daemon just recorded,
// or whose Legion setting a write just named or removed: its board, and the legion_issue view of
// each of its issues. The sampler announces every read, failed or not: the header and an issue's
// read carry the read itself, so a view whose daemon stopped answering moves as surely as one
// whose issues did. A setting's write moves every such view too: the board appears or is refused,
// and an issue's view says whether the project names a daemon (configured). The Legion views are
// the ones that show a daemon's state: a view that names the project and shows none (an evals
// view) is left, since the sampler reads each daemon once a minute and nothing such a view shows
// moves with that read.
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
// read ran, is lost, and the catch-up of the change log marks a Legion view only for what the log
// carries, a write of a board's stored facts (touchedBy), never for a daemon's read or a run's
// write. So each is read again, and its read answers whatever the lost notification announced;
// one that finds nothing moved sends nothing, since the board's computed_at is in no delta, nor
// are the values of its issues that only grew with the clock, and an issue's body has no computed
// time.
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
