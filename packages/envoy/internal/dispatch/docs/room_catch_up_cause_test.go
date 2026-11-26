package docs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows here drive the cause a catch-up carries (catchUpCause), for which the test hooks record
// the head the catch-up leaves its room at (RoomCounters.NotifiedHead, SweptHead), through each
// way a catch-up is asked again: in place of a close the locked read does not confirm, as a retry,
// as a look again for a room still loading, as the catch-up that answers for a room whose retry
// waits, and as the catch-up a loading room's instance schedules after one that found no room -
// but not as the catch-up of a load that follows one that failed. cmd/dispatch's
// awaitNotifiedCatchUp waits for those heads. The window several of them meet is the one
// TestACatchUpInsideThisProcessesOwnCommitWindowClosesNothing (room_catch_up_test.go) holds open:
// this process's own write has recorded its version on the room's cursor and has not committed, so
// a catch-up reads a replaced history and confirms it under the document's lock
// (historyReplacedUnderLock).

// A catch-up another process's notification asked for meets the same window: this process's own
// write has recorded its version on the cursor and not committed. The catch-up is asked again in
// place of the close the locked read does not confirm, and keeps the notification's cause, so it
// records the head the room then holds (RoomCounters.NotifiedHead). Asked again as this process's
// own catch-up, it records none, and a wait for that head (cmd/dispatch's awaitNotifiedCatchUp)
// gives up although the room holds every change.
func TestANotifiedCatchUpThatMeetsThisProcessesOwnWriteRecordsTheHead(t *testing.T) {
	service, database, artifactID, room := newHookedResidentRoom(t)
	notes := runCatchUps(t, service)
	tx, version := holdOwnWrite(t, service, database, artifactID)

	// Another process's notification: a bare announcement, as an old build's write sends.
	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, documentChangedChannel, artifactID); err != nil {
		t.Fatalf("announce another process's change: %v", err)
	}
	waitNote(t, notes.reads, artifactID, "the notified catch-up's read")
	awaitVerdictUnderLock(t, database, "the notified catch-up")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	requireRoomKeptOnceAskedAgain(t, service, notes.reads, artifactID, room)
	if head := requireRoomCounters(t, service, artifactID).NotifiedHead; head != int64(version) {
		t.Fatalf("the notified head is %d once the catch-up asked again in place of the close has run, want %d, the head the room holds", head, version)
	}
}

// The sweep after a LISTEN stands meets the same window when room sync starts, or the listener
// redials, while this process's own write is in flight. The catch-up asked again in place of the
// close keeps the sweep's cause, so it records the head the room then holds
// (RoomCounters.SweptHead): the head cmd/dispatch's awaitNotifiedCatchUp takes, after a redial,
// as the carrier of a write whose notification reached no listener of the process. Asked again
// as this process's own catch-up, it records none.
func TestASweptCatchUpThatMeetsThisProcessesOwnWriteRecordsTheHead(t *testing.T) {
	service, database, artifactID, room := newHookedResidentRoom(t)
	tx, version := holdOwnWrite(t, service, database, artifactID)

	// Room sync starts with the write in its window, so its first LISTEN's sweep meets it.
	notes := watchCatchUps(t, service)
	listening, _ := startRoomSync(t, newListenerHub(service), newRoomSync(service))
	waitNote(t, notes.reads, artifactID, "the first LISTEN's sweep's read")
	awaitVerdictUnderLock(t, database, "the sweep's catch-up")
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	testwait.Receive(t, "the first LISTEN's sweep to end", listening)
	requireRoomKeptOnceAskedAgain(t, service, notes.reads, artifactID, room)
	counters := requireRoomCounters(t, service, artifactID)
	if counters.SweptHead != int64(version) || counters.NotifiedHead != 0 {
		t.Fatalf("the swept head is %d and the notified head %d once the catch-up asked again in place of the close has run, want %d, the head the room holds, and 0: no notification asked for it",
			counters.SweptHead, counters.NotifiedHead, version)
	}
}

