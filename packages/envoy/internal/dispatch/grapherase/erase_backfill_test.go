package grapherase

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheBlockBatchAndAnEraseOverTwoIssuesBothFinish is a block batch and an erase that share an
// owner row. The erase is of a comment on issue CORE-1's document, whose text a version of issue
// ACME-1's document quotes; that version is ACME-1's document's text and not the comment's, so
// the erase lists it and leaves it, and writes under CORE-1's row alone (lockInStoreOrder). The
// batch backfills a block of each document, so it takes both documents' owner rows. Another
// transaction holds CORE-1's document's document_block_versions row while the batch starts, so
// the batch stops there holding what it took before it; the erase then starts and queues behind
// the batch. Once that row is let go both have to finish.
//
// Taking both owner rows first, in owner-key order (graphcensus's lockBlockDocuments), the batch
// holds both while it waits, and the erase waits behind it holding none of them, so neither holds
// a row the other waits for.
func TestTheBlockBatchAndAnEraseOverTwoIssuesBothFinish(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	core := seedIssueDocument(t, database, "CORE", coreDocumentID, "Written before the store existed.")
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, core.artifactID).Scan(&comment); err != nil {
		t.Fatalf("write the comment on %s: %v", core.refKey, err)
	}
	acme := seedIssueDocument(t, database, "ACME", acmeDocumentID, "Alice wrote: A remark on the spec.")
	// The first run gives the comment its node, which the erase needs; the blocks are written
	// after it, so the run under test has them to backfill.
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the comment: %v", err)
	}
	seedPreStoreBlock(t, database, core, "only")
	seedPreStoreBlock(t, database, acme, "only")

	fence := lockstep.HoldRow(t, database, &lockstep.Hold{Name: core.refKey + "'s document_block_versions row", Step: lockstep.StepFence},
		`select 1 from document_block_versions where artifact_id = $1::uuid for update`, core.artifactID)
	backfillPID, backfilled := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, fence.Tx, backfillPID, backfilled, lockstep.Wait); done {
		t.Fatalf("the backfill answered (err %v) without waiting on %s's document_block_versions row, which another transaction held", early.err, core.refKey)
	}
	t.Logf("the backfill waits behind %s at %q, holding locks on %v", fence.Name,
		lockstep.WaitingStatement(t, database, backfillPID), lockstep.TablesLockedBy(t, ctx, database, backfillPID))
	erasePID, erased := startErase(t, database, commentNodeKind, comment)
	waitBlockedBy(t, ctx, database, erasePID, backfillPID, erased)
	t.Logf("the erase waits behind the backfill at %q", lockstep.WaitingStatement(t, database, erasePID))
	fence.Release()

	deadline := time.After(lockstep.Wait)
	for backfilled != nil || erased != nil {
		select {
		case answer := <-backfilled:
			if answer.err != nil {
				t.Errorf("the backfill failed once %s was let go: %v", fence.Name, answer.err)
			}
			backfilled = nil
		case answer := <-erased:
			if answer.err != nil {
				t.Errorf("the erase failed once %s was let go: %v", fence.Name, answer.err)
			} else if !answer.erasure.Applied {
				t.Errorf("the erase removed nothing once %s was let go: %+v", fence.Name, answer.erasure)
			}
			erased = nil
		case <-deadline:
			t.Fatalf("the backfill and the erase did not both answer within %s of the release of %s", lockstep.Wait, fence.Name)
		}
	}
	if t.Failed() {
		return
	}
	var named, commentStands int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks
		        where artifact_id = any($1::uuid[]) and block_id = 'only' and node_kind = $2 and node_address is not null),
		       (select count(*) from comments where id::text = $3)
	`, []string{core.artifactID, acme.artifactID}, refs.BlockNodeKind, comment).Scan(&named, &commentStands); err != nil {
		t.Fatalf("read what the two runs left: %v", err)
	}
	if named != 2 || commentStands != 0 {
		t.Errorf("after both runs %d of the two blocks name their node and %d comment row(s) stand, want both blocks and none", named, commentStands)
	}
}

// backfillAnswer is what one run of graphcensus.Backfill answered, of which this test reads the
// error alone.
type backfillAnswer struct {
	err error
}

// startBackfill runs graphcensus.Backfill on a pool of one connection, off the test's goroutine,
// and answers that connection's backend process id and the channel the run's answer arrives on.
// One connection is what lets the test watch the run's own backend: every statement of the run
// goes through it, and a session queued on the same lock with anything else is never taken for
// it.
func startBackfill(t *testing.T, database *store.Store) (uint32, <-chan backfillAnswer) {
	t.Helper()
	backfiller := storetest.SingleConnection(t, database)
	var pid uint32
	if err := backfiller.Pool.QueryRow(context.Background(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backfill's backend: %v", err)
	}
	answered := make(chan backfillAnswer, 1)
	go func() {
		_, err := graphcensus.Backfill(context.Background(), backfiller, io.Discard)
		answered <- backfillAnswer{err}
	}()
	return pid, answered
}

// waitBlockedBy waits until backend waiter waits on a lock backend blocker holds, which
// pg_blocking_pids answers, and fails the test if the waiter's run answers first or the wait
// passes lockstep.Wait.
func waitBlockedBy(t *testing.T, ctx context.Context, database *store.Store, waiter, blocker uint32, answered <-chan eraseAnswer) {
	t.Helper()
	deadline := time.Now().Add(lockstep.Wait)
	for {
		select {
		case answer := <-answered:
			t.Fatalf("the erase answered (err %v) without waiting on the backfill", answer.err)
		default:
		}
		var blocked bool
		if err := database.Pool.QueryRow(ctx,
			`select $2::int = any(pg_blocking_pids($1::int))`, int32(waiter), int32(blocker)).Scan(&blocked); err != nil {
			t.Fatalf("read what backend %d waits on: %v", waiter, err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d did not wait on backend %d within %s", waiter, blocker, lockstep.Wait)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
