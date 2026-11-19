//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// One tab on A sends a single awareness frame that names 2,048 clients, then leaves. A reader who
// opens the document on A a moment later must still be shown to a reader on B within a second, as
// a reader on A is shown to the other readers on A at once. With a room client bound below what
// the sockets of a process may hold (maxAwarenessClientsPerRoom), B's room is full of the frame's
// clients, which stay there until they expire and their tombstones are purged (about 75 s, though
// the flooding tab left at once), and B drops every client it has not seen before: the new reader
// stays invisible on B for that long, while on A it is seen at once.
func TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLD")
	pair.a.createProject(t, project, "Awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded", "x")
	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openSyncedEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	const invented = 2048
	if err := announceAwarenessClients(flood, artifactID, 500_000, invented, 1, inventedState(500_000)); err != nil {
		t.Fatalf("send the flooding frame: %v", err)
	}
	if err := bob.WaitForAwareness(t, func(states map[uint64]json.RawMessage) bool {
		return holdsAnyClientFrom(states, 500_000, invented)
	}); err != nil {
		t.Fatalf("none of the flooding frame's clients reached bob on B: %v", err)
	}
	flood.Close()
	// The flooding tab has left before alice joins: bob, on B, holds none of its clients once B's
	// room has taken their removal, which is the tombstones the bound must not count.
	if err := bob.WaitForAwareness(t, func(states map[uint64]json.RawMessage) bool {
		return !holdsAnyClientFrom(states, 500_000, invented)
	}); err != nil {
		t.Fatalf("the flooding tab's clients never left bob's view on B: %v", err)
	}

	alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice, who joined A after the flood: %v", err)
	}
	if err := bob.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A once one tab's %d-client awareness frame had left B's view, was seen by carol on A but not by bob on B: B's room still counts the frame's clients, the tombstones of the ones that left included, against its bound, and drops every client it has not seen before",
			invented)
	}
}

// The same flood, from a tab that stays: it keeps its 2,048 invented clients alive, re-announcing
// them every ten seconds as a tab's heartbeat does. Alice, opening the document on A once A's room
// holds them, is seen at once by carol on A; she must be seen by bob on B within a second too, as
// she would be were all three on one process. With a room bound that counts every client a room
// holds (maxAwarenessClientsPerRoom), A's room is full of the tab's clients, refuses alice's client
// id, and never relays it: bob sees her only once the flooding tab stops re-announcing and its
// clients expire.
func TestAReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLL")
	pair.a.createProject(t, project, "Live awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded live", "x")
	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openSyncedEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	const invented = 2048
	announce := func(clock uint64) {
		if err := announceAwarenessClients(flood, artifactID, 600_000, invented, clock, inventedState(600_000)); err != nil {
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
		return holdsAnyClientFrom(states, 600_000, invented)
	}); err != nil {
		t.Fatalf("none of the flooding tab's clients reached carol on A: %v", err)
	}

	alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice: %v", err)
	}
	if err := bob.WaitForAwareness(t, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A while one tab there kept %d invented awareness clients alive, was seen by carol on A but not by bob on B: A's room is full of that tab's clients and never relays hers",
			invented)
	}
}

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
