package docs

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A load of an issue room that read the room's layout before a join's transaction committed is
// published only after that commit has looked for a resident instance to close: ygo answers no
// room for a room still loading (GetDoc), so the commit's close (closeJoinedRooms) finds none,
// closes nothing, and the commit answers its writer. The room is then resident over the layout
// without the joined document. The settlement whose warm-up made that load finds the layout stale
// under its lock and reserves the room's close (errRoomLayoutMoved, reloadReplacedRoom), and every
// write that comes while that close runs is refused (errRoomReloading): the writer's next write
// is refused, which TestAJoinsCommitWaitsOutACloseReservedOnItsRoom says must not happen.
//
// The race is held open with the service's own hooks, not hoped for: the warm-up load is held
// right after it read the room's layout (admissionRendered, which onLoadDocument runs after its
// issue read and before ygo publishes the room) while the join commits, and a close the
// settlement reserves is held (afterRoomCloseReserved) while the writer writes. No catch-up moves
// the room's generation in between: this service registers no room sync, so no follower runs.
func TestAJoinsCommitClosesAnIssueRoomLoadedAcrossIt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	room := createIssueRoom(t, database, 1)
	if service.srv.GetDoc(room.tree) != nil {
		t.Fatal("the issue room is resident before any load")
	}

	loading, letLoad := make(chan struct{}), make(chan struct{})
	var loadHeld atomic.Bool
	service.admissionRendered = func(loaded string) {
		if loaded != room.tree || loadHeld.Swap(true) {
			return
		}
		close(loading)
		<-letLoad
	}
	reserved, letClose := make(chan struct{}), make(chan struct{})
	var reservedOnce sync.Once
	service.afterRoomCloseReserved = func(closing string) {
		if closing != room.tree {
			return
		}
		reservedOnce.Do(func() { close(reserved) })
		<-letClose
	}
	releaseLoad := sync.OnceFunc(func() { close(letLoad) })
	releaseClose := sync.OnceFunc(func() { close(letClose) })
	t.Cleanup(releaseLoad)
	t.Cleanup(releaseClose)

	// The room's settlement, as its timer runs it: the room is not resident, so its warm-up loads it.
	state := service.lockState(room.tree)
	generation := state.roomGeneration
	service.unlockState(room.tree, state)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(room.tree, generation)
	}()
	select {
	case <-loading:
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement's warm-up never loaded the issue room")
	}

	// The join commits while that load holds the layout it read before the join. A commit that
	// waits for the load in flight is not held for it: the load goes on after a moment either way.
	joined, ledger := seedJoinedDocument(t, service, room, "notes", "Joined words.\n")
	committed := make(chan error, 1)
	go func() { committed <- ledger.Commit(ctx) }()
	var commitErr error
	answered := false
	select {
	case commitErr = <-committed:
		answered = true
	case <-time.After(200 * time.Millisecond):
	}
	releaseLoad()
	if !answered {
		select {
		case commitErr = <-committed:
		case <-reserved:
			// A close reserved before the commit answered is one the commit waits out
			// (awaitReservedClose), so it is let go here and the commit's answer awaited.
			releaseClose()
			select {
			case commitErr = <-committed:
			case <-time.After(roomWait(t)):
				t.Fatal("the join's commit did not answer once the close reserved on its room returned")
			}
		case <-time.After(roomWait(t)):
			t.Fatal("the join's commit never answered")
		}
	}
	if commitErr != nil {
		t.Fatalf("commit the join: %v", commitErr)
	}

	// The commit has answered its writer. The settlement goes on until it reserves a close or ends,
	// and the writer's next write comes then, while a reserved close is held.
	select {
	case <-reserved:
	case <-settled:
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement neither reserved a close nor ended")
	}
	_, writeErr := joinedReplaceText(service, joined, "Joined words, written again.\n", model.Actor{Kind: "user", ID: "alice"})
	releaseClose()
	select {
	case <-settled:
	case <-time.After(roomWait(t)):
		t.Fatal("the settlement did not end once its reserved close was let go")
	}
	if writeErr != nil {
		t.Fatalf("the writer's next write after the join's commit: %v", writeErr)
	}
}

