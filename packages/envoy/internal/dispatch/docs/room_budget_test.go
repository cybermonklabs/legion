package docs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// Writes through the API to more documents than the budget of resident peerless rooms leave at
// most that many rooms resident, once their settlements have run and they are past the budget's
// window (roomBudgetMinIdle), and the ones left are the ones written last: the budget closes the
// least recently used first (peerlessRoomsOverBudget). Nothing here waits a minute - the rooms
// idle out no sooner than roomIdleTimeout, so without the budget every room a write opened is
// still resident when this ends and the count is the documents written, not the budget.
//
// The document a browser has open is never one of them, however long ago it was written: the
// budget's close is unforced (ygo's CloseRoomWithin without force), which refuses a room with a
// peer, and the service does not even list such a room (its state holds the connection from the
// moment the browser is authorized). Here that document is the one written first, so an eviction
// by age alone would take it before any other.
func TestTheRoomBudgetClosesTheLeastRecentlyUsedPeerlessRooms(t *testing.T) {
	t.Parallel()
	const budget = 8
	// Wide enough that nothing is closed while the writes run, so the rooms the trim below
	// weighs are every document written and the order it closes them in is the write order. A
	// trim that ran between the writes would be closing rooms from a list the writes were still
	// adding to, and the rooms left would be the newest only to within a pass.
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 3*budget)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)

	// The browser's document is written first, so it is the oldest room of all.
	browsers := ids[0]
	connection, err := dialDocument(t, httpServer.URL, browsers)
	if err != nil {
		t.Fatalf("open the browser's document: %v", err)
	}
	defer leaveDocument(connection)
	waitFor(t, 30*time.Second, "the browser's room to load", func() bool {
		return service.srv.GetDoc(browsers) != nil
	})

	// The budget closes no room owing a settlement, and a settlement touches the room it
	// settles, so each write's settlement is run before the next write: at the end no room owes
	// one and the last thing each room saw is its own write. A second of the test's clock
	// separates the writes, so the order the budget reads off the rooms is the write order
	// whatever the writes themselves took, and the clock then moves past the window the budget
	// holds a freshly used room for. Both exclusions hold every room here, so without them the
	// trim closes nothing.
	for _, id := range ids {
		appendThroughAPI(t, service, id)
		settleRoomNow(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the rooms over the budget to close", func() bool {
		return service.PeerlessRoomBytes() <= service.peerlessRoomBytes.Load()
	})

	peerless := peerlessRoomNames(service)
	if len(peerless) > budget {
		t.Fatalf("%d peerless rooms resident after writes to %d documents, want at most the budget of %d",
			len(peerless), len(ids), budget)
	}
	want := ids[len(ids)-len(peerless):]
	slices.Sort(want)
	slices.Sort(peerless)
	if !slices.Equal(peerless, want) {
		t.Fatalf("the peerless rooms left are %v, want the %d documents written last, %v", peerless, len(want), want)
	}
	if service.srv.GetDoc(browsers) == nil {
		t.Fatalf("the budget closed the room the browser has open (%s), which it must never close", browsers)
	}
}

// A write to a document whose room the budget closed lands and settles: the room reloads from the
// store, which holds every update the closed room took (ygo flushes a room before it retires it),
// the write appends to what was there, and the settlement the write arms commits a version of the
// whole document. So the budget costs a reload and loses nothing.
func TestAWriteToARoomTheBudgetClosedLandsAndSettles(t *testing.T) {
	t.Parallel()
	const budget = 1
	service, database := newRoomBudgetService(t, budget)
	service.settle = 20 * time.Millisecond
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)
	first, rest := ids[0], ids[1:]

	// Each write's settlement is run before the next write, as in the test above, so the first
	// document's room is the least recently used of the four and owes nothing; the clock then
	// moves past the window the budget holds a freshly used room for.
	appendMarkdownThroughAPI(t, service, first, "The write before the close.")
	settleRoomNow(t, service, first)
	advance(time.Second)
	for _, id := range rest {
		appendMarkdownThroughAPI(t, service, id, "A write to another document.")
		settleRoomNow(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)
	trimNow(service)
	waitFor(t, 30*time.Second, "the first document's room to close", func() bool {
		return service.srv.GetDoc(first) == nil
	})

	appendMarkdownThroughAPI(t, service, first, "The write after the close.")
	const want = "Room cap probe.\n\nThe write before the close.\n\nThe write after the close.\n"
	text, err := service.Text(context.Background(), first)
	if err != nil {
		t.Fatalf("read the document the budget closed: %v", err)
	}
	if text != want {
		t.Fatalf("the document reads back %q, want %q", text, want)
	}
	var markdown string
	waitFor(t, 30*time.Second, "the settlement of the reloaded room to commit its version", func() bool {
		return database.Pool.QueryRow(context.Background(), `
			select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
		`, first).Scan(&markdown) == nil && markdown == want
	})
}

