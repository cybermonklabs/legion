package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"
)

// The test hooks let a test of several server processes (cmd/dispatch's multiprocess suite) see
// and steer what one process's rooms do: which rooms it holds, how often a room failed, compacted
// waiting for the document lock or was closed by the service, what another process's
// notifications carried to its rooms, when the settlement resumption armed a document's
// settlement, a room's persistence held back, and an agent's open write parked at a named point.
// A service built without Deps.TestHooks holds none of this: s.hooks is nil, every seam below is a
// nil check, and every exported hook answers ErrTestHooksOff.

var (
	// ErrTestHooksOff refuses a test hook on a service built without Deps.TestHooks.
	ErrTestHooksOff = errors.New("document test hooks are off")
	// ErrLiveWritePausePoint refuses a pause at a point a live write has no seam for.
	ErrLiveWritePausePoint = errors.New("unknown live write pause point")
	// ErrLiveWritePauseArmed refuses a pause armed for a document and point already armed.
	ErrLiveWritePauseArmed = errors.New("a live write pause is already armed for this document and point")
	// ErrLiveWritePauseNotArmed refuses a wait for, or a release of, a pause nobody armed.
	ErrLiveWritePauseNotArmed = errors.New("no live write pause is armed for this document and point")
)

// LiveWritePausePoint names where an open live write of a document parks for a test.
type LiveWritePausePoint string

const (
	// PauseAfterFirstAppend parks a write right after its transaction's first append to the
	// document, while that transaction holds the document's advisory lock (applyLive).
	PauseAfterFirstAppend LiveWritePausePoint = "after-first-append"
	// PauseAfterCommit parks a write once its transaction has committed, before its publish
	// applies it to the room (publishLiveWrite).
	PauseAfterCommit LiveWritePausePoint = "after-commit"
)

const (
	// MaxAwarenessClientsPerSocketForTest and MaxAwarenessClientsPerRoomForTest are the awareness
	// client bounds of a document socket (maxAwarenessClientsPerSocket, awareness_limit.go) and
	// of a room (maxAwarenessClientsPerRoom, awareness_relay.go), for the multiprocess suite: it
	// runs the built binaries, so it reads neither constant, and it reads what a room holds
	// through RoomCounters.AwarenessClients.
	MaxAwarenessClientsPerSocketForTest = maxAwarenessClientsPerSocket
	MaxAwarenessClientsPerRoomForTest   = maxAwarenessClientsPerRoom
)

