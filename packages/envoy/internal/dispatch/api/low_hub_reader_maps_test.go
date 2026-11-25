package api

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheHubForgetsAReaderWhoseLastViewHasGone: a generation of the delivery hub keeps, by each
// reader's part of a view key, the unfetchable count that reader's views last read and the
// freshness verdict last sent to its pages. Once a reader's last view has gone, the generation
// keeps nothing of that reader, however long it runs on for others. alice and carol, who see other
// projects, each hold a timeline on one socket: carol's sub ends, and the generation holds
// alice's entries alone; alice's ends, and it holds none.
//
// Control: leaveLocked drops a view and keeps its reader's entries; carol's stay once her sub has
// ended, and the test fails naming them.
func TestTheHubForgetsAReaderWhoseLastViewHasGone(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seedHubSettings(t, seed.database)
	rig := hubOn(t, seed.handler, seed.database, seed.deps, newNoonHubClock(), nil)
	reader := func(login string) model.Actor {
		t.Helper()
		resolved, err := seed.server.readerFor(t.Context(), model.BrowserActor(login))
		if err != nil {
			t.Fatalf("resolve what %s sees: %v", login, err)
		}
		return resolved
	}
	alice, carol := reader(seedAlice), reader(seedCarol)
	aliceKey, carolKey := deliveryReaderKey(alice), deliveryReaderKey(carol)
	if aliceKey == carolKey {
		t.Fatalf("alice and carol share the reader key %q, so this test proves nothing", aliceKey)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "a", View: presetView(deliveryLiveTimeline, 7, ""), Reader: alice})
	rig.hub.Subscribe(conn, deliverySubscription{Sub: "c", View: presetView(deliveryLiveTimeline, 7, ""), Reader: carol})
	// Each reader's first read sets its verdict silently, so the two snapshots come with none.
	for answered := map[string]bool{}; len(answered) < 2; {
		if snapshot, ok := conn.next(t, 5*time.Second, "the snapshots of a and c").(DeliveryLiveSnapshot); ok {
			answered[snapshot.Sub] = true
		}
	}
	rig.hub.mu.Lock()
	g := rig.hub.generation
	rig.hub.mu.Unlock()
	// held is every entry the generation keeps of a reader, each named by its map and the reader's
	// key, sorted. It is read under the hub's lock, which each recompute that sets an entry holds
	// from the snapshot it sends to the entry it sets.
	held := func() []string {
		rig.hub.mu.Lock()
		defer rig.hub.mu.Unlock()
		var entries []string
		for key := range g.unfetchable {
			entries = append(entries, "unfetchable "+key)
		}
		for key := range g.sent {
			entries = append(entries, "sent "+key)
		}
		slices.Sort(entries)
		return entries
	}
	of := func(keys ...string) []string {
		var entries []string
		for _, key := range keys {
			entries = append(entries, "unfetchable "+key, "sent "+key)
		}
		slices.Sort(entries)
		return entries
	}

	if got, want := held(), of(aliceKey, carolKey); !slices.Equal(got, want) {
		t.Fatalf("with alice's and carol's timelines open the generation holds %q, want %q", got, want)
	}
	rig.hub.Unsubscribe(conn, "c")
	if got, want := held(), of(aliceKey); !slices.Equal(got, want) {
		t.Errorf("after carol's last sub ended the generation holds %q, want alice's alone, %q", got, want)
	}
	rig.hub.Unsubscribe(conn, "a")
	if got := held(); len(got) != 0 {
		t.Errorf("after every sub ended the generation holds %q, want nothing", got)
	}
}