// A document socket's admission reads the issue with its room's layout, records that read on the
// room's record and holds the record (holdForAdmission), and the room's load takes that read
// rather than reading the issue again (admittedIssueRead). An admission whose read began before a
// join's transaction committed holds a layout without the joined document, and a load that begins
// after the commit looked for a load under way (awaitRoomLoad) is no load the commit waits for or
// closes. Were it to take that read, it would publish the room over the layout from before the
// join, the room's settlement would find that layout stale and reserve the room's close
// (errRoomLayoutMoved, reloadReplacedRoom), and the writer's next write would be refused
// (errRoomReloading). So a load that begins after the commit reads the layout itself, and the room
// it publishes holds the joined document. That holds whether the admission recorded its read before
// the commit or after it: ServeHTTP reads the issue some statements before it records the read.
//
// The load is the room's settlement's warm-up: whatever loads the room takes the read an admission
// holds on its record. The room's layout is read the moment the warm-up has published it
// (afterSettleWarm), and a close the settlement reserves is held (afterRoomCloseReserved) while the
// writer writes, as in TestAJoinsCommitClosesAnIssueRoomLoadedAcrossIt.
func TestAnIssueRoomLoadedAfterAJoinOnAnAdmissionsEarlierReadHoldsTheJoinedDocument(t *testing.T) {
	for _, order := range []struct {
		name          string
		holdsBeforeIt bool
	}{
		{name: "the admission records its read before the join commits", holdsBeforeIt: true},
		{name: "the admission records its read after the join commits", holdsBeforeIt: false},
	} {
		t.Run(order.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			service, _ := newTestServiceInstance(t, database)
			room := createIssueRoom(t, database, 1)

			var published atomic.Pointer[roomLayout]
			service.afterSettleWarm = func(warmed string) {
				if warmed != room.tree {
					return
				}
				if live := service.srv.GetDoc(room.tree); live != nil {
					layout := service.layoutOf(live)
					published.CompareAndSwap(nil, &layout)
				}
			}
			reserved, letClose := make(chan struct{}), make(chan struct{})
			var reservedOnce sync.Once
			service.afterRoomCloseReserved = func(closing string) {
				if closing != room.tree {
					return
				}
				reservedOnce.Do(func() { close(reserved) })
				<-letClose
			}
			releaseClose := sync.OnceFunc(func() { close(letClose) })
			t.Cleanup(releaseClose)

			// The admission's read of the issue, as ServeHTTP makes it, before the join commits.
			read, err := service.readIssue(ctx, service.queryFrom(ctx), room.tree)
			if err != nil {
				t.Fatalf("read the issue as a socket's admission reads it: %v", err)
			}
			hold := func() {
				held := service.holdForAdmission(room.tree, read)
				t.Cleanup(func() { service.releaseAdmission(room.tree, held) })
			}
			if order.holdsBeforeIt {
				hold()
			}
			joined, ledger := seedJoinedDocument(t, service, room, "notes", "Joined words.\n")
			if slices.Contains(read.layout, joined) {
				t.Fatalf("the admission's read %v already holds the joined document, so this test would say nothing", read.layout)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the join: %v", err)
			}
			if !order.holdsBeforeIt {
				hold()
			}
			if service.srv.GetDoc(room.tree) != nil {
				t.Fatal("the issue room is resident before its load, so this test would say nothing")
			}

			// The room's settlement, as its timer runs it: the room is not resident, so its warm-up
			// loads it, after the join's commit.
			state := service.lockState(room.tree)
			generation := state.roomGeneration
			service.unlockState(room.tree, state)
			settled := make(chan struct{})
			go func() {
				defer close(settled)
				service.settleRoom(room.tree, generation)
			}()
			select {
			case <-reserved:
			case <-settled:
			case <-time.After(roomWait(t)):
				t.Fatal("the settlement neither reserved a close nor ended")
			}
			_, writeErr := joinedReplaceText(service, joined, "Joined words, written again.\n", model.Actor{Kind: "user", ID: "alice"})
			releaseClose()
			select {
			case <-settled:
			case <-time.After(roomWait(t)):
				t.Fatal("the settlement did not end once its reserved close was let go")
			}
			if writeErr != nil {
				t.Errorf("the writer's next write after the join's commit: %v", writeErr)
			}
			switch layout := published.Load(); {
			case layout == nil:
				t.Error("the settlement's warm-up published no room of the issue")
			case !slices.Contains(*layout, joined):
				t.Errorf("the room loaded after the join's commit was published over %v, without the joined document %s", *layout, joined)
			}
		})
	}
}