// RoomCounters is what one process did with one document's rooms and owed settlement since the
// process started, and what its room of the document holds now (AwarenessClients).
type RoomCounters struct {
	// RoomFailures counts the times a room of the document failed (failRoomLocked).
	RoomFailures int64 `json:"room_failures"`
	// LockWaitingCompactions counts the compactions of a failed room, the one compaction that
	// waits for the document's lock rather than leaving a busy room for later
	// (servicePersistenceAdapter.Compact).
	LockWaitingCompactions int64 `json:"lock_waiting_compactions"`
	// RoomCloses counts the rooms of the document the service closed (roomServer.closeHeld): a
	// room whose stored history was replaced, a room of an issue that closed or reopened, a failed
	// room's eviction, a room live at shutdown. ygo's retirement of an idle room is not the
	// service's close and is not counted.
	RoomCloses int64 `json:"room_closes"`
	// NotifiedHead is the highest version of the document this process's room held every update
	// through (roomCursor.heldThrough) after a catch-up that carried another process's
	// notification (roomCatchUps.headNotified, changed) brought the room to the stored head or
	// found it there; 0 before any. A notification's cause goes to the next catch-up of the room
	// to start (roomCatchUps.request): the one its request queued, or another that started first,
	// such as a sweep's. A catch-up that leaves the room to a later try - a try that reached no
	// verdict, a look again for a room still loading, a replaced history the locked read did not
	// confirm (roomCatchUps.closeReplaced) - carries its causes on the same way, and one that found
	// no room while ygo was loading one leaves them to the catch-up that room's instance schedules
	// (keepForLoadingRoom). A room instance's start (schedule) and its retries raise neither this
	// nor SweptHead, except where they carry such a cause.
	NotifiedHead int64 `json:"notified_head"`
	// SweptHead is NotifiedHead for the sweep each LISTEN that stands runs (roomCatchUps.sweep):
	// what carries a change whose notification was sent while this process's listener had no
	// connection, and so reached nobody here. A catch-up that carries both causes raises both.
	SweptHead int64 `json:"swept_head"`
	// ListenStands counts the LISTENs this process's listener has stood up, each followed by the
	// sweep (roomSync.connected): the first as room sync starts, and one more for each redial. It
	// is the process's count, the same whatever the document.
	ListenStands int64 `json:"listen_stands"`
	// NotifiedIssueCloses counts the notifications that the document's issue closed or reopened
	// which this process's issue-open lane applied to the document (issueOpenChanged), each of
	// which closes its room of the document when one is resident here (setRoomIssueOpen). The
	// recheck after a LISTEN (recheckResidentIssues) and the append gate's close of a room whose
	// append it refused (refuseClosedIssueAppend) are not counted.
	NotifiedIssueCloses int64 `json:"notified_issue_closes"`
	// RecheckedIssueCloses counts the closes of this process's room of the document that the
	// recheck after a LISTEN made (recheckResidentIssues, setRoomIssueOpen), each for an issue
	// that closed or reopened while the room's record still held its earlier state: what carries
	// a close or a reopen whose notification was sent while this process's listener had no
	// connection, and so reached nobody here. A recheck that finds the record matching the issue
	// closes nothing and is not counted.
	RecheckedIssueCloses int64 `json:"rechecked_issue_closes"`
	// RelayedAwareness is, per awareness client, what this process's rooms of the document took
	// of that client from another process's notification (awarenessRelay.received, touched): a
	// state each time a room added or updated the client, a removal each time it took the client
	// away. An entry a room dropped as stale changed nothing and is not counted, and nor is a
	// change a room made itself - a local peer's frame or departure - except one: Expired counts
	// a room's removal, with no notification, of a client it had taken a state of from another
	// process: its expiry of that client (awarenessExpiry), or a local peer's frame that removes
	// it. It is empty, never nil, in what RoomCounters answers.
	RelayedAwareness map[uint64]RelayedAwarenessChanges `json:"relayed_awareness"`
	// SettlementResumptions holds the time of each settlement-resumption pass of this process that
	// armed the document's owed settlement (resumeOwedSettlements), on the service's clock, oldest
	// first.
	SettlementResumptions []time.Time `json:"settlement_resumptions"`
	// AwarenessClients is how many client entries this process's room of the document holds now
	// - the clients present and the tombstones of the ones that left, whoever sent them - which
	// is what maxAwarenessClientsPerRoom bounds. It is 0 when no room of the document is
	// resident here. Unlike the counters above it is a gauge, read as the hook answers.
	AwarenessClients int `json:"awareness_clients"`
}

// RelayedAwarenessChanges is what a process's rooms of one document took of one awareness client
// from other processes' notifications (RoomCounters.RelayedAwareness).
type RelayedAwarenessChanges struct {
	// States counts the times a room added the client or took a later state of it.
	States int64 `json:"states"`
	// Removals counts the times a room took the client away.
	Removals int64 `json:"removals"`
	// Expired counts the times a room took the client away with no notification, after taking a
	// state of it from another process's: the room's expiry of a client it stopped hearing from
	// (awarenessExpiry), or a frame of a local peer that removes the client, which ygo applies for
	// any client a peer names (a browser's provider sends such a removal once its own timeout for
	// the client passes). A room's closes and its peers' departures remove only this process's own
	// clients. It is what carries a removal whose notification was sent while this process's
	// listener had no connection, and so reached nobody here.
	Expired int64 `json:"expired"`
}

// awarenessChange is one change a room's awareness made to one client, as the test hooks count
// it (RoomCounters.RelayedAwareness, countAwarenessChange).
type awarenessChange int

const (
	// relayedState: another process's notification added the client or took a later state of it.
	relayedState awarenessChange = iota
	// relayedRemoval: another process's notification took the client away.
	relayedRemoval
	// ownRemoval: the room took the client away by itself - a peer of this process leaving, a
	// close's removal of this process's clients, an expiry (awarenessExpiry), or a local peer's
	// frame that removes the client.
	ownRemoval
)

