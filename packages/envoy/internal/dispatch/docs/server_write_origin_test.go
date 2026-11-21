package docs

import (
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// A server write whose apply parks something leaves the room's document no reference to the
// write's origin, and so none to the update it applied (applyServerWrite). The room's document
// records the origin of each apply that parked a struct or a delete range, to name the senders a
// discarded drain closes (crdt.Doc.SetDiscardRefusedDrain, closeDiscardedSenders); a server write's
// origin holds its update (writtenApply.written), and only a browser is ever closed. Each row's
// write is a published live write's update or a catch-up's batch of rows, carrying a paragraph of
// 512 KiB the room took in an earlier catch-up and one struct or one delete range under a client
// the room has never seen, which parks. Once it has applied and its rows are stored, and the test
// has let go of the update, a weak pointer to the update's bytes is nil after a collection. Before
// the room forgot the origin, the record held it, and the update with it, for as long as what the
// write parked stayed parked; the live heap the test logs, read before the update is made and
// after the apply, shows what it held.
//
// The pointer is read right after the apply. Each of these writes subscribes a closure that picks
// out its own commit and holds its origin (applyRemoteBatch, applyCaptured), and before
// theorem-labs/ygo v1.55.1-theorem.13 an unsubscribe left the closure in the slot of the observer
// list it vacated, so the room kept the update reachable until a later subscription took the slot.
func TestAServerWriteThatParksLeavesTheRoomNoReferenceToItsUpdate(t *testing.T) {
	const client = 7_600_003
	for _, write := range []struct {
		name  string
		apply func(t *testing.T, service *Service, artifactID string, room *crdt.Doc, update []byte)
	}{
		{"a catch-up", func(t *testing.T, service *Service, artifactID string, room *crdt.Doc, update []byte) {
			if _, open, err := service.applyRemoteBatch(artifactID, room, update); !open || err != nil {
				t.Fatalf("the catch-up answered open %t, %v", open, err)
			}
		}},
		{"a live write's publish", func(t *testing.T, service *Service, artifactID string, _ *crdt.Doc, update []byte) {
			if err := service.publishLiveUpdate(artifactID, update, 0, publishedRendering{}, weak.Pointer[crdt.Doc]{}, nil); err != nil {
				t.Fatalf("the publish: %v", err)
			}
		}},
	} {
		for _, parks := range []struct {
			name   string
			frame  []byte
			parked func(crdt.PendingStats) int
		}{
			{"a struct", newV1Frame(1).group(1, client, 1).raw(structStringRef | infoHasOrigin).uint(client).uint(0).str("x").noDeletes(),
				func(stats crdt.PendingStats) int { return stats.Items }},
			{"a delete range", deleteRangesFrom(client, 1),
				func(stats crdt.PendingStats) int { return stats.DeleteRanges }},
		} {
			t.Run(write.name+" that parks "+parks.name, func(t *testing.T) {
				service, artifactID, serverURL := clientIDRoom(t)
				service.settle = time.Hour
				page := openPage(t, serverURL, artifactID, "alice")
				awaitAnswer(t, page, "the document")
				room := roomDocument(t, service, artifactID)
				writer := newDocumentCopy()
				if err := crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
					t.Fatal(err)
				}
				held := docstest.Transact(writer, func(txn *crdt.Transaction) {
					fragment := txn.GetXmlFragment(fragmentName)
					paragraph := crdt.NewYXmlElement("paragraph")
					fragment.InsertElement(txn, fragment.Len(), paragraph)
					run := crdt.NewYXmlText()
					paragraph.InsertText(txn, 0, run)
					run.Insert(txn, 0, strings.Repeat("p", 512<<10), nil)
				})
				if _, open, err := service.applyRemoteBatch(artifactID, room, held); !open || err != nil {
					t.Fatalf("the catch-up of the paragraph answered open %t, %v", open, err)
				}
				// One write of the same kind first, so the heap read before the update already
				// holds what the room builds on its first write of that kind.
				write.apply(t, service, artifactID, room, docstest.Transact(writer, func(txn *crdt.Transaction) {
					texts := paragraphTextsOf(txn.GetXmlFragment(fragmentName))
					texts[len(texts)-1].Insert(txn, 0, "q", nil)
				}))
				waitForLandedAppends(t, service, artifactID)

				before := liveHeap()
				update, err := crdt.MergeUpdatesV1(held, parks.frame)
				if err != nil {
					t.Fatal(err)
				}
				size := len(update)
				written := weak.Make(&update[0])
				write.apply(t, service, artifactID, room, update)
				update = nil
				if got := parks.parked(room.PendingStats()); got != 1 {
					t.Fatalf("after %s the room parks %+v, want %s", write.name, room.PendingStats(), parks.name)
				}
				waitForLandedAppends(t, service, artifactID)
				after := liveHeap()
				t.Logf("%s of %d bytes that parks %s: the live heap went from %d bytes before the update was made to %d after its apply, %+d",
					write.name, size, parks.name, before, after, int64(after)-int64(before))
				if written.Value() != nil {
					t.Errorf("after %s that parked %s, the room still holds the write's %d-byte update",
						write.name, parks.name, size)
				}
				runtime.KeepAlive(held)
				runtime.KeepAlive(writer)
			})
		}
	}
}

// liveHeap is the heap the process holds live, read after two collections.
func liveHeap() uint64 {
	var stats runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}
