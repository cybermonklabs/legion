package docs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func lockOrderServiceFor(t *testing.T, database *store.Store) *Service {
	t.Helper()
	service := New(Deps{Store: database, Settle: time.Hour})
	// Taken before the cleanup below is registered, so the context is cancelled after it runs.
	stop := testwait.Context(t)
	t.Cleanup(func() { _ = service.Shutdown(stop) })
	return service
}

func lockOrderService(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	id := createDocument(t, database, "")
	return database, lockOrderServiceFor(t, database), id
}

// waitForLockWait returns once a statement matching like waits on a lock, and fails at once with
// the waiting operation's result if it returns first. ctx is the context the count of lock waits
// is read under.
func waitForLockWait(t *testing.T, ctx context.Context, database *store.Store, like string, returned <-chan error) {
	t.Helper()
	waitFor(t, "a statement matching "+like+" to wait on a lock", func() bool {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before any statement matching %s waited on a lock", err, like)
		default:
		}
		return lockWaits(t, ctx, database, like) > 0
	})
}

// lockWaits counts the sessions of the test's database waiting on a lock whose statement matches
// like (storetest.CountLockWaits), failing the test when it cannot read them.
func lockWaits(t *testing.T, ctx context.Context, database *store.Store, like string) int {
	t.Helper()
	waiting, err := storetest.CountLockWaits(ctx, database, like)
	if err != nil {
		t.Fatal(err)
	}
	return waiting
}

func TestConditionalEditDoesNotInvertTheRoomLockOrder(t *testing.T) {
	ctx := testwait.Context(t)
	database, service, id := lockOrderService(t)
	seedServiceText(t, service, id, "first\n\nsecond")
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}

	_, token, err := service.TextWithToken(ctx, id)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}

	txOne, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer txOne.Rollback(context.Background())
	joinedOne, ledgerOne := service.Join(ctx, txOne)
	defer ledgerOne.Discard()
	if _, err := service.ApplyOps(joinedOne, id, []model.EditOp{{
		Op: "replace", Find: "first", With: "FIRST",
	}}, alice, &model.EditPrecondition{Document: token}); err != nil {
		t.Fatalf("first conditional edit: %v", err)
	}

	txTwo, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second transaction: %v", err)
	}
	defer txTwo.Rollback(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		joinedTwo, ledgerTwo := service.Join(ctx, txTwo)
		_, err := service.ApplyOps(joinedTwo, id, []model.EditOp{{
			Op: "replace", Find: "second", With: "SECOND",
		}}, bob, &model.EditPrecondition{Document: token})
		ledgerTwo.Discard()
		secondDone <- err
	}()
	// The second edit takes the document owner's lock first (see liveWrite), so it waits there,
	// in the database, holding nothing of the document.
	waitForLockWait(t, ctx, database, "%as owner_lock%", secondDone)

	snapshotDone := make(chan error, 1)
	go func() {
		_, err := service.SnapshotVersion(joinedOne, id, alice)
		snapshotDone <- err
	}()

	var snapshotErr error
	waitFor(t, "the snapshot to finish while the second edit waits: one that never does is a lock-order inversion, a conditional edit holding the document mutex while it waits for the advisory lock another writer holds", func() bool {
		select {
		case snapshotErr = <-snapshotDone:
			return true
		case err := <-secondDone:
			t.Fatalf("second edit returned before first transaction released its lock: %v", err)
		default:
		}
		return false
	})
	if snapshotErr != nil {
		t.Fatalf("snapshot while second edit waits: %v", snapshotErr)
	}

	// txTwo's connection belongs to the second edit until ApplyOps returns, so the deferred
	// rollback may not run under it. Ending the first transaction and its write lets the edit
	// through: its owner lock, then the writer slot, then the unchanged document's token.
	if err := txOne.Rollback(ctx); err != nil {
		t.Fatalf("roll back the first transaction: %v", err)
	}
	ledgerOne.Discard()
	if err := testwait.Receive(t, "the second edit to finish once the first transaction ended", secondDone); err != nil {
		t.Fatalf("second edit once the first transaction ended: %v", err)
	}
}

