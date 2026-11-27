package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Awareness - readers' cursors and presence - crosses processes through the awareness relay
// (awareness_relay.go): each process sends its own rooms' awareness on documentAwarenessChannel and
// applies the others' to a room of its own that a reader of its own is in. The tests here run two
// services on one database, as production's two tasks are, each with its own document socket and
// its own room sync.

// The awareness client ids the readers below announce, as a Yjs provider announces its
// document's client id.
const (
	aliceClient uint64 = 1001
	bobClient   uint64 = 2002
)

// runAwareness runs service's room sync, as cmd/dispatch does, and returns a channel that receives
// each awareness notification once the service's relay has handled it.
func runAwareness(t *testing.T, service *Service) <-chan awarenessMessage {
	t.Helper()
	rs := newRoomSync(service)
	relay := rs.awareness
	handled := make(chan awarenessMessage, 256)
	rs.awareness = func(ctx context.Context, payload string) {
		relay(ctx, payload)
		var message awarenessMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			t.Errorf("an awareness notification does not parse: %v: %s", err, payload)
			return
		}
		select {
		case handled <- message:
		default:
			t.Errorf("the test read no note of an awareness notification for %s", message.Room)
		}
	}
	runRoomSync(t, rs)
	return handled
}

// cursorOf is a reader's awareness state as the dashboard announces it.
func cursorOf(login string, at int) map[string]any {
	return map[string]any{"user": map[string]any{"name": login}, "cursor": map[string]any{"anchor": at, "head": at}}
}

// holdsAwareness is a WaitForAwareness condition: client's state is want, compared as JSON values.
func holdsAwareness(client uint64, want any) func(map[uint64]json.RawMessage) bool {
	encoded, _ := json.Marshal(want)
	var wanted any
	_ = json.Unmarshal(encoded, &wanted)
	return func(states map[uint64]json.RawMessage) bool {
		raw, present := states[client]
		if !present {
			return false
		}
		var got any
		return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, wanted)
	}
}

// lacksAwareness is a WaitForAwareness condition: client has no state.
func lacksAwareness(client uint64) func(map[uint64]json.RawMessage) bool {
	return func(states map[uint64]json.RawMessage) bool {
		_, present := states[client]
		return !present
	}
}

// roomHoldsClient reports whether service's resident room of artifactID holds a state of client.
func roomHoldsClient(service *Service, artifactID string, client uint64) bool {
	aw, resident := service.srv.GetAwareness(artifactID)
	if !resident {
		return false
	}
	_, held := aw.GetStates()[client]
	return held
}

// setAwareness announces client's state through reader, failing the test when it cannot.
func setAwareness(t *testing.T, reader *docstest.Peer, client uint64, state any) {
	t.Helper()
	if err := reader.SetAwareness(client, state); err != nil {
		t.Fatalf("announce client %d's awareness: %v", client, err)
	}
}

// requireAwareness waits for reader to see states that hold, failing with what as the cause.
func requireAwareness(t *testing.T, reader *docstest.Peer, holds func(map[uint64]json.RawMessage) bool, what string) {
	t.Helper()
	started := time.Now()
	if err := reader.WaitForAwareness(roomWait(t), holds); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	t.Logf("%s: %v", what, time.Since(started).Round(time.Millisecond))
}

// A reader's cursor set on one process reaches a reader of the same document on the other, each
// way, and so does its next move.
func TestAwarenessSetOnOneProcessReachesAReaderOnTheOther(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")

	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")
	setAwareness(t, bob, bobClient, cursorOf("bob", 2))
	requireAwareness(t, alice, holdsAwareness(bobClient, cursorOf("bob", 2)), "bob's cursor on task B reaching alice on task A")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 3))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 3)), "alice's cursor moving on task A reaching bob on task B")
}