// A catch-up a notification or a sweep asked for whose locked read fails - here its context ends
// while the read waits for this process's own write - is tried again (armRetry), as is one whose
// close is held off or fails, and the retry keeps its cause: once the write has committed, the
// retry finds the room at the head and records the head as the catch-up it retries would have
// (RoomCounters.NotifiedHead, SweptHead).
func TestARetryOfANotifiedOrSweptCatchUpRecordsTheHead(t *testing.T) {
	for _, asked := range []struct {
		name  string
		cause catchUpCause
		head  func(RoomCounters) int64
	}{
		{"by a notification", catchUpNotified, func(counters RoomCounters) int64 { return counters.NotifiedHead }},
		{"by a sweep", catchUpSwept, func(counters RoomCounters) int64 { return counters.SweptHead }},
	} {
		t.Run(asked.name, func(t *testing.T) {
			service, database, artifactID, room := newHookedResidentRoom(t)
			catchUps := service.catchUps
			now := time.Now()
			catchUps.now = func() time.Time { return now }
			tx, version := holdOwnWrite(t, service, database, artifactID)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			caught := make(chan struct{})
			go func() {
				defer close(caught)
				catchUps.catchUp(ctx, artifactID, asked.cause)
			}()
			awaitVerdictUnderLock(t, database, "the catch-up")
			cancel()
			testwait.Receive(t, "the catch-up to return once its context ended under the locked read", caught)
			wake := catchUps.wakeIn()
			if wake <= 0 {
				t.Fatalf("the catch-up whose locked read failed armed no retry (wakeIn = %s)", wake)
			}
			if err := tx.Commit(context.Background()); err != nil {
				t.Fatalf("commit the write: %v", err)
			}

			now = now.Add(wake)
			catchUps.woke(context.Background())
			if service.srv.GetDoc(artifactID) != room {
				t.Fatal("the retry closed the room this process was writing")
			}
			if head := asked.head(requireRoomCounters(t, service, artifactID)); head != int64(version) {
				t.Fatalf("the head recorded once the retry has run is %d, want %d, the head the room holds", head, version)
			}
			if wake := catchUps.wakeIn(); wake != 0 {
				t.Fatalf("the retry that found the room at the head left another armed (wakeIn = %s)", wake)
			}
		})
	}
}

// A catch-up another process's notification asked for, of a room whose instance scheduled its own
// catch-up while ygo was still loading it, is looked for again until ygo publishes the room
// (takeLoading), and the look again keeps the notification's cause: once it finds the room
// published, it records the head the room holds (RoomCounters.NotifiedHead). Looked for again as
// this process's own catch-up, it records none, and a wait for that head (cmd/dispatch's
// awaitNotifiedCatchUp) gives up although the room holds every change. The lane is held while the
// notification queues and the load starts the room's instance, so the notification is decided
// while the instance's own catch-up waits on the lane and merges into it (request); the load is
// held past its hook, so that catch-up finds the room still loading. The catch-ups' clock is a
// fake the test moves only once the room is published, so no real moment brings the look again
// or ends it (catchUpLoadingTimeout).
func TestANotifiedCatchUpOfARoomStillLoadingRecordsTheHeadOnceTheRoomIsPublished(t *testing.T) {
	service, database, artifactID := newHookedService(t)
	fake := clocktest.New(time.Now())
	catchUps := service.catchUps
	catchUps.now = fake.Now
	// The room's first load is held once its hook has started the instance, which schedules the
	// instance's catch-up (keepInstance), and before ygo publishes the room. Registered after the
	// service's shutdown, the cleanup that lets it go runs first.
	load := service.srv.OnLoadDocument
	reached := make(chan struct{})
	release := make(chan struct{})
	letLoad := sync.OnceFunc(func() { close(release) })
	testcleanup.Add(t, letLoad)
	var holding atomic.Bool
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		if room == artifactID && holding.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
		return nil
	}
	notes := runCatchUps(t, service)
	catchUps.mu.Lock()
	lane := catchUps.lane
	catchUps.mu.Unlock()
	held := make(chan struct{})
	releaseLane := sync.OnceFunc(func() { close(held) })
	testcleanup.Add(t, releaseLane)
	laneHeld := make(chan struct{})
	lane.push(func(context.Context) {
		close(laneHeld)
		<-held
	})
	testwait.Receive(t, "the lane to run the task that holds it", laneHeld)

	// Another process's notification: a bare announcement, as an old build's write sends.
	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, documentChangedChannel, artifactID); err != nil {
		t.Fatalf("announce another process's change: %v", err)
	}
	waitFor(t, "the notification to queue on the held lane", func() bool {
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return len(lane.queue) >= 1
	})
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	testwait.Receive(t, "the room's load to start the room's instance and reach the end of its hook", reached)
	releaseLane()
	waitNote(t, notes.decided, artifactID, "the notification's decision")
	awaitCatchUpLane(t, service)
	catchUps.mu.Lock()
	_, loading := catchUps.loading[artifactID]
	_, lookingAgain := catchUps.deferred[artifactID]
	catchUps.mu.Unlock()
	if published := service.srv.GetDoc(artifactID) != nil; !loading || !lookingAgain || published {
		t.Fatalf("once the notified catch-up ran, the room is loading %t, looked for again %t and published %t, want loading, looked for again and not published: nothing here is tested",
			loading, lookingAgain, published)
	}

	letLoad()
	if err := testwait.Receive(t, "the room's load to return once its hook was let go", loaded); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	fake.Advance(catchUpLoadingWait)
	waitNote(t, notes.reads, artifactID, "the look again's read once the room was published")
	awaitCatchUpLane(t, service)
	head := storedHead(t, database, artifactID)
	if notified := requireRoomCounters(t, service, artifactID).NotifiedHead; notified != head {
		t.Fatalf("the notified head is %d once the look again found the room published, want %d, the stored head the room holds", notified, head)
	}
}

