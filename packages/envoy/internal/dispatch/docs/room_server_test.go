package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A room's close gate leaves with the last close or repair that held it: the server keeps none
// for a room nothing is closing or repairing, however many documents were repaired, evicted, or
// closed with their issue without ever loading.
func TestCloseGatesDoNotOutliveTheirRooms(t *testing.T) {
	service, first := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	generation := owedStamp(t, service, first)
	service.settleRoom(first, generation) // a repair (applySuppressed)
	if err := service.srv.CloseRoom(first, true); err != nil {
		t.Fatalf("close the settled room: %v", err)
	}
	var evicted []string
	for number := 2; number <= 26; number++ { // rooms the service evicts
		room := createIssueDocument(t, service.store, number, "before")
		seedServiceText(t, service, room, "before")
		liveTree(t, service, room)
		if service.srv.GetDoc(room) == nil {
			t.Fatalf("document %d is not resident after its live read", number)
		}
		if err := service.Evict(ctx, room); err != nil {
			t.Fatalf("evict document %d: %v", number, err)
		}
		evicted = append(evicted, room)
	}
	for number := 27; number <= 51; number++ { // closed issues, documents never loaded
		createIssueDocument(t, service.store, number, "before")
		setIssueClosedHere(t, service, fmt.Sprintf("DOC-%d", number), true)
	}
	live := 0
	for _, room := range append([]string{first}, evicted...) {
		if service.srv.GetDoc(room) != nil {
			live++
		}
	}
	if gates := closeGates(service); gates > live {
		t.Fatalf("%d close gates kept for %d live rooms after 51 documents were repaired, evicted or closed", gates, live)
	}
}

// A repair that meets a close of its room under way writes nothing and returns rather than waiting
// for the close: the close waits for repairs already committing, and a settlement writing a repair
// holds the document's lock, which a failed room's close compacts under (roomServer).
func TestARepairRefusesRatherThanWaitsForACloseUnderWay(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	generation := owedStamp(t, service, artifactID)

	closing := service.srv.acquireGate(artifactID)
	closing.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			closing.Unlock()
			service.srv.releaseGate(artifactID, closing)
		}
	}
	t.Cleanup(release)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	// The gate stays held until the settlement returns; the cleanup lets it go if it never does.
	testwait.Receive(t, "the settlement to refuse its repair rather than wait for the close under way", settled)
	release()
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the refused repair stamped the room")
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the refused repair")
	requireNextSettlementStamps(t, service, artifactID, "before\n\nadded\n")
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept once nothing closes or repairs the room", gates)
	}
}

// A repair holds the exact document Server.Apply found. Once its room has retired that document,
// holdOpen refuses and releases the gate it acquired for the check.
func TestHoldOpenRefusesRoomThatNoLongerHoldsItsDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		t.Fatal("document room is not resident")
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close document room: %v", err)
	}
	if release, open := service.srv.holdOpen(artifactID, doc); open {
		release()
		t.Fatal("a repair was held open on a document its room no longer holds")
	}
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept after the refusal", gates)
	}
}

// A close whose caller stopped waiting must not still take a free room. Evict receives its
// context from an API caller, and a cancellation before the close begins leaves the resident
// editor connected for the next request rather than reporting a close that happened after its
// caller gave up.
func TestEvictWithACancelledContextLeavesTheRoomResident(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load document room: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Evict(ctx, artifactID); !errors.Is(err, context.Canceled) {
		t.Fatalf("evict with a cancelled context = %v, want %v", err, context.Canceled)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("evict with a cancelled context closed the resident room")
	}
}

func TestACancelledEvictionLeavesTheFailedRoomForItsOwnEviction(t *testing.T) {
	server := websocket.NewServer()
	// Only to keep ygo's idle sweep away from the room the test holds open: that sweep reads the
	// wall clock inside ygo (provider/websocket/idle_sweep.go), so no clock of the test's reaches it.
	server.RoomIdleTimeout = time.Hour
	service := &Service{srv: newRoomServer(server)}
	entered, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	evicting, evict := make(chan struct{}), make(chan struct{})
	var released, evicted sync.Once
	letGo := func() { released.Do(func() { close(release) }) }
	recoverRoom := func() { evicted.Do(func() { close(evict) }) }
	service.beforeFailedEviction = func(string) {
		close(evicting)
		<-evict
	}
	t.Cleanup(func() {
		letGo()
		recoverRoom()
		<-applied
		service.evictWG.Wait()
		if err := server.Shutdown(testwait.Context(t)); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	go func() {
		defer close(applied)
		if err := server.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {
			close(entered)
			<-release
		}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Errorf("hold the room open: %v", err)
		}
	}()
	<-entered
	service.failRoom("room", errors.New("failed room"))
	<-evicting
	state := service.lockExistingState("room")
	done := state.failedDone
	service.unlockState("room", state)

	// The eviction's context ends on a fake clock, which the test moves past its deadline once
	// the eviction has tried the room the Apply above is inside and been refused.
	var attempts atomic.Int32
	service.srv.beforeClose = func(room string) {
		if room == "room" {
			attempts.Add(1)
		}
	}
	evictClock := clocktest.New(time.Now())
	ctx, cancel := evictClock.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	evictReturned := make(chan error, 1)
	go func() { evictReturned <- service.Evict(ctx, "room") }()
	waitFor(t, "the eviction to try the room the Apply is inside", func() bool { return attempts.Load() >= 1 })
	evictClock.Advance(10 * time.Millisecond)
	if err := testwait.Receive(t, "the eviction to return once its context ended", evictReturned); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled eviction = %v, want %v", err, context.DeadlineExceeded)
	}
	if !service.roomFailed("room") || server.GetDoc("room") == nil {
		t.Fatal("cancelled eviction must keep the resident room's failure until its own eviction closes it")
	}
	letGo()
	<-applied
	recoverRoom()
	testwait.Receive(t, "the failed room's eviction to finish", done)
	if server.GetDoc("room") != nil || service.roomFailed("room") {
		t.Fatal("the failed room's eviction must close the room and clear its failure")
	}
}

func TestAnEvictionHandsOverFailureOnlyAfterTheRoomCloses(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		closed bool
	}{
		{"closed", nil, true},
		{"already gone", websocket.ErrRoomNotFound, true},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"still in use", websocket.ErrRoomInUse, false},
		{"shutdown", websocket.ErrServerShutdown, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{srv: newRoomServer(websocket.NewServer())}
			state := newRoomState()
			state.failed = errors.New("failed room")
			service.rooms.Store("room", state)
			var closed bool
			service.afterRoomClosed = func(string) { closed = true }
			err := service.forgetEvicted("room", state, test.err)
			if test.closed && err != nil || !test.closed && !errors.Is(err, test.err) {
				t.Fatalf("eviction error = %v, close error = %v", err, test.err)
			}
			if state.released != test.closed || closed != test.closed || service.roomFailed("room") == test.closed {
				t.Fatalf("released=%v afterRoomClosed=%v failure=%v, want closed=%v", state.released, closed, service.roomFailed("room"), test.closed)
			}
		})
	}
}

// closeGates counts the close gates service's server holds.
func closeGates(service *Service) int {
	service.srv.gatesMu.Lock()
	defer service.srv.gatesMu.Unlock()
	return len(service.srv.gates)
}