// Every reader who moves at the same moment on one process reaches the readers on the other
// within a second, not only the last of them: ygo's outbound lane keeps one awareness blob per
// room, latest only (internal/relaylane/lane.go), and each blob holds only the clients the
// change it came from touched, so a relay that sent what that lane handed it would drop the
// cursors it superseded until each client's next heartbeat - 15 s in a browser, and never for
// the readers here, which send none.
func TestEveryReaderMovingAtOnceOnOneProcessReachesAReaderOnTheOther(t *testing.T) {
	// Not parallel: it holds five readers' cursors to reaching a reader on the other process within
	// one second, and among the package's parallel tests the relay took longer (envoy-go-docs-finalizers
	// job 137379091221).
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	const movers = 5
	readers := make([]*docstest.Peer, movers)
	for reader := range readers {
		readers[reader] = pair.openReader(t, pair.serverA, fmt.Sprintf("reader%d", reader))
	}
	bob := pair.openReader(t, pair.serverB, "bob")

	// Each reader announces its own client in the same instant, as a room of readers typing does.
	start := make(chan struct{})
	var moved sync.WaitGroup
	for reader, peer := range readers {
		moved.Add(1)
		go func() {
			defer moved.Done()
			<-start
			if err := peer.SetAwareness(movingClient(reader), cursorOf(fmt.Sprintf("reader%d", reader), reader)); err != nil {
				t.Errorf("announce reader %d's cursor: %v", reader, err)
			}
		}()
	}
	close(start)
	moved.Wait()

	held := func(states map[uint64]json.RawMessage) bool {
		for reader := range readers {
			if !holdsAwareness(movingClient(reader), cursorOf(fmt.Sprintf("reader%d", reader), reader))(states) {
				return false
			}
		}
		return true
	}
	if err := bob.WaitForAwareness(time.Second, held); err != nil {
		t.Fatalf("the cursors %d readers on task A set at once did not all reach bob on task B within a second: %v", movers, err)
	}
}

// movingClient is the awareness client of the reader'th reader in the test above, as a provider
// announces its tab's own.
func movingClient(reader int) uint64 { return 3_000 + uint64(reader) }

// A reader who opens the document second, on the other process, sees the first reader's cursor
// at once: the room that loads for it asks the other processes for their readers' states, and
// they answer. The test's readers send no heartbeat, so nothing else would bring it.
func TestAReaderOpeningSecondOnTheOtherProcessSeesTheFirstReadersAwareness(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitFor(t, roomWait(t), "task A's room to hold alice's cursor", func() bool {
		return roomHoldsClient(pair.taskA, pair.artifactID, aliceClient)
	})

	bob := pair.openReader(t, pair.serverB, "bob")
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)),
		"bob, opening second on task B, seeing the cursor alice set on task A")
}

// A reader who joins a room already resident on its process, and that loaded with no reader in
// it, asks for the other processes' readers' states as it joins: the load asked nobody.
func TestAReaderJoiningAResidentRoomOnTheOtherProcessSeesTheFirstReadersAwareness(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's state reaching task B")
	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	if roomHoldsClient(pair.taskB, pair.artifactID, aliceClient) {
		t.Fatal("task B's room, loaded with no reader, already holds alice's cursor, so nothing here is tested")
	}

	bob := pair.openReader(t, pair.serverB, "bob")
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)),
		"bob, joining task B's resident room, seeing the cursor alice set on task A")
}

// A reader leaving on one process leaves on the other: ygo's removal of the departing peer's
// states is relayed like any other change.
func TestADisconnectOnOneProcessRemovesTheClientsAwarenessOnTheOther(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")

	alice.Close()
	requireAwareness(t, bob, lacksAwareness(aliceClient), "alice leaving task A removing her from bob's reader on task B")
}

// A room this process closes itself - here a forced close, as a replaced history, a closed issue
// or a failed room's eviction makes - removes its readers' states on the other processes too,
// though the close disconnects them and tears the room's relay down together.
func TestARoomThisProcessClosesRemovesItsReadersAwarenessOnTheOther(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")

	if err := pair.taskA.srv.CloseRoom(pair.artifactID, true); err != nil {
		t.Fatalf("close task A's room: %v", err)
	}
	select {
	case <-alice.Ended:
	case <-time.After(roomWait(t)):
		t.Fatal("alice's socket outlived the close of her room")
	}
	requireAwareness(t, bob, lacksAwareness(aliceClient), "task A closing alice's room removing her from bob's reader on task B")
}