// heldLoad is a service of its own with the test hooks on and room sync running, whose room's
// first load is held at the start of its hook - ygo holds the room in its table and hands out no
// document of it, and the hook has not started the room's instance (keepInstance) - while another
// process's notification for the room is decided and its catch-up runs
// (holdLoadPastANotification). The catch-ups' clock is a fake, which the test moves only once a
// room is published, so no real moment brings a look again or ends it.
type heldLoad struct {
	service    *Service
	database   *store.Store
	artifactID string
	fake       *clocktest.Fake
	notes      catchUpNotes
	// let lets the held load go, and loaded is what the call that loads the room returns.
	let    func()
	loaded <-chan error
}

// holdLoadPastANotification holds a room's load at the start of its hook, sends a bare
// announcement for the room, as an old build's write sends, and returns once that notification's
// catch-up has run. It fails unless that catch-up found the room held, not published and with no
// instance's catch-up scheduled, and read nothing: the window the rows that call it drive.
func holdLoadPastANotification(t *testing.T) heldLoad {
	t.Helper()
	service, database, artifactID := newHookedService(t)
	fake := clocktest.New(time.Now())
	catchUps := service.catchUps
	catchUps.now = fake.Now
	// Registered after the service's shutdown, the cleanup that lets the load go runs first.
	load := service.srv.OnLoadDocument
	reached := make(chan struct{})
	release := make(chan struct{})
	letLoad := sync.OnceFunc(func() { close(release) })
	testcleanup.Add(t, letLoad)
	var holding atomic.Bool
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if room == artifactID && holding.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
		return load(ctx, room, doc)
	}
	notes := runCatchUps(t, service)
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	testwait.Receive(t, "the room's load to reach its hook", reached)

	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, documentChangedChannel, artifactID); err != nil {
		t.Fatalf("announce another process's change: %v", err)
	}
	waitNote(t, notes.decided, artifactID, "the notification's decision")
	awaitCatchUpLane(t, service)
	catchUps.mu.Lock()
	_, scheduled := catchUps.loading[artifactID]
	catchUps.mu.Unlock()
	published, held, reads := service.srv.GetDoc(artifactID) != nil, service.roomHeld(artifactID), countNotes(notes.reads, artifactID)
	if scheduled || published || !held || reads != 0 {
		t.Fatalf("once the notified catch-up ran, the room's instance had scheduled its catch-up %t, the room is published %t and held %t, and the catch-up read %d time(s), want no catch-up scheduled, a room held and not published, and no read: nothing here is tested",
			scheduled, published, held, reads)
	}
	return heldLoad{service: service, database: database, artifactID: artifactID, fake: fake, notes: notes, let: letLoad, loaded: loaded}
}

