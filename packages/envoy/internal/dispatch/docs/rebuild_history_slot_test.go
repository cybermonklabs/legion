package docs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A rebuild of a document no room of this server holds reads its stored history and decodes it
// before it decides anything (rebuildSource), and the read, the decode and the check's re-scan of
// it run under one grant of the pod's load concurrency, taken at copyWeight of the bytes the read
// returned (loadDecodedHistory, nearACap under the grant the context carries). With the pool full,
// the rebuild waits rather than decoding beside the holder, and once those bytes are given back it
// runs, holding nothing of the pool after it. TestARebuildOfAResidentDocumentTakesASlotBeforeItWeighsIt
// holds the same of a resident room's check.
//
// Two histories are rebuilt, and the first grant each rebuild takes is required to be that copy's
// weight, copyWeight of the bytes its read returned, which no later step of a rebuild takes: the
// seed's validation is weighed on the seed (RebuildTx). One history loads and stands near no cap,
// so the rebuild decodes it and re-scans it and answers ErrDocumentLoads. One ygo cannot decode -
// the store hands the read one byte that does not decode, as rebuildCaptureStore does - so the
// rebuild answers from the decode alone and rebuilds the document from its latest version: no
// re-scan follows to take a grant after the decode, so this is the row that holds the decode
// itself inside the grant. With the read's own grant taken out, the first a rebuild of it waited
// for was the seed's.
func TestARebuildOfAStoredHistoryTakesASlotBeforeItDecodesIt(t *testing.T) {
	for _, row := range []struct {
		name string
		// document is the service the rebuild runs on, the document it rebuilds, and the bytes
		// of stored history the rebuild's read returns.
		document func(t *testing.T) (service *Service, artifactID string, read int64)
		// answered is what the rebuild answers once the slot is free.
		answered func(err error) bool
		want     string
	}{
		{
			name: "a history that loads and stands near no cap",
			document: func(t *testing.T) (*Service, string, int64) {
				service, artifactID := newTestService(t)
				storeHistory(t, service, artifactID, proseState(t, 16<<10))
				loaded, err := service.persistence.Load(context.Background(), artifactID)
				if err != nil {
					t.Fatal(err)
				}
				return service, artifactID, int64(len(loaded.Update))
			},
			answered: func(err error) bool { return errors.Is(err, ErrDocumentLoads) },
			want:     "ErrDocumentLoads",
		},
		{
			name: "a history ygo cannot decode",
			document: func(t *testing.T) (*Service, string, int64) {
				database := storetest.Open(t)
				artifactID := createDocument(t, database, "before\n")
				deps := testServiceDeps(database)
				deps.Persistence = &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}
				service := New(deps)
				t.Cleanup(func() {
					if err := service.Shutdown(context.Background()); err != nil {
						t.Errorf("shutdown document service: %v", err)
					}
				})
				return service, artifactID, 1
			},
			answered: func(err error) bool { return err == nil },
			want:     "the document rebuilt from its latest version",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			service, artifactID, read := row.document(t)
			service.settle = time.Hour
			if live := service.srv.GetDoc(artifactID); live != nil {
				t.Fatal("the document's room is resident, want a rebuild that reads the stored history")
			}
			slots := useLoadSlots(t, newLoadSlots(1))
			holder := newLoadSlot()
			if !holder.tryTake(loadSlotBytes) {
				t.Fatal("the empty pool refused a whole slot")
			}
			waiting := make(chan struct{})
			var waitOnce sync.Once
			slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
			var taken slotsHeld
			slots.taken = taken.record
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin rebuild: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			answered := make(chan error, 1)
			go func() {
				_, _, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
				answered <- err
			}()
			select {
			case <-waiting:
			case err := <-answered:
				t.Fatalf("the rebuild answered %v while the only slot was held: it decoded the stored history outside the concurrency", err)
			case <-time.After(30 * time.Second):
				t.Fatal("the rebuild neither waited for a slot nor answered in 30 s")
			}
			select {
			case err := <-answered:
				t.Fatalf("the rebuild answered %v while the only slot was held", err)
			case <-time.After(200 * time.Millisecond):
			}
			holder.release()
			select {
			case err := <-answered:
				if !row.answered(err) {
					t.Fatalf("the rebuild answered %v once the slot was free, want %s", err, row.want)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the rebuild did not answer in 30 s once the slot was free")
			}
			weight := copyWeight(read, 0)
			if got := taken.all(); len(got) == 0 || got[0] != weight {
				t.Fatalf("the rebuild's grants held %v bytes of the pool as each was taken, want the first to be the %d of a copy of the %d-byte history it read",
					got, weight, read)
			}
			if held := slots.held.Load(); held != 0 {
				t.Fatalf("the rebuild still holds %d bytes once it has answered", held)
			}
		})
	}
}
