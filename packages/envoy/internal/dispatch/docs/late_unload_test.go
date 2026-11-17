package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room of a document can report its unload after a later room of the same document has loaded,
// and a browser's departure can run the settlement it owes after its room's close has returned.
// The tests here hold those orders on one fixture (unloadAcrossAReload) and pin what each leaves:
// the loads and resident bytes the service counts, and the room a late departure's settlement
// loads, which is retired as that settlement ends unless a browser, a writer or another settlement
// has taken it up (retireSettlementRoom).

// The resident bytes the room figures report (RoomFigures, dispatch_rooms.resident_bytes) are the
// bytes the loads ygo holds took, and each unload of a name ends the name's oldest load. A room
// that loaded again before an earlier room of the same name reported its unload keeps the bytes it
// counted: that late unload is the older load's end, and ygo's OnUnloadDocument names the room
// alone, so bytes kept on the room's record would be taken back by it - leaving the figure short
// by the whole of the resident room's history, for as long as that room lives.
func TestAnEarlierRoomsLateUnloadLeavesTheResidentRoomsBytesResident(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")
	// What is left is the second room's load and its write.
	if got := r.service.residentBytes.Load(); got != r.second {
		t.Errorf("the resident bytes after the first room's late unload = %d, want the %d the second room counted with its write: that unload takes back the bytes of the load that ended", got, r.second)
	}

	// The second room's unload takes back what that load counted. Its browser's departure runs
	// late, after the room's close has returned, as ygo may run it (holdDeparture): the settlement
	// it owes the write loads the document into a room of its own (settlement.take), which the
	// service retires as the departing browser's socket ends (retireSettlementRoom), so nothing is
	// left counted, with no wait for ygo's idle sweep.
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	letDeparture()
	r.awaitSocketEnd("the second room's browser")
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the late departure's settlement has ended")
}

// The room a late departure's settlement loads for itself is retired as the departing browser's
// socket ends only while no one has taken it up. A browser that opens the document while the
// settlement runs joins that room, and the room stays, with the browser in it and its load counted,
// once the departing socket has ended.
func TestARoomALateSettlementLoadedStaysForABrowserThatJoinedIt(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")

	// The late departure's settlement holds once its warm-up has loaded the room
	// (afterSettleWarm), and a browser opens the document then.
	warmed, goOn := make(chan struct{}), make(chan struct{})
	var goOnce sync.Once
	letSettle := func() { goOnce.Do(func() { close(goOn) }) }
	t.Cleanup(letSettle)
	var settling atomic.Bool
	r.service.afterSettleWarm = func(room string) {
		if room != r.room || settling.Swap(true) {
			return
		}
		close(warmed)
		<-goOn
	}
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	letDeparture()
	select {
	case <-warmed:
	case <-time.After(roomWait(t)):
		t.Fatal("the late departure's settlement never reached its room, so nothing here is tested")
	}
	settlementRoom := r.service.srv.GetDoc(r.room)
	if loads := r.loads.Load(); settlementRoom == nil || loads != 3 {
		t.Fatalf("the late departure's settlement holds room %p after %d loads of the document, want a room it loaded itself, the third; nothing here is tested", settlementRoom, loads)
	}
	admitSocket(t, r.url, r.room, "the browser that opens the document while the settlement runs")
	letSettle()
	r.awaitSocketEnd("the second room's browser")

	if got := r.service.srv.GetDoc(r.room); got != settlementRoom {
		t.Errorf("the room the late settlement loaded is %p once that settlement has ended, want it resident (%p): a browser joined it", got, settlementRoom)
	}
	r.service.roomSlots.mu.Lock()
	loads := len(r.service.roomSlots.rooms[r.room].loads)
	r.service.roomSlots.mu.Unlock()
	if bytes := r.service.residentBytes.Load(); loads != 1 || bytes <= 0 {
		t.Errorf("the document counts %d loads and %d resident bytes with the browser in the room, want that room's one load and its bytes", loads, bytes)
	}
	if browsers := registeredConnections(r.service); browsers != 1 {
		t.Errorf("the room records %d browsers once the settlement has ended, want the one that joined it", browsers)
	}
}