// A catch-up another process's notification asked for that finds no room while ygo is loading
// one, before the load has started the room's instance (keepInstance), leaves its cause to the
// catch-up that instance schedules (keepForLoadingRoom), which records the head the room holds
// once it finds the room published (RoomCounters.NotifiedHead). Leaving none, the cause is lost
// with the catch-up that found no room, the instance's own catch-up records no head, and a wait
// for that head (cmd/dispatch's awaitNotifiedCatchUp) gives up although the room holds every
// change. The load is held at the start of its hook while the notification is decided
// (holdLoadPastANotification).
func TestANotifiedCatchUpBeforeALoadingRoomsInstanceStartsRecordsTheHead(t *testing.T) {
	held := holdLoadPastANotification(t)
	held.let()
	if err := testwait.Receive(t, "the room's load to return once its hook was let go", held.loaded); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	// The instance's own catch-up, where it found the room still loading, looks again once the
	// fake has passed catchUpLoadingWait.
	held.fake.Advance(catchUpLoadingWait)
	waitForCatchUpsIdle(t, held.service, held.artifactID)
	awaitCatchUpLane(t, held.service)
	head := storedHead(t, held.database, held.artifactID)
	if notified := requireRoomCounters(t, held.service, held.artifactID).NotifiedHead; notified != head {
		t.Fatalf("the notified head is %d once the catch-up the room's instance scheduled has run, want %d, the stored head the room holds", notified, head)
	}
}

// A load that ends without starting the room's instance takes with it the causes a catch-up that
// found no room left it (keepForLoadingRoom, takeKeptForLoad). The catch-up the next load's
// instance schedules is this process's own, and no catch-up the notification asked for carried
// the change it finds, which the next load read from the store itself, so it records no notified
// head (RoomCounters.NotifiedHead). Handed the cause, it would record one, and a wait for that
// head (cmd/dispatch's awaitNotifiedCatchUp) would pass on a head no notified catch-up carried.
// The held load is refused as a rebuild refuses it (refuseIfRebuilding): the load's own failure
// path drops the record it began (onLoadDocument), and ygo drops the room.
func TestACauseLeftToALoadThatFailsIsNotCreditedToTheNextLoad(t *testing.T) {
	held := holdLoadPastANotification(t)
	service, artifactID, hooks := held.service, held.artifactID, held.service.hooks
	hooks.mu.Lock()
	kept := hooks.forLoads[artifactID].cause
	hooks.mu.Unlock()
	if kept != catchUpNotified {
		t.Fatalf("the notified catch-up kept causes %b for the room's load, want the notification's, %b: nothing here is tested", kept, catchUpNotified)
	}

	service.rebuilding.Store(artifactID, struct{}{})
	held.let()
	if err := testwait.Receive(t, "the refused load to return once its hook was let go", held.loaded); err == nil {
		t.Fatal("the load a rebuild refuses loaded the room: nothing here is tested")
	}
	service.rebuilding.Delete(artifactID)
	if service.roomHeld(artifactID) {
		t.Fatal("ygo still holds the room whose load was refused: nothing here is tested")
	}

	// The next load starts the room's instance, which schedules this process's own catch-up.
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room again: %v", err)
	}
	held.fake.Advance(catchUpLoadingWait)
	waitForCatchUpsIdle(t, service, artifactID)
	awaitCatchUpLane(t, service)
	if reads := countNotes(held.notes.reads, artifactID); reads == 0 {
		t.Fatal("the next load's own catch-up read nothing: nothing here is tested")
	}
	if notified := requireRoomCounters(t, service, artifactID).NotifiedHead; notified != 0 {
		t.Fatalf("the notified head is %d once the next load's own catch-up ran, want 0: the notification's catch-up left its cause to the load that failed, and no catch-up it asked for carried the change", notified)
	}
}

