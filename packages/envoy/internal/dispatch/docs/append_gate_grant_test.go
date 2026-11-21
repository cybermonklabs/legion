package docs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// besideAFullPoolWait bounds the wait for a keystroke's store that a full pool must not hold up
// (TestTheWorkersGateDecodesAFoldedHistoryBesideAFullPool): one keystroke stored on a document of
// one paragraph, a decode of a few hundred bytes and three round trips, where the longest whole
// run of a test that waits roomWait measured 2.1 s at load 17 (roomWaitCap). A store the pool
// holds up never comes while the test holds the pool, so the test fails at this bound by name
// rather than at roomWaitCap's two minutes or the binary's timeout.
const besideAFullPoolWait = 30 * time.Second

// The room's persistence worker is the one reader of the room's queue of updates to store, and a
// heavy apply holding its grant of the pod's load concurrency through its commit waits on that
// queue while it is full (ygo's persistence observer). So the worker's gate decodes a history
// replaced under its room taking only bytes free at once (storedHistoryHolds, withFreeLoadOnly):
// with every byte of the pool held, a keystroke whose append finds the history a compaction
// folded past its room's cursor is stored, the fold of that history and the decode of the
// keystroke onto it running beside the pool's holders. Before, the fold waited for the pool while
// the worker held the document's lock and a connection of the room-appends pool, and the store
// never came while the pool stayed full.
//
// The history past the cursor is one folded row, which the gate decodes as one run
// (runsDocument), or that row and one written after it, which the gate folds and reads back
// (fold): the two grants a gate's fold takes, one each.
func TestTheWorkersGateDecodesAFoldedHistoryBesideAFullPool(t *testing.T) {
	for _, history := range []struct {
		name string
		// writtenAfter writes a row after the fold, so the history past the room's cursor holds
		// two rows.
		writtenAfter bool
	}{
		{"of one folded row", false},
		{"of a folded row and a row written after it", true},
	} {
		t.Run(history.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "before")
			service, server := newTestServiceInstance(t, database)
			seedServiceText(t, service, artifactID, "before")
			waitForLandedAppends(t, service, artifactID)
			appends, ok := service.persistence.(*PgVersioned)
			if !ok {
				t.Fatalf("the service appends through %T, which has no seam for the folded-history path", service.persistence)
			}
			var held atomic.Int32
			appends.storedHistoryHeld = func(room string) {
				if room == artifactID {
					held.Add(1)
				}
			}
			stored := make(chan struct{})
			var committed sync.Once
			appends.afterAppendCommit = func(room string, _ persistence.Version) {
				if room == artifactID {
					committed.Do(func() { close(stored) })
				}
			}
			alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
			// The connect stores nothing: alice's copy holds nothing the room lacks, and a
			// transaction that changes nothing commits no update (ygo calls no update observer for
			// one, reearth/ygo#350), so her keystroke is the first append to commit from here.
			head := storedHead(t, database, artifactID)

			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the transaction that holds the document's lock: %v", err)
			}
			defer holder.Rollback(ctx)
			if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
				t.Fatalf("hold the document's lock: %v", err)
			}
			typeOver(t, alice, "before", "first keystroke")
			if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
				t.Fatal("alice's append never came to wait on the document's lock")
			}
			foldHistoryPastRoom(t, holder, artifactID, head)
			replacedHead := head + 1
			if history.writtenAfter {
				replacedHead++
				writeAfterTheFold(t, holder, artifactID, replacedHead)
			}

			// Every byte of the pool is held from before the worker takes the document's lock
			// until the test ends, which gives it back before the service shuts down.
			slots := newLoadSlots(1)
			var waited atomic.Bool
			slots.waiting = func() { waited.Store(true) }
			pool := useLoadSlots(t, slots)
			filler := newLoadSlot()
			if !filler.tryTake(pool.poolBytes()) {
				t.Fatalf("the empty pool refused its %d bytes", pool.poolBytes())
			}
			t.Cleanup(filler.release)
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("release the document's lock: %v", err)
			}

			wait := min(roomWait(t), besideAFullPoolWait)
			select {
			case <-stored:
			case <-time.After(wait):
				t.Fatalf("alice's keystroke on the folded history was not stored within %s while every byte of the pool was held (a taker began to wait for the pool: %t): the worker's gate waits for the pool holding the document's lock",
					wait, waited.Load())
			}
			if got := held.Load(); got != 1 {
				t.Fatalf("%d appends were admitted on the folded history, want alice's keystroke's alone: nothing here is tested", got)
			}
			if moved := storedHead(t, database, artifactID); moved <= replacedHead {
				t.Fatalf("the document stands at update %d once alice's keystroke committed, want past the folded history's head %d", moved, replacedHead)
			}
			if inUse := pool.held.Load(); inUse != pool.poolBytes() {
				t.Fatalf("%d of the pool's %d bytes are held once the keystroke is stored, want the test's whole hold and nothing else", inUse, pool.poolBytes())
			}
		})
	}
}