// The budget leaves a room used inside roomBudgetMinIdle alone and takes an older one instead,
// even where the budget's bytes reach into the young rooms: a room the service used seconds ago
// is one it is about to use again, and closing it costs four reads of the store to load it back
// where keeping it costs one.
func TestTheRoomBudgetKeepsARoomUsedInsideItsWindow(t *testing.T) {
	t.Parallel()
	const budget = 1
	// Wide while the rooms load, so the order below is the order this test set and not what a
	// trim between two loads left.
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 3)
	older, young, youngest := ids[0], ids[1], ids[2]

	warmRoom(t, service, older)
	advance(roomBudgetMinIdle)
	warmRoom(t, service, young)
	warmRoom(t, service, youngest)

	// One room's weight: the budget reaches past the youngest room into young, which it may not
	// close, and on to older, which it may.
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the room past the window to close", func() bool {
		return service.srv.GetDoc(older) == nil
	})
	if service.srv.GetDoc(young) == nil {
		t.Fatalf("the budget closed %s, used inside its %v window", young, roomBudgetMinIdle)
	}
	if service.srv.GetDoc(youngest) == nil {
		t.Fatalf("the budget closed %s, used inside its %v window", youngest, roomBudgetMinIdle)
	}
}

// The budget leaves a room owing a settlement alone and takes the next room over, however long
// ago that room was used: the settlement loads the document again the moment the close retires
// its room, so closing it buys the heap nothing and costs the settlement a reload.
func TestTheRoomBudgetKeepsARoomThatOwesASettlement(t *testing.T) {
	t.Parallel()
	const budget = 2
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)
	owing, next := ids[0], ids[1]

	for _, id := range ids {
		warmRoom(t, service, id)
		advance(time.Second)
	}
	// The service's settlements are an hour out, so this one stays owed for the whole test.
	service.scheduleSettle(owing)
	advance(roomBudgetMinIdle)

	// Two rooms' weight, so the budget reaches the two least recently used: the room owing the
	// settlement, which it must leave, and the room after it, which it closes instead.
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the budget to close the room behind the one owing a settlement", func() bool {
		return service.srv.GetDoc(next) == nil
	})
	if service.srv.GetDoc(owing) == nil {
		t.Fatalf("the budget closed %s, which owes a settlement", owing)
	}
}

// Past the window the budget's order is what it always was: the least recently used room is the
// first closed, and the rooms held back are the newest. The window only holds a room back until
// it is old enough to close.
func TestTheRoomBudgetClosesTheLeastRecentlyUsedRoomPastItsWindow(t *testing.T) {
	t.Parallel()
	const budget = 2
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)

	for _, id := range ids {
		warmRoom(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)

	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the rooms over the budget to close", func() bool {
		return service.PeerlessRoomBytes() <= service.peerlessRoomBytes.Load()
	})
	peerless := peerlessRoomNames(service)
	want := ids[len(ids)-budget:]
	slices.Sort(want)
	slices.Sort(peerless)
	if !slices.Equal(peerless, want) {
		t.Fatalf("the peerless rooms left are %v, want the %d loaded last, %v", peerless, budget, want)
	}
}