// An earlier room's unload that runs while the room a late departure's settlement loads is still
// loading - its load hook has recorded it as the settlement's room, and ygo has not closed its
// ready barrier, so GetDoc answers nil for the name - is that earlier room's end, not the
// settlement room's: the room keeps its mark, and the departing browser's socket end retires it as
// it retires one that finished loading first.
func TestAnEarlierRoomsUnloadLeavesTheMarkOfASettlementRoomStillLoading(t *testing.T) {
	r := unloadAcrossAReload(t)
	// The second room leaves while the first room's unload is held. The first close holds the
	// service's close gate for the room (roomServer.CloseRoom) until its unload returns, so the
	// second room is closed by ygo's own CloseRoom, as in
	// TestUnloadsOfTwoRoomsInEitherOrderLeaveNoResidentBytes; its unload ends the oldest load.
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.Server.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")

	// The late departure's settlement loads a third room, held once its load hook has recorded it
	// as the settlement's, and the first room's unload runs then.
	letReady := r.awaitSettlementLoadHeld(letDeparture)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")
	letReady()
	r.awaitSocketEnd("the second room's browser")
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the late departure's settlement has ended")
}

// A retirement's look at the record while the room a late departure's settlement loads is still
// loading - another settlement ending, say - leaves that room's mark: GetDoc answers nil for a room
// still loading as for one that has gone, and the room is the loading settlement's to retire as its
// departing browser's socket ends.
func TestARetirementLeavesTheMarkOfASettlementRoomStillLoading(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")

	letReady := r.awaitSettlementLoadHeld(letDeparture)
	r.service.retireSettlementRoom(r.room)
	letReady()
	r.awaitSocketEnd("the second room's browser")
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the late departure's settlement has ended")
}

// A writer that opens on the document while the room a late departure's settlement loaded retires -
// past the retirement's last look at the room's record, before ygo's close of the room - takes up
// no room the close then takes away: it waits for the close to return (awaitRetirement) and forks
// from what is there then, so its write is published into the room it forked from, or forks from
// the store where no room is. A writer is inside no Apply on the room while it holds the writer
// slot, so ygo's own checks before its close cannot see it.
func TestAWriterOpeningAsASettlementRoomRetiresTakesUpNoRoomTheCloseTakesAway(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")

	// The retirement holds once it has reserved the record (afterRetireReserved), and the writer
	// opens then. The hold ends once the writer has forked from the room, which it does with the
	// writer slot taken (afterForkRead), or once it waits for the retirement (awaitingRetirement).
	forked, waiting := make(chan struct{}, 1), make(chan struct{}, 1)
	r.service.afterForkRead = func(room string) {
		if room == r.room {
			signal(forked)
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			signal(waiting)
		}
	}
	type publish struct{ cold, resident bool }
	publishes := make(chan publish, 1)
	r.service.checkRendering = func(at string, write *liveWrite) {
		if at == "publish" && write.artifactID == r.room {
			select {
			case publishes <- publish{cold: write.cold, resident: r.service.srv.GetDoc(r.room) != nil}:
			default:
			}
		}
	}
	wrote := make(chan error, 1)
	var reserved atomic.Bool
	r.service.afterRetireReserved = func(room string) {
		if room != r.room || reserved.Swap(true) {
			return
		}
		go func() {
			_, err := joinedReplaceText(r.service, r.room, "A write that opens as the settlement's room retires.\n", model.Actor{Kind: "user", ID: "alice"})
			wrote <- err
		}()
		select {
		case <-forked:
		case <-waiting:
		case <-time.After(roomWait(t)):
			t.Error("the writer neither forked from the retiring room nor waited for its retirement")
		}
	}
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	letDeparture()
	r.awaitSocketEnd("the second room's browser")
	if !reserved.Load() {
		t.Fatal("no retirement reserved the room the late departure's settlement loaded, so nothing here is tested")
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the write that opened as the room retired: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the write that opened as the room retired never returned")
	}
	select {
	case got := <-publishes:
		if !got.cold && !got.resident {
			t.Error("the write forked from the room the late departure's settlement loaded and found no room to publish into: the retirement closed the room under the writer holding it")
		}
	default:
		t.Fatal("the write that opened as the room retired published nothing, so nothing here is tested")
	}
}