// writeAfterTheFold writes artifactID once more as another process would, in holder, which holds
// the document's lock, at version at, the row after the one foldHistoryPastRoom folded the
// history into: a key of a map of its own, so the history still holds every item a room's browser
// updates build on, now in two rows.
func writeAfterTheFold(t *testing.T, holder pgx.Tx, artifactID string, at int64) {
	t.Helper()
	ctx := context.Background()
	through := persistence.Version(at - 1)
	stored, err := documentThrough(ctx, artifactID, through, storedUpdates(ctx, holder, artifactID, through), maxUpdateItems)
	if err != nil {
		t.Fatalf("read the folded document: %v", err)
	}
	before := stored.StateVector()
	written := stored.GetMap("written-after-the-fold")
	stored.Transact(func(txn *crdt.Transaction) {
		written.Set(txn, "key", "value")
	})
	if _, err := holder.Exec(ctx, `insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, false)`,
		artifactID, at, crdt.EncodeStateAsUpdateV1(stored, before)); err != nil {
		t.Fatalf("write the document after the fold: %v", err)
	}
}

// The gate's check of a history replaced under a room applies the run it would store to the
// document that history makes (storedHistoryHolds), a whole decode of each of the run's updates.
// That decode is counted in the pod's load concurrency: with the pool free it holds what the
// run's copy costs (storedCopyWeight) while it decodes, both on the room's persistence worker,
// whose context takes only bytes free at once (withFreeLoadOnly), and in a transaction's flush of
// the room, whose context waits for them. It took nothing before: the fold's grants were given
// back before it decoded.
func TestTheGatesDecodeOfARunOnAReplacedHistoryIsCountedInThePool(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant beside the gate's
	seedServiceText(t, service, artifactID, "before")
	head := storedHead(t, service.store, artifactID)
	run := marginChain(t, crdt.New(), chainStructs)
	requireWeighedByItsStructs(t, run)
	want := storedCopyWeight(run)
	for _, gate := range []struct {
		name string
		ctx  context.Context
	}{
		{"on the room's persistence worker", withFreeLoadOnly(context.Background())},
		{"in a transaction's flush of the room", context.Background()},
	} {
		t.Run(gate.name, func(t *testing.T) {
			slots := newLoadSlots(1)
			var taken slotsHeld
			slots.taken = taken.record
			pool := useLoadSlots(t, slots)
			holds, err := storedHistoryHolds(gate.ctx, service.store.Pool, artifactID, head, []BrowserUpdate{{Update: run, ContentChanged: true}}, maxUpdateItems)
			if err != nil || !holds {
				t.Fatalf("the stored history holds the run = %t (%v), want it held: the chain builds on its own items alone, and nothing here is tested otherwise", holds, err)
			}
			switch grants := taken.all(); {
			case len(grants) == 0:
				t.Fatalf("the gate decoded the history and a %d-byte run declaring %d structs taking nothing of the free pool, want the run's decode to hold the %d bytes its copy costs",
					len(run), declaredStructs(run), want)
			case grants[len(grants)-1] != want:
				t.Fatalf("the gate's decodes took grants leaving %v held, want the last, the run's decode, to hold the %d bytes its %d bytes and %d structs cost (storedCopyWeight)",
					grants, want, len(run), declaredStructs(run))
			}
			if held := pool.held.Load(); held != 0 {
				t.Fatalf("%d bytes of the pool are held once the gate has judged the run, want none", held)
			}
			t.Logf("the gate's decode of a %d-byte run declaring %d structs held %d bytes of the pool", len(run), declaredStructs(run), want)
		})
	}
}