// testHooks is the test hooks' state, on a service built with Deps.TestHooks.
type testHooks struct {
	mu       sync.Mutex
	counters map[string]*RoomCounters
	// listenStands is RoomCounters.ListenStands, the process's own count.
	listenStands int64
	// catchUpCauses holds, for each room, the causes the next catch-up of the room to start takes
	// on (roomCatchUps.catchUp, carryCatchUpCause): those of the requests made since the last one
	// started (roomCatchUps.request), and those of a catch-up that left the room to a later try - a
	// retry (armRetry), a look again (takeLoading), a catch-up asked again in place of a close
	// (closeReplaced).
	catchUpCauses map[string]catchUpCause
	// forLoads holds, for each room ygo is loading, the causes of a catch-up that found no room
	// before the load started the room's instance, and the load's record they wait for
	// (keepForLoadingRoom). A catch-up a request queues takes them on while that record is still
	// the room's: the one the load's instance schedules (roomCatchUps.schedule), or one a
	// notification asks for while it loads. Once the load has ended without an instance, its
	// record is gone or a later load's stands, and the catch-up drops them (takeKeptForLoad): no
	// catch-up they asked for carries the change to the next room.
	forLoads map[string]keptForLoad
	// holds is, per document, the time until which its rooms' appends of browser updates wait.
	holds  map[string]time.Time
	pauses map[livePauseKey]*livePause
}

// keptForLoad is what a catch-up that found no room while ygo loaded one kept for that load
// (keepForLoadingRoom): the load's record (roomLoad), held weakly, and the catch-up's causes.
type keptForLoad struct {
	load  weak.Pointer[roomLoad]
	cause catchUpCause
}

type livePauseKey struct {
	artifactID string
	point      LiveWritePausePoint
}

// livePause is one armed pause: the first write to reach its point parks there (parked closes)
// until the test releases it (released closes).
type livePause struct {
	taken    bool
	parked   chan struct{}
	released chan struct{}
}

func newTestHooks() *testHooks {
	return &testHooks{
		counters:      make(map[string]*RoomCounters),
		catchUpCauses: make(map[string]catchUpCause),
		forLoads:      make(map[string]keptForLoad),
		holds:         make(map[string]time.Time),
		pauses:        make(map[livePauseKey]*livePause),
	}
}

// ResidentRooms lists the documents this process holds a room of now, sorted.
func (s *Service) ResidentRooms() []string {
	rooms := s.srv.Rooms()
	slices.Sort(rooms)
	return rooms
}

// RoomCounters reads what this process did with artifactID's rooms and owed settlement since it
// started, and how many awareness client entries its room of the document holds now. Its
// RelayedAwareness and SettlementResumptions are copies, empty rather than nil when none was
// counted.
func (s *Service) RoomCounters(artifactID string) (RoomCounters, error) {
	h := s.hooks
	if h == nil {
		return RoomCounters{}, ErrTestHooksOff
	}
	h.mu.Lock()
	var counted RoomCounters
	if counters := h.counters[artifactID]; counters != nil {
		counted = *counters
	}
	counted.ListenStands = h.listenStands
	relayed := make(map[uint64]RelayedAwarenessChanges, len(counted.RelayedAwareness))
	maps.Copy(relayed, counted.RelayedAwareness)
	counted.RelayedAwareness = relayed
	h.mu.Unlock()
	counted.SettlementResumptions = append([]time.Time{}, counted.SettlementResumptions...)
	clients, err := s.heldAwarenessClients(artifactID)
	if err != nil {
		return RoomCounters{}, err
	}
	counted.AwarenessClients = clients
	return counted, nil
}

// heldAwarenessClients is how many client entries this process's room of artifactID holds now,
// the tombstones of the clients that left included: the clients a full awareness update of the
// room encodes. A document with no room here holds none.
func (s *Service) heldAwarenessClients(artifactID string) (int, error) {
	aw, resident := s.srv.GetAwareness(artifactID)
	if !resident {
		return 0, nil
	}
	entries, err := decodeAwarenessEntries(aw.EncodeUpdate(nil))
	if err != nil {
		return 0, fmt.Errorf("read the awareness of the room of %s: %w", artifactID, err)
	}
	return len(entries), nil
}