// A browser opening a document while the room cap is full of rooms nobody has open is admitted,
// not refused: its socket waits for a place (awaitRoomPlace) and the trim closes one room for it,
// the least recently used of the rooms it may close (peerlessRoomsOverBudget). The rooms it may not
// close stay, however long ago they were loaded: here the oldest owes a settlement, the next was
// just written through the API, which leaves it inside roomBudgetMinIdle and owing its settlement,
// and the third was just read again, which leaves it inside the window. The fourth, the least
// recently used of the rest, is the one closed, and only it, since one socket waits; the bytes stay
// far inside the budget, so nothing is closed for them. The trim once kept a tenth of the cap free
// of peerless rooms ahead of any socket instead, and a socket that found the cap full was refused
// at once.
func TestABrowserAtAFullRoomCapIsAdmittedInTheLeastRecentlyUsedClosableRoomsPlace(t *testing.T) {
	t.Parallel()
	const roomCap = 10
	service, database := newRoomBudgetServiceWithCap(t, 100*roomCap, roomCap)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, roomCap+1)
	rooms, browsers := ids[:roomCap], ids[roomCap]
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)

	// A tenth of a second apart, so the order the trim reads is the order loaded, and then all of
	// them past the window.
	for _, id := range rooms {
		warmRoom(t, service, id)
		advance(100 * time.Millisecond)
	}
	advance(roomBudgetMinIdle)
	owing, written, read, oldest := rooms[0], rooms[1], rooms[2], rooms[3]
	// The service's settlements are an hour out, so these stay owed for the whole test.
	service.scheduleSettle(owing)
	appendThroughAPI(t, service, written)
	warmRoom(t, service, read)
	if places := service.roomPlaces(); places != roomCap {
		t.Fatalf("%d rooms hold places under a cap of %d, so the cap is not full and nothing here is tested", places, roomCap)
	}

	connection := admitSocket(t, httpServer.URL, browsers, "a browser opening a document while peerless rooms fill the room cap")
	defer leaveDocument(connection)
	waitFor(t, 30*time.Second, "the trim to end", func() bool { return !service.trimmingRooms.Load() })
	if service.srv.GetDoc(oldest) != nil {
		t.Errorf("the least recently used room the trim may close, %s, is still resident", oldest)
	}
	for _, kept := range []struct{ room, why string }{
		{owing, "owes a settlement"},
		{written, "was written through the API inside its window and owes that write's settlement"},
		{read, "was read inside its window"},
	} {
		if service.srv.GetDoc(kept.room) == nil {
			t.Errorf("the trim closed %s, which %s", kept.room, kept.why)
		}
	}
	for _, kept := range rooms[4:] {
		if service.srv.GetDoc(kept) == nil {
			t.Errorf("the trim closed %s too, more rooms than the one socket waiting needed", kept)
		}
	}
	if closed := service.RoomsClosedToBudget(); closed != 1 {
		t.Errorf("the trim closed %d rooms for one waiting socket, want 1", closed)
	}
	if waiting := service.roomWaiters.Load(); waiting != 0 {
		t.Errorf("%d sockets count as waiting for a place once the browser was admitted, want none", waiting)
	}
}

// The trim closes no room for a place no socket waits for. Here the rooms API reads loaded fill the
// cap and stand past it, which reserveRoom refuses them nothing to do, every one past its window,
// while their bytes stay far inside the budget: neither an ask for a trim (trimNow) nor a pass run
// as the trim's goroutine runs one (closeRoomsOverBudget) closes anything, so no API-loaded room is
// reloaded for a browser that never came. The standing headroom the trim kept before closed the
// rooms past nine of these ten places, and on the load test's 10x step (INFRA-592) that closed 2.6
// rooms a second, each reloaded by its next write or read.
func TestTheTrimClosesNoRoomForAPlaceNoSocketWaitsFor(t *testing.T) {
	t.Parallel()
	const roomCap = 10
	service, database := newRoomBudgetServiceWithCap(t, 100*roomCap, roomCap)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, roomCap+2)
	for _, id := range ids {
		warmRoom(t, service, id)
		advance(100 * time.Millisecond)
	}
	advance(roomBudgetMinIdle)
	if places := service.roomPlaces(); places != len(ids) {
		t.Fatalf("%d rooms hold places after the test loaded %d, so the cap is not what is tested", places, len(ids))
	}

	trimNow(service)
	waitFor(t, 30*time.Second, "the trim to end", func() bool { return !service.trimmingRooms.Load() })
	service.closeRoomsOverBudget()
	if closed := service.RoomsClosedToBudget(); closed != 0 {
		t.Errorf("the trim closed %d rooms with no socket waiting for a place and the bytes inside the budget, want none", closed)
	}
	if left := peerlessRoomNames(service); len(left) != len(ids) {
		t.Errorf("%d of the %d API-loaded rooms are resident after the trim, want all of them", len(left), len(ids))
	}
}