// A settlement that starts on the document while the room a late departure's settlement loaded
// retires - past the retirement's last look at the room's record, before ygo's close of the room -
// takes up no room the close then takes away: it waits for the close to return (awaitRetirement)
// and then takes the room ygo holds, loading the document again where the close took the room, so
// it settles what the document owes on a room ygo still holds. A settlement holds the room it
// settles without being inside an Apply on it, so ygo's own checks before its close cannot see it.
// The settlement is an issue's synchronous one (settleOwedNow), which, as every caller of
// settleRoomWithin does, holds the room's record while it runs.
func TestASettlementStartingAsASettlementRoomRetiresTakesUpNoRoomTheCloseTakesAway(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")

	// The retirement holds once it has reserved the record (afterRetireReserved): the document
	// is made to owe a settlement, and one starts then. The hold ends once that settlement has
	// taken its room and the document's lock (afterSettleLock), or once it waits for the
	// retirement (awaitingRetirement). settledOn is the room it took.
	locked, waiting := make(chan struct{}, 1), make(chan struct{}, 1)
	var started atomic.Bool
	var settledOn atomic.Pointer[crdt.Doc]
	r.service.afterSettleLock = func(room string) {
		if room == r.room && started.Load() {
			settledOn.CompareAndSwap(nil, r.service.srv.GetDoc(room))
			signal(locked)
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			signal(waiting)
		}
	}
	settled := make(chan error, 1)
	var reserved atomic.Bool
	r.service.afterRetireReserved = func(room string) {
		if room != r.room || reserved.Swap(true) {
			return
		}
		if _, err := r.service.store.Pool.Exec(context.Background(), `insert into doc_settlements_pending (artifact_id) values ($1)`, r.room); err != nil {
			t.Errorf("owe the document's settlement: %v", err)
			return
		}
		started.Store(true)
		go func() { settled <- r.service.settleOwedNow(context.Background(), r.room, time.Now().Add(roomWait(t))) }()
		select {
		case <-locked:
		case <-waiting:
		case <-time.After(roomWait(t)):
			t.Error("the settlement neither took the retiring room nor waited for its retirement")
		}
	}
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	letDeparture()
	r.awaitSocketEnd("the second room's browser")
	if !reserved.Load() {
		t.Fatal("no retirement reserved the room the late departure's settlement loaded, so nothing here is tested")
	}
	var settleErr error
	select {
	case settleErr = <-settled:
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement that started as the room retired never ended")
	}
	room := settledOn.Load()
	if room == nil {
		t.Fatalf("the settlement that started as the room retired took no room (it ended: %v), so nothing here is tested", settleErr)
	}
	if got := r.service.srv.GetDoc(r.room); got != room {
		t.Errorf("the settlement that started as the room retired settled on room %p, and ygo holds %p once it has ended: the retirement closed the room under the settlement holding it", room, got)
	}
	owed, err := settlementPending(context.Background(), r.service.store.Pool, r.room)
	if settleErr != nil || err != nil || owed {
		t.Errorf("the settlement that started as the room retired ended with %v, and the document still owes a settlement: %v (%v); want the debt settled", settleErr, owed, err)
	}
}