// waitForAwarenessState waits for handled to note an awareness state update (not a query) for
// artifactID, which the relay has handled by then.
func waitForAwarenessState(t *testing.T, handled <-chan awarenessMessage, artifactID, what string) {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case message := <-handled:
			if message.Room == artifactID && !message.Query {
				return
			}
		case <-deadline:
			t.Fatalf("%s: no awareness state for %s handled within %v", what, artifactID, roomWait(t))
		}
	}
}

// Another process's awareness is applied only to a room this process holds with a reader of its
// own in it: it does not load the document on a process no reader of it is on, and it does not
// reach - nor restamp the idle clock of - a room resident here with nobody reading it.
func TestRelayedAwarenessNeitherLoadsNorKeepsARoomWithNoReaderOfItsOwn(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")

	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's state reaching task B, which holds no room")
	if slices.Contains(pair.taskB.srv.Rooms(), pair.artifactID) {
		t.Fatal("alice's awareness loaded a room on task B, where nobody reads the document")
	}

	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	setAwareness(t, alice, aliceClient, cursorOf("alice", 2))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's next state reaching task B, which holds a room with no reader")
	if roomHoldsClient(pair.taskB, pair.artifactID, aliceClient) {
		t.Fatal("alice's awareness reached task B's room, which no reader of task B is in: relayed heartbeats would keep it resident")
	}
}

// daveClient is the awareness client of a reader of a second document in the test below.
const daveClient uint64 = 4004

// Another process's awareness for a room still loading here is not relayed into it: ygo has not
// published the room, and an Inject into it would wait on its load on the awareness lane, which
// serves every room's relayed awareness (holdsRoomForReaders). Here task B loads a second document
// for a reader opening it cold, and the load waits for the pod's load concurrency, which the test
// holds whole; dave, reading that document on task A, moves, and then alice moves in the first
// document, and bob sees her move on task B within relayWait. Before, task B's awareness lane
// injected dave's move into the loading room and waited on its load, whose wait for the pool has
// no deadline, and alice's move waited behind it for as long as the load did.
func TestAColdOpenWhoseLoadWaitsDoesNotStallAnotherRoomsRelayedAwareness(t *testing.T) {
	const relayWait = 10 * time.Second
	pair := newProcessPair(t)
	other := createIssueDocument(t, pair.database, 2, "Other.")
	seedServiceText(t, pair.taskA, other, "Other document.\n")
	pool, waits := poolWaits(t)
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	dave := connectPeer(t, pair.serverA.URL, other, "dave")
	if err := dave.WaitForText(peerMarkdown, storedMarkdown(t, pair.database, other), roomWait(t)); err != nil {
		t.Fatalf("dave's reader never received the other document: %v", err)
	}
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")

	// Task B's load of the other document, for a reader opening it cold, waits for the full pool,
	// its room a placeholder ygo has not published.
	filler := fillPoolBut(t, pool, 0)
	for len(waits) > 0 {
		<-waits
	}
	pair.taskB.addConnection(other, 9001, model.Actor{Kind: "user", ID: "carol"})
	loaded := make(chan error, 1)
	go func() { loaded <- pair.taskB.warmLiveDocument(context.Background(), other) }()
	select {
	case <-waits:
	case err := <-loaded:
		t.Fatalf("task B's load of the other document finished (%v) while the pool was full, want it waiting for its grant", err)
	case <-time.After(relayWait):
		t.Fatalf("task B's load of the other document never came to wait for the pool within %s", relayWait)
	}
	if pair.taskB.srv.GetDoc(other) != nil {
		t.Fatal("ygo published task B's room of the other document while its load waits, so nothing here is tested")
	}
	setAwareness(t, dave, daveClient, cursorOf("dave", 1))
	waitFor(t, relayWait, "task B's awareness lane to take dave's state, dropping it or waiting on the load", func() bool {
		for {
			select {
			case message := <-handledB:
				if message.Room == other && !message.Query {
					return true
				}
			default:
				return roomLoadWaiters() > 0
			}
		}
	})
	setAwareness(t, alice, aliceClient, cursorOf("alice", 2))
	if err := bob.WaitForAwareness(relayWait, holdsAwareness(aliceClient, cursorOf("alice", 2))); err != nil {
		t.Fatalf("alice's move on task A did not reach bob on task B within %s while task B loaded another document a reader opened cold: %v", relayWait, err)
	}
	filler.release()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("task B's load of the other document, once the pool had room: %v", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("task B's load of the other document did not finish once the pool had room")
	}
}