// A browser opening a document while the room cap is full of rooms the trim may not close - here
// every one was used inside roomBudgetMinIdle - waits for a place for roomPlaceWait, no less and not
// much more, and is then refused 503 with Retry-After, as a socket at a full cap was refused before.
// Its wait closed nothing, and it counts as waiting no longer.
func TestABrowserAtARoomCapNoCloseCanFreeIsRefusedAfterItsWait(t *testing.T) {
	t.Parallel()
	const roomCap = 4
	service, database := newRoomBudgetServiceWithCap(t, 100*roomCap, roomCap)
	// The clock stands still, so every room stays inside its window.
	roomBudgetClock(service)
	ids := createDocuments(t, database, roomCap+1)
	rooms, browsers := ids[:roomCap], ids[roomCap]
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	for _, id := range rooms {
		warmRoom(t, service, id)
	}

	started := time.Now()
	refuseSocket(t, httpServer.URL, browsers, "a browser opening a document while rooms inside their window fill the room cap")
	waited := time.Since(started)
	if waited < roomPlaceWait || waited > roomPlaceWait+time.Second {
		t.Errorf("the socket was refused %v after it dialled, want about roomPlaceWait, %v", waited, roomPlaceWait)
	}
	if waiting := service.roomWaiters.Load(); waiting != 0 {
		t.Errorf("%d sockets count as waiting for a place once the browser was refused, want none", waiting)
	}
	if closed := service.RoomsClosedToBudget(); closed != 0 {
		t.Errorf("the trim closed %d rooms used inside their window, want none", closed)
	}
	for _, room := range rooms {
		if service.srv.GetDoc(room) == nil {
			t.Errorf("the trim closed %s, used inside its window", room)
		}
	}
}

// Every browser that opens a document while the room cap is full of rooms the trim may close gets a
// place, in milliseconds: its socket's wait asks for a pass that runs at once (demandRoomTrim), the
// pass closes one room for it, and that room's unload wakes it (roomSlots.freed). Here a cap of a
// hundred is full of a hundred such rooms, and a hundred browsers open a hundred other documents
// one after another, each kept open, so each finds the cap full and takes the place of one closed
// room. The test logs the median, p99 and longest time from dial to admission.
func TestEveryBrowserAtAFullRoomCapGetsAPlaceInMilliseconds(t *testing.T) {
	t.Parallel()
	const roomCap = 100
	service, database := newRoomBudgetServiceWithCap(t, 10*roomCap, roomCap)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 2*roomCap)
	rooms, browsers := ids[:roomCap], ids[roomCap:]
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	warmRooms(t, service, rooms)
	advance(roomBudgetMinIdle)
	if places := service.roomPlaces(); places != roomCap {
		t.Fatalf("%d rooms hold places under a cap of %d, so the cap is not full and nothing here is tested", places, roomCap)
	}

	waits := make([]time.Duration, 0, len(browsers))
	for i, id := range browsers {
		started := time.Now()
		opened := <-dialSocketAsync(httpServer.URL, id)
		waited := time.Since(started)
		if opened.err != nil {
			t.Fatalf("browser %d of %d: the socket on %s was refused (%v), want it admitted in a closed room's place", i+1, len(browsers), id, opened.err)
		}
		testcleanup.Add(t, func() { _ = opened.connection.Close() })
		// The room's first frame, so its room holds its place before the next browser dials.
		_ = opened.connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, _, err := opened.connection.ReadMessage(); err != nil {
			t.Fatalf("browser %d of %d: the socket on %s sent no first frame: %v", i+1, len(browsers), id, err)
		}
		_ = opened.connection.SetReadDeadline(time.Time{})
		waits = append(waits, waited)
	}
	slices.Sort(waits)
	// Nearest rank: the shortest wait at least p percent of the sockets waited no longer than.
	rank := func(p int) time.Duration { return waits[(len(waits)*p+99)/100-1] }
	t.Logf("time to a place over %d sockets: median=%v p99=%v max=%v", len(waits), rank(50), rank(99), waits[len(waits)-1])
	if p99 := rank(99); p99 >= time.Second {
		t.Errorf("the p99 time from dial to admission is %v, want under a second", p99)
	}
	waitFor(t, 30*time.Second, "the trim to end", func() bool { return !service.trimmingRooms.Load() })
	if closed := service.RoomsClosedToBudget(); closed != roomCap {
		t.Errorf("the trim closed %d rooms for %d browsers, want one for each", closed, len(browsers))
	}
	for _, room := range rooms {
		if service.srv.GetDoc(room) != nil {
			t.Errorf("%s, one of the rooms that filled the cap, is still resident after %d browsers each took a place", room, len(browsers))
		}
	}
}

