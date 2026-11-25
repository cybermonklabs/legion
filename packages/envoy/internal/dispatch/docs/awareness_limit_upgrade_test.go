package docs

import "testing"

// The per-socket bound holds on a real websocket upgrade, not only on a connection a test hands
// the filter: a socket that announces ten awareness clients reaches the other readers of its room,
// on its own process and on the other, as maxAwarenessClientsPerSocket of them. gorilla reads an
// upgraded connection through the buffered reader the hijack returns, which in the gorilla this
// repository's workspace builds is not reset onto the connection the hijack hands back, so a
// filter that only wraps the connection never sees a frame.
func TestADocumentSocketHoldsItsAwarenessBoundThroughARealUpgrade(t *testing.T) {
	t.Parallel()
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	carol := pair.openReader(t, pair.serverA, "carol")
	bob := pair.openReader(t, pair.serverB, "bob")
	mallory := pair.openReader(t, pair.serverA, "mallory")

	const first, announced = uint64(500_000), 10
	for client := first; client < first+announced; client++ {
		if err := mallory.SetAwareness(client, map[string]any{"user": map[string]any{"name": "mallory"}}); err != nil {
			t.Fatalf("announce client %d: %v", client, err)
		}
	}
	// The marker: the socket's first client, which the bound lets through, moved once more on the
	// same socket. The socket's frames reach its room in order, and the room's changes reach carol
	// on task A and, through the relay's one lane, bob on task B in order, so a reader holding the
	// marker holds whatever the frames before it left there.
	marker := map[string]any{"user": map[string]any{"name": "mallory"}, "cursor": map[string]any{"anchor": 1, "head": 1}}
	setAwareness(t, mallory, first, marker)
	requireAwareness(t, carol, holdsAwareness(first, marker), "mallory's marker, sent after her ten clients, reaching carol on task A")
	requireAwareness(t, bob, holdsAwareness(first, marker), "mallory's marker, sent after her ten clients, reaching bob on task B")
	onA, onB := 0, 0
	for client := range carol.AwarenessStates() {
		if client >= first && client < first+announced {
			onA++
		}
	}
	for client := range bob.AwarenessStates() {
		if client >= first && client < first+announced {
			onB++
		}
	}
	if onA > maxAwarenessClientsPerSocket || onB > maxAwarenessClientsPerSocket {
		t.Fatalf("one document socket announced %d awareness clients and carol on task A holds %d of them, bob on task B %d; want at most %d each (maxAwarenessClientsPerSocket): the filter never read the socket's frames",
			announced, onA, onB, maxAwarenessClientsPerSocket)
	}
	if onA == 0 {
		t.Fatalf("carol on task A holds none of the socket's clients, so the socket's awareness never reached its room")
	}
}