// relayedState is a notification another process's relay sends of client's state at clock 1
// (awarenessNotifications), for room.
func relayedState(t *testing.T, room string, client uint64, state any) string {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode client %d's state: %v", client, err)
	}
	update := awarenessStates(client, 1, 1, string(encoded))
	payload, err := json.Marshal(awarenessMessage{Sender: "another process", Room: room, Data: base64.StdEncoding.EncodeToString(update)})
	if err != nil {
		t.Fatalf("encode the notification of client %d's state: %v", client, err)
	}
	return string(payload)
}

// Another process's awareness for a room that goes between the relay's look at it
// (holdsRoomForReaders) and its Inject - here a close the service makes (Evict) - loads nothing on
// the awareness lane, which serves every room's relayed awareness, and holds the lane no longer
// than the Inject's own bound (awarenessInjectBound): the Inject is marked as a caller that may
// load no room (withNoRoomLoad), so the load ygo begins for the room it no longer finds is refused
// before it reads anything and the frame is dropped at once, and where another caller's load of
// the room began in that moment the Inject's wait on it ends at its bound and drops the frame the
// same way. The pool is held whole from the close on, so a load on the lane would wait for it with
// no deadline. Before, the Inject loaded the room on the lane, its load waiting for the full pool,
// or waited on the other caller's load, for as long as the test held the pool.
func TestRelayedAwarenessWhoseRoomGoesBeforeItsInjectLoadsNothingOnTheLane(t *testing.T) {
	// dropBound bounds the relay's handling of the update once its room has gone: a refused load
	// reads nothing, and a wait on another caller's load ends at the Inject's own bound.
	const dropBound = awarenessInjectBound + 10*time.Second
	for _, route := range []struct {
		name string
		// alsoLoading begins another caller's load of the room in the gap, once it has closed.
		alsoLoading bool
	}{
		{"the close leaves no room, which the Inject would load", false},
		{"another caller's load of the room begins in the gap, which the Inject would wait on", true},
	} {
		t.Run(route.name, func(t *testing.T) {
			service, document := newTestService(t)
			service.settle = time.Hour
			pool, waits := poolWaits(t)
			if err := loadRoomOf(service, document); err != nil {
				t.Fatalf("load the document's room: %v", err)
			}
			service.addConnection(document, 9001, model.Actor{Kind: "user", ID: "carol"})
			if !service.holdsRoomForReaders(document) {
				t.Fatal("the document's room is not one relayed awareness is applied to, so nothing here is tested")
			}
			filler := newLoadSlot()
			t.Cleanup(filler.release)
			loaded := make(chan error, 1)
			gone := make(chan error, 1)
			service.beforeAwarenessInject = func(room string) {
				if room != document {
					return
				}
				if err := service.Evict(context.Background(), room); err != nil {
					gone <- fmt.Errorf("close the document's room: %w", err)
					return
				}
				if service.srv.GetDoc(room) != nil {
					gone <- errors.New("the document's room is still resident once it was closed")
					return
				}
				for len(waits) > 0 {
					<-waits
				}
				if !filler.tryTake(pool.poolBytes() - pool.held.Load()) {
					gone <- errors.New("the pool refused the bytes its holders do not hold")
					return
				}
				if route.alsoLoading {
					go func() { loaded <- loadRoomOf(service, room) }()
					select {
					case <-waits:
					case err := <-loaded:
						gone <- fmt.Errorf("the other caller's load of the room finished (%v) while the pool was full, want it waiting for its grant", err)
						return
					case <-time.After(holdAndWaitBound):
						gone <- fmt.Errorf("the other caller's load of the room never came to wait for the pool within %s", holdAndWaitBound)
						return
					}
				}
				gone <- nil
			}
			payload := relayedState(t, document, daveClient, cursorOf("dave", 1))
			handled := make(chan struct{})
			go func() {
				defer close(handled)
				service.awarenessRelay.received(context.Background(), payload)
			}()
			// letGo gives the pool back and waits for the relay's handling of the update, once, and
			// runs as the test ends in any case, so nothing of it runs past the test's end.
			letGo := sync.OnceFunc(func() {
				filler.release()
				select {
				case <-handled:
				case <-time.After(holdAndWaitBound):
					t.Errorf("the relay's handling of the update did not end within %s once the pool was free", holdAndWaitBound)
				}
			})
			t.Cleanup(letGo)
			select {
			case err := <-gone:
				if err != nil {
					t.Fatal(err)
				}
			case <-handled:
				t.Fatal("the relay handled the update without reaching its Inject, so nothing here is tested")
			case <-time.After(holdAndWaitBound):
				t.Fatalf("the relay did not reach its Inject within %s", holdAndWaitBound)
			}
			closed := time.Now()
			select {
			case <-handled:
			case <-time.After(dropBound):
				t.Fatalf("the relay's handling of the update had not ended %s after its room went: its Inject loaded the room, or waited on another caller's load of it, on the awareness lane with no deadline", dropBound)
			}
			dropped := time.Since(closed)
			t.Logf("the relay dropped the update %s after its room went", dropped.Round(time.Millisecond))
			if !route.alsoLoading {
				if dropped >= awarenessInjectBound {
					t.Fatalf("the relay dropped the update %s after its room went, at or past the Inject's own bound %s: its Inject did not refuse the load of the room it no longer found", dropped, awarenessInjectBound)
				}
				if service.srv.GetDoc(document) != nil || slices.Contains(service.srv.Rooms(), document) {
					t.Fatal("a room of the document loaded on the awareness lane once the close took it out")
				}
				if failure := service.roomFailure(document); failure != nil {
					t.Fatalf("the document's room is failed once the relay's load of it was refused: %v", failure)
				}
			}
			letGo()
			if !route.alsoLoading {
				return
			}
			select {
			case err := <-loaded:
				if err != nil {
					t.Fatalf("the other caller's load of the room, once the pool had room: %v", err)
				}
			case <-time.After(roomWait(t)):
				t.Fatal("the other caller's load of the room did not finish once the pool had room")
			}
		})
	}
}