// The resident weight the trim reads in place of walking the rooms (residentWeight,
// roomTrimHasWork) is the walk's own answer whenever the service is at rest: the weight on each
// resident room's state (roomState.weight), summed over the rooms ygo holds. Each load counts its
// room's weight against its own token and each unload takes back the token it ends, whichever way
// the room goes, so nothing drifts. Here a seeded random run of loads by reads and writes, unloads
// of three kinds (the budget's close, Evict, a failed room's eviction), loads that fail on a history
// that does not decode, and writes whose settlements run, compares the two after every step. The
// writes grow the documents past the weight floor, so the rooms do not all weigh alike. A failure
// logs the run's seed.
func TestTheResidentWeightIsTheResidentRoomsWeightAfterEveryStep(t *testing.T) {
	t.Parallel()
	const steps = 240
	seed := uint64(time.Now().UnixNano())
	testcleanup.Add(t, func() {
		if t.Failed() {
			t.Logf("the run's seed: %d", seed)
		}
	})
	random := rand.New(rand.NewPCG(seed, seed))
	ctx := context.Background()
	// The budget holds far more than these rooms weigh, so no trim closes anything here.
	service, database := newRoomBudgetService(t, 1_000)
	ids := createDocuments(t, database, 10)
	documents, unloadable := ids[:8], ids[8:]
	for _, id := range unloadable {
		corruptStoredHistory(t, database, id)
	}
	paragraph := func() string { return strings.TrimSpace(strings.Repeat("word ", 100+random.IntN(300))) }
	resident := func() []string {
		var rooms []string
		for _, id := range documents {
			if service.srv.GetDoc(id) != nil {
				rooms = append(rooms, id)
			}
		}
		return rooms
	}
	// At rest: no room loading, no unload still to report, no eviction or trim running.
	atRest := func() bool {
		if service.evictionsRunning.Load() != 0 || service.trimmingRooms.Load() {
			return false
		}
		rooms := service.srv.Rooms()
		if int64(len(rooms)) != service.residentRooms.Load() {
			return false
		}
		for _, room := range rooms {
			if service.srv.GetDoc(room) == nil {
				return false
			}
		}
		return true
	}

	heaviest := int64(0)
	for step := range steps {
		var did string
		switch roll := random.IntN(20); {
		case roll < 7:
			id := documents[random.IntN(len(documents))]
			if random.IntN(2) == 0 {
				did = "a read's load of " + id
				if err := service.warmLiveDocument(ctx, id); err != nil {
					t.Fatalf("step %d, %s: %v", step, did, err)
				}
			} else {
				did = "a write's load of " + id
				appendMarkdownThroughAPI(t, service, id, paragraph())
			}
		case roll < 13:
			rooms := resident()
			if len(rooms) == 0 {
				did = "an unload with no room resident"
				break
			}
			id := rooms[random.IntN(len(rooms))]
			switch random.IntN(3) {
			case 0:
				did = "the budget's close of " + id
				service.closeRoomOverBudget(id)
			case 1:
				did = "an eviction of " + id
				if err := service.Evict(ctx, id); err != nil {
					t.Fatalf("step %d, %s: %v", step, did, err)
				}
			default:
				did = "a failed room's eviction of " + id
				service.failRoom(id, errors.New("the test fails the room"))
			}
		case roll < 15:
			id := unloadable[random.IntN(len(unloadable))]
			did = "a failed load of " + id
			if err := service.warmLiveDocument(ctx, id); err == nil {
				t.Fatalf("step %d, %s: the room loaded, want its load to fail", step, did)
			}
		default:
			id := documents[random.IntN(len(documents))]
			did = "a settled write to " + id
			appendMarkdownThroughAPI(t, service, id, paragraph())
			settleRoomNow(t, service, id)
		}
		waitFor(t, 30*time.Second, fmt.Sprintf("the service to come to rest after step %d, %s", step, did), atRest)
		walked := int64(0)
		for _, room := range service.srv.Rooms() {
			state := service.lockExistingState(room)
			if state == nil {
				t.Fatalf("step %d, after %s: the resident room %s has no state", step, did, room)
			}
			walked += state.weight
			heaviest = max(heaviest, state.weight)
			service.unlockState(room, state)
		}
		if counted := service.residentWeight.Load(); counted != walked {
			t.Fatalf("step %d, after %s: the resident weight is %d, and the resident rooms' weights sum to %d", step, did, counted, walked)
		}
	}
	if heaviest <= roomHeapFloor {
		t.Fatalf("no resident room weighed more than the floor of %d, so every room weighed alike and a drift of whole rooms could hide", roomHeapFloor)
	}
}

