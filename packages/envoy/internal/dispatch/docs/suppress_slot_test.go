package docs

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Two writers queue suppression slots in one order, and their updates reach the room's worker in
// the other: a published write and a settlement's repair each prepare a slot before their
// transaction, and the document's lock can take the second one's first. Each update takes its own
// writer's slot, wherever that slot stands, and no slot is left queued
// (consumeSuppressedPersistence). Paired with the first slot instead, the update handed on first
// went by as no slot's and left its own slot queued.
func TestEachUpdateTakesItsOwnWritersSuppressionSlot(t *testing.T) {
	t.Parallel()
	published, repaired := []byte("a published write's update"), []byte("a repair's update")
	// A published write's slot is finished with its update as the room commits it
	// (onLoadDocument); a repair's commit records its update on the slot, and its writer finishes
	// the slot once its own transaction holds the update (finishSlots).
	publish := func(service *Service, slot *suppressSlot) {
		service.finishSuppressedPersistence(slot, published)
	}
	repair := func(service *Service, slot *suppressSlot) {
		service.recordSuppressedCommit(slot, repaired)
		service.finishSuppressedPersistence(slot, repaired)
	}
	for _, test := range []struct {
		name string
		// queued are the writers in the order they prepared their slots; handed is the order the
		// room's worker is handed their updates.
		queued []func(*Service, *suppressSlot)
		handed [][]byte
	}{
		{"a published write queued first and a repair handed on first", []func(*Service, *suppressSlot){publish, repair}, [][]byte{repaired, published}},
		{"a repair queued first and a published write handed on first", []func(*Service, *suppressSlot){repair, publish}, [][]byte{published, repaired}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &Service{}
			slots := make([]*suppressSlot, len(test.queued))
			for index := range test.queued {
				slots[index] = service.prepareSuppressedPersistence("room")
			}
			for index, writer := range test.queued {
				writer(service, slots[index])
			}
			for _, update := range test.handed {
				if !service.consumeSuppressedPersistence("room", update) {
					t.Fatalf("%q went by as no writer's: it did not take its own writer's slot", update)
				}
			}
			requireSlotsQueued(t, service, "room", 0)
		})
	}
}

// A repair's update that reaches the room's worker while another writer's slot stands first, and
// before the repair's own writer has stored it, waits for that writer: the worker removes the
// repair's queued class (finishUpdateClass) only once the repair is stored (storeRepair), so the
// class is still there for it. Taken by the first slot, the update went by at once, and the worker
// removed the class the repair's writer had yet to read (errUnclassifiedRepair). The test sees the
// worker's call parked on a slot before it finishes the repair's slot, so a call that waits for
// nothing fails it.
func TestARepairsUpdateWaitsForItsOwnWriterBehindAnotherWritersSlot(t *testing.T) {
	// Not parallel: it reads every goroutine's stack in the process (goroutinesIn), where another
	// test's goroutines would count as its own.
	service := &Service{}
	published, repaired := []byte("a published write's update"), []byte("a repair's update")
	ahead, own := service.prepareSuppressedPersistence("room"), service.prepareSuppressedPersistence("room")
	service.finishSuppressedPersistence(ahead, published)
	service.recordSuppressedCommit(own, repaired)
	consumed := startParkedConsume(t, service, repaired, "the repair's update")
	service.finishSuppressedPersistence(own, repaired)
	if !testwait.Receive(t, "the repair's update to take its slot once its writer finished it", consumed) {
		t.Fatal("the repair's update went by as no writer's: it did not take its own writer's slot")
	}
	if !service.consumeSuppressedPersistence("room", published) {
		t.Fatal("the published write's update went by as no writer's")
	}
	requireSlotsQueued(t, service, "room", 0)
}

// An update that is no writer's - a browser's - waits on the first slot queued, not on another,
// until that slot's writer has finished, and then goes by as no slot's, taking neither slot. Two
// writers' slots stand queued, neither finished: finishing the second leaves the call waiting, and
// finishing the first lets it go by with both slots still queued for their own writers' updates.
func TestABrowsersUpdateWaitsOnTheFirstSlotAndTakesNone(t *testing.T) {
	t.Parallel()
	testwait.Bubble(t, func(t testing.TB) {
		service := &Service{}
		firstUpdate, secondUpdate := []byte("the first writer's update"), []byte("the second writer's update")
		first, second := service.prepareSuppressedPersistence("room"), service.prepareSuppressedPersistence("room")
		defer service.finishSuppressedPersistence(first, firstUpdate)
		defer service.finishSuppressedPersistence(second, secondUpdate)
		consumed := make(chan bool, 1)
		go func() { consumed <- service.consumeSuppressedPersistence("room", []byte("a browser's update")) }()
		synctest.Wait()
		select {
		case taken := <-consumed:
			t.Fatalf("the browser's update went by (took a slot: %t) before either writer finished", taken)
		default:
		}

		service.finishSuppressedPersistence(second, secondUpdate)
		// Let any worker made runnable by finishing the second slot run until it blocks
		// or returns. A worker waiting on the first slot still cannot return.
		synctest.Wait()
		select {
		case taken := <-consumed:
			t.Fatalf("the browser's update went by (took a slot: %t) once the second slot was finished, before the first", taken)
		default:
		}
		service.finishSuppressedPersistence(first, firstUpdate)
		if testwait.Receive(t, "the browser's update to go by once the first writer finished", consumed) {
			t.Fatal("a browser's update took a writer's slot")
		}
		requireSlotsQueued(t, service, "room", 2)
		for _, update := range [][]byte{firstUpdate, secondUpdate} {
			if !service.consumeSuppressedPersistence("room", update) {
				t.Fatalf("%q went by as no writer's", update)
			}
		}
		requireSlotsQueued(t, service, "room", 0)
	})
}