// A client whose state is too large for one notification is dropped and counted rather than sent,
// on every batch that carries it, and what the room relays after it still crosses. The count is
// not always one: nothing takes an oversized client off the list this process answers queries
// from, and a query is answered with the state of every local client (the query note at the top
// of awareness_relay.go, answer), so the query task B sends as bob's open activates the room
// there offers alice's state to the size check again. How many batches carry it is what
// scheduling decides; the invariant is that every one of them was dropped, so none of them
// reached task B.
func TestAnAwarenessStateTooLargeToRelayIsDroppedAndCounted(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	// Each batch of the room that carries alice's client as a state rather than a removal offers
	// that state to the size check once (awarenessNotifications), so these offers are what the
	// drops counted below are owed against. The window is opened before either reader connects,
	// since task B queries task A as bob's open activates the room there and the answer's batch
	// carries alice's state like any other.
	var offered atomic.Uint64
	pair.taskA.beforeAwarenessResolve = func(room string, batch map[uint64]awarenessEntry) {
		if entry, carried := batch[aliceClient]; room == pair.artifactID && carried && !entry.removed {
			offered.Add(1)
		}
	}
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")

	oversized := map[string]any{"user": map[string]any{"name": strings.Repeat("a", roomSyncPayloadLimit)}}
	setAwareness(t, alice, aliceClient, oversized)
	waitFor(t, roomWait(t), "task A's relay to count the oversized state", func() bool {
		return pair.taskA.awarenessRelay.oversized.Load() >= 1
	})
	const otherClient = aliceClient + 1
	setAwareness(t, alice, otherClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(otherClient, cursorOf("alice", 1)), "the small state after the oversized one reaching bob on task B")
	if _, present := bob.AwarenessStates()[aliceClient]; present {
		t.Fatal("the oversized state reached bob on task B")
	}
	if offered.Load() == 0 {
		t.Fatal("no batch of task A's room carried the oversized state, so the drops below stand for nothing")
	}
	// The drop is counted after this window has run, so a flush already in flight leaves the two
	// counts apart until it reaches the size check: this waits for that flush rather than for a
	// time, and a state sent instead of dropped never closes the gap.
	waitFor(t, roomWait(t), "task A's relay to count a drop for every batch that carried the oversized state", func() bool {
		return pair.taskA.awarenessRelay.oversized.Load() == offered.Load()
	})
}