// ownerLockedTransaction holds the document owner's lock for the duration of one test, and the
// owner's row as an event append's write phase holds it (events.lockEventOwner, `for no key
// update`), so neither may block a durable writer.
func ownerLockedTransaction(t *testing.T, ctx context.Context, database *store.Store, id string) pgx.Tx {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner-locked transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, _, err := lockArtifactOwner(ctx, tx, id); err != nil {
		t.Fatalf("lock the document owner: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		select last_seq from artifacts where id = $1 and issue_key is null for no key update
	`, id); err != nil {
		t.Fatalf("hold the document owner's row: %v", err)
	}
	return tx
}

func lockOrderDocument(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	id := createProjectDocument(t, database, "first")
	return database, lockOrderServiceFor(t, database), id
}

// awaitUnblocked runs op and waits for it to return; subject names the writer in what the wait
// waits for, which a lock-order inversion never lets come.
func awaitUnblocked(t *testing.T, subject string, op func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	if err := testwait.Receive(t, subject+" to return while the owner's lock and row are held (one that never does is blocked by the document's owner lock, a lock-order inversion)", done); err != nil {
		t.Fatalf("run %s while the owner's lock and row are held: %v", subject, err)
	}
}

// An owner-locked writer and a room-locked writer must not block each other. A settlement, an
// event append and a comment write all take the document owner's lock - for a project document
// that owner is the artifact itself - and an append then its row, while the durable writers take
// the room lock and then reach that same row through doc_updates', doc_snapshots' and
// doc_checkpoints' foreign keys. A `for update` row lock would conflict with those key-share
// checks, so the two would close a cycle that Postgres breaks with `deadlock detected` - a 500
// for an upload that races the settlement its own previous write armed. The owner lock is an
// advisory lock and the append's row lock is `for no key update`: they still serialise the
// writers that take them, including the event sequence allocation, and do not block a foreign key.
func TestDurableAppendIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx := testwait.Context(t)
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	appending, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the appending transaction: %v", err)
	}
	defer appending.Rollback(context.Background())
	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	awaitUnblocked(t, "a durable append", func() error {
		_, err := service.persistence.AppendUpdateTx(
			ctx, appending, id, crdt.EncodeStateAsUpdateV1(doc, nil), true,
		)
		return err
	})
	if err := appending.Commit(context.Background()); err != nil {
		t.Fatalf("commit the append: %v", err)
	}
	// And the owner-locked transaction can still take the room lock behind it.
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the append: %v", err)
	}
}

func TestSnapshotIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx := testwait.Context(t)
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	// CaptureSnapshot owns its transaction and takes the room lock on its own connection
	// before it can reach the artifact row, so it is the writer the owner lock could only
	// ever deadlock with rather than merely delay.
	awaitUnblocked(t, "a snapshot", func() error {
		_, err := service.persistence.(*PgVersioned).CaptureSnapshot(ctx, id, "live", []byte{0})
		return err
	})
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the snapshot: %v", err)
	}
}

// The live-document path is the same cycle with the worst loser. A browser edit reaches
// AppendRoomFlush through websocket.go - and AppendUpdatesWithCredit, the append this exercises,
// when its room recorded no class for it - which takes the room lock on its own connection
// and then the artifact row through doc_updates' foreign key; when the owner lock was
// `for update` and an event append held it, Postgres killed one of them, and on this path the
// loser is failRoom - it evicts the room and drops the in-flight update rather than returning
// an error to anyone.
func TestLiveUpdateIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx := testwait.Context(t)
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	awaitUnblocked(t, "a live document update", func() error {
		_, err := service.persistence.(*PgVersioned).AppendUpdatesWithCredit(
			ctx, id, []BrowserUpdate{{Update: crdt.EncodeStateAsUpdateV1(doc, nil), ContentChanged: true}},
		)
		return err
	})
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the live update: %v", err)
	}
}
