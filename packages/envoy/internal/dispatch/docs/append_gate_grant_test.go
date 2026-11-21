package docs

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// besideAFullPoolWait bounds the wait for work of a room's persistence worker that waits for the
// pod's load concurrency while a heavy apply's grant fills it (apply_grant_release_test.go): one
// keystroke stored on a document of one paragraph, a decode of a few hundred bytes and three round
// trips (TestTheWorkersGateFoldWaitsForAPoolAHeavyApplyGaveBack), and the close of a room of three
// paragraphs, whose worker compacts three rows as it exits
// (TestARoomsClosingCompactionWaitsForAPoolAHeavyApplyGaveBack), where the longest whole run of a
// test that waits roomWait measured 2.1 s at load 17 (roomWaitCap). Work the pool holds up never
// ends while the test holds the pool, so the test fails at this bound by name rather than at
// roomWaitCap's two minutes or the binary's timeout.
const besideAFullPoolWait = 30 * time.Second

// Cycle (ii). The room's persistence worker's gate decodes a history replaced under its room
// (storedHistoryHolds), waiting for its bytes of the pod's load concurrency while it holds the
// document's lock and a connection of the room-appends pool, and it is the one reader of the
// room's queue of updates to store, on which a heavy apply's commit waits while the queue is full
// (ygo's persistence observer). With the queue full and the pool full but for that apply's grant,
// a keystroke whose append finds the history a compaction folded past its room's cursor is
// stored, and the apply returns, because the apply gave its grant back before its commit's
// observers ran (weighPeerUpdate, crdt.ApplyBudget.Integrated): the gate's fold waits for the pool
// and gets what the apply gave back. Held through the commit, the grant is what the fold waited
// for while the apply waited for the fold's worker. The fold took only free bytes before, under a
// mark on the worker's context this change deleted, so a fold of every room whose history was
// replaced ran beside a full pool, counted by nothing and bounded by nothing.
//
// The history past the cursor is one folded row, which the gate decodes as one run
// (runsDocument), or that row and one written after it, which the gate folds and reads back
// (fold): the two grants a gate's fold takes, one each.
func TestTheWorkersGateFoldWaitsForAPoolAHeavyApplyGaveBack(t *testing.T) {
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

			// Every byte of the pool but a heavy apply's grant is held from before the worker takes
			// the document's lock until the test ends, which gives it back before the service shuts
			// down. The room's queue of updates to store fills behind the worker's wait for the
			// lock, and the heavy apply takes the rest of the pool and commits, its commit waiting
			// to hand its update to the worker.
			live := roomDocument(t, service, artifactID)
			update, grant := heavyRoomUpdate(t, live)
			pool, waited := poolOfOneSlot(t, nil)
			fillPoolBut(t, pool, grant)
			newRoomCommitter(t, live).fill(t)
			observer := holdNextObserver(t, service, artifactID, false)
			heavy := applyHeavy(service, artifactID, live, update)
			awaitHeavyApply(t, observer, heavy)
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("release the document's lock: %v", err)
			}

			wait := min(roomWait(t), besideAFullPoolWait)
			deadline := time.After(wait)
			select {
			case <-stored:
			case <-deadline:
				t.Fatalf("alice's keystroke on the folded history was not stored within %s while a heavy apply's grant filled the pool (a taker began to wait for the pool: %t): the worker's gate waits for the pool holding the document's lock",
					wait, waited.Load())
			}
			requireAnswered(t, heavy, deadline, "the heavy apply", wait, waited)
			if got := held.Load(); got < 1 {
				t.Fatal("no append was admitted on the folded history: nothing here is tested")
			}
			if moved := storedHead(t, database, artifactID); moved <= replacedHead {
				t.Fatalf("the document stands at update %d once alice's keystroke committed, want past the folded history's head %d", moved, replacedHead)
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

// deletedCharacters is how many characters deletedHistory types and then deletes: enough that the
// rows' bytes weigh many times the state the fold leaves of them (copyWeight).
const deletedCharacters = 4096

// deletedHistory is two rows: characters characters typed into a text in one update, and their
// deletion in another. A document applying them collects the deleted characters, keeping their
// item's id and length but none of its content (stateThrough), so the state the rows fold into is
// a fraction of their bytes. run is one keystroke typed into another text after them, which builds
// on the history's clocks alone.
func deletedHistory(characters int) (rows [][]byte, run []byte) {
	doc := crdt.New()
	write := func(change func(txn *crdt.Transaction)) []byte {
		before := doc.StateVector()
		doc.Transact(change)
		return crdt.EncodeStateAsUpdateV1(doc, before)
	}
	deleted := doc.GetText("deleted")
	rows = append(rows,
		write(func(txn *crdt.Transaction) { deleted.Insert(txn, 0, strings.Repeat("x", characters), nil) }),
		write(func(txn *crdt.Transaction) { deleted.Delete(txn, 0, characters) }))
	typed := doc.GetText("typed")
	return rows, write(func(txn *crdt.Transaction) { typed.Insert(txn, 0, "x", nil) })
}

// The gate's check of a history replaced under a room folds that history (storedHistoryHolds,
// fold), and the fold builds its own document row by row before it reads anything back. That
// document is counted in the pod's load concurrency: with the pool free, the gate's grant holds
// what the rows weigh - their bytes and the structs their headers declare (copyWeight), as
// foldRead weighs the rows it folds - once the fold has read them. Before, the fold's first pass
// took nothing: the gate read the rows outside foldRead, and the fold's one grant was its
// read-back's, weighed on the folded state alone.
//
// The history is prose typed and then deleted (deletedHistory): the fold's document collects the
// deleted content, so the rows weigh many times the state they fold into, and a grant on that
// state cannot pass for one on the rows.
func TestTheGatesFoldOfAReplacedHistoryIsCountedOnItsRows(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	rows, run := deletedHistory(deletedCharacters)
	var bytes, structs int64
	for version, row := range rows {
		if _, err := database.Pool.Exec(ctx, `insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, false)`,
			artifactID, version+1, row); err != nil {
			t.Fatalf("store row %d: %v", version+1, err)
		}
		bytes += int64(len(row))
		structs += int64(declaredStructs(row))
	}
	want := copyWeight(bytes, structs)
	state, _, _, misread, err := fold(ctx, memoryUpdates(ctx, rows), maxUpdateItems)
	if err != nil || misread != nil {
		t.Fatalf("fold the history: %v, %v", err, misread)
	}
	if folded := storedCopyWeight(state); folded >= want {
		t.Fatalf("the history's %d rows fold into a state weighing %d, against %d on the rows: want the rows heavier, or nothing here is tested", len(rows), folded, want)
	}
	slots := newLoadSlots(1)
	var taken slotsHeld
	slots.taken = taken.record
	pool := useLoadSlots(t, slots)
	holds, err := storedHistoryHolds(ctx, database.Pool, artifactID, int64(len(rows)), []BrowserUpdate{{Update: run, ContentChanged: true}}, maxUpdateItems)
	if err != nil || !holds {
		t.Fatalf("the stored history holds the keystroke = %t (%v), want it held: it builds on the history's clocks alone, and nothing here is tested otherwise", holds, err)
	}
	if grants := taken.all(); len(grants) < 2 || grants[len(grants)-2] != want {
		t.Fatalf("the gate's grants left %v held, want the one before the run's decode to hold the %d bytes the history's %d rows weigh, their %d bytes and %d declared structs (copyWeight), where the state they fold into weighs %d: the fold's own document was not counted",
			grants, want, len(rows), bytes, structs, storedCopyWeight(state))
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("%d bytes of the pool are held once the gate has judged the run, want none", held)
	}
	t.Logf("the gate's fold of %d rows of %d bytes declaring %d structs held %d bytes of the pool", len(rows), bytes, structs, want)
}

// The gate's check of a history replaced under a room decodes the run it would store onto the
// document that history makes (storedHistoryHolds), a whole decode of each of the run's updates,
// and holds that document until the run is judged. Both are counted in the pod's load
// concurrency: with the pool free, the run's decode raises the grant that holds the document by
// what the run's copy costs (storedCopyWeight), and the grant is given back only once the run is
// judged. Before, the document's grant was given back as the history's decode returned, and the
// run's decode held what the run cost alone while the document stood beside it uncounted.
func TestTheGatesDocumentIsCountedUntilItsRunIsJudged(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant beside the gate's
	seedServiceText(t, service, artifactID, "before")
	head := storedHead(t, service.store, artifactID)
	run := marginChain(t, crdt.New(), chainStructs)
	requireWeighedByItsStructs(t, run)
	want := storedCopyWeight(run)
	slots := newLoadSlots(1)
	var taken slotsHeld
	slots.taken = taken.record
	pool := useLoadSlots(t, slots)
	holds, err := storedHistoryHolds(context.Background(), service.store.Pool, artifactID, head, []BrowserUpdate{{Update: run, ContentChanged: true}}, maxUpdateItems)
	if err != nil || !holds {
		t.Fatalf("the stored history holds the run = %t (%v), want it held: the chain builds on its own items alone, and nothing here is tested otherwise", holds, err)
	}
	grants := taken.all()
	if len(grants) < 2 {
		t.Fatalf("the gate's grants left %v held, want one holding the document the history makes and then the run's decode onto it", grants)
	}
	document := grants[len(grants)-2]
	if last := grants[len(grants)-1]; last != document+want {
		t.Fatalf("the gate's grants left %v held, want the last, the run's decode, to hold the %d bytes the document held before it and the %d bytes the run's %d bytes and %d structs cost (storedCopyWeight)",
			grants, document, want, len(run), declaredStructs(run))
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("%d bytes of the pool are held once the gate has judged the run, want none", held)
	}
	t.Logf("the gate's document held %d bytes of the pool, and %d more while a %d-byte run declaring %d structs decoded onto it", document, want, len(run), declaredStructs(run))
}