// awarenessStates is one y-protocols awareness update announcing count clients from first, each at
// clock with state, as one peer's frame carries them: the state of a cursor, or "null" to take
// those clients away as the disconnect of the peer that sent them does. It is the one encoder the
// tests of this package announce awareness clients with.
func awarenessStates(first, count, clock uint64, state string) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(count)
		for client := first; client < first+count; client++ {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(clock)
			encoder.WriteVarString(state)
		}
	})
}

// floodState is the state each client of a flood announces below.
const floodState = `{"user":{"name":"flood"}}`

// relayedClients is how many of room's clients service's relay is sending to the other processes.
func relayedClients(service *Service, room string) int {
	relay := service.awarenessRelay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return len(relay.local[room])
}

// One frame inventing far more awareness clients than any room has readers costs the room, and
// the other processes, the bound and no more: the room takes the bound's clients of the frame
// and refuses the rest (maxAwarenessClientsPerRoom), and the relay sends those in the few
// notifications they fill, not one per client. What keeps such a frame from reaching a room at
// all is the bound each socket is held to before ygo applies it (awareness_limit.go); this one
// is applied to the room's awareness directly, as ygo's own expiry and removal paths do.
func TestAnAwarenessFrameOverTheClientBoundRelaysNoMoreThanTheBound(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	handledB := runAwareness(t, pair.taskB)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 8 * maxAwarenessClientsPerRoom
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, floodState), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the bound's clients of the frame", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == maxAwarenessClientsPerRoom
	})
	tracked, err := pair.taskA.heldAwarenessClients(pair.artifactID)
	if err != nil {
		t.Fatalf("read the awareness task A's room holds: %v", err)
	}
	if tracked != maxAwarenessClientsPerRoom {
		t.Fatalf("task A's room tracks %d of the frame's %d clients, want the bound's %d", tracked, flood, maxAwarenessClientsPerRoom)
	}

	deadline := time.After(roomWait(t))
	sends, relayed := 0, 0
	for relayed < maxAwarenessClientsPerRoom {
		select {
		case message := <-handledB:
			if message.Room != pair.artifactID || message.Query {
				continue
			}
			sends++
			update, err := base64.StdEncoding.DecodeString(message.Data)
			if err != nil {
				t.Fatalf("decode a relayed awareness update: %v", err)
			}
			entries, err := decodeAwarenessEntries(update)
			if err != nil {
				t.Fatalf("read a relayed awareness update: %v", err)
			}
			relayed += len(entries)
		case <-deadline:
			t.Fatalf("task B heard %d awareness clients in %d notifications within %v, want the bound's %d",
				relayed, sends, roomWait(t), maxAwarenessClientsPerRoom)
		}
	}
	if relayed > maxAwarenessClientsPerRoom || sends > maxAwarenessClientsPerRoom {
		t.Fatalf("a frame of %d clients relayed %d of them in %d notifications, past the %d-client bound",
			flood, relayed, sends, maxAwarenessClientsPerRoom)
	}
	t.Logf("a frame of %d awareness clients relayed %d of them in %d notifications", flood, relayed, sends)
}

