package docs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// chainStructs is how many characters the store tests' update chains (marginChain): past
// unslottedApplyStructs, so its apply holds a grant of the pod's load concurrency, and enough that
// its structs rather than its bytes decide what a copy of it costs (copyWeight).
const chainStructs = 10_000

// marginChain is the update doc makes by nesting a text under its margin (marksMapName) and
// typing structs characters into it, each at the head of the text, ahead of the one typed before
// it. Each is an item of its own naming the one typed before it as its right origin, so no two
// merge and the update declares a struct for each, and one more for the text's entry in the
// margin. No rendering shows the margin, so the update is weighed by the state cap alone, and its
// apply scans nothing: each item lands at the head of the text, with nothing before it.
func marginChain(t *testing.T, doc *crdt.Doc, structs int) []byte {
	t.Helper()
	const key = "chain"
	held := doc.StateVector()
	margin := doc.GetMap(marksMapName)
	doc.Transact(func(txn *crdt.Transaction) { margin.Set(txn, key, crdt.NewTextPrelim()) })
	value, _ := margin.Get(key)
	text, isText := value.(*crdt.YText)
	if !isText {
		t.Fatalf("the margin holds %T under %q, want the nested text", value, key)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		for range structs {
			text.Insert(txn, 0, "x", nil)
		}
	})
	update := crdt.EncodeStateAsUpdateV1(doc, held)
	if declared := declaredStructs(update); declared != uint64(structs+1) {
		t.Fatalf("the chain's update declares %d structs, want its %d characters and the text's entry in the margin", declared, structs)
	}
	return update
}

// requireWeighedByItsStructs fails the test unless update's structs, and not its bytes, decide
// what a copy of it costs, so a grant of that cost is one the update's structs alone ask for.
func requireWeighedByItsStructs(t *testing.T, update []byte) {
	t.Helper()
	structs := declaredStructs(update)
	if bytesAlone := copyWeight(int64(len(update)), 0); storedCopyWeight(update) <= bytesAlone || structs <= unslottedApplyStructs {
		t.Fatalf("the %d-byte update declares %d structs and weighs %d, against %d on its bytes alone: want its structs past the %d an apply takes no slot for, and deciding its weight",
			len(update), structs, storedCopyWeight(update), bytesAlone, unslottedApplyStructs)
	}
}

// latestStoredUpdate is the update the document's head row stores.
func latestStoredUpdate(t *testing.T, service *Service, artifactID string) []byte {
	t.Helper()
	var update []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select update from doc_updates where artifact_id = $1 order by version desc limit 1
	`, artifactID).Scan(&update); err != nil {
		t.Fatalf("read the document's head row: %v", err)
	}
	return update
}

// A browser's update declaring more structs than an apply takes no slot for is decoded twice: by
// its apply, under the grant the hook takes for it (weighPeerUpdate), and again by the room's
// persistence worker, which reads the commit back into a fresh copy before it stores it
// (StoreUpdateStored) once the hook has given that grant back. The read-back is as heavy as the
// apply, so it is counted in the pod's load concurrency too: with the pool free it holds what the
// stored update's copy costs (storedCopyWeight) while it decodes. It took nothing before, so a
// step 2 of as many structs as fit under unslottedApplyBytes decoded there outside every bound
// of the pod.
//
// The grants are read off the pool as they are taken (loadSlots.taken), from the moment the store
// begins (beforeRoomAppend) and the apply's own grant has been given back, so what is read is the
// store's alone.
func TestAStoredBrowserUpdateHeavyInStructsIsCountedInThePoolWhileItIsReadBack(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	service.settle = time.Hour // no settlement takes a grant beside the store's
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document")
	update := marginChain(t, page.Doc, chainStructs)
	requireWeighedByItsStructs(t, update)

	pool := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	var storing atomic.Bool
	pool.taken = func(held int64) {
		if storing.Load() {
			taken.record(held)
		}
	}
	// The worker can reach the store before the hook has returned and given the apply's grant
	// back, so the store's grants are read from once the pool holds nothing.
	service.beforeRoomAppend = func(room string) error {
		if room != artifactID {
			return nil
		}
		for deadline := time.Now().Add(30 * time.Second); pool.held.Load() != 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				return errors.New("the apply never gave its grant back")
			}
		}
		storing.Store(true)
		return nil
	}
	before := storedHead(t, service.store, artifactID)
	if err := page.Write(ygsync.EncodeUpdate(update)); err != nil {
		t.Fatalf("send the update: %v", err)
	}
	waitFor(t, 30*time.Second, "the room to store the update", func() bool {
		return storedHead(t, service.store, artifactID) > before
	})
	if !storing.Load() {
		t.Fatal("the room stored the update without reaching the store's hook")
	}

	stored := latestStoredUpdate(t, service, artifactID)
	requireWeighedByItsStructs(t, stored)
	want := storedCopyWeight(stored)
	switch grants := taken.all(); {
	case len(grants) == 0:
		t.Fatalf("the room stored a %d-byte update declaring %d structs taking nothing of the free pool, want its read-back to hold the %d bytes its copy costs",
			len(stored), declaredStructs(stored), want)
	case grants[0] != want:
		t.Fatalf("the store's read-back held %d bytes of the pool, want the %d the stored update's %d bytes and %d structs cost (storedCopyWeight); the store took grants leaving %v held",
			grants[0], want, len(stored), declaredStructs(stored), grants)
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("%d bytes of the pool are held once the update is stored, want none", held)
	}
	t.Logf("the room's read-back of a %d-byte update declaring %d structs held %d bytes of the pool", len(stored), declaredStructs(stored), want)
}

// An append of updates the store is handed checks each by decoding it whole (validateUpdate), and
// is the append the room's persistence worker makes of a room's update no class holds
// (StoreUpdateStored). That decode is counted in the pod's load concurrency as the room's read-back
// is: with the pool free it holds what the update's copy costs (storedCopyWeight) while it
// decodes, and gives it back before the append's transaction begins.
func TestAnAppendedUpdateHeavyInStructsIsCountedInThePoolWhileItIsChecked(t *testing.T) {
	service, artifactID := newTestService(t)
	update := marginChain(t, crdt.New(), chainStructs)
	requireWeighedByItsStructs(t, update)
	want := storedCopyWeight(update)

	pool := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	pool.taken = taken.record
	if _, err := service.persistence.AppendUpdatesWithCredit(context.Background(), artifactID,
		[]BrowserUpdate{{Update: update, ContentChanged: true}}); err != nil {
		t.Fatalf("append the update: %v", err)
	}
	switch grants := taken.all(); {
	case len(grants) == 0:
		t.Fatalf("the store checked a %d-byte update declaring %d structs taking nothing of the free pool, want its decode to hold the %d bytes its copy costs",
			len(update), declaredStructs(update), want)
	case grants[0] != want:
		t.Fatalf("the store's check held %d bytes of the pool, want the %d the update's %d bytes and %d structs cost (storedCopyWeight); the append took grants leaving %v held",
			grants[0], want, len(update), declaredStructs(update), grants)
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("%d bytes of the pool are held once the update is appended, want none", held)
	}
}