// HoldRoomPersistence makes every append of a browser update from this process's rooms of
// artifactID that starts within duration from now wait until duration has passed. The room
// applies and broadcasts each such update meanwhile; only its store waits.
func (s *Service) HoldRoomPersistence(artifactID string, duration time.Duration) error {
	h := s.hooks
	if h == nil {
		return ErrTestHooksOff
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holds[artifactID] = time.Now().Add(duration)
	return nil
}

// ArmLiveWritePause makes the next live write of artifactID to reach point park there until
// ReleaseLiveWritePause. AwaitLiveWriteParked says when it has.
func (s *Service) ArmLiveWritePause(artifactID string, point LiveWritePausePoint) error {
	h := s.hooks
	if h == nil {
		return ErrTestHooksOff
	}
	if point != PauseAfterFirstAppend && point != PauseAfterCommit {
		return ErrLiveWritePausePoint
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := livePauseKey{artifactID: artifactID, point: point}
	if h.pauses[key] != nil {
		return ErrLiveWritePauseArmed
	}
	h.pauses[key] = &livePause{parked: make(chan struct{}), released: make(chan struct{})}
	return nil
}

// AwaitLiveWriteParked waits until a write has parked at the pause armed for artifactID and
// point, or ctx ends.
func (s *Service) AwaitLiveWriteParked(ctx context.Context, artifactID string, point LiveWritePausePoint) error {
	h := s.hooks
	if h == nil {
		return ErrTestHooksOff
	}
	h.mu.Lock()
	pause := h.pauses[livePauseKey{artifactID: artifactID, point: point}]
	h.mu.Unlock()
	if pause == nil {
		return ErrLiveWritePauseNotArmed
	}
	select {
	case <-pause.parked:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseLiveWritePause disarms the pause armed for artifactID and point and lets the write
// parked there, if one is, go on. It reports whether a write had parked.
func (s *Service) ReleaseLiveWritePause(artifactID string, point LiveWritePausePoint) (bool, error) {
	h := s.hooks
	if h == nil {
		return false, ErrTestHooksOff
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := livePauseKey{artifactID: artifactID, point: point}
	pause := h.pauses[key]
	if pause == nil {
		return false, ErrLiveWritePauseNotArmed
	}
	delete(h.pauses, key)
	close(pause.released)
	return pause.taken, nil
}

// countRoomFailure counts a room of artifactID failing.
func (h *testHooks) countRoomFailure(artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countersLocked(artifactID).RoomFailures++
}

// countLockWaitingCompaction counts a compaction of a room of artifactID that waits for the
// document's lock.
func (h *testHooks) countLockWaitingCompaction(artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countersLocked(artifactID).LockWaitingCompactions++
}

// countRoomClose counts a close of a room of artifactID the service made.
func (h *testHooks) countRoomClose(artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countersLocked(artifactID).RoomCloses++
}

// recordCaughtUpHead records that a catch-up of a room of artifactID that cause asked for left
// live, the room's instance, holding every update through its cursor's head: as NotifiedHead
// where a notification asked for it, and as SweptHead where a sweep did. A catch-up this process
// asked for itself (catchUpAsked) records nothing.
func (h *testHooks) recordCaughtUpHead(s *Service, artifactID string, cause catchUpCause, live *crdt.Doc) {
	if h == nil || cause == catchUpAsked {
		return
	}
	cursor := s.cursorOf(live)
	if cursor == nil {
		return
	}
	head := cursor.heldThrough()
	h.mu.Lock()
	defer h.mu.Unlock()
	counters := h.countersLocked(artifactID)
	if cause&catchUpNotified != 0 {
		counters.NotifiedHead = max(counters.NotifiedHead, head)
	}
	if cause&catchUpSwept != 0 {
		counters.SweptHead = max(counters.SweptHead, head)
	}
}

// carryCatchUpCause adds cause, a notification's or a sweep's, to the causes the next catch-up of
// room to start takes on (catchUpCauses).
func (h *testHooks) carryCatchUpCause(room string, cause catchUpCause) {
	if h == nil || cause == catchUpAsked {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.catchUpCauses[room] |= cause
}

// takeCatchUpCauses takes the causes carried to room's next catch-up (carryCatchUpCause), for the
// one that starts now; catchUpAsked where none is carried.
func (h *testHooks) takeCatchUpCauses(room string) catchUpCause {
	if h == nil {
		return catchUpAsked
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cause := h.catchUpCauses[room]
	delete(h.catchUpCauses, room)
	return cause
}

// keepForLoadingRoom keeps cause, the causes of a catch-up of room that found no room, for the
// load of room ygo is running (forLoads), where ygo holds a room of room (Service.roomHeld): ygo
// hands out no document of a room it is still loading, and a load that had scheduled its
// instance's catch-up (keepInstance, roomCatchUps.schedule) would have been looked for again
// (takeLoading), so that catch-up is still to come. The load is named by the record it began
// before it read the store (beginRoomLoad). The catch-up the load's instance schedules takes cause
// on (takeKeptForLoad), and carries it through its own look again while ygo has not published the
// room, so it records the heads this catch-up would have. A room ygo does not hold has gone, and
// cause goes with it; so does cause where no load's record stands yet, since that load reads the
// store after this. Without the hooks it keeps nothing and asks ygo nothing.
func (h *testHooks) keepForLoadingRoom(s *Service, room string, cause catchUpCause) {
	if h == nil || cause == catchUpAsked || !s.roomHeld(room) {
		return
	}
	value, ok := s.roomLoads.Load(room)
	if !ok {
		return
	}
	load := weak.Make(value.(*roomLoad))
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.forLoads[room]
	if kept.load != load {
		kept = keptForLoad{load: load}
	}
	kept.cause |= cause
	h.forLoads[room] = kept
}

// takeKeptForLoad takes room's causes kept for a load (keepForLoadingRoom) out of forLoads, and
// returns them while that load's record is still the room's (Service.roomLoads): the load is the
// one whose instance this catch-up catches up, or it is still loading. A load that ended without
// an instance dropped its record (failRoomLoad, onLoadDocument), or ygo dropped its room and the
// next load's record replaced it (beginRoomLoad): that load's causes go then, carried by no
// catch-up of the room that follows.
func (h *testHooks) takeKeptForLoad(s *Service, room string) catchUpCause {
	if h == nil {
		return catchUpAsked
	}
	h.mu.Lock()
	kept, ok := h.forLoads[room]
	delete(h.forLoads, room)
	h.mu.Unlock()
	if !ok {
		return catchUpAsked
	}
	if value, ok := s.roomLoads.Load(room); ok && weak.Make(value.(*roomLoad)) == kept.load {
		return kept.cause
	}
	return catchUpAsked
}

// countListenStand counts a LISTEN this process's listener stood up (RoomCounters.ListenStands).
func (h *testHooks) countListenStand() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listenStands++
}

// countNotifiedIssueClose counts a notification that artifactID's issue closed or reopened which
// the issue-open lane applied to it (RoomCounters.NotifiedIssueCloses).
func (h *testHooks) countNotifiedIssueClose(artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countersLocked(artifactID).NotifiedIssueCloses++
}

// countRecheckedIssueClose counts a close of artifactID's room that the recheck after a LISTEN
// made (RoomCounters.RecheckedIssueCloses).
func (h *testHooks) countRecheckedIssueClose(artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.countersLocked(artifactID).RecheckedIssueCloses++
}

// countAwarenessChange counts change, which a room of artifactID's awareness made to client
// (RoomCounters.RelayedAwareness). A removal the room made itself counts, as Expired, only for a
// client the hooks have counted a relayed state of: the room's expiry of that client, or a local
// peer's frame that removes it. This process's own clients take no entry.
func (h *testHooks) countAwarenessChange(artifactID string, client uint64, change awarenessChange) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	counters := h.countersLocked(artifactID)
	changes := counters.RelayedAwareness[client]
	switch change {
	case relayedState:
		changes.States++
	case relayedRemoval:
		changes.Removals++
	case ownRemoval:
		if changes.States == 0 {
			return
		}
		changes.Expired++
	}
	if counters.RelayedAwareness == nil {
		counters.RelayedAwareness = make(map[uint64]RelayedAwarenessChanges)
	}
	counters.RelayedAwareness[client] = changes
}

// countSettlementResumption records that a settlement-resumption pass armed the owed settlement
// of each of artifactIDs at at.
func (h *testHooks) countSettlementResumption(artifactIDs []string, at time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, artifactID := range artifactIDs {
		counters := h.countersLocked(artifactID)
		counters.SettlementResumptions = append(counters.SettlementResumptions, at)
	}
}

func (h *testHooks) countersLocked(artifactID string) *RoomCounters {
	counters := h.counters[artifactID]
	if counters == nil {
		counters = &RoomCounters{}
		h.counters[artifactID] = counters
	}
	return counters
}

// awaitPersistenceHold waits out a hold HoldRoomPersistence put on artifactID's appends, or
// until ctx ends.
func (h *testHooks) awaitPersistenceHold(ctx context.Context, artifactID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	until := h.holds[artifactID]
	h.mu.Unlock()
	wait := time.Until(until)
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// parkLiveWrite parks the calling write of artifactID at point when a pause is armed there and
// no write has taken it yet, until the test releases it.
func (h *testHooks) parkLiveWrite(artifactID string, point LiveWritePausePoint) {
	if h == nil {
		return
	}
	h.mu.Lock()
	pause := h.pauses[livePauseKey{artifactID: artifactID, point: point}]
	if pause == nil || pause.taken {
		h.mu.Unlock()
		return
	}
	pause.taken = true
	h.mu.Unlock()
	slog.Info("dispatch: test hook parked a live document write", "room", artifactID, "point", string(point))
	close(pause.parked)
	<-pause.released
	slog.Info("dispatch: test hook released a live document write", "room", artifactID, "point", string(point))
}