// The clients of a flood that leaves give their entries back as they go: a room's bound counts
// the clients it holds now, not every client id it has ever heard of, so a cursor announced just
// after a frame of thousands has gone is taken by the room and relayed at once rather than
// refused until the frame's clients have expired and been purged.
func TestAwarenessClientsThatLeaveFreeTheRoomsBoundAtOnce(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 8 * maxAwarenessClientsPerRoom
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, floodState), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the bound's clients of the frame", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == maxAwarenessClientsPerRoom
	})
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, "null"), "a peer of task A"); err != nil {
		t.Fatalf("take the frame's %d awareness clients away: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending none of the frame's clients once they have left", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == 0
	})

	// A client id of the joining reader's own: the frame claims 1 through flood, and a client the
	// frame has left a tombstone of stands at a clock of its own.
	const joined = flood + aliceClient
	setAwareness(t, alice, joined, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(joined, cursorOf("alice", 1)),
		fmt.Sprintf("alice's cursor, announced on task A just after a frame of %d clients had come and gone, reaching bob on task B", flood))
}

// A batch too large for one notification is split across several, each within the limit and
// holding whole clients, and a client too large for any is dropped and counted.
func TestAnAwarenessBatchIsSplitAcrossNotificationsWithinTheLimit(t *testing.T) {
	t.Parallel()
	relay := newAwarenessRelay(&Service{processID: "00000000-0000-0000-0000-000000000001"})
	room := "00000000-0000-0000-0000-000000000002"
	batch := make(map[uint64]awarenessEntry)
	for client := uint64(1); client <= 5; client++ {
		state := `{"name":"` + strings.Repeat("x", 2_500) + `"}`
		entries, err := decodeAwarenessEntries(awarenessStates(client, 1, client, state))
		if err != nil {
			t.Fatal(err)
		}
		batch[client] = entries[0]
	}
	huge, err := decodeAwarenessEntries(awarenessStates(99, 1, 1, `{"name":"`+strings.Repeat("y", roomSyncPayloadLimit)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	batch[99] = huge[0]

	payloads := relay.awarenessNotifications(room, batch)
	if len(payloads) < 2 {
		t.Fatalf("five clients of about 2.5 kB each went out in %d notifications, want them split", len(payloads))
	}
	var sent []uint64
	for _, payload := range payloads {
		if len(payload) > roomSyncPayloadLimit {
			t.Errorf("a notification of %d bytes passes the %d-byte limit", len(payload), roomSyncPayloadLimit)
		}
		var message awarenessMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			t.Fatal(err)
		}
		if message.Room != room || message.Sender != relay.host.relaySender() {
			t.Fatalf("a notification names room %q and sender %q", message.Room, message.Sender)
		}
		update, err := base64.StdEncoding.DecodeString(message.Data)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := decodeAwarenessEntries(update)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			sent = append(sent, entry.client)
		}
	}
	if want := []uint64{1, 2, 3, 4, 5}; !slices.Equal(sent, want) {
		t.Fatalf("the notifications carry clients %v, want %v, each once", sent, want)
	}
	if dropped := relay.oversized.Load(); dropped != 1 {
		t.Fatalf("the relay counted %d oversized states, want 1", dropped)
	}
}

// Every document socket's upgrade names the process serving it.
func TestTheDocumentWebsocketUpgradeNamesItsProcess(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	reader := pair.openReader(t, pair.serverA, "alice")
	if pair.taskA.hostname == "" {
		t.Fatal("the service read no host name")
	}
	if served := reader.UpgradeHeader.Get(ProcessHeader); served != pair.taskA.hostname {
		t.Fatalf("the upgrade names %q as its process, want %q; header %v", served, pair.taskA.hostname, reader.UpgradeHeader)
	}
}
