package docs

import "testing"

// A peer that sends frame after frame of invented awareness clients - some announced and taken
// away again, some sent as null states straight away - must not grow a room's awareness without
// limit, on the process its socket is on or on the one the relay reaches: each entry, a tombstone
// included, is memory until a purge two expiries later, and the frames cost the peer nothing.
// Here 16 frames of 1,024 clients each reach task A, half taken away again and half sent as nulls,
// and each room may track no more than its own readers plus the relay's bound of live clients per
// process: 2 * maxAwarenessClientsPerRoom.
func TestRepeatedAwarenessFloodsDoNotGrowARoomsAwarenessWithoutLimit(t *testing.T) {
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

	const frames, perFrame = 16, 1024
	next := uint64(1_000_000)
	for frame := range frames {
		if frame%2 == 0 {
			if err := aw.ApplyUpdate(awarenessStates(next, perFrame, 1, floodState), "a peer of task A"); err != nil {
				t.Fatalf("frame %d: %v", frame, err)
			}
			if err := aw.ApplyUpdate(awarenessStates(next, perFrame, 1, "null"), "a peer of task A"); err != nil {
				t.Fatalf("frame %d's removal: %v", frame, err)
			}
		} else if err := aw.ApplyUpdate(awarenessStates(next, perFrame, 1, "null"), "a peer of task A"); err != nil {
			t.Fatalf("frame %d: %v", frame, err)
		}
		next += perFrame
	}

	bound := 2 * maxAwarenessClientsPerRoom
	waitFor(t, "task A's relay to have sent every batch of the frames", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == 0 && relaySentAll(pair.taskA, pair.artifactID)
	})
	onA, err := pair.taskA.heldAwarenessClients(pair.artifactID)
	if err != nil {
		t.Fatalf("read the awareness task A's room holds: %v", err)
	}
	if onA > bound {
		t.Errorf("after %d frames of %d invented awareness clients, task A's room tracks %d client entries, want at most %d: each frame grows the room by its clients until a purge two expiries later",
			frames, perFrame, onA, bound)
	}
	// What task B takes in arrives through room sync, on one lane, in the order task A's relay
	// sent it: alice's cursor, set on task A once every batch of the frames has been sent, reaches
	// bob on task B only after task B has applied every one of them.
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor, set on task A after every batch of the frames, reaching bob on task B")
	onB, err := pair.taskB.heldAwarenessClients(pair.artifactID)
	if err != nil {
		t.Fatalf("read the awareness task B's room holds: %v", err)
	}
	if onB > bound {
		t.Errorf("after %d frames of %d invented awareness clients on task A, task B's room tracks %d client entries, want at most %d: every relayed removal of a client B never held is stored as a tombstone",
			frames, perFrame, onB, bound)
	}
	t.Logf("task A's room tracks %d client entries, task B's %d (bound %d)", onA, onB, bound)
}