// The budget counts a room as closed only where its own close unloaded it. ygo's idle sweep takes
// no close gate, and a close by name answers nil where the sweep took the room out of ygo's map
// while the close was flushing it (sjawhar/ygo v1.55.1-sami.2, provider/websocket/inject.go:666-681),
// so the budget's close holds the room in a Server.Apply of its own, under which the sweep evicts
// nothing (closeBudgetRoom). Here that close is held there (roomCloseHeld) until ygo's sweep has
// evicted a room left idle after this one: the sweep takes the rooms past their idle timeout oldest
// first (sweepIdleRooms, idle_sweep.go:107-129), so it tried this room before that one, and left
// it. Let go, the close unloads the room once, and the budget and the service's own close count
// (RoomCounters.RoomCloses) each count that close once and nothing for the room the sweep unloaded
// meanwhile, which a close made after it finds gone.
func TestTheBudgetCountsOnlyTheRoomsItsOwnCloseUnloaded(t *testing.T) {
	t.Parallel()
	service, database := newIdleSweptRoomBudgetService(t, nil)
	unloads := countUnloads(service)
	ids := createDocuments(t, database, 2)
	closing, swept := ids[0], ids[1]

	live, release := holdBudgetClose(t, service, closing)
	// Used once the close holds the other room, so idle since after it.
	warmRoom(t, service, swept)
	waitFor(t, roomWait(t), "ygo's idle sweep to unload the room left idle after the one the budget's close holds", func() bool {
		return unloads(swept) == 1
	})
	if service.srv.GetDoc(closing) != live || unloads(closing) != 0 {
		t.Fatalf("ygo's idle sweep unloaded %s (%d unloads) while the budget's close held it, want the room left to that close", closing, unloads(closing))
	}
	release()

	if got := unloads(closing); got != 1 {
		t.Fatalf("%s was unloaded %d times, want once, by the budget's close", closing, got)
	}
	if service.srv.GetDoc(closing) != nil {
		t.Fatalf("%s is still resident once the budget's close returned", closing)
	}
	if got := service.RoomsClosedToBudget(); got != 1 {
		t.Fatalf("the budget counts %d rooms closed, want 1: the room its close unloaded, and not the one ygo's idle sweep unloaded", got)
	}
	service.closeRoomOverBudget(swept)
	if got := service.RoomsClosedToBudget(); got != 1 {
		t.Fatalf("the budget counts %d rooms closed once it tried the room ygo's idle sweep had unloaded, want still 1", got)
	}
	for room, want := range map[string]int64{closing: 1, swept: 0} {
		counters, err := service.RoomCounters(room)
		if err != nil {
			t.Fatalf("read the room counters of %s: %v", room, err)
		}
		if counters.RoomCloses != want {
			t.Errorf("the service counts %d closes of %s, want %d", counters.RoomCloses, room, want)
		}
	}
}

// No unload of a peerless room takes a place of the request pool's work outside admission
// (admission.Outside, the share cmd/dispatch hands the service as Deps.Outside). ygo's idle sweep
// unloads a room on its own goroutine under the background context (idle_sweep.go:49-61, 222-226),
// and the budget's close runs on the trim's goroutines and carries no place either
// (closeBudgetRoom); a socket's admission reads are the service's one taker of a place
// (socket_admission.go). Here the test holds every place, and an idle unload and a budget close each
// run to their end all the same: at each unload, and inside the close, the places held are the
// test's own, and once both have ended nothing has waited for a place or been refused one. With the
// share full a place taken on either way could not be held, so it would show as work that waited
// behind the test's holds (MaxQueued) and was refused at the share's wait (Refused).
func TestNoUnloadOfAPeerlessRoomTakesAPlaceOutsideAdmission(t *testing.T) {
	t.Parallel()
	outside := admission.New(admission.DefaultLimits()).Outside()
	service, database := newIdleSweptRoomBudgetService(t, outside)
	ids := createDocuments(t, database, 2)
	swept, closed := ids[0], ids[1]

	// The places held at each moment read, on the goroutine of whatever unloaded the room. The
	// count of unloads wraps this, so an unload is counted once both of its readings are taken.
	var mu sync.Mutex
	var held []int
	sample := func() {
		mu.Lock()
		defer mu.Unlock()
		held = append(held, outside.Stats().InUse)
	}
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		sample()
		unload(ctx, room)
		sample()
	}
	unloads := countUnloads(service)
	for range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.HeartbeatCheck); err != nil {
			t.Fatalf("take a place outside admission: %v", err)
		}
	}
	defer func() {
		for range admission.OutsideLimit {
			outside.Release()
		}
	}()
	before := outside.Stats()

	warmRoom(t, service, swept)
	waitFor(t, roomWait(t), "ygo's idle sweep to unload the peerless room", func() bool {
		return unloads(swept) == 1
	})
	_, release := holdBudgetClose(t, service, closed)
	sample()
	release()
	if got := unloads(closed); got != 1 {
		t.Fatalf("%s was unloaded %d times by the budget's close, want once", closed, got)
	}
	if got := service.RoomsClosedToBudget(); got != 1 {
		t.Fatalf("the budget counts %d rooms closed, want the 1 its close unloaded", got)
	}

	after := outside.Stats()
	mu.Lock()
	defer mu.Unlock()
	if len(held) != 5 {
		t.Fatalf("the places were read %d times, want 5: on each side of each of the two unloads, and inside the close", len(held))
	}
	for at, inUse := range held {
		if inUse != admission.OutsideLimit {
			t.Errorf("reading %d: %d places held during the unloads, want the test's own %d", at, inUse, admission.OutsideLimit)
		}
	}
	if after.InUse != admission.OutsideLimit {
		t.Errorf("%d places held once both unloads ended, want the test's own %d", after.InUse, admission.OutsideLimit)
	}
	if after.MaxQueued != before.MaxQueued {
		t.Errorf("at most %d waited for a place, %d before the unloads, want no unload to have waited for one", after.MaxQueued, before.MaxQueued)
	}
	if after.Refused != before.Refused {
		t.Errorf("the places refused %+v by the end, %+v before the unloads, want no unload to have asked for one", after.Refused, before.Refused)
	}
}

