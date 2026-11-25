//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// One tab on A sends a single awareness frame that names more clients than a room may hold
// (floodClients), then leaves. A reader who opens the document on A a moment later must still be
// shown to a reader on B within a second, as a reader on A is shown to the other readers on A at
// once. A frame that no socket bound cuts fills A's room and, through the relay, B's
// (maxAwarenessClientsPerRoom), and a room that keeps the frame's clients - the ones present and
// the tombstones of the ones that left - until they expire and are purged (about 75 s, though the
// flooding tab left at once) has no entry for a client it has not seen before: the new reader
// stays invisible on B for that long, while on A it is seen at once. The waits for the reader are
// the shared wait, which would sit those 75 s out, so the row reads B's room itself, once it has
// taken what A relayed of the frame and again once the frame has left bob's view: each time it
// must hold an entry free for her. The second is held only by the row's twin in benchtime's timing
// lane (withinLive).
func TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	aReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t, benchtime.EveryRun)
}

// TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcessOnTheClock is the row above in
// benchtime's timing lane, where alice's cursor reaching carol on A and bob on B is held to
// liveWithin of its announce (withinLive). The product's promise is README.md's Cursors and
// presence ("a reader who joins after such a frame is taken in and relayed at once"), at the
// spec's "at once", the one second the plan pins (#2414).
func TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcessOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess)
}

// aReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess is the scene of the two rows above;
// lane says whether its readings of the clock are held to their figures (benchtime.Lane).
func aReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLD")
	pair.a.createProject(t, project, "Awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded", "x")
	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openSyncedEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	if err := announceAwarenessClients(flood, artifactID, 500_000, floodClients, 1, inventedState(500_000)); err != nil {
		t.Fatalf("send the flooding frame: %v", err)
	}
	if err := bob.WaitForAwareness(t, func(states map[uint64]json.RawMessage) bool {
		return holdsAnyClientFrom(states, 500_000, floodClients)
	}); err != nil {
		t.Fatalf("none of the flooding frame's clients reached bob on B: %v", err)
	}
	// A's room applies the frame in one step before it relays any of it, so what A holds once bob
	// has seen some of the frame is all A took; B has taken all A relayed once its room holds as
	// many entries. A room the frame filled refuses a reader who joins while its clients are there.
	took := pair.a.roomCounters(t, artifactID).AwarenessClients
	if held := testwait.Until(t, fmt.Sprintf("B's room to take what A's room relayed of the flood (A's holds %d entries)", took),
		func() (int, bool) {
			held := pair.b.roomCounters(t, artifactID).AwarenessClients
			return held, held >= took
		}); held >= docs.MaxAwarenessClientsPerRoomForTest {
		t.Fatalf("B's room holds %d awareness client entries once one tab's %d-client frame reached it, at or past its bound of %d: "+
			"no bound a socket is held to cut the frame, so one tab took every entry, and B has none for a reader who joins while its clients are there",
			held, floodClients, docs.MaxAwarenessClientsPerRoomForTest)
	}
	// The frame's clients that crossed are the ones B's room took a state of from A's relay, and
	// the only ones bob's view can hold; their departure must reach B the same way, and B's own
	// expiry of them 30 s on (awarenessExpiry), which ygo tells bob's tab nothing of, stands in for
	// it only where B's listener redialed meanwhile (awaitRemovalReachesReader,
	// awaitRelayedAwareness).
	//
	// The wait below is for each of those clients, so they must be every flooding client bob holds,
	// and at least one. Bob's view is read before B's counts: B's room counts a client the relay
	// hands it inside the apply, from the observer ygo fires there (awarenessRelay.touched), and
	// only then sends the client to its peers (ygo's Server.Inject), so each flooding client bob
	// holds by then is one the counts hold a state of.
	seen := bob.AwarenessStates()
	crossed := pair.b.roomCounters(t, artifactID)
	counted := 0
	for client, taken := range crossed.RelayedAwareness {
		if client >= 500_000 && client < 500_000+floodClients && taken.States > 0 {
			counted++
		}
	}
	if counted == 0 {
		t.Fatalf("B's room counts a relayed state of none of the flooding frame's %d clients, though bob on B was shown some of them: "+
			"the wait for their departure from bob's view would check nothing", floodClients)
	}
	for client := range seen {
		if client >= 500_000 && client < 500_000+floodClients && crossed.RelayedAwareness[client].States == 0 {
			t.Fatalf("bob on B holds the flooding tab's client %d, of which B's room counts no relayed state (%+v): "+
				"the wait for the departure of the clients that crossed would leave it in bob's view unchecked",
				client, crossed.RelayedAwareness[client])
		}
	}
	flood.Close()
	// The flooding tab has left before alice joins: bob, on B, holds none of its clients once B's
	// room has taken their removal, which is the tombstones the bound must not count.
	for client, taken := range crossed.RelayedAwareness {
		if client >= 500_000 && client < 500_000+floodClients {
			before := awarenessBase{taken: taken, listenStands: crossed.ListenStands}
			what := fmt.Sprintf("the flooding tab's client %d leaving bob's view on B", client)
			pair.b.awaitRemovalReachesReader(t, artifactID, client, bob, before, what)
			pair.b.awaitRelayedAwareness(t, artifactID, client, true, before, what)
		}
	}
	// What lets B show a reader it has not seen is a free entry in its room: the bound counts the
	// clients present and the tombstones of the ones that left (docs.RoomCounters.AwarenessClients),
	// and a room the frame filled keeps those tombstones until they expire and are purged.
	if held := pair.b.roomCounters(t, artifactID).AwarenessClients; held >= docs.MaxAwarenessClientsPerRoomForTest {
		t.Fatalf("B's room holds %d awareness client entries once one tab's %d-client frame had left bob's view, at or past its bound of %d: "+
			"it has no entry for a reader who joins now, who stays invisible on B until the tombstones expire and are purged",
			held, floodClients, docs.MaxAwarenessClientsPerRoomForTest)
	}

	alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
	aliceOnB := pair.b.relayedAwareness(t, artifactID, aliceAwarenessClient)
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	announced := time.Now()
	if err := carol.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice, who joined A after the flood: %v", err)
	}
	withinLive(t, lane, announced, "alice's cursor reaching carol on A")
	if err := bob.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A once one tab's %d-client awareness frame had left B's view, was seen by carol on A but not by bob on B: B's room still counts the frame's clients, the tombstones of the ones that left included, against its bound, and drops every client it has not seen before",
			floodClients)
	}
	withinLive(t, lane, announced, "alice's cursor reaching bob on B")
	pair.b.awaitRelayedAwareness(t, artifactID, aliceAwarenessClient, false, aliceOnB, "alice's cursor reaching bob on B")
}