// startParkedConsume hands update to service's persistence for "room" in a call of its own and
// returns once that call is parked on a slot's ready channel - a goroutine in
// consumeSuppressedPersistence whose state is [chan receive] - failing the test if the call returns
// first. Counting any goroutine in the function would count a call on its way to returning. The
// call's answer arrives on the channel returned.
func startParkedConsume(t *testing.T, service *Service, update []byte, what string) <-chan bool {
	t.Helper()
	parked := goroutinesIn("consumeSuppressedPersistence", "[chan receive]")
	consumed := make(chan bool, 1)
	go func() { consumed <- service.consumeSuppressedPersistence("room", update) }()
	waitFor(t, 10*time.Second, what+" to park on a slot in the room's persistence", func() bool {
		select {
		case taken := <-consumed:
			t.Fatalf("%s went by (took a slot: %t) before the writer it waits for finished", what, taken)
		default:
		}
		return goroutinesIn("consumeSuppressedPersistence", "[chan receive]") > parked
	})
	return consumed
}

// A settlement's block-id stamp is stored after two writers' updates reached the room in the
// other order than they queued their slots, as a published write's and a repair's do when the
// document's lock takes their transactions in the other order. Each update takes its own writer's
// slot, so none is left queued, and the stamp's update keeps its class until the settlement has
// stored the stamp. Paired with the first slot, the second writer's slot stayed queued, the stamp's
// update met it first and went by as no slot's, and the worker removed the stamp's class before
// the settlement read it: the room failed with "store document identity update: the room recorded
// no class for a repair written into it" (TestStoredOrderSoak, job 140488601479).
//
// The two writers' updates are applied as a publish applies a committed live write's
// (publishLiveUpdate), with slots queued ahead of their Applies. The store holds neither, and no
// open live write stands for them: what this test pins is the worker's pairing, which reads
// neither, their classes being queued stored. It is no test of a published write's durability.
// The block the settlement stamps is written before them, so it builds only on stored history and
// a load of the store holds it: the store check reads that block before the settlement, without
// an id, and after it, with one.
func TestAStampIsStoredAfterTwoWritersCommittedOutOfTheirSlotsOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the room has no block to stamp: the settlement would write no stamp")
	}
	waitForPersistedProofText(t, service.store, artifactID, "before\n\nadded\n")
	if repairs := storedBlocksWithoutID(t, service, artifactID); repairs != 1 {
		t.Fatalf("the store holds %d blocks without an id before the settlement, want the 1 it is to stamp", repairs)
	}
	live := service.srv.GetDoc(artifactID)
	first, second := service.prepareSuppressedPersistence(artifactID), service.prepareSuppressedPersistence(artifactID)
	publish := func(slot *suppressSlot, markdown string) {
		_, update := peerEdit(t, live, appendBlocks(t, markdown))
		origin := &liveWriteOrigin{slot: slot}
		if _, err := service.applyCaptured(ctx, artifactID, origin, func(doc *crdt.Doc) error {
			return crdt.ApplyUpdateV1(doc, update, origin)
		}); err != nil {
			t.Fatalf("publish %q: %v", markdown, err)
		}
	}
	publish(second, "The second writer's paragraph.\n")
	publish(first, "The first writer's paragraph.\n")

	var reached bool
	service.afterSettleReconcile = func(room string) {
		if room != artifactID || reached {
			return
		}
		reached = true
		// The stamp has committed: its update reaches the room's worker before the settlement
		// stores the stamp (commitRepairs). A class already gone is the worker's having removed
		// it, which is the fault this test catches.
		testwait.Until(t, "the stamp's update to reach the room's persistence", func() (string, bool) {
			state := service.lockExistingState(artifactID)
			if state == nil {
				return "no room state", true
			}
			defer service.unlockState(artifactID, state)
			for _, class := range state.updateClasses {
				if !class.durable {
					if class.handed {
						return "the stamp's class reached persistence", true
					}
					return "the stamp's class has not reached persistence", false
				}
			}
			return "no undurable class", true
		})
	}
	settleCurrentGeneration(t, service, artifactID)
	service.afterSettleReconcile = nil
	if !reached {
		t.Fatal("the settlement never reached the window between its stamp and its store")
	}
	if failure := service.roomFailure(artifactID); failure != nil {
		t.Fatalf("the room failed: %v", failure)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp")
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
		t.Fatalf("the room holds %d blocks without an id: the settlement wrote no stamp", repairs)
	}
	if repairs := storedBlocksWithoutID(t, service, artifactID); repairs != 0 {
		t.Fatalf("the store holds %d blocks without an id: the stamp was not stored", repairs)
	}
}

// storedBlocksWithoutID loads artifactID's document from the store and counts the blocks it holds
// without a block id, once it has found the block "added" there: a load that misses the block
// would count none, as a store that holds the stamp does.
func storedBlocksWithoutID(t *testing.T, service *Service, artifactID string) int {
	t.Helper()
	stored := loadDocument(t, service.persistence.(*PgVersioned), artifactID)
	if markdown := docMarkdown(stored.Doc); !strings.Contains(markdown, "added") {
		t.Fatalf("the stored document renders %q, without the block the settlement stamps", markdown)
	}
	tree, err := treeOf(stored.Doc)
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	return pmdoc.BlockIDRepairCount(tree)
}

// requireSlotsQueued fails the test unless want suppression slots are queued for room.
func requireSlotsQueued(t testing.TB, service *Service, room string, want int) {
	t.Helper()
	service.suppressMu.Lock()
	queued := len(service.suppressed[room])
	service.suppressMu.Unlock()
	if queued != want {
		t.Fatalf("%d suppression slots queued for the room, want %d", queued, want)
	}
}
