package docs

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// What a frame parks is charged to the room budget where it parks: the apply's report counts each
// parked struct among the structs the room holds (crdt.ApplyReport.Structs), the drain of a commit
// that only parked runs after the transaction, since no update observer fires for it
// (onLoadDocument's OnAfterTransaction), and the charge weighs the room on that report
// (renderedReplica.chargeAdded, structsHeld, roomWeight). So the parked content a later drain
// will land is already in the room's charge before any update fills its gap.
//
// A browser of a real room sends 2,000 GC structs behind a clock the room does not hold, and the
// budget then charges the room for its tracked state and its store's structs with the 2,000 parked
// ones beside them. Charged from the store alone, the room was weighed as if it parked nothing.
func TestARoomIsChargedForTheStructsAFrameOnlyParks(t *testing.T) {
	const parked = 2_000
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.settle = time.Hour // no settlement writes into the room while it is weighed
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	room := createDocuments(t, database, 1)[0]
	browser := connectBrowser(t, httpServer.URL, room, "alice")
	live := service.srv.GetDoc(room)
	if live == nil {
		t.Fatal("the room is not resident while its browser is connected")
	}
	replica := service.replicaOf(live)
	if replica == nil || replica.roomCharge() == nil {
		t.Fatal("the loaded room's replica holds no charge in the room budget")
	}
	if err := browser.Write(ygsync.EncodeUpdate(parkedGCFrame(8_100_001, parked))); err != nil {
		t.Fatalf("send the frame the room parks: %v", err)
	}
	waitFor(t, roomWait(t), "the room to park the frame", func() bool { return live.PendingStats().Items == parked })
	held := int64(live.StructCount())
	var tracked, used int64
	charged := testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		tracked, used = replica.roomStateBytes(), budgetUsed(service)
		return used == roomWeight(tracked, held+parked)
	})
	with, without := roomWeight(tracked, held+parked), roomWeight(tracked, held)
	t.Logf("the room holds %d structs in its store and %d parked, tracked at %d bytes of state: the budget holds %d, against %d with the parked structs and %d without",
		held, parked, tracked, used, with, without)
	if with == without {
		t.Fatalf("the room weighs %d with its %d parked structs and without them, so this run says nothing about charging them: its state term sets the charge",
			with, parked)
	}
	if !charged {
		t.Fatalf("the budget holds %d bytes for a room parking %d structs, want the %d its %d tracked bytes and its %d structs with the parked ones come to (%d without them)",
			used, parked, with, tracked, held+parked, without)
	}
}