// A notified catch-up that reached no verdict leaves a retry of the room armed that keeps the
// notification's cause (armRetry). A sweep's catch-up that answers for the room before that retry
// runs ends it (settled), and the retry never runs, so the sweep's catch-up takes on its cause:
// it records the head the room then holds as the notified head as well as the swept one.
// Answering with its own cause alone, it records no notified head, and a wait for that head
// (cmd/dispatch's awaitNotifiedCatchUp) gives up although the room holds every change. The sweep's
// catch-up answers for the room where it finds the room at the head, and where it meets this
// process's own write in flight and the locked read does not confirm the close (closeReplaced):
// the catch-up asked again in its place carries the retry's cause on. The notified catch-up's read
// fails as a pool refuses a second connection under a transaction (store.ErrNestedAcquire), and
// the catch-ups' clock stands still, so its retry never comes due.
func TestANotifiedRetryAnsweredByASweptCatchUpRecordsBothHeads(t *testing.T) {
	armNotifiedRetry := func(t *testing.T, service *Service, artifactID string) {
		t.Helper()
		catchUps := service.catchUps
		now := time.Now()
		catchUps.now = func() time.Time { return now }
		blind, release := store.HoldsConnection(context.Background())
		defer release()
		catchUps.catchUp(blind, artifactID, catchUpNotified)
		if wake := catchUps.wakeIn(); wake <= 0 {
			t.Fatalf("the notified catch-up whose read failed armed no retry (wakeIn = %s)", wake)
		}
	}
	requireBothHeads := func(t *testing.T, service *Service, artifactID string, head int64) {
		t.Helper()
		counters := requireRoomCounters(t, service, artifactID)
		if counters.NotifiedHead != head || counters.SweptHead != head {
			t.Fatalf("the notified head is %d and the swept head %d once the sweep's catch-up answered for the room, want %d for both, the head the room holds",
				counters.NotifiedHead, counters.SweptHead, head)
		}
		if wake := service.catchUps.wakeIn(); wake != 0 {
			t.Fatalf("the sweep's catch-up that answered for the room left the notified retry armed (wakeIn = %s)", wake)
		}
	}

	t.Run("found at the head", func(t *testing.T) {
		service, database, artifactID, room := newHookedResidentRoom(t)
		armNotifiedRetry(t, service, artifactID)

		service.catchUps.sweep(context.Background())
		if service.srv.GetDoc(artifactID) != room {
			t.Fatal("the sweep closed the room")
		}
		requireBothHeads(t, service, artifactID, storedHead(t, database, artifactID))
	})

	t.Run("asked again in place of the close", func(t *testing.T) {
		service, database, artifactID, room := newHookedResidentRoom(t)
		armNotifiedRetry(t, service, artifactID)
		tx, version := holdOwnWrite(t, service, database, artifactID)

		// Room sync starts with the write in its window, so its first LISTEN's sweep meets it.
		notes := watchCatchUps(t, service)
		listening, _ := startRoomSync(t, newListenerHub(service), newRoomSync(service))
		waitNote(t, notes.reads, artifactID, "the first LISTEN's sweep's read")
		awaitVerdictUnderLock(t, database, "the sweep's catch-up")
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatalf("commit the write: %v", err)
		}
		testwait.Receive(t, "the first LISTEN's sweep to end", listening)
		requireRoomKeptOnceAskedAgain(t, service, notes.reads, artifactID, room)
		requireBothHeads(t, service, artifactID, int64(version))
	})
}

// newHookedResidentRoom is a service of its own with the test hooks on, and its resident room of a
// document of its own whose appends have all landed: what the catch-up rows above drive. Room
// sync is not started.
func newHookedResidentRoom(t *testing.T) (*Service, *store.Store, string, *crdt.Doc) {
	t.Helper()
	service, database, artifactID := newHookedService(t)
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	return service, database, artifactID, service.srv.GetDoc(artifactID)
}

// newHookedService is a service of its own, on a database of its own, with the test hooks on, and
// a document of its own whose text the service has seeded; no room of it is loaded yet.
func newHookedService(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	service.hooks = newTestHooks()
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	return service, database, artifactID
}

// requireRoomKeptOnceAskedAgain waits for the read of the catch-up asked again in place of the
// close the locked read did not confirm (on reads, a catchUpNotes' own) and for the catch-up lane
// to drain (awaitCatchUpLane), and fails t unless artifactID's room is still room: the catch-up
// closed nothing this process was writing.
func requireRoomKeptOnceAskedAgain(t *testing.T, service *Service, reads chan string, artifactID string, room *crdt.Doc) {
	t.Helper()
	waitNote(t, reads, artifactID, "the catch-up asked again once the locked read found the room's own history")
	awaitCatchUpLane(t, service)
	if service.srv.GetDoc(artifactID) != room {
		t.Fatal("the catch-up closed the room this process was writing")
	}
}

// holdOwnWrite holds a write of service's own to artifactID in the window between its cursor
// advance and its commit, as a live write holds it (Ledger.takeRoomCursors): the document's lock
// taken in the write's transaction, an update appended there by the service's own store - so its
// commit announces it as this process's own, which headNotified drops with its twin - and its
// version recorded on the room's cursor. The caller commits the transaction to end the window;
// the test's end rolls back one it did not.
func holdOwnWrite(t *testing.T, service *Service, database *store.Store, artifactID string) (pgx.Tx, persistence.Version) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
	version, err := service.persistence.(*PgVersioned).AppendUpdateTx(ctx, tx, artifactID, paragraphUpdate(), true)
	if err != nil {
		t.Fatalf("append the write's update: %v", err)
	}
	advanceRoomCursor(service.cursorOf(service.srv.GetDoc(artifactID)), version)
	return tx, version
}
