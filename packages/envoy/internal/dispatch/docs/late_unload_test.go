package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/testcleanup"
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
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()
	// What is left is the second room's load and its write.
	if got := r.service.residentBytes.Load(); got != r.second {
		t.Errorf("the resident bytes after the first room's late unload = %d, want the %d the second room counted with its write: that unload takes back the bytes of the load that ended", got, r.second)
	}

	// The second room's unload takes back what that load counted. Its browser's departure runs
	// late, after the room's close has returned, as ygo may run it (holdDeparture): the settlement
	// it owes the write loads the document into a room of its own (settlement.take), which the
	// service retires as the departing browser's socket ends (retireSettlementRoom), so nothing is
	// left counted, with no wait for ygo's idle sweep.
	r.departLate()
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the late departure's settlement has ended")
}

// The room a late departure's settlement loads for itself is retired as the departing browser's
// socket ends only while no one has taken it up. A browser that opens the document while the
// settlement runs joins that room, and the room stays, with the browser in it and its load counted,
// once the departing socket has ended. The room is then no longer the settlement's
// (takeUpSettlementRoom): when that browser leaves too, its socket's end retires nothing, and the
// room idles out as any room a browser left.
func TestARoomALateSettlementLoadedStaysForABrowserThatJoinedIt(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	settlementRoom, letSettle := r.holdLateSettlementInItsRoom()
	browser := admitSocket(t, r.url, r.room, "the browser that opens the document while the settlement runs")
	letSettle()
	r.awaitSocketEnd("the second room's browser")

	if got := r.service.srv.GetDoc(r.room); got != settlementRoom {
		t.Errorf("the room the late settlement loaded is %p once that settlement has ended, want it resident (%p): a browser joined it", got, settlementRoom)
	}
	if loads, bytes := r.counted(); loads != 1 || bytes <= 0 {
		t.Errorf("the document counts %d loads and %d resident bytes with the browser in the room, want that room's one load and its bytes", loads, bytes)
	}
	if browsers := registeredConnections(r.service); browsers != 1 {
		t.Errorf("the room records %d browsers once the settlement has ended, want the one that joined it", browsers)
	}

	_ = browser.Close()
	r.awaitSocketEnd("the browser that joined the room")
	if got := r.service.srv.GetDoc(r.room); got != settlementRoom {
		t.Errorf("the room the late settlement loaded is %p once the browser that joined it has left, want it resident (%p): the browser took it up, so it idles out as any room a browser left", got, settlementRoom)
	}
	if loads, _ := r.counted(); loads != 1 {
		t.Errorf("the document counts %d loads once the browser that joined the room has left, want the room's one", loads)
	}
}

// A writer that takes the writer slot on the room a late departure's settlement loaded takes the
// room up, as a browser joining it does (openLiveWrite): once the write, its own settlement and
// the late settlement have all ended, the departing browser's socket end retires nothing, and nor
// does a retirement run after it, as the end of any later holder would run one. The room idles out
// as any room a write loaded.
func TestARoomALateSettlementLoadedStaysForAWriterThatTookItUp(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	settlementRoom, letSettle := r.holdLateSettlementInItsRoom()
	// The write's own settlement runs once the write has ended, so nothing holds the room's
	// record by the time the test retires the room.
	r.service.settle = 20 * time.Millisecond
	forkedFrom := make(chan *crdt.Doc, 1)
	r.service.beforePublish = func(write *liveWrite) {
		if write.artifactID == r.room {
			select {
			case forkedFrom <- write.forkedFrom.Value():
			default:
			}
		}
	}
	if _, err := joinedReplaceText(r.service, r.room, "A write that takes the settlement's room up.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write while the late settlement holds its room: %v", err)
	}
	select {
	case from := <-forkedFrom:
		if from != settlementRoom {
			t.Fatalf("the write forked from room %p, want the room the late settlement loaded (%p); nothing here is tested", from, settlementRoom)
		}
	default:
		t.Fatal("the write published nothing, so nothing here is tested")
	}
	letSettle()
	r.awaitSocketEnd("the second room's browser")
	waitFor(t, roomWait(t), "the write and every settlement of the document to end", func() bool {
		owed, err := settlementPending(context.Background(), r.service.store.Pool, r.room)
		if err != nil || owed || settlementUnderway(r.service, r.room) {
			return false
		}
		state := r.service.lockExistingState(r.room)
		if state == nil {
			return true
		}
		defer r.service.unlockState(r.room, state)
		return state.liveWriter == nil
	})
	r.service.retireSettlementRoom(r.room)
	if got := r.service.srv.GetDoc(r.room); got != settlementRoom {
		t.Errorf("the room the late settlement loaded is %p once the writer that took it up and every settlement have ended, want it resident (%p): the writer took it up, so it idles out as any room a write loaded", got, settlementRoom)
	}
}