// signal sends on c, a channel with room for one value, unless it already holds one.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// Two rooms of one document that have both left ygo's map can report their unloads in either
// order, and an unload that has begun may wait while a later room of the document loads and
// unloads in full. Whatever the order, each unload ends one of the loads counted under the name,
// so once both have run no bytes of either are counted and the name counts no load.
func TestUnloadsOfTwoRoomsInEitherOrderLeaveNoResidentBytes(t *testing.T) {
	r := unloadAcrossAReload(t)
	// The first room's unload goes on past what releaseUnloadedRoom reads, then waits before it
	// ends a load (beforeUnloadEndsLoad); only that unload waits, and a sync.Once would make the
	// second wait for the first as well.
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	letProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(letProceed)
	var held atomic.Bool
	r.service.beforeUnloadEndsLoad = func(room string) {
		if room != r.room || held.Swap(true) {
			return
		}
		close(waiting)
		<-proceed
	}
	r.letFirstUnload()
	select {
	case <-waiting:
	case <-time.After(roomWait(t)):
		t.Fatal("the first room never began its unload, so nothing here is tested")
	}

	// The second room unloads in full while the first unload waits. The first close holds the
	// service's close gate for the room (roomServer.CloseRoom) until its unload returns, so the
	// second room is closed by ygo's own CloseRoom, as ygo's idle sweep and a last browser's
	// departure retire a room, taking no gate.
	_ = r.socket.Close()
	r.awaitSocketEnd("the second room's browser")
	if err := r.service.srv.Server.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")

	letProceed()
	r.awaitFirstClose()
	r.requireNothingCounted("once both rooms have unloaded")
}

// reloadedRoom is a document whose first room has left ygo's map with its unload held, and whose
// second room has loaded and taken a write meanwhile (unloadAcrossAReload). first and second are
// the resident bytes each load counted, and second is larger, so an unload that took back the
// wrong load's bytes shows. loads counts the loads of the document (Service.bootstrapping).
//
// ended reports each end of a document socket of the room, once the service's handler has
// returned. ygo calls OnLastPeer (Service.settleLastPeer) as the socket's browser leaves, before the
// socket ends, and can do so after a close of the room has returned (sjawhar/ygo v1.55.1-sami.2,
// provider/websocket/inject.go:483-485). That departure runs the settlement the document owes, and
// when no room of the document is resident, the settlement's warm-up loads one (settlement.take),
// counted under the name as any load is, which the socket's end retires unless someone took it up
// (retireSettlementRoom). So the count is final once the socket has ended. hold, when set, holds
// the next departure before it runs, so a test can run it after the room's close (holdDeparture).
//
// The fixture's service keeps a room idle for lateUnloadRoomIdleTimeout rather than ygo's minute,
// so ygo's idle sweep retires none of the rooms a test closes itself, however slowly the machine
// runs, and its Shutdown is bounded by lateUnloadShutdownWait, so a test whose hook never returns
// fails in its cleanup rather than at the binary's deadline.
type reloadedRoom struct {
	t              *testing.T
	service        *Service
	url            string
	room           string
	socket         *gws.Conn
	first, second  int64
	letFirstUnload func()
	closed         chan error
	unloaded       chan struct{}
	ended          chan struct{}
	hold           atomic.Pointer[departureHold]
	loads          atomic.Int32
}

// departureHold holds a departure of the room's last browser (reloadedRoom.holdDeparture): held is
// closed once it has reached the hold, and it runs once release is closed.
type departureHold struct {
	held, release chan struct{}
}

const (
	// lateUnloadRoomIdleTimeout is how long the fixture's rooms stay resident once idle. Far
	// above any test's run, so ygo's idle sweep (roomIdleTimeout in production) retires no room
	// before the test's own close of it, however long a settlement on it takes.
	lateUnloadRoomIdleTimeout = time.Hour
	// lateUnloadShutdownWait bounds the fixture's Shutdown: the drain budget Shutdown spends inside
	// its caller's deadline (ShutdownDrainBudget), and ten seconds for what follows it, and less
	// than roomWaitMargin, which roomWait leaves of the binary's deadline for a failed test's
	// cleanups.
	lateUnloadShutdownWait = ShutdownDrainBudget + 10*time.Second
)