// newRoomBudgetService is a document service whose budget of resident peerless rooms holds rooms
// of this package's test documents, and whose rooms idle out no sooner than production's
// roomIdleTimeout, so a test sees the budget's closes and nothing else. The budget is bytes
// (peerlessRoomHeapBudget): each of these documents' stored state is far below the weight floor,
// so each of their rooms weighs exactly roomHeapFloor and a budget of rooms of it holds that many.
// Its settlements are an hour out, so no settlement warms a room the budget closed; a test that
// wants one shortens it.
func newRoomBudgetService(t *testing.T, rooms int) (*Service, *store.Store) {
	t.Helper()
	return newRoomBudgetServiceWithCap(t, rooms, 0)
}

// newRoomBudgetServiceWithCap is newRoomBudgetService with a room cap of roomCap
// (Deps.MaxLiveRooms; 0 is maxLiveRooms).
func newRoomBudgetServiceWithCap(t *testing.T, rooms, roomCap int) (*Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	service := New(Deps{
		Store:        database,
		Events:       events.NewBroker(),
		Identity:     identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)},
		ServerURL:    "https://dispatch.example",
		Settle:       time.Hour,
		MaxLiveRooms: roomCap,
	})
	service.peerlessRoomBytes.Store(int64(rooms) * roomHeapFloor)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database
}

// peerlessRoomNames is the documents the service holds a resident room for with no browser
// connected - what the budget counts, as Service.PeerlessRooms counts them - by name, so a test can
// say which rooms are left and not only how many.
func peerlessRoomNames(service *Service) []string {
	var peerless []string
	for _, room := range service.srv.Rooms() {
		if service.srv.GetDoc(room) == nil {
			continue
		}
		state := service.lockExistingState(room)
		if state == nil {
			continue
		}
		if len(state.connected) == 0 {
			peerless = append(peerless, room)
		}
		service.unlockState(room, state)
	}
	return peerless
}

// roomBudgetClock puts service on a clock this test moves, so a room's age against the budget's
// window (roomBudgetMinIdle) is the one the test set and not however long its writes took. It
// answers the function that moves the clock on; it is installed before any room loads, so no
// trim reads the clock while it is replaced.
func roomBudgetClock(service *Service) func(time.Duration) {
	started := time.Now()
	var aged atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(aged.Load())) }
	return func(d time.Duration) { aged.Add(int64(d)) }
}

// trimNow weighs the peerless rooms at once, as any use of a room asks the service to
// (noteRoomUsed), without waiting out the roomTrimInterval since the last pass.
func trimNow(service *Service) {
	service.lastRoomTrim.Store(0)
	service.trimPeerlessRooms()
}