// A writer that takes the writer slot while the room a late departure's settlement loads is still
// loading - before that load's hook takes the room's record - takes that room up too: the load
// that reaches the record under the writer's slot records no mark for the settlement
// (onLoadDocument), so the write's own clearing of the mark at its slot (openLiveWrite) is not
// undone by a load that ends after it. The writer forks from the store, since the room is still
// loading (coldFork), and publishes into the room once it has loaded (publishLiveWrite): its commit
// can come before the load finishes, when its publish waits for the load, or after. Once the
// write, its own settlement and the late settlement have all ended, a retirement leaves the room
// resident, as for a writer that took the room up after it loaded.
func TestARoomALateSettlementLoadedStaysForAWriterThatTookItUpWhileItLoaded(t *testing.T) {
	t.Parallel()
	t.Run("the load ends before the write commits", func(t *testing.T) {
		writerTakesUpALoadingSettlementRoom(t, false)
	})
	t.Run("the write commits before the load ends", func(t *testing.T) {
		writerTakesUpALoadingSettlementRoom(t, true)
	})
}

// writerTakesUpALoadingSettlementRoom holds the load of the room a late departure's settlement
// makes while it decodes the stored history (bootstrapping), before its hook takes the room's
// record, and opens a joined write held after its fork read. It lets the load go first, or, with
// writeFirst, the write first, which commits and reaches its publish while the room still loads.
// It then requires that room to stay resident once a retirement runs after every holder has ended
// (TestARoomALateSettlementLoadedStaysForAWriterThatTookItUpWhileItLoaded).
func writerTakesUpALoadingSettlementRoom(t *testing.T, writeFirst bool) {
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()
	// The write's own settlement runs once the write has ended, so nothing holds the room's
	// record by the time the test retires the room.
	r.service.settle = 20 * time.Millisecond

	// The load held is the late departure's settlement's own, which carries its mark
	// (withSettlementLoad): its hook is on the load's goroutine, ahead of the decode, and is handed
	// the room's document, which GetDoc answers once the room has loaded.
	var settlementDoc atomic.Pointer[crdt.Doc]
	load := r.service.srv.OnLoadDocument
	r.service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == r.room && isSettlementLoad(ctx) {
			settlementDoc.CompareAndSwap(nil, doc)
		}
		return load(ctx, room, doc)
	}
	loading, letLoad := make(chan struct{}), make(chan struct{})
	var loadOnce sync.Once
	releaseLoad := func() { loadOnce.Do(func() { close(letLoad) }) }
	testcleanup.Add(t, releaseLoad)
	var loadHeld atomic.Bool
	countLoad := r.service.bootstrapping
	r.service.bootstrapping = func(room string, state []byte) {
		countLoad(room, state)
		if room == r.room && settlementDoc.Load() != nil && !loadHeld.Swap(true) {
			close(loading)
			<-letLoad
		}
	}
	// The writer holds the writer slot from before its fork read, and is held after that read.
	forked, letWrite := make(chan struct{}), make(chan struct{})
	var writeOnce sync.Once
	releaseWrite := func() { writeOnce.Do(func() { close(letWrite) }) }
	testcleanup.Add(t, releaseWrite)
	var writeHeld atomic.Bool
	r.service.afterForkRead = func(room string) {
		if room == r.room && !writeHeld.Swap(true) {
			close(forked)
			<-letWrite
		}
	}
	type publish struct {
		cold bool
		held *crdt.Doc
	}
	publishes := make(chan publish, 1)
	r.service.beforePublish = func(write *liveWrite) {
		if write.artifactID == r.room {
			select {
			case publishes <- publish{cold: write.cold, held: r.service.srv.GetDoc(r.room)}:
			default:
			}
		}
	}
	var refused atomic.Bool
	r.service.afterPublishRefused = func(room string) {
		if room == r.room {
			refused.Store(true)
		}
	}

	r.closeUnderHeldDeparture()()
	select {
	case <-loading:
	case <-time.After(roomWait(t)):
		t.Fatal("the late departure's settlement never loaded a room, so nothing here is tested")
	}
	if loads := r.loads.Load(); loads != 3 {
		t.Fatalf("the late departure's settlement's load is the document's load %d, want the third; nothing here is tested", loads)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := joinedReplaceText(r.service, r.room, "A write that takes the settlement's room up while it loads.\n", model.Actor{Kind: "user", ID: "alice"})
		wrote <- err
	}()
	select {
	case <-forked:
	case err := <-wrote:
		t.Fatalf("the write returned before its fork read was held: %v; nothing here is tested", err)
	case <-time.After(roomWait(t)):
		t.Fatal("the write never read its fork while the settlement's room loaded, so nothing here is tested")
	}
	var published publish
	if writeFirst {
		releaseWrite()
		select {
		case published = <-publishes:
		case <-time.After(roomWait(t)):
			t.Fatal("the write never reached its publish while the settlement's room loaded, so nothing here is tested")
		}
		if published.held != nil {
			t.Fatalf("the write reached its publish with room %p resident, want the settlement's room still loading; nothing here is tested", published.held)
		}
		releaseLoad()
	} else {
		releaseLoad()
		waitFor(t, roomWait(t), "the settlement's room to finish loading", func() bool {
			return r.service.srv.GetDoc(r.room) != nil
		})
		releaseWrite()
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("write while the late settlement's room loads: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the write that took the slot while the room loaded never returned")
	}
	if !writeFirst {
		select {
		case published = <-publishes:
		default:
			t.Fatal("the write published nothing, so nothing here is tested")
		}
	}
	// No other room of the document loaded and nothing refused the publish; the room's text, read
	// once nothing holds the record, says the publish reached the settlement's room.
	settlementRoom := settlementDoc.Load()
	if loads := r.loads.Load(); loads != 3 || !published.cold || refused.Load() {
		t.Fatalf("after the write the document was loaded %d times, and the write forked cold: %v and had its publish refused: %v; want the settlement's room, the third load, published into by a cold write; nothing here is tested", loads, published.cold, refused.Load())
	}
	if !writeFirst && published.held != settlementRoom {
		t.Fatalf("the write published into room %p, want the room the late settlement loaded (%p); nothing here is tested", published.held, settlementRoom)
	}

	r.awaitSocketEnd("the second room's browser")
	waitFor(t, roomWait(t), "the write and every settlement of the document to end, and nothing else to hold the room's record", func() bool {
		owed, err := settlementPending(context.Background(), r.service.store.Pool, r.room)
		if err != nil || owed {
			return false
		}
		state := r.service.lockExistingState(r.room)
		if state == nil {
			return true
		}
		defer r.service.unlockState(r.room, state)
		return r.service.unusedLocked(state)
	})
	// The settlement's load read the store before the write committed, so only the write's publish
	// put the write's text into that room.
	if got := docMarkdown(settlementRoom); !strings.Contains(got, "up while it loads.") {
		t.Fatalf("the room the late settlement loaded holds %q, want the write's text: the write did not publish into it, so nothing here is tested", got)
	}
	if state := r.service.lockExistingState(r.room); state != nil {
		marked := state.warmed != nil
		r.service.unlockState(r.room, state)
		if marked {
			t.Errorf("the room's record holds the late settlement's mark once the writer that took the room up while it loaded has ended, want none: a retirement closes a marked room")
		}
	}
	r.service.retireSettlementRoom(r.room)
	if got := r.service.srv.GetDoc(r.room); got != settlementRoom {
		t.Errorf("the room the late settlement loaded is %p once the writer that took it up while it loaded and every settlement have ended, want it resident (%p): the writer took it up, so it idles out as any room a write loaded", got, settlementRoom)
	}
	if loads, _ := r.counted(); loads != 1 {
		t.Errorf("the document counts %d loads once the write and every settlement have ended, want the settlement room's one", loads)
	}
}