// unloadAcrossAReload loads a document's room, closes it, holds ygo's call of the room's unload,
// and loads and writes to the room again while that unload is held.
func unloadAcrossAReload(t *testing.T) *reloadedRoom {
	t.Helper()
	database := storetest.Open(t)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	service.srv.RoomIdleTimeout = lateUnloadRoomIdleTimeout
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), lateUnloadShutdownWait)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	room := createDocuments(t, database, 1)[0]
	ended := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		service.ServeHTTP(w, req)
		if socketRoom(req) == room {
			ended <- struct{}{}
		}
	}))
	t.Cleanup(server.Close)
	r := &reloadedRoom{t: t, service: service, url: server.URL, room: room, closed: make(chan error, 1), unloaded: make(chan struct{}, 3), ended: ended}
	service.bootstrapping = func(room string, _ []byte) {
		if room == r.room {
			r.loads.Add(1)
		}
	}
	settleLastPeer := service.srv.OnLastPeer
	service.srv.OnLastPeer = func(ctx context.Context, room string) {
		if room == r.room {
			if hold := r.hold.Swap(nil); hold != nil {
				close(hold.held)
				<-hold.release
			}
		}
		settleLastPeer(ctx, room)
	}
	admitSocket(t, server.URL, r.room, "the socket that loads the first room").Close()
	r.awaitSocketEnd("the first room's browser")
	// The service holds this room alone, so the figure is the first room's load.
	if r.first = service.residentBytes.Load(); r.first <= 0 {
		t.Fatalf("the first room's load counted %d resident bytes, want the history it decoded; nothing here is tested", r.first)
	}

	// ygo calls the first room's unload, after taking the room out of its map, only once it is let
	// go. Every unload of the room reports itself once it has run.
	entering := make(chan struct{})
	enter := make(chan struct{})
	var enterOnce sync.Once
	r.letFirstUnload = func() { enterOnce.Do(func() { close(enter) }) }
	t.Cleanup(r.letFirstUnload)
	var entered atomic.Bool
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		if room == r.room && !entered.Swap(true) {
			close(entering)
			<-enter
		}
		unload(ctx, room)
		if room == r.room {
			r.unloaded <- struct{}{}
		}
	}
	go func() { r.closed <- service.srv.CloseRoom(r.room, true) }()
	select {
	case <-entering:
	case <-time.After(roomWait(t)):
		t.Fatal("the first room never reached its unload, so nothing here is tested")
	}

	// The room loads afresh while the first room's unload is held, so the service counts both
	// loads, and a write the second room takes counts against it alone.
	r.socket = admitSocket(t, server.URL, r.room, "the socket that loads the second room")
	if _, err := joinedReplaceText(service, r.room, "A write the second room takes, longer than the seed.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write to the second room: %v", err)
	}
	if r.second = service.residentBytes.Load() - r.first; r.second <= r.first {
		t.Fatalf("the second room counted %d resident bytes against the first room's %d, want more; nothing here is tested", r.second, r.first)
	}
	return r
}