// The same flood, from a tab that stays: it keeps its floodClients invented clients alive,
// re-announcing them every ten seconds as a tab's heartbeat does. Alice, opening the document on A
// once A's room holds them, is seen at once by carol on A; she must be seen by bob on B within a
// second too, as she would be were all three on one process. With a frame that no socket bound
// cuts and a room bound that counts every client a room holds (maxAwarenessClientsPerRoom), A's
// room is full of the tab's clients, refuses alice's client id, and never relays it: bob sees her
// only once the flooding tab stops re-announcing and its clients expire. The row reads A's room
// before she joins - it must hold an entry free for her - and the second is held only by the
// row's twin in benchtime's timing lane (withinLive).
func TestAReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	aReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess(t, benchtime.EveryRun)
}

// TestAReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcessOnTheClock is the row above in
// benchtime's timing lane, where alice's cursor reaching carol on A and bob on B is held to
// liveWithin of its announce (withinLive). The product's promise is README.md's Cursors and
// presence (a room "stands above every client real readers can put in a room and refuses none of
// them, here or on another process", and its readers' changes are relayed once per 50 ms at
// most), at the spec's "at once", the one second the plan pins (#2414).
func TestAReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcessOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess)
}

// aReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess is the scene of the two rows
// above; lane says whether its readings of the clock are held to their figures (benchtime.Lane).
func aReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLL")
	pair.a.createProject(t, project, "Live awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded live", "x")
	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openSyncedEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	announce := func(clock uint64) {
		if err := announceAwarenessClients(flood, artifactID, 600_000, floodClients, clock, inventedState(600_000)); err != nil {
			t.Errorf("announce the flooding tab's clients at clock %d: %v", clock, err)
		}
	}
	announce(1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for clock := uint64(2); ; clock++ {
			select {
			case <-stop:
				return
			case <-ticker.C:
				announce(clock)
			}
		}
	}()
	// The flood is in A's room before alice joins: carol, on A, holds one of the tab's clients once
	// A's room has taken its frame.
	if err := carol.WaitForAwareness(t, func(states map[uint64]json.RawMessage) bool {
		return holdsAnyClientFrom(states, 600_000, floodClients)
	}); err != nil {
		t.Fatalf("none of the flooding tab's clients reached carol on A: %v", err)
	}
	if held := pair.a.roomCounters(t, artifactID).AwarenessClients; held >= docs.MaxAwarenessClientsPerRoomForTest {
		t.Fatalf("A's room holds %d awareness client entries while one tab there keeps %d invented clients alive, at or past its bound of %d: "+
			"it has no entry for a reader who joins now, and refuses her client id",
			held, floodClients, docs.MaxAwarenessClientsPerRoomForTest)
	}

	alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
	aliceOnB := pair.b.relayedAwareness(t, artifactID, aliceAwarenessClient)
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	announced := time.Now()
	if err := carol.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice: %v", err)
	}
	withinLive(t, lane, announced, "alice's cursor reaching carol on A")
	if err := bob.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A while one tab there kept %d invented awareness clients alive, was seen by carol on A but not by bob on B: A's room is full of that tab's clients and never relays hers",
			floodClients)
	}
	withinLive(t, lane, announced, "alice's cursor reaching bob on B")
	pair.b.awaitRelayedAwareness(t, artifactID, aliceAwarenessClient, false, aliceOnB, "alice's cursor reaching bob on B")
}

// floodClients is how many client ids the flooding tabs above invent: more than a room may hold
// (docs.MaxAwarenessClientsPerRoomForTest, production's bound, which the rows run at), so a frame
// that no socket bound cuts fills the room; and few enough that the frame stays under the largest
// awareness message a socket's filter reads whole (maxAwarenessMessageBytes, 256 KiB, in
// docs/awareness_limit.go), since each invented client takes under 40 bytes of it (inventedState).
const floodClients = docs.MaxAwarenessClientsPerRoomForTest + 96

// inventedState is the state a flooding tab gives each of the client ids it invents from first:
// a cursor-sized one, as the tabs of this suite's readers announce.
func inventedState(first uint64) func(client uint64) string {
	return func(client uint64) string {
		return fmt.Sprintf(`{"user":{"name":"invented %d"}}`, client-first)
	}
}

// holdsAnyClientFrom is whether states holds one of the count client ids a flooding tab invents
// from first.
func holdsAnyClientFrom(states map[uint64]json.RawMessage, first, count uint64) bool {
	for client := range states {
		if client >= first && client < first+count {
			return true
		}
	}
	return false
}