// An earlier room's unload that runs while the room a late departure's settlement loads is still
// loading - its load hook has recorded it as the settlement's room, and ygo has not closed its
// ready barrier, so GetDoc answers nil for the name - is that earlier room's end, not the
// settlement room's: the room keeps its mark, and the departing browser's socket end retires it as
// it retires one that finished loading first.
func TestAnEarlierRoomsUnloadLeavesTheMarkOfASettlementRoomStillLoading(t *testing.T) {
	t.Parallel()
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
	r.releaseFirstRoom()
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
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()
	letDeparture := r.closeUnderHeldDeparture()

	letReady := r.awaitSettlementLoadHeld(letDeparture)
	r.service.retireSettlementRoom(r.room)
	letReady()
	r.awaitSocketEnd("the second room's browser")
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the late departure's settlement has ended")
}

// Two retirements of the room a late departure's settlement loaded, inside the room at once - as
// when several browsers' sockets end after a forced close - still retire it. The first to take
// the room's close gate has its close refused, since the second is inside the room too
// (websocket.ErrRoomInUse); the room keeps its mark, and the second, which meets the first's
// reservation once it has the gate, waits for it to end and then closes the room. Here the
// departing browser's socket end is the first, and the test runs the second: it enters the room
// while the first is in it (retireEntered), takes the gate only once the first has reserved the
// record (afterRetireReserved), and meets that reservation still held, since the first, having
// left the room, ends it only once the second has met it or returned (afterRetireLeft).
func TestTwoRetirementsInsideASettlementRoomAtOnceRetireIt(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	var entered atomic.Int32
	secondIn, firstReserved := make(chan struct{}), make(chan struct{})
	var reserveOnce sync.Once
	letSecond := func() { reserveOnce.Do(func() { close(firstReserved) }) }
	testcleanup.Add(t, letSecond)
	secondDone, secondWaits := make(chan struct{}), make(chan struct{}, 1)
	r.service.retireEntered = func(room string) {
		if room != r.room {
			return
		}
		switch entered.Add(1) {
		case 1:
			// The first retirement, inside the room: the second enters it too before the first
			// goes on to the gate.
			go func() {
				defer close(secondDone)
				r.service.retireSettlementRoom(r.room)
			}()
			select {
			case <-secondIn:
			case <-time.After(roomWait(t)):
				t.Error("the second retirement never entered the room, so nothing here is tested")
			}
		case 2:
			// The second, inside the room: it takes the gate only after the first has reserved.
			close(secondIn)
			select {
			case <-firstReserved:
			case <-time.After(roomWait(t)):
				t.Error("the first retirement never reserved the room's record")
			}
		}
	}
	var reservations atomic.Int32
	r.service.afterRetireReserved = func(room string) {
		if room == r.room && reservations.Add(1) == 1 {
			letSecond()
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			signal(secondWaits)
		}
	}
	var lefts atomic.Int32
	r.service.afterRetireLeft = func(room string) {
		if room != r.room || lefts.Add(1) != 1 {
			return
		}
		select {
		case <-secondWaits:
		case <-secondDone:
		case <-time.After(roomWait(t)):
			t.Error("the second retirement neither met the first's reservation nor returned")
		}
	}
	r.departLate()
	select {
	case <-secondDone:
	case <-time.After(roomWait(t)):
		t.Fatal("the second retirement never returned")
	}
	if got := entered.Load(); got != 2 {
		t.Fatalf("%d retirements entered the room, want the two this test runs at once; nothing here is tested", got)
	}
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once both retirements have returned")
}

