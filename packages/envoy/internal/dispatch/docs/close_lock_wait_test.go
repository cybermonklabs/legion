package docs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// pastTheWriteLockBound is how long the row below holds a document's lock: a second past the
// write lock bound every request-pool transaction carries (store's lockWait, 10 s), so a wait for
// the lock on the request pool is refused 55P03 while the holder still holds it.
const pastTheWriteLockBound = 11 * time.Second

// A close for a closed issue writes the latest edit source its room's record holds to the
// document's pending-settlement row, under the document's lock (persistLastActor). That is no
// request's work: the issue-open lane and the append gate's close run it with no request's ticket
// on their context. So it waits for the lock's holder for as long as the holder holds it, as every
// taker of the lock without a ticket does (lockDocumentRoom), and writes the row once the lock is
// free. On the request pool, whose every transaction carries the write lock bound (store.Pool's
// begin), the wait was refused 55P03 at 10 s while the holder still held the lock, and the close
// logged an ERROR and left the row without the source (INFRA-592).
//
// Here alice's edit is on the room's record and the seed's on the row, so the close is what puts
// alice on the row. A plain connection of the test's own holds the document's lock, and the issue
// then closes, notified to this process on a goroutine of its own (closeIssueOnItsOwnGoroutine).
// A second past the write lock bound the close must still be waiting, with the row as it was, and
// once the lock is let go the row must name alice. The row runs for that bound, eleven seconds.
// The service's settlements are held from the close on (holdSettlementsOnceArmed), since one
// could take the row before the read below gets to it.
func TestAClosesWriteOfTheLatestEditSourceWaitsForTheDocumentsLockPastTheWriteLockBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, _ := newTestServiceInstance(t, database)
	holdSettlements := holdSettlementsOnceArmed(t, service)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alicesEdit := model.Actor{Kind: "user", ID: "alice"}
	service.recordActor(t, artifactID, alicesEdit)
	// recordActor writes the row as well as the record, as a landed edit does; the seed's name
	// goes back on the row, so alice's edit is the record's alone and the close has a row to
	// write her to.
	seed := model.Actor{Kind: "user", ID: "seed"}
	recordRowEditSource(t, service, artifactID, seed)
	requireRecordLastActor(t, service, artifactID, alicesEdit)
	requirePendingSettlementActor(t, service, artifactID, seed, "the seed's append, which leaves the row the close reads")

	plain, err := pgx.Connect(ctx, database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect the session that holds the document's lock: %v", err)
	}
	testcleanup.Add(t, func() { _ = plain.Close(context.Background()) })
	holder, err := plain.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the document's lock: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}
	held := time.Now()

	holdSettlements()
	handled := closeIssueOnItsOwnGoroutine(t, service, "DOC-1")
	if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
		t.Fatal("the close's write of the latest edit source never came to wait on the document's lock")
	}
	time.Sleep(time.Until(held.Add(pastTheWriteLockBound)))
	select {
	case <-handled:
		t.Fatalf("the close finished %s after the document's lock was taken, while the holder still held it: its write of "+
			"the latest edit source must wait for the holder", time.Since(held).Round(time.Millisecond))
	default:
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting < 1 {
		t.Fatalf("no session waits on the document's lock %s after it was taken, want the close's write",
			time.Since(held).Round(time.Millisecond))
	}
	requirePendingSettlementActor(t, service, artifactID, seed, "the close waiting on the document's lock, which has written nothing yet")

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the document's lock go: %v", err)
	}
	awaitNotifiedClose(t, handled)
	requirePendingSettlementActor(t, service, artifactID, alicesEdit, "the close, once the document's lock was free")
}