// holdDeparture holds the next departure of the room's last browser before it runs, until letGo:
// held is closed once the departure has reached the hold (awaitHeld).
func (r *reloadedRoom) holdDeparture() (held <-chan struct{}, letGo func()) {
	hold := &departureHold{held: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	letGo = func() { once.Do(func() { close(hold.release) }) }
	r.t.Cleanup(letGo)
	r.hold.Store(hold)
	return hold.held, letGo
}

// awaitHeld waits for who's departure to reach its hold (holdDeparture).
func (r *reloadedRoom) awaitHeld(held <-chan struct{}, who string) {
	r.t.Helper()
	select {
	case <-held:
	case <-time.After(roomWait(r.t)):
		r.t.Fatalf("%s never began to leave the room, so nothing here is tested", who)
	}
}

// requireLateSettlementLoadedARoom requires that the settlement a late departure ran loaded the
// document into a room of its own, the third load, and committed: otherwise no room was left for
// the service to retire, and nothing is tested.
func (r *reloadedRoom) requireLateSettlementLoadedARoom() {
	r.t.Helper()
	owed, err := settlementPending(context.Background(), r.service.store.Pool, r.room)
	if loads := r.loads.Load(); err != nil || owed || loads != 3 {
		r.t.Fatalf("after the late departure the document was loaded %d times and still owes a settlement: %v (%v); want the settlement to have loaded a room of its own, the third, and committed; nothing here is tested", loads, owed, err)
	}
}

// awaitSettlementLoadHeld lets the held departure go (holdDeparture) and holds the room its
// settlement's warm-up loads once ygo's load hook (Service.onLoadDocument) has returned, before ygo
// closes the room's ready barrier and hands the room to anyone, until letReady. It requires the
// hold to find the room recorded as the settlement's and not yet handed out by GetDoc: otherwise
// nothing is tested.
func (r *reloadedRoom) awaitSettlementLoadHeld(letDeparture func()) (letReady func()) {
	r.t.Helper()
	reached, ready := make(chan struct{}), make(chan struct{})
	var once sync.Once
	letReady = func() { once.Do(func() { close(ready) }) }
	r.t.Cleanup(letReady)
	var holding atomic.Bool
	load := r.service.srv.OnLoadDocument
	r.service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		err := load(ctx, room, doc)
		if err == nil && room == r.room && isSettlementLoad(ctx) && !holding.Swap(true) {
			close(reached)
			<-ready
		}
		return err
	}
	letDeparture()
	select {
	case <-reached:
	case <-time.After(roomWait(r.t)):
		r.t.Fatal("the late departure's settlement never loaded a room, so nothing here is tested")
	}
	state := r.service.lockExistingState(r.room)
	marked := state != nil && state.warmed != nil
	if state != nil {
		r.service.unlockState(r.room, state)
	}
	if !marked || r.service.srv.GetDoc(r.room) != nil {
		r.t.Fatalf("the settlement's room, held before ygo hands it out, is recorded as the settlement's: %v, and handed out by GetDoc: %v; want recorded and not handed out, or nothing here is tested", marked, r.service.srv.GetDoc(r.room) != nil)
	}
	return letReady
}

// awaitUnload waits for the next unload of the room to have run.
func (r *reloadedRoom) awaitUnload(what string) {
	r.t.Helper()
	select {
	case <-r.unloaded:
	case <-time.After(roomWait(r.t)):
		r.t.Fatalf("%s never reported its unload", what)
	}
}

// awaitSocketEnd waits for the next end of a document socket of the room: who's browser has left,
// the settlement its departure owed has run, and the socket has let go of the room's record
// (reloadedRoom).
func (r *reloadedRoom) awaitSocketEnd(who string) {
	r.t.Helper()
	select {
	case <-r.ended:
	case <-time.After(roomWait(r.t)):
		r.t.Fatalf("%s's socket never ended", who)
	}
}

// awaitFirstClose waits for the first room's close to return.
func (r *reloadedRoom) awaitFirstClose() {
	r.t.Helper()
	if err := <-r.closed; err != nil {
		r.t.Fatalf("close the first room: %v", err)
	}
}

// requireNothingCounted requires that no bytes and no load of the room are counted.
func (r *reloadedRoom) requireNothingCounted(when string) {
	r.t.Helper()
	if got := r.service.residentBytes.Load(); got != 0 {
		r.t.Errorf("the resident bytes %s = %d, want 0: a load whose bytes no unload took back stays counted for good", when, got)
	}
	r.service.roomSlots.mu.Lock()
	slot := r.service.roomSlots.rooms[r.room]
	r.service.roomSlots.mu.Unlock()
	if len(slot.loads) != 0 {
		r.t.Errorf("the document still counts %d loads %s, want none", len(slot.loads), when)
	}
}