// warmRoom loads the document's room with an Apply that changes nothing, as a settlement's
// warm-up does (warmLiveDocument), and stamps it used now. The document's first version already
// matches its stored state (createDocuments), so the load leaves no settlement owed and the
// budget may close the room as soon as it is past its window.
func warmRoom(t *testing.T, service *Service, id string) {
	t.Helper()
	if err := service.warmLiveDocument(context.Background(), id); err != nil {
		t.Fatalf("load the room of document %s: %v", id, err)
	}
	if service.srv.GetDoc(id) == nil {
		t.Fatalf("document %s's room is not resident after its load", id)
	}
}

// settleRoomNow settles the document at once rather than at the end of its settle window and
// waits for the settlement to end, so its room is one the budget may close: it closes no room
// owing a settlement (peerlessRooms).
func settleRoomNow(t *testing.T, service *Service, id string) {
	t.Helper()
	service.SettleNow(id)
	waitFor(t, 30*time.Second, "the settlement of document "+id+" to end", func() bool {
		return !roomOwesSettlement(service, id)
	})
}

// roomOwesSettlement is whether the service holds a settlement of the document running or still
// to run (owesSettlementLocked), which keeps its room out of the budget's closes.
func roomOwesSettlement(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return owesSettlementLocked(state, service.isSettleTimerArmed(state.settle))
}

// newIdleSweptRoomBudgetService is a document service whose rooms ygo's idle sweep evicts a second
// after their last use (Deps.RoomIdleTimeout; ygo sweeps every second at that timeout), with the
// test hooks' counters on, outside as the request pool's places outside admission (nil for none),
// settlements an hour out and no trim of the budget's own: a budget close here is the test's own
// call (closeRoomOverBudget), so the closes a test sees are its own and the sweep's.
func newIdleSweptRoomBudgetService(t *testing.T, outside *admission.Outside) (*Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	deps := testServiceDeps(database)
	deps.Settle = time.Hour
	deps.RoomIdleTimeout = time.Second
	deps.TestHooks = true
	deps.Outside = outside
	service := New(deps)
	service.peerlessRoomBytes.Store(0)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database
}

// countUnloads counts, by name, the rooms ygo unloads, whichever way it unloads them
// (OnUnloadDocument), each once the service's own unload hook has returned, and answers the count of
// one room. It wraps the hook, so it is called before any room loads.
func countUnloads(service *Service) func(room string) int {
	var mu sync.Mutex
	counts := make(map[string]int)
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		unload(ctx, room)
		mu.Lock()
		defer mu.Unlock()
		counts[room]++
	}
	return func(room string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[room]
	}
}

// holdBudgetClose loads room and starts the budget's close of it (closeRoomOverBudget) on a
// goroutine, which it holds once the close holds the room in its own Server.Apply, before it
// closes it (roomCloseHeld). The room is loaded by an Apply of the test's own that stays inside
// it until the close holds it in turn, so ygo's idle sweep takes nothing from the room before the
// close does: it answers once that Apply has left, with the room's instance and a release that lets
// the close go on and waits for it to return.
func holdBudgetClose(t *testing.T, service *Service, room string) (*crdt.Doc, func()) {
	t.Helper()
	inside, leave, left := make(chan *crdt.Doc, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		err := service.srv.Apply(context.Background(), room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			inside <- doc
			<-leave
		})
		if errors.Is(err, websocket.ErrNoChanges) {
			err = nil
		}
		left <- err
	}()
	var live *crdt.Doc
	select {
	case live = <-inside:
	case err := <-left:
		t.Fatalf("load the room of document %s: %v", room, err)
	case <-time.After(roomWait(t)):
		t.Fatalf("the room of document %s did not load within %v", room, roomWait(t))
	}
	held, release := make(chan struct{}), make(chan struct{})
	var holding sync.Once
	service.roomCloseHeld = func(closing string) {
		if closing == room {
			holding.Do(func() { close(held) })
			<-release
		}
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		service.closeRoomOverBudget(room)
	}()
	select {
	case <-held:
	case <-closed:
		close(leave)
		t.Fatalf("the budget's close of %s returned without holding the room", room)
	case <-time.After(roomWait(t)):
		close(leave)
		t.Fatalf("the budget's close of %s did not hold the room within %v", room, roomWait(t))
	}
	close(leave)
	if err := <-left; err != nil {
		t.Fatalf("the test's own Apply on %s: %v", room, err)
	}
	return live, func() {
		t.Helper()
		close(release)
		select {
		case <-closed:
		case <-time.After(roomWait(t)):
			t.Fatalf("the budget's close of %s did not return within %v once let go", room, roomWait(t))
		}
		service.roomCloseHeld = nil
	}
}
