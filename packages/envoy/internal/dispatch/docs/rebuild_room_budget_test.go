package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A rebuild of a resident room adds nothing to the room budget. It closes the room before it
// reads the history it replaces (evictBeforeRebuild), and the close's unload retires the room's
// charge (releaseUnloadedRoom), so by the rebuild's commit the budget charges the document
// nothing; a load while the rebuild's transaction runs is refused (holdRebuild,
// refuseIfRebuilding), so nothing is charged for the document until the commit; and the browser
// that reconnects once it has committed loads the rebuilt history, whose room the budget charges
// at its own load: roomWeight of the rebuilt state that load decoded and the structs that state
// holds (chargeLoadedRoom). Without the close, the room holding the replaced history stayed
// resident through the whole rebuild, still charged, beside the history that replaced it.
//
// The document stands near the scan total (tangledHistory, as
// TestRebuildingADocumentNearTheScanTotalLeavesItWithASmallScanTotalAndReadingTheSame builds it),
// a cap the route rebuilds a resident room for, and is loaded by a browser that stays connected
// until the rebuild's close ends its socket.
func TestARebuildRetiresTheResidentRoomsChargeAndTheReloadedRoomIsChargedItsOwnWeight(t *testing.T) {
	state := tangledHistory(t, 2_000, roomScanTotal-honestHandoffScans)
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement loads the room or writes into it while it is weighed
	storeHistory(t, service, artifactID, state)
	closeResidentRoom(t, service, artifactID)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	ctx := context.Background()

	held := connectBrowser(t, server.URL, artifactID, "alice")
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the document's room is not resident with a browser connected")
	}
	charges, used, _ := service.RoomChargesForTest()
	resident := charges[artifactID]
	if resident == 0 || used != resident {
		t.Fatalf("the budget charges the resident room %d of the %d bytes it holds, want a charge that is the whole of it", resident, used)
	}

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	report, _, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("rebuild the resident room near the scan total: %v", err)
	}
	t.Logf("rebuilt with its room resident and charged %d bytes: %s", resident, report.ValidationError)
	if charges, used, _ := service.RoomChargesForTest(); charges[artifactID] != 0 || used != 0 {
		t.Fatalf("with the rebuild about to commit, the budget charges the document %d of %d bytes, want the resident room's %d retired by its close",
			charges[artifactID], used, resident)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the room holding the replaced history is still resident with the rebuild about to commit")
	}
	if err := service.warmLiveDocument(ctx, artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("a load while the rebuild holds the room answered %v, want ErrServiceUnavailable", err)
	}
	if charges, used, _ := service.RoomChargesForTest(); charges[artifactID] != 0 || used != 0 {
		t.Fatalf("a load refused while the rebuild holds the room left the budget charging the document %d of %d bytes, want nothing", charges[artifactID], used)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
	if charges, used, _ := service.RoomChargesForTest(); charges[artifactID] != 0 || used != 0 {
		t.Fatalf("once the rebuild committed the budget charges the document %d of %d bytes, before any browser reloaded it, want nothing", charges[artifactID], used)
	}
	select {
	case <-held.Ended:
	case <-time.After(roomWait(t)):
		t.Fatal("the browser's socket stayed open after the rebuild closed its room")
	}

	connectBrowser(t, server.URL, artifactID, "alice")
	stored, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := decodedHistory(t, stored.Update)
	want := roomWeight(int64(len(stored.Update)), int64(heldStructs(rebuilt)))
	charges, used, _ = service.RoomChargesForTest()
	t.Logf("the resident room was charged %d bytes; the room reloaded from the %d-byte rebuilt state holding %d structs is charged %d",
		resident, len(stored.Update), heldStructs(rebuilt), charges[artifactID])
	if charges[artifactID] != want || used != want {
		t.Fatalf("the budget charges the reloaded room %d of %d bytes, want the rebuilt state's own weight, %d: its %d bytes and %d structs at both copies",
			charges[artifactID], used, want, len(stored.Update), heldStructs(rebuilt))
	}
}