// A writer that opens on the document while the room a late departure's settlement loaded retires -
// past the retirement's last look at the room's record, before ygo's close of the room - takes up
// no room the close then takes away: it waits for the close to return (awaitRetirement) and forks
// from what is there then, so its write is published into the very room it forked from, or forks
// from the store where no room is. A writer is inside no Apply on the room while it holds the
// writer slot, so ygo's own checks before its close cannot see it. The check is the room the write
// forked from against the room the name holds at the publish, not that some room is resident: a
// room another load put back under the name is no room the write forked from.
func TestAWriterOpeningAsASettlementRoomRetiresTakesUpNoRoomTheCloseTakesAway(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	// The retirement holds once it has reserved the record (afterRetireReserved), and the writer
	// opens then. The hold ends once the writer has forked from the room, which it does with the
	// writer slot taken (afterForkRead), or once it waits for the retirement (awaitingRetirement).
	forked, waiting := make(chan struct{}, 1), make(chan struct{}, 1)
	var waited atomic.Bool
	r.service.afterForkRead = func(room string) {
		if room == r.room {
			signal(forked)
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			waited.Store(true)
			signal(waiting)
		}
	}
	type publish struct {
		cold             bool
		forkedFrom, held *crdt.Doc
	}
	publishes := make(chan publish, 1)
	r.service.beforePublish = func(write *liveWrite) {
		if write.artifactID == r.room {
			select {
			case publishes <- publish{cold: write.cold, forkedFrom: write.forkedFrom.Value(), held: r.service.srv.GetDoc(r.room)}:
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
	// The retirement keeps its reservation until it has left the room, which is after ygo's close
	// of the room has returned (afterRetireLeft): the writer is still waiting for it then, with no
	// writer slot, so the close takes no room from under it, and the write forks from what the
	// close left.
	var left atomic.Bool
	r.service.afterRetireLeft = func(room string) {
		if room != r.room || !reserved.Load() || left.Swap(true) {
			return
		}
		state := r.service.lockExistingState(r.room)
		writing := state != nil && state.liveWriter != nil
		if state != nil {
			r.service.unlockState(r.room, state)
		}
		if !waited.Load() || writing {
			t.Errorf("once ygo's close of the retiring room has returned, the writer that opened as it retired waited for the retirement: %v, and holds the writer slot: %v; want it waiting, with no slot, until the retirement ends its reservation", waited.Load(), writing)
		}
	}
	r.departLate()
	if !reserved.Load() {
		t.Fatal("no retirement reserved the room the late departure's settlement loaded, so nothing here is tested")
	}
	if !left.Load() {
		t.Fatal("the retirement that reserved the room's record ended its reservation before it left the room (afterRetireLeft never ran): a writer can take up the room before ygo's close of it returns")
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
		if !got.cold && (got.forkedFrom == nil || got.held != got.forkedFrom) {
			t.Errorf("the write forked from room %p and found room %p under the name to publish into, want the room it forked from: the retirement closed the room under the writer holding it", got.forkedFrom, got.held)
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
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	// The retirement holds once it has reserved the record (afterRetireReserved): the document
	// is made to owe a settlement, and one starts then. The hold ends once that settlement has
	// taken its room and the document's lock (afterSettleLock), or once it waits for the
	// retirement (awaitingRetirement). settledOn is the room it took.
	locked, waiting := make(chan struct{}, 1), make(chan struct{}, 1)
	var started, reachedLock, waited atomic.Bool
	var settledOn atomic.Pointer[crdt.Doc]
	r.service.afterSettleLock = func(room string) {
		if room == r.room && started.Load() {
			reachedLock.Store(true)
			settledOn.CompareAndSwap(nil, r.service.srv.GetDoc(room))
			signal(locked)
		}
	}
	r.service.awaitingRetirement = func(room string) {
		if room == r.room {
			waited.Store(true)
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
	// The retirement keeps its reservation until it has left the room, which is after ygo's close
	// of the room has returned (afterRetireLeft): the settlement is still waiting for it then, and
	// has taken no room, so the close takes no room from under it.
	var left atomic.Bool
	r.service.afterRetireLeft = func(room string) {
		if room != r.room || !reserved.Load() || left.Swap(true) {
			return
		}
		if !waited.Load() || reachedLock.Load() {
			t.Errorf("once ygo's close of the retiring room has returned, the settlement that started as it retired waited for the retirement: %v, and has taken its room and the document's lock (afterSettleLock): %v; want it waiting, with no room, until the retirement ends its reservation", waited.Load(), reachedLock.Load())
		}
	}
	r.departLate()
	if !reserved.Load() {
		t.Fatal("no retirement reserved the room the late departure's settlement loaded, so nothing here is tested")
	}
	if !left.Load() {
		t.Fatal("the retirement that reserved the room's record ended its reservation before it left the room (afterRetireLeft never ran): a settlement can take up the room before ygo's close of it returns")
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

// errLateSettlementStopsShort is what afterSettleVersionWrite answers for the late departure's
// settlement in the retry tests: it stops that settlement short once, after its version write, so
// the settlement is retried (retrySettleLocked).
var errLateSettlementStopsShort = errors.New("the test stops the late departure's settlement short once")

// A late departure's settlement that stops short is retried on the settle timer
// (retrySettleLocked), and the room it loaded is retired as the timer's retry of it ends
// (scheduleSettleAfterLocked): the departing browser's socket end retires nothing while the retry is
// armed or running, since the retry holds the room's record. The retry is held at its version write
// until the departing socket has ended, so the retry's end, not the socket's, is what retires the
// room, and that end comes whatever the order the machine runs the two in.
func TestARoomALateSettlementLoadedIsRetiredAsTheTimersRetryOfThatSettlementEnds(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	// The retry is armed a settle delay after the late settlement stops short.
	r.service.settle = 20 * time.Millisecond
	socketEnded := make(chan struct{})
	var endOnce sync.Once
	letRetry := func() { endOnce.Do(func() { close(socketEnded) }) }
	testcleanup.Add(t, letRetry)
	var versionWrites atomic.Int32
	r.service.afterSettleVersionWrite = func(room string) error {
		if room != r.room {
			return nil
		}
		if versionWrites.Add(1) == 1 {
			return errLateSettlementStopsShort
		}
		select {
		case <-socketEnded:
		case <-time.After(roomWait(t)):
			t.Error("the departing browser's socket never ended while the retry waited for it")
		}
		return nil
	}
	r.departLate()
	if got := versionWrites.Load(); got < 1 {
		t.Fatal("the late departure's settlement never reached its version write, so nothing here is tested")
	}
	if r.service.srv.GetDoc(r.room) == nil {
		t.Fatal("no room of the document is resident once the departing browser's socket has ended, want the one the late settlement loaded: its retry holds the room's record")
	}

	letRetry()
	r.awaitUnload("the room the late settlement loaded, which the timer's retry of that settlement retires as it ends,")
	if got := versionWrites.Load(); got != 2 {
		t.Fatalf("the document's settlements reached %d version writes, want 2: the late settlement's, stopped short, and its retry's", got)
	}
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the timer's retry of the late departure's settlement has ended")
}

// A late departure's settlement that stops short arms its retry (retrySettleLocked), and an issue's
// synchronous settlement (settleOwedNow) that runs before the retry's timer fires takes that retry
// over: the room the late settlement loaded is retired as the synchronous settlement ends. The
// departing browser's socket end retires nothing while the retry's timer is armed, and the
// fixture's settle delay of an hour keeps it armed until the synchronous settlement takes it.
func TestARoomALateSettlementLoadedIsRetiredAsTheSynchronousSettlementThatTakesItsRetryOverEnds(t *testing.T) {
	t.Parallel()
	r := unloadAcrossAReload(t)
	r.releaseFirstRoom()

	var versionWrites atomic.Int32
	r.service.afterSettleVersionWrite = func(room string) error {
		if room == r.room && versionWrites.Add(1) == 1 {
			return errLateSettlementStopsShort
		}
		return nil
	}
	r.departLate()
	if got := versionWrites.Load(); got != 1 {
		t.Fatalf("the late departure's settlement reached %d version writes, want the one it stopped short at; nothing here is tested", got)
	}
	state := r.service.lockExistingState(r.room)
	armed := state != nil && r.service.isSettleTimerArmed(state.settle)
	if state != nil {
		r.service.unlockState(r.room, state)
	}
	if !armed {
		t.Fatal("no retry of the late departure's settlement is armed once its browser's socket has ended, so nothing here is tested")
	}
	if r.service.srv.GetDoc(r.room) == nil {
		t.Fatal("no room of the document is resident once the departing browser's socket has ended, want the one the late settlement loaded: its armed retry holds the room's record")
	}

	if err := r.service.settleOwedNow(context.Background(), r.room, time.Now().Add(roomWait(t))); err != nil {
		t.Fatalf("the synchronous settlement that takes the retry over: %v", err)
	}
	if got := versionWrites.Load(); got != 2 {
		t.Fatalf("the document's settlements reached %d version writes, want 2: the late settlement's, stopped short, and the synchronous one's", got)
	}
	r.requireLateSettlementLoadedARoom()
	r.requireNothingCounted("once the synchronous settlement that took the retry over has ended")
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
	t.Parallel()
	r := unloadAcrossAReload(t)
	// The first room's unload goes on past what releaseUnloadedRoom reads, then waits before it
	// ends a load (beforeUnloadEndsLoad); only that unload waits, and a sync.Once would make the
	// second wait for the first as well.
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	letProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	testcleanup.Add(t, letProceed)
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
		Store:    database,
		Events:   events.NewBroker(),
		Identity: headerIdentity(database),
		Origins:  text.NewOrigins("https://dispatch.example"),
		Settle:   time.Hour,
	})
	service.srv.RoomIdleTimeout = lateUnloadRoomIdleTimeout
	testcleanup.Add(t, func() {
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
	testcleanup.Add(t, server.Close)
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
	testcleanup.Add(t, r.letFirstUnload)
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
	testcleanup.Add(r.t, letGo)
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

// releaseFirstRoom lets the first room's held unload go (unloadAcrossAReload), and waits for that
// room's close to return and its unload to run.
func (r *reloadedRoom) releaseFirstRoom() {
	r.t.Helper()
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")
}

// closeUnderHeldDeparture closes the second room's browser's socket with its departure held
// (holdDeparture), then closes the second room and waits for its unload, so the departure runs
// after the room's close has returned, as ygo may run it. It answers the release of the held
// departure.
func (r *reloadedRoom) closeUnderHeldDeparture() func() {
	r.t.Helper()
	held, letDeparture := r.holdDeparture()
	_ = r.socket.Close()
	r.awaitHeld(held, "the second room's browser")
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		r.t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	return letDeparture
}

// departLate lets the second room's browser depart after the room's close has returned
// (closeUnderHeldDeparture), and waits for that browser's socket to end.
func (r *reloadedRoom) departLate() {
	r.t.Helper()
	r.closeUnderHeldDeparture()()
	r.awaitSocketEnd("the second room's browser")
}

// holdLateSettlementInItsRoom lets the first room's held unload go (releaseFirstRoom), and closes
// the second room while its browser's departure is held (closeUnderHeldDeparture). It then lets
// the departure go and holds the settlement the departure runs once that settlement's warm-up has
// loaded a room of its own, the document's third load (afterSettleWarm). It answers that room and
// the release of the hold.
func (r *reloadedRoom) holdLateSettlementInItsRoom() (settlementRoom *crdt.Doc, letSettle func()) {
	r.t.Helper()
	r.releaseFirstRoom()
	warmed, goOn := make(chan struct{}), make(chan struct{})
	var goOnce sync.Once
	letSettle = func() { goOnce.Do(func() { close(goOn) }) }
	testcleanup.Add(r.t, letSettle)
	var settling atomic.Bool
	r.service.afterSettleWarm = func(room string) {
		if room != r.room || settling.Swap(true) {
			return
		}
		close(warmed)
		<-goOn
	}
	letDeparture := r.closeUnderHeldDeparture()
	letDeparture()
	select {
	case <-warmed:
	case <-time.After(roomWait(r.t)):
		r.t.Fatal("the late departure's settlement never reached its room, so nothing here is tested")
	}
	settlementRoom = r.service.srv.GetDoc(r.room)
	if loads := r.loads.Load(); settlementRoom == nil || loads != 3 {
		r.t.Fatalf("the late departure's settlement holds room %p after %d loads of the document, want a room it loaded itself, the third; nothing here is tested", settlementRoom, loads)
	}
	return settlementRoom, letSettle
}

// counted answers the loads the service counts under the room's name and its resident bytes.
func (r *reloadedRoom) counted() (loads int, bytes int64) {
	r.service.roomSlots.mu.Lock()
	loads = len(r.service.roomSlots.rooms[r.room].loads)
	r.service.roomSlots.mu.Unlock()
	return loads, r.service.residentBytes.Load()
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
	testcleanup.Add(r.t, letReady)
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

// awaitFirstClose waits for the first room's close to return, as long as roomWait allows: what the
// test binary's deadline leaves after roomWaitMargin (t.Deadline), at most roomWaitCap. A close
// that never returns fails the test here by name, and leaves the rest of the deadline to the
// fixture's bounded Shutdown and to the package's later tests.
func (r *reloadedRoom) awaitFirstClose() {
	r.t.Helper()
	select {
	case err := <-r.closed:
		if err != nil {
			r.t.Fatalf("close the first room: %v", err)
		}
	case <-time.After(roomWait(r.t)):
		r.t.Fatal("the first room's close never returned")
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
